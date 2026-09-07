package gateway

import (
	"context"
	"errors"
	"net/url"
	"os"
	"regexp"
	"time"
)

// provider 配置管理的依赖声明与校验规则。
//
// 沿用本包「消费方定义接口」的约定（见 deps.go）: gateway 不 import
// internal/store，只声明自己真正用到的方法与数据形状，由装配层桥接。

// ProviderConfigView 是一个 provider 的配置视图。
//
// 字段与 API 请求体、DB 列、版本快照三者同名，让运维在界面、审计与
// 库里看到的是同一套词汇 —— 少一层翻译就少一处可能漏配的映射表。
type ProviderConfigView struct {
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	BaseURL    string `json:"base_url"`
	QuotaKind  string `json:"quota_kind"`
	QuotaLimit int64  `json:"quota_limit"`
	// QuotaWindow 对外以纳秒整数传输，与库中存储一致。
	//
	// 不传 "24h" 文本: 文本要在两端各解析一次，而解析失败的默认行为是
	// 静默退化为 0 —— 配额周期为 0 意味着分桶键恒定、额度永不刷新，
	// 全程没有任何报错。整数没有解析失败这个状态。
	QuotaWindow int64 `json:"quota_window_nanos"`
	// RefreshHour 为 nil 表示无固定刷新点，与「0 点刷新」是两件事。
	// 用 *int 而非 -1 之类哨兵值 —— 哨兵值总有一天会被某处当成合法小时数。
	RefreshHour     *int              `json:"refresh_hour"`
	ModelMapping    map[string]string `json:"model_mapping"`
	CountModels     []string          `json:"count_models"`
	ReasoningModels []string          `json:"reasoning_models"`
	AdapterKind     string            `json:"adapter_kind"`
	// CredentialEnv 是环境变量名，永远不是凭据原文。见 schema.sql 的说明。
	CredentialEnv string     `json:"credential_env"`
	Version       int64      `json:"version"`
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// ProviderListEntry 是列表视图: 配置 + 本配额日用量。
type ProviderListEntry struct {
	ProviderConfigView
	// TodayUsed 的量纲随 QuotaKind 变化: 按次计费返回调用次数，
	// token 计费返回 token 数。一律返回 token 口径会让按次 provider
	// 的已用量恒为 0，界面上看起来永远空闲。
	TodayUsed     int64 `json:"today_used"`
	TodayRequests int64 `json:"today_requests"`
	// CredentialPresent 只回布尔，永不回值。
	CredentialPresent bool `json:"credential_present"`
	HasTraffic        bool `json:"has_traffic"`
}

// ProviderVersionView 是一条配置版本历史。
type ProviderVersionView struct {
	ID             int64     `json:"id"`
	ProviderName   string    `json:"provider_name"`
	Action         string    `json:"action"`
	ChangedFields  []string  `json:"changed_fields"`
	Reason         string    `json:"reason"`
	RolledBackFrom *int64    `json:"rolled_back_from,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	CreatedBy      string    `json:"created_by"`
	// Snapshot 只在读取单个版本时填充，列表接口留空以免响应膨胀。
	Snapshot *ProviderConfigView `json:"snapshot,omitempty"`
}

// ProviderFieldDiff 是单个字段的变更。
//
// dry-run 的预演结果与跨量纲回滚被拒时的原因说明共用这一个结构:
// 两端讲的是同一件事，拆成两套只会让前端各写一份解析，其中一份迟早漂移。
type ProviderFieldDiff struct {
	Field  string `json:"field"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// ProviderWriteInput 是一次 provider 配置写入的入参。
type ProviderWriteInput struct {
	Config ProviderConfigView
	Action string
	Reason string
	Actor  string
	// ExpectedVersion 非 nil 时启用乐观并发控制。
	ExpectedVersion *int64
}

// ProviderConfigStore 是 gateway 对 provider 配置持久化的依赖。
//
// 单独成接口而不并入 deps.go 的 Store: 那个接口是请求热路径的依赖，
// 每个实现方（含测试 fake）都必须实现全部方法。配置管理是低频管理面，
// 把它塞进去会让所有只关心热路径的 fake 平白多实现 9 个方法。
// Server 在处理配置端点时对 s.store 做一次类型断言，未实现即返回 501。
type ProviderConfigStore interface {
	// ListProvidersWithUsage 返回当前生效态 + 本配额日用量与请求数。
	ListProvidersWithUsage(ctx context.Context, now time.Time) ([]ProviderListEntry, error)
	// GetProviderConfig 读取单个 provider。不存在时返回 ErrProviderNotFound。
	GetProviderConfig(ctx context.Context, name string) (ProviderConfigView, error)
	// ListProviderConfigs 返回全部 provider，供构造候选配置做整体校验。
	ListProviderConfigs(ctx context.Context, includeDeleted bool) ([]ProviderConfigView, error)
	// GetProviderPeakUsage 返回近 days 个配额日内单 Key 单日的最高用量。
	GetProviderPeakUsage(ctx context.Context, name string, days int, now time.Time) (int64, error)
	// ProviderHasTraffic 报告该 provider 是否有过流量。
	ProviderHasTraffic(ctx context.Context, name string) (bool, error)

	CreateProvider(ctx context.Context, in ProviderWriteInput) (int64, error)
	UpdateProvider(ctx context.Context, in ProviderWriteInput) (int64, error)
	DeleteProvider(ctx context.Context, name, reason, actor string, expectedVersion *int64) (int64, error)
	// RollbackProvider 以目标版本的快照创建新版本，版本号继续递增。
	RollbackProvider(ctx context.Context, name string, targetVersionID int64, reason, actor string, expectedVersion *int64) (int64, ProviderConfigView, error)

	ListProviderVersions(ctx context.Context, name string, limit int, before int64) ([]ProviderVersionView, error)
	GetProviderVersion(ctx context.Context, id int64) (ProviderVersionView, error)

	// DiffProviderConfigs 返回两份配置的字段级差异。
	//
	// 这是个纯函数，放在存储端口上是刻意的: dry-run 给运维看的差异、
	// 跨量纲回滚被拒时给出的差异、以及落进 config_versions.changed_fields
	// 的字段清单，三者必须来自同一次计算。在 gateway 侧另写一份实现，
	// 迟早会出现「预演说改了 3 个字段、历史记录里只有 2 个」，
	// 而运维无从判断哪一份是真的。
	DiffProviderConfigs(before, after ProviderConfigView) []ProviderFieldDiff
}

// ProviderReloader 触发配置热加载。
//
// 挂在存储适配器上而非 Server 字段上是权衡的结果: 热加载要读 DB 装配
// 快照，storeAdapter 是装配层唯一同时握有存储与快照持有者的对象，
// 而 gateway 侧只需要「触发一次并拿到新版本号」这一个动作。
// 未实现时配置写操作仍然成功，只是响应里 reloaded=false ——
// 配置已落库、进程仍用旧快照，这个不一致必须让运维看见。
type ProviderReloader interface {
	// ReloadProviderConfig 从 DB 重新装配配置快照，返回生效的版本号。
	ReloadProviderConfig(ctx context.Context) (int64, error)
}

// provider 配置管理的错误。
var (
	// ErrProviderNotFound 对外映射为 404。
	ErrProviderNotFound = errors.New("gateway: provider 不存在")
	// ErrProviderExists 对外映射为 409。
	ErrProviderExists = errors.New("gateway: provider 已存在")
	// ErrProviderVersionConflict 对外映射为 409，语义是「刷新后重试」。
	ErrProviderVersionConflict = errors.New("gateway: provider 配置版本冲突")
	// ErrProviderQuotaKindImmutable 对外映射为 422 而非 409。
	//
	// 与版本冲突分开是必需的: 409 的语义是「别人先改了，刷新重试即可」，
	// 而跨量纲变更是永远不会成功的操作 —— 合并成同一个码，运维会反复
	// 重试一个注定失败的动作，且每次都得到同一句「冲突」。
	ErrProviderQuotaKindImmutable = errors.New("gateway: quota_kind 不可修改")
	// ErrProviderNameImmutable 对外映射为 400。
	ErrProviderNameImmutable = errors.New("gateway: provider 名不可修改")
)

// providerNamePattern 与 schema.sql 的 CHECK 约束一致。
//
// 两处都校验不是冗余: DB 约束的报错只有列名，定位不到是哪个请求；
// 而只在 API 校验的话，任何绕过 API 的写入（迁移脚本、手工 SQL）都能
// 塞进一个非法名，而非法名会让 Redis 配额 key 出现意料之外的分隔符。
var providerNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// providerCheck 是一条校验结果。
type providerCheck struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// validateProviderInput 校验单个 provider 配置。
//
// **dry-run 与真实提交必须调用同一个函数** —— 两套校验等于没有 dry-run:
// 预演通过而提交失败会让运维彻底不再相信预演，预演通过而提交也通过、
// 但两者放行的条件不同，则会放进一份预演从未检查过的配置。
//
// failures 是阻断项，warnings 是需要运维知晓但不阻断的风险项。
//
// 这里是 provider 字段校验的**唯一**来源，别指望 config.Config.Validate()
// 兜底: 那个函数（config.go:897-1008）只校验 server / redis / egress /
// scheduler / refresh，全程不读 c.Providers 的任何字段。把它串进写路径会得到
// 一道永远通过的门禁 —— 比没有门禁更糟，因为它看起来像有。
func validateProviderInput(p ProviderConfigView, isCreate bool) (failures, warnings []providerCheck) {
	fail := func(field, code, msg string) {
		failures = append(failures, providerCheck{Field: field, Code: code, Message: msg})
	}
	warn := func(field, code, msg string) {
		warnings = append(warnings, providerCheck{Field: field, Code: code, Message: msg})
	}

	if !providerNamePattern.MatchString(p.Name) {
		fail("name", "invalid_name",
			"provider 名需匹配 ^[a-z][a-z0-9_]{0,31}$。该名字会进入 Redis 配额 key 前缀与归档维度，不允许其他字符")
	}

	switch p.QuotaKind {
	case "token", "count":
	default:
		fail("quota_kind", "invalid_quota_kind", "quota_kind 只能是 token 或 count")
	}

	if p.QuotaLimit <= 0 {
		fail("quota_limit", "invalid_quota_limit", "quota_limit 必须大于 0")
	}

	// 周期为 0 或负数会让配额分桶键恒定，额度永远不刷新且不报错。
	if p.QuotaWindow <= 0 {
		fail("quota_window_nanos", "invalid_quota_window",
			"quota_window_nanos 必须大于 0（纳秒）。为 0 会让配额分桶键恒定、额度永不刷新，且全程无任何报错")
	}

	if p.RefreshHour != nil && (*p.RefreshHour < 0 || *p.RefreshHour > 23) {
		fail("refresh_hour", "invalid_refresh_hour",
			"refresh_hour 需在 0-23 之间，或省略/传 null 表示无固定刷新点")
	}

	switch u, err := url.Parse(p.BaseURL); {
	case p.BaseURL == "":
		fail("base_url", "invalid_base_url", "base_url 不能为空")
	case err != nil || u.Host == "":
		fail("base_url", "invalid_base_url", "base_url 不是合法的绝对 URL")
	case u.Scheme == "http":
		// 不阻断: 本地与 CI 的假上游、内网自建上游都走 http，
		// 硬拒会让这些环境根本起不来。但明文传输上游凭据的风险要说出来。
		warn("base_url", "insecure_scheme",
			"base_url 使用 http，上游凭据将以明文传输。生产环境应使用 https")
	case u.Scheme != "https":
		fail("base_url", "invalid_base_url", "base_url 的 scheme 只能是 https 或 http")
	}

	for pub, up := range p.ModelMapping {
		if pub == "" {
			fail("model_mapping", "empty_public_model", "model_mapping 的对外模型名不能为空")
		}
		if up == "" {
			fail("model_mapping."+pub, "empty_upstream_model",
				"上游模型名不能为空。空值会让该模型的请求原样透传对外名，上游必然返回模型不存在")
		}
	}

	// count_models / reasoning_models 里的名字既可以是对外名也可以是上游名
	// （config.IsCountModel 两侧都匹配），且按次计费模型常常不改名、
	// 因此本就不会出现在 model_mapping 里。所以这里不能阻断，
	// 只在两侧都对不上时提醒 —— 对不上的名字永远匹配不到，是个静默的空配置。
	known := make(map[string]bool, len(p.ModelMapping)*2)
	for pub, up := range p.ModelMapping {
		known[pub] = true
		known[up] = true
	}
	for _, m := range p.CountModels {
		if m != "" && !known[m] {
			warn("count_models", "model_not_in_mapping",
				"count_models 中的 "+m+" 未出现在 model_mapping 的任何一侧，若它也不是可直接调用的上游名，这条配置永远不会被匹配到")
		}
	}
	for _, m := range p.ReasoningModels {
		if m != "" && !known[m] {
			warn("reasoning_models", "model_not_in_mapping",
				"reasoning_models 中的 "+m+" 未出现在 model_mapping 的任何一侧。推理模型按子串匹配，若确为版本前缀可忽略此提示")
		}
	}

	// 凭据变量名缺失只告警不阻断: 允许运维先建好配置、再补环境变量与 Key，
	// 这是新增 provider 的常规顺序。但 enabled 的 provider 没有凭据会让
	// 所有派往它的请求 401，而运维只会看到「配置保存成功」，必须点出来。
	switch {
	case p.CredentialEnv == "":
		if p.Enabled {
			warn("credential_env", "credential_env_missing",
				"未指定凭据环境变量名，若上游需要鉴权，启用后所有请求都会 401")
		}
	case os.Getenv(p.CredentialEnv) == "":
		warn("credential_env", "credential_absent",
			"环境变量 "+p.CredentialEnv+" 在当前进程中不存在或为空，启用后所有请求都会 401")
	}

	if isCreate && p.Enabled {
		warn("enabled", "enabled_on_create",
			"新建即启用会让配置一保存就立刻承接流量。建议先以 enabled=false 建好、验证凭据与 model_mapping 后再启用")
	}

	return failures, warnings
}
