package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/quota"
)

// 本文件按「消费方定义接口」的方式声明 gateway 对调度与存储的依赖。
//
// 这样做有两个收益: 一是 gateway 与 internal/scheduler、internal/store 之间
// 没有编译期依赖，两侧可独立开发与测试；二是接口只包含 gateway 真正用到的
// 方法，实现方可以自由重构其余部分。

// Candidate 是调度器选出的一个可用 Key。
type Candidate struct {
	// KeyID 是业务标识，如 volc_001。
	KeyID string
	// Secret 是可直接用于上游认证的明文密钥。
	Secret string
	// EgressIP 是该 Key 绑定的出口 IP（direct 模式为空串），仅用于日志与审计。
	EgressIP string
	// Pool 是该 Key 的档位（hot / warm / cold）。
	//
	// 用途不止于观测: 尚未建立绑定的新 Key 会在请求热路径上首次绑定出口，
	// 此时必须落在对应档位的 IP 上，否则分层会出现缺口。
	Pool string
	// Kind 决定走 Token 型还是次数型配额。
	Kind quota.Kind
}

// FailureKind 是上报给调度器的失败类型，驱动 Key 状态机。
type FailureKind int

const (
	// FailureAuth: 401/403，Key 无效或被封，必须禁用。
	FailureAuth FailureKind = iota
	// FailureRateLimit: 429 速率限制，该 Key 进入冷却。
	FailureRateLimit
	// FailureServer: 上游 5xx，降低健康分但不禁用。
	FailureServer
	// FailureQuota: 上游侧额度耗尽，该 Key 应停止调度至下次刷新确认。
	FailureQuota
	// FailureNetwork: 连接失败或超时。
	FailureNetwork
)

// String 返回可读名称，用于日志与指标标签。
func (k FailureKind) String() string {
	switch k {
	case FailureAuth:
		return "auth"
	case FailureRateLimit:
		return "rate_limit"
	case FailureServer:
		return "server"
	case FailureQuota:
		return "quota"
	case FailureNetwork:
		return "network"
	}
	return "unknown"
}

// SelectRequest 是一次调度请求的输入。
type SelectRequest struct {
	// Provider 是上游服务商，调度器按此过滤 Key 池。
	Provider string
	// Model 是对外模型名。
	Model string
	// Kind 决定读取哪一套配额快照 —— Token 型与次数型的水位完全独立，
	// 用错会让次数型模型按 Token 水位排序而几乎永远排在最前。
	Kind quota.Kind
	// Exclude 是本次请求已尝试过的 Key，必须跳过 ——
	// 否则重试会反复撞上同一个坏 Key。
	Exclude map[string]bool
}

// Scheduler 是 gateway 对调度器的依赖。
type Scheduler interface {
	// Select 选出一个可用 Key。无可用 Key 时返回包装了 ErrNoCandidate 的错误。
	Select(ctx context.Context, req SelectRequest) (*Candidate, error)

	// MarkSuccess 上报一次成功请求，用于恢复健康分与更新最近使用时间。
	MarkSuccess(keyID string)

	// MarkFailure 上报一次失败，由调度器推进 Key 状态机。
	MarkFailure(keyID string, kind FailureKind)

	// SetKeyStatus 显式设置某 Key 在调度器内存中的状态。
	//
	// 管理接口改完库必须再调它，不能指望下一次 Reload 生效: healthTable.seed
	// 对已存在的 Key 不覆盖（「已有实时观测值，不被库中的旧快照覆盖」），
	// 所以只写库的封禁会在内存里完全不生效，被封的 Key 继续承接流量。
	//
	// status 取 store 的状态字面量（active/cooldown/banned/invalid）而非
	// 另立一套枚举: 这个值直接来自 PATCH 请求体，中间再翻译一层只会多一处
	// 可能漏配的映射表。
	SetKeyStatus(keyID, status string)

	// KeyStates 返回全部 Key 的状态快照。
	KeyStates(ctx context.Context) ([]KeyState, error)

	// Reload 重新从存储装载 Key 池。
	//
	// 需要这个入口是因为调度器持有的是内存快照，后台 key_reload 每 5 分钟
	// 才跑一次。新部署导入完 Key 后若不立刻重载，网关会在长达 5 分钟里
	// 对所有请求返回 503 —— 运维会合理地认为导入失败了。
	Reload(ctx context.Context) error
}

// ErrNoCandidate 表示当前无可用 Key。
//
// gateway 据此返回 503 service_busy，而非 500 —— 这是容量问题而非程序错误。
var ErrNoCandidate = errors.New("gateway: 无可用 Key")

// UserContext 是通过鉴权的用户身份与限额。
type UserContext struct {
	UserID   int64
	APIKeyID int64
	Name     string
	// RPMLimit / TPMLimit 为 0 表示该维度不限。
	RPMLimit        int
	TPMLimit        int64
	DailyTokenLimit int64
}

// UsageRecord 是一条用量流水，写入 Postgres 作为计费与审计的事实来源。
type UsageRecord struct {
	RequestID     string
	UserID        int64
	UserAPIKeyID  int64
	UpstreamKeyID string
	EgressIP      string
	Provider      string
	Model         string
	BillingKind   string
	// QuotaDay 是配额日（P0-3: 12:00 之前算作前一天），非自然日。
	QuotaDay         time.Time
	PromptTokens     int64
	CompletionTokens int64
	// ReasoningTokens 已含在 CompletionTokens 内，不参与计费。
	ReasoningTokens int64
	TotalTokens     int64
	CountUnits      int64
	EstimatedTokens int64
	StatusCode      int
	IsStream        bool
	ErrorCode       string
	RetryCount      int
	LatencyMS       int
}

// Store 是 gateway 对持久化层的依赖。
type Store interface {
	// AuthenticateUserKey 校验用户 API Key 明文。
	// 失败时返回包装了 ErrUnauthorized 的错误。
	AuthenticateUserKey(ctx context.Context, plaintext string) (*UserContext, error)

	// RecordUsage 写入用量流水。
	RecordUsage(ctx context.Context, r UsageRecord) error

	// Audit 写入管理操作审计日志。
	Audit(ctx context.Context, actor, action, target string, detail map[string]any) error

	// CreateUser 创建用户，返回补全了 ID 与默认限额的记录。
	CreateUser(ctx context.Context, in NewUser) (*UserContext, error)

	// CreateUserAPIKey 为用户签发 API Key。
	// plaintext 仅此一次可见，服务端只存哈希。
	CreateUserAPIKey(ctx context.Context, userID int64, name string) (plaintext string, key IssuedKey, err error)

	// RevokeUserAPIKey 吊销一条 API Key。
	//
	// userID 用于归属校验，防止拼错路径吊掉别人的 Key。目标不存在、
	// 不属于该用户、或已非 active 时返回包装了 ErrKeyNotFound 的错误。
	RevokeUserAPIKey(ctx context.Context, userID, keyID int64) error

	// UpsertUpstreamKey 导入或更新一个火山 Key。
	//
	// 没有这个入口，全新部署的 Key 池永远为空，网关只能对所有业务请求返回
	// 503 —— 一套装配完好但无法承接任何流量的系统。
	//
	// 语义是 upsert 而非 insert: 重复导入同一批 Key 应当是幂等的，
	// 便于运维用同一份清单反复执行。Secret 为空时保留库中已有密文。
	//
	// 返回 created 表示本次是新建而非更新，供导入接口区分「新增了几个」
	// 与「更新了几个」——运维执行同一份清单两次时这个区分是唯一的反馈。
	UpsertUpstreamKey(ctx context.Context, in NewUpstreamKey) (created bool, err error)

	// PatchUpstreamKeyState 原子地局部更新 Key 的 status / pool / persona_id。
	//
	// 语义见 KeyPatch。三种返回:
	//   - 成功: (结果, nil)
	//   - key_id 不存在: (nil, ErrKeyNotFound)
	//   - 存在但前置条件不满足: (当前值快照, ErrPreconditionFailed)
	//
	// 第三种同时返回结果与错误，让 409 的文案能写出冲突的实际内容 ——
	// 「当前状态为 banned，与 expected_status=active 不符」远比一句
	// 「状态冲突」有用。
	PatchUpstreamKeyState(ctx context.Context, keyID string, p KeyPatch) (*KeyPatchResult, error)

	// AssignShard 批量指派 Key 的机器归属（多机部署分片），返回改动行数。
	//
	// 分片过滤是严格相等: 未指派的 Key 不被任何实例装载。这个方法是把
	// Key 划入某台机器的唯一写入口 —— 归属决定该 Key 用哪台机器的出口
	// IP 发请求，一经指派不应再漂移（跨机 = 换出口 = 风控信号）。
	AssignShard(ctx context.Context, shard string, keyIDs []string) (int64, error)

	// SetVolcKeyEgressIP 记录 Key 当前绑定的出口 IP。
	//
	// 与 PatchUpstreamKeyState 分开是刻意的: 出口绑定不是运维随手可改的元数据，
	// 而是「这个账号从哪个 IP 出去」这一既成事实的存档。写入方只应是那些
	// 真正改变了内存中绑定关系的代码路径（Rebind / Migrate / 首次分配），
	// 它们必须在改完内存后立刻落库 —— 否则重启后 restoreBindings 读到旧值，
	// 会把 Key 换回原来的出口，等于凭空制造一次「老账号换了 IP」。
	SetVolcKeyEgressIP(ctx context.Context, keyID, egressIP string) error

	// Ping 检查数据库可达性，供 /readyz 使用。
	Ping(ctx context.Context) error
}

// Limiter 是 gateway 对用户级限流的依赖（P1-9）。
//
// 用接口而非直接依赖 *RateLimiter: 限流实现走 Redis Lua，而网关侧真正需要
// 被验证的是「拒绝时是否带 Retry-After」「fail-open 时是否放行」这类分支
// 判断。让测试能注入可控的判定结果，才能在没有 Redis 的环境里覆盖它们。
//
// *RateLimiter 已实现本接口。
type Limiter interface {
	// AllowRequest 检查 RPM 维度。rpmLimit <= 0 表示不限。
	AllowRequest(ctx context.Context, userID int64, rpmLimit int) (Result, error)
	// AllowTokens 检查 TPM 维度，tokens 是本次请求的预估用量。
	AllowTokens(ctx context.Context, userID int64, tpmLimit, tokens int64) (Result, error)
	// RefundTokens 归还预扣与实际用量的差额。
	RefundTokens(ctx context.Context, userID int64, tpmLimit, refund int64) error
}

// ErrUnauthorized 表示用户鉴权失败。
//
// 存储层可能区分「不存在 / 已吊销 / 用户停用」，但对外必须收敛为同一个
// 401 且同一段文案 —— 区分它们等于给撞库者一个「这个 Key 存在过」的
// 信息源。
var ErrUnauthorized = errors.New("gateway: 用户鉴权失败")

// QuotaManager 是 gateway 对配额管理器的依赖。
//
// internal/quota.Manager 已实现该接口。用接口而非具体类型是为了让测试
// 能注入可控的配额行为（如强制返回 ErrInsufficient）。
type QuotaManager interface {
	Acquire(ctx context.Context, provider, keyID string, kind quota.Kind, amount int64, lim quota.Limits, ttl time.Duration) (quota.Decision, *quota.Lease, error)
	Commit(ctx context.Context, lease *quota.Lease, actual int64) error
	Release(ctx context.Context, lease *quota.Lease) error
	Get(ctx context.Context, provider, keyID string, kind quota.Kind) (quota.Snapshot, error)
}
