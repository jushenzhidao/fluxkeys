package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/httpcore"
	"slices"
)

// PATCH /admin/keys/{key_id}: 调整 Key 的状态、池与画像。
//
// 本端点刻意只开放三个字段，另两个被排除的原因各不相同:
//
//	egress_ip —— 出口 IP 变更走 PUT /admin/keys/{key_id}/ip。那条路径会
//	调 egress.Rebind() 丢弃该 Key 的旧连接。在此处放开 egress_ip 会造成
//	「库里 IP 改了但连接仍走旧 IP」的静默不一致，绑定变更形同虚设。
//
//	health_score —— 运行时观测值，只在单实例内存中有意义。人工改写会被
//	下一次成功/失败请求立刻覆盖，只会给运维「改了但没用」的错觉。要让一个
//	Key 恢复调度应当改 status。

// 合法枚举值。取值与 store 的状态常量、schema.sql 的注释一致。
var (
	validKeyStatuses = []string{"active", "cooldown", "banned", "invalid"}
	validKeyPools    = []string{"hot", "warm", "cold"}

	// terminalKeyStatuses 是不会自动恢复的终态。
	//
	// 调度器把这两个状态当终态处理（markSuccess 只放 cooldown 回 active，
	// available 对二者直接返回不可用）。invalid 由鉴权失败触发 —— 即上游
	// 明确拒绝了该 Key。把这样的 Key 直接放回流量，若根因未解决它会立刻
	// 再次失败并向上游多贡献一次异常请求，而异常请求正是本项目最敏感的
	// 信号。所以终态转 active 要求显式 force。
	terminalKeyStatuses = []string{"banned", "invalid"}
)

// patchKeyRequest 是 PATCH /admin/keys/{key_id} 的请求体。
//
// 三个可改字段与 expected_status 全部用 *string: nil 表示调用方没提这个
// 字段。这是本端点最容易写错的地方 —— 用 string 的话，`{"status":""}`
// 与请求体里根本没有 status 解出来完全一样，服务端会把「没提」当成
// 「改成空串」。与 UpsertUpstreamKey 踩过的 EXCLUDED 被 VALUES 兜底污染同型:
// 都是无法区分未提供与空值，后果都是静默改写不该改的字段。
type patchKeyRequest struct {
	Status    *string `json:"status"`
	Pool      *string `json:"pool"`
	PersonaID *string `json:"persona_id"`

	// ExpectedStatus 是乐观并发控制。与当前状态不符时返回 409。
	ExpectedStatus *string `json:"expected_status"`

	// Force 为 true 时允许把终态 Key 转回 active。
	Force bool `json:"force"`

	// Reason 只进审计，不进业务表。
	Reason string `json:"reason"`
}

// handleAdminKeyPatch 处理 PATCH /admin/keys/{key_id}。
func (a *API) handleAdminKeyPatch(w http.ResponseWriter, r *http.Request) {
	// 路径参数由 ServeMux 的增强模式解析，不再手工切路径。
	// 手工切的话这个 handler 就得挂在 /admin/keys/ 上，与既有的
	// handleAdminKeyByID 争夺同一个模式。
	keyID := strings.TrimSpace(r.PathValue("key_id"))
	if keyID == "" {
		a.writeError(w, r, http.StatusNotFound, "invalid_request",
			"路径格式应为 /admin/keys/{key_id}")
		return
	}

	req, ok := a.decodePatchKeyRequest(w, r)
	if !ok {
		return
	}

	patch := KeyPatch{
		Status:         req.Status,
		Pool:           req.Pool,
		PersonaID:      req.PersonaID,
		ExpectedStatus: req.ExpectedStatus,
	}
	// 状态机约束交给存储层在单条语句内判定，不在这里先查再改 ——
	// 后者在并发下两个请求会各自校验通过再互相覆盖。
	//
	// 只在「本次确实要把状态改成 active」时才挂守卫: 若无条件挂上，
	// 一个只改 pool 的请求也会因为该 Key 当前是 banned 而被拒，
	// 而隔离中的 Key 恰恰是最需要调整池归属的。
	if !req.Force && req.Status != nil && *req.Status == "active" {
		patch.RejectStatusFrom = terminalKeyStatuses
	}

	res, err := a.deps.Store().PatchUpstreamKeyState(r.Context(), keyID, patch)
	switch {
	case errors.Is(err, ErrKeyNotFound):
		a.writeError(w, r, http.StatusNotFound, "invalid_request", "Key 不存在: "+keyID)
		return
	case errors.Is(err, ErrPreconditionFailed):
		a.writeError(w, r, http.StatusConflict, "invalid_request",
			patchConflictMessage(req, res))
		return
	case err != nil:
		a.deps.Log().ErrorContext(r.Context(), "更新 Key 元数据失败",
			"request_id", httpcore.RequestIDFromContext(r.Context()),
			"key_id", keyID, "err", err)
		a.writeError(w, r, http.StatusInternalServerError, "internal_error", "更新 Key 失败")
		return
	}

	// 顺序不可换: 先写库，成功后才同步内存。
	//
	// 反过来（先改内存再写库）一旦写库失败，就会留下「内存已封禁但库里仍
	// active」的状态，而下一次 Reload 会用库里的值 seed 回来，封禁悄悄失效。
	// 而 seed 对已存在的 Key 不覆盖，所以这里必须显式 SetKeyStatus，
	// 不能指望 Reload 把新状态带进内存。
	if req.Status != nil {
		a.deps.Scheduler().SetKeyStatus(keyID, res.NewStatus)
	}

	changed := patchChangedFields(res)
	forced := req.Force && slices.Contains(terminalKeyStatuses, res.PrevStatus)

	// 池归属变了就必须换出口，否则分层形同虚设。
	migration := a.migrateEgressForPool(r, keyID, res)

	// 审计只记元数据与新旧值，绝不记 secret。
	// 本端点的请求体本就不含 secret，这条注释是为了钉住将来扩展字段时
	// 不要把密钥带进审计 —— 审计表被读取的门槛远低于密钥表。
	detail := map[string]any{
		"changed":     changed,
		"status_from": res.PrevStatus, "status_to": res.NewStatus,
		"pool_from": res.PrevPool, "pool_to": res.NewPool,
		"persona_from": res.PrevPersonaID, "persona_to": res.NewPersonaID,
		// forced 单独成字段，便于日后筛出所有「强制复活终态 Key」的操作。
		"forced":     forced,
		"reason":     req.Reason,
		"request_id": httpcore.RequestIDFromContext(r.Context()),
	}
	if req.ExpectedStatus != nil {
		detail["expected_status"] = *req.ExpectedStatus
	}
	// 出口迁移进审计: 「这个账号何时从哪个 IP 换到哪个 IP」是排查封禁时
	// 最先要查的线索，而失败的迁移更需要留痕以便事后补做。
	if migration != nil {
		m := map[string]any{
			"applied": migration.Applied,
			"ip_from": migration.FromIP,
			"ip_to":   migration.ToIP,
		}
		if migration.Error != "" {
			m["error"] = migration.Error
		}
		detail["egress_migration"] = m
	}
	a.audit(r, "patch_volc_key", keyID, detail)

	a.deps.Log().InfoContext(r.Context(), "Key 元数据已更新",
		"key_id", keyID, "changed", changed,
		"status_from", res.PrevStatus, "status_to", res.NewStatus,
		"forced", forced, "actor", adminActor(r))

	resp := map[string]any{
		"key_id":  keyID,
		"changed": changed,
		// 三个字段一律回显 from/to，即使本次没改。
		// 运维需要确认「改动是否如预期」，而 from 是唯一能证明
		// 「改之前确实是那个值」的凭据。
		"status":     map[string]string{"from": res.PrevStatus, "to": res.NewStatus},
		"pool":       map[string]string{"from": res.PrevPool, "to": res.NewPool},
		"persona_id": map[string]string{"from": res.PrevPersonaID, "to": res.NewPersonaID},
	}
	// 迁移失败时仍返回 200（pool 确实改了），但必须把降级如实告知 ——
	// 调用方据此决定是否手动补做迁移。
	if migration != nil {
		m := map[string]any{
			"applied": migration.Applied,
			"ip_from": migration.FromIP,
			"ip_to":   migration.ToIP,
		}
		if migration.Error != "" {
			m["error"] = migration.Error
		}
		resp["egress_migration"] = m
	}
	httpcore.WriteJSON(w, http.StatusOK, resp)
}

// decodePatchKeyRequest 解析并校验请求体。校验失败时已写出 400。
func (a *API) decodePatchKeyRequest(w http.ResponseWriter, r *http.Request) (patchKeyRequest, bool) {
	var req patchKeyRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	// 拒绝未知字段。放过 egress_ip / health_score 这类被刻意排除的字段会让
	// 调用方以为改动生效了，而服务端其实完全忽略 —— 静默无效比明确报错糟。
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		a.writeError(w, r, http.StatusBadRequest, "invalid_request", "请求体非法: "+err.Error())
		return req, false
	}

	// 三个可改字段全部缺席时返回 400 而非 200:
	// 否则调用方无法区分「改了」和「什么都没改」。
	if !(KeyPatch{Status: req.Status, Pool: req.Pool, PersonaID: req.PersonaID}).HasFieldUpdate() {
		a.writeError(w, r, http.StatusBadRequest, "invalid_request",
			"没有需要变更的字段，status / pool / persona_id 至少提供一个")
		return req, false
	}

	if req.Status != nil && !slices.Contains(validKeyStatuses, *req.Status) {
		a.writeError(w, r, http.StatusBadRequest, "invalid_request",
			"status 取值非法，应为 "+strings.Join(validKeyStatuses, " / "))
		return req, false
	}
	if req.Pool != nil && !slices.Contains(validKeyPools, *req.Pool) {
		a.writeError(w, r, http.StatusBadRequest, "invalid_request",
			"pool 取值非法，应为 "+strings.Join(validKeyPools, " / "))
		return req, false
	}
	// expected_status 也要校验枚举: 写错成 "actve" 时若不校验，条件永远
	// 匹配不上，运维只会看到一个无法解释的 409。
	if req.ExpectedStatus != nil && !slices.Contains(validKeyStatuses, *req.ExpectedStatus) {
		a.writeError(w, r, http.StatusBadRequest, "invalid_request",
			"expected_status 取值非法，应为 "+strings.Join(validKeyStatuses, " / "))
		return req, false
	}
	return req, true
}

// egressMigration 描述一次因池归属变更触发的出口迁移结果。
//
// Applied 为 false 且 Error 非空表示「池改了但出口没换」—— 这是必须让
// 调用方看见的降级状态，不能静默。
type egressMigration struct {
	Applied bool
	FromIP  string
	ToIP    string
	Error   string
}

// migrateEgressForPool 在 Key 的池归属变更后把它迁移到新档位的出口 IP。
//
// 为什么池变更必须换出口:
//
//	cold 档的单个 IP 可能承载上百个几乎静默的 Key，hot 档则要求低密度。
//	一个 Key 从 cold 转 hot 后若仍留在原出口，它会带着「这个 IP 上曾有
//	上百个账号活动」的历史开始高频请求 —— 分层想避免的正是这件事。
//	反方向（hot 转 cold）同理: 占着低密度 IP 的名额不释放，hot 档会提前满。
//
// 用 Migrate 而非 Rebind: 后者会给原 IP 记一次失败（语义是「原出口有问题」）。
// 池间流转是符合预期的运营动作，频繁扣分会把健康 IP 逐个推入 cooldown。
//
// 失败不回滚也不返回 5xx，而是降级并如实上报:
//
//	pool 已经写库成功了。此时若返回 500，运维会以为整个操作没生效而重试，
//	但重试时 pool 已是新值、patchChangedFields 判定无变更，于是不再触发迁移
//	—— 反而永久卡在「池改了、出口没换」且无人知晓的状态。返回 200 并在
//	响应与审计里标出 egress_migration.applied=false，运维才能据此手动处理。
func (a *API) migrateEgressForPool(r *http.Request, keyID string, res *KeyPatchResult) *egressMigration {
	if res == nil || res.PrevPool == res.NewPool {
		return nil
	}
	// direct 模式下没有出口可迁移。
	if a.deps.Egress().Mode() == egress.ModeDirect {
		return nil
	}

	from := a.deps.Egress().BoundIP(keyID)
	to, err := a.deps.Egress().Migrate(keyID, res.NewPool)
	if err != nil {
		a.deps.Log().WarnContext(r.Context(), "池归属已变更但出口迁移失败",
			"key_id", keyID, "pool_from", res.PrevPool, "pool_to", res.NewPool,
			"egress_ip", from, "err", err)
		return &egressMigration{FromIP: from, Error: err.Error()}
	}

	// 落库，否则重启后 restoreBindings 会读到旧出口并把 Key 换回去。
	// 写库失败只降级告警: 内存中的迁移已生效，本次运行是正确的。
	if err := a.deps.Store().SetVolcKeyEgressIP(r.Context(), keyID, to); err != nil {
		a.deps.Log().WarnContext(r.Context(), "出口迁移已生效但落库失败，重启后可能回退",
			"key_id", keyID, "egress_ip", to, "err", err)
		return &egressMigration{Applied: true, FromIP: from, ToIP: to,
			Error: "落库失败: " + err.Error()}
	}

	a.deps.Log().InfoContext(r.Context(), "Key 已随池归属迁移出口",
		"key_id", keyID, "pool_from", res.PrevPool, "pool_to", res.NewPool,
		"ip_from", from, "ip_to", to)
	return &egressMigration{Applied: true, FromIP: from, ToIP: to}
}

// patchChangedFields 返回本次实际发生变更的字段名。
//
// 按「值确实变了」判定而非按「调用方提供了哪些字段」: 提交 status=active
// 而它本来就是 active 时，changed 应为空，让调用方一眼看出这是次空操作。
func patchChangedFields(res *KeyPatchResult) []string {
	changed := make([]string, 0, 3)
	if res.PrevStatus != res.NewStatus {
		changed = append(changed, "status")
	}
	if res.PrevPool != res.NewPool {
		changed = append(changed, "pool")
	}
	if res.PrevPersonaID != res.NewPersonaID {
		changed = append(changed, "persona_id")
	}
	return changed
}

// patchConflictMessage 组装 409 的文案。
//
// 必须说清「当前实际是什么」: 只回一句「状态冲突」会让运维不得不再去查一次
// 当前状态才能决定下一步，而这个值服务端刚刚已经读到了。
func patchConflictMessage(req patchKeyRequest, res *KeyPatchResult) string {
	cur := "未知"
	if res != nil {
		cur = res.PrevStatus
	}
	if req.ExpectedStatus != nil && res != nil && res.PrevStatus != *req.ExpectedStatus {
		return "expected_status 为 " + *req.ExpectedStatus + "，但当前状态是 " + cur
	}
	return "当前状态 " + cur +
		" 是终态，不会自动恢复。若已排查过根因，请带 force=true 重试 —— " +
		"未解决根因就放回流量会立刻再次失败，并向上游多贡献一次异常请求"
}
