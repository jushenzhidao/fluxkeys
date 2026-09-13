package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/httpcore"
)

// handleProviderVersions 处理 GET /admin/providers/{name}/versions。
func (a *API) handleProviderVersions(w http.ResponseWriter, r *http.Request) {
	ps, ok := a.providerStore()
	if !ok {
		a.writeError(w, r, http.StatusNotImplemented, "not_implemented", "当前部署未启用配置管理")
		return
	}

	name := r.PathValue("name")
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			a.writeError(w, r, http.StatusBadRequest, "invalid_request", "limit 需为正整数")
			return
		}
		limit = n
	}
	var before int64
	if v := r.URL.Query().Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			a.writeError(w, r, http.StatusBadRequest, "invalid_request", "before 需为正整数版本号")
			return
		}
		before = n
	}

	versions, err := ps.ListProviderVersions(r.Context(), name, limit, before)
	if err != nil {
		a.writeProviderStoreErr(w, r, err, "读取配置版本历史失败")
		return
	}

	// next_before 给游标翻页用。不返回总数: count(*) 在历史表上会随
	// 变更次数线性变慢，而翻页只需要知道「还有没有下一页」。
	var nextBefore int64
	if len(versions) == limit {
		nextBefore = versions[len(versions)-1].ID
	}
	httpcore.WriteJSON(w, http.StatusOK, map[string]any{
		"provider_name": name,
		"versions":      versions,
		"next_before":   nextBefore,
	})
}

// handleProviderVersion 处理 GET /admin/providers/{name}/versions/{version_id}。
func (a *API) handleProviderVersion(w http.ResponseWriter, r *http.Request) {
	ps, ok := a.providerStore()
	if !ok {
		a.writeError(w, r, http.StatusNotImplemented, "not_implemented", "当前部署未启用配置管理")
		return
	}

	name := r.PathValue("name")
	id, err := strconv.ParseInt(r.PathValue("version_id"), 10, 64)
	if err != nil || id <= 0 {
		a.writeError(w, r, http.StatusBadRequest, "invalid_request", "版本号非法")
		return
	}

	v, err := ps.GetProviderVersion(r.Context(), id)
	if err != nil {
		a.writeProviderStoreErr(w, r, err, "读取配置版本失败")
		return
	}
	// 版本号全局单调，A 的版本号拿去查 B 是能查到行的。不校验会让界面
	// 在 B 的页面上展示 A 的配置（含 base_url 与凭据变量名），
	// 而运维完全看不出这份配置不属于当前 provider。
	if v.ProviderName != name {
		a.writeError(w, r, http.StatusNotFound, "version_not_found",
			"版本 "+strconv.FormatInt(id, 10)+" 不属于 provider "+name+
				"。版本号是全局单调的，跨 provider 引用不成立")
		return
	}

	httpcore.WriteJSON(w, http.StatusOK, map[string]any{"version": v})
}

// handleProviderRollback 处理 POST /admin/providers/{name}/rollback。
//
// 回滚是**向前**的操作: 以目标版本的快照创建一个新版本，版本号继续递增，
// 而不是把 provider_configs.version 改回旧值。后者会让版本链断裂 ——
// 「当前生效的是哪个」与「哪些版本曾生效过」都读不出来。
func (a *API) handleProviderRollback(w http.ResponseWriter, r *http.Request) {
	ps, ok := a.providerStore()
	if !ok {
		a.writeError(w, r, http.StatusNotImplemented, "not_implemented", "当前部署未启用配置管理")
		return
	}

	name := r.PathValue("name")
	var body struct {
		TargetVersionID int64  `json:"target_version_id"`
		Reason          string `json:"reason"`
		ExpectedVersion *int64 `json:"expected_version"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		a.writeError(w, r, http.StatusBadRequest, "invalid_request", "请求体非法: "+err.Error())
		return
	}
	if body.TargetVersionID <= 0 {
		a.writeError(w, r, http.StatusBadRequest, "invalid_request", "target_version_id 需为正整数")
		return
	}

	cur, err := ps.GetProviderConfig(r.Context(), name)
	if err != nil {
		a.writeProviderStoreErr(w, r, err, "读取 provider 配置失败")
		return
	}

	// 先取目标快照做量纲检查，好让拒绝时能给出字段级差异。
	// 直接交给存储层也会被拒（那是最后一道），但错误里只有「不可修改」
	// 这一句，运维看不到「当前 token、目标 count」这个关键事实。
	//
	// 反向验证证据: 停用下面这个 if 后，用例仍因 store 层拦截而拿到正确的
	// quota_kind_immutable 错误码，但响应里没有字段级差异 —— 这恰好就是
	// 本层拦截的独有贡献，不是冗余。
	target, err := ps.GetProviderVersion(r.Context(), body.TargetVersionID)
	if err != nil {
		a.writeProviderStoreErr(w, r, err, "读取目标配置版本失败")
		return
	}
	if target.ProviderName != name {
		a.writeError(w, r, http.StatusNotFound, "version_not_found",
			"版本 "+strconv.FormatInt(body.TargetVersionID, 10)+" 不属于 provider "+name)
		return
	}
	if target.Snapshot != nil && target.Snapshot.QuotaKind != cur.QuotaKind {
		a.writeProviderImmutableConflict(w, r, http.StatusUnprocessableEntity, cur, *target.Snapshot)
		return
	}

	version, applied, err := ps.RollbackProvider(r.Context(), name,
		body.TargetVersionID, body.Reason, adminActor(r), body.ExpectedVersion)
	if err != nil {
		a.writeProviderStoreErr(w, r, err, "回滚 provider 配置失败")
		return
	}

	diff := ps.DiffProviderConfigs(cur, applied)
	a.audit(r, "rollback_provider", name, map[string]any{
		"version":           version,
		"previous_version":  cur.Version,
		"target_version_id": body.TargetVersionID,
		"reason":            body.Reason,
		"before":            cur,
		"after":             applied,
		"diff":              diff,
	})

	reloaded, newVersion := a.reloadAfterWrite(r, "rollback_provider", name)
	httpcore.WriteJSON(w, http.StatusOK, map[string]any{
		"provider":          applied,
		"version":           version,
		"target_version_id": body.TargetVersionID,
		"diff":              diff,
		// 说清版本号递增，避免运维以为回滚后版本号会退回旧值、
		// 进而怀疑操作没生效。
		"note":           "回滚已生成新版本，版本号继续递增（未改写历史）",
		"reloaded":       reloaded,
		"active_version": newVersion,
	})
}

// handleProviderDryRun 处理 POST /admin/providers/{name}/dry-run。
//
// 预演不写库、不触发热加载，只回「若提交会怎样」: 校验结果 + 字段级差异。
// 校验与差异都走真实提交用的同一个函数（validateProviderInput /
// DiffProviderConfigs）—— 另写一套等于没有预演。
func (a *API) handleProviderDryRun(w http.ResponseWriter, r *http.Request) {
	ps, ok := a.providerStore()
	if !ok {
		a.writeError(w, r, http.StatusNotImplemented, "not_implemented", "当前部署未启用配置管理")
		return
	}

	name := r.PathValue("name")
	body, ok := a.decodeProviderBody(w, r)
	if !ok {
		return
	}

	cur, err := ps.GetProviderConfig(r.Context(), name)
	isCreate := errors.Is(err, ErrProviderNotFound)
	if err != nil && !isCreate {
		a.writeProviderStoreErr(w, r, err, "读取 provider 配置失败")
		return
	}

	next := body.ProviderConfigView
	next.Name = name

	// 禁改字段在预演里也要报，而且要报成与提交同一类问题:
	// 预演放过、提交拒绝，运维就会开始怀疑预演的价值。
	var blockers []providerCheck
	if body.Name != "" && body.Name != name {
		blockers = append(blockers, providerCheck{
			Field: "name", Code: "name_immutable", Message: msgNameImmutable,
		})
	}
	if !isCreate {
		if body.QuotaKind != "" && body.QuotaKind != cur.QuotaKind {
			blockers = append(blockers, providerCheck{
				Field: "quota_kind", Code: "quota_kind_immutable", Message: msgQuotaKindImmutable,
			})
		}
		// 只补空缺，不覆盖不一致的值 —— 与 handleProviderUpdate 同一条纪律。
		// 无条件赋值会把冲突抹平，一旦上面那段 blocker 逻辑日后被改动，
		// 预演就会在「量纲被改」这件事上静默给出合格结论。
		if next.QuotaKind == "" {
			next.QuotaKind = cur.QuotaKind
		}
	}

	failures, warnings := validateProviderInput(next, isCreate)
	failures = append(blockers, failures...)

	diff := ps.DiffProviderConfigs(cur, next)

	resp := map[string]any{
		"provider_name": name,
		"is_create":     isCreate,
		"valid":         len(failures) == 0,
		"failures":      failures,
		"warnings":      warnings,
		"diff":          diff,
		"applied":       false,
	}

	// quota_limit 下调到历史峰值以下会让那批 Key 一生效就判超额。
	// 这个提示只在预演里给 —— 它需要一次归档表聚合，不该压在提交路径上。
	if !isCreate && next.QuotaLimit < cur.QuotaLimit {
		if peak, perr := ps.GetProviderPeakUsage(r.Context(), name, 7, time.Now()); perr == nil {
			resp["peak_used_7d"] = peak
			if peak > next.QuotaLimit {
				warnings = append(warnings, providerCheck{
					Field: "quota_limit",
					Code:  "limit_below_peak",
					Message: "新 quota_limit (" + strconv.FormatInt(next.QuotaLimit, 10) +
						") 低于近 7 个配额日的单 Key 单日峰值 (" + strconv.FormatInt(peak, 10) +
						")，生效后这批 Key 当日会立刻被判超额",
				})
				resp["warnings"] = warnings
			}
		}
	}

	// 预演永远返回 200: 校验不通过是预演的正常结论而非请求错误。
	// 用 4xx 表达「预演发现问题」会让前端无法区分「预演跑完了，结果是不合格」
	// 与「预演本身没跑起来」。
	httpcore.WriteJSON(w, http.StatusOK, resp)
}

// handleReloadConfig 处理 POST /admin/reload-config。
//
// **单实例部署的前提写在这里，别删。** 当前 deploy_remote.sh 是
// systemctl restart 单进程、无副本，所以「本进程重载完成 + 新版本号」
// 就等于全局已生效，不需要实例注册表或广播。
//
// 一旦横向扩容到多实例，这个端点会变成一个**静默错误**: 它只重载了
// 接到请求的那一个实例，其余实例继续用旧配置，而响应里依然写着「已生效」。
// 那时必须补上 Redis pub/sub 广播或各实例轮询 provider_configs.version
// 二者之一，并让本响应改为汇总各实例的生效状态。
func (a *API) handleReloadConfig(w http.ResponseWriter, r *http.Request) {
	rl, ok := a.deps.Store().(ProviderReloader)
	if !ok {
		a.writeError(w, r, http.StatusNotImplemented, "not_implemented",
			"当前部署未启用配置热加载")
		return
	}

	version, err := rl.ReloadProviderConfig(r.Context())
	if err != nil {
		a.deps.Log().ErrorContext(r.Context(), "配置热加载失败", "err", err,
			"request_id", httpcore.RequestIDFromContext(r.Context()))
		// 503 而非 500: 重载失败时进程仍在用旧快照正常服务，
		// 这是「暂时没能生效，可重试」而不是「服务坏了」。
		a.writeError(w, r, http.StatusServiceUnavailable, "reload_failed",
			"配置热加载失败，进程仍在使用重载前的配置: "+err.Error())
		return
	}

	a.audit(r, "reload_config", "provider_configs", map[string]any{
		"active_version": version,
	})

	httpcore.WriteJSON(w, http.StatusOK, map[string]any{
		"reloaded":       true,
		"active_version": version,
		"scope":          "current_instance",
		"note": "已在当前实例生效。本端点只重载接到请求的实例，" +
			"当前为单实例部署故等同于全局生效；若后续扩容到多实例，" +
			"需补 Redis pub/sub 广播或各实例轮询 version",
	})
}

// reloadAfterWrite 在配置写入成功后触发热加载。
//
// 失败只记录不影响写操作的返回码: 配置已经落库了，此时返回 5xx 会让运维
// 以为改动没保存而重复提交，反而制造一串重复版本。但 reloaded=false 必须
// 出现在响应里 —— 「已落库、进程仍用旧配置」这个中间态如果不说，
// 运维会以为改动已经在生效，然后花很长时间排查一个「配置明明改了没用」的问题。
func (a *API) reloadAfterWrite(r *http.Request, action, target string) (bool, int64) {
	rl, ok := a.deps.Store().(ProviderReloader)
	if !ok {
		return false, 0
	}
	version, err := rl.ReloadProviderConfig(r.Context())
	if err != nil {
		a.deps.Log().ErrorContext(r.Context(), "配置写入后热加载失败，进程仍在使用旧配置",
			"action", action, "target", target, "err", err,
			"request_id", httpcore.RequestIDFromContext(r.Context()))
		return false, 0
	}
	return true, version
}
