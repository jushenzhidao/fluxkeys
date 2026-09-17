package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/confsnap"
	"github.com/fluxkeys/fluxkeys/internal/gateway"
	"github.com/fluxkeys/fluxkeys/internal/quota"
	"github.com/fluxkeys/fluxkeys/internal/scheduler"
	"github.com/fluxkeys/fluxkeys/internal/store"
)

// 本文件是装配层的适配器。
//
// internal/gateway 用「消费方定义接口」的方式声明依赖，internal/scheduler 与
// internal/store 则按自己的领域语义定义签名。两侧都不引用对方，差异在这里
// 一次性桥接。
//
// 这么做而不是让某一侧去迁就另一侧，是因为两侧的抽象层级本就不同:
// gateway 关心「拿一个能用的 Key」，scheduler 关心「打分与加权随机」；
// gateway 关心「鉴权失败」，store 关心「记录不存在 / 已吊销 / 用户停用」。
// 强行统一会把领域概念泄漏到对面。适配器的代价是几十行转换，收益是两个包
// 各自的测试都不需要引入对方。

// ===== 调度器适配 =====

// schedulerAdapter 把 scheduler.Scheduler 适配为 gateway.Scheduler。
type schedulerAdapter struct {
	sched *scheduler.Scheduler
	st    *store.Store
	qm    *quota.Manager
	cfg   quotaLimits
}

// quotaLimits 按 provider 提供构造 KeyState 时需要的水位。
//
// 按 provider 取值而非两个标量: 水位本身是 provider 维度的，
// 单一标量必然让 GET /admin/keys 对所有上游报同一个上限 —— 运维会据此
// 得出错误的余量结论（额度小的上游看起来还剩很多，实际已接近耗尽）。
//
// 持有 Holder 而非 *config.Config: 这是进程级长生命周期对象，直接抓一份
// config 指针等于把启动时的水位钉死。热切后管理页面会继续按旧上限算余量，
// 而这个接口正是运维判断「要不要加 Key」的依据。
type quotaLimits struct {
	snaps *confsnap.Holder
}

// current 取一次快照，供一次逻辑操作全程使用。
func (q quotaLimits) current() *confsnap.Snapshot {
	if q.snaps == nil {
		return nil
	}
	return q.snaps.Current()
}

// hardFor 返回该 provider 在该配额类型下的硬水位。
//
// 显式收 snap 而不在内部取: KeyStates 要为上千个 Key 报水位，逐 Key 取快照
// 会让同一张表里前后两行按不同配置计算 —— 运维看到的是一份自相矛盾的报表，
// 却没有任何迹象表明发生过配置切换。
func (q quotaLimits) hardFor(snap *confsnap.Snapshot, provider string, kindCount bool) int64 {
	if snap == nil {
		return 0
	}
	hard, _ := snap.Cfg.LimitsFor(provider, kindCount)
	return hard
}

func (a *schedulerAdapter) Select(ctx context.Context, req gateway.SelectRequest) (*gateway.Candidate, error) {
	cand, err := a.sched.Select(ctx, scheduler.Request{
		Provider: req.Provider,
		Model:    req.Model,
		Kind:     req.Kind,
		Exclude:  req.Exclude,
	})
	if err != nil {
		// 把 scheduler.ErrNoCandidate 翻译为 gateway.ErrNoCandidate。
		// gateway 据此返回 503 service_busy 而非 500 —— 无可用 Key 是
		// 容量问题，500 会让上游告警系统误判为程序缺陷。
		if errors.Is(err, scheduler.ErrNoCandidate) {
			return nil, fmt.Errorf("%w: %w", gateway.ErrNoCandidate, err)
		}
		return nil, err
	}
	return &gateway.Candidate{
		KeyID:    cand.KeyID,
		Secret:   cand.Secret,
		EgressIP: cand.EgressIP,
		Pool:     cand.Pool,
		Kind:     req.Kind,
	}, nil
}

func (a *schedulerAdapter) MarkSuccess(keyID string) { a.sched.MarkSuccess(keyID) }

// SetKeyStatus 把管理接口改过的状态同步进调度器内存。
//
// 由 PATCH /admin/keys/{key_id} 在写库成功后调用。不能省: healthTable.seed
// 对已存在的 Key 不覆盖，只写库的封禁在内存里完全不生效 —— 被封的 Key 会
// 继续被 Select 选中并承接流量，而库里显示 banned，两侧长期不一致。
func (a *schedulerAdapter) SetKeyStatus(keyID, status string) {
	a.sched.SetKeyStatus(keyID, scheduler.KeyStatus(status))
}

// Reload 让调度器立即重载 Key 池。
//
// 由 Key 导入接口在写库成功后调用，避免新部署要干等一轮 key_reload
// （5 分钟）才能开始服务。
func (a *schedulerAdapter) Reload(ctx context.Context) error { return a.sched.Reload(ctx) }

// RefreshKey 让调度器把单个 Key 的活跃池归属与库中现状对齐。
//
// 由 PATCH /admin/keys/{key_id} 在写库成功后调用（KI-034）。与 SetKeyStatus
// 的分工: SetKeyStatus 改 health（封禁/复活的状态机），RefreshKey 改活跃池
// 归属（把复活的 Key 拉回选择集）。两者都要，缺一个都会留下一种不一致。
func (a *schedulerAdapter) RefreshKey(ctx context.Context, keyID string) error {
	return a.sched.RefreshKey(ctx, keyID)
}

func (a *schedulerAdapter) MarkFailure(keyID string, kind gateway.FailureKind) {
	a.sched.MarkFailure(keyID, mapFailureKind(kind))
}

// mapFailureKind 把 gateway 的失败分类映射到调度器的状态机事件。
//
// 两套枚举没有合并成一套，是因为它们的语义边界不同: gateway 从 HTTP 层看到
// 的是「上游返回了什么」，调度器关心的是「该 Key 应当进入什么状态」。
// 目前是一一对应，但比如未来「网关自身构造请求失败」不应算 Key 失败，
// 差异就会出现在这里而不需要改两个包。
func mapFailureKind(k gateway.FailureKind) scheduler.FailureKind {
	switch k {
	case gateway.FailureAuth:
		return scheduler.FailureAuth
	case gateway.FailureRateLimit:
		return scheduler.Failure429
	case gateway.FailureQuota:
		return scheduler.FailureQuota
	case gateway.FailureNetwork:
		return scheduler.FailureTimeout
	default:
		return scheduler.Failure5xx
	}
}

// KeyStates 组装 Key 状态快照，供 GET /admin/keys 使用。
//
// 数据来自三处: Postgres 的 Key 元数据、调度器内存中的健康分、Redis 的配额
// 水位。不缓存 —— 这是低频管理接口，实时性比性能重要。
func (a *schedulerAdapter) KeyStates(ctx context.Context) ([]gateway.KeyState, error) {
	// WithSecret 明确为 false: 管理接口只展示状态，没有任何理由把 1000 个
	// Key 的明文密钥解密进内存。
	keys, err := a.st.ListUpstreamKeys(ctx, store.UpstreamKeyFilter{})
	if err != nil {
		return nil, fmt.Errorf("读取 Key 列表: %w", err)
	}

	health := a.sched.HealthAll()

	// 整张表共用一份快照，理由见 quotaLimits.hardFor。
	snap := a.cfg.current()

	// 按 provider 分组 key IDs
	keysByProvider := make(map[string][]string)
	for _, k := range keys {
		keysByProvider[k.Provider] = append(keysByProvider[k.Provider], k.KeyID)
	}

	// 配额读取失败不阻断整个接口: 状态与健康分仍有诊断价值，
	// 水位显示为 0 好过整个管理接口不可用。
	tokenSnaps := make(map[string]quota.Snapshot)
	countSnaps := make(map[string]quota.Snapshot)

	for provider, ids := range keysByProvider {
		if ts, err := a.qm.GetMany(ctx, provider, ids, quota.KindToken); err == nil {
			for k, v := range ts {
				tokenSnaps[k] = v
			}
		}
		if cs, err := a.qm.GetMany(ctx, provider, ids, quota.KindCount); err == nil {
			for k, v := range cs {
				countSnaps[k] = v
			}
		}
	}

	out := make([]gateway.KeyState, 0, len(keys))
	for _, k := range keys {
		st := gateway.KeyState{
			KeyID:      k.KeyID,
			Provider:   k.Provider,
			Status:     k.Status,
			Pool:       k.Pool,
			EgressIP:   k.EgressIP,
			PersonaID:  k.PersonaID,
			TokenLimit: a.cfg.hardFor(snap, k.Provider, false),
			CountLimit: a.cfg.hardFor(snap, k.Provider, true),
		}
		// 健康分优先取调度器内存值: Postgres 里的 health_score 是周期落盘的，
		// 落后于内存状态，而排障时最需要的恰恰是「此刻」的健康分。
		if h, ok := health[k.KeyID]; ok {
			st.HealthScore = h.Score
			st.Status = string(h.Status)
			st.LastUsedAt = h.LastSelectedAt
		} else {
			st.HealthScore = k.HealthScore
		}
		if k.LastUsedAt != nil && st.LastUsedAt.IsZero() {
			st.LastUsedAt = *k.LastUsedAt
		}
		if s, ok := tokenSnaps[k.KeyID]; ok {
			st.TokenUsed = s.Used + s.Prededuct
			if s.Hard > 0 {
				st.TokenLimit = s.Hard
			}
		}
		if s, ok := countSnaps[k.KeyID]; ok {
			st.CountUsed = s.Used + s.Prededuct
			if s.Hard > 0 {
				st.CountLimit = s.Hard
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// ===== 存储适配 =====

// storeAdapter 把 store.Store 适配为 gateway.Store。
//
// 后三个字段是为 adminapi.ProviderReloader 准备的：热加载要「读库 → 装配快照 →
// 换入」，而装配层里同时握有存储、快照持有者与常驻协程的只有这里。
// 它们全为空时 ReloadProviderConfig 返回 errReloadUnavailable —— 这是刻意保留的
// **降级路径**（未装配就如实说不支持），而不是崩溃或静默 no-op。
type storeAdapter struct {
	st *store.Store
	// snaps 是运行期热配置的唯一来源，换入即生效。
	snaps *confsnap.Holder
	// base 是启动期配置。热加载只替换 providers 段，其余冷配置原样继承。
	base *config.Config
	// afterSwap 在快照换入之后调用，把依赖快照的常驻协程（刷新探测器）对齐到新配置。
	afterSwap func(context.Context) error
	log       *slog.Logger
}

func (a *storeAdapter) AuthenticateUserKey(ctx context.Context, plaintext string) (*gateway.UserContext, error) {
	ac, err := a.st.AuthenticateUserKey(ctx, plaintext)
	if err != nil {
		// 三种失败原因（不存在 / 已吊销 / 用户停用）统一收敛为
		// ErrUnauthorized。区分它们会向调用方泄漏「这个 Key 曾经存在」，
		// 对撞库者是有价值的信息。原因保留在 wrap 里供服务端日志使用。
		if errors.Is(err, store.ErrNotFound) ||
			errors.Is(err, store.ErrKeyRevoked) ||
			errors.Is(err, store.ErrUserSuspended) {
			return nil, fmt.Errorf("%w: %w", gateway.ErrUnauthorized, err)
		}
		return nil, err
	}
	return &gateway.UserContext{
		UserID:          ac.UserID,
		APIKeyID:        ac.KeyID,
		Name:            ac.UserName,
		RPMLimit:        ac.RPMLimit,
		TPMLimit:        ac.TPMLimit,
		DailyTokenLimit: ac.DailyTokenLimit,
	}, nil
}

func (a *storeAdapter) RecordUsage(ctx context.Context, r gateway.UsageRecord) error {
	return a.st.InsertUsageRecord(ctx, store.UsageRecord{
		RequestID:        r.RequestID,
		UserID:           r.UserID,
		UserAPIKeyID:     r.UserAPIKeyID,
		UpstreamKeyID:    r.UpstreamKeyID,
		EgressIP:         r.EgressIP,
		Provider:         r.Provider,
		Model:            r.Model,
		BillingKind:      r.BillingKind,
		QuotaDay:         r.QuotaDay,
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		ReasoningTokens:  r.ReasoningTokens,
		TotalTokens:      r.TotalTokens,
		CountUnits:       int(r.CountUnits),
		EstimatedTokens:  r.EstimatedTokens,
		StatusCode:       r.StatusCode,
		IsStream:         r.IsStream,
		ErrorCode:        r.ErrorCode,
		RetryCount:       r.RetryCount,
		LatencyMS:        r.LatencyMS,
	})
}

// TouchUserAPIKey 满足 gateway 的 keyToucher 可选接口。
//
// 网关只在鉴权缓存回源（miss）时调用，写频率被钳到每 Key 每 TTL 一次，
// last_used_at 由此从「恒为 NULL」变成可用的活跃度信号。
func (a *storeAdapter) TouchUserAPIKey(ctx context.Context, keyID int64) error {
	return a.st.TouchUserAPIKey(ctx, keyID)
}

func (a *storeAdapter) Audit(ctx context.Context, actor, action, target string, detail map[string]any) error {
	return a.st.InsertAuditLog(ctx, store.AuditLog{
		Actor: actor, Action: action, Target: target, Detail: detail,
	})
}

func (a *storeAdapter) CreateUser(ctx context.Context, in gateway.NewUser) (*gateway.UserContext, error) {
	u, err := a.st.CreateUser(ctx, &store.User{
		Name:            in.Name,
		Email:           in.Email,
		RPMLimit:        in.RPMLimit,
		TPMLimit:        in.TPMLimit,
		DailyTokenLimit: in.DailyTokenLimit,
	})
	if err != nil {
		return nil, err
	}
	return &gateway.UserContext{
		UserID:          u.ID,
		Name:            u.Name,
		RPMLimit:        u.RPMLimit,
		TPMLimit:        u.TPMLimit,
		DailyTokenLimit: u.DailyTokenLimit,
	}, nil
}

func (a *storeAdapter) CreateUserAPIKey(ctx context.Context, userID int64, name string) (string, gateway.IssuedKey, error) {
	plaintext, rec, err := a.st.CreateUserAPIKey(ctx, userID, name)
	if err != nil {
		return "", gateway.IssuedKey{}, err
	}
	return plaintext, gateway.IssuedKey{ID: rec.ID, Prefix: rec.KeyPrefix}, nil
}

// RevokeUserAPIKey 吊销用户 API Key，store.ErrNotFound 翻译为 gateway.ErrKeyNotFound。
func (a *storeAdapter) RevokeUserAPIKey(ctx context.Context, userID, keyID int64) error {
	if err := a.st.RevokeUserAPIKey(ctx, userID, keyID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: %w", gateway.ErrKeyNotFound, err)
		}
		return err
	}
	return nil
}

// AssignShard 批量指派 Key 的机器归属，签名一致直接透传。
func (a *storeAdapter) AssignShard(ctx context.Context, shard string, keyIDs []string) (int64, error) {
	return a.st.AssignShard(ctx, shard, keyIDs)
}

// UpsertUpstreamKey 导入或更新一个上游 Key。
//
// created 的判定用 CreatedAt.Equal(UpdatedAt): schema 里两列的 DEFAULT 都是
// now()，而 PostgreSQL 的 now() 在同一事务内返回同一时刻，所以新插入的行
// 两值严格相等；ON CONFLICT 分支显式写了 updated_at = now()，会把 UpdatedAt
// 推到晚于 CreatedAt。
//
// 这比「先 SELECT 再判断」可靠: 后者在并发导入下有 TOCTOU 窗口，两个请求
// 会同时报告 created。也比让 store 层加 RETURNING xmax = 0 更克制 —— 那是
// 依赖 PostgreSQL 内部事务 ID 的技巧，换存储引擎即失效。
func (a *storeAdapter) UpsertUpstreamKey(ctx context.Context, in gateway.NewUpstreamKey) (bool, error) {
	k, err := a.st.UpsertUpstreamKey(ctx, &store.UpstreamKey{
		Provider:  in.Provider,
		KeyID:     in.KeyID,
		Secret:    in.Secret,
		Pool:      in.Pool,
		Status:    in.Status,
		PersonaID: in.PersonaID,
		EgressIP:  in.EgressIP,
	})
	if err != nil {
		return false, err
	}
	return k.CreatedAt.Equal(k.UpdatedAt), nil
}

// PatchUpstreamKeyState 局部更新 Key 元数据。
//
// 这层只做类型转换与错误哨兵翻译，条件判定全在 store 的单条 SQL 里完成 ——
// 在适配层「先查再改」会留下 TOCTOU 窗口，让 expected_status 这类乐观并发
// 控制在并发下失效。
func (a *storeAdapter) PatchUpstreamKeyState(ctx context.Context, keyID string, p gateway.KeyPatch) (*gateway.KeyPatchResult, error) {
	res, err := a.st.PatchUpstreamKeyState(ctx, keyID, store.UpstreamKeyPatch{
		Status:           p.Status,
		Pool:             p.Pool,
		PersonaID:        p.PersonaID,
		ExpectedStatus:   p.ExpectedStatus,
		RejectStatusFrom: p.RejectStatusFrom,
	})

	var out *gateway.KeyPatchResult
	if res != nil {
		out = &gateway.KeyPatchResult{
			PrevStatus: res.PrevStatus, PrevPool: res.PrevPool, PrevPersonaID: res.PrevPersonaID,
			NewStatus: res.NewStatus, NewPool: res.NewPool, NewPersonaID: res.NewPersonaID,
		}
	}

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, fmt.Errorf("%w: %w", gateway.ErrKeyNotFound, err)
	case errors.Is(err, store.ErrPreconditionFailed):
		// 结果与错误一并返回: 409 的文案要能写出冲突的实际内容
		// （当前状态是什么），只给错误值会让运维还得再查一次。
		return out, fmt.Errorf("%w: %w", gateway.ErrPreconditionFailed, err)
	case err != nil:
		return nil, err
	}
	return out, nil
}

// SetVolcKeyEgressIP 记录 Key 当前绑定的出口 IP。
//
// 走 UpdateUpstreamKeyState 的单列更新而非 PatchUpstreamKeyState:
// 后者带状态机守卫与乐观并发控制，而出口绑定的落库是对既成事实的存档
// —— 内存里已经换过去了，此处若因状态冲突被拒，只会造成库与内存不一致。
func (a *storeAdapter) SetVolcKeyEgressIP(ctx context.Context, keyID, egressIP string) error {
	return a.st.UpdateUpstreamKeyState(ctx, keyID, store.UpstreamKeyState{EgressIP: &egressIP})
}

// DeleteUpstreamKey 按 key_id 删除上游 Key，委托底层存储。
//
// 出口绑定的回收不在这里做: 它属于「管理面 DELETE 端点」这一调用方的职责，
// 与删行成同一事务。把解绑塞进存储适配层会让「删行成功但解绑失败」的降级
// 无处安放 —— 端点层才能决定「已删的行要不要回滚」。
func (a *storeAdapter) DeleteUpstreamKey(ctx context.Context, keyID string) error {
	return a.st.DeleteUpstreamKey(ctx, keyID)
}

// Ping 检查 Postgres 可达性。
//
// store 未暴露 Ping，直接用底层连接池 —— 用一条真实 SQL 探测会把
// /readyz 变成对表结构的隐式依赖，迁移期间会误报不就绪。
func (a *storeAdapter) Ping(ctx context.Context) error {
	return a.st.Pool().Ping(ctx)
}

// ===== 调度器所需的配额读取适配 =====

// quotaReader 让 quota.Manager 满足 scheduler.QuotaReader。
//
// 签名已经一致，这层存在的意义是让依赖方向显式: scheduler 不导入 quota 的
// 具体实现，只认接口。
type quotaReader struct{ qm *quota.Manager }

func (q quotaReader) GetMany(ctx context.Context, provider string, keyIDs []string, kind quota.Kind) (map[string]quota.Snapshot, error) {
	return q.qm.GetMany(ctx, provider, keyIDs, kind)
}

// ===== 调度器所需的分片作用域存储 =====

// shardScopedStore 把本机分片过滤注入 Key 列表查询，实现 scheduler.Store。
//
// 包装在装配层而非改 scheduler: 调度器不应该知道「多机分片」这个部署概念，
// 它只管「给我一批 Key 我来调度」。分片是装配期决定的装载范围，与打分逻辑
// 正交 —— 混进调度器意味着每个消费 Store 接口的测试都要多一个分片维度。
//
// 只在 filter.Shard 为空时注入: 显式指定分片的调用（如运维工具查别的分片）
// 不被覆盖。shard 为空时本包装是恒等透传，单机部署行为不变。
type shardScopedStore struct {
	st    *store.Store
	shard string
}

func (s shardScopedStore) ListUpstreamKeys(ctx context.Context, f store.UpstreamKeyFilter) ([]store.UpstreamKey, error) {
	if f.Shard == "" {
		f.Shard = s.shard
	}
	return s.st.ListUpstreamKeys(ctx, f)
}

func (s shardScopedStore) GetKeyHistory(ctx context.Context, keyIDs []string, quotaDay time.Time) (map[store.HistoryKey]store.KeyDailyHistory, error) {
	return s.st.GetKeyHistory(ctx, keyIDs, quotaDay)
}

// GetUpstreamKey 按 key_id 读取单个 Key，但只接管本机分片的 Key。
//
// 与 ListUpstreamKeys 同一套分片语义: 单机部署（shard 为空）恒等透传；
// 多机部署下，属于别的分片的 Key 对本机视同不存在（ErrNotFound）。否则
// PATCH 一台机器上的外机 Key 会把它拉进本机活跃池 —— 同一 Key 同时出现在
// 两台机器的选择集里，等于让它从两个出口发请求，正是分片要避免的事。
func (s shardScopedStore) GetUpstreamKey(ctx context.Context, keyID string) (*store.UpstreamKey, error) {
	k, err := s.st.GetUpstreamKey(ctx, keyID)
	if err != nil {
		return nil, err
	}
	if s.shard != "" && k.Shard != s.shard {
		return nil, store.ErrNotFound
	}
	return k, nil
}

// parseClock 解析 "HH:MM" 为自零点起的偏移量。
func parseClock(s string, def time.Duration) time.Duration {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return def
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return def
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
}

// clockStr 是 parseClock 的逆，把偏移量还原成 "HH:MM" 供日志阅读。
//
// 日志里打原始 Duration（如 12h0m0s）会让读日志的人自己去和配置文件里的
// "12:00" 对应，分叉告警的价值就在于一眼看出两个窗口不一样。
func clockStr(d time.Duration) string {
	return fmt.Sprintf("%02d:%02d", int(d.Hours()), int(d.Minutes())%60)
}
