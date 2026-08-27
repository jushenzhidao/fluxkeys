package quota

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testRefresherConfig() RefresherConfig {
	return RefresherConfig{
		WindowStart:   12 * time.Hour,
		WindowEnd:     14 * time.Hour,
		ProbeInterval: 5 * time.Minute,
		TokenLimits:   Limits{Hard: 4_500_000, Soft: 4_000_000},
		CountLimits:   Limits{Hard: 90, Soft: 80},
		RampDuration:  30 * time.Minute,
	}
}

func at(h, m int) time.Time {
	loc := time.FixedZone("CST", 8*3600)
	return time.Date(2026, 8, 23, h, m, 0, 0, loc)
}

func TestInWindow(t *testing.T) {
	r := NewRefresher(testRefresherConfig(), nil, nil, nil, quietLogger())

	cases := []struct {
		at   time.Time
		want bool
	}{
		{at(11, 59), false},
		{at(12, 0), true},
		{at(13, 30), true},
		{at(13, 59), true},
		{at(14, 0), false},
		{at(20, 0), false},
	}
	for _, c := range cases {
		if got := r.InWindow(c.at); got != c.want {
			t.Errorf("InWindow(%s) = %v, want %v", c.at.Format("15:04"), got, c.want)
		}
	}
}

// P0-4 核心: 探测未确认刷新时，Key 必须保持不可调度，绝不能因为
// 「时间到了」就恢复流量 —— 那会在火山尚未重置额度时直接超刷。
func TestTick_DoesNotResumeBeforeProbeConfirms(t *testing.T) {
	m, ctx := newTestManager(t)

	var probeCalls int
	var mu sync.Mutex
	probe := func(ctx context.Context, keyID string) (bool, error) {
		mu.Lock()
		probeCalls++
		mu.Unlock()
		return false, nil // 火山侧尚未刷新
	}
	list := func(ctx context.Context) ([]string, error) { return []string{"key_1"}, nil }

	r := NewRefresher(testRefresherConfig(), m, probe, list, quietLogger())

	// 先制造已消耗的配额
	lim := Limits{Hard: 10_000, Soft: 8_000}
	_, lease, _ := m.Acquire(ctx, "key_1", KindToken, 5_000, lim, time.Minute)
	_ = m.Commit(ctx, lease, 5_000)

	// 推进到窗口末尾附近，确保已越过错峰偏移
	r.SetClock(func() time.Time { return at(13, 30) })
	if err := r.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	mu.Lock()
	calls := probeCalls
	mu.Unlock()
	if calls == 0 {
		t.Fatal("应已发起探测")
	}

	if r.State("key_1") != RefreshProbing {
		t.Errorf("未确认刷新时状态应为 probing, got %s", r.State("key_1"))
	}
	if r.Schedulable("key_1") {
		t.Error("未确认刷新的 Key 绝不能被调度 —— 这正是 P0-4 要防的超刷")
	}

	// 关键断言: 配额不得被清零
	snap, _ := m.Get(ctx, "key_1", KindToken)
	if snap.Used != 5_000 {
		t.Errorf("未确认刷新前不得清零配额, used = %d, 期望 5000", snap.Used)
	}
}

// 探测确认后才清零配额并恢复调度。
func TestTick_ConfirmsAndResetsAfterSuccessfulProbe(t *testing.T) {
	m, ctx := newTestManager(t)

	probe := func(ctx context.Context, keyID string) (bool, error) { return true, nil }
	list := func(ctx context.Context) ([]string, error) { return []string{"key_1"}, nil }

	r := NewRefresher(testRefresherConfig(), m, probe, list, quietLogger())

	lim := Limits{Hard: 10_000, Soft: 8_000}
	_, lease, _ := m.Acquire(ctx, "key_1", KindToken, 5_000, lim, time.Minute)
	_ = m.Commit(ctx, lease, 5_000)

	r.SetClock(func() time.Time { return at(13, 30) })
	m.SetClock(func() time.Time { return at(13, 30) })

	if err := r.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if r.State("key_1") != RefreshConfirmed {
		t.Fatalf("探测成功后应为 confirmed, got %s", r.State("key_1"))
	}
	if !r.Schedulable("key_1") {
		t.Error("确认刷新后应可调度")
	}

	snap, _ := m.Get(ctx, "key_1", KindToken)
	if snap.Used != 0 || snap.Prededuct != 0 {
		t.Errorf("确认后应清零, got used=%d prededuct=%d", snap.Used, snap.Prededuct)
	}
	if snap.Hard != 4_500_000 {
		t.Errorf("应重置为配置的水位, got %d", snap.Hard)
	}
}

// 确认刷新后应进入限速期（起床缓冲），避免整齐的流量尖峰。
func TestRateLimitFactor_RampsAfterRefresh(t *testing.T) {
	m, ctx := newTestManager(t)

	probe := func(ctx context.Context, keyID string) (bool, error) { return true, nil }
	list := func(ctx context.Context) ([]string, error) { return []string{"key_1"}, nil }
	r := NewRefresher(testRefresherConfig(), m, probe, list, quietLogger())

	now := at(13, 0)
	r.SetClock(func() time.Time { return now })
	m.SetClock(func() time.Time { return now })
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	if f := r.RateLimitFactor("key_1"); f != 0.5 {
		t.Errorf("刚刷新应限速 50%%, got %v", f)
	}

	// 越过限速期
	r.SetClock(func() time.Time { return at(13, 45) })
	if f := r.RateLimitFactor("key_1"); f != 1.0 {
		t.Errorf("限速期结束应恢复, got %v", f)
	}
}

// 窗口结束仍未确认的 Key 必须标记失败并告警 —— 这是超刷风险信号。
func TestTick_MarksFailedWhenWindowEnds(t *testing.T) {
	m, ctx := newTestManager(t)

	probe := func(ctx context.Context, keyID string) (bool, error) { return false, nil }
	list := func(ctx context.Context) ([]string, error) { return []string{"key_stuck"}, nil }
	r := NewRefresher(testRefresherConfig(), m, probe, list, quietLogger())

	r.SetClock(func() time.Time { return at(13, 59) })
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	if got := r.State("key_stuck"); got != RefreshFailed {
		t.Errorf("窗口结束未确认应为 failed, got %s", got)
	}
	if r.Schedulable("key_stuck") {
		t.Error("failed 状态不应被调度")
	}
}

// 探测失败（网络错误）不应误判为已刷新。
func TestTick_ProbeErrorDoesNotConfirm(t *testing.T) {
	m, ctx := newTestManager(t)

	probe := func(ctx context.Context, keyID string) (bool, error) {
		return false, errors.New("connection reset")
	}
	list := func(ctx context.Context) ([]string, error) { return []string{"key_1"}, nil }
	r := NewRefresher(testRefresherConfig(), m, probe, list, quietLogger())

	lim := Limits{Hard: 10_000, Soft: 8_000}
	_, lease, _ := m.Acquire(ctx, "key_1", KindToken, 3_000, lim, time.Minute)
	_ = m.Commit(ctx, lease, 3_000)

	r.SetClock(func() time.Time { return at(13, 0) })
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	if r.State("key_1") == RefreshConfirmed {
		t.Error("探测报错不应确认刷新")
	}
	snap, _ := m.Get(ctx, "key_1", KindToken)
	if snap.Used != 3_000 {
		t.Errorf("探测失败不应改动配额, used = %d", snap.Used)
	}
}

// 窗口外不做任何探测。
func TestTick_NoOpOutsideWindow(t *testing.T) {
	m, ctx := newTestManager(t)

	var called bool
	probe := func(ctx context.Context, keyID string) (bool, error) {
		called = true
		return true, nil
	}
	list := func(ctx context.Context) ([]string, error) { return []string{"key_1"}, nil }
	r := NewRefresher(testRefresherConfig(), m, probe, list, quietLogger())

	r.SetClock(func() time.Time { return at(20, 0) })
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("窗口外不应发起探测")
	}
	if !r.Schedulable("key_1") {
		t.Error("窗口外 Key 应可正常调度")
	}
}

// 错峰偏移必须确定且分散，避免所有 Key 在 12:00 同时打探测请求。
func TestStaggerOffset_DeterministicAndSpread(t *testing.T) {
	window := 2 * time.Hour
	day := "20260823"

	// 确定性
	a := StaggerOffset("key_1", day, window)
	b := StaggerOffset("key_1", day, window)
	if a != b {
		t.Errorf("同一 Key 同一天偏移应稳定: %s vs %s", a, b)
	}

	// 落在窗口前半段内
	if a < 0 || a >= window/2 {
		t.Errorf("偏移应落在窗口前半段, got %s", a)
	}

	// 分散性: 100 个 Key 不应挤在同一分钟
	buckets := make(map[int]int)
	for i := 0; i < 100; i++ {
		off := StaggerOffset(string(rune('a'+i%26))+string(rune('0'+i/26)), day, window)
		buckets[int(off.Minutes())/10]++
	}
	if len(buckets) < 4 {
		t.Errorf("偏移过于集中，仅落在 %d 个十分钟桶内", len(buckets))
	}
}

func TestPreRefreshActive(t *testing.T) {
	r := NewRefresher(testRefresherConfig(), nil, nil, nil, quietLogger())
	lead := 30 * time.Minute

	if !r.PreRefreshActive(at(11, 40), lead) {
		t.Error("11:40 应处于刷新前低功耗期")
	}
	if r.PreRefreshActive(at(11, 0), lead) {
		t.Error("11:00 尚未进入低功耗期")
	}
	if r.PreRefreshActive(at(12, 30), lead) {
		t.Error("已进入窗口则不再是低功耗期")
	}
}

func TestStats(t *testing.T) {
	m, ctx := newTestManager(t)

	confirmed := map[string]bool{"key_ok": true}
	probe := func(ctx context.Context, keyID string) (bool, error) {
		return confirmed[keyID], nil
	}
	list := func(ctx context.Context) ([]string, error) {
		return []string{"key_ok", "key_slow"}, nil
	}
	r := NewRefresher(testRefresherConfig(), m, probe, list, quietLogger())

	now := at(13, 30)
	r.SetClock(func() time.Time { return now })
	m.SetClock(func() time.Time { return now })
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	stats := r.Stats()
	if stats[RefreshConfirmed] != 1 {
		t.Errorf("confirmed 数 = %d, 期望 1", stats[RefreshConfirmed])
	}
	if stats[RefreshProbing] != 1 {
		t.Errorf("probing 数 = %d, 期望 1", stats[RefreshProbing])
	}
}
