package adminapi

import (
	"errors"
	"time"
)

// 本文件是管理面自有的数据形状。
//
// 它们原先定义在 gateway 的 deps.go / provider_deps.go 里，与热路径的依赖
// 混在一起。搬到本包是有意的: 这些类型全部只服务于管理接口的请求与响应，
// 把它们留在网关主体，会让「管理面的改动」看起来像「核心契约的改动」。
//
// 网关主体通过类型别名（见 internal/gateway/aliases.go）继续使用同一批类型，
// 因此既有调用点无需改动 —— 别名是同一类型，不是副本。

// KeyState 是单个 Key 的对外状态视图，供 GET /admin/keys 使用。
type KeyState struct {
	KeyID string `json:"key_id"`
	// Provider 是上游服务商。
	//
	// 多 provider 部署下没有这个字段，/admin/keys 就无法回答「这个 Key 属于
	// 哪家上游」—— 运维只能靠 Key 命名去猜，或回库查。额度、水位、封禁处置
	// 都是 provider 维度的，缺了归属这些判断都无从落地。
	Provider    string    `json:"provider"`
	Status      string    `json:"status"`
	Pool        string    `json:"pool"`
	EgressIP    string    `json:"egress_ip"`
	PersonaID   string    `json:"persona_id"`
	TokenUsed   int64     `json:"quota_token_used"`
	TokenLimit  int64     `json:"quota_token_limit"`
	CountUsed   int64     `json:"quota_count_used"`
	CountLimit  int64     `json:"quota_count_limit"`
	HealthScore int       `json:"health_score"`
	LastUsedAt  time.Time `json:"last_used"`
}

// TokenRatio 返回 Token 配额消耗比例。
func (s KeyState) TokenRatio() float64 {
	if s.TokenLimit <= 0 {
		return 0
	}
	return float64(s.TokenUsed) / float64(s.TokenLimit)
}

// KeyPatch 是一次 Key 元数据局部更新的入参。指针为 nil 表示该列不更新。
//
// 用指针而非零值判空是硬要求: status 的空串与「字段缺席」在 encoding/json
// 解成 string 后不可区分，据零值判断会把「请求里没提 status」当成「要把
// status 清空」。这与 UpsertUpstreamKey 踩过的 EXCLUDED 被 VALUES 兜底污染是
// 同一类缺陷 —— 都是无法区分未提供与空值，后果都是静默改写不该改的列。
type KeyPatch struct {
	Status    *string
	Pool      *string
	PersonaID *string

	// ExpectedStatus 非 nil 时启用乐观并发控制: 当前 status 与之不等则拒绝。
	ExpectedStatus *string

	// RejectStatusFrom 非空时，当前 status 落在其中即拒绝本次更新。
	//
	// 用它表达「终态不许直接复活」。判定必须由存储层在单条语句内完成 ——
	// 在这里先查再改会留下 TOCTOU 窗口，两个并发 PATCH 各自校验通过再
	// 互相覆盖，守卫看起来生效而实际形同虚设。
	RejectStatusFrom []string
}

// HasFieldUpdate 报告本次是否至少要改一列。
func (p KeyPatch) HasFieldUpdate() bool {
	return p.Status != nil || p.Pool != nil || p.PersonaID != nil
}

// KeyPatchResult 是局部更新的新旧值对照。
//
// 回显旧值而非只回显新值: 运维需要「改之前确实是那个值」的凭据，
// 且只记新值的审计无法回答「这个 Key 何时从 active 变成 banned 的」。
type KeyPatchResult struct {
	PrevStatus    string
	PrevPool      string
	PrevPersonaID string
	NewStatus     string
	NewPool       string
	NewPersonaID  string
}

// ErrKeyNotFound 表示目标 Key 不存在，对外映射为 404。
var ErrKeyNotFound = errors.New("gateway: Key 不存在")

// ErrPreconditionFailed 表示 Key 存在但不满足前置条件，对外映射为 409。
//
// 与 ErrKeyNotFound 分开是必需的: 两者在 UPDATE 层面都是「影响 0 行」，
// 混在一起会让运维在「Key ID 打错」与「状态已被别人改过」之间无从下手。
var ErrPreconditionFailed = errors.New("gateway: 前置条件不满足")

// NewUser 是创建用户的入参。零值限额交由存储层填默认值。
type NewUser struct {
	Name            string
	Email           string
	RPMLimit        int
	TPMLimit        int64
	DailyTokenLimit int64
}

// NewUpstreamKey 是导入一个上游 Key 的入参。
//
// 名字里刻意不含具体厂商: 同一张 upstream_keys 表同时承载 volc 与 sensenova
// 等多个上游的 Key，字段里的 Provider 才是区分它们的唯一依据。
type NewUpstreamKey struct {
	// Provider 是上游服务商标识（volc/sensenova/qwen 等），必填。
	Provider string `json:"provider"`

	KeyID string `json:"key_id"`
	// Secret 是明文密钥，由存储层加密后落库。
	//
	// 加密职责放在存储层而非调用方: 让运维手写 SQL 导入意味着要自行复刻
	// AES-GCM 加密逻辑，一旦实现有偏差密文就永远解不开。
	Secret string `json:"secret"`
	Pool   string `json:"pool"`
	Status string `json:"status"`

	// PersonaID 是该 Key 的行为画像标识。
	//
	// 必须可在导入时指定: 火山商务反馈封禁根因是「用户行为规律相似」，
	// 而画像分配是打散规律的手段。若只能事后逐个改，1000 个 Key 的
	// 画像分配就变成 1000 次接口调用。
	PersonaID string `json:"persona_id"`

	// EgressIP 是该 Key 终身绑定的出口 IP。
	//
	// 留空表示交由出口池按哈希自动分配。显式指定的场景是迁移已有池子 ——
	// 此时绑定关系已经在火山侧形成历史，重新哈希会让每个 Key 换 IP，
	// 等于一次性制造 1000 个「换了出口的老账号」。
	EgressIP string `json:"egress_ip"`
}

// IssuedKey 是一条新签发的用户 API Key 的元信息（不含明文）。
type IssuedKey struct {
	ID     int64
	Prefix string
}
