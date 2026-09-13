package mockark

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// arkError 是火山返回的错误结构（与 OpenAI 兼容）。
type arkError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func writeArkError(w http.ResponseWriter, status int, code, typ, msg string) {
	var e arkError
	e.Error.Message = msg
	e.Error.Type = typ
	e.Error.Code = code
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e)
}

// 火山额度耗尽的真实错误形态。网关的 MapError 必须能把它归类为 ErrClassQuota，
// 而不是当成普通限流 —— 两者的处置完全不同。
func writeQuotaExhausted(w http.ResponseWriter, keyID string) {
	writeArkError(w, http.StatusTooManyRequests, "QuotaExceeded", "quota_exceeded",
		fmt.Sprintf("The account %s free trial quota has been exhausted. 该模型免费额度已用完。", keyID))
}

// applyFault 处理会直接终止请求的故障，返回 true 表示已响应完毕。
func (s *Server) applyFault(w http.ResponseWriter, r *http.Request, f Fault, keyID string) bool {
	switch f.Kind {
	case Fault401:
		writeArkError(w, http.StatusUnauthorized, "AuthenticationError", "invalid_request_error",
			"The API key in the request is invalid.")
		return true
	case Fault403:
		writeArkError(w, http.StatusForbidden, "AccessDenied", "permission_error",
			"You do not have permission to access this model.")
		return true
	case Fault429RateLimit:
		w.Header().Set("Retry-After", "1")
		writeArkError(w, http.StatusTooManyRequests, "RateLimitExceeded", "rate_limit_error",
			"Rate limit exceeded, please retry later.")
		return true
	case Fault429Quota:
		writeQuotaExhausted(w, keyID)
		return true
	case Fault500:
		writeArkError(w, http.StatusInternalServerError, "InternalServiceError", "server_error",
			"Internal service error, please retry.")
		return true
	case Fault503:
		writeArkError(w, http.StatusServiceUnavailable, "ServiceUnavailable", "server_error",
			"Service temporarily unavailable.")
		return true
	case FaultBadJSON:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"broken","choices":[`))
		return true
	case FaultTimeout:
		// 挂住直至客户端超时或断开。必须尊重 ctx，否则测试结束后 goroutine 泄漏。
		d := f.Delay
		if d <= 0 {
			d = 60 * time.Second
		}
		select {
		case <-r.Context().Done():
		case <-time.After(d):
		}
		return true
	}
	return false
}

// chatRequest 是 Mock 需要理解的请求字段。
type chatRequest struct {
	Model     string `json:"model"`
	Stream    bool   `json:"stream"`
	MaxTokens int64  `json:"max_tokens"`
	Messages  []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

// promptTokens 按输入字符数折算 prompt_tokens，让 usage 看起来真实可信。
func (req *chatRequest) promptTokens() int64 {
	var chars int64
	for _, m := range req.Messages {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			chars += int64(utf8.RuneCountInString(s))
			continue
		}
		chars += int64(utf8.RuneCount(m.Content))
	}
	n := chars / 2
	if n <= 0 {
		n = 1
	}
	return n
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeArkError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "invalid_request_error", "method not allowed")
		return
	}

	keyID, ok := extractKeyID(r)
	if !ok {
		writeArkError(w, http.StatusUnauthorized, "AuthenticationError", "invalid_request_error",
			"Missing Authorization header.")
		return
	}
	srcIP := sourceIP(r)
	reqID := r.Header.Get("X-Request-Id")

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeArkError(w, http.StatusBadRequest, "InvalidParameter", "invalid_request_error",
			"invalid request body: "+err.Error())
		s.recordUsage(keyID, srcIP, 0, 0, true)
		return
	}

	logEntry := RequestLog{
		At: time.Now(), KeyID: keyID, SourceIP: srcIP,
		Path: r.URL.Path, Model: req.Model, Stream: req.Stream, RequestID: reqID,
	}

	// Key 被封禁: 无条件 401，优先于一切故障注入
	if s.isDisabled(keyID) {
		writeArkError(w, http.StatusUnauthorized, "AuthenticationError", "invalid_request_error",
			"The API key has been disabled.")
		s.recordUsage(keyID, srcIP, 0, 0, true)
		logEntry.Status = http.StatusUnauthorized
		s.appendLog(logEntry)
		return
	}

	fault := s.pickFault(keyID, srcIP, r)
	logEntry.Fault = fault.Kind

	if fault.Kind == FaultSlow {
		d := fault.Delay
		if d <= 0 {
			d = 2 * time.Second
		}
		select {
		case <-r.Context().Done():
			logEntry.Status = 499
			s.appendLog(logEntry)
			return
		case <-time.After(d):
		}
	} else if s.applyFault(w, r, fault, keyID) {
		s.recordUsage(keyID, srcIP, 0, 0, true)
		logEntry.Status = faultStatus(fault.Kind)
		s.appendLog(logEntry)
		return
	}

	// 额度检查在故障注入之后: 真实场景里额度耗尽本身就是一种「上游拒绝」
	if s.checkQuota(keyID, "token") {
		writeQuotaExhausted(w, keyID)
		s.recordUsage(keyID, srcIP, 0, 0, true)
		logEntry.Status = http.StatusTooManyRequests
		s.appendLog(logEntry)
		return
	}

	pt := req.promptTokens()
	ct := s.completionTokens(req.MaxTokens)
	logEntry.Prompt, logEntry.Completion = pt, ct

	if req.Stream {
		status := s.streamChat(w, r, &req, keyID, pt, ct, fault)
		logEntry.Status = status
		// 流式中断时 usage 未发出，但上游已实际消耗 —— 仍须记账，
		// 这正是「网关侧必须靠租约兜底」的原因。
		s.recordUsage(keyID, srcIP, pt+ct, 0, status != http.StatusOK)
		s.appendLog(logEntry)
		return
	}

	resp := map[string]any{
		"id":      s.nextID("chatcmpl"),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": s.fakeContent(ct),
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     pt,
			"completion_tokens": ct,
			"total_tokens":      pt + ct,
		},
	}

	s.recordUsage(keyID, srcIP, pt+ct, 0, false)
	logEntry.Status = http.StatusOK
	s.appendLog(logEntry)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// streamChat 输出 SSE 流，返回最终状态码。
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, req *chatRequest,
	keyID string, pt, ct int64, fault Fault) int {

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeArkError(w, http.StatusInternalServerError, "InternalServiceError", "server_error",
			"streaming unsupported")
		return http.StatusInternalServerError
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	id := s.nextID("chatcmpl")
	created := time.Now().Unix()
	chunks := s.opt.StreamChunks

	// abortAt 让流在中途断开，用于验证网关的租约回收路径
	abortAt := -1
	if fault.Kind == FaultStreamAbort {
		abortAt = chunks / 2
		if abortAt < 1 {
			abortAt = 1
		}
	}

	emit := func(payload map[string]any) bool {
		b, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// 首个 chunk 声明 role
	if !emit(map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}}},
		"usage":   nil,
	}) {
		return 499
	}

	for i := 0; i < chunks; i++ {
		if i == abortAt {
			// 不发 [DONE]、不发 usage，直接返回 —— 模拟上游异常中断
			return 499
		}
		select {
		case <-r.Context().Done():
			return 499
		default:
		}
		if s.opt.StreamChunkDelay > 0 {
			select {
			case <-r.Context().Done():
				return 499
			case <-time.After(s.opt.StreamChunkDelay):
			}
		}
		if !emit(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": fmt.Sprintf("片段%d ", i)},
			}},
			"usage": nil,
		}) {
			return 499
		}
	}

	// 结束 chunk 带 finish_reason
	if !emit(map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		"usage":   nil,
	}) {
		return 499
	}

	// usage chunk。火山仅在 stream_options.include_usage=true 时发送，
	// 网关必须主动补该参数，否则流式请求永远拿不到真实用量。
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		if !emit(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct,
			},
		}) {
			return 499
		}
	}

	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return 499
	}
	flusher.Flush()
	return http.StatusOK
}

type imagesRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	N      int64  `json:"n"`
	Size   string `json:"size"`
}

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeArkError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "invalid_request_error", "method not allowed")
		return
	}
	keyID, ok := extractKeyID(r)
	if !ok {
		writeArkError(w, http.StatusUnauthorized, "AuthenticationError", "invalid_request_error", "Missing Authorization header.")
		return
	}
	srcIP := sourceIP(r)

	var req imagesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeArkError(w, http.StatusBadRequest, "InvalidParameter", "invalid_request_error", "invalid request body")
		return
	}
	if req.N <= 0 {
		req.N = 1
	}

	logEntry := RequestLog{At: time.Now(), KeyID: keyID, SourceIP: srcIP,
		Path: r.URL.Path, Model: req.Model, RequestID: r.Header.Get("X-Request-Id")}

	if s.isDisabled(keyID) {
		writeArkError(w, http.StatusUnauthorized, "AuthenticationError", "invalid_request_error", "The API key has been disabled.")
		s.recordUsage(keyID, srcIP, 0, 0, true)
		logEntry.Status = http.StatusUnauthorized
		s.appendLog(logEntry)
		return
	}

	fault := s.pickFault(keyID, srcIP, r)
	logEntry.Fault = fault.Kind
	if s.applyFault(w, r, fault, keyID) {
		s.recordUsage(keyID, srcIP, 0, 0, true)
		logEntry.Status = faultStatus(fault.Kind)
		s.appendLog(logEntry)
		return
	}

	// 次数型额度: Seedream 每日 100 次
	if s.checkQuota(keyID, "count") {
		writeQuotaExhausted(w, keyID)
		s.recordUsage(keyID, srcIP, 0, 0, true)
		logEntry.Status = http.StatusTooManyRequests
		s.appendLog(logEntry)
		return
	}

	data := make([]any, 0, req.N)
	for i := int64(0); i < req.N; i++ {
		data = append(data, map[string]any{
			"url":            fmt.Sprintf("https://mock.local/img/%s-%d.png", s.nextID("img"), i),
			"revised_prompt": req.Prompt,
		})
	}

	s.recordUsage(keyID, srcIP, 0, req.N, false)
	logEntry.Status = http.StatusOK
	s.appendLog(logEntry)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"created": time.Now().Unix(),
		"model":   req.Model,
		"data":    data,
	})
}

type embeddingsRequest struct {
	Model string          `json:"model"`
	Input json.RawMessage `json:"input"`
}

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeArkError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "invalid_request_error", "method not allowed")
		return
	}
	keyID, ok := extractKeyID(r)
	if !ok {
		writeArkError(w, http.StatusUnauthorized, "AuthenticationError", "invalid_request_error", "Missing Authorization header.")
		return
	}
	srcIP := sourceIP(r)

	var req embeddingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeArkError(w, http.StatusBadRequest, "InvalidParameter", "invalid_request_error", "invalid request body")
		return
	}

	logEntry := RequestLog{At: time.Now(), KeyID: keyID, SourceIP: srcIP,
		Path: r.URL.Path, Model: req.Model, RequestID: r.Header.Get("X-Request-Id")}

	if s.isDisabled(keyID) {
		writeArkError(w, http.StatusUnauthorized, "AuthenticationError", "invalid_request_error", "The API key has been disabled.")
		logEntry.Status = http.StatusUnauthorized
		s.recordUsage(keyID, srcIP, 0, 0, true)
		s.appendLog(logEntry)
		return
	}

	fault := s.pickFault(keyID, srcIP, r)
	logEntry.Fault = fault.Kind
	if s.applyFault(w, r, fault, keyID) {
		s.recordUsage(keyID, srcIP, 0, 0, true)
		logEntry.Status = faultStatus(fault.Kind)
		s.appendLog(logEntry)
		return
	}
	if s.checkQuota(keyID, "token") {
		writeQuotaExhausted(w, keyID)
		s.recordUsage(keyID, srcIP, 0, 0, true)
		logEntry.Status = http.StatusTooManyRequests
		s.appendLog(logEntry)
		return
	}

	inputs := parseEmbedInputs(req.Input)
	items := make([]any, 0, len(inputs))
	var pt int64
	for i, text := range inputs {
		pt += int64(utf8.RuneCountInString(text))/2 + 1
		vec := make([]float64, 8)
		for j := range vec {
			vec[j] = float64((i*8+j)%100) / 100
		}
		items = append(items, map[string]any{"object": "embedding", "embedding": vec, "index": i})
	}

	s.recordUsage(keyID, srcIP, pt, 0, false)
	logEntry.Status = http.StatusOK
	logEntry.Prompt = pt
	s.appendLog(logEntry)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list", "data": items, "model": req.Model,
		"usage": map[string]any{"prompt_tokens": pt, "total_tokens": pt},
	})
}

func parseEmbedInputs(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	return []string{""}
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	items := make([]any, 0, len(s.opt.Models))
	for _, m := range s.opt.Models {
		items = append(items, map[string]any{
			"id": m, "object": "model", "created": time.Now().Unix(), "owned_by": "volc",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": items})
}

// completionTokens 生成一个有波动但受 max_tokens 约束的输出长度。
func (s *Server) completionTokens(maxTokens int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := int64(50 + s.rnd.Intn(200))
	if maxTokens > 0 && base > maxTokens {
		base = maxTokens
	}
	if base <= 0 {
		base = 1
	}
	return base
}

func (s *Server) fakeContent(tokens int64) string {
	n := int(tokens)
	if n > 200 {
		n = 200
	}
	if n < 1 {
		n = 1
	}
	return strings.Repeat("模拟回复。", n/5+1)
}

func faultStatus(k FaultKind) int {
	switch k {
	case Fault401:
		return http.StatusUnauthorized
	case Fault403:
		return http.StatusForbidden
	case Fault429RateLimit, Fault429Quota:
		return http.StatusTooManyRequests
	case Fault500:
		return http.StatusInternalServerError
	case Fault503:
		return http.StatusServiceUnavailable
	case FaultTimeout:
		return 0
	case FaultBadJSON:
		return http.StatusOK
	}
	return http.StatusOK
}
