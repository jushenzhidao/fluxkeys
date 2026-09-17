package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/adapter"
	"github.com/fluxkeys/fluxkeys/internal/confsnap"
	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/quota"
)

// 本文件是网关的核心执行路径，两个不变量贯穿全文:
//
//  1. 租约必须结束。Acquire 成功后，每一条 return 路径都必须走到 Commit
//     或 Release。做法是 Acquire 之后立即 defer 一个结束函数，由后续代码
//     只负责设置「实际用量」，而不负责调用 —— 依赖每条分支各自记得调用
//     必然会漏（P0-2 的根因）。
//
//  2. 流式必须真流。逐 chunk 读取、逐 chunk 刷出，不缓冲完整响应体。缓冲
//     会让首字节延迟等于完整生成耗时，对用户是不可接受的体验回退。

// attemptResult 是单次上游尝试的结果。
type attemptResult struct {
	// Done 表示已向客户端写出响应，调用方不得再写。
	Done bool
	// Err 是本次尝试的失败信息，nil 表示成功。
	Err *adapter.UpstreamError
	// KeyID 是本次使用的 Key。
	KeyID string
	// EgressIP 是本次使用的出口 IP，用于审计与排障。
	EgressIP string
	// Usage 是实际用量（成功时有效）。
	Usage adapter.Usage
	// Estimated 是本次预扣量。
	Estimated int64
	// StatusCode 是上游返回的状态码。
	StatusCode int
	// HeadersSent 表示是否已向客户端写出响应头。
	//
	// 这是流式场景的关键状态: 一旦写出 200 和首个 chunk，就不能再改状态码，
	// 也不能换 Key 重试 —— 客户端已经在消费这个流了。
	HeadersSent bool
}

// requestPlan 描述一次用户请求的执行计划。
type requestPlan struct {
	Endpoint  adapter.Endpoint
	Provider  string // 上游服务商
	Model     string
	Body      []byte
	Stream    bool
	Estimated int64
	QuotaKind quota.Kind
	LeaseTTL  time.Duration
	RequestID string
	UserCtx   *UserContext

	// Snap 是本请求的配置快照，在 handler 入口取一次并贯穿全程。
	//
	// 用显式字段而非 context.Value: 配置读取必须在代码 diff 里看得见。
	// 藏进 context 后，「这里读了配置」既不在函数签名上，也躲过编译检查，
	// 而配置读错正是本项目最贵的失效类型 —— 它不报错，只是算错。
	//
	// execute 及其以下全部只读此字段，禁止重新调用 Holder.Current()。
	Snap *confsnap.Snapshot

	// 以下字段由 execute 在尝试结束后回填，供用量流水使用。
	// 单个请求在一个 goroutine 内串行执行，无需加锁。
	finalUsage    *adapter.Usage
	finalKeyID    string
	finalEgressIP string

	// committed 与 committedAmount 记录本次请求**实际从配额里扣掉**的量。
	//
	// 由 attempt 结束租约的地方回填（见 attempt 的 defer）。存在的理由: 让
	// 流水与配额同源。若让 handlers 侧按对外状态码去猜，就会漏掉
	// 「已按预扣量 Commit、但对外返回 502」的那几条路径 —— Redis 说扣了 1、
	// 流水说 0，偏差方向是少记。
	//
	// 累加而非覆盖: 一次用户请求可能换 Key 重试，而每次重试都可能真实消耗
	// 上游额度（例如第一次响应体超限、第二次成功）。逐次累加才能让
	// 流水合计与 Redis 实扣合计严格相等。
	committed       bool
	committedAmount int64
}

// execute 执行一次用户请求，含换 Key 重试。
//
// 返回最终状态码、总重试次数与最后一次的错误（若有）。
func (s *Server) execute(w http.ResponseWriter, r *http.Request, plan *requestPlan) (int, int, *adapter.UpstreamError) {
	// 快照在循环外取一次，循环内严禁重取。
	//
	// 重试期间发生热加载时，若每个 attempt 各取一次快照，同一个用户请求的
	// 三次尝试可能分别用上新旧两套配置 —— 第 1 次用旧 base_url + 旧 mapping，
	// 第 2 次用新 base_url + 旧 adapter。这类混合态不报错，表现为请求打到
	// 一个能连通的地址却发了错的模型名。
	//
	// plan.Snap 已由 handler 入口填好，这里只是把「循环外」这个约束写死。
	snap := plan.Snap

	exclude := make(map[string]bool, snap.Cfg.Upstream.MaxRetries+1)
	var lastErr *adapter.UpstreamError
	maxAttempts := snap.Cfg.Upstream.MaxRetries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// 重试退避必须叠加随机抖动。
			//
			// 反作弊要求: 纯指数退避（200ms/400ms/800ms 精确等差）本身就是
			// 机器特征，风控侧一眼可辨。抖动让重试间隔落在一个区间内而非
			// 固定点上。这不是可选的优化项。
			if !s.sleepBackoff(r.Context(), attempt) {
				// 客户端已断开，无需继续
				return 499, attempt, lastErr
			}
		}

		res := s.attempt(w, r, plan, exclude)

		// 回填流水字段。即便本次失败也要记 —— 用量流水需要知道是哪个 Key
		// 在哪个出口上失败的，否则排障时无法定位问题 Key。
		if res.KeyID != "" {
			plan.finalKeyID = res.KeyID
			plan.finalEgressIP = res.EgressIP
		}
		if !res.Usage.Empty() {
			u := res.Usage
			plan.finalUsage = &u
		}

		if res.Err == nil {
			s.metrics.ObserveRetryCount(attempt)
			return res.StatusCode, attempt, nil
		}
		lastErr = res.Err

		// 已向客户端写出响应（流式已开始，或错误已透传），不能再重试
		if res.Done {
			s.metrics.ObserveRetryCount(attempt)
			return res.StatusCode, attempt, res.Err
		}

		if !res.Err.Class.Retryable() {
			s.metrics.ObserveRetryCount(attempt)
			return res.StatusCode, attempt, res.Err
		}

		if res.KeyID != "" {
			exclude[res.KeyID] = true
		}
		s.metrics.ObserveRetry(res.Err.Class.String())
		s.log.WarnContext(r.Context(), "上游失败，准备换 Key 重试",
			"request_id", plan.RequestID, "attempt", attempt+1,
			"key_id", res.KeyID, "class", res.Err.Class.String(),
			"status", res.Err.StatusCode, "msg", res.Err.Message)
	}

	s.metrics.ObserveRetryCount(maxAttempts - 1)
	// 重试耗尽，透传最后一个错误
	if lastErr == nil {
		lastErr = adapter.NewNetworkError("重试耗尽且无具体错误")
	}
	return lastErr.ClientStatus, maxAttempts - 1, lastErr
}

// sleepBackoff 执行带抖动的退避等待。返回 false 表示 ctx 已取消。
func (s *Server) sleepBackoff(ctx context.Context, attempt int) bool {
	base := s.cfg.Upstream.RetryBaseDelay
	if base <= 0 {
		base = 200 * time.Millisecond
	}
	// 指数增长，但限制上限避免用户等待过久
	backoff := base << (attempt - 1)
	if max := 5 * time.Second; backoff > max {
		backoff = max
	}

	jitter := s.cfg.Upstream.RetryJitter
	if jitter > 0 {
		// 全抖动: 在 [0, jitter) 上均匀取值叠加。相比「等比抖动」，
		// 全抖动打散得更彻底。
		backoff += time.Duration(s.randInt63n(int64(jitter)))
	}

	select {
	case <-ctx.Done():
		return false
	case <-time.After(backoff):
		return true
	}
}

// randInt63n 提供并发安全的随机数。
func (s *Server) randInt63n(n int64) int64 {
	if n <= 0 {
		return 0
	}
	s.randMu.Lock()
	defer s.randMu.Unlock()
	return s.rnd.Int63n(n)
}

// attempt 执行单次上游尝试。
//
// 配额租约的完整生命周期都在本函数内闭合。
func (s *Server) attempt(w http.ResponseWriter, r *http.Request, plan *requestPlan, exclude map[string]bool) (res attemptResult) {
	ctx := r.Context()

	// 用具名返回值 + defer 统一回填出口 IP，避免十余条 return 路径各自填一遍
	// 而必然漏掉某几条。
	var boundIP string
	defer func() { res.EgressIP = boundIP }()

	// ===== 1. 选 Key =====
	selStart := time.Now()
	cand, err := s.sched.Select(ctx, SelectRequest{
		Provider: plan.Provider,
		Model:    plan.Model,
		Kind:     plan.QuotaKind,
		Exclude:  exclude,
	})
	selDur := time.Since(selStart)
	if err != nil {
		if errors.Is(err, ErrNoCandidate) {
			s.metrics.ObserveSchedule("no_candidate", selDur)

			// 把调度器的拒绝原因分类写进日志。
			//
			// 调度器已在错误里带了「已排除=N 不健康=N 间隔不足=N 超硬水位=N
			// 非活跃时段=N」，丢掉它会让运维只看到「配额耗尽」这一种解释，
			// 而实际最常见的原因是「间隔不足」—— Key 数量不足以支撑当前
			// QPS（每 Key 有 MinRequestInterval 的自我节流）。
			//
			// 这两种情况的处置完全相反: 配额耗尽要等 12:00 刷新，
			// 间隔不足要加 Key 或降速。误判会让人枯等一整天。
			s.log.WarnContext(ctx, "无可用 Key",
				"request_id", RequestIDFromContext(ctx),
				"reason", err.Error(),
				"excluded_count", len(exclude))

			// 对外仍只回一句笼统文案: 拒绝原因分布是内部容量信息，
			// 暴露给调用方等于告诉对方「现在有几个 Key、还剩多少额度」。
			return attemptResult{Err: &adapter.UpstreamError{
				Class:        adapter.ErrClassQuota,
				ClientStatus: http.StatusServiceUnavailable,
				Code:         "service_busy",
				Message:      "暂无可用容量，请稍后重试",
			}}
		}
		s.metrics.ObserveSchedule("error", selDur)
		return attemptResult{Err: &adapter.UpstreamError{
			Class:        adapter.ErrClassServer,
			ClientStatus: http.StatusInternalServerError,
			Code:         "internal_error",
			Message:      "调度失败: " + err.Error(),
		}}
	}
	s.metrics.ObserveSchedule("ok", selDur)

	boundIP = cand.EgressIP
	if boundIP == "" {
		// 调度器未给出时以出口池的实际绑定为准
		boundIP = s.egress.BoundIP(cand.KeyID)
	}

	kind := cand.Kind
	if kind == "" {
		kind = plan.QuotaKind
	}

	// ===== 2. 预扣配额 =====
	lim := s.limitsFor(plan.Snap, plan.Provider, kind)
	decision, lease, err := s.quota.Acquire(ctx, plan.Provider, cand.KeyID, kind, plan.Estimated, lim, plan.LeaseTTL)
	if err != nil {
		if errors.Is(err, quota.ErrInsufficient) {
			s.metrics.ObserveQuotaAcquire(string(kind), "denied")
			// 该 Key 额度不足，换下一个。这是正常的调度收敛过程，不是错误。
			return attemptResult{KeyID: cand.KeyID, Err: &adapter.UpstreamError{
				Class:        adapter.ErrClassQuota,
				ClientStatus: http.StatusServiceUnavailable,
				Code:         "service_busy",
				Message:      "Key 配额不足",
			}}
		}
		s.metrics.ObserveQuotaAcquire(string(kind), "error")
		return attemptResult{KeyID: cand.KeyID, Err: &adapter.UpstreamError{
			Class:        adapter.ErrClassServer,
			ClientStatus: http.StatusInternalServerError,
			Code:         "internal_error",
			Message:      "配额预扣失败: " + err.Error(),
		}}
	}
	if decision == quota.GrantedLow {
		s.metrics.ObserveQuotaAcquire(string(kind), "granted_low")
	} else {
		s.metrics.ObserveQuotaAcquire(string(kind), "granted")
	}

	// ===== 租约结束的唯一出口 =====
	//
	// commitActual < 0 表示本次失败，走 Release（不计入 used）。
	// >= 0 表示成功，走 Commit（按实际用量计入）。
	//
	// 用 defer 而非在各分支手动调用: 本函数有十余条 return 路径，包含
	// panic 与 ctx 取消，任何一条漏掉都会永久泄漏 prededuct。
	commitActual := int64(-1)
	defer func() { //nolint:contextcheck // 刻意脱离请求 ctx：客户端断连后仍要归还租约，理由见下方注释
		// 用独立 ctx: 客户端断连时 r.Context() 已取消，用它会导致
		// Release/Commit 立刻失败，租约只能等超时回收 —— 那正是要避免的泄漏。
		endCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if commitActual >= 0 {
			// 先回填流水口径，再落 Commit。顺序无关紧要（两者都不依赖对方的结果），
			// 但必须都执行: 只 Commit 不回填，就是「Redis 说扣了、流水说没扣」。
			plan.committed = true
			plan.committedAmount += commitActual

			if cerr := s.quota.Commit(endCtx, lease, commitActual); cerr != nil {
				s.log.ErrorContext(endCtx, "配额修正失败",
					"request_id", plan.RequestID, "key_id", cand.KeyID,
					"lease", lease.ID, "actual", commitActual, "err", cerr)
			}
			s.metrics.ObserveEstimateError(plan.Estimated, commitActual)
			return
		}
		if rerr := s.quota.Release(endCtx, lease); rerr != nil {
			s.log.ErrorContext(endCtx, "配额释放失败",
				"request_id", plan.RequestID, "key_id", cand.KeyID,
				"lease", lease.ID, "err", rerr)
		}
	}()

	// ===== 3. 获取适配器 =====
	//
	// 从快照取而非 s.adapters: adapter 在构造时把 model_mapping 固化成两张
	// map，与 BaseURL 必须同源。分开取就会出现「新 base_url + 旧 mapping」——
	// 请求发到对的地址、带着错的上游模型名，上游返回 404 或更糟地返回了
	// 另一个模型的结果。
	ad, err := plan.Snap.Adapters.Get(plan.Provider)
	if err != nil {
		return attemptResult{KeyID: cand.KeyID, Err: &adapter.UpstreamError{
			Class:        adapter.ErrClassClient,
			ClientStatus: http.StatusNotFound,
			Code:         "provider_not_found",
			Message:      fmt.Sprintf("未注册的 provider: %s", plan.Provider),
		}}
	}

	// ===== 4. 构造上游请求 =====
	upstreamPath, upstreamBody, terr := ad.TransformRequest(plan.Endpoint, plan.Body)
	if terr != nil {
		var ue *adapter.UpstreamError
		if errors.As(terr, &ue) {
			return attemptResult{KeyID: cand.KeyID, Err: ue}
		}
		return attemptResult{KeyID: cand.KeyID, Err: adapter.NewClientError(
			http.StatusBadRequest, "invalid_request", terr.Error())}
	}

	// 从 provider 配置获取 BaseURL。与上面的 adapter 同取自 plan.Snap，
	// 保证一次请求的全部 attempt 打到同一个地址、用同一套 mapping。
	provider, ok := plan.Snap.Cfg.Providers[plan.Provider]
	if !ok {
		return attemptResult{KeyID: cand.KeyID, Err: adapter.NewClientError(
			http.StatusInternalServerError, "provider_not_found", "provider 配置缺失: "+plan.Provider)}
	}

	url := strings.TrimRight(provider.BaseURL, "/") + upstreamPath
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(upstreamBody))
	if err != nil {
		return attemptResult{KeyID: cand.KeyID, Err: adapter.NewNetworkError("构造上游请求失败: " + err.Error())}
	}
	for k, vs := range ad.BuildAuthHeaders(cand.Secret) {
		for _, v := range vs {
			upReq.Header.Add(k, v)
		}
	}
	// 请求 ID 贯穿到上游，便于跨系统排障
	upReq.Header.Set("X-Request-Id", plan.RequestID)
	if plan.Stream {
		upReq.Header.Set("Accept", "text/event-stream")
	}

	// 每 Key 独立 HTTP 客户端（P1-8: 禁止跨 Key 复用 TCP/TLS 连接）。
	//
	// 传入档位: 正常情况下启动时 restoreBindings 已建好绑定，这里走
	// 「已有绑定直接返回」分支，档位不起作用。但新导入的 Key（尚未经过
	// 一次 Reload+restore）会在此首次绑定，此时必须落到对应档位的出口上，
	// 否则它会被分到任意档位的 IP，分层就有了缺口。
	prevBound := s.egress.BoundIP(cand.KeyID)
	client, err := s.egress.ClientForPool(cand.KeyID, cand.Pool)
	if err != nil {
		s.sched.MarkFailure(cand.KeyID, FailureNetwork)
		return attemptResult{KeyID: cand.KeyID, Err: adapter.NewNetworkError("获取出口客户端失败: " + err.Error())}
	}

	// 惰性绑定一旦在本调用中建立或改绑，必须立刻落库（livetest-ai KI-037）:
	// 两种形态都会走到这 —— ① Key 从未绑定（启动/导入时容量满，恢复失败），
	// 请求来时首次分配；② 原绑定出口已被封（护栏撤离失败仍留在 banned IP 上，
	// 或冷却解禁后），BindInPool 删掉旧绑定重新哈希。两种都只改内存:
	// 重启后 restoreBindings 按库里的空值/旧值恢复，Key 会换到另一个出口，
	// 与「一 Key 一 IP 终身绑定」的承诺相抵，而 /admin/ips（内存权威）与库
	// 长期对不上，只读接口给出与事实相反的证据。
	//
	// 落库失败不阻断请求: 本次运行内存绑定已生效；后台 15s 一轮的对账
	// （collectMetrics 的反向回写）会把它补齐，兜底这一笔的失败。
	// direct 模式下 BoundIP 恒为空串，prevBound 与现值相等，自然不会进来。
	if addr := s.egress.BoundIP(cand.KeyID); addr != prevBound {
		// pErr 而非 err: 本函数外层（请求构造处）还有一个 err 在使用中，
		// govet 的 shadow 检查不允许内层同名遮蔽。
		if pErr := s.store.SetVolcKeyEgressIP(ctx, cand.KeyID, addr); pErr != nil {
			s.log.WarnContext(ctx, "惰性出口绑定落库失败，将依赖后台对账回写",
				"key_id", cand.KeyID, "egress_ip", addr, "err", pErr)
		}
	}

	// ===== 4. 发起请求 =====
	start := time.Now()
	// 记在发出之前而非返回之后: 出口间隔约束的是「请求发出的密度」。
	// 按返回时刻计会让慢请求（SSE 流式动辄几十秒）期间该出口不受任何约束，
	// 恰好在最该限流的时候放开。
	s.egress.MarkUsedBy(cand.KeyID, start)
	upResp, err := client.Do(upReq)
	if err != nil {
		// 连接层失败: 请求没能抵达上游，这是最直接的出口故障信号。
		s.markEgress(cand.KeyID, egressFaulty)
		// 客户端主动断开不应算作 Key 的失败 —— 否则用户频繁取消会误伤 Key 健康分
		if ctx.Err() != nil {
			return attemptResult{Done: true, KeyID: cand.KeyID, StatusCode: 499,
				Err: adapter.NewNetworkError("客户端已断开")}
		}
		s.sched.MarkFailure(cand.KeyID, FailureNetwork)
		return attemptResult{KeyID: cand.KeyID, Err: adapter.NewNetworkError(err.Error())}
	}
	defer upResp.Body.Close()

	// ===== 5. 错误响应处理 =====
	if upResp.StatusCode >= 400 {
		// 错误体通常很小，但仍需限长防御异常大的响应
		errBody, _ := io.ReadAll(io.LimitReader(upResp.Body, 64<<10))
		ue := ad.MapError(upResp.StatusCode, errBody)

		s.markEgress(cand.KeyID, egressVerdictFor(ue.Class))
		// auth 类失败单独走出口封禁判定: 它对单次请求而言与出口无关，
		// 但「多个不同 Key 从同一出口相继被拒」是出口被拉黑的唯一可观测信号。
		if ue.Class == adapter.ErrClassAuth {
			s.detectEgressBan(r.Context(), cand.KeyID)
		}
		s.reportFailure(cand.KeyID, ue.Class)

		return attemptResult{KeyID: cand.KeyID, Err: ue, StatusCode: upResp.StatusCode,
			Estimated: plan.Estimated}
	}

	s.markEgress(cand.KeyID, egressHealthy)
	// 成功即证明「该出口 + 该 Key」的组合可用，撤销此前的 auth 失败记录。
	// 不撤销会让计数随时间单调累积，最终把长期健康的出口误判为被封。
	if ip := s.egress.IPFor(cand.KeyID); ip != nil {
		ip.ClearAuthFailure(cand.KeyID)
	}
	s.sched.MarkSuccess(cand.KeyID)

	// ===== 6. 成功响应转发 =====
	if plan.Stream {
		usage, sErr := s.streamResponse(w, r, upResp, plan, ad, start)
		if sErr != nil {
			// 流式已开始写出，无法再换 Key。有多少用量算多少，
			// 拿不到 usage 时按预扣量保守 Commit —— 上游确实已经消耗了。
			commitActual = actualFor(usage, plan.Estimated, kind)
			return attemptResult{Done: true, HeadersSent: true, KeyID: cand.KeyID,
				StatusCode: 499, Err: sErr, Usage: usage, Estimated: plan.Estimated}
		}
		commitActual = actualFor(usage, plan.Estimated, kind)
		s.metrics.ObserveTokens(plan.Model, usage.PromptTokens, usage.CompletionTokens)
		return attemptResult{Done: true, HeadersSent: true, KeyID: cand.KeyID,
			StatusCode: http.StatusOK, Usage: usage, Estimated: plan.Estimated}
	}

	// 多读 1 字节以区分「恰好等于上限」与「超过上限」。直接按上限截断的
	// 问题: 截断不报错，残缺 JSON 解析 usage 失败 → 按预扣量记账，且截断
	// 的 body 会被原样发给客户端 —— 用户收到一段解析不了的半截 JSON。
	limit := s.cfg.Server.MaxBodyBytes
	body, err := io.ReadAll(io.LimitReader(upResp.Body, limit+1))
	if err != nil {
		// 响应读取失败: 上游可能已经产生了完整用量，按预扣量保守计入
		commitActual = plan.Estimated
		s.sched.MarkFailure(cand.KeyID, FailureNetwork)
		return attemptResult{KeyID: cand.KeyID, Estimated: plan.Estimated,
			Err: adapter.NewNetworkError("读取上游响应失败: " + err.Error())}
	}
	if int64(len(body)) > limit {
		// 超限按上游异常处理: 上游确实消耗了额度，保守按预扣量计入。
		commitActual = plan.Estimated
		s.sched.MarkFailure(cand.KeyID, FailureServer)
		return attemptResult{KeyID: cand.KeyID, Estimated: plan.Estimated,
			Err: &adapter.UpstreamError{
				Class:        adapter.ErrClassServer,
				ClientStatus: http.StatusBadGateway,
				Code:         "upstream_response_too_large",
				Message:      fmt.Sprintf("上游响应超过 %d 字节上限", limit),
			}}
	}

	usage, _ := ad.ParseUsage(plan.Endpoint, body)
	commitActual = actualFor(usage, plan.Estimated, kind)
	s.metrics.ObserveTokens(plan.Model, usage.PromptTokens, usage.CompletionTokens)

	out, _ := ad.TransformResponse(plan.Endpoint, body)

	copyResponseHeaders(w, upResp)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(upResp.StatusCode)
	if _, err := w.Write(out); err != nil {
		s.log.WarnContext(ctx, "写出响应失败", "request_id", plan.RequestID, "err", err)
	}

	return attemptResult{Done: true, HeadersSent: true, KeyID: cand.KeyID,
		StatusCode: upResp.StatusCode, Usage: usage, Estimated: plan.Estimated}
}

// actualFor 决定成功请求应当 Commit 的量。
//
// 拿不到 usage 时按预扣量计入而非按 0: 上游确实消耗了额度，按 0 计入会
// 造成本地水位低于真实值，累积后必然超刷。宁可高估。
func actualFor(u adapter.Usage, estimated int64, kind quota.Kind) int64 {
	if kind == quota.KindCount {
		if u.CountUnits > 0 {
			return u.CountUnits
		}
		return estimated
	}
	if t := u.Total(); t > 0 {
		return t
	}
	return estimated
}

// reportFailure 将错误分类翻译为 Key 状态机事件。
func (s *Server) reportFailure(keyID string, class adapter.ErrorClass) {
	switch class {
	case adapter.ErrClassAuth:
		// 401/403: 立即禁用该 Key（scheduler-solution.md 13.2）
		s.sched.MarkFailure(keyID, FailureAuth)
	case adapter.ErrClassRateLimit:
		s.sched.MarkFailure(keyID, FailureRateLimit)
	case adapter.ErrClassQuota:
		// 上游报额度耗尽: 必须让调度器停止选它，否则会反复撞上同一个 Key
		s.sched.MarkFailure(keyID, FailureQuota)
	case adapter.ErrClassServer:
		s.sched.MarkFailure(keyID, FailureServer)
	case adapter.ErrClassNetwork:
		s.sched.MarkFailure(keyID, FailureNetwork)
	}
}

// detectEgressBan 记录一次 auth 失败，并在达到阈值时撤离整个出口。
//
// 判据是「窗口内有多少个**不同** Key 从这个出口发出的请求被拒鉴权」。
// 单个 Key 反复失败通常是它自己被上游禁用，与出口无关；多个互不相干的 Key
// 相继失败才指向出口被拉黑。只看失败次数会让几个坏 Key 诬陷健康出口。
//
// 撤离在当前 goroutine 内同步完成，不派后台任务:
//
//	撤离只改内存中的绑定映射与连接池，不发网络请求，耗时是微秒级。
//	而异步会引入「判定已达阈值但绑定尚未更新」的窗口，期间后续请求继续
//	从被封出口发出，白白向上游多贡献异常请求 —— 那正是本项目最敏感的信号。
//
// 落库放在撤离之后逐个进行。任一 Key 落库失败只降级告警: 内存中的迁移
// 已生效，本次运行是正确的，重启后 restoreBindings 会把它换回被封出口，
// 但那时该出口已在配置或库中被标记，属于运维需要处理的状态。
func (s *Server) detectEgressBan(ctx context.Context, keyID string) {
	threshold := s.cfg.Egress.BanDetectKeys
	if threshold <= 0 {
		return // 未启用自动判定
	}
	ip := s.egress.IPFor(keyID)
	if ip == nil {
		return // direct 模式或尚无绑定
	}
	// 已经封禁过就不必重复撤离
	if ip.State() == egress.IPBanned {
		return
	}

	window := s.cfg.Egress.BanDetectWindow
	n := ip.MarkAuthFailure(keyID, window)
	if n < threshold {
		return
	}

	// 护栏：不许把最后一个可用出口封掉。
	//
	// 阈值判据只说明「这个出口有问题」，不说明「还有别的出口可用」。两者都成立
	// 才能撤离 —— 否则 EvacuateIP 会先把本出口标记封禁、再把 Key 往外迁，而外面
	// 没有接收方：实测（2026-09-14）两出口同时达阈值时得到 active=0 +
	// moved=0 / failed=3，整池不可用，且日志里只留下一句 WARN。
	//
	// 这里选择「拒绝封禁 + 报错」而不是「封禁但迁到任意出口」：后者会把一批
	// 本该低密度的 Key 压到同一个出口上（正是分层的反面），而且并没有解决
	// 「上游认为我们的出口都有问题」这个根因。宁可保留一个已知有问题的出口
	// 继续试，也不要进入一个必然全量失败的状态。
	if !s.egress.OtherAssignable(ip.Addr) {
		s.log.ErrorContext(ctx, "判定该出口应被封禁，但它是最后一个可用出口，拒绝撤离",
			"egress_ip", ip.Addr, "public_ip", ip.PublicIP,
			"auth_failed_keys", n, "threshold", threshold, "window", window,
			"hint", "需要扩容出口 IP，或人工确认上游是否整体不可用（此时封禁无意义）")
		return
	}

	// 撤离目标取被封出口自身的档位: 其上的 Key 都是该档位的，
	// 迁到同档位才能维持分层。通用出口（PoolAny）则不限档位。
	res, err := s.egress.EvacuateIP(ip.Addr, ip.Pool)
	if err != nil {
		s.log.ErrorContext(ctx, "出口撤离失败",
			"egress_ip", ip.Addr, "err", err)
		return
	}

	s.log.WarnContext(ctx, "判定出口已被上游封禁，已撤离其上全部 Key",
		"egress_ip", ip.Addr, "public_ip", ip.PublicIP,
		"auth_failed_keys", n, "threshold", threshold, "window", window,
		"moved", len(res.Moved), "failed", len(res.Failed))

	// 无处可去的 Key 必须单独告警: 它们仍绑在被封出口上，已不可用且不会自愈。
	for k, e := range res.Failed {
		s.log.ErrorContext(ctx, "Key 无法从被封出口迁出，需扩容目标档位",
			"key_id", k, "banned_ip", ip.Addr, "err", e)
	}

	for k, addr := range res.Moved {
		if err := s.store.SetVolcKeyEgressIP(ctx, k, addr); err != nil {
			s.log.WarnContext(ctx, "撤离后出口落库失败，重启可能回退到被封出口",
				"key_id", k, "egress_ip", addr, "err", err)
		}
	}
}

// egressVerdict 是一次请求对出口 IP 健康度的判定。
//
// 三态而非布尔: 「这次失败与出口无关」既不是成功也不是失败，必须能表达
// 「不记账」。用布尔表达会强迫把无关的失败归入其中一侧 ——
// 归为成功会凭空恢复信誉、掩盖真实的出口问题；归为失败会让健康 IP
// 因为 Key 自身的问题被降级，进而连带影响其上所有 Key。
type egressVerdict int

const (
	// egressHealthy 请求成功抵达上游并正常返回，是出口可用的正面证据。
	egressHealthy egressVerdict = iota
	// egressFaulty 失败原因指向出口本身（连接失败、超时、上游 5xx）。
	egressFaulty
	// egressUnrelated 失败与出口无关（Key 被封、限流、额度耗尽）。
	egressUnrelated
)

// egressVerdictFor 把上游错误分类映射为出口健康度判定。
//
// 分类依据是「换一个出口能否解决」:
//
//	auth      —— Key 无效或被封。同一个 Key 换出口照样被拒，与出口无关。
//	rate_limit —— 上游按 Key 限流。换出口不改变限流状态。
//	quota     —— 该 Key 额度耗尽。与出口无关。
//	server    —— 上游 5xx。可能是该出口被上游侧特殊对待，算出口嫌疑。
//	network   —— 连接失败或超时。最直接的出口故障信号。
//	client    —— 用户请求本身有问题（400/404），与出口无关。
func egressVerdictFor(class adapter.ErrorClass) egressVerdict {
	switch class {
	case adapter.ErrClassAuth, adapter.ErrClassRateLimit,
		adapter.ErrClassQuota, adapter.ErrClassClient:
		return egressUnrelated
	case adapter.ErrClassServer, adapter.ErrClassNetwork:
		return egressFaulty
	default:
		return egressUnrelated
	}
}

// markEgress 更新出口 IP 的健康状态。
func (s *Server) markEgress(keyID string, v egressVerdict) {
	if v == egressUnrelated {
		return
	}
	ip := s.egress.IPFor(keyID)
	if ip == nil {
		return
	}
	if v == egressHealthy {
		ip.MarkSuccess()
	} else {
		ip.MarkFailure()
	}
}

// limitsFor 返回该 provider 在该配额类型下的水位。
//
// 必须带 provider: 各家上游的单 Key 限额可以差一个数量级（火山按 token 给
// 数百万，商汤公测按次给一千多）。忽略 provider 会让额度小的上游被超发 ——
// 本地水位还没到就放行，请求全部撞上游 429；额度大的则被白白闲置。
// 这类偏差不报错，只表现为「配了 quota_limit 却毫无效果」。
//
// 必须带 snap: 水位与本次请求的 base_url、model_mapping 取自同一份配置。
// 从 s.cfg 读会拿到启动时的副本，热切后水位静默不变。
func (s *Server) limitsFor(snap *confsnap.Snapshot, provider string, kind quota.Kind) quota.Limits {
	hard, soft := snap.Cfg.LimitsFor(provider, kind == quota.KindCount)
	return quota.Limits{Hard: hard, Soft: soft}
}

// copyResponseHeaders 透传上游的相关响应头，跳过逐跳头。
func copyResponseHeaders(w http.ResponseWriter, upResp *http.Response) {
	for _, k := range []string{"Content-Type", "X-Request-Id"} {
		if v := upResp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
}

// newRand 构造一个独立随机源。
func newRand() *rand.Rand {
	return rand.New(rand.NewSource(time.Now().UnixNano()))
}
