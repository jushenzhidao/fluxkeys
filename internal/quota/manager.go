package quota

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Kind 区分两种配额计费方式。
type Kind string

const (
	KindToken Kind = "token" // 500 万 Token/日
	KindCount Kind = "count" // 100 次/日（如 Seedream）
)

// Limits 描述单个 Key 在某类配额上的水位配置。
type Limits struct {
	Hard int64 // 硬水位: 超过即拒绝调度
	Soft int64 // 软水位: 超过则大幅降权，但仍可调度
}

// Decision 是一次预扣的结果。
type Decision int

const (
	Denied     Decision = iota // 超硬水位，拒绝
	Granted                    // 正常放行
	GrantedLow                 // 放行，但已过软水位（调度器应降权）
)

// Lease 代表一次成功预扣所持有的租约。必须以 Commit 或 Release 结束。
type Lease struct {
	ID       string
	Provider string
	KeyID    string
	Kind     Kind
	Amount   int64
	QuotaDay string
	ExpireAt time.Time
}

// Snapshot 是某个 Key 的配额只读快照，仅供调度打分使用。
type Snapshot struct {
	KeyID     string
	Kind      Kind
	Used      int64
	Prededuct int64
	Hard      int64
	Soft      int64
}

// Remaining 返回相对硬水位的剩余可用量。
func (s Snapshot) Remaining() int64 {
	r := s.Hard - s.Used - s.Prededuct
	if r < 0 {
		return 0
	}
	return r
}

// Ratio 返回已消耗比例（含预扣）。
func (s Snapshot) Ratio() float64 {
	if s.Hard <= 0 {
		return 1
	}
	return float64(s.Used+s.Prededuct) / float64(s.Hard)
}

// ErrInsufficient 表示配额不足。
var ErrInsufficient = errors.New("quota: insufficient")

// 脚本对象在包级构建一次。
//
// 用 redis.Script 而非手工 ScriptLoad + EvalSha: Script.Run 内部先试 EvalSha，
// 收到 NOSCRIPT 时自动退回 Eval 并重新缓存。手工方案在 Redis 重启 / 主从
// 切换到未加载脚本的实例后，所有配额调用会持续失败直到进程重启 ——
// 表现为全站 503，而 Redis 本身是健康的，极难定位。
var (
	scriptAcquire      = redis.NewScript(luaAcquire)
	scriptCommit       = redis.NewScript(luaCommit)
	scriptRelease      = redis.NewScript(luaRelease)
	scriptReap         = redis.NewScript(luaReap)
	scriptReconcile    = redis.NewScript(luaReconcile)
	scriptRefreshReset = redis.NewScript(luaRefreshReset)
)

// Manager 是配额的唯一权威入口。
//
// P0-1: 所有写操作都经由 Redis Lua 原子执行；本结构体不缓存任何可写状态。
type Manager struct {
	rdb *redis.Client
	now func() time.Time
}

// NewManager 创建配额管理器并预热全部 Lua 脚本。
//
// 预热失败不阻塞构造: Script.Run 在首次执行时会自动 Eval 并缓存，
// 这里的预加载只是把「首个请求多一次 RTT」提前消化掉。
func NewManager(ctx context.Context, rdb *redis.Client) (*Manager, error) {
	m := &Manager{rdb: rdb, now: time.Now}
	for _, s := range []*redis.Script{
		scriptAcquire, scriptCommit, scriptRelease,
		scriptReap, scriptReconcile, scriptRefreshReset,
	} {
		if err := s.Load(ctx, rdb).Err(); err != nil {
			return nil, fmt.Errorf("quota: 预加载脚本: %w", err)
		}
	}
	return m, nil
}

// SetClock 替换时钟，仅供测试使用。
func (m *Manager) SetClock(f func() time.Time) { m.now = f }

func quotaKey(provider string, kind Kind, keyID, day string) string {
	return fmt.Sprintf("%s:quota:%s:%s:%s", provider, kind, keyID, day)
}

func leaseZSet(provider, day string) string { return provider + ":lease:" + day }

func leaseDataKey(provider, leaseID string) string { return provider + ":lease:data:" + leaseID }

// leaseDataPrefix 是某 provider 的租约数据 key 前缀，供 Lua 内拼接。
func leaseDataPrefix(provider string) string { return provider + ":lease:data:" }

func newLeaseID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Acquire 原子预扣配额并返回租约。
//
// 调用方必须以 Commit（成功，带实际用量）或 Release（失败）结束租约。
// 即便调用方失联，租约也会在 ttl 后被 Reap 自动回收（P0-2）。
func (m *Manager) Acquire(ctx context.Context, provider, keyID string, kind Kind, amount int64, lim Limits, ttl time.Duration) (Decision, *Lease, error) {
	if amount <= 0 {
		return Denied, nil, fmt.Errorf("quota: amount must be positive, got %d", amount)
	}
	now := m.now()
	day := QuotaDay(now)
	leaseID := newLeaseID()
	expireAt := now.Add(ttl)

	res, err := scriptAcquire.Run(ctx, m.rdb,
		[]string{quotaKey(provider, kind, keyID, day), leaseZSet(provider, day), leaseDataKey(provider, leaseID)},
		amount, lim.Hard, lim.Soft, leaseID, expireAt.Unix(),
		keyID, string(kind), now.Unix(), int64(KeyTTL(now).Seconds()),
	).Result()
	if err != nil {
		return Denied, nil, fmt.Errorf("quota: acquire: %w", err)
	}

	vals, ok := res.([]interface{})
	if !ok || len(vals) < 1 {
		return Denied, nil, fmt.Errorf("quota: unexpected acquire reply %#v", res)
	}
	code, _ := vals[0].(int64)
	if code == 0 {
		return Denied, nil, ErrInsufficient
	}

	lease := &Lease{
		ID: leaseID, Provider: provider, KeyID: keyID, Kind: kind,
		Amount: amount, QuotaDay: day, ExpireAt: expireAt,
	}
	if code == 2 {
		return GrantedLow, lease, nil
	}
	return Granted, lease, nil
}

// Commit 以实际消耗量结束租约。
//
// 保守策略: 即使 actual 小于预扣量，也只释放预扣、按 actual 累加 used，
// 不做任何回退式的 used 减法，避免并发下互相抵消导致超刷。
func (m *Manager) Commit(ctx context.Context, lease *Lease, actual int64) error {
	if lease == nil {
		return errors.New("quota: nil lease")
	}
	if actual < 0 {
		actual = 0
	}
	now := m.now()

	res, err := scriptCommit.Run(ctx, m.rdb,
		[]string{leaseZSet(lease.Provider, lease.QuotaDay), leaseDataKey(lease.Provider, lease.ID)},
		lease.ID, actual, now.Unix(),
	).Result()
	if err != nil {
		return fmt.Errorf("quota: commit: %w", err)
	}

	// 租约已被回收器清理（请求耗时超过 TTL）。预扣早已还原，
	// 但实际用量仍须补记，否则会少算而导致后续超刷。
	if vals, ok := res.([]interface{}); ok && len(vals) > 0 {
		if code, _ := vals[0].(int64); code == 0 && actual > 0 {
			qk := quotaKey(lease.Provider, lease.Kind, lease.KeyID, lease.QuotaDay)
			if err := m.rdb.HIncrBy(ctx, qk, "used", actual).Err(); err != nil {
				return fmt.Errorf("quota: commit orphan usage: %w", err)
			}
		}
	}
	return nil
}

// Release 释放预扣且不计入 used，用于请求失败（如上游 5xx、Key 被封）。
func (m *Manager) Release(ctx context.Context, lease *Lease) error {
	if lease == nil {
		return nil
	}
	err := scriptRelease.Run(ctx, m.rdb,
		[]string{leaseZSet(lease.Provider, lease.QuotaDay), leaseDataKey(lease.Provider, lease.ID)},
		lease.ID,
	).Err()
	if err != nil {
		return fmt.Errorf("quota: release: %w", err)
	}
	return nil
}

// Reap 回收已过期的租约，返回回收条数。应由后台任务周期调用（建议 30s）。
func (m *Manager) Reap(ctx context.Context, provider string, batch int) (int64, error) {
	now := m.now()
	total := int64(0)
	// 同时处理今日与昨日的租约集合，覆盖配额日切换的边界。
	for _, day := range []string{QuotaDay(now), QuotaDay(now.AddDate(0, 0, -1))} {
		n, err := scriptReap.Run(ctx, m.rdb, []string{leaseZSet(provider, day)},
			now.Unix(), batch, leaseDataPrefix(provider)).Int64()
		if err != nil {
			return total, fmt.Errorf("quota: reap %s: %w", day, err)
		}
		total += n
	}
	return total, nil
}

// Get 读取单个 Key 的配额快照（只读，用于调度打分）。
func (m *Manager) Get(ctx context.Context, provider, keyID string, kind Kind) (Snapshot, error) {
	day := QuotaDay(m.now())
	vals, err := m.rdb.HGetAll(ctx, quotaKey(provider, kind, keyID, day)).Result()
	if err != nil {
		return Snapshot{}, fmt.Errorf("quota: get: %w", err)
	}
	s := Snapshot{KeyID: keyID, Kind: kind}
	s.Used = parseInt(vals["used"])
	s.Prededuct = parseInt(vals["prededuct"])
	s.Hard = parseInt(vals["hard_limit"])
	s.Soft = parseInt(vals["soft_limit"])
	return s, nil
}

// GetMany 批量读取配额快照。后台快照刷新用，单次 pipeline 完成。
func (m *Manager) GetMany(ctx context.Context, provider string, keyIDs []string, kind Kind) (map[string]Snapshot, error) {
	if len(keyIDs) == 0 {
		return map[string]Snapshot{}, nil
	}
	day := QuotaDay(m.now())
	pipe := m.rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(keyIDs))
	for i, id := range keyIDs {
		cmds[i] = pipe.HGetAll(ctx, quotaKey(provider, kind, id, day))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("quota: get many: %w", err)
	}

	out := make(map[string]Snapshot, len(keyIDs))
	for i, id := range keyIDs {
		vals, err := cmds[i].Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			continue
		}
		out[id] = Snapshot{
			KeyID: id, Kind: kind,
			Used:      parseInt(vals["used"]),
			Prededuct: parseInt(vals["prededuct"]),
			Hard:      parseInt(vals["hard_limit"]),
			Soft:      parseInt(vals["soft_limit"]),
		}
	}
	return out, nil
}

// Drift 是一次对账修正的记录。
type Drift struct {
	KeyID string
	Kind  Kind
	// Amount 是修正前的偏差量（prededuct - Σ未过期租约），正值代表泄漏。
	Amount int64
}

// ReconcileProvider 对某 provider 的一批 Key 做一轮对账，整体在单个 Lua
// 脚本内原子完成，返回被修正的条目。
//
// 取代旧的逐 Key Reconcile。旧实现有两个不可接受的缺陷:
//
//  1. 竞态: 「ZRANGE 列租约 → 逐个 HGETALL 求和 → Lua 覆写 prededuct」三步
//     跨越多个 RTT。求和之后、覆写之前完成的新 Acquire 会被覆写抹掉预扣，
//     该租约随后 Release/Commit 时再减一次 → prededuct 被钳到 0，本地水位
//     低于真实值。偏差方向是超刷 —— 恰是本项目最不能接受的方向。
//
//  2. N+1: 每 Key×kind 一轮「ZRANGE + 逐租约 HGETALL」，K 个 Key、L 条
//     租约就是 O(K×L) 次 RTT。1000 Key、500 租约时达到百万级往返，
//     每小时一轮的对账任务在 5 分钟超时内根本跑不完。
//
// 新实现把「扫租约 → 分组求和 → 修正」全部放进一个脚本: 数据本就都在
// Redis 里，原子性由 Redis 单线程执行保证，总开销 O(K+L) 且单次往返。
//
// keyIDs 必须传入全量 Key（而非只扫租约集合推导）: 「有泄漏但已无未过期
// 租约」的 Key 不在租约集合里，只扫租约永远修不到它。
//
// 脚本用前缀动态拼租约数据 key，不做跨 slot 静态声明，因此只适用于
// 单实例 / 主从架构 —— 本项目部署形态即是如此（docker compose 单机）。
func (m *Manager) ReconcileProvider(ctx context.Context, provider string, keyIDs []string) ([]Drift, error) {
	if len(keyIDs) == 0 {
		return nil, nil
	}
	now := m.now()
	day := QuotaDay(now)

	args := make([]interface{}, 0, 3+len(keyIDs))
	args = append(args,
		now.Unix(),
		leaseDataPrefix(provider),
		fmt.Sprintf("%s:quota:%%s:%%s:%s", provider, day),
	)
	for _, id := range keyIDs {
		args = append(args, id)
	}

	res, err := scriptReconcile.Run(ctx, m.rdb, []string{leaseZSet(provider, day)}, args...).Result()
	if err != nil {
		return nil, fmt.Errorf("quota: reconcile %s: %w", provider, err)
	}

	// 返回形如 {key_id, kind, drift, key_id, kind, drift, ...} 的扁平数组
	vals, ok := res.([]interface{})
	if !ok {
		return nil, fmt.Errorf("quota: unexpected reconcile reply %#v", res)
	}
	out := make([]Drift, 0, len(vals)/3)
	for i := 0; i+2 < len(vals); i += 3 {
		keyID, _ := vals[i].(string)
		kind, _ := vals[i+1].(string)
		var amount int64
		switch v := vals[i+2].(type) {
		case int64:
			amount = v
		case string:
			amount = parseInt(v)
		}
		out = append(out, Drift{KeyID: keyID, Kind: Kind(kind), Amount: amount})
	}
	return out, nil
}

// MarkRefreshed 在探测确认某 Key 已在上游侧刷新后，清零其配额计数。
//
// P0-4: 仅允许由探测器在收到成功响应后调用，绝不可按时间推测触发。
func (m *Manager) MarkRefreshed(ctx context.Context, provider, keyID string, kind Kind, lim Limits) error {
	now := m.now()
	day := QuotaDay(now)
	err := scriptRefreshReset.Run(ctx, m.rdb,
		[]string{quotaKey(provider, kind, keyID, day)},
		lim.Hard, lim.Soft, now.Unix(), int64(KeyTTL(now).Seconds()),
	).Err()
	if err != nil {
		return fmt.Errorf("quota: mark refreshed: %w", err)
	}
	return nil
}

func parseInt(s string) int64 {
	var n int64
	var neg bool
	for i, c := range s {
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	if neg {
		return -n
	}
	return n
}
