package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/metrics"
)

// provider 配置管理端点的守卫测试。
//
// 这一组测试的重点不是「功能可用」，而是**不该被允许的事真的做不到**。
// 三条不可变性判据（name / quota_kind / 跨量纲回滚）都做了反向验证:
// 把拦截逻辑注释掉，对应用例必须 FAIL。反向验证的意义在于排除
// 「用例因为别的原因恰好通过」—— 一条永远绿的守卫测试比没有测试更危险，
// 因为它会让人以为这条边界已经被守住了。

// ===== 假 provider 配置存储 =====

// fakeProviderStore 是 ProviderConfigStore 的内存实现。
//
// 刻意在存储层也复刻不可变性拒绝: 真实存储（internal/store）在事务里
// 是最后一道防线，fake 不复刻的话，测试就无法区分「API 层拦住了」
// 与「API 层放过去、存储层拦住了」。
type fakeProviderStore struct {
	*fakeStore
	providers map[string]ProviderConfigView
	versions  map[int64]ProviderVersionView
	nextID    int64
	// writes 记录成功落库的写入，用于断言「被拒的请求没有留下任何痕迹」。
	writes []ProviderWriteInput
	peak   int64
}

func newFakeProviderStore() *fakeProviderStore {
	return &fakeProviderStore{
		fakeStore: newFakeStore(),
		providers: make(map[string]ProviderConfigView),
		versions:  make(map[int64]ProviderVersionView),
		nextID:    100,
	}
}

func (f *fakeProviderStore) seed(p ProviderConfigView) int64 {
	f.nextID++
	id := f.nextID
	p.Version = id
	if p.ModelMapping == nil {
		p.ModelMapping = map[string]string{}
	}
	f.providers[p.Name] = p
	f.versions[id] = ProviderVersionView{
		ID: id, ProviderName: p.Name, Action: "seed",
		CreatedAt: time.Now(), CreatedBy: "system",
		Snapshot: &p,
	}
	return id
}

// seedVersion 追加一条历史版本，快照可与当前态不同（用于构造跨量纲回滚）。
func (f *fakeProviderStore) seedVersion(name string, snap ProviderConfigView) int64 {
	f.nextID++
	id := f.nextID
	snap.Version = id
	f.versions[id] = ProviderVersionView{
		ID: id, ProviderName: name, Action: "update",
		CreatedAt: time.Now(), CreatedBy: "ops",
		Snapshot: &snap,
	}
	return id
}

func (f *fakeProviderStore) ListProvidersWithUsage(_ context.Context, _ time.Time) ([]ProviderListEntry, error) {
	out := make([]ProviderListEntry, 0, len(f.providers))
	for _, p := range f.providers {
		out = append(out, ProviderListEntry{ProviderConfigView: p})
	}
	return out, nil
}

func (f *fakeProviderStore) GetProviderConfig(_ context.Context, name string) (ProviderConfigView, error) {
	p, ok := f.providers[name]
	if !ok {
		return ProviderConfigView{}, ErrProviderNotFound
	}
	return p, nil
}

func (f *fakeProviderStore) ListProviderConfigs(_ context.Context, _ bool) ([]ProviderConfigView, error) {
	out := make([]ProviderConfigView, 0, len(f.providers))
	for _, p := range f.providers {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeProviderStore) GetProviderPeakUsage(_ context.Context, _ string, _ int, _ time.Time) (int64, error) {
	return f.peak, nil
}

func (f *fakeProviderStore) ProviderHasTraffic(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func (f *fakeProviderStore) CreateProvider(_ context.Context, in ProviderWriteInput) (int64, error) {
	if _, exists := f.providers[in.Config.Name]; exists {
		return 0, ErrProviderExists
	}
	f.writes = append(f.writes, in)
	return f.seed(in.Config), nil
}

func (f *fakeProviderStore) UpdateProvider(_ context.Context, in ProviderWriteInput) (int64, error) {
	cur, ok := f.providers[in.Config.Name]
	if !ok {
		return 0, ErrProviderNotFound
	}
	// 存储层的最后一道防线。
	if in.Config.QuotaKind != cur.QuotaKind {
		return 0, ErrProviderQuotaKindImmutable
	}
	if in.ExpectedVersion != nil && *in.ExpectedVersion != cur.Version {
		return 0, ErrProviderVersionConflict
	}
	f.writes = append(f.writes, in)
	return f.seed(in.Config), nil
}

func (f *fakeProviderStore) DeleteProvider(_ context.Context, name, reason, actor string, _ *int64) (int64, error) {
	cur, ok := f.providers[name]
	if !ok {
		return 0, ErrProviderNotFound
	}
	now := time.Now()
	cur.DeletedAt = &now
	cur.Enabled = false
	f.writes = append(f.writes, ProviderWriteInput{
		Config: cur, Action: "delete", Reason: reason, Actor: actor,
	})
	return f.seed(cur), nil
}

func (f *fakeProviderStore) RollbackProvider(_ context.Context, name string, targetVersionID int64, reason, actor string, _ *int64) (int64, ProviderConfigView, error) {
	cur, ok := f.providers[name]
	if !ok {
		return 0, ProviderConfigView{}, ErrProviderNotFound
	}
	v, ok := f.versions[targetVersionID]
	if !ok || v.Snapshot == nil {
		return 0, ProviderConfigView{}, ErrProviderNotFound
	}
	if v.Snapshot.QuotaKind != cur.QuotaKind {
		return 0, ProviderConfigView{}, ErrProviderQuotaKindImmutable
	}
	next := *v.Snapshot
	next.Name = name
	f.writes = append(f.writes, ProviderWriteInput{
		Config: next, Action: "rollback", Reason: reason, Actor: actor,
	})
	return f.seed(next), next, nil
}

func (f *fakeProviderStore) ListProviderVersions(_ context.Context, name string, limit int, _ int64) ([]ProviderVersionView, error) {
	out := make([]ProviderVersionView, 0, len(f.versions))
	for _, v := range f.versions {
		if v.ProviderName == name {
			v.Snapshot = nil
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeProviderStore) GetProviderVersion(_ context.Context, id int64) (ProviderVersionView, error) {
	v, ok := f.versions[id]
	if !ok {
		return ProviderVersionView{}, ErrProviderNotFound
	}
	return v, nil
}

func (f *fakeProviderStore) DiffProviderConfigs(before, after ProviderConfigView) []ProviderFieldDiff {
	var out []ProviderFieldDiff
	if before.BaseURL != after.BaseURL {
		out = append(out, ProviderFieldDiff{Field: "base_url", Before: before.BaseURL, After: after.BaseURL})
	}
	if before.QuotaLimit != after.QuotaLimit {
		out = append(out, ProviderFieldDiff{Field: "quota_limit", Before: before.QuotaLimit, After: after.QuotaLimit})
	}
	if before.Enabled != after.Enabled {
		out = append(out, ProviderFieldDiff{Field: "enabled", Before: before.Enabled, After: after.Enabled})
	}
	return out
}

// ===== 测试环境 =====

type providerEnv struct {
	ts    *httptest.Server
	store *fakeProviderStore
}

func newProviderEnv(t *testing.T) *providerEnv {
	t.Helper()

	cfg := config.Default()
	cfg.Admin.APIKey = "admin-secret"
	cfg.Server.MaxBodyBytes = 1 << 20
	volc := cfg.Providers["volc"]
	volc.ModelMapping = map[string]string{"gpt-4o": "ep-test"}
	cfg.Providers["volc"] = volc
	if err := cfg.Validate(); err != nil {
		t.Fatalf("测试配置非法: %v", err)
	}

	ps := newFakeProviderStore()
	ps.seed(ProviderConfigView{
		Name: "volc", Enabled: true, BaseURL: "https://ark.example.com",
		QuotaKind: "token", QuotaLimit: 1_000_000,
		QuotaWindow:  int64(24 * time.Hour),
		ModelMapping: map[string]string{"gpt-4o": "ep-test"},
		AdapterKind:  "volc", CredentialEnv: "VOLC_API_KEY",
	})

	pool, err := egress.NewPool(egress.ModeDirect, nil, 30*time.Second)
	if err != nil {
		t.Fatalf("构造出口池: %v", err)
	}
	srv, err := New(Deps{
		Config: cfg, Quota: newFakeQuota(1000), Egress: pool,
		Sched: newFakeSched("volc_001"), Store: ps,
		Metrics: metrics.New(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("构造网关: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &providerEnv{ts: ts, store: ps}
}

func (e *providerEnv) do(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Authorization", "Bearer admin-secret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Actor", "ops-test")

	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return resp.StatusCode, parsed
}

// errCodeOf 取出响应里的业务错误码。
func errCodeOf(body map[string]any) string {
	e, ok := body["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := e["code"].(string)
	return code
}

// diffFieldsOf 取出响应里的字段级差异，返回 field → [before, after]。
func diffFieldsOf(body map[string]any) map[string][2]any {
	out := map[string][2]any{}
	raw, _ := body["diff"].([]any)
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		field, _ := m["field"].(string)
		out[field] = [2]any{m["before"], m["after"]}
	}
	return out
}

// ===== 守卫 1: name 不可修改 =====

func TestAdminProvider_请求体改name被拒且不落库(t *testing.T) {
	// name 进入 Redis 配额 key 前缀 {provider}:quota:... 与归档维度。
	// 改名等于把现有计数整体孤立、新名从 0 起算 —— 当日额度瞬间翻倍，
	// 且全程不报错。前端置灰不是唯一防线: 直接打 API 也必须被拒。
	env := newProviderEnv(t)
	before := len(env.store.writes)

	code, body := env.do(t, http.MethodPut, "/admin/providers/volc", `{
		"name": "volc_new",
		"base_url": "https://ark.example.com",
		"quota_limit": 1000000,
		"quota_window_nanos": 86400000000000,
		"enabled": true
	}`)

	if code != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 期望 400（team-lead 明确要求 400 而非 409）", code)
	}
	if got := errCodeOf(body); got != "name_immutable" {
		t.Errorf("错误码 = %q, 期望 name_immutable", got)
	}
	// 拒绝必须是彻底的: 留下任何一次写入都意味着版本链里多了一条
	// 本不该存在的记录，而回滚会把它当成合法目标。
	if len(env.store.writes) != before {
		t.Errorf("被拒的请求仍产生了 %d 次写入，期望 0", len(env.store.writes)-before)
	}
	if _, exists := env.store.providers["volc_new"]; exists {
		t.Error("被拒的改名请求竟创建了新 provider volc_new")
	}
}

func TestAdminProvider_请求体不带name时按路径处理(t *testing.T) {
	// 与上一条配对: 「拒绝改名」不能退化成「凡是不带 name 就拒」。
	// 脚本省略 name 是常态，若一并拒掉，等于把正常更新也堵死了 ——
	// 而只看上一条测试是发现不了的。
	env := newProviderEnv(t)

	code, _ := env.do(t, http.MethodPut, "/admin/providers/volc", `{
		"base_url": "https://ark2.example.com",
		"quota_limit": 2000000,
		"quota_window_nanos": 86400000000000,
		"enabled": true
	}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（省略 name 应视为按路径为准）", code)
	}
	if got := env.store.providers["volc"].BaseURL; got != "https://ark2.example.com" {
		t.Errorf("base_url = %q, 期望已更新", got)
	}
}

// ===== 守卫 2: quota_kind 不可修改 =====

func TestAdminProvider_改quota_kind被拒且带字段级差异(t *testing.T) {
	// quota_kind 决定 Redis key 的 {kind} 段与归档量纲。改掉它意味着
	// 整批 Key 换命名空间，而旧 key 还在 Redis 里握着今天的已用量等 TTL，
	// key_daily_history 里当天的行也已经把量纲烧进去了。
	env := newProviderEnv(t)
	before := len(env.store.writes)

	code, body := env.do(t, http.MethodPut, "/admin/providers/volc", `{
		"quota_kind": "count",
		"base_url": "https://ark.example.com",
		"quota_limit": 5000,
		"quota_window_nanos": 86400000000000,
		"enabled": true
	}`)

	if code != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 期望 400", code)
	}
	if got := errCodeOf(body); got != "quota_kind_immutable" {
		t.Errorf("错误码 = %q, 期望 quota_kind_immutable", got)
	}
	// 必须告诉运维「当前是什么、你要改成什么」。只回一句「不可修改」
	// 会让人怀疑是自己看错了字段，进而反复重试。
	diff := diffFieldsOf(body)
	d, ok := diff["quota_kind"]
	if !ok {
		t.Fatalf("响应缺少 quota_kind 的字段级差异: %v", body)
	}
	if d[0] != "token" || d[1] != "count" {
		t.Errorf("差异 = before %v / after %v, 期望 token → count", d[0], d[1])
	}
	if len(env.store.writes) != before {
		t.Errorf("被拒的请求仍产生了 %d 次写入，期望 0", len(env.store.writes)-before)
	}
	if got := env.store.providers["volc"].QuotaKind; got != "token" {
		t.Errorf("当前 quota_kind = %q, 期望仍为 token", got)
	}
}

func TestAdminProvider_quota_kind与当前一致时放行(t *testing.T) {
	// 配对用例: 拦截条件必须是「与当前不符」而不是「带了这个字段」。
	// 界面提交全量配置时一定会带上 quota_kind，若带上就拒，
	// 界面上任何一次保存都会失败。
	env := newProviderEnv(t)

	code, _ := env.do(t, http.MethodPut, "/admin/providers/volc", `{
		"quota_kind": "token",
		"base_url": "https://ark.example.com",
		"quota_limit": 3000000,
		"quota_window_nanos": 86400000000000,
		"enabled": true
	}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（量纲未变应放行）", code)
	}
	if got := env.store.providers["volc"].QuotaLimit; got != 3_000_000 {
		t.Errorf("quota_limit = %d, 期望 3000000", got)
	}
}

func TestAdminProvider_量纲不一致时不得被归一化后放过存储层(t *testing.T) {
	// 这条用例来自一次反向验证的副产物。停用 API 层拦截后，原本的
	// `next.QuotaKind = cur.QuotaKind` 无条件赋值会把不一致的量纲**抹平**
	// 成当前值再传给存储层，于是存储层那道「最后防线」收到的输入永远合规,
	// 请求被当成一次普通更新静默通过 —— 两道防线之间夹一次归一化，
	// 防线数量看着是二，实际是一。
	//
	// 断言方式: 直接检查传到存储层的入参保留了原始量纲。这样即使有人
	// 日后把 API 层拦截删掉，存储层也仍会拒，不会静默通过。
	env := newProviderEnv(t)

	code, _ := env.do(t, http.MethodPut, "/admin/providers/volc", `{
		"quota_kind": "count",
		"base_url": "https://ark.example.com",
		"quota_limit": 5000,
		"quota_window_nanos": 86400000000000,
		"enabled": true
	}`)
	if code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400", code)
	}
	for _, wr := range env.store.writes {
		if wr.Config.QuotaKind != "token" {
			continue
		}
		t.Error("不一致的 quota_kind 被归一化成当前值后传给了存储层，" +
			"存储层的最后防线因此永远看不到冲突")
	}
}

// ===== 守卫 3: 跨量纲回滚被拒 =====

func TestAdminProvider_跨量纲回滚被拒且用专用错误码(t *testing.T) {
	// 目标版本的 quota_kind 与当前不一致时，这个回滚永远不会成功。
	// 用 422 而非 409 是必需的: 409 的语义是「别人先改了，刷新重试即可」，
	// 混进 409 会让运维反复重试一个注定失败的动作。
	env := newProviderEnv(t)
	// 构造一条历史版本，其快照是按次计费 —— 即跨量纲。
	oldID := env.store.seedVersion("volc", ProviderConfigView{
		Name: "volc", Enabled: true, BaseURL: "https://ark.example.com",
		QuotaKind: "count", QuotaLimit: 5000,
		QuotaWindow: int64(24 * time.Hour),
	})
	before := len(env.store.writes)

	code, body := env.do(t, http.MethodPost, "/admin/providers/volc/rollback",
		`{"target_version_id": `+itoa(oldID)+`}`)

	if code != http.StatusUnprocessableEntity {
		t.Errorf("状态码 = %d, 期望 422（专用码，不能混进 409）", code)
	}
	if code == http.StatusConflict {
		t.Error("跨量纲回滚被归入 409，运维会把它当成可重试的版本冲突")
	}
	if got := errCodeOf(body); got != "quota_kind_immutable" {
		t.Errorf("错误码 = %q, 期望 quota_kind_immutable", got)
	}
	diff := diffFieldsOf(body)
	d, ok := diff["quota_kind"]
	if !ok {
		t.Fatalf("响应缺少 quota_kind 字段级差异: %v", body)
	}
	if d[0] != "token" || d[1] != "count" {
		t.Errorf("差异 = before %v / after %v, 期望 token → count", d[0], d[1])
	}
	// retryable=false 是给前端看的: 有了它前端才能不显示「重试」按钮。
	if v, ok := body["retryable"].(bool); !ok || v {
		t.Errorf("retryable = %v, 期望 false", body["retryable"])
	}
	if len(env.store.writes) != before {
		t.Errorf("被拒的回滚仍产生了 %d 次写入，期望 0", len(env.store.writes)-before)
	}
}

func TestAdminProvider_同量纲回滚放行且版本号继续递增(t *testing.T) {
	// 配对用例，同时验证回滚的核心语义: 生成**新版本**，而不是把
	// version 改回旧值。后者会让版本链断裂 —— 「当前生效的是哪个」
	// 与「哪些版本曾生效过」都读不出来。
	env := newProviderEnv(t)
	curVersion := env.store.providers["volc"].Version
	oldID := env.store.seedVersion("volc", ProviderConfigView{
		Name: "volc", Enabled: true, BaseURL: "https://ark-old.example.com",
		QuotaKind: "token", QuotaLimit: 500_000,
		QuotaWindow: int64(24 * time.Hour),
	})

	code, body := env.do(t, http.MethodPost, "/admin/providers/volc/rollback",
		`{"target_version_id": `+itoa(oldID)+`}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200: %v", code, body)
	}

	newVersion := env.store.providers["volc"].Version
	if newVersion <= curVersion {
		t.Errorf("回滚后版本号 = %d, 期望大于回滚前的 %d（版本号必须继续递增）",
			newVersion, curVersion)
	}
	if newVersion == oldID {
		t.Errorf("回滚把 version 改回了旧值 %d，版本链已断裂", oldID)
	}
	if got := env.store.providers["volc"].BaseURL; got != "https://ark-old.example.com" {
		t.Errorf("base_url = %q, 期望已恢复为目标版本的值", got)
	}
}

// itoa 避免为一处数字转换引入 strconv 的导入噪声。
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// ===== dry-run 与真实提交共用同一条校验路径 =====

func TestAdminProvider_dryrun与真实提交校验结论一致(t *testing.T) {
	// 两套校验等于没有 dry-run: 预演通过而提交失败会让运维彻底不再
	// 相信预演；两者都通过但放行条件不同，则会放进一份预演从未检查过的配置。
	env := newProviderEnv(t)
	// quota_limit 为 0 是必拒的（配额上限为 0 意味着该 provider 全程不可用）。
	bad := `{
		"base_url": "https://ark.example.com",
		"quota_limit": 0,
		"quota_window_nanos": 86400000000000,
		"enabled": true
	}`

	dryCode, dryBody := env.do(t, http.MethodPost, "/admin/providers/volc/dry-run", bad)
	putCode, putBody := env.do(t, http.MethodPut, "/admin/providers/volc", bad)

	// 状态码刻意不同，这不是缺陷:
	//   dry-run  → 200 + valid:false。「预演跑完了，结论是不合格」。
	//   真实提交 → 400。「这个请求我拒绝执行」。
	// 若预演也用 4xx，前端就无法区分「预演跑完发现问题」与「预演没跑起来」。
	if dryCode != http.StatusOK {
		t.Errorf("dry-run 状态码 = %d, 期望 200（校验不通过是预演的正常结论）", dryCode)
	}
	if putCode != http.StatusBadRequest {
		t.Errorf("真实提交状态码 = %d, 期望 400", putCode)
	}

	// 真正要断言的是**结论一致**: 两边给出的不合格字段与原因码必须逐项相同。
	// 预演说合格而提交被拒，运维会彻底不再相信预演；预演说不合格而提交通过，
	// 则会放进一份预演从未真正检查过的配置。
	if valid, _ := dryBody["valid"].(bool); valid {
		t.Error("dry-run 判定为合格，而真实提交被拒 —— 存在两条校验路径")
	}
	dryFails := checkCodesOf(dryBody)
	putFails := checkCodesOf(putBody)
	if len(dryFails) == 0 {
		t.Fatalf("dry-run 未给出任何不合格项: %v", dryBody)
	}
	if !sameStrings(dryFails, putFails) {
		t.Errorf("不合格项不一致: dry-run %v, 真实提交 %v —— 说明两边走了不同的校验",
			dryFails, putFails)
	}
}

// checkCodesOf 取出响应 failures 数组里的原因码。
func checkCodesOf(body map[string]any) []string {
	raw, _ := body["failures"].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		field, _ := m["field"].(string)
		code, _ := m["code"].(string)
		out = append(out, field+"/"+code)
	}
	sort.Strings(out)
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAdminProvider_dryrun不产生任何写入(t *testing.T) {
	// 预演一旦真写了库，运维就再也不敢用它了。
	env := newProviderEnv(t)
	before := len(env.store.writes)
	beforeVersion := env.store.providers["volc"].Version

	code, body := env.do(t, http.MethodPost, "/admin/providers/volc/dry-run", `{
		"base_url": "https://ark-new.example.com",
		"quota_limit": 9000000,
		"quota_window_nanos": 86400000000000,
		"enabled": true
	}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200: %v", code, body)
	}
	if len(env.store.writes) != before {
		t.Errorf("dry-run 产生了 %d 次写入，期望 0", len(env.store.writes)-before)
	}
	if got := env.store.providers["volc"].Version; got != beforeVersion {
		t.Errorf("dry-run 后版本号从 %d 变为 %d", beforeVersion, got)
	}
	// 预演必须回字段级差异，否则运维无法判断「这次改动到底动了什么」。
	diff := diffFieldsOf(body)
	if _, ok := diff["base_url"]; !ok {
		t.Errorf("dry-run 未回 base_url 差异: %v", body)
	}
}

// ===== 其余端点的基本契约 =====

func TestAdminProvider_删除是软删除(t *testing.T) {
	// 物理删会让 usage_records / key_daily_history 里那批行变成无法
	// 归因的孤儿数据，账目从此对不上。
	env := newProviderEnv(t)

	code, _ := env.do(t, http.MethodDelete, "/admin/providers/volc", "")
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", code)
	}
	p, exists := env.store.providers["volc"]
	if !exists {
		t.Fatal("provider 行被物理删除，归档数据已成孤儿")
	}
	if p.DeletedAt == nil {
		t.Error("deleted_at 未被置上，软删除未生效")
	}
	if p.Enabled {
		t.Error("软删除后 enabled 仍为 true，路由可能仍在使用它")
	}
}

func TestAdminProvider_列表不泄露凭据原文(t *testing.T) {
	// 只回布尔不回值。配置会整份进版本历史，而历史对所有持管理口令的人可查。
	env := newProviderEnv(t)

	code, _ := env.do(t, http.MethodGet, "/admin/providers", "")
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", code)
	}

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/admin/providers", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	// credential_env 是变量名，出现是对的；变量的**值**绝不能出现。
	t.Setenv("VOLC_API_KEY", "sk-should-never-appear")
	if strings.Contains(string(raw), "sk-should-never-appear") {
		t.Error("响应体含凭据原文")
	}
	if !strings.Contains(string(raw), "credential_present") {
		t.Errorf("响应缺少 credential_present 字段: %s", raw)
	}
}

func TestAdminProvider_capabilities返回受支持取值(t *testing.T) {
	// capabilities 是字面量路径段，必须优先于 GET /admin/providers/{name}
	// 命中 —— 被 {name} 吞掉时会返回 404 provider_not_found，前端表单
	// 就拿不到 name 字段的合法取值。
	//
	// 用未接 ProviderConfigStore 的普通环境: 取值集合由编译进来的适配器
	// 决定，这个端点必须在任何部署形态下都可用（不依赖配置管理）。
	env := newTestEnv(t)

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/admin/providers/capabilities", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（被 {name} 路由吞掉时会是 404/501）", resp.StatusCode)
	}
	var body struct {
		Supported []string `json:"supported_providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	want := map[string]bool{"volc": true, "sensenova": true}
	if len(body.Supported) != len(want) {
		t.Fatalf("supported_providers = %v, 期望恰好含 volc 与 sensenova", body.Supported)
	}
	for _, p := range body.Supported {
		if !want[p] {
			t.Errorf("意外的 provider 取值: %s", p)
		}
	}
	// 有序性: 前端下拉框的顺序不应随 map 遍历抖动
	if !sort.StringsAreSorted(body.Supported) {
		t.Errorf("取值应按字典序返回, got %v", body.Supported)
	}
}

func TestAdminProvider_未实现配置存储时返回501(t *testing.T) {	// 装配层若忘了接上 ProviderConfigStore，必须明确报「未实现」，
	// 而不是 panic（进程挂掉）或 404（看起来像路径写错了，会把
	// 排查方向引向路由配置）。
	env := newTestEnv(t) // 普通 fakeStore，未实现 ProviderConfigStore

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/admin/providers", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("状态码 = %d, 期望 501", resp.StatusCode)
	}
}

func TestAdminProvider_版本冲突返回409且可重试(t *testing.T) {
	// 与跨量纲拒绝形成对照: 这一类**是**可以重试的，故用 409。
	// 两者若共用一个码，运维无法区分「刷新重试」与「永远别试了」。
	env := newProviderEnv(t)
	stale := env.store.providers["volc"].Version - 1

	code, body := env.do(t, http.MethodPut, "/admin/providers/volc", `{
		"base_url": "https://ark.example.com",
		"quota_limit": 1000000,
		"quota_window_nanos": 86400000000000,
		"enabled": true,
		"expected_version": `+itoa(stale)+`
	}`)
	if code != http.StatusConflict {
		t.Errorf("状态码 = %d, 期望 409", code)
	}
	if got := errCodeOf(body); got != "version_conflict" {
		t.Errorf("错误码 = %q, 期望 version_conflict", got)
	}
	if got := errCodeOf(body); got == "quota_kind_immutable" {
		t.Error("版本冲突与跨量纲拒绝共用了错误码，两者可重试性相反")
	}
}

var _ ProviderConfigStore = (*fakeProviderStore)(nil)

var _ = errors.Is
