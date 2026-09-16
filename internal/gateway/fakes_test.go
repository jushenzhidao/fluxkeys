package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/confsnap"
	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/metrics"
	"github.com/fluxkeys/fluxkeys/internal/quota"
)

// 本文件是 gateway 单元测试的可控替身。
//
// 用手写 fake 而非 mock 框架: 这些测试要断言的核心是「租约有没有正确结束」
// 「配额有没有超刷」这类跨调用的时序性质，需要 fake 自己维护状态并做一致性
// 校验，而不只是记录调用参数。

// ===== 假调度器 =====

type fakeSched struct {
	mu sync.Mutex
	// keys 是按顺序返回的候选 Key。
	keys []Candidate
	// idx 指向下一个待返回的 Key。
	idx int
	// banned 记录被标记为不可用的 Key。
	banned map[string]FailureKind
	// successes / failures 记录上报次数。
	successes map[string]int
	failures  map[string][]FailureKind
	// selectErr 非 nil 时 Select 直接返回它。
	selectErr error
	// selectCalls 记录每次 Select 的 exclude 集合，用于验证重试是否真的换 Key。
	selectCalls []map[string]bool
	// reloads 记录 Reload 被调用次数；reloadErr 非 nil 时 Reload 返回它。
	reloads   int
	reloadErr error
	// statusSets 按调用顺序记录 SetKeyStatus 的参数，形如 "volc_001=banned"。
	statusSets []string
}

func newFakeSched(keyIDs ...string) *fakeSched {
	f := &fakeSched{
		banned:    map[string]FailureKind{},
		successes: map[string]int{},
		failures:  map[string][]FailureKind{},
	}
	for _, id := range keyIDs {
		f.keys = append(f.keys, Candidate{
			KeyID: id, Secret: "sk-mock-" + id, EgressIP: "", Kind: quota.KindToken,
		})
	}
	return f
}

func (f *fakeSched) Select(ctx context.Context, req SelectRequest) (*Candidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.selectErr != nil {
		return nil, f.selectErr
	}

	cp := make(map[string]bool, len(req.Exclude))
	for k, v := range req.Exclude {
		cp[k] = v
	}
	f.selectCalls = append(f.selectCalls, cp)

	// 从当前位置起找第一个未被排除且未被禁用的 Key
	for i := 0; i < len(f.keys); i++ {
		c := f.keys[(f.idx+i)%len(f.keys)]
		if req.Exclude[c.KeyID] {
			continue
		}
		if _, bad := f.banned[c.KeyID]; bad {
			continue
		}
		c.Kind = req.Kind
		return &c, nil
	}
	return nil, fmt.Errorf("%w: 全部 Key 已排除", ErrNoCandidate)
}

func (f *fakeSched) MarkSuccess(keyID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.successes[keyID]++
}

func (f *fakeSched) MarkFailure(keyID string, kind FailureKind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[keyID] = append(f.failures[keyID], kind)
	// 模拟真实调度器: auth 与 quota 类失败会让 Key 立即退出可选集合
	if kind == FailureAuth || kind == FailureQuota {
		f.banned[keyID] = kind
	}
}

// SetKeyStatus 记录管理接口对内存状态的显式改写。
//
// 记录顺序而非只记最终值: PATCH 端点必须「先写库成功、再同步内存」，
// 而验证这一顺序的唯一办法是断言写库失败时这里根本没被调用过。
func (f *fakeSched) SetKeyStatus(keyID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusSets = append(f.statusSets, keyID+"="+status)
}

func (f *fakeSched) statusSetCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.statusSets...)
}

func (f *fakeSched) KeyStates(ctx context.Context) ([]KeyState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]KeyState, 0, len(f.keys))
	for _, k := range f.keys {
		st := KeyState{KeyID: k.KeyID, Status: "active", Pool: "main",
			TokenLimit: 1000, TokenUsed: 100, HealthScore: 100}
		if kind, bad := f.banned[k.KeyID]; bad {
			st.Status = kind.String()
		}
		out = append(out, st)
	}
	return out, nil
}

// Reload 记录被调用次数，供「导入 Key 后应立即重载」的断言使用。
//
// reloadErr 可设置为非 nil 以验证重载失败不影响导入接口返回 200 ——
// Key 已落库，此时报错会让调用方误以为导入失败而重试。
func (f *fakeSched) Reload(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloads++
	return f.reloadErr
}

func (f *fakeSched) reloadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reloads
}

func (f *fakeSched) failuresFor(keyID string) []FailureKind {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FailureKind(nil), f.failures[keyID]...)
}

func (f *fakeSched) successCount(keyID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.successes[keyID]
}

// ===== 假存储 =====

type fakeStore struct {
	mu sync.Mutex
	// users 以明文 Key 为索引。
	users map[string]*UserContext
	// usage 收集写入的流水。
	usage []UsageRecord
	// audits 收集审计日志。
	audits []auditEntry
	// upstreamKeys 是已导入的火山 Key，以 key_id 为索引。
	upstreamKeys map[string]NewUpstreamKey
	// pingErr 非 nil 时 Ping 失败。
	pingErr error
	// recordErr 非 nil 时 RecordUsage 失败。
	recordErr error
	// upsertErr 非 nil 时 UpsertUpstreamKey 失败。
	upsertErr error
	// keyMeta 是 Key 的可 PATCH 元数据，以 key_id 为索引。
	keyMeta map[string]keyMeta
	// patchErr 非 nil 时 PatchUpstreamKeyState 失败，用于覆盖 500 分支。
	patchErr error
	// egressIPs 记录 SetVolcKeyEgressIP 落库的出口绑定，以 key_id 为索引。
	egressIPs map[string]string
	// setEgressErr 非 nil 时 SetVolcKeyEgressIP 失败，用于覆盖「迁移成功但落库失败」的降级分支。
	setEgressErr error
	// shardCalls 记录 AssignShard 的调用，供分片端点测试断言。
	shardCalls []shardCall
}

// shardCall 是一次 AssignShard 调用的入参快照。
type shardCall struct {
	Shard  string
	KeyIDs []string
}

// keyMeta 是 PATCH 端点可改的三个字段。
type keyMeta struct {
	status  string
	pool    string
	persona string
}

type auditEntry struct {
	Actor, Action, Target string
	Detail                map[string]any
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:   map[string]*UserContext{},
		keyMeta: map[string]keyMeta{},
	}
}

func (f *fakeStore) addUser(key string, uc UserContext) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[key] = &uc
}

func (f *fakeStore) AuthenticateUserKey(ctx context.Context, plaintext string) (*UserContext, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	uc, ok := f.users[plaintext]
	if !ok {
		return nil, fmt.Errorf("%w: 记录不存在", ErrUnauthorized)
	}
	cp := *uc
	return &cp, nil
}

func (f *fakeStore) RecordUsage(ctx context.Context, r UsageRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return f.recordErr
	}
	f.usage = append(f.usage, r)
	return nil
}

func (f *fakeStore) Audit(ctx context.Context, actor, action, target string, detail map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audits = append(f.audits, auditEntry{actor, action, target, detail})
	return nil
}

func (f *fakeStore) CreateUser(ctx context.Context, in NewUser) (*UserContext, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := int64(len(f.users) + 1)
	rpm := in.RPMLimit
	if rpm == 0 {
		rpm = 60 // 模拟存储层填默认值
	}
	return &UserContext{UserID: id, Name: in.Name, RPMLimit: rpm,
		TPMLimit: in.TPMLimit, DailyTokenLimit: in.DailyTokenLimit}, nil
}

func (f *fakeStore) CreateUserAPIKey(ctx context.Context, userID int64, name string) (string, IssuedKey, error) {
	return "fk-secret-plaintext", IssuedKey{ID: 42, Prefix: "fk-secre"}, nil
}

// RevokeUserAPIKey 按 (userID, keyID) 吊销。
//
// fake 以明文 token 为索引存用户，这里通过 APIKeyID + UserID 双匹配定位，
// 复刻真实存储「归属校验在同一条语句内完成」的语义。
func (f *fakeStore) RevokeUserAPIKey(ctx context.Context, userID, keyID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for token, uc := range f.users {
		if uc.APIKeyID == keyID && uc.UserID == userID {
			delete(f.users, token)
			return nil
		}
	}
	return fmt.Errorf("%w: key %d", ErrKeyNotFound, keyID)
}

func (f *fakeStore) UpsertUpstreamKey(ctx context.Context, in NewUpstreamKey) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return false, f.upsertErr
	}
	if f.upstreamKeys == nil {
		f.upstreamKeys = map[string]NewUpstreamKey{}
	}
	prev, existed := f.upstreamKeys[in.KeyID]
	// 复刻真实语义: secret 留空时保留库中已有密文，
	// 这让运维能用同一份清单只更新 pool 等元数据而不接触密钥
	if in.Secret == "" && existed {
		in.Secret = prev.Secret
	}
	f.upstreamKeys[in.KeyID] = in
	return !existed, nil
}

// AssignShard 复刻「只改存在的 Key，返回真实命中数」的语义。
//
// 不无条件返回 len(keyIDs): 端点会把 affected < requested 作为
// 「有 key_id 没匹配上」的信号透出，fake 若恒等于请求数，这个分支
// 在测试里就永远走不到。
func (f *fakeStore) AssignShard(ctx context.Context, shard string, keyIDs []string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shardCalls = append(f.shardCalls, shardCall{Shard: shard, KeyIDs: append([]string(nil), keyIDs...)})
	var n int64
	for _, id := range keyIDs {
		if _, ok := f.upstreamKeys[id]; ok {
			n++
		}
	}
	return n, nil
}

// PatchUpstreamKeyState 在内存中复刻存储层的原子局部更新语义。
//
// 刻意完整实现「nil 字段不改」「条件不匹配返回 ErrPreconditionFailed 且不写入」
// 而非直接返回成功: 如果 fake 无条件成功，状态机与并发控制的测试就只是在
// 断言 handler 把参数拼对了，而真正要防的「status 缺席时把状态改成空串」
// 恰恰发生在写入这一步。fake 必须自己维护状态才能让这类断言有意义。
func (f *fakeStore) PatchUpstreamKeyState(ctx context.Context, keyID string, p KeyPatch) (*KeyPatchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.patchErr != nil {
		return nil, f.patchErr
	}
	cur, ok := f.keyMeta[keyID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrKeyNotFound, keyID)
	}

	snapshot := &KeyPatchResult{
		PrevStatus: cur.status, PrevPool: cur.pool, PrevPersonaID: cur.persona,
		NewStatus: cur.status, NewPool: cur.pool, NewPersonaID: cur.persona,
	}
	if p.ExpectedStatus != nil && cur.status != *p.ExpectedStatus {
		return snapshot, fmt.Errorf("%w: expected_status 不匹配", ErrPreconditionFailed)
	}
	for _, bad := range p.RejectStatusFrom {
		if cur.status == bad {
			return snapshot, fmt.Errorf("%w: 当前状态 %s 被拒绝", ErrPreconditionFailed, cur.status)
		}
	}

	next := cur
	if p.Status != nil {
		next.status = *p.Status
	}
	if p.Pool != nil {
		next.pool = *p.Pool
	}
	if p.PersonaID != nil {
		next.persona = *p.PersonaID
	}
	f.keyMeta[keyID] = next

	return &KeyPatchResult{
		PrevStatus: cur.status, PrevPool: cur.pool, PrevPersonaID: cur.persona,
		NewStatus: next.status, NewPool: next.pool, NewPersonaID: next.persona,
	}, nil
}

// setKeyMeta 预置一个 Key 的元数据。
func (f *fakeStore) setKeyMeta(keyID, status, pool, persona string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyMeta[keyID] = keyMeta{status: status, pool: pool, persona: persona}
}

// keyMetaOf 返回某 Key 当前的元数据。
func (f *fakeStore) keyMetaOf(keyID string) (keyMeta, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.keyMeta[keyID]
	return m, ok
}

// SetVolcKeyEgressIP 记录出口绑定。真的存起来，让测试能断言落库确实发生。
func (f *fakeStore) SetVolcKeyEgressIP(ctx context.Context, keyID, egressIP string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setEgressErr != nil {
		return f.setEgressErr
	}
	if f.egressIPs == nil {
		f.egressIPs = map[string]string{}
	}
	f.egressIPs[keyID] = egressIP
	return nil
}

// DeleteUpstreamKey 复刻「删行 + 清内存态」语义。
//
// 同时清 upstreamKeys / keyMeta / egressIPs 三处: 真实存储删行后若内存态
// 残留，PATCH 还能改到一个已删 Key 的元数据、出口 IP 还能查到已删 Key 的绑定，
// 与测试收尾被迫用 SQL 直删时遇到的「容量假满」同源。fake 一次清干净，
// DELETE 端点的「删行与解绑同事务」不变量在测试里才断言得出来。
func (f *fakeStore) DeleteUpstreamKey(ctx context.Context, keyID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.upstreamKeys[keyID]; !ok {
		if _, ok2 := f.keyMeta[keyID]; !ok2 {
			return fmt.Errorf("%w: %s", ErrKeyNotFound, keyID)
		}
	}
	delete(f.upstreamKeys, keyID)
	delete(f.keyMeta, keyID)
	delete(f.egressIPs, keyID)
	return nil
}

// egressIPOf 返回落库的出口 IP。
func (f *fakeStore) egressIPOf(keyID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.egressIPs[keyID]
	return v, ok
}

func (f *fakeStore) Ping(ctx context.Context) error { return f.pingErr }

func (f *fakeStore) upstreamKeyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.upstreamKeys)
}

func (f *fakeStore) upstreamKey(id string) (NewUpstreamKey, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.upstreamKeys[id]
	return k, ok
}

func (f *fakeStore) usageRecords() []UsageRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]UsageRecord(nil), f.usage...)
}

func (f *fakeStore) auditRecords() []auditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]auditEntry(nil), f.audits...)
}

// ===== 假配额管理器 =====

// fakeQuota 在内存中复刻 Redis 侧的配额语义，用于验证租约不变量。
//
// 关键点: 它会主动检测「租约被结束两次」和「Commit 一个不存在的租约」，
// 这两种情况在真实 Redis 里表现为静默的数据错乱，测试里必须直接失败。
type fakeQuota struct {
	mu sync.Mutex
	// used 是已确认用量。
	used map[string]int64
	// prededuct 是预扣量（未结算的租约之和）。
	prededuct map[string]int64
	// open 是当前未结束的租约。
	open map[string]*quota.Lease
	// ended 记录已结束的租约 ID，用于检测重复结束。
	ended map[string]string
	// hard 是硬水位，超过即拒绝。
	hard int64
	// acquireErr 非 nil 时 Acquire 直接失败。
	acquireErr error
	// commits / releases 计数。
	commits  int64
	releases int64
	// violations 记录不变量被破坏的情况。
	violations []string
	// seq 用于生成租约 ID。
	seq int64
}

func newFakeQuota(hard int64) *fakeQuota {
	return &fakeQuota{
		used: map[string]int64{}, prededuct: map[string]int64{},
		open: map[string]*quota.Lease{}, ended: map[string]string{},
		hard: hard,
	}
}

func qkey(keyID string, kind quota.Kind) string { return keyID + "|" + string(kind) }

func (f *fakeQuota) Acquire(ctx context.Context, provider, keyID string, kind quota.Kind, amount int64,
	lim quota.Limits, ttl time.Duration) (quota.Decision, *quota.Lease, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.acquireErr != nil {
		return quota.Denied, nil, f.acquireErr
	}
	if amount <= 0 {
		return quota.Denied, nil, errors.New("fakeQuota: 预扣量必须为正")
	}

	k := qkey(keyID, kind)
	hard := lim.Hard
	if hard <= 0 {
		hard = f.hard
	}
	// 与真实 Lua 一致: used + prededuct + amount 超硬水位即拒绝
	if f.used[k]+f.prededuct[k]+amount > hard {
		return quota.Denied, nil, quota.ErrInsufficient
	}

	f.prededuct[k] += amount
	f.seq++
	lease := &quota.Lease{
		ID: fmt.Sprintf("lease_%d", f.seq), KeyID: keyID, Kind: kind,
		Amount: amount, QuotaDay: quota.QuotaDay(time.Now()),
		ExpireAt: time.Now().Add(ttl),
	}
	f.open[lease.ID] = lease

	dec := quota.Granted
	if lim.Soft > 0 && f.used[k]+f.prededuct[k] > lim.Soft {
		dec = quota.GrantedLow
	}
	return dec, lease, nil
}

func (f *fakeQuota) Commit(ctx context.Context, lease *quota.Lease, actual int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if lease == nil {
		f.violations = append(f.violations, "Commit 收到 nil 租约")
		return errors.New("nil lease")
	}
	if prev, done := f.ended[lease.ID]; done {
		f.violations = append(f.violations,
			fmt.Sprintf("租约 %s 被重复结束（首次: %s，本次: commit）", lease.ID, prev))
		return errors.New("租约已结束")
	}
	if _, ok := f.open[lease.ID]; !ok {
		f.violations = append(f.violations, "Commit 了一个不存在的租约: "+lease.ID)
		return errors.New("租约不存在")
	}

	k := qkey(lease.KeyID, lease.Kind)
	f.prededuct[k] -= lease.Amount
	f.used[k] += actual
	delete(f.open, lease.ID)
	f.ended[lease.ID] = "commit"
	f.commits++
	return nil
}

func (f *fakeQuota) Release(ctx context.Context, lease *quota.Lease) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if lease == nil {
		f.violations = append(f.violations, "Release 收到 nil 租约")
		return errors.New("nil lease")
	}
	if prev, done := f.ended[lease.ID]; done {
		f.violations = append(f.violations,
			fmt.Sprintf("租约 %s 被重复结束（首次: %s，本次: release）", lease.ID, prev))
		return errors.New("租约已结束")
	}
	if _, ok := f.open[lease.ID]; !ok {
		f.violations = append(f.violations, "Release 了一个不存在的租约: "+lease.ID)
		return errors.New("租约不存在")
	}

	k := qkey(lease.KeyID, lease.Kind)
	f.prededuct[k] -= lease.Amount
	delete(f.open, lease.ID)
	f.ended[lease.ID] = "release"
	f.releases++
	return nil
}

func (f *fakeQuota) Get(ctx context.Context, provider, keyID string, kind quota.Kind) (quota.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := qkey(keyID, kind)
	return quota.Snapshot{KeyID: keyID, Kind: kind,
		Used: f.used[k], Prededuct: f.prededuct[k], Hard: f.hard}, nil
}

// assertClean 校验所有租约都已结束且无不变量违规。
//
// 这是租约相关测试的统一收尾断言。「泄漏一个租约」在生产中的后果是该 Key
// 的额度被永久占用一部分，累积后整个池看起来满载而实际空闲。
func (f *fakeQuota) assertClean(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, v := range f.violations {
		t.Errorf("配额不变量被破坏: %s", v)
	}
	if n := len(f.open); n != 0 {
		for id, l := range f.open {
			t.Errorf("租约泄漏: %s (key=%s amount=%d)", id, l.KeyID, l.Amount)
		}
		t.Errorf("共 %d 个租约未结束", n)
	}
	for k, v := range f.prededuct {
		if v != 0 {
			t.Errorf("预扣量未归零: %s = %d", k, v)
		}
	}
}

func (f *fakeQuota) usedFor(keyID string, kind quota.Kind) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.used[qkey(keyID, kind)]
}

func (f *fakeQuota) counts() (commits, releases int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commits, f.releases
}

// ===== 测试用服务器构造 =====

// testEnv 是一套完整的测试环境。
type testEnv struct {
	srv      *Server
	ts       *httptest.Server
	sched    *fakeSched
	store    *fakeStore
	quota    *fakeQuota
	upstream *upstreamStub
	cfg      *config.Config
}

func (e *testEnv) Close() {
	e.ts.Close()
	e.upstream.Close()
}

// hotSwap 用改过的配置发布一份新快照，模拟管理端保存配置后的热生效。
//
// 传入的 mutate 拿到的是深拷贝而非 e.cfg 本身: 快照语义要求已发布的配置
// 一律只读，就地改 e.cfg 会让旧快照的持有者（正在处理中的请求）看到新值，
// 那恰好是这套机制要消灭的撕裂态，测试自己先破坏它就测不出问题了。
func (e *testEnv) hotSwap(t *testing.T, version int64, mutate func(*config.Config)) *confsnap.Snapshot {
	t.Helper()
	next := cloneConfig(t, e.cfg)
	mutate(next)
	snap, err := confsnap.Build(next, version)
	if err != nil {
		t.Fatalf("构建快照 v%d: %v", version, err)
	}
	e.srv.snaps.Store(snap)
	return snap
}

// cloneConfig 深拷贝配置中热加载会碰到的部分。
//
// 只深拷 Providers 这一层 map 及其内部 map: 浅拷贝下 next.Providers 与
// 原配置共用同一个 map，改 next 会同时改到旧快照，热切前后就分不开了。
func cloneConfig(t *testing.T, src *config.Config) *config.Config {
	t.Helper()
	out := *src
	out.Providers = make(map[string]config.Provider, len(src.Providers))
	for name, p := range src.Providers {
		cp := p
		if p.ModelMapping != nil {
			cp.ModelMapping = make(map[string]string, len(p.ModelMapping))
			for k, v := range p.ModelMapping {
				cp.ModelMapping[k] = v
			}
		}
		cp.CountModels = append([]string(nil), p.CountModels...)
		out.Providers[name] = cp
	}
	return &out
}

// newTestEnv 搭起「假上游 + 网关」的完整链路。
func newTestEnv(t *testing.T, opts ...func(*config.Config)) *testEnv {
	t.Helper()

	up := newUpstreamStub()

	cfg := config.Default()
	cfg.Upstream.MaxRetries = 2
	// 测试里把退避压到近零，否则重试测试会白等数秒
	cfg.Upstream.RetryBaseDelay = time.Millisecond
	cfg.Upstream.RetryJitter = time.Millisecond
	volc := cfg.Providers["volc"]
	volc.BaseURL = up.URL()
	volc.ModelMapping = map[string]string{"gpt-4o": "ep-test-4o"}
	volc.CountModels = []string{"seedream-3.0"}
	cfg.Providers["volc"] = volc
	cfg.Quota.DefaultMaxTokens = 100
	cfg.Quota.EstimateMultiplier = 1.2
	cfg.Admin.APIKey = "admin-secret"
	cfg.Server.MaxBodyBytes = 1 << 20
	for _, o := range opts {
		o(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("测试配置非法: %v", err)
	}

	sched := newFakeSched("volc_001", "volc_002", "volc_003")
	st := newFakeStore()
	st.addUser("user-key-ok", UserContext{UserID: 7, APIKeyID: 70, Name: "tester"})
	fq := newFakeQuota(1_000_000)

	pool, err := egress.NewPool(egress.ModeDirect, nil, 30*time.Second)
	if err != nil {
		t.Fatalf("构造出口池: %v", err)
	}

	srv, err := New(Deps{
		Config: cfg, Quota: fq, Egress: pool, Sched: sched, Store: st,
		Metrics: metrics.New(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("构造网关: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	env := &testEnv{srv: srv, ts: ts, sched: sched, store: st,
		quota: fq, upstream: up, cfg: cfg}
	t.Cleanup(env.Close)
	return env
}

// ===== 假上游 =====

// upstreamStub 是一个可编程的假火山服务。
//
// 与 test/mockark 的分工: mockark 是完整的 Mock 上游，由集成测试在进程内
// 启动；这里的 stub 只服务单元测试，行为完全由测试逐条指定，不涉及记账与
// 故障注入规则等状态。
type upstreamStub struct {
	ts *httptest.Server
	mu sync.Mutex
	// script 是按调用序返回的响应，最后一项会被后续调用重复使用。
	script []stubResponse
	// seenAuth 记录每次请求的 Authorization 头，用于验证换 Key 是否真的生效。
	seenAuth []string
	// seenModel 记录每次请求体里的 model 字段，即经 adapter 映射后真正发给
	// 上游的模型名。热切 model_mapping 后同一请求的各次 attempt 必须一致，
	// 否则就是「重试时换了配置」——这是不报错、只发错的那类缺陷。
	seenModel []string
	// onRequest 在每次请求进入时同步调用（参数为第几次调用，从 1 计）。
	// 用于在请求处理途中执行热切，制造「攻击窗口」。
	onRequest func(n int64)
	calls     int64
}

// stubResponse 描述假上游的一次响应。
type stubResponse struct {
	Status int
	Body   string
	// SSE 非空时按流式返回，每项作为一个 data 行的内容原样发出。
	SSE []string
	// ChunkDelay 是 SSE 各 chunk 之间的间隔，用于测试真流式与断连。
	ChunkDelay time.Duration
	// HoldUntilCancel 为 true 时在发完 SSE 后挂住直到客户端断开，
	// 用于验证「客户端断连必须被感知」。
	HoldUntilCancel bool
}

func newUpstreamStub() *upstreamStub {
	u := &upstreamStub{}
	u.ts = httptest.NewServer(http.HandlerFunc(u.serve))
	return u
}

func (u *upstreamStub) serve(w http.ResponseWriter, r *http.Request) {
	n := atomic.AddInt64(&u.calls, 1)

	// 先读完请求体再取 hook: 记录必须发生在测试执行热切之前，否则记下的
	// 是热切后的状态，就验证不出「本次 attempt 用的是哪份配置」。
	var model string
	if raw, err := io.ReadAll(r.Body); err == nil {
		var probe struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(raw, &probe) == nil {
			model = probe.Model
		}
	}

	u.mu.Lock()
	u.seenAuth = append(u.seenAuth, r.Header.Get("Authorization"))
	u.seenModel = append(u.seenModel, model)
	hook := u.onRequest
	var resp stubResponse
	switch {
	case len(u.script) == 0:
		resp = stubResponse{Status: 200, Body: `{"choices":[]}`}
	case int(n) <= len(u.script):
		resp = u.script[n-1]
	default:
		resp = u.script[len(u.script)-1]
	}
	u.mu.Unlock()

	// 在锁外调用: hook 里会执行热切并可能回读 stub 状态，持锁调用会死锁。
	if hook != nil {
		hook(n)
	}

	if len(resp.SSE) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.Status)
		_, _ = io.WriteString(w, resp.Body)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(resp.Status)
	flusher, _ := w.(http.Flusher)
	for _, line := range resp.SSE {
		if resp.ChunkDelay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(resp.ChunkDelay):
			}
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", line); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	if resp.HoldUntilCancel {
		<-r.Context().Done()
	}
}

func (u *upstreamStub) URL() string { return u.ts.URL }
func (u *upstreamStub) Close()      { u.ts.Close() }

func (u *upstreamStub) setScript(rs ...stubResponse) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.script = rs
	atomic.StoreInt64(&u.calls, 0)
	u.seenAuth = nil
	u.seenModel = nil
}

// setOnRequest 注册每次上游请求进入时的回调，用于在请求处理途中热切配置。
func (u *upstreamStub) setOnRequest(fn func(n int64)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.onRequest = fn
}

// models 返回各次 attempt 实际发给上游的模型名。
func (u *upstreamStub) models() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.seenModel...)
}

func (u *upstreamStub) callCount() int64 { return atomic.LoadInt64(&u.calls) }

func (u *upstreamStub) authHeaders() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.seenAuth...)
}
