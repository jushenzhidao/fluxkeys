package gateway

import "github.com/fluxkeys/fluxkeys/internal/gateway/adminapi"

// 本文件把已迁到 internal/gateway/adminapi 的符号在本包重新导出。
//
// 全部用**类型别名**（`=`）而非新类型定义: 别名与目标类型是同一个类型，
// 不是副本。因此:
//  1. 既有调用点（cmd/gateway 的装配层、各测试）完全无需改动；
//  2. 本包接口与 adminapi 接口之间的方法签名仍然逐字相等，可相互赋值 ——
//     这正是 *Server 能满足 adminapi.Deps 的前提（新类型定义做不到这一点，
//     那会让两边的签名变成不同类型，赋值处全部报错）。
//
// 之所以不把定义留在本包再让 adminapi 反向引用: 那会形成导入环。这些类型
// 全部只服务于管理接口的请求与响应，因此定义在 adminapi 才是正确的方向。
type (
	// KeyState 是单个 Key 的对外状态视图，供 GET /admin/keys 使用。
	KeyState = adminapi.KeyState

	// KeyPatch 是一次 Key 元数据局部更新的入参。
	KeyPatch = adminapi.KeyPatch

	// KeyPatchResult 是局部更新的新旧值对照。
	KeyPatchResult = adminapi.KeyPatchResult

	// NewUser 是创建用户的入参。
	NewUser = adminapi.NewUser

	// NewUpstreamKey 是导入一个上游 Key 的入参。
	NewUpstreamKey = adminapi.NewUpstreamKey

	// IssuedKey 是一条新签发的用户 API Key 的元信息（不含明文）。
	IssuedKey = adminapi.IssuedKey

	// ProviderConfigView 是一个 provider 的配置视图。
	ProviderConfigView = adminapi.ProviderConfigView

	// ProviderListEntry 是 provider 列表视图：配置 + 本配额日用量。
	ProviderListEntry = adminapi.ProviderListEntry

	// ProviderVersionView 是一条 provider 配置版本历史。
	ProviderVersionView = adminapi.ProviderVersionView

	// ProviderFieldDiff 是单个字段的变更。
	ProviderFieldDiff = adminapi.ProviderFieldDiff

	// ProviderWriteInput 是一次 provider 配置写入的入参。
	ProviderWriteInput = adminapi.ProviderWriteInput

	// ProviderConfigStore 是 provider 配置持久化的依赖端口。
	ProviderConfigStore = adminapi.ProviderConfigStore

	// ProviderReloader 触发配置热加载。
	ProviderReloader = adminapi.ProviderReloader
)

// 错误哨兵同样必须复用同一批值而不是各建一份: 调用方靠 errors.Is 判定，
// 两份取值不同的哨兵会让「不存在」被误判成「前置条件不满足」。
var (
	// ErrKeyNotFound 表示目标 Key 不存在，对外映射为 404。
	ErrKeyNotFound = adminapi.ErrKeyNotFound

	// ErrPreconditionFailed 表示 Key 存在但不满足前置条件，对外映射为 409。
	ErrPreconditionFailed = adminapi.ErrPreconditionFailed

	// ErrProviderNotFound 对外映射为 404。
	ErrProviderNotFound = adminapi.ErrProviderNotFound

	// ErrProviderExists 对外映射为 409。
	ErrProviderExists = adminapi.ErrProviderExists

	// ErrProviderVersionConflict 对外映射为 409，语义是「刷新后重试」。
	ErrProviderVersionConflict = adminapi.ErrProviderVersionConflict

	// ErrProviderQuotaKindImmutable 对外映射为 422 而非 409。
	ErrProviderQuotaKindImmutable = adminapi.ErrProviderQuotaKindImmutable

	// ErrProviderNameImmutable 对外映射为 400。
	ErrProviderNameImmutable = adminapi.ErrProviderNameImmutable
)
