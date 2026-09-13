// Package config 定义 FluxKeys 网关的全部可配置项。
//
// 配置来源优先级: 环境变量 > YAML 文件 > 内置默认值。
package config

import (
	"time"
)

// 本文件是全部配置子结构体的唯一定义处。
// 纯数据声明，不含逻辑 —— 校验在 validate.go，默认值在 defaults.go。

// Server 是 HTTP 服务配置。
type Server struct {
	Addr            string        `yaml:"addr"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
	IdleTimeout     time.Duration `yaml:"idle_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	MaxBodyBytes    int64         `yaml:"max_body_bytes"`
	MetricsAddr     string        `yaml:"metrics_addr"`

	// ShardID 是本实例的机器分片标识（多机部署）。
	//
	// 非空时调度器只装载 shard 等于该值的 Key（迁移期兼容: 首次启动还会
	// 带上 shard 为空的存量 Key，见 store.UpstreamKeyFilter.IncludeUnsharded）。
	// 空值 = 单机模式，全量装载，与既有行为完全一致。
	//
	// 属冷配置: 改机器归属涉及出口 IP 在云厂商侧的重绑定，本就要重启进程。
	ShardID string `yaml:"shard_id"`
}

// Redis 是热状态存储配置。
type Redis struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
	PoolSize int    `yaml:"pool_size"`
}

// Postgres 是持久化存储配置。
type Postgres struct {
	DSN         string `yaml:"dsn"`
	MaxConns    int32  `yaml:"max_conns"`
	MinConns    int32  `yaml:"min_conns"`
	AutoMigrate bool   `yaml:"auto_migrate"`
}

// Quota 是配额水位与租约配置。
type Quota struct {
	// TokenLimit 是单 Key 每日 Token 额度上限（火山赠送 500 万）。
	TokenLimit int64 `yaml:"token_limit"`
	// TokenHardRatio 硬水位占上限的比例，超过即拒绝调度。
	TokenHardRatio float64 `yaml:"token_hard_ratio"`
	// TokenSoftRatio 软水位占上限的比例，超过则降权。
	TokenSoftRatio float64 `yaml:"token_soft_ratio"`

	// CountLimit 是单 Key 每日次数额度上限（如 Seedream 100 次）。
	CountLimit     int64   `yaml:"count_limit"`
	CountHardRatio float64 `yaml:"count_hard_ratio"`
	CountSoftRatio float64 `yaml:"count_soft_ratio"`

	// LeaseTTL 是普通请求的租约有效期。
	LeaseTTL time.Duration `yaml:"lease_ttl"`
	// StreamLeaseTTL 是流式请求的租约有效期（需覆盖长连接场景）。
	StreamLeaseTTL time.Duration `yaml:"stream_lease_ttl"`
	// ReapInterval 是过期租约回收的执行间隔。
	ReapInterval time.Duration `yaml:"reap_interval"`
	// ReapBatch 是单次回收的最大租约数。
	ReapBatch int `yaml:"reap_batch"`
	// ReconcileInterval 是配额对账的执行间隔。
	ReconcileInterval time.Duration `yaml:"reconcile_interval"`
	// DefaultMaxTokens 是请求未指定 max_tokens 时的预扣基准。
	DefaultMaxTokens int64 `yaml:"default_max_tokens"`
	// EstimateMultiplier 是预扣估算的放大系数。
	EstimateMultiplier float64 `yaml:"estimate_multiplier"`
	// ReasoningOutputMultiplier 是推理模型输出部分的额外放大系数。
	//
	// 为什么必须单列: 推理模型的 reasoning_content 不受 max_tokens 约束。
	// 实测 deepseek-v4-flash（max_tokens=64）实际 completion_tokens 达 121，
	// 为上限的 1.89 倍；max_tokens=16 时更极端，达 8.8 倍。仅靠通用的
	// EstimateMultiplier（1.2）远不足以覆盖，预扣会被系统性击穿 —— 真实
	// 用量越过硬水位后 Commit 才发现，额度已经超刷。
	//
	// 取 3.0: 覆盖实测 1.89 倍并留出余量。不取更大值是因为预扣过高会让
	// 单 Key 可并发请求数下降，而 max_tokens 极小的请求本身占用绝对值很低，
	// 由 ReasoningFloorTokens 兜底更划算。
	ReasoningOutputMultiplier float64 `yaml:"reasoning_output_multiplier"`
	// ReasoningFloorTokens 是推理模型输出部分的预扣下限。
	//
	// 小 max_tokens 场景下按比例放大仍然不够（16 × 3 = 48，实测 141），
	// 因为思维链长度取决于问题复杂度而非用户声明的上限。此处设一个绝对
	// 下限，把这类请求托到安全线以上。
	ReasoningFloorTokens int64 `yaml:"reasoning_floor_tokens"`
	// SnapshotInterval 是调度打分用的只读快照刷新间隔。
	SnapshotInterval time.Duration `yaml:"snapshot_interval"`
}

// Egress 是出口 IP 配置。
type Egress struct {
	// Mode 取 direct（单出口，本地/CI）或 multi_ip（生产多 EIP）。
	Mode string `yaml:"mode"`
	// IPs 是出口 IP 列表，仅 multi_ip 模式使用。
	IPs []EgressIP `yaml:"ips"`
	// RequestTimeout 是上游请求的整体超时。
	RequestTimeout time.Duration `yaml:"request_timeout"`
	// VerifyOnStart 启动时校验每个出口 IP 的连通性。
	VerifyOnStart bool `yaml:"verify_on_start"`
	// VerifyTarget 是连通性校验的目标 host:port。
	VerifyTarget string `yaml:"verify_target"`

	// BanDetectKeys 是判定「出口被上游拉黑」所需的不同 Key 数。
	//
	// 0 表示关闭自动判定（默认），此时出口级封禁只能靠运维观察后手动处理。
	//
	// 为什么以「不同 Key 数」为判据而非失败次数: 上游返回 401/403 时无法区分
	// 「这个 Key 被封」与「这个 IP 被封」。单个 Key 反复失败通常是它自己被禁用；
	// 多个互不相干的 Key 从同一出口相继失败才指向出口被拉黑。
	//
	// 取值不宜过小: 阈值为 2 时，两个恰好同时被上游禁用的 Key 就会让一个健康
	// 出口上的全部 Key 被迫迁移 —— 一次性制造大批「换了出口的老账号」，
	// 比不做自动判定更糟。建议 3 起步，且该档位 Key 数应显著大于阈值。
	BanDetectKeys int `yaml:"ban_detect_keys"`

	// BanDetectWindow 是上述判据的观察窗口。
	//
	// 过长会把跨越数小时的、彼此无关的零星失败累积成误判；
	// 过短则真实封禁可能因为 Key 作息错开而凑不满阈值 ——
	// 画像窄化后同一出口在任一时刻只有少数 Key 活跃，这一点尤其要注意。
	BanDetectWindow time.Duration `yaml:"ban_detect_window"`

	// BanCooldown 是被判定封禁的出口尝试恢复前的等待时长，0 表示永不恢复。
	//
	// 上游封禁通常是临时的（几小时到几天），而 banned 在状态机里是终态:
	// MarkSuccess 只处理 suspect / cooldown，Assignable 又要求 active，
	// 于是被封的出口永久退出服务。32 个 IP 逐个损耗且不可回收，
	// 最终会耗尽备用余量 —— 每个档位的空位本是为「撤离」准备的，
	// 不是为「永久报废」准备的。
	//
	// 恢复不是直接转 active，而是先转 cooldown 让健康探测有机会介入，
	// 探测成功才回 active。cooldown 不满足 Assignable，所以不会有一批
	// Key 立刻涌回一个可能仍被封的出口。
	//
	// 反复被封的出口按 2 的幂次延长等待（上限 BanCooldownMax）——
	// 它在上游眼里已经脏了，按初始时长反复放行只会重复受损。
	BanCooldown time.Duration `yaml:"ban_cooldown"`

	// BanCooldownMax 是指数退避的上限，避免多次被封后等待时长无限增长。
	BanCooldownMax time.Duration `yaml:"ban_cooldown_max"`

	// HealthCheckInterval 是 IP 健康探测间隔。
	HealthCheckInterval time.Duration `yaml:"health_check_interval"`
}

// EgressIP 是单个出口 IP 的配置项。
type EgressIP struct {
	// Addr 是绑定在本机网卡上的地址，用作 TCP 源地址。
	Addr string `yaml:"addr"`
	// PublicIP 是对应的弹性公网 IP，仅用于展示审计。
	PublicIP string `yaml:"public_ip"`
	// MaxKeys 限制该 IP 下绑定的 Key 数量。
	MaxKeys int `yaml:"max_keys"`
	// Pool 限定该 IP 只承接哪一档 Key（hot / warm / cold）。
	//
	// 空串表示通用 IP，任何池的 Key 都可落在其上 —— 这是未显式分层时的
	// 兼容行为，等价于旧版单一池语义。
	//
	// 分层的意义在于让不同活跃度的 Key 物理隔离: cold 池单 IP 可承载上百个
	// 几乎不发请求的 Key，而 hot 池必须保持低密度。混放会让一个高频 Key
	// 与上百个 Key 共享出口，分层的收益归零。
	Pool string `yaml:"pool"`
}

// Provider 是单个上游服务商的配置。
type Provider struct {
	// BaseURL 是上游 API 的基础地址。
	BaseURL string `yaml:"base_url"`
	// QuotaKind 是配额计量单位: "token" | "count"
	QuotaKind string `yaml:"quota_kind"`
	// QuotaLimit 是单 Key 的配额上限（token 数或调用次数）。
	QuotaLimit int64 `yaml:"quota_limit"`
	// QuotaWindow 是配额周期（如 "24h" 或 "5h"）。
	QuotaWindow time.Duration `yaml:"quota_window"`
	// RefreshHour 是固定刷新点（0-23），火山为 12。商汤无固定刷新点时填 nil。
	RefreshHour *int `yaml:"refresh_hour"`
	// ModelMapping 将对外模型名映射为上游实际模型名。
	ModelMapping map[string]string `yaml:"model_mapping"`
	// CountModels 列出按次计费的模型。
	CountModels []string `yaml:"count_models"`
	// ReasoningModels 列出会输出思维链（reasoning_content）的模型。
	ReasoningModels []string `yaml:"reasoning_models"`
}

// UpstreamCommon 是跨渠道的通用上游配置。
type UpstreamCommon struct {
	// MaxRetries 是单次用户请求内的最大换 Key 重试次数。
	MaxRetries int `yaml:"max_retries"`
	// RetryBaseDelay 是重试退避的基准间隔。
	RetryBaseDelay time.Duration `yaml:"retry_base_delay"`
	// RetryJitter 是叠加在退避上的随机抖动上限。
	RetryJitter time.Duration `yaml:"retry_jitter"`
}

// Scheduler 是调度打分配置。
type Scheduler struct {
	// 五维分数权重
	WeightQuota   float64 `yaml:"weight_quota"`
	WeightHistory float64 `yaml:"weight_history"`
	WeightPersona float64 `yaml:"weight_persona"`
	WeightHealth  float64 `yaml:"weight_health"`
	// SoftPenalty 是越过软水位的扣分。
	SoftPenalty float64 `yaml:"soft_penalty"`
	// ActivePoolSize 是活跃池的目标大小。
	ActivePoolSize int `yaml:"active_pool_size"`
	// EnablePersona 控制是否启用行为画像时段过滤。
	EnablePersona bool `yaml:"enable_persona"`
	// MinRequestInterval 是同一 Key 的最小请求间隔（反作弊自约束）。
	//
	// 这个值直接决定系统吞吐上限，是最容易被忽略的容量约束:
	//
	//	理论上限 QPS = 活跃 Key 数 / MinRequestInterval(秒)
	//
	// 默认 100 Key / 5s = 20 QPS，正好匹配设计流量。但若只导入了 10 个 Key，
	// 上限就骤降到 2 QPS —— 超出部分会被调度器以「间隔不足」拒绝并返回 503，
	// 而错误现场看起来像配额耗尽。用 MaxQPS() 在启动时把这个数算给运维看。
	MinRequestInterval time.Duration `yaml:"min_request_interval"`

	// ExpectedPeakQPS 是预期的峰值请求速率，用于启动时校验档位份额与 IP 数是否匹配。
	//
	// 与 MinRequestInterval 推出的 MaxQPS() 不同: 后者是「全部 Key 同时满速」
	// 的理论天花板，而这个是运维对真实流量的预估。校验需要的是后者 ——
	// 用天花板去算每 IP 的请求密度会把任何配置都判超标。
	//
	// 留空（0）时跳过份额容量校验。这是刻意的默认: 不知道自己流量的部署
	// 不该被一个凭空假设的数字挡在启动之外。
	ExpectedPeakQPS float64 `yaml:"expected_peak_qps"`

	// PoolShares 是各档位（hot / warm / cold）应承担的流量份额。
	//
	// 出口分层把活跃度差异巨大的 Key 物理隔离，但只有出口侧分层是不够的:
	// 若调度器对所有档位一视同仁，cold 档的 Key 会与 hot 档同频被选中，
	// 「cold 档 Key 几乎不发请求」这个支撑高 max_keys 的前提就不成立，
	// cold 档单 IP 的瞬时并发反而会最高。
	//
	// 份额是相对值，无需归一化。例: {hot:70, warm:25, cold:5} 表示
	// 70% 的请求应落在 hot 档。空 map 表示不做配比（全档等权）。
	//
	// 实现方式是**先按份额抽档、再在档内加权随机**，而不是给 pool 一个
	// 附加分。加分会被其余四维（配额/历史/画像/健康）稀释，实际配比
	// 完全不可控 —— 一个 cold 档但配额充裕的 Key 照样能压过 hot 档。
	//
	// ⚠️ 份额、各档 IP 数、各档 max_keys 三者互相锁定，改一个必须复核另两个。
	//
	// 唯一的验收判据是**单个出口每秒发出多少请求**（上限 2 req/s，
	// 见 scheduler.TestSelect_份额决定单IP请求密度）。风控看的是这个绝对值，
	// 与档位间的比值无关。
	//
	// 默认值 {hot:70, warm:25, cold:5} 与 deploy/README.md 的 32 IP 方案
	// （IP 19/5/8，max_keys 10/50/100）配套，实测 20 QPS 下:
	//
	//	hot   100 Key / 19 IP 承担 70% → 0.74 req/s，单 Key 504 req/h
	//	warm  200 Key /  5 IP 承担 25% → 1.00 req/s，单 Key  90 req/h ← 最密
	//	cold  700 Key /  8 IP 承担  5% → 0.12 req/s，单 Key 5.1 req/h
	//
	// 这个配比让 cold 档单 Key 频率只有 hot 档的 1/99，**因而它才能安全地
	// 绑 100 个 Key/IP**（满载 514 req/h，仍是 hot 档单 IP 的 1/5）。
	// 也就是说 cold 档的高密度是份额倾斜换来的，不是白给的。
	//
	// 反例: 若改成 {hot:15, warm:25, cold:60}（贴近 Key 数量比例），
	// cold 档单 IP 升到 1.50 req/s、满载 6171 req/h，反超 hot 档 5.7 倍 ——
	// max_keys=100 立即失去依据，必须同步降到 20 以下。
	// 「按 Key 数量分配份额」看似公平，实际是把密度压力还给了 Key 最多的那档。
	PoolShares map[string]float64 `yaml:"pool_shares"`

	// PoolFallback 控制目标档位无可用 Key 时是否回退到其他档位。
	//
	// true（推荐）: 回退，保可用性。代价是极端情况下 cold 档 Key 会承接
	// 本应由 hot 档处理的流量，配比被打破。
	// false: 直接返回 ErrNoCandidate。配比严格，但 hot 档 Key 全部
	// 进入非活跃时段时会整体 503。
	PoolFallback bool `yaml:"pool_fallback"`

	// EgressMinInterval 是同一出口 IP 的最小请求间隔。
	//
	// MinRequestInterval 只约束单个 Key，管不住出口: 一个 IP 上 25 个 Key
	// 各自守着 5 秒间隔，IP 层面仍可达 5 QPS。而风控看到的是 IP —— 同一地址
	// 每秒冒出几个请求，无论背后几个账号都不像真人在操作。
	//
	// 启用分级配比后这个缺口尤其明显: 流量集中到 Key 数较少的 hot 档，
	// 同一出口被连续选中的概率远高于全档等权时。
	//
	// 0 表示不启用（此时出口密度完全不受约束）。它同时也是新的吞吐上限:
	//
	//	理论上限 QPS = 出口 IP 数 / EgressMinInterval(秒)
	//
	// 32 个 IP / 1s = 32 QPS。设得过大会让「出口间隔不足」成为主要拒绝原因，
	// 排查时看 rejectReasons 里的 egressTooSoon 计数即可区分。
	EgressMinInterval time.Duration `yaml:"egress_min_interval"`
}

// Refresh 是配额刷新窗口配置。
type Refresh struct {
	// PreRefreshHour 进入刷新前低功耗模式的小时数。
	PreRefreshStart string `yaml:"pre_refresh_start"`
	// WindowStart / WindowEnd 界定探测窗口。
	WindowStart string `yaml:"window_start"`
	WindowEnd   string `yaml:"window_end"`
	// ProbeInterval 是单个 Key 探测失败后的重试间隔。
	ProbeInterval time.Duration `yaml:"probe_interval"`
	// ProbeModel 是探测请求使用的模型。
	ProbeModel string `yaml:"probe_model"`
	// PostRefreshRampMinutes 是刷新确认后的限速时长（起床缓冲）。
	PostRefreshRampMinutes int `yaml:"post_refresh_ramp_minutes"`
	// Enabled 控制是否启用刷新探测器。
	Enabled bool `yaml:"enabled"`
}

// Fallback 是跨渠道兜底配置。
//
// P1-10: 默认关闭。付费渠道 fallback 若无预算护栏，一天可烧掉数月预算。
type Fallback struct {
	Enabled bool `yaml:"enabled"`
	// DailyBudgetCents 是每日预算上限（分）。触顶即熔断。
	DailyBudgetCents int64 `yaml:"daily_budget_cents"`
}

// Admin 是管理接口配置。
type Admin struct {
	// APIKey 是管理接口的鉴权密钥。为空则禁用管理接口。
	APIKey string `yaml:"api_key"`
}
