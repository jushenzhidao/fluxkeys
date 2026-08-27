package mockark

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

// Stats 是 Mock 服务的聚合统计。
type Stats struct {
	Uptime       string       `json:"uptime"`
	TotalRequest int64        `json:"total_requests"`
	TotalError   int64        `json:"total_errors"`
	TokenUsed    int64        `json:"token_used"`
	CountUsed    int64        `json:"count_used"`
	Keys         []KeyAccount `json:"keys"`
	// SourceIPs 是全局源 IP 分布。
	//
	// 这是验证 P1-6 的核心数据: 若 multi_ip 模式下这里只出现一个 IP，
	// 说明策略路由未生效，出口隔离设计已静默失效。
	SourceIPs map[string]int64 `json:"source_ips"`
	// KeysPerIP 统计每个源 IP 承载了多少个不同的 Key，用于校验绑定关系。
	KeysPerIP    map[string]int      `json:"keys_per_ip"`
	IPsPerKey    map[string][]string `json:"ips_per_key"`
	ActiveFaults []Fault             `json:"active_faults"`
	LogCount     int                 `json:"log_count"`
}

var startedAt = time.Now()

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	st := Stats{
		Uptime:    time.Since(startedAt).Round(time.Second).String(),
		SourceIPs: make(map[string]int64),
		KeysPerIP: make(map[string]int),
		IPsPerKey: make(map[string][]string),
		LogCount:  len(s.logs),
	}
	for _, a := range s.accounts {
		cp := *a
		cp.SourceIPs = make(map[string]int64, len(a.SourceIPs))
		var ips []string
		for ip, n := range a.SourceIPs {
			cp.SourceIPs[ip] = n
			st.SourceIPs[ip] += n
			st.KeysPerIP[ip]++
			ips = append(ips, ip)
		}
		sort.Strings(ips)
		st.IPsPerKey[a.KeyID] = ips
		st.Keys = append(st.Keys, cp)
		st.TotalRequest += a.Requests
		st.TotalError += a.Errors
		st.TokenUsed += a.TokenUsed
		st.CountUsed += a.CountUsed
	}
	for _, f := range s.faults {
		st.ActiveFaults = append(st.ActiveFaults, *f)
	}
	s.mu.Unlock()

	sort.Slice(st.Keys, func(i, j int) bool { return st.Keys[i].KeyID < st.Keys[j].KeyID })

	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "需要 POST"})
		return
	}
	s.Reset()
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}

// injectRequest 是 POST /_mock/inject 的请求体。
//
// delay_ms 用整数毫秒而非 Go duration 字符串，方便 curl 与其他语言调用。
type injectRequest struct {
	Kind  string `json:"kind"`
	KeyID string `json:"key_id"`
	// SourceIP 按请求源 IP 过滤，用于模拟「整个出口 IP 被封」。
	// 与 KeyID 同时给出时取交集（该 Key 从该 IP 发出的请求）。
	SourceIP  string `json:"source_ip"`
	Remaining int    `json:"remaining"`
	DelayMS   int    `json:"delay_ms"`
	// 以下字段用于同时配置 Key 额度，省去单独调用 /_mock/keys
	TokenLimit *int64 `json:"token_limit"`
	CountLimit *int64 `json:"count_limit"`
	Disabled   *bool  `json:"disabled"`
}

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "需要 POST"})
		return
	}
	var req injectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体非法: " + err.Error()})
		return
	}

	if req.Kind != "" {
		if !validFault(FaultKind(req.Kind)) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "未知故障类型: " + req.Kind, "valid": validFaultKinds(),
			})
			return
		}
		s.Inject(Fault{
			Kind:      FaultKind(req.Kind),
			KeyID:     req.KeyID,
			SourceIP:  req.SourceIP,
			Remaining: req.Remaining,
			Delay:     time.Duration(req.DelayMS) * time.Millisecond,
		})
	}

	if req.KeyID != "" && (req.TokenLimit != nil || req.CountLimit != nil || req.Disabled != nil) {
		s.mu.Lock()
		a := s.account(req.KeyID)
		if req.TokenLimit != nil {
			a.TokenLimit = *req.TokenLimit
		}
		if req.CountLimit != nil {
			a.CountLimit = *req.CountLimit
		}
		if req.Disabled != nil {
			a.Disabled = *req.Disabled
		}
		s.mu.Unlock()
	}

	writeJSON(w, http.StatusOK, map[string]any{"injected": true})
}

// handleKeys 支持查询与批量预置 Key 额度。
func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		out := make([]KeyAccount, 0, len(s.accounts))
		for _, a := range s.accounts {
			cp := *a
			cp.SourceIPs = make(map[string]int64, len(a.SourceIPs))
			for k, v := range a.SourceIPs {
				cp.SourceIPs[k] = v
			}
			out = append(out, cp)
		}
		s.mu.Unlock()
		sort.Slice(out, func(i, j int) bool { return out[i].KeyID < out[j].KeyID })
		writeJSON(w, http.StatusOK, map[string]any{"keys": out})

	case http.MethodPost:
		var req struct {
			Keys []struct {
				KeyID      string `json:"key_id"`
				TokenLimit int64  `json:"token_limit"`
				CountLimit int64  `json:"count_limit"`
				TokenUsed  int64  `json:"token_used"`
				CountUsed  int64  `json:"count_used"`
				Disabled   bool   `json:"disabled"`
			} `json:"keys"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.mu.Lock()
		for _, k := range req.Keys {
			if k.KeyID == "" {
				continue
			}
			a := s.account(k.KeyID)
			a.TokenLimit = k.TokenLimit
			a.CountLimit = k.CountLimit
			a.TokenUsed = k.TokenUsed
			a.CountUsed = k.CountUsed
			a.Disabled = k.Disabled
		}
		n := len(req.Keys)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"configured": n})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 GET/POST"})
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"logs": s.Logs()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func validFaultKinds() []string {
	return []string{
		string(Fault429RateLimit), string(Fault429Quota), string(Fault401), string(Fault403),
		string(Fault500), string(Fault503), string(FaultTimeout), string(FaultSlow),
		string(FaultStreamAbort), string(FaultBadJSON),
	}
}

func validFault(k FaultKind) bool {
	for _, v := range validFaultKinds() {
		if string(k) == v {
			return true
		}
	}
	return false
}
