// Package mockark 是集成测试用的可控火山 Ark 假上游（测试夹具）。
//
// 存在意义: 全链路的正确性依赖大量「上游异常」路径 —— 额度耗尽熔断、429
// 换 Key、401 禁用 Key、流式中断后的租约回收。这些在真实火山上既无法稳定
// 复现，也不能拿 1000 个生产 Key 去试错。Mock 让这些场景变成确定性测试。
//
// 同时它承担一个无法用其他手段验证的职责: 记录每个请求的源 IP。
// P1-6 的失效模式是静默的（策略路由未配置时流量会全部走主 IP 而不报错），
// 只有在上游侧观察源 IP 才能证明「Key-IP 绑定」真的生效。
//
// 本夹具通常由集成测试在进程内启动（NewServer 返回 http.Handler），
// 无需拉起子进程。它不随发布流程构建镜像、也不出现在任何 compose 里；
// test/mockark/cmd 那个独立入口只供手工联调与本地 e2e 使用，不属产品。
package mockark

import (
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FaultKind 是可注入的故障类型。
type FaultKind string

const (
	FaultNone         FaultKind = ""
	Fault429RateLimit FaultKind = "429_rate_limit" // 纯速率限制
	Fault429Quota     FaultKind = "429_quota"      // 额度耗尽
	Fault401          FaultKind = "401"
	Fault403          FaultKind = "403"
	Fault500          FaultKind = "500"
	Fault503          FaultKind = "503"
	FaultTimeout      FaultKind = "timeout"      // 挂住不响应直至客户端超时
	FaultSlow         FaultKind = "slow"         // 慢响应但最终成功
	FaultStreamAbort  FaultKind = "stream_abort" // 流式中途断开，不发送 usage
	FaultBadJSON      FaultKind = "bad_json"     // 返回非法响应体
)

// Fault 描述一条故障注入规则。
//
// KeyID 与 SourceIP 是两个独立的过滤维度，同时给出时取交集（AND）。
// 二者都为空表示对所有请求生效。
type Fault struct {
	Kind FaultKind `json:"kind"`
	// KeyID 限定生效的 Key（空串表示不按 Key 过滤）。
	KeyID string `json:"key_id,omitempty"`
	// SourceIP 限定生效的请求源 IP（空串表示不按源 IP 过滤）。
	//
	// 这是模拟「整个出口 IP 被上游封禁」的唯一手段。只按 KeyID 过滤时，
	// 要封一个承载了 100 个 Key 的 cold 档 IP 就得注入 100 条规则，
	// 且无法覆盖「此后新绑到该 IP 的 Key 也立即被封」这一真实特征 ——
	// 而后者恰恰是验证备用 IP 迁移容量是否充足的关键场景。
	SourceIP string `json:"source_ip,omitempty"`
	// Remaining 是剩余生效次数，-1 表示永久生效。
	//
	// 按源 IP 封禁时通常要配 -1: 真实封禁不会「生效 N 次后自动解除」，
	// 用有限次数会让被封 IP 上的 Key 在耗尽次数后悄悄恢复，
	// 掩盖掉迁移逻辑本该暴露的问题。
	Remaining int `json:"remaining"`
	// Delay 是 slow/timeout 故障的等待时长。
	Delay time.Duration `json:"delay,omitempty"`
}

// KeyAccount 是单个 Key 在 Mock 侧的独立账本。
//
// 每个 Key 独立记账是验证「安心模式熔断」的前提: 必须能让某个 Key 真的
// 耗尽额度并返回火山格式的额度错误，才能测出网关是否正确切换 Key。
type KeyAccount struct {
	KeyID string `json:"key_id"`
	// TokenLimit 为 0 表示不限额。
	TokenLimit int64 `json:"token_limit"`
	TokenUsed  int64 `json:"token_used"`
	CountLimit int64 `json:"count_limit"`
	CountUsed  int64 `json:"count_used"`
	Requests   int64 `json:"requests"`
	Errors     int64 `json:"errors"`
	// Disabled 模拟 Key 被封禁，一律返回 401。
	Disabled bool `json:"disabled"`
	// SourceIPs 记录该 Key 请求过的源 IP 及次数。
	SourceIPs map[string]int64 `json:"source_ips"`
	LastSeen  time.Time        `json:"last_seen"`
}

// RequestLog 是单条请求的审计记录。
type RequestLog struct {
	At         time.Time `json:"at"`
	KeyID      string    `json:"key_id"`
	SourceIP   string    `json:"source_ip"`
	Path       string    `json:"path"`
	Model      string    `json:"model"`
	Stream     bool      `json:"stream"`
	Status     int       `json:"status"`
	Fault      FaultKind `json:"fault,omitempty"`
	Prompt     int64     `json:"prompt_tokens"`
	Completion int64     `json:"completion_tokens"`
	RequestID  string    `json:"request_id"`
}

// Options 是 Mock 服务的构造参数。
type Options struct {
	// DefaultTokenLimit 是未显式注册的 Key 的默认额度（0 = 不限）。
	DefaultTokenLimit int64
	// DefaultCountLimit 是未显式注册的 Key 的默认次数额度。
	DefaultCountLimit int64
	// StreamChunkDelay 是流式 chunk 之间的间隔，模拟真实生成速度。
	StreamChunkDelay time.Duration
	// StreamChunks 是每次流式响应的内容 chunk 数。
	StreamChunks int
	// MaxLogs 限制审计日志的保留条数。
	MaxLogs int
	// Models 是 /api/v3/models 返回的模型列表。
	Models []string
	// Seed 固定随机源，让 completion_tokens 可复现。
	Seed int64
}

// Server 是 Mock Ark 服务。
type Server struct {
	opt Options
	mux *http.ServeMux

	mu       sync.Mutex
	accounts map[string]*KeyAccount
	faults   []*Fault
	logs     []RequestLog
	rnd      *rand.Rand
	// seq 用于生成递增的响应 ID。
	seq int64
}

// NewServer 构造 Mock 服务。
func NewServer(opt Options) *Server {
	if opt.StreamChunks <= 0 {
		opt.StreamChunks = 5
	}
	if opt.MaxLogs <= 0 {
		opt.MaxLogs = 5000
	}
	if len(opt.Models) == 0 {
		opt.Models = []string{"deepseek-v3-241226", "doubao-pro-32k-241215", "seedream-3.0", "doubao-embedding"}
	}
	if opt.Seed == 0 {
		opt.Seed = 42
	}

	s := &Server{
		opt:      opt,
		mux:      http.NewServeMux(),
		accounts: make(map[string]*KeyAccount),
		rnd:      rand.New(rand.NewSource(opt.Seed)),
	}

	s.mux.HandleFunc("/api/v3/chat/completions", s.handleChat)
	s.mux.HandleFunc("/api/v3/images/generations", s.handleImages)
	s.mux.HandleFunc("/api/v3/embeddings", s.handleEmbeddings)
	s.mux.HandleFunc("/api/v3/models", s.handleModels)

	// 控制面
	s.mux.HandleFunc("/_mock/stats", s.handleStats)
	s.mux.HandleFunc("/_mock/reset", s.handleReset)
	s.mux.HandleFunc("/_mock/inject", s.handleInject)
	s.mux.HandleFunc("/_mock/keys", s.handleKeys)
	s.mux.HandleFunc("/_mock/logs", s.handleLogs)

	return s
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// RegisterKey 预置一个 Key 的额度。
func (s *Server) RegisterKey(keyID string, tokenLimit, countLimit int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[keyID] = &KeyAccount{
		KeyID: keyID, TokenLimit: tokenLimit, CountLimit: countLimit,
		SourceIPs: make(map[string]int64),
	}
}

// SetKeyDisabled 模拟 Key 被封禁。
func (s *Server) SetKeyDisabled(keyID string, disabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.account(keyID).Disabled = disabled
}

// Inject 追加一条故障注入规则。
//
// Remaining 为 0 时兜底成 1（只生效一次）。按源 IP 封禁时几乎总要
// 显式传 -1，否则规则会在第一个请求后消失 —— 用 BanSourceIP 更稳妥。
func (s *Server) Inject(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Remaining == 0 {
		f.Remaining = 1
	}
	s.faults = append(s.faults, &f)
}

// BanSourceIP 模拟某个出口 IP 被上游封禁，对该 IP 发出的所有请求返回 403。
//
// Remaining 固定为 -1: 真实封禁不会「生效 N 次后自动解除」。用有限次数会让
// 该 IP 上的 Key 在耗尽次数后悄悄恢复，掩盖掉迁移逻辑本该暴露的问题。
//
// ⚠️ 用它验证「IP 被封后 Key 迁移到健康出口」时会发现走不通，这不是本方法
// 的缺陷，而是网关当前的真实行为:
//
//	403 在 adapter 侧归为 ErrClassAuth（adapter.go:68「Key 无效或被封」），
//	proxy.go:359 又刻意把 auth 类排除在出口信誉记账之外 ——
//	  markEgress(keyID, ue.Class != ErrClassAuth && ue.Class != ErrClassRateLimit)
//	于是 403 的后果是**禁用该 Key**（FailureAuth），出口 IP 的信誉分毫发无损。
//
//	这个取舍本身是合理的: 上游返回 401/403 时无法区分「这个 Key 被封了」
//	与「这个 IP 被封了」，而错误地降级一个健康 IP 会连带影响其上所有 Key。
//	但代价是**出口级封禁目前无法被自动识别**，只能靠运维观察到「某个 IP 上
//	的 Key 成批变 invalid」后手动介入。
//
// 因此本方法的实际用途是复现这一现象供分析，而非验证自动迁移 ——
// 自动迁移尚未实现。要让它可行，需要一个跨 Key 的关联判据，例如
// 「同一出口上 N 个不同 Key 在短时间内相继 auth 失败 → 判定为 IP 被封」。
func (s *Server) BanSourceIP(addr string) {
	s.Inject(Fault{Kind: Fault403, SourceIP: addr, Remaining: -1})
}

// activeFaults 返回当前仍生效的故障规则快照。
//
// 返回值拷贝而非指针: 调用方拿到指针会绕过 mu 直接改 Remaining，
// 与 pickFault 的递减形成数据竞争。
func (s *Server) activeFaults() []Fault {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Fault, 0, len(s.faults))
	for _, f := range s.faults {
		out = append(out, *f)
	}
	return out
}

// Reset 清空所有账本、故障与日志。
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts = make(map[string]*KeyAccount)
	s.faults = nil
	s.logs = nil
	s.seq = 0
	s.rnd = rand.New(rand.NewSource(s.opt.Seed))
}

// Account 返回某 Key 的账本副本。
func (s *Server) Account(keyID string) KeyAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.account(keyID)
	cp := *a
	cp.SourceIPs = make(map[string]int64, len(a.SourceIPs))
	for k, v := range a.SourceIPs {
		cp.SourceIPs[k] = v
	}
	return cp
}

// Logs 返回请求日志副本。
func (s *Server) Logs() []RequestLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RequestLog, len(s.logs))
	copy(out, s.logs)
	return out
}

// account 返回（必要时创建）某 Key 的账本。调用方须持锁。
func (s *Server) account(keyID string) *KeyAccount {
	a, ok := s.accounts[keyID]
	if !ok {
		a = &KeyAccount{
			KeyID:      keyID,
			TokenLimit: s.opt.DefaultTokenLimit,
			CountLimit: s.opt.DefaultCountLimit,
			SourceIPs:  make(map[string]int64),
		}
		s.accounts[keyID] = a
	}
	if a.SourceIPs == nil {
		a.SourceIPs = make(map[string]int64)
	}
	return a
}

// extractKeyID 从 Authorization 头中取出 Key 标识。
//
// Mock 约定密钥形如 "sk-mock-{key_id}"，从而无需维护密钥表就能按 Key 记账。
// 不符合该格式时用整个密钥作为标识。
func extractKeyID(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return "", false
	}
	secret := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if secret == "" {
		return "", false
	}
	if id, ok := strings.CutPrefix(secret, "sk-mock-"); ok && id != "" {
		return id, true
	}
	return secret, true
}

// sourceIP 提取请求的源 IP。这是验证出口绑定是否生效的唯一可信数据。
func sourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// pickFault 取出对该 Key 与源 IP 生效的故障，并递减其剩余次数。
//
// 请求头 X-Mock-Fault 的优先级高于注册的规则 —— 测试里逐请求指定故障
// 比先注册再发请求更直观。
func (s *Server) pickFault(keyID, srcIP string, r *http.Request) Fault {
	if h := r.Header.Get("X-Mock-Fault"); h != "" {
		f := Fault{Kind: FaultKind(h), Remaining: 1}
		if d := r.Header.Get("X-Mock-Delay"); d != "" {
			if ms, err := strconv.Atoi(d); err == nil {
				f.Delay = time.Duration(ms) * time.Millisecond
			}
		}
		return f
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for i, f := range s.faults {
		if !f.matches(keyID, srcIP) {
			continue
		}
		if f.Remaining == 0 {
			continue
		}
		out := *f
		if f.Remaining > 0 {
			f.Remaining--
			if f.Remaining == 0 {
				s.faults = append(s.faults[:i:i], s.faults[i+1:]...)
			}
		}
		return out
	}
	return Fault{}
}

// matches 判断该规则是否适用于给定的 Key 与源 IP。
//
// 两个维度取交集: `{key_id: k, source_ip: ip}` 只命中「k 从 ip 发出的请求」。
// 这允许表达「某个 Key 从某个出口发的请求被拒，换出口后正常」——
// 即出口级封禁的精确形态。
func (f *Fault) matches(keyID, srcIP string) bool {
	if f.KeyID != "" && f.KeyID != keyID {
		return false
	}
	if f.SourceIP != "" && f.SourceIP != srcIP {
		return false
	}
	return true
}

func (s *Server) nextID(prefix string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return fmt.Sprintf("%s-mock%06d", prefix, s.seq)
}

func (s *Server) appendLog(l RequestLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.logs) >= s.opt.MaxLogs {
		// 丢弃最旧的一半，避免长跑测试无界增长
		s.logs = append(s.logs[:0], s.logs[len(s.logs)/2:]...)
	}
	s.logs = append(s.logs, l)
}

// recordUsage 更新 Key 账本，返回是否已超额。
func (s *Server) recordUsage(keyID, srcIP string, tokens, count int64, isErr bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.account(keyID)
	a.Requests++
	a.TokenUsed += tokens
	a.CountUsed += count
	a.LastSeen = time.Now()
	if srcIP != "" {
		a.SourceIPs[srcIP]++
	}
	if isErr {
		a.Errors++
	}
}

// checkQuota 判断该 Key 是否已耗尽额度。
func (s *Server) checkQuota(keyID string, kind string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.account(keyID)
	switch kind {
	case "count":
		return a.CountLimit > 0 && a.CountUsed >= a.CountLimit
	default:
		return a.TokenLimit > 0 && a.TokenUsed >= a.TokenLimit
	}
}

// isDisabled 判断 Key 是否被禁用。
func (s *Server) isDisabled(keyID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.account(keyID).Disabled
}
