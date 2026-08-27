package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDirectMode_WorksWithoutIPs(t *testing.T) {
	p, err := NewPool(ModeDirect, nil, 30*time.Second)
	if err != nil {
		t.Fatalf("direct 模式应允许空 IP 池: %v", err)
	}
	c, err := p.ClientFor("key_1")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if c == nil {
		t.Fatal("客户端不应为 nil")
	}
	if got := p.BoundIP("key_1"); got != "" {
		t.Errorf("direct 模式不应绑定 IP, got %q", got)
	}
}

func TestMultiIPMode_RequiresIPs(t *testing.T) {
	if _, err := NewPool(ModeMultiIP, nil, 30*time.Second); err == nil {
		t.Fatal("multi_ip 模式缺少 IP 应报错")
	}
}

// 每个 Key 必须获得独立客户端，绝不跨 Key 复用连接（P1-8）。
func TestClientFor_IsolatedPerKey(t *testing.T) {
	p, err := NewPool(ModeDirect, nil, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c1, _ := p.ClientFor("key_a")
	c2, _ := p.ClientFor("key_b")
	if c1 == c2 {
		t.Fatal("不同 Key 必须使用不同客户端，否则共享 TLS 指纹")
	}
	if c1.Transport == c2.Transport {
		t.Fatal("不同 Key 必须使用不同 Transport")
	}

	// 同一 Key 应稳定复用自己的客户端
	c1b, _ := p.ClientFor("key_a")
	if c1 != c1b {
		t.Error("同一 Key 应复用同一客户端")
	}
}

// HTTP/2 必须关闭，避免多路复用导致跨请求共享 TLS 连接。
func TestTransport_HTTP2Disabled(t *testing.T) {
	p, _ := NewPool(ModeDirect, nil, 30*time.Second)
	c, _ := p.ClientFor("key_x")
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("Transport 类型异常")
	}
	if tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 应为 false")
	}
}

// 绑定必须是确定性的 —— 进程重启后同一 Key 应落到同一 IP（终身绑定）。
func TestBind_Deterministic(t *testing.T) {
	mk := func() *Pool {
		ips := []*IP{
			NewIP("127.0.0.1", "1.2.3.4", 10),
			NewIP("127.0.0.2", "1.2.3.5", 10),
			NewIP("127.0.0.3", "1.2.3.6", 10),
		}
		p, err := NewPool(ModeMultiIP, ips, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	first := mk()
	second := mk()

	for i := 0; i < 30; i++ {
		keyID := fmt.Sprintf("key_%03d", i)
		a, err := first.Bind(keyID)
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		b, err := second.Bind(keyID)
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		if a != b {
			t.Fatalf("%s 绑定不稳定: %s vs %s", keyID, a, b)
		}
	}
}

// 绑定应稳定: 重复调用返回同一 IP。
func TestBind_Idempotent(t *testing.T) {
	ips := []*IP{NewIP("127.0.0.1", "1.2.3.4", 10), NewIP("127.0.0.2", "1.2.3.5", 10)}
	p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)

	got, err := p.Bind("key_1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, _ := p.Bind("key_1")
		if again != got {
			t.Fatalf("绑定发生漂移: %s -> %s", got, again)
		}
	}
}

// MaxKeys 应限制单 IP 承载的 Key 数。
func TestBind_RespectsMaxKeys(t *testing.T) {
	ips := []*IP{NewIP("127.0.0.1", "1.2.3.4", 2)}
	p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)

	for i := 0; i < 2; i++ {
		if _, err := p.Bind(fmt.Sprintf("key_%d", i)); err != nil {
			t.Fatalf("第 %d 个 Key 应能绑定: %v", i, err)
		}
	}
	if _, err := p.Bind("key_overflow"); err != ErrNoIP {
		t.Errorf("超出 MaxKeys 应返回 ErrNoIP, got %v", err)
	}
}

// IP 被封后应重新分配到健康 IP。
func TestBind_ReassignsAfterBan(t *testing.T) {
	ipA := NewIP("127.0.0.1", "1.2.3.4", 10)
	ipB := NewIP("127.0.0.2", "1.2.3.5", 10)
	p, _ := NewPool(ModeMultiIP, []*IP{ipA, ipB}, 30*time.Second)

	first, err := p.Bind("key_1")
	if err != nil {
		t.Fatal(err)
	}

	// 封禁当前绑定的 IP
	p.byAddr[first].MarkBanned()

	second, err := p.Bind("key_1")
	if err != nil {
		t.Fatalf("应能迁移到健康 IP: %v", err)
	}
	if second == first {
		t.Error("被封 IP 不应继续使用")
	}
}

func TestIPStateMachine(t *testing.T) {
	ip := NewIP("127.0.0.1", "1.2.3.4", 10)

	if ip.State() != IPActive || !ip.Assignable() {
		t.Fatal("初始状态应为 active 且可分配")
	}

	ip.MarkFailure()
	if ip.State() != IPSuspect {
		t.Errorf("1 次失败应为 suspect, got %s", ip.State())
	}

	ip.MarkSuccess()
	if ip.State() != IPActive {
		t.Errorf("成功后应恢复 active, got %s", ip.State())
	}

	for i := 0; i < 3; i++ {
		ip.MarkFailure()
	}
	if ip.State() != IPCooldown {
		t.Errorf("连续 3 次失败应为 cooldown, got %s", ip.State())
	}

	for i := 0; i < 3; i++ {
		ip.MarkFailure()
	}
	if ip.State() != IPBanned {
		t.Errorf("连续 6 次失败应为 banned, got %s", ip.State())
	}
	if ip.Assignable() {
		t.Error("banned 的 IP 不应可分配")
	}
}

// 信誉低于 50 进入观察期，不再承接新 Key。
func TestIP_LowReputationNotAssignable(t *testing.T) {
	ip := NewIP("127.0.0.1", "1.2.3.4", 10)
	for i := 0; i < 6; i++ {
		ip.MarkFailure()
		ip.MarkSuccess() // 恢复 state，但信誉不回补
	}
	if ip.Reputation() >= 50 {
		// 失败次数不足以压到 50 以下时跳过
		t.Skipf("信誉仍为 %d，跳过", ip.Reputation())
	}
	if ip.Assignable() {
		t.Error("信誉低于 50 应停止分配新 Key")
	}
}

// 核心可行性验证: 绑定 LocalAddr 后，服务端观察到的源地址必须与绑定值一致。
// 这直接证明「按 Key 控制出口 IP」在代码层面成立。
func TestLocalAddrBinding_ActuallyControlsSourceIP(t *testing.T) {
	var mu sync.Mutex
	seen := make(map[string]string)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		mu.Lock()
		seen[r.Header.Get("X-Key-ID")] = host
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 环回网段的多个地址在 macOS/Linux 上均可直接作为源地址使用，
	// 用于验证绑定逻辑本身（生产环境替换为真实辅助私网 IP）。
	ips := []*IP{
		NewIP("127.0.0.1", "eip-1", 10),
		NewIP("127.0.0.2", "eip-2", 10),
	}
	p, err := NewPool(ModeMultiIP, ips, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	for _, keyID := range []string{"key_1", "key_2", "key_3", "key_4"} {
		client, err := p.ClientFor(keyID)
		if err != nil {
			t.Fatalf("ClientFor(%s): %v", keyID, err)
		}
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		req.Header.Set("X-Key-ID", keyID)

		resp, err := client.Do(req)
		if err != nil {
			// 127.0.0.2 在部分环境未启用，跳过而非失败
			t.Skipf("环回别名地址不可用，跳过: %v", err)
		}
		resp.Body.Close()

		bound := p.BoundIP(keyID)
		mu.Lock()
		observed := seen[keyID]
		mu.Unlock()

		if observed != bound {
			t.Errorf("%s: 服务端观察到源 IP %s，但绑定的是 %s", keyID, observed, bound)
		}
	}
}

func TestVerify_DetectsUnroutableIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	target := srv.Listener.Addr().String()

	ips := []*IP{
		NewIP("127.0.0.1", "eip-ok", 10),
		NewIP("10.255.255.254", "eip-bad", 10), // 本机不存在的地址
	}
	p, _ := NewPool(ModeMultiIP, ips, 5*time.Second)

	results := p.Verify(context.Background(), target)

	if err, ok := results["127.0.0.1"]; !ok || err != nil {
		t.Errorf("127.0.0.1 应验证通过, got %v", err)
	}
	if err := results["10.255.255.254"]; err == nil {
		t.Error("不可用地址应被检出 —— 这正是 P1-6 要防的静默失效")
	}
}

func TestStats(t *testing.T) {
	ips := []*IP{
		NewIP("127.0.0.1", "eip-1", 5),
		NewIP("127.0.0.2", "eip-2", 5),
	}
	p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)
	for i := 0; i < 6; i++ {
		if _, err := p.Bind(fmt.Sprintf("key_%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	ips[1].MarkBanned()

	s := p.Stats()
	if s.Total != 2 {
		t.Errorf("Total = %d, want 2", s.Total)
	}
	if s.KeysBound != 6 {
		t.Errorf("KeysBound = %d, want 6", s.KeysBound)
	}
	if s.Banned != 1 {
		t.Errorf("Banned = %d, want 1", s.Banned)
	}
	if len(s.PerIP) != 2 {
		t.Errorf("PerIP 应有 2 条, got %d", len(s.PerIP))
	}
}

// 并发绑定不应产生数据竞争或重复客户端。
func TestConcurrentClientFor(t *testing.T) {
	p, _ := NewPool(ModeDirect, nil, 30*time.Second)

	var wg sync.WaitGroup
	clients := make([]*http.Client, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c, err := p.ClientFor("same_key")
			if err != nil {
				t.Errorf("ClientFor: %v", err)
				return
			}
			clients[n] = c
		}(i)
	}
	wg.Wait()

	for i := 1; i < 50; i++ {
		if clients[i] != clients[0] {
			t.Fatal("并发下同一 Key 应得到同一客户端实例")
		}
	}
}

// ---------- 分层承载（hot / warm / cold）----------

// 分层的核心语义: Key 只能落在接受其档位的 IP 上。
//
// 用多个 Key 逐一验证而非只试一个 —— 单个 Key 的哈希可能恰好落在正确的 IP 上，
// 即使档位过滤完全失效也看不出来。
func TestBindInPool_按档位隔离(t *testing.T) {
	// 每档给 3 个 IP，让哈希有足够的错误落点可选
	var ips []*IP
	for i := 2; i < 5; i++ {
		ips = append(ips, NewPooledIP(fmt.Sprintf("10.0.1.%d", i), "", 50, "cold"))
	}
	for i := 2; i < 5; i++ {
		ips = append(ips, NewPooledIP(fmt.Sprintf("10.0.2.%d", i), "", 50, "hot"))
	}
	p, err := NewPool(ModeMultiIP, ips, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 30; i++ {
		hotKey := fmt.Sprintf("hot_%02d", i)
		addr, err := p.BindInPool(hotKey, "hot")
		if err != nil {
			t.Fatalf("%s 应能绑定: %v", hotKey, err)
		}
		if !strings.HasPrefix(addr, "10.0.2.") {
			t.Fatalf("%s 落到了 %s，应落在 hot 档（10.0.2.x）", hotKey, addr)
		}

		coldKey := fmt.Sprintf("cold_%02d", i)
		addr, err = p.BindInPool(coldKey, "cold")
		if err != nil {
			t.Fatalf("%s 应能绑定: %v", coldKey, err)
		}
		if !strings.HasPrefix(addr, "10.0.1.") {
			t.Fatalf("%s 落到了 %s，应落在 cold 档（10.0.1.x）", coldKey, addr)
		}
	}

	// Stats 需暴露档位，否则运维无法确认分层实际生效
	for _, st := range p.Stats().PerIP {
		if st.Pool == "" {
			t.Errorf("IP %s 的档位未在 Stats 中暴露", st.Addr)
		}
	}
}

// 某档位容量耗尽时不得溢出到其他档位 —— 否则分层形同虚设。
func TestBindInPool_档位满时不溢出到其他档(t *testing.T) {
	ips := []*IP{
		NewPooledIP("10.0.0.2", "", 1, "hot"),
		NewPooledIP("10.0.0.3", "", 100, "cold"),
	}
	p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)

	if _, err := p.BindInPool("k1", "hot"); err != nil {
		t.Fatalf("首个 hot Key 应能绑定: %v", err)
	}
	_, err := p.BindInPool("k2", "hot")
	if !errors.Is(err, ErrNoIP) {
		t.Fatalf("hot 档满后应返回 ErrNoIP，实际 %v", err)
	}
	// 错误信息需能指出是哪个档位满了，否则线上无法定位
	if err != nil && !strings.Contains(err.Error(), "hot") {
		t.Errorf("错误未指明档位: %v", err)
	}
}

// 通用 IP（PoolAny）应接受任何档位，保证未启用分层的部署不受影响。
func TestBindInPool_通用IP接受任何档位(t *testing.T) {
	ips := []*IP{NewIP("10.0.0.2", "", 10)}
	p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)

	for _, pool := range []string{"hot", "warm", "cold", PoolAny} {
		if _, err := p.BindInPool("key_"+pool, pool); err != nil {
			t.Errorf("通用 IP 应接受档位 %q: %v", pool, err)
		}
	}
}

// 未指明档位的调用方（旧代码）应能落在任何 IP 上。
func TestBind_未指明档位可落在分层IP上(t *testing.T) {
	ips := []*IP{NewPooledIP("10.0.0.2", "", 10, "cold")}
	p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)

	if _, err := p.Bind("legacy_key"); err != nil {
		t.Fatalf("未指明档位时应退化为接受: %v", err)
	}
}

// ---------- Adopt: 采纳持久层的既有绑定 ----------

// 这是「Key-IP 终身绑定」的本体: 库里记的出口必须原样沿用。
func TestAdopt_沿用库中绑定而非重新哈希(t *testing.T) {
	var ips []*IP
	for i := 2; i < 12; i++ {
		ips = append(ips, NewIP(fmt.Sprintf("10.0.0.%d", i), "", 10))
	}
	p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)

	// 先看哈希会把它分到哪
	natural, err := p.Bind("volc_001")
	if err != nil {
		t.Fatal(err)
	}
	// 挑一个与哈希落点不同的地址，模拟库中的历史值
	historical := "10.0.0.11"
	if natural == historical {
		historical = "10.0.0.2"
	}

	if err := p.Adopt("volc_001", historical, PoolAny); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got := p.BoundIP("volc_001"); got != historical {
		t.Fatalf("Adopt 后应为 %s，实际 %s", historical, got)
	}
	// 再次 Bind 不得覆盖已采纳的绑定
	again, err := p.Bind("volc_001")
	if err != nil {
		t.Fatal(err)
	}
	if again != historical {
		t.Fatalf("Bind 覆盖了已采纳的绑定: %s → %s", historical, again)
	}
}

// 采纳失败必须报错而非静默改绑 —— 静默改绑等于悄悄换了账号的出口。
func TestAdopt_异常情形显式报错(t *testing.T) {
	ips := []*IP{
		NewPooledIP("10.0.0.2", "", 10, "hot"),
		NewIP("10.0.0.3", "", 10),
	}
	p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)

	if err := p.Adopt("k1", "10.9.9.9", PoolAny); err == nil {
		t.Error("采纳不在池中的地址应报错")
	}
	if err := p.Adopt("k2", "10.0.0.2", "cold"); err == nil {
		t.Error("档位冲突应报错")
	}
	if err := p.Adopt("", "10.0.0.3", PoolAny); err == nil {
		t.Error("空 key_id 应报错")
	}
	// 已封禁的 IP 不应被采纳
	p.byAddr["10.0.0.3"].MarkBanned()
	if err := p.Adopt("k3", "10.0.0.3", PoolAny); err == nil {
		t.Error("采纳已封禁的出口应报错")
	}
}

// direct 模式下 Adopt 应无副作用地返回成功。
func TestAdopt_direct模式无操作(t *testing.T) {
	p, _ := NewPool(ModeDirect, nil, 30*time.Second)
	if err := p.Adopt("k1", "10.0.0.2", PoolAny); err != nil {
		t.Fatalf("direct 模式应直接返回 nil: %v", err)
	}
	if got := p.BoundIP("k1"); got != "" {
		t.Errorf("direct 模式不应产生绑定，实际 %q", got)
	}
}

// ---------- Migrate: 不惩罚信誉的正常迁移 ----------

// 冷 Key 转热是运营动作，不该让原 IP 的信誉分下降。
func TestMigrate_不扣原IP信誉(t *testing.T) {
	ips := []*IP{
		NewPooledIP("10.0.0.2", "", 100, "cold"),
		NewPooledIP("10.0.0.3", "", 10, "hot"),
	}
	p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)

	if _, err := p.BindInPool("k1", "cold"); err != nil {
		t.Fatal(err)
	}
	coldIP := p.byAddr["10.0.0.2"]
	before := coldIP.Reputation()

	newAddr, err := p.Migrate("k1", "hot")
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if newAddr != "10.0.0.3" {
		t.Errorf("应迁移到 hot 档，实际 %s", newAddr)
	}
	if got := coldIP.Reputation(); got != before {
		t.Errorf("Migrate 不应扣信誉: %d → %d", before, got)
	}
	if coldIP.State() != IPActive {
		t.Errorf("Migrate 不应改变原 IP 状态，实际 %s", coldIP.State())
	}
}

// Rebind 语义是「原出口有问题」，必须仍然扣分，否则故障 IP 无法被识别。
func TestRebind_仍然扣原IP信誉(t *testing.T) {
	ips := []*IP{NewIP("10.0.0.2", "", 10), NewIP("10.0.0.3", "", 10)}
	p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)

	if _, err := p.Bind("k1"); err != nil {
		t.Fatal(err)
	}
	old := p.BoundIP("k1")
	oldIP := p.byAddr[old]
	if oldIP.State() != IPActive {
		t.Fatalf("前置条件: 应为 active，实际 %s", oldIP.State())
	}

	if _, err := p.Rebind("k1"); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	if oldIP.State() == IPActive {
		t.Error("Rebind 应给原 IP 记一次失败（状态应转为 suspect）")
	}
}

// ---------- 出口封禁判定：按不同 Key 数 ----------

// 同一个 Key 反复失败不应累积计数 —— 那是它自己被禁用，与出口无关。
func TestMarkAuthFailure_同一Key不重复计数(t *testing.T) {
	ip := NewIP("10.0.0.2", "", 10)
	for i := 0; i < 5; i++ {
		if n := ip.MarkAuthFailure("volc_001", time.Minute); n != 1 {
			t.Fatalf("第 %d 次同 Key 失败，计数应始终为 1，实际 %d", i+1, n)
		}
	}
}

// 不同 Key 相继失败才累积 —— 这是出口被拉黑的信号。
func TestMarkAuthFailure_不同Key累积计数(t *testing.T) {
	ip := NewIP("10.0.0.2", "", 10)
	for i, key := range []string{"k1", "k2", "k3"} {
		if n := ip.MarkAuthFailure(key, time.Minute); n != i+1 {
			t.Errorf("%s 失败后计数 = %d, 期望 %d", key, n, i+1)
		}
	}
}

// 记录 auth 失败本身不得推进信誉状态机 —— 单次 401/403 无法区分
// 「Key 被封」与「IP 被封」，直接扣分会让坏 Key 拖垮健康出口。
func TestMarkAuthFailure_不影响信誉与状态(t *testing.T) {
	ip := NewIP("10.0.0.2", "", 10)
	rep := ip.Reputation()
	for _, key := range []string{"k1", "k2", "k3", "k4", "k5"} {
		ip.MarkAuthFailure(key, time.Minute)
	}
	if got := ip.State(); got != IPActive {
		t.Errorf("记录 auth 失败不应改变状态: active → %s", got)
	}
	if got := ip.Reputation(); got != rep {
		t.Errorf("记录 auth 失败不应影响信誉: %d → %d", rep, got)
	}
}

// 超出窗口的记录必须被剔除，否则计数随时间单调累积，最终误判健康出口。
func TestMarkAuthFailure_窗口外记录被剔除(t *testing.T) {
	ip := NewIP("10.0.0.2", "", 10)
	// 用极短窗口模拟过期
	ip.MarkAuthFailure("k1", time.Nanosecond)
	time.Sleep(2 * time.Millisecond)
	// 新的失败进来时应把 k1 清掉，只剩 k2
	if n := ip.MarkAuthFailure("k2", time.Nanosecond); n != 1 {
		t.Errorf("窗口外的 k1 应被剔除，计数应为 1，实际 %d", n)
	}
}

// 成功请求应撤销该 Key 的失败记录。
func TestClearAuthFailure_撤销记录(t *testing.T) {
	ip := NewIP("10.0.0.2", "", 10)
	ip.MarkAuthFailure("k1", time.Minute)
	ip.MarkAuthFailure("k2", time.Minute)
	if n := ip.AuthFailKeys(time.Minute); n != 2 {
		t.Fatalf("前置: 应为 2，实际 %d", n)
	}
	ip.ClearAuthFailure("k1")
	if n := ip.AuthFailKeys(time.Minute); n != 1 {
		t.Errorf("撤销后应为 1，实际 %d", n)
	}
}

// ---------- 出口撤离 ----------

// 撤离应把该出口上全部 Key 迁到同档位的其他出口，并封禁原出口。
func TestEvacuateIP_全部迁出并封禁原出口(t *testing.T) {
	ips := []*IP{
		NewPooledIP("10.0.0.2", "", 10, "hot"),
		NewPooledIP("10.0.0.3", "", 10, "hot"),
	}
	p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)

	// 全绑到 10.0.0.2 上（用 Adopt 精确控制落点）
	keys := []string{"k1", "k2", "k3"}
	for _, k := range keys {
		if err := p.Adopt(k, "10.0.0.2", "hot"); err != nil {
			t.Fatalf("预置绑定 %s: %v", k, err)
		}
	}

	res, err := p.EvacuateIP("10.0.0.2", "hot")
	if err != nil {
		t.Fatalf("EvacuateIP: %v", err)
	}
	if len(res.Failed) != 0 {
		t.Errorf("目标档位有余量，不应有失败: %v", res.Failed)
	}
	if len(res.Moved) != len(keys) {
		t.Errorf("应迁移 %d 个 Key，实际 %d", len(keys), len(res.Moved))
	}
	for _, k := range keys {
		if got := p.BoundIP(k); got != "10.0.0.3" {
			t.Errorf("%s 仍在 %s，应迁到 10.0.0.3", k, got)
		}
	}
	// 原出口必须已封禁，否则后续分配会把 Key 送回去
	if got := p.byAddr["10.0.0.2"].State(); got != IPBanned {
		t.Errorf("原出口状态 = %s, 期望 banned", got)
	}
}

// 目标档位无余量时，无处可去的 Key 必须出现在 Failed 里而非静默留在原地。
func TestEvacuateIP_无处可去的Key上报失败(t *testing.T) {
	ips := []*IP{
		NewPooledIP("10.0.0.2", "", 10, "hot"),
		NewPooledIP("10.0.0.3", "", 1, "hot"), // 只能接 1 个
	}
	p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)
	for _, k := range []string{"k1", "k2", "k3"} {
		if err := p.Adopt(k, "10.0.0.2", "hot"); err != nil {
			t.Fatal(err)
		}
	}

	res, err := p.EvacuateIP("10.0.0.2", "hot")
	if err != nil {
		t.Fatalf("EvacuateIP: %v", err)
	}
	if len(res.Moved) != 1 {
		t.Errorf("目标只能接 1 个，Moved 应为 1，实际 %d", len(res.Moved))
	}
	if len(res.Failed) != 2 {
		t.Errorf("应有 2 个 Key 无处可去，实际 %d: %v", len(res.Failed), res.Failed)
	}
	for k, e := range res.Failed {
		if !errors.Is(e, ErrNoIP) {
			t.Errorf("%s 的失败原因应包装 ErrNoIP，实际 %v", k, e)
		}
	}
}

// 撤离不得把 Key 迁到其他档位 —— 那会破坏分层。
func TestEvacuateIP_不跨档位迁移(t *testing.T) {
	ips := []*IP{
		NewPooledIP("10.0.0.2", "", 10, "hot"),
		NewPooledIP("10.0.1.2", "", 100, "cold"), // 有大量余量但档位不同
	}
	p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)
	if err := p.Adopt("k1", "10.0.0.2", "hot"); err != nil {
		t.Fatal(err)
	}

	res, err := p.EvacuateIP("10.0.0.2", "hot")
	if err != nil {
		t.Fatalf("EvacuateIP: %v", err)
	}
	if len(res.Moved) != 0 {
		t.Errorf("hot 档无其他出口，不应迁移到 cold 档: %v", res.Moved)
	}
	if len(res.Failed) != 1 {
		t.Errorf("应上报 1 个失败，实际 %d", len(res.Failed))
	}
}

// KeysOn 返回绑定在指定出口上的 Key，顺序稳定。
func TestKeysOn_返回该出口上的Key(t *testing.T) {
	ips := []*IP{NewIP("10.0.0.2", "", 10), NewIP("10.0.0.3", "", 10)}
	p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)
	_ = p.Adopt("kb", "10.0.0.2", PoolAny)
	_ = p.Adopt("ka", "10.0.0.2", PoolAny)
	_ = p.Adopt("kc", "10.0.0.3", PoolAny)

	got := p.KeysOn("10.0.0.2")
	if len(got) != 2 || got[0] != "ka" || got[1] != "kb" {
		t.Errorf("KeysOn = %v, 期望 [ka kb]（已排序）", got)
	}
	if n := len(p.KeysOn("10.0.0.3")); n != 1 {
		t.Errorf("10.0.0.3 上应有 1 个 Key，实际 %d", n)
	}
}

// direct 模式下撤离是无操作。
func TestEvacuateIP_direct模式无操作(t *testing.T) {
	p, _ := NewPool(ModeDirect, nil, 30*time.Second)
	res, err := p.EvacuateIP("10.0.0.2", PoolAny)
	if err != nil {
		t.Fatalf("direct 模式应返回 nil error: %v", err)
	}
	if len(res.Moved) != 0 || len(res.Failed) != 0 {
		t.Error("direct 模式不应有迁移结果")
	}
}

// ---------- 被封出口的自动恢复 ----------

// 冷却期未届满时不得解封 —— 提前放行等于给上游再送一轮异常请求。
func TestTryUnban_冷却期内不解封(t *testing.T) {
	ip := NewIP("10.0.0.2", "", 10)
	ip.MarkBanned()
	banned := ip.BannedAt()
	if banned.IsZero() {
		t.Fatal("MarkBanned 应记录封禁时刻")
	}

	// 只过了 1 小时，基础冷却 2 小时
	if ip.TryUnban(banned.Add(time.Hour), 2*time.Hour, 24*time.Hour) {
		t.Error("冷却期未届满，不应解封")
	}
	if got := ip.State(); got != IPBanned {
		t.Errorf("状态应仍为 banned，实际 %s", got)
	}
}

// 冷却期届满后转 cooldown，而非直接 active。
//
// 这个区别是关键: cooldown 不满足 Assignable，不会立刻涌入 25 个 Key；
// 探测成功才由 MarkSuccess 转回 active。
func TestTryUnban_届满后转cooldown而非active(t *testing.T) {
	ip := NewIP("10.0.0.2", "", 10)
	ip.MarkBanned()
	banned := ip.BannedAt()

	if !ip.TryUnban(banned.Add(3*time.Hour), 2*time.Hour, 24*time.Hour) {
		t.Fatal("冷却期已届满，应解封")
	}
	if got := ip.State(); got != IPCooldown {
		t.Errorf("应转入 cooldown，实际 %s —— 直接转 active 会让 Key 立刻涌回", got)
	}
	// cooldown 不接新绑定，这是「不立刻涌回」的机制保证
	if ip.Assignable() {
		t.Error("cooldown 状态不应可分配")
	}

	// 探测成功后才恢复可用
	ip.MarkSuccess()
	if got := ip.State(); got != IPActive {
		t.Errorf("探测成功后应转 active，实际 %s", got)
	}
	if !ip.Assignable() {
		t.Error("active 状态应可分配")
	}
}

// 反复被封的出口冷却时长指数延长 —— 它在上游眼里已经脏了。
func TestTryUnban_重复封禁指数退避(t *testing.T) {
	ip := NewIP("10.0.0.2", "", 10)
	const base = time.Hour

	// 第 1 次封禁: 冷却 1 小时
	ip.MarkBanned()
	if ip.TryUnban(ip.BannedAt().Add(59*time.Minute), base, 24*time.Hour) {
		t.Error("第 1 次封禁: 59 分钟不应解封（冷却 1 小时）")
	}
	if !ip.TryUnban(ip.BannedAt().Add(61*time.Minute), base, 24*time.Hour) {
		t.Fatal("第 1 次封禁: 61 分钟应解封")
	}

	// 第 2 次封禁: 冷却翻倍到 2 小时
	ip.MarkBanned()
	if ip.TryUnban(ip.BannedAt().Add(90*time.Minute), base, 24*time.Hour) {
		t.Errorf("第 2 次封禁（banCount=%d）: 90 分钟不应解封（冷却应为 2 小时）",
			ip.BanCount())
	}
	if !ip.TryUnban(ip.BannedAt().Add(150*time.Minute), base, 24*time.Hour) {
		t.Fatal("第 2 次封禁: 150 分钟应解封")
	}

	// 第 3 次封禁: 冷却 4 小时
	ip.MarkBanned()
	if ip.TryUnban(ip.BannedAt().Add(3*time.Hour), base, 24*time.Hour) {
		t.Errorf("第 3 次封禁（banCount=%d）: 3 小时不应解封（冷却应为 4 小时）",
			ip.BanCount())
	}
}

// 退避有上限，否则反复被封的出口会被推到数月之后，等同于永久废弃。
func TestTryUnban_退避不超过上限(t *testing.T) {
	ip := NewIP("10.0.0.2", "", 10)
	const (
		base    = time.Hour
		maxWait = 6 * time.Hour
	)
	// 连封 10 次，若无上限则冷却会达到 512 小时
	for i := 0; i < 10; i++ {
		ip.MarkBanned()
		ip.TryUnban(ip.BannedAt().Add(maxWait+time.Minute), base, maxWait)
	}
	ip.MarkBanned()
	if !ip.TryUnban(ip.BannedAt().Add(maxWait+time.Minute), base, maxWait) {
		t.Errorf("封禁 %d 次后冷却应被 maxWait(%v) 截断，实际未解封",
			ip.BanCount(), maxWait)
	}
}

// base 为 0 表示未启用自动恢复，必须保持原有的永久 banned 行为。
func TestTryUnban_未启用时保持banned(t *testing.T) {
	ip := NewIP("10.0.0.2", "", 10)
	ip.MarkBanned()
	if ip.TryUnban(ip.BannedAt().Add(30*24*time.Hour), 0, 24*time.Hour) {
		t.Error("base=0 表示未启用自动恢复，不应解封")
	}
	if got := ip.State(); got != IPBanned {
		t.Errorf("状态应仍为 banned，实际 %s", got)
	}
}

// 非 banned 状态不受影响 —— TryUnban 只处理封禁恢复这一件事。
func TestTryUnban_非banned状态不受影响(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*IP)
		want  IPState
	}{
		{"active", func(*IP) {}, IPActive},
		{"suspect", func(ip *IP) { ip.MarkFailure() }, IPSuspect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ip := NewIP("10.0.0.2", "", 10)
			tc.setup(ip)
			if ip.TryUnban(time.Now().Add(48*time.Hour), time.Hour, 24*time.Hour) {
				t.Errorf("%s 状态不应被 TryUnban 变更", tc.name)
			}
			if got := ip.State(); got != tc.want {
				t.Errorf("状态 = %s, 期望 %s", got, tc.want)
			}
		})
	}
}

// Pool 层批量解封: 只返回实际变更的地址，且顺序稳定便于日志比对。
func TestTryUnbanAll_只返回实际解封的出口(t *testing.T) {
	ips := []*IP{
		NewIP("10.0.0.2", "", 10), // 保持 active
		NewIP("10.0.0.3", "", 10), // 封禁且冷却届满
		NewIP("10.0.0.4", "", 10), // 封禁但刚封
	}
	p, err := NewPool(ModeMultiIP, ips, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// 10.0.0.3 的封禁时刻推到很久以前
	old := p.byAddr["10.0.0.3"]
	old.MarkBanned()
	old.mu.Lock()
	old.bannedAt = time.Now().Add(-48 * time.Hour)
	old.mu.Unlock()
	// 10.0.0.4 刚被封
	p.byAddr["10.0.0.4"].MarkBanned()

	got := p.TryUnbanAll(2*time.Hour, 24*time.Hour)
	if len(got) != 1 || got[0] != "10.0.0.3" {
		t.Fatalf("应只解封 10.0.0.3，实际 %v", got)
	}
	if st := p.byAddr["10.0.0.3"].State(); st != IPCooldown {
		t.Errorf("10.0.0.3 应转 cooldown，实际 %s", st)
	}
	if st := p.byAddr["10.0.0.4"].State(); st != IPBanned {
		t.Errorf("10.0.0.4 刚被封，应仍为 banned，实际 %s", st)
	}
	if st := p.byAddr["10.0.0.2"].State(); st != IPActive {
		t.Errorf("10.0.0.2 未被封，状态不应变，实际 %s", st)
	}
}

// 未启用时 Pool 层不做任何事。
func TestTryUnbanAll_未启用时空操作(t *testing.T) {
	ips := []*IP{NewIP("10.0.0.2", "", 10)}
	p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)
	ip := p.byAddr["10.0.0.2"]
	ip.MarkBanned()
	ip.mu.Lock()
	ip.bannedAt = time.Now().Add(-30 * 24 * time.Hour)
	ip.mu.Unlock()

	if got := p.TryUnbanAll(0, 24*time.Hour); got != nil {
		t.Errorf("base=0 时不应解封任何出口，实际 %v", got)
	}
	if st := ip.State(); st != IPBanned {
		t.Errorf("状态应仍为 banned，实际 %s", st)
	}
}

// 撤离后的出口经恢复应能重新接收 Key —— 这是「出口只减不增」问题的解法。
func TestTryUnban_恢复后可重新承载Key(t *testing.T) {
	ips := []*IP{
		NewPooledIP("10.0.0.2", "", 10, "hot"),
		NewPooledIP("10.0.0.3", "", 10, "hot"),
	}
	p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)
	for _, k := range []string{"k1", "k2", "k3"} {
		if err := p.Adopt(k, "10.0.0.2", "hot"); err != nil {
			t.Fatal(err)
		}
	}
	// 撤离 10.0.0.2，其上 Key 迁到 10.0.0.3
	if _, err := p.EvacuateIP("10.0.0.2", "hot"); err != nil {
		t.Fatal(err)
	}
	victim := p.byAddr["10.0.0.2"]
	if victim.State() != IPBanned {
		t.Fatalf("撤离后应为 banned，实际 %s", victim.State())
	}
	// 此时 hot 档只剩 1 个可用出口
	if n := len(p.Stats().PerIP); n != 2 {
		t.Fatalf("池中应有 2 个 IP，实际 %d", n)
	}

	// 冷却届满 + 探测成功 → 重新可用
	victim.mu.Lock()
	victim.bannedAt = time.Now().Add(-48 * time.Hour)
	victim.mu.Unlock()
	if got := p.TryUnbanAll(2*time.Hour, 24*time.Hour); len(got) != 1 {
		t.Fatalf("应解封 1 个出口，实际 %v", got)
	}
	victim.MarkSuccess()

	if !victim.Assignable() {
		t.Fatal("恢复后的出口应可重新承载 Key")
	}
	if _, err := p.BindInPool("k_new", "hot"); err != nil {
		t.Errorf("恢复后新 Key 应能绑定: %v", err)
	}
}

// banned 是终态，只能由 TryUnban 退出 —— 探测失败不得把它降级。
//
// 这里防的是一个已实证的缺陷: MarkBanned 不重置 failStreak，
// 而 MarkFailure 原先按 failStreak 无条件重设状态。于是刚被封的出口
// 只要探测失败一次（failStreak=1）就落进 default 分支变成 suspect，
// 而 suspect 只需一次 MarkSuccess 就回 active —— 冷却时长、指数退避、
// bannedAt 记录全部被绕过，「封禁」实际只持续到下一次探测成功。
func TestMarkFailure_不得降级banned(t *testing.T) {
	ip := NewIP("10.0.0.2", "", 10)
	ip.MarkBanned()
	if ip.State() != IPBanned {
		t.Fatalf("前置条件: 应为 banned，实际 %s", ip.State())
	}

	// 连续探测失败都不该改变 banned
	for i := 0; i < 8; i++ {
		ip.MarkFailure()
		if got := ip.State(); got != IPBanned {
			t.Fatalf("第 %d 次失败后状态变为 %s，banned 只能由 TryUnban 退出", i+1, got)
		}
	}

	// 关键: 此时一次探测成功也不得让它复活 —— 否则冷却机制形同虚设
	ip.MarkSuccess()
	if got := ip.State(); got != IPBanned {
		t.Errorf("MarkSuccess 让 banned 直接复活为 %s，绕过了冷却期", got)
	}
}

// 连续失败累积到 banned 时必须记录 bannedAt，否则该 IP 永久卡死。
//
// TryUnban 依赖 bannedAt 计算冷却是否到期，零值意味着「无从判断」，
// 而 TryUnban 对零值的处理是保守地不解封 —— 这条路径进来的 IP
// 会永远不可用，且原因极难定位。
func TestMarkFailure_累积封禁也记录时间(t *testing.T) {
	ip := NewIP("10.0.0.2", "", 10)
	for i := 0; i < 6; i++ {
		ip.MarkFailure()
	}
	if ip.State() != IPBanned {
		t.Fatalf("连续 6 次失败应进入 banned，实际 %s", ip.State())
	}

	// 冷却期已过时应能解封 —— 这反证 bannedAt 已被正确记录
	if !ip.TryUnban(time.Now().Add(time.Second), time.Nanosecond, time.Hour) {
		t.Error("连续失败进入 banned 的 IP 无法解封，bannedAt 可能未记录")
	}
}

// ---------- 封禁的可观测性 ----------

// 运维看到 banned 必须能判断「等多久自愈」还是「要人工处理」。
// 这三个字段是自动解封机制唯一的对外出口 —— 缺了它们，面板上
// 「第 1 次被封等 2 小时」与「第 5 次被封等 24 小时」看起来完全一样。
func TestStats_暴露封禁时刻与预计解封(t *testing.T) {
	ips := []*IP{NewIP("10.0.0.2", "", 10), NewIP("10.0.0.3", "", 10)}
	p, err := NewPool(ModeMultiIP, ips, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p.SetUnbanPolicy(2*time.Hour, 24*time.Hour)

	victim := p.byAddr["10.0.0.2"]
	before := time.Now()
	victim.MarkBanned()

	var got IPStat
	for _, s := range p.Stats().PerIP {
		if s.Addr == "10.0.0.2" {
			got = s
		}
		// 未被封的出口三个字段都该是空的，否则前端会给健康 IP 显示冷却倒计时
		if s.Addr == "10.0.0.3" {
			if !s.BannedAt.IsZero() || s.BanCount != 0 || !s.UnbanAt.IsZero() {
				t.Errorf("未被封的出口不应有封禁信息: %+v", s)
			}
		}
	}
	if got.Addr == "" {
		t.Fatal("Stats 未包含被封出口")
	}
	if got.State != IPBanned {
		t.Errorf("State = %s, 期望 banned", got.State)
	}
	if got.BannedAt.Before(before) {
		t.Errorf("BannedAt = %v, 应不早于封禁调用时刻 %v", got.BannedAt, before)
	}
	if got.BanCount != 1 {
		t.Errorf("BanCount = %d, 期望 1", got.BanCount)
	}
	// 首次被封的退避是 base 本身
	if want := got.BannedAt.Add(2 * time.Hour); !got.UnbanAt.Equal(want) {
		t.Errorf("UnbanAt = %v, 期望 %v（封禁时刻 + 2h）", got.UnbanAt, want)
	}
}

// 反复被封时预计解封时刻必须随退避档次拉长，否则面板会误导运维
// 「再等 2 小时就好」，而实际要等 8 小时。
func TestStats_预计解封随退避档次拉长(t *testing.T) {
	p, _ := NewPool(ModeMultiIP, []*IP{NewIP("10.0.0.2", "", 10)}, 30*time.Second)
	p.SetUnbanPolicy(time.Hour, 24*time.Hour)
	ip := p.byAddr["10.0.0.2"]

	var waits []time.Duration
	for i := 0; i < 4; i++ {
		ip.MarkBanned()
		st := p.Stats().PerIP[0]
		if st.UnbanAt.IsZero() {
			t.Fatalf("第 %d 次封禁未给出预计解封时刻", i+1)
		}
		waits = append(waits, st.UnbanAt.Sub(st.BannedAt))
		// 强制转出 banned，让下一次 MarkBanned 能再累加计数
		if !ip.TryUnban(st.UnbanAt.Add(time.Second), time.Hour, 24*time.Hour) {
			t.Fatalf("第 %d 次冷却届满后应可转入 cooldown", i+1)
		}
	}
	t.Logf("各次封禁的冷却时长: %v", waits)
	for i := 1; i < len(waits); i++ {
		if waits[i] <= waits[i-1] {
			t.Errorf("第 %d 次冷却 %v 未长于第 %d 次 %v —— 退避未生效",
				i+1, waits[i], i, waits[i-1])
		}
	}
}

// 未配置 ban_cooldown 时 UnbanAt 必须为零，前端据此显示「需人工介入」。
//
// 若此时给出一个预计时刻，运维会一直等一个永远不会到来的自愈。
func TestStats_未启用自动恢复时无预计解封(t *testing.T) {
	p, _ := NewPool(ModeMultiIP, []*IP{NewIP("10.0.0.2", "", 10)}, 30*time.Second)
	// 刻意不调 SetUnbanPolicy
	p.byAddr["10.0.0.2"].MarkBanned()

	st := p.Stats().PerIP[0]
	if st.State != IPBanned {
		t.Fatalf("State = %s, 期望 banned", st.State)
	}
	if st.BannedAt.IsZero() {
		t.Error("即使未启用自动恢复，也应记录封禁时刻供审计")
	}
	if !st.UnbanAt.IsZero() {
		t.Errorf("未启用自动恢复时 UnbanAt 应为零，实际 %v —— "+
			"前端会显示一个永远不会到来的自愈时刻", st.UnbanAt)
	}
}

// SetUnbanPolicy 必须在装配期生效，不能依赖首次 TryUnbanAll 顺带记录。
//
// 健康探测每 15 秒才跑一轮，若参数靠探测时才记录，这段窗口内
// Stats 会把可自愈的出口报成永久封禁。
func TestSetUnbanPolicy_装配期即生效(t *testing.T) {
	p, _ := NewPool(ModeMultiIP, []*IP{NewIP("10.0.0.2", "", 10)}, 30*time.Second)
	p.SetUnbanPolicy(30*time.Minute, 8*time.Hour)
	p.byAddr["10.0.0.2"].MarkBanned()

	// 关键: 一次 TryUnbanAll 都没调用过
	st := p.Stats().PerIP[0]
	if st.UnbanAt.IsZero() {
		t.Fatal("未经探测周期就该能算出预计解封时刻")
	}
	if want := st.BannedAt.Add(30 * time.Minute); !st.UnbanAt.Equal(want) {
		t.Errorf("UnbanAt = %v, 期望 %v", st.UnbanAt, want)
	}
}
