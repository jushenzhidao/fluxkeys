package quota

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"
)

// RefreshState 是单个 Key 在刷新窗口内的状态。
type RefreshState string

const (
	// RefreshIdle 非刷新窗口，正常服务。
	RefreshIdle RefreshState = "idle"
	// RefreshPending 已进入窗口，等待错峰时刻到达。
	RefreshPending RefreshState = "pending"
	// RefreshProbing 正在探测火山侧是否已刷新。此状态下不承接正常流量。
	RefreshProbing RefreshState = "probing"
	// RefreshConfirmed 已确认刷新完成，配额已清零。
	RefreshConfirmed RefreshState = "confirmed"
	// RefreshFailed 窗口结束仍未确认。这是超刷风险信号，必须告警。
	RefreshFailed RefreshState = "failed"
)

// Probe 对单个 Key 发起一次最小成本的探测请求。
//
// 返回 refreshed=true 表示确认火山侧额度已重置。实现方应发送一个
// max_tokens=1 的请求并检查是否仍返回额度耗尽错误。
type Probe func(ctx context.Context, keyID string) (refreshed bool, err error)

// KeyLister 返回当前需要参与刷新流程的 Key 列表。
type KeyLister func(ctx context.Context) ([]string, error)

// RefresherConfig 配置刷新探测器。
type RefresherConfig struct {
	// WindowStart / WindowEnd 是探测窗口（相对当日的时刻）。
	WindowStart time.Duration // 如 12h
	WindowEnd   time.Duration // 如 14h
	// ProbeInterval 是探测失败后的重试间隔。
	ProbeInterval time.Duration
	// Limits 是刷新确认后重置的水位配置。
	TokenLimits Limits
	CountLimits Limits
	// RampDuration 是确认刷新后的限速时长（起床缓冲）。
	RampDuration time.Duration
}

// Refresher 管理配额刷新窗口。
//
// P0-4 的核心设计: 刷新发生在上游侧，系统无法控制其发生时刻，只能探测
// 「是否已刷新」。V3 按时间推测刷新完成并清零本地 used，若上游实际尚未
// 重置就恢复调度，会直接超刷。
//
// 因此本实现遵循两条铁律:
//  1. 只有探测到成功响应才认为已刷新，绝不按时间推测；
//  2. stagger_offset 作用于「恢复调度的时刻」，不是「上游刷新的时刻」。
//
// 每个 provider 应独立创建一个 Refresher 实例。
type Refresher struct {
	provider string
	cfg      RefresherConfig
	qm       *Manager
	probe    Probe
	list     KeyLister
	log      *slog.Logger

	mu     sync.RWMutex
	states map[string]RefreshState
	// rampUntil 记录每个 Key 的限速截止时刻。
	rampUntil map[string]time.Time
	// lastProbe 记录上次探测时刻，用于控制重试间隔。
	lastProbe map[string]time.Time

	now func() time.Time
}

// NewRefresher 构造刷新探测器。每个 provider 应独立创建一个实例。
func NewRefresher(provider string, cfg RefresherConfig, qm *Manager, probe Probe, list KeyLister, log *slog.Logger) *Refresher {
	if cfg.ProbeInterval <= 0 {
		cfg.ProbeInterval = 5 * time.Minute
	}
	if cfg.RampDuration <= 0 {
		cfg.RampDuration = 30 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &Refresher{
		provider: provider,
		cfg:      cfg, qm: qm, probe: probe, list: list, log: log,
		states:    make(map[string]RefreshState),
		rampUntil: make(map[string]time.Time),
		lastProbe: make(map[string]time.Time),
		now:       time.Now,
	}
}

// SetClock 替换时钟，仅供测试使用。
func (r *Refresher) SetClock(f func() time.Time) { r.now = f }

// StaggerOffset 返回该 Key 在窗口内的错峰偏移。
//
// 语义澄清: 这是「该 Key 最早允许开始探测的时刻偏移」，用于避免所有 Key
// 在 12:00 同时打探测请求（这本身就是明显的机器行为特征）。
// 它不代表、也无法控制火山侧的刷新时刻。
func StaggerOffset(keyID, day string, window time.Duration) time.Duration {
	// 只在窗口前半段分散，为失败重试留出时间
	span := window / 2
	if span <= 0 {
		return 0
	}

	// 注意: 必须按「秒」取模再放大回 Duration。
	// 若直接对纳秒数取模，由于 32 位哈希最大约 4.3e9 而 1 小时是 3.6e12 纳秒，
	// 哈希值恒小于模数，取模退化为恒等映射 —— 所有 Key 的偏移都会挤在开头
	// 4 秒内，错峰完全失效，反而形成「百个账号同时发请求」的强机器特征。
	spanSec := uint64(span / time.Second)
	if spanSec == 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(keyID + "|" + day + "|stagger"))
	return time.Duration(h.Sum64()%spanSec) * time.Second
}

// State 返回 Key 当前的刷新状态。
func (r *Refresher) State(keyID string) RefreshState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.states[keyID]; ok {
		return s
	}
	return RefreshIdle
}

// Schedulable 判断 Key 当前是否可承接正常流量。
//
// probing / pending / failed 状态的 Key 必须被排除 —— 在确认刷新前放行流量
// 就是超刷。
func (r *Refresher) Schedulable(keyID string) bool {
	switch r.State(keyID) {
	case RefreshIdle, RefreshConfirmed:
		return true
	default:
		return false
	}
}

// RateLimitFactor 返回该 Key 当前应施加的限速系数（1.0 表示不限速）。
//
// 刚确认刷新的 Key 在 RampDuration 内限速 50%，模拟「起床缓冲」，
// 避免刷新瞬间出现整齐的流量尖峰。
func (r *Refresher) RateLimitFactor(keyID string) float64 {
	r.mu.RLock()
	until, ok := r.rampUntil[keyID]
	r.mu.RUnlock()
	if ok && r.now().Before(until) {
		return 0.5
	}
	return 1.0
}

// InWindow 判断当前是否处于刷新探测窗口内。
func (r *Refresher) InWindow(t time.Time) bool {
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	since := t.Sub(midnight)
	return since >= r.cfg.WindowStart && since < r.cfg.WindowEnd
}

// Tick 推进一次刷新流程。应由后台任务周期调用（建议 30s）。
func (r *Refresher) Tick(ctx context.Context) error {
	now := r.now()

	if !r.InWindow(now) {
		r.resetAfterWindow(now)
		return nil
	}

	keys, err := r.list(ctx)
	if err != nil {
		return fmt.Errorf("refresh: 列举 Key: %w", err)
	}

	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	windowStart := midnight.Add(r.cfg.WindowStart)
	window := r.cfg.WindowEnd - r.cfg.WindowStart
	day := QuotaDay(now)

	for _, keyID := range keys {
		state := r.State(keyID)
		if state == RefreshConfirmed {
			continue
		}

		// 进入窗口后先置为 pending，停止承接正常流量
		if state == RefreshIdle {
			r.setState(keyID, RefreshPending)
		}

		// 未到该 Key 的错峰时刻则继续等待
		offset := StaggerOffset(keyID, day, window)
		if now.Before(windowStart.Add(offset)) {
			continue
		}

		// 控制探测重试频率
		r.mu.RLock()
		last, probed := r.lastProbe[keyID]
		r.mu.RUnlock()
		if probed && now.Sub(last) < r.cfg.ProbeInterval {
			continue
		}

		r.setState(keyID, RefreshProbing)
		r.mu.Lock()
		r.lastProbe[keyID] = now
		r.mu.Unlock()

		refreshed, err := r.probe(ctx, keyID)
		if err != nil {
			r.log.Warn("刷新探测失败", "key_id", keyID, "err", err)
			continue
		}
		if !refreshed {
			// 火山侧尚未重置额度，保持 probing 并等待下次重试。
			// 关键: 此时绝不能清零本地配额或恢复调度。
			r.log.Debug("尚未刷新，继续等待", "key_id", keyID)
			continue
		}

		if err := r.confirm(ctx, keyID, now); err != nil {
			r.log.Error("刷新确认后重置配额失败", "key_id", keyID, "err", err)
			continue
		}
		r.log.Info("已确认配额刷新", "key_id", keyID)
	}

	// 进入窗口最后一分钟后，把仍未确认的 Key 标记为失败并告警。
	// 边界取闭区间，确保恰好落在该时刻的 Tick 也能触发。
	if !now.Before(midnight.Add(r.cfg.WindowEnd).Add(-time.Minute)) {
		r.markUnconfirmedFailed(keys)
	}
	return nil
}

// UpdateLimits 就地替换刷新确认后要写回的水位，不重建实例。
//
// 存在的理由: 存量 provider 改了 quota_limit 或水位系数后，若靠重建 Refresher
// 来生效，会连带清空状态机（states / rampUntil / lastProbe）。重建若恰好落在
// 刷新窗口内，全部 Key 从 confirmed 退回 idle 再走一遍 pending → probing ——
// 而 pending 状态是不承接流量的，等于用一次「必然中断刷新流程」去换一次配置
// 生效。就地更新没有这个代价。
//
// 就地换值安全的前提: 这两个字段只在 confirm 写回时被读，不参与状态机推进，
// 因此中途换值不会让状态机进入不一致状态。
//
// 不更新 WindowStart / WindowEnd / ProbeInterval / RampDuration: 这四项参与
// 状态机判定，且来自全局 refresh.* 配置，不在本期热加载范围内。改它们需重启
// 网关 —— reconcileRefreshers 会为此打 WARN。
func (r *Refresher) UpdateLimits(token, count Limits) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg.TokenLimits = token
	r.cfg.CountLimits = count
}

// limitsFor 在读锁下取当次写回要用的水位。
//
// 必须加锁: UpdateLimits 可能与 confirm 并发（前者在 reconcile 协程、后者在
// 探测协程），裸读 r.cfg 会与写侧构成数据竞争。
func (r *Refresher) limitsFor() (token, count Limits) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg.TokenLimits, r.cfg.CountLimits
}

// confirm 在探测确认后清零配额并进入限速期。
func (r *Refresher) confirm(ctx context.Context, keyID string, now time.Time) error {
	// 两路水位一次取齐。分两次取会让 token 用旧值、count 用新值,
	// 同一个 Key 的两个量纲落在不同配置版本上。
	tokenLim, countLim := r.limitsFor()
	if err := r.qm.MarkRefreshed(ctx, r.provider, keyID, KindToken, tokenLim); err != nil {
		return err
	}
	if err := r.qm.MarkRefreshed(ctx, r.provider, keyID, KindCount, countLim); err != nil {
		return err
	}
	r.mu.Lock()
	r.states[keyID] = RefreshConfirmed
	r.rampUntil[keyID] = now.Add(r.cfg.RampDuration)
	r.mu.Unlock()
	return nil
}

func (r *Refresher) markUnconfirmedFailed(keys []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, keyID := range keys {
		switch r.states[keyID] {
		case RefreshPending, RefreshProbing:
			r.states[keyID] = RefreshFailed
			r.log.Error("配额刷新未确认，存在超刷风险", "key_id", keyID)
		}
	}
}

// resetAfterWindow 在离开窗口后清理状态，为下一个周期做准备。
func (r *Refresher) resetAfterWindow(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for keyID, st := range r.states {
		if st == RefreshConfirmed || st == RefreshFailed {
			delete(r.states, keyID)
			delete(r.lastProbe, keyID)
		}
	}
	for keyID, until := range r.rampUntil {
		if now.After(until) {
			delete(r.rampUntil, keyID)
		}
	}
}

func (r *Refresher) setState(keyID string, s RefreshState) {
	r.mu.Lock()
	r.states[keyID] = s
	r.mu.Unlock()
}

// Stats 汇总刷新状态分布，供看板与告警使用。
func (r *Refresher) Stats() map[RefreshState]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[RefreshState]int)
	for _, s := range r.states {
		out[s]++
	}
	return out
}

// PreRefreshActive 判断是否处于刷新前的低功耗期。
//
// 刷新前 30 分钟降低消耗，避免在额度即将重置时还大量占用余量。
func (r *Refresher) PreRefreshActive(t time.Time, lead time.Duration) bool {
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	since := t.Sub(midnight)
	return since >= r.cfg.WindowStart-lead && since < r.cfg.WindowStart
}
