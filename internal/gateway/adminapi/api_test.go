package adminapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fluxkeys/fluxkeys/internal/confsnap"
	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/metrics"
)

// fakeDeps 用最小实现满足 Deps。
//
// 这本身就是一条断言: 如果管理面包需要网关主体的内部结构才能接上，这里就写不出来。
// 它同时证明依赖方向是单向的 —— adminapi 不需要 import internal/gateway。
type fakeDeps struct {
	chains int
}

func (f *fakeDeps) Log() *slog.Logger         { return slog.Default() }
func (f *fakeDeps) Store() Store              { return nil }
func (f *fakeDeps) Scheduler() Scheduler      { return nil }
func (f *fakeDeps) Egress() *egress.Pool      { return nil }
func (f *fakeDeps) Metrics() *metrics.Metrics { return nil }
func (f *fakeDeps) Snaps() *confsnap.Holder   { return nil }
func (f *fakeDeps) InvalidateAuthCache()      {}
func (f *fakeDeps) CreateUser(context.Context, NewUser) (CreatedUser, error) {
	return CreatedUser{}, nil
}

// AdminChain 直接放行，但记录调用次数 —— 每条路由都必须过它。
func (f *fakeDeps) AdminChain(h http.HandlerFunc) http.Handler {
	f.chains++
	return h
}

// 路由表是管理面的对外契约。锁住它，并钉住几条「字面量段优先于通配段」的
// 优先级关系 —— 这类关系一旦被改错，症状是某个端点莫名其妙地按「把路径段
// 当成 name 查详情」来处理，而不是 404。
func TestRegister_路由表与优先级(t *testing.T) {
	mux := http.NewServeMux()
	d := &fakeDeps{}
	Register(mux, d)

	cases := []struct{ method, path, want string }{
		// Key 与用户
		{http.MethodGet, "/admin/keys", "/admin/keys"},
		{http.MethodPost, "/admin/keys", "/admin/keys"},
		{http.MethodGet, "/admin/ips", "/admin/ips"},
		{http.MethodGet, "/admin/keys/volc_001", "/admin/keys/"},
		{http.MethodPut, "/admin/keys/volc_001/ip", "/admin/keys/"},
		// {key_id} 单段通配比 /admin/keys/ 多段通配更具体，故优先
		{http.MethodPatch, "/admin/keys/volc_001", "PATCH /admin/keys/{key_id}"},
		// shard 是字面量段，比 {key_id} 更具体
		{http.MethodPost, "/admin/keys/shard", "POST /admin/keys/shard"},
		{http.MethodGet, "/admin/users", "/admin/users"},
		{http.MethodGet, "/admin/users/7/keys", "/admin/users/"},

		// provider 配置
		{http.MethodGet, "/admin/providers", "GET /admin/providers"},
		{http.MethodPost, "/admin/providers", "POST /admin/providers"},
		// capabilities 是字面量段，比 {name} 更具体 —— 不依赖注册顺序
		{http.MethodGet, "/admin/providers/capabilities", "GET /admin/providers/capabilities"},
		{http.MethodGet, "/admin/providers/volc", "GET /admin/providers/{name}"},
		{http.MethodPut, "/admin/providers/volc", "PUT /admin/providers/{name}"},
		{http.MethodDelete, "/admin/providers/volc", "DELETE /admin/providers/{name}"},
		{http.MethodGet, "/admin/providers/volc/versions", "GET /admin/providers/{name}/versions"},
		{http.MethodGet, "/admin/providers/volc/versions/3",
			"GET /admin/providers/{name}/versions/{version_id}"},
		{http.MethodPost, "/admin/providers/volc/rollback", "POST /admin/providers/{name}/rollback"},
		{http.MethodPost, "/admin/providers/volc/dry-run", "POST /admin/providers/{name}/dry-run"},
		{http.MethodPost, "/admin/reload-config", "POST /admin/reload-config"},
	}

	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		_, pattern := mux.Handler(req)
		if pattern != c.want {
			t.Errorf("%s %s 命中 %q, 期望 %q", c.method, c.path, pattern, c.want)
		}
	}

	// 每一条注册的路由都必须经过 AdminChain。
	//
	// 这条断言是安全相关的: 管理面能改 Key 状态、导入密钥、换出口 IP。
	// 漏包一层 = 该端点裸奔，而「少写了一处 a.deps.AdminChain」在 review 里
	// 极难看出来 —— 它只是少一层函数调用，没有语法差异。
	// Register 共注册 18 条路由模式（Key/用户 7 条 + provider 11 条）。
	// 上表的 20 条路径里有两条（/admin/keys/ 与 /admin/users/）各由多条路径
	// 命中同一模式，所以这里比的是「模式数」而不是「用例数」。
	const wantPatterns = 18
	if d.chains != wantPatterns {
		t.Errorf("AdminChain 被调用 %d 次，期望 %d —— 每条注册的路由都必须过它",
			d.chains, wantPatterns)
	}
}

// 未注册的路径必须真的不匹配，否则上面的优先级断言可能因 mux 兜底而假绿。
func TestRegister_未注册路径不匹配(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, &fakeDeps{})

	// 注意 /admin/keys/ 是**子树**通配（尾斜杠），更深的路径仍会命中它 ——
	// 这是刻意的（PUT /admin/keys/{id}/ip 就走它）。故这里只放真正无人认领的路径。
	for _, p := range []string{"/admin/nope", "/admin/keysx", "/admin/user", "/v1/chat/completions"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		if _, pattern := mux.Handler(req); pattern != "" {
			t.Errorf("%s 不应命中任何模式，实际命中 %q", p, pattern)
		}
	}
}

// writeError 必须带上 deps 的 logger（nil logger 也不得 panic）。
func TestWriteError_可独立使用(t *testing.T) {
	a := &API{deps: &fakeDeps{}}
	rec := httptest.NewRecorder()
	a.writeError(rec, httptest.NewRequest(http.MethodGet, "/", nil),
		http.StatusNotImplemented, "not_implemented", "当前部署未启用配置管理")

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("状态码 = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}
