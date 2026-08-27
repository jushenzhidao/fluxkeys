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

// Manager 是配额的唯一权威入口。
//
// P0-1: 所有写操作都经由 Redis Lua 原子执行；本结构体不缓存任何可写状态。
type Manager struct {
	rdb *redis.Client

	shaAcquire      string
	shaCommit       string
	shaRelease      string
	shaReap         string
	shaReconcile    string
	shaRefreshReset string

	now func() time.Time
}

// NewManager 创建并预加载全部 Lua 脚本。
func NewManager(ctx context.Context, rdb *redis.Client) (*Manager, error) {
	m := &Manager{rdb: rdb, now: time.Now}

	for _, s := range []struct {
		script string
		dst    *string
		name   string
	}{
		{luaAcquire, &m.shaAcquire, "acquire"},
		{luaCommit, &m.shaCommit, "commit"},
		{luaRelease, &m.shaRelease, "release"},
		{luaReap, &m.shaReap, "reap"},
		{luaReconcile, &m.shaReconcile, "reconcile"},
		{luaRefreshReset, &m.shaRefreshReset, "refresh_reset"},
	} {
		sha, err := rdb.ScriptLoad(ctx, s.script).Result()
		if err != nil {
			return nil, fmt.Errorf("quota: load script %s: %w", s.name, err)
		}
		*s.dst = sha
	}
	return m, nil
}

// SetClock 替换时钟，仅供测试使用。
func (m *Manager) SetClock(f func() time.Time) { m.now = f }

func quotaKey(kind Kind, keyID, day string) string {
	return fmt.Sprintf("volc:quota:%s:%s:%s", kind, keyID, day)
}

func leaseZSet(day string) string { return "volc:lease:" + day }

func leaseDataKey(leaseID string) string { return "volc:lease:data:" + leaseID }

func newLeaseID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Acquire 原子预扣配额并返回租约。
//
// 调用方必须以 Commit（成功，带实际用量）或 Release（失败）结束租约。
// 即便调用方失联，租约也会在 ttl 后被 Reap 自动回收（P0-2）。
func (m *Manager) Acquire(ctx context.Context, keyID string, kind Kind, amount int64, lim Limits, ttl time.Duration) (Decision, *Lease, error) {
	if amount <= 0 {
		return Denied, nil, fmt.Errorf("quota: amount must be positive, got %d", amount)
	}
	now := m.now()
	day := QuotaDay(now)
	leaseID := newLeaseID()
	expireAt := now.Add(ttl)

	res, err := m.rdb.EvalSha(ctx, m.shaAcquire,
		[]string{quotaKey(kind, keyID, day), leaseZSet(day), leaseDataKey(leaseID)},
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
		ID: leaseID, KeyID: keyID, Kind: kind,
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

	res, err := m.rdb.EvalSha(ctx, m.shaCommit,
		[]string{leaseZSet(lease.QuotaDay), leaseDataKey(lease.ID)},
		lease.ID, actual, now.Unix(),
	).Result()
	if err != nil {
		return fmt.Errorf("quota: commit: %w", err)
	}

	// 租约已被回收器清理（请求耗时超过 TTL）。预扣早已还原，
	// 但实际用量仍须补记，否则会少算而导致后续超刷。
	if vals, ok := res.([]interface{}); ok && len(vals) > 0 {
		if code, _ := vals[0].(int64); code == 0 && actual > 0 {
			qk := quotaKey(lease.Kind, lease.KeyID, lease.QuotaDay)
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
	err := m.rdb.EvalSha(ctx, m.shaRelease,
		[]string{leaseZSet(lease.QuotaDay), leaseDataKey(lease.ID)},
		lease.ID,
	).Err()
	if err != nil {
		return fmt.Errorf("quota: release: %w", err)
	}
	return nil
}

// Reap 回收已过期的租约，返回回收条数。应由后台任务周期调用（建议 30s）。
func (m *Manager) Reap(ctx context.Context, batch int) (int64, error) {
	now := m.now()
	total := int64(0)
	// 同时处理今日与昨日的租约集合，覆盖配额日切换的边界。
	for _, day := range []string{QuotaDay(now), QuotaDay(now.AddDate(0, 0, -1))} {
		n, err := m.rdb.EvalSha(ctx, m.shaReap, []string{leaseZSet(day)}, now.Unix(), batch).Int64()
		if err != nil {
			return total, fmt.Errorf("quota: reap %s: %w", day, err)
		}
		total += n
	}
	return total, nil
}

// Get 读取单个 Key 的配额快照（只读，用于调度打分）。
func (m *Manager) Get(ctx context.Context, keyID string, kind Kind) (Snapshot, error) {
	day := QuotaDay(m.now())
	vals, err := m.rdb.HGetAll(ctx, quotaKey(kind, keyID, day)).Result()
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
func (m *Manager) GetMany(ctx context.Context, keyIDs []string, kind Kind) (map[string]Snapshot, error) {
	if len(keyIDs) == 0 {
		return map[string]Snapshot{}, nil
	}
	day := QuotaDay(m.now())
	pipe := m.rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(keyIDs))
	for i, id := range keyIDs {
		cmds[i] = pipe.HGetAll(ctx, quotaKey(kind, id, day))
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

// Reconcile 对某 Key 执行对账，以未过期租约之和强制修正 prededuct。
// 返回修正前的偏差量（0 表示一致）。
func (m *Manager) Reconcile(ctx context.Context, keyID string, kind Kind) (int64, error) {
	now := m.now()
	day := QuotaDay(now)

	// 汇总该 Key 名下所有未过期租约
	leaseIDs, err := m.rdb.ZRangeByScore(ctx, leaseZSet(day), &redis.ZRangeBy{
		Min: fmt.Sprintf("%d", now.Unix()), Max: "+inf",
	}).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return 0, fmt.Errorf("quota: reconcile scan: %w", err)
	}

	var expected int64
	for _, id := range leaseIDs {
		data, err := m.rdb.HGetAll(ctx, leaseDataKey(id)).Result()
		if err != nil || data["key_id"] != keyID || data["kind"] != string(kind) {
			continue
		}
		expected += parseInt(data["amount"])
	}

	res, err := m.rdb.EvalSha(ctx, m.shaReconcile,
		[]string{quotaKey(kind, keyID, day)}, expected).Result()
	if err != nil {
		return 0, fmt.Errorf("quota: reconcile: %w", err)
	}
	if vals, ok := res.([]interface{}); ok && len(vals) > 0 {
		drift, _ := vals[0].(int64)
		return drift, nil
	}
	return 0, nil
}

// MarkRefreshed 在探测确认某 Key 已在火山侧刷新后，清零其配额计数。
//
// P0-4: 仅允许由探测器在收到成功响应后调用，绝不可按时间推测触发。
func (m *Manager) MarkRefreshed(ctx context.Context, keyID string, kind Kind, lim Limits) error {
	now := m.now()
	day := QuotaDay(now)
	err := m.rdb.EvalSha(ctx, m.shaRefreshReset,
		[]string{quotaKey(kind, keyID, day)},
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
