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
	if h == nil || h.UpstreamKeyID == "" {
		return errors.New("store: upstream_key_id 不能为空")
	}
	if h.Provider == "" {
		// 提前拦下而不是让 NOT NULL 约束报错: 约束错误里只有列名，
		// 定位不到是哪条归档、来自哪个调用方。
		return errors.New("store: provider 不能为空")
	}
	if h.QuotaDay.IsZero() {
		return errors.New("store: quota_day 不能为零值")
	}
	ratio := h.TokenRatio
	if ratio == 0 && h.TokenLimit > 0 {
		// 分子优先取 token，仅在 token 为零而按次有量时改用按次 —— store 层
		// 拿不到 provider 的计费口径（那是 config 的知识），但「token 一个没用
		// 却扣了次数」只可能是按次计费的上游。写死用 TokenUsed 会让这类行的
		// ratio 恒为 0，历史打分把已经刷了很多次的 Key 一直当成最闲的那个。
		used := h.TokenUsed
		if used == 0 && h.CountUsed > 0 {
			used = int64(h.CountUsed)
		}
		ratio = float64(used) / float64(h.TokenLimit)
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO key_daily_history (
			upstream_key_id, provider, quota_day, token_used, count_used, token_limit,
			token_ratio, request_count, error_count, consecutive_light_days)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (upstream_key_id, provider, quota_day) DO UPDATE SET
			token_used   = EXCLUDED.token_used,
			count_used   = EXCLUDED.count_used,
			token_limit  = EXCLUDED.token_limit,
			token_ratio  = EXCLUDED.token_ratio,
			request_count = EXCLUDED.request_count,
			error_count  = EXCLUDED.error_count,
			consecutive_light_days = EXCLUDED.consecutive_light_days`,
		h.UpstreamKeyID, h.Provider, h.QuotaDay, h.TokenUsed, h.CountUsed, h.TokenLimit,
		ratio, h.RequestCount, h.ErrorCount, h.ConsecutiveLightDays)
	if err != nil {
		return fmt.Errorf("store: upsert 每日归档: %w", err)
	}
	return nil
}

// HistoryKey 是归档记录的标识，与表主键一致。
//
// 单独用 key_id 做键不够: 同一 Key 迁移过 provider 时会有多行历史，
// 撞键会静默丢数据，而调度侧只关心「这个 Key 在当前 provider 下」的历史。
type HistoryKey struct {
	UpstreamKeyID string
	Provider      string
}

// GetKeyHistory 批量读取指定配额日的归档，供调度 S_history 打分使用。
//
// 返回 map 而非 slice: 调度器按 (key_id, provider) 查表，且缺失即视为
// "无历史"（中性分），不需要区分"没这个 Key"和"这个 Key 昨日没跑"。
func (s *Store) GetKeyHistory(ctx context.Context, keyIDs []string, quotaDay time.Time) (map[HistoryKey]KeyDailyHistory, error) {
	out := make(map[HistoryKey]KeyDailyHistory, len(keyIDs))
	if len(keyIDs) == 0 {
		return out, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT upstream_key_id, provider, quota_day, token_used, count_used, token_limit,
		       token_ratio, request_count, error_count, consecutive_light_days
		FROM key_daily_history
		WHERE quota_day = $1 AND upstream_key_id = ANY($2)`, quotaDay, keyIDs)
	if err != nil {
		return nil, fmt.Errorf("store: 读取每日归档: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var h KeyDailyHistory
		if err := rows.Scan(&h.UpstreamKeyID, &h.Provider, &h.QuotaDay, &h.TokenUsed, &h.CountUsed,
			&h.TokenLimit, &h.TokenRatio, &h.RequestCount, &h.ErrorCount,
			&h.ConsecutiveLightDays); err != nil {
			return nil, fmt.Errorf("store: 扫描每日归档: %w", err)
		}
		out[HistoryKey{UpstreamKeyID: h.UpstreamKeyID, Provider: h.Provider}] = h
	}
	return out, rows.Err()
}

// AggregateUsageByKey 按配额日汇总某些 Key 的用量，供归档任务生成 history。
func (s *Store) AggregateUsageByKey(ctx context.Context, quotaDay time.Time) ([]KeyDailyHistory, error) {
	// provider 从流水里带出来，而不是让归档任务事后猜: 流水记录了这笔请求
	// 实际打到哪个上游，是唯一可靠的来源。
	//
	// 必须把 provider 放进 GROUP BY，不能只按 key 分组再取 MAX(provider):
	// 那样多个上游的流水会被合并成一行，provider 标签由字典序决定 —— 归档
	// 会把商汤的用量挂到 volc 名下，下游再用这个错 provider 去取水位算
	// token_ratio，负载判断整条链路跟着错，且每一列都「有值」不报错。
	rows, err := s.pool.Query(ctx, `
		SELECT upstream_key_id,
		       provider,
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(count_units), 0),
		       COUNT(*),
		       COUNT(*) FILTER (WHERE status_code >= 400 OR error_code <> '')
		FROM usage_records
		WHERE quota_day = $1 AND upstream_key_id <> ''
		GROUP BY upstream_key_id, provider`, quotaDay)
	if err != nil {
		return nil, fmt.Errorf("store: 汇总用量: %w", err)
	}
	defer rows.Close()

	// 返回 slice 而非 map[keyID]: 同一 Key 在同一天可能有多个 provider 的
	// 流水（Key 被挪到别的上游），用 keyID 做 map 键会静默丢掉其中一条。
	var out []KeyDailyHistory
	for rows.Next() {
		h := KeyDailyHistory{QuotaDay: quotaDay}
		if err := rows.Scan(&h.UpstreamKeyID, &h.Provider, &h.TokenUsed, &h.CountUsed,
			&h.RequestCount, &h.ErrorCount); err != nil {
			return nil, fmt.Errorf("store: 扫描用量汇总: %w", err)
		}
		out = append(out, h)
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
	if d.UpstreamKeyID == "" {
		return errors.New("store: upstream_key_id 不能为空")
	}
	if d.QuotaDay.IsZero() {
		return errors.New("store: quota_day 不能为零值")
	}
	// provider 在库里是 NOT NULL。这里前置校验而非交给数据库报约束错误:
	// 唯一调用方（对账任务）写失败只 warn 不中断，约束错误会变成一行
	// 日志里的 SQLSTATE，而偏差记录就此静默丢失。
	if d.Provider == "" {
		return errors.New("store: provider 不能为空")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO quota_drift_logs (upstream_key_id, provider, billing_kind, quota_day, drift)
		VALUES ($1,$2,$3,$4,$5)`,
		d.UpstreamKeyID, d.Provider, d.BillingKind, d.QuotaDay, d.Drift)
	if err != nil {
		return fmt.Errorf("store: 写入配额偏差: %w", err)
	}
	return nil
}
