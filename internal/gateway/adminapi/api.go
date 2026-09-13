package adminapi

import (
	"net/http"

	"github.com/fluxkeys/fluxkeys/internal/httpcore"
)

// API 是管理面 HTTP 处理器的宿主。
//
// 所有依赖都经 Deps 取得，本结构自身不持有任何配置或存储对象 —— 这样它
// 无法在两次调用之间偷偷缓存状态，也就不会出现「配置热切了但管理面还按
// 旧值校验」这类不一致。
type API struct {
	deps Deps
	mux  *http.ServeMux
}

// Register 把全部管理端点注册到 mux 上。
//
// 调用方负责「APIKey 未配置就完全不要调用本函数」这条规则 —— 不注册比
// 注册后靠中间件拒绝更好: 一旦鉴权中间件因重构失效，管理接口就会裸奔，
// 而不注册则连路径都不存在。该判断留在装配层是因为它读的是冷配置
// （改动本就要重启进程），不属于管理面的运行时关切。
func Register(mux *http.ServeMux, d Deps) {
	a := &API{deps: d, mux: mux}

	mux.Handle("/admin/keys", d.AdminChain(a.handleAdminKeys))
	mux.Handle("/admin/ips", d.AdminChain(a.handleAdminIPs))
	mux.Handle("/admin/keys/", d.AdminChain(a.handleAdminKeyByID))

	// 与上一行共存且优先。
	//
	// /admin/keys/ 的尾斜杠等价于匿名多段通配，而 {key_id} 是单段通配，
	// 后者匹配的是前者的严格子集，故 ServeMux 判定新模式更具体、优先命中，
	// 不会 panic。两者的分工是: PATCH /admin/keys/volc_001 命中这里，
	// PUT /admin/keys/volc_001/ip 是两段路径、单段通配匹配不到，仍落到
	// handleAdminKeyByID。
	//
	// 优先级由模式具体性决定而非注册顺序，紧挨着写只是为了阅读时能看到
	// 两者的关系。路由冲突是注册期 panic（进程直接起不来），故另有一条
	// 回归测试断言 New() 不 panic —— 推理正确不能替代对 panic 级故障的断言。
	mux.Handle("PATCH /admin/keys/{key_id}", d.AdminChain(a.handleAdminKeyPatch))

	// shard 是字面量段，比 {key_id} 单段通配更具体，优先命中 ——
	// 不依赖注册顺序。批量指派 Key 的机器归属，见 admin_shard.go。
	mux.Handle("POST /admin/keys/shard", d.AdminChain(a.handleAdminKeyShard))

	mux.Handle("/admin/users", d.AdminChain(a.handleAdminUsers))
	mux.Handle("/admin/users/", d.AdminChain(a.handleAdminUserByID))

	// provider 配置管理，见 admin_provider.go。
	a.providerRoutes()
}

// writeError 写出统一格式的错误响应。
//
// 本包内 65 处调用点都走这里，而不是各自取 logger 再调 httpcore ——
// 错误响应的形状与日志字段只应有一处决定。
func (a *API) writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	httpcore.WriteError(w, r, a.deps.Log(), status, code, msg)
}
