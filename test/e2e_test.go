package test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/mockark"
	"github.com/fluxkeys/fluxkeys/internal/quota"
)

const itChat = `{"model":"gpt-4o","max_tokens":100,` +
	`"messages":[{"role":"user","content":"请简单介绍一下你自己"}]}`

const itStream = `{"model":"gpt-4o","stream":true,"max_tokens":100,` +
	`"messages":[{"role":"user","content":"请简单介绍一下你自己"}]}`

// ===== 正常链路 =====

func Test集成_非流式请求打通全链路(t *testing.T) {
	env := newItEnv(t, []string{"k1", "k2", "k3"})

	resp, body := env.chat(t, itChat)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", resp.StatusCode, body)
	}

	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应非合法 JSON: %v\n%s", err, body)
	}

	// 上游端点 ID 必须被映射回对外模型名
	if out.Model != "gpt-4o" {
		t.Errorf("model = %q, 期望 gpt-4o（上游返回的是 ep-itest-chat）", out.Model)
	}
	if len(out.Choices) == 0 || out.Choices[0].Message.Content == "" {
		t.Errorf("响应缺少内容: %s", body)
	}
	if out.Usage.TotalTokens <= 0 {
		t.Errorf("响应缺少 usage: %s", body)
	}

	// mockark 侧独立记账，与网关的记录应当一致
	acct := env.ark.Account("k1")
	if acct.Requests != 1 {
		t.Errorf("mockark 记录 k1 请求数 = %d, 期望 1", acct.Requests)
	}
	if acct.TokenUsed != out.Usage.TotalTokens {
		t.Errorf("mockark 记账 %d 与响应 usage %d 不一致",
			acct.TokenUsed, out.Usage.TotalTokens)
	}

	// 网关按上游的真实 usage 修正配额，而非停留在预扣量
	if got := env.quota.usedOf("k1", quota.KindToken); got != out.Usage.TotalTokens {
		t.Errorf("配额已确认用量 = %d, 期望等于上游 usage %d", got, out.Usage.TotalTokens)
	}
	env.quota.assertClean(t)

	// 上游侧看到的请求 ID 应与网关返回的一致，链路可追
	logs := env.arkLogs()
	if len(logs) != 1 {
		t.Fatalf("mockark 日志条数 = %d", len(logs))
	}
	if logs[0].RequestID == "" {
		t.Error("网关未把请求 ID 传播到上游，跨系统排障将无法关联")
	}
	if logs[0].RequestID != resp.Header.Get("X-Request-Id") {
		t.Errorf("上游看到的请求 ID %q 与返回给客户端的 %q 不一致",
			logs[0].RequestID, resp.Header.Get("X-Request-Id"))
	}
}

func Test集成_流式请求真流式且用量修正正确(t *testing.T) {
	env := newItEnv(t, []string{"k1"}, func(c *config.Config, o *mockark.Options) {
		o.StreamChunks = 8
		o.StreamChunkDelay = 25 * time.Millisecond
	})

	req, _ := http.NewRequest(http.MethodPost, env.gw.URL+"/v1/chat/completions",
		strings.NewReader(itStream))
	req.Header.Set("Authorization", "Bearer itest-key")

	start := time.Now()
	resp, err := env.gw.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d: %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}

	var (
		firstAt   time.Duration
		dataLines []string
		usage     struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		}
		gotUsage bool
		models   = map[string]bool{}
	)

	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "data:") {
			if firstAt == 0 {
				firstAt = time.Since(start)
			}
			payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
			dataLines = append(dataLines, payload)

			if payload != "[DONE]" {
				var chunk struct {
					Model string `json:"model"`
					Usage *struct {
						PromptTokens     int64 `json:"prompt_tokens"`
						CompletionTokens int64 `json:"completion_tokens"`
						TotalTokens      int64 `json:"total_tokens"`
					} `json:"usage"`
				}
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					if chunk.Model != "" {
						models[chunk.Model] = true
					}
					if chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
						usage.PromptTokens = chunk.Usage.PromptTokens
						usage.CompletionTokens = chunk.Usage.CompletionTokens
						usage.TotalTokens = chunk.Usage.TotalTokens
						gotUsage = true
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	total := time.Since(start)

	if len(dataLines) < 3 {
		t.Fatalf("收到 %d 个 data 行，过少: %v", len(dataLines), dataLines)
	}
	if dataLines[len(dataLines)-1] != "[DONE]" {
		t.Errorf("末行 = %q, 期望 [DONE]", dataLines[len(dataLines)-1])
	}

	// 8 个 chunk × 25ms ≈ 200ms。首 chunk 必须远早于此。
	if total < 150*time.Millisecond {
		t.Fatalf("总耗时 %v 过短，mockark 未按预期分块，本用例判定失效", total)
	}
	if firstAt > total/2 {
		t.Errorf("首 chunk 耗时 %v，总耗时 %v —— 疑似缓冲了整个响应", firstAt, total)
	}

	// 流式 chunk 里的模型名也必须被映射
	if models["ep-itest-chat"] {
		t.Error("流式 chunk 泄漏了上游端点 ID")
	}
	if !models["gpt-4o"] {
		t.Errorf("流式 chunk 未映射回对外模型名: %v", models)
	}

	// 网关必须自动补 stream_options.include_usage，否则上游不会发 usage chunk。
	// 这是流式配额修正的前提。
	if !gotUsage {
		t.Fatal("流式响应未包含 usage chunk —— 网关未补齐 include_usage")
	}
	if got := env.quota.usedOf("k1", quota.KindToken); got != usage.TotalTokens {
		t.Errorf("配额确认用量 = %d, 期望等于流式 usage %d", got, usage.TotalTokens)
	}
	env.quota.assertClean(t)

	recs := env.store.records()
	if len(recs) != 1 || !recs[0].IsStream {
		t.Fatalf("流水未标记为流式: %+v", recs)
	}
	if recs[0].TotalTokens != usage.TotalTokens {
		t.Errorf("流水 TotalTokens = %d, 期望 %d", recs[0].TotalTokens, usage.TotalTokens)
	}
}

// ===== 配额耗尽换 Key =====

func Test集成_上游额度耗尽后换Key并停止选用(t *testing.T) {
	env := newItEnv(t, []string{"k1", "k2", "k3"})

	// 让 mockark 对 k1 返回火山真实形态的额度耗尽错误（429 + 中文文案）。
	//
	// 用故障注入而非把 TokenLimit 调低: mockark 的额度判定是
	// TokenUsed >= TokenLimit，而 RegisterKey 会把 TokenUsed 重置为 0，
	// 所以「限额调到很小」并不能让首次请求就耗尽。
	env.ark.Inject(mockark.Fault{Kind: mockark.Fault429Quota, KeyID: "k1", Remaining: -1})

	resp, body := env.chat(t, itChat)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望换 Key 后成功: %s", resp.StatusCode, body)
	}

	// k1 必须因额度耗尽被移出可选集合，否则后续请求会反复撞上它
	if fs := env.sched.failuresOf("k1"); len(fs) == 0 {
		t.Fatal("k1 额度耗尽未上报给调度器")
	}
	disabled := env.sched.disabledKeys()
	found := false
	for _, d := range disabled {
		if d == "k1" {
			found = true
		}
	}
	if !found {
		t.Errorf("k1 未被停用，调度器会反复选中已耗尽的 Key: %v", disabled)
	}

	// 上游侧应看到两次请求，且用了不同的 Key
	logs := env.arkLogs()
	if len(logs) < 2 {
		t.Fatalf("mockark 日志 %d 条, 期望至少 2（失败 + 重试）", len(logs))
	}
	if logs[0].KeyID == logs[len(logs)-1].KeyID {
		t.Errorf("重试使用了同一个 Key: %s", logs[0].KeyID)
	}

	// 额度耗尽的那次不能计入本地用量
	if used := env.quota.usedOf("k1", quota.KindToken); used != 0 {
		t.Errorf("额度耗尽的 k1 记入了 %d 用量, 期望 0", used)
	}
	env.quota.assertClean(t)

	// 后续请求不应再触达 k1
	before := len(env.arkLogs())
	env.chat(t, itChat)
	for _, l := range env.arkLogs()[before:] {
		if l.KeyID == "k1" {
			t.Error("已停用的 k1 仍被选中")
		}
	}
}

func Test集成_本地配额耗尽时换Key(t *testing.T) {
	// 上游还有额度，但本地水位已满 —— 这是正常的调度收敛，不是错误
	env := newItEnv(t, []string{"k1", "k2"})

	// 硬水位压到只够一个请求的预扣量
	env.quota.mu.Lock()
	env.quota.hardOverride = 400
	env.quota.mu.Unlock()

	var okCount int
	for i := 0; i < 4; i++ {
		resp, _ := env.chat(t, itChat)
		if resp.StatusCode == http.StatusOK {
			okCount++
		}
	}
	if okCount == 0 {
		t.Fatal("全部请求失败，测试环境有问题")
	}

	// 两个 Key 都应被用到 —— 单个 Key 满了要能换到另一个
	seen := map[string]bool{}
	for _, l := range env.arkLogs() {
		seen[l.KeyID] = true
	}
	if len(seen) < 2 {
		t.Errorf("只用到了 %v，本地配额满后未换 Key", seen)
	}
	env.quota.assertClean(t)
}

// ===== 429 重试 =====

func Test集成_429限流后重试成功(t *testing.T) {
	env := newItEnv(t, []string{"k1", "k2", "k3"})

	// 注入两次纯限流 429（非额度耗尽）
	env.ark.Inject(mockark.Fault{Kind: mockark.Fault429RateLimit, Remaining: 2})

	resp, body := env.chat(t, itChat)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望重试后成功: %s", resp.StatusCode, body)
	}

	logs := env.arkLogs()
	if len(logs) != 3 {
		t.Fatalf("mockark 日志 %d 条, 期望 3（2 次 429 + 1 次成功）", len(logs))
	}
	// 限流必须映射为 rate_limit 而非 quota: 前者冷却几秒即恢复，
	// 后者要等到次日刷新。混淆会让 Key 池被过早判死。
	for _, id := range []string{logs[0].KeyID, logs[1].KeyID} {
		fs := env.sched.failuresOf(id)
		if len(fs) == 0 {
			t.Errorf("%s 的 429 未上报", id)
			continue
		}
		if fs[0].String() != "rate_limit" {
			t.Errorf("%s 的失败分类 = %s, 期望 rate_limit", id, fs[0].String())
		}
	}
	// 纯限流不应导致 Key 被停用
	if d := env.sched.disabledKeys(); len(d) != 0 {
		t.Errorf("纯速率限流导致 Key 被停用: %v", d)
	}
	env.quota.assertClean(t)

	recs := env.store.records()
	if len(recs) != 1 || recs[0].RetryCount != 2 {
		t.Errorf("流水 RetryCount = %v, 期望 2", recs)
	}
}

func Test集成_429耗尽重试上限后返回503(t *testing.T) {
	// 需要 3 个 Key: 每次重试都会把上次的 Key 加入 exclude，
	// Key 数少于尝试次数时调度器会先返回「无可用 Key」而提前收尾，
	// 那样测的就不是「重试上限」而是「Key 耗尽」了。
	env := newItEnv(t, []string{"k1", "k2", "k3"}, func(c *config.Config, o *mockark.Options) {
		c.Upstream.MaxRetries = 2
	})
	// -1 表示永久生效
	env.ark.Inject(mockark.Fault{Kind: mockark.Fault429RateLimit, Remaining: -1})

	resp, body := env.chat(t, itChat)
	// 上游 429 对外报 503: 避免客户端把上游限流误当成本网关的限流
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d, 期望 503: %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "rate_limit_error") {
		t.Errorf("上游限流不应对外呈现为本网关限流: %s", body)
	}

	if n := len(env.arkLogs()); n != 3 {
		t.Errorf("上游调用 %d 次, 期望 3（1 + MaxRetries 2）", n)
	}
	// 3 次尝试的租约必须全部结束
	env.quota.assertClean(t)
}

func Test集成_401立即禁用Key(t *testing.T) {
	env := newItEnv(t, []string{"k1", "k2", "k3"})
	env.ark.Inject(mockark.Fault{Kind: mockark.Fault401, KeyID: "k1", Remaining: -1})

	resp, body := env.chat(t, itChat)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望换 Key 后成功: %s", resp.StatusCode, body)
	}

	fs := env.sched.failuresOf("k1")
	if len(fs) == 0 || fs[0].String() != "auth" {
		t.Errorf("k1 失败分类 = %v, 期望 auth", fs)
	}
	// 401 意味着 Key 已失效或被封，必须立即停用而非冷却
	for _, d := range env.sched.disabledKeys() {
		if d == "k1" {
			env.quota.assertClean(t)
			return
		}
	}
	t.Error("401 后 k1 未被停用")
}

func Test集成_5xx换Key重试(t *testing.T) {
	env := newItEnv(t, []string{"k1", "k2", "k3"})
	env.ark.Inject(mockark.Fault{Kind: mockark.Fault500, Remaining: 1})

	resp, body := env.chat(t, itChat)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", resp.StatusCode, body)
	}
	logs := env.arkLogs()
	if len(logs) != 2 {
		t.Fatalf("上游调用 %d 次, 期望 2", len(logs))
	}
	if logs[0].KeyID == logs[1].KeyID {
		t.Error("5xx 后未换 Key")
	}
	// 5xx 降低健康分但不停用: 上游偶发故障不代表 Key 有问题
	if d := env.sched.disabledKeys(); len(d) != 0 {
		t.Errorf("5xx 导致 Key 被停用: %v", d)
	}
	env.quota.assertClean(t)
}

// ===== 客户端断连 =====

func Test集成_客户端断连后租约被回收(t *testing.T) {
	env := newItEnv(t, []string{"k1"}, func(c *config.Config, o *mockark.Options) {
		o.StreamChunks = 30
		o.StreamChunkDelay = 30 * time.Millisecond
	})

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		env.gw.URL+"/v1/chat/completions", strings.NewReader(itStream))
	req.Header.Set("Authorization", "Bearer itest-key")

	resp, err := env.gw.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}

	// 读到首个 chunk 后立刻断开
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("读取首 chunk: %v", err)
	}
	cancel()
	resp.Body.Close()

	// 等网关感知断连并结束租约
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if env.quota.usedOf("k1", quota.KindToken) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 核心断言: 租约不能悬置等 TTL 超时回收
	env.quota.assertClean(t)

	// 上游已实际消耗，必须计入而非释放
	if used := env.quota.usedOf("k1", quota.KindToken); used <= 0 {
		t.Error("断连时上游已消耗额度，必须计入")
	}
	// 客户端主动断开不算 Key 失败
	if fs := env.sched.failuresOf("k1"); len(fs) != 0 {
		t.Errorf("客户端断连被误报为 Key 失败: %v", fs)
	}
}

func Test集成_上游流式中断时仍结束租约(t *testing.T) {
	// FaultStreamAbort: 上游发到一半就断，不发 usage 也不发 [DONE]。
	// 上游已实际消耗额度，网关必须按预扣量兜底 Commit。
	env := newItEnv(t, []string{"k1"}, func(c *config.Config, o *mockark.Options) {
		o.StreamChunks = 10
	})
	env.ark.Inject(mockark.Fault{Kind: mockark.FaultStreamAbort, Remaining: -1})

	req, _ := http.NewRequest(http.MethodPost, env.gw.URL+"/v1/chat/completions",
		strings.NewReader(itStream))
	req.Header.Set("Authorization", "Bearer itest-key")
	resp, err := env.gw.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	time.Sleep(300 * time.Millisecond)
	env.quota.assertClean(t)

	// 拿不到 usage 时按预扣量计入。按 0 计入会让本地水位低于真实值，
	// 累积后必然超刷。
	if used := env.quota.usedOf("k1", quota.KindToken); used <= 0 {
		t.Errorf("流式中断后已确认用量 = %d, 期望按预扣量兜底", used)
	}
}

// ===== 并发不超刷 =====

func Test集成_并发请求不超刷(t *testing.T) {
	// 水位取「明显不足以容纳全部请求」的量: 单请求预扣约
	// (prompt 估算 + max_tokens 200) × 1.2 ≈ 250，80 个请求约需 20000。
	// 水位设为 6000 保证必然有请求被拒，从而真正检验准入而非碰巧全过。
	const hard = 6_000
	env := newItEnv(t, []string{"k1"}, func(c *config.Config, o *mockark.Options) {
		c.Upstream.MaxRetries = 0
		c.Quota.DefaultMaxTokens = 200
		c.Quota.EstimateMultiplier = 1.2
	})
	env.quota.mu.Lock()
	env.quota.hardOverride = hard
	env.quota.mu.Unlock()

	const n = 80
	var wg sync.WaitGroup
	var ok, rejected int
	var mu sync.Mutex

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := env.chat(t, itChat)
			mu.Lock()
			if resp.StatusCode == http.StatusOK {
				ok++
			} else {
				rejected++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if ok == 0 {
		t.Fatal("全部请求被拒")
	}
	if rejected == 0 {
		t.Fatalf("%d 个请求全部通过，硬水位 %d 未生效", n, hard)
	}

	// 网关侧的确认用量与 mockark 侧的独立记账都不得越界。
	// 后者是关键: 它证明「真实发出去的请求」总量受控，
	// 而不只是网关自己的账面数字受控。
	gwUsed := env.quota.usedOf("k1", quota.KindToken)
	arkUsed := env.ark.Account("k1").TokenUsed

	if gwUsed > hard {
		t.Errorf("网关确认用量 %d 超过硬水位 %d", gwUsed, hard)
	}
	if arkUsed > hard {
		t.Errorf("上游实际消耗 %d 超过硬水位 %d —— 真正的超刷", arkUsed, hard)
	}
	// 两侧记账应当一致（网关按上游返回的 usage 修正）
	if gwUsed != arkUsed {
		t.Errorf("网关记账 %d 与上游记账 %d 不一致，配额修正有误", gwUsed, arkUsed)
	}
	t.Logf("成功 %d / 拒绝 %d，用量 %d / 水位 %d", ok, rejected, gwUsed, hard)
	env.quota.assertClean(t)
}

func Test集成_并发混合场景无租约泄漏(t *testing.T) {
	env := newItEnv(t, []string{"k1", "k2", "k3"}, func(c *config.Config, o *mockark.Options) {
		o.StreamChunks = 5
		o.StreamChunkDelay = 15 * time.Millisecond
	})
	// 随机注入故障，让成功/失败/重试路径混在一起跑
	env.ark.Inject(mockark.Fault{Kind: mockark.Fault500, Remaining: 5})
	env.ark.Inject(mockark.Fault{Kind: mockark.Fault429RateLimit, Remaining: 5})

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				env.chat(t, itChat)
			case 1:
				req, _ := http.NewRequest(http.MethodPost,
					env.gw.URL+"/v1/chat/completions", strings.NewReader(itStream))
				req.Header.Set("Authorization", "Bearer itest-key")
				if resp, err := env.gw.Client().Do(req); err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			case 2:
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
					env.gw.URL+"/v1/chat/completions", strings.NewReader(itStream))
				req.Header.Set("Authorization", "Bearer itest-key")
				if resp, err := env.gw.Client().Do(req); err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}(i)
	}
	wg.Wait()

	// 给断连路径的 defer 时间跑完
	time.Sleep(800 * time.Millisecond)
	env.quota.assertClean(t)
}

// ===== 次数型计费 =====

func Test集成_图片生成按次计费(t *testing.T) {
	env := newItEnv(t, []string{"k1"})

	resp, body := env.do(t, http.MethodPost, "/v1/images/generations", "itest-key",
		`{"model":"seedream-3.0","prompt":"一只在屋顶的猫","n":2}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", resp.StatusCode, body)
	}

	// 次数型走独立配额，不能记到 token 上
	if c := env.quota.usedOf("k1", quota.KindCount); c <= 0 {
		t.Errorf("count 用量 = %d, 期望大于 0", c)
	}
	if tk := env.quota.usedOf("k1", quota.KindToken); tk != 0 {
		t.Errorf("次数型请求消耗了 %d token 配额", tk)
	}

	acct := env.ark.Account("k1")
	if acct.CountUsed <= 0 {
		t.Errorf("mockark count 记账 = %d", acct.CountUsed)
	}
	if got := env.quota.usedOf("k1", quota.KindCount); got != acct.CountUsed {
		t.Errorf("网关 count 记账 %d 与上游 %d 不一致", got, acct.CountUsed)
	}
	env.quota.assertClean(t)

	recs := env.store.records()
	if len(recs) != 1 || recs[0].BillingKind != string(quota.KindCount) {
		t.Errorf("流水计费类型错误: %+v", recs)
	}
}

func Test集成_次数额度耗尽后换Key(t *testing.T) {
	env := newItEnv(t, []string{"k1", "k2"})
	// CountLimit=0 在 mockark 里表示「不限」而非「耗尽」，
	// 故仍用故障注入来构造额度耗尽。
	env.ark.Inject(mockark.Fault{Kind: mockark.Fault429Quota, KeyID: "k1", Remaining: -1})

	resp, body := env.do(t, http.MethodPost, "/v1/images/generations", "itest-key",
		`{"model":"seedream-3.0","prompt":"猫","n":1}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望换 Key 后成功: %s", resp.StatusCode, body)
	}
	if env.quota.usedOf("k1", quota.KindCount) != 0 {
		t.Error("额度耗尽的 k1 被记入了 count 用量")
	}
	env.quota.assertClean(t)
}

// ===== 异常响应 =====

func Test集成_上游返回非法JSON时不崩且结束租约(t *testing.T) {
	env := newItEnv(t, []string{"k1", "k2"}, func(c *config.Config, o *mockark.Options) {
		c.Upstream.MaxRetries = 1
	})
	env.ark.Inject(mockark.Fault{Kind: mockark.FaultBadJSON, Remaining: -1})

	resp, body := env.chat(t, itChat)
	// 关键不是状态码具体是几，而是不能 panic 且租约要结束
	if resp.StatusCode == 0 {
		t.Fatal("请求未得到响应")
	}
	t.Logf("非法 JSON 场景状态码 = %d, 响应 = %s", resp.StatusCode, body)
	env.quota.assertClean(t)
}

func Test集成_上游超时被感知(t *testing.T) {
	env := newItEnv(t, []string{"k1", "k2"}, func(c *config.Config, o *mockark.Options) {
		c.Upstream.MaxRetries = 0
		// 出口请求超时压到很短，让 timeout 故障能被及时判定
		c.Egress.RequestTimeout = 200 * time.Millisecond
	})
	env.ark.Inject(mockark.Fault{
		Kind: mockark.FaultTimeout, Remaining: -1, Delay: 3 * time.Second,
	})

	start := time.Now()
	resp, _ := env.chat(t, itChat)
	elapsed := time.Since(start)

	if resp.StatusCode == http.StatusOK {
		t.Error("上游超时却返回了 200")
	}
	// 必须在出口超时附近返回，不能一直等到上游的 3 秒
	if elapsed > 2*time.Second {
		t.Errorf("耗时 %v，出口请求超时未生效", elapsed)
	}
	env.quota.assertClean(t)
}

// ===== 健康检查与管理接口 =====

func Test集成_健康与就绪端点(t *testing.T) {
	env := newItEnv(t, []string{"k1"})

	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := env.gw.Client().Get(env.gw.URL + path)
		if err != nil {
			t.Fatalf("%s 请求失败: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s 状态码 = %d, 期望 200", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func Test集成_管理接口列出Key(t *testing.T) {
	env := newItEnv(t, []string{"k1", "k2"})
	resp, body := env.do(t, http.MethodGet, "/admin/keys", "itest-admin", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", resp.StatusCode, body)
	}
	// 绝不能泄漏密钥明文
	if strings.Contains(body, "sk-mock-") {
		t.Errorf("管理接口泄漏了密钥明文: %s", body)
	}
}

func Test集成_出口绑定在上游侧可验证(t *testing.T) {
	// direct 模式下所有 Key 共享同一出口，本用例验证的是
	// 「mockark 确实记录了源 IP」这一观测能力本身 —— 它是
	// multi_ip 模式下验证 P1-6 是否生效的唯一手段。
	env := newItEnv(t, []string{"k1", "k2"})
	env.chat(t, itChat)
	env.chat(t, itChat)

	for _, id := range []string{"k1", "k2"} {
		acct := env.ark.Account(id)
		if acct.Requests == 0 {
			continue
		}
		if len(acct.SourceIPs) == 0 {
			t.Errorf("%s 的请求未记录源 IP，无法验证出口绑定是否生效", id)
		}
	}

	logs := env.arkLogs()
	for _, l := range logs {
		if l.SourceIP == "" {
			t.Error("请求日志缺少源 IP")
		}
	}
}

// ===== 优雅关闭 =====

func Test集成_优雅关闭等待流式完成(t *testing.T) {
	env := newItEnv(t, []string{"k1"}, func(c *config.Config, o *mockark.Options) {
		o.StreamChunks = 6
		o.StreamChunkDelay = 40 * time.Millisecond
	})

	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, env.gw.URL+"/v1/chat/completions",
			strings.NewReader(itStream))
		req.Header.Set("Authorization", "Bearer itest-key")
		resp, err := env.gw.Client().Do(req)
		if err != nil {
			done <- -1
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- strings.Count(string(b), "data:")
	}()

	time.Sleep(80 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := env.srv.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown 返回错误: %v", err)
	}

	select {
	case n := <-done:
		if n < 2 {
			t.Errorf("流式只收到 %d 个 chunk，被关闭打断了", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("流式请求未完成")
	}
	env.quota.assertClean(t)
}

// ===== 出口 IP 被封禁的实际行为 =====

// 本用例把「出口 IP 被上游封禁」时网关的真实行为钉死，并记录其局限。
//
// 结论（已实证，非推测）: 出口级封禁目前**无法**被自动识别和迁移。
//
//	403 在 adapter 侧归为 ErrClassAuth（adapter.go:68），
//	proxy.go:359 又刻意把 auth 类排除在出口信誉记账之外:
//	  markEgress(keyID, ue.Class != ErrClassAuth && ue.Class != ErrClassRateLimit)
//	于是后果是逐个 Key 被判 auth 失败，出口 IP 的信誉分毫发无损。
//
// 这个取舍本身合理: 上游返回 401/403 时无法区分「Key 被封」与「IP 被封」，
// 错误地降级一个健康 IP 会连带影响其上所有 Key。但代价是出口被封时只能靠
// 运维观察到「某个 IP 上的 Key 成批失败」后手动介入。
//
// 若将来实现自动识别（例如「同一出口上 N 个不同 Key 短时间内相继 auth 失败
// → 判定 IP 被封」），本用例的断言需要相应更新 —— 那时它会正好失败，
// 提醒改动者确认新行为符合预期。
func TestE2E_出口IP被封时的实际行为(t *testing.T) {
	ips := []*egress.IP{
		egress.NewIP("127.0.0.1", "203.0.113.1", 10),
	}
	env := newItEnvWithEgress(t, []string{"k1", "k2"}, ips)

	// 建立绑定，确认两个 Key 都落在这个唯一的出口上
	for _, id := range []string{"k1", "k2"} {
		addr, err := env.egress.Bind(id)
		if err != nil {
			t.Fatalf("绑定 %s: %v", id, err)
		}
		if addr != "127.0.0.1" {
			t.Fatalf("%s 绑定到 %s，期望 127.0.0.1", id, addr)
		}
	}

	ip := env.egress.IPFor("k1")
	if ip == nil {
		t.Fatal("取不到 k1 的出口 IP 对象")
	}
	repBefore := ip.Reputation()
	stateBefore := ip.State()

	// 封禁该出口: 从这个 IP 发出的所有请求一律 403
	env.ark.BanSourceIP("127.0.0.1")

	resp, body := env.chat(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	resp.Body.Close()

	// 所有 Key 都从这个被封的出口发出，重试也救不回来，故请求必然失败
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("出口被封时请求不应成功: %s", body)
	}

	// 关键断言: 出口 IP 的信誉分未受影响 —— 这正是当前设计的取舍
	if got := ip.Reputation(); got != repBefore {
		t.Errorf("auth 类错误不应影响出口信誉，实际 %d → %d\n"+
			"若这是有意的行为变更，请同步更新本用例与 mockark.BanSourceIP 的注释",
			repBefore, got)
	}
	if got := ip.State(); got != stateBefore {
		t.Errorf("auth 类错误不应改变出口状态，实际 %s → %s", stateBefore, got)
	}

	// mockark 侧应确实收到了来自该源 IP 的请求并全部拒绝
	logs := env.ark.Logs()
	if len(logs) == 0 {
		t.Fatal("mockark 未收到任何请求，按源 IP 封禁可能未生效")
	}
	for _, l := range logs {
		if l.Status != http.StatusForbidden {
			t.Errorf("被封出口的请求状态 = %d, 期望全部 403", l.Status)
			break
		}
	}
	t.Logf("出口被封后: %d 次请求全部 403，出口信誉仍为 %d（当前设计不自动降级出口）",
		len(logs), ip.Reputation())
}

// 对照用例: 网络类错误（非 auth）确实会降级出口信誉。
//
// 与上一个用例并置，说明「出口信誉记账」这条链路本身是通的，
// 上面之所以不生效是 auth 类被刻意排除，而非记账逻辑失灵。
func TestE2E_网络错误会降级出口信誉(t *testing.T) {
	ips := []*egress.IP{egress.NewIP("127.0.0.1", "203.0.113.1", 10)}
	env := newItEnvWithEgress(t, []string{"k1"}, ips, func(c *config.Config, _ *mockark.Options) {
		// 缩短超时，让 timeout 故障快速收敛
		c.Egress.RequestTimeout = 150 * time.Millisecond
		c.Upstream.MaxRetries = 1
	})

	if _, err := env.egress.Bind("k1"); err != nil {
		t.Fatal(err)
	}
	ip := env.egress.IPFor("k1")
	if ip == nil {
		t.Fatal("取不到出口 IP 对象")
	}
	if ip.State() != egress.IPActive {
		t.Fatalf("前置条件: 出口应为 active，实际 %s", ip.State())
	}

	// timeout 归为 ErrClassNetwork，不在排除名单里
	env.ark.Inject(mockark.Fault{
		Kind: mockark.FaultTimeout, SourceIP: "127.0.0.1",
		Remaining: -1, Delay: 2 * time.Second,
	})

	resp, _ := env.chat(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	resp.Body.Close()

	if ip.State() == egress.IPActive {
		t.Error("网络类错误应降级出口状态（active → suspect），实际未变")
	}
	t.Logf("网络错误后出口状态: %s，信誉 %d", ip.State(), ip.Reputation())
}

// ===== 出口被封的自动识别与撤离 =====

// 启用 ban_detect_keys 后，多个不同 Key 从同一出口相继 auth 失败应触发
// 自动撤离: 该出口被标记封禁，其上 Key 迁到同档位的健康出口。
func TestE2E_出口被封时自动撤离(t *testing.T) {
	ips := []*egress.IP{
		egress.NewIP("127.0.0.1", "203.0.113.1", 10),
		egress.NewIP("127.0.0.2", "203.0.113.2", 10),
	}
	env := newItEnvWithEgress(t, []string{"k1", "k2", "k3"}, ips,
		func(c *config.Config, _ *mockark.Options) {
			c.Egress.BanDetectKeys = 3
			c.Egress.BanDetectWindow = time.Minute
			// 关掉重试，让每次请求只打一个 Key，便于精确累积不同 Key 数
			c.Upstream.MaxRetries = 0
		})

	// 三个 Key 全绑到 127.0.0.1，模拟它们共享一个出口
	for _, k := range []string{"k1", "k2", "k3"} {
		if err := env.egress.Adopt(k, "127.0.0.1", egress.PoolAny); err != nil {
			t.Fatalf("预置绑定 %s: %v", k, err)
		}
	}
	banned := env.egress.IPFor("k1")
	if banned == nil || banned.State() != egress.IPActive {
		t.Fatal("前置条件: 出口应存在且为 active")
	}

	// 封禁该出口: 从它发出的请求一律 403
	env.ark.BanSourceIP("127.0.0.1")

	// 反复请求直到三个不同 Key 都失败过。调度器按画像挑 Key，
	// 故用足够多次数覆盖，而非假设某一次打到哪个 Key。
	for i := 0; i < 30; i++ {
		resp, _ := env.chat(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
		resp.Body.Close()
		if banned.State() == egress.IPBanned {
			break
		}
	}

	if got := banned.State(); got != egress.IPBanned {
		t.Fatalf("三个 Key 均 auth 失败后应判定出口被封，实际状态 %s（auth 失败 Key 数 %d）",
			got, banned.AuthFailKeys(time.Minute))
	}

	// 撤离结果: 所有 Key 都应离开被封出口
	if left := env.egress.KeysOn("127.0.0.1"); len(left) != 0 {
		t.Errorf("仍有 Key 留在被封出口: %v", left)
	}
	for _, k := range []string{"k1", "k2", "k3"} {
		if got := env.egress.BoundIP(k); got != "127.0.0.2" {
			t.Errorf("%s 绑定在 %s，应已迁到 127.0.0.2", k, got)
		}
	}
	t.Logf("出口 127.0.0.1 已判定封禁，3 个 Key 全部迁至 127.0.0.2")
}

// 只有一个 Key 出问题时，出口不应被判定封禁，其余 Key 也不应被无谓迁移。
//
// 注意本用例实际覆盖的是「单个坏 Key 不影响出口与同出口的其他 Key」，
// 而非「同一个 Key 反复失败不累积计数」—— 后者在真实链路里不会发生:
// 调度器在该 Key 首次 auth 失败后就把它判为 invalid 并不再选取
// （实测 k1 只被打到 1 次，k2/k3 各十几次）。
// 「同 Key 反复失败不累积」这一判据由 egress 包的
// TestMarkAuthFailure_同一Key不重复计数 直接覆盖，那里能精确控制调用。
func TestE2E_单个坏Key不影响出口与其他Key(t *testing.T) {
	ips := []*egress.IP{
		egress.NewIP("127.0.0.1", "203.0.113.1", 10),
		egress.NewIP("127.0.0.2", "203.0.113.2", 10),
	}
	env := newItEnvWithEgress(t, []string{"k1", "k2", "k3"}, ips,
		func(c *config.Config, _ *mockark.Options) {
			c.Egress.BanDetectKeys = 3
			c.Egress.BanDetectWindow = time.Minute
			c.Upstream.MaxRetries = 0
		})

	for _, k := range []string{"k1", "k2", "k3"} {
		if err := env.egress.Adopt(k, "127.0.0.1", egress.PoolAny); err != nil {
			t.Fatal(err)
		}
	}
	ip := env.egress.IPFor("k1")

	// 只让 k1 失败，其余 Key 正常
	env.ark.Inject(mockark.Fault{
		Kind: mockark.Fault403, KeyID: "k1", Remaining: -1,
	})

	for i := 0; i < 30; i++ {
		resp, _ := env.chat(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
		resp.Body.Close()
	}

	if got := ip.State(); got == egress.IPBanned {
		t.Error("只有一个 Key 失败，不应判定出口被封")
	}
	if n := ip.AuthFailKeys(time.Minute); n > 1 {
		t.Errorf("auth 失败的不同 Key 数应为 1，实际 %d", n)
	}
	// 其余 Key 应仍在原出口上，未被无谓迁移
	for _, k := range []string{"k2", "k3"} {
		if got := env.egress.BoundIP(k); got != "127.0.0.1" {
			t.Errorf("%s 被无谓迁移到 %s", k, got)
		}
	}
}

// 未启用 ban_detect_keys 时保持原行为，不做任何自动撤离。
func TestE2E_未启用判定时不自动撤离(t *testing.T) {
	ips := []*egress.IP{
		egress.NewIP("127.0.0.1", "203.0.113.1", 10),
		egress.NewIP("127.0.0.2", "203.0.113.2", 10),
	}
	env := newItEnvWithEgress(t, []string{"k1", "k2", "k3"}, ips,
		func(c *config.Config, _ *mockark.Options) {
			c.Egress.BanDetectKeys = 0 // 显式关闭
			c.Upstream.MaxRetries = 0
		})

	for _, k := range []string{"k1", "k2", "k3"} {
		if err := env.egress.Adopt(k, "127.0.0.1", egress.PoolAny); err != nil {
			t.Fatal(err)
		}
	}
	ip := env.egress.IPFor("k1")
	env.ark.BanSourceIP("127.0.0.1")

	for i := 0; i < 20; i++ {
		resp, _ := env.chat(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
		resp.Body.Close()
	}

	if got := ip.State(); got == egress.IPBanned {
		t.Error("未启用自动判定时不应封禁出口")
	}
	if left := env.egress.KeysOn("127.0.0.1"); len(left) != 3 {
		t.Errorf("未启用时不应迁移，被封出口上应仍有 3 个 Key，实际 %d", len(left))
	}
}

// ===== 档位归属传递到出口层 =====

// 尚未建立绑定的 Key（新导入、还没经过 restoreBindings）会在请求热路径上
// 首次绑定出口。此时必须落在其档位对应的 IP 上，否则分层会出现缺口 ——
// 一个 hot 档的高频 Key 若被分到承载上百个 Key 的 cold 档 IP 上，
// 分层的全部收益归零。
//
// 用例构造有两个关键点。
//
// 一是必须让「不传档位」时的哈希落点与「传档位」不同，否则断言恒真:
// cold 档给 4 个 IP 稀释哈希空间，并选用实测确认落点不同的 key_id "k0"
// （传 hot 档 → 127.0.0.1，不传 → 127.0.0.5）。若 cold 只有 1 个 IP，
// 哈希在 2 个候选里选，很容易碰巧命中 hot —— 那样的用例即使把档位传递
// 整段删掉也照样通过。
//
// 二是 hot 档必须用 127.0.0.1: 只有它绑在 lo0 上，其余 127.0.0.x 未配
// 别名，dialer.LocalAddr 会直接失败，请求变成 503 而非落点不同。
func TestE2E_热路径首次绑定落在正确档位(t *testing.T) {
	ips := []*egress.IP{
		egress.NewPooledIP("127.0.0.1", "203.0.113.1", 10, "hot"),
		egress.NewPooledIP("127.0.0.3", "203.0.113.3", 50, "cold"),
		egress.NewPooledIP("127.0.0.4", "203.0.113.4", 50, "cold"),
		egress.NewPooledIP("127.0.0.5", "203.0.113.5", 50, "cold"),
		egress.NewPooledIP("127.0.0.6", "203.0.113.6", 50, "cold"),
	}
	env := newItEnvWithEgress(t, []string{"k0"}, ips)
	env.sched.pools["k0"] = "hot"

	if got := env.egress.BoundIP("k0"); got != "" {
		t.Fatalf("前置条件: k0 不应有绑定，实际 %q", got)
	}

	resp, body := env.chat(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("请求应成功: %d %s", resp.StatusCode, body)
	}

	// 档位传下去 → hot 档只有 127.0.0.1 一个候选；没传 → 哈希落到 127.0.0.5
	if got := env.egress.BoundIP("k0"); got != "127.0.0.1" {
		t.Errorf("hot 档 Key 绑定在 %q，应落在唯一的 hot 档出口 127.0.0.1 —— "+
			"档位归属未从调度器传到出口层", got)
	}
}

// 未指定档位的 Key 应能落在任何出口上，不因分层而无法服务。
func TestE2E_未指定档位的Key仍可绑定(t *testing.T) {
	ips := []*egress.IP{
		egress.NewPooledIP("127.0.0.1", "203.0.113.1", 50, "cold"),
	}
	env := newItEnvWithEgress(t, []string{"k1"}, ips)
	// 刻意不设 pools["k1"]，模拟历史数据里 pool 为空的 Key

	resp, body := env.chat(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("未指定档位不应导致请求失败: %d %s", resp.StatusCode, body)
	}
	if got := env.egress.BoundIP("k1"); got != "127.0.0.1" {
		t.Errorf("未指定档位的 Key 应能绑定到任意出口，实际 %q", got)
	}
}
