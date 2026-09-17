package adminapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/fluxkeys/fluxkeys/internal/httpcore"
)

// handleAdminKeyDelete 处理 DELETE /admin/keys/{key_id}。
//
// 这是 KI-035 根因（测试收尾被迫用 SQL 直删 upstream_keys，而出口绑定的权威
// 副本在网关内存，删行不会释放它）的受管出口: 删行与解绑在同一次调用里完成，
// 不再留下「候选集恒空、新 Key 全部 502」的容量假满，也不必等 15 秒一轮的
// 背景对账去兜底（那只是把假满窗口从分钟级压到十几秒）。
//
// 顺序与 PATCH 一致 —— 先改库这一权威真相，成功后才同步内存态；任一步失败都
// 按「已提交的就保留、未生效的告警」处理，不回滚已完成的步骤:
//
//   - 删行失败（含 404）: 不解绑、不重载，直接报错。
//   - 删行成功、egress.Release 失败: Release 不返回 error（最坏只是无绑定可解），
//     实际上不会发生；即便如此也坚持先删行再解绑。
//   - 删行 + 解绑成功、scheduler.Reload 失败: 该 Key 仍可能在下一次被 Select 选中，
//     直到周期 key_reload 把它剔除。降级告警即可 —— 回滚删行会让已删的 Key 复活，
//     比短暂的不一致更糟。
func (a *API) handleAdminKeyDelete(w http.ResponseWriter, r *http.Request) {
	keyID := strings.TrimSpace(r.PathValue("key_id"))
	if keyID == "" {
		a.writeError(w, r, http.StatusNotFound, "invalid_request",
			"路径格式应为 /admin/keys/{key_id}")
		return
	}

	if err := a.deps.Store().DeleteUpstreamKey(r.Context(), keyID); err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			// code 与 revokeUserKey 的 404 对齐（key_not_found）: 自动化清理
			// 脚本按 code 区分「已删过」与「服务端故障」（livetest-ai KI-036）。
			a.writeError(w, r, http.StatusNotFound, "key_not_found", "Key 不存在: "+keyID)
			return
		}
		a.deps.Log().ErrorContext(r.Context(), "删除上游 Key 失败",
			"request_id", httpcore.RequestIDFromContext(r.Context()),
			"key_id", keyID, "err", err)
		a.writeError(w, r, http.StatusInternalServerError, "internal_error", "删除 Key 失败")
		return
	}

	// 删行成功，现在同步内存态。先解绑出口（与删行成对），再让调度器
	// 重载剔除该 Key —— Reload 只取 active 行，已删的 Key 不会回到选择集。
	releasedIP := a.deps.Egress().Release(keyID)

	if err := a.deps.Scheduler().Reload(r.Context()); err != nil {
		a.deps.Log().WarnContext(r.Context(), "Key 已删除但调度器重载失败，下一次选择可能仍命中该 Key",
			"key_id", keyID, "err", err)
	}

	a.audit(r, "delete_volc_key", keyID, map[string]any{
		"released_egress_ip": releasedIP,
		"request_id":         httpcore.RequestIDFromContext(r.Context()),
	})
	a.deps.Log().InfoContext(r.Context(), "上游 Key 已删除",
		"key_id", keyID, "released_egress_ip", releasedIP, "actor", adminActor(r))

	resp := map[string]any{
		"key_id":             keyID,
		"released_egress_ip": releasedIP,
	}
	httpcore.WriteJSON(w, http.StatusOK, resp)
}
