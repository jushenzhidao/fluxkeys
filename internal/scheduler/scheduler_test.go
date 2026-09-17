package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/persona"
	"github.com/fluxkeys/fluxkeys/internal/quota"
	"github.com/fluxkeys/fluxkeys/internal/store"
)

// ---------- 测试替身 ----------

// fakeStore 用内存数据替代 Postgres，让调度逻辑测试不依赖外部服务。
type fakeStore struct {
	mu      sync.Mutex
	keys    []store.UpstreamKey
	history map[store.HistoryKey]store.KeyDailyHistory
	listErr error
	histErr error
}

func (f *fakeStore) ListUpstreamKeys(ctx context.Context, filter store.UpstreamKeyFilter) ([]store.UpstreamKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []store.UpstreamKey
	for _, k := range f.keys {
		if filter.Status != "" && k.Status != filter.Status {
			continue
		}
		out = append(out, k)
	}
	return out, nil
}

func (f *fakeStore) GetUpstreamKey(ctx context.Context, keyID string) (*store.UpstreamKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.keys {
		if k.KeyID == keyID {
			cp := k
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) GetKeyHistory(ctx context.Context, keyIDs []string, day time.Time) (map[store.HistoryKey]store.KeyDailyHistory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.histErr != nil {
		return nil, f.histErr
	}
	// 按 keyID 过滤但保留完整复合键: 真实实现返回的是 (key, provider) 维度，
	// 替身若把 provider 抹平，就无法暴露调度侧拿错 provider 历史的问题。
	want := make(map[string]bool, len(keyIDs))
	for _, id := range keyIDs {
		want[id] = true
	}
	out := map[store.HistoryKey]store.KeyDailyHistory{}
	for k, h := range f.history {
		if want[k.UpstreamKeyID] {
			out[k] = h
		}
	}
	return out, nil
}

// fakeQuota 用内存 map 替代 Redis 快照读取。
type fakeQuota struct {
	mu    sync.Mutex
	snaps map[string]quota.Snapshot
	err   error
	calls int
}

func (f *fakeQuota) GetMany(ctx context.Context, provider string, keyIDs []string, kind quota.Kind) (map[string]quota.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]quota.Snapshot{}
	for _, id := range keyIDs {
		if s, ok := f.snaps[id]; ok {
			s.KeyID, s.Kind = id, kind
			out[id] = s
		}
	}
	return out, nil
}

func (f *fakeQuota) set(keyID string, s quota.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snaps == nil {
		f.snaps = map[string]quota.Snapshot{}
	}
	f.snaps[keyID] = s
}

// ---------- 构造辅助 ----------

// testTime 固定在 15:00，处在多数画像的活跃时段内。
func testTime() time.Time { return time.Date(2026, 8, 23, 15, 0, 0, 0, time.Local) }

func testCfg() config.Scheduler {
	c := config.Default().Scheduler
	c.MinRequestInterval = 0 // 多数测试不关心间隔，需要时单独打开
	return c
}

// testConf 把调度配置包成完整的 *config.Config 配置源。
//
// 水位与量纲要走 config.Config 上的真实规则（provider 覆盖优先、比例取全局），
// 在测试里另搭一套假的会让量纲类断言测的是假实现而不是生产逻辑。
func testConf(sc config.Scheduler) StaticConfig {
	cfg := config.Default()
	cfg.Scheduler = sc
	return StaticConfig{Cfg: cfg}
}

func newFixture(t *testing.T, n int, opts ...func(*config.Scheduler)) (*Scheduler, *fakeStore, *fakeQuota) {
	t.Helper()

	cfg := testCfg()
	for _, o := range opts {
		o(&cfg)
	}

	fs := &fakeStore{history: map[store.HistoryKey]store.KeyDailyHistory{}}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("volc_%03d", i)
		fs.keys = append(fs.keys, store.UpstreamKey{
			KeyID: id, Secret: "secret-" + id, Status: store.KeyStatusActive,
			Pool: "hot", EgressIP: fmt.Sprintf("172.16.0.%d", i%250+2), HealthScore: 100,
		})
	}
	fq := &fakeQuota{}

	s := New(testConf(cfg), fs, fq)
	s.SetClock(testTime)
	s.SetRandSource(42)
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	s.RefreshSnapshot(context.Background())
	return s, fs, fq
}

// setQuotaInterval 改配置源里的快照刷新间隔。
//
// 不能再像以前那样直接改 s.quotaC —— 配置已改为从 ConfigSource 读，
// 改一份副本不会影响调度器实际读到的值，测试会静默失去时序控制。
func setQuotaInterval(t *testing.T, s *Scheduler, d time.Duration) {
	t.Helper()
	sc, ok := s.conf.(StaticConfig)
	if !ok {
		t.Fatalf("测试配置源应为 StaticConfig，实际为 %T", s.conf)
	}
	sc.Cfg.Quota.SnapshotInterval = d
}

// activeKeyAt 返回在给定时刻处于活跃时段的一个 Key ID。
func activeKeyAt(t *testing.T, s *Scheduler, at time.Time) string {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.keys {
		if e.persona.IsActiveAt(at) {
			return e.keyID
		}
	}
	t.Fatalf("%v 没有活跃 Key", at)
	return ""
}

// ---------- 基本选择 ----------

func TestSelect_基本可用(t *testing.T) {
	s, _, _ := newFixture(t, 20)

	c, err := s.Select(context.Background(), Request{Model: "deepseek-v3", Now: testTime()})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if c.KeyID == "" {
		t.Fatal("未返回 KeyID")
	}
	if c.Secret != "secret-"+c.KeyID {
		t.Fatalf("密钥未随候选返回: %q", c.Secret)
	}
	if c.EgressIP == "" {
		t.Fatal("未返回绑定出口 IP")
	}
	if c.Persona == nil {
		t.Fatal("未返回画像")
	}
	if c.Score.Total <= 0 {
		t.Fatalf("总分异常: %+v", c.Score)
	}
	if c.CandidateCount <= 0 {
		t.Fatalf("候选数异常: %d", c.CandidateCount)
	}
}

func TestSelect_空池返回可诊断错误(t *testing.T) {
	s, _, _ := newFixture(t, 0)

	_, err := s.Select(context.Background(), Request{Now: testTime()})
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("期望 ErrNoCandidate，实际 %v", err)
	}
	if !contains(err.Error(), "Key 池为空") {
		t.Fatalf("错误信息不可诊断: %v", err)
	}
}

func TestSelect_Now为零值时自动填当前时间(t *testing.T) {
	s, _, _ := newFixture(t, 20)
	if _, err := s.Select(context.Background(), Request{Model: "deepseek-v3"}); err != nil {
		// 当前真实时间可能落在低谷时段，只要不是 panic 即可
		if !errors.Is(err, ErrNoCandidate) {
			t.Fatalf("非预期错误: %v", err)
		}
	}
}

func TestSelect_排除已尝试的Key(t *testing.T) {
	s, _, _ := newFixture(t, 20)
	ctx := context.Background()
	now := testTime()

	exclude := map[string]bool{}
	// 逐个排除，直到无候选。每次返回的 Key 都不该在 exclude 里。
	for i := 0; i < 20; i++ {
		c, err := s.Select(ctx, Request{Model: "deepseek-v3", Now: now, Exclude: exclude})
		if err != nil {
			if !errors.Is(err, ErrNoCandidate) {
				t.Fatalf("非预期错误: %v", err)
			}
			break
		}
		if exclude[c.KeyID] {
			t.Fatalf("返回了已排除的 Key: %s", c.KeyID)
		}
		exclude[c.KeyID] = true
	}
	if len(exclude) == 0 {
		t.Fatal("一个 Key 都没选出来")
	}

	// 全部排除后必须报无候选，并说明原因
	all := map[string]bool{}
	s.mu.RLock()
	for _, e := range s.keys {
		all[e.keyID] = true
	}
	s.mu.RUnlock()

	_, err := s.Select(ctx, Request{Model: "deepseek-v3", Now: now, Exclude: all})
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("期望 ErrNoCandidate，实际 %v", err)
	}
	if !contains(err.Error(), "已排除") {
		t.Fatalf("错误未说明排除原因: %v", err)
	}
}

// ---------- 加权随机 ----------

func TestSelect_加权随机而非总取最高分(t *testing.T) {
	// 若总取最高分，配额曲线会呈"一个 Key 榨干、下一个接棒"的阶梯状，
	// 在风控侧极其醒目。这里验证选择确实是分散的。
	//
	// 样本量取 120: 画像窄化后（72 个模板，日均活跃 3 小时）任一时刻
	// 约 1/7 的 Key 处于活跃时段，20 个 Key 在固定时刻只剩 2-3 个候选，
	// 不足以观察分散性。生产规模 1000 Key 对应任意时刻约 170 个活跃。
	s, _, _ := newFixture(t, 120)
	ctx := context.Background()
	now := testTime()

	counts := map[string]int{}
	for i := 0; i < 1000; i++ {
		c, err := s.Select(ctx, Request{Model: "deepseek-v3", Now: now})
		if err != nil {
			t.Fatal(err)
		}
		counts[c.KeyID]++
	}

	if len(counts) < 5 {
		t.Fatalf("选择过于集中，只命中 %d 个 Key: %v", len(counts), counts)
	}
	// 单个 Key 占比不应超过 50%
	for id, c := range counts {
		if float64(c)/1000 > 0.5 {
			t.Fatalf("Key %s 占比 %.2f 过高，退化为最高分选择", id, float64(c)/1000)
		}
	}
	t.Logf("命中 %d 个不同 Key", len(counts))
}

func TestSelect_高分Key被选中概率更高(t *testing.T) {
	// 加权随机的"加权"必须真的生效: 剩余配额多的 Key 应被更多选中。
	cfg := testCfg()
	cfg.EnablePersona = false // 排除画像干扰，只看配额维度
	qcfg := config.Default().Quota

	fs := &fakeStore{history: map[store.HistoryKey]store.KeyDailyHistory{}}
	for _, id := range []string{"rich", "poor"} {
		fs.keys = append(fs.keys, store.UpstreamKey{
			KeyID: id, Secret: "s", Status: store.KeyStatusActive, HealthScore: 100,
		})
	}
	fq := &fakeQuota{}
	hard, soft := qcfg.TokenHard(), qcfg.TokenSoft()
	fq.set("rich", quota.Snapshot{Used: 0, Hard: hard, Soft: soft})
	// 取 95% 硬水位，此时已越过软水位（soft/hard = 0.8/0.9），会叠加 SoftPenalty
	fq.set("poor", quota.Snapshot{Used: hard * 95 / 100, Hard: hard, Soft: soft})

	s := New(testConf(cfg), fs, fq)
	s.SetClock(testTime)
	s.SetRandSource(7)
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.RefreshSnapshot(context.Background())

	counts := map[string]int{}
	for i := 0; i < 2000; i++ {
		c, err := s.Select(context.Background(), Request{Now: testTime()})
		if err != nil {
			t.Fatal(err)
		}
		counts[c.KeyID]++
	}
	if counts["rich"] <= counts["poor"] {
		t.Fatalf("高分 Key 未获更高概率: rich=%d poor=%d", counts["rich"], counts["poor"])
	}
	// poor 越过软水位且配额分归零，占比应显著低
	if float64(counts["poor"])/2000 > 0.35 {
		t.Fatalf("低分 Key 占比过高 %.2f", float64(counts["poor"])/2000)
	}
	t.Logf("rich=%d poor=%d", counts["rich"], counts["poor"])
}

// TestSelect_配额维度不足以独自压制耗尽的Key 记录一个权重设计的固有性质。
//
// 按文档给定权重（35/25/20/15），S_quota 只占总分的 35/95。一个配额剩余
// 15%、但尚未越过软水位的 Key，其余三维仍是满分，因此仍能拿到约 40% 的流量。
//
// 这不是 bug，而是要明确: **防超刷完全依赖硬水位淘汰 + Redis Lua 准入
// （P0-1），而不是依赖打分把耗尽的 Key "自然压下去"**。打分只负责让消耗
// 在池内均衡分布，不承担正确性责任。若未来希望配额维度更强势，应当调整
// 软水位比例，而不是加大 WeightQuota。
func TestSelect_配额维度不足以独自压制耗尽的Key(t *testing.T) {
	cfg := testCfg()
	cfg.EnablePersona = false
	qcfg := config.Default().Quota
	hard, soft := qcfg.TokenHard(), qcfg.TokenSoft()

	fs := &fakeStore{history: map[store.HistoryKey]store.KeyDailyHistory{}}
	for _, id := range []string{"rich", "thin"} {
		fs.keys = append(fs.keys, store.UpstreamKey{
			KeyID: id, Secret: "s", Status: store.KeyStatusActive, HealthScore: 100,
		})
	}
	fq := &fakeQuota{}
	fq.set("rich", quota.Snapshot{Used: 0, Hard: hard, Soft: soft})
	// 剩余 15%，但 used 仍低于软水位 → 不触发 SoftPenalty
	fq.set("thin", quota.Snapshot{Used: hard * 85 / 100, Hard: hard, Soft: soft})

	s := New(testConf(cfg), fs, fq)
	s.SetClock(testTime)
	s.SetRandSource(11)
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.RefreshSnapshot(context.Background())

	counts := map[string]int{}
	for i := 0; i < 2000; i++ {
		c, err := s.Select(context.Background(), Request{Now: testTime()})
		if err != nil {
			t.Fatal(err)
		}
		counts[c.KeyID]++
	}
	share := float64(counts["thin"]) / 2000
	if share < 0.25 {
		t.Fatalf("与预期性质不符: 未越软水位的 Key 份额 %.2f 意外偏低，"+
			"说明权重或打分逻辑已变更，请复核本测试记录的结论", share)
	}
	t.Logf("剩余 15%% 但未越软水位的 Key 仍占 %.2f 流量 —— 防超刷靠硬水位与 Lua，不靠打分", share)
}

// ---------- 水位 ----------

func TestSelect_硬水位直接淘汰(t *testing.T) {
	s, _, fq := newFixture(t, 10, func(c *config.Scheduler) { c.EnablePersona = false })
	qcfg := config.Default().Quota
	hard := qcfg.TokenHard()

	// 除一个 Key 外全部打到硬水位
	s.mu.RLock()
	var survivor string
	for i, e := range s.keys {
		if i == 0 {
			survivor = e.keyID
			fq.set(e.keyID, quota.Snapshot{Used: 0, Hard: hard, Soft: qcfg.TokenSoft()})
			continue
		}
		fq.set(e.keyID, quota.Snapshot{Used: hard, Hard: hard, Soft: qcfg.TokenSoft()})
	}
	s.mu.RUnlock()
	s.RefreshSnapshot(context.Background())

	for i := 0; i < 50; i++ {
		c, err := s.Select(context.Background(), Request{Now: testTime()})
		if err != nil {
			t.Fatal(err)
		}
		if c.KeyID != survivor {
			t.Fatalf("选中了超硬水位的 Key: %s", c.KeyID)
		}
	}

	// 全部超硬水位 → 无候选，且原因可诊断
	fq.set(survivor, quota.Snapshot{Used: hard, Hard: hard, Soft: qcfg.TokenSoft()})
	s.RefreshSnapshot(context.Background())
	_, err := s.Select(context.Background(), Request{Now: testTime()})
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("期望 ErrNoCandidate，实际 %v", err)
	}
	if !contains(err.Error(), "超硬水位") {
		t.Fatalf("错误未指出硬水位原因: %v", err)
	}
}

func TestSelect_硬水位含预扣量(t *testing.T) {
	// used 没到硬水位但 used+prededuct 到了，也必须淘汰 ——
	// 否则并发下多个请求会一起挤进最后一点额度。
	s, _, fq := newFixture(t, 2, func(c *config.Scheduler) { c.EnablePersona = false })
	qcfg := config.Default().Quota
	hard, soft := qcfg.TokenHard(), qcfg.TokenSoft()

	s.mu.RLock()
	ids := []string{s.keys[0].keyID, s.keys[1].keyID}
	s.mu.RUnlock()

	fq.set(ids[0], quota.Snapshot{Used: hard / 2, Prededuct: hard / 2, Hard: hard, Soft: soft})
	fq.set(ids[1], quota.Snapshot{Used: 0, Hard: hard, Soft: soft})
	s.RefreshSnapshot(context.Background())

	for i := 0; i < 20; i++ {
		c, err := s.Select(context.Background(), Request{Now: testTime()})
		if err != nil {
			t.Fatal(err)
		}
		if c.KeyID == ids[0] {
			t.Fatal("used+prededuct 已达硬水位仍被选中")
		}
	}
}

func TestSelect_软水位扣分但仍可选(t *testing.T) {
	// 关键区别: 软水位是降权，不是淘汰。全池都过软水位时系统仍需可用。
	s, _, fq := newFixture(t, 3, func(c *config.Scheduler) { c.EnablePersona = false })
	qcfg := config.Default().Quota
	hard, soft := qcfg.TokenHard(), qcfg.TokenSoft()

	s.mu.RLock()
	var ids []string
	for _, e := range s.keys {
		ids = append(ids, e.keyID)
	}
	s.mu.RUnlock()

	for _, id := range ids {
		fq.set(id, quota.Snapshot{Used: soft + 1, Hard: hard, Soft: soft})
	}
	s.RefreshSnapshot(context.Background())

	c, err := s.Select(context.Background(), Request{Now: testTime()})
	if err != nil {
		t.Fatalf("全池过软水位时应仍可调度: %v", err)
	}
	if c.Score.SoftPenalty != s.conf.Scheduler().SoftPenalty {
		t.Fatalf("软水位未扣分: %+v", c.Score)
	}

	// 未过软水位的 Key 不应被扣分
	fq.set(ids[0], quota.Snapshot{Used: 0, Hard: hard, Soft: soft})
	s.RefreshSnapshot(context.Background())
	for i := 0; i < 100; i++ {
		c, err := s.Select(context.Background(), Request{Now: testTime()})
		if err != nil {
			t.Fatal(err)
		}
		if c.KeyID == ids[0] && c.Score.SoftPenalty != 0 {
			t.Fatalf("未过软水位的 Key 被扣分: %+v", c.Score)
		}
	}
}

// ---------- P0-1: 快照只用于排序 ----------

func TestSelect_快照缺失时视为满额而非零额(t *testing.T) {
	// 今日尚未有任何请求的 Key，Redis 里没有对应 hash。若把缺失当作
	// "剩余 0"，这些最该被用的新鲜 Key 会被全部淘汰，系统直接不可用。
	s, _, fq := newFixture(t, 5, func(c *config.Scheduler) { c.EnablePersona = false })
	fq.snaps = map[string]quota.Snapshot{} // 全部快照缺失
	s.RefreshSnapshot(context.Background())

	c, err := s.Select(context.Background(), Request{Now: testTime()})
	if err != nil {
		t.Fatalf("快照缺失不应导致无候选: %v", err)
	}
	if c.Score.Quota != 100 {
		t.Fatalf("快照缺失应按满额打分，实际 %v", c.Score.Quota)
	}
	if c.Snapshot.Hard <= 0 {
		t.Fatal("快照缺失时未用配置水位补齐 Hard")
	}
}

func TestRefreshSnapshot_失败时保留上一轮值且不影响调度(t *testing.T) {
	// 快照本就允许陈旧且不参与准入（P0-1），因此刷新失败不需要降级动作。
	s, _, fq := newFixture(t, 5, func(c *config.Scheduler) { c.EnablePersona = false })
	qcfg := config.Default().Quota

	s.mu.RLock()
	id := s.keys[0].keyID
	s.mu.RUnlock()
	fq.set(id, quota.Snapshot{Used: 123, Hard: qcfg.TokenHard(), Soft: qcfg.TokenSoft()})
	s.RefreshSnapshot(context.Background())

	fq.mu.Lock()
	fq.err = errors.New("redis 挂了")
	fq.mu.Unlock()
	s.RefreshSnapshot(context.Background())

	s.mu.RLock()
	got := s.snaps[quota.KindToken][id]
	s.mu.RUnlock()
	if got.Used != 123 {
		t.Fatalf("刷新失败应保留上一轮快照，实际 %+v", got)
	}
	if _, err := s.Select(context.Background(), Request{Now: testTime()}); err != nil {
		t.Fatalf("快照刷新失败不应阻塞调度: %v", err)
	}
}

func TestStart_后台周期刷新快照(t *testing.T) {
	s, _, fq := newFixture(t, 5)
	setQuotaInterval(t, s, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Stop()

	time.Sleep(120 * time.Millisecond)

	fq.mu.Lock()
	calls := fq.calls
	fq.mu.Unlock()
	// 每轮刷 token + count 两种，至少应有若干次调用
	if calls < 4 {
		t.Fatalf("后台刷新次数过少: %d", calls)
	}
}

func TestStop_可重复调用(t *testing.T) {
	s, _, _ := newFixture(t, 3)
	setQuotaInterval(t, s, 10*time.Millisecond)
	s.Start(context.Background())
	s.Stop()
	s.Stop() // 重复调用不应 panic
}

// ---------- persona ----------

func TestSelect_非活跃时段的Key被淘汰(t *testing.T) {
	// 让一个"夜猫子"Key 在早上持续发请求，是最容易被识别的机器特征之一。
	s, _, _ := newFixture(t, 50)
	ctx := context.Background()

	for _, hour := range []int{3, 8, 15, 23} {
		at := time.Date(2026, 8, 23, hour, 0, 0, 0, time.Local)
		for i := 0; i < 100; i++ {
			c, err := s.Select(ctx, Request{Model: "deepseek-v3", Now: at})
			if err != nil {
				if errors.Is(err, ErrNoCandidate) {
					continue
				}
				t.Fatal(err)
			}
			if !c.Persona.IsActiveAt(at) {
				t.Fatalf("%02d 时选中了非活跃 Key %s（画像 %s）", hour, c.KeyID, c.Persona.ID)
			}
		}
	}
}

func TestSelect_关闭persona后不做时段过滤(t *testing.T) {
	s, _, _ := newFixture(t, 30, func(c *config.Scheduler) { c.EnablePersona = false })

	// 凌晨 4 点，多数画像都不活跃，但关闭 persona 后应仍可调度
	at := time.Date(2026, 8, 23, 4, 0, 0, 0, time.Local)
	c, err := s.Select(context.Background(), Request{Now: at})
	if err != nil {
		t.Fatalf("关闭 persona 后应无时段限制: %v", err)
	}
	if c.Score.Persona != 100 {
		t.Fatalf("关闭 persona 时该维度应为满分，实际 %v", c.Score.Persona)
	}
}

func TestSelect_全部Key非活跃时报可诊断错误(t *testing.T) {
	cfg := testCfg()
	fs := &fakeStore{
		history: map[store.HistoryKey]store.KeyDailyHistory{},
		keys: []store.UpstreamKey{
			{KeyID: "k1", Secret: "s", Status: store.KeyStatusActive, HealthScore: 100},
		},
	}
	s := New(testConf(cfg), fs, &fakeQuota{})
	s.SetClock(testTime)
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 找到 k1 不活跃的小时
	p := persona.For("k1")
	var offHour = -1
	for h := 0; h < 24; h++ {
		if !p.IsActiveAt(time.Date(2026, 8, 23, h, 0, 0, 0, time.Local)) {
			offHour = h
			break
		}
	}
	if offHour < 0 {
		t.Skip("k1 的画像是全天候，跳过")
	}

	_, err := s.Select(context.Background(),
		Request{Now: time.Date(2026, 8, 23, offHour, 0, 0, 0, time.Local)})
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("期望 ErrNoCandidate，实际 %v", err)
	}
	if !contains(err.Error(), "非活跃时段") {
		t.Fatalf("错误未指出时段原因: %v", err)
	}
}

// ---------- 最小请求间隔 ----------

func TestSelect_最小请求间隔约束(t *testing.T) {
	// 同一 Key 短时间内被连续选中，会形成远超人类操作频率的请求密度。
	cfg := testCfg()
	cfg.EnablePersona = false
	cfg.MinRequestInterval = 5 * time.Second

	fs := &fakeStore{
		history: map[store.HistoryKey]store.KeyDailyHistory{},
		keys: []store.UpstreamKey{
			{KeyID: "only", Secret: "s", Status: store.KeyStatusActive, HealthScore: 100},
		},
	}
	s := New(testConf(cfg), fs, &fakeQuota{})
	s.SetClock(testTime)
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	base := testTime()
	if _, err := s.Select(context.Background(), Request{Now: base}); err != nil {
		t.Fatal(err)
	}
	// 立刻再选同一个 Key 应被间隔约束挡住
	_, err := s.Select(context.Background(), Request{Now: base.Add(time.Second)})
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("期望被最小间隔挡住，实际 %v", err)
	}
	if !contains(err.Error(), "间隔不足") {
		t.Fatalf("错误未指出间隔原因: %v", err)
	}

	// 等待足够长（含画像节奏放大，稀疏型可达 2 倍）后应恢复
	if _, err := s.Select(context.Background(), Request{Now: base.Add(30 * time.Second)}); err != nil {
		t.Fatalf("间隔满足后应可选: %v", err)
	}
}

func TestSelect_间隔按画像节奏放大(t *testing.T) {
	// 所有 Key 都贴着同一个最小间隔跑，节奏维度就白设了。
	bursty := (&persona.Persona{Rhythm: persona.RhythmBursty}).PaceFactor()
	sparse := (&persona.Persona{Rhythm: persona.RhythmSparse}).PaceFactor()
	if bursty >= sparse {
		t.Fatalf("阵发型的间隔因子应小于稀疏型: %v vs %v", bursty, sparse)
	}
}

// ---------- 健康度状态机 ----------

func TestMarkFailure_连续429进入冷却(t *testing.T) {
	s, _, _ := newFixture(t, 5)
	s.mu.RLock()
	id := s.keys[0].keyID
	s.mu.RUnlock()

	for i := 1; i < consecutive429Cooldown; i++ {
		h := s.MarkFailure(id, Failure429)
		if h.Status != StatusActive {
			t.Fatalf("第 %d 次 429 就冷却了，过于激进", i)
		}
		if h.Consecutive429 != i {
			t.Fatalf("连续 429 计数错误: %d", h.Consecutive429)
		}
	}
	h := s.MarkFailure(id, Failure429)
	if h.Status != StatusCooldown {
		t.Fatalf("连续 %d 次 429 应进入冷却，实际 %s", consecutive429Cooldown, h.Status)
	}
	if h.CooldownUntil.IsZero() {
		t.Fatal("冷却未设置截止时间")
	}
	// 健康度递增惩罚: 10+20+30 = 60，应已明显下降
	if h.Score >= 100-penalty429*consecutive429Cooldown {
		t.Fatalf("连续 429 未递增惩罚: %d", h.Score)
	}
}

func TestSelect_冷却中的Key不被选中且到期自动恢复(t *testing.T) {
	cfg := testCfg()
	cfg.EnablePersona = false
	fs := &fakeStore{
		history: map[store.HistoryKey]store.KeyDailyHistory{},
		keys: []store.UpstreamKey{
			{KeyID: "k1", Secret: "s", Status: store.KeyStatusActive, HealthScore: 100},
		},
	}
	fq := &fakeQuota{}
	s := New(testConf(cfg), fs, fq)

	nowVal := testTime()
	s.SetClock(func() time.Time { return nowVal })
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < consecutive429Cooldown; i++ {
		s.MarkFailure("k1", Failure429)
	}
	if _, err := s.Select(context.Background(), Request{Now: nowVal}); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("冷却中的 Key 不应被选中，实际 %v", err)
	}

	// 冷却到期后应自动恢复，但健康度不会一次性拉满
	nowVal = nowVal.Add(cooldownDuration + time.Second)
	c, err := s.Select(context.Background(), Request{Now: nowVal})
	if err != nil {
		t.Fatalf("冷却到期应自动恢复: %v", err)
	}
	if c.Score.Health >= 100 {
		t.Fatalf("冷却到期不应把健康度拉满: %v", c.Score.Health)
	}
	h := s.Health("k1")
	if h.Status != StatusActive || h.Consecutive429 != 0 {
		t.Fatalf("冷却到期后状态未复位: %+v", h)
	}
}

func TestMarkSuccess_恢复健康度并解除冷却(t *testing.T) {
	s, _, _ := newFixture(t, 3)

	for i := 0; i < consecutive429Cooldown; i++ {
		s.MarkFailure("k", Failure429)
	}
	before := s.Health("k")
	if before.Status != StatusCooldown {
		t.Fatalf("应处于冷却: %+v", before)
	}

	h := s.MarkSuccess("k")
	if h.Status != StatusActive {
		t.Fatalf("成功后应解除冷却: %s", h.Status)
	}
	if h.Consecutive429 != 0 {
		t.Fatalf("成功后应清零连续 429: %d", h.Consecutive429)
	}
	if h.Score != before.Score+recoverSuccess {
		t.Fatalf("健康度恢复量不符: %d -> %d", before.Score, h.Score)
	}

	// 恢复有上限
	for i := 0; i < 200; i++ {
		h = s.MarkSuccess("k")
	}
	if h.Score != healthMax {
		t.Fatalf("健康度应封顶在 %d，实际 %d", healthMax, h.Score)
	}
}

func TestMarkFailure_各失败类型惩罚不同(t *testing.T) {
	s, _, _ := newFixture(t, 1)

	cases := []struct {
		kind FailureKind
		drop int
	}{
		{Failure5xx, penalty5xx},
		{FailureTimeout, penaltyTimeout},
	}
	for _, c := range cases {
		id := "k_" + string(c.kind)
		h := s.MarkFailure(id, c.kind)
		if h.Score != healthMax-c.drop {
			t.Fatalf("%s 扣分不符: 期望 %d 实际 %d", c.kind, healthMax-c.drop, h.Score)
		}
		if h.Status != StatusActive {
			t.Fatalf("%s 单次失败不应改变状态: %s", c.kind, h.Status)
		}
		if h.LastFailureKind != c.kind {
			t.Fatalf("未记录失败类型: %s", h.LastFailureKind)
		}
	}

	// 5xx 应清零连续 429 计数（这是上游问题，与 Key 无关）
	s.MarkFailure("mix", Failure429)
	h := s.MarkFailure("mix", Failure5xx)
	if h.Consecutive429 != 0 {
		t.Fatalf("5xx 应清零连续 429: %d", h.Consecutive429)
	}
}

func TestMarkFailure_鉴权失败置为终态(t *testing.T) {
	// Key 已失效不可能自愈，反复重试只会积累失败记录。
	s, _, _ := newFixture(t, 1)

	h := s.MarkFailure("dead", FailureAuth)
	if h.Status != StatusInvalid || h.Score != healthMin {
		t.Fatalf("鉴权失败应置 invalid 且健康度归零: %+v", h)
	}
	// 终态不因成功而恢复（正常流程下已 invalid 的 Key 不会再被选中）
	h = s.MarkSuccess("dead")
	if h.Status != StatusInvalid {
		t.Fatalf("invalid 是终态，不应自动恢复: %s", h.Status)
	}
}

func TestMarkFailure_额度耗尽立即冷却(t *testing.T) {
	// 上游报额度耗尽说明本地账与火山侧不一致，继续打只会持续报错。
	s, _, _ := newFixture(t, 1)
	h := s.MarkFailure("k", FailureQuota)
	if h.Status != StatusCooldown {
		t.Fatalf("额度耗尽应立即冷却: %s", h.Status)
	}
}

func TestMarkFailure_健康度过低自动冷却(t *testing.T) {
	s, _, _ := newFixture(t, 1)
	for i := 0; i < 30; i++ {
		s.MarkFailure("k", FailureTimeout)
	}
	h := s.Health("k")
	if h.Status != StatusCooldown {
		t.Fatalf("健康度跌破 %d 应冷却: %+v", healthCooldownFloor, h)
	}
	if h.Score < healthMin {
		t.Fatalf("健康度不应为负: %d", h.Score)
	}
}

func TestSetKeyStatus_显式封禁与恢复(t *testing.T) {
	cfg := testCfg()
	cfg.EnablePersona = false
	fs := &fakeStore{
		history: map[store.HistoryKey]store.KeyDailyHistory{},
		keys: []store.UpstreamKey{
			{KeyID: "k1", Secret: "s", Status: store.KeyStatusActive, HealthScore: 100},
		},
	}
	s := New(testConf(cfg), fs, &fakeQuota{})
	s.SetClock(testTime)
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	s.SetKeyStatus("k1", StatusBanned)
	if _, err := s.Select(context.Background(), Request{Now: testTime()}); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("已封禁的 Key 不应被选中: %v", err)
	}
	if !contains(mustErr(s.Select(context.Background(), Request{Now: testTime()})), "不健康") {
		t.Fatal("错误未指出健康原因")
	}

	s.SetKeyStatus("k1", StatusActive)
	if _, err := s.Select(context.Background(), Request{Now: testTime()}); err != nil {
		t.Fatalf("恢复后应可选: %v", err)
	}
}

func TestHealthAll_返回全部快照(t *testing.T) {
	s, _, _ := newFixture(t, 3)
	s.MarkSuccess("a")
	s.MarkFailure("b", Failure5xx)

	all := s.HealthAll()
	if len(all) < 2 {
		t.Fatalf("期望至少 2 条健康记录，实际 %d", len(all))
	}
	if all["b"].Failures != 1 {
		t.Fatalf("失败计数未记录: %+v", all["b"])
	}
	if all["a"].Successes != 1 {
		t.Fatalf("成功计数未记录: %+v", all["a"])
	}
}

func TestHealth_未知Key返回满分默认值(t *testing.T) {
	s, _, _ := newFixture(t, 1)
	h := s.Health("从未见过")
	if h.Score != healthMax || h.Status != StatusActive {
		t.Fatalf("未知 Key 应返回满分 active: %+v", h)
	}
}

// ---------- 打分维度 ----------

func TestScoreQuota_分段规则(t *testing.T) {
	hard := int64(1000)
	cases := []struct {
		name string
		snap quota.Snapshot
		want float64
	}{
		{"快照缺失视为满额", quota.Snapshot{}, 100},
		{"全额剩余", quota.Snapshot{Hard: hard, Used: 0}, 100},      // 100+10 截断到 100
		{"剩余60%加分", quota.Snapshot{Hard: hard, Used: 400}, 70},  // 60+10
		{"剩余40%正常", quota.Snapshot{Hard: hard, Used: 600}, 40},  // 无调整
		{"剩余15%紧张扣分", quota.Snapshot{Hard: hard, Used: 850}, 5}, // 15-10
		{"剩余为0", quota.Snapshot{Hard: hard, Used: hard}, 0},
		{"超额不为负", quota.Snapshot{Hard: hard, Used: hard * 2}, 0},
		{"预扣计入消耗", quota.Snapshot{Hard: hard, Used: 300, Prededuct: 300}, 40},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scoreQuota(c.snap)
			if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("scoreQuota = %v, 期望 %v", got, c.want)
			}
		})
	}
}

func TestScoreHistory_昨日刷满受罚(t *testing.T) {
	cases := []struct {
		name string
		h    store.KeyDailyHistory
		has  bool
		want float64
	}{
		{"无历史给基础分", store.KeyDailyHistory{}, false, 25},
		{"昨日刷满罚15", store.KeyDailyHistory{TokenRatio: 0.98}, true, -15},
		{"昨日恰好95%不罚", store.KeyDailyHistory{TokenRatio: 0.95}, true, 25 * 0.05},
		{"昨日消耗90%", store.KeyDailyHistory{TokenRatio: 0.9}, true, 2.5},
		{"昨日消耗40%含1天连续", store.KeyDailyHistory{TokenRatio: 0.4, ConsecutiveLightDays: 1}, true, 18},
		{"昨日消耗10%含3天连续", store.KeyDailyHistory{TokenRatio: 0.1, ConsecutiveLightDays: 3}, true, 31.5},
		{"连续加分封顶15", store.KeyDailyHistory{TokenRatio: 0, ConsecutiveLightDays: 99}, true, 40},
		{"ratio由used推导", store.KeyDailyHistory{TokenUsed: 500, TokenLimit: 1000}, true, 12.5},
		// 按次计费上游（商汤公测）的 token_used 恒为 0，额度也是按次给的。
		// 只看 TokenUsed 会让刷满次数的 Key 拿到满额历史分 —— 与「昨日刷满
		// 必须让位」的反封禁规则正好相反，越危险的 Key 越优先被选中。
		{"按次计费刷满同样受罚", store.KeyDailyHistory{CountUsed: 1300, TokenLimit: 1260}, true, -15},
		{"按次计费半额正常打分", store.KeyDailyHistory{CountUsed: 630, TokenLimit: 1260}, true, 12.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scoreHistory(c.h, c.has)
			if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("scoreHistory = %v, 期望 %v", got, c.want)
			}
		})
	}
}

func TestScoreHistory_昨日刷满的Key在打分中排最后(t *testing.T) {
	// 连续每日刷满是账号被盯上的最强特征，必须让位。
	fresh := scoreHistory(store.KeyDailyHistory{TokenRatio: 0.1, ConsecutiveLightDays: 2}, true)
	burned := scoreHistory(store.KeyDailyHistory{TokenRatio: 0.99}, true)
	if burned >= fresh {
		t.Fatalf("刷满的 Key 得分不低于清闲的 Key: %v vs %v", burned, fresh)
	}
	if burned >= 0 {
		t.Fatalf("刷满应为负分: %v", burned)
	}
}

func TestScorePersona_淘汰与偏好(t *testing.T) {
	p := persona.For("volc_001")

	// 关闭时不过滤
	sc, drop := scorePersona(p, Request{Now: testTime()}, false)
	if drop || sc != 100 {
		t.Fatalf("关闭 persona 应满分不淘汰: %v %v", sc, drop)
	}
	// nil 画像不过滤
	if sc, drop := scorePersona(nil, Request{Now: testTime()}, true); drop || sc != 100 {
		t.Fatalf("nil 画像应满分不淘汰: %v %v", sc, drop)
	}

	// 找到不活跃时刻 → 应淘汰
	for h := 0; h < 24; h++ {
		at := time.Date(2026, 8, 23, h, 0, 0, 0, time.Local)
		sc, drop := scorePersona(p, Request{Now: at}, true)
		if p.IsActiveAt(at) {
			if drop {
				t.Fatalf("%02d 时活跃却被淘汰", h)
			}
			if sc <= 0 || sc > 100 {
				t.Fatalf("%02d 时分数越界: %v", h, sc)
			}
		} else {
			if !drop || sc != 0 {
				t.Fatalf("%02d 时不活跃应淘汰: %v %v", h, sc, drop)
			}
		}
	}
}

func TestScorePersona_首选模型得分更高(t *testing.T) {
	p := persona.For("volc_001")
	var at time.Time
	for h := 0; h < 24; h++ {
		cand := time.Date(2026, 8, 23, h, 0, 0, 0, time.Local)
		if p.IsActiveAt(cand) {
			at = cand
			break
		}
	}

	pref, _ := scorePersona(p, Request{Now: at, Model: p.PreferredModels[0]}, true)
	other, _ := scorePersona(p, Request{Now: at, Model: "unknown-model"}, true)
	if pref <= other {
		t.Fatalf("首选模型得分应更高: %v vs %v", pref, other)
	}
	// 非偏好模型也不能被压到 0，否则小众模型无 Key 可用
	if other <= 0 {
		t.Fatalf("非偏好模型分数不应为 0: %v", other)
	}
}

func TestScoreAll_不改变状态(t *testing.T) {
	// 样本量取 40 而非 10: 画像窄化后任一时刻约 1/7 的 Key 活跃，
	// 10 个 Key 在 testTime() 可能一个都不活跃，activeKeyAt 会直接 Fatal。
	const n = 40
	s, _, _ := newFixture(t, n)
	before := s.Health(activeKeyAt(t, s, testTime()))

	scores := s.ScoreAll(Request{Now: testTime(), Model: "deepseek-v3"})
	if len(scores) != n {
		t.Fatalf("期望 %d 条打分，实际 %d", n, len(scores))
	}
	after := s.Health(before.KeyID)
	if !after.LastSelectedAt.Equal(before.LastSelectedAt) {
		t.Fatal("ScoreAll 不应记录选中时刻")
	}
}

// ---------- Reload ----------

func TestReload_装载失败返回错误(t *testing.T) {
	cfg := testCfg()
	fs := &fakeStore{listErr: errors.New("库挂了")}
	s := New(testConf(cfg), fs, &fakeQuota{})
	if err := s.Reload(context.Background()); err == nil {
		t.Fatal("装载失败应返回错误")
	}
}

func TestReload_历史查询失败时降级为无历史而非整体失败(t *testing.T) {
	// 缺历史时 S_history 退化为中性分，比整体拒绝服务好得多。
	cfg := testCfg()
	cfg.EnablePersona = false
	fs := &fakeStore{
		histErr: errors.New("历史表挂了"),
		keys: []store.UpstreamKey{
			{KeyID: "k1", Secret: "s", Status: store.KeyStatusActive, HealthScore: 100},
		},
	}
	s := New(testConf(cfg), fs, &fakeQuota{})
	s.SetClock(testTime)
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("历史查询失败不应导致 Reload 失败: %v", err)
	}
	c, err := s.Select(context.Background(), Request{Now: testTime()})
	if err != nil {
		t.Fatal(err)
	}
	if c.Score.History != 25 {
		t.Fatalf("无历史应给中性基础分，实际 %v", c.Score.History)
	}
}

func TestReload_只装载active状态的Key(t *testing.T) {
	cfg := testCfg()
	fs := &fakeStore{
		history: map[store.HistoryKey]store.KeyDailyHistory{},
		keys: []store.UpstreamKey{
			{KeyID: "ok", Secret: "s", Status: store.KeyStatusActive, HealthScore: 100},
			{KeyID: "banned", Secret: "s", Status: store.KeyStatusBanned, HealthScore: 0},
			{KeyID: "cool", Secret: "s", Status: store.KeyStatusCooldown, HealthScore: 50},
		},
	}
	s := New(testConf(cfg), fs, &fakeQuota{})
	s.SetClock(testTime)
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.PoolSize() != 1 {
		t.Fatalf("只应装载 active 的 Key，实际 %d 个", s.PoolSize())
	}
}

func TestReload_不覆盖运行时健康度(t *testing.T) {
	// 库中的 health_score 是旧快照，内存里的是实时观测值。
	// 重载 Key 池时用旧快照覆盖实时值，会把刚刚探测到的问题一笔抹掉。
	s, fs, _ := newFixture(t, 3)
	s.mu.RLock()
	id := s.keys[0].keyID
	s.mu.RUnlock()

	for i := 0; i < 3; i++ {
		s.MarkFailure(id, Failure5xx)
	}
	degraded := s.Health(id).Score

	fs.mu.Lock()
	for i := range fs.keys {
		fs.keys[i].HealthScore = 100
	}
	fs.mu.Unlock()

	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.Health(id).Score; got != degraded {
		t.Fatalf("重载覆盖了实时健康度: %d -> %d", degraded, got)
	}
}

// ---------- 并发 ----------

func TestSelect_并发选择无数据竞争(t *testing.T) {
	s, _, _ := newFixture(t, 50)
	ctx := context.Background()
	now := testTime()

	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				c, err := s.Select(ctx, Request{Model: "deepseek-v3", Now: now})
				if err != nil && !errors.Is(err, ErrNoCandidate) {
					t.Errorf("Select: %v", err)
					return
				}
				if c != nil {
					s.MarkSuccess(c.KeyID)
				}
			}
		}()
	}
	wg.Wait()
}

func TestSelect_与Reload及快照刷新并发(t *testing.T) {
	s, _, fq := newFixture(t, 50)
	ctx := context.Background()

	stop := make(chan struct{})
	var wg sync.WaitGroup // 有限工作量的 goroutine
	var bg sync.WaitGroup // 需 stop 信号才退出的后台 goroutine

	// 节流到 1ms: 目的是与 Select 交错触发读写竞争，而非压测吞吐。
	// 无节流的紧密循环在 race 模式下会因海量分配被 OOM kill。
	bg.Add(1)
	go func() {
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
				_ = s.Reload(ctx)
			}
		}
	}()

	bg.Add(1)
	go func() {
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
				s.RefreshSnapshot(ctx)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			fq.set(fmt.Sprintf("volc_%03d", i%50),
				quota.Snapshot{Used: int64(i) * 1000, Hard: 4_500_000, Soft: 4_000_000})
		}
	}()

	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				c, err := s.Select(ctx, Request{Model: "deepseek-v3", Now: testTime()})
				if err != nil && !errors.Is(err, ErrNoCandidate) {
					t.Errorf("Select: %v", err)
					return
				}
				if c != nil {
					if i%3 == 0 {
						s.MarkFailure(c.KeyID, Failure5xx)
					} else {
						s.MarkSuccess(c.KeyID)
					}
				}
				_ = s.HealthAll()
				_ = s.ScoreAll(Request{Now: testTime()})
			}
		}()
	}
	wg.Wait()
	close(stop)
	bg.Wait()
}

// ---------- 性能 ----------

// BenchmarkSelect 验证 P1-7 的判断: 100 个 Key 朴素遍历是微秒级，
// 相对 3 秒级的上游 LLM 调用完全不可测，分桶采样纯属过度设计。
func BenchmarkSelect(b *testing.B) {
	cfg := config.Default().Scheduler
	cfg.MinRequestInterval = 0
	qcfg := config.Default().Quota

	fs := &fakeStore{history: map[store.HistoryKey]store.KeyDailyHistory{}}
	fq := &fakeQuota{snaps: map[string]quota.Snapshot{}}
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("volc_%03d", i)
		fs.keys = append(fs.keys, store.UpstreamKey{
			KeyID: id, Secret: "s", Status: store.KeyStatusActive, HealthScore: 100,
		})
		fq.snaps[id] = quota.Snapshot{
			Used: int64(i) * 10_000, Hard: qcfg.TokenHard(), Soft: qcfg.TokenSoft(),
		}
	}

	s := New(testConf(cfg), fs, fq)
	s.SetClock(testTime)
	if err := s.Reload(context.Background()); err != nil {
		b.Fatal(err)
	}
	s.RefreshSnapshot(context.Background())

	ctx := context.Background()
	req := Request{Model: "deepseek-v3", Now: testTime()}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Select(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------- 辅助 ----------

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func mustErr(_ *Candidate, err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---------- 按档位分配流量 ----------

// newPooledFixture 造一个混合档位的池。counts 形如 {"hot":10,"cold":100}。
//
// 刻意让 cold 档的 Key 数量远多于 hot —— 这正是真实分层的形态，
// 也是「不按份额抽档就会让 cold 凭数量优势吃掉流量」这个问题的复现条件。
func newPooledFixture(t *testing.T, counts map[string]int,
	opts ...func(*config.Scheduler)) (*Scheduler, *fakeStore) {
	t.Helper()

	cfg := testCfg()
	for _, o := range opts {
		o(&cfg)
	}

	fs := &fakeStore{history: map[store.HistoryKey]store.KeyDailyHistory{}}
	i := 0
	// 固定档位顺序，保证 Key ID 与档位的对应关系可复现
	for _, pool := range []string{PoolHot, PoolWarm, PoolCold} {
		for n := 0; n < counts[pool]; n++ {
			id := fmt.Sprintf("%s_%03d", pool, n)
			fs.keys = append(fs.keys, store.UpstreamKey{
				KeyID: id, Secret: "secret-" + id, Status: store.KeyStatusActive,
				Pool: pool, EgressIP: fmt.Sprintf("172.16.0.%d", i%250+2),
				HealthScore: 100,
			})
			i++
		}
	}

	s := New(testConf(cfg), fs, &fakeQuota{})
	s.SetClock(testTime)
	s.SetRandSource(42)
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	s.RefreshSnapshot(context.Background())
	return s, fs
}

// poolDistribution 连续调度 n 次，统计各档位被选中的比例。
func poolDistribution(t *testing.T, s *Scheduler, n int) map[string]float64 {
	t.Helper()
	counts := map[string]int{}
	got := 0
	for i := 0; i < n; i++ {
		c, err := s.Select(context.Background(), Request{Now: testTime(), Model: "deepseek-v3"})
		if err != nil {
			continue
		}
		counts[c.Pool]++
		got++
	}
	if got == 0 {
		t.Fatal("一次都没调度成功，无法统计分布")
	}
	out := map[string]float64{}
	for p, c := range counts {
		out[p] = float64(c) / float64(got)
	}
	return out
}

// 核心语义: 流量按 PoolShares 分配，与各档的 Key 数量无关。
//
// 这是 cold 档能安全设更高 max_keys 的前提 —— cold 档 Key 数量最多，
// 若不按份额抽档，它会凭数量优势吃掉大部分流量，其单 IP 的瞬时并发
// 反而会成为全池最高，分层就完全失去意义。
func TestSelect_流量按档位份额分配(t *testing.T) {
	// cold 档 Key 数是 hot 的 20 倍，但只该拿到 5% 的流量
	s, _ := newPooledFixture(t, map[string]int{PoolHot: 5, PoolWarm: 20, PoolCold: 100},
		func(c *config.Scheduler) {
			c.PoolShares = map[string]float64{PoolHot: 70, PoolWarm: 25, PoolCold: 5}
		})

	dist := poolDistribution(t, s, 3000)
	t.Logf("实际分布: hot=%.1f%% warm=%.1f%% cold=%.1f%%",
		100*dist[PoolHot], 100*dist[PoolWarm], 100*dist[PoolCold])

	want := map[string]float64{PoolHot: 0.70, PoolWarm: 0.25, PoolCold: 0.05}
	for pool, exp := range want {
		got := dist[pool]
		// 容差 5 个百分点: 3000 次抽样的标准差约 0.8pp，5pp 足够宽松
		// 而又能抓出「配比完全没生效」（那时 cold 会拿到 80%）。
		if got < exp-0.05 || got > exp+0.05 {
			t.Errorf("%s 档实际占比 %.1f%%，期望 %.0f%%（±5pp）",
				pool, 100*got, 100*exp)
		}
	}
}

// 份额是相对值，不要求配置里加起来等于 100。
func TestSelect_份额无需归一化(t *testing.T) {
	s, _ := newPooledFixture(t, map[string]int{PoolHot: 10, PoolCold: 10},
		func(c *config.Scheduler) {
			// 3:1，等价于 75% / 25%
			c.PoolShares = map[string]float64{PoolHot: 3, PoolCold: 1}
		})

	dist := poolDistribution(t, s, 2000)
	t.Logf("实际分布: hot=%.1f%% cold=%.1f%%", 100*dist[PoolHot], 100*dist[PoolCold])
	if dist[PoolHot] < 0.70 || dist[PoolHot] > 0.80 {
		t.Errorf("hot 档占比 %.1f%%，期望 75%%（±5pp）", 100*dist[PoolHot])
	}
}

// 未配置份额时退化为对全体候选加权随机 —— 此时 Key 数量多的档位自然占优。
func TestSelect_未配置份额时按数量自然分布(t *testing.T) {
	s, _ := newPooledFixture(t, map[string]int{PoolHot: 10, PoolCold: 90},
		func(c *config.Scheduler) {
			c.PoolShares = nil
		})

	dist := poolDistribution(t, s, 2000)
	t.Logf("实际分布: hot=%.1f%% cold=%.1f%%", 100*dist[PoolHot], 100*dist[PoolCold])
	// 90% 的 Key 在 cold 档，分数相近时它应拿到约 90% 的流量
	if dist[PoolCold] < 0.80 {
		t.Errorf("未配置份额时 cold 档应按数量占优（约 90%%），实际 %.1f%%",
			100*dist[PoolCold])
	}
}

// 所有份额为 0 或负数等同于未配置，不得让服务不可用。
func TestSelect_份额全为零视为未启用(t *testing.T) {
	s, _ := newPooledFixture(t, map[string]int{PoolHot: 5, PoolCold: 5},
		func(c *config.Scheduler) {
			c.PoolShares = map[string]float64{PoolHot: 0, PoolCold: -1}
		})

	if _, err := s.Select(context.Background(),
		Request{Now: testTime(), Model: "deepseek-v3"}); err != nil {
		t.Fatalf("份额非法应退化为等权而非拒绝服务: %v", err)
	}
}

// ---------- 空档位的回退 ----------

// 目标档位无 Key 时，其份额应重新分配给其他档位而非让抽样落空。
func TestSelect_目标档位为空时回退(t *testing.T) {
	// 只有 cold 档有 Key，但份额表里 hot 占 70%
	s, _ := newPooledFixture(t, map[string]int{PoolCold: 20},
		func(c *config.Scheduler) {
			c.PoolShares = map[string]float64{PoolHot: 70, PoolWarm: 25, PoolCold: 5}
			c.PoolFallback = true
		})

	ok := 0
	fellBack := 0
	for i := 0; i < 500; i++ {
		c, err := s.Select(context.Background(), Request{Now: testTime(), Model: "deepseek-v3"})
		if err != nil {
			continue
		}
		ok++
		if c.Pool != PoolCold {
			t.Fatalf("只有 cold 档有 Key，却选出了 %s 档", c.Pool)
		}
		if c.PoolFellBack {
			fellBack++
		}
	}
	if ok < 450 {
		t.Errorf("启用回退后应几乎全部成功，实际 %d/500", ok)
	}
	// 95% 的抽样会落在空的 hot/warm 上，这些都应标记为回退
	if fellBack < ok*8/10 {
		t.Errorf("回退次数 %d/%d 偏低，PoolFellBack 可能未正确标记", fellBack, ok)
	}
	t.Logf("成功 %d/500，其中 %d 次为跨档回退", ok, fellBack)
}

// 禁用回退时，抽中空档位应直接失败 —— 严格配比的代价。
func TestSelect_禁用回退时抽中空档即失败(t *testing.T) {
	s, _ := newPooledFixture(t, map[string]int{PoolCold: 20},
		func(c *config.Scheduler) {
			c.PoolShares = map[string]float64{PoolHot: 70, PoolWarm: 25, PoolCold: 5}
			c.PoolFallback = false
		})

	ok, failed := 0, 0
	for i := 0; i < 500; i++ {
		_, err := s.Select(context.Background(), Request{Now: testTime(), Model: "deepseek-v3"})
		if err != nil {
			if !errors.Is(err, ErrNoCandidate) {
				t.Fatalf("失败应包装 ErrNoCandidate，实际 %v", err)
			}
			failed++
			continue
		}
		ok++
	}
	// hot+warm 占 95% 份额且都为空 → 约 95% 的请求应失败
	if failed < 400 {
		t.Errorf("禁用回退时应有约 95%% 失败，实际 %d/500", failed)
	}
	// cold 档的 5% 份额仍应成功，否则说明连正常路径都断了
	if ok == 0 {
		t.Error("cold 档有 Key，其 5%% 份额的请求应能成功")
	}
	t.Logf("成功 %d，失败 %d（禁用回退，hot/warm 均为空）", ok, failed)
}

// 回退时不得让 Key 数量最多的档位吃掉全部回退流量。
//
// 若回退实现为「对全体候选做加权随机」，cold 档（Key 最多）会几乎全拿。
// 正确做法是在其余档位间按份额重新归一化。
func TestSelect_回退按剩余份额而非数量分配(t *testing.T) {
	// hot 档为空。warm:cold 份额为 25:5（即 5:1），但 cold 的 Key 数是 warm 的 20 倍。
	s, _ := newPooledFixture(t, map[string]int{PoolWarm: 5, PoolCold: 100},
		func(c *config.Scheduler) {
			c.PoolShares = map[string]float64{PoolHot: 70, PoolWarm: 25, PoolCold: 5}
			c.PoolFallback = true
		})

	dist := poolDistribution(t, s, 3000)
	t.Logf("hot 档为空时的分布: warm=%.1f%% cold=%.1f%%",
		100*dist[PoolWarm], 100*dist[PoolCold])

	// hot 的 70% 份额按 25:5 重分配 → warm 得 25/30，cold 得 5/30
	if dist[PoolWarm] < 0.75 {
		t.Errorf("warm 档应按份额（25:5）拿到约 83%%，实际 %.1f%% —— "+
			"回退可能退化成了按数量分配", 100*dist[PoolWarm])
	}
}

// ---------- 档位归属的边界 ----------

// pool 为空的 Key 应归入 cold，与 schema 的 DEFAULT 'cold' 一致。
func TestPoolOf_空值归入cold(t *testing.T) {
	if got := (keyEntry{pool: ""}).poolOf(); got != PoolCold {
		t.Errorf("空 pool 应归入 %q，实际 %q", PoolCold, got)
	}
	if got := (keyEntry{pool: PoolHot}).poolOf(); got != PoolHot {
		t.Errorf("poolOf 不应改写已有值，实际 %q", got)
	}
}

// 档位名不在份额表里时退化为等权，而非拒绝服务 ——
// 配置错误不该表现为「服务不可用」这种难以定位的形态。
func TestSelect_未知档位退化为等权(t *testing.T) {
	s, _ := newPooledFixture(t, map[string]int{PoolHot: 10},
		func(c *config.Scheduler) {
			// 份额表里只有 warm/cold，而池里全是 hot
			c.PoolShares = map[string]float64{PoolWarm: 50, PoolCold: 50}
		})

	c, err := s.Select(context.Background(), Request{Now: testTime(), Model: "deepseek-v3"})
	if err != nil {
		t.Fatalf("候选档位不在份额表里时应退化为等权: %v", err)
	}
	if c.Pool != PoolHot {
		t.Errorf("应选出 hot 档 Key，实际 %q", c.Pool)
	}
}

// 档位遍历顺序必须确定，否则同一随机数的落点不可复现。
func TestPoolOrder_顺序确定(t *testing.T) {
	shares := map[string]float64{PoolCold: 1, PoolHot: 1, PoolWarm: 1, "custom": 1}
	want := []string{PoolHot, PoolWarm, PoolCold, "custom"}
	for i := 0; i < 20; i++ {
		got := poolOrder(shares)
		if len(got) != len(want) {
			t.Fatalf("长度 = %d, 期望 %d", len(got), len(want))
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("第 %d 次遍历顺序 = %v, 期望 %v", i+1, got, want)
			}
		}
	}
}

// NormalizedPoolShares 的归一化结果总和应为 1。
func TestNormalizedPoolShares_总和为一(t *testing.T) {
	cfg := config.Scheduler{PoolShares: map[string]float64{
		PoolHot: 70, PoolWarm: 25, PoolCold: 5,
	}}
	got := cfg.NormalizedPoolShares()
	var sum float64
	for _, v := range got {
		sum += v
	}
	if sum < 0.999 || sum > 1.001 {
		t.Errorf("归一化后总和 = %f, 期望 1.0", sum)
	}
	if got[PoolHot] < 0.699 || got[PoolHot] > 0.701 {
		t.Errorf("hot 份额 = %f, 期望 0.7", got[PoolHot])
	}

	// 负数与零应被剔除而非参与归一化
	cfg2 := config.Scheduler{PoolShares: map[string]float64{
		PoolHot: 10, PoolWarm: 0, PoolCold: -5,
	}}
	got2 := cfg2.NormalizedPoolShares()
	if len(got2) != 1 || got2[PoolHot] != 1 {
		t.Errorf("非正份额应被剔除，实际 %v", got2)
	}
}

// ---------- 出口级最小间隔 ----------

// fakeEgress 让测试精确控制「某个 Key 的出口上次何时被用过」。
//
// 真实的 egress.Pool 需要构造 IP 池并建立绑定，而这里要验证的是调度器
// 对 LastUsedOn 返回值的反应，不是出口池自身的行为。
type fakeEgress struct {
	// lastUsed 按 keyID 索引，缺失即视为该出口从未使用过。
	lastUsed map[string]time.Time
	// calls 记录被查询的次数，用于验证未启用时不做无谓调用。
	calls int
}

func (f *fakeEgress) LastUsedOn(keyID string) time.Time {
	f.calls++
	return f.lastUsed[keyID]
}

// 同一出口在间隔内不得被再次选中，哪怕是绑在它上面的另一个 Key。
//
// 这是出口级节流存在的全部理由: MinRequestInterval 只看单个 Key，
// 25 个 Key 各守 5 秒，出口层面仍是 5 QPS。
func TestSelect_出口间隔内的Key被排除(t *testing.T) {
	s, _, _ := newFixture(t, 120, func(c *config.Scheduler) {
		c.EgressMinInterval = 10 * time.Second
		c.MinRequestInterval = 0 // 隔离变量: 只验证出口级约束
	})

	// 先正常选出一个 Key，确认基线可用
	first, err := s.Select(context.Background(), Request{Now: testTime(), Model: "deepseek-v3"})
	if err != nil {
		t.Fatalf("基线 Select 应成功: %v", err)
	}

	// 把该 Key 的出口标记为「刚刚用过」
	fe := &fakeEgress{lastUsed: map[string]time.Time{
		first.KeyID: testTime().Add(-2 * time.Second),
	}}
	s.SetEgressReader(fe)

	// 连选多次都不该再选到它
	for i := 0; i < 20; i++ {
		got, err := s.Select(context.Background(), Request{Now: testTime(), Model: "deepseek-v3"})
		if err != nil {
			t.Fatalf("第 %d 次 Select 失败: %v", i+1, err)
		}
		if got.KeyID == first.KeyID {
			t.Fatalf("出口刚用过 2 秒（间隔 10 秒），不应再选中 %s", first.KeyID)
		}
	}
}

// 超过间隔后应重新可选 —— 节流是延后而非永久排除。
func TestSelect_出口间隔外的Key可选(t *testing.T) {
	s, _, _ := newFixture(t, 120, func(c *config.Scheduler) {
		c.EgressMinInterval = 10 * time.Second
		c.MinRequestInterval = 0
	})
	first, err := s.Select(context.Background(), Request{Now: testTime(), Model: "deepseek-v3"})
	if err != nil {
		t.Fatal(err)
	}

	// 上次使用已在 30 秒前，远超 10 秒间隔
	s.SetEgressReader(&fakeEgress{lastUsed: map[string]time.Time{
		first.KeyID: testTime().Add(-30 * time.Second),
	}})

	var hit bool
	for i := 0; i < 200; i++ {
		got, err := s.Select(context.Background(), Request{Now: testTime(), Model: "deepseek-v3"})
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if got.KeyID == first.KeyID {
			hit = true
			break
		}
	}
	if !hit {
		t.Errorf("出口上次使用已过 30 秒，%s 应重新可选", first.KeyID)
	}
}

// EgressMinInterval=0 时不得查询出口 —— 未启用的特性不该在热路径上产生开销。
func TestSelect_未配置出口间隔时不查询(t *testing.T) {
	s, _, _ := newFixture(t, 20, func(c *config.Scheduler) {
		c.EgressMinInterval = 0
	})
	fe := &fakeEgress{lastUsed: map[string]time.Time{}}
	s.SetEgressReader(fe)

	if _, err := s.Select(context.Background(), Request{Now: testTime(), Model: "deepseek-v3"}); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if fe.calls != 0 {
		t.Errorf("未配置出口间隔时不应查询出口，实际查询 %d 次", fe.calls)
	}
}

// 未注入出口视图时（direct 模式或装配遗漏）不得 panic，退化为不约束。
func TestSelect_未注入出口视图时不约束(t *testing.T) {
	s, _, _ := newFixture(t, 20, func(c *config.Scheduler) {
		c.EgressMinInterval = 10 * time.Second
	})
	// 刻意不调 SetEgressReader
	if _, err := s.Select(context.Background(), Request{Now: testTime(), Model: "deepseek-v3"}); err != nil {
		t.Fatalf("未注入出口视图时应正常工作: %v", err)
	}
}

// 全部出口都在间隔内时，错误必须指明是出口密度到顶而非 Key 不健康 ——
// 两者的处置完全不同: 前者要加 IP 或放宽间隔，后者要查 Key 状态。
func TestSelect_出口全忙时错误可诊断(t *testing.T) {
	s, _, _ := newFixture(t, 20, func(c *config.Scheduler) {
		c.EgressMinInterval = time.Minute
		c.MinRequestInterval = 0
	})

	// 把所有 Key 的出口都标记为刚用过
	lu := map[string]time.Time{}
	for _, sc := range s.ScoreAll(Request{Now: testTime(), Model: "deepseek-v3"}) {
		lu[sc.KeyID] = testTime().Add(-time.Second)
	}
	s.SetEgressReader(&fakeEgress{lastUsed: lu})

	_, err := s.Select(context.Background(), Request{Now: testTime(), Model: "deepseek-v3"})
	if err == nil {
		t.Fatal("所有出口都在间隔内，应返回错误")
	}
	if !errors.Is(err, ErrNoCandidate) {
		t.Errorf("错误应包装 ErrNoCandidate: %v", err)
	}
	// 断言计数值而非字段名。
	//
	// rejectReasons.String() 无条件列出所有字段，「出口间隔不足」这几个字
	// 永远在错误里 —— 只查子串的断言恒真，测不出计数被错记到 tooSoon 上。
	if !strings.Contains(err.Error(), "出口间隔不足=20") {
		t.Errorf("20 个 Key 全因出口间隔被淘汰，计数应记在 egressTooSoon 上。"+
			"若记成 tooSoon，运维会误判为「单 Key 太热需扩池」，"+
			"而实情是出口密度到顶需加 IP。实际: %v", err)
	}
}

// ---------- 份额 → 单 IP 请求密度 ----------

// 本用例是「cold 档可以塞更多 Key」这个结论的唯一依据，串起完整链路:
// 份额配比 → Key 被选中的频率 → 各出口 IP 承接的请求数。
//
// 分层的真正约束不是「一个 IP 上绑了多少 Key」，而是**单个 IP 单位时间承接
// 多少请求**。cold 档绑 100 个 Key 却只拿 5% 流量时，它的请求密度反而低于
// hot 档绑 25 个拿 70% —— 这才是提高 cold 档 max_keys 的依据。
//
// 改动前调度器不看 pool，cold 档凭 Key 数量优势吃掉约 79% 流量（已由
// TestSelect_流量按档位份额分配 的反向验证实证），此时 100 Key/IP 是危险的。
func TestSelect_份额决定单IP请求密度(t *testing.T) {
	// 与 deploy/README.md 的推荐配置逐项对齐: Key 按 1:2:7 分布
	// （100/200/700），份额 70:25:5。两处数字必须一致 —— 文档给的是
	// 可直接复制的 EGRESS_IPS，本测试验证它成立。
	counts := map[string]int{PoolHot: 100, PoolWarm: 200, PoolCold: 700}
	s, _ := newPooledFixture(t, counts, func(c *config.Scheduler) {
		c.PoolShares = map[string]float64{PoolHot: 70, PoolWarm: 25, PoolCold: 5}
		c.PoolFallback = true
	})

	// 每档的出口 IP 数，与 deploy/README.md 的 32 IP 方案逐项一致。
	//
	// 分配依据不是按 Key 数量平分，而是「把富余 IP 全给密度瓶颈档」:
	// 撤离余量给出的下限是 hot 11 / warm 5 / cold 8（公式见文档），
	// 余出的 8 个全部补给 hot —— 它吃 70% 流量却只有 100 个 Key，
	// 是唯一的瓶颈；多给 IP 直接摊薄单 IP 速率。
	ipsPerPool := map[string]int{PoolHot: 19, PoolWarm: 5, PoolCold: 8}

	// realQPS 是 deploy/README.md 记录的实测总流量。
	// 把相对份额换算成绝对速率才有物理意义 —— 风控看的是
	// 「这个 IP 每秒发了多少请求」，与其他 IP 的比值无关。
	const realQPS = 20.0

	const n = 20000
	hits := map[string]int{}
	for i := 0; i < n; i++ {
		c, err := s.Select(context.Background(), Request{Now: testTime(), Model: "deepseek-v3"})
		if err != nil {
			continue
		}
		hits[c.Pool]++
	}

	type row struct {
		pool     string
		share    float64
		keys     int
		ips      int
		reqs     int
		ipQPS    float64 // 单 IP 每秒请求数
		keyPerHr float64 // 单 Key 每小时请求数
	}
	var rows []row
	for _, pool := range []string{PoolHot, PoolWarm, PoolCold} {
		r := row{pool: pool, keys: counts[pool], ips: ipsPerPool[pool], reqs: hits[pool]}
		r.share = float64(r.reqs) / float64(n)
		r.ipQPS = realQPS * r.share / float64(r.ips)
		r.keyPerHr = realQPS * r.share / float64(r.keys) * 3600
		rows = append(rows, r)
	}
	for _, r := range rows {
		t.Logf("%-5s 份额=%5.1f%% Key=%3d IP=%2d 单IP=%5.2f req/s 单Key=%7.1f req/h",
			r.pool, 100*r.share, r.keys, r.ips, r.ipQPS, r.keyPerHr)
	}

	// 断言 1: 任一出口的绝对请求速率不得超过 2 req/s。
	//
	// 判据是绝对值而非档位间比值: 风控关心「这个 IP 每秒发多少请求」，
	// 与其他 IP 无关。按当前分配最高点是 warm 档 1.01 req/s
	// （它只有 5 个 IP 却吃 25% 流量，比 hot 档 19 个 IP 吃 70% 更密）。
	//
	// 曾经用「档位间密度比 < 3」做判据，结果无论怎么分配都无解 ——
	// cold 档份额只有 5%，它的单 IP 速率天然远低于 hot 档，
	// 强行拉平比值只会逼着 cold 档减少 IP 数，反而装不下 Key。
	for _, r := range rows {
		if r.ipQPS > 2.0 {
			t.Errorf("%s 档单 IP 速率 %.2f req/s 超过上限 2.0。"+
				"给该档增加出口 IP 或下调其 pool_shares", r.pool, r.ipQPS)
		}
	}

	// 断言 2: cold 档单 Key 的请求频率必须显著低于 hot 档。
	//
	// 这是 cold 档能绑 100 个 Key 的直接依据 —— 单 Key 频率越低，
	// 同一 IP 上多个 Key 的行为越稀疏，聚集特征越弱。
	var hotPerKey, coldPerKey float64
	for _, r := range rows {
		switch r.pool {
		case PoolHot:
			hotPerKey = r.keyPerHr
		case PoolCold:
			coldPerKey = r.keyPerHr
		}
	}
	if coldPerKey == 0 {
		t.Fatal("cold 档零请求，份额配比可能未生效")
	}
	if ratio := hotPerKey / coldPerKey; ratio < 20 {
		t.Errorf("hot 档单 Key 频率仅为 cold 档的 %.1f 倍（期望 ≥20 倍）。"+
			"频率差不够大时，cold 档不应配置远高于 hot 档的 max_keys", ratio)
	}
	t.Logf("单 Key 频率比 hot:cold = %.0f:1 —— cold 档 max_keys 可相应放大",
		hotPerKey/coldPerKey)

	// 断言 3: cold 档单 Key 频率必须低到「一个 IP 上叠 100 个 Key 也不密集」。
	//
	// cold 档 max_keys=100，单 Key 约 5 req/h，该 IP 合计约 500 req/h
	// （0.14 req/s）—— 仍远低于 hot 档单 IP 的 2647 req/h。这个不等式
	// 成立，「用低频换高密度」才是安全的。
	const coldMaxKeys = 100
	coldIPPerHr := coldPerKey * coldMaxKeys
	var hotIPPerHr float64
	for _, r := range rows {
		if r.pool == PoolHot {
			hotIPPerHr = r.ipQPS * 3600
		}
	}
	if coldIPPerHr > hotIPPerHr {
		t.Errorf("cold 档满载单 IP 速率 %.0f req/h 已超过 hot 档 %.0f req/h，"+
			"此时 max_keys=%d 失去依据 —— 应下调 cold 的 max_keys 或其份额",
			coldIPPerHr, hotIPPerHr, coldMaxKeys)
	}
	t.Logf("cold 档满载(%d Key/IP)单 IP %.0f req/h vs hot 档 %.0f req/h",
		coldMaxKeys, coldIPPerHr, hotIPPerHr)
}
