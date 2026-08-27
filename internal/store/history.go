package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// UpsertKeyDailyHistory 写入或更新某 Key 某配额日的归档。
//
// consecutive_light_days 由调用方（归档任务）基于前一日的值推导后传入，
// 而不是在 SQL 里自增 —— 归档任务可能重跑，自增会导致重复累加。
func (s *Store) UpsertKeyDailyHistory(ctx context.Context, h *KeyDailyHistory) error {
	if h == nil || h.VolcKeyID == "" {
		return errors.New("store: volc_key_id 不能为空")
	}
	if h.QuotaDay.IsZero() {
		return errors.New("store: quota_day 不能为零值")
	}
	ratio := h.TokenRatio
	if ratio == 0 && h.TokenLimit > 0 {
		ratio = float64(h.TokenUsed) / float64(h.TokenLimit)
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO key_daily_history (
			volc_key_id, quota_day, token_used, count_used, token_limit,
			token_ratio, request_count, error_count, consecutive_light_days)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (volc_key_id, quota_day) DO UPDATE SET
			token_used   = EXCLUDED.token_used,
			count_used   = EXCLUDED.count_used,
			token_limit  = EXCLUDED.token_limit,
			token_ratio  = EXCLUDED.token_ratio,
			request_count = EXCLUDED.request_count,
			error_count  = EXCLUDED.error_count,
			consecutive_light_days = EXCLUDED.consecutive_light_days`,
		h.VolcKeyID, h.QuotaDay, h.TokenUsed, h.CountUsed, h.TokenLimit,
		ratio, h.RequestCount, h.ErrorCount, h.ConsecutiveLightDays)
	if err != nil {
		return fmt.Errorf("store: upsert 每日归档: %w", err)
	}
	return nil
}

// GetKeyHistory 批量读取指定配额日的归档，供调度 S_history 打分使用。
//
// 返回 map 而非 slice: 调度器按 key_id 查表，且缺失即视为"无历史"（中性分），
// 不需要区分"没这个 Key"和"这个 Key 昨日没跑"。
func (s *Store) GetKeyHistory(ctx context.Context, keyIDs []string, quotaDay time.Time) (map[string]KeyDailyHistory, error) {
	out := make(map[string]KeyDailyHistory, len(keyIDs))
	if len(keyIDs) == 0 {
		return out, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT volc_key_id, quota_day, token_used, count_used, token_limit,
		       token_ratio, request_count, error_count, consecutive_light_days
		FROM key_daily_history
		WHERE quota_day = $1 AND volc_key_id = ANY($2)`, quotaDay, keyIDs)
	if err != nil {
		return nil, fmt.Errorf("store: 读取每日归档: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var h KeyDailyHistory
		if err := rows.Scan(&h.VolcKeyID, &h.QuotaDay, &h.TokenUsed, &h.CountUsed,
			&h.TokenLimit, &h.TokenRatio, &h.RequestCount, &h.ErrorCount,
			&h.ConsecutiveLightDays); err != nil {
			return nil, fmt.Errorf("store: 扫描每日归档: %w", err)
		}
		out[h.VolcKeyID] = h
	}
	return out, rows.Err()
}

// AggregateUsageByKey 按配额日汇总某些 Key 的用量，供归档任务生成 history。
func (s *Store) AggregateUsageByKey(ctx context.Context, quotaDay time.Time) (map[string]KeyDailyHistory, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT volc_key_id,
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(count_units), 0),
		       COUNT(*),
		       COUNT(*) FILTER (WHERE status_code >= 400 OR error_code <> '')
		FROM usage_records
		WHERE quota_day = $1 AND volc_key_id <> ''
		GROUP BY volc_key_id`, quotaDay)
	if err != nil {
		return nil, fmt.Errorf("store: 汇总用量: %w", err)
	}
	defer rows.Close()

	out := map[string]KeyDailyHistory{}
	for rows.Next() {
		h := KeyDailyHistory{QuotaDay: quotaDay}
		if err := rows.Scan(&h.VolcKeyID, &h.TokenUsed, &h.CountUsed,
			&h.RequestCount, &h.ErrorCount); err != nil {
			return nil, fmt.Errorf("store: 扫描用量汇总: %w", err)
		}
		out[h.VolcKeyID] = h
	}
	return out, rows.Err()
}

// SumUserTokens 返回某用户在指定配额日的总 Token 消耗，用于用户级日限额校验。
func (s *Store) SumUserTokens(ctx context.Context, userID int64, quotaDay time.Time) (int64, error) {
	var total int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(total_tokens), 0) FROM usage_records
		WHERE user_id = $1 AND quota_day = $2`, userID, quotaDay).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("store: 汇总用户用量: %w", err)
	}
	return total, nil
}

// InsertAuditLog 写入一条管理操作审计。
//
// 审计是同步写: 管理操作频率极低（人工触发），且"操作已执行但审计丢了"
// 在合规上是不可接受的。
func (s *Store) InsertAuditLog(ctx context.Context, log AuditLog) error {
	if log.Action == "" {
		return errors.New("store: 审计 action 不能为空")
	}
	var detail any
	if len(log.Detail) > 0 {
		b, err := json.Marshal(log.Detail)
		if err != nil {
			return fmt.Errorf("store: 序列化审计详情: %w", err)
		}
		detail = string(b)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO audit_logs (actor, action, target, detail) VALUES ($1,$2,$3,$4)`,
		log.Actor, log.Action, log.Target, detail)
	if err != nil {
		return fmt.Errorf("store: 写入审计: %w", err)
	}
	return nil
}

// InsertQuotaDrift 记录一次配额对账偏差（P0-2）。
//
// 偏差非零意味着 prededuct 与未过期租约之和不一致，通常是进程崩溃或
// Redis 主从切换导致的租约丢失。持久化后才能回溯"哪天哪个 Key 漏了多少"。
func (s *Store) InsertQuotaDrift(ctx context.Context, d QuotaDrift) error {
	if d.VolcKeyID == "" {
		return errors.New("store: volc_key_id 不能为空")
	}
	if d.QuotaDay.IsZero() {
		return errors.New("store: quota_day 不能为零值")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO quota_drift_logs (volc_key_id, billing_kind, quota_day, drift)
		VALUES ($1,$2,$3,$4)`, d.VolcKeyID, d.BillingKind, d.QuotaDay, d.Drift)
	if err != nil {
		return fmt.Errorf("store: 写入配额偏差: %w", err)
	}
	return nil
}
