package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// 本文件只做一件事: 在单条 SQL 内完成「条件匹配 + 取旧值 + 写新值」。
//
// 为什么不复用 UpdateUpstreamKeyState:
//
// 那个方法的签名是 (…) error —— 拿不到旧值，也没有条件匹配。用它实现
// 管理接口的 PATCH 就必须退化成「先 SELECT 取旧值并校验，再 UPDATE」，
// 而这在两个并发 PATCH 之间存在 TOCTOU 窗口: 两者各自读到同一个旧状态、
// 各自通过 expected_status 校验、再互相覆盖。表现是乐观并发控制看起来
// 生效（顺序调用的测试全绿），实际形同虚设。
//
// 状态机策略（哪些状态不许转成什么）刻意不放在本层: 数据访问层只负责
// 「按调用方给的条件原子地改」，规则由调用方传入。

// ErrPreconditionFailed 表示记录存在，但不满足调用方给出的前置条件
// （ExpectedStatus 不匹配，或当前状态落在 RejectStatusFrom 内）。
//
// 必须与 ErrNotFound 分开: 两者在 UPDATE 层面都表现为「影响 0 行」，
// 但对外语义一个是 409 一个是 404。不区分会让运维在「Key ID 打错了」
// 和「状态已被别人改过」之间无从下手。
var ErrPreconditionFailed = errors.New("store: 前置条件不满足")

// UpstreamKeyPatch 是一次 Key 元数据的局部更新意图。
//
// 指针为 nil 表示「调用方未提供该字段」，该列一律不参与更新。
//
// 用指针而非零值判空是硬要求: status 的空串与「字段缺席」在 encoding/json
// 解成 string 之后不可区分，据零值判断会把「请求里没提 status」当成
// 「要把 status 清空」。这与 UpsertUpstreamKey 注释里记的 EXCLUDED 被 VALUES
// 兜底污染是同一类缺陷 —— 都是无法区分未提供与空值，后果都是静默改写
// 不该改的列。
type UpstreamKeyPatch struct {
	Status    *string
	Pool      *string
	PersonaID *string

	// ExpectedStatus 非 nil 时启用乐观并发控制: 仅当该行当前 status 等于
	// 它才更新，否则返回 ErrPreconditionFailed。
	ExpectedStatus *string

	// RejectStatusFrom 非空时，该行当前 status 落在其中即拒绝本次更新。
	//
	// 供调用方表达「终态不许直接复活」这类状态机约束。判定必须在 SQL 内
	// 完成，理由同本文件开头 —— 放在 Go 侧先查再改就只是看起来安全。
	RejectStatusFrom []string
}

// HasFieldUpdate 报告本次是否至少要改一列。
//
// 三个可改字段全部缺席时调用方应当报错而不是返回成功: 否则调用方无法
// 区分「改了」与「什么都没改」。
func (p UpstreamKeyPatch) HasFieldUpdate() bool {
	return p.Status != nil || p.Pool != nil || p.PersonaID != nil
}

// UpstreamKeyPatchResult 是一次局部更新的新旧值对照。
//
// 回显旧值而非只回显新值: 运维需要「改之前确实是那个值」的凭据，
// 且只记新值的审计无法回答「这个 Key 是什么时候从 active 变成 banned 的」。
type UpstreamKeyPatchResult struct {
	PrevStatus    string
	PrevPool      string
	PrevPersonaID string
	NewStatus     string
	NewPool       string
	NewPersonaID  string
}

// PatchUpstreamKeyState 原子地局部更新 Key 的 status / pool / persona_id。
//
// 返回值约定:
//   - 成功: (结果, nil)
//   - 行不存在: (nil, ErrNotFound)
//   - 行存在但前置条件不满足: (当前值快照, ErrPreconditionFailed)
//
// 第三种情形刻意同时返回结果与错误 —— 调用方要靠这份当前值写出
// 「当前状态为 banned，与 expected_status=active 不符」这样可自解释的
// 409 文案，只给一个错误值会让运维看不到冲突的实际内容。
//
// 只开放这三列是有意的:
//
//	egress_ip 不在此处，出口 IP 变更必须走 PUT /admin/keys/{id}/ip ——
//	它需要 egress.Rebind() 丢弃旧连接这一副作用，否则会出现「库里 IP
//	改了但连接仍走旧 IP」的静默不一致，绑定变更形同虚设。
//
//	health_score 不在此处，它是运行时观测值，人工改写会立刻被下一次
//	成功/失败请求覆盖，只会给运维「改了但没用」的错觉。
func (s *Store) PatchUpstreamKeyState(ctx context.Context, keyID string, p UpstreamKeyPatch) (*UpstreamKeyPatchResult, error) {
	if keyID == "" {
		return nil, errors.New("store: key_id 不能为空")
	}
	if !p.HasFieldUpdate() {
		return nil, errors.New("store: 没有需要变更的字段")
	}

	// 显式转 nil: 空切片与 nil 在 pgx 侧分别编码为空数组与 NULL，
	// 而 SQL 里靠 IS NULL 判断「无需守卫」。空数组会让 ANY 比较恒为假
	// 从而走进 NOT(...) 为真的分支，结果虽然一致但依赖了额外一层推理，
	// 不如让两种「无守卫」写法收敛成同一个 NULL。
	var reject []string
	if len(p.RejectStatusFrom) > 0 {
		reject = p.RejectStatusFrom
	}

	// old 是同表自连接，只用于 RETURNING 取 pre-image（RETURNING t.* 拿到的
	// 是更新后的值，拿不到旧值）。
	//
	// 关键: 条件判定必须写在 t 上，绝不能写在 old 上。
	//
	// 两个并发请求争同一行时，后到者会阻塞在行锁上；前者提交后，PostgreSQL
	// 只对**更新目标行**重新求值 WHERE 条件（EvalPlanQual 重检），而 FROM
	// 侧 old 关系仍沿用该语句开始时的快照。于是把 CAS 条件写成 old.status
	// = $5 时，后到者在解除阻塞后看到的 old.status 还是它进来时读到的旧值，
	// 条件照样成立，更新就被放行 —— 乐观并发控制静默失效。
	//
	// 已实测: 条件在 old 上时两个并发 CAS 都返回 1 行（都成功、互相覆盖）；
	// 条件改到 t 上时后到者返回 0 行。回归测试见
	// TestPatchUpstreamKeyState_并发下乐观并发控制严格成立。
	//
	// t.status 出现在 WHERE 里读到的是本行更新前的值，语义上正是「当前值」，
	// 与判定意图一致。
	row := s.pool.QueryRow(ctx, `
		UPDATE upstream_keys AS t
		   SET status     = COALESCE($2::text, t.status),
		       pool       = COALESCE($3::text, t.pool),
		       persona_id = COALESCE($4::text, t.persona_id),
		       updated_at = now()
		  FROM upstream_keys AS old
		 WHERE t.key_id = old.key_id
		   AND t.key_id = $1
		   AND ($5::text   IS NULL OR t.status = $5::text)
		   AND ($6::text[] IS NULL OR NOT (t.status = ANY($6::text[])))
		RETURNING old.status, old.pool, old.persona_id,
		          t.status, t.pool, t.persona_id`,
		keyID, p.Status, p.Pool, p.PersonaID, p.ExpectedStatus, reject)

	var out UpstreamKeyPatchResult
	err := row.Scan(&out.PrevStatus, &out.PrevPool, &out.PrevPersonaID,
		&out.NewStatus, &out.NewPool, &out.NewPersonaID)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.explainPatchMiss(ctx, keyID)
	}
	if err != nil {
		return nil, fmt.Errorf("store: 局部更新火山 Key: %w", err)
	}
	return &out, nil
}

// explainPatchMiss 判定 PatchUpstreamKeyState 未命中的原因。
//
// 这次补查不构成竞态隐患: 无论期间该行是否被别人改动，两种结论
// （不存在 → 404、存在但条件不满足 → 409）都是当时真实发生过的事实，
// 而关键的「不误改」已由上一条 SQL 的原子条件保证。
func (s *Store) explainPatchMiss(ctx context.Context, keyID string) (*UpstreamKeyPatchResult, error) {
	var cur UpstreamKeyPatchResult
	err := s.pool.QueryRow(ctx,
		`SELECT status, pool, persona_id FROM upstream_keys WHERE key_id = $1`, keyID).
		Scan(&cur.PrevStatus, &cur.PrevPool, &cur.PrevPersonaID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 判定局部更新未命中的原因: %w", err)
	}
	// 新旧值填成同一份: 本次没有任何改动，让调用方的回显逻辑无需分叉。
	cur.NewStatus, cur.NewPool, cur.NewPersonaID = cur.PrevStatus, cur.PrevPool, cur.PrevPersonaID
	return &cur, ErrPreconditionFailed
}
