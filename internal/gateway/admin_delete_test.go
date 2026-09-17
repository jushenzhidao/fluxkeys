package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// doDelete 向 DELETE /admin/keys/{key_id} 发请求，返回状态码与解析后的 body。
func doDelete(t *testing.T, env *testEnv, keyID string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/admin/keys/"+keyID, nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	w := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(w, req)
	var body map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析响应体: %v (raw=%s)", err, w.Body.String())
		}
	}
	return w.Code, body
}

// 删行与解绑必须成对: DELETE 成功后该 Key 的出口绑定应立即释放，
// 而不仅等 15s 一轮的背景对账（那是 KI-035 根因留下的容量假满窗口）。
func TestAdminDelete_删行与解绑成对(t *testing.T) {
	env := newPooledIPEnv(t)
	// 预置一个真正落库的 Key（fakeStore.upstreamKeys 里要有，DELETE 才能命中）。
	if _, err := env.store.UpsertUpstreamKey(context.Background(),
		NewUpstreamKey{KeyID: "volc_900", Secret: "sk-x", Pool: "cold"}); err != nil {
		t.Fatal(err)
	}
	env.store.setKeyMeta("volc_900", "active", "cold", "p_01")

	// 先在 cold 档建立出口绑定
	ip, err := env.srv.egress.BindInPool("volc_900", "cold")
	if err != nil {
		t.Fatalf("预置绑定: %v", err)
	}
	if got := env.srv.egress.BoundIP("volc_900"); got != ip {
		t.Fatalf("预置绑定未生效: 期望 %q 实际 %q", ip, got)
	}

	before := env.sched.reloadCount()
	code, body := doDelete(t, env, "volc_900")
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d: %v", code, body)
	}

	// 1) 库行已删
	if _, ok := env.store.upstreamKey("volc_900"); ok {
		t.Error("DELETE 后库中仍有该 Key")
	}
	// 2) 出口绑定立即释放（不等背景对账）
	if got := env.srv.egress.BoundIP("volc_900"); got != "" {
		t.Errorf("出口绑定未释放: %q", got)
	}
	// 3) 调度器已重载剔除该 Key
	if n := env.sched.reloadCount(); n != before+1 {
		t.Errorf("scheduler.Reload 未被调用: before=%d after=%d", before, n)
	}
	// 4) 审计落了，且 target 是 key_id
	if a := lastAuditOf(t, env, "delete_volc_key"); a.Target != "volc_900" {
		t.Errorf("审计 target 不符: %+v", a)
	}
	// 5) 响应带回释放的出口 IP
	if rip, _ := body["released_egress_ip"].(string); rip != ip {
		t.Errorf("released_egress_ip = %v, 期望 %q", body["released_egress_ip"], ip)
	}
}

// 不存在的 Key 必须返回 404，且不得误伤任何内存态。
func TestAdminDelete_不存在的Key返回404(t *testing.T) {
	env := newPooledIPEnv(t)

	before := env.sched.reloadCount()
	code, body := doDelete(t, env, "volc_no_such")
	if code != http.StatusNotFound {
		t.Fatalf("状态码 = %d, 期望 404: %v", code, body)
	}
	// code 是自动化清理脚本区分「已删过」与「服务端故障」的判据（KI-036）:
	// 旧版此处是 500 internal_error，脚本会误判为失败并重试。
	if e, _ := body["error"].(map[string]any); e == nil || e["code"] != "key_not_found" {
		t.Errorf("404 的 error.code = %v, 期望 key_not_found", body["error"])
	}
	// 删行未命中，不应触发解绑或重载（避免对无关 Key 的副作用）。
	if n := env.sched.reloadCount(); n != before {
		t.Errorf("404 不应触发重载: before=%d after=%d", before, n)
	}
	// 审计里不应出现 delete_volc_key
	for _, a := range env.store.auditRecords() {
		if a.Action == "delete_volc_key" {
			t.Errorf("404 不应写审计: %+v", a)
		}
	}
}

// 路由注册不冲突: DELETE /admin/keys/{key_id} 与既有的 PATCH 同形、
// 都在 /admin/keys/ 子树之上优先命中，注册期不应 panic。
func TestAdminDelete_路由注册不与PATCH冲突(t *testing.T) {
	env := newTestEnv(t)
	defer env.Close()
	// newTestEnv 已调用 adminapi.Register；若路由冲突，构造期即 panic。
	// 这里再补一发真实请求，确认 DELETE 落到专属 handler 而非 /ip 的 404 文案。
	code, _ := doDelete(t, env, "volc_001")
	if code == http.StatusMethodNotAllowed {
		t.Errorf("DELETE 被误判为方法错误，可能路由到了 /admin/keys/ 子树: %d", code)
	}
}

// DELETE 在 direct 模式下也应成功删行（Release 是空操作，但不应报错）。
func TestAdminDelete_direct模式仍可删行(t *testing.T) {
	env := newTestEnv(t) // 默认 ModeDirect
	if _, err := env.store.UpsertUpstreamKey(context.Background(),
		NewUpstreamKey{KeyID: "volc_800", Secret: "sk-y", Pool: "cold"}); err != nil {
		t.Fatal(err)
	}
	code, body := doDelete(t, env, "volc_800")
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200: %v", code, body)
	}
	if _, ok := env.store.upstreamKey("volc_800"); ok {
		t.Error("direct 模式下库行未被删除")
	}
}
