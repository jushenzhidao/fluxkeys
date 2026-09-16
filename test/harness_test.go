// Package test 是 FluxKeys 的端到端集成测试。
//
// 与 internal/gateway 的单元测试的分工:
//
//	单元测试用逐条编程的假上游，验证「网关对某个具体响应的反应是否正确」；
//	本包用 test/mockark 假上游（测试进程内启动），验证「整条链路在真实协议下能否跑通」。
//
// 后者能抓到前者抓不到的问题: 假上游是按测试的期望写的，若网关和假上游
// 对协议的理解同时错了（比如都以为 usage 一定在 [DONE] 之前），单元测试
// 会全绿而线上失败。假上游按火山的真实报文格式实现，是独立的第二份实现。
//
// 仍被替换的是 Redis 与 Postgres —— 它们需要外部进程，而本包必须能在
// 无依赖的 CI 环境里跑（离线验证全链路是假上游存在的全部理由）。
package test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/gateway"
	"github.com/fluxkeys/fluxkeys/internal/metrics"
	"github.com/fluxkeys/fluxkeys/internal/quota"
	"github.com/fluxkeys/fluxkeys/test/mockark"
)

// ===== 内存调度器 =====

// memSched 是最小可用的调度器: 轮转选 Key，跳过被排除与被禁用的。
//
// 不复用 internal/scheduler 是有意的: 那个包有打分与加权随机，
// 选中哪个 Key 不确定，会让「配额耗尽后换到下一个 Key」这类断言变得
// 无法精确表达。集成测试要验证的是网关的换 Key 逻辑，不是打分算法。
type memSched struct {
	mu       sync.Mutex
	keys     []string
	cursor   int
	disabled map[string]string
	failures map[string][]gateway.FailureKind
	success  map[string]int
	reloads  int
	// pools 是 Key 的档位归属，决定它首次绑定出口时落在哪档 IP 上。
	// 未设置的 Key 用空串（不限档位）。
	pools map[string]string
}

func newMemSched(keys ...string) *memSched {
	return &memSched{
		keys:     keys,
		disabled: map[string]string{},
		failures: map[string][]gateway.FailureKind{},
		success:  map[string]int{},
		pools:    map[string]string{},
	}
}

func (m *memSched) Select(ctx context.Context, req gateway.SelectRequest) (*gateway.Candidate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := 0; i < len(m.keys); i++ {
		id := m.keys[(m.cursor+i)%len(m.keys)]
		if req.Exclude[id] {
			continue
		}
		if _, bad := m.disabled[id]; bad {
			continue
		}
		m.cursor = (m.cursor + i + 1) % len(m.keys)
		return &gateway.Candidate{
			KeyID: id,
			// mockark 按 sk-mock-{key_id} 约定从密钥反推 Key 身份，
			// 从而无需在两侧维护一张密钥表
			Secret: "sk-mock-" + id,
			// Pool 决定该 Key 首次绑定出口时落在哪个档位的 IP 上
			Pool: m.pools[id],
			Kind: req.Kind,
		}, nil
	}
	return nil, fmt.Errorf("%w: 全部 Key 不可用", gateway.ErrNoCandidate)
}

func (m *memSched) MarkSuccess(keyID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.success[keyID]++
}

func (m *memSched) MarkFailure(keyID string, kind gateway.FailureKind) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures[keyID] = append(m.failures[keyID], kind)
	// 复刻真实调度器: auth 与 quota 类失败让 Key 退出可选集合
	if kind == gateway.FailureAuth || kind == gateway.FailureQuota {
		m.disabled[keyID] = kind.String()
	}
}

func (m *memSched) KeyStates(ctx context.Context) ([]gateway.KeyState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]gateway.KeyState, 0, len(m.keys))
	for _, id := range m.keys {
		st := gateway.KeyState{KeyID: id, Status: "active", Pool: "main", HealthScore: 100}
		if s, bad := m.disabled[id]; bad {
			st.Status = s
		}
		out = append(out, st)
	}
	return out, nil
}

// Reload 在内存实现里无需真正重载，只记录调用次数。
func (m *memSched) Reload(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reloads++
	return nil
}

// SetKeyStatus 复刻真实调度器的行为: 非 active 的 Key 退出可选集合。
//
// 必须真的让 Key 退出而非只记一笔，否则「PATCH 封禁后该 Key 不再被选中」
// 这条断言就无从验证 —— 而这正是管理接口封禁功能的全部意义。
func (m *memSched) SetKeyStatus(keyID, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if status == "active" {
		delete(m.disabled, keyID)
		return
	}
	m.disabled[keyID] = status
}

func (m *memSched) failuresOf(keyID string) []gateway.FailureKind {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]gateway.FailureKind(nil), m.failures[keyID]...)
}

func (m *memSched) disabledKeys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.disabled))
	for k := range m.disabled {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ===== 内存存储 =====

type memStore struct {
	mu     sync.Mutex
	users  map[string]*gateway.UserContext
	usage  []gateway.UsageRecord
	audits int
	keys   map[string]gateway.NewUpstreamKey
}

func newMemStore() *memStore {
	return &memStore{
		users: map[string]*gateway.UserContext{
			"itest-key": {UserID: 1, APIKeyID: 11, Name: "itest"},
		},
		keys: map[string]gateway.NewUpstreamKey{},
	}
}

func (s *memStore) AuthenticateUserKey(ctx context.Context, plaintext string) (*gateway.UserContext, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	uc, ok := s.users[plaintext]
	if !ok {
		return nil, fmt.Errorf("%w: 记录不存在", gateway.ErrUnauthorized)
	}
	cp := *uc
	return &cp, nil
}

func (s *memStore) RecordUsage(ctx context.Context, r gateway.UsageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = append(s.usage, r)
	return nil
}

func (s *memStore) Audit(ctx context.Context, actor, action, target string, d map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits++
	return nil
}

func (s *memStore) CreateUser(ctx context.Context, in gateway.NewUser) (*gateway.UserContext, error) {
	return &gateway.UserContext{UserID: 2, Name: in.Name, RPMLimit: in.RPMLimit}, nil
}

func (s *memStore) CreateUserAPIKey(ctx context.Context, userID int64, name string) (string, gateway.IssuedKey, error) {
	return "fk-itest-plain", gateway.IssuedKey{ID: 9, Prefix: "fk-itest"}, nil
}

func (s *memStore) RevokeUserAPIKey(ctx context.Context, userID, keyID int64) error {
	// 集成测试不覆盖吊销路径，返回未命中即可。
	return fmt.Errorf("%w: key %d", gateway.ErrKeyNotFound, keyID)
}

func (s *memStore) AssignShard(ctx context.Context, shard string, keyIDs []string) (int64, error) {
	// 集成测试不覆盖分片指派路径。
	return 0, nil
}

func (s *memStore) UpsertUpstreamKey(ctx context.Context, in gateway.NewUpstreamKey) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.keys[in.KeyID]
	s.keys[in.KeyID] = in
	return !existed, nil
}

// DeleteUpstreamKey 从内存表删除 Key，复刻存储层「影响 0 行即不存在」。
func (s *memStore) DeleteUpstreamKey(ctx context.Context, keyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[keyID]; !ok {
		return fmt.Errorf("%w: %s", gateway.ErrKeyNotFound, keyID)
	}
	delete(s.keys, keyID)
	return nil
}

// PatchUpstreamKeyState 在内存中复刻局部更新语义。
//
// 复用 keys 这张表而非另开一张: 集成测试里 PATCH 的对象就是刚导入的 Key，
// 分成两张表会让「导入后立刻 PATCH」这条最真实的路径反而测不到。
func (s *memStore) PatchUpstreamKeyState(ctx context.Context, keyID string, p gateway.KeyPatch) (*gateway.KeyPatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, ok := s.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", gateway.ErrKeyNotFound, keyID)
	}
	// 导入时未指定 status 的 Key 在真实存储层会落成 active，这里对齐该默认值，
	// 否则终态守卫会因为当前值是空串而永远不触发。
	if cur.Status == "" {
		cur.Status = "active"
	}

	snapshot := &gateway.KeyPatchResult{
		PrevStatus: cur.Status, PrevPool: cur.Pool, PrevPersonaID: cur.PersonaID,
		NewStatus: cur.Status, NewPool: cur.Pool, NewPersonaID: cur.PersonaID,
	}
	if p.ExpectedStatus != nil && cur.Status != *p.ExpectedStatus {
		return snapshot, fmt.Errorf("%w: expected_status 不匹配", gateway.ErrPreconditionFailed)
	}
	for _, bad := range p.RejectStatusFrom {
		if cur.Status == bad {
			return snapshot, fmt.Errorf("%w: 当前状态 %s 被拒绝",
				gateway.ErrPreconditionFailed, cur.Status)
		}
	}

	next := cur
	if p.Status != nil {
		next.Status = *p.Status
	}
	if p.Pool != nil {
		next.Pool = *p.Pool
	}
	if p.PersonaID != nil {
		next.PersonaID = *p.PersonaID
	}
	s.keys[keyID] = next

	return &gateway.KeyPatchResult{
		PrevStatus: cur.Status, PrevPool: cur.Pool, PrevPersonaID: cur.PersonaID,
		NewStatus: next.Status, NewPool: next.Pool, NewPersonaID: next.PersonaID,
	}, nil
}

// SetVolcKeyEgressIP 写回出口绑定。
//
// 复用 keys 这张表，使得「PATCH 改池 → 出口迁移 → 落库」这条链路在集成
// 测试里可以直接用 GET /admin/keys 观察到结果。
func (s *memStore) SetVolcKeyEgressIP(ctx context.Context, keyID, egressIP string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.keys[keyID]
	if !ok {
		return fmt.Errorf("%w: %s", gateway.ErrKeyNotFound, keyID)
	}
	cur.EgressIP = egressIP
	s.keys[keyID] = cur
	return nil
}

func (s *memStore) Ping(ctx context.Context) error { return nil }

func (s *memStore) records() []gateway.UsageRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gateway.UsageRecord(nil), s.usage...)
}

// ===== 内存配额管理器 =====

// memQuota 复刻 Redis Lua 的准入语义，并对租约生命周期做强校验。
//
// 与 internal/gateway 单测里的那份是同一思路的两份实现。刻意不抽取共享:
// 这份替身的价值恰恰在于它是独立写出来的 —— 抽成公共代码后，一个理解偏差
// 会同时影响两层测试，等于失去交叉验证。
type memQuota struct {
	mu         sync.Mutex
	used       map[string]int64
	prededuct  map[string]int64
	open       map[string]*quota.Lease
	ended      map[string]string
	violations []string
	seq        int64
	// hardOverride > 0 时覆盖调用方传入的水位，便于构造「配额耗尽」场景。
	hardOverride int64
}

func newMemQuota() *memQuota {
	return &memQuota{
		used: map[string]int64{}, prededuct: map[string]int64{},
		open: map[string]*quota.Lease{}, ended: map[string]string{},
	}
}

func mqKey(keyID string, kind quota.Kind) string { return keyID + "|" + string(kind) }

func (q *memQuota) Acquire(ctx context.Context, provider, keyID string, kind quota.Kind, amount int64,
	lim quota.Limits, ttl time.Duration) (quota.Decision, *quota.Lease, error) {

	q.mu.Lock()
	defer q.mu.Unlock()

	if amount <= 0 {
		return quota.Denied, nil, errors.New("memQuota: 预扣量必须为正")
	}
	hard := lim.Hard
	if q.hardOverride > 0 {
		hard = q.hardOverride
	}

	k := mqKey(keyID, kind)
	if q.used[k]+q.prededuct[k]+amount > hard {
		return quota.Denied, nil, quota.ErrInsufficient
	}

	q.prededuct[k] += amount
	q.seq++
	l := &quota.Lease{
		ID: fmt.Sprintf("l%d", q.seq), KeyID: keyID, Kind: kind, Amount: amount,
		QuotaDay: quota.QuotaDay(time.Now()), ExpireAt: time.Now().Add(ttl),
	}
	q.open[l.ID] = l

	if lim.Soft > 0 && q.used[k]+q.prededuct[k] > lim.Soft {
		return quota.GrantedLow, l, nil
	}
	return quota.Granted, l, nil
}

func (q *memQuota) Commit(ctx context.Context, l *quota.Lease, actual int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.endLease(l, "commit"); err != nil {
		return err
	}
	q.used[mqKey(l.KeyID, l.Kind)] += actual
	return nil
}

func (q *memQuota) Release(ctx context.Context, l *quota.Lease) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.endLease(l, "release")
}

// endLease 是租约结束的公共校验。调用者必须持有 q.mu。
func (q *memQuota) endLease(l *quota.Lease, how string) error {
	if l == nil {
		q.violations = append(q.violations, how+" 收到 nil 租约")
		return errors.New("nil lease")
	}
	if prev, done := q.ended[l.ID]; done {
		// 重复结束在真实 Redis 里表现为 prededuct 被多减一次，
		// 之后该 Key 的水位会永久偏低 —— 直接导致超刷。
		q.violations = append(q.violations,
			fmt.Sprintf("租约 %s 被重复结束（首次 %s，本次 %s）", l.ID, prev, how))
		return errors.New("租约已结束")
	}
	if _, ok := q.open[l.ID]; !ok {
		q.violations = append(q.violations, how+" 了一个不存在的租约: "+l.ID)
		return errors.New("租约不存在")
	}
	q.prededuct[mqKey(l.KeyID, l.Kind)] -= l.Amount
	delete(q.open, l.ID)
	q.ended[l.ID] = how
	return nil
}

func (q *memQuota) Get(ctx context.Context, provider, keyID string, kind quota.Kind) (quota.Snapshot, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	k := mqKey(keyID, kind)
	return quota.Snapshot{KeyID: keyID, Kind: kind,
		Used: q.used[k], Prededuct: q.prededuct[k], Hard: q.hardOverride}, nil
}

func (q *memQuota) usedOf(keyID string, kind quota.Kind) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.used[mqKey(keyID, kind)]
}

// assertClean 校验租约不变量。
func (q *memQuota) assertClean(t *testing.T) {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, v := range q.violations {
		t.Errorf("配额不变量被破坏: %s", v)
	}
	for id, l := range q.open {
		t.Errorf("租约泄漏: %s (key=%s amount=%d)", id, l.KeyID, l.Amount)
	}
	for k, v := range q.prededuct {
		if v != 0 {
			t.Errorf("预扣量未归零: %s = %d", k, v)
		}
	}
}

// ===== 集成环境 =====

// itEnv 是一套「真实 mockark + 真实 gateway」的环境。
type itEnv struct {
	gw    *httptest.Server
	ark   *mockark.Server
	arkTS *httptest.Server
	sched *memSched
	store *memStore
	quota *memQuota
	srv   *gateway.Server
	cfg   *config.Config
	// egress 是本环境实际使用的出口池，供用例断言 IP 信誉与绑定变化。
	egress *egress.Pool
}

// newItEnvWithEgress 搭建使用指定出口池的集成环境。
//
// 出口相关的行为（IP 信誉记账、绑定迁移）在 direct 模式下全是空操作，
// 用默认的 newItEnv 测这类逻辑会得到「零覆盖的全绿」。
func newItEnvWithEgress(t *testing.T, keys []string, ips []*egress.IP,
	tune ...func(*config.Config, *mockark.Options)) *itEnv {
	t.Helper()
	return newItEnvOpts(t, keys, ips, tune...)
}

// newItEnv 搭建集成环境（direct 出口）。keys 是参与调度的火山 Key ID。
func newItEnv(t *testing.T, keys []string, tune ...func(*config.Config, *mockark.Options)) *itEnv {
	t.Helper()
	return newItEnvOpts(t, keys, nil, tune...)
}

// newItEnvOpts 是 newItEnv 与 newItEnvWithEgress 的共同实现。
// ips 为 nil 时用 direct 模式出口池。
func newItEnvOpts(t *testing.T, keys []string, ips []*egress.IP,
	tune ...func(*config.Config, *mockark.Options)) *itEnv {
	t.Helper()

	arkOpt := mockark.Options{
		DefaultTokenLimit: 5_000_000,
		DefaultCountLimit: 100,
		StreamChunks:      4,
		StreamChunkDelay:  10 * time.Millisecond,
		MaxLogs:           500,
		Models:            []string{"ep-itest-chat", "ep-itest-image"},
		Seed:              42, // 固定随机源让 completion_tokens 可复现
	}

	cfg := config.Default()
	cfg.Upstream.MaxRetries = 3
	cfg.Upstream.RetryBaseDelay = 2 * time.Millisecond
	cfg.Upstream.RetryJitter = 2 * time.Millisecond
	volcCfg := cfg.Providers["volc"]
	volcCfg.ModelMapping = map[string]string{
		"gpt-4o":       "ep-itest-chat",
		"seedream-3.0": "ep-itest-image",
	}
	volcCfg.CountModels = []string{"seedream-3.0"}
	cfg.Providers["volc"] = volcCfg
	cfg.Quota.DefaultMaxTokens = 200
	cfg.Quota.EstimateMultiplier = 1.2
	cfg.Admin.APIKey = "itest-admin"

	for _, f := range tune {
		f(cfg, &arkOpt)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("配置非法: %v", err)
	}

	ark := mockark.NewServer(arkOpt)
	arkTS := httptest.NewServer(ark)
	volcCfg = cfg.Providers["volc"]
	volcCfg.BaseURL = arkTS.URL
	cfg.Providers["volc"] = volcCfg

	for _, id := range keys {
		ark.RegisterKey(id, arkOpt.DefaultTokenLimit, arkOpt.DefaultCountLimit)
	}

	sched := newMemSched(keys...)
	st := newMemStore()
	q := newMemQuota()

	// 超时必须取配置值而非硬编码: 超时相关的用例靠调低它来收敛运行时长，
	// 写死会让那些用例静默地依赖上游的实际延迟。
	reqTimeout := cfg.Egress.RequestTimeout
	if reqTimeout <= 0 {
		reqTimeout = 30 * time.Second
	}
	mode := egress.ModeDirect
	if len(ips) > 0 {
		mode = egress.ModeMultiIP
	}
	pool, err := egress.NewPool(mode, ips, reqTimeout)
	if err != nil {
		t.Fatalf("构造出口池: %v", err)
	}

	srv, err := gateway.New(gateway.Deps{
		Config: cfg, Quota: q, Egress: pool, Sched: sched, Store: st,
		Metrics: metrics.New(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("构造网关: %v", err)
	}

	gw := httptest.NewServer(srv.Handler())
	env := &itEnv{gw: gw, ark: ark, arkTS: arkTS, sched: sched,
		store: st, quota: q, srv: srv, cfg: cfg, egress: pool}
	t.Cleanup(func() {
		gw.Close()
		arkTS.Close()
	})
	return env
}

// chat 发起一次对话请求。
func (e *itEnv) chat(t *testing.T, body string) (*http.Response, string) {
	t.Helper()
	return e.do(t, http.MethodPost, "/v1/chat/completions", "itest-key", body)
}

func (e *itEnv) do(t *testing.T, method, path, token, body string) (*http.Response, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.gw.URL+path, rdr)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.gw.Client().Do(req)
	if err != nil {
		t.Fatalf("发起请求: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应: %v", err)
	}
	return resp, string(raw)
}

// arkLogs 返回 mockark 记录的请求日志。
//
// 这是集成测试的核心观测点: 它是上游侧的独立事实，不受网关内部状态影响。
func (e *itEnv) arkLogs() []mockark.RequestLog { return e.ark.Logs() }
