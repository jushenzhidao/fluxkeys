// Package config 定义 FluxKeys 网关的全部可配置项。
//
// 配置来源优先级: 环境变量 > YAML 文件 > 内置默认值。
package config

import (
	"time"
)

// 本文件是内置默认配置的唯一来源。

// Default 返回内置默认配置。
func Default() *Config {
	return &Config{
		Server: Server{
			Addr:            ":8080",
			ReadTimeout:     60 * time.Second,
			WriteTimeout:    0, // 流式响应不设写超时
			IdleTimeout:     120 * time.Second,
			ShutdownTimeout: 30 * time.Second,
			MaxBodyBytes:    10 << 20,
			MetricsAddr:     ":9090",
		},
		Redis: Redis{Addr: "127.0.0.1:6379", DB: 0, PoolSize: 32},
		Postgres: Postgres{
			DSN:         "postgres://fluxkeys:fluxkeys@127.0.0.1:5432/fluxkeys?sslmode=disable",
			MaxConns:    16,
			MinConns:    2,
			AutoMigrate: true,
		},
		Quota: Quota{
			TokenLimit:     5_000_000,
			TokenHardRatio: 0.90, // 450 万，留 50 万缓冲
			TokenSoftRatio: 0.80, // 400 万起降权
			CountLimit:     100,
			CountHardRatio: 0.90, // 90 次
			CountSoftRatio: 0.80, // 80 次
			LeaseTTL:       120 * time.Second,
			StreamLeaseTTL: 600 * time.Second,
			// 回收间隔需明显短于租约 TTL，确保泄漏能被及时释放
			ReapInterval:       30 * time.Second,
			ReapBatch:          500,
			ReconcileInterval:  time.Hour,
			DefaultMaxTokens:   4096,
			EstimateMultiplier: 1.2,
			// 推理模型思维链不受 max_tokens 约束，实测最高 8.8 倍上限，
			// 需独立的放大系数与绝对下限，详见字段注释。
			ReasoningOutputMultiplier: 3.0,
			ReasoningFloorTokens:      1024,
			SnapshotInterval:          time.Second,
		},
		Egress: Egress{
			Mode:           "direct",
			RequestTimeout: 300 * time.Second,
			VerifyOnStart:  true,
			// 刻意留空: 非空默认值会让 EgressVerifyTarget 永远命中
			// 「显式配置」分支，从 provider base_url 推导的逻辑就成了死代码，
			// 自检也就永远在拨一个与真实上游无关的域名。
			VerifyTarget: "",
			// 默认关闭自动封禁判定。误判的代价（健康出口上全部 Key 被迫换 IP）
			// 高于漏判（运维观察后手动处理），故要求显式开启。
			BanDetectKeys:   0,
			BanDetectWindow: 10 * time.Minute,
			// 自动恢复默认开启。与自动判定不同，这里的误判方向是安全的:
			// 恢复只把出口转入 cooldown，不会立刻绑定 Key，探测失败会退回。
			// 而不恢复的代价是确定的 —— 出口只减不增，直至备用余量耗尽。
			BanCooldown:         2 * time.Hour,
			BanCooldownMax:      24 * time.Hour,
			HealthCheckInterval: 30 * time.Second,
		},
		Upstream: UpstreamCommon{
			MaxRetries:     3,
			RetryBaseDelay: 200 * time.Millisecond,
			RetryJitter:    500 * time.Millisecond,
		},
		Providers: map[string]Provider{
			"volc": {
				BaseURL:      "https://ark.cn-beijing.volces.com",
				ModelMapping: map[string]string{},
				CountModels:  []string{"seedream", "seedream-3.0"},
				// 子串匹配，覆盖带版本后缀的实际模型名
				ReasoningModels: []string{"deepseek", "doubao-1-5-thinking", "thinking", "-r1"},
				QuotaKind:       "token",
				// 刻意不预设 QuotaLimit，留 0 让它回退到全局 quota.token_limit。
				//
				// 若在此填一个默认额度，运维调 quota.token_limit 会「配了没反应」——
				// provider 默认值总是胜出，而日志里看不出任何异常。provider 的
				// quota_limit 只应在用户明确要为该上游覆盖额度时才出现。
				RefreshHour: intPtr(12),
			},
		},
		Scheduler: Scheduler{
			WeightQuota:        35,
			WeightHistory:      25,
			WeightPersona:      20,
			WeightHealth:       15,
			SoftPenalty:        20,
			ActivePoolSize:     100,
			EnablePersona:      true,
			MinRequestInterval: 5 * time.Second,
			// 默认按 Key 数量的倒数配比: hot 占 10% 的 Key 却承接 70% 的流量。
			//
			// 这正是分层的意义 —— 让绝大多数请求集中在少数低密度出口上，
			// cold 档的大量 Key 只作为「储备」存在，被选中的频率极低，
			// 从而支撑更高的 max_keys 而不推高瞬时并发。
			// 接近各档 Key 数量比例（10/20/70），而非激进的 70/25/5 ——
			// 后者会让 hot 档的 req/h 每 IP 超标 4 倍，见 PoolShares 注释。
			PoolShares:   map[string]float64{"hot": 70, "warm": 25, "cold": 5},
			PoolFallback: true,
		},
		Refresh: Refresh{
			Enabled:                true,
			PreRefreshStart:        "11:30",
			WindowStart:            "12:00",
			WindowEnd:              "14:00",
			ProbeInterval:          5 * time.Minute,
			ProbeModel:             "deepseek-v3",
			PostRefreshRampMinutes: 30,
		},
		Fallback: Fallback{Enabled: false, DailyBudgetCents: 0},
		Admin:    Admin{},
	}
}
