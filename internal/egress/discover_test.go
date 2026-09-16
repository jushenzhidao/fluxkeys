package egress

import (
	"net"
	"strings"
	"testing"
)

// 本文件覆盖「出口地址自动发现」（egress.ips_source=scan）的两段逻辑:
//
//	① 过滤: 本机地址里哪些不能当出口；
//	② 分档填充: 过滤后的地址如何按计划进各档。
//
// 二者都属于「写错了不报错、只在运行时表现为出口集合或容量不对」的那类逻辑，
// 所以断言全部落在可核对的产物上（地址集合、档位归属、max_keys），而不是
// 只看函数有没有返回错误。

func cand(iface, ip string) AddrCandidate {
	return AddrCandidate{Iface: iface, IP: net.ParseIP(ip)}
}

func TestFilterAddrs_只留可作出口的地址(t *testing.T) {
	got, err := FilterAddrs([]AddrCandidate{
		cand("eth0", "172.16.0.11"),     // 保留
		cand("eth0", "172.16.0.31"),     // 保留: 同网卡 secondary，辅助 IP 的典型形态
		cand("lo", "127.0.0.1"),         // 回环
		cand("eth0", "169.254.7.7"),     // 链路本地（DHCP 失败时的自动配置）
		cand("eth0", "0.0.0.0"),         // 未指定
		cand("eth0", "224.0.0.1"),       // 组播
		cand("docker0", "172.17.0.1"),   // 容器网桥
		cand("br-1a2b3c", "172.18.0.1"), // compose 网络桥
		cand("veth9f8e", "10.1.1.1"),    // 容器对端
		cand("tun0", "10.8.0.1"),        // 隧道
		cand("eth0", "fe80::1"),         // IPv6 链路本地
		cand("eth0", "2001:db8::1"),     // IPv6 全局（本服务按 IPv4 建策略路由）
	}, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := "172.16.0.11,172.16.0.31"
	if strings.Join(got, ",") != want {
		t.Errorf("过滤结果 = [%s]，期望 [%s]", strings.Join(got, ","), want)
	}
}

func TestFilterAddrs_前缀黑白名单且deny优先(t *testing.T) {
	cands := []AddrCandidate{
		cand("eth0", "172.16.0.11"),
		cand("eth0", "172.16.0.31"),
		cand("eth0", "10.0.0.5"), // 管理网
	}
	for _, tc := range []struct {
		name string
		opts ScanOptions
		want string
	}{
		{
			"allow 只纳入出口网段",
			ScanOptions{PrefixAllow: []string{"172.16.0.0/24"}},
			"172.16.0.11,172.16.0.31",
		},
		{
			"deny 单独使用可排除管理网",
			ScanOptions{PrefixDeny: []string{"10.0.0.0/8"}},
			"172.16.0.11,172.16.0.31",
		},
		{
			"deny 优先于 allow",
			ScanOptions{
				PrefixAllow: []string{"172.16.0.0/24"},
				PrefixDeny:  []string{"172.16.0.31/32"},
			},
			"172.16.0.11",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FilterAddrs(cands, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, ",") != tc.want {
				t.Errorf("结果 = [%s]，期望 [%s]", strings.Join(got, ","), tc.want)
			}
		})
	}
}

func TestFilterAddrs_自定义网卡黑名单是追加而非替换(t *testing.T) {
	got, err := FilterAddrs([]AddrCandidate{
		cand("eth0", "172.16.0.11"),
		cand("eth1", "172.16.0.12"),   // 自定义黑名单命中
		cand("docker0", "172.17.0.1"), // 内置黑名单命中
	}, ScanOptions{IfaceDeny: []string{"eth1"}})
	if err != nil {
		t.Fatal(err)
	}
	// 若自定义项**替换**了内置列表，这里会多出 172.17.0.1 —— 出口池里混进
	// 容器网桥不会报错，只会在上游侧表现为「同一批账号从内网地址访问」。
	if want := "172.16.0.11"; strings.Join(got, ",") != want {
		t.Errorf("结果 = [%s]，期望 [%s]（自定义黑名单必须追加在内置黑名单之后）",
			strings.Join(got, ","), want)
	}
}

func TestFilterAddrs_同一地址出现在多张网卡只算一个(t *testing.T) {
	got, err := FilterAddrs([]AddrCandidate{
		cand("eth0", "172.16.0.11"),
		cand("eth1", "172.16.0.11"),
	}, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 重复纳入会让容量算术多算一个名额，而池里其实只有一个出口。
	if len(got) != 1 {
		t.Errorf("同一地址应只保留一个，实际 %v", got)
	}
}

func TestFilterAddrs_非法CIDR直接报错(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts ScanOptions
	}{
		{"前缀长度越界", ScanOptions{PrefixAllow: []string{"172.16.0.0/33"}}},
		{"不是 CIDR", ScanOptions{PrefixDeny: []string{"172.16.0.11"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 静默跳过会让「过滤规则写错了」表现为「地址莫名少了一批」。
			if _, err := FilterAddrs([]AddrCandidate{cand("eth0", "172.16.0.11")}, tc.opts); err == nil {
				t.Error("非法 CIDR 应报错，而不是被静默忽略")
			}
		})
	}
}

func TestFilterAddrs_按数值排序(t *testing.T) {
	got, err := FilterAddrs([]AddrCandidate{
		cand("eth0", "172.16.0.228"),
		cand("eth0", "172.16.0.31"),
		cand("eth0", "172.16.0.11"),
		cand("eth0", "172.16.1.2"),
	}, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 排序必须是数值序而非字典序: 字典序会把 .11 < .228 < .31，与地址段实际
	// 顺序不符。顺序重要，是因为分档填充按顺序消费地址 —— 顺序一变，同一个
	// 地址就可能从 hot 档跑到 cold 档，其上的 Key 密度假设随之改变。
	want := "172.16.0.11,172.16.0.31,172.16.0.228,172.16.1.2"
	if strings.Join(got, ",") != want {
		t.Errorf("结果 = [%s]，期望数值序 [%s]", strings.Join(got, ","), want)
	}
}

// TestDiscoverAddrs_真实网卡 验证枚举这一层（过滤器已由上面的注入用例覆盖）。
func TestDiscoverAddrs_真实网卡(t *testing.T) {
	got, err := DiscoverAddrs(ScanOptions{})
	if err != nil {
		t.Fatalf("枚举本机地址失败: %v", err)
	}

	// 枚举结果里绝不能出现回环 / 链路本地 —— 这两类一旦进池，会把 Key 绑到
	// 一个不存在于上游视角的地址上。
	for _, a := range got {
		ip := net.ParseIP(a)
		if ip == nil {
			t.Errorf("返回了非法地址 %q", a)
			continue
		}
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.To4() == nil {
			t.Errorf("不可作出口的地址混入结果: %s", a)
		}
	}

	// 反向断言: 只要本机确实有一张「up、非回环、未被内置黑名单排除」且有 IPv4 的
	// 网卡，就必须至少发现一个地址。没有这条断言，「枚举永远返回空」也能绿灯。
	if !hostHasUsableIPv4(t) {
		t.Skip("本机没有可作出口的 IPv4 网卡（只有回环或被黑名单覆盖），跳过反向断言")
	}
	if len(got) == 0 {
		t.Error("本机存在可作出口的 IPv4 网卡，但一个地址都没被发现")
	}
	t.Logf("本机发现 %d 个可用出口地址: %v", len(got), got)
}

// hostHasUsableIPv4 判断本机是否存在「过滤后应当保留」的地址。
//
// 刻意用与实现不同的写法（只看 flags 与前缀黑名单），避免与被测代码同源 ——
// 同源的判据会在实现出错时一起出错，等于没断言。
func hostHasUsableIPv4(t *testing.T) bool {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("枚举网卡: %v", err)
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		if ifaceDenied(ifc.Name, DefaultIfaceDeny) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && usableEgressIP(ipnet.IP.To4()) {
				return true
			}
		}
	}
	return false
}

func TestPlanTierIPs_按计划分档(t *testing.T) {
	addrs := []string{
		"172.16.0.11", "172.16.0.12", "172.16.0.13", "172.16.0.14",
		"172.16.0.15", "172.16.0.16", "172.16.0.17",
	}
	tiers := []TierSpec{
		{Pool: "hot", Count: 2, MaxKeys: 10},
		{Pool: "warm", Count: 2, MaxKeys: 50},
		{Pool: "cold", Count: 0, MaxKeys: 100}, // 接住剩余
	}
	ips, unused, err := PlanTierIPs(addrs, tiers, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(unused) != 0 {
		t.Errorf("末项是「接住剩余」，不应有剩余地址，实际 %v", unused)
	}

	type got struct {
		pool    string
		maxKeys int
		addrs   []string
	}
	byPool := map[string]*got{}
	for _, ip := range ips {
		g := byPool[ip.Pool]
		if g == nil {
			g = &got{pool: ip.Pool, maxKeys: ip.MaxKeys}
			byPool[ip.Pool] = g
		}
		g.addrs = append(g.addrs, ip.Addr)
	}
	want := map[string]struct {
		count   int
		maxKeys int
	}{
		"hot":  {2, 10},
		"warm": {2, 50},
		"cold": {3, 100},
	}
	if len(byPool) != len(want) {
		t.Fatalf("档位数 = %d，期望 %d：%+v", len(byPool), len(want), byPool)
	}
	for pool, w := range want {
		g := byPool[pool]
		if g == nil {
			t.Errorf("档位 %q 没有任何出口", pool)
			continue
		}
		if len(g.addrs) != w.count {
			t.Errorf("档位 %q 分到 %d 个地址，期望 %d（%v）", pool, len(g.addrs), w.count, g.addrs)
		}
		if g.maxKeys != w.maxKeys {
			t.Errorf("档位 %q 的 max_keys = %d，期望 %d", pool, g.maxKeys, w.maxKeys)
		}
	}
	// 地址按顺序消费: hot 拿前两个，cold 拿剩下的 —— 顺序错会让容量与风控假设错位。
	if strings.Join(byPool["hot"].addrs, ",") != "172.16.0.11,172.16.0.12" {
		t.Errorf("hot 档地址 = %v，期望按序取前两个", byPool["hot"].addrs)
	}
	if strings.Join(byPool["cold"].addrs, ",") != "172.16.0.15,172.16.0.16,172.16.0.17" {
		t.Errorf("cold 档地址 = %v，期望接住剩余三个", byPool["cold"].addrs)
	}
}

func TestPlanTierIPs_地址不足时报错而不是缩水运行(t *testing.T) {
	addrs := []string{"172.16.0.11", "172.16.0.12", "172.16.0.13"}
	// 32 IP 的生产分层计划（19 hot / 5 warm / 8 cold）套到只有 3 个地址的机器上。
	tiers := []TierSpec{
		{Pool: "hot", Count: 19, MaxKeys: 10},
		{Pool: "warm", Count: 5, MaxKeys: 50},
		{Pool: "cold", Count: 8, MaxKeys: 100},
	}
	ips, _, err := PlanTierIPs(addrs, tiers, 10)
	if err == nil {
		t.Fatalf("地址不足应拒绝构造出口池（拿了 %d 个出口继续跑会静默丢掉撤离余量）", len(ips))
	}
	// 报错必须带上可核对的数字: 计划要几个、本机有几个。
	for _, want := range []string{"32", "3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错应含数字 %q，实际: %v", want, err)
		}
	}
}

func TestPlanTierIPs_计划未用完的地址如实上报(t *testing.T) {
	addrs := []string{"172.16.0.11", "172.16.0.12", "172.16.0.13", "172.16.0.14", "172.16.0.15"}
	// 没有「接住剩余」的项 ⇒ 多出来的地址不参与，必须报出来（它们是纯容量损失）。
	tiers := []TierSpec{
		{Pool: "hot", Count: 2, MaxKeys: 10},
		{Pool: "cold", Count: 1, MaxKeys: 100},
	}
	ips, unused, err := PlanTierIPs(addrs, tiers, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 3 {
		t.Errorf("应使用 3 个地址，实际 %d", len(ips))
	}
	if strings.Join(unused, ",") != "172.16.0.14,172.16.0.15" {
		t.Errorf("未参与计划的地址 = %v，期望 [172.16.0.14 172.16.0.15]", unused)
	}
}

func TestPlanTierIPs_默认承载上限与空输入(t *testing.T) {
	ips, _, err := PlanTierIPs([]string{"172.16.0.11"},
		[]TierSpec{{Pool: "", Count: 0, MaxKeys: 0}}, DefaultMaxKeysForTest)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 1 || ips[0].MaxKeys != DefaultMaxKeysForTest {
		t.Errorf("max_keys=0 应取默认值 %d，实际 %+v", DefaultMaxKeysForTest, ips)
	}
	if ips[0].Pool != PoolAny {
		t.Errorf("pool 留空应为通用档，实际 %q", ips[0].Pool)
	}

	if _, _, err := PlanTierIPs(nil, []TierSpec{{Pool: "hot", Count: 1}}, 10); err == nil {
		t.Error("未发现任何地址时应报错并给出排查方向")
	}
	if _, _, err := PlanTierIPs([]string{"172.16.0.11"}, nil, 10); err == nil {
		t.Error("分档计划为空时应报错（扫描无法自行决定分层）")
	}
}

// DefaultMaxKeysForTest 与被测代码内置的 10 保持一致。
//
// 这个数字在 config 包是 DefaultMaxKeysPerIP、在 egress 是内部默认值，测试里
// 只用来断言「MaxKeys=0 时确实取到了默认值」，不参与生产逻辑。
const DefaultMaxKeysForTest = 10
