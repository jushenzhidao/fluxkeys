package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

const volcKeyColumns = `id, key_id, secret_enc, provider, pool, status, persona_id, egress_ip,
	health_score, refresh_state, refresh_confirmed_at, last_error, last_used_at, created_at, updated_at`

func scanVolcKey(row pgx.Row) (*VolcKey, error) {
	var k VolcKey
	err := row.Scan(&k.ID, &k.KeyID, &k.SecretEnc, &k.Provider, &k.Pool, &k.Status,
		&k.PersonaID, &k.EgressIP, &k.HealthScore, &k.RefreshState, &k.RefreshConfirmedAt,
		&k.LastError, &k.LastUsedAt, &k.CreatedAt, &k.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// UpsertVolcKey 按 key_id 插入或更新一个火山 Key。
//
// 传入的 in.Secret 为明文，落库前用 AES-GCM 加密。Secret 为空时保留原密文，
// 便于只更新元数据而不接触密钥。
func (s *Store) UpsertVolcKey(ctx context.Context, in *VolcKey) (*VolcKey, error) {
	if in == nil || in.KeyID == "" {
		return nil, errors.New("store: key_id 不能为空")
	}
	provider := in.Provider
	if provider == "" {
		provider = "volc"
	}
	pool := in.Pool
	if pool == "" {
		pool = "cold"
	}
	// status 有两个语义要同时表达: 「新插入时用什么」与「已存在时是否覆盖」。
	//
	// 不能在这里直接默认成 active: 运维重跑同一份导入清单（清单里通常
	// 只有 key_id/secret/pool）会把已 banned 的 Key 复活并重新投入流量，
	// 而封禁状态恰恰是不该被一次例行导入抹掉的。
	//
	// 也不能靠 ON CONFLICT 里判断 EXCLUDED.status = '': EXCLUDED 拿到的是
	// VALUES 子句求值后的结果，若 VALUES 侧写了 COALESCE 兜底，EXCLUDED
	// 看到的就已经是兜底后的 'active'，那个判断永远为假 —— 实测踩过，
	// 表现是 banned Key 被例行导入静默复活。
	//
	// 所以拆成两个参数: status 是插入用的实际值，statusGiven 单独承载
	// 「调用方是否显式指定」这个事实，不经过 VALUES 求值。
	status := in.Status
	statusGiven := status != ""
	if !statusGiven {
		status = VolcStatusActive
	}
	// persona_id 与 egress_ip 同理，用独立标记而非 EXCLUDED 判空。
	personaGiven := in.PersonaID != ""
	egressGiven := in.EgressIP != ""
	health := in.HealthScore
	if health == 0 {
		health = 100
	}
	refreshState := in.RefreshState
	if refreshState == "" {
		refreshState = RefreshIdle
	}

	var enc string
	if in.Secret != "" {
		var err error
		if enc, err = s.cipher.EncryptSecret(in.Secret); err != nil {
			return nil, err
		}
	} else {
		enc = in.SecretEnc
	}

	// 判据是 enc != "" 而非 in.Secret != "": 密文有两个来源（明文加密、
	// 或调用方直传 SecretEnc），只要最终有密文就应写入。
	secretGiven := enc != ""

	row := s.pool.QueryRow(ctx, `
		INSERT INTO volc_keys (key_id, secret_enc, provider, pool, status, persona_id,
		                       egress_ip, health_score, refresh_state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (key_id) DO UPDATE SET
			-- 这三个「是否覆盖」用 $10/$11/$12 而非 EXCLUDED 判空:
			-- EXCLUDED 是 VALUES 求值后的结果，无法区分「调用方传了空」
			-- 与「Go 侧兜底填的默认值」。
			secret_enc   = CASE WHEN $10 THEN EXCLUDED.secret_enc
			                    ELSE volc_keys.secret_enc END,
			provider     = EXCLUDED.provider,
			pool         = EXCLUDED.pool,
			-- 未显式指定则保留原状态，避免例行导入复活 banned Key
			status       = CASE WHEN $11 THEN EXCLUDED.status
			                    ELSE volc_keys.status END,
			persona_id   = CASE WHEN $12 THEN EXCLUDED.persona_id
			                    ELSE volc_keys.persona_id END,
			-- 出口 IP 是终身绑定，未显式指定时绝不改动:
			-- 换出口等于把一个有历史的老账号变成「换了地址的账号」，
			-- 这正是风控最敏感的信号。
			egress_ip    = CASE WHEN $13 THEN EXCLUDED.egress_ip
			                    ELSE volc_keys.egress_ip END,
			health_score = EXCLUDED.health_score,
			refresh_state = EXCLUDED.refresh_state,
			updated_at   = now()
		RETURNING `+volcKeyColumns,
		in.KeyID, enc, provider, pool, status, in.PersonaID,
		in.EgressIP, health, refreshState,
		secretGiven, statusGiven, personaGiven, egressGiven)

	k, err := scanVolcKey(row)
	if err != nil {
		return nil, fmt.Errorf("store: upsert 火山 Key: %w", err)
	}
	return k, nil
}

// GetVolcKey 按业务 key_id 读取，并解密 Secret。
func (s *Store) GetVolcKey(ctx context.Context, keyID string) (*VolcKey, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+volcKeyColumns+` FROM volc_keys WHERE key_id = $1`, keyID)
	k, err := scanVolcKey(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取火山 Key: %w", err)
	}
	if k.SecretEnc != "" {
		if k.Secret, err = s.cipher.DecryptSecret(k.SecretEnc); err != nil {
			return nil, err
		}
	}
	return k, nil
}

// ListVolcKeys 按条件列出火山 Key。
//
// filter.WithSecret 为 true 时才解密密钥 —— 看板、状态列表等只读场景不该
// 把明文密钥载入内存。
func (s *Store) ListVolcKeys(ctx context.Context, filter VolcKeyFilter) ([]VolcKey, error) {
	var (
		conds []string
		args  []any
	)
	add := func(col, val string) {
		if val == "" {
			return
		}
		args = append(args, val)
		conds = append(conds, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	add("pool", filter.Pool)
	add("status", filter.Status)
	add("provider", filter.Provider)
	add("refresh_state", filter.RefreshState)

	q := `SELECT ` + volcKeyColumns + ` FROM volc_keys`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY key_id"
	if filter.Limit > 0 {
		args = append(args, filter.Limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 列出火山 Key: %w", err)
	}
	defer rows.Close()

	var out []VolcKey
	for rows.Next() {
		k, err := scanVolcKey(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描火山 Key: %w", err)
		}
		if filter.WithSecret && k.SecretEnc != "" {
			if k.Secret, err = s.cipher.DecryptSecret(k.SecretEnc); err != nil {
				return nil, err
			}
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// UpdateVolcKeyState 局部更新 Key 的运行时状态。nil 字段不参与更新。
func (s *Store) UpdateVolcKeyState(ctx context.Context, keyID string, st VolcKeyState) error {
	var (
		sets []string
		args []any
	)
	set := func(col string, val any) {
		args = append(args, val)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if st.Status != nil {
		set("status", *st.Status)
	}
	if st.Pool != nil {
		set("pool", *st.Pool)
	}
	if st.HealthScore != nil {
		set("health_score", *st.HealthScore)
	}
	if st.EgressIP != nil {
		set("egress_ip", *st.EgressIP)
	}
	if st.PersonaID != nil {
		set("persona_id", *st.PersonaID)
	}
	if st.LastError != nil {
		set("last_error", *st.LastError)
	}
	if st.TouchLastUsed {
		sets = append(sets, "last_used_at = now()")
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = now()")

	args = append(args, keyID)
	q := fmt.Sprintf("UPDATE volc_keys SET %s WHERE key_id = $%d", strings.Join(sets, ", "), len(args))

	tag, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("store: 更新火山 Key 状态: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateRefreshState 更新刷新探测状态（P0-4）。
//
// state == RefreshConfirmed 时同时写入 refresh_confirmed_at —— 这个时间戳是
// "已确认刷新"的唯一凭据，探测器重启后据此判断当日是否还需再探。
func (s *Store) UpdateRefreshState(ctx context.Context, keyID, state, lastErr string) error {
	q := `UPDATE volc_keys SET refresh_state = $1, last_error = $2, updated_at = now()`
	if state == RefreshConfirmed {
		q += `, refresh_confirmed_at = now()`
	}
	q += ` WHERE key_id = $3`

	tag, err := s.pool.Exec(ctx, q, state, lastErr, keyID)
	if err != nil {
		return fmt.Errorf("store: 更新刷新状态: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
