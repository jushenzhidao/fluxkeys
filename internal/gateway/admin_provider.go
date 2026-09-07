package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/confsnap"
)

// provider 配置管理端点。
//
// 与 admin.go 的既有端点共用 adminChain（同一把管理密钥、同一条审计链路），
// 单独成文件是因为这组端点有三条自己的规则:
//
//  1. **name 与 quota_kind 物理禁改**。两者都进 Redis 配额 key
//     {provider}:quota:{kind}:{key_id}:{day} 与归档维度，改掉等于把现有计数
//     整体孤立、新维度从 0 起算 —— 当日额度瞬间翻倍，且没有任何一层会报错。
//     前端置灰不是防线: 直接打 API 也必须被拒。
//
//  2. **两类拒绝要用不同的码**。409 的语义是「别人先改了，刷新重试即可」，
//     而跨量纲变更永远不会成功。合并成一个码，运维会反复重试一个注定失败的
//     动作，每次都得到同一句「冲突」。
//
//  3. **dry-run 与真实提交走同一条校验路径**。两套校验等于没有 dry-run。

// providerStore 取出配置管理所需的存储能力。
//
// 类型断言而非在 Deps 里多加一个字段: 热路径的 Store 实现（含测试 fake）
// 不该被迫实现这 13 个低频管理方法。未实现时返回 501 而非 panic ——
// 配置管理不可用不该让整个网关起不来。
func (s *Server) providerStore() (ProviderConfigStore, bool) {
	ps, ok := s.store.(ProviderConfigStore)
	return ps, ok
}

// providerRoutes 注册 provider 配置管理路由。
//
// 由 routes() 在 Admin.APIKey 非空的分支里调用，与其他管理端点同生共死。
func (s *Server) providerRoutes() {
	s.mux.Handle("GET /admin/providers", s.adminChain(s.handleProviderList))
	s.mux.Handle("POST /admin/providers", s.adminChain(s.handleProviderCreate))
	// capabilities 是字面量段，比 {name} 单段通配更具体，ServeMux 会优先
	// 命中它 —— 不依赖注册顺序，但必须先于 {name} 存在，否则
	// GET /admin/providers/capabilities 会被当成 name="capabilities" 的详情查询。
	s.mux.Handle("GET /admin/providers/capabilities", s.adminChain(s.handleProviderCapabilities))
	s.mux.Handle("GET /admin/providers/{name}", s.adminChain(s.handleProviderGet))
	s.mux.Handle("PUT /admin/providers/{name}", s.adminChain(s.handleProviderUpdate))
	s.mux.Handle("DELETE /admin/providers/{name}", s.adminChain(s.handleProviderDelete))
	s.mux.Handle("GET /admin/providers/{name}/versions", s.adminChain(s.handleProviderVersions))
	s.mux.Handle("GET /admin/providers/{name}/versions/{version_id}", s.adminChain(s.handleProviderVersion))
	s.mux.Handle("POST /admin/providers/{name}/rollback", s.adminChain(s.handleProviderRollback))
	s.mux.Handle("POST /admin/providers/{name}/dry-run", s.adminChain(s.handleProviderDryRun))
	s.mux.Handle("POST /admin/reload-config", s.adminChain(s.handleReloadConfig))
}

// providerBody 是创建/更新/预演共用的请求体。
//
// 三个端点同结构是刻意的: dry-run 要预演的就是提交本身，若两者的请求体
// 形状不同，预演通过而提交失败就只是时间问题。
type providerBody struct {
	ProviderConfigView
	Reason string `json:"reason"`
	// ExpectedVersion 非 nil 时启用乐观锁。缺省即不检查 ——
	// 界面上的编辑一定带版本号，而脚本化的批量改配置往往不关心并发。
	ExpectedVersion *int64 `json:"expected_version"`
}

// decodeProviderBody 解析并限长请求体。
//
// DisallowUnknownFields 是必需的: 字段名写错（quota_window 少写 _nanos、
// refresh_hour 写成 refreshHour）在宽松解析下会静默取零值，
// 于是「我明明填了刷新点」的配置落库时刷新点是空的。
func (s *Server) decodeProviderBody(w http.ResponseWriter, r *http.Request) (providerBody, bool) {
	var body providerBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "invalid_request", "请求体非法: "+err.Error())
		return providerBody{}, false
	}
	return body, true
}

// writeProviderStoreErr 把存储层错误映射为 HTTP 响应。
//
// 映射集中在一处，避免每个 handler 各写一份 —— 分散写最常见的后果是
// 某个 handler 漏了 ErrVersionConflict 的分支，于是乐观锁失败被当成 500，
// 运维看到「服务器错误」而不是「刷新重试」。
func (s *Server) writeProviderStoreErr(w http.ResponseWriter, r *http.Request, err error, what string) {
	switch {
	case errors.Is(err, ErrProviderNotFound):
		s.writeError(w, r, http.StatusNotFound, "provider_not_found", "provider 不存在")
	case errors.Is(err, ErrProviderExists):
		s.writeError(w, r, http.StatusConflict, "provider_exists",
			"provider 名已被占用（含已停用的）。provider 名进入配额 key 与归档维度，不允许复用")
	case errors.Is(err, ErrProviderVersionConflict):
		s.writeError(w, r, http.StatusConflict, "version_conflict",
			"配置已被他人修改，请刷新后重试")
	case errors.Is(err, ErrProviderNameImmutable):
		s.writeError(w, r, http.StatusBadRequest, "name_immutable", msgNameImmutable)
	case errors.Is(err, ErrProviderQuotaKindImmutable):
		s.writeError(w, r, http.StatusUnprocessableEntity, "quota_kind_immutable", msgQuotaKindImmutable)
	default:
		s.log.ErrorContext(r.Context(), what, "err", err,
			"request_id", RequestIDFromContext(r.Context()))
		s.writeError(w, r, http.StatusInternalServerError, "internal_error", what)
	}
}

// 禁改字段的文案。
//
// 单独成常量是因为同一句话要在三处出现（PUT 拦截、存储层错误映射、
// 回滚拒绝），而运维靠文案里的「怎么办」来决定下一步 ——
// 三处说法不一致会让人以为遇到的是三个不同的问题。
const (
	msgNameImmutable = "provider 名不可修改。该名字进入 Redis 配额 key 前缀与归档维度，" +
		"改名会让现有计数被孤立、新名从 0 起算，当日额度瞬间翻倍且不报错。要改名请新建 provider 再停用旧的"
	msgQuotaKindImmutable = "quota_kind 不可修改，要换量纲请新建 provider 再停用旧的。" +
		"quota_kind 决定配额 key 的量纲段与归档维度，改掉等于整批 Key 换命名空间，" +
		"而 Redis 里旧 key 仍握着当日已用量等 TTL 过期、归档表里当日的行也已按旧量纲写死"
)

// handleProviderCapabilities 处理 GET /admin/providers/capabilities。
//
// 返回代码支持的 provider 取值集合。它与列表响应里的 supported_providers
// 同源（都来自 confsnap.SupportedProviders），单独开端点是给「不需要拉全量
// 列表、只要选项」的前端表单用的轻量入口。
//
// 不查库、不依赖 providerStore: 取值集合由编译进来的适配器决定，
// 与部署是否启用配置管理无关，因此这个端点在任何部署形态下都可用。
func (s *Server) handleProviderCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"supported_providers": confsnap.SupportedProviders(),
	})
}

// handleProviderList 处理 GET /admin/providers。
func (s *Server) handleProviderList(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.providerStore()
	if !ok {
		s.writeError(w, r, http.StatusNotImplemented, "not_implemented", "当前部署未启用配置管理")
		return
	}

	items, err := ps.ListProvidersWithUsage(r.Context(), time.Now())
	if err != nil {
		s.writeProviderStoreErr(w, r, err, "读取 provider 列表失败")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"providers": items,
		"count":     len(items),
		// 受支持的 provider 取值随列表一起带出: 选择器选项与列表数据来自
		// 同一次响应，其间不可能隔着一次热加载。与 /capabilities 同源。
		"supported_providers": confsnap.SupportedProviders(),
	})
}

// handleProviderGet 处理 GET /admin/providers/{name}。
//
// 附带近 7 个配额日的单 Key 单日峰值用量: 编辑 quota_limit 时若填到峰值
// 以下，改动一生效就会让那批 Key 当日立刻被判超额 —— 界面必须先告知这个数。
func (s *Server) handleProviderGet(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.providerStore()
	if !ok {
		s.writeError(w, r, http.StatusNotImplemented, "not_implemented", "当前部署未启用配置管理")
		return
	}

	name := r.PathValue("name")
	cur, err := ps.GetProviderConfig(r.Context(), name)
	if err != nil {
		s.writeProviderStoreErr(w, r, err, "读取 provider 配置失败")
		return
	}

	// 峰值与流量标记失败不阻断主体响应: 它们是辅助信息，
	// 让整个详情页因为一次归档表查询失败而打不开是过度耦合。
	peak, peakErr := ps.GetProviderPeakUsage(r.Context(), name, 7, time.Now())
	if peakErr != nil {
		s.log.WarnContext(r.Context(), "读取 provider 峰值用量失败", "provider", name, "err", peakErr)
	}
	hasTraffic, trafficErr := ps.ProviderHasTraffic(r.Context(), name)
	if trafficErr != nil {
		s.log.WarnContext(r.Context(), "检查 provider 流量失败", "provider", name, "err", trafficErr)
	}

	resp := map[string]any{
		"provider":    cur,
		"has_traffic": hasTraffic,
	}
	if peakErr == nil {
		resp["peak_used_7d"] = peak
		resp["peak_hint"] = "近 7 个配额日内单 Key 单日最高用量。quota_limit 是单 Key 上限，" +
			"设到此值以下会让那批 Key 一生效就判超额"
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleProviderCreate 处理 POST /admin/providers。
func (s *Server) handleProviderCreate(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.providerStore()
	if !ok {
		s.writeError(w, r, http.StatusNotImplemented, "not_implemented", "当前部署未启用配置管理")
		return
	}

	body, ok := s.decodeProviderBody(w, r)
	if !ok {
		return
	}

	failures, warnings := validateProviderInput(body.ProviderConfigView, true)
	if len(failures) > 0 {
		s.writeProviderInvalid(w, r, failures, warnings)
		return
	}

	version, err := ps.CreateProvider(r.Context(), ProviderWriteInput{
		Config: body.ProviderConfigView,
		Action: "create",
		Reason: body.Reason,
		Actor:  adminActor(r),
	})
	if err != nil {
		s.writeProviderStoreErr(w, r, err, "创建 provider 失败")
		return
	}

	s.audit(r, "create_provider", body.Name, map[string]any{
		"version": version,
		"reason":  body.Reason,
		"after":   body.ProviderConfigView,
	})

	reloaded, newVersion := s.reloadAfterWrite(r, "create_provider", body.Name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"provider":       body.ProviderConfigView,
		"version":        version,
		"warnings":       warnings,
		"reloaded":       reloaded,
		"active_version": newVersion,
	})
}

// handleProviderUpdate 处理 PUT /admin/providers/{name}。
//
// 语义是全量替换而非 PATCH: 请求体缺失的字段一律按零值处理。
// 局部合并交由前端「读当前值 → 改动 → 全量提交」完成，因为服务端做合并
// 就必须知道「哪些零值是没填、哪些是真的要清空」，而那正是 PATCH 语义
// 最常写错的地方 —— 把 count_models 清空与不改动混为一谈，
// 会让一批按次计费的模型静默退回 token 计量。
func (s *Server) handleProviderUpdate(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.providerStore()
	if !ok {
		s.writeError(w, r, http.StatusNotImplemented, "not_implemented", "当前部署未启用配置管理")
		return
	}

	name := r.PathValue("name")
	body, ok := s.decodeProviderBody(w, r)
	if !ok {
		return
	}

	cur, err := ps.GetProviderConfig(r.Context(), name)
	if err != nil {
		s.writeProviderStoreErr(w, r, err, "读取 provider 配置失败")
		return
	}

	// 路径与请求体都能表达 name，冲突时拒绝而不是二选一 ——
	// 「按路径为准」会让一个写着别人名字的请求体被静默接受。
	if body.Name != "" && body.Name != name {
		s.writeError(w, r, http.StatusBadRequest, "name_immutable", msgNameImmutable)
		return
	}
	// quota_kind 与当前不符即拒。空串视为「沿用当前」而非「改成空」:
	// 界面提交全量配置时一定带上它，而脚本省略它是常态。
	if body.QuotaKind != "" && body.QuotaKind != cur.QuotaKind {
		s.writeProviderImmutableConflict(w, r, http.StatusBadRequest, cur, body.ProviderConfigView)
		return
	}

	next := body.ProviderConfigView
	next.Name = name
	// 这里只补「请求体省略了 quota_kind」的情况。不一致的情况已在上面拒掉，
	// 走不到这里。
	//
	// 反向验证发现的坑，别改成无条件赋值: 把上面的拦截停用后，无条件赋值
	// 会把不一致的量纲**抹平**成当前值再往下传，于是存储层那道最后防线
	// 根本看不到冲突，请求被当成一次普通更新静默通过。两道防线之间夹一次
	// 归一化，等于把第二道防线的输入改成了永远合规 —— 防线数量看着是二，
	// 实际是一。
	if next.QuotaKind == "" {
		next.QuotaKind = cur.QuotaKind
	}

	failures, warnings := validateProviderInput(next, false)
	if len(failures) > 0 {
		s.writeProviderInvalid(w, r, failures, warnings)
		return
	}

	diff := ps.DiffProviderConfigs(cur, next)
	version, err := ps.UpdateProvider(r.Context(), ProviderWriteInput{
		Config:          next,
		Action:          "update",
		Reason:          body.Reason,
		Actor:           adminActor(r),
		ExpectedVersion: body.ExpectedVersion,
	})
	if err != nil {
		s.writeProviderStoreErr(w, r, err, "更新 provider 失败")
		return
	}

	// detail 同时带 before 与 diff: diff 便于快速看「改了什么」，
	// before 是回答「改之前到底是什么」的唯一凭据 —— 只存 diff 的话，
	// 一旦某个字段的 diff 计算有误，原值就永久丢失了。
	s.audit(r, "update_provider", name, map[string]any{
		"version":          version,
		"previous_version": cur.Version,
		"reason":           body.Reason,
		"before":           cur,
		"after":            next,
		"diff":             diff,
	})

	reloaded, newVersion := s.reloadAfterWrite(r, "update_provider", name)
	writeJSON(w, http.StatusOK, map[string]any{
		"provider":       next,
		"version":        version,
		"diff":           diff,
		"warnings":       warnings,
		"reloaded":       reloaded,
		"active_version": newVersion,
	})
}

// handleProviderDelete 处理 DELETE /admin/providers/{name}。
//
// 软删除。物理删会让 usage_records / key_daily_history 里那批行失去归因，
// 账目从此对不上，且 provider 名会被释放给下一个同名配置复用 ——
// 两段互不相干的用量从此混在同一个维度下。
func (s *Server) handleProviderDelete(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.providerStore()
	if !ok {
		s.writeError(w, r, http.StatusNotImplemented, "not_implemented", "当前部署未启用配置管理")
		return
	}

	name := r.PathValue("name")
	var body struct {
		Reason          string `json:"reason"`
		ExpectedVersion *int64 `json:"expected_version"`
	}
	// 允许空请求体: DELETE 带 body 本就不是普通用法，强制要求会挡住 curl。
	if r.ContentLength > 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			s.writeError(w, r, http.StatusBadRequest, "invalid_request", "请求体非法: "+err.Error())
			return
		}
	}

	cur, err := ps.GetProviderConfig(r.Context(), name)
	if err != nil {
		s.writeProviderStoreErr(w, r, err, "读取 provider 配置失败")
		return
	}

	version, err := ps.DeleteProvider(r.Context(), name, body.Reason, adminActor(r), body.ExpectedVersion)
	if err != nil {
		s.writeProviderStoreErr(w, r, err, "停用 provider 失败")
		return
	}

	s.audit(r, "delete_provider", name, map[string]any{
		"version":          version,
		"previous_version": cur.Version,
		"reason":           body.Reason,
		"before":           cur,
		"soft_delete":      true,
	})

	reloaded, newVersion := s.reloadAfterWrite(r, "delete_provider", name)
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    name,
		"version": version,
		// 明确回「软删除」而非「已删除」: 运维需要知道数据还在、
		// 名字仍被占用，否则会以为可以立刻新建一个同名 provider。
		"soft_delete":    true,
		"note":           "已停用并保留历史用量。provider 名仍被占用，如需同名重建请先回滚或改名",
		"reloaded":       reloaded,
		"active_version": newVersion,
	})
}

// writeProviderInvalid 写出校验不通过的响应。
func (s *Server) writeProviderInvalid(w http.ResponseWriter, r *http.Request, failures, warnings []providerCheck) {
	// 与 writeError 不同: 校验失败必须逐字段回，只回一句「参数非法」
	// 会让运维在十几个字段里靠猜定位。故此处直接写业务结构。
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": "provider 配置校验未通过",
			"type":    "invalid_request_error",
			"code":    "provider_invalid",
		},
		"failures": failures,
		"warnings": warnings,
	}); err != nil {
		s.log.WarnContext(r.Context(), "写出校验错误响应失败", "err", err)
	}
}

// writeProviderImmutableConflict 写出跨量纲变更被拒的响应。
//
// status 由调用方给出，两条路径刻意不同:
//
//	PUT      → 400。请求体自身就不合法（带了个不该改的字段），
//	           属于「这个请求写错了」，与参数校验失败同类。
//	rollback → 422。请求本身合法、目标版本也真实存在，是这两者的**组合**
//	           在业务上不可达。用 422 与 400 区分开，是为了让运维看出
//	           「不是我请求写错了，是这个版本真的回不去」。
//
// 两者都不用 409: 409 的语义是「别人先改了，刷新重试即可」，而跨量纲变更
// 重试一万次也不会成功，正确的补救是新建 provider。混进 409 的话，
// 运维会反复重试一个注定失败的动作，且每次都得到同一句「冲突」。
//
// 无论哪条路径都带字段级差异 —— 只说「不可修改」而不说当前值与目标值，
// 运维还得自己去翻两个版本对比才知道差在哪。
func (s *Server) writeProviderImmutableConflict(w http.ResponseWriter, r *http.Request, status int, cur, target ProviderConfigView) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msgQuotaKindImmutable,
			"type":    "invalid_request_error",
			"code":    "quota_kind_immutable",
		},
		// diff 手工构造而非走 DiffProviderConfigs: 后者刻意跳过 quota_kind
		// （正常路径下它不可能变，列出来只会误导）。而这里要说的恰好就是它。
		"diff": []ProviderFieldDiff{{
			Field:  "quota_kind",
			Before: cur.QuotaKind,
			After:  target.QuotaKind,
		}},
		"retryable": false,
		"remedy": "新建一个使用目标量纲的 provider，把 Key 迁过去，再停用当前 provider。" +
			"重试本操作不会成功",
	}); err != nil {
		s.log.WarnContext(r.Context(), "写出量纲冲突响应失败", "err", err)
	}
}
