// Package gateway 实现 OpenAI 兼容的 HTTP 网关。
//
// 请求路径概览:
//
//	用户请求 → 请求 ID → 鉴权 → 用户级限流 → 解析元信息与预扣估算
//	        → [选 Key → 预扣配额 → 上游请求 → 转发/流式转发 → 结束租约]
//	        → 失败则换 Key 重试（带抖动退避）
//	        → 写用量流水
//
// 方括号内的部分可能重复执行至 MaxRetries 次，每次使用不同的 Key。
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/adapter"
	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/confsnap"
	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/gateway/adminapi"
	"github.com/fluxkeys/fluxkeys/internal/httpcore"
	"github.com/fluxkeys/fluxkeys/internal/metrics"
	"github.com/fluxkeys/fluxkeys/internal/quota"
)

// Deps 是 Server 的依赖集合。
type Deps struct {
	Config *config.Config
	Quota  QuotaManager
	Egress *egress.Pool
	Sched  Scheduler
	Store  Store
	// Limiter 可为 nil，此时不做用户级限流。
	Limiter Limiter
	Metrics *metrics.Metrics
	Logger  *slog.Logger
	// AdapterRegistry 是多 provider 适配器注册表。可为 nil，此时按 Config.Providers 自动构造。
	//
	// 已传 Snaps 时本字段被忽略: 快照内的 registry 与其 Config 同源，
	// 再叠一个外部 registry 就等于允许「adapter 与 config 不同源」，
	// 那恰好是热加载要根除的混合态。
	AdapterRegistry *adapter.Registry
	// Snaps 是配置快照持有者。可为 nil，此时按 Config 自行构建一份版本 0 的快照。
	//
	// 请求路径上的全部 provider 配置读取都必须经由它，Config 只留给
	// 冷配置（Server / Admin / Upstream / Egress）。
	Snaps *confsnap.Holder
	// Ready 用于 /readyz 检查 Redis 可达性。可为 nil（此时跳过 Redis 检查）。
	RedisPing func(ctx context.Context) error
}

// Server 是网关 HTTP 服务。
type Server struct {
	// cfg 只用于冷配置: Server / Admin / Upstream / Egress 这些改动本就要
	// 重启进程的项。provider 相关的一切必须走 snaps —— 从这里读会拿到
	// 启动时的副本，热切后静默失效。
	cfg *config.Config
	// snaps 是热配置的唯一来源。请求路径在入口取一次快照并向下传递。
	snaps     *confsnap.Holder
	quota     QuotaManager
	egress    *egress.Pool
	sched     Scheduler
	store     Store
	limiter   Limiter
	metrics   *metrics.Metrics
	log       *slog.Logger
	redisPing func(ctx context.Context) error

	// authCache 缓存鉴权结果，把 Postgres 从每请求关键路径上摘下来。
	authCache *authCache

	mux *http.ServeMux
	srv *http.Server

	randMu sync.Mutex
	rnd    *rand.Rand

	// gate 统一管理「是否正在关闭」与「进行中请求计数」。
	//
	// 两者必须由同一把锁保护，不能拆成 atomic.Bool + sync.WaitGroup:
	// 那样「检查未关闭」与「计数 +1」之间存在窗口，Shutdown 可能在窗口内
	// 开始 Wait，于是这个请求既没被拒绝也没被等待 —— 流式响应会被硬切。
	// sync.WaitGroup 本身也不允许 Add 与 Wait 并发。
	gate requestGate
}

// requestGate 是准入闸门与在途计数。
type requestGate struct {
	mu       sync.Mutex
	closing  bool
	inFlight int
	// drained 在「已置位关闭且在途归零」时被关闭。
	//
	// 用通道而非 sync.Cond: Cond 的等待方在超时放弃后仍挂在等待队列上，
	// 需要额外的放弃标记才能真正退出，反而更容易写错。通道的关闭是一次性
	// 且可被 select 超时，语义正好对上「等到归零，但最多等这么久」。
	drained chan struct{}
}

// enter 尝试进入。返回 false 表示服务正在关闭，应拒绝该请求。
func (g *requestGate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closing {
		return false
	}
	g.inFlight++
	return true
}

// leave 标记一个请求结束。
func (g *requestGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inFlight--
	g.signalIfDrained()
}

// close 置位关闭标记。此后 enter 一律失败，故在途计数只会单调下降。
func (g *requestGate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closing = true
	g.signalIfDrained()
}

// signalIfDrained 在满足排空条件时发信号。调用者必须持有 g.mu。
func (g *requestGate) signalIfDrained() {
	if !g.closing || g.inFlight > 0 || g.drained == nil {
		return
	}
	select {
	case <-g.drained: // 已关闭，避免重复 close 引发 panic
	default:
		close(g.drained)
	}
}

// isClosing 供 /readyz 判断是否应报不就绪。
func (g *requestGate) isClosing() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closing
}

// waitDrained 阻塞直到在途请求归零或 ctx 结束，返回剩余的在途请求数。
//
// 必须在 close 之后调用: 未置位关闭时新请求可以持续进入，等待没有终点。
func (g *requestGate) waitDrained(ctx context.Context) int {
	g.mu.Lock()
	if g.drained == nil {
		g.drained = make(chan struct{})
	}
	ch := g.drained
	g.signalIfDrained()
	g.mu.Unlock()

	select {
	case <-ch:
		return 0
	case <-ctx.Done():
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.inFlight
	}
}

// New 构造网关服务。
func New(d Deps) (*Server, error) {
	if d.Config == nil {
		return nil, errors.New("gateway: Config 不能为 nil")
	}
	if d.Quota == nil {
		return nil, errors.New("gateway: Quota 不能为 nil")
	}
	if d.Sched == nil {
		return nil, errors.New("gateway: Sched 不能为 nil")
	}
	if d.Store == nil {
		return nil, errors.New("gateway: Store 不能为 nil")
	}
	if d.Egress == nil {
		return nil, errors.New("gateway: Egress 不能为 nil")
	}

	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	m := d.Metrics
	if m == nil {
		m = metrics.New()
	}
	// 类型化的 nil 指针装进接口后接口本身非 nil，`s.limiter == nil` 会漏判
	// 而在调用时 panic。在装配处一次性归一，比在每个使用点都判两次可靠。
	lim := d.Limiter
	if rl, ok := lim.(*RateLimiter); ok && rl == nil {
		lim = nil
	}

	// 快照是 config 与 adapter registry 的不可分割组合，故在此一次性构建
	// 而非分别装配: 两者若能各自替换，热切 model_mapping 就会出现
	// 「新 base_url 配旧 mapping」，请求被改写成错误的上游模型名且不报错。
	snaps := d.Snaps
	if snaps == nil {
		var err error
		snaps, err = confsnap.NewHolderFromConfig(d.Config, 0)
		if err != nil {
			return nil, fmt.Errorf("gateway: 构建配置快照: %w", err)
		}
	}

	s := &Server{
		cfg: d.Config, snaps: snaps, quota: d.Quota, egress: d.Egress, sched: d.Sched,
		store: d.Store, limiter: lim, metrics: m, log: log,
		redisPing: d.RedisPing,
		authCache: newAuthCache(authCacheTTL),
		mux:       http.NewServeMux(), rnd: newRand(),
	}

	s.routes()

	s.srv = &http.Server{
		Addr:        d.Config.Server.Addr,
		Handler:     s.mux,
		ReadTimeout: d.Config.Server.ReadTimeout,
		// WriteTimeout 必须为 0: 流式响应可持续数分钟，任何写超时都会
		// 在生成中途掐断连接。超时控制交给 ReadTimeout 与上游客户端超时。
		WriteTimeout:      d.Config.Server.WriteTimeout,
		IdleTimeout:       d.Config.Server.IdleTimeout,
		ReadHeaderTimeout: 15 * time.Second,
	}
	return s, nil
}

// Handler 返回 HTTP 处理器，便于测试直接挂到 httptest.Server。
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	// OpenAI 兼容端点
	s.mux.Handle("/v1/chat/completions", s.chain(s.handleChat, "chat"))
	s.mux.Handle("/v1/images/generations", s.chain(s.handleImages, "images"))
	s.mux.Handle("/v1/embeddings", s.chain(s.handleEmbeddings, "embeddings"))
	s.mux.Handle("/v1/models", s.chain(s.handleModels, "models"))

	// 健康检查不经鉴权与限流
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/readyz", s.handleReadyz)

	// 管理端点。
	//
	// APIKey 为空时完全不注册路由 —— 注册后靠中间件返回 403 是更差的做法:
	// 一旦鉴权中间件因重构失效，管理接口就会裸奔。不注册则连路径都不存在。
	//
	// 具体路由表与各条路由的优先级说明（/admin/keys/ 与 {key_id} 的关系、
	// shard 字面量段的优先级）已随实现迁到 internal/gateway/adminapi/api.go。
	if s.cfg.Admin.APIKey != "" {
		adminapi.Register(s.mux, s)
		s.log.Info("管理接口已启用")
	} else {
		s.log.Warn("admin.api_key 未配置，管理接口已完全禁用")
	}
}

// ===== 中间件 =====

// requestIDKey 是 context 中请求 ID 的键。
type ctxKey int

const (
	// 请求 ID 的 context 键已下沉到 httpcore（管理面子包也要读它）。
	// 本包只剩「已鉴权用户」这一个键。
	ctxKeyUser ctxKey = iota
)

// RequestIDFromContext 取出请求 ID。
//
// 实现已下沉到 httpcore —— 管理面子包同样要读请求 ID，留在本包会让它反向依赖。
// 保留同名薄包装，使本包既有的 20 处调用点无需改动。
func RequestIDFromContext(ctx context.Context) string {
	return httpcore.RequestIDFromContext(ctx)
}

// UserFromContext 取出已鉴权的用户。
func UserFromContext(ctx context.Context) *UserContext {
	if v, ok := ctx.Value(ctxKeyUser).(*UserContext); ok {
		return v
	}
	return nil
}

// chain 组合业务端点的中间件: 请求 ID → 计数 → 鉴权。
func (s *Server) chain(h http.HandlerFunc, endpoint string) http.Handler {
	return s.withRequestID(s.withInFlight(endpoint, s.withAuth(h)))
}

// adminChain 组合管理端点的中间件。
func (s *Server) adminChain(h http.HandlerFunc) http.Handler {
	return s.withRequestID(s.withAdminAuth(h))
}

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return httpcore.WithRequestID(next)
}

func (s *Server) withInFlight(endpoint string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 关闭中拒绝新请求，但已进入的请求（含流式）必须允许跑完。
		// 检查与计数在 enter 内原子完成，不存在「已放行但未计数」的窗口。
		if !s.gate.enter() {
			s.writeError(w, r, http.StatusServiceUnavailable, "service_busy", "服务正在关闭")
			return
		}
		s.metrics.IncInFlight(endpoint)
		defer func() {
			s.metrics.DecInFlight(endpoint)
			s.gate.leave()
		}()
		next.ServeHTTP(w, r)
	})
}

// withAuth 校验用户 API Key。
//
// 结果经进程内缓存（30s TTL + 并发去重），Postgres 从每请求一查降到
// 每 Key 每 TTL 一查。吊销的生效延迟上限即 TTL；管理接口吊销时会主动
// 清空缓存，把延迟压到零。
func (s *Server) withAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			s.writeError(w, r, http.StatusUnauthorized, "invalid_auth",
				"缺少或格式错误的 Authorization 头，应为 Bearer {user_key}")
			return
		}

		uc, fromCache, err := s.authCache.authenticate(r.Context(), token,
			func(ctx context.Context) (*UserContext, error) {
				return s.store.AuthenticateUserKey(ctx, token)
			})
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				s.writeError(w, r, http.StatusUnauthorized, "invalid_auth", "API Key 无效或已吊销")
				return
			}
			s.log.ErrorContext(r.Context(), "鉴权查询失败",
				"request_id", RequestIDFromContext(r.Context()), "err", err)
			s.writeError(w, r, http.StatusInternalServerError, "internal_error", "鉴权服务不可用")
			return
		}

		// 回源成功时顺带异步记一次 last_used_at。挂在「缓存 miss」而非每个
		// 请求上，写频率被钳到每 Key 每 TTL 一次，不会让该行成为热点。
		if !fromCache && uc.APIKeyID > 0 {
			if toucher, ok := s.store.(keyToucher); ok {
				go func(keyID int64) { //nolint:contextcheck // 异步留痕刻意脱离请求 ctx，断连后仍要更新 last_used_at
					tctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					if err := toucher.TouchUserAPIKey(tctx, keyID); err != nil {
						s.log.Debug("更新 API Key 使用时间失败", "key_id", keyID, "err", err)
					}
				}(uc.APIKeyID)
			}
		}

		ctx := context.WithValue(r.Context(), ctxKeyUser, uc)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withAdminAuth 校验管理密钥。
func (s *Server) withAdminAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		// 用常量时间比较避免通过响应时间侧信道推测密钥
		if !ok || !constantTimeEqual(token, s.cfg.Admin.APIKey) {
			s.writeError(w, r, http.StatusUnauthorized, "invalid_auth", "管理密钥无效")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ===== 健康检查 =====

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// 存活检查只反映进程是否活着，不检查依赖 —— 否则 Redis 短暂抖动会
	// 导致容器编排重启一个本来健康的进程。
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	type depStatus struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	deps := make(map[string]depStatus, 2)
	ready := true

	if s.gate.isClosing() {
		// 关闭期间主动报不就绪，让负载均衡先摘流量
		ready = false
		deps["shutdown"] = depStatus{OK: false, Error: "服务正在关闭"}
	}

	if s.redisPing != nil {
		if err := s.redisPing(ctx); err != nil {
			ready = false
			deps["redis"] = depStatus{Error: err.Error()}
			s.metrics.SetDependencyUp("redis", false)
		} else {
			deps["redis"] = depStatus{OK: true}
			s.metrics.SetDependencyUp("redis", true)
		}
	}

	if err := s.store.Ping(ctx); err != nil {
		ready = false
		deps["postgres"] = depStatus{Error: err.Error()}
		s.metrics.SetDependencyUp("postgres", false)
	} else {
		deps["postgres"] = depStatus{OK: true}
		s.metrics.SetDependencyUp("postgres", true)
	}

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ready": ready, "dependencies": deps})
}

// ===== 生命周期 =====

// Start 在后台监听业务端口。
func (s *Server) Start() <-chan error {
	ch := make(chan error, 1)
	go func() {
		// 收集所有 provider 名称用于日志
		providers := make([]string, 0, len(s.cfg.Providers))
		for name := range s.cfg.Providers {
			providers = append(providers, name)
		}
		s.log.Info("网关启动", "addr", s.srv.Addr,
			"egress_mode", string(s.egress.Mode()),
			"providers", providers)
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			ch <- err
			return
		}
		close(ch)
	}()
	return ch
}

// Shutdown 优雅关闭。
//
// 顺序很关键:
//  1. 置关闭标记 → /readyz 报不就绪，负载均衡开始摘流量，新请求被拒
//  2. srv.Shutdown → 停止接受新连接，等待进行中的请求
//  3. 等在途归零 → 覆盖流式请求（Shutdown 不会中断已开始写出的长响应）
//  4. 关闭空闲连接
//
// 用量流水在各请求路径内同步写入，故在途归零即代表流水已提交。
func (s *Server) Shutdown(ctx context.Context) error {
	s.gate.close()
	s.log.Info("开始优雅关闭，停止接收新请求")

	shutErr := s.srv.Shutdown(ctx)

	// 等进行中的请求（含流式）自然结束
	if n := s.gate.waitDrained(ctx); n > 0 {
		s.log.Warn("等待进行中请求超时，强制退出", "in_flight", n)
	} else {
		s.log.Info("所有进行中的请求已完成")
	}

	s.egress.CloseIdle()
	return shutErr
}

// ===== 辅助 =====

func bearerToken(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", false
	}
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		// 部分客户端会漏掉 Bearer 前缀，宽容处理但仍要求非空
		token = auth
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// constantTimeEqual 做常量时间字符串比较。
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// writeError 写出统一格式的错误响应。
//
// 实现已下沉到 httpcore —— 管理面子包也要写同一种错误信封，留在本包会让它
// 反向依赖。保留同名薄包装，使本包既有的 82 处调用点无需改动。
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	httpcore.WriteError(w, r, s.log, status, code, msg)
}

// writeJSON 是对 httpcore.WriteJSON 的薄包装，理由同 writeError
// （本包既有 23 处调用点不动）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	httpcore.WriteJSON(w, status, v)
}

// readBody 读取并限长请求体。
func (s *Server) readBody(r *http.Request) ([]byte, error) {
	limit := s.cfg.Server.MaxBodyBytes
	if limit <= 0 {
		limit = 10 << 20
	}
	// 多读 1 字节以区分「恰好等于上限」和「超过上限」
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("读取请求体失败: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("请求体超过 %d 字节上限", limit)
	}
	return body, nil
}

// quotaKindFor 判断模型在指定 provider 上的计费方式。
//
// 必须带 snap: 此处的返回值决定 Redis key 的 {kind} 段。一次请求内前后两次
// 取值不一致，就会扣费写到一个 key、归档读另一个 key —— 两边都有数、都不
// 报错，只是这次用量永远对不上账。
func (s *Server) quotaKindFor(snap *confsnap.Snapshot, provider, model string) quota.Kind {
	if snap.Cfg.IsCountModel(provider, model) {
		return quota.KindCount
	}
	return quota.KindToken
}

// reasoningEstimateFor 返回模型在指定 provider 上对应的推理预扣修正参数。
//
// 非推理模型返回零值，估算即退化为通用公式，无额外开销。
func (s *Server) reasoningEstimateFor(snap *confsnap.Snapshot, provider, model string) adapter.ReasoningEstimate {
	if !snap.Cfg.IsReasoningModel(provider, model) {
		return adapter.ReasoningEstimate{}
	}
	return adapter.ReasoningEstimate{
		Enabled:          true,
		OutputMultiplier: snap.Cfg.Quota.ReasoningOutputMultiplier,
		FloorTokens:      snap.Cfg.Quota.ReasoningFloorTokens,
	}
}

// leaseTTLFor 返回租约有效期。
//
// 流式请求用更长的 TTL: 长文本生成可持续数分钟，用普通 TTL 会让租约在
// 请求还在进行时被回收，导致同一个 Key 被重复分配额度而超刷。
func (s *Server) leaseTTLFor(stream bool) time.Duration {
	if stream {
		if s.cfg.Quota.StreamLeaseTTL > 0 {
			return s.cfg.Quota.StreamLeaseTTL
		}
		return 600 * time.Second
	}
	if s.cfg.Quota.LeaseTTL > 0 {
		return s.cfg.Quota.LeaseTTL
	}
	return 120 * time.Second
}

// 请求 ID 的生成实现已下沉到 httpcore.NewRequestID。
