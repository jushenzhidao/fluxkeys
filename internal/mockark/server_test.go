package mockark

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, opt Options) (*Server, *httptest.Server) {
	t.Helper()
	s := NewServer(opt)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

func postChat(t *testing.T, ts *httptest.Server, keyID string, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v3/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-mock-"+keyID)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

const simpleChat = `{"model":"deepseek-v3-241226","messages":[{"role":"user","content":"你好"}],"max_tokens":100}`

func TestMock_非流式返回真实格式的usage(t *testing.T) {
	_, ts := newTestServer(t, Options{})
	resp := postChat(t, ts, "volc_001", simpleChat, nil)
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	var got struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
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
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "chat.completion" {
		t.Errorf("object = %q", got.Object)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content == "" {
		t.Errorf("choices 异常: %+v", got.Choices)
	}
	if got.Usage.TotalTokens != got.Usage.PromptTokens+got.Usage.CompletionTokens {
		t.Errorf("usage 不自洽: %+v", got.Usage)
	}
	if got.Usage.CompletionTokens > 100 {
		t.Errorf("completion_tokens = %d, 应受 max_tokens 约束", got.Usage.CompletionTokens)
	}
}

// readSSE 读取 SSE 响应，返回所有 data 行。
func readSSE(t *testing.T, resp *http.Response) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if after, ok := strings.CutPrefix(line, "data: "); ok {
			out = append(out, after)
		}
	}
	return out
}

func TestMock_流式SSE末尾chunk携带usage(t *testing.T) {
	_, ts := newTestServer(t, Options{StreamChunks: 3})
	body := `{"model":"m","stream":true,"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":"你好"}]}`
	resp := postChat(t, ts, "volc_001", body, nil)
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}

	lines := readSSE(t, resp)
	if len(lines) == 0 {
		t.Fatal("未收到任何 SSE 数据")
	}
	if lines[len(lines)-1] != "[DONE]" {
		t.Errorf("最后一行 = %q, 期望 [DONE]", lines[len(lines)-1])
	}

	// 倒数第二条应是 usage chunk
	var usageFound bool
	for _, l := range lines {
		if l == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage *struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(l), &chunk) == nil && chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
			usageFound = true
		}
	}
	if !usageFound {
		t.Error("流式响应缺少 usage chunk，网关将无法做配额修正")
	}
}

func TestMock_未设include_usage时不发送usage(t *testing.T) {
	_, ts := newTestServer(t, Options{StreamChunks: 2})
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp := postChat(t, ts, "volc_001", body, nil)
	defer resp.Body.Close()

	for _, l := range readSSE(t, resp) {
		if l == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage *json.RawMessage `json:"usage"`
		}
		_ = json.Unmarshal([]byte(l), &chunk)
		if chunk.Usage != nil && string(*chunk.Usage) != "null" {
			t.Errorf("未要求 usage 却收到: %s", l)
		}
	}
}

func TestMock_每个Key独立记账(t *testing.T) {
	s, ts := newTestServer(t, Options{})
	for i := 0; i < 3; i++ {
		resp := postChat(t, ts, "volc_A", simpleChat, nil)
		resp.Body.Close()
	}
	resp := postChat(t, ts, "volc_B", simpleChat, nil)
	resp.Body.Close()

	a := s.Account("volc_A")
	b := s.Account("volc_B")
	if a.Requests != 3 {
		t.Errorf("volc_A 请求数 = %d, 期望 3", a.Requests)
	}
	if b.Requests != 1 {
		t.Errorf("volc_B 请求数 = %d, 期望 1", b.Requests)
	}
	if a.TokenUsed <= b.TokenUsed {
		t.Errorf("volc_A 用量 %d 应大于 volc_B 的 %d", a.TokenUsed, b.TokenUsed)
	}
}

func TestMock_额度耗尽返回火山格式的额度错误(t *testing.T) {
	s, ts := newTestServer(t, Options{})
	// 额度设得极小，一次请求即耗尽
	s.RegisterKey("volc_poor", 10, 0)

	resp := postChat(t, ts, "volc_poor", simpleChat, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("首次请求应成功, got %d", resp.StatusCode)
	}

	resp2 := postChat(t, ts, "volc_poor", simpleChat, nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("额度耗尽后状态码 = %d, 期望 429", resp2.StatusCode)
	}

	var e arkError
	if err := json.NewDecoder(resp2.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	// 错误体必须能被 adapter 识别为「额度耗尽」而非普通限流
	if !strings.Contains(strings.ToLower(e.Error.Code), "quota") {
		t.Errorf("错误码 = %q, 应含 quota 以区分于纯限流", e.Error.Code)
	}
	if !strings.Contains(e.Error.Message, "额度已用完") {
		t.Errorf("错误消息 = %q", e.Error.Message)
	}
}

func TestMock_次数型额度独立于Token额度(t *testing.T) {
	s, ts := newTestServer(t, Options{})
	s.RegisterKey("volc_img", 0, 2)

	post := func() int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v3/images/generations",
			strings.NewReader(`{"model":"seedream-3.0","prompt":"cat","n":1}`))
		req.Header.Set("Authorization", "Bearer sk-mock-volc_img")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := post(); got != 200 {
		t.Fatalf("第 1 次 = %d", got)
	}
	if got := post(); got != 200 {
		t.Fatalf("第 2 次 = %d", got)
	}
	if got := post(); got != http.StatusTooManyRequests {
		t.Errorf("第 3 次 = %d, 期望 429（次数额度耗尽）", got)
	}
	if a := s.Account("volc_img"); a.CountUsed != 2 {
		t.Errorf("CountUsed = %d, 期望 2", a.CountUsed)
	}
}

func TestMock_请求头注入故障(t *testing.T) {
	_, ts := newTestServer(t, Options{})
	tests := []struct {
		fault  FaultKind
		status int
	}{
		{Fault401, 401},
		{Fault403, 403},
		{Fault429RateLimit, 429},
		{Fault429Quota, 429},
		{Fault500, 500},
		{Fault503, 503},
	}
	for _, tc := range tests {
		t.Run(string(tc.fault), func(t *testing.T) {
			resp := postChat(t, ts, "volc_001", simpleChat, map[string]string{
				"X-Mock-Fault": string(tc.fault),
			})
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Errorf("状态码 = %d, 期望 %d", resp.StatusCode, tc.status)
			}
		})
	}
}

func TestMock_区分429限流与429额度耗尽的错误体(t *testing.T) {
	_, ts := newTestServer(t, Options{})

	rl := postChat(t, ts, "k1", simpleChat, map[string]string{"X-Mock-Fault": string(Fault429RateLimit)})
	defer rl.Body.Close()
	var e1 arkError
	_ = json.NewDecoder(rl.Body).Decode(&e1)
	if strings.Contains(strings.ToLower(e1.Error.Code), "quota") {
		t.Errorf("纯限流错误码不应含 quota: %q", e1.Error.Code)
	}

	q := postChat(t, ts, "k1", simpleChat, map[string]string{"X-Mock-Fault": string(Fault429Quota)})
	defer q.Body.Close()
	var e2 arkError
	_ = json.NewDecoder(q.Body).Decode(&e2)
	if !strings.Contains(strings.ToLower(e2.Error.Code), "quota") {
		t.Errorf("额度耗尽错误码应含 quota: %q", e2.Error.Code)
	}
}

func TestMock_注入的故障按剩余次数消耗(t *testing.T) {
	s, ts := newTestServer(t, Options{})
	s.Inject(Fault{Kind: Fault500, KeyID: "volc_x", Remaining: 2})

	for i := 1; i <= 2; i++ {
		resp := postChat(t, ts, "volc_x", simpleChat, nil)
		resp.Body.Close()
		if resp.StatusCode != 500 {
			t.Errorf("第 %d 次 = %d, 期望 500", i, resp.StatusCode)
		}
	}
	resp := postChat(t, ts, "volc_x", simpleChat, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("故障用尽后应恢复, got %d", resp.StatusCode)
	}
}

func TestMock_故障可限定Key生效(t *testing.T) {
	s, ts := newTestServer(t, Options{})
	s.Inject(Fault{Kind: Fault401, KeyID: "volc_bad", Remaining: -1})

	bad := postChat(t, ts, "volc_bad", simpleChat, nil)
	bad.Body.Close()
	if bad.StatusCode != 401 {
		t.Errorf("目标 Key 状态码 = %d, 期望 401", bad.StatusCode)
	}

	good := postChat(t, ts, "volc_good", simpleChat, nil)
	good.Body.Close()
	if good.StatusCode != 200 {
		t.Errorf("其他 Key 不应受影响, got %d", good.StatusCode)
	}
}

func TestMock_禁用Key一律返回401(t *testing.T) {
	s, ts := newTestServer(t, Options{})
	s.SetKeyDisabled("volc_banned", true)

	resp := postChat(t, ts, "volc_banned", simpleChat, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("状态码 = %d, 期望 401", resp.StatusCode)
	}
}

func TestMock_流式中断不发送usage(t *testing.T) {
	_, ts := newTestServer(t, Options{StreamChunks: 6})
	body := `{"model":"m","stream":true,"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":"hi"}]}`
	resp := postChat(t, ts, "volc_001", body, map[string]string{
		"X-Mock-Fault": string(FaultStreamAbort),
	})
	defer resp.Body.Close()

	lines := readSSE(t, resp)
	for _, l := range lines {
		if l == "[DONE]" {
			t.Error("中断的流不应发送 [DONE]")
		}
		var chunk struct {
			Usage *struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(l), &chunk) == nil && chunk.Usage != nil {
			t.Error("中断的流不应发送 usage —— 网关必须靠租约兜底")
		}
	}
	if len(lines) == 0 {
		t.Error("应至少发出若干 chunk 后才中断")
	}
}

func TestMock_慢响应最终仍成功(t *testing.T) {
	_, ts := newTestServer(t, Options{})
	start := time.Now()
	resp := postChat(t, ts, "volc_001", simpleChat, map[string]string{
		"X-Mock-Fault": string(FaultSlow),
		"X-Mock-Delay": "150",
	})
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("状态码 = %d, 期望 200", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("耗时 %v, 应至少 150ms", elapsed)
	}
}

func TestMock_超时故障挂住直到客户端取消(t *testing.T) {
	_, ts := newTestServer(t, Options{})
	client := &http.Client{Timeout: 200 * time.Millisecond}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v3/chat/completions", strings.NewReader(simpleChat))
	req.Header.Set("Authorization", "Bearer sk-mock-volc_001")
	req.Header.Set("X-Mock-Fault", string(FaultTimeout))
	req.Header.Set("X-Mock-Delay", "5000")

	_, err := client.Do(req)
	if err == nil {
		t.Error("期望客户端超时")
	}
}

func TestMock_记录源IP用于验证出口绑定(t *testing.T) {
	s, ts := newTestServer(t, Options{})
	resp := postChat(t, ts, "volc_001", simpleChat, nil)
	resp.Body.Close()

	a := s.Account("volc_001")
	if len(a.SourceIPs) == 0 {
		t.Fatal("未记录源 IP —— 无法验证 P1-6 出口绑定是否生效")
	}
	var total int64
	for _, n := range a.SourceIPs {
		total += n
	}
	if total != 1 {
		t.Errorf("源 IP 计数 = %d, 期望 1", total)
	}
}

func TestMock_stats接口返回聚合统计(t *testing.T) {
	s, ts := newTestServer(t, Options{})
	s.RegisterKey("volc_001", 1_000_000, 0)
	for i := 0; i < 2; i++ {
		resp := postChat(t, ts, "volc_001", simpleChat, nil)
		resp.Body.Close()
	}

	resp, err := ts.Client().Get(ts.URL + "/_mock/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var st Stats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.TotalRequest != 2 {
		t.Errorf("TotalRequest = %d, 期望 2", st.TotalRequest)
	}
	if st.TokenUsed == 0 {
		t.Error("TokenUsed 应大于 0")
	}
	if len(st.SourceIPs) == 0 {
		t.Error("SourceIPs 为空")
	}
	if len(st.IPsPerKey["volc_001"]) == 0 {
		t.Error("IPsPerKey 应记录该 Key 的源 IP 列表")
	}
	if st.LogCount != 2 {
		t.Errorf("LogCount = %d, 期望 2", st.LogCount)
	}
}

func TestMock_reset清空全部状态(t *testing.T) {
	s, ts := newTestServer(t, Options{})
	s.Inject(Fault{Kind: Fault500, Remaining: 5})
	resp := postChat(t, ts, "volc_001", simpleChat, nil)
	resp.Body.Close()

	r, err := ts.Client().Post(ts.URL+"/_mock/reset", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()

	if a := s.Account("volc_001"); a.Requests != 0 || a.TokenUsed != 0 {
		t.Errorf("reset 后账本未清空: %+v", a)
	}
	if len(s.Logs()) != 0 {
		t.Error("reset 后日志未清空")
	}
	// 故障也应清空
	after := postChat(t, ts, "volc_001", simpleChat, nil)
	after.Body.Close()
	if after.StatusCode != 200 {
		t.Errorf("reset 后残留故障, got %d", after.StatusCode)
	}
}

func TestMock_inject接口注入故障并配置额度(t *testing.T) {
	s, ts := newTestServer(t, Options{})

	body := `{"kind":"429_quota","key_id":"volc_001","remaining":1,"token_limit":999}`
	resp, err := ts.Client().Post(ts.URL+"/_mock/inject", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("inject 状态码 = %d", resp.StatusCode)
	}

	if a := s.Account("volc_001"); a.TokenLimit != 999 {
		t.Errorf("TokenLimit = %d, 期望 999", a.TokenLimit)
	}

	c := postChat(t, ts, "volc_001", simpleChat, nil)
	c.Body.Close()
	if c.StatusCode != 429 {
		t.Errorf("注入的故障未生效, got %d", c.StatusCode)
	}
}

func TestMock_inject拒绝未知故障类型(t *testing.T) {
	_, ts := newTestServer(t, Options{})
	resp, err := ts.Client().Post(ts.URL+"/_mock/inject", "application/json",
		strings.NewReader(`{"kind":"not_a_fault"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 期望 400", resp.StatusCode)
	}
}

func TestMock_keys接口批量预置额度(t *testing.T) {
	s, ts := newTestServer(t, Options{})
	body := `{"keys":[{"key_id":"k1","token_limit":100,"token_used":90},{"key_id":"k2","count_limit":5}]}`
	resp, err := ts.Client().Post(ts.URL+"/_mock/keys", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if a := s.Account("k1"); a.TokenLimit != 100 || a.TokenUsed != 90 {
		t.Errorf("k1 = %+v", a)
	}
	if a := s.Account("k2"); a.CountLimit != 5 {
		t.Errorf("k2 = %+v", a)
	}
}

func TestMock_缺少Authorization返回401(t *testing.T) {
	_, ts := newTestServer(t, Options{})
	resp, err := ts.Client().Post(ts.URL+"/api/v3/chat/completions", "application/json",
		strings.NewReader(simpleChat))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("状态码 = %d, 期望 401", resp.StatusCode)
	}
}

func TestMock_embeddings返回向量与usage(t *testing.T) {
	_, ts := newTestServer(t, Options{})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v3/embeddings",
		strings.NewReader(`{"model":"doubao-embedding","input":["你好","世界"]}`))
	req.Header.Set("Authorization", "Bearer sk-mock-volc_001")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got struct {
		Object string `json:"object"`
		Data   []struct {
			Embedding []float64 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != 2 {
		t.Errorf("data 长度 = %d, 期望 2", len(got.Data))
	}
	if got.Usage.PromptTokens == 0 {
		t.Error("usage.prompt_tokens 应大于 0")
	}
}

func TestMock_models列表(t *testing.T) {
	_, ts := newTestServer(t, Options{Models: []string{"m1", "m2"}})
	resp, err := ts.Client().Get(ts.URL + "/api/v3/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if len(got.Data) != 2 || got.Data[0].ID != "m1" {
		t.Errorf("models = %+v", got.Data)
	}
}

func TestMock_固定种子下用量可复现(t *testing.T) {
	run := func() int64 {
		_, ts := newTestServer(t, Options{Seed: 7})
		resp := postChat(t, ts, "k", simpleChat, nil)
		defer resp.Body.Close()
		var got struct {
			Usage struct {
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&got)
		return got.Usage.CompletionTokens
	}
	if a, b := run(), run(); a != b {
		t.Errorf("同种子下 completion_tokens 不一致: %d vs %d", a, b)
	}
}

func TestMock_并发请求下账本无竞态(t *testing.T) {
	s, ts := newTestServer(t, Options{})
	const n = 40
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			resp := postChat(t, ts, fmt.Sprintf("k%d", i%4), simpleChat, nil)
			_, _ = bytes.NewBuffer(nil).ReadFrom(resp.Body)
			resp.Body.Close()
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}

	var total int64
	for i := 0; i < 4; i++ {
		total += s.Account(fmt.Sprintf("k%d", i)).Requests
	}
	if total != n {
		t.Errorf("总请求数 = %d, 期望 %d", total, n)
	}
}

// ---------- 按源 IP 注入故障 ----------

// 命中的源 IP 应触发故障，不命中的不受影响。
func TestMock_故障可限定源IP生效(t *testing.T) {
	s, ts := newTestServer(t, Options{})

	// 测试客户端从回环地址发起，故 127.0.0.1 必然命中
	s.Inject(Fault{Kind: Fault403, SourceIP: "127.0.0.1", Remaining: -1})

	hit := postChat(t, ts, "volc_001", simpleChat, nil)
	defer hit.Body.Close()
	if hit.StatusCode != http.StatusForbidden {
		t.Errorf("源 IP 命中应返回 403，实际 %d", hit.StatusCode)
	}

	// 换一个不可能命中的源 IP，同一个 Key 应恢复正常
	s.Reset()
	s.Inject(Fault{Kind: Fault403, SourceIP: "10.99.99.99", Remaining: -1})
	miss := postChat(t, ts, "volc_001", simpleChat, nil)
	defer miss.Body.Close()
	if miss.StatusCode != http.StatusOK {
		t.Errorf("源 IP 不匹配不应触发故障，实际 %d", miss.StatusCode)
	}
}

// KeyID 与 SourceIP 同时给出时取交集，而非任一命中即生效。
func TestMock_KeyID与源IP取交集(t *testing.T) {
	s, ts := newTestServer(t, Options{})
	// 限定 volc_001 且来自 127.0.0.1
	s.Inject(Fault{Kind: Fault500, KeyID: "volc_001", SourceIP: "127.0.0.1", Remaining: -1})

	a := postChat(t, ts, "volc_001", simpleChat, nil)
	defer a.Body.Close()
	if a.StatusCode != http.StatusInternalServerError {
		t.Errorf("两个条件都命中应返回 500，实际 %d", a.StatusCode)
	}

	// 换 Key: 源 IP 仍命中但 Key 不符，不应触发
	b := postChat(t, ts, "volc_002", simpleChat, nil)
	defer b.Body.Close()
	if b.StatusCode != http.StatusOK {
		t.Errorf("Key 不匹配不应触发故障，实际 %d", b.StatusCode)
	}

	// 源 IP 不符时即使 Key 命中也不应触发
	s.Reset()
	s.Inject(Fault{Kind: Fault500, KeyID: "volc_001", SourceIP: "10.99.99.99", Remaining: -1})
	c := postChat(t, ts, "volc_001", simpleChat, nil)
	defer c.Body.Close()
	if c.StatusCode != http.StatusOK {
		t.Errorf("源 IP 不匹配不应触发故障，实际 %d", c.StatusCode)
	}
}

// 只给 SourceIP 时对该 IP 上的所有 Key 生效 —— 这是「整个出口被封」的形态。
func TestMock_按源IP封禁影响该IP上全部Key(t *testing.T) {
	s, ts := newTestServer(t, Options{})
	s.BanSourceIP("127.0.0.1")

	for _, keyID := range []string{"volc_001", "volc_002", "volc_003"} {
		resp := postChat(t, ts, keyID, simpleChat, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s 应受出口封禁影响返回 403，实际 %d", keyID, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// BanSourceIP 必须永久生效: 用有限次数会让被封 IP 上的 Key 悄悄恢复。
func TestMock_BanSourceIP永久生效(t *testing.T) {
	s, ts := newTestServer(t, Options{})
	s.BanSourceIP("127.0.0.1")

	// 连发多次，每次都应被拒
	for i := 0; i < 5; i++ {
		resp := postChat(t, ts, "volc_001", simpleChat, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("第 %d 次请求应仍被拒，实际 %d —— 封禁规则被消耗掉了", i+1, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// 规则应仍在 active_faults 中
	var found bool
	for _, f := range s.activeFaults() {
		if f.SourceIP == "127.0.0.1" && f.Remaining == -1 {
			found = true
		}
	}
	if !found {
		t.Error("永久规则不应从故障列表中消失")
	}
}

// 未指定任何过滤条件时对所有请求生效（既有语义不得被破坏）。
func TestMock_无过滤条件对全部请求生效(t *testing.T) {
	s, ts := newTestServer(t, Options{})
	s.Inject(Fault{Kind: Fault503, Remaining: -1})

	for _, keyID := range []string{"volc_001", "volc_002"} {
		resp := postChat(t, ts, keyID, simpleChat, nil)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s 应命中无过滤条件的规则，实际 %d", keyID, resp.StatusCode)
		}
		resp.Body.Close()
	}
}
