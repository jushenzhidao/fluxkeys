package quota

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: 15})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("跳过: 无可用 Redis (%s): %v", addr, err)
	}
	rdb.FlushDB(ctx)
	t.Cleanup(func() {
		rdb.FlushDB(context.Background())
		rdb.Close()
	})
	return rdb
}

func newTestManager(t *testing.T) (*Manager, context.Context) {
	t.Helper()
	ctx := context.Background()
	m, err := NewManager(ctx, testRedis(t))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m, ctx
}

// P0-1 核心验证: 高并发下 Lua 原子预扣绝不允许突破硬水位。
//
// 这正是 V3「本地内存标记 + 异步刷 Redis」方案会失败的场景 ——
// 多个并发请求各自读到陈旧的 used 值，同时判定「还有余额」而全部放行。
func TestAcquire_NeverExceedsHardLimit_UnderConcurrency(t *testing.T) {
	m, ctx := newTestManager(t)

	const (
		hard      = 100_000
		amount    = 1_000
		goroutine = 200 // 总需求 200_000，是硬水位的 2 倍
	)
	lim := Limits{Hard: hard, Soft: hard * 8 / 10}

	var granted, denied atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < goroutine; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 尽可能同时发起，最大化竞争
			_, lease, err := m.Acquire(ctx, "volc", "key_hot", KindToken, amount, lim, time.Minute)
			switch {
			case err == nil && lease != nil:
				granted.Add(1)
			case errors.Is(err, ErrInsufficient):
				denied.Add(1)
			default:
				t.Errorf("意外错误: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	wantGranted := int64(hard / amount) // 恰好 100 次
	if granted.Load() != wantGranted {
		t.Errorf("放行 %d 次, 期望恰好 %d 次", granted.Load(), wantGranted)
	}
	if granted.Load()+denied.Load() != goroutine {
		t.Errorf("放行+拒绝 = %d, 期望 %d", granted.Load()+denied.Load(), goroutine)
	}

	snap, err := m.Get(ctx, "volc", "key_hot", KindToken)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// 不变量: used + prededuct 永不超过硬水位
	if total := snap.Used + snap.Prededuct; total > hard {
		t.Fatalf("超刷! used+prededuct = %d > hard = %d", total, hard)
	}
	if snap.Prededuct != hard {
		t.Errorf("prededuct = %d, 期望占满 %d", snap.Prededuct, hard)
	}
}

// 并发 Acquire/Commit 混合场景下不变量依然成立。
func TestAcquireCommit_InvariantHolds(t *testing.T) {
	m, ctx := newTestManager(t)

	const hard = 50_000
	lim := Limits{Hard: hard, Soft: hard * 8 / 10}

	var wg sync.WaitGroup
	for i := 0; i < 120; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, lease, err := m.Acquire(ctx, "volc", "key_mix", KindToken, 1000, lim, time.Minute)
			if err != nil {
				return // 配额不足是预期结果
			}
			// 实际用量小于预扣量（真实场景的常态）
			if err := m.Commit(ctx, lease, 400); err != nil {
				t.Errorf("Commit: %v", err)
			}
		}(i)
	}
	wg.Wait()

	snap, err := m.Get(ctx, "volc", "key_mix", KindToken)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if snap.Prededuct != 0 {
		t.Errorf("全部提交后 prededuct 应归零, got %d", snap.Prededuct)
	}
	if snap.Used+snap.Prededuct > hard {
		t.Fatalf("超刷! used=%d prededuct=%d hard=%d", snap.Used, snap.Prededuct, hard)
	}
	if snap.Used == 0 {
		t.Error("used 应累计实际消耗量")
	}
}

// 软水位应触发降权信号，但不阻断调度。
func TestAcquire_SoftWatermarkSignalsDegrade(t *testing.T) {
	m, ctx := newTestManager(t)
	lim := Limits{Hard: 1000, Soft: 500}

	d, lease, err := m.Acquire(ctx, "volc", "key_soft", KindToken, 400, lim, time.Minute)
	if err != nil || d != Granted {
		t.Fatalf("软水位以下应正常放行, got d=%v err=%v", d, err)
	}
	_ = m.Commit(ctx, lease, 400)

	d, lease, err = m.Acquire(ctx, "volc", "key_soft", KindToken, 200, lim, time.Minute)
	if err != nil {
		t.Fatalf("软水位之上仍应放行: %v", err)
	}
	if d != GrantedLow {
		t.Errorf("越过软水位应返回 GrantedLow, got %v", d)
	}
	_ = m.Commit(ctx, lease, 200)

	if _, _, err := m.Acquire(ctx, "volc", "key_soft", KindToken, 500, lim, time.Minute); !errors.Is(err, ErrInsufficient) {
		t.Errorf("越过硬水位必须拒绝, got %v", err)
	}
}

// P0-2 核心验证: SSE 断连导致 Commit 永不执行时，租约必须被回收。
//
// V3 无此机制，prededuct 会被永久占用，数日后出现「配额虚耗殆尽
// 但火山侧实际未用」的假枯竭。
func TestReap_ReclaimsLeakedPrededuct(t *testing.T) {
	m, ctx := newTestManager(t)
	lim := Limits{Hard: 10_000, Soft: 8_000}

	base := time.Now()
	m.SetClock(func() time.Time { return base })

	// 模拟 5 个请求预扣后客户端断开，Commit 永不到达
	for i := 0; i < 5; i++ {
		if _, _, err := m.Acquire(ctx, "volc", "key_leak", KindToken, 1000, lim, 60*time.Second); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
	}

	snap, _ := m.Get(ctx, "volc", "key_leak", KindToken)
	if snap.Prededuct != 5000 {
		t.Fatalf("回收前 prededuct = %d, 期望 5000", snap.Prededuct)
	}

	// 租约未到期时不应被回收
	if n, err := m.Reap(ctx, "volc", 100); err != nil || n != 0 {
		t.Fatalf("未到期租约不应回收, n=%d err=%v", n, err)
	}

	// 时间推进到租约过期之后
	m.SetClock(func() time.Time { return base.Add(90 * time.Second) })

	n, err := m.Reap(ctx, "volc", 100)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n != 5 {
		t.Errorf("回收 %d 条, 期望 5 条", n)
	}

	snap, _ = m.Get(ctx, "volc", "key_leak", KindToken)
	if snap.Prededuct != 0 {
		t.Errorf("回收后 prededuct 应归零, got %d", snap.Prededuct)
	}
	// 泄漏的预扣不应计入实际用量
	if snap.Used != 0 {
		t.Errorf("回收不应计入 used, got %d", snap.Used)
	}
}

// 非 volc provider 的租约回收必须真正还原 prededuct。
//
// 回归背景: luaReap 曾把租约数据前缀写死为 'volc:lease:data:'，对其他
// provider 执行时 ZREM 成功但 prededuct 永不还原 —— 回收器每轮报成功，
// 额度却持续泄漏。
func TestReap_NonVolcProvider(t *testing.T) {
	m, ctx := newTestManager(t)
	lim := Limits{Hard: 1_000, Soft: 800}

	base := time.Now()
	m.SetClock(func() time.Time { return base })

	if _, _, err := m.Acquire(ctx, "sensenova", "sn_key1", KindCount, 3, lim, 30*time.Second); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	snap, _ := m.Get(ctx, "sensenova", "sn_key1", KindCount)
	if snap.Prededuct != 3 {
		t.Fatalf("预扣未生效: %+v", snap)
	}

	m.SetClock(func() time.Time { return base.Add(60 * time.Second) })
	n, err := m.Reap(ctx, "sensenova", 100)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("回收 %d 条, 期望 1", n)
	}

	snap, _ = m.Get(ctx, "sensenova", "sn_key1", KindCount)
	if snap.Prededuct != 0 {
		t.Errorf("非 volc provider 回收后 prededuct 应归零, got %d", snap.Prededuct)
	}
	// 租约数据 Hash 也必须被删除，而不是只从 ZSET 摘除
	keys, _ := m.rdb.Keys(ctx, "sensenova:lease:data:*").Result()
	if len(keys) != 0 {
		t.Errorf("租约数据未清理: %v", keys)
	}
}

// 回收后迟到的 Commit 仍须补记实际用量，否则会少算导致后续超刷。
func TestCommit_AfterReap_StillRecordsUsage(t *testing.T) {
	m, ctx := newTestManager(t)
	lim := Limits{Hard: 10_000, Soft: 8_000}

	base := time.Now()
	m.SetClock(func() time.Time { return base })

	_, lease, err := m.Acquire(ctx, "volc", "key_late", KindToken, 1000, lim, 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	m.SetClock(func() time.Time { return base.Add(60 * time.Second) })
	if _, err := m.Reap(ctx, "volc", 100); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	// 请求实际完成了，只是耗时超过了租约 TTL
	if err := m.Commit(ctx, lease, 750); err != nil {
		t.Fatalf("迟到的 Commit: %v", err)
	}

	snap, _ := m.Get(ctx, "volc", "key_late", KindToken)
	if snap.Used != 750 {
		t.Errorf("used = %d, 期望补记 750", snap.Used)
	}
	if snap.Prededuct != 0 {
		t.Errorf("prededuct = %d, 期望 0", snap.Prededuct)
	}
}

// 请求失败时释放预扣且不计入 used。
func TestRelease_FreesPredeductWithoutUsage(t *testing.T) {
	m, ctx := newTestManager(t)
	lim := Limits{Hard: 10_000, Soft: 8_000}

	_, lease, err := m.Acquire(ctx, "volc", "key_fail", KindToken, 2000, lim, time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := m.Release(ctx, lease); err != nil {
		t.Fatalf("Release: %v", err)
	}

	snap, _ := m.Get(ctx, "volc", "key_fail", KindToken)
	if snap.Prededuct != 0 || snap.Used != 0 {
		t.Errorf("释放后应全为 0, got used=%d prededuct=%d", snap.Used, snap.Prededuct)
	}
}

// 对账应能修复任何异常路径造成的 prededuct 漂移。
func TestReconcile_CorrectsDrift(t *testing.T) {
	m, ctx := newTestManager(t)
	lim := Limits{Hard: 10_000, Soft: 8_000}

	_, _, err := m.Acquire(ctx, "volc", "key_drift", KindToken, 1000, lim, time.Hour)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// 人为注入漂移，模拟 Lua 之外的异常写入
	day := QuotaDay(time.Now())
	m.rdb.HSet(ctx, quotaKey("volc", KindToken, "key_drift", day), "prededuct", 7777)

	drifts, err := m.ReconcileProvider(ctx, "volc", []string{"key_drift"})
	if err != nil {
		t.Fatalf("ReconcileProvider: %v", err)
	}
	if len(drifts) != 1 {
		t.Fatalf("期望 1 条修正, got %d: %+v", len(drifts), drifts)
	}
	if drifts[0].Amount != 7777-1000 {
		t.Errorf("检出漂移 %d, 期望 %d", drifts[0].Amount, 7777-1000)
	}
	if drifts[0].KeyID != "key_drift" || drifts[0].Kind != KindToken {
		t.Errorf("修正条目错位: %+v", drifts[0])
	}

	snap, _ := m.Get(ctx, "volc", "key_drift", KindToken)
	if snap.Prededuct != 1000 {
		t.Errorf("对账后 prededuct = %d, 期望以租约之和 1000 为准", snap.Prededuct)
	}
}

// 无漂移时对账不产生任何修正，也不动在途租约的预扣。
func TestReconcile_NoDriftNoChange(t *testing.T) {
	m, ctx := newTestManager(t)
	lim := Limits{Hard: 10_000, Soft: 8_000}

	_, _, err := m.Acquire(ctx, "volc", "key_clean", KindToken, 1500, lim, time.Hour)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	drifts, err := m.ReconcileProvider(ctx, "volc", []string{"key_clean"})
	if err != nil {
		t.Fatalf("ReconcileProvider: %v", err)
	}
	if len(drifts) != 0 {
		t.Fatalf("无漂移时不应有修正, got %+v", drifts)
	}

	snap, _ := m.Get(ctx, "volc", "key_clean", KindToken)
	if snap.Prededuct != 1500 {
		t.Errorf("在途租约的预扣不应被动过, got %d", snap.Prededuct)
	}
}

// 有泄漏但已无未过期租约的 Key 也必须被修正 —— 这是对账最核心的场景
// （预扣悬置、租约记录又丢了），只扫租约集合的实现修不到它。
func TestReconcile_OrphanPrededuct(t *testing.T) {
	m, ctx := newTestManager(t)

	// 直接向配额 Hash 注入孤儿 prededuct（无任何租约与之对应）
	day := QuotaDay(time.Now())
	qk := quotaKey("volc", KindToken, "key_orphan", day)
	m.rdb.HSet(ctx, qk, "used", 100, "prededuct", 4321)

	drifts, err := m.ReconcileProvider(ctx, "volc", []string{"key_orphan"})
	if err != nil {
		t.Fatalf("ReconcileProvider: %v", err)
	}
	if len(drifts) != 1 || drifts[0].Amount != 4321 {
		t.Fatalf("孤儿预扣应被全额检出, got %+v", drifts)
	}

	snap, _ := m.Get(ctx, "volc", "key_orphan", KindToken)
	if snap.Prededuct != 0 {
		t.Errorf("孤儿预扣应清零, got %d", snap.Prededuct)
	}
	if snap.Used != 100 {
		t.Errorf("used 不应被动, got %d", snap.Used)
	}
}

// 次数型配额（如 Seedream 100 次/日）走同一套机制。
func TestAcquire_CountKind(t *testing.T) {
	m, ctx := newTestManager(t)
	lim := Limits{Hard: 90, Soft: 80} // 100 次上限，留 10 次缓冲

	for i := 0; i < 90; i++ {
		_, lease, err := m.Acquire(ctx, "volc", "key_img", KindCount, 1, lim, time.Minute)
		if err != nil {
			t.Fatalf("第 %d 次预扣失败: %v", i+1, err)
		}
		if err := m.Commit(ctx, lease, 1); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	if _, _, err := m.Acquire(ctx, "volc", "key_img", KindCount, 1, lim, time.Minute); !errors.Is(err, ErrInsufficient) {
		t.Errorf("达到硬水位后必须拒绝, got %v", err)
	}

	snap, _ := m.Get(ctx, "volc", "key_img", KindCount)
	if snap.Used != 90 {
		t.Errorf("used = %d, 期望 90", snap.Used)
	}
}

// P0-4: 探测确认刷新后清零配额。
func TestMarkRefreshed_ResetsCounters(t *testing.T) {
	m, ctx := newTestManager(t)
	lim := Limits{Hard: 10_000, Soft: 8_000}

	_, lease, _ := m.Acquire(ctx, "volc", "key_rf", KindToken, 3000, lim, time.Minute)
	_ = m.Commit(ctx, lease, 3000)

	if err := m.MarkRefreshed(ctx, "volc", "key_rf", KindToken, lim); err != nil {
		t.Fatalf("MarkRefreshed: %v", err)
	}

	snap, _ := m.Get(ctx, "volc", "key_rf", KindToken)
	if snap.Used != 0 || snap.Prededuct != 0 {
		t.Errorf("刷新后应清零, got used=%d prededuct=%d", snap.Used, snap.Prededuct)
	}
	if snap.Hard != lim.Hard {
		t.Errorf("刷新后应重置水位, got %d", snap.Hard)
	}
}

// 批量快照读取供调度打分使用。
func TestGetMany(t *testing.T) {
	m, ctx := newTestManager(t)
	lim := Limits{Hard: 10_000, Soft: 8_000}

	ids := []string{"k1", "k2", "k3"}
	for i, id := range ids {
		_, lease, err := m.Acquire(ctx, "volc", id, KindToken, int64((i+1)*1000), lim, time.Minute)
		if err != nil {
			t.Fatalf("Acquire %s: %v", id, err)
		}
		_ = m.Commit(ctx, lease, int64((i+1)*1000))
	}

	snaps, err := m.GetMany(ctx, "volc", ids, KindToken)
	if err != nil {
		t.Fatalf("GetMany: %v", err)
	}
	if len(snaps) != 3 {
		t.Fatalf("返回 %d 条, 期望 3 条", len(snaps))
	}
	for i, id := range ids {
		if got := snaps[id].Used; got != int64((i+1)*1000) {
			t.Errorf("%s used = %d, 期望 %d", id, got, (i+1)*1000)
		}
	}
}

// 压测: 验证 20 QPS 目标下 Lua 预扣的延迟表现。
func BenchmarkAcquireCommit(b *testing.B) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: 14})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		b.Skip("无可用 Redis")
	}
	defer func() { rdb.FlushDB(ctx); rdb.Close() }()
	rdb.FlushDB(ctx)

	m, err := NewManager(ctx, rdb)
	if err != nil {
		b.Fatal(err)
	}
	lim := Limits{Hard: 1 << 62, Soft: 1 << 61}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, lease, err := m.Acquire(ctx, "volc", fmt.Sprintf("bench_%d", i%100), KindToken, 100, lim, time.Minute)
		if err != nil {
			b.Fatal(err)
		}
		if err := m.Commit(ctx, lease, 80); err != nil {
			b.Fatal(err)
		}
	}
}
