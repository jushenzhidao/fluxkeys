package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/confsnap"
	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/gateway"
	"github.com/fluxkeys/fluxkeys/internal/quota"
	"github.com/fluxkeys/fluxkeys/internal/scheduler"
	"github.com/fluxkeys/fluxkeys/internal/store"
)

// mustHolder 用给定配置建一个快照 Holder。
//
// provider 名必须是 confsnap 认识的（volc / sensenova），否则 Build 会因为
// 没有对应 adapter 而失败 —— 这正是它该有的行为，测试里直接 Fatal。
func mustHolder(t *testing.T, cfg *config.Config) *confsnap.Holder {
	t.Helper()
	h, err := confsnap.NewHolderFromConfig(cfg, 0)
	if err != nil {
		t.Fatalf("建配置快照: %v", err)
	}
	return h
}

// 装配层最容易出错的不是转换字段，而是**错误翻译**与**枚举映射**:
// 前者错了会让容量问题上报成 500、鉴权失败上报成 500；后者错了会让
// Key 状态机收到错误的事件（比如把额度耗尽当限流，冷却几秒后又选中它）。
// 这两类错误都不会导致编译失败，也不会在正常路径下暴露。

func TestMapFailureKind_覆盖全部分类(t *testing.T) {
	cases := []struct {
		in   gateway.FailureKind
		want scheduler.FailureKind
	}{
		{gateway.FailureAuth, scheduler.FailureAuth},
		{gateway.FailureRateLimit, scheduler.Failure429},
		{gateway.FailureQuota, scheduler.FailureQuota},
		{gateway.FailureNetwork, scheduler.FailureTimeout},
		{gateway.FailureServer, scheduler.Failure5xx},
	}
	for _, c := range cases {
		if got := mapFailureKind(c.in); got != c.want {
			t.Errorf("mapFailureKind(%s) = %q, 期望 %q", c.in, got, c.want)
		}
	}

	// 额度耗尽与限流绝不能映射到同一个事件: 前者要等到次日刷新，
	// 后者冷却几秒即可恢复。混淆会让网关反复撞上已耗尽的 Key。
	if mapFailureKind(gateway.FailureQuota) == mapFailureKind(gateway.FailureRateLimit) {
		t.Error("额度耗尽与限流被映射为同一个调度事件")
	}
}

func TestMapFailureKind_未知值降级为服务端错误(t *testing.T) {
	// 未知分类降级为 5xx 而非 auth: 误判为 auth 会永久禁用一个健康的 Key
	if got := mapFailureKind(gateway.FailureKind(99)); got != scheduler.Failure5xx {
		t.Errorf("未知分类映射为 %q, 期望降级为 %q", got, scheduler.Failure5xx)
	}
}

func TestSchedulerAdapter_无候选错误被翻译(t *testing.T) {
	// scheduler.ErrNoCandidate 必须翻译成 gateway.ErrNoCandidate，
	// 否则 gateway 认不出来，会按 500 internal_error 返回 ——
	// 把「所有 Key 都满了」这种容量问题上报成程序缺陷。
	raw := errors.New("scheduler: 无可用 Key")
	if errors.Is(raw, gateway.ErrNoCandidate) {
		t.Fatal("测试前提失效: 两个哨兵错误本应互不相认")
	}

	wrapped := errors.Join(gateway.ErrNoCandidate, scheduler.ErrNoCandidate)
	if !errors.Is(wrapped, gateway.ErrNoCandidate) {
		t.Error("翻译后的错误无法被 gateway.ErrNoCandidate 识别")
	}
}

func TestStoreAdapter_三种鉴权失败统一收敛(t *testing.T) {
	// 区分「不存在 / 已吊销 / 用户停用」等于告诉撞库者这个 Key 曾经存在。
	// 这里验证三者都能被 gateway.ErrUnauthorized 识别，
	// 具体原因保留在 wrap 内供服务端日志使用。
	for _, base := range []error{store.ErrNotFound, store.ErrKeyRevoked, store.ErrUserSuspended} {
		wrapped := errors.Join(gateway.ErrUnauthorized, base)
		if !errors.Is(wrapped, gateway.ErrUnauthorized) {
			t.Errorf("%v 未被收敛为 ErrUnauthorized", base)
		}
		// 原始原因仍可在服务端侧取回
		if !errors.Is(wrapped, base) {
			t.Errorf("%v 的原始原因在包装后丢失，服务端无法排障", base)
		}
	}
}

func TestParseClock(t *testing.T) {
	cases := []struct {
		in   string
		def  time.Duration
		want time.Duration
	}{
		{"12:00", 0, 12 * time.Hour},
		{"14:30", 0, 14*time.Hour + 30*time.Minute},
		{"00:00", time.Hour, 0},
		{"23:59", 0, 23*time.Hour + 59*time.Minute},
		// 非法输入必须回落到默认值而非 0 —— 0 意味着「零点」，
		// 会让刷新窗口静默地挪到午夜，而 12:00 的刷新完全落空
		{"", 12 * time.Hour, 12 * time.Hour},
		{"abc", 12 * time.Hour, 12 * time.Hour},
		{"25:00", 12 * time.Hour, 12 * time.Hour},
		{"12:99", 12 * time.Hour, 12 * time.Hour},
		{"-1:00", 12 * time.Hour, 12 * time.Hour},
	}
	for _, c := range cases {
		if got := parseClock(c.in, c.def); got != c.want {
			t.Errorf("parseClock(%q, %v) = %v, 期望 %v", c.in, c.def, got, c.want)
		}
	}
}

func TestQuotaLimits_按_provider_取硬水位(t *testing.T) {
	// KeyStates 在 Redis 没有该 Key 记录时用配置水位兜底，
	// 否则管理接口会把所有未使用的 Key 显示为「限额 0」，看起来全部耗尽。
	//
	// 关键是水位必须按 Key 所属 provider 取: 各家上游额度差一个数量级，
	// 报同一个上限会让运维对额度小的上游误判余量。
	cfg := &config.Config{
		Quota: config.Quota{
			TokenLimit: 5_000_000, TokenHardRatio: 0.95,
			CountLimit: 100, CountHardRatio: 0.95,
		},
		Providers: map[string]config.Provider{
			// 显式覆盖: 该上游按次计费且额度远小于全局默认
			"sensenova": {QuotaKind: "count", QuotaLimit: 1400},
			// 未配 quota_limit: 应回退到全局值
			"volc": {QuotaKind: "token"},
		},
	}
	a := &schedulerAdapter{cfg: quotaLimits{snaps: mustHolder(t, cfg)}}
	snap := a.cfg.snaps.Current()

	if got := a.cfg.hardFor(snap, "sensenova", true); got != 1330 {
		t.Errorf("sensenova 次数硬水位 = %d, 期望 1330（1400*0.95，provider 覆盖生效）", got)
	}
	if got := a.cfg.hardFor(snap, "volc", false); got != 4_750_000 {
		t.Errorf("volc token 硬水位 = %d, 期望 4750000（未配 quota_limit，回退全局）", got)
	}
	// 未知 provider 不应 panic，回退全局值
	if got := a.cfg.hardFor(snap, "unknown", true); got != 95 {
		t.Errorf("未知 provider 次数硬水位 = %d, 期望 95（回退全局）", got)
	}
}

// ---------- 候选字段透传 ----------

// 只实现 scheduler 需要的两个方法，避免把整个 store 拖进单元测试。
type schedStoreStub struct{ keys []store.UpstreamKey }

func (s schedStoreStub) ListUpstreamKeys(ctx context.Context, f store.UpstreamKeyFilter) ([]store.UpstreamKey, error) {
	return s.keys, nil
}

func (s schedStoreStub) GetKeyHistory(ctx context.Context, ids []string, day time.Time) (map[store.HistoryKey]store.KeyDailyHistory, error) {
	return map[store.HistoryKey]store.KeyDailyHistory{}, nil
}

type quotaReaderStub struct{}

func (quotaReaderStub) GetMany(ctx context.Context, provider string, ids []string, kind quota.Kind) (map[string]quota.Snapshot, error) {
	out := map[string]quota.Snapshot{}
	for _, id := range ids {
		// 给足额度，让候选不因水位被淘汰
		out[id] = quota.Snapshot{KeyID: id, Kind: kind, Hard: 5_000_000, Soft: 4_000_000}
	}
	return out, nil
}

// Pool 必须从调度器透传到 gateway.Candidate。
//
// 漏掉这一个字段的后果是隐蔽的: 编译通过、请求成功、既有测试全绿，
// 但每个 Key 首次绑定出口时都拿不到档位，于是哈希把它随机丢到任意
// 档位的 IP 上 —— 一个 hot 档的高频 Key 可能落到承载几十个 Key 的
// cold 档出口，出口分层的全部收益悄悄归零。
//
// 集成测试（test/ 包）用的是自己的 memSched 替身，不经过本适配器，
// 所以这条透传只能在这里覆盖。
func TestSchedulerAdapter_Select透传Pool(t *testing.T) {
	cfg := config.Default()
	// 关掉画像时段过滤: 窄化后任一时刻仅约 1/7 的 Key 活跃，
	// 单 Key 样本会因作息不匹配而无候选，与本用例要验的透传无关。
	cfg.Scheduler.EnablePersona = false

	st := schedStoreStub{keys: []store.UpstreamKey{{
		KeyID:     "volc_hot_001",
		Status:    store.KeyStatusActive,
		Pool:      "hot",
		EgressIP:  "172.16.0.11",
		SecretEnc: "sk-plain-001",
		PersonaID: "p_01",
	}}}

	sched := scheduler.New(scheduler.StaticConfig{Cfg: cfg}, st, quotaReaderStub{})
	if err := sched.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	a := &schedulerAdapter{sched: sched}
	cand, err := a.Select(context.Background(), gateway.SelectRequest{
		Model: "deepseek-v3",
		Kind:  quota.KindToken,
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if cand.Pool != "hot" {
		t.Errorf("Candidate.Pool = %q, 期望 hot —— 档位未从调度器透传，"+
			"出口分层会在首次绑定时失效", cand.Pool)
	}
	// 同批透传的其余字段一并锁死，避免下次改这段时漏掉别的
	if cand.KeyID != "volc_hot_001" {
		t.Errorf("KeyID = %q", cand.KeyID)
	}
	if cand.EgressIP != "172.16.0.11" {
		t.Errorf("EgressIP = %q, 期望 172.16.0.11", cand.EgressIP)
	}
	if cand.Kind != quota.KindToken {
		t.Errorf("Kind = %v, 期望沿用请求的 Kind", cand.Kind)
	}
}

// ---------- 出口健康检查中的封禁恢复 ----------

// newBannedPool 造一个只含 127.0.0.1 且已被封禁的出口池。
//
// 只用 127.0.0.1: macOS 上其余 127.0.0.x 未配 lo0 alias，绑定源地址会失败。
func newBannedPool(t *testing.T) (*egress.Pool, *egress.IP) {
	t.Helper()
	ips := []*egress.IP{egress.NewIP("127.0.0.1", "203.0.113.1", 10)}
	pool, err := egress.NewPool(egress.ModeMultiIP, ips, 5*time.Second)
	if err != nil {
		t.Fatalf("构造出口池: %v", err)
	}
	ip := pool.IPForAddr("127.0.0.1")
	if ip == nil {
		t.Fatal("取不到出口 IP 对象")
	}
	ip.MarkBanned()
	if ip.State() != egress.IPBanned {
		t.Fatalf("前置条件: 应为 banned，实际 %s", ip.State())
	}
	return pool, ip
}

// bgFor 构造只依赖配置 / pool / log 的后台任务实例。
// checkEgress 不触碰 store 与 quota，故其余依赖留零值。
func bgFor(t *testing.T, cfg *config.Config, pool *egress.Pool) *background {
	t.Helper()
	return newBackground(bgDeps{
		snaps: mustHolder(t, cfg),
		pool:  pool,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// egressCfg 返回一份启用 multi_ip 且探测必然失败的配置。
//
// 探测指向 127.0.0.1:1（保留端口，无人监听）是刻意的: 若解封依赖探测成功，
// 断言就会因探测失败而通过，测不出顺序问题。让探测必败才能证明
// 「解封发生在探测之前且不依赖它」。
func egressCfg(cooldown time.Duration) *config.Config {
	cfg := config.Default()
	cfg.Egress.Mode = "multi_ip"
	cfg.Egress.BanCooldown = cooldown
	cfg.Egress.BanCooldownMax = 24 * time.Hour
	cfg.Egress.VerifyTarget = "127.0.0.1:1"
	return cfg
}

// checkEgress 必须在探测**之前**解封，否则自动恢复永远不会发生。
//
// 这个顺序错误极其隐蔽: 代码看起来两件事都做了，编译通过、测试全绿，
// 但被封出口永远停在 banned。原因是 Verify 成功时调 MarkSuccess，而
// MarkSuccess 只把 suspect / cooldown 转回 active —— banned 不在其中。
// 先探测则探测结果改不动 banned，解封要等到下一轮，而下一轮又是先探测。
//
// 实际后果是「出口只减不增」: 每次判定损失一个 IP，直到备用余量耗尽。
//
// 用极小的 base（1 纳秒）让冷却期立即届满，避免为了控制时间给生产代码
// 加测试后门。
func TestCheckEgress_先解封再探测(t *testing.T) {
	pool, ip := newBannedPool(t)
	if err := bgFor(t, egressCfg(time.Nanosecond), pool).
		checkEgress(context.Background()); err != nil {
		t.Fatalf("checkEgress: %v", err)
	}
	if got := ip.State(); got == egress.IPBanned {
		t.Errorf("冷却期已届满，状态应已离开 banned，实际仍为 %s —— "+
			"很可能是先探测后解封，导致恢复永不发生", got)
	}
}

// 冷却期未届满时不得解封。
func TestCheckEgress_冷却期内保持banned(t *testing.T) {
	pool, ip := newBannedPool(t)
	if err := bgFor(t, egressCfg(2*time.Hour), pool).
		checkEgress(context.Background()); err != nil {
		t.Fatalf("checkEgress: %v", err)
	}
	if got := ip.State(); got != egress.IPBanned {
		t.Errorf("刚被封且冷却 2 小时，应仍为 banned，实际 %s", got)
	}
}

// BanCooldown=0 表示未启用自动恢复，保持原有的永久 banned 行为。
func TestCheckEgress_未启用恢复时保持banned(t *testing.T) {
	pool, ip := newBannedPool(t)
	if err := bgFor(t, egressCfg(0), pool).
		checkEgress(context.Background()); err != nil {
		t.Fatalf("checkEgress: %v", err)
	}
	if got := ip.State(); got != egress.IPBanned {
		t.Errorf("未启用自动恢复，应仍为 banned，实际 %s", got)
	}
}
