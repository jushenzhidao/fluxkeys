package gateway

import (
	"context"
	"errors"
	"fmt"
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

	// 取快照的唯一位置: 解析出 model 之后、任何配置决策之前。
	//
	// 三个推理端点（chat / images / embeddings）都经此路径，此后本请求的
	// 全部配置读取都走 snap，不再触碰 s.cfg 的 provider 相关字段。
	// 中途重取会让同一请求前后看到两套配置。
	snap := s.snaps.Current()

	// 从模型名推断 provider
	provider := snap.Cfg.ProviderForModel(meta.Model)
	if provider == "" {
		s.writeError(w, r, http.StatusNotFound, "model_not_found", 
			fmt.Sprintf("模型 %s 在所有上游中均不存在", meta.Model))
		s.metrics.ObserveRequest(epLabel, meta.Model, "404", false, time.Since(start))
		return
	}

	// embeddings 与 images 不支持流式
	if ep != adapter.EndpointChat {
		meta.Stream = false
	}

	kind := s.quotaKindFor(snap, provider, meta.Model)

	// 预扣量。Token 型按 (prompt 估算 + max_tokens) × 倍数，次数型按 n。
	var estimated int64
	if kind == quota.KindCount {
		estimated = adapter.EstimateCountUnits(meta)
	} else {
		estimated = adapter.EstimateTokensFor(meta,
			snap.Cfg.Quota.DefaultMaxTokens, snap.Cfg.Quota.EstimateMultiplier,
			s.reasoningEstimateFor(snap, provider, meta.Model))
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
		Provider:  provider,
		Model:     meta.Model,
		Body:      body,
		Stream:    meta.Stream,
		Estimated: estimated,
		QuotaKind: kind,
		LeaseTTL:  s.leaseTTLFor(meta.Stream),
		RequestID: reqID,
		UserCtx:   uc,
		Snap:      snap,
	}

	status, retries, upErr := s.execute(w, r, plan)

	// 重试全部失败且尚未写出响应时，透传最后一个错误
	if upErr != nil && status != 499 && !responseStarted(status) {
		s.writeError(w, r, upErr.ClientStatus, upErr.Code, upErr.Message)
		status = upErr.ClientStatus
	}

	// ===== 归还 TPM 预扣与实际用量的差额 =====
	//
	// AllowTokens 按预估量（含 1.2 倍放大系数与 max_tokens 上限）预扣了
	// 令牌桶，而实际用量通常远小于预估。不归还的话 TPM 被系统性高估，
	// 用户在远低于名义限额时就会被 429 —— 且误差随流式长输出请求放大。
	//
	// 失败请求全额归还: 上游没有产生任何消耗，预扣的令牌不应计入。
	s.refundTokens(ctx, plan, uc, status)

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

// refundTokens 把 TPM 预扣量与实际用量的差额还给用户的令牌桶。
//
// 只处理 Token 型配额: 次数型请求在 checkRateLimit 里就没有消耗 TPM。
// 归还失败只记日志 —— 限流是用量保护而非账务，偏差会随桶的时间填充自愈。
func (s *Server) refundTokens(ctx context.Context, plan *requestPlan, uc *UserContext, status int) {
	if s.limiter == nil || uc == nil || uc.TPMLimit <= 0 || plan.QuotaKind == quota.KindCount {
		return
	}

	var actual int64
	if status == http.StatusOK && plan.finalUsage != nil {
		actual = plan.finalUsage.Total()
	}
	// 失败或无用量: actual=0，全额归还预扣。
	refund := plan.Estimated - actual
	if refund <= 0 {
		return
	}

	// 用独立 ctx: 客户端断连时 r.Context() 已取消，而归还恰恰在断连场景
	// 最重要（预扣了满额却几乎没消耗）。
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := s.limiter.RefundTokens(rctx, uc.UserID, uc.TPMLimit, refund); err != nil {
		s.log.WarnContext(rctx, "归还 TPM 令牌失败",
			"request_id", plan.RequestID, "user_id", uc.UserID,
			"refund", refund, "err", err)
	}
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
		RequestID: plan.RequestID,
		// 必须取本次请求实际路由到的 provider，而不是写死一个常量。
		// 写死会让所有上游的流水都归到同一家名下: 归档、对账、按 provider
		// 出账全部错位，而每一行看起来都「有值」，不会有任何报错提示。
		Provider: plan.Provider,
		Model:    plan.Model,
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
		rec.ReasoningTokens = u.ReasoningTokens
		rec.TotalTokens = u.Total()
	}
	// 落库的计费量必须与真正从配额里扣掉的量同源。
	//
	// 直接抄 usage.CountUnits 会让 chat 端点的按次计费流水恒为 0（上游只在
	// 图片类响应里返回张数），而 Redis 侧按预扣量实扣了 1 —— 账面对不平，
	// 且偏差方向是「少记」，对账时看起来像是网关白送了额度。
	if status == http.StatusOK && plan.QuotaKind == quota.KindCount {
		var u adapter.Usage
		if plan.finalUsage != nil {
			u = *plan.finalUsage
		}
		rec.CountUnits = actualFor(u, plan.Estimated, quota.KindCount)
	}
	rec.UpstreamKeyID = plan.finalKeyID
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
	items := make([]any, 0)

	add := func(id, ownedBy string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		items = append(items, map[string]any{
			"id": id, "object": "model", "created": created, "owned_by": ownedBy,
		})
	}
	
	// 遍历所有 providers，收集模型列表。
	// 取一次快照即可: 本端点只读一处配置，不存在跨读不一致的风险。
	for providerName, p := range s.snaps.Current().Cfg.Providers {
		for public := range p.ModelMapping {
			add(public, providerName)
		}
		for _, m := range p.CountModels {
			add(m, providerName)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": items})
}
