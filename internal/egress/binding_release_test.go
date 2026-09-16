package egress

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// 本文件守两条不变量，它们对应 livetest-ai KI-035 的两个后果:
//
//	① 库中已不存在的 Key 必须被回收绑定 —— 否则容量「假满」，
//	   新导入的 Key 全部绑不上出口；
//	② 候选集为空时必须按**成因**报告 —— 旧的通用文案把排查引向档位与份额，
//	   而实测成因是容量被已不存在的 Key 占满。
//
// 同时钉住边界: 回收的判据是「库中是否还有这个 Key」，**不是**它的状态。
// banned / cooldown 的 Key 仍在库中，它们的绑定必须留着 —— 复活时要回同一个
// 出口，换出口等于凭空制造一次「老账号换了 IP」。

func TestReleaseBindings_回收库中已不存在的Key(t *testing.T) {
	ip := NewIP("127.0.0.1", "1.2.3.4", 2) // 只容 2 个，便于观察名额释放
	p, err := NewPool(ModeMultiIP, []*IP{ip}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"k1", "k2"} {
		if _, err := p.Bind(id); err != nil {
			t.Fatalf("预置绑定 %s: %v", id, err)
		}
		// 请求路径会为每个 Key 建立独立客户端，回收时必须一并丢弃 ——
		// 只删绑定不删客户端等于让一整套 Transport 连接池永久驻留。
		if _, err := p.ClientFor(id); err != nil {
			t.Fatalf("建立 %s 的客户端: %v", id, err)
		}
	}

	// 前置事实: 此刻容量已满，第 3 个 Key 绑不进来。
	if _, err := p.Bind("k3"); !errors.Is(err, ErrNoIP) {
		t.Fatalf("池满时应返回 ErrNoIP，实际 %v", err)
	}

	// 库中删掉了 k2（对账时 keep 只剩 k1）。
	if n := p.ReleaseBindings(map[string]bool{"k1": true}); n != 1 {
		t.Fatalf("应回收 1 个绑定，实际 %d", n)
	}
	if st := p.Stats(); st.KeysBound != 1 {
		t.Errorf("回收后内存绑定数 = %d，期望 1", st.KeysBound)
	}
	if len(p.clients) != 1 {
		t.Errorf("回收后客户端数 = %d，期望 1（k2 的 Transport 必须一并释放）", len(p.clients))
	}
	// /admin/ips 的 bound_keys 直接来自这份快照 —— 它就是测试判据里
	// 「Σ bound_keys 应等于库中 Key 数」的那个数。
	if st := p.Stats(); st.PerIP[0].BoundKeys != 1 {
		t.Errorf("出口 %s 的 bound_keys = %d，期望 1", st.PerIP[0].Addr, st.PerIP[0].BoundKeys)
	}

	// 名额回到池子里：这是本修复的直接目的（容量假满 → 可继续导入 Key）。
	addr, err := p.Bind("k3")
	if err != nil {
		t.Fatalf("回收后应能绑定新 Key: %v", err)
	}
	if addr != ip.Addr {
		t.Errorf("新 Key 应绑到唯一出口 %s，实际 %s", ip.Addr, addr)
	}
}

func TestReleaseBindings_按库中是否存在判定而非按状态(t *testing.T) {
	p, err := NewPool(ModeMultiIP, []*IP{NewIP("127.0.0.1", "1.2.3.4", 10)}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	const key = "k_banned"
	if _, err := p.Bind(key); err != nil {
		t.Fatal(err)
	}
	p.IPFor(key).MarkBanned() // 出口被判封禁，但该 Key 仍在库中

	// keep 是「库中仍然存在的 key_id 全集」（含非 active 状态）⇒ 一个都不该回收。
	if n := p.ReleaseBindings(map[string]bool{key: true}); n != 0 {
		t.Fatalf("Key 仍在库中（仅状态非 active）时不应回收绑定，实际回收 %d 个", n)
	}
	if got := p.BoundIP(key); got == "" {
		t.Error("banned Key 的绑定必须保留：复活时要回同一个出口，换出口等于制造「老账号换 IP」")
	}
}

func TestReleaseBindings_direct模式无绑定可回收(t *testing.T) {
	p, err := NewPool(ModeDirect, nil, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if n := p.ReleaseBindings(nil); n != 0 {
		t.Errorf("direct 模式不应有绑定可回收，实际回收 %d 个", n)
	}
	if addr := p.Release("k1"); addr != "" {
		t.Errorf("direct 模式 Release 应返回空地址，实际 %q", addr)
	}
}

func TestRelease_单个Key解绑并返回原地址(t *testing.T) {
	ip := NewIP("127.0.0.1", "1.2.3.4", 10)
	p, err := NewPool(ModeMultiIP, []*IP{ip}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Bind("k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ClientFor("k1"); err != nil {
		t.Fatal(err)
	}

	if addr := p.Release("k1"); addr != ip.Addr {
		t.Errorf("Release 应返回被释放的地址 %s，实际 %q", ip.Addr, addr)
	}
	if got := p.BoundIP("k1"); got != "" {
		t.Errorf("解绑后 BoundIP 应为空，实际 %q", got)
	}
	if len(p.clients) != 0 {
		t.Errorf("解绑后客户端应被丢弃，实际仍有 %d 个", len(p.clients))
	}
	// 幂等: 重复释放不应报错，也不应误伤别的 Key。
	if addr := p.Release("k1"); addr != "" {
		t.Errorf("无绑定时 Release 应返回空串，实际 %q", addr)
	}
	if addr := p.Release(""); addr != "" {
		t.Errorf("空 key_id 应直接返回空串，实际 %q", addr)
	}
}

// TestBindInPool_候选为空时按成因报告 逐个成因断言文案判据。
//
// 断言的是**成因必须排在档位之前**，不是「文案里出现了某个词」——
// 旧文案 `档位 %q 无可用出口` 被原样保留在范围后缀里，只断言关键词会漏掉
// 「第一眼仍然指向档位」这个真实缺陷。
func TestBindInPool_候选为空时按成因报告(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T) (*Pool, string) // 返回池与要请求的档位
		want    string                             // 必须紧跟在 ErrNoIP 之后出现的成因
		wantSub []string                           // 文案里必须出现的判据
	}{
		{
			name: "容量已满",
			setup: func(t *testing.T) (*Pool, string) {
				ip := NewIP("10.0.0.1", "", 1)
				p, err := NewPool(ModeMultiIP, []*IP{ip}, 30*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := p.BindInPool("k1", "cold"); err != nil {
					t.Fatal(err)
				}
				return p, "cold"
			},
			want: "容量已满",
			// 容量满必须带上可核对的数字: 占了多少 / 上限多少 / 哪个出口。
			wantSub: []string{"10.0.0.1", "1/1", "合计上限 1"},
		},
		{
			name: "档位不符",
			setup: func(t *testing.T) (*Pool, string) {
				p, err := NewPool(ModeMultiIP, []*IP{NewPooledIP("10.0.0.2", "", 10, "hot")}, 30*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				return p, "cold"
			},
			want:    "档位不符",
			wantSub: []string{`"cold"`, "10.0.0.2"},
		},
		{
			name: "信誉不足或处于观察期",
			setup: func(t *testing.T) (*Pool, string) {
				ip := NewIP("10.0.0.3", "", 10)
				// 信誉只在 failStreak>=3（进 cooldown）那一步扣 10 分，
				// 而 MarkSuccess 会把 failStreak 清零 —— 所以「失败一次成功一次」
				// 无论重复多少轮都不会掉信誉（既存的 TestIP_LowReputationNotAssignable
				// 正是这样写的，故它恒 skip、从未真正断言过）。
				// 正确构造: 连续 3 次失败进 cooldown 扣分，再成功一次回到 active。
				for i := 0; i < 6; i++ {
					ip.MarkFailure()
					ip.MarkFailure()
					ip.MarkFailure()
					ip.MarkSuccess()
				}
				if ip.Assignable() {
					t.Fatalf("构造失败: 信誉 %d、状态 %s，未进入「信誉不足」", ip.Reputation(), ip.State())
				}
				p, err := NewPool(ModeMultiIP, []*IP{ip}, 30*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				return p, "cold"
			},
			want:    "暂停分配",
			wantSub: []string{"10.0.0.3", "信誉="},
		},
		{
			name: "已封禁",
			setup: func(t *testing.T) (*Pool, string) {
				ip := NewIP("10.0.0.4", "", 10)
				ip.MarkBanned()
				p, err := NewPool(ModeMultiIP, []*IP{ip}, 30*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				return p, ""
			},
			want:    "已封禁",
			wantSub: []string{"10.0.0.4"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, pool := tc.setup(t)
			_, err := p.BindInPool("probe", pool)
			if err == nil {
				t.Fatal("候选集为空时应返回错误")
			}
			// 调用方（网关热路径）靠 errors.Is 判定「出口不可用」，必须保持可识别。
			if !errors.Is(err, ErrNoIP) {
				t.Fatalf("错误应可被 errors.Is(err, ErrNoIP) 识别，实际 %v", err)
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, ErrNoIP.Error()+": "+tc.want) {
				t.Errorf("成因 %q 必须紧跟在 %q 之后（先读到的应是成因而不是档位），实际: %s",
					tc.want, ErrNoIP.Error(), msg)
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(msg, sub) {
					t.Errorf("文案缺少判据 %q，实际: %s", sub, msg)
				}
			}
			// 直接改库不会立刻反映到容量算术上，这条提示必须带上。
			if !strings.Contains(msg, "对账") {
				t.Errorf("文案应提示「内存绑定量按库周期对账」，实际: %s", msg)
			}
			// 旧文案是收尾的断语「档位 %q 无可用出口」，它把成因断言成了
			// 档位问题 —— 逐字禁止它回归。只断言「出现了某个成因词」不够:
			// 那句话与成因可以并存，而先被读到的是它。
			if strings.Contains(msg, "无可用出口") {
				t.Errorf("文案仍含旧断语「无可用出口」，会把排查引向档位/份额配置，实际: %s", msg)
			}
		})
	}
}
