// Package adminapi 是管理面 HTTP 接口：Key 元数据、用户与 Key 签发、
// provider 配置及其版本历史。
//
// 单独成包的理由: 管理面占原 gateway 包生产代码的约 44%（2075 行），
// 而它与请求热路径的耦合只有「读同一批配置与存储」这一层 —— 热路径不认识
// 管理面，管理面却要认识热路径的全部内部状态。把管理面留在大包里，
// 结果是任何一次热路径重构都要通读管理代码才能确认影响面。
//
// 依赖方向是单向的: 本包不 import internal/gateway。所需能力由 Deps 显式声明
// （沿用原 deps.go「消费方定义接口」的约定），由装配层实现。
// 因此本包可以脱离网关主体单独测试。
package adminapi

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/fluxkeys/fluxkeys/internal/confsnap"
	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/metrics"
)

// Store 是管理面真正用到的那部分持久化能力。
//
// 刻意只声明本包用到的 7 个方法，而不是复用热路径的 Store 接口（11 个方法，
// 含鉴权与用量流水写入 —— 管理面一个都不用）。接口窄了，实现方与测试替身
// 都不必为管理面实现无关能力。
//
// 热路径的 Store 接口是本接口的超集，故同一个实现对象可同时满足两者。
type Store interface {
	// Audit 写入管理操作审计日志。
	Audit(ctx context.Context, actor, action, target string, detail map[string]any) error

	// UpsertUpstreamKey 导入或更新一个上游 Key。created 表示本次是新建。
	UpsertUpstreamKey(ctx context.Context, in NewUpstreamKey) (created bool, err error)

	// RevokeUserAPIKey 吊销一条 API Key，带归属校验。
	RevokeUserAPIKey(ctx context.Context, userID, keyID int64) error

	// PatchUpstreamKeyState 原子地局部更新 Key 的 status / pool / persona_id。
	//
	// 三种返回: 成功 (结果, nil)；key_id 不存在 (nil, ErrKeyNotFound)；
	// 存在但前置条件不满足 (当前值快照, ErrPreconditionFailed)。
	PatchUpstreamKeyState(ctx context.Context, keyID string, p KeyPatch) (*KeyPatchResult, error)

	// AssignShard 批量指派 Key 的机器归属，返回改动行数。
	AssignShard(ctx context.Context, shard string, keyIDs []string) (int64, error)

	// SetVolcKeyEgressIP 记录 Key 当前绑定的出口 IP。
	//
	// 只应由真正改变了内存绑定关系的路径调用（Rebind / Migrate / 首次分配），
	// 且必须在改完内存后立刻落库 —— 否则重启后 restoreBindings 读到旧值，
	// 会把 Key 换回原来的出口，等于凭空制造一次「老账号换了 IP」。
	SetVolcKeyEgressIP(ctx context.Context, keyID, egressIP string) error

	// CreateUserAPIKey 为用户签发 API Key。plaintext 仅此一次可见。
	CreateUserAPIKey(ctx context.Context, userID int64, name string) (plaintext string, key IssuedKey, err error)
}

// Scheduler 是管理面用到的调度器能力。
//
// 只取 3 个方法: 管理面要的是「改完库之后让内存立刻跟上」以及「读出当前
// 状态」。选路、打分的其余能力属于热路径，管理面不碰。
type Scheduler interface {
	// SetKeyStatus 显式设置某 Key 在调度器内存中的状态。
	//
	// 管理接口改完库必须再调它，不能指望下一次 Reload 生效: healthTable.seed
	// 对已存在的 Key 不覆盖（「已有实时观测值，不被库中的旧快照覆盖」），
	// 所以只写库的封禁会在内存里完全不生效，被封的 Key 继续承接流量。
	//
	// status 取存储层的状态字面量（active/cooldown/banned/invalid）。
	SetKeyStatus(keyID, status string)

	// KeyStates 返回全部 Key 的状态快照。
	KeyStates(ctx context.Context) ([]KeyState, error)

	// Reload 重新从存储装载 Key 池。
	Reload(ctx context.Context) error
}

// Deps 是本包对装配层的全部依赖。
//
// 用具名访问器而非直接传具体类型: 本包不该知道网关主体把状态存在哪些字段里，
// 也不该被允许修改它们。装配层只需实现这 9 个成员。
type Deps interface {
	Log() *slog.Logger
	Store() Store
	Scheduler() Scheduler
	Egress() *egress.Pool
	Metrics() *metrics.Metrics
	Snaps() *confsnap.Holder

	// AdminChain 给管理端点套上「请求 ID → 管理鉴权」中间件。
	//
	// 由装配层提供而不是本包自己拼: 鉴权密钥、请求 ID 的生成与响应头
	// 都属于网关主体的职责，本包只声明「需要一个把 handler 包起来的中间件」。
	AdminChain(http.HandlerFunc) http.Handler

	// InvalidateAuthCache 清空鉴权缓存。
	//
	// 吊销用户 Key 后必须调用，否则 TTL 内的旧鉴权结果仍会放行 ——
	// 用户看到的是「已经删了但还能用」。
	InvalidateAuthCache()

	// CreateUser 创建用户，返回回填了默认限额的结果。
	//
	// 之所以是动作而不是 Store 上的方法: 返回值的类型是网关主体的鉴权上下文
	// （UserContext），把它拉进本包会让核心类型反向依赖管理面。装配层在此
	// 做一次字段搬运即可。
	//
	// 返回值必须是存储层回填后的值: 调用方没传限额时存储层会填默认值，
	// 回显请求值会让调用方以为「不限」而实际上有默认上限。
	CreateUser(ctx context.Context, in NewUser) (CreatedUser, error)
}

// CreatedUser 是创建用户后回填了默认限额的结果。
type CreatedUser struct {
	UserID          int64
	Name            string
	RPMLimit        int
	TPMLimit        int64
	DailyTokenLimit int64
}
