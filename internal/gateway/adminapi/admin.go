package adminapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/fluxkeys/fluxkeys/internal/httpcore"
)

// 管理接口。
//
// 两条硬性规则:
//
//  1. cfg.Admin.APIKey 为空时路由完全不注册（见 api.go 的 Register）。管理接口
//     能改 Key 绑定、建用户、发密钥，裸奔的后果等同于系统被接管。
//
//  2. 所有写操作都写审计日志。审计的目的不是事后追责，而是当 Key 出现异常
//     封禁时能回答「这个 Key 的出口 IP 是什么时候被谁改的」—— 没有这条
//     记录，多 IP 环境下的封禁根因几乎无法定位。

// adminActor 从请求中提取操作者标识。
//
// 管理密钥是共享的，无法区分具体人员，故支持通过 X-Admin-Actor 头自报身份。
// 未提供时记为 admin，至少保留「经由管理接口」这个事实。
func adminActor(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Admin-Actor")); v != "" {
		return v
	}
	return "admin"
}

// audit 写审计日志。失败只告警不阻断 —— 操作已经完成，此时返回错误会让
// 调用方误以为操作失败而重试。
func (a *API) audit(r *http.Request, action, target string, detail map[string]any) {
	if err := a.deps.Store().Audit(r.Context(), adminActor(r), action, target, detail); err != nil {
		a.deps.Log().ErrorContext(r.Context(), "写入审计日志失败",
			"request_id", httpcore.RequestIDFromContext(r.Context()),
			"action", action, "target", target, "err", err)
	}
}

// handleAdminKeys 处理 GET /admin/keys 与 POST /admin/keys。
func (a *API) handleAdminKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// 继续往下走，列出 Key 状态
	case http.MethodPost:
		a.handleImportKeys(w, r)
		return
	default:
		a.writeError(w, r, http.StatusMethodNotAllowed, "invalid_request",
			"该端点只接受 GET 与 POST")
		return
	}

	states, err := a.deps.Scheduler().KeyStates(r.Context())
	if err != nil {
		a.deps.Log().ErrorContext(r.Context(), "读取 Key 状态失败", "err", err)
		a.writeError(w, r, http.StatusInternalServerError, "internal_error", "读取 Key 状态失败")
		return
	}

	byStatus := make(map[string]int, 6)
	byPool := make(map[string]int, 3)
	items := make([]map[string]any, 0, len(states))
	for _, st := range states {
		byStatus[st.Status]++
		byPool[st.Pool]++

		item := map[string]any{
			"key_id":            st.KeyID,
			"provider":          st.Provider,
			"status":            st.Status,
			"pool":              st.Pool,
			"quota_token_used":  st.TokenUsed,
			"quota_token_limit": st.TokenLimit,
			"quota_token_ratio": st.TokenRatio(),
			"quota_count_used":  st.CountUsed,
			"quota_count_limit": st.CountLimit,
			"health_score":      st.HealthScore,
			"egress_ip":         st.EgressIP,
			"persona_id":        st.PersonaID,
		}
		if !st.LastUsedAt.IsZero() {
			item["last_used"] = st.LastUsedAt
		}
		items = append(items, item)
	}

	// 顺带把分布同步到指标，省掉一个专门的后台采集任务
	a.deps.Metrics().SetKeyDistribution(byStatus, byPool)

	httpcore.WriteJSON(w, http.StatusOK, map[string]any{
		"total":     len(states),
		"active":    byStatus["active"],
		"cooldown":  byStatus["cooldown"],
		"banned":    byStatus["banned"],
		"invalid":   byStatus["invalid"],
		"by_status": byStatus,
		"by_pool":   byPool,
		"keys":      items,
	})
}

// handleAdminIPs 处理 GET /admin/ips。
func (a *API) handleAdminIPs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.writeError(w, r, http.StatusMethodNotAllowed, "invalid_request", "该端点只接受 GET")
		return
	}

	st := a.deps.Egress().Stats()

	// 同步出口指标
	byState := make(map[string]int, 4)
	reputation := make(map[string]int, len(st.PerIP))
	bound := make(map[string]int, len(st.PerIP))
	for _, ip := range st.PerIP {
		byState[string(ip.State)]++
		reputation[ip.Addr] = ip.Reputation
		bound[ip.Addr] = ip.BoundKeys
	}
	a.deps.Metrics().SetEgressStats(byState, reputation, bound)

	httpcore.WriteJSON(w, http.StatusOK, st)
}

// handleAdminKeyByID 处理 /admin/keys/ 子树下除 PATCH 单段路径以外的请求。
//
// 目前该子树有两种合法形态:
//
//	PUT   /admin/keys/{key_id}/ip  —— 本 handler 处理
//	PATCH /admin/keys/{key_id}     —— 由 handleAdminKeyPatch 处理（更具体的
//	                                  模式优先命中，不会走到这里）
//
// 单段路径落到这里只有两种情况: 方法不是 PATCH（如 GET /admin/keys/volc_001），
// 或路径是 /admin/keys/（尾斜杠，单段通配匹配不到）。两者都给出把两种形态
// 都写清的文案 —— 只提 /ip 会让用错方法的调用方以为 PATCH 端点不存在。
func (a *API) handleAdminKeyByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/keys/")
	// 只裁尾部斜杠，不裁头部。
	//
	// 用 strings.Trim 同时裁两端会把 /admin/keys//ip（key_id 为空）压成单段
	// 的 "ip"，与 /admin/keys/volc_001 无法区分，于是空 key_id 会被误判成
	// 「路径合法只是方法不对」而回 405。裁尾保留了首段为空这个信号。
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")

	if len(parts) != 2 || parts[0] == "" || parts[1] != "ip" {
		// 文案同时列出两种合法形态。
		//
		// 只提 /ip 会误导用错方法的调用方 —— 比如 PUT /admin/keys/volc_001
		// 会落到这里，此时提示里若没有 PATCH 那一行，调用方会以为改状态的
		// 端点不存在，而实际上只是方法用错了。
		//
		// 这里保持 404 而不改成 405: 单段路径能落到本 handler 说明方法不是
		// PATCH，看似该回 405，但 /admin/keys/ 是子树模式，它对任意方法、
		// 任意深度的路径都匹配，本 handler 无法可靠区分「路径存在但方法不对」
		// 与「路径本就不存在」。在信息不足的情况下回 405 是在猜。
		a.writeError(w, r, http.StatusNotFound, "invalid_request",
			"路径格式应为 PUT /admin/keys/{key_id}/ip 或 PATCH /admin/keys/{key_id}")
		return
	}
	keyID := parts[0]

	if r.Method != http.MethodPut {
		a.writeError(w, r, http.StatusMethodNotAllowed, "invalid_request", "该端点只接受 PUT")
		return
	}

	var req struct {
		EgressIP string `json:"egress_ip"`
		Mode     string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		a.writeError(w, r, http.StatusBadRequest, "invalid_request", "请求体非法: "+err.Error())
		return
	}

	oldIP := a.deps.Egress().BoundIP(keyID)

	// Rebind 会丢弃该 Key 的旧连接。这是必须的: 保留旧连接意味着后续请求
	// 仍从旧 IP 发出，绑定变更形同虚设。
	newIP, err := a.deps.Egress().Rebind(keyID)
	if err != nil {
		a.writeError(w, r, http.StatusServiceUnavailable, "service_busy",
			"重新绑定失败: "+err.Error())
		return
	}

	// 落库，否则重启时 restoreBindings 会读到旧出口并把 Key 换回去 ——
	// 本次换出口白做，还多制造一次「老账号换 IP」。
	// 失败只降级告警: 内存中的变更已生效，本次运行是正确的。
	var persistErr string
	if newIP != "" {
		if err := a.deps.Store().SetVolcKeyEgressIP(r.Context(), keyID, newIP); err != nil {
			persistErr = err.Error()
			a.deps.Log().WarnContext(r.Context(), "出口变更已生效但落库失败，重启后可能回退",
				"key_id", keyID, "new_ip", newIP, "err", err)
		}
	}

	detail := map[string]any{
		"old_ip":       oldIP,
		"new_ip":       newIP,
		"requested_ip": req.EgressIP,
		"mode":         req.Mode,
		"request_id":   httpcore.RequestIDFromContext(r.Context()),
	}
	if persistErr != "" {
		detail["persist_error"] = persistErr
	}
	a.audit(r, "rebind_key_ip", keyID, detail)

	a.deps.Log().InfoContext(r.Context(), "Key 出口 IP 已变更",
		"key_id", keyID, "old_ip", oldIP, "new_ip", newIP, "actor", adminActor(r))

	resp := map[string]any{
		"key_id":        keyID,
		"old_ip":        oldIP,
		"new_ip":        newIP,
		"switch_status": "completed",
	}
	if persistErr != "" {
		// 内存已切换，故仍是 completed，但要让调用方知道重启后可能回退。
		resp["persist_error"] = persistErr
	}
	httpcore.WriteJSON(w, http.StatusOK, resp)
}

// handleImportKeys 处理 POST /admin/keys，导入或更新上游 Key。
//
// 支持单个对象与数组两种请求体，方便运维用一份清单一次导入上千个 Key。
//
// 语义是 upsert: 重复导入同一份清单是安全的幂等操作。secret 留空时保留库中
// 已有密文，因此可以用同一份清单只更新 pool 等元数据而不接触密钥。
func (a *API) handleImportKeys(w http.ResponseWriter, r *http.Request) {
	// 1000 个 Key 的清单约几百 KB，给到 8MB 足够且仍能挡住异常请求。
	body := http.MaxBytesReader(w, r.Body, 8<<20)

	raw, err := io.ReadAll(body)
	if err != nil {
		a.writeError(w, r, http.StatusBadRequest, "invalid_request", "读取请求体失败: "+err.Error())
		return
	}

	var items []NewUpstreamKey
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case strings.HasPrefix(trimmed, "["):
		if err := json.Unmarshal(raw, &items); err != nil {
			a.writeError(w, r, http.StatusBadRequest, "invalid_request", "请求体非法: "+err.Error())
			return
		}
	case strings.HasPrefix(trimmed, "{"):
		var one NewUpstreamKey
		if err := json.Unmarshal(raw, &one); err != nil {
			a.writeError(w, r, http.StatusBadRequest, "invalid_request", "请求体非法: "+err.Error())
			return
		}
		items = []NewUpstreamKey{one}
	default:
		a.writeError(w, r, http.StatusBadRequest, "invalid_request",
			"请求体应为 JSON 对象或数组")
		return
	}

	if len(items) == 0 {
		a.writeError(w, r, http.StatusBadRequest, "invalid_request", "没有待导入的 Key")
		return
	}

	type failure struct {
		KeyID  string `json:"key_id"`
		Reason string `json:"reason"`
	}

	imported := make([]string, 0, len(items))
	failures := make([]failure, 0)
	seen := make(map[string]bool, len(items))
	var createdCount int

	// 整批导入共用一份快照。分两次取会出现「按 A 快照解析出 provider、
	// 按 B 快照校验它是否存在」—— 中间恰好停用了该 provider 时，前一步
	// 认领成功、后一步报不存在，同一批里相邻两个 Key 得到不同结论。
	snap := a.deps.Snaps().Current()

	for _, it := range items {
		it.KeyID = strings.TrimSpace(it.KeyID)
		// 单上游部署允许省略 provider，多上游必须显式指定，否则会静默
		// 把 Key 挂到错误的上游上。
		it.Provider = snap.Cfg.ResolveProvider(strings.TrimSpace(it.Provider))

		if it.Provider == "" {
			failures = append(failures, failure{
				KeyID:  it.KeyID,
				Reason: "provider 不能为空（配置了多个上游时必须显式指定）",
			})
			continue
		}
		if it.KeyID == "" {
			failures = append(failures, failure{Reason: "key_id 不能为空"})
			continue
		}

		// 验证 provider 是否在配置中存在
		if _, exists := snap.Cfg.Providers[it.Provider]; !exists {
			failures = append(failures, failure{
				KeyID:  it.KeyID,
				Reason: "provider '" + it.Provider + "' 未在配置中定义",
			})
			continue
		}

		// 同一批内重复的 provider+key_id 组合直接报错
		compositeKey := it.Provider + ":" + it.KeyID
		if seen[compositeKey] {
			failures = append(failures, failure{KeyID: it.KeyID, Reason: "同一批内重复出现"})
			continue
		}
		seen[compositeKey] = true

		created, err := a.deps.Store().UpsertUpstreamKey(r.Context(), it)
		if err != nil {
			// 单个失败不中断整批: 导入 1000 个 Key 时因第 3 个格式错误而
			// 全部回滚，运维只能反复试错。逐条报告让一次调用就能修完。
			a.deps.Log().ErrorContext(r.Context(), "导入 Key 失败", "key_id", it.KeyID, "err", err)
			failures = append(failures, failure{KeyID: it.KeyID, Reason: err.Error()})
			continue
		}
		if created {
			createdCount++
		}

		// 立刻建立出口绑定，避免等到首个请求到达时才惰性绑定 ——
		// 惰性绑定的顺序取决于请求到达顺序，重启后同一 Key 可能落到不同 IP。
		//
		// 必须带档位: Bind(PoolAny) 会把 Key 哈希到任意档位的出口上，
		// 分层就有了缺口 —— hot Key 可能落到 cold 档 IP，白白带上
		// 「这个出口曾有大量账号」的历史。
		//
		// 分配结果必须落库: 只改内存等于没有终身绑定 —— 重启后
		// restoreBindings 读到空 egress_ip，会按当时的候选集重新哈希，
		// 而候选集已随 IP 增删与封禁变化，Key 大概率换到另一个出口。
		// 对上游而言就是「这个账号换了 IP」，正是风控最敏感的信号。
		if addr, err := a.deps.Egress().BindInPool(it.KeyID, it.Pool); err != nil {
			a.deps.Log().WarnContext(r.Context(), "Key 出口绑定失败",
				"key_id", it.KeyID, "pool", it.Pool, "err", err)
		} else if addr != "" {
			// 落库失败只降级告警: Key 已导入且本次运行的绑定是正确的，
			// 此时整批报错会让运维误以为导入失败而重试。
			if err := a.deps.Store().SetVolcKeyEgressIP(r.Context(), it.KeyID, addr); err != nil {
				a.deps.Log().WarnContext(r.Context(), "出口绑定已生效但落库失败，重启后可能改绑",
					"key_id", it.KeyID, "egress_ip", addr, "err", err)
			}
		}

		imported = append(imported, it.KeyID)
	}

	// 立即重载调度器的 Key 池。
	//
	// 不重载的话，新部署导入完 Key 仍会在长达一轮 key_reload 周期（5 分钟）
	// 内对所有请求返回 503 —— 运维只能看到「导入成功但服务不可用」，
	// 会合理地怀疑导入没生效。
	//
	// 失败只告警不影响响应: Key 已经落库，后台 key_reload 最终会捡起来，
	// 此时报错会让调用方误以为导入失败而重试。
	if len(imported) > 0 {
		if err := a.deps.Scheduler().Reload(r.Context()); err != nil {
			a.deps.Log().WarnContext(r.Context(), "导入后重载 Key 池失败，将等待后台重载",
				"err", err)
		}
	}

	// 审计只记 key_id 与元数据，绝不记 secret。
	a.audit(r, "import_upstream_keys", strconv.Itoa(len(imported)), map[string]any{
		"imported":   imported,
		"created":    createdCount,
		"updated":    len(imported) - createdCount,
		"failed":     len(failures),
		"request_id": httpcore.RequestIDFromContext(r.Context()),
	})

	a.deps.Log().InfoContext(r.Context(), "上游 Key 导入完成",
		"imported", len(imported), "created", createdCount,
		"updated", len(imported)-createdCount,
		"failed", len(failures), "actor", adminActor(r))

	status := http.StatusOK
	if len(imported) == 0 {
		// 全部失败时不能返回 200: 调用方（含 CI 脚本）需要据此判断是否成功。
		status = http.StatusBadRequest
	}

	httpcore.WriteJSON(w, status, map[string]any{
		"imported_count": len(imported),
		"created_count":  createdCount,
		"updated_count":  len(imported) - createdCount,
		"imported":       imported,
		"failed_count":   len(failures),
		"failures":       failures,
	})
}

// handleAdminUsers 处理 POST /admin/users。
func (a *API) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.writeError(w, r, http.StatusMethodNotAllowed, "invalid_request", "该端点只接受 POST")
		return
	}

	var req struct {
		Name            string `json:"name"`
		Email           string `json:"email"`
		RPMLimit        int    `json:"rpm_limit"`
		TPMLimit        int64  `json:"tpm_limit"`
		DailyTokenLimit int64  `json:"daily_token_limit"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		a.writeError(w, r, http.StatusBadRequest, "invalid_request", "请求体非法: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		a.writeError(w, r, http.StatusBadRequest, "invalid_request", "name 不能为空")
		return
	}

	uc, err := a.deps.CreateUser(r.Context(), NewUser{
		Name:            req.Name,
		Email:           req.Email,
		RPMLimit:        req.RPMLimit,
		TPMLimit:        req.TPMLimit,
		DailyTokenLimit: req.DailyTokenLimit,
	})
	if err != nil {
		a.deps.Log().ErrorContext(r.Context(), "创建用户失败", "err", err)
		a.writeError(w, r, http.StatusInternalServerError, "internal_error", "创建用户失败: "+err.Error())
		return
	}

	// 审计与响应都用存储层回填后的值: 用户没传限额时存储层会填默认值，
	// 回显请求值会让调用方以为「不限」而实际上有默认上限。
	a.audit(r, "create_user", strconv.FormatInt(uc.UserID, 10), map[string]any{
		"name": uc.Name, "email": req.Email,
		"rpm_limit": uc.RPMLimit, "tpm_limit": uc.TPMLimit,
		"daily_token_limit": uc.DailyTokenLimit,
		"request_id":        httpcore.RequestIDFromContext(r.Context()),
	})

	httpcore.WriteJSON(w, http.StatusCreated, map[string]any{
		"id": uc.UserID, "name": uc.Name, "email": req.Email,
		"rpm_limit": uc.RPMLimit, "tpm_limit": uc.TPMLimit,
		"daily_token_limit": uc.DailyTokenLimit,
	})
}

// handleAdminUserByID 处理 /admin/users/{id}/keys 与 /admin/users/{id}/keys/{key_id}。
//
//	POST   /admin/users/{id}/keys           签发新 Key
//	DELETE /admin/users/{id}/keys/{key_id}  吊销 Key（立即生效）
func (a *API) handleAdminUserByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/users/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 || parts[1] != "keys" || len(parts) > 3 {
		a.writeError(w, r, http.StatusNotFound, "invalid_request",
			"路径格式应为 /admin/users/{id}/keys 或 /admin/users/{id}/keys/{key_id}")
		return
	}
	userID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || userID <= 0 {
		a.writeError(w, r, http.StatusBadRequest, "invalid_request", "用户 ID 非法")
		return
	}

	// DELETE /admin/users/{id}/keys/{key_id}: 吊销
	if len(parts) == 3 {
		if r.Method != http.MethodDelete {
			a.writeError(w, r, http.StatusMethodNotAllowed, "invalid_request", "该端点只接受 DELETE")
			return
		}
		var keyID int64
		keyID, err = strconv.ParseInt(parts[2], 10, 64)
		if err != nil || keyID <= 0 {
			a.writeError(w, r, http.StatusBadRequest, "invalid_request", "Key ID 非法")
			return
		}
		a.revokeUserKey(w, r, userID, keyID)
		return
	}

	if r.Method != http.MethodPost {
		a.writeError(w, r, http.StatusMethodNotAllowed, "invalid_request", "该端点只接受 POST")
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	// 请求体可省略
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)

	plaintext, issued, err := a.deps.Store().CreateUserAPIKey(r.Context(), userID, req.Name)
	if err != nil {
		a.deps.Log().ErrorContext(r.Context(), "签发用户 API Key 失败", "user_id", userID, "err", err)
		a.writeError(w, r, http.StatusInternalServerError, "internal_error", "签发失败: "+err.Error())
		return
	}

	// 审计只记前缀，绝不记明文 —— 审计表被读取的门槛远低于密钥表
	a.audit(r, "create_user_api_key", strconv.FormatInt(userID, 10), map[string]any{
		"api_key_id": issued.ID, "key_prefix": issued.Prefix, "name": req.Name,
		"request_id": httpcore.RequestIDFromContext(r.Context()),
	})

	a.deps.Log().InfoContext(r.Context(), "已签发用户 API Key",
		"user_id", userID, "api_key_id", issued.ID, "prefix", issued.Prefix, "actor", adminActor(r))

	httpcore.WriteJSON(w, http.StatusCreated, map[string]any{
		"id":      issued.ID,
		"user_id": userID,
		"name":    req.Name,
		"prefix":  issued.Prefix,
		// 明文仅此一次返回，服务端只存哈希
		"api_key": plaintext,
		"warning": "请立即保存，该明文不会再次返回",
	})
}

// revokeUserKey 吊销一条用户 API Key 并使鉴权缓存立即失效。
func (a *API) revokeUserKey(w http.ResponseWriter, r *http.Request, userID, keyID int64) {
	if err := a.deps.Store().RevokeUserAPIKey(r.Context(), userID, keyID); err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			a.writeError(w, r, http.StatusNotFound, "key_not_found",
				"Key 不存在、不属于该用户或已非 active 状态")
			return
		}
		a.deps.Log().ErrorContext(r.Context(), "吊销用户 API Key 失败",
			"user_id", userID, "api_key_id", keyID, "err", err)
		a.writeError(w, r, http.StatusInternalServerError, "internal_error", "吊销失败: "+err.Error())
		return
	}

	// 清空鉴权缓存，把吊销的生效延迟从缓存 TTL 压到零。
	//
	// 全清而非按条目清: 这里只有 key_id，而缓存以 token 哈希为键，无法定位
	// 单条。吊销是低频管理操作，全清的代价只是一轮回源。
	a.deps.InvalidateAuthCache()

	a.audit(r, "revoke_user_api_key", strconv.FormatInt(userID, 10), map[string]any{
		"api_key_id": keyID,
		"request_id": httpcore.RequestIDFromContext(r.Context()),
	})
	a.deps.Log().InfoContext(r.Context(), "已吊销用户 API Key",
		"user_id", userID, "api_key_id", keyID, "actor", adminActor(r))

	httpcore.WriteJSON(w, http.StatusOK, map[string]any{
		"revoked": true, "user_id": userID, "api_key_id": keyID,
	})
}
