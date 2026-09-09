package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/quota"
)

// ===== 辅助 =====

const chatBody = `{"model":"gpt-4o","max_tokens":50,` +
	`"messages":[{"role":"user","content":"你好，请介绍一下你自己"}]}`

const okChatResp = `{"id":"c1","object":"chat.completion","model":"ep-test-4o",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":30,"completion_tokens":12,"total_tokens":42}}`

// post 向网关发一次请求。
func (e *testEnv) post(t *testing.T, path, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("发起请求: %v", err)
	}
	return resp
}

func readAll(t *testing.T, r *http.Response) string {
	t.Helper()
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("读取响应体: %v", err)
	}
	return string(b)
}

// ===== 鉴权 =====

func TestAuth_缺少Authorization头返回401(t *testing.T) {
	env := newTestEnv(t)
	resp := env.post(t, "/v1/chat/completions", "", chatBody)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望 401", resp.StatusCode)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "authentication_error") {
		t.Errorf("错误类型应为 authentication_error，实际: %s", body)
	}
	// 鉴权失败不应触达上游，否则等于把未授权流量转发出去
	if env.upstream.callCount() != 0 {
		t.Errorf("鉴权失败仍调用了上游 %d 次", env.upstream.callCount())
	}
}

func TestAuth_无效Key返回401且不泄漏原因(t *testing.T) {
	env := newTestEnv(t)
	resp := env.post(t, "/v1/chat/completions", "bad-key", chatBody)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望 401", resp.StatusCode)
	}
	first := readAll(t, resp)

	// 「不存在 / 已吊销 / 用户停用」必须返回完全相同的响应 —— 若响应随原因
	// 变化，撞库者就能借此判定「这个 Key 曾经存在」。
	// 这里用另一个同样不存在的 Key 再打一次，两次响应必须逐字节相同。
	resp2 := env.post(t, "/v1/chat/completions", "another-bad-key", chatBody)
	second := readAll(t, resp2)
	if resp2.StatusCode != resp.StatusCode || first != second {
		t.Errorf("不同鉴权失败原因返回了不同响应:\n第一次: %d %s\n第二次: %d %s",
			resp.StatusCode, first, resp2.StatusCode, second)
	}
}

func TestAuth_请求ID贯穿响应头(t *testing.T) {
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{Status: 200, Body: okChatResp})

	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/v1/chat/completions",
		strings.NewReader(chatBody))
	req.Header.Set("Authorization", "Bearer user-key-ok")
	req.Header.Set("X-Request-Id", "trace-abc-123")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	// 复用上游传入的 ID 而非生成新的，否则跨系统链路无法关联
	if got := resp.Header.Get("X-Request-Id"); got != "trace-abc-123" {
		t.Errorf("X-Request-Id = %q, 期望沿用传入的 trace-abc-123", got)
	}
	recs := env.store.usageRecords()
	if len(recs) != 1 || recs[0].RequestID != "trace-abc-123" {
		t.Errorf("用量流水未记录同一个请求 ID: %+v", recs)
	}
}

// ===== 正常请求 =====

func TestChat_非流式请求成功并按实际用量Commit(t *testing.T) {
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{Status: 200, Body: okChatResp})

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200，响应: %s", resp.StatusCode, readAll(t, resp))
	}
	body := readAll(t, resp)

	// 上游模型名（ep-test-4o）必须被映射回对外名，否则客户端会看到内部端点 ID
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("响应非合法 JSON: %v", err)
	}
	if got["model"] != "gpt-4o" {
		t.Errorf("响应 model = %v, 期望映射回 gpt-4o", got["model"])
	}

	// 按 usage 里的 42 而非预扣量 Commit
	if used := env.quota.usedFor("volc_001", quota.KindToken); used != 42 {
		t.Errorf("已确认用量 = %d, 期望 42（来自上游 usage）", used)
	}
	env.quota.assertClean(t)

	commits, releases := env.quota.counts()
	if commits != 1 || releases != 0 {
		t.Errorf("Commit/Release = %d/%d, 期望 1/0", commits, releases)
	}
	if env.sched.successCount("volc_001") != 1 {
		t.Error("成功请求未上报 MarkSuccess")
	}
}

func TestChat_用量流水记录完整字段(t *testing.T) {
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{Status: 200, Body: okChatResp})

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	resp.Body.Close()

	recs := env.store.usageRecords()
	if len(recs) != 1 {
		t.Fatalf("流水条数 = %d, 期望 1", len(recs))
	}
	r := recs[0]
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"UserID", r.UserID, int64(7)},
		{"UserAPIKeyID", r.UserAPIKeyID, int64(70)},
		{"UpstreamKeyID", r.UpstreamKeyID, "volc_001"},
		{"Model", r.Model, "gpt-4o"},
		{"BillingKind", r.BillingKind, string(quota.KindToken)},
		{"PromptTokens", r.PromptTokens, int64(30)},
		{"CompletionTokens", r.CompletionTokens, int64(12)},
		{"TotalTokens", r.TotalTokens, int64(42)},
		{"StatusCode", r.StatusCode, 200},
		{"IsStream", r.IsStream, false},
		{"RetryCount", r.RetryCount, 0},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("流水 %s = %v, 期望 %v", c.name, c.got, c.want)
		}
	}
	if r.EstimatedTokens <= 0 {
		t.Error("流水未记录预扣量，无法事后评估估算准确度")
	}
	// P0-3: 配额日按 12:00 分界，不是自然日
	if want := quota.QuotaDayTime(time.Now()); !r.QuotaDay.Equal(want) {
		t.Errorf("流水 QuotaDay = %v, 期望配额日 %v", r.QuotaDay, want)
	}
}

func TestChat_预扣量包含prompt估算(t *testing.T) {
	// P2: V3 只按 max_tokens 预扣，漏算 prompt。长 prompt + 小 max_tokens 时
	// 预扣量会远低于真实消耗，是超刷的直接来源。
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{Status: 200, Body: okChatResp})

	long := strings.Repeat("这是一段很长的中文提示词内容。", 200) // 约 3000 字符
	body := fmt.Sprintf(`{"model":"gpt-4o","max_tokens":10,`+
		`"messages":[{"role":"user","content":%q}]}`, long)

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", body)
	resp.Body.Close()

	recs := env.store.usageRecords()
	if len(recs) != 1 {
		t.Fatalf("流水条数 = %d", len(recs))
	}
	// 只按 max_tokens(10) × 1.2 算是 12。含 prompt 估算后应远大于此。
	if est := recs[0].EstimatedTokens; est < 500 {
		t.Errorf("预扣量 = %d, 长 prompt 场景下明显偏低，说明未计入 prompt 估算", est)
	}
}

func TestModels_只暴露已配置映射的模型(t *testing.T) {
	env := newTestEnv(t)
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer user-key-ok")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body := readAll(t, resp)

	var out struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应非合法 JSON: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range out.Data {
		ids[m.ID] = true
	}
	// 只列本网关确实能路由的模型: 透传上游列表会让用户请求到未配映射的模型
	// 而在转换阶段才失败
	if !ids["gpt-4o"] || !ids["seedream-3.0"] {
		t.Errorf("模型列表不完整: %v", ids)
	}
	if ids["ep-test-4o"] {
		t.Error("模型列表泄漏了上游端点 ID")
	}
}

// ===== 流式 =====

func TestChat_流式真流式且逐chunk刷出(t *testing.T) {
	env := newTestEnv(t)
	// 每个 chunk 间隔 60ms。若网关缓冲整个响应，首个 chunk 的到达时间
	// 会等于全部 chunk 的总耗时。
	env.upstream.setScript(stubResponse{
		Status:     200,
		ChunkDelay: 60 * time.Millisecond,
		SSE: []string{
			`{"id":"c1","model":"ep-test-4o","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"id":"c1","model":"ep-test-4o","choices":[{"index":0,"delta":{"content":"你"}}]}`,
			`{"id":"c1","model":"ep-test-4o","choices":[{"index":0,"delta":{"content":"好"}}]}`,
			`{"id":"c1","model":"ep-test-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"c1","model":"ep-test-4o","choices":[],"usage":{"prompt_tokens":25,"completion_tokens":8,"total_tokens":33}}`,
			"[DONE]",
		},
	})

	start := time.Now()
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"max_tokens":50,`+
			`"messages":[{"role":"user","content":"你好"}]}`))
	req.Header.Set("Authorization", "Bearer user-key-ok")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, 期望 text/event-stream", ct)
	}
	// 必须显式关闭 Nginx 缓冲，否则中间层会抵消所有流式努力
	if v := resp.Header.Get("X-Accel-Buffering"); v != "no" {
		t.Errorf("X-Accel-Buffering = %q, 期望 no", v)
	}

	br := bufio.NewReader(resp.Body)
	var firstAt time.Duration
	var lines []string
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data:") {
				if firstAt == 0 {
					firstAt = time.Since(start)
				}
				lines = append(lines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
			}
		}
		if err != nil {
			break
		}
	}

	total := time.Since(start)
	if len(lines) != 6 {
		t.Fatalf("收到 %d 个 data 行, 期望 6: %v", len(lines), lines)
	}
	if lines[len(lines)-1] != "[DONE]" {
		t.Errorf("末行 = %q, 期望 [DONE]", lines[len(lines)-1])
	}

	// 真流式的判定: 首 chunk 必须远早于整体完成。6 个 chunk × 60ms ≈ 360ms，
	// 首 chunk 应在 200ms 内到达。
	if firstAt > 200*time.Millisecond {
		t.Errorf("首 chunk 耗时 %v, 总耗时 %v —— 疑似缓冲了整个响应", firstAt, total)
	}
	if total < 250*time.Millisecond {
		t.Fatalf("总耗时 %v 过短，假上游未按预期分块，本用例的判定失效", total)
	}

	// 末尾 chunk 的 usage 必须被解析并用于配额修正
	if used := env.quota.usedFor("volc_001", quota.KindToken); used != 33 {
		t.Errorf("已确认用量 = %d, 期望 33（来自流式末尾 usage）", used)
	}
	env.quota.assertClean(t)
}

func TestChat_流式自动补齐include_usage(t *testing.T) {
	// 不补齐则上游不会返回 usage，只能按预扣量 Commit，误差随流量累积
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{Status: 200, SSE: []string{"[DONE]"}})

	resp := env.post(t, "/v1/chat/completions", "user-key-ok",
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// 无法直接检查上游收到的 body（stub 未记录），改为验证行为等价性:
	// EnsureStreamUsage 的单测在 adapter 包，这里只确认流程跑通且租约干净
	env.quota.assertClean(t)
}

func TestChat_流式无usage时按预扣量Commit(t *testing.T) {
	// 按 0 计入会让本地水位低于真实值，累积后必然超刷。宁可高估。
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{
		Status: 200,
		SSE: []string{
			`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
			"[DONE]",
		},
	})

	resp := env.post(t, "/v1/chat/completions", "user-key-ok",
		`{"model":"gpt-4o","stream":true,"max_tokens":80,`+
			`"messages":[{"role":"user","content":"你好"}]}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	used := env.quota.usedFor("volc_001", quota.KindToken)
	if used <= 0 {
		t.Errorf("已确认用量 = %d, 拿不到 usage 时必须按预扣量计入而非按 0", used)
	}
	env.quota.assertClean(t)
}

func TestChat_客户端断连后租约必须被回收(t *testing.T) {
	// P0-2: 这是租约模型最关键的场景。断连时 r.Context() 已取消，
	// 若用它调 Release 会立刻失败，租约只能等 TTL 超时 —— 那正是要避免的泄漏。
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{
		Status:     200,
		ChunkDelay: 30 * time.Millisecond,
		SSE: []string{
			`{"choices":[{"index":0,"delta":{"content":"a"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"b"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"c"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"d"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"e"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"f"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"g"}}]}`,
		},
		HoldUntilCancel: true,
	})

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		env.ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"max_tokens":50,`+
			`"messages":[{"role":"user","content":"你好"}]}`))
	req.Header.Set("Authorization", "Bearer user-key-ok")

	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}

	// 读到第一个 chunk 后立即断开
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("读取首 chunk: %v", err)
	}
	cancel()
	resp.Body.Close()

	// 等服务端感知断连并走完 defer
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, r := env.quota.counts(); c+r > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 关键断言: 租约必须已结束，不能悬置等 TTL
	env.quota.assertClean(t)

	// 断连时上游已实际消耗，故走 Commit 而非 Release
	if used := env.quota.usedFor("volc_001", quota.KindToken); used <= 0 {
		t.Error("客户端断连时上游已消耗额度，必须计入而非释放")
	}

	// 客户端主动断开不算 Key 的失败，否则用户频繁取消会误伤健康分
	if f := env.sched.failuresFor("volc_001"); len(f) != 0 {
		t.Errorf("客户端断连被误报为 Key 失败: %v", f)
	}
}

// ===== 重试与换 Key =====

func TestRetry_配额耗尽时换Key(t *testing.T) {
	env := newTestEnv(t)
	// 第一次返回火山的额度耗尽错误（429 + 中文文案），第二次成功
	env.upstream.setScript(
		stubResponse{Status: 429, Body: `{"error":{"code":"QuotaExceeded","message":"该密钥额度已用完"}}`},
		stubResponse{Status: 200, Body: okChatResp},
	)

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望换 Key 后成功 200，响应: %s",
			resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()

	if n := env.upstream.callCount(); n != 2 {
		t.Errorf("上游调用 %d 次, 期望 2（首次失败 + 重试）", n)
	}

	// 必须真的换了 Key，而不是拿同一个 Key 重试
	auths := env.upstream.authHeaders()
	if len(auths) == 2 && auths[0] == auths[1] {
		t.Errorf("两次请求使用了同一个 Key: %s", auths[0])
	}

	// 额度耗尽必须上报 FailureQuota，否则调度器会反复选中同一个坏 Key
	fs := env.sched.failuresFor("volc_001")
	if len(fs) != 1 || fs[0] != FailureQuota {
		t.Errorf("volc_001 的失败上报 = %v, 期望 [quota]", fs)
	}

	// 失败的那次必须 Release（不计用量），成功的那次 Commit
	commits, releases := env.quota.counts()
	if commits != 1 || releases != 1 {
		t.Errorf("Commit/Release = %d/%d, 期望 1/1", commits, releases)
	}
	if used := env.quota.usedFor("volc_001", quota.KindToken); used != 0 {
		t.Errorf("失败的 Key 记入了 %d 用量, 期望 0", used)
	}
	env.quota.assertClean(t)

	recs := env.store.usageRecords()
	if len(recs) != 1 || recs[0].RetryCount != 1 {
		t.Errorf("流水 RetryCount = %v, 期望 1", recs)
	}
}

func TestRetry_限流429冷却并换Key(t *testing.T) {
	env := newTestEnv(t)
	// 无额度耗尽标记的 429 应判定为限流而非额度耗尽 —— 两者处置完全不同:
	// 限流是冷却后可恢复，额度耗尽要等到次日刷新
	env.upstream.setScript(
		stubResponse{Status: 429, Body: `{"error":{"code":"TooManyRequests","message":"request rate limit exceeded"}}`},
		stubResponse{Status: 200, Body: okChatResp},
	)

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", resp.StatusCode)
	}
	resp.Body.Close()

	fs := env.sched.failuresFor("volc_001")
	if len(fs) != 1 || fs[0] != FailureRateLimit {
		t.Errorf("失败分类 = %v, 期望 [rate_limit]", fs)
	}
	env.quota.assertClean(t)
}

func TestRetry_401立即禁用Key(t *testing.T) {
	env := newTestEnv(t)
	env.upstream.setScript(
		stubResponse{Status: 401, Body: `{"error":{"code":"AuthenticationError","message":"invalid api key"}}`},
		stubResponse{Status: 200, Body: okChatResp},
	)

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", resp.StatusCode)
	}
	resp.Body.Close()

	fs := env.sched.failuresFor("volc_001")
	if len(fs) != 1 || fs[0] != FailureAuth {
		t.Errorf("失败分类 = %v, 期望 [auth]", fs)
	}
	env.quota.assertClean(t)
}

func TestRetry_400客户端错误不重试(t *testing.T) {
	// 请求本身有问题，换多少个 Key 都是同样的结果。重试只是白白消耗额度。
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{
		Status: 400,
		Body:   `{"error":{"code":"InvalidParameter","message":"messages 字段格式错误"}}`,
	})

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 期望透传 400", resp.StatusCode)
	}
	resp.Body.Close()

	if n := env.upstream.callCount(); n != 1 {
		t.Errorf("上游调用 %d 次, 客户端错误不应重试", n)
	}
	env.quota.assertClean(t)
}

func TestRetry_耗尽上限后返回错误且租约全部结束(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) { c.Upstream.MaxRetries = 2 })
	env.upstream.setScript(stubResponse{
		Status: 500,
		Body:   `{"error":{"code":"InternalError","message":"upstream boom"}}`,
	})

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	if resp.StatusCode < 500 {
		t.Errorf("状态码 = %d, 期望 5xx", resp.StatusCode)
	}
	resp.Body.Close()

	// MaxRetries=2 意味着总共 3 次尝试
	if n := env.upstream.callCount(); n != 3 {
		t.Errorf("上游调用 %d 次, 期望 3（1 + MaxRetries 2）", n)
	}
	// 3 个租约全部 Release，一个都不能漏
	_, releases := env.quota.counts()
	if releases != 3 {
		t.Errorf("Release 次数 = %d, 期望 3", releases)
	}
	env.quota.assertClean(t)

	recs := env.store.usageRecords()
	if len(recs) != 1 {
		t.Fatalf("流水条数 = %d, 期望 1", len(recs))
	}
	// 失败也要记流水: 排障时需要知道是哪个 Key 在哪个出口上失败的
	if recs[0].UpstreamKeyID == "" || recs[0].ErrorCode == "" {
		t.Errorf("失败流水缺少定位信息: %+v", recs[0])
	}
}

func TestRetry_全部Key不可用返回503而非500(t *testing.T) {
	// 无可用 Key 是容量问题。返回 500 会让告警系统误判为程序缺陷。
	env := newTestEnv(t)
	env.sched.mu.Lock()
	env.sched.selectErr = fmt.Errorf("%w: 池为空", ErrNoCandidate)
	env.sched.mu.Unlock()

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d, 期望 503", resp.StatusCode)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "service_busy") {
		t.Errorf("错误码应为 service_busy: %s", body)
	}
	env.quota.assertClean(t)
}

func TestRetry_退避带随机抖动(t *testing.T) {
	// 反作弊: 纯指数退避（精确的 200/400/800ms）本身就是机器特征。
	env := newTestEnv(t, func(c *config.Config) {
		c.Upstream.RetryBaseDelay = 20 * time.Millisecond
		c.Upstream.RetryJitter = 40 * time.Millisecond
	})

	seen := map[time.Duration]int{}
	for i := 0; i < 40; i++ {
		start := time.Now()
		if !env.srv.sleepBackoff(context.Background(), 1) {
			t.Fatal("退避被意外取消")
		}
		// 按 5ms 粒度归档，抖动范围 40ms 应落到多个桶
		seen[time.Since(start)/(5*time.Millisecond)]++
	}
	if len(seen) < 3 {
		t.Errorf("40 次退避只落在 %d 个时间桶内，抖动未生效", len(seen))
	}
}

// ===== 并发不超刷 =====

func TestConcurrent_并发请求不超过硬水位(t *testing.T) {
	// P0-1 的核心保证。fakeQuota 复刻了 Lua 的 used+prededuct+amount>hard
	// 判定；若网关在 Acquire 之外做了任何准入决策（比如信任调度器的陈旧快照），
	// 这个用例会出现超刷。
	const hard = 3000
	env := newTestEnv(t, func(c *config.Config) {
		c.Quota.TokenLimit = hard
		c.Quota.TokenHardRatio = 1.0
		c.Quota.TokenSoftRatio = 0.9
		c.Quota.DefaultMaxTokens = 100
		c.Quota.EstimateMultiplier = 1.2
		// 关掉重试: 本用例要看的是单次 Acquire 的准入是否严密，
		// 重试会让请求跨 Key 而模糊掉单 Key 的水位判定
		c.Upstream.MaxRetries = 0
	})
	// 只留一个 Key，让所有请求竞争同一份额度
	env.sched.mu.Lock()
	env.sched.keys = env.sched.keys[:1]
	env.sched.mu.Unlock()

	// 实际用量低于预扣量 —— 这是 EstimateMultiplier 存在的常态。
	// 反过来的情况（实际超出预扣）由下一个用例专门覆盖。
	env.upstream.setScript(stubResponse{
		Status: 200,
		Body: `{"id":"c1","model":"ep-test-4o","choices":[],` +
			`"usage":{"prompt_tokens":20,"completion_tokens":40,"total_tokens":60}}`,
	})

	const n = 60
	var wg sync.WaitGroup
	var ok, denied int64
	var mu sync.Mutex

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := env.post(t, "/v1/chat/completions", "user-key-ok",
				`{"model":"gpt-4o","max_tokens":100,`+
					`"messages":[{"role":"user","content":"hi"}]}`)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			mu.Lock()
			if resp.StatusCode == http.StatusOK {
				ok++
			} else {
				denied++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if ok == 0 {
		t.Fatal("全部请求被拒，测试环境有问题")
	}
	if denied == 0 {
		t.Fatalf("60 个请求全部通过，硬水位 %d 未生效", hard)
	}

	used := env.quota.usedFor("volc_001", quota.KindToken)
	if used > hard {
		t.Errorf("已确认用量 %d 超过硬水位 %d —— 发生超刷", used, hard)
	}
	t.Logf("成功 %d 笔，拒绝 %d 笔，最终用量 %d / 硬水位 %d", ok, denied, used, hard)
	env.quota.assertClean(t)
}

func TestConcurrent_预扣阶段的水位约束严格成立(t *testing.T) {
	// 上一个用例断言的是「已确认用量不超硬水位」，那依赖实际用量不高于预扣量。
	// 真正由 Acquire 保证的更强性质是: **任一时刻的 prededuct 之和不超硬水位**。
	//
	// 这个区分很重要。若上游实际用量系统性高于预扣量（估算偏低），used 是会
	// 越过硬水位的 —— 那不是准入失效，而是估算失准，对应的防线是
	// EstimateMultiplier 与 quota_estimate_error 指标，不是 Acquire。
	// 把两者混为一谈会在排障时指向错误的方向。
	const hard = 2000
	env := newTestEnv(t, func(c *config.Config) {
		c.Quota.TokenLimit = hard
		c.Quota.TokenHardRatio = 1.0
		c.Quota.TokenSoftRatio = 0.95
		c.Quota.DefaultMaxTokens = 200
		c.Quota.EstimateMultiplier = 1.0
		c.Upstream.MaxRetries = 0
	})
	env.sched.mu.Lock()
	env.sched.keys = env.sched.keys[:1]
	env.sched.mu.Unlock()

	// 上游挂住不返回，让所有成功 Acquire 的租约同时处于未结算状态 ——
	// 这正是 prededuct 峰值时刻。
	env.upstream.setScript(stubResponse{
		Status: 200, ChunkDelay: 400 * time.Millisecond,
		SSE: []string{`{"choices":[{"delta":{"content":"x"}}]}`, "[DONE]"},
	})

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
				env.ts.URL+"/v1/chat/completions",
				strings.NewReader(`{"model":"gpt-4o","stream":true,"max_tokens":200,`+
					`"messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer user-key-ok")
			if resp, err := env.ts.Client().Do(req); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}

	// 在租约堆积期间反复采样 prededuct，任一次越界即失败
	var peak int64
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		snap, _ := env.quota.Get(context.Background(), "volc", "volc_001", quota.KindToken)
		if snap.Prededuct > peak {
			peak = snap.Prededuct
		}
		if snap.Used+snap.Prededuct > hard {
			t.Fatalf("used(%d) + prededuct(%d) = %d 越过硬水位 %d —— 准入失效",
				snap.Used, snap.Prededuct, snap.Used+snap.Prededuct, hard)
		}
		time.Sleep(5 * time.Millisecond)
	}
	wg.Wait()

	if peak == 0 {
		t.Fatal("采样期间未观测到任何预扣，用例的判定失效")
	}
	t.Logf("预扣峰值 %d / 硬水位 %d", peak, hard)

	time.Sleep(600 * time.Millisecond)
	env.quota.assertClean(t)
}

func TestConcurrent_并发场景下无租约泄漏(t *testing.T) {
	// 混合成功、失败、断连三种结束方式并发跑，验证每条路径都结束了租约。
	env := newTestEnv(t, func(c *config.Config) { c.Upstream.MaxRetries = 1 })

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				env.upstream.setScript(stubResponse{Status: 200, Body: okChatResp})
				resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			case 1:
				env.upstream.setScript(stubResponse{Status: 500, Body: `{"error":{"message":"boom"}}`})
				resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			case 2:
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
					env.ts.URL+"/v1/chat/completions", strings.NewReader(chatBody))
				req.Header.Set("Authorization", "Bearer user-key-ok")
				resp, err := env.ts.Client().Do(req)
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}(i)
	}
	wg.Wait()

	// 给断连路径的 defer 一点时间跑完
	time.Sleep(500 * time.Millisecond)
	env.quota.assertClean(t)
}

// ===== 健康检查 =====

func TestHealthz_不检查依赖(t *testing.T) {
	// 存活检查若依赖 Redis，一次网络抖动就会让编排重启一个本来健康的进程
	env := newTestEnv(t)
	env.store.mu.Lock()
	env.store.pingErr = fmt.Errorf("数据库不可达")
	env.store.mu.Unlock()

	resp, err := env.ts.Client().Get(env.ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码 = %d, 依赖故障时存活检查仍应返回 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestReadyz_依赖故障时返回503(t *testing.T) {
	env := newTestEnv(t)

	resp, err := env.ts.Client().Get(env.ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("依赖正常时 /readyz = %d, 期望 200", resp.StatusCode)
	}
	resp.Body.Close()

	env.store.mu.Lock()
	env.store.pingErr = fmt.Errorf("数据库不可达")
	env.store.mu.Unlock()

	resp, err = env.ts.Client().Get(env.ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("Postgres 故障时 /readyz = %d, 期望 503", resp.StatusCode)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "postgres") {
		t.Errorf("响应应指出故障依赖: %s", body)
	}
}

// ===== 管理接口 =====

func TestAdmin_密钥为空时路由完全不注册(t *testing.T) {
	// 靠中间件返回 403 是更差的做法: 鉴权中间件一旦因重构失效，
	// 管理接口就会裸奔。不注册则连路径都不存在。
	env := newTestEnv(t, func(c *config.Config) { c.Admin.APIKey = "" })

	for _, path := range []string{"/admin/keys", "/admin/ips", "/admin/users"} {
		req, _ := http.NewRequest(http.MethodGet, env.ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer anything")
		resp, err := env.ts.Client().Do(req)
		if err != nil {
			t.Fatalf("请求 %s 失败: %v", path, err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s 状态码 = %d, 未配置管理密钥时应为 404（路由不存在）",
				path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestAdmin_错误密钥返回401(t *testing.T) {
	env := newTestEnv(t)
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer wrong-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("状态码 = %d, 期望 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdmin_用户密钥不能访问管理接口(t *testing.T) {
	env := newTestEnv(t)
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer user-key-ok")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("状态码 = %d, 用户密钥不得访问管理接口", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdmin_列出Key状态(t *testing.T) {
	env := newTestEnv(t)
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body := readAll(t, resp)

	var out struct {
		Total int `json:"total"`
		Keys  []struct {
			KeyID  string `json:"key_id"`
			Secret string `json:"secret"`
		} `json:"keys"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应非合法 JSON: %v", err)
	}
	if out.Total != 3 {
		t.Errorf("total = %d, 期望 3", out.Total)
	}
	// 管理接口绝不能返回明文密钥
	for _, k := range out.Keys {
		if k.Secret != "" {
			t.Errorf("Key %s 泄漏了明文密钥", k.KeyID)
		}
	}
	if strings.Contains(body, "sk-mock-") {
		t.Errorf("响应体包含密钥明文: %s", body)
	}
}

func TestAdmin_创建用户回显存储层填的默认限额(t *testing.T) {
	env := newTestEnv(t)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/users",
		strings.NewReader(`{"name":"alice"}`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req.Header.Set("X-Admin-Actor", "ops-bob")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("状态码 = %d, 期望 201，响应: %s", resp.StatusCode, readAll(t, resp))
	}
	var out map[string]any
	json.Unmarshal([]byte(readAll(t, resp)), &out)

	// fakeStore 会把未指定的 RPM 填为 60。回显请求值会让调用方
	// 以为「不限」而实际上有默认上限。
	if out["rpm_limit"] != float64(60) {
		t.Errorf("rpm_limit = %v, 期望回显存储层填的默认值 60", out["rpm_limit"])
	}

	audits := env.store.auditRecords()
	if len(audits) != 1 {
		t.Fatalf("审计条数 = %d, 期望 1", len(audits))
	}
	if audits[0].Actor != "ops-bob" {
		t.Errorf("审计 Actor = %q, 期望取 X-Admin-Actor", audits[0].Actor)
	}
	if audits[0].Action != "create_user" {
		t.Errorf("审计 Action = %q", audits[0].Action)
	}
}

func TestAdmin_签发用户Key审计只记前缀不记明文(t *testing.T) {
	env := newTestEnv(t)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/users/7/keys",
		strings.NewReader(`{"name":"prod"}`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("状态码 = %d, 期望 201: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "fk-secret-plaintext") {
		t.Error("响应应包含明文密钥（仅此一次）")
	}

	audits := env.store.auditRecords()
	if len(audits) != 1 {
		t.Fatalf("审计条数 = %d", len(audits))
	}
	// 审计表被读取的门槛远低于密钥表，绝不能记明文
	detail := fmt.Sprint(audits[0].Detail)
	if strings.Contains(detail, "fk-secret-plaintext") {
		t.Errorf("审计日志记录了密钥明文: %s", detail)
	}
	if audits[0].Detail["key_prefix"] != "fk-secre" {
		t.Errorf("审计未记录密钥前缀: %v", audits[0].Detail)
	}
}

func TestAdmin_吊销用户Key后鉴权缓存立即失效(t *testing.T) {
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{Status: 200, Body: okChatResp})

	// 1. 先用该 Key 成功请求一次，把鉴权结果灌进缓存
	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("预热请求状态码 = %d", resp.StatusCode)
	}

	// 2. 吊销（默认用户 UserID=7, APIKeyID=70）
	req, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/admin/users/7/keys/70", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	dresp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("吊销请求失败: %v", err)
	}
	body := readAll(t, dresp)
	if dresp.StatusCode != http.StatusOK {
		t.Fatalf("吊销状态码 = %d: %s", dresp.StatusCode, body)
	}

	// 3. 吊销必须立即生效 —— 不能等缓存 TTL 过期。
	// fakeStore 已删掉该用户，若缓存未被清空，这里会拿到缓存的旧结果 200。
	resp = env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("吊销后请求状态码 = %d, 期望 401（缓存未随吊销失效）", resp.StatusCode)
	}

	// 4. 审计已记录
	audits := env.store.auditRecords()
	found := false
	for _, a := range audits {
		if a.Action == "revoke_user_api_key" {
			found = true
		}
	}
	if !found {
		t.Error("吊销操作未写审计日志")
	}
}

func TestAdmin_吊销不存在的Key返回404(t *testing.T) {
	env := newTestEnv(t)

	// key_id 不存在
	req, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/admin/users/7/keys/999", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("状态码 = %d, 期望 404", resp.StatusCode)
	}

	// key 存在但归属不符（真实 user_id 是 7）: 同样 404，不能吊掉别人的 Key
	req, _ = http.NewRequest(http.MethodDelete, env.ts.URL+"/admin/users/8/keys/70", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err = env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("跨用户吊销状态码 = %d, 期望 404", resp.StatusCode)
	}

	// 归属不符的吊销不得产生任何效果: 原 Key 仍可用
	env.upstream.setScript(stubResponse{Status: 200, Body: okChatResp})
	ok := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Errorf("跨用户吊销后原 Key 状态码 = %d, 应仍为 200", ok.StatusCode)
	}
}

// ===== 优雅关闭 =====

func TestShutdown_关闭期间拒绝新请求但等待进行中的流式(t *testing.T) {
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{
		Status:     200,
		ChunkDelay: 50 * time.Millisecond,
		SSE: []string{
			`{"choices":[{"index":0,"delta":{"content":"a"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"b"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"c"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"d"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":4,"total_tokens":9}}`,
			"[DONE]",
		},
	})

	streamDone := make(chan int, 1)
	go func() {
		resp := env.post(t, "/v1/chat/completions", "user-key-ok",
			`{"model":"gpt-4o","stream":true,"max_tokens":20,`+
				`"messages":[{"role":"user","content":"hi"}]}`)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		streamDone <- strings.Count(string(b), "data:")
	}()

	// 等流式确实开始
	time.Sleep(80 * time.Millisecond)

	// 置位关闭标记（不真正 Shutdown httptest server，那会切断连接）
	env.srv.gate.close()

	// 新请求被拒
	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("关闭期间新请求状态码 = %d, 期望 503", resp.StatusCode)
	}
	resp.Body.Close()

	// /readyz 报不就绪，让负载均衡摘流量
	rz, _ := env.ts.Client().Get(env.ts.URL + "/readyz")
	if rz.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("关闭期间 /readyz = %d, 期望 503", rz.StatusCode)
	}
	rz.Body.Close()

	// 进行中的流式必须跑完
	select {
	case n := <-streamDone:
		if n < 6 {
			t.Errorf("进行中的流式收到 %d 个 chunk, 期望 6（不应被关闭打断）", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("进行中的流式请求未能完成")
	}
	env.quota.assertClean(t)
}

func TestShutdown_等待inFlight归零(t *testing.T) {
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{
		Status: 200, ChunkDelay: 40 * time.Millisecond,
		SSE: []string{`{"choices":[{"delta":{"content":"a"}}]}`,
			`{"choices":[{"delta":{"content":"b"}}]}`, "[DONE]"},
	})

	go func() {
		resp := env.post(t, "/v1/chat/completions", "user-key-ok",
			`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	time.Sleep(60 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := env.srv.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown 返回错误: %v", err)
	}
	// Shutdown 必须等到流式结束，不能立刻返回
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("Shutdown 仅耗时 %v，未等待进行中的流式请求", elapsed)
	}
	env.quota.assertClean(t)
}

// ===== 请求体限长 =====

func TestBody_超过上限返回400(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) { c.Server.MaxBodyBytes = 512 })
	big := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`,
		strings.Repeat("x", 2000))

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", big)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 期望 400", resp.StatusCode)
	}
	resp.Body.Close()
	if env.upstream.callCount() != 0 {
		t.Error("超长请求体不应触达上游")
	}
	env.quota.assertClean(t)
}

func TestChat_缺少model字段返回400(t *testing.T) {
	env := newTestEnv(t)
	resp := env.post(t, "/v1/chat/completions", "user-key-ok",
		`{"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 期望 400", resp.StatusCode)
	}
	resp.Body.Close()
	env.quota.assertClean(t)
}

func TestChat_GET方法返回405(t *testing.T) {
	env := newTestEnv(t)
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer user-key-ok")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("状态码 = %d, 期望 405", resp.StatusCode)
	}
	resp.Body.Close()
}

// ===== 次数型计费 =====

func TestImages_按次计费(t *testing.T) {
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{
		Status: 200,
		Body:   `{"model":"seedream-3.0","data":[{"url":"https://x/1.png"},{"url":"https://x/2.png"}]}`,
	})

	resp := env.post(t, "/v1/images/generations", "user-key-ok",
		`{"model":"seedream-3.0","prompt":"一只猫","n":2}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200: %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()

	// 次数型走独立的 count 配额，不能记到 token 上
	if c := env.quota.usedFor("volc_001", quota.KindCount); c != 2 {
		t.Errorf("count 用量 = %d, 期望 2（n=2）", c)
	}
	if tk := env.quota.usedFor("volc_001", quota.KindToken); tk != 0 {
		t.Errorf("次数型请求错误地消耗了 %d token 配额", tk)
	}
	env.quota.assertClean(t)

	recs := env.store.usageRecords()
	if len(recs) != 1 || recs[0].BillingKind != string(quota.KindCount) {
		t.Errorf("流水计费类型 = %v, 期望 count", recs)
	}
	// 落库的计费量必须与实扣量一致，否则对账时账面凭空少掉一部分用量。
	if recs[0].CountUnits != 2 {
		t.Errorf("流水 count_units = %d, 期望 2（与实扣量一致）", recs[0].CountUnits)
	}
	if recs[0].Provider != "volc" {
		t.Errorf("流水 provider = %q, 期望 volc（本次实际路由的上游）", recs[0].Provider)
	}
}

// 上游不回 usage 的按次计费请求（chat 端点即如此）仍要按实扣量落库。
// 这是真实环境暴露过的缺陷: Redis 实扣 1，流水却记 0。
func TestCount_上游不回usage时流水仍记实扣量(t *testing.T) {
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{
		Status: 200,
		Body:   `{"model":"seedream-3.0","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`,
	})

	resp := env.post(t, "/v1/chat/completions", "user-key-ok",
		`{"model":"seedream-3.0","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()

	used := env.quota.usedFor("volc_001", quota.KindCount)
	if used == 0 {
		t.Fatalf("count 配额未扣减，用例前提不成立")
	}
	recs := env.store.usageRecords()
	if len(recs) != 1 {
		t.Fatalf("流水条数 = %d, 期望 1", len(recs))
	}
	if got := recs[0].CountUnits; got != used {
		t.Errorf("流水 count_units = %d, 实扣 = %d —— 账面与配额不一致", got, used)
	}
}

func TestImages_不走流式(t *testing.T) {
	// images/embeddings 端点即便传 stream:true 也必须按非流式处理，
	// 否则会按 SSE 解析一个 JSON 响应而失败
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{
		Status: 200, Body: `{"model":"seedream-3.0","data":[{"url":"https://x/1.png"}]}`,
	})
	resp := env.post(t, "/v1/images/generations", "user-key-ok",
		`{"model":"seedream-3.0","prompt":"猫","stream":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "event-stream") {
		t.Errorf("images 端点返回了 SSE: %s", ct)
	}
	resp.Body.Close()

	recs := env.store.usageRecords()
	if len(recs) != 1 || recs[0].IsStream {
		t.Error("images 请求被错误标记为流式")
	}
	env.quota.assertClean(t)
}

func TestEmbeddings_按Token计费(t *testing.T) {
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{
		Status: 200,
		Body: `{"object":"list","model":"ep-test-4o","data":[{"embedding":[0.1,0.2]}],` +
			`"usage":{"prompt_tokens":8,"total_tokens":8}}`,
	})
	resp := env.post(t, "/v1/embeddings", "user-key-ok",
		`{"model":"gpt-4o","input":"你好世界"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()

	if used := env.quota.usedFor("volc_001", quota.KindToken); used != 8 {
		t.Errorf("token 用量 = %d, 期望 8", used)
	}
	env.quota.assertClean(t)
}

// ===== Key 导入 =====

func TestAdmin_导入Key支持单个与批量(t *testing.T) {
	env := newTestEnv(t)

	// 数组形式
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/keys",
		strings.NewReader(`[{"key_id":"volc_100","secret":"sk-a","pool":"main"},`+
			`{"key_id":"volc_101","secret":"sk-b","pool":"warm"}]`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", resp.StatusCode, body)
	}
	var out map[string]any
	json.Unmarshal([]byte(body), &out)
	if out["created_count"] != float64(2) {
		t.Errorf("created_count = %v, 期望 2", out["created_count"])
	}

	// 单个对象形式
	req2, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/keys",
		strings.NewReader(`{"key_id":"volc_102","secret":"sk-c"}`))
	req2.Header.Set("Authorization", "Bearer admin-secret")
	resp2, _ := env.ts.Client().Do(req2)
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("单个对象形式状态码 = %d", resp2.StatusCode)
	}
	resp2.Body.Close()

	if n := env.store.volcKeyCount(); n != 3 {
		t.Errorf("已导入 %d 个 Key, 期望 3", n)
	}
}

func TestAdmin_导入Key是幂等的(t *testing.T) {
	// 运维用同一份清单反复执行必须安全，否则每次导入都要先人工比对差异
	env := newTestEnv(t)
	payload := `[{"key_id":"volc_200","secret":"sk-x","pool":"main"}]`

	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/keys",
			strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer admin-secret")
		resp, err := env.ts.Client().Do(req)
		if err != nil {
			t.Fatalf("第 %d 次请求失败: %v", i+1, err)
		}
		body := readAll(t, resp)
		var out map[string]any
		json.Unmarshal([]byte(body), &out)

		if i == 0 && out["created_count"] != float64(1) {
			t.Errorf("首次导入 created_count = %v, 期望 1", out["created_count"])
		}
		// 第二次必须报告为 updated 而非 created —— 这是运维判断
		// 「清单是否真的新增了东西」的唯一反馈
		if i == 1 && out["created_count"] != float64(0) {
			t.Errorf("重复导入 created_count = %v, 期望 0", out["created_count"])
		}
		if i == 1 && out["updated_count"] != float64(1) {
			t.Errorf("重复导入 updated_count = %v, 期望 1", out["updated_count"])
		}
	}
	if n := env.store.volcKeyCount(); n != 1 {
		t.Errorf("重复导入产生了 %d 条记录, 期望 1", n)
	}
}

func TestAdmin_导入Key留空secret保留原密文(t *testing.T) {
	// 让运维能用同一份清单只改 pool 而不接触密钥
	env := newTestEnv(t)

	post := func(payload string) {
		req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/keys",
			strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer admin-secret")
		resp, err := env.ts.Client().Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		resp.Body.Close()
	}

	post(`{"key_id":"volc_300","secret":"sk-original","pool":"main"}`)
	post(`{"key_id":"volc_300","pool":"cold"}`)

	k, ok := env.store.volcKey("volc_300")
	if !ok {
		t.Fatal("Key 不存在")
	}
	if k.Secret != "sk-original" {
		t.Errorf("secret = %q, 留空时应保留原密文", k.Secret)
	}
	if k.Pool != "cold" {
		t.Errorf("pool = %q, 期望更新为 cold", k.Pool)
	}
}

func TestAdmin_导入Key单条失败不中断整批(t *testing.T) {
	// 1000 个 Key 因第 3 个格式错误而全部回滚，运维只能反复试错
	env := newTestEnv(t)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/keys",
		strings.NewReader(`[{"key_id":"volc_400","secret":"a"},`+
			`{"key_id":"","secret":"b"},`+
			`{"key_id":"volc_401","secret":"c"},`+
			`{"key_id":"volc_400","secret":"dup"}]`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body := readAll(t, resp)

	var out struct {
		ImportedCount int `json:"imported_count"`
		FailedCount   int `json:"failed_count"`
		Failures      []struct {
			KeyID  string `json:"key_id"`
			Reason string `json:"reason"`
		} `json:"failures"`
	}
	json.Unmarshal([]byte(body), &out)

	if out.ImportedCount != 2 {
		t.Errorf("imported_count = %d, 期望 2（合法的两条）", out.ImportedCount)
	}
	// 空 key_id 与批内重复各算一条失败
	if out.FailedCount != 2 {
		t.Errorf("failed_count = %d, 期望 2: %+v", out.FailedCount, out.Failures)
	}
	// 逐条报告原因，让一次调用就能修完
	for _, f := range out.Failures {
		if f.Reason == "" {
			t.Error("失败项缺少原因说明")
		}
	}
}

func TestAdmin_导入Key全部失败返回400(t *testing.T) {
	// CI 脚本需要据状态码判断成败，全失败返回 200 会让流水线误判成功
	env := newTestEnv(t)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/keys",
		strings.NewReader(`[{"key_id":"","secret":"a"}]`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 全部失败时期望 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdmin_导入Key审计不记密钥明文(t *testing.T) {
	env := newTestEnv(t)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/keys",
		strings.NewReader(`{"key_id":"volc_500","secret":"sk-super-secret-value"}`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body := readAll(t, resp)

	if strings.Contains(body, "sk-super-secret-value") {
		t.Errorf("响应体回显了密钥明文: %s", body)
	}
	audits := env.store.auditRecords()
	if len(audits) != 1 {
		t.Fatalf("审计条数 = %d", len(audits))
	}
	if d := fmt.Sprint(audits[0].Detail); strings.Contains(d, "sk-super-secret-value") {
		t.Errorf("审计日志记录了密钥明文: %s", d)
	}
}

func TestAdmin_导入Key非法请求体返回400(t *testing.T) {
	env := newTestEnv(t)
	for _, payload := range []string{`not json`, `[]`, `[{"key_id":`} {
		req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/keys",
			strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer admin-secret")
		resp, err := env.ts.Client().Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("payload %q 状态码 = %d, 期望 400", payload, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestAdmin_不支持的方法返回405(t *testing.T) {
	env := newTestEnv(t)
	req, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("状态码 = %d, 期望 405", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdmin_导入Key后立即重载Key池(t *testing.T) {
	// 不重载则新 Key 要等后台 key_reload（5 分钟一轮）才生效，
	// 期间网关对所有请求返回 503 —— 运维会合理地认为导入失败。
	env := newTestEnv(t)
	before := env.sched.reloadCount()

	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/keys",
		strings.NewReader(`{"key_id":"volc_600","secret":"sk-y"}`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()

	if got := env.sched.reloadCount(); got != before+1 {
		t.Errorf("Reload 调用次数 = %d, 期望 %d（导入后应立即重载）", got, before+1)
	}
}

func TestAdmin_全部导入失败时不触发重载(t *testing.T) {
	// 没有任何 Key 落库，重载纯属浪费一次全量查库
	env := newTestEnv(t)
	before := env.sched.reloadCount()

	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/keys",
		strings.NewReader(`[{"key_id":"","secret":"a"}]`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()

	if got := env.sched.reloadCount(); got != before {
		t.Errorf("Reload 被调用了 %d 次, 全部失败时不应重载", got-before)
	}
}

func TestAdmin_重载失败不影响导入返回成功(t *testing.T) {
	// Key 已落库，此时返回错误会让调用方误以为导入失败而重试。
	// 后台 key_reload 最终会捡到这批 Key。
	env := newTestEnv(t)
	env.sched.mu.Lock()
	env.sched.reloadErr = fmt.Errorf("数据库连接中断")
	env.sched.mu.Unlock()

	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/keys",
		strings.NewReader(`{"key_id":"volc_700","secret":"sk-z"}`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码 = %d, 重载失败不应影响导入结果: %s", resp.StatusCode, body)
	}
	if n := env.store.volcKeyCount(); n != 1 {
		t.Errorf("Key 未落库: %d", n)
	}
}

// ===== 用户级限流（P1-9）=====

// fakeLimiter 是可编程的限流器替身。
//
// 真实实现走 Redis Lua，无法在无依赖环境里跑；而网关侧真正需要验证的是
// 「拒绝时返回什么」「fail-open 时是否放行」这些分支，与 Lua 无关。
type fakeLimiter struct {
	mu sync.Mutex
	// rpm / tpm 是预设的判定结果。
	rpm, tpm Result
	// rpmErr / tpmErr 模拟 Redis 故障。
	rpmErr, tpmErr error
	// refunds 记录归还调用。
	refunds []int64
	// tokenCalls 记录 AllowTokens 收到的预估量。
	tokenCalls []int64
}

func newFakeLimiter() *fakeLimiter {
	return &fakeLimiter{
		rpm: Result{Allowed: true, Remaining: 10, Dimension: "rpm"},
		tpm: Result{Allowed: true, Remaining: 1000, Dimension: "tpm"},
	}
}

func (f *fakeLimiter) AllowRequest(ctx context.Context, userID int64, rpmLimit int) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rpm, f.rpmErr
}

func (f *fakeLimiter) AllowTokens(ctx context.Context, userID int64, tpmLimit, tokens int64) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenCalls = append(f.tokenCalls, tokens)
	return f.tpm, f.tpmErr
}

func (f *fakeLimiter) RefundTokens(ctx context.Context, userID int64, tpmLimit, refund int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refunds = append(f.refunds, refund)
	return nil
}

func (f *fakeLimiter) tokenAmounts() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.tokenCalls...)
}

// withLimiter 把限流器挂到已构造好的环境上。
func (e *testEnv) withLimiter(l Limiter) { e.srv.limiter = l }

func TestRateLimit_RPM超限返回429并带RetryAfter(t *testing.T) {
	env := newTestEnv(t)
	lim := newFakeLimiter()
	lim.rpm = Result{Allowed: false, RetryAfter: 3 * time.Second, Dimension: "rpm"}
	env.withLimiter(lim)

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d, 期望 429", resp.StatusCode)
	}
	// 没有 Retry-After，客户端只能盲目立即重试，反而加剧拥塞
	if got := resp.Header.Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After = %q, 期望 3", got)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "rate_limit_error") {
		t.Errorf("错误类型应为 rate_limit_error: %s", body)
	}

	// 被限流的请求不得触达上游，也不得占用任何配额
	if env.upstream.callCount() != 0 {
		t.Error("被限流的请求仍调用了上游")
	}
	env.quota.assertClean(t)
}

func TestRateLimit_TPM超限返回429(t *testing.T) {
	env := newTestEnv(t)
	lim := newFakeLimiter()
	lim.tpm = Result{Allowed: false, RetryAfter: 500 * time.Millisecond, Dimension: "tpm"}
	env.withLimiter(lim)

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d, 期望 429", resp.StatusCode)
	}
	// 不足 1 秒的等待也要上取整到 1 —— Retry-After 头只接受整数秒，
	// 取 0 等于告诉客户端「立刻重试」，会形成忙等
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, 亚秒级等待应上取整到 1", got)
	}
	env.quota.assertClean(t)
}

func TestRateLimit_TPM按预估量扣减(t *testing.T) {
	env := newTestEnv(t)
	lim := newFakeLimiter()
	env.withLimiter(lim)
	env.upstream.setScript(stubResponse{Status: 200, Body: okChatResp})

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	resp.Body.Close()

	amounts := lim.tokenAmounts()
	if len(amounts) != 1 {
		t.Fatalf("AllowTokens 调用 %d 次, 期望 1", len(amounts))
	}
	// 传入的必须是预估量而非 0 或 max_tokens 原值 ——
	// 真实用量只有请求结束后才知道，限流必须在请求前判定
	if amounts[0] <= 0 {
		t.Errorf("TPM 扣减量 = %d, 必须为正的预估量", amounts[0])
	}
	env.quota.assertClean(t)
}

func TestRateLimit_请求结束后归还TPM差额(t *testing.T) {
	// 预扣按 (估算 + max_tokens) × 放大系数，通常远大于实际用量。
	// 不归还的话 TPM 被系统性高估，用户在远低于名义限额时就会被 429。
	env := newTestEnv(t)
	lim := newFakeLimiter()
	env.withLimiter(lim)
	env.store.addUser("user-tpm", UserContext{
		UserID: 8, APIKeyID: 80, Name: "tpm-user", TPMLimit: 1_000_000,
	})
	env.upstream.setScript(stubResponse{Status: 200, Body: okChatResp})

	resp := env.post(t, "/v1/chat/completions", "user-tpm", chatBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}

	amounts := lim.tokenAmounts()
	if len(amounts) != 1 {
		t.Fatalf("AllowTokens 调用 %d 次, 期望 1", len(amounts))
	}
	lim.mu.Lock()
	refunds := append([]int64(nil), lim.refunds...)
	lim.mu.Unlock()
	if len(refunds) != 1 {
		t.Fatalf("RefundTokens 调用 %d 次, 期望 1", len(refunds))
	}
	// okChatResp 的实际用量远小于预扣量，归还量必须为正且小于预扣量
	if refunds[0] <= 0 || refunds[0] >= amounts[0] {
		t.Errorf("归还量 = %d, 应在 (0, %d) 内", refunds[0], amounts[0])
	}
}

func TestRateLimit_失败请求全额归还TPM(t *testing.T) {
	// 上游全部失败时没有任何真实消耗，预扣的令牌必须原数还回，
	// 否则重试风暴会把用户的 TPM 白白吃光。
	env := newTestEnv(t)
	lim := newFakeLimiter()
	env.withLimiter(lim)
	env.store.addUser("user-tpm2", UserContext{
		UserID: 9, APIKeyID: 90, Name: "tpm-user2", TPMLimit: 1_000_000,
	})
	env.upstream.setScript(stubResponse{Status: 500, Body: `{"error":{"message":"boom"}}`})

	resp := env.post(t, "/v1/chat/completions", "user-tpm2", chatBody)
	resp.Body.Close()

	amounts := lim.tokenAmounts()
	lim.mu.Lock()
	refunds := append([]int64(nil), lim.refunds...)
	lim.mu.Unlock()
	if len(amounts) != 1 || len(refunds) != 1 {
		t.Fatalf("allow=%d refund=%d, 均期望 1 次", len(amounts), len(refunds))
	}
	if refunds[0] != amounts[0] {
		t.Errorf("失败请求应全额归还: 预扣 %d, 归还 %d", amounts[0], refunds[0])
	}
}

func TestRateLimit_Redis故障时放行(t *testing.T) {
	// fail-open: 限流是用量保护而非安全边界。Redis 抖动时拒绝全部请求会把
	// 一次依赖故障放大成完全不可用。真正的超刷防线是配额层的 Lua 预扣。
	env := newTestEnv(t)
	lim := newFakeLimiter()
	lim.rpmErr = fmt.Errorf("redis: connection refused")
	lim.tpmErr = fmt.Errorf("redis: connection refused")
	// 限流器在故障时返回放行（与真实实现的 failOpen 行为一致）
	env.withLimiter(lim)
	env.upstream.setScript(stubResponse{Status: 200, Body: okChatResp})

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码 = %d, 限流器故障时应放行", resp.StatusCode)
	}
	resp.Body.Close()
	env.quota.assertClean(t)
}

func TestRateLimit_次数型请求不消耗TPM(t *testing.T) {
	// 图片生成按次计费，与 Token 无关。占用 TPM 会让用户的文本额度
	// 被图片请求无谓地吃掉。
	env := newTestEnv(t)
	lim := newFakeLimiter()
	env.withLimiter(lim)
	env.upstream.setScript(stubResponse{
		Status: 200, Body: `{"model":"seedream-3.0","data":[{"url":"https://x/1.png"}]}`,
	})

	resp := env.post(t, "/v1/images/generations", "user-key-ok",
		`{"model":"seedream-3.0","prompt":"猫","n":1}`)
	resp.Body.Close()

	if n := len(lim.tokenAmounts()); n != 0 {
		t.Errorf("次数型请求调用了 %d 次 AllowTokens, 期望 0", n)
	}
	env.quota.assertClean(t)
}

func TestRateLimit_限流器为nil时不影响请求(t *testing.T) {
	env := newTestEnv(t)
	env.withLimiter(nil)
	env.upstream.setScript(stubResponse{Status: 200, Body: okChatResp})

	resp := env.post(t, "/v1/chat/completions", "user-key-ok", chatBody)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码 = %d, 未配置限流器时应正常放行", resp.StatusCode)
	}
	resp.Body.Close()
	env.quota.assertClean(t)
}

func TestRateLimit_类型化nil指针不导致panic(t *testing.T) {
	// (*RateLimiter)(nil) 装进接口后接口非 nil，若不归一化会在调用时 panic
	var typed *RateLimiter
	pool, err := egress.NewPool(egress.ModeDirect, nil, time.Second)
	if err != nil {
		t.Fatalf("构造出口池: %v", err)
	}
	cfg := config.Default()
	volcCfg := cfg.Providers["volc"]
	volcCfg.ModelMapping = map[string]string{"gpt-4o": "ep"}
	cfg.Providers["volc"] = volcCfg
	srv, err := New(Deps{
		Config: cfg, Quota: newFakeQuota(1000), Egress: pool,
		Sched: newFakeSched("k1"), Store: newFakeStore(),
		Limiter: typed,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("构造网关: %v", err)
	}
	if srv.limiter != nil {
		t.Error("类型化 nil 指针未被归一化为 nil，后续调用会 panic")
	}
}

// ===== 限流器自身的无 Redis 可测部分 =====

func TestRateLimiter_限额为零表示不限(t *testing.T) {
	// rdb 为 nil 也不能 panic: 不限的分支必须在触达 Redis 之前返回
	l := &RateLimiter{now: time.Now, failOpen: true}
	ctx := context.Background()

	res, err := l.AllowRequest(ctx, 1, 0)
	if err != nil || !res.Allowed {
		t.Errorf("RPM 限额为 0 时应放行: allowed=%v err=%v", res.Allowed, err)
	}
	if res.Remaining != -1 {
		t.Errorf("Remaining = %d, 不限时应为 -1", res.Remaining)
	}

	res, err = l.AllowTokens(ctx, 1, 0, 500)
	if err != nil || !res.Allowed {
		t.Errorf("TPM 限额为 0 时应放行: allowed=%v err=%v", res.Allowed, err)
	}

	if err := l.RefundTokens(ctx, 1, 0, 100); err != nil {
		t.Errorf("不限时归还应为空操作: %v", err)
	}
	if err := l.RefundTokens(ctx, 1, 1000, 0); err != nil {
		t.Errorf("归还量为 0 时应为空操作: %v", err)
	}
}

// ===== 出口 IP 管理 =====

// newMultiIPEnv 构造一个 multi_ip 模式的环境。
//
// 用回环地址做辅助出口: 它们在任何机器上都可绑定，无需依赖真实的
// 弹性网卡配置，而验证「绑定关系是否按 key_id 确定性分配」并不需要
// 真的从不同 IP 发包。
func newMultiIPEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t)

	// 取 127.0.0.0/8 内的不同地址: 出口池按 Addr 去重，用同一个地址会被拒；
	// 而整个回环网段在 macOS 与 Linux 上都默认路由到本机，无需额外配置。
	ips := []*egress.IP{
		egress.NewIP("127.0.0.1", "203.0.113.1", 10),
		egress.NewIP("127.0.0.2", "203.0.113.2", 10),
	}
	pool, err := egress.NewPool(egress.ModeMultiIP, ips, 5*time.Second)
	if err != nil {
		t.Fatalf("构造 multi_ip 出口池: %v", err)
	}
	env.srv.egress = pool
	return env
}

func TestAdmin_列出出口IP(t *testing.T) {
	env := newMultiIPEnv(t)
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/admin/ips", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", resp.StatusCode, body)
	}

	var out struct {
		Mode   string `json:"mode"`
		Total  int    `json:"total"`
		Active int    `json:"active"`
		PerIP  []struct {
			Addr       string `json:"addr"`
			State      string `json:"state"`
			Reputation int    `json:"reputation"`
		} `json:"per_ip"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应非合法 JSON: %v\n%s", err, body)
	}
	if out.Mode != "multi_ip" {
		t.Errorf("mode = %q, 期望 multi_ip", out.Mode)
	}
	if out.Total != 2 || len(out.PerIP) != 2 {
		t.Errorf("出口数 = %d/%d, 期望 2", out.Total, len(out.PerIP))
	}
	// 信誉分是判断 IP 是否接近被封的唯一依据，必须暴露
	for _, ip := range out.PerIP {
		if ip.State == "" {
			t.Errorf("出口 %s 缺少状态字段", ip.Addr)
		}
	}
}

func TestAdmin_出口IP接口只接受GET(t *testing.T) {
	env := newMultiIPEnv(t)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/ips",
		strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("状态码 = %d, 期望 405", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdmin_重绑定KeyIP并写审计(t *testing.T) {
	env := newMultiIPEnv(t)

	// 先建立初始绑定
	oldIP, err := env.srv.egress.Bind("volc_001")
	if err != nil {
		t.Fatalf("初始绑定失败: %v", err)
	}
	if oldIP == "" {
		t.Fatal("初始绑定返回空地址")
	}

	req, _ := http.NewRequest(http.MethodPut,
		env.ts.URL+"/admin/keys/volc_001/ip",
		strings.NewReader(`{"egress_ip":"203.0.113.2","mode":"manual"}`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req.Header.Set("X-Admin-Actor", "ops-carol")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", resp.StatusCode, body)
	}

	var out struct {
		KeyID        string `json:"key_id"`
		OldIP        string `json:"old_ip"`
		NewIP        string `json:"new_ip"`
		SwitchStatus string `json:"switch_status"`
	}
	json.Unmarshal([]byte(body), &out)
	if out.KeyID != "volc_001" {
		t.Errorf("key_id = %q", out.KeyID)
	}
	if out.OldIP != oldIP {
		t.Errorf("old_ip = %q, 期望 %q", out.OldIP, oldIP)
	}
	if out.NewIP == "" {
		t.Error("new_ip 为空，未完成重绑定")
	}

	// 审计是多 IP 环境下定位封禁根因的唯一线索:
	// 「这个 Key 的出口 IP 是什么时候被谁改的」
	audits := env.store.auditRecords()
	if len(audits) != 1 {
		t.Fatalf("审计条数 = %d, 期望 1", len(audits))
	}
	a := audits[0]
	if a.Action != "rebind_key_ip" {
		t.Errorf("审计 Action = %q", a.Action)
	}
	if a.Actor != "ops-carol" {
		t.Errorf("审计 Actor = %q, 期望取 X-Admin-Actor", a.Actor)
	}
	if a.Target != "volc_001" {
		t.Errorf("审计 Target = %q", a.Target)
	}
	// 必须同时记录改动前后，只记新值无法回答「原来是哪个 IP」
	if a.Detail["old_ip"] != oldIP {
		t.Errorf("审计缺少变更前的 IP: %v", a.Detail)
	}
	if a.Detail["new_ip"] == nil {
		t.Errorf("审计缺少变更后的 IP: %v", a.Detail)
	}
}

func TestAdmin_重绑定路径格式错误返回404(t *testing.T) {
	env := newMultiIPEnv(t)
	for _, path := range []string{
		"/admin/keys/volc_001",       // 缺少 /ip 后缀
		"/admin/keys/volc_001/other", // 后缀不对
		"/admin/keys//ip",            // key_id 为空
		"/admin/keys/a/b/c",          // 段数过多
	} {
		req, _ := http.NewRequest(http.MethodPut, env.ts.URL+path,
			strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer admin-secret")
		resp, err := env.ts.Client().Do(req)
		if err != nil {
			t.Fatalf("请求 %s 失败: %v", path, err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s 状态码 = %d, 期望 404", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestAdmin_重绑定只接受PUT(t *testing.T) {
	env := newMultiIPEnv(t)
	req, _ := http.NewRequest(http.MethodGet,
		env.ts.URL+"/admin/keys/volc_001/ip", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("状态码 = %d, 期望 405", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdmin_重绑定请求体非法返回400(t *testing.T) {
	env := newMultiIPEnv(t)
	req, _ := http.NewRequest(http.MethodPut,
		env.ts.URL+"/admin/keys/volc_001/ip", strings.NewReader(`not json`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 期望 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdmin_创建用户名称为空返回400(t *testing.T) {
	env := newTestEnv(t)
	for _, payload := range []string{`{}`, `{"name":""}`, `{"name":"   "}`} {
		req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/users",
			strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer admin-secret")
		resp, err := env.ts.Client().Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("payload %q 状态码 = %d, 期望 400", payload, resp.StatusCode)
		}
		resp.Body.Close()
	}
	// 校验失败不应留下审计记录 —— 什么都没发生
	if n := len(env.store.auditRecords()); n != 0 {
		t.Errorf("校验失败仍写了 %d 条审计", n)
	}
}

func TestAdmin_签发Key用户ID非法返回400(t *testing.T) {
	env := newTestEnv(t)
	for _, path := range []string{
		"/admin/users/abc/keys",
		"/admin/users/0/keys",
		"/admin/users/-1/keys",
	} {
		req, _ := http.NewRequest(http.MethodPost, env.ts.URL+path,
			strings.NewReader(`{"name":"x"}`))
		req.Header.Set("Authorization", "Bearer admin-secret")
		resp, err := env.ts.Client().Do(req)
		if err != nil {
			t.Fatalf("请求 %s 失败: %v", path, err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 状态码 = %d, 期望 400", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestAdmin_签发Key路径格式错误返回404(t *testing.T) {
	env := newTestEnv(t)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/users/7/tokens",
		strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("状态码 = %d, 期望 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdmin_签发Key省略请求体也可成功(t *testing.T) {
	// 名称是可选的，强制要求会让最常见的「快速签一个 Key」变得啰嗦
	env := newTestEnv(t)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/users/7/keys", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("状态码 = %d, 期望 201: %s", resp.StatusCode, readAll(t, resp))
	} else {
		resp.Body.Close()
	}
}

func TestFailureKind_String覆盖全部取值(t *testing.T) {
	// 这个字符串直接进指标标签与日志，写错会让告警规则匹配不到
	cases := map[FailureKind]string{
		FailureAuth:      "auth",
		FailureRateLimit: "rate_limit",
		FailureServer:    "server",
		FailureQuota:     "quota",
		FailureNetwork:   "network",
		FailureKind(99):  "unknown",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("FailureKind(%d).String() = %q, 期望 %q", int(k), got, want)
		}
	}
}

func TestKeyState_TokenRatio(t *testing.T) {
	// 限额为 0 时必须返回 0 而非除零 panic —— 新导入的 Key 在
	// Redis 里还没有记录，限额确实可能是 0
	if r := (KeyState{TokenUsed: 100, TokenLimit: 0}).TokenRatio(); r != 0 {
		t.Errorf("限额为 0 时比例 = %v, 期望 0", r)
	}
	if r := (KeyState{TokenUsed: 250, TokenLimit: 1000}).TokenRatio(); r != 0.25 {
		t.Errorf("比例 = %v, 期望 0.25", r)
	}
}
