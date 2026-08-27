package persona

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestFor_同一KeyID画像稳定(t *testing.T) {
	// 无状态与可复现是这个包的核心前提: 进程重启后画像必须一致，
	// 否则 Key 的"人设"会在每次部署时突变。
	for _, id := range []string{"volc_001", "volc_999", "abc"} {
		a, b := For(id), For(id)
		if a.ID != b.ID || a.Chronotype != b.Chronotype || a.Rhythm != b.Rhythm {
			t.Fatalf("%s 画像不稳定: %+v vs %+v", id, a, b)
		}
		if a.Loyalty != b.Loyalty || a.JitterRatio != b.JitterRatio {
			t.Fatalf("%s 数值维度不稳定", id)
		}
		if len(a.PreferredModels) != len(b.PreferredModels) {
			t.Fatalf("%s 模型偏好长度不一致", id)
		}
		for i := range a.PreferredModels {
			if a.PreferredModels[i] != b.PreferredModels[i] {
				t.Fatalf("%s 模型偏好顺序不稳定", id)
			}
		}
	}
}

func TestFor_千个Key的画像分布不集中(t *testing.T) {
	// 反封禁的实际要求: 1000 个 Key 不能挤在同一个作息/节奏上。
	const n = 1000
	chrono := map[Chronotype]int{}
	rhythm := map[Rhythm]int{}
	ids := map[string]int{}
	firstModel := map[string]int{}

	for i := 0; i < n; i++ {
		p := For(fmt.Sprintf("volc_%04d", i))
		chrono[p.Chronotype]++
		rhythm[p.Rhythm]++
		ids[p.ID]++
		firstModel[p.PreferredModels[0]]++
	}

	if len(chrono) < 3 {
		t.Fatalf("作息类型过于集中: %v", chrono)
	}
	if len(rhythm) != 3 {
		t.Fatalf("节奏类型应覆盖全部 3 种: %v", rhythm)
	}
	// 任一节奏占比不应超过 60%
	for r, c := range rhythm {
		if float64(c)/n > 0.6 {
			t.Fatalf("节奏 %s 占比过高 %.2f", r, float64(c)/n)
		}
	}
	if len(firstModel) < 3 {
		t.Fatalf("首选模型过于集中: %v", firstModel)
	}
	// 画像 ID 的重复度: 相同 ID 的 Key 数量不应过高，否则日志里无法区分个体
	maxDup := 0
	for _, c := range ids {
		if c > maxDup {
			maxDup = c
		}
	}
	if maxDup > n/10 {
		t.Fatalf("画像 ID 重复过多，最大重复 %d 次", maxDup)
	}
	t.Logf("作息分布 %v / 节奏分布 %v / 不同画像 ID %d 个", chrono, rhythm, len(ids))
}

func TestFor_维度之间不强相关(t *testing.T) {
	// 各维度用不同 salt 的目的: 若"夜猫子必定稀疏"，风控只需看时段就能推出节奏，
	// 三个维度实际只提供一个维度的信息量。
	const n = 600
	cross := map[Chronotype]map[Rhythm]int{}
	for i := 0; i < n; i++ {
		p := For(fmt.Sprintf("k_%d", i))
		if cross[p.Chronotype] == nil {
			cross[p.Chronotype] = map[Rhythm]int{}
		}
		cross[p.Chronotype][p.Rhythm]++
	}
	for c, m := range cross {
		if len(m) < 2 {
			t.Fatalf("作息 %s 下节奏只有 %d 种，两个维度强相关", c, len(m))
		}
	}
}

func TestActiveWindow_Contains(t *testing.T) {
	cases := []struct {
		name string
		w    ActiveWindow
		hour int
		want bool
	}{
		{"普通时段内", ActiveWindow{9, 18}, 12, true},
		{"普通时段起点", ActiveWindow{9, 18}, 9, true},
		{"普通时段终点开区间", ActiveWindow{9, 18}, 18, false},
		{"普通时段外", ActiveWindow{9, 18}, 3, false},
		{"跨零点_夜间", ActiveWindow{22, 4}, 23, true},
		{"跨零点_凌晨", ActiveWindow{22, 4}, 2, true},
		{"跨零点_边界", ActiveWindow{22, 4}, 4, false},
		{"跨零点_白天", ActiveWindow{22, 4}, 12, false},
		{"退化全天", ActiveWindow{0, 0}, 7, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.w.Contains(c.hour); got != c.want {
				t.Fatalf("Contains(%d) = %v, 期望 %v", c.hour, got, c.want)
			}
		})
	}
}

func TestWindowTemplates_时段足够窄以压低同IP并发(t *testing.T) {
	// 这是出口 IP 数量的直接决定因素，不是风格偏好，故用测试锁死。
	//
	// 风控观测到的是「同一 IP 在同一时刻有多少 Key 在活动」，而非绑定总数。
	// 模板越宽，同 IP 的 Key 活跃时段重叠越多，单 IP 能安全承载的 Key 越少，
	// 所需 IP 数越多。曾经的 9 个宽模板日均活跃 9.6 小时，是 MaxKeys 只能
	// 取 10 的根因。任何把模板改宽的改动都会推高 IP 成本，必须在此暴露。
	if len(windowTemplates) < 48 {
		t.Fatalf("模板数 %d 过少，同起点的 Key 会拿到完全相同的时段向量", len(windowTemplates))
	}

	totalHours := 0
	for i, tpl := range windowTemplates {
		hours := 0
		for h := 0; h < 24; h++ {
			for _, w := range tpl.windows {
				if w.Contains(h) {
					hours++
					break
				}
			}
		}
		if hours == 0 {
			t.Fatalf("模板 %d 没有任何活跃小时，该 Key 永远不会被调度", i)
		}
		// 单模板不得覆盖超过 8 小时: 24 小时全覆盖等于没有作息
		if hours > 8 {
			t.Errorf("模板 %d 覆盖 %d 小时过宽: %+v", i, hours, tpl.windows)
		}
		totalHours += hours
	}

	avg := float64(totalHours) / float64(len(windowTemplates))
	if avg > 5.0 {
		t.Fatalf("日均活跃 %.1f 小时超上限 5.0，单 IP 并发密度会显著升高", avg)
	}
	t.Logf("模板数 %d，日均活跃 %.1f 小时", len(windowTemplates), avg)
}

func TestFor_同一时刻的时段向量高度分散(t *testing.T) {
	// 对应 dashboard 的行为相似度自检（时段余弦 > 0.6 告警、> 0.8 紧急拆分）。
	// 若大量 Key 的时段向量完全相同，它们在风控侧就是同一批脚本的分身。
	const n = 200
	vecs := make([][24]bool, n)
	for i := 0; i < n; i++ {
		p := For(fmt.Sprintf("volc_%04d", i))
		for h := 0; h < 24; h++ {
			vecs[i][h] = p.IsActiveAt(time.Date(2026, 8, 23, h, 30, 0, 0, time.Local))
		}
	}

	identical, pairs := 0, 0
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			pairs++
			if vecs[i] == vecs[j] {
				identical++
			}
		}
	}
	ratio := float64(identical) / float64(pairs)
	// 宽模板时代该比例约 8.7%，窄化后应降到 2% 以下
	if ratio > 0.02 {
		t.Fatalf("时段向量完全相同的 Key 对占比 %.1f%% 过高", ratio*100)
	}
	t.Logf("完全相同的 Key 对: %d/%d (%.2f%%)", identical, pairs, ratio*100)
}

func TestIsActiveAt_各时段都有可用Key(t *testing.T) {
	// 关键可用性约束: 若某个小时全部 Key 都不活跃，调度器会在那个小时
	// 因"无候选"而全量拒绝请求 —— 画像不能把服务打死。
	const n = 200
	personas := make([]*Persona, n)
	for i := range personas {
		personas[i] = For(fmt.Sprintf("volc_%04d", i))
	}

	for h := 0; h < 24; h++ {
		ts := time.Date(2026, 8, 23, h, 30, 0, 0, time.Local)
		active := 0
		for _, p := range personas {
			if p.IsActiveAt(ts) {
				active++
			}
		}
		if active == 0 {
			t.Fatalf("%02d 时没有任何活跃 Key，该时段服务将完全不可用", h)
		}
		// 凌晨低谷可以少，但不能只剩个别 Key
		if active < 5 {
			t.Fatalf("%02d 时仅 %d 个活跃 Key，容量不足", h, active)
		}
		t.Logf("%02d 时活跃 Key: %d/%d", h, active, n)
	}
}

func TestIsActiveAt_夜猫子白天不活跃(t *testing.T) {
	p := &Persona{Chronotype: ChronoNightOwl, Windows: []ActiveWindow{{22, 4}}}
	day := time.Date(2026, 8, 23, 10, 0, 0, 0, time.Local)
	night := time.Date(2026, 8, 23, 23, 0, 0, 0, time.Local)

	if p.IsActiveAt(day) {
		t.Fatal("夜猫子在 10 点应不活跃")
	}
	if !p.IsActiveAt(night) {
		t.Fatal("夜猫子在 23 点应活跃")
	}
	// nil 画像视为始终活跃，便于关闭 persona 特性时退化
	var nilP *Persona
	if !nilP.IsActiveAt(day) {
		t.Fatal("nil 画像应视为始终活跃")
	}
}

func TestModelAffinity_按位次衰减且池外有底分(t *testing.T) {
	p := For("volc_001")
	first := p.ModelAffinity(p.PreferredModels[0])
	second := p.ModelAffinity(p.PreferredModels[1])
	third := p.ModelAffinity(p.PreferredModels[2])

	if !(first > second && second > third) {
		t.Fatalf("偏好度未按位次衰减: %v %v %v", first, second, third)
	}
	if first != p.Loyalty {
		t.Fatalf("首选模型偏好度应等于忠诚度: %v vs %v", first, p.Loyalty)
	}

	// 未知模型必须有非零底分，否则小众模型的请求会无 Key 可用
	other := p.ModelAffinity("some-unknown-model")
	if other <= 0 {
		t.Fatalf("池外模型偏好度应为正数底分，实际 %v", other)
	}
	if other >= first {
		t.Fatalf("池外模型偏好度不应高于首选: %v vs %v", other, first)
	}

	// 空模型名与 nil 画像不参与过滤
	if p.ModelAffinity("") != 1 {
		t.Fatal("空模型名应返回 1（不做偏好过滤）")
	}
	var nilP *Persona
	if nilP.ModelAffinity("x") != 1 {
		t.Fatal("nil 画像应返回 1")
	}
}

func TestLoyalty与JitterRatio在合理区间(t *testing.T) {
	for i := 0; i < 500; i++ {
		p := For(fmt.Sprintf("volc_%04d", i))
		if p.Loyalty < 0.55 || p.Loyalty > 0.95 {
			t.Fatalf("%s 忠诚度越界: %v", p.KeyID, p.Loyalty)
		}
		if p.JitterRatio < 0.10 || p.JitterRatio > 0.60 {
			t.Fatalf("%s 抖动越界: %v", p.KeyID, p.JitterRatio)
		}
		if p.DailyRequestBudget <= 0 {
			t.Fatalf("%s 日预算非正: %d", p.KeyID, p.DailyRequestBudget)
		}
		if len(p.PreferredModels) != 3 {
			t.Fatalf("%s 偏好模型数应为 3，实际 %d", p.KeyID, len(p.PreferredModels))
		}
		seen := map[string]bool{}
		for _, m := range p.PreferredModels {
			if seen[m] {
				t.Fatalf("%s 偏好模型重复: %v", p.KeyID, p.PreferredModels)
			}
			seen[m] = true
		}
	}
}

func TestPaceFactor_按节奏区分(t *testing.T) {
	bursty := &Persona{Rhythm: RhythmBursty}
	steady := &Persona{Rhythm: RhythmSteady}
	sparse := &Persona{Rhythm: RhythmSparse}

	if !(bursty.PaceFactor() < steady.PaceFactor() && steady.PaceFactor() < sparse.PaceFactor()) {
		t.Fatalf("节奏因子未区分: %v %v %v",
			bursty.PaceFactor(), steady.PaceFactor(), sparse.PaceFactor())
	}
	var nilP *Persona
	if nilP.PaceFactor() != 1 {
		t.Fatal("nil 画像节奏因子应为 1")
	}
}

func TestStaggerOffset_确定性且均匀分布(t *testing.T) {
	const span = time.Hour
	day := "20260823"

	// 确定性
	a := StaggerOffset("volc_001", day, span)
	b := StaggerOffset("volc_001", day, span)
	if a != b {
		t.Fatalf("同 Key 同日偏移不一致: %v vs %v", a, b)
	}
	// 换日应该变化，避免每天都是同一批 Key 抢在最前面
	if StaggerOffset("volc_001", "20260824", span) == a {
		t.Log("提示: 换日偏移相同（哈希碰撞，可接受）")
	}

	// 分布检查: 1000 个 Key 在 1 小时内应铺开到各个 6 分钟桶
	buckets := make([]int, 10)
	for i := 0; i < 1000; i++ {
		off := StaggerOffset(fmt.Sprintf("volc_%04d", i), day, span)
		if off < 0 || off >= span {
			t.Fatalf("偏移越界: %v", off)
		}
		buckets[int(off/(span/10))]++
	}
	for i, c := range buckets {
		if c == 0 {
			t.Fatalf("第 %d 个桶为空，错峰不均匀: %v", i, buckets)
		}
	}
	t.Logf("错峰分布: %v", buckets)

	if StaggerOffset("volc_001", day, 0) != 0 {
		t.Fatal("span<=0 应返回 0")
	}
}

func TestFor_并发安全(t *testing.T) {
	// For 是纯函数，必须能在请求热路径上无锁并发调用
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				id := fmt.Sprintf("volc_%04d", i)
				p := For(id)
				if p.KeyID != id {
					t.Errorf("KeyID 不符: %q", p.KeyID)
					return
				}
				_ = p.IsActiveAt(time.Now())
				_ = p.ModelAffinity("deepseek-v3")
			}
		}(g)
	}
	wg.Wait()
}
