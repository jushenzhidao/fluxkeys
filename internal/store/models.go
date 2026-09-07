package store

import "time"

// User 是平台用户（对外服务的计费主体）。
type User struct {
	ID              int64
	Name            string
	Email           string
	Status          string
	DailyTokenLimit int64
	RPMLimit        int
	TPMLimit        int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// UserAPIKey 是用户的一条 API Key 记录（不含明文）。
type UserAPIKey struct {
	ID         int64
	UserID     int64
	KeyHash    string
	KeyPrefix  string
	Name       string
	Status     string
	LastUsedAt *time.Time
	CreatedAt  time.Time
	RevokedAt  *time.Time
}

// AuthContext 是鉴权成功后附加到请求上下文的调用方身份与限额。
//
// P1-9: 用户级 RPM/TPM/日 Token 上限随身份一起返回，避免限流层再查一次库。
type AuthContext struct {
	UserID          int64
	UserName        string
	KeyID           int64 // user_api_keys.id
	KeyPrefix       string
	Status          string // 用户状态
	RPMLimit        int
	TPMLimit        int64
	DailyTokenLimit int64
}

// UpstreamKey 是上游 Key 池中的一个 Key。Secret 字段为解密后的明文，
// 仅在显式调用带解密的读取方法时填充。
type UpstreamKey struct {
	ID                 int64
	KeyID              string
	SecretEnc          string
	Secret             string // 解密后的明文，默认为空
	Provider           string
	Pool               string
	Status             string
	PersonaID          string
	EgressIP           string
	// Shard 是该 Key 的机器归属。空串表示未分片。语义见 schema.sql。
	Shard              string
	HealthScore        int
	RefreshState       string
	RefreshConfirmedAt *time.Time
	LastError          string
	LastUsedAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// 上游 Key 的状态取值。
const (
	KeyStatusActive   = "active"
	KeyStatusCooldown = "cooldown"
	KeyStatusBanned   = "banned"
	KeyStatusInvalid  = "invalid"
)

// 刷新探测状态取值（P0-4）。
const (
	RefreshIdle      = "idle"
	RefreshProbing   = "probing"
	RefreshConfirmed = "confirmed"
	RefreshFailed    = "failed"
)

// UpstreamKeyFilter 是 ListUpstreamKeys 的过滤条件，零值表示不过滤。
type UpstreamKeyFilter struct {
	Pool         string
	Status       string
	Provider     string
	RefreshState string

	// Shard 非空时只返回归属该分片的 Key。
	//
	// 语义是严格的: 分片实例绝不装载别的分片或未指派分片的 Key。
	// 「顺带装载未分片的」看似方便迁移，实际会让多台分片实例同时装载
	// 同一批存量 Key —— Key 与出口 IP 终身绑定，等于让同一账号从两个
	// IP 出去，正是风控最敏感的信号。未指派的 Key 由启动日志告警，
	// 运维用 AssignShard 指派后自然出现在对应实例。
	//
	// 空串仍表示不过滤（单机部署全量装载），装载范围的决定权在调用方。
	Shard string

	// WithSecret 为 true 时解密并填充 Secret 字段。
	WithSecret bool
	Limit      int
}

// UpstreamKeyState 是一次 Key 运行时状态更新。指针字段为 nil 表示不更新该列。
type UpstreamKeyState struct {
	Status      *string
	Pool        *string
	HealthScore *int
	EgressIP    *string
	PersonaID   *string
	LastError   *string
	// TouchLastUsed 为 true 时把 last_used_at 更新为 now()。
	TouchLastUsed bool
}


// UsageRecord 是一条用量流水，计费与审计的事实来源。
type UsageRecord struct {
	RequestID        string
	UserID           int64 // 0 表示无归属（如内部探测请求）
	UserAPIKeyID     int64
	UpstreamKeyID        string
	EgressIP         string
	Provider         string
	Model            string
	BillingKind      string
	QuotaDay         time.Time
	PromptTokens     int64
	CompletionTokens int64
	// ReasoningTokens 已含在 CompletionTokens 内，不参与计费。
	ReasoningTokens int64
	TotalTokens     int64
	CountUnits      int
	EstimatedTokens int64
	StatusCode      int
	IsStream        bool
	ErrorCode       string
	RetryCount      int
	LatencyMS       int
}

// KeyDailyHistory 是 Key 的每日归档，供调度的 S_history 维度使用。
type KeyDailyHistory struct {
	UpstreamKeyID string
	// Provider 必须随归档一起落库: S_history 是 provider 内部的相对打分，
	// 缺了它就无法把「这个 Key 昨天很闲」限定在同一上游的池子里比较。
	// 表上是 NOT NULL 列，漏传会让整个归档任务每轮静默失败。
	Provider             string
	QuotaDay             time.Time
	TokenUsed            int64
	CountUsed            int
	TokenLimit           int64
	TokenRatio           float64
	RequestCount         int
	ErrorCount           int
	ConsecutiveLightDays int
}

// AuditLog 是一条管理操作审计。
type AuditLog struct {
	Actor  string
	Action string
	Target string
	Detail map[string]any
}

// QuotaDrift 是一次配额对账偏差记录（P0-2）。
//
// Provider 必填: 同一个 upstream_key_id 在不同 provider 下是两套独立配额，
// 偏差不带 provider 就无法归因到具体上游账户，而排查超刷时第一件事就是
// 定位「哪个上游的水位对不上」。
type QuotaDrift struct {
	UpstreamKeyID string
	Provider      string
	BillingKind   string
	QuotaDay      time.Time
	Drift         int64
}
