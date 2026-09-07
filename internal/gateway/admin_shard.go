package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
)

// POST /admin/keys/shard: 批量指派 Key 的机器归属（多机部署分片）。
//
// 为什么是独立端点而非并入 PATCH /admin/keys/{key_id}:
//
//   - 分片指派天然是批量动作。扩容时运维要的是「把这 300 个 Key 划给
//     新机器」一次调用，逐个 PATCH 是 300 次往返加 300 条审计噪音。
//   - 归属与 status/pool 不是同一量级的元数据。它决定该 Key 由哪台机器、
//     从哪个出口 IP 发请求，指派错误的表现是「账号从两个 IP 出去」或
//     「账号从所有机器上消失」，都不报错。独立端点让审计里这类操作
//     一眼可辨。
//
// 指派后不热生效: 目标机器要到下一轮 key_reload（≤5 分钟）或重启后才
// 装载新划入的 Key。这是接受的 —— 分片调整本就伴随出口 IP 在云厂商侧
// 的重绑定，属于分钟级的运维动作，不值得为它加跨机通知机制。

// shardAssignRequest 是 POST /admin/keys/shard 的请求体。
type shardAssignRequest struct {
	// Shard 是目标分片标识。允许显式传空串 —— 语义是「解除归属」，
	// 解除后该批 Key 不被任何实例装载，用于下线前的摘流。
	Shard string `json:"shard"`
	// KeyIDs 是待指派的 Key 列表，必填非空。
	KeyIDs []string `json:"key_ids"`
	// Reason 只进审计。
	Reason string `json:"reason"`
}

// handleAdminKeyShard 处理 POST /admin/keys/shard。
func (s *Server) handleAdminKeyShard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, r, http.StatusMethodNotAllowed, "invalid_request", "该端点只接受 POST")
		return
	}

	var req shardAssignRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "invalid_request", "请求体非法: "+err.Error())
		return
	}

	// 去空白与去重。空 key_id 直接拒绝而非静默跳过 ——
	// 它多半意味着调用方的清单生成有 bug，静默跳过会掩盖它。
	ids := make([]string, 0, len(req.KeyIDs))
	seen := make(map[string]bool, len(req.KeyIDs))
	for _, id := range req.KeyIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			s.writeError(w, r, http.StatusBadRequest, "invalid_request",
				"key_ids 含空白项")
			return
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		s.writeError(w, r, http.StatusBadRequest, "invalid_request", "key_ids 不能为空")
		return
	}

	affected, err := s.store.AssignShard(r.Context(), req.Shard, ids)
	if err != nil {
		s.log.ErrorContext(r.Context(), "指派机器归属失败",
			"request_id", RequestIDFromContext(r.Context()),
			"shard", req.Shard, "count", len(ids), "err", err)
		s.writeError(w, r, http.StatusInternalServerError, "internal_error", "指派失败")
		return
	}

	// affected < len(ids) 说明有 key_id 没匹配上（拼错或已删除）。
	// 不算失败但必须让调用方看见 —— 漏指派的 Key 会静默地不被任何
	// 实例装载。
	s.audit(r, "assign_key_shard", req.Shard, map[string]any{
		"shard":      req.Shard,
		"requested":  len(ids),
		"affected":   affected,
		"key_ids":    ids,
		"reason":     req.Reason,
		"request_id": RequestIDFromContext(r.Context()),
	})
	s.log.InfoContext(r.Context(), "已指派 Key 机器归属",
		"shard", req.Shard, "requested", len(ids), "affected", affected,
		"actor", adminActor(r))

	writeJSON(w, http.StatusOK, map[string]any{
		"shard":     req.Shard,
		"requested": len(ids),
		"affected":  affected,
		"hint":      "目标机器将在下一轮 key_reload（≤5 分钟）或重启后装载新划入的 Key",
	})
}
