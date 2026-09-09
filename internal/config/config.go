// Package config 定义 FluxKeys 网关的全部可配置项。
//
// 配置来源优先级: 环境变量 > YAML 文件 > 内置默认值。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是网关的根配置。
type Config struct {
	Server    Server              `yaml:"server"`
	Redis     Redis               `yaml:"redis"`
	Postgres  Postgres            `yaml:"postgres"`
	Quota     Quota               `yaml:"quota"`
	Egress    Egress              `yaml:"egress"`
	Providers map[string]Provider `yaml:"providers"` // 多上游配置（替代 Upstream）
	Upstream  UpstreamCommon      `yaml:"upstream"`  // 通用重试配置
	Scheduler Scheduler           `yaml:"scheduler"`
	Refresh   Refresh             `yaml:"refresh"`
	Fallback  Fallback            `yaml:"fallback"`
	Admin     Admin               `yaml:"admin"`

	// DefaultProvider 是省略 provider 时的兜底上游。
	//
	// 存在的理由: 导入 Key 与模型路由都需要一个 provider，但单上游部署下
	// 强制每次显式指定纯属噪音。留空时若只配了一个 provider 则自动采用它，
	// 多 provider 场景则必须显式指定，避免静默路由到错误上游。
	DefaultProvider string `yaml:"default_provider"`
}

// ResolveProvider 补全省略的 provider。
//
// 规则: 显式值原样返回；留空时仅在 DefaultProvider 已配置、或全局只有一个
// provider 时才推断，多 provider 且未配默认值时返回空串由调用方报错。
func (c *Config) ResolveProvider(provider string) string {
	if provider != "" {
		return provider
	}
	if c.DefaultProvider != "" {
		return c.DefaultProvider
	}
	if len(c.Providers) == 1 {
		for name := range c.Providers {
			return name
		}
	}
	return ""
}

// EgressVerifyTarget 返回出口自检应当拨测的 host:port。
//
// 优先取显式配置的 verify_target；未配置时从实际启用的 provider base_url
// 推导，而**不是**退回某个写死的厂商域名。
//
// 为什么不能写死: 自检的意义是「从这个出口能否连到我们真正要发请求的地方」。
// 目标一旦与真实上游不同，自检就变成了对无关域名的连通性测试 —— 它会在
// 真实上游被出口 IP 拉黑时依旧全绿，恰好在最需要它报警时失效。这个偏差
// 在多 provider 化后必然出现: 配置里是商汤，自检却在拨火山。
//
// 多 provider 时取 default_provider，无默认值则按名称排序取第一个，
// 保证同一份配置每次得到相同目标（便于运维对照日志）。
func (c *Config) EgressVerifyTarget() string {
	if c.Egress.VerifyTarget != "" {
		return c.Egress.VerifyTarget
	}
	if len(c.Providers) == 0 {
		return ""
	}

	name := c.ResolveProvider("")
	if _, ok := c.Providers[name]; !ok {
		names := make([]string, 0, len(c.Providers))
		for n := range c.Providers {
			names = append(names, n)
		}
		sort.Strings(names)
		name = names[0]
	}

	u, err := url.Parse(c.Providers[name].BaseURL)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "http" {
		return u.Hostname() + ":80"
	}
	return u.Hostname() + ":443"
}

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

// TokenHard 返回 Token 型配额的硬水位。
func (q Quota) TokenHard() int64 { return int64(float64(q.TokenLimit) * q.TokenHardRatio) }

// TokenSoft 返回 Token 型软水位。
func (q Quota) TokenSoft() int64 { return int64(float64(q.TokenLimit) * q.TokenSoftRatio) }

// CountHard 返回次数型硬水位。
func (q Quota) CountHard() int64 { return int64(float64(q.CountLimit) * q.CountHardRatio) }

// CountSoft 返回次数型软水位。
func (q Quota) CountSoft() int64 { return int64(float64(q.CountLimit) * q.CountSoftRatio) }

// LimitsFor 返回指定 provider 在该配额类型下的软硬水位。
//
// 优先采用 provider 自己的 quota_limit，仅在其未配置（<=0）时回退到全局
// quota.token_limit / count_limit。比例（hard_ratio / soft_ratio）始终取全局值 ——
// 它表达的是「留多少安全余量」这一运维策略，与上游是谁无关。
//
// 必须按 provider 取额度: 各家上游的单 Key 限额天差地别（火山按 token 给
// 数百万，商汤公测按次给一千多）。用全局值会同时错向两边 ——
// 对额度小的上游是超发（真实额度耗尽后网关仍在放行，请求全部撞上游 429），
// 对额度大的上游是白白闲置。而且这类偏差不会报错，只会表现为「配了
// quota_limit 但完全没用」，极难从日志看出来。
func (c *Config) LimitsFor(provider string, kindCount bool) (hard, soft int64) {
	limit := int64(0)
	if p, ok := c.Providers[provider]; ok {
		limit = p.QuotaLimit
	}
	if limit <= 0 {
		if kindCount {
			limit = c.Quota.CountLimit
		} else {
			limit = c.Quota.TokenLimit
		}
	}
	if kindCount {
		return int64(float64(limit) * c.Quota.CountHardRatio),
			int64(float64(limit) * c.Quota.CountSoftRatio)
	}
	return int64(float64(limit) * c.Quota.TokenHardRatio),
		int64(float64(limit) * c.Quota.TokenSoftRatio)
}

// IsCountProvider 判断 provider 的配额口径是否为「按次」。
//
// 单独给出这个方法而不是让调用方各自读 QuotaKind: 归档、展示、对账都需要
// 知道「这个 provider 的额度该拿哪个量纲去比」。少一处判断就会拿 token 数
// 去除以按次额度 —— 两个数都真实存在、都非零，比值也在 [0,1] 里，没有任何
// 一层会报错，只会让历史打分读到一个毫无意义的小数。
func (c *Config) IsCountProvider(provider string) bool {
	p, ok := c.Providers[provider]
	if !ok {
		return false
	}
	return strings.EqualFold(p.QuotaKind, "count")
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

// maxReqPerHourPerIP 是单个出口 IP 每小时的请求上限。
//
// 约 0.8 QPS。真人使用 AI 助手的强度远低于此，取这个值是留足余量的同时
// 仍能拦住「某档位份额远超其 IP 数所能承担」这类配置错误。
const maxReqPerHourPerIP = 3000

// validatePoolShareCapacity 检查各档份额是否与该档的出口 IP 数匹配。
//
// 拦的是这类错误: 给 hot 档配 70% 份额，但它只有 4 个 IP —— 20 QPS 下
// 每 IP 每小时要发 12600 次请求，是真人强度的 4 倍。配比本身不提高容量，
// 它只是重新分配压力，配错会把密度问题从一个档位搬到另一个档位。
//
// 仅在 multi_ip 且已配份额时检查。direct 模式没有出口概念，
// 未配份额时全档等权，压力自然按 Key 数量分布，不会失配。
func (c *Config) validatePoolShareCapacity() error {
	shares := c.Scheduler.NormalizedPoolShares()
	if shares == nil || c.Egress.Mode != "multi_ip" || len(c.Egress.IPs) == 0 {
		return nil
	}

	// 统计各档的 IP 数。未标档位的 IP 通用，计入所有档位的可用量。
	perPool := map[string]int{}
	generic := 0
	for _, ip := range c.Egress.IPs {
		if ip.Pool == "" {
			generic++
			continue
		}
		perPool[ip.Pool]++
	}
	// 没有任何 IP 标了档位 → 用户没在用出口分层，份额与 IP 数的匹配无从谈起
	// （所有 IP 对所有档位等价可用）。这时份额只影响 Key 的选取偏好，
	// 不会造成某个出口被过度使用，故跳过检查。
	if len(perPool) == 0 {
		return nil
	}

	// 用预期峰值 QPS 推算，而不是 Scheduler.MaxQPS()。
	//
	// MaxQPS 算的是「全部 Key 同时按最小间隔发请求」的理论上限 ——
	// 那是系统能承受的天花板，不是实际负载。用它做校验会把任何配置都判超标
	// （1000 个 Key ÷ 60s 间隔 = 16 QPS/Key 累计上千 QPS，除以 4 个 IP 必然爆表）。
	//
	// 真实约束是用户的峰值流量。未配置时跳过检查 —— 没有这个数字，
	// 任何推算都是凭空假设，宁可不查也不要给出错误的拦截。
	if c.Scheduler.ExpectedPeakQPS <= 0 {
		return nil
	}
	reqPerHour := c.Scheduler.ExpectedPeakQPS * 3600

	for pool, share := range shares {
		if share <= 0 {
			continue
		}
		ips := perPool[pool] + generic
		if ips == 0 {
			return fmt.Errorf(
				"config: scheduler.pool_shares[%s]=%.0f%% 但 egress.ips 里没有该档位的 IP，"+
					"这些流量会全部走回退路径（或直接失败）", pool, share*100)
		}
		perIP := reqPerHour * share / float64(ips)
		if perIP > maxReqPerHourPerIP {
			return fmt.Errorf(
				"config: scheduler.pool_shares[%s]=%.0f%% 与该档 %d 个 IP 不匹配 —— "+
					"每 IP 每小时 %.0f 次请求，超过上限 %d（约 0.8 QPS，真人强度）。"+
					"要么下调该档份额，要么给该档增加 IP",
				pool, share*100, ips, perIP, maxReqPerHourPerIP)
		}
	}
	return nil
}

// NormalizedPoolShares 返回归一化后的份额，总和为 1。
//
// 返回 nil 表示未启用配比（未配置，或所有份额均为非正数）。
// 调用方应据此退化为全档等权的加权随机。
func (s Scheduler) NormalizedPoolShares() map[string]float64 {
	if len(s.PoolShares) == 0 {
		return nil
	}
	var sum float64
	for _, v := range s.PoolShares {
		if v > 0 {
			sum += v
		}
	}
	if sum <= 0 {
		return nil
	}
	out := make(map[string]float64, len(s.PoolShares))
	for k, v := range s.PoolShares {
		if v > 0 {
			out[k] = v / sum
		}
	}
	return out
}

// MaxQPS 返回当前配置下的理论吞吐上限。
//
// keyCount 传实际装载的活跃 Key 数而非 ActivePoolSize —— 后者只是目标值，
// 新部署时可能只导入了几个 Key，用目标值算会得出一个偏乐观的数字。
//
// MinRequestInterval 为 0 表示不做节流，此时不存在这个上限。
func (s Scheduler) MaxQPS(keyCount int) float64 {
	if s.MinRequestInterval <= 0 || keyCount <= 0 {
		return 0
	}
	return float64(keyCount) / s.MinRequestInterval.Seconds()
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

// Load 读取配置文件并叠加环境变量覆盖。path 为空时仅使用默认值 + 环境变量。
func Load(path string) (*Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: 读取 %s: %w", path, err)
		}
		// 严格模式: 未知字段直接报错而非静默丢弃。
		//
		// 静默丢弃的代价在生产上是隐蔽且昂贵的 —— 把 admin.api_key 误写成
		// admin.token 会让整组管理路由不注册（server.go 以 APIKey 为空作为
		// 禁用信号），把 egress.ips 误写成 bind_ips 会让多出口静默降级为
		// direct 单出口。两者都表现为「配置写了但功能不存在」，且启动日志
		// 一切正常，只能靠逐行比对结构体标签才能发现。
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("config: 解析 %s: %w", path, err)
		}

		// providers 必须整体替换默认值，不能合并。
		//
		// yaml 对 map 是逐键合并的，于是 Default() 里的 "volc" 会残留在
		// 只配了 sensenova 的部署上。危害是双重的:
		//
		//  1. 模型静默路由到未部署的上游 —— seedream-3.0 命中 volc 的
		//     count_models，请求被送去一个从未导入过 Key 的 provider，
		//     表现为「配额耗尽」而非「模型不存在」，排查方向完全错。
		//  2. ResolveProvider 的「只有一个 provider 时自动推断」永久失效，
		//     因为 map 里永远有两个键。单上游部署被迫每次显式写 provider，
		//     而这正是 DefaultProvider 想消除的噪音。
		//
		// 判据用「文件里是否出现 providers 键」而非「解析后是否非空」:
		// 后者无法区分「没配」与「配成空 map」，而显式配空 map 应当报错
		// （Validate 会拦），不该悄悄回落到 volc 默认值。
		var probe struct {
			Providers map[string]Provider `yaml:"providers"`
		}
		if err := yaml.Unmarshal(data, &probe); err == nil && probe.Providers != nil {
			cfg.Providers = probe.Providers
		}
	}

	applyEnv(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("FLUXKEYS_ADDR"); v != "" {
		cfg.Server.Addr = v
	}
	if v := os.Getenv("FLUXKEYS_METRICS_ADDR"); v != "" {
		cfg.Server.MetricsAddr = v
	}
	// 分片标识走环境变量是刻意的: 多机部署时同一份配置文件与镜像分发到
	// 每台机器，唯一的差异就是这个值。写进 YAML 就意味着每台机器一份配置。
	if v := os.Getenv("FLUXKEYS_SHARD_ID"); v != "" {
		cfg.Server.ShardID = v
	}
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		cfg.Redis.Addr = v
	}
	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		cfg.Redis.Password = v
	}
	if v := os.Getenv("REDIS_DB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Redis.DB = n
		}
	}
	if v := os.Getenv("POSTGRES_DSN"); v != "" {
		cfg.Postgres.DSN = v
	}
	// 推理模型预扣的两个调优旋钮暴露为环境变量：真实输出倍率随模型版本
	// 漂移，需要能在不重建镜像的前提下调整。非法值忽略，交由 Validate 兜底。
	if v := os.Getenv("QUOTA_REASONING_OUTPUT_MULTIPLIER"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Quota.ReasoningOutputMultiplier = f
		}
	}
	if v := os.Getenv("QUOTA_REASONING_FLOOR_TOKENS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Quota.ReasoningFloorTokens = n
		}
	}
	if v := os.Getenv("VOLC_BASE_URL"); v != "" {
		// 向后兼容：VOLC_BASE_URL 环境变量更新 volc provider 的 BaseURL
		if p, ok := cfg.Providers["volc"]; ok {
			p.BaseURL = v
			cfg.Providers["volc"] = p
		}
	}
	if v := os.Getenv("EGRESS_MODE"); v != "" {
		cfg.Egress.Mode = v
	}
	// EGRESS_IPS 形如 "172.16.0.2=1.2.3.4,172.16.0.3=1.2.3.5"
	if v := os.Getenv("EGRESS_IPS"); v != "" {
		cfg.Egress.IPs = parseEgressIPs(v)
	}
	if v := os.Getenv("EGRESS_VERIFY_ON_START"); v != "" {
		cfg.Egress.VerifyOnStart = v == "true" || v == "1"
	}
	if v := os.Getenv("EGRESS_BAN_DETECT_KEYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Egress.BanDetectKeys = n
		}
	}
	if v := os.Getenv("EGRESS_BAN_DETECT_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Egress.BanDetectWindow = d
		}
	}
	// 允许显式设为 0 关闭自动恢复。
	//
	// 与上面几项不同，这里不能用「解析失败就保持默认」——默认值是 2h（已启用），
	// 而运维写 EGRESS_BAN_COOLDOWN=0 的意图恰恰是关掉它。time.ParseDuration("0")
	// 返回 0 且无错误，所以直接赋值即可；写了非法值则保持默认，避免把
	// 一个笔误变成「出口永不恢复」这种静默的容量泄漏。
	if v := os.Getenv("EGRESS_BAN_COOLDOWN"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Egress.BanCooldown = d
		}
	}
	if v := os.Getenv("EGRESS_BAN_COOLDOWN_MAX"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Egress.BanCooldownMax = d
		}
	}
	if v := os.Getenv("ADMIN_API_KEY"); v != "" {
		cfg.Admin.APIKey = v
	}
	if v := os.Getenv("REFRESH_ENABLED"); v != "" {
		cfg.Refresh.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("QUOTA_TOKEN_LIMIT"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Quota.TokenLimit = n
		}
	}
}

// defaultMaxKeysPerIP 是未指定时单个出口 IP 的 Key 承载上限。
//
// 取 10 是保守值，适用于未做分层、也未收窄行为画像的部署。
// 画像窄化后（见 internal/persona）同 IP 的并发密度大幅下降，
// warm/cold 档可显著调高 —— 但这必须显式配置，不做隐式放宽。
const defaultMaxKeysPerIP = 10

// parseEgressIPs 解析 EGRESS_IPS 环境变量。
//
// 单项语法: <addr>[=<public_ip>][|<pool>[|<max_keys>]]
//
//	172.16.0.11                              仅私网地址
//	172.16.0.11=203.0.113.11                 带公网地址
//	172.16.0.11=203.0.113.11|hot             限定 hot 档
//	172.16.0.11=203.0.113.11|cold|100        限定 cold 档且承载 100 个 Key
//
// 用 '|' 而非 ':' 分隔是为了避免与 IPv6 地址的冒号冲突。
//
// 注意本变量一旦设置会**整体覆盖** YAML 里的 egress.ips，包括分层配置。
// 若已在 YAML 中做了分层，就不要再设置 EGRESS_IPS，否则分层会被静默清掉。
func parseEgressIPs(s string) []EgressIP {
	var out []EgressIP
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// 先切出档位与容量，剩余部分才是 addr[=public]
		pool, maxKeys := "", defaultMaxKeysPerIP
		if i := strings.Index(part, "|"); i >= 0 {
			rest := part[i+1:]
			part = strings.TrimSpace(part[:i])
			if j := strings.Index(rest, "|"); j >= 0 {
				pool = strings.TrimSpace(rest[:j])
				if n, err := strconv.Atoi(strings.TrimSpace(rest[j+1:])); err == nil && n > 0 {
					maxKeys = n
				}
			} else {
				pool = strings.TrimSpace(rest)
			}
		}

		addr, public := part, ""
		if i := strings.Index(part, "="); i >= 0 {
			addr, public = strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])
		}
		if addr == "" {
			continue
		}
		out = append(out, EgressIP{
			Addr: addr, PublicIP: public, MaxKeys: maxKeys, Pool: pool,
		})
	}
	return out
}

// Validate 校验配置的自洽性，尽早暴露错误而非在运行时才失败。
func (c *Config) Validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("config: server.addr 不能为空")
	}
	if c.Redis.Addr == "" {
		return fmt.Errorf("config: redis.addr 不能为空")
	}

	switch c.Egress.Mode {
	case "direct":
	case "multi_ip":
		if len(c.Egress.IPs) == 0 {
			return fmt.Errorf("config: egress.mode=multi_ip 必须配置 egress.ips")
		}
	default:
		return fmt.Errorf("config: egress.mode 非法 %q（应为 direct 或 multi_ip）", c.Egress.Mode)
	}

	// 阈值 1 意味着任何单个 Key 被上游禁用都会连带撤离整个出口 ——
	// 那正是本判据要避免的误判。2 同样过于激进（两个 Key 恰好同时出问题
	// 并不罕见），而误判的代价是一次性制造大批「换了出口的老账号」。
	if c.Egress.BanDetectKeys != 0 && c.Egress.BanDetectKeys < 3 {
		return fmt.Errorf(
			"config: egress.ban_detect_keys=%d 过低，会把单个 Key 被禁误判为出口被封"+
				"（应为 0 关闭，或 >=3）", c.Egress.BanDetectKeys)
	}
	if c.Egress.BanDetectKeys > 0 && c.Egress.BanDetectWindow <= 0 {
		return fmt.Errorf("config: 启用 egress.ban_detect_keys 必须设置 ban_detect_window")
	}

	// max 小于 base 会让指数退避彻底失效: backoff 先按 2 的幂次累乘，
	// 再被 maxWait 截断，于是每次封禁都只等 max —— 反复被封的脏出口
	// 与首次被封的出口等同处理，而前者恰恰最需要长时间冷却。
	// 这个错配不会报错也不影响功能，只是退避默默不生效。
	if c.Egress.BanCooldown > 0 && c.Egress.BanCooldownMax < c.Egress.BanCooldown {
		return fmt.Errorf(
			"config: egress.ban_cooldown_max=%s 小于 ban_cooldown=%s，指数退避会失效"+
				"（每次封禁都只等 max）", c.Egress.BanCooldownMax, c.Egress.BanCooldown)
	}

	// 档位名写错（如 "Hot" / "hot " / "hott"）不会报错，只会让那份额永远
	// 抽不到 Key，从而静默退化 —— 配比看着配了却完全没生效。
	for pool, share := range c.Scheduler.PoolShares {
		switch pool {
		case "hot", "warm", "cold":
		default:
			return fmt.Errorf(
				"config: scheduler.pool_shares 含未知档位 %q（应为 hot / warm / cold）", pool)
		}
		if share < 0 {
			return fmt.Errorf("config: scheduler.pool_shares[%s]=%v 不能为负", pool, share)
		}
	}
	// 配了 pool_shares 但全是 0 等同于没配。这几乎一定是笔误，
	// 而它的表现是「配比静默失效」，比直接报错难查得多。
	if len(c.Scheduler.PoolShares) > 0 && c.Scheduler.NormalizedPoolShares() == nil {
		return fmt.Errorf("config: scheduler.pool_shares 全为 0，至少一个档位需为正数")
	}
	if err := c.validatePoolShareCapacity(); err != nil {
		return err
	}

	if c.Quota.TokenHardRatio <= 0 || c.Quota.TokenHardRatio > 1 {
		return fmt.Errorf("config: quota.token_hard_ratio 应在 (0,1] 区间")
	}
	if c.Quota.TokenSoftRatio >= c.Quota.TokenHardRatio {
		return fmt.Errorf("config: 软水位比例必须小于硬水位比例")
	}
	if c.Quota.CountSoftRatio >= c.Quota.CountHardRatio {
		return fmt.Errorf("config: 次数型软水位比例必须小于硬水位比例")
	}
	if c.Quota.ReapInterval >= c.Quota.LeaseTTL {
		return fmt.Errorf("config: quota.reap_interval 必须短于 lease_ttl，否则泄漏无法及时回收")
	}
	if c.Quota.EstimateMultiplier < 1 {
		return fmt.Errorf("config: quota.estimate_multiplier 不应小于 1")
	}
	// 小于 1 会让推理模型的预扣低于常规模型，与该系数的设计意图相反。
	if c.Quota.ReasoningOutputMultiplier < 1 {
		return fmt.Errorf("config: quota.reasoning_output_multiplier 不应小于 1")
	}
	if c.Quota.ReasoningFloorTokens < 0 {
		return fmt.Errorf("config: quota.reasoning_floor_tokens 不应为负")
	}

	// P1-10: 开启付费渠道 fallback 必须同时设定预算上限
	if c.Fallback.Enabled && c.Fallback.DailyBudgetCents <= 0 {
		return fmt.Errorf("config: 启用 fallback 必须设置 fallback.daily_budget_cents 预算上限")
	}

	// 探测间隔下限。
	//
	// 探测是发往上游的真实请求，窗口内每轮会对所有未确认的 Key 各发一次。
	// 100 个活跃 Key 配 5s 间隔就是 20 QPS 的纯探测流量，且这些请求内容
	// 高度雷同 —— 正好是火山商务反馈的封禁根因「用户行为规律相似」。
	//
	// 这个下限只能放在配置校验里: 放在调用点兜底，一旦出现第二个调用点就
	// 会漏（此前 main.go 与 background.go 各有一份循环，只有一份做了限制）。
	if c.Refresh.Enabled && c.Refresh.ProbeInterval < 30*time.Second {
		return fmt.Errorf("config: refresh.probe_interval 不得短于 30s（当前 %s），"+
			"高频雷同探测本身就是封禁特征", c.Refresh.ProbeInterval)
	}

	// 探测间隔必须能在窗口内至少跑几轮，否则等于没探测。
	window := parseClockOrZero(c.Refresh.WindowEnd) - parseClockOrZero(c.Refresh.WindowStart)
	if c.Refresh.Enabled && window > 0 && c.Refresh.ProbeInterval*3 > window {
		return fmt.Errorf("config: refresh.probe_interval (%s) 过长，"+
			"在 %s~%s 窗口内不足 3 轮探测，刷新很可能整天都确认不了",
			c.Refresh.ProbeInterval, c.Refresh.WindowStart, c.Refresh.WindowEnd)
	}
	return nil
}

// parseClockOrZero 解析 "HH:MM" 为当日偏移；无法解析时返回 0，交由调用方
// 的 window > 0 判断跳过校验 —— 时钟格式本身的校验不属于这里的职责。
func parseClockOrZero(s string) time.Duration {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
}

// IsCountModel 判断模型在指定 provider 上是否按次计费。
//
// 先把 model 归一化到上游名再匹配。调用方（gateway 的 quotaKindFor）传入的是
// 用户请求里的**对外名**，而 count_models 通常按**上游名**书写 —— 例如商汤配
// model_mapping: {gpt-4: SenseChat-5} 且 count_models: [SenseChat-5]。不归一化
// 时 "gpt-4" 匹配不上 "SenseChat-5"，按次计费的模型会被当成 token 型计量:
// 配额扣的是估算 token 而非调用次数，count 档位的额度永远扣不动，
// 而 token 档位被凭空消耗。两侧都能命中才是安全的。
func (c *Config) IsCountModel(provider, model string) bool {
	p, ok := c.Providers[provider]
	if !ok {
		return false
	}
	upstream := c.UpstreamModel(provider, model)
	for _, m := range p.CountModels {
		if strings.EqualFold(m, model) || strings.EqualFold(m, upstream) {
			return true
		}
	}
	return false
}

// IsReasoningModel 判断模型在指定 provider 上是否会输出思维链。
//
// 用子串匹配而非全等: 火山推理模型名带版本后缀（deepseek-v4-flash-ga-260731），
// 且新版本会持续发布，枚举全名会漏掉未来的型号 —— 而漏判的后果是预扣不足、
// 额度超刷，比误判（预扣偏高、并发略降）严重得多。
func (c *Config) IsReasoningModel(provider, model string) bool {
	if model == "" {
		return false
	}
	p, ok := c.Providers[provider]
	if !ok {
		return false
	}
	// 同 IsCountModel: 调用方传对外名，配置按上游名书写，两侧都要匹配。
	// 漏判在这里的代价是预扣不足、额度超刷。
	lower := strings.ToLower(model)
	lowerUp := strings.ToLower(c.UpstreamModel(provider, model))
	for _, m := range p.ReasoningModels {
		if m == "" {
			continue
		}
		needle := strings.ToLower(m)
		if strings.Contains(lower, needle) || strings.Contains(lowerUp, needle) {
			return true
		}
	}
	return false
}

// UpstreamModel 返回模型在指定 provider 上的实际名称。
func (c *Config) UpstreamModel(provider, model string) string {
	p, ok := c.Providers[provider]
	if !ok {
		return model
	}
	if v, ok := p.ModelMapping[model]; ok && v != "" {
		return v
	}
	return model
}

// ProviderForModel 从模型名推断 provider。
//
// 遍历所有 provider 的 ModelMapping，返回第一个声明支持该模型的 provider。
// 找不到时返回空字符串——调用方需判断并返回 400 "模型不存在"。
func (c *Config) ProviderForModel(model string) string {
	for name, p := range c.Providers {
		// 模型名完全匹配（对外名）
		if _, ok := p.ModelMapping[model]; ok {
			return name
		}
		// 模型名匹配上游实际名（允许直接用上游名调用）
		for _, upstreamName := range p.ModelMapping {
			if upstreamName == model {
				return name
			}
		}
		// CountModels / ReasoningModels 里声明过的模型同样算这个 provider 认领。
		// 按次计费模型常常不需要改名，因此不会出现在 ModelMapping 里。
		for _, m := range p.CountModels {
			if strings.EqualFold(m, model) {
				return name
			}
		}
	}
	return ""
}

// intPtr 返回 int 指针，用于默认配置中的可选字段。
func intPtr(v int) *int {
	return &v
}
