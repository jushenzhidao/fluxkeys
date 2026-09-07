package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// 验证 deploy/README.md 推荐的 32 IP 配置真的能解析出宣称的容量。
// 文档中的配置若无法生效，读者会照抄一份静默失效的配置。
//
// 方案是**按档位分级 max_keys**: hot 10 / warm 50 / cold 100。
// 差异来自各档承担的流量份额（scheduler.pool_shares 默认 70/25/5）——
// cold 档 700 个 Key 合计只拿 5% 流量，单 Key 请求频率仅为 hot 的 1%，
// 故可安全设更高的 max_keys 而不推高实际请求密度。
//
// 各档 IP 数下限由撤离余量决定: n >= keys/max_keys + 1。
// 余出的 IP 全部补给 hot —— 它是唯一的密度瓶颈档。
func TestDocExample_32IP推荐配置容量(t *testing.T) {
	ips := "172.16.0.11|hot|10,172.16.0.12|hot|10,172.16.0.13|hot|10,172.16.0.14|hot|10," +
		"172.16.0.15|hot|10,172.16.0.16|hot|10,172.16.0.17|hot|10,172.16.0.18|hot|10," +
		"172.16.0.19|hot|10,172.16.0.20|hot|10,172.16.0.21|hot|10,172.16.0.22|hot|10," +
		"172.16.0.23|hot|10,172.16.0.24|hot|10,172.16.0.25|hot|10,172.16.0.26|hot|10," +
		"172.16.0.27|hot|10,172.16.0.28|hot|10,172.16.0.29|hot|10,172.16.0.30|warm|50," +
		"172.16.0.31|warm|50,172.16.0.32|warm|50,172.16.0.33|warm|50,172.16.0.34|warm|50," +
		"172.16.0.35|cold|100,172.16.0.36|cold|100,172.16.0.37|cold|100,172.16.0.38|cold|100," +
		"172.16.0.39|cold|100,172.16.0.40|cold|100,172.16.0.41|cold|100,172.16.0.42|cold|100"

	got := parseEgressIPs(ips)
	if len(got) != 32 {
		t.Fatalf("应解析出 32 个 IP，实际 %d", len(got))
	}

	// 各档的 max_keys 必须与该档的流量份额匹配。写错会静默失效:
	// 例如把 cold 也写成 10，容量骤降到 80，700 个 cold Key 装不下。
	wantMaxKeys := map[string]int{"hot": 10, "warm": 50, "cold": 100}
	capacity := map[string]int{}
	count := map[string]int{}
	for _, ip := range got {
		if ip.Pool == "" {
			t.Errorf("IP %s 未解析出档位", ip.Addr)
			continue
		}
		if want := wantMaxKeys[ip.Pool]; ip.MaxKeys != want {
			t.Errorf("IP %s（%s 档）的 max_keys = %d, 期望 %d",
				ip.Addr, ip.Pool, ip.MaxKeys, want)
		}
		capacity[ip.Pool] += ip.MaxKeys
		count[ip.Pool]++
	}

	want := []struct {
		pool     string
		ipCount  int
		capacity int
	}{
		{"hot", 19, 190},
		{"warm", 5, 250},
		{"cold", 8, 800},
	}
	total := 0
	for _, w := range want {
		if count[w.pool] != w.ipCount {
			t.Errorf("%s 档应有 %d 个 IP，实际 %d", w.pool, w.ipCount, count[w.pool])
		}
		if capacity[w.pool] != w.capacity {
			t.Errorf("%s 档容量应为 %d，实际 %d", w.pool, capacity[w.pool], w.capacity)
		}
		total += capacity[w.pool]
	}

	// 1000 个 Key 按 1:2:7 分布 → 100 / 200 / 700。
	// 各档容量必须分别覆盖，光看总量不够 —— 总量够但某档不足时那档会绑定失败。
	const targetKeys = 1000
	needs := map[string]int{
		"hot":  targetKeys / 10,
		"warm": targetKeys * 2 / 10,
		"cold": targetKeys * 7 / 10,
	}
	for pool, need := range needs {
		if capacity[pool] < need {
			t.Errorf("%s 档容量 %d 不足 %d", pool, capacity[pool], need)
		}
		// 撤离余量: 封掉一个出口后，同档其余 IP 要装得下它的 Key。
		// Bind 均摊后单出口负载约 need/count，故要求 (n-1)×max >= need，
		// 等价于 capacity - max_keys >= need。
		if capacity[pool]-wantMaxKeys[pool] < need {
			t.Errorf("%s 档容量 %d 扣掉单个出口的 %d 后不足 %d，"+
				"任一 IP 被封时其上的 Key 无处可去",
				pool, capacity[pool], wantMaxKeys[pool], need)
		}
	}
	if total < targetKeys {
		t.Errorf("总容量 %d 不足 %d 个 Key", total, targetKeys)
	}
	t.Logf("hot=%d(需%d) warm=%d(需%d) cold=%d(需%d) 总容量=%d",
		capacity["hot"], needs["hot"], capacity["warm"], needs["warm"],
		capacity["cold"], needs["cold"], total)
}

// 阈值过低会把「单个 Key 被禁」误判为「出口被封」，必须被配置校验拦住。
func TestValidate_封禁判定阈值下限(t *testing.T) {
	cases := []struct {
		keys    int
		window  time.Duration
		wantErr bool
		why     string
	}{
		{0, 0, false, "0 表示关闭，无需窗口"},
		{1, time.Minute, true, "阈值 1 会让单个坏 Key 撤离整个出口"},
		{2, time.Minute, true, "阈值 2 仍过于激进"},
		{3, time.Minute, false, "3 是允许的最小值"},
		{5, time.Minute, false, "更高阈值更保守"},
		{3, 0, true, "启用判定必须给窗口"},
	}
	for _, c := range cases {
		cfg := Default()
		cfg.Egress.BanDetectKeys = c.keys
		cfg.Egress.BanDetectWindow = c.window
		err := cfg.Validate()
		if c.wantErr && err == nil {
			t.Errorf("keys=%d window=%v 应报错（%s）", c.keys, c.window, c.why)
		}
		if !c.wantErr && err != nil {
			t.Errorf("keys=%d window=%v 不应报错（%s）: %v", c.keys, c.window, c.why, err)
		}
	}
}

// 文档里写的环境变量必须真的能生效。
func TestApplyEnv_封禁判定参数(t *testing.T) {
	t.Setenv("EGRESS_BAN_DETECT_KEYS", "4")
	t.Setenv("EGRESS_BAN_DETECT_WINDOW", "15m")

	cfg := Default()
	applyEnv(cfg)

	if cfg.Egress.BanDetectKeys != 4 {
		t.Errorf("BanDetectKeys = %d, 期望 4", cfg.Egress.BanDetectKeys)
	}
	if cfg.Egress.BanDetectWindow != 15*time.Minute {
		t.Errorf("BanDetectWindow = %v, 期望 15m", cfg.Egress.BanDetectWindow)
	}
}

// 档位名写错不会自然报错，只会让那份额永远抽不到 Key —— 配比静默失效。
// 这类错误必须在启动时拦住。
func TestValidate_档位份额(t *testing.T) {
	cases := []struct {
		name    string
		shares  map[string]float64
		wantErr bool
	}{
		{"未配置", nil, false},
		{"标准三档", map[string]float64{"hot": 70, "warm": 25, "cold": 5}, false},
		{"只配一档", map[string]float64{"hot": 1}, false},
		{"某档为零", map[string]float64{"hot": 70, "cold": 0}, false},
		{"档位名大写", map[string]float64{"Hot": 70}, true},
		{"档位名带空格", map[string]float64{"hot ": 70}, true},
		{"档位名拼错", map[string]float64{"hott": 70}, true},
		{"份额为负", map[string]float64{"hot": -1}, true},
		{"全部为零", map[string]float64{"hot": 0, "cold": 0}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Default()
			cfg.Scheduler.PoolShares = c.shares
			err := cfg.Validate()
			if c.wantErr && err == nil {
				t.Errorf("shares=%v 应报错", c.shares)
			}
			if !c.wantErr && err != nil {
				t.Errorf("shares=%v 不应报错: %v", c.shares, err)
			}
		})
	}
}

// 默认份额必须与 deploy/README.md 的 32 IP 方案匹配，判据是单 IP 请求速率。
//
// 本用例曾断言「份额应正比于 Key 数量比例」，前提是「IP 数正比于 Key 数」。
// 分级 max_keys 方案下该前提不成立: IP 是按**密度瓶颈**分配的，
// hot 档只有 100 个 Key 却配 19 个 IP，正因为它吃 70% 流量。
//
// 现在锁的是端到端的物理量: 份额 ÷ IP 数 × 总 QPS <= 2 req/s。
// 这与 scheduler.TestSelect_份额决定单IP请求密度 用同一判据，
// 后者跑真实调度采样，本用例只查默认配置是否自洽（不启动调度器）。
func TestDefault_默认档位配比与IP方案匹配(t *testing.T) {
	cfg := Default()
	shares := cfg.Scheduler.NormalizedPoolShares()
	if shares == nil {
		t.Fatal("默认应启用档位配比")
	}

	// deploy/README.md 的 32 IP 方案: 各档 IP 数与 max_keys
	ips := map[string]int{"hot": 19, "warm": 5, "cold": 8}
	maxKeys := map[string]int{"hot": 10, "warm": 50, "cold": 100}
	keyCount := map[string]int{"hot": 100, "warm": 200, "cold": 700}
	const realQPS = 20.0

	// 判据 1: 任一档的单 IP 速率不得超过 2 req/s。
	//
	// 这条防的是「IP 数被砍而份额没跟着降」。反方向（抬高份额）它拦不住 ——
	// hot 有 19 个 IP，份额即使到 100% 也只 1.05 req/s；warm 只有 5 个，
	// 是三档中唯一份额超 50% 就会触发的。抬高份额的风险由判据 2、3 覆盖。
	for _, pool := range []string{"hot", "warm", "cold"} {
		ipQPS := realQPS * shares[pool] / float64(ips[pool])
		t.Logf("%-5s 份额=%.0f%% IP=%2d → 单 IP %.2f req/s",
			pool, 100*shares[pool], ips[pool], ipQPS)
		if ipQPS > 2.0 {
			t.Errorf("%s 档单 IP 速率 %.2f req/s 超过上限 2.0。"+
				"改份额时须同步调整 deploy/README.md 的各档 IP 数", pool, ipQPS)
		}
	}

	// 判据 2: cold 档满载时的单 IP 速率不得超过 hot 档。
	//
	// 这是 cold max_keys=100 的依据。若份额改成贴近 Key 数量比例
	// （如 cold 60%），cold 满载会反超 hot 5.7 倍，此时 100 立即失去依据。
	coldPerKey := realQPS * shares["cold"] / float64(keyCount["cold"])
	coldFullIP := coldPerKey * float64(maxKeys["cold"])
	hotIP := realQPS * shares["hot"] / float64(ips["hot"])
	if coldFullIP > hotIP {
		t.Errorf("cold 档满载单 IP %.2f req/s 超过 hot 档 %.2f req/s，"+
			"max_keys=%d 失去依据 —— 应下调 cold 份额或其 max_keys",
			coldFullIP, hotIP, maxKeys["cold"])
	}

	// 判据 3: hot 档单 Key 频率必须显著高于 cold，否则分层没有意义。
	hotPerKey := realQPS * shares["hot"] / float64(keyCount["hot"])
	if ratio := hotPerKey / coldPerKey; ratio < 20 {
		t.Errorf("hot 档单 Key 频率仅为 cold 的 %.1f 倍（期望 >=20），"+
			"频率差不足时 cold 档不应配置远高于 hot 的 max_keys", ratio)
	}

	if !cfg.Scheduler.PoolFallback {
		t.Error("默认应启用跨档回退，否则 hot 档 Key 全部进入非活跃时段时会整体 503")
	}
}

// 份额与该档 IP 数严重失配必须在启动时被拦住。
//
// 配置看起来合理（份额加起来 100%），但某个档位的 IP 根本承担不了分给它的
// 流量，不校验就要等跑起来才暴露。
//
// 注意本用例里 hot 只有 4 个 IP，故默认份额 70% 会被拒 —— 这是刻意的。
// 默认值配套的是 deploy/README.md 的 19 个 hot IP；照抄默认份额却只给
// 4 个 hot IP 的部署，就该在启动时失败而不是带着 12600 req/h 的密度上线。
func TestValidate_份额与档位IP数失配(t *testing.T) {
	// 构造 hot 4 个 / cold 28 个 IP，每 IP 25 个 Key 容量
	buildCfg := func(shares map[string]float64) *Config {
		cfg := Default()
		cfg.Egress.Mode = "multi_ip"
		cfg.Egress.IPs = nil
		for i := 0; i < 4; i++ {
			cfg.Egress.IPs = append(cfg.Egress.IPs,
				EgressIP{Addr: fmt.Sprintf("172.16.0.%d", i+11), MaxKeys: 25, Pool: "hot"})
		}
		for i := 0; i < 8; i++ {
			cfg.Egress.IPs = append(cfg.Egress.IPs,
				EgressIP{Addr: fmt.Sprintf("172.16.1.%d", i+11), MaxKeys: 25, Pool: "warm"})
		}
		for i := 0; i < 28; i++ {
			cfg.Egress.IPs = append(cfg.Egress.IPs,
				EgressIP{Addr: fmt.Sprintf("172.16.2.%d", i+11), MaxKeys: 25, Pool: "cold"})
		}
		cfg.Scheduler.PoolShares = shares
		// 按 deploy/README.md 的实测流量。不设这个值校验会被跳过。
		cfg.Scheduler.ExpectedPeakQPS = 20
		return cfg
	}

	// 20 QPS = 72000 次/小时。每 IP 上限 3000 次/小时。
	// hot 4 个 IP 能承担 12000 次 → 份额上限 12000/72000 ≈ 16.7%
	// cold 28 个 IP 能承担 84000 次 → 份额可达 100%
	cases := []struct {
		name    string
		shares  map[string]float64
		wantErr bool
		why     string
	}{
		{
			name:    "份额贴合各档IP数",
			shares:  map[string]float64{"hot": 10, "warm": 20, "cold": 70},
			wantErr: false,
			why:     "hot 10% = 7200 次 ÷ 4 IP = 1800 次/IP，在上限内",
		},
		{
			name:    "hot档严重超配",
			shares:  map[string]float64{"hot": 70, "warm": 25, "cold": 5},
			wantErr: true,
			why:     "hot 70% = 50400 次 ÷ 4 IP = 12600 次/IP，超标 4 倍",
		},
		{
			// cold 未出现在 map 里 → 归一化后份额为 0 → 跳过，不因「没配」而报错。
			// 与「配了份额却无该档 IP」是两种不同情形，后者单独覆盖。
			// map 里缺 warm → 归一化后 warm 份额为 0，应被跳过而非报错。
			// hot 10 / cold 90 归一化后 hot 10%（7200÷4=1800，在 3000 内）、
			// cold 90%（64800÷28=2314，在 3000 内），两档都合法。
			name:    "档位缺失视为份额零",
			shares:  map[string]float64{"hot": 10, "cold": 90},
			wantErr: false,
			why:     "未在 map 里出现的档位份额为 0，不应因「没配」而报错",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := buildCfg(c.shares).Validate()
			if c.wantErr && err == nil {
				t.Errorf("应报错（%s）", c.why)
			}
			if !c.wantErr && err != nil {
				t.Errorf("不应报错（%s）: %v", c.why, err)
			}
		})
	}

	// 未预估流量时必须跳过检查。同一份「超配」配置在 ExpectedPeakQPS=0 下应通过 ——
	// 不知道自己流量的部署不该被一个凭空假设的数字挡在启动之外。
	t.Run("未预估流量时跳过", func(t *testing.T) {
		cfg := buildCfg(map[string]float64{"hot": 70, "warm": 25, "cold": 5})
		cfg.Scheduler.ExpectedPeakQPS = 0
		if err := cfg.Validate(); err != nil {
			t.Errorf("未设 expected_peak_qps 时不应校验份额容量: %v", err)
		}
	})

	// 某档配了份额但完全没有该档 IP —— 这些流量会全部走回退或直接失败，
	// 与「份额为 0」是两回事，必须拦住。
	t.Run("配了份额却无该档IP", func(t *testing.T) {
		cfg := Default()
		cfg.Egress.Mode = "multi_ip"
		cfg.Egress.IPs = []EgressIP{
			{Addr: "172.16.0.11", MaxKeys: 25, Pool: "hot"},
			{Addr: "172.16.0.12", MaxKeys: 25, Pool: "hot"},
		}
		cfg.Scheduler.ExpectedPeakQPS = 1 // 极低，确保不因密度超标而报错
		cfg.Scheduler.PoolShares = map[string]float64{"hot": 50, "cold": 50}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("cold 档有份额但无 IP，应报错")
		}
		if !strings.Contains(err.Error(), "cold") {
			t.Errorf("错误未指明是哪个档位: %v", err)
		}
	})
}

// 未使用出口分层（IP 都没标档位）时不做份额与 IP 数的匹配检查 ——
// 此时所有 IP 对所有档位等价可用，份额只影响 Key 选取偏好。
func TestValidate_未分层时跳过份额容量检查(t *testing.T) {
	cfg := Default()
	cfg.Egress.Mode = "multi_ip"
	cfg.Egress.IPs = []EgressIP{{Addr: "172.16.0.2", MaxKeys: 8}} // 无 Pool
	cfg.Scheduler.PoolShares = map[string]float64{"hot": 70, "warm": 25, "cold": 5}

	if err := cfg.Validate(); err != nil {
		t.Errorf("未分层的部署不应因份额被拦: %v", err)
	}
}

// ---------- 封禁自动恢复的配置 ----------

// 文档里写的环境变量必须真的能生效，且必须能显式关闭。
//
// 关闭这条尤其重要: 默认值是 2h（已启用），若 EGRESS_BAN_COOLDOWN=0
// 被静默忽略，运维以为关掉了自动恢复、实际仍在恢复 —— 这类「配了没生效」
// 的偏差在出口策略上代价很高。
func TestApplyEnv_封禁恢复参数(t *testing.T) {
	t.Run("覆盖默认值", func(t *testing.T) {
		t.Setenv("EGRESS_BAN_COOLDOWN", "30m")
		t.Setenv("EGRESS_BAN_COOLDOWN_MAX", "12h")

		cfg := Default()
		applyEnv(cfg)

		if cfg.Egress.BanCooldown != 30*time.Minute {
			t.Errorf("BanCooldown = %v, 期望 30m", cfg.Egress.BanCooldown)
		}
		if cfg.Egress.BanCooldownMax != 12*time.Hour {
			t.Errorf("BanCooldownMax = %v, 期望 12h", cfg.Egress.BanCooldownMax)
		}
	})

	t.Run("显式设零关闭恢复", func(t *testing.T) {
		t.Setenv("EGRESS_BAN_COOLDOWN", "0")

		cfg := Default()
		if cfg.Egress.BanCooldown == 0 {
			t.Fatal("前置条件: 默认值应为非零，否则测不出「关闭」的效果")
		}
		applyEnv(cfg)

		if cfg.Egress.BanCooldown != 0 {
			t.Errorf("BanCooldown = %v, 显式设 0 应关闭自动恢复", cfg.Egress.BanCooldown)
		}
	})

	t.Run("非法值保持默认", func(t *testing.T) {
		t.Setenv("EGRESS_BAN_COOLDOWN", "两小时")

		cfg := Default()
		want := cfg.Egress.BanCooldown
		applyEnv(cfg)

		// 笔误不该被当成「关闭」——那会让出口永不恢复，是静默的容量泄漏
		if cfg.Egress.BanCooldown != want {
			t.Errorf("非法值应保持默认 %v，实际 %v", want, cfg.Egress.BanCooldown)
		}
	})
}

// max < base 会让指数退避静默失效，必须在启动时拦住。
func TestValidate_封禁冷却上限(t *testing.T) {
	cases := []struct {
		name    string
		base    time.Duration
		max     time.Duration
		wantErr bool
		why     string
	}{
		{"默认值", 2 * time.Hour, 24 * time.Hour, false, "max 远大于 base"},
		{"相等", time.Hour, time.Hour, false, "退避无空间但不算错配"},
		{"max小于base", 4 * time.Hour, time.Hour, true, "退避会被截断成恒等于 max"},
		{"关闭恢复时不校验", 0, time.Nanosecond, false, "base 为 0 表示不恢复，max 无意义"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Default()
			cfg.Egress.BanCooldown = c.base
			cfg.Egress.BanCooldownMax = c.max
			err := cfg.Validate()
			if c.wantErr && err == nil {
				t.Errorf("base=%v max=%v 应报错（%s）", c.base, c.max, c.why)
			}
			if !c.wantErr && err != nil {
				t.Errorf("base=%v max=%v 不应报错（%s）: %v", c.base, c.max, c.why, err)
			}
		})
	}
}

// 默认必须启用自动恢复。
//
// 关闭时被封出口永久退出服务，而每档的空位是为「撤离」准备的，
// 不是为「永久报废」准备的 —— 32 个 IP 逐个损耗会耗尽备用余量。
func TestDefault_默认启用封禁恢复(t *testing.T) {
	cfg := Default()
	if cfg.Egress.BanCooldown <= 0 {
		t.Error("默认应启用封禁自动恢复，否则被封出口永久报废")
	}
	if cfg.Egress.BanCooldownMax < cfg.Egress.BanCooldown {
		t.Errorf("默认 max（%v）不应小于 base（%v）",
			cfg.Egress.BanCooldownMax, cfg.Egress.BanCooldown)
	}
}

// deploy/.env.example 里写的推理预扣旋钮必须真的能生效，否则运维改了没反应。
func TestApplyEnv_推理预扣参数(t *testing.T) {
	t.Setenv("QUOTA_REASONING_OUTPUT_MULTIPLIER", "4.5")
	t.Setenv("QUOTA_REASONING_FLOOR_TOKENS", "2048")

	cfg := Default()
	applyEnv(cfg)

	if cfg.Quota.ReasoningOutputMultiplier != 4.5 {
		t.Errorf("ReasoningOutputMultiplier = %v, 期望 4.5",
			cfg.Quota.ReasoningOutputMultiplier)
	}
	if cfg.Quota.ReasoningFloorTokens != 2048 {
		t.Errorf("ReasoningFloorTokens = %d, 期望 2048",
			cfg.Quota.ReasoningFloorTokens)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("环境变量覆盖后配置应仍合法: %v", err)
	}
}

// 非法值必须被拦住：系数小于 1 会让推理模型预扣低于常规模型，
// 与该系数的设计意图完全相反，属于静默失效。
func TestValidate_推理预扣参数下限(t *testing.T) {
	cases := []struct {
		multiplier float64
		floor      int64
		wantErr    bool
		why        string
	}{
		{3.0, 1024, false, "默认值"},
		{1.0, 0, false, "1.0 等价于不额外放大，是允许的下限"},
		{0.5, 1024, true, "小于 1 会缩小预扣，与设计意图相反"},
		{0, 1024, true, "0 会让输出部分预扣归零"},
		{3.0, -1, true, "负下限无意义"},
	}
	for _, c := range cases {
		cfg := Default()
		cfg.Quota.ReasoningOutputMultiplier = c.multiplier
		cfg.Quota.ReasoningFloorTokens = c.floor
		err := cfg.Validate()
		if c.wantErr && err == nil {
			t.Errorf("multiplier=%v floor=%d 应报错（%s）",
				c.multiplier, c.floor, c.why)
		}
		if !c.wantErr && err != nil {
			t.Errorf("multiplier=%v floor=%d 不应报错（%s）: %v",
				c.multiplier, c.floor, c.why, err)
		}
	}
}

// 环境变量传入垃圾值时应保持默认，而非污染成 0 导致预扣归零。
func TestApplyEnv_推理预扣非法值保持默认(t *testing.T) {
	t.Setenv("QUOTA_REASONING_OUTPUT_MULTIPLIER", "abc")
	t.Setenv("QUOTA_REASONING_FLOOR_TOKENS", "not-a-number")

	def := Default()
	cfg := Default()
	applyEnv(cfg)

	if cfg.Quota.ReasoningOutputMultiplier != def.Quota.ReasoningOutputMultiplier {
		t.Errorf("非法值不应改动默认系数，得到 %v",
			cfg.Quota.ReasoningOutputMultiplier)
	}
	if cfg.Quota.ReasoningFloorTokens != def.Quota.ReasoningFloorTokens {
		t.Errorf("非法值不应改动默认下限，得到 %d",
			cfg.Quota.ReasoningFloorTokens)
	}
}
