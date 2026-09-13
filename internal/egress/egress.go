// Package egress 负责出口 IP 的选择与绑定。
//
// 设计背景（P0-5 / P1-6 / P1-8）:
//
//   - 反封禁的核心诉求是「每个 Key 表现为一个独立用户」，因此每个 Key 必须
//     终身绑定固定的出口 IP，且不同 Key 之间不得复用 TCP/TLS 连接 ——
//     共享连接会让多个 Key 暴露同一 JA3 指纹，等于在传输层坐实「同一台机器」。
//
//   - EIP 绑定在特定主机的弹性网卡上，Pod 漂移后无法使用该主机的辅助 IP，
//     故多副本部署下「Key-IP 终身绑定」物理上不可实现 —— MVP 锁定单实例。
//
//   - 云厂商绑定辅助私网 IP 后，操作系统默认不会自动配置到网卡，也不会建立
//     策略路由，此时所有出口流量仍走主 IP，按 Key 绑定出口的设计会整体失效。
//     必须配合 scripts/setup-egress.sh 配置 ip rule + per-IP 路由表。
//
// Provider 抽象使得本地/CI 环境可用 direct 模式跑通全链路，生产切 multi-ip
// 而无需改动任何调用方代码。
package egress

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Mode 决定出口 IP 的实现策略。
type Mode string

const (
	// ModeDirect 不绑定本地地址，所有请求走系统默认路由。
	// 用于本地开发与 CI —— 能跑通全链路，但无出口隔离能力。
	ModeDirect Mode = "direct"

	// ModeMultiIP 将每个 Key 绑定到宿主机的一个辅助 IP。生产形态。
	ModeMultiIP Mode = "multi_ip"
)

// IPState 描述一个出口 IP 的可用状态。
type IPState string

const (
	IPActive   IPState = "active"   // 正常服务
	IPSuspect  IPState = "suspect"  // 探测失败 1 次，观察中
	IPCooldown IPState = "cooldown" // 连续失败，暂停分配
	// IPBanned 表示判定被上游封禁，停止分配。
	//
	// 不是终态: 配了 ban_cooldown 时会在冷却期届满后由 TryUnban 转入
	// cooldown 重新探测（反复被封则指数延长冷却）。未配则为永久废弃，
	// 需人工介入 —— 这两种情形在 IPStat 里靠 UnbanAt 是否为零区分。
	IPBanned IPState = "banned"
)

// PoolAny 表示该出口 IP 不限定 Key 的池归属，任何 Key 都可落在其上。
const PoolAny = ""

// IP 是出口 IP 池中的一个条目。
type IP struct {
	// Addr 是绑定到本机网卡的地址（辅助私网 IP），用作 TCP 源地址。
	Addr string
	// PublicIP 是该私网 IP 对应的弹性公网 IP，仅用于展示与审计。
	PublicIP string
	// MaxKeys 限制该 IP 下可绑定的 Key 数量。
	MaxKeys int
	// Pool 限定该 IP 只承接哪一档 Key（hot / warm / cold）。
	//
	// PoolAny 表示不限定。分层的目的是让活跃度差异巨大的 Key 物理隔离:
	// cold 档的 Key 几乎不发请求，单 IP 可承载上百个；hot 档必须保持低密度。
	// 若混放，一个高频 Key 会与上百个几乎静默的 Key 共享出口，分层收益归零。
	Pool string

	mu         sync.RWMutex
	state      IPState
	reputation int
	failStreak int
	lastCheck  time.Time
	// authFails 记录近期发生鉴权失败的 Key 及其最后失败时刻。
	//
	// 存 Key 集合而非计数是判据的核心: 单个 Key 反复 auth 失败通常是它自己
	// 被上游禁用（密钥泄露、账号异常），与出口无关；而**多个互不相干的 Key
	// 从同一出口相继 auth 失败**才指向「这个 IP 被上游拉黑」。
	// 只看次数会让几个坏 Key 诬陷一个健康 IP，代价是其上所有 Key 被迫换出口。
	authFails map[string]time.Time
	// lastUsed 是该出口最近一次被选中发起请求的时刻。
	//
	// 调度器的 MinRequestInterval 只约束单个 Key，约束不了出口:
	// 一个 IP 上 25 个 Key 各自满足 5 秒间隔，IP 层面仍可达 5 QPS。
	// 而风控看到的是 IP —— 同一地址每秒冒出好几个请求，无论背后是几个账号，
	// 都不像真人在用。故出口自身也要记账并限速。
	lastUsed time.Time
	// bannedAt 是最近一次被判定封禁的时刻，零值表示从未被封。
	bannedAt time.Time
	// banCount 是历史被封次数，用于对反复被封的出口指数延长冷却。
	//
	// 不清零是刻意的: 一个反复被封的 IP 说明它在上游眼里已经脏了，
	// 每次都按初始冷却时长放它回来，只会反复给上游送异常请求。
	banCount int
}

// NewIP 构造一个初始状态为 active、信誉满分的通用出口 IP。
func NewIP(addr, publicIP string, maxKeys int) *IP {
	return NewPooledIP(addr, publicIP, maxKeys, PoolAny)
}

// NewPooledIP 构造一个限定池归属的出口 IP。
func NewPooledIP(addr, publicIP string, maxKeys int, pool string) *IP {
	if maxKeys <= 0 {
		maxKeys = 10
	}
	return &IP{
		Addr: addr, PublicIP: publicIP, MaxKeys: maxKeys, Pool: pool,
		state: IPActive, reputation: 100,
	}
}

// Accepts 判断该 IP 是否愿意承接指定档位的 Key。
//
// 通用 IP（Pool 为 PoolAny）接受任何 Key；调用方未指明档位时同样退化为接受，
// 这样未启用分层的部署与旧调用方无需改动即可继续工作。
func (ip *IP) Accepts(pool string) bool {
	return ip.Pool == PoolAny || pool == PoolAny || ip.Pool == pool
}

// LastUsed 返回该出口最近一次发起请求的时刻，从未使用过时返回零值。
func (ip *IP) LastUsed() time.Time {
	ip.mu.RLock()
	defer ip.mu.RUnlock()
	return ip.lastUsed
}

// MarkUsed 记录该出口在 now 时刻被用于发起请求。
//
// 与 MarkSuccess 分开: 出口节奏的判据是「发过请求」而非「请求成功」。
// 失败的请求同样落在上游的日志里，同样构成该 IP 的请求密度。
func (ip *IP) MarkUsed(now time.Time) {
	ip.mu.Lock()
	defer ip.mu.Unlock()
	if now.After(ip.lastUsed) {
		ip.lastUsed = now
	}
}

// State 返回当前状态。
func (ip *IP) State() IPState {
	ip.mu.RLock()
	defer ip.mu.RUnlock()
	return ip.state
}

// Reputation 返回当前信誉分（0-100）。
func (ip *IP) Reputation() int {
	ip.mu.RLock()
	defer ip.mu.RUnlock()
	return ip.reputation
}

// Assignable 判断该 IP 是否可继续承接新 Key。
func (ip *IP) Assignable() bool {
	ip.mu.RLock()
	defer ip.mu.RUnlock()
	// 信誉低于 50 进入观察期，不再分配新 Key
	return ip.state == IPActive && ip.reputation >= 50
}

// MarkSuccess 记录一次成功请求，逐步恢复信誉。
func (ip *IP) MarkSuccess() {
	ip.mu.Lock()
	defer ip.mu.Unlock()
	ip.failStreak = 0
	ip.lastCheck = time.Now()
	if ip.state == IPSuspect || ip.state == IPCooldown {
		ip.state = IPActive
	}
}

// ClearAuthFailure 撤销某个 Key 在本出口上的 auth 失败记录。
//
// 该 Key 从本出口成功发出过请求，即证明「出口 + 该 Key」这个组合是好的，
// 此前的 auth 失败不应再计入「多少个不同 Key 失败了」这一判据。
// 不清理会让计数随时间单调累积，最终把一个长期健康的出口误判为被封。
func (ip *IP) ClearAuthFailure(keyID string) {
	ip.mu.Lock()
	defer ip.mu.Unlock()
	delete(ip.authFails, keyID)
}

// MarkFailure 记录一次失败，按连续失败次数推进状态机。
func (ip *IP) MarkFailure() {
	ip.mu.Lock()
	defer ip.mu.Unlock()
	ip.failStreak++
	ip.lastCheck = time.Now()

	// banned 只能由 TryUnban 退出，失败不得把它降级。
	//
	// 不加这一条会绕过整个冷却机制: MarkBanned 不重置 failStreak，
	// 所以刚被封的出口只要探测失败一次，failStreak 就落进下面的 default
	// 分支被改成 suspect —— 而 suspect 只需一次探测成功就回 active，
	// 冷却时长与指数退避全部失效，bannedAt 记录也形同虚设。
	if ip.state == IPBanned {
		ip.reputation = 0
		return
	}

	switch {
	case ip.failStreak >= 6:
		ip.state = IPBanned
		ip.bannedAt = time.Now()
		ip.banCount++
		ip.reputation = 0
	case ip.failStreak >= 3:
		ip.state = IPCooldown
		ip.reputation -= 10
	default:
		ip.state = IPSuspect
	}
	if ip.reputation < 0 {
		ip.reputation = 0
	}
}

// MarkAuthFailure 记录某个 Key 从本出口发出的请求遭到鉴权拒绝（401/403）。
//
// 返回窗口内发生过 auth 失败的**不同 Key 数**，供调用方判断是否达到
// 「整个出口被上游拉黑」的阈值。
//
// 这里刻意不推进信誉状态机: 上游返回 401/403 时无法区分「这个 Key 被封」
// 与「这个 IP 被封」，直接扣分会让几个坏 Key 拖垮一个健康出口，
// 而该出口上其余 Key 会被迫换 IP —— 那正是风控最敏感的信号。
// 只有当不同 Key 数达到阈值时，调用方才应调 MarkBanned。
func (ip *IP) MarkAuthFailure(keyID string, window time.Duration) int {
	ip.mu.Lock()
	defer ip.mu.Unlock()

	if ip.authFails == nil {
		ip.authFails = make(map[string]time.Time)
	}
	now := time.Now()
	ip.authFails[keyID] = now

	// 顺带清理过期记录。出口池规模有限（单机几十个 IP），
	// 每个 IP 的 Key 数也有上限，全量遍历成本可忽略。
	if window > 0 {
		for k, at := range ip.authFails {
			if now.Sub(at) > window {
				delete(ip.authFails, k)
			}
		}
	}
	return len(ip.authFails)
}

// AuthFailKeys 返回窗口内发生过鉴权失败的不同 Key 数。
func (ip *IP) AuthFailKeys(window time.Duration) int {
	ip.mu.Lock()
	defer ip.mu.Unlock()
	if window <= 0 {
		return len(ip.authFails)
	}
	now := time.Now()
	n := 0
	for _, at := range ip.authFails {
		if now.Sub(at) <= window {
			n++
		}
	}
	return n
}

// MarkBanned 在确认该 IP 被上游封禁时调用。
func (ip *IP) MarkBanned() {
	ip.mu.Lock()
	defer ip.mu.Unlock()
	ip.state = IPBanned
	ip.bannedAt = time.Now()
	ip.banCount++
	ip.reputation -= 30
	if ip.reputation < 0 {
		ip.reputation = 0
	}
}

// BannedAt 返回最近一次被判定封禁的时刻，零值表示从未被封。
func (ip *IP) BannedAt() time.Time {
	ip.mu.RLock()
	defer ip.mu.RUnlock()
	return ip.bannedAt
}

// BanCount 返回历史被封次数。
func (ip *IP) BanCount() int {
	ip.mu.RLock()
	defer ip.mu.RUnlock()
	return ip.banCount
}

// TryUnban 在封禁时长届满后把出口转入 cooldown，让它有机会被重新探测。
//
// 返回是否实际发生了状态变更。
//
// 为什么转 cooldown 而不是直接 active:
// cooldown 不满足 Assignable，所以不会立刻涌入一批新 Key —— 若该 IP 其实
// 仍被上游封禁，此时放 25 个 Key 回去等于再送一轮异常请求。cooldown 状态
// 下健康探测仍会执行，探测成功才由 MarkSuccess 转回 active。
//
// base 是首次封禁的冷却时长，每次重复封禁翻倍，上限 maxWait。
// 反复被封说明该 IP 在上游眼里已经脏了，按初始时长反复放行只会重复受损。
func (ip *IP) TryUnban(now time.Time, base, maxWait time.Duration) bool {
	if base <= 0 {
		return false // 未启用自动恢复
	}
	ip.mu.Lock()
	defer ip.mu.Unlock()
	if ip.state != IPBanned || ip.bannedAt.IsZero() {
		return false
	}

	if now.Sub(ip.bannedAt) < backoff(ip.banCount, base, maxWait) {
		return false
	}

	ip.state = IPCooldown
	ip.failStreak = 0
	return true
}

// backoff 按被封次数计算本次应等待的时长，上限 maxWait。
//
// 单独抽出而非内联在 TryUnban 里，是为了让 BanInfo 能算出「预计解封时刻」
// 并展示给运维 —— 两处各写一遍指数退避，改了一处忘另一处时，
// 面板显示的解封时间会与实际行为不符，而这种偏差没有任何报错。
func backoff(banCount int, base, maxWait time.Duration) time.Duration {
	wait := base
	// banCount 已在 MarkBanned 里自增，故第 1 次封禁时它是 1，退避指数为 0。
	for i := 1; i < banCount && wait < maxWait; i++ {
		wait *= 2
	}
	if wait > maxWait {
		wait = maxWait
	}
	return wait
}

// BanInfo 返回封禁时刻、累计被封次数，以及按当前退避计算的预计解封时刻。
//
// 运维看到 banned 时最需要判断的是「等着自愈还是现在就介入」。
// 只给状态不给时间，第 1 次被封（等 30 分钟）与第 5 次被封（等 8 小时）
// 在面板上完全一样，只能靠翻日志区分。
//
// base <= 0（未启用自动恢复）时 unbanAt 为零值 —— 该出口不会自愈。
func (ip *IP) BanInfo(base, maxWait time.Duration) (bannedAt time.Time, banCount int, unbanAt time.Time) {
	ip.mu.RLock()
	defer ip.mu.RUnlock()
	if ip.bannedAt.IsZero() || base <= 0 {
		return ip.bannedAt, ip.banCount, time.Time{}
	}
	return ip.bannedAt, ip.banCount, ip.bannedAt.Add(backoff(ip.banCount, base, maxWait))
}

// ErrNoIP 表示池中没有可用出口 IP。
var ErrNoIP = errors.New("egress: no assignable ip")

// Pool 管理出口 IP 与 Key 的绑定关系，并为每个 Key 提供独立的 HTTP 客户端。
type Pool struct {
	mode    Mode
	timeout time.Duration

	// 自动解封的退避参数。存在 Pool 上而非作为 Stats 的参数，
	// 是为了不改 Stats() 的签名 —— 它有 4 处调用（admin / background /
	// 容量测试 / 单测），而这两个值全程不变，作为参数逐层透传纯属噪音。
	unbanBase time.Duration
	unbanMax  time.Duration

	mu       sync.RWMutex
	ips      []*IP
	byAddr   map[string]*IP
	bindings map[string]string       // key_id -> ip addr
	clients  map[string]*http.Client // key_id -> 独立客户端（不跨 Key 复用连接）
}

// NewPool 创建出口池。direct 模式下 ips 可为空。
func NewPool(mode Mode, ips []*IP, timeout time.Duration) (*Pool, error) {
	if mode == ModeMultiIP && len(ips) == 0 {
		return nil, errors.New("egress: multi_ip 模式必须提供至少一个出口 IP")
	}
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	p := &Pool{
		mode: mode, timeout: timeout,
		byAddr:   make(map[string]*IP, len(ips)),
		bindings: make(map[string]string),
		clients:  make(map[string]*http.Client),
	}
	for _, ip := range ips {
		if _, dup := p.byAddr[ip.Addr]; dup {
			return nil, fmt.Errorf("egress: 出口 IP 重复 %s", ip.Addr)
		}
		p.ips = append(p.ips, ip)
		p.byAddr[ip.Addr] = ip
	}
	sort.Slice(p.ips, func(i, j int) bool { return p.ips[i].Addr < p.ips[j].Addr })
	return p, nil
}

// Mode 返回当前运行模式。
func (p *Pool) Mode() Mode { return p.mode }

// Bind 为 Key 分配（或返回已有的）出口 IP，不限定池归属。
//
// 等价于 BindInPool(keyID, PoolAny)，供不关心档位的调用方使用（测试与
// 一次性工具）。生产路径一律走 BindInPool —— 档位是池化隔离的一部分，
// 不声明档位等于放弃该 Key 的画像归属。
func (p *Pool) Bind(keyID string) (string, error) {
	return p.BindInPool(keyID, PoolAny)
}

// BindInPool 在指定档位（hot / warm / cold）内为 Key 分配出口 IP。
//
// 分配是确定性的: 以 key_id 哈希落到候选 IP 上，保证同一候选集下结果不变。
// 仅当目标 IP 不可用时才回退到次优 IP。
//
// 注意「终身绑定」并不单靠本方法成立 —— 候选集会随 IP 填满与封禁而变化，
// 哈希落点随之改变。真正的终身绑定依赖 Adopt 从库中恢复历史绑定，
// 本方法只负责为**尚无绑定记录**的 Key 做首次分配。
func (p *Pool) BindInPool(keyID, pool string) (string, error) {
	if p.mode == ModeDirect {
		return "", nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if addr, ok := p.bindings[keyID]; ok {
		if ip := p.byAddr[addr]; ip != nil && ip.State() != IPBanned {
			return addr, nil
		}
		// 原绑定 IP 已被封，需重新分配
		delete(p.bindings, keyID)
		delete(p.clients, keyID)
	}

	candidates := make([]*IP, 0, len(p.ips))
	load := make(map[string]int, len(p.ips))
	for _, boundAddr := range p.bindings {
		load[boundAddr]++
	}
	for _, ip := range p.ips {
		if ip.Assignable() && ip.Accepts(pool) && load[ip.Addr] < ip.MaxKeys {
			candidates = append(candidates, ip)
		}
	}
	if len(candidates) == 0 {
		if pool != PoolAny {
			return "", fmt.Errorf("%w: 档位 %q 无可用出口", ErrNoIP, pool)
		}
		return "", ErrNoIP
	}

	// 确定性哈希，同一候选集下结果一致。
	// 取 uint32 余数后再转 int，避免 32 位平台上 int(h.Sum32()) 溢出为负数
	// 导致 candidates[start] 越界 panic。
	h := fnv.New32a()
	_, _ = h.Write([]byte(keyID + "|primary_ip_salt"))
	start := int(h.Sum32() % uint32(len(candidates)))

	chosen := candidates[start]
	p.bindings[keyID] = chosen.Addr
	return chosen.Addr, nil
}

// Adopt 采纳一条来自持久层的既有 Key-IP 绑定。
//
// 这是「Key-IP 终身绑定」真正成立的关键: 重启后若靠 BindInPool 重新哈希，
// 候选集的任何变化（新增 IP、某 IP 被封、绑定顺序不同）都会让 Key 落到
// 另一个出口。对上游而言就是「这个账号换了 IP」，而这正是风控最敏感的信号。
//
// 返回 error 的情形是调用方需要知晓的异常，不应静默忽略:
//   - 该地址不在当前出口池中（配置被改小、IP 被下线）
//   - 该地址已被判定 banned
//   - 该 IP 不接受此档位的 Key（分层配置与库中记录冲突）
//
// 容量上限（MaxKeys）对 Adopt 不设硬限制: 既有绑定是历史事实，
// 强行拒绝只会让该 Key 被迫换 IP，与本方法的目的相悖。超限会在
// Stats 中体现为 BoundKeys > MaxKeys，由运维决定是否迁移。
func (p *Pool) Adopt(keyID, addr, pool string) error {
	if p.mode == ModeDirect {
		return nil
	}
	if keyID == "" || addr == "" {
		return errors.New("egress: Adopt 需要非空 key_id 与 addr")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	ip := p.byAddr[addr]
	if ip == nil {
		return fmt.Errorf("egress: 出口 %s 不在当前池中", addr)
	}
	if ip.State() == IPBanned {
		return fmt.Errorf("egress: 出口 %s 已封禁", addr)
	}
	if !ip.Accepts(pool) {
		return fmt.Errorf("egress: 出口 %s 属于档位 %q，不接受 %q", addr, ip.Pool, pool)
	}

	if old, ok := p.bindings[keyID]; ok && old != addr {
		// 绑定变更必须丢弃旧连接，否则库里 IP 改了但连接仍走旧出口。
		if c, ok := p.clients[keyID]; ok {
			if tr, ok := c.Transport.(*http.Transport); ok {
				tr.CloseIdleConnections()
			}
			delete(p.clients, keyID)
		}
	}
	p.bindings[keyID] = addr
	return nil
}

// ClientFor 返回该 Key 专属的 HTTP 客户端，不限定池归属。
//
// 等价于 ClientForPool(keyID, PoolAny)。
func (p *Pool) ClientFor(keyID string) (*http.Client, error) {
	return p.ClientForPool(keyID, PoolAny)
}

// ClientForPool 返回该 Key 专属的 HTTP 客户端，必要时在指定档位内建立绑定。
//
// P1-8: 每个 Key 独立 Transport，禁止跨 Key 连接复用。共享连接会使多个 Key
// 暴露相同的 TLS 指纹，把「这些账号来自同一台机器」写进传输层证据。
func (p *Pool) ClientForPool(keyID, pool string) (*http.Client, error) {
	p.mu.RLock()
	if c, ok := p.clients[keyID]; ok {
		p.mu.RUnlock()
		return c, nil
	}
	p.mu.RUnlock()

	// 已有绑定时 BindInPool 直接返回原地址，不会因 pool 不符而改绑 ——
	// 既有绑定优先于分层配置，避免请求路径上意外换出口。
	addr, err := p.BindInPool(keyID, pool)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// 双检，避免并发重复创建
	if c, ok := p.clients[keyID]; ok {
		return c, nil
	}

	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	if addr != "" {
		// 关键: 将 TCP 源地址钉死为该 Key 绑定的辅助 IP。
		// 需宿主机已配置策略路由，否则内核默认路由会让流量仍走主 IP。
		dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(addr)}
	}

	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConns:          10,
		MaxIdleConnsPerHost:   5,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// 禁用 HTTP/2: 多路复用会促使跨请求共享单条 TLS 连接，
		// 削弱 Key 之间的传输层隔离。
		ForceAttemptHTTP2: false,
	}
	client := &http.Client{Transport: transport, Timeout: p.timeout}
	p.clients[keyID] = client
	return client, nil
}

// BoundIP 返回 Key 当前绑定的出口 IP（direct 模式下为空串）。
func (p *Pool) BoundIP(keyID string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.bindings[keyID]
}

// IPFor 返回 Key 绑定的 IP 对象，用于记录成功/失败。
func (p *Pool) IPFor(keyID string) *IP {
	p.mu.RLock()
	defer p.mu.RUnlock()
	addr, ok := p.bindings[keyID]
	if !ok {
		return nil
	}
	return p.byAddr[addr]
}

// LastUsedOn 返回该 Key 绑定的出口最近一次发起请求的时刻。
//
// 未绑定或 direct 模式返回零值 —— 调用方（调度器的出口级节流）据此跳过约束。
// direct 模式下所有 Key 共用本机出口，没有「出口密度」可言。
func (p *Pool) LastUsedOn(keyID string) time.Time {
	ip := p.IPFor(keyID)
	if ip == nil {
		return time.Time{}
	}
	return ip.LastUsed()
}

// MarkUsedBy 记录该 Key 的出口在 now 时刻被用于发起请求。
//
// 必须在**真正发出请求时**调用，而非选中 Key 时 —— 选中后仍可能因配额
// 预扣失败而放弃，那种情况没有实际流量到达上游，不该占用出口的间隔配额。
func (p *Pool) MarkUsedBy(keyID string, now time.Time) {
	if ip := p.IPFor(keyID); ip != nil {
		ip.MarkUsed(now)
	}
}

// SetUnbanPolicy 记录自动解封的退避参数，供 Stats 计算预计解封时刻。
//
// 必须在装配期调用，不能依赖首次 TryUnbanAll 顺带记录: 健康探测每 15 秒
// 才跑一轮，在此之前 Stats 会把参数当成 0，于是面板把可自愈的出口显示成
// 「永久封禁、需人工介入」—— 恰好是运维最不该看错的那一格。
func (p *Pool) SetUnbanPolicy(base, maxWait time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unbanBase, p.unbanMax = base, maxWait
}

// TryUnbanAll 把冷却期届满的被封出口转入 cooldown，返回实际变更的地址。
//
// base 为 0 时不做任何事（自动恢复未启用）。调用方应在健康探测**之前**调用它:
// 探测成功只会通过 MarkSuccess 把 cooldown 转回 active，而 banned 不在
// MarkSuccess 的处理范围内，先探测则恢复永远不会发生。
func (p *Pool) TryUnbanAll(base, maxWait time.Duration) []string {
	if base <= 0 {
		return nil
	}
	p.mu.RLock()
	ips := make([]*IP, len(p.ips))
	copy(ips, p.ips)
	p.mu.RUnlock()

	now := time.Now()
	var out []string
	for _, ip := range ips {
		if ip.TryUnban(now, base, maxWait) {
			out = append(out, ip.Addr)
		}
	}
	sort.Strings(out)
	return out
}

// IPForAddr 按地址返回出口对象，不存在时返回 nil。
//
// 与 IPFor 的区别: 后者按 key_id 反查该 Key 绑定的出口，本方法直接按地址取。
// 运维接口（查单个出口状态）与后台任务需要的是后者。
func (p *Pool) IPForAddr(addr string) *IP {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byAddr[addr]
}

// KeysOn 返回绑定在指定出口上的所有 Key。
func (p *Pool) KeysOn(addr string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []string
	for keyID, bound := range p.bindings {
		if bound == addr {
			out = append(out, keyID)
		}
	}
	sort.Strings(out) // 顺序稳定，便于日志比对与测试断言
	return out
}

// EvacuationResult 是一次出口撤离的结果。
type EvacuationResult struct {
	// Moved 是成功迁移的 Key 及其新出口。
	Moved map[string]string
	// Failed 是无处可去的 Key（目标档位已满）。这些 Key 仍绑在被封出口上，
	// 必须让运维看见 —— 它们已不可用，且不会自行恢复。
	Failed map[string]error
}

// EvacuateIP 把某个出口上的所有 Key 迁走，并将该出口标记为封禁。
//
// 用于确认出口被上游拉黑后的批量撤离。顺序是先标记封禁再迁移:
// 反过来的话，先迁移的 Key 可能被重新分配回这个尚未标记的出口。
//
// 迁移用 Migrate 语义（不额外扣信誉）: 出口已经 banned 了，
// 再逐个 Key 调 MarkFailure 既无意义，也会污染失败计数。
//
// 部分失败不回滚: 已迁走的 Key 是净收益，把它们退回被封出口毫无道理。
// 失败的 Key 通过 Failed 上报，由运维扩容目标档位后重试。
func (p *Pool) EvacuateIP(addr, pool string) (*EvacuationResult, error) {
	if p.mode == ModeDirect {
		return &EvacuationResult{Moved: map[string]string{}, Failed: map[string]error{}}, nil
	}

	p.mu.Lock()
	ip := p.byAddr[addr]
	p.mu.Unlock()
	if ip == nil {
		return nil, fmt.Errorf("egress: 出口 %s 不在当前池中", addr)
	}

	// 先封禁，避免撤离过程中 Key 被重新分配回来。
	ip.MarkBanned()

	res := &EvacuationResult{Moved: map[string]string{}, Failed: map[string]error{}}
	for _, keyID := range p.KeysOn(addr) {
		newAddr, err := p.Migrate(keyID, pool)
		if err != nil {
			res.Failed[keyID] = err
			continue
		}
		res.Moved[keyID] = newAddr
	}
	return res, nil
}

// Rebind 将 Key 迁移到新的出口 IP，并丢弃其旧连接。
//
// 语义是「原出口有问题，换一个」，因此会给原 IP 记一次失败。
// 若迁移原因与 IP 健康无关（如冷 Key 转热需换到低密度出口），
// 应改用 Migrate —— 用本方法会无故扣掉一个健康 IP 的信誉分。
func (p *Pool) Rebind(keyID string) (string, error) {
	return p.rebind(keyID, PoolAny, true)
}

// Migrate 将 Key 迁移到指定档位的出口 IP，不惩罚原 IP 的信誉。
//
// 用于与 IP 健康无关的正常迁移。最典型的场景是冷 Key 转热:
// cold 档单 IP 可能承载上百个 Key，该 Key 一旦活跃就必须换到 hot 档的
// 低密度出口，否则它会带着「这个出口曾有上百个账号活动」的历史上线。
//
// 这类迁移是符合预期的运营动作，不应让原 IP 的信誉分为此下降 ——
// 否则频繁的池间流转会把健康 IP 逐个推入 cooldown。
func (p *Pool) Migrate(keyID, pool string) (string, error) {
	return p.rebind(keyID, pool, false)
}

// rebind 是 Rebind 与 Migrate 的共同实现。
//
// penalize 决定是否给原 IP 记一次失败，这是两者唯一的语义差异。
func (p *Pool) rebind(keyID, pool string, penalize bool) (string, error) {
	p.mu.Lock()
	if old, ok := p.bindings[keyID]; ok {
		if ip := p.byAddr[old]; ip != nil && penalize {
			ip.MarkFailure()
		}
		delete(p.bindings, keyID)
	}
	if c, ok := p.clients[keyID]; ok {
		if tr, ok := c.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
		delete(p.clients, keyID)
	}
	p.mu.Unlock()
	return p.BindInPool(keyID, pool)
}

// RetainClients 清理不在存活集合内的 Key 的 HTTP 客户端。
//
// clients 映射在请求路径上只增不减: Key 被删除 / 禁用 / 移出活跃池后，
// 其独立 Transport（连接池、空闲连接、内部 goroutine）会永久驻留。
// 1000 Key 规模且有正常汰换时这是稳定的慢泄漏。
//
// 由周期性 key_reload 在装载完新 Key 池后调用。只清 client 不清 bindings:
// 绑定关系是「终身绑定」语义的一部分，Key 短暂下线再回来必须还落在原出口，
// 而 client 只是随时可重建的传输资源。
//
// 返回清理的数量，供日志观测。
func (p *Pool) RetainClients(live map[string]bool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for keyID, c := range p.clients {
		if live[keyID] {
			continue
		}
		if tr, ok := c.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
		delete(p.clients, keyID)
		n++
	}
	return n
}

// Stats 汇总出口池状态，供看板与 /admin 使用。
type Stats struct {
	Mode      Mode           `json:"mode"`
	Total     int            `json:"total"`
	Active    int            `json:"active"`
	Banned    int            `json:"banned"`
	KeysBound int            `json:"keys_bound"`
	PerIP     []IPStat       `json:"per_ip"`
	LoadByIP  map[string]int `json:"load_by_ip"`
}

// IPStat 是单个出口 IP 的统计快照。
type IPStat struct {
	Addr       string  `json:"addr"`
	PublicIP   string  `json:"public_ip"`
	State      IPState `json:"state"`
	Reputation int     `json:"reputation"`
	BoundKeys  int     `json:"bound_keys"`
	MaxKeys    int     `json:"max_keys"`
	// Pool 是该 IP 限定的档位，空串表示通用。
	Pool string `json:"pool"`

	// BannedAt 是最近一次被判定封禁的时刻，未曾被封则为零值。
	BannedAt time.Time `json:"banned_at,omitempty"`
	// BanCount 是累计被封次数，决定指数退避的档次。
	BanCount int `json:"ban_count,omitempty"`
	// UnbanAt 是按当前退避档次预计转入 cooldown 的时刻。
	//
	// 零值有两种含义: 从未被封，或未启用自动恢复（此时该出口不会自愈，
	// 必须人工介入）。前端应结合 State 区分 —— state=banned 且 UnbanAt
	// 为零，就是「永久封禁、等运维处理」。
	UnbanAt time.Time `json:"unban_at,omitempty"`
}

// Stats 返回当前出口池快照。
func (p *Pool) Stats() Stats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	load := make(map[string]int, len(p.ips))
	for _, addr := range p.bindings {
		load[addr]++
	}

	s := Stats{Mode: p.mode, Total: len(p.ips), KeysBound: len(p.bindings), LoadByIP: load}
	for _, ip := range p.ips {
		st := ip.State()
		switch st {
		case IPActive:
			s.Active++
		case IPBanned:
			s.Banned++
		}
		bannedAt, banCount, unbanAt := ip.BanInfo(p.unbanBase, p.unbanMax)
		s.PerIP = append(s.PerIP, IPStat{
			Addr: ip.Addr, PublicIP: ip.PublicIP, State: st,
			Reputation: ip.Reputation(), BoundKeys: load[ip.Addr], MaxKeys: ip.MaxKeys,
			Pool:     ip.Pool,
			BannedAt: bannedAt, BanCount: banCount, UnbanAt: unbanAt,
		})
	}
	return s
}

// Verify 校验每个出口 IP 能否真正建立到目标地址的连接。
//
// P1-6: 策略路由配置错误时，绑定源地址的连接会直接失败或静默走主 IP。
// 启动时调用本方法可尽早暴露配置问题，而不是等到线上流量出错。
func (p *Pool) Verify(ctx context.Context, target string) map[string]error {
	p.mu.RLock()
	ips := make([]*IP, len(p.ips))
	copy(ips, p.ips)
	p.mu.RUnlock()

	out := make(map[string]error, len(ips))
	if p.mode == ModeDirect {
		return out
	}
	for _, ip := range ips {
		d := &net.Dialer{
			Timeout:   5 * time.Second,
			LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip.Addr)},
		}
		conn, err := d.DialContext(ctx, "tcp", target)
		if err != nil {
			out[ip.Addr] = err
			ip.MarkFailure()
			continue
		}
		_ = conn.Close()
		ip.MarkSuccess()
		out[ip.Addr] = nil
	}
	return out
}

// CloseIdle 释放所有 Key 客户端的空闲连接。
func (p *Pool) CloseIdle() {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, c := range p.clients {
		if tr, ok := c.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
}
