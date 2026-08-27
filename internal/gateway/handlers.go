package gateway

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/adapter"
	"github.com/fluxkeys/fluxkeys/internal/quota"
)

// handleChat 处理 POST /v1/chat/completions。
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	s.handleInference(w, r, adapter.EndpointChat, "chat")
}

// handleEmbeddings 处理 POST /v1/embeddings。
func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	s.handleInference(w, r, adapter.EndpointEmbeddings, "embeddings")
}

// handleImages 处理 POST /v1/images/generations。
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	s.handleInference(w, r, adapter.EndpointImages, "images")
}

// handleInference 是三个推理端点的共同实现。
//
// 三者的差异只在计费方式与预扣量的算法上，请求生命周期完全一致，
// 故不为每个端点各写一份 —— 那会让「租约必须结束」这个不变量需要
// 在三处分别维护。
func (s *Server) handleInference(w http.ResponseWriter, r *http.Request, ep adapter.Endpoint, epLabel string) {
	if r.Method != http.MethodPost {
		s.writeError(w, r, http.StatusMethodNotAllowed, "invalid_request", "该端点只接受 POST")
		return
	}

	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)
	uc := UserFromContext(ctx)
	start := time.Now()

	body, err := s.readBody(r)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		s.metrics.ObserveRequest(epLabel, "unknown", "400", false, time.Since(start))
		return
	}

	meta, perr := adapter.ParseChatMeta(body)
	if perr != nil {
		var ue *adapter.UpstreamError
		if errors.As(perr, &ue) {
			s.writeError(w, r, ue.ClientStatus, ue.Code, ue.Message)
		} else {
			s.writeError(w, r, http.StatusBadRequest, "invalid_request", perr.Error())
		}
		s.metrics.ObserveRequest(epLabel, "unknown", "400", false, time.Since(start))
		return
	}
	if meta.Model == "" {
		s.writeError(w, r, http.StatusBadRequest, "invalid_request", "缺少 model 字段")
		s.metrics.ObserveRequest(epLabel, "unknown", "400", false, time.Since(start))
		return
	}
	// embeddings 与 images 不支持流式
	if ep != adapter.EndpointChat {
		meta.Stream = false
	}

	kind := s.quotaKindFor(meta.Model)

	// 预扣量。Token 型按 (prompt 估算 + max_tokens) × 倍数，次数型按 n。
	var estimated int64
	if kind == quota.KindCount {
		estimated = adapter.EstimateCountUnits(meta)
	} else {
		estimated = adapter.EstimateTokens(meta,
			s.cfg.Quota.DefaultMaxTokens, s.cfg.Quota.EstimateMultiplier)
	}

	// ===== 用户级限流（P1-9）=====
	if !s.checkRateLimit(w, r, uc, estimated, kind, epLabel, start) {
		return
	}

	// 流式请求须显式要求上游返回 usage，否则拿不到真实用量做配额修正
	if meta.Stream {
		if newBody, changed := adapter.EnsureStreamUsage(body); changed {
			body = newBody
		}
	}

	plan := &requestPlan{
		Endpoint:  ep,
		Model:     meta.Model,
		Body:      body,
		Stream:    meta.Stream,
		Estimated: estimated,
		QuotaKind: kind,
		LeaseTTL:  s.leaseTTLFor(meta.Stream),
		RequestID: reqID,
		UserCtx:   uc,
	}

	status, retries, upErr := s.execute(w, r, plan)

	// 重试全部失败且尚未写出响应时，透传最后一个错误
	if upErr != nil && status != 499 && !responseStarted(status) {
		s.writeError(w, r, upErr.ClientStatus, upErr.Code, upErr.Message)
		status = upErr.ClientStatus
	}

	elapsed := time.Since(start)
	s.metrics.ObserveRequest(epLabel, meta.Model, strconv.Itoa(status), meta.Stream, elapsed)

	// ===== 写用量流水 =====
	s.recordUsage(ctx, plan, uc, status, retries, upErr, elapsed)

	logLevel := s.log.InfoContext
	if upErr != nil {
		logLevel = s.log.WarnContext
	}
	attrs := []any{
		"request_id", reqID, "endpoint", epLabel, "model", meta.Model,
		"stream", meta.Stream, "status", status, "retries", retries,
		"estimated", estimated, "latency_ms", elapsed.Milliseconds(),
	}
	if uc != nil {
		attrs = append(attrs, "user_id", uc.UserID)
	}
	if upErr != nil {
		attrs = append(attrs, "error_class", upErr.Class.String(), "error_code", upErr.Code)
	}
	logLevel(ctx, "请求完成", attrs...)
}

// responseStarted 判断该状态码是否意味着响应已开始写出。
func responseStarted(status int) bool { return status >= 200 && status < 400 }

// checkRateLimit 执行用户级 RPM / TPM 限流。返回 false 表示已拒绝。
func (s *Server) checkRateLimit(w http.ResponseWriter, r *http.Request, uc *UserContext,
	estimated int64, kind quota.Kind, epLabel string, start time.Time) bool {

	if s.limiter == nil || uc == nil {
		return true
	}
	ctx := r.Context()

	res, err := s.limiter.AllowRequest(ctx, uc.UserID, uc.RPMLimit)
	if err != nil {
		// fail-open 已在限流器内决定，这里只记录
		s.log.WarnContext(ctx, "RPM 限流检查异常，已放行",
			"request_id", RequestIDFromContext(ctx), "err", err)
	}
	if !res.Allowed {
		s.metrics.ObserveRateLimited("rpm")
		s.rejectRateLimited(w, r, res, "请求频率超过限额（RPM）")
		s.metrics.ObserveRequest(epLabel, "unknown", "429", false, time.Since(start))
		return false
	}

	// 次数型请求不消耗 TPM 额度
	if kind == quota.KindCount {
		return true
	}

	tres, err := s.limiter.AllowTokens(ctx, uc.UserID, uc.TPMLimit, estimated)
	if err != nil {
		s.log.WarnContext(ctx, "TPM 限流检查异常，已放行",
			"request_id", RequestIDFromContext(ctx), "err", err)
	}
	if !tres.Allowed {
		s.metrics.ObserveRateLimited("tpm")
		s.rejectRateLimited(w, r, tres, "Token 消耗速率超过限额（TPM）")
		s.metrics.ObserveRequest(epLabel, "unknown", "429", false, time.Since(start))
		return false
	}
	return true
}

func (s *Server) rejectRateLimited(w http.ResponseWriter, r *http.Request, res Result, msg string) {
	if res.RetryAfter > 0 {
		secs := int(res.RetryAfter.Seconds())
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	s.writeError(w, r, http.StatusTooManyRequests, "rate_limit", msg)
}

// recordUsage 写入用量流水。
//
// 用独立 context: 客户端断连时 r.Context() 已取消，用它会导致流水丢失，
// 而流水是计费与审计的事实来源，不能因为用户提前挂断就不记。
func (s *Server) recordUsage(ctx context.Context, plan *requestPlan, uc *UserContext,
	status, retries int, upErr *adapter.UpstreamError, elapsed time.Duration) {

	rec := UsageRecord{
		RequestID:       plan.RequestID,
		Provider:        string(adapter.ProviderVolc),
		Model:           plan.Model,
		BillingKind:     string(plan.QuotaKind),
		QuotaDay:        quota.QuotaDayTime(time.Now()),
		EstimatedTokens: plan.Estimated,
		StatusCode:      status,
		IsStream:        plan.Stream,
		RetryCount:      retries,
		LatencyMS:       int(elapsed.Milliseconds()),
	}
	if uc != nil {
		rec.UserID = uc.UserID
		rec.UserAPIKeyID = uc.APIKeyID
	}
	if upErr != nil {
		rec.ErrorCode = upErr.Code
	}
	if u := plan.finalUsage; u != nil {
		rec.PromptTokens = u.PromptTokens
		rec.CompletionTokens = u.CompletionTokens
		rec.TotalTokens = u.Total()
		rec.CountUnits = u.CountUnits
	}
	rec.VolcKeyID = plan.finalKeyID
	rec.EgressIP = plan.finalEgressIP

	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.store.RecordUsage(wctx, rec); err != nil {
		// 流水写入失败不影响用户响应（响应可能已发出），但必须告警 ——
		// 丢流水意味着这次用量无法计费也无法审计。
		s.log.ErrorContext(wctx, "写入用量流水失败",
			"request_id", plan.RequestID, "err", err)
	}
}

// handleModels 处理 GET /v1/models。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, r, http.StatusMethodNotAllowed, "invalid_request", "该端点只接受 GET")
		return
	}

	// 模型列表来自配置的映射表: 只暴露本网关确实能路由的模型。
	// 直接透传上游列表会让用户请求到未配映射的模型而在转换阶段才失败。
	created := time.Now().Unix()
	seen := make(map[string]bool)
	items := make([]any, 0, len(s.cfg.Upstream.ModelMapping)+len(s.cfg.Upstream.CountModels))

	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		items = append(items, map[string]any{
			"id": id, "object": "model", "created": created, "owned_by": "volc",
		})
	}
	for public := range s.cfg.Upstream.ModelMapping {
		add(public)
	}
	for _, m := range s.cfg.Upstream.CountModels {
		add(m)
	}

	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": items})
}
