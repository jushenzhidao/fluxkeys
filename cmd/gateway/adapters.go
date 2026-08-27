package main

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// quotaLimits 是构造 KeyState 时需要的水位，避免适配器再依赖整个 config。
type quotaLimits struct {
	TokenHard int64
	CountHard int64
}

func (a *schedulerAdapter) Select(ctx context.Context, req gateway.SelectRequest) (*gateway.Candidate, error) {
	cand, err := a.sched.Select(ctx, scheduler.Request{
		Model:   req.Model,
		Kind:    req.Kind,
		Exclude: req.Exclude,
	})
	if err != nil {
		// 把 scheduler.ErrNoCandidate 翻译为 gateway.ErrNoCandidate。
		// gateway 据此返回 503 service_busy 而非 500 —— 无可用 Key 是
		// 容量问题，500 会让上游告警系统误判为程序缺陷。
		if errors.Is(err, scheduler.ErrNoCandidate) {
			return nil, fmt.Errorf("%w: %v", gateway.ErrNoCandidate, err)
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
	keys, err := a.st.ListVolcKeys(ctx, store.VolcKeyFilter{})
	if err != nil {
		return nil, fmt.Errorf("读取 Key 列表: %w", err)
	}

	health := a.sched.HealthAll()

	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		ids = append(ids, k.KeyID)
	}

	// 配额读取失败不阻断整个接口: 状态与健康分仍有诊断价值，
	// 水位显示为 0 好过整个管理接口不可用。
	tokenSnaps, terr := a.qm.GetMany(ctx, ids, quota.KindToken)
	countSnaps, cerr := a.qm.GetMany(ctx, ids, quota.KindCount)

	out := make([]gateway.KeyState, 0, len(keys))
	for _, k := range keys {
		st := gateway.KeyState{
			KeyID:      k.KeyID,
			Status:     k.Status,
			Pool:       k.Pool,
			EgressIP:   k.EgressIP,
			PersonaID:  k.PersonaID,
			TokenLimit: a.cfg.TokenHard,
			CountLimit: a.cfg.CountHard,
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
		if terr == nil {
			if s, ok := tokenSnaps[k.KeyID]; ok {
				st.TokenUsed = s.Used + s.Prededuct
				if s.Hard > 0 {
					st.TokenLimit = s.Hard
				}
			}
		}
		if cerr == nil {
			if s, ok := countSnaps[k.KeyID]; ok {
				st.CountUsed = s.Used + s.Prededuct
				if s.Hard > 0 {
					st.CountLimit = s.Hard
				}
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// ===== 存储适配 =====

// storeAdapter 把 store.Store 适配为 gateway.Store。
type storeAdapter struct{ st *store.Store }

func (a *storeAdapter) AuthenticateUserKey(ctx context.Context, plaintext string) (*gateway.UserContext, error) {
	ac, err := a.st.AuthenticateUserKey(ctx, plaintext)
	if err != nil {
		// 三种失败原因（不存在 / 已吊销 / 用户停用）统一收敛为
		// ErrUnauthorized。区分它们会向调用方泄漏「这个 Key 曾经存在」，
		// 对撞库者是有价值的信息。原因保留在 wrap 里供服务端日志使用。
		if errors.Is(err, store.ErrNotFound) ||
			errors.Is(err, store.ErrKeyRevoked) ||
			errors.Is(err, store.ErrUserSuspended) {
			return nil, fmt.Errorf("%w: %v", gateway.ErrUnauthorized, err)
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
		VolcKeyID:        r.VolcKeyID,
		EgressIP:         r.EgressIP,
		Provider:         r.Provider,
		Model:            r.Model,
		BillingKind:      r.BillingKind,
		QuotaDay:         r.QuotaDay,
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
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

// UpsertVolcKey 导入或更新一个火山 Key。
//
// created 的判定用 CreatedAt.Equal(UpdatedAt): schema 里两列的 DEFAULT 都是
// now()，而 PostgreSQL 的 now() 在同一事务内返回同一时刻，所以新插入的行
// 两值严格相等；ON CONFLICT 分支显式写了 updated_at = now()，会把 UpdatedAt
// 推到晚于 CreatedAt。
//
// 这比「先 SELECT 再判断」可靠: 后者在并发导入下有 TOCTOU 窗口，两个请求
// 会同时报告 created。也比让 store 层加 RETURNING xmax = 0 更克制 —— 那是
// 依赖 PostgreSQL 内部事务 ID 的技巧，换存储引擎即失效。
func (a *storeAdapter) UpsertVolcKey(ctx context.Context, in gateway.NewVolcKey) (bool, error) {
	k, err := a.st.UpsertVolcKey(ctx, &store.VolcKey{
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

// PatchVolcKeyState 局部更新 Key 元数据。
//
// 这层只做类型转换与错误哨兵翻译，条件判定全在 store 的单条 SQL 里完成 ——
// 在适配层「先查再改」会留下 TOCTOU 窗口，让 expected_status 这类乐观并发
// 控制在并发下失效。
func (a *storeAdapter) PatchVolcKeyState(ctx context.Context, keyID string, p gateway.KeyPatch) (*gateway.KeyPatchResult, error) {
	res, err := a.st.PatchVolcKeyState(ctx, keyID, store.VolcKeyPatch{
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
		return nil, fmt.Errorf("%w: %v", gateway.ErrKeyNotFound, err)
	case errors.Is(err, store.ErrPreconditionFailed):
		// 结果与错误一并返回: 409 的文案要能写出冲突的实际内容
		// （当前状态是什么），只给错误值会让运维还得再查一次。
		return out, fmt.Errorf("%w: %v", gateway.ErrPreconditionFailed, err)
	case err != nil:
		return nil, err
	}
	return out, nil
}

// SetVolcKeyEgressIP 记录 Key 当前绑定的出口 IP。
//
// 走 UpdateVolcKeyState 的单列更新而非 PatchVolcKeyState:
// 后者带状态机守卫与乐观并发控制，而出口绑定的落库是对既成事实的存档
// —— 内存里已经换过去了，此处若因状态冲突被拒，只会造成库与内存不一致。
func (a *storeAdapter) SetVolcKeyEgressIP(ctx context.Context, keyID, egressIP string) error {
	return a.st.UpdateVolcKeyState(ctx, keyID, store.VolcKeyState{EgressIP: &egressIP})
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

func (q quotaReader) GetMany(ctx context.Context, keyIDs []string, kind quota.Kind) (map[string]quota.Snapshot, error) {
	return q.qm.GetMany(ctx, keyIDs, kind)
}

// ===== 刷新探测所需的 Key 列举 =====

// keyLister 返回参与刷新探测的 Key 列表。
func keyLister(st *store.Store) quota.KeyLister {
	return func(ctx context.Context) ([]string, error) {
		keys, err := st.ListVolcKeys(ctx, store.VolcKeyFilter{Status: store.VolcStatusActive})
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(keys))
		for _, k := range keys {
			ids = append(ids, k.KeyID)
		}
		return ids, nil
	}
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
