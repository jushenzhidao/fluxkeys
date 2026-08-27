package egress

import (
	"fmt"
	"testing"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/persona"
)

// recommendedKeyCount 是单台 32 IP 机器的推荐承载量。
//
// 这个数字经历过三次修正，值得记下推导链:
//
//  1. 1000（离线推演 cold=100/IP）—— 错，推演假设 Key 均匀分布到 72 个
//     作息模板，实际哈希分配必然有重复，峰值被低估 3 倍。
//  2. 650（全档统一 MaxKeys=25）—— 对，但过于保守。当时调度器不区分
//     档位，cold 档 Key 与 hot 档同频被选中，只能按最严标准配置全池。
//  3. 1000（分层 MaxKeys）—— 调度器按 PoolShares 配比后，cold 档单 Key
//     的实际请求频率只有 hot 档的 1%（实测），它的 max_keys 可以远高于
//     hot 档而不推高等效密度。
const recommendedKeyCount = 1000

// 各档位的 max_keys。差异来自「等效密度」而非拍脑袋:
//
//	等效密度 = 画像峰值并发 × 该档单 Key 的相对请求频率
//
// 相对频率由调度器的 PoolShares 决定，实测（份额 70/25/5、1000 Key）为
// hot=1.000 / warm=0.181 / cold=0.010。
//
// 基准取 hot 档 10 Key/IP 的实测峰值 —— 分布为 min=2 / 中位=4 / max=5、
// 均值 3.4，故上限定在 5.5（覆盖最差值并留方差空间）。各档实测:
//
//	hot   10/IP → 画像峰值  4-5 × 1.000 = 4.0-5.0  ← 瓶颈档位
//	warm  50/IP → 画像峰值   10 × 0.181 = 1.81
//	cold 100/IP → 画像峰值   21 × 0.010 = 0.21
//
// hot 档是唯一的瓶颈: 它承接 70% 的流量，每个 Key 都在高频使用，
// 必须保持低密度。cold 档相反 —— 700 个 Key 合计只拿 5% 流量，
// 单 Key 平均每 20000 次请求才被选中 1.4 次。
//
// warm/cold 没有取到等效密度允许的上限（60/200），而是留了一档余量:
// 相对频率随 PoolShares 变化，若日后把 cold 份额从 5% 调到 10%，
// 200/IP 的等效密度会翻倍。留余量让份额微调不必同时改出口配置。
const (
	maxKeysHot  = 10
	maxKeysWarm = 50
	maxKeysCold = 100
)

// maxEffectiveDensity 是单 IP 等效密度的上限，见上方推导。
//
// 这个指标只对**高频档位**有约束力。cold 档相对频率仅 0.010，按此模型
// 它要到 max_keys ≈ 2200 才触顶 —— 实际限制 cold 的是另外两条:
//
//  1. 撤离余量（见 TestCapacity_撤离余量的临界值）: max_keys 越大，
//     封掉一个 IP 时需要的空位越多，反而要配更多 IP。
//  2. pool_shares 的稳定性: 相对频率由份额决定，把 cold 从 5% 调到 10%
//     会让等效密度翻倍。
//
// 所以 cold 取 100 而非模型允许的上限，是为了给份额调整留空间。
// 若看到「把 cold 的 max_keys 调到很大而测试仍通过」，那是正确结论
// 而非测试失灵 —— 该配置的问题在撤离成本，不在密度。
const maxEffectiveDensity = 5.5

// buildRecommended32IP 按 deploy/README.md 推荐配置构造出口池。
//
// 「承载」与「备用」只是规划口径上的区分，运行时所有 IP 都会被 Bind 填入。
// 真正的约束是**撤离余量**: 封掉一个出口后，同档其余 IP 必须装得下它的 Key。
// Bind 均摊后单出口负载约为 keys/n，故
//
//	(n-1) × max_keys >= keys   →   n >= keys/max_keys + 1
//
// 实测验证（TestCapacity_撤离余量的临界值）: cold 档 700 Key / 100 上限时，
// 7 个 IP 撤离全部失败、8 个全部成功，临界点正是这个公式。
func buildRecommended32IP() []*IP {
	var ips []*IP
	add := func(from, to int, pool string, maxKeys int) {
		for i := from; i <= to; i++ {
			ips = append(ips, NewPooledIP(fmt.Sprintf("172.16.0.%d", i), "", maxKeys, pool))
		}
	}
	// 按公式的下限是 hot 11 / warm 5 / cold 8 = 24 个，余出 8 个。
	// 全部补给 hot —— 它承接 70% 流量、是唯一的密度瓶颈档，
	// 多给 IP 能直接摊薄其单 IP 负载（11 个时 9.1/IP，19 个时 5.3/IP）。
	add(11, 29, "hot", maxKeysHot)   // 19 × 10 = 190，承载 100 → 平均 5.3/IP
	add(30, 34, "warm", maxKeysWarm) // 5 × 50 = 250，承载 200
	add(35, 42, "cold", maxKeysCold) // 8 × 100 = 800，承载 700
	return ips
}

// peakConcurrency 返回给定 Key 集合在 24 小时内的最大同时活跃数。
//
// 这是风控实际观测到的密度指标 —— 绑定总数只是上限，同一时刻有多少 Key
// 在活动才决定「这个 IP 看起来像几个人在用」。
func peakConcurrency(keys []string) (peak, hour int) {
	for h := 0; h < 24; h++ {
		ts := time.Date(2026, 8, 24, h, 30, 0, 0, time.Local)
		n := 0
		for _, k := range keys {
			if persona.For(k).IsActiveAt(ts) {
				n++
			}
		}
		if n > peak {
			peak, hour = n, h
		}
	}
	return peak, hour
}

// 用真实代码验证 deploy/README.md 宣称的容量。
//
// 此前这个数字是离线推演的，而推演偏低了 3 倍（假设 Key 均匀分布到 72 个
// 作息模板，实际哈希分配必然有重复）。容量承诺必须由代码验证。
func TestCapacity_32IP推荐容量(t *testing.T) {
	ips := buildRecommended32IP()
	if len(ips) != 32 {
		t.Fatalf("测试自身配置错误: IP 数 = %d, 应为 32", len(ips))
	}
	p, err := NewPool(ModeMultiIP, ips, 30*time.Second)
	if err != nil {
		t.Fatalf("构造出口池: %v", err)
	}

	// 按 1:2:7 分档
	hotN, warmN := recommendedKeyCount/10, recommendedKeyCount*3/10
	poolOf := func(i int) string {
		switch {
		case i < hotN:
			return "hot"
		case i < warmN:
			return "warm"
		default:
			return "cold"
		}
	}
	var failed []string
	for i := 0; i < recommendedKeyCount; i++ {
		id := fmt.Sprintf("volc_%04d", i)
		if _, err := p.BindInPool(id, poolOf(i)); err != nil {
			failed = append(failed, fmt.Sprintf("%s(%s): %v", id, poolOf(i), err))
		}
	}
	if len(failed) != 0 {
		t.Fatalf("%d 个 Key 绑定失败，32 IP 无法承载 %d 个 Key，例如: %v",
			len(failed), recommendedKeyCount, failed[:min(3, len(failed))])
	}

	st := p.Stats()
	if st.KeysBound != recommendedKeyCount {
		t.Errorf("已绑定 Key 数 = %d, 期望 %d", st.KeysBound, recommendedKeyCount)
	}
	byPool := map[string]int{}
	for _, s := range st.PerIP {
		byPool[s.Pool] += s.BoundKeys
		if s.BoundKeys > s.MaxKeys {
			t.Errorf("IP %s 超载: %d/%d", s.Addr, s.BoundKeys, s.MaxKeys)
		}
	}
	t.Logf("各档实际承载: hot=%d warm=%d cold=%d", byPool["hot"], byPool["warm"], byPool["cold"])

	// 核心指标: 各档位单 IP 的**等效密度**。
	//
	// 分层后不能再用统一的「画像峰值 <= 10」口径 —— cold 档单 IP 绑 100 个
	// Key，画像峰值必然到 20+，但那些 Key 合计只拿 5% 的流量，
	// 实际请求密度远低于 hot 档。用统一口径会把设计意图误判为缺陷。
	type worst struct {
		peak, bound, hour int
		addr              string
	}
	worstOf := map[string]worst{}
	for _, s := range st.PerIP {
		keys := p.KeysOn(s.Addr)
		if len(keys) == 0 {
			continue
		}
		peak, hour := peakConcurrency(keys)
		if peak > worstOf[s.Pool].peak {
			worstOf[s.Pool] = worst{peak: peak, bound: len(keys), hour: hour, addr: s.Addr}
		}
	}

	for _, tc := range []struct {
		pool string
		freq float64 // 该档单 Key 的相对请求频率（实测，见 maxKeys* 常量注释）
	}{
		{"hot", 1.000},
		{"warm", 0.181},
		{"cold", 0.010},
	} {
		w := worstOf[tc.pool]
		if w.bound == 0 {
			t.Errorf("%s 档没有任何 IP 承载 Key", tc.pool)
			continue
		}
		effective := float64(w.peak) * tc.freq
		t.Logf("%-5s 最差 IP %s: 绑 %d 个，%02d 时峰值并发 %d，等效密度 %.2f",
			tc.pool, w.addr, w.bound, w.hour, w.peak, effective)

		// 硬断言: 等效密度不超过 maxEffectiveDensity。
		//
		// 基准是 hot 档 10 Key/IP 的实测峰值（中位 4、最差 5）——
		// 那是分层前全池都要满足的标准。超过说明要么画像窄化失效、
		// 要么该档 max_keys 与其流量份额不匹配（例如提高了 cold 份额
		// 却没同步下调 max_keys）。
		if effective > maxEffectiveDensity {
			t.Errorf("%s 档等效密度 %.2f 超过上限 %.1f（峰值 %d × 频率 %.3f）。"+
				"要么画像窄化失效（检查 internal/persona 的 windowDurations），"+
				"要么该档 max_keys 与 scheduler.pool_shares 的份额不匹配",
				tc.pool, effective, maxEffectiveDensity, w.peak, tc.freq)
		}

		// 峰值占比不超过 50%: 用于识别画像窄化整体失效。
		//
		// 阈值取 50% 而非更严的 1/3 —— 实测占比在 20-30% 区间，但小样本
		// 单 IP 方差大。50% 足以区分「已窄化」与「未窄化」（宽模板会到 80%+）。
		if w.bound >= 20 && w.peak*2 > w.bound {
			t.Errorf("%s 档峰值并发 %d/%d 超过 50%%，画像窄化可能失效",
				tc.pool, w.peak, w.bound)
		}
	}
}

// 撤离一个满载出口，验证备用 IP 是否接得住。
//
// 这是 6 个备用 IP 的存在理由: 出口被判封禁后，其上 Key 需要同档位的落脚点，
// 否则它们会卡在被封出口上，不可用且不会自愈。
func TestCapacity_满载出口撤离时备用够用(t *testing.T) {
	p, err := NewPool(ModeMultiIP, buildRecommended32IP(), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// 只填 cold 档，它是承载压力最大的一档
	for i := recommendedKeyCount * 3 / 10; i < recommendedKeyCount; i++ {
		if _, err := p.BindInPool(fmt.Sprintf("volc_%04d", i), "cold"); err != nil {
			t.Fatalf("预置绑定失败: %v", err)
		}
	}

	// 挑承载最多的 cold 出口撤离
	victim, maxKeys := "", 0
	for _, s := range p.Stats().PerIP {
		if s.Pool == "cold" && s.BoundKeys > maxKeys {
			victim, maxKeys = s.Addr, s.BoundKeys
		}
	}
	t.Logf("撤离出口 %s（承载 %d 个 Key）", victim, maxKeys)

	res, err := p.EvacuateIP(victim, "cold")
	if err != nil {
		t.Fatalf("EvacuateIP: %v", err)
	}
	if len(res.Failed) != 0 {
		t.Errorf("cold 档备用容量应能接住 %d 个 Key，实际 %d 个无处可去: %v",
			maxKeys, len(res.Failed), res.Failed)
	}
	if len(res.Moved) != maxKeys {
		t.Errorf("应迁移 %d 个，实际 %d 个", maxKeys, len(res.Moved))
	}
	if left := p.KeysOn(victim); len(left) != 0 {
		t.Errorf("仍有 %d 个 Key 留在被封出口", len(left))
	}

	// 迁移后不得有出口超载
	for _, s := range p.Stats().PerIP {
		if s.BoundKeys > s.MaxKeys {
			t.Errorf("撤离后 IP %s 超载: %d/%d", s.Addr, s.BoundKeys, s.MaxKeys)
		}
	}
}

// 验证各档 max_keys 在**被填满**时的等效密度。
//
// 与 TestCapacity_32IP推荐容量 的区别，也是这个用例存在的理由:
// 那个用例按推荐 IP 数配置，Bind 会把 Key 均摊，各 IP 的实际负载远低于
// max_keys（hot 档 100 个 Key 摊到 17 个 IP 只有 5.9 个/IP）。
// 于是 max_keys 成了一个**无效上限** —— 把它从 10 改成 50 也不会有任何
// IP 真装到 50 个，密度断言抓不出这个错。
//
// max_keys 真正的语义是「最坏情况下单 IP 能装多少」。缩减 IP 数直到
// 每个 IP 都被填满，才是它该被检验的场景 —— 运维缩容、大批 IP 被封后
// 恰恰会出现这种局面。
func TestCapacity_各档满载时的等效密度(t *testing.T) {
	for _, tc := range []struct {
		pool    string
		maxKeys int
		freq    float64
	}{
		{"hot", maxKeysHot, 1.000},
		{"warm", maxKeysWarm, 0.181},
		{"cold", maxKeysCold, 0.010},
	} {
		t.Run(tc.pool, func(t *testing.T) {
			// 用固定的 8 个 IP 并按 max_keys 反推 Key 数，而非用该档
			// 真实的 Key 总量。
			//
			// 用真实总量（cold 700）会有个陷阱: max_keys 若被改到 400，
			// 700 个 Key 只够填满 1 个 IP，构造不出满载场景，
			// 测试反而抓不出这个激进配置。按 max_keys 反推则始终满载。
			const n = 8
			keys := n * tc.maxKeys
			var ips []*IP
			for i := 0; i < n; i++ {
				ips = append(ips, NewPooledIP(
					fmt.Sprintf("10.%d.0.%d", i/250, i%250+2), "", tc.maxKeys, tc.pool))
			}
			p, err := NewPool(ModeMultiIP, ips, 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < keys; i++ {
				if _, err := p.BindInPool(fmt.Sprintf("volc_%05d", i), tc.pool); err != nil {
					t.Fatalf("预置绑定失败（第 %d 个）: %v", i, err)
				}
			}

			worstPeak, worstBound := 0, 0
			for _, s := range p.Stats().PerIP {
				keys := p.KeysOn(s.Addr)
				if len(keys) == 0 {
					continue
				}
				peak, _ := peakConcurrency(keys)
				if peak > worstPeak {
					worstPeak, worstBound = peak, len(keys)
				}
			}
			if worstBound != tc.maxKeys {
				t.Fatalf("构造有误: 最差 IP 只绑了 %d 个，应为满载的 %d 个",
					worstBound, tc.maxKeys)
			}
			effective := float64(worstPeak) * tc.freq
			t.Logf("%s 档满载: %d 个 IP × %d 上限，最差 IP 绑 %d 个，"+
				"峰值并发 %d，等效密度 %.2f",
				tc.pool, n, tc.maxKeys, worstBound, worstPeak, effective)

			// 满载时才是 max_keys 的真实考验
			if effective > maxEffectiveDensity {
				t.Errorf("%s 档 max_keys=%d 满载时等效密度 %.2f 超过上限 %.1f。"+
					"该档承担 %.1f%% 的流量强度，max_keys 需下调",
					tc.pool, tc.maxKeys, effective, maxEffectiveDensity, 100*tc.freq)
			}
		})
	}
}

// 撤离余量的真实约束: (n-1)×max_keys >= keys，即
//
//	n >= keys/max_keys + 1
//
// 注意是 +1 而非 +2。我一开始按「撤离需要 max_keys 个空位」推出 +2，
// 那是把上限当成了实际负载 —— Bind 会把 Key 均摊，单出口实际负载
// 约为 keys/n，通常远低于 max_keys。按 +2 规划会多买一个 IP。
//
// 与 TestCapacity_满载出口撤离时备用够用 的区别: 那个用例按推荐配置
// （余量充裕）验证，此处把 IP 数压到临界值两侧，直接检验这条不等式 ——
// 前者在余量充足时抓不出「IP 数少配了一个」。
func TestCapacity_撤离余量的临界值(t *testing.T) {
	const (
		pool    = "cold"
		maxKeys = 100
		keys    = 700
	)
	// n >= 700/100 + 1 = 8。故 7 个（刚好装满、零空位）应失败，8 个应成功。
	for _, tc := range []struct {
		n        int
		wantFail bool
		why      string
	}{
		{7, true, "刚好装满，撤离时零空位"},
		{8, false, "多一个 IP 的余量"},
	} {
		t.Run(fmt.Sprintf("%d个IP", tc.n), func(t *testing.T) {
			var ips []*IP
			for i := 0; i < tc.n; i++ {
				ips = append(ips, NewPooledIP(
					fmt.Sprintf("10.0.0.%d", i+2), "", maxKeys, pool))
			}
			p, _ := NewPool(ModeMultiIP, ips, 30*time.Second)
			for i := 0; i < keys; i++ {
				if _, err := p.BindInPool(fmt.Sprintf("volc_%04d", i), pool); err != nil {
					t.Fatalf("预置绑定失败: %v", err)
				}
			}
			// 挑承载最多的出口撤离 —— 最坏情况
			victim, most := "", 0
			for _, s := range p.Stats().PerIP {
				if s.BoundKeys > most {
					victim, most = s.Addr, s.BoundKeys
				}
			}
			res, err := p.EvacuateIP(victim, pool)
			if err != nil {
				t.Fatal(err)
			}

			// 其余 IP 的空位总数: 总容量减去它们已承载的 Key
			spare := (tc.n-1)*maxKeys - (keys - most)
			t.Logf("%d 个 IP: 撤离 %s（%d 个 Key），其余空位 %d，"+
				"迁移成功 %d，失败 %d（%s）",
				tc.n, victim, most, spare, len(res.Moved), len(res.Failed), tc.why)

			if tc.wantFail && len(res.Failed) == 0 {
				t.Errorf("%d 个 IP 装 %d 个 Key 已无余量，撤离应有 Key 无处可去",
					tc.n, keys)
			}
			if !tc.wantFail && len(res.Failed) != 0 {
				t.Errorf("其余 IP 有 %d 个空位、需迁移 %d 个，不应失败，"+
					"实际 %d 个无处可去", spare, most, len(res.Failed))
			}
		})
	}
}
