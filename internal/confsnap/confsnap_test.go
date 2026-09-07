package confsnap

import (
	"sync"
	"testing"

	"github.com/fluxkeys/fluxkeys/internal/config"
)

func baseCfg(mapping map[string]string) *config.Config {
	cfg := config.Default()
	cfg.Providers = map[string]config.Provider{
		"volc": {BaseURL: "https://volc.example", ModelMapping: mapping},
	}
	return cfg
}

// Build 失败绝不能触碰 Holder —— 这是本包的三条硬约束之一。
// 管理页面填错一个 provider 名就让整个网关退回无配置状态，是不可接受的。
func TestBuild_未知provider返回错误且不影响已有快照(t *testing.T) {
	good := baseCfg(map[string]string{"gpt-4o": "ep-旧"})
	h, err := NewHolderFromConfig(good, 1)
	if err != nil {
		t.Fatalf("初始快照: %v", err)
	}

	bad := config.Default()
	bad.Providers = map[string]config.Provider{"不存在的厂商": {BaseURL: "x"}}

	snap, err := Build(bad, 2)
	if err == nil {
		t.Fatalf("Build 应拒绝未登记的 provider，实际返回快照 %+v", snap)
	}
	if snap != nil {
		t.Errorf("Build 失败时应返回 nil 快照，实际 = %+v", snap)
	}

	// Holder 必须还是原样。
	if got := h.Version(); got != 1 {
		t.Errorf("Build 失败后 Holder 版本 = %d, 期望仍为 1", got)
	}
	if got := h.Cfg().Providers["volc"].ModelMapping["gpt-4o"]; got != "ep-旧" {
		t.Errorf("Build 失败后映射 = %q, 期望仍为 ep-旧", got)
	}
}

func TestBuild_配置与adapter同源(t *testing.T) {
	// 这是整个快照层存在的理由: 若只换 config 不重建 adapter，热切
	// model_mapping 后 BaseURL 是新的、adapter 里固化的映射还是旧的，
	// 请求会发到能连通的地址却带着错的模型名 —— 不报错，只发错。
	cfg := baseCfg(map[string]string{"gpt-4o": "ep-第一版"})
	snap, err := Build(cfg, 1)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ad, err := snap.Adapters.Get("volc")
	if err != nil {
		t.Fatalf("取 volc adapter: %v", err)
	}
	if ad.Provider() != "volc" {
		t.Errorf("adapter.Provider() = %q, 期望 volc", ad.Provider())
	}

	// 换配置后必须是一份全新的 adapter，且映射跟着变。
	next := baseCfg(map[string]string{"gpt-4o": "ep-第二版"})
	snap2, err := Build(next, 2)
	if err != nil {
		t.Fatalf("Build v2: %v", err)
	}
	ad2, err := snap2.Adapters.Get("volc")
	if err != nil {
		t.Fatalf("取 v2 volc adapter: %v", err)
	}
	if ad2 == ad {
		t.Error("两次 Build 复用了同一个 adapter 实例; " +
			"model_mapping 是在构造时固化的，复用会让热切对映射无效")
	}
	// 旧快照必须完全不受影响 —— 在途请求还持有它。
	if snap.Cfg.Providers["volc"].ModelMapping["gpt-4o"] != "ep-第一版" {
		t.Error("构建新快照污染了旧快照的配置")
	}
}

func TestHolder_并发读写无竞态(t *testing.T) {
	// 判据是 -race。生产里读侧有 148 处、写侧只有 1 处，全部安全性都压在
	// 这个原子指针上，config 包内部没有任何并发保护。
	h, err := NewHolderFromConfig(baseCfg(map[string]string{"gpt-4o": "ep-0"}), 0)
	if err != nil {
		t.Fatalf("初始快照: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := h.Current()
				// 深读到 map 内部: 只读顶层指针不会碰到被替换的 map，
				// 那样即使有问题也压不出来。
				_ = s.Cfg.Providers["volc"].ModelMapping["gpt-4o"]
				_, _ = s.Adapters.Get("volc")
			}
		}()
	}

	for v := 1; v <= 200; v++ {
		s, err := Build(baseCfg(map[string]string{"gpt-4o": "ep-新"}), int64(v))
		if err != nil {
			t.Fatalf("Build v%d: %v", v, err)
		}
		h.Store(s)
	}

	close(stop)
	wg.Wait()

	if got := h.Version(); got != 200 {
		t.Errorf("最终版本 = %d, 期望 200", got)
	}
}

// Store(nil) 必须被拒绝。热加载链路上任何一处返回 nil 快照，若直接存进去，
// 之后每个请求都会在 s.Cfg 上 nil panic —— 整个网关瞬间全挂。
func TestHolder_拒绝存入nil快照(t *testing.T) {
	h, err := NewHolderFromConfig(baseCfg(map[string]string{"gpt-4o": "ep-0"}), 7)
	if err != nil {
		t.Fatalf("初始快照: %v", err)
	}
	h.Store(nil)
	if h.Current() == nil {
		t.Fatal("Store(nil) 把 Holder 置空了; 后续所有请求都会 nil panic")
	}
	if got := h.Version(); got != 7 {
		t.Errorf("Store(nil) 后版本 = %d, 期望仍为 7", got)
	}
}
