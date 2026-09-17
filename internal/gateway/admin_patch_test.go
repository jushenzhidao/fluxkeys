package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/adapter"
	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/metrics"
)

// 本文件覆盖 PATCH /admin/keys/{key_id}。
//
// 测试的重点不是「happy path 能通」，而是三类会静默出错的行为:
//
//  1. 路由注册冲突 —— 这是注册期 panic，即进程起不来。推理证明不冲突不能
//     替代断言，故有专门一条注册期回归测试。
//  2. 字段缺席被当成空值 —— 与 EXCLUDED 被 VALUES 兜底污染同型的缺陷，
//     表现是静默改写不该改的字段，编译和 happy path 测试都查不出。
//  3. 写库与内存状态的同步顺序 —— 顺序反了会留下「内存已封禁但库里仍
//     active」，而下一次 Reload 会让封禁悄悄失效。

// patchEnv 构造一个预置了 Key 元数据的测试环境。
func patchEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t)
	env.store.setKeyMeta("volc_001", "active", "hot", "p_day")
	env.store.setKeyMeta("volc_banned", "banned", "cold", "p_night")
	env.store.setKeyMeta("volc_invalid", "invalid", "cold", "")
	return env
}

// doPatch 发一次 PATCH 请求，返回状态码与已解析的响应体。
func doPatch(t *testing.T, env *testEnv, keyID, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch,
		env.ts.URL+"/admin/keys/"+keyID, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer admin-secret")
	req.Header.Set("Content-Type", "application/json")

	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return resp.StatusCode, parsed
}

// ===== 路由注册（panic 级故障）=====

func TestAdminPatch_路由注册不与既有子树冲突(t *testing.T) {
	// /admin/keys/ 已作为子树注册，新增 PATCH /admin/keys/{key_id} 若被
	// ServeMux 判定为冲突就会在 New() 里 panic —— 进程根本起不来。
	// 架构师用移植的冲突判定算法验证过不冲突，但那是推理；这条测试是断言。
	cfg := config.Default()
	cfg.Admin.APIKey = "admin-secret"
	volc := cfg.Providers["volc"]
	volc.ModelMapping = map[string]string{"gpt-4o": "ep-test"}
	cfg.Providers["volc"] = volc
	if err := cfg.Validate(); err != nil {
		t.Fatalf("测试配置非法: %v", err)
	}

	pool, err := egress.NewPool(egress.ModeDirect, nil, 30*time.Second)
	if err != nil {
		t.Fatalf("构造出口池: %v", err)
	}

	defer func() {
		if v := recover(); v != nil {
			t.Fatalf("管理路由注册期 panic（进程将无法启动）: %v", v)
		}
	}()

	if _, err := New(Deps{
		Config: cfg, Quota: newFakeQuota(1000), Egress: pool,
		Sched: newFakeSched("volc_001"), Store: newFakeStore(),
		Metrics: metrics.New(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err != nil {
		t.Fatalf("构造网关失败: %v", err)
	}
}

func TestAdminPatch_与出口IP端点各自路由到预期handler(t *testing.T) {
	env := patchEnv(t)

	// PATCH 单段路径 → 新 handler，改状态
	if code, _ := doPatch(t, env, "volc_001", `{"status":"cooldown"}`); code != http.StatusOK {
		t.Fatalf("PATCH /admin/keys/volc_001 状态码 = %d, 期望 200", code)
	}
	if m, _ := env.store.keyMetaOf("volc_001"); m.status != "cooldown" {
		t.Errorf("PATCH 未生效，status = %q, 期望 cooldown", m.status)
	}

	// PUT 两段路径 → 仍命中既有的重绑定 handler。
	// 若新模式抢走了这条路径，这里会拿到 405 或 404 而非 200。
	req, _ := http.NewRequest(http.MethodPut,
		env.ts.URL+"/admin/keys/volc_001/ip", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("PUT /admin/keys/{id}/ip 状态码 = %d, 期望 200（不应被 PATCH 模式抢走）",
			resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["switch_status"] != "completed" {
		t.Errorf("响应不是重绑定 handler 的产物: %v", body)
	}
}

func TestAdminPatch_管理密钥为空时端点不存在(t *testing.T) {
	// 沿用「APIKey 为空则整组管理路由不注册」的约定，新端点必须自动继承
	// 这一保护，而不是靠中间件返回 403。
	env := newTestEnv(t, func(c *config.Config) { c.Admin.APIKey = "" })
	code, _ := doPatch(t, env, "volc_001", `{"status":"banned"}`)
	if code != http.StatusNotFound {
		t.Errorf("管理密钥为空时 PATCH 状态码 = %d, 期望 404（路由不应注册）", code)
	}
}

func TestAdminPatch_错误管理密钥返回401(t *testing.T) {
	env := patchEnv(t)
	req, _ := http.NewRequest(http.MethodPatch,
		env.ts.URL+"/admin/keys/volc_001", strings.NewReader(`{"status":"banned"}`))
	req.Header.Set("Authorization", "Bearer wrong-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("状态码 = %d, 期望 401", resp.StatusCode)
	}
	if m, _ := env.store.keyMetaOf("volc_001"); m.status != "active" {
		t.Errorf("鉴权失败却改动了数据，status = %q", m.status)
	}
}

// ===== 字段缺席与空值的区分（EXCLUDED 污染同型缺陷）=====

func TestAdminPatch_status缺席时不改动原状态(t *testing.T) {
	// 这是本端点最容易写错的一条: 若用 string 而非 *string，
	// 「请求里没有 status」与「status 为空串」不可区分，服务端会把
	// 原状态改写成空串 —— 而空串不在任何合法枚举内，该 Key 会从
	// 调度器视角彻底消失，且没有任何报错。
	env := patchEnv(t)

	code, body := doPatch(t, env, "volc_001", `{"pool":"cold"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200: %v", code, body)
	}

	m, _ := env.store.keyMetaOf("volc_001")
	if m.status != "active" {
		t.Errorf("status 缺席却被改动，现为 %q, 期望仍是 active", m.status)
	}
	if m.persona != "p_day" {
		t.Errorf("persona_id 缺席却被改动，现为 %q, 期望仍是 p_day", m.persona)
	}
	if m.pool != "cold" {
		t.Errorf("pool 未生效，现为 %q", m.pool)
	}

	// changed 只应包含真正变了的字段
	changed := toStringSlice(body["changed"])
	if !slices.Equal(changed, []string{"pool"}) {
		t.Errorf("changed = %v, 期望 [pool]", changed)
	}
}

func TestAdminPatch_显式空串与字段缺席行为不同(t *testing.T) {
	// persona_id 显式传空串是合法的「清空画像」意图，必须真的生效 ——
	// 否则运维无法取消一个 Key 的画像绑定。
	// 这条与上一条互为对照: 同一个字段，缺席则保留、显式空串则清空。
	env := patchEnv(t)

	code, _ := doPatch(t, env, "volc_001", `{"persona_id":""}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", code)
	}
	m, _ := env.store.keyMetaOf("volc_001")
	if m.persona != "" {
		t.Errorf("显式传空 persona_id 未生效，现为 %q", m.persona)
	}
	if m.status != "active" || m.pool != "hot" {
		t.Errorf("其他字段被误改: status=%q pool=%q", m.status, m.pool)
	}
}

func TestAdminPatch_无可变更字段返回400(t *testing.T) {
	// 三个可改字段全部缺席时不能返回 200: 否则调用方无法区分
	// 「改了」与「什么都没改」。
	env := patchEnv(t)
	for _, body := range []string{`{}`, `{"reason":"只写了原因"}`,
		`{"expected_status":"active"}`} {
		code, resp := doPatch(t, env, "volc_001", body)
		if code != http.StatusBadRequest {
			t.Errorf("请求体 %s 状态码 = %d, 期望 400", body, code)
		}
		if msg := errMessage(resp); !strings.Contains(msg, "没有需要变更的字段") {
			t.Errorf("请求体 %s 的文案未说明原因: %q", body, msg)
		}
	}
}

func TestAdminPatch_被排除的字段应显式报错而非静默忽略(t *testing.T) {
	// egress_ip 与 health_score 刻意不在本端点内。放过它们会让调用方
	// 以为改动生效，而服务端完全忽略 —— 静默无效比明确报错糟得多。
	// egress_ip 尤其危险: 运维会以为出口已切换，而连接仍走旧 IP。
	env := patchEnv(t)
	for _, body := range []string{
		`{"egress_ip":"10.0.0.9"}`,
		`{"status":"banned","health_score":50}`,
	} {
		code, _ := doPatch(t, env, "volc_001", body)
		if code != http.StatusBadRequest {
			t.Errorf("请求体 %s 状态码 = %d, 期望 400（未知字段应报错）", body, code)
		}
	}
	if m, _ := env.store.keyMetaOf("volc_001"); m.status != "active" {
		t.Errorf("被拒的请求改动了数据，status = %q", m.status)
	}
}

func TestAdminPatch_枚举值非法返回400(t *testing.T) {
	env := patchEnv(t)
	cases := map[string]string{
		`{"status":"disabled"}`:                         "status",
		`{"pool":"lukewarm"}`:                           "pool",
		`{"status":""}`:                                 "status",
		`{"status":"active","expected_status":"actve"}`: "expected_status",
	}
	for body, field := range cases {
		code, resp := doPatch(t, env, "volc_001", body)
		if code != http.StatusBadRequest {
			t.Errorf("请求体 %s 状态码 = %d, 期望 400", body, code)
		}
		if msg := errMessage(resp); !strings.Contains(msg, field) {
			t.Errorf("请求体 %s 的文案未指明是哪个字段非法: %q", body, msg)
		}
	}
}

// ===== 状态机约束 =====

func TestAdminPatch_终态转active需要force(t *testing.T) {
	// banned/invalid 在调度器里是终态。invalid 由鉴权失败触发，即上游
	// 明确拒绝过该 Key；直接放回流量会立刻再次失败并向上游多贡献一次
	// 异常请求，而异常请求正是本项目最敏感的信号。
	env := patchEnv(t)

	for _, keyID := range []string{"volc_banned", "volc_invalid"} {
		before, _ := env.store.keyMetaOf(keyID)

		code, resp := doPatch(t, env, keyID, `{"status":"active"}`)
		if code != http.StatusConflict {
			t.Errorf("%s 终态转 active 未带 force，状态码 = %d, 期望 409", keyID, code)
		}
		// 文案必须给出处置动作，否则运维不知道下一步该做什么
		if msg := errMessage(resp); !strings.Contains(msg, "force") {
			t.Errorf("%s 的 409 文案未提示 force: %q", keyID, msg)
		}
		if after, _ := env.store.keyMetaOf(keyID); after.status != before.status {
			t.Errorf("%s 被拒却改动了状态: %q → %q", keyID, before.status, after.status)
		}
		// 被拒的请求不应同步内存状态
		if calls := env.sched.statusSetCalls(); len(calls) != 0 {
			t.Errorf("%s 被拒却同步了内存状态: %v", keyID, calls)
		}
	}
}

func TestAdminPatch_带force可复活终态Key(t *testing.T) {
	env := patchEnv(t)

	code, body := doPatch(t, env, "volc_banned", `{"status":"active","force":true}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200: %v", code, body)
	}
	if m, _ := env.store.keyMetaOf("volc_banned"); m.status != "active" {
		t.Errorf("force 复活未生效，status = %q", m.status)
	}

	// forced 必须单独成字段，便于日后筛出所有强制复活操作
	audit := lastAuditOf(t, env, "patch_volc_key")
	if audit.Detail["forced"] != true {
		t.Errorf("审计的 forced = %v, 期望 true", audit.Detail["forced"])
	}
}

func TestAdminPatch_force复活后触发单键刷新(t *testing.T) {
	// KI-034: 复活只改 health 不够，必须调 RefreshKey 把 Key 拉回活跃池 —
	// 活跃池只由 Reload 按「库状态 = active」重建，重启前被禁的 Key 不在池里。
	env := patchEnv(t)

	code, _ := doPatch(t, env, "volc_banned", `{"status":"active","force":true}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", code)
	}
	if calls := env.sched.refreshKeyCalls(); !slices.Equal(calls, []string{"volc_banned"}) {
		t.Errorf("RefreshKey 调用 = %v, 期望 [volc_banned]", calls)
	}
}

func TestAdminPatch_只改pool时也刷新单键归属(t *testing.T) {
	// RefreshKey 不只为复活服务: pool / persona 变更后活跃池里的元数据
	// 也要就地刷新，否则要等下一个周期 key_reload 才生效。
	env := patchEnv(t)

	code, _ := doPatch(t, env, "volc_001", `{"pool":"warm"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", code)
	}
	if calls := env.sched.refreshKeyCalls(); !slices.Equal(calls, []string{"volc_001"}) {
		t.Errorf("RefreshKey 调用 = %v, 期望 [volc_001]", calls)
	}
}

func TestAdminPatch_收紧方向无需force(t *testing.T) {
	// 隔离一个可疑 Key 必须随时可做，加确认步骤只会延误处置。
	env := patchEnv(t)

	code, body := doPatch(t, env, "volc_001", `{"status":"banned"}`)
	if code != http.StatusOK {
		t.Fatalf("active → banned 状态码 = %d, 期望 200: %v", code, body)
	}

	// 这个方向不算 forced，否则审计里筛「强制复活」会混进大量正常封禁
	audit := lastAuditOf(t, env, "patch_volc_key")
	if audit.Detail["forced"] != false {
		t.Errorf("正常封禁的 forced = %v, 期望 false", audit.Detail["forced"])
	}
}

func TestAdminPatch_终态Key仍可调整池归属(t *testing.T) {
	// 终态守卫只在「本次要把状态改成 active」时挂上。若无条件挂上，
	// 一个只改 pool 的请求也会因该 Key 当前是 banned 而被拒 ——
	// 而隔离中的 Key 恰恰是最需要调整池归属的。
	env := patchEnv(t)

	code, body := doPatch(t, env, "volc_banned", `{"pool":"warm"}`)
	if code != http.StatusOK {
		t.Fatalf("终态 Key 改 pool 状态码 = %d, 期望 200: %v", code, body)
	}
	m, _ := env.store.keyMetaOf("volc_banned")
	if m.pool != "warm" {
		t.Errorf("pool 未生效，现为 %q", m.pool)
	}
	if m.status != "banned" {
		t.Errorf("status 被误改为 %q, 期望仍是 banned", m.status)
	}
}

func TestAdminPatch_终态转cooldown不需要force(t *testing.T) {
	// 只有转 active 受限。转 cooldown 不会立刻放回流量，无需确认。
	env := patchEnv(t)
	if code, body := doPatch(t, env, "volc_banned", `{"status":"cooldown"}`); code != http.StatusOK {
		t.Fatalf("banned → cooldown 状态码 = %d, 期望 200: %v", code, body)
	}
}

// ===== 乐观并发控制 =====

func TestAdminPatch_expectedStatus不匹配返回409(t *testing.T) {
	env := patchEnv(t)

	code, resp := doPatch(t, env, "volc_001",
		`{"status":"banned","expected_status":"cooldown"}`)
	if code != http.StatusConflict {
		t.Fatalf("状态码 = %d, 期望 409", code)
	}
	// 文案必须说清当前实际值，否则运维还得再查一次
	msg := errMessage(resp)
	if !strings.Contains(msg, "active") {
		t.Errorf("409 文案未给出当前实际状态: %q", msg)
	}
	if m, _ := env.store.keyMetaOf("volc_001"); m.status != "active" {
		t.Errorf("冲突却改动了数据，status = %q", m.status)
	}
}

func TestAdminPatch_expectedStatus匹配时正常改写(t *testing.T) {
	env := patchEnv(t)
	code, body := doPatch(t, env, "volc_001",
		`{"status":"banned","expected_status":"active"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200: %v", code, body)
	}
	if m, _ := env.store.keyMetaOf("volc_001"); m.status != "banned" {
		t.Errorf("status = %q, 期望 banned", m.status)
	}
}

func TestAdminPatch_不存在的Key返回404(t *testing.T) {
	// 404 与 409 必须可区分: 两者在 UPDATE 层面都是「影响 0 行」，
	// 混在一起会让运维在「Key ID 打错」与「状态被别人改过」之间无从下手。
	env := patchEnv(t)
	code, resp := doPatch(t, env, "volc_nope", `{"status":"banned"}`)
	if code != http.StatusNotFound {
		t.Errorf("状态码 = %d, 期望 404", code)
	}
	if msg := errMessage(resp); !strings.Contains(msg, "volc_nope") {
		t.Errorf("404 文案未指出是哪个 key_id: %q", msg)
	}
}

// ===== 幂等性 =====

func TestAdminPatch_同值重放返回200且changed为空(t *testing.T) {
	// 运维重跑脚本应当安全。同时 changed 为空让调用方一眼看出这是空操作。
	env := patchEnv(t)

	if code, _ := doPatch(t, env, "volc_001", `{"pool":"cold"}`); code != http.StatusOK {
		t.Fatalf("首次改写失败")
	}
	code, body := doPatch(t, env, "volc_001", `{"pool":"cold"}`)
	if code != http.StatusOK {
		t.Fatalf("同值重放状态码 = %d, 期望 200", code)
	}
	if changed := toStringSlice(body["changed"]); len(changed) != 0 {
		t.Errorf("同值重放 changed = %v, 期望空", changed)
	}
}

func TestAdminPatch_幂等但每次都写审计(t *testing.T) {
	// 幂等不代表无副作用。「谁反复试过改这个 Key」本身是排障信息，
	// 按值去重会丢掉「反复尝试」这个信号。
	env := patchEnv(t)
	for i := 0; i < 3; i++ {
		if code, _ := doPatch(t, env, "volc_001", `{"pool":"cold"}`); code != http.StatusOK {
			t.Fatalf("第 %d 次请求失败", i+1)
		}
	}
	var n int
	for _, a := range env.store.auditRecords() {
		if a.Action == "patch_volc_key" {
			n++
		}
	}
	if n != 3 {
		t.Errorf("审计条数 = %d, 期望 3（幂等不应去重审计）", n)
	}
}

// ===== 与内存状态的同步顺序 =====

func TestAdminPatch_写库成功后才同步内存状态(t *testing.T) {
	// 顺序不可换。先改内存后写库失败会留下「内存已封禁但库里仍 active」，
	// 而下一次 Reload 会用库里的值 seed 回来，封禁悄悄失效。
	env := patchEnv(t)

	if code, _ := doPatch(t, env, "volc_001", `{"status":"banned"}`); code != http.StatusOK {
		t.Fatalf("请求失败")
	}
	calls := env.sched.statusSetCalls()
	if !slices.Equal(calls, []string{"volc_001=banned"}) {
		t.Errorf("SetKeyStatus 调用 = %v, 期望 [volc_001=banned]", calls)
	}
}

func TestAdminPatch_写库失败时绝不同步内存(t *testing.T) {
	env := patchEnv(t)
	env.store.patchErr = errAssert("数据库连接中断")

	code, _ := doPatch(t, env, "volc_001", `{"status":"banned"}`)
	if code != http.StatusInternalServerError {
		t.Errorf("状态码 = %d, 期望 500", code)
	}
	// 这是本条测试的核心断言: 写库失败后内存必须保持原状，
	// 否则内存与库的状态会永久分叉。
	if calls := env.sched.statusSetCalls(); len(calls) != 0 {
		t.Errorf("写库失败却同步了内存状态: %v", calls)
	}
}

func TestAdminPatch_只改pool时不触碰内存状态(t *testing.T) {
	// pool 当前未参与调度打分，是纯标签，没有对应的内存状态。
	// 无条件调 SetKeyStatus 会把一个只改标签的操作变成状态改写。
	env := patchEnv(t)
	if code, _ := doPatch(t, env, "volc_001", `{"pool":"warm"}`); code != http.StatusOK {
		t.Fatalf("请求失败")
	}
	if calls := env.sched.statusSetCalls(); len(calls) != 0 {
		t.Errorf("只改 pool 却同步了内存状态: %v", calls)
	}
}

// ===== 响应与审计 =====

func TestAdminPatch_响应回显三个字段的新旧值(t *testing.T) {
	// 回显旧值而非仅新值: from 是唯一能证明「改之前确实是那个值」的凭据。
	env := patchEnv(t)

	code, body := doPatch(t, env, "volc_001", `{"status":"banned","pool":"cold"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200: %v", code, body)
	}
	if body["key_id"] != "volc_001" {
		t.Errorf("key_id = %v", body["key_id"])
	}

	changed := toStringSlice(body["changed"])
	if !slices.Equal(changed, []string{"status", "pool"}) {
		t.Errorf("changed = %v, 期望 [status pool]", changed)
	}

	for field, want := range map[string][2]string{
		"status":     {"active", "banned"},
		"pool":       {"hot", "cold"},
		"persona_id": {"p_day", "p_day"}, // 未改动也要回显
	} {
		fc, ok := body[field].(map[string]any)
		if !ok {
			t.Errorf("响应缺少 %s 的新旧值", field)
			continue
		}
		if fc["from"] != want[0] || fc["to"] != want[1] {
			t.Errorf("%s = %v→%v, 期望 %s→%s", field, fc["from"], fc["to"], want[0], want[1])
		}
	}
}

func TestAdminPatch_审计记新旧值双方与操作者(t *testing.T) {
	// 只记新值的审计无法回答「这个 Key 是什么时候从 active 变成 banned 的」,
	// 而这正是审计存在的理由。
	env := patchEnv(t)

	req, _ := http.NewRequest(http.MethodPatch,
		env.ts.URL+"/admin/keys/volc_001",
		strings.NewReader(`{"status":"banned","reason":"火山侧提示异常，先隔离观察"}`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req.Header.Set("X-Admin-Actor", "ops-zhang")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()

	audit := lastAuditOf(t, env, "patch_volc_key")
	if audit.Actor != "ops-zhang" {
		t.Errorf("actor = %q, 期望 ops-zhang", audit.Actor)
	}
	if audit.Target != "volc_001" {
		t.Errorf("target = %q, 期望 volc_001", audit.Target)
	}
	if audit.Detail["status_from"] != "active" || audit.Detail["status_to"] != "banned" {
		t.Errorf("审计未记全新旧值: from=%v to=%v",
			audit.Detail["status_from"], audit.Detail["status_to"])
	}
	if audit.Detail["reason"] != "火山侧提示异常，先隔离观察" {
		t.Errorf("审计未记 reason: %v", audit.Detail["reason"])
	}
	if audit.Detail["request_id"] == nil || audit.Detail["request_id"] == "" {
		t.Error("审计未记 request_id，无法与日志关联")
	}
}

func TestAdminPatch_审计不含任何密钥字段(t *testing.T) {
	// 本端点请求体本就不含 secret，这条测试是为了在将来扩展字段时
	// 把这条约束钉住 —— 审计表被读取的门槛远低于密钥表。
	env := patchEnv(t)
	if code, _ := doPatch(t, env, "volc_001", `{"status":"banned"}`); code != http.StatusOK {
		t.Fatalf("请求失败")
	}
	audit := lastAuditOf(t, env, "patch_volc_key")
	raw, err := json.Marshal(audit.Detail)
	if err != nil {
		t.Fatalf("序列化审计 detail: %v", err)
	}
	for _, banned := range []string{"secret", "sk-", "api_key", "password"} {
		if strings.Contains(strings.ToLower(string(raw)), banned) {
			t.Errorf("审计 detail 含敏感字段 %q: %s", banned, raw)
		}
	}
}

func TestAdminPatch_请求体超限返回400(t *testing.T) {
	env := patchEnv(t)
	// 远超 1MB 上限的 reason
	body := `{"status":"banned","reason":"` + strings.Repeat("x", 2<<20) + `"}`
	if code, _ := doPatch(t, env, "volc_001", body); code != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 期望 400", code)
	}
}

// ===== 辅助 =====

// errAssert 是测试用的简单错误类型。
type errAssert string

func (e errAssert) Error() string { return string(e) }

// errMessage 从错误响应体里取出文案。
func errMessage(body map[string]any) string {
	e, ok := body["error"].(map[string]any)
	if !ok {
		return ""
	}
	msg, _ := e["message"].(string)
	return msg
}

// toStringSlice 把 JSON 解出的 []any 转成 []string。
func toStringSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

// lastAuditOf 返回指定 action 的最后一条审计。
func lastAuditOf(t *testing.T, env *testEnv, action string) auditEntry {
	t.Helper()
	records := env.store.auditRecords()
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Action == action {
			return records[i]
		}
	}
	t.Fatalf("未找到 action=%s 的审计记录", action)
	return auditEntry{}
}

// 保证 httptest 与 metrics 的导入被使用（构造环境时用到）。
var _ = httptest.NewServer
var _ = metrics.New

// ---------- 池归属变更触发出口迁移 ----------

// newPooledIPEnv 构造带 hot / cold 分层的 multi_ip 环境。
func newPooledIPEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t)
	ips := []*egress.IP{
		egress.NewPooledIP("127.0.0.2", "203.0.113.2", 50, "cold"),
		egress.NewPooledIP("127.0.0.3", "203.0.113.3", 50, "cold"),
		egress.NewPooledIP("127.0.0.11", "203.0.113.11", 10, "hot"),
		egress.NewPooledIP("127.0.0.12", "203.0.113.12", 10, "hot"),
	}
	pool, err := egress.NewPool(egress.ModeMultiIP, ips, 5*time.Second)
	if err != nil {
		t.Fatalf("构造分层出口池: %v", err)
	}
	env.srv.egress = pool
	return env
}

// 冷 Key 转热必须换出口: 留在 cold 档 IP 上等于带着「这个出口曾有大量账号
// 活动」的历史开始高频请求，分层要避免的正是这件事。
func TestPatch_池变更触发出口迁移(t *testing.T) {
	env := newPooledIPEnv(t)
	env.store.setKeyMeta("volc_001", "active", "cold", "p_01")

	// 先在 cold 档建立绑定
	oldIP, err := env.srv.egress.BindInPool("volc_001", "cold")
	if err != nil {
		t.Fatalf("预置 cold 绑定: %v", err)
	}
	if !strings.HasPrefix(oldIP, "127.0.0.") || oldIP == "127.0.0.11" || oldIP == "127.0.0.12" {
		t.Fatalf("预置绑定应在 cold 档，实际 %s", oldIP)
	}

	code, body := doPatch(t, env, "volc_001", `{"pool":"hot"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d: %v", code, body)
	}

	mig, ok := body["egress_migration"].(map[string]any)
	if !ok {
		t.Fatalf("响应未包含 egress_migration: %v", body)
	}
	if applied, _ := mig["applied"].(bool); !applied {
		t.Fatalf("迁移应已生效: %v", mig)
	}
	if from, _ := mig["ip_from"].(string); from != oldIP {
		t.Errorf("ip_from = %q, 期望 %q", from, oldIP)
	}
	to, _ := mig["ip_to"].(string)
	if to != "127.0.0.11" && to != "127.0.0.12" {
		t.Errorf("ip_to = %q, 应落在 hot 档", to)
	}
	// 内存中的绑定必须真的换了
	if got := env.srv.egress.BoundIP("volc_001"); got != to {
		t.Errorf("内存绑定 = %q, 期望 %q", got, to)
	}
	// 必须落库，否则重启后 restoreBindings 会把它换回旧出口
	stored, ok := env.store.egressIPOf("volc_001")
	if !ok {
		t.Fatal("出口迁移未落库")
	}
	if stored != to {
		t.Errorf("落库的出口 = %q, 期望 %q", stored, to)
	}
}

// 池没变时不应触发迁移 —— 无谓换出口本身就是风控敏感信号。
func TestPatch_池未变不迁移出口(t *testing.T) {
	env := newPooledIPEnv(t)
	env.store.setKeyMeta("volc_001", "active", "cold", "p_01")
	before, err := env.srv.egress.BindInPool("volc_001", "cold")
	if err != nil {
		t.Fatal(err)
	}

	// 只改 persona_id
	code, body := doPatch(t, env, "volc_001", `{"persona_id":"p_02"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d: %v", code, body)
	}
	if _, exists := body["egress_migration"]; exists {
		t.Errorf("池未变不应有 egress_migration: %v", body["egress_migration"])
	}
	if got := env.srv.egress.BoundIP("volc_001"); got != before {
		t.Errorf("出口被无谓改动: %q → %q", before, got)
	}
	if _, ok := env.store.egressIPOf("volc_001"); ok {
		t.Error("池未变不该写出口到库")
	}
}

// 提交与当前值相同的 pool 属于空操作，同样不该迁移。
func TestPatch_池值相同视为空操作(t *testing.T) {
	env := newPooledIPEnv(t)
	env.store.setKeyMeta("volc_001", "active", "cold", "p_01")
	before, _ := env.srv.egress.BindInPool("volc_001", "cold")

	code, body := doPatch(t, env, "volc_001", `{"pool":"cold"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d: %v", code, body)
	}
	if _, exists := body["egress_migration"]; exists {
		t.Error("池值未实际变化不应触发迁移")
	}
	if got := env.srv.egress.BoundIP("volc_001"); got != before {
		t.Errorf("出口被无谓改动: %q → %q", before, got)
	}
}

// 目标档位无可用 IP 时: pool 已落库，故仍返回 200，但必须如实报告迁移失败。
// 静默成功会让运维以为分层已生效，而该 Key 实际还在旧出口上。
func TestPatch_迁移失败仍返回200但标明降级(t *testing.T) {
	env := newTestEnv(t)
	// 只给 cold 档 IP，转 hot 必然失败
	ips := []*egress.IP{egress.NewPooledIP("127.0.0.2", "", 50, "cold")}
	pool, err := egress.NewPool(egress.ModeMultiIP, ips, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	env.srv.egress = pool
	env.store.setKeyMeta("volc_001", "active", "cold", "p_01")
	if _, err := pool.BindInPool("volc_001", "cold"); err != nil {
		t.Fatal(err)
	}

	code, body := doPatch(t, env, "volc_001", `{"pool":"hot"}`)
	if code != http.StatusOK {
		t.Fatalf("pool 已落库，应返回 200，实际 %d: %v", code, body)
	}
	// pool 的变更必须已生效
	if m, _ := env.store.keyMetaOf("volc_001"); m.pool != "hot" {
		t.Errorf("库中 pool = %q, 期望 hot", m.pool)
	}
	mig, ok := body["egress_migration"].(map[string]any)
	if !ok {
		t.Fatalf("响应未包含 egress_migration: %v", body)
	}
	if applied, _ := mig["applied"].(bool); applied {
		t.Error("hot 档无 IP，applied 应为 false")
	}
	if errMsg, _ := mig["error"].(string); errMsg == "" {
		t.Error("迁移失败必须给出原因")
	}
}

// 迁移成功但落库失败: 本次运行已正确，但要提示重启后可能回退。
func TestPatch_迁移成功落库失败时降级告警(t *testing.T) {
	env := newPooledIPEnv(t)
	env.store.setKeyMeta("volc_001", "active", "cold", "p_01")
	if _, err := env.srv.egress.BindInPool("volc_001", "cold"); err != nil {
		t.Fatal(err)
	}
	env.store.setEgressErr = errors.New("库连接中断")

	code, body := doPatch(t, env, "volc_001", `{"pool":"hot"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d: %v", code, body)
	}
	mig, ok := body["egress_migration"].(map[string]any)
	if !ok {
		t.Fatalf("响应未包含 egress_migration: %v", body)
	}
	// 内存迁移已生效，applied 应为 true
	if applied, _ := mig["applied"].(bool); !applied {
		t.Error("内存迁移已生效，applied 应为 true")
	}
	if errMsg, _ := mig["error"].(string); !strings.Contains(errMsg, "落库失败") {
		t.Errorf("应标明落库失败，实际 %q", errMsg)
	}
}

// direct 模式没有出口可迁移，不应产生 egress_migration 字段。
func TestPatch_direct模式不迁移(t *testing.T) {
	env := newTestEnv(t)
	env.store.setKeyMeta("volc_001", "active", "cold", "p_01")

	code, body := doPatch(t, env, "volc_001", `{"pool":"hot"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d: %v", code, body)
	}
	if _, exists := body["egress_migration"]; exists {
		t.Error("direct 模式不应有 egress_migration")
	}
}

// ---------- PUT /admin/keys/{id}/ip 的落库 ----------

func doPutIP(t *testing.T, env *testEnv, keyID string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut,
		env.ts.URL+"/admin/keys/"+keyID+"/ip", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer admin-secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// 换出口后必须落库: 否则重启时 restoreBindings 读到旧值把 Key 换回去，
// 本次操作白做，还多制造一次「老账号换 IP」。
func TestPutIP_出口变更落库(t *testing.T) {
	env := newMultiIPEnv(t)
	if _, err := env.srv.egress.Bind("volc_001"); err != nil {
		t.Fatal(err)
	}

	code, body := doPutIP(t, env, "volc_001")
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d: %v", code, body)
	}
	newIP, _ := body["new_ip"].(string)
	if newIP == "" {
		t.Fatalf("响应未给出 new_ip: %v", body)
	}
	stored, ok := env.store.egressIPOf("volc_001")
	if !ok {
		t.Fatal("出口变更未落库")
	}
	if stored != newIP {
		t.Errorf("落库的出口 = %q, 期望 %q", stored, newIP)
	}
}

// 落库失败时内存已切换，仍返回 200，但要标明可能回退。
func TestPutIP_落库失败时标明降级(t *testing.T) {
	env := newMultiIPEnv(t)
	if _, err := env.srv.egress.Bind("volc_001"); err != nil {
		t.Fatal(err)
	}
	env.store.setEgressErr = errors.New("库连接中断")

	code, body := doPutIP(t, env, "volc_001")
	if code != http.StatusOK {
		t.Fatalf("内存已切换，应返回 200，实际 %d: %v", code, body)
	}
	if st, _ := body["switch_status"].(string); st != "completed" {
		t.Errorf("switch_status = %q, 期望 completed", st)
	}
	if pe, _ := body["persist_error"].(string); pe == "" {
		t.Error("落库失败必须在响应中标明")
	}
}

// ---------- 出口健康度判定 ----------

// 每种错误分类对出口的判定都必须明确，判据是「换一个出口能否解决」。
//
// 这个映射曾经写错过: 原先是
//
//	markEgress(keyID, ue.Class != ErrClassAuth && ue.Class != ErrClassRateLimit)
//
// 注释意图是「auth/rate_limit 不记账」，但布尔 false 走的是 MarkFailure 分支，
// 实际变成「对 auth/rate_limit 记失败」；而 5xx / 网络错误传 true 走 MarkSuccess，
// 反而在给可疑出口恢复信誉 —— 信号完全反向。故此处逐类锁死。
func TestEgressVerdictFor_逐类锁死(t *testing.T) {
	cases := []struct {
		class adapter.ErrorClass
		want  egressVerdict
		why   string
	}{
		{adapter.ErrClassAuth, egressUnrelated, "Key 被封，换出口照样被拒"},
		{adapter.ErrClassRateLimit, egressUnrelated, "按 Key 限流，与出口无关"},
		{adapter.ErrClassQuota, egressUnrelated, "该 Key 额度耗尽，与出口无关"},
		{adapter.ErrClassClient, egressUnrelated, "用户请求本身有问题"},
		{adapter.ErrClassServer, egressFaulty, "上游 5xx，该出口有嫌疑"},
		{adapter.ErrClassNetwork, egressFaulty, "连接失败或超时，最直接的出口故障"},
	}
	for _, c := range cases {
		if got := egressVerdictFor(c.class); got != c.want {
			t.Errorf("%s → %v, 期望 %v（%s）", c.class, got, c.want, c.why)
		}
	}
}

// unrelated 判定必须完全不触碰出口，既不加分也不减分。
//
// 前置状态取 active 而非 suspect: 从 suspect 出发时，「不操作」与「记一次失败」
// 的结果都是 suspect（连续失败要到 3 次才进 cooldown），断言看不出区别 ——
// 删掉 markEgress 里的 unrelated 短路后测试仍会通过，是个假测试。
// 从 active 出发，任何一次误记失败都会立刻让状态变成 suspect，可观察。
func TestMarkEgress_unrelated不触碰出口(t *testing.T) {
	env := newMultiIPEnv(t)
	if _, err := env.srv.egress.Bind("volc_001"); err != nil {
		t.Fatal(err)
	}
	ip := env.srv.egress.IPFor("volc_001")
	if ip == nil {
		t.Fatal("取不到出口 IP")
	}
	if ip.State() != egress.IPActive {
		t.Fatalf("前置条件: 应为 active，实际 %s", ip.State())
	}
	repBefore := ip.Reputation()

	// 连续多次 unrelated: 若被误记为失败，3 次就会进 cooldown 并扣信誉分
	for i := 0; i < 4; i++ {
		env.srv.markEgress("volc_001", egressUnrelated)
	}

	if got := ip.State(); got != egress.IPActive {
		t.Errorf("unrelated 不应改变出口状态: active → %s", got)
	}
	if got := ip.Reputation(); got != repBefore {
		t.Errorf("unrelated 不应影响信誉分: %d → %d", repBefore, got)
	}
}

// healthy 判定应让降级过的出口恢复。
func TestMarkEgress_healthy恢复出口(t *testing.T) {
	env := newMultiIPEnv(t)
	if _, err := env.srv.egress.Bind("volc_001"); err != nil {
		t.Fatal(err)
	}
	ip := env.srv.egress.IPFor("volc_001")
	ip.MarkFailure()
	if ip.State() != egress.IPSuspect {
		t.Fatalf("前置条件: 应为 suspect，实际 %s", ip.State())
	}

	env.srv.markEgress("volc_001", egressHealthy)
	if got := ip.State(); got != egress.IPActive {
		t.Errorf("healthy 应恢复出口: suspect → %s，期望 active", got)
	}
}

// faulty 判定应降级出口。
func TestMarkEgress_faulty降级出口(t *testing.T) {
	env := newMultiIPEnv(t)
	if _, err := env.srv.egress.Bind("volc_001"); err != nil {
		t.Fatal(err)
	}
	ip := env.srv.egress.IPFor("volc_001")
	if ip.State() != egress.IPActive {
		t.Fatalf("前置条件: 应为 active，实际 %s", ip.State())
	}

	env.srv.markEgress("volc_001", egressFaulty)
	if got := ip.State(); got == egress.IPActive {
		t.Error("faulty 应降级出口状态，实际仍为 active")
	}
}
