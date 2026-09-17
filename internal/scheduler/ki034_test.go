package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/fluxkeys/fluxkeys/internal/store"
)

// 本文件的用例钉住 KI-034 的修复: 活跃池 s.keys 只在 Reload 时按「库状态 =
// active」重建，而周期 key_reload 有数分钟滞后。一个「重启前就被 ban」的
// Key 不在活跃池里，SetKeyStatus 只改 health、无法让它进入选择集。RefreshKey
// 是管理面在改完库后定向同步单个 Key 池归属的入口。
//
// 三个方向分别覆盖: 复活（active）拉回选择集、删除（ErrNotFound）剔出选择集、
// 非活跃（cooldown/banned/invalid）不动池归属。

func TestRefreshKey_复活重启前被禁的Key进入运行池(t *testing.T) {
	cfg := testCfg()
	cfg.EnablePersona = false // 关掉作息过滤，与「能否进选择集」的断言无关

	// 模拟「重启前就被 ban」: 库里该 Key 状态是 banned，Reload 只装 active。
	fs := &fakeStore{
		keys: []store.UpstreamKey{{
			KeyID: "volc_b1", Secret: "secret-volc_b1", Status: store.KeyStatusBanned,
			Pool: "hot", EgressIP: "172.16.0.11", HealthScore: 100,
		}},
		history: map[store.HistoryKey]store.KeyDailyHistory{},
	}
	s := New(testConf(cfg), fs, &fakeQuota{})
	s.SetClock(testTime)
	s.SetRandSource(42)
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if got := s.PoolSize(); got != 0 {
		t.Fatalf("banned Key 不应进入活跃池，PoolSize = %d", got)
	}
	if _, err := s.Select(context.Background(), Request{Now: testTime()}); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("活跃池为空应报 ErrNoCandidate，实际 %v", err)
	}

	// force 复活: 库状态先被管理面改成 active，随后调 RefreshKey 同步池归属。
	fs.mu.Lock()
	fs.keys[0].Status = store.KeyStatusActive
	fs.mu.Unlock()
	if err := s.RefreshKey(context.Background(), "volc_b1"); err != nil {
		t.Fatalf("RefreshKey: %v", err)
	}

	if got := s.PoolSize(); got != 1 {
		t.Fatalf("复活后应进入活跃池，PoolSize = %d", got)
	}
	c, err := s.Select(context.Background(), Request{Now: testTime()})
	if err != nil {
		t.Fatalf("复活后应可被选中: %v", err)
	}
	if c.KeyID != "volc_b1" {
		t.Fatalf("选中的应是复活的 Key，实际 %q", c.KeyID)
	}
	if c.Secret != "secret-volc_b1" || c.EgressIP != "172.16.0.11" || c.Pool != "hot" {
		t.Fatalf("复活后应带回库中元数据, got %+v", c)
	}
}

func TestRefreshKey_库中已删除的Key被剔出运行池(t *testing.T) {
	cfg := testCfg()
	cfg.EnablePersona = false

	fs := &fakeStore{
		keys: []store.UpstreamKey{{
			KeyID: "volc_d1", Secret: "secret-volc_d1", Status: store.KeyStatusActive,
			Pool: "hot", EgressIP: "172.16.0.12", HealthScore: 100,
		}},
		history: map[store.HistoryKey]store.KeyDailyHistory{},
	}
	s := New(testConf(cfg), fs, &fakeQuota{})
	s.SetClock(testTime)
	s.SetRandSource(42)
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := s.PoolSize(); got != 1 {
		t.Fatalf("PoolSize = %d, 期望 1", got)
	}

	// 模拟行被删（如 DELETE 端点 / SQL 直删）: 库里没了，调度器内存还留着。
	fs.mu.Lock()
	fs.keys = nil
	fs.mu.Unlock()
	if err := s.RefreshKey(context.Background(), "volc_d1"); err != nil {
		t.Fatalf("RefreshKey: %v", err)
	}

	if got := s.PoolSize(); got != 0 {
		t.Fatalf("库中已删的 Key 应被剔出活跃池，PoolSize = %d", got)
	}
	if _, err := s.Select(context.Background(), Request{Now: testTime()}); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("删除后不应再被选中，实际 %v", err)
	}
}

func TestRefreshKey_非活跃状态不进入运行池(t *testing.T) {
	cfg := testCfg()
	cfg.EnablePersona = false

	// cooldown 的 Key 本就不在活跃池（Reload 只装 active）。
	fs := &fakeStore{
		keys: []store.UpstreamKey{{
			KeyID: "volc_c1", Secret: "secret-volc_c1", Status: store.KeyStatusCooldown,
			Pool: "hot", EgressIP: "172.16.0.13", HealthScore: 100,
		}},
		history: map[store.HistoryKey]store.KeyDailyHistory{},
	}
	s := New(testConf(cfg), fs, &fakeQuota{})
	s.SetClock(testTime)
	s.SetRandSource(42)
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := s.PoolSize(); got != 0 {
		t.Fatalf("cooldown Key 不应进入活跃池，PoolSize = %d", got)
	}

	// RefreshKey 对非 active 不动池归属: 封禁/冷却方向由 health.available 把关，
	// 与「PATCH 封禁后 Key 仍留在池里」的既有行为一致。
	if err := s.RefreshKey(context.Background(), "volc_c1"); err != nil {
		t.Fatalf("RefreshKey: %v", err)
	}
	if got := s.PoolSize(); got != 0 {
		t.Fatalf("非 active 的 Key 不应被 RefreshKey 拉进活跃池，PoolSize = %d", got)
	}
}
