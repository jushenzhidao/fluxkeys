package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/confsnap"
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
	// snaps 是配置的唯一来源。后台任务是进程级长生命周期对象，持一份
	// *config.Config 等于把启动时的配置钉死在这里 —— 热切后新增的 provider
	// 不会进入任何一轮循环，而循环本身照常报「执行成功」。
	snaps   *confsnap.Holder
	qm      *quota.Manager
	st      *store.Store
	sched   *scheduler.Scheduler
	pool    *egress.Pool
	metrics *metrics.Metrics
	log     *slog.Logger

	// shard 是本实例的机器分片标识，空串 = 单机模式不过滤。
	//
	// 后台任务必须与调度器同一装载口径: reconcile / 刷新探测按全量跑的话，
	// 每台机器都会对别的分片的 Key 做对账与探测 —— 探测请求真实消耗上游
	// 额度，N 台机器就是 N 倍探测成本，还会并发改同一个 Key 的刷新状态。
	shard string
}

// background 管理全部后台任务的生命周期。
type background struct {
	bgDeps
	wg     sync.WaitGroup
	cancel context.CancelFunc

	// refreshers 记录每个 provider 存活的探测器，供热切时增删与就地更新。
	// 只由 reconcileRefreshers / stopRefreshers 访问，两者都在同一个 goroutine
	// 里被调用（启动装配 + 热加载回调串行执行），故不需要额外加锁。
	refreshers map[string]*refresherHandle
}

// refresherHandle 是一个存活探测器的全部把手。
//
// 为什么除 cancel 外还要存 ref 和 rc:
//   - ref: 存量 provider 改了额度要能就地调 UpdateLimits。只有 cancel 就只能
//     「停掉重建」，而重建会清空状态机、中断正在进行的刷新流程。
//   - rc: 存的是该实例「构造时」的窗口参数。refresh.* 不热加载，但每次
//     reconcile 都会重算一份新 rc；两者不一致意味着存活实例与配置已分叉，
//     必须打 WARN。不留构造时的值就无从比对，分叉在运行期完全不可见。
type refresherHandle struct {
	cancel context.CancelFunc
	ref    *quota.Refresher
	rc     quota.RefresherConfig
}

func newBackground(d bgDeps) *background {
	return &background{bgDeps: d, refreshers: make(map[string]*refresherHandle)}
}

// snap 取一次快照，供单轮任务全程使用。
//
// 每轮取一次而非每次读取都取: 一轮归档要处理上千条记录，逐条取快照会让
// 同一天的归档结果前后按不同口径计算 —— 落库后无从分辨哪几条用了旧配置。
func (b *background) snap() *confsnap.Snapshot { return b.snaps.Current() }

// coldCfg 读启动期就固定的配置项（各任务的间隔、窗口）。
//
// 这些值在 start 时一次性读出来交给 ticker，本就不参与热切；单独开个方法
// 是为了让「这里读的是冷配置」在调用点看得出来，不至于被误当成漏改。
func (b *background) coldCfg() *config.Config { return b.snaps.Cfg() }

// start 启动全部后台任务。
//
// 返回 error 而非只记日志: 刷新探测器起不来会让 Key 池在刷新窗口后无法恢复
// 调度，属于必须在启动阶段就暴露的失败，不能让进程带着半残的后台任务运行。
func (b *background) start(parent context.Context) error {
	// 用独立的 cancel 而非直接用 parent: 关闭顺序要求后台任务在网关之后停
	// （网关关闭期间的流式请求仍在结束租约），而 parent 在收到信号时就已取消。
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	b.cancel = cancel

	// 各任务的间隔与开关在此一次性读出。它们决定 ticker 周期，改这些值本就
	// 要重启进程（冷配置）；热区的读取在每轮任务内部按 b.snap() 取。
	cfg := b.coldCfg()

	// 租约回收（P0-2 的第二条回收路径）。
	//
	// 前两条路径是「正常 Commit 修正」和「崩溃后的超时回收」，这个任务
	// 就是后者的执行者。没有它，任何进程崩溃或未捕获的 return 路径都会
	// 让 prededuct 永久占用额度，最终整个 Key 池看起来全满而实际空闲。
	b.every(ctx, "lease_reap", b.dur(cfg.Quota.ReapInterval, 30*time.Second),
		30*time.Second, b.reapLeases)

	// 配额快照刷新不在这里注册。
	//
	// sched.Start()（main.go 启动路径）已经起了同周期的内部刷新循环，
	// 这里再注册一个 quota_snapshot 任务就是双跑 —— 同一份 pipeline
	// HGetAll 每周期打两遍 Redis，1000 Key × 2 kind 规模下白白翻倍。

	// 配额对账（P0-2 的偏差检测）
	b.every(ctx, "reconcile", b.dur(cfg.Quota.ReconcileInterval, time.Hour),
		5*time.Minute, b.reconcile)

	// Key 池重载。
	//
	// 周期性重载而非依赖管理接口触发: 运维可能直接改库，也可能有另一个
	// 进程在导入 Key。5 分钟的延迟对「天」级变化的 Key 元数据完全够用。
	//
	// 重载后顺带清理下线 Key 的出口客户端: clients 映射在请求路径上只增
	// 不减，不清理就是 1000 Key 规模下的稳定慢泄漏（每个下线 Key 驻留
	// 一整套 Transport 连接池）。
	b.every(ctx, "key_reload", 5*time.Minute, time.Minute, func(ctx context.Context) error {
		if err := b.sched.Reload(ctx); err != nil {
			return err
		}
		if n := b.pool.RetainClients(b.sched.KeyIDSet()); n > 0 {
			b.log.Info("已清理下线 Key 的出口客户端", "count", n)
		}
		return nil
	})

	// 指标采集: Key 状态分布、配额水位、出口 IP 状态
	b.every(ctx, "metrics_collect", 15*time.Second, 30*time.Second, b.collectMetrics)

	// 出口 IP 健康检查（P1-6）
	if b.pool.Mode() == egress.ModeMultiIP {
		iv := b.dur(cfg.Egress.HealthCheckInterval, 5*time.Minute)
		b.every(ctx, "egress_health", iv, time.Minute, b.checkEgress)
	}

	// 刷新探测（P0-4: 不假设刷新时刻，只探测「是否已刷新」）
	//
	// 走 reconcile 而非一次性 start: 同一个函数既负责启动装配，也负责热切后
	// 把探测器对齐到新配置。两条路径共用一份代码，才不会出现「启动时正确、
	// 热切后漏建」这种只在运行几小时后才暴露的偏差。
	if !cfg.Refresh.Enabled {
		b.log.Warn("刷新探测器已禁用，配额将只依赖 Redis key 的自然过期（P0-4）")
	}
	if err := b.reconcileRefreshers(ctx); err != nil {
		return fmt.Errorf("启动刷新探测器: %w", err)
	}

	// 每日归档: 把当日用量汇总进 key_daily_history，供调度的 S_history 使用
	b.every(ctx, "daily_archive", time.Hour, 5*time.Minute, b.archiveDaily)

	return nil
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
	snap := b.snap()

	batch := snap.Cfg.Quota.ReapBatch
	if batch <= 0 {
		batch = 500
	}

	// 遍历所有 providers 进行回收。
	//
	// 必须走快照而非启动时的配置: 热切后新增的 provider 若不在这个循环里，
	// 它的租约永远不会被回收，prededuct 只增不减，该上游的 Key 会逐步被
	// 锁死到完全不可调度 —— 而回收任务每轮都报成功，日志上看不出任何异常。
	totalReaped := int64(0)
	for provider := range snap.Cfg.Providers {
		n, err := b.qm.Reap(ctx, provider, batch)
		if err != nil {
			return err
		}
		totalReaped += n
	}

	if totalReaped > 0 {
		b.metrics.ObserveReap(totalReaped)
		// 用 Info 而非 Debug: 持续有租约被超时回收，说明有请求路径没有
		// 正常结束租约，这是需要人看到的信号而非常态噪声。
		b.log.Info("回收过期租约", "count", totalReaped)
	}
	return nil
}

// reconcile 对全部 provider 执行配额对账。
//
// 对账的意义: 租约回收依赖 Redis 有序集合的过期扫描，若某条记录因异常
// 未进入该集合，超时回收就会漏掉它，prededuct 永久偏高。对账以
// 「未过期租约之和」强制重算 prededuct，是最后一道自愈手段。
//
// 每 provider 一次 ReconcileProvider 调用，扫描与修正整体在单个 Lua 内
// 原子完成 —— 既消除了旧实现「Go 侧求和、Lua 覆写」跨 RTT 的竞态
// （会抹掉在途 Acquire 的预扣，偏差方向是超刷），也把 O(Key×租约) 的
// N+1 往返压成 O(Key+租约) 的单次往返。
func (b *background) reconcile(ctx context.Context) error {
	keys, err := b.st.ListUpstreamKeys(ctx, store.UpstreamKeyFilter{Shard: b.shard})
	if err != nil {
		return err
	}

	// 按 provider 分组
	byProvider := make(map[string][]string)
	for _, k := range keys {
		byProvider[k.Provider] = append(byProvider[k.Provider], k.KeyID)
	}

	day := quota.QuotaDayTime(time.Now())
	var checked, drifted int
	for provider, ids := range byProvider {
		if ctx.Err() != nil {
			// 关停中的取消不是失败，但也不能谎报「对账完成」。
			// every() 循环在 ctx 取消后不会把该错误记为告警。
			return ctx.Err()
		}
		drifts, err := b.qm.ReconcileProvider(ctx, provider, ids)
		if err != nil {
			b.log.Warn("对账失败", "provider", provider, "err", err)
			continue
		}
		checked += len(ids)
		for _, d := range drifts {
			drifted++
			b.metrics.ObserveDrift(string(d.Kind), d.Amount)
			b.log.Warn("配额偏差已修正",
				"key_id", d.KeyID, "kind", d.Kind, "drift", d.Amount)

			// 偏差落库供看板与事后分析。写失败不影响修正本身 ——
			// 修正已在 Redis 完成，这里只是留痕。
			if err := b.st.InsertQuotaDrift(ctx, store.QuotaDrift{
				UpstreamKeyID: d.KeyID, Provider: provider,
				BillingKind: string(d.Kind),
				QuotaDay:    day, Drift: d.Amount,
			}); err != nil {
				b.log.Warn("记录配额偏差失败", "key_id", d.KeyID, "err", err)
			}
		}
	}

	b.log.Info("配额对账完成", "checked", checked, "drifted", drifted)
	return nil
}

// collectMetrics 采集 Key 状态、配额水位与出口 IP 状态。
func (b *background) collectMetrics(ctx context.Context) error {
	keys, err := b.st.ListUpstreamKeys(ctx, store.UpstreamKeyFilter{Shard: b.shard})
	if err != nil {
		return err
	}

	health := b.sched.HealthAll()
	byStatus := make(map[string]int, 4)
	byPool := make(map[string]int, 3)

	// 按 provider 分组 key IDs
	keysByProvider := make(map[string][]string)
	allIDs := make([]string, 0, len(keys))

	for _, k := range keys {
		status := k.Status
		// 内存健康状态比库里的落盘值新，冷却/封禁这类瞬时状态只存在于内存
		if h, ok := health[k.KeyID]; ok {
			status = string(h.Status)
		}
		byStatus[status]++
		byPool[k.Pool]++
		allIDs = append(allIDs, k.KeyID)
		keysByProvider[k.Provider] = append(keysByProvider[k.Provider], k.KeyID)
	}
	// Reset 语义的快照式覆盖: 增量更新漏一次状态迁移会造成永久偏差
	b.metrics.SetKeyDistribution(byStatus, byPool)

	// 配额水位按 key_id 展开。这是唯一按 key_id 打标签的指标 ——
	// 1000 个 Key 产生 2000 条序列，可接受；再多维度就会爆。
	for _, kind := range []quota.Kind{quota.KindToken, quota.KindCount} {
		// 遍历所有 providers，分别获取配额快照
		for provider, ids := range keysByProvider {
			snaps, err := b.qm.GetMany(ctx, provider, ids, kind)
			if err != nil {
				b.log.Warn("读取配额快照失败", "provider", provider, "kind", kind, "err", err)
				continue
			}
			for id, s := range snaps {
				b.metrics.SetQuotaRatio(id, string(kind), s.Ratio())
			}
		}
	}

	// 清理已删除 Key 的序列，否则指标序列只增不减
	live := make(map[string]bool, len(allIDs))
	for _, id := range allIDs {
		live[id] = true
	}
	b.forgetStaleKeys(live)

	// 出口绑定按库对账（livetest-ai KI-035）。
	//
	// live 与上一行同源，是「库中仍然存在的 Key」的全集 —— 注意**不限状态**:
	// banned 的 Key 也在内，它的绑定必须留着，复活时要回同一个出口。
	// 差别在于 forgetStaleKeys 回收的是指标序列，这一步回收的是**出口的可绑定
	// 名额占用**: 绑定的权威副本在网关内存，库里的行被直接删掉（SQL、另一个
	// 进程、看板）后内存不感知，两个出口的 load 就永久停在历史值上 ⇒
	// 候选集恒空、新导入的 Key 全部 502，而报错文案指向「档位无可用出口」
	// 这个根本不存在的成因。
	//
	// 刻意放在 15 秒一轮的指标采集里，而不是 5 分钟一轮的 key_reload:
	// 复用本轮已经查出来的 key 列表（零额外查询），并把「容量假满」的持续
	// 窗口从分钟级压到十几秒。key_reload 那侧的 RetainClients 只清客户端、
	// 保留绑定，语义不同，两者不能合并。
	if n := b.pool.ReleaseBindings(live); n > 0 {
		b.log.Warn("已回收库中不存在的 Key 的出口绑定",
			"count", n, "catalog_keys", len(live),
			"hint", "库中已无这些 Key（多为直接改库删行）；"+
				"直接改库不会立刻反映到内存容量，请改用 DELETE /admin/keys/{key_id}（删行与解绑同事务）")
	}

	// 反向对账: 内存绑定回写库（livetest-ai KI-037）。
	//
	// 与 ReleaseBindings 方向相反、语义互补 —— 那条回收「库中已不存在」的
	// 绑定（库是真相），这条回写「库中存在、但绑定值与内存不一致」的 Key
	// （内存是真相: 请求已经从这个出口发出去了，此刻改内存等于给账号换 IP）。
	// 惰性绑定（热路径首次分配 / 原出口被封后的重绑）在 proxy.go 里随请求
	// 落库，本循环只是它失败时的兜底 —— 不兜底的话，那一笔落库失败会留下
	// 永久不一致: 重启后 restoreBindings 按库里的空值/旧值恢复，Key 换出口。
	//
	// 复用本轮已查出的 Key 列表（零额外查询），15s 一轮把不一致窗口压在
	// 十几秒。判定本体在 reconcileEgressBindings，可脱离采集循环单独测试。
	if n := b.reconcileEgressBindings(ctx, keys); n > 0 {
		b.log.Info("内存出口绑定已回写库", "count", n, "catalog_keys", len(keys))
	}

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

// reconcileEgressBindings 把「内存有、库里不一致」的出口绑定回写库，
// 返回实际回写的条数。
//
// 真相方向是内存 → 库，与 ReleaseBindings（库 → 内存）严格互补:
// 请求已经从内存里的这个出口发出去了，绑定在内存里是既成事实；库只是
// 它在重启后的存档点。所以这里绝不拿库值去改内存 —— 那等于给账号换 IP。
//
// 只回写「内存非空且与库值不同」的 Key:
//   - 内存为空: 该 Key 尚未绑定（冷 Key 或刚导入未承接请求），无可写。
//   - 与库值相同: 已一致，跳过 —— 每轮全量重写会无意义地刷 updated_at。
//
// 写失败不告警升级: ErrNotFound 说明 Key 在查询之后被删，下一轮
// ReleaseBindings 会回收内存侧，属正常竞态；其余错误记 WARN 后交给下一轮
// 重试，15s 一轮的周期本身就是退避。
func (b *background) reconcileEgressBindings(ctx context.Context, keys []store.UpstreamKey) int {
	var persisted int
	for _, k := range keys {
		bound := b.pool.BoundIP(k.KeyID)
		if bound == "" || bound == k.EgressIP {
			continue
		}
		if err := b.st.UpdateUpstreamKeyState(ctx, k.KeyID, store.UpstreamKeyState{EgressIP: &bound}); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				b.log.Warn("出口绑定回写库失败", "key_id", k.KeyID, "egress_ip", bound, "err", err)
			}
			continue
		}
		persisted++
	}
	return persisted
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
	snap := b.snap()

	// 目标从配置或实际 provider base_url 推导，绝不写死某个厂商域名 ——
	// 拨测无关域名会让自检在真实上游拉黑该出口时依然全绿，
	// 恰好在最需要它报警的时候失效。
	//
	// 因为要回落到 provider base_url，这一项必须走快照: 上游域名换了而拨测
	// 还在打旧域名，自检就退化成了「测一个我们已经不用的地址」。
	target := snap.Cfg.EgressVerifyTarget()
	if target == "" {
		b.log.Warn("无法确定出口自检目标（未配 verify_target 且 provider base_url 不可解析），跳过本轮探测")
		return nil
	}

	// 顺序不可换: 先解封再探测。
	//
	// Verify 对所有 IP 探测并在成功时调 MarkSuccess，但 MarkSuccess 只把
	// suspect / cooldown 转回 active —— banned 不在其中。若先探测，被封出口
	// 即使探测成功也仍是 banned，自动恢复永远不会发生。
	for _, addr := range b.pool.TryUnbanAll(
		snap.Cfg.Egress.BanCooldown, snap.Cfg.Egress.BanCooldownMax) {
		b.log.Info("被封出口冷却期届满，转入 cooldown 等待探测",
			"addr", addr, "base_cooldown", snap.Cfg.Egress.BanCooldown)
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

	// 整轮归档共用一份快照，理由见 background.snap。
	snap := b.snap()

	var n int
	for i := range agg {
		rec := agg[i]
		rec.QuotaDay = day
		if rec.Provider == "" {
			// 流水里没带 provider 的只可能是改造前的历史数据。用兜底 provider
			// 补齐而不是丢弃 —— 归档喂给 S_history，缺一天会让打分误判成「很闲」。
			rec.Provider = snap.Cfg.ResolveProvider("")
		}
		// 计费口径决定 ratio 的分子分母都取哪一路。按次计费的上游（商汤公测
		// 按调用次数给额度）如果沿用 token 量纲，会拿「用掉的 token 数」去除
		// 「按次额度」—— 实测 token_used=113 / count 额度 1260 得出 0.09，
		// 看起来是个正常的低负载值，实际这个 Key 已经用掉了 4/1260 次。
		// 两个数都真实、比值也落在合法区间，没有任何一层会报错。
		kindCount := snap.Cfg.IsCountProvider(rec.Provider)
		used := rec.TokenUsed
		if kindCount {
			used = int64(rec.CountUsed)
		}
		if rec.TokenLimit <= 0 {
			// 水位按 provider 取: ratio 是「这个 Key 用掉了自己额度的几成」，
			// 用全局值算会让额度小的上游永远显示成低负载，调度持续往它上面压。
			rec.TokenLimit, _ = snap.Cfg.LimitsFor(rec.Provider, kindCount)
		}
		if rec.TokenLimit > 0 {
			rec.TokenRatio = float64(used) / float64(rec.TokenLimit)
		}
		if err := b.st.UpsertKeyDailyHistory(ctx, &rec); err != nil {
			b.log.Warn("归档 Key 日用量失败", "key_id", rec.UpstreamKeyID, "err", err)
			continue
		}
		n++
	}
	b.log.Info("当日用量归档完成", "quota_day", day.Format(time.DateOnly), "keys", n)
	return nil
}

// ReconcileRefreshers 把刷新探测器对齐到当前快照，供配置热加载换入新快照后调用。
//
// 不做这一步的后果是单向的**静默偏差**：新增的 provider 永远没有探测器（它的 Key
// 在 12:00 之后不会被确认刷新，额度恢复也无从得知），而被移除的 provider 的探测器
// 会继续按旧配置跑，对一个「配置上已经不存在」的上游持续产生副作用。
//
// 它是幂等的（内部按快照里的 provider 集合求差集），因此重复调用安全。
func (b *background) ReconcileRefreshers(ctx context.Context) error {
	return b.reconcileRefreshers(ctx)
}

// reconcileRefreshers 把运行中的刷新探测器对齐到当前快照（P0-4）。
//
// 这是本文件唯一需要副作用编排的点。其他任务都是「每轮重新读一次配置」，
// 天然跟得上热切；探测器是每 provider 一个常驻协程，配置变了必须真的起/停
// 协程，不是换个读取来源就完事。
//
// 幂等: 已在运行的 provider 跳过，快照里消失的 provider 停掉。可以在启动时
// 和每次热切后重复调用。
//
// 返回 error 而非只记日志: 探测器起不来意味着该上游在 12:00-14:00 窗口内不会
// 恢复调度 —— 表现是「到点了 Key 还是 503」，而进程一切正常。这种失败必须
// 能被调用方看到并向上报，不能吞掉。
func (b *background) reconcileRefreshers(ctx context.Context) error {
	snap := b.snap()
	cfg := snap.Cfg

	if !cfg.Refresh.Enabled {
		// 从启用改为禁用时要把已起的协程停掉，否则它们会带着旧配置继续探测。
		b.stopRefreshers(nil)
		return nil
	}

	rc := quota.RefresherConfig{
		WindowStart:   parseClock(cfg.Refresh.WindowStart, 12*time.Hour),
		WindowEnd:     parseClock(cfg.Refresh.WindowEnd, 14*time.Hour),
		ProbeInterval: b.dur(cfg.Refresh.ProbeInterval, 60*time.Second),
		RampDuration:  time.Duration(cfg.Refresh.PostRefreshRampMinutes) * time.Minute,
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
	//
	// 探测器按 provider 建，probe 也必须按 provider 建 —— 见 newProbe 的注释:
	// 共用一个探测器等于拿某个上游的 model_mapping 去探测所有上游的 Key。
	probes, err := newProbes(snap, b.st, b.pool, b.log)
	if err != nil {
		return err
	}

	var errs []error
	started := make(map[string]bool, len(cfg.Providers))

	// 为每个 provider 创建独立的 Refresher 实例
	for provider := range cfg.Providers {
		started[provider] = true

		// 水位按 provider 取，不能共用循环外的一份 —— 刷新确认后写回的
		// hard_limit 决定该 Key 后续能放行多少量。用全局值会让额度小的
		// 上游在刷新后被重置成一个远超真实额度的水位，等于把准入防线拆了。
		tHard, tSoft := cfg.LimitsFor(provider, false)
		cHard, cSoft := cfg.LimitsFor(provider, true)
		tokenLim := quota.Limits{Hard: tHard, Soft: tSoft}
		countLim := quota.Limits{Hard: cHard, Soft: cSoft}

		if h := b.refreshers[provider]; h != nil {
			// 已在运行。不重启: 重启会丢掉状态机当前所处的窗口阶段，
			// 正在 probing 的 provider 会被打回 idle 并重新走一遍确认流程。
			//
			// 但「不重启」不等于「不更新」。存活实例内部持的是构造时的水位
			// 值拷贝，不推新值进去的话，改完额度看似生效（库里、界面、调度
			// 都是新值），直到当天刷新窗口 confirm 时把 hard_limit 写回成
			// 旧值 —— 表现为「额度每天中午自己变回去」，且改配置与症状之间
			// 隔着几小时，几乎无法归因到这里。
			h.ref.UpdateLimits(tokenLim, countLim)

			// refresh.* 不热加载，但 rc 每轮都重算。存活实例仍用旧窗口，
			// 新建实例会用新窗口 —— 两批 Key 在不同时刻进入 pending（进
			// pending 即停止承接流量）。这条 WARN 是该分叉唯一的可观测出口。
			if h.rc.WindowStart != rc.WindowStart || h.rc.WindowEnd != rc.WindowEnd ||
				h.rc.ProbeInterval != rc.ProbeInterval || h.rc.RampDuration != rc.RampDuration {
				b.log.Warn("refresher rc 分叉（refresh.* 变更不热加载，需重启网关使全部 refresher 对齐）",
					"provider", provider,
					"存活窗口", clockStr(h.rc.WindowStart)+"-"+clockStr(h.rc.WindowEnd),
					"当前配置窗口", clockStr(rc.WindowStart)+"-"+clockStr(rc.WindowEnd),
					"存活探测间隔", h.rc.ProbeInterval,
					"当前探测间隔", rc.ProbeInterval,
					"存活限速时长", h.rc.RampDuration,
					"当前限速时长", rc.RampDuration)
			}
			continue
		}

		probe, ok := probes[provider]
		if !ok {
			errs = append(errs, fmt.Errorf("provider %s 无可用探测器", provider))
			continue
		}

		// 为该 provider 创建专用的 KeyLister
		providerLister := func(p string) quota.KeyLister {
			return func(ctx context.Context) ([]string, error) {
				keys, err := b.st.ListUpstreamKeys(ctx, store.UpstreamKeyFilter{
					Provider: p,
					Status:   store.KeyStatusActive,
					Shard:    b.shard,
				})
				if err != nil {
					return nil, err
				}
				ids := make([]string, len(keys))
				for i, k := range keys {
					ids[i] = k.KeyID
				}
				return ids, nil
			}
		}(provider)

		prc := rc
		prc.TokenLimits = tokenLim
		prc.CountLimits = countLim

		r := quota.NewRefresher(provider, prc, b.qm, probe, providerLister, b.log)

		// 独立 cancel: 该 provider 从配置里移除时要能单独停掉它的探测协程，
		// 不影响其他上游。
		rctx, cancel := context.WithCancel(ctx)
		b.every(rctx, "refresh_probe_"+provider, b.dur(cfg.Refresh.ProbeInterval, 30*time.Second),
			2*time.Minute, r.Tick)
		// 存 rc 而非 prc: 比对的是「窗口类参数是否分叉」，而 limits 两项本就
		// 每轮就地更新、永不分叉。存 prc 会让比对把 limits 差异也算进来。
		b.refreshers[provider] = &refresherHandle{cancel: cancel, ref: r, rc: rc}

		b.log.Info("刷新探测器已启动",
			"provider", provider,
			"window", cfg.Refresh.WindowStart+"-"+cfg.Refresh.WindowEnd,
			"probe_model", cfg.Refresh.ProbeModel,
			"ramp_minutes", cfg.Refresh.PostRefreshRampMinutes)
	}

	// 快照里已消失的 provider: 停掉它的探测协程。
	//
	// 不停的后果是这个协程继续按旧配置调 qm.Reap / 写回 hard_limit，
	// 对一个「配置上已经不存在」的上游持续产生副作用。
	b.stopRefreshers(started)

	return errors.Join(errs...)
}

// stopRefreshers 停掉不在 keep 集合里的探测协程。keep 为 nil 表示全停。
func (b *background) stopRefreshers(keep map[string]bool) {
	for provider, h := range b.refreshers {
		if keep[provider] {
			continue
		}
		h.cancel()
		delete(b.refreshers, provider)
		b.log.Info("刷新探测器已停止（provider 已从配置中移除）", "provider", provider)
	}
}
