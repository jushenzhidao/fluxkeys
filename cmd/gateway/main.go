// Command gateway 是 FluxKeys 网关主程序。
//
// 本文件只做一件事: 按正确顺序装配组件、驱动后台循环、优雅关闭。
// 所有业务决策都在 internal/* 里，这里不含任何策略逻辑。
//
// 装配顺序不是随意的:
//
//	配置 → 校验 → Redis → 配额管理器 → Postgres → 迁移 → 出口池 →
//	出口自检 → 调度器 → 刷新探测器 → 后台循环 → HTTP 服务
//
// 其中两处顺序是强制的:
//  1. 配置校验必须在任何连接建立之前 —— 配置自身的矛盾（如
//     reap_interval >= lease_ttl）会让租约永远回收不及时，属于 P0 级隐患，
//     必须在启动阶段就失败，而不是运行时才暴露。
//  2. 出口自检必须在 HTTP 服务监听之前 —— multi_ip 模式下策略路由未配置时，
//     绑定会「静默失效」（不报错，流量照走主 IP）。一旦开始接流量再发现，
//     所有 Key 已经共享同一出口 IP，反封禁设计当场归零。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fluxkeys/fluxkeys/internal/adapter"
	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/confsnap"
	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/gateway"
	"github.com/fluxkeys/fluxkeys/internal/metrics"
	"github.com/fluxkeys/fluxkeys/internal/quota"
	"github.com/fluxkeys/fluxkeys/internal/scheduler"
	"github.com/fluxkeys/fluxkeys/internal/store"
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	if err := run(); err != nil {
		// 启动失败必须以非零码退出并打印原因: 容器编排依赖退出码判断
		// 是否重启，日志是唯一的排障线索。
		fmt.Fprintf(os.Stderr, "fluxkeys 启动失败: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		cfgPath     = flag.String("config", "", "配置文件路径（留空则使用默认值 + 环境变量）")
		showVersion = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("fluxkeys gateway", version)
		return nil
	}

	log := newLogger()

	// ===== 1. 配置 =====
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("加载配置: %w", err)
	}
	// Validate 会拒绝自相矛盾的配置。这一步失败必须终止启动 ——
	// 带着矛盾配置跑起来，故障会推迟到最坏的时刻才暴露。
	if err = cfg.Validate(); err != nil {
		return fmt.Errorf("配置校验: %w", err)
	}

	// 收集 provider 名称
	providers := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		providers = append(providers, name)
	}

	log.Info("fluxkeys 启动",
		"version", version,
		"addr", cfg.Server.Addr,
		"egress_mode", cfg.Egress.Mode,
		"providers", providers,
		"quota_day", quota.QuotaDay(time.Now()),
	)

	// 顶层 context: 收到信号后取消，所有后台循环随之退出。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	m := metrics.New()

	// ===== 2. Redis 与配额管理器 =====
	//
	// Redis 是配额的唯一权威写路径（P0-1）。它不可用时网关无法保证不超刷，
	// 因此启动阶段就必须连通 —— 「先起来再说」等于放弃正确性保证。
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
		PoolSize: cfg.Redis.PoolSize,
	})
	defer func() { _ = rdb.Close() }()

	pingCtx, cancelPing := context.WithTimeout(ctx, 10*time.Second)
	defer cancelPing()
	if err = rdb.Ping(pingCtx).Err(); err != nil {
		return fmt.Errorf("连接 Redis %s: %w", cfg.Redis.Addr, err)
	}
	log.Info("Redis 已连接", "addr", cfg.Redis.Addr)

	qm, err := quota.NewManager(ctx, rdb)
	if err != nil {
		return fmt.Errorf("初始化配额管理器: %w", err)
	}

	limiter, err := gateway.NewRateLimiter(ctx, rdb)
	if err != nil {
		return fmt.Errorf("初始化限流器: %w", err)
	}

	// ===== 3. Postgres =====
	st, err := store.New(ctx, cfg.Postgres)
	if err != nil {
		return fmt.Errorf("连接 Postgres: %w", err)
	}
	defer func() {
		// 关闭前刷盘: 用量流水是异步批量写的，缓冲区里可能还有未落库的记录。
		// 直接 Close 会丢掉这批数据，而它是计费依据。
		flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err = st.FlushUsage(flushCtx); err != nil {
			log.Error("关闭前刷写用量流水失败", "error", err)
		}
		if err = st.Close(); err != nil {
			log.Error("关闭 Postgres 失败", "error", err)
		}
	}()

	if cfg.Postgres.AutoMigrate {
		if err = st.Migrate(ctx); err != nil {
			return fmt.Errorf("执行数据库迁移: %w", err)
		}
		log.Info("数据库迁移完成")
	}

	// provider 配置首次入库。必须在迁移之后、装配之前 ——
	// 表空时若不 seed，进程会带着零个 provider 正常启动，
	// healthz/readyz 全绿而所有业务请求失败。详见 provider_seed.go。
	if _, err = seedProviderConfigs(ctx, st, cfg, log); err != nil {
		return fmt.Errorf("导入 provider 配置: %w", err)
	}

	// ===== 4. 出口池 =====
	pool, err := buildEgressPool(cfg)
	if err != nil {
		return fmt.Errorf("构建出口池: %w", err)
	}
	defer pool.CloseIdle()

	// 为已有 Key 恢复绑定。Bind 用哈希做确定性分配，重启后同一 Key 仍落到
	// 同一 IP —— 出口 IP 漂移会让火山侧看到「同一账号换了机器」。
	if pool.Mode() == egress.ModeMultiIP {
		if err = restoreBindings(ctx, st, pool, cfg.Server.ShardID, log); err != nil {
			return fmt.Errorf("恢复 Key-IP 绑定: %w", err)
		}
	}

	// 出口自检必须在开始接流量之前。见文件头注释第 2 点。
	if cfg.Egress.VerifyOnStart {
		if err = verifyEgress(ctx, cfg, pool, log); err != nil {
			return err
		}
	}

	// ===== 5. 配置快照 =====
	//
	// 所有 provider 相关的读取都经由它，热切时整体原子替换。
	//
	// 建在调度器之前: 调度器、后台任务、网关三方必须拿到同一个 Holder，
	// 各自留一份配置副本就等于各自在不同时刻定格 —— 热切后三者行为不一致，
	// 而每一方单看都正常。version 从 0 起，由热加载路径接管后覆盖。
	snaps, err := confsnap.NewHolderFromConfig(cfg, 0)
	if err != nil {
		return fmt.Errorf("构建配置快照: %w", err)
	}

	// ===== 6. 调度器 =====
	//
	// 存储包一层分片作用域: ShardID 非空时调度器只装载本机分片的 Key。
	// 多机部署下每台机器持有独立的 Key 分片与出口 IP 池，Key 跟着 IP 走、
	// 永不跨机 —— 跨机等于换出口，正是风控最敏感的「老账号换 IP」信号。
	// ShardID 为空时透传，单机行为不变。
	schedStore := shardScopedStore{st: st, shard: cfg.Server.ShardID}
	if cfg.Server.ShardID != "" {
		log.Info("多机分片模式已启用", "shard_id", cfg.Server.ShardID)
		warnUnshardedKeys(ctx, st, log)
	}
	qr := quotaReader{qm: qm}
	sched := scheduler.New(confsnap.SchedConfig{H: snaps}, schedStore, qr)

	// 注入出口只读视图，启用出口级最小间隔约束。
	//
	// 顺序上必须在 Reload 之前 —— 装载完 Key 才注入的话，首轮 Select
	// 会在没有出口视图的情况下放行，出口密度不受约束。
	sched.SetEgressReader(pool)

	// 启动时必须显式装载一次 Key 池。
	//
	// Start() 只起配额快照循环，不读 Key 表；后台 key_reload 首轮还带
	// StaggerOffset 错峰延迟，间隔 5 分钟。少了这一次装载，网关重启后会
	// 在长达数分钟里对所有请求返回 503，而健康检查一路 200 ——
	// 从外部看完全正常，只是不干活。
	//
	// 装载失败不阻止启动: Key 表暂时读不到时仍应把服务拉起来，
	// 让 /readyz 与后台重载去反映真实状态，比整个进程起不来更容易运维。
	if err = sched.Reload(ctx); err != nil {
		log.Error("启动装载 Key 池失败，将由后台重载重试", "error", err)
	}

	sched.Start(ctx)
	defer sched.Stop()

	// 把容量上限算给运维看。
	//
	// 吞吐上限 = 活跃 Key 数 / MinRequestInterval，超出部分会被调度器以
	// 「间隔不足」拒绝。这个数不打出来，运维就只能在压测时通过 503 反推
	// —— 而 503 的文案看起来像配额问题，会把人引向完全错误的方向。
	poolSize := sched.PoolSize()
	if poolSize == 0 {
		log.Warn("Key 池为空，网关将对所有业务请求返回 503；" +
			"请通过 POST /admin/keys 导入上游 Key")
	} else {
		// 两个口径都打: 池子总量决定日配额总额，此刻可调度量决定当下吞吐。
		// 启用画像时后者会随时段浮动，只报总量会让人高估容量。
		schedulable := sched.SchedulableAt(time.Now())
		log.Info("Key 池已装载",
			"active_keys", poolSize,
			"schedulable_now", schedulable,
			"min_request_interval", cfg.Scheduler.MinRequestInterval,
			"max_qps_now", fmt.Sprintf("%.1f", cfg.Scheduler.MaxQPS(schedulable)))

		// 可调度量明显低于池子总量时点出原因，否则运维会以为 Key 丢了。
		if cfg.Scheduler.EnablePersona && schedulable*2 < poolSize {
			log.Warn("当前时段可调度 Key 不足池子的一半，系统吞吐将低于预期",
				"active_keys", poolSize, "schedulable_now", schedulable,
				"hint", "这是行为画像的时段过滤所致（scheduler.enable_persona）；"+
					"若确实需要全天满负荷可关闭画像，但会削弱反封禁效果")
		}
	}

	schedAdapter := &schedulerAdapter{
		sched: sched, st: st, qm: qm,
		cfg: quotaLimits{snaps: snaps},
	}
	storeAdapter := &storeAdapter{st: st}

	// ===== 7. 后台任务 =====
	//
	// 全部后台循环统一由 background 托管（见 background.go）。这里不再单独
	// 起 reap/refresh 协程 —— 那样会让租约回收同时跑两遍，更要紧的是两套
	// 循环的生命周期不同:
	//
	// 直接挂在 ctx 上的循环会在收到退出信号的瞬间退出，而此刻优雅关闭才
	// 刚开始，仍有流式请求在收尾并归还租约。回收器比网关先死，这批租约
	// 只能等下次启动后由超时路径兜底。background 用
	// WithoutCancel(parent) + 显式 stop()，保证后台任务活得比网关久。
	//
	// 这些任务承载正确性职责，不是可选的运维增强:
	//   - lease_reap:    P0-2 租约回收。不跑则 SSE 断连的预扣永久泄漏，
	//                    可用额度逐日缩水直到该 Key 完全不可用。
	//   - refresh_probe: P0-4 刷新探测。不跑则 12:00 后不会恢复调度。
	bg := newBackground(bgDeps{
		snaps: snaps, qm: qm, st: st, sched: sched,
		pool: pool, metrics: m, log: log,
		shard: cfg.Server.ShardID,
	})
	if err = bg.start(ctx); err != nil {
		return fmt.Errorf("启动后台任务: %w", err)
	}

	// 把配置热加载所需的依赖补进 storeAdapter。
	//
	// 必须在这里（bg 之后）而不是第 266 行就地构造：afterSwap 要指向
	// bg.ReconcileRefreshers，而 bg 在那一行还不存在；少了它，热加载换入的新
	// provider 不会有刷新探测器，被移除的探测器也不会停。
	//
	// 这四个字段为空时管理面的写操作会退回「reloaded=false 且不生效」的旧形态
	// （ReloadProviderConfig 返回 errReloadUnavailable），不会 panic。
	storeAdapter.snaps = snaps
	storeAdapter.base = cfg
	storeAdapter.afterSwap = bg.ReconcileRefreshers
	storeAdapter.log = log

	// ===== 8. HTTP 服务 =====
	//
	// adapter registry 不在这里建 —— 它由 confsnap.Build 与 config 绑成同一个
	// 快照对象。两处分别构造会让热切后出现「换了 base_url 但 adapter 还持着
	// 旧 model_mapping」，请求会带着错误的模型名成功发出去。
	srv, err := gateway.New(gateway.Deps{
		Config:  cfg,
		Snaps:   snaps,
		Quota:   qm,
		Egress:  pool,
		Sched:   schedAdapter,
		Store:   storeAdapter,
		Limiter: limiter,
		Metrics: m,
		Logger:  log,
		RedisPing: func(ctx context.Context) error {
			return rdb.Ping(ctx).Err()
		},
	})
	if err != nil {
		return fmt.Errorf("构建网关: %w", err)
	}

	gwErr := srv.Start()
	log.Info("网关已监听", "addr", cfg.Server.Addr)

	var metricsSrv *metrics.Server
	var metricsErr <-chan error
	if cfg.Server.MetricsAddr != "" {
		metricsSrv = metrics.NewServer(cfg.Server.MetricsAddr, m)
		metricsErr = metricsSrv.Start()
		log.Info("指标端点已监听", "addr", cfg.Server.MetricsAddr, "path", "/metrics")
	}

	// ===== 9. 等待退出 =====
	select {
	case <-ctx.Done():
		log.Info("收到退出信号，开始优雅关闭")
	case err := <-gwErr:
		// http.ErrServerClosed 是 Shutdown 的正常返回，不是故障。
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("网关异常退出: %w", err)
		}
	case err := <-metricsErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("指标服务异常退出", "error", err)
		}
	}

	// 关闭用独立 context: 顶层 ctx 已被信号取消，用它做超时控制会
	// 立即到期，等于跳过优雅关闭直接掐断进行中的流式请求。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("网关关闭超时，仍有请求未完成", "error", err)
	}

	// 顺序不可交换: 必须等网关的进行中请求全部收尾后才停后台任务。
	//
	// 反过来先停后台，最后一批流式请求在关闭期间归还的租约就没有回收器
	// 处理了 —— 那部分预扣会一直占额度到下次启动后被超时路径捡走。
	bg.stop()

	// 指标端点最后关: 关闭过程本身的指标（如 reap 的最后一轮计数）也应
	// 有机会被采集走。
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutdownCtx)
	}

	log.Info("已退出")
	return nil
}

// newLogger 构造结构化日志器。
//
// 用 JSON 而非文本格式: 生产日志必须可被检索。级别由 FLUXKEYS_LOG_LEVEL
// 控制，默认 info。
func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("FLUXKEYS_LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}

// buildEgressPool 按配置构造出口池。
func buildEgressPool(cfg *config.Config) (*egress.Pool, error) {
	mode := egress.Mode(cfg.Egress.Mode)

	var ips []*egress.IP
	if mode == egress.ModeMultiIP {
		for _, ic := range cfg.Egress.IPs {
			ips = append(ips, egress.NewPooledIP(ic.Addr, ic.PublicIP, ic.MaxKeys, ic.Pool))
		}
	}
	pool, err := egress.NewPool(mode, ips, cfg.Egress.RequestTimeout)
	if err != nil {
		return nil, err
	}
	// 与后台探测任务用同一对参数，让 GET /admin/ips 报出的「预计解封时刻」
	// 与实际恢复行为一致。不设的话 Stats 会把可自愈的出口报成永久封禁。
	pool.SetUnbanPolicy(cfg.Egress.BanCooldown, cfg.Egress.BanCooldownMax)
	return pool, nil
}

// restoreBindings 为库中已有的活跃 Key 恢复出口绑定。
//
// 不做这一步的后果: 新请求到达时才惰性绑定，而绑定顺序取决于请求到达顺序，
// 重启后同一 Key 可能落到不同 IP。对火山侧而言就是「这个账号换了出口」。
//
// 恢复顺序必须是「库中记录优先，其次才是重新分配」:
// 仅调 Bind 并不足以保证终身绑定 —— 哈希落点取决于当时的候选集，
// 而候选集会随 IP 增删、封禁、绑定顺序而变化。只有采纳 volc_keys.egress_ip
// 里的历史值，才能让 Key 在重启、扩容、故障恢复后仍走同一个出口。
//
// shard 非空时只恢复本机分片的 Key: 别的分片的 Key 绑的是别的机器的 IP，
// 本机出口池里根本没有那些地址，尝试 Adopt 只会打出一排误导性的
// 「绑定无法沿用，将重新分配」告警，然后把它错绑到本机 IP 上。
//
// 新分配的绑定会写回库中，使其在下次启动时成为「历史值」。
func restoreBindings(ctx context.Context, st *store.Store, pool *egress.Pool, shard string, log *slog.Logger) error {
	keys, err := st.ListUpstreamKeys(ctx, store.UpstreamKeyFilter{
		Status: store.KeyStatusActive,
		Shard:  shard,
	})
	if err != nil {
		return err
	}
	var adopted, assigned, failed int
	for _, k := range keys {
		// 库里已有绑定 —— 原样沿用，这是终身绑定的本体。
		if k.EgressIP != "" {
			if err := pool.Adopt(k.KeyID, k.EgressIP, k.Pool); err == nil {
				adopted++
				continue
			} else {
				// 采纳失败的原因（IP 已下线 / 已封禁 / 档位冲突）必须可见:
				// 静默改绑等于悄悄给这个账号换了出口。
				log.Warn("库中出口绑定无法沿用，将重新分配",
					"key_id", k.KeyID, "egress_ip", k.EgressIP, "pool", k.Pool, "error", err)
			}
		}

		addr, err := pool.BindInPool(k.KeyID, k.Pool)
		if err != nil {
			// 单个 Key 绑定失败（如该档位容量已满）不应阻断启动:
			// 其余 Key 仍可正常服务，该 Key 会在调度时被跳过。
			log.Warn("恢复 Key 出口绑定失败", "key_id", k.KeyID, "pool", k.Pool, "error", err)
			failed++
			continue
		}
		assigned++

		// 写回库中，让本次分配在下次启动时成为可沿用的历史值。
		// 失败不阻断启动 —— 本次运行的绑定已在内存中生效。
		if addr != "" {
			if err := st.UpdateUpstreamKeyState(ctx, k.KeyID, store.UpstreamKeyState{EgressIP: &addr}); err != nil {
				log.Warn("出口绑定写回失败，重启后可能改绑",
					"key_id", k.KeyID, "egress_ip", addr, "error", err)
			}
		}
	}
	log.Info("Key-IP 绑定已恢复",
		"adopted", adopted, "assigned", assigned, "failed", failed, "total", len(keys))
	if adopted+assigned == 0 && len(keys) > 0 {
		return fmt.Errorf("全部 %d 个 Key 均绑定失败，出口 IP 配置可能有误", len(keys))
	}
	return nil
}

// warnUnshardedKeys 在分片模式下告警尚未指派归属的活跃 Key。
//
// 存在的理由: 分片过滤是严格的，未指派的 Key 不会被任何实例装载 —— 它们
// 静默地从池中消失，而每台机器单看都「正常装载了自己的 Key」。这个盲区
// 只能靠启动时主动报出来。
//
// 只告警不自动指派: 自动分配会把 Key 绑到一台机器的出口 IP 上，而这个
// 决定是不可逆的（改绑等于换出口）。这种决定必须由运维显式做出。
//
// 查询失败仅记日志: 这是可观测性辅助，不该阻断启动。
func warnUnshardedKeys(ctx context.Context, st *store.Store, log *slog.Logger) {
	keys, err := st.ListUpstreamKeys(ctx, store.UpstreamKeyFilter{Status: store.KeyStatusActive})
	if err != nil {
		log.Warn("检查未指派分片的 Key 失败", "error", err)
		return
	}
	var orphans []string
	for _, k := range keys {
		if k.Shard == "" {
			orphans = append(orphans, k.KeyID)
		}
	}
	if len(orphans) == 0 {
		return
	}
	sample := orphans
	if len(sample) > 10 {
		sample = sample[:10]
	}
	log.Warn("存在未指派机器归属的活跃 Key，它们不会被任何实例装载；"+
		"请用 POST /admin/keys/shard 指派",
		"count", len(orphans), "sample", sample)
}

// verifyEgress 校验每个出口 IP 的实际连通性。
//
// 这是 P1-6 的启动护栏。multi_ip 模式下云厂商绑定辅助私网 IP 后，OS 既不会
// 自动配置到网卡也不会建策略路由，此时 LocalAddr 绑定会失败或流量回落到
// 主 IP —— 两种情况都不会有任何报错，只是「不生效」。
func verifyEgress(ctx context.Context, cfg *config.Config, pool *egress.Pool, log *slog.Logger) error {
	target := cfg.EgressVerifyTarget()
	if target == "" {
		log.Warn("egress.verify_on_start 已开启，但未设置 verify_target 且无法从 provider " +
			"base_url 推导自检目标，跳过自检")
		return nil
	}

	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	results := pool.Verify(vctx, target)
	var bad []string
	for addr, err := range results {
		if err != nil {
			log.Error("出口 IP 自检失败", "addr", addr, "target", target, "error", err)
			bad = append(bad, addr)
		} else {
			log.Info("出口 IP 自检通过", "addr", addr, "target", target)
		}
	}

	if len(bad) > 0 {
		// 这里选择拒绝启动而非降级运行。降级的后果是所有 Key 共享同一出口，
		// 火山侧看到上百账号来自同一 IP —— 这正是要避免的封禁特征。
		// 宁可启动失败被立即发现，也不要静默失效。
		return fmt.Errorf("出口 IP 自检失败 %v: 请检查策略路由配置（见 deploy/setup-egress.sh），"+
			"或临时设置 egress.verify_on_start=false 跳过", bad)
	}
	return nil
}

// newProbes 按 provider 构造刷新探测函数。
//
// 探测的语义是「问上游: 这个 Key 的额度恢复了吗」。实现方式是发一个最小
// 成本的真实请求:
//   - 成功 → 额度已恢复
//   - 配额类错误 → 尚未恢复，继续等
//   - 其他错误 → 无法判断，视为未恢复并记录
//
// 关键点是绝不按时间推测（P0-4）。V3 的做法是「到点了就认为刷新完成并清零
// 本地计数」，若火山实际还没重置，恢复调度的瞬间就是超刷。
//
// 一个 provider 一个探测器，而不是共用一个。此前的实现是
// `for _, p := range cfg.Providers { mapping = p.ModelMapping; break }`，
// 靠 map 遍历取「第一个」provider 的映射去探测所有上游的 Key —— 而 Go 的
// map 遍历顺序是随机的，实际每次启动随机挑一个。后果是探测请求带着 A 上游的
// 模型名打到 B 上游: B 大概率返回「模型不存在」，被归入「其他错误」分支，
// 于是恒返回未恢复。表现就是刷新窗口过后 B 的 Key 迟迟不恢复调度，而日志里
// 只有一条「探测返回非配额错误」，看不出根因是模型名串了上游。
//
// adapter 也从快照的 registry 取，不再一律 NewVolc —— 探测请求的鉴权头格式
// 各家不同，用错 adapter 会拿到 401，同样落进「其他错误」分支。
func newProbes(snap *confsnap.Snapshot, st *store.Store, pool *egress.Pool,
	log *slog.Logger) (map[string]quota.Probe, error) {

	out := make(map[string]quota.Probe, len(snap.Cfg.Providers))
	for name := range snap.Cfg.Providers {
		ad, err := snap.Adapters.Get(name)
		if err != nil {
			return nil, fmt.Errorf("构造 %s 的探测器: %w", name, err)
		}
		out[name] = newProbe(name, ad, snap, st, pool, log)
	}
	return out, nil
}

// newProbe 构造单个 provider 的探测函数。
//
// snap 从参数传入并被闭包捕获: 探测器是常驻协程，捕获快照而非 Holder 保证
// 它整个生命周期内用的是同一份配置。配置变了由 reconcileRefreshers 重建，
// 而不是让运行中的探测器自己去读新值 —— 后者会出现「新 base_url 配旧
// model_mapping」的混合态，而探测失败在这条路径上是静默的。
func newProbe(providerName string, ad adapter.Adapter, snap *confsnap.Snapshot,
	st *store.Store, pool *egress.Pool, log *slog.Logger) quota.Probe {

	cfg := snap.Cfg

	return func(ctx context.Context, keyID string) (bool, error) {
		// WithSecret 为 true: 探测需要真实调用上游，必须解密。
		key, err := st.GetUpstreamKey(ctx, keyID)
		if err != nil {
			return false, fmt.Errorf("读取 Key %s: %w", keyID, err)
		}

		// 守卫错配: 这个探测器只认自己那个 provider 的 Key。调度侧若因某种
		// 原因把别家的 Key 传进来，宁可报错也不能拿错误的模型名去打上游。
		if key.Provider != providerName {
			return false, fmt.Errorf("key %s 属于 provider %s，不能用 %s 的探测器",
				keyID, key.Provider, providerName)
		}

		secret, err := st.Cipher().DecryptSecret(key.SecretEnc)
		if err != nil {
			return false, fmt.Errorf("解密 Key %s: %w", keyID, err)
		}

		// 获取该 Key 所属 provider 的 BaseURL
		provider, ok := cfg.Providers[key.Provider]
		if !ok {
			return false, fmt.Errorf("key %s 的 provider %s 在配置中不存在", keyID, key.Provider)
		}

		// 必须用该 Key 自己的出口客户端 —— 探测请求走错 IP 等于
		// 在火山侧留下「这个账号偶尔从另一台机器访问」的记录。
		cli, err := pool.ClientFor(keyID)
		if err != nil {
			return false, fmt.Errorf("获取出口客户端 %s: %w", keyID, err)
		}

		// 走 TransformRequest 而非自己拼路径: 它同时负责把对外模型名按该
		// provider 的 model_mapping 换成上游真实模型名。绕过它就等于拿
		// 对外名去打上游，上游返回「模型不存在」，被归入非配额错误 ——
		// 探测于是恒返回未恢复，而这条路径不会报任何错。
		path, body, err := ad.TransformRequest(adapter.EndpointChat, []byte(probeBody(cfg)))
		if err != nil {
			return false, fmt.Errorf("构造 %s 的探测请求体: %w", providerName, err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimRight(provider.BaseURL, "/")+path,
			strings.NewReader(string(body)))
		if err != nil {
			return false, err
		}
		for k, vs := range ad.BuildAuthHeaders(secret) {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := cli.Do(req)
		if err != nil {
			return false, fmt.Errorf("探测请求 %s: %w", keyID, err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode == http.StatusOK {
			return true, nil
		}

		buf := make([]byte, 2048)
		n, _ := resp.Body.Read(buf)
		ue := ad.MapError(resp.StatusCode, buf[:n])

		if ue.Class == adapter.ErrClassQuota {
			// 明确的「额度仍未恢复」，这是探测窗口内的预期结果。
			return false, nil
		}
		log.Warn("探测返回非配额错误",
			"key_id", keyID, "status", resp.StatusCode, "class", ue.Class.String())
		return false, nil
	}
}

// probeBody 构造探测请求体。
//
// 刻意用最小 max_tokens: 探测本身也消耗额度，成本应尽可能低。
func probeBody(cfg *config.Config) string {
	model := cfg.Refresh.ProbeModel
	if model == "" {
		model = "gpt-3.5-turbo"
	}
	return fmt.Sprintf(
		`{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_tokens":1}`, model)
}
