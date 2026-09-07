package gateway

import (
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fluxkeys/fluxkeys/internal/config"
)

// 本文件专门守卫配置热加载的两条正确性不变量:
//
//  1. 请求内一致性 —— 一个请求从入口到最后一次重试，全程只认入口那一份
//     快照。跨 attempt 混用新旧配置会产出「新 base_url + 旧 model_mapping」
//     这类混合态: 不报错、不超时，只是安静地把请求发到错误的模型上。
//  2. 并发安全 —— config 包内部零并发保护（无 mutex、无 atomic），读侧的
//     全部安全性都压在快照指针的原子替换上。这里用 -race 压出实证。
//
// 这两条都属于「测不出就等于没有」的性质: 线上表现是账单和结果不对，
// 而非报错，靠观察日志发现不了。

// ===== 请求内一致性 =====

func TestHotReload_单请求跨重试只用一份快照(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) { c.Upstream.MaxRetries = 2 })

	// 三次尝试: 前两次失败逼出重试，第三次成功。
	env.upstream.setScript(
		stubResponse{Status: 500, Body: `{"error":{"code":"InternalError","message":"boom"}}`},
		stubResponse{Status: 500, Body: `{"error":{"code":"InternalError","message":"boom"}}`},
		stubResponse{Status: 200, Body: okChatResp},
	)

	// 第一次 attempt 一进上游就热切 model_mapping。这是最刁的时机: 此刻
	// 请求已经开始，但后面还有两次 attempt。若 execute 在循环内重取快照，
	// 第二、三次就会用上新映射，同一个请求发出两个不同的模型名。
	env.upstream.setOnRequest(func(n int64) {
		if n != 1 {
			return
		}
		env.hotSwap(t, 2, func(c *config.Config) {
			p := c.Providers["volc"]
			p.ModelMapping = map[string]string{"gpt-4o": "ep-已热切-新端点"}
			c.Providers["volc"] = p
		})
	})

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200, 响应: %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()

	models := env.upstream.models()
	if len(models) != 3 {
		t.Fatalf("上游收到 %d 次请求（模型名 %v）, 期望 3 次", len(models), models)
	}
	// 断言的是「三次一致」而非「三次都等于旧值」: 前者才是不变量本身。
	// 只断言旧值的话，将来若入口取快照的时机变了，用例会给出误导性的失败。
	for i, m := range models {
		if m != models[0] {
			t.Errorf("第 %d 次 attempt 的模型名 = %q, 第 1 次 = %q; "+
				"同一请求跨重试用了不同配置", i+1, m, models[0])
		}
	}
	if models[0] != "ep-test-4o" {
		t.Errorf("模型名 = %q, 期望入口快照的 ep-test-4o", models[0])
	}
}

func TestHotReload_热切后的新请求用新快照(t *testing.T) {
	// 与上一个用例互为对照: 一致性不能靠「永不刷新」来实现。若把快照做成
	// 启动时取一次就再也不更新，上面那个用例同样会通过，但热加载整个失去
	// 意义。这里证明新请求确实看得到新配置。
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{Status: 200, Body: okChatResp})

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	resp.Body.Close()

	env.hotSwap(t, 2, func(c *config.Config) {
		p := c.Providers["volc"]
		p.ModelMapping = map[string]string{"gpt-4o": "ep-新端点"}
		c.Providers["volc"] = p
	})

	resp = env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200, 响应: %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()

	models := env.upstream.models()
	if len(models) != 2 {
		t.Fatalf("上游收到 %d 次请求, 期望 2", len(models))
	}
	if models[0] != "ep-test-4o" {
		t.Errorf("热切前的模型名 = %q, 期望 ep-test-4o", models[0])
	}
	if models[1] != "ep-新端点" {
		t.Errorf("热切后的模型名 = %q, 期望 ep-新端点; 快照没有真正生效", models[1])
	}
}

// ===== 并发安全 =====

func TestHotReload_并发请求下反复热切无竞态(t *testing.T) {
	// 本用例的判据是 -race 是否干净，而非某个断言。config 包没有任何并发
	// 保护，148 处读取点对 1 处写入点，只要哪里漏走快照、直接读了被替换的
	// 那份 config，race detector 就会在这里抓到。
	env := newTestEnv(t, func(c *config.Config) { c.Upstream.MaxRetries = 1 })
	env.upstream.setScript(stubResponse{Status: 200, Body: okChatResp})

	const (
		readers = 8
		rounds  = 12
	)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var bad atomic.Int64
	var done atomic.Int64

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
				// 热切期间也不允许出 5xx: 换配置是运维常规操作，
				// 让它把正在处理的请求打挂就等于不能在线上用。
				if resp.StatusCode != http.StatusOK {
					bad.Add(1)
				}
				resp.Body.Close()
				done.Add(1)
			}
		}()
	}

	// 写侧: 每轮都换一份全新的 Providers map 与 mapping，让读侧只要
	// 持有跨轮的陈旧引用就会被 race detector 逮到。
	//
	// 关键是每轮必须真的落在在途请求上。hotSwap 只是换一个原子指针，
	// 12 轮跑完比一次 HTTP 往返还快 —— 初版这里不等读侧，实测热切窗口
	// 内上游请求数为 0，读写零重叠，-race 干净纯属没测到东西。所以每轮
	// 都卡在「至少又完成一个请求」上，让写入夹在读取之间发生。
	for r := 0; r < rounds; r++ {
		base := done.Load()
		for done.Load() < base+1 {
			runtime.Gosched()
		}
		env.hotSwap(t, int64(r+2), func(c *config.Config) {
			p := c.Providers["volc"]
			p.ModelMapping = map[string]string{"gpt-4o": "ep-第" + strconv.Itoa(r) + "轮"}
			c.Providers["volc"] = p
		})
	}
	overlapped := done.Load()

	close(stop)
	wg.Wait()

	if n := bad.Load(); n != 0 {
		t.Errorf("热切期间有 %d 个请求非 200; 配置热切不应影响在途请求", n)
	}
	// 自我校验: 断言这一轮确实有并发压力穿过热切窗口。少了这条，
	// 上面的 -race 干净可能只是因为压根没并发过。
	if overlapped < rounds {
		t.Errorf("热切窗口内只完成了 %d 个请求, 少于 %d 轮热切; "+
			"读写没有真正重叠, 本用例没有验证到并发安全", overlapped, rounds)
	}
}
