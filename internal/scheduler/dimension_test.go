package scheduler

import (
	"context"
	"testing"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/store"
)

// 本文件专门守卫配额量纲的正确性: 按 token 计费的 provider 取 token 水位，
// 按次计费的取 count 水位。
//
// 这是上一阶段那个缺陷的同源残留 —— 当时修了归档与打分的 ratio 口径，漏了
// Key 池装载这一层。直到热加载改造读 scheduler.go 才发现 Reload 里仍是一律
// 调 TokenHard/TokenSoft，完全不看 provider 的 quota_kind。
//
// 失效模式: 按次计费的 Key 拿到「TokenLimit × TokenHardRatio」这个数，
// 它混合了两个量纲因而无语义，但它是正数且远大于真实 CountLimit，于是硬水位
// 过滤对按次 provider 形同失效 —— 超额只能等 Acquire 的 Lua 兜底，调度打分的
// Ratio 也一路失真。不报错、不超时，只表现为「配了 count_limit 但完全没用」。

func TestScheduler_按次provider的Key水位取count量纲(t *testing.T) {
	cfg := config.Default()
	// 刻意让两个量纲的绝对值天差地别: 500 万 token vs 1200 次。
	// 量纲一旦搞错，数量级差异会让断言无从辩解。
	cfg.Quota.TokenLimit = 5_000_000
	cfg.Quota.TokenHardRatio = 0.95
	cfg.Quota.TokenSoftRatio = 0.80
	cfg.Quota.CountLimit = 1200
	cfg.Quota.CountHardRatio = 0.90
	cfg.Quota.CountSoftRatio = 0.70
	cfg.Providers = map[string]config.Provider{
		"provider-token": {QuotaKind: "token"},
		"provider-count": {QuotaKind: "count"},
	}

	fs := &fakeStore{
		history: map[store.HistoryKey]store.KeyDailyHistory{},
		keys: []store.UpstreamKey{
			{
				KeyID: "key-token-1", Provider: "provider-token",
				Secret: "secret-token", Status: store.KeyStatusActive,
				Pool: "hot", HealthScore: 100,
			},
			{
				KeyID: "key-count-1", Provider: "provider-count",
				Secret: "secret-count", Status: store.KeyStatusActive,
				Pool: "hot", HealthScore: 100,
			},
		},
	}

	s := New(StaticConfig{Cfg: cfg}, fs, &fakeQuota{})
	s.SetClock(testTime)
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	byID := map[string]keyEntry{}
	s.mu.Lock()
	for _, e := range s.keys {
		byID[e.keyID] = e
	}
	s.mu.Unlock()

	tokenKey, ok := byID["key-token-1"]
	if !ok {
		t.Fatalf("Key 池里没有 key-token-1，实际装载 %d 个 Key", len(byID))
	}
	countKey, ok := byID["key-count-1"]
	if !ok {
		t.Fatalf("Key 池里没有 key-count-1，实际装载 %d 个 Key", len(byID))
	}

	// 按 token 计费: 水位取 TokenLimit × token 比例。
	wantTokenHard, wantTokenSoft := int64(4_750_000), int64(4_000_000)
	if tokenKey.hardLimit != wantTokenHard || tokenKey.softLimit != wantTokenSoft {
		t.Errorf("provider-token 的 Key 水位 = (硬 %d, 软 %d), 期望 (%d, %d)",
			tokenKey.hardLimit, tokenKey.softLimit, wantTokenHard, wantTokenSoft)
	}

	// 按次计费: 水位取 CountLimit × count 比例。
	wantCountHard, wantCountSoft := int64(1080), int64(840)
	if countKey.hardLimit != wantCountHard || countKey.softLimit != wantCountSoft {
		t.Errorf("provider-count 的 Key 水位 = (硬 %d, 软 %d), 期望 (%d, %d); "+
			"按次计费的 provider 拿到了 token 量纲的水位",
			countKey.hardLimit, countKey.softLimit, wantCountHard, wantCountSoft)
	}

	// 数量级守卫: 旧逻辑下按次 Key 会拿到 4_750_000。单独断这一条是因为它
	// 直指缺陷本身 —— 即便将来 ratio 配置调整、上面的精确值随之变化，
	// 「按次额度只有 1200 却给出百万级水位」这个判据依然成立。
	if countKey.hardLimit > cfg.Quota.CountLimit {
		t.Errorf("provider-count 的硬水位 = %d, 超过其 CountLimit %d; "+
			"这是旧缺陷的典型表现 —— 用 TokenLimit 的绝对值替代了 CountLimit",
			countKey.hardLimit, cfg.Quota.CountLimit)
	}
}

// TestScheduler_热切配置后调度读到新水位 守卫 config.Scheduler 值拷贝问题。
//
// 改造前 Scheduler 持的是 config.Scheduler 值拷贝，热切后它手里仍是启动时的
// 副本。这个 bug 的表现最不能接受: 管理页面显示「配置已生效」，而调度实际
// 完全没变 —— 操作成功但状态与用户心智模型不符。
func TestScheduler_热切配置后调度读到新水位(t *testing.T) {
	cfg := config.Default()
	cfg.Quota.TokenLimit = 1_000_000
	cfg.Quota.TokenHardRatio = 0.90
	cfg.Providers = map[string]config.Provider{
		"volc": {QuotaKind: "token"},
	}

	// 可变配置源: 模拟快照替换。生产里是 confsnap.SchedConfig 走
	// atomic.Pointer，这里用最小实现，只为验证 Scheduler 每次读都回到
	// 配置源、而非缓存启动时的值拷贝。
	src := &mutableConf{cfg: cfg}

	fs := &fakeStore{
		history: map[store.HistoryKey]store.KeyDailyHistory{},
		keys: []store.UpstreamKey{{
			KeyID: "volc-1", Provider: "volc", Secret: "s1",
			Status: store.KeyStatusActive, Pool: "hot", HealthScore: 100,
		}},
	}

	s := New(src, fs, &fakeQuota{})
	s.SetClock(testTime)
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("首次 Reload: %v", err)
	}

	s.mu.Lock()
	before := s.keys[0].hardLimit
	s.mu.Unlock()
	if before != 900_000 {
		t.Fatalf("热切前硬水位 = %d, 期望 900000", before)
	}

	// 热切: 额度上限翻倍。
	next := config.Default()
	next.Quota = cfg.Quota
	next.Quota.TokenLimit = 2_000_000
	next.Providers = cfg.Providers
	src.store(next)

	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("热切后 Reload: %v", err)
	}

	s.mu.Lock()
	after := s.keys[0].hardLimit
	s.mu.Unlock()
	if after != 1_800_000 {
		t.Errorf("热切后硬水位 = %d, 期望 1800000; 调度器仍在用启动时的配置副本",
			after)
	}
}

// mutableConf 是可替换的配置源，仅供测试模拟热切。
type mutableConf struct{ cfg *config.Config }

func (m *mutableConf) store(c *config.Config)      { m.cfg = c }
func (m *mutableConf) Scheduler() config.Scheduler { return m.cfg.Scheduler }
func (m *mutableConf) Quota() config.Quota         { return m.cfg.Quota }
func (m *mutableConf) LimitsFor(provider string, kindCount bool) (int64, int64) {
	return m.cfg.LimitsFor(provider, kindCount)
}
func (m *mutableConf) IsCountProvider(provider string) bool {
	return m.cfg.IsCountProvider(provider)
}
