package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/metrics"
	"github.com/fluxkeys/fluxkeys/internal/quota"
	"github.com/fluxkeys/fluxkeys/internal/scheduler"
	"github.com/fluxkeys/fluxkeys/internal/store"
)

// 后台任务集合。
//
// 全部任务遵守三条规则:
//
//  1. 每轮循环都先 select ctx.Done()。否则关闭时会多跑一轮，而此时依赖
//     可能已经开始关闭，产生一堆无意义的错误日志。
//
//  2. 单轮失败只记日志不退出。这些任务都是周期性自愈的 —— 一次租约回收
//     失败，下一轮会把上一轮该收的一起收掉。因失败退出反而让问题永久化。
//
//  3. 每轮有独立超时。没有超时的话，一次 Redis 卡死会让该任务永久停摆，
//     而外部看起来进程还是健康的。

// bgDeps 是后台任务的依赖。
type bgDeps struct {
	cfg     *config.Config
	qm      *quota.Manager
	st      *store.Store
	sched   *scheduler.Scheduler
	pool    *egress.Pool
	metrics *metrics.Metrics
	log     *slog.Logger
}

// background 管理全部后台任务的生命周期。
type background struct {
	bgDeps
	wg     sync.WaitGroup
	cancel context.CancelFunc
}

func newBackground(d bgDeps) *background { return &background{bgDeps: d} }

// start 启动全部后台任务。
func (b *background) start(parent context.Context) {
	// 用独立的 cancel 而非直接用 parent: 关闭顺序要求后台任务在网关之后停
	// （网关关闭期间的流式请求仍在结束租约），而 parent 在收到信号时就已取消。
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	b.cancel = cancel

	// 租约回收（P0-2 的第二条回收路径）。
	//
	// 前两条路径是「正常 Commit 修正」和「崩溃后的超时回收」，这个任务
	// 就是后者的执行者。没有它，任何进程崩溃或未捕获的 return 路径都会
	// 让 prededuct 永久占用额度，最终整个 Key 池看起来全满而实际空闲。
	b.every(ctx, "lease_reap", b.dur(b.cfg.Quota.ReapInterval, 30*time.Second),
		30*time.Second, b.reapLeases)

	// 配额快照刷新（只读，供调度排序用，P0-1: 绝不参与准入）
	b.every(ctx, "quota_snapshot", b.dur(b.cfg.Quota.SnapshotInterval, time.Second),
		5*time.Second, func(ctx context.Context) error {
			b.sched.RefreshSnapshot(ctx)
			return nil
		})

	// 配额对账（P0-2 的偏差检测）
	b.every(ctx, "reconcile", b.dur(b.cfg.Quota.ReconcileInterval, time.Hour),
		5*time.Minute, b.reconcile)

	// Key 池重载。
	//
	// 周期性重载而非依赖管理接口触发: 运维可能直接改库，也可能有另一个
	// 进程在导入 Key。5 分钟的延迟对「天」级变化的 Key 元数据完全够用。
	b.every(ctx, "key_reload", 5*time.Minute, time.Minute, b.sched.Reload)

	// 指标采集: Key 状态分布、配额水位、出口 IP 状态
	b.every(ctx, "metrics_collect", 15*time.Second, 30*time.Second, b.collectMetrics)

	// 出口 IP 健康检查（P1-6）
	if b.pool.Mode() == egress.ModeMultiIP {
		iv := b.dur(b.cfg.Egress.HealthCheckInterval, 5*time.Minute)
		b.every(ctx, "egress_health", iv, time.Minute, b.checkEgress)
	}

	// 刷新探测（P0-4: 不假设刷新时刻，只探测「是否已刷新」）
	if b.cfg.Refresh.Enabled {
		b.startRefresher(ctx)
	} else {
		b.log.Warn("刷新探测器已禁用，配额将只依赖 Redis key 的自然过期（P0-4）")
	}

	// 每日归档: 把当日用量汇总进 key_daily_history，供调度的 S_history 使用
	b.every(ctx, "daily_archive", time.Hour, 5*time.Minute, b.archiveDaily)
}

// stop 停止全部后台任务并等待退出。
func (b *background) stop() {
	if b.cancel == nil {
		return
	}
	b.cancel()

	// 等待有上限: 某个任务卡在网络 I/O 上不应无限阻塞进程退出。
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
		b.log.Info("后台任务已全部退出")
	case <-time.After(10 * time.Second):
		b.log.Warn("等待后台任务退出超时，强制继续")
	}
}

// every 以固定间隔运行 fn，每轮独立超时。
func (b *background) every(ctx context.Context, name string, interval, timeout time.Duration,
	fn func(context.Context) error) {

	if interval <= 0 {
		b.log.Warn("后台任务间隔非法，已跳过", "task", name, "interval", interval)
		return
	}

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()

		// 首轮加随机偏移错开各任务，避免多个任务同一时刻并发打 Redis。
		// 用 StaggerOffset 而非 rand 是为了让同一进程的偏移可复现，
		// 便于对照日志时间戳排查。
		initial := quota.StaggerOffset(name, "boot", interval)
		select {
		case <-ctx.Done():
			return
		case <-time.After(initial):
		}

		t := time.NewTicker(interval)
		defer t.Stop()

		for {
			// 先检查取消再执行: 否则关闭时会多跑一轮，而依赖可能已在关闭中
			select {
			case <-ctx.Done():
				b.log.Debug("后台任务退出", "task", name)
				return
			default:
			}

			start := time.Now()
			rctx, cancel := context.WithTimeout(ctx, timeout)
			err := fn(rctx)
			cancel()

			if err != nil && ctx.Err() == nil {
				// 单轮失败只告警。这些任务都是周期性自愈的。
				b.log.Warn("后台任务本轮失败，将在下一轮重试",
					"task", name, "elapsed_ms", time.Since(start).Milliseconds(), "err", err)
			}

			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// dur 返回配置值，非正时用默认值。
func (b *background) dur(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

// reapLeases 回收过期租约。
func (b *background) reapLeases(ctx context.Context) error {
	batch := b.cfg.Quota.ReapBatch
	if batch <= 0 {
		batch = 500
	}
	n, err := b.qm.Reap(ctx, batch)
	if err != nil {
		return err
	}
	if n > 0 {
		b.metrics.ObserveReap(n)
		// 用 Info 而非 Debug: 持续有租约被超时回收，说明有请求路径没有
		// 正常结束租约，这是需要人看到的信号而非常态噪声。
		b.log.Info("回收过期租约", "count", n)
	}
	return nil
}

// reconcile 对全部 Key 执行配额对账。
//
// 对账的意义: 租约回收依赖 Redis 有序集合的过期扫描，若某条记录因异常
// 未进入该集合，超时回收就会漏掉它，prededuct 永久偏高。对账直接以
// 「未过期租约之和」强制重算 prededuct，是最后一道自愈手段。
func (b *background) reconcile(ctx context.Context) error {
	keys, err := b.st.ListVolcKeys(ctx, store.VolcKeyFilter{})
	if err != nil {
		return err
	}

	day := quota.QuotaDayTime(time.Now())
	var checked, drifted int
	for _, k := range keys {
		if ctx.Err() != nil {
			break
		}
		for _, kind := range []quota.Kind{quota.KindToken, quota.KindCount} {
			drift, err := b.qm.Reconcile(ctx, k.KeyID, kind)
			if err != nil {
				b.log.Warn("对账失败", "key_id", k.KeyID, "kind", kind, "err", err)
				continue
			}
			checked++
			if drift == 0 {
				continue
			}
			drifted++
			b.metrics.ObserveDrift(string(kind), drift)
			b.log.Warn("配额偏差已修正", "key_id", k.KeyID, "kind", kind, "drift", drift)

			// 偏差落库供看板与事后分析。写失败不影响修正本身 ——
			// 修正已在 Redis 完成，这里只是留痕。
			if err := b.st.InsertQuotaDrift(ctx, store.QuotaDrift{
				VolcKeyID: k.KeyID, BillingKind: string(kind),
				QuotaDay: day, Drift: drift,
			}); err != nil {
				b.log.Warn("记录配额偏差失败", "key_id", k.KeyID, "err", err)
			}
		}
	}

	b.log.Info("配额对账完成", "checked", checked, "drifted", drifted)
	return nil
}

// collectMetrics 采集 Key 状态、配额水位与出口 IP 状态。
func (b *background) collectMetrics(ctx context.Context) error {
	keys, err := b.st.ListVolcKeys(ctx, store.VolcKeyFilter{})
	if err != nil {
		return err
	}

	health := b.sched.HealthAll()
	byStatus := make(map[string]int, 4)
	byPool := make(map[string]int, 3)
	ids := make([]string, 0, len(keys))

	for _, k := range keys {
		status := k.Status
		// 内存健康状态比库里的落盘值新，冷却/封禁这类瞬时状态只存在于内存
		if h, ok := health[k.KeyID]; ok {
			status = string(h.Status)
		}
		byStatus[status]++
		byPool[k.Pool]++
		ids = append(ids, k.KeyID)
	}
	// Reset 语义的快照式覆盖: 增量更新漏一次状态迁移会造成永久偏差
	b.metrics.SetKeyDistribution(byStatus, byPool)

	// 配额水位按 key_id 展开。这是唯一按 key_id 打标签的指标 ——
	// 1000 个 Key 产生 2000 条序列，可接受；再多维度就会爆。
	for _, kind := range []quota.Kind{quota.KindToken, quota.KindCount} {
		snaps, err := b.qm.GetMany(ctx, ids, kind)
		if err != nil {
			b.log.Warn("读取配额快照失败", "kind", kind, "err", err)
			continue
		}
		for id, s := range snaps {
			b.metrics.SetQuotaRatio(id, string(kind), s.Ratio())
		}
	}

	// 清理已删除 Key 的序列，否则指标序列只增不减
	live := make(map[string]bool, len(ids))
	for _, id := range ids {
		live[id] = true
	}
	b.forgetStaleKeys(live)

	st := b.pool.Stats()
	byState := make(map[string]int, 4)
	reputation := make(map[string]int, len(st.PerIP))
	bound := make(map[string]int, len(st.PerIP))
	for _, ip := range st.PerIP {
		byState[string(ip.State)]++
		reputation[ip.Addr] = ip.Reputation
		bound[ip.Addr] = ip.BoundKeys
	}
	b.metrics.SetEgressStats(byState, reputation, bound)

	return nil
}

// trackedKeys 记录已上报指标的 Key，用于检测下线的 Key。
var trackedKeys sync.Map

// forgetStaleKeys 清理已不在池中的 Key 的指标序列。
func (b *background) forgetStaleKeys(live map[string]bool) {
	for id := range live {
		trackedKeys.Store(id, struct{}{})
	}
	trackedKeys.Range(func(k, _ any) bool {
		id, ok := k.(string)
		if !ok {
			return true
		}
		if !live[id] {
			b.metrics.ForgetKey(id)
			trackedKeys.Delete(id)
			b.log.Info("已清理下线 Key 的指标序列", "key_id", id)
		}
		return true
	})
}

// checkEgress 检查出口 IP 连通性（P1-6）。
//
// 运行期而非只在启动时检查: EIP 可能被解绑，路由表可能被其他运维操作清掉，
// 而这类失效是静默的 —— 流量会悄悄全部走主 IP。
func (b *background) checkEgress(ctx context.Context) error {
	target := b.cfg.Egress.VerifyTarget
	if target == "" {
		target = "api.volcengine.com:443"
	}

	// 顺序不可换: 先解封再探测。
	//
	// Verify 对所有 IP 探测并在成功时调 MarkSuccess，但 MarkSuccess 只把
	// suspect / cooldown 转回 active —— banned 不在其中。若先探测，被封出口
	// 即使探测成功也仍是 banned，自动恢复永远不会发生。
	for _, addr := range b.pool.TryUnbanAll(
		b.cfg.Egress.BanCooldown, b.cfg.Egress.BanCooldownMax) {
		b.log.Info("被封出口冷却期届满，转入 cooldown 等待探测",
			"addr", addr, "base_cooldown", b.cfg.Egress.BanCooldown)
	}

	results := b.pool.Verify(ctx, target)
	for addr, err := range results {
		if err != nil {
			b.log.Warn("出口 IP 探测失败", "addr", addr, "err", err)
		}
	}

	// 探测结果通过 Stats 反映到指标，具体的状态机推进由 egress 包在
	// 真实请求路径上完成 —— 探测用的是独立连接，不足以判定「被封」。
	st := b.pool.Stats()
	if st.Active == 0 && st.Total > 0 {
		b.log.Error("全部出口 IP 均不可用，请求将失败", "total", st.Total, "banned", st.Banned)
	}
	return nil
}

// archiveDaily 归档当日用量到 key_daily_history。
func (b *background) archiveDaily(ctx context.Context) error {
	// 用配额日而非自然日（P0-3: 12:00 之前算作前一天）
	day := quota.QuotaDayTime(time.Now())

	agg, err := b.st.AggregateUsageByKey(ctx, day)
	if err != nil {
		return err
	}

	var n int
	for _, h := range agg {
		rec := h
		rec.QuotaDay = day
		if rec.TokenLimit <= 0 {
			rec.TokenLimit = b.cfg.Quota.TokenLimit
		}
		if rec.TokenLimit > 0 {
			rec.TokenRatio = float64(rec.TokenUsed) / float64(rec.TokenLimit)
		}
		if err := b.st.UpsertKeyDailyHistory(ctx, &rec); err != nil {
			b.log.Warn("归档 Key 日用量失败", "key_id", rec.VolcKeyID, "err", err)
			continue
		}
		n++
	}
	b.log.Info("当日用量归档完成", "quota_day", day.Format(time.DateOnly), "keys", n)
	return nil
}

// startRefresher 启动刷新探测器（P0-4）。
func (b *background) startRefresher(ctx context.Context) {
	rc := quota.RefresherConfig{
		WindowStart:   parseClock(b.cfg.Refresh.WindowStart, 12*time.Hour),
		WindowEnd:     parseClock(b.cfg.Refresh.WindowEnd, 14*time.Hour),
		ProbeInterval: b.dur(b.cfg.Refresh.ProbeInterval, 60*time.Second),
		TokenLimits:   quota.Limits{Hard: b.cfg.Quota.TokenHard(), Soft: b.cfg.Quota.TokenSoft()},
		CountLimits:   quota.Limits{Hard: b.cfg.Quota.CountHard(), Soft: b.cfg.Quota.CountSoft()},
		RampDuration:  time.Duration(b.cfg.Refresh.PostRefreshRampMinutes) * time.Minute,
	}

	// probe 用一次最小成本的真实请求判断额度是否已恢复。
	//
	// 这里必须是真实探测，不能是恒返回 false 的桩。原因是 Refresher 的状态机
	// 只在 idle / confirmed 两个状态放行调度（见 Schedulable）:
	//
	//	12:00 进窗口 → 全部 Key 置 pending（立即停止承接流量）
	//	          → 探测恒 false，卡在 probing
	//	          → 13:59 全部标记 failed
	//	          → 仅在窗口结束后才被清理
	//
	// 即恒 false 的桩不是「保守地不清零配额」，而是让整个 Key 池在
	// 12:00-14:00 停止服务两小时，所有用户请求返回 503。
	//
	// 判定规则（P0-4: 只探测事实，绝不按时间推测）:
	//   - 200            → 额度已恢复，确认刷新
	//   - 配额类错误      → 明确尚未恢复，保持等待
	//   - 其他错误/异常   → 无法判断，按未恢复处理（此时才是保守的正确选择）
	probe := newProbe(b.cfg, b.st, b.pool, b.log)

	r := quota.NewRefresher(rc, b.qm, probe, keyLister(b.st), b.log)

	b.every(ctx, "refresh_probe", b.dur(b.cfg.Refresh.ProbeInterval, 30*time.Second),
		2*time.Minute, r.Tick)

	b.log.Info("刷新探测器已启动",
		"window", b.cfg.Refresh.WindowStart+"-"+b.cfg.Refresh.WindowEnd,
		"probe_model", b.cfg.Refresh.ProbeModel,
		"ramp_minutes", b.cfg.Refresh.PostRefreshRampMinutes)
}
