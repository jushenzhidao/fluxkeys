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

// VolcKey 是火山 Key 池中的一个 Key。Secret 字段为解密后的明文，
// 仅在显式调用带解密的读取方法时填充。
type VolcKey struct {
	ID                 int64
	KeyID              string
	SecretEnc          string
	Secret             string // 解密后的明文，默认为空
	Provider           string
	Pool               string
	Status             string
	PersonaID          string
	EgressIP           string
	HealthScore        int
	RefreshState       string
	RefreshConfirmedAt *time.Time
	LastError          string
	LastUsedAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// 火山 Key 的状态取值。
const (
	VolcStatusActive   = "active"
	VolcStatusCooldown = "cooldown"
	VolcStatusBanned   = "banned"
	VolcStatusInvalid  = "invalid"
)

// 刷新探测状态取值（P0-4）。
const (
	RefreshIdle      = "idle"
	RefreshProbing   = "probing"
	RefreshConfirmed = "confirmed"
	RefreshFailed    = "failed"
)

// VolcKeyFilter 是 ListVolcKeys 的过滤条件，零值表示不过滤。
type VolcKeyFilter struct {
	Pool         string
	Status       string
	Provider     string
	RefreshState string
	// WithSecret 为 true 时解密并填充 Secret 字段。
	WithSecret bool
	Limit      int
}

// VolcKeyState 是一次 Key 运行时状态更新。指针字段为 nil 表示不更新该列。
type VolcKeyState struct {
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
	VolcKeyID        string
	EgressIP         string
	Provider         string
	Model            string
	BillingKind      string
	QuotaDay         time.Time
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CountUnits       int
	EstimatedTokens  int64
	StatusCode       int
	IsStream         bool
	ErrorCode        string
	RetryCount       int
	LatencyMS        int
}

// KeyDailyHistory 是 Key 的每日归档，供调度的 S_history 维度使用。
type KeyDailyHistory struct {
	VolcKeyID            string
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
type QuotaDrift struct {
	VolcKeyID   string
	BillingKind string
	QuotaDay    time.Time
	Drift       int64
}
