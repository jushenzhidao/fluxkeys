package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	defaultUsageBuffer   = 4096
	defaultUsageFlush    = 500 * time.Millisecond
	defaultUsageBatch    = 200
	usageFlushTimeout    = 10 * time.Second
	usageShutdownTimeout = 15 * time.Second
)

// usageWriter 是用量流水的异步批量写入器。
//
// 为什么必须异步: 流水写入位于请求返回后的收尾阶段，但仍在请求 goroutine 上。
// 一次同步 INSERT 在 Postgres 抖动时可能耗时数百毫秒甚至超时，直接把数据库
// 延迟传导成用户可见延迟。而流水是事后审计数据，晚几百毫秒落库没有任何影响。
//
// 有损策略: 缓冲区满时丢弃并计数，绝不阻塞热路径。计费精度由 Redis 侧的
// used 计数保证（P0-1），Postgres 流水是明细而非余额，允许极端情况下有损。
type usageWriter struct {
	store *Store

	ch       chan UsageRecord
	batch    int
	interval time.Duration

	dropped  atomic.Int64
	written  atomic.Int64
	failed   atomic.Int64
	closed   atomic.Bool
	done     chan struct{}
	closeOne sync.Once
}

func newUsageWriter(s *Store, opt Options) *usageWriter {
	bufSize := opt.UsageBuffer
	if bufSize <= 0 {
		bufSize = defaultUsageBuffer
	}
	interval := opt.UsageFlushInterval
	if interval <= 0 {
		interval = defaultUsageFlush
	}
	batch := opt.UsageBatchSize
	if batch <= 0 {
		batch = defaultUsageBatch
	}

	w := &usageWriter{
		store:    s,
		ch:       make(chan UsageRecord, bufSize),
		batch:    batch,
		interval: interval,
		done:     make(chan struct{}),
	}
	go w.loop()
	return w
}

func (w *usageWriter) loop() {
	defer close(w.done)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	buf := make([]UsageRecord, 0, w.batch)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		// 刻意从 Background 派生，不继承 New 传入的 ctx。
		//
		// golangci-lint 的 contextcheck 会建议"传 context"，但在这里方向是反的:
		// New 收到的 ctx 通常只覆盖启动阶段（带超时），若 flush 继承它，
		// 超时之后所有用量记录都会静默写不进去 —— 而 usage_records 是计费与
		// 配额对账的事实来源。写入器的生命周期由 Close() 界定，不由构造期 ctx 界定。
		// 回归测试: TestInsertUsageRecord_构造context取消后仍能落库
		ctx, cancel := context.WithTimeout(context.Background(), usageFlushTimeout)
		if err := w.store.insertUsageBatch(ctx, buf); err != nil {
			w.failed.Add(int64(len(buf)))
		} else {
			w.written.Add(int64(len(buf)))
		}
		cancel()
		buf = buf[:0]
	}

	for {
		select {
		case rec, ok := <-w.ch:
			if !ok {
				flush()
				return
			}
			buf = append(buf, rec)
			if len(buf) >= w.batch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// enqueue 投递一条流水。缓冲区满时立即返回而不阻塞。
func (w *usageWriter) enqueue(rec UsageRecord) error {
	if w.closed.Load() {
		return errors.New("store: 流水写入器已关闭")
	}
	select {
	case w.ch <- rec:
		return nil
	default:
		w.dropped.Add(1)
		return fmt.Errorf("store: 流水缓冲区已满，丢弃 1 条（累计丢弃 %d）", w.dropped.Load())
	}
}

// Close 关闭通道并等待剩余流水落库。
func (w *usageWriter) Close() error {
	var err error
	w.closeOne.Do(func() {
		w.closed.Store(true)
		close(w.ch)
		select {
		case <-w.done:
		case <-time.After(usageShutdownTimeout):
			err = errors.New("store: 等待流水落库超时，可能有数据未写入")
		}
	})
	return err
}

// UsageStats 是流水写入器的运行统计。
type UsageStats struct {
	Written int64 // 成功落库条数
	Failed  int64 // 写库失败条数
	Dropped int64 // 缓冲区满而丢弃的条数
	Pending int64 // 通道内待处理条数
}

// UsageStats 返回流水写入统计，供监控暴露。
//
// Dropped 或 Failed 持续增长说明 Postgres 已成为瓶颈，需要告警 ——
// 虽然不影响请求成败，但会导致计费明细缺失。
func (s *Store) UsageStats() UsageStats {
	w := s.usage
	if w == nil {
		return UsageStats{}
	}
	return UsageStats{
		Written: w.written.Load(),
		Failed:  w.failed.Load(),
		Dropped: w.dropped.Load(),
		Pending: int64(len(w.ch)),
	}
}

// InsertUsageRecord 异步写入一条用量流水。
//
// 返回的错误仅表示"未能进入缓冲队列"，调用方应当记日志但不要因此让请求失败。
func (s *Store) InsertUsageRecord(ctx context.Context, rec UsageRecord) error {
	if rec.QuotaDay.IsZero() {
		return errors.New("store: quota_day 不能为零值")
	}
	if rec.Provider == "" {
		rec.Provider = "volc"
	}
	if rec.BillingKind == "" {
		rec.BillingKind = "token"
	}
	return s.usage.enqueue(rec)
}

// FlushUsage 同步 flush 当前缓冲区，仅供测试与优雅关闭前的显式落库使用。
func (s *Store) FlushUsage(ctx context.Context) error {
	w := s.usage
	if w == nil {
		return nil
	}
	// 等待通道排空。批量写由后台 goroutine 按 interval 触发，
	// 这里只需确认队列已被消费完。
	deadline := time.Now().Add(usageFlushTimeout)
	for len(w.ch) > 0 {
		if time.Now().After(deadline) {
			return errors.New("store: flush 流水超时")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	// 再等一个 flush 周期，确保最后一批已提交。
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(w.interval + 200*time.Millisecond):
	}
	return nil
}

func (s *Store) insertUsageBatch(ctx context.Context, recs []UsageRecord) error {
	if len(recs) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, r := range recs {
		var userID, keyID any
		if r.UserID > 0 {
			userID = r.UserID
		}
		if r.UserAPIKeyID > 0 {
			keyID = r.UserAPIKeyID
		}
		b.Queue(`
			INSERT INTO usage_records (
				request_id, user_id, user_api_key_id, volc_key_id, egress_ip, provider,
				model, billing_kind, quota_day, prompt_tokens, completion_tokens,
				total_tokens, count_units, estimated_tokens, status_code, is_stream,
				error_code, retry_count, latency_ms)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
			r.RequestID, userID, keyID, r.VolcKeyID, r.EgressIP, r.Provider,
			r.Model, r.BillingKind, r.QuotaDay, r.PromptTokens, r.CompletionTokens,
			r.TotalTokens, r.CountUnits, r.EstimatedTokens, r.StatusCode, r.IsStream,
			r.ErrorCode, r.RetryCount, r.LatencyMS)
	}
	if err := s.pool.SendBatch(ctx, b).Close(); err != nil {
		return fmt.Errorf("store: 批量写入用量流水: %w", err)
	}
	return nil
}
