package egress

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
)

// 本文件实现「出口地址自动发现」（egress.ips_source=scan）。
//
// # 为什么需要它
//
// egress.ips / EGRESS_IPS 要求逐条手抄地址清单，而地址是云厂商按机器分配的:
// 抄错、漏抄、机器扩容后忘补，表现都是「某个档位没出口」或「容量比预期小」。
// 这类失效不报错，只在运行时以 503 或「no assignable ip」出现 —— 而那条文案
// 又会把人引向档位与份额配置（实测，见 livetest-ai KI-035 的同机排查）。
//
// 扫描把「地址从哪来」这件事自动化，但**不改变分层规则**: 每档分多少个地址、
// 单出口承载多少 Key 仍由 egress.scan.tiers 显式给出 —— 那些数字来自
// 反封禁的容量算术（各档 IP 数须满足撤离余量 n >= keys/max_keys + 1，
// 且单 IP 等效密度不超过上限），不能靠猜。
//
// # 过滤掉什么，以及为什么
//
// 本机地址里有大量**不能**用作出口的地址: 回环、链路本地、容器网桥
// （docker0 / br-*）、veth 对、各类隧道网卡。把它们当出口不会报错，
// 只会在上游侧表现为「一批账号从内网地址访问」—— 拿不到真实出口身份，
// 同时白占可绑定名额。

// DefaultIfaceDeny 是内置的网卡名前缀黑名单。
//
// 前缀匹配（而非全名匹配）是因为容器网卡名带随机后缀: veth1a2b3c、br-0f1e2d。
//
// 注意 lo 同时会被 FlagLoopback 过滤掉，这里再列一次是为了让黑名单自身可读 ——
// 运维改动它时能看到「回环本来就在排除之列」，而不会以为删掉 lo 就能纳入回环。
var DefaultIfaceDeny = []string{
	"lo",     // 回环
	"docker", // docker0 及 docker 自建的桥
	"br-",    // docker compose 网络桥
	"veth",   // 容器对端
	"virbr",  // libvirt 桥
	"vnet",   // libvirt 虚机网卡
	"tun",    // 隧道
	"tap",
	"wg", // wireguard
	"zt", // zerotier
	"tailscale",
	"flannel", // k8s CNI
	"cni",
	"cali", // calico
	"kube",
	"lxc",
	"lxd",
}

// ScanOptions 是地址发现的过滤条件。
type ScanOptions struct {
	// IfaceDeny 是**追加**在 DefaultIfaceDeny 之后的自定义网卡名前缀黑名单。
	//
	// 语义是追加而非替换，这是安全侧的选择: 若允许整体替换，一次
	// `iface_deny: [eth1]` 就会把 docker0 / br-* / veth 一并放行 —— 出口池里
	// 混进容器网桥不会报错，只会在上游侧表现为「同一批账号从内网地址访问」。
	// 反向需求（把内置黑名单里的某张网卡当出口，如隧道网卡）属于少见场景，
	// 走 egress.ips 显式指定地址即可 —— 那条路径不经过过滤。
	IfaceDeny []string

	// PrefixAllow / PrefixDeny 是按 CIDR 匹配的地址黑白名单，deny 优先。
	//
	// 典型用途: 机器上同时有管理网（10.0.0.0/8）与出口网段（172.16.0.0/24），
	// 用 prefix_allow 只纳入后者。留空表示不限。
	PrefixAllow []string
	PrefixDeny  []string
}

// AddrCandidate 是一条「来自某网卡的地址」候选，供过滤逻辑单测注入。
type AddrCandidate struct {
	Iface string
	IP    net.IP
}

// DiscoverAddrs 枚举本机网卡地址并返回可用作出口的地址（已过滤、已排序）。
//
// 排序是硬要求而非美观问题: 分档填充按顺序消费地址，顺序一变，同一个地址就
// 可能从 hot 档跑到 cold 档，其上承载的 Key 密度假设随之改变。按数值排序是
// 稳定且与内核枚举顺序无关的选择。
//
// 注意**包含**每个网卡的首地址（主 IP）。它是否该当出口取决于部署: 若主机
// 承担别的职责（SSH、管理面），用 prefix_deny 把它排掉即可 —— 用启发式
// 「跳过首地址」在 Go 里不可靠（secondary 是内核态标记，标准库不暴露）。
func DiscoverAddrs(opts ScanOptions) ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("枚举本机网卡: %w", err)
	}

	var cands []AddrCandidate
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			// 单张网卡读不到地址不阻断整体: 这类故障常见于权限受限的容器，
			// 而其余网卡仍然可用。整体失败会让「多一个网卡读不了」变成「起不来」。
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			cands = append(cands, AddrCandidate{Iface: ifc.Name, IP: ipnet.IP})
		}
	}
	return FilterAddrs(cands, opts)
}

// FilterAddrs 按过滤条件筛出可用出口地址，去重并排序。
//
// 与网卡枚举分开，是为了让过滤规则可以被直接测试（注入候选而非依赖真实网卡）。
func FilterAddrs(cands []AddrCandidate, opts ScanOptions) ([]string, error) {
	// 内置黑名单始终生效，自定义项只做追加 —— 理由见 ScanOptions.IfaceDeny。
	denyNames := make([]string, 0, len(DefaultIfaceDeny)+len(opts.IfaceDeny))
	denyNames = append(denyNames, DefaultIfaceDeny...)
	denyNames = append(denyNames, opts.IfaceDeny...)
	allow, err := parseCIDRs(opts.PrefixAllow)
	if err != nil {
		return nil, fmt.Errorf("prefix_allow: %w", err)
	}
	deny, err := parseCIDRs(opts.PrefixDeny)
	if err != nil {
		return nil, fmt.Errorf("prefix_deny: %w", err)
	}

	seen := make(map[string]bool, len(cands))
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		if ifaceDenied(c.Iface, denyNames) {
			continue
		}
		ip4 := c.IP.To4()
		if ip4 == nil {
			continue // 只要 IPv4: setup-egress.sh 的策略路由与上游封禁判据都按 IPv4 建立
		}
		if !usableEgressIP(ip4) {
			continue
		}
		if len(allow) > 0 && !matchAny(allow, ip4) {
			continue
		}
		if matchAny(deny, ip4) {
			continue
		}
		s := ip4.String()
		if seen[s] {
			// 同一地址出现在两张网卡上（绑定/桥接场景）时只保留一个:
			// 它在池里是同一个出口，重复纳入会让容量算术多算一个名额。
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sortAddrs(out)
	return out, nil
}

// usableEgressIP 判断一个 IPv4 地址是否可能作为出口源地址。
func usableEgressIP(ip net.IP) bool {
	switch {
	case ip.IsLoopback(), // 127.0.0.0/8
		ip.IsUnspecified(),        // 0.0.0.0
		ip.IsLinkLocalUnicast(),   // 169.254.0.0/16（DHCP 失败时的自动配置）
		ip.IsLinkLocalMulticast(), // 224.0.0.0/24
		ip.IsInterfaceLocalMulticast(),
		ip.IsMulticast():
		return false
	}
	return true
}

// ifaceDenied 报告网卡名是否命中前缀黑名单。
func ifaceDenied(name string, deny []string) bool {
	for _, d := range deny {
		if d != "" && strings.HasPrefix(name, d) {
			return true
		}
	}
	return false
}

// parseCIDRs 解析 CIDR 列表。非法条目直接报错: 静默跳过会让「过滤规则写错了」
// 表现为「地址莫名其妙少了一批」，而配置看起来是对的。
func parseCIDRs(list []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("非法 CIDR %q: %w", s, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func matchAny(nets []*net.IPNet, ip net.IP) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// sortAddrs 按 IPv4 数值升序排序，保证同一台机器每次启动的地址顺序一致。
func sortAddrs(addrs []string) {
	sort.Slice(addrs, func(i, j int) bool {
		a, b := net.ParseIP(addrs[i]).To4(), net.ParseIP(addrs[j]).To4()
		if a == nil || b == nil {
			return addrs[i] < addrs[j]
		}
		for k := 0; k < 4; k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return false
	})
}

// TierSpec 是分档计划的一项（与 config.EgressScanTier 对应）。
//
// 在两个包各有一个同形结构是有意的: internal/egress 不依赖 internal/config
// （装配层负责搬运），这样出口池的规划逻辑可以脱离配置格式单独测试。
type TierSpec struct {
	// Pool 是档位（hot / warm / cold），空串表示通用档。
	Pool string
	// Count 是本档分配的地址数；0 表示接住剩余全部（只允许最后一项）。
	Count int
	// MaxKeys 是单出口可绑定的 Key 数上限；0 表示取 defaultMaxKeys。
	MaxKeys int
}

// PlanTierIPs 把扫描到的地址按分档计划填充成出口 IP 列表。
//
// 返回未参与计划的剩余地址（计划没用完的），由调用方告警 —— 那通常意味着
// 机器上还有可用出口没在分担 Key，属于运维需要知道的容量信息，不该静默丢弃。
//
// 地址不足时返回错误而不是「有多少用多少」: 分层计划里的地址数来自容量算术
// （撤离余量、单 IP 等效密度），按缺额运行等于悄悄降低反封禁冗余 —— 而它同时
// 也是「出口被封时 Key 有地方可去」的唯一保障。宁可起不来，也不要带着缩水的
// 冗余跑。
func PlanTierIPs(addrs []string, tiers []TierSpec, defaultMaxKeys int) ([]*IP, []string, error) {
	if defaultMaxKeys <= 0 {
		defaultMaxKeys = 10
	}
	if len(addrs) == 0 {
		return nil, nil, errors.New(
			"egress: 未在本机发现任何可用出口地址 —— 请检查网卡是否已配置辅助 IP" +
				"（scripts/setup-egress.sh）、以及 egress.scan 的过滤条件是否过严")
	}
	if len(tiers) == 0 {
		return nil, nil, errors.New(
			"egress: 分档计划为空（egress.scan.tiers 未配置）——" +
				"扫描只决定「有哪些地址」，各档要几个地址、单出口承载多少 Key 必须显式给出")
	}

	var (
		ips   []*IP
		next  int
		tally []string
	)
	for i, t := range tiers {
		count := t.Count
		if count == 0 {
			count = len(addrs) - next // 接住剩余全部
		}
		if count > len(addrs)-next {
			tally = append(tally, fmt.Sprintf("%s=%d", tierName(t.Pool), count))
			return nil, nil, fmt.Errorf(
				"egress: 分档计划要求 %d 个出口地址（%s），本机只发现 %d 个，第 %d 项不足 —— "+
					"请按本机实际地址数调整 egress.scan.tiers，或改用 egress.ips 显式指定",
				sumCounts(tiers), strings.Join(tally, " "), len(addrs), i+1)
		}
		maxKeys := t.MaxKeys
		if maxKeys <= 0 {
			maxKeys = defaultMaxKeys
		}
		for j := 0; j < count; j++ {
			ips = append(ips, NewPooledIP(addrs[next], "", maxKeys, t.Pool))
			next++
		}
		tally = append(tally, fmt.Sprintf("%s=%d", tierName(t.Pool), count))
	}

	unused := addrs[next:]
	sort.Strings(tally)
	return ips, unused, nil
}

func tierName(pool string) string {
	if pool == "" {
		return "通用"
	}
	return pool
}

func sumCounts(tiers []TierSpec) int {
	n := 0
	for _, t := range tiers {
		// count=0 是「剩余全部」，无法在事前求和；按 0 计入，
		// 该值只用于报错文案，由调用处在不足时补充说明。
		n += t.Count
	}
	return n
}
