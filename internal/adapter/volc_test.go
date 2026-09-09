package adapter

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func newTestVolc() *Volc {
	return NewVolc(map[string]string{
		"deepseek-v3":    "deepseek-v3-241226",
		"ds-v3":          "deepseek-v3-241226",
		"doubao-pro-32k": "doubao-pro-32k-241215",
	})
}

func TestVolc_请求体模型名映射为上游名称(t *testing.T) {
	v := newTestVolc()
	in := []byte(`{"model":"deepseek-v3","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`)

	path, out, err := v.TransformRequest(EndpointChat, in)
	if err != nil {
		t.Fatalf("TransformRequest 失败: %v", err)
	}
	if path != pathChat {
		t.Errorf("路径 = %q, 期望 %q", path, pathChat)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if got["model"] != "deepseek-v3-241226" {
		t.Errorf("model = %v, 期望 deepseek-v3-241226", got["model"])
	}
	// 未知字段必须保留，否则会静默丢弃火山新增参数
	if got["temperature"] != 0.7 {
		t.Errorf("temperature 丢失: %v", got["temperature"])
	}
}

func TestVolc_未配置映射的模型名原样透传且不重新编码(t *testing.T) {
	v := newTestVolc()
	in := []byte(`{"model":"unknown-model","messages":[]}`)

	_, out, err := v.TransformRequest(EndpointChat, in)
	if err != nil {
		t.Fatalf("TransformRequest 失败: %v", err)
	}
	if string(out) != string(in) {
		t.Errorf("无需映射时应原样返回，got %s", out)
	}
}

func TestVolc_缺少model字段返回客户端错误(t *testing.T) {
	v := newTestVolc()
	_, _, err := v.TransformRequest(EndpointChat, []byte(`{"messages":[]}`))
	if err == nil {
		t.Fatal("期望报错")
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("错误类型 = %T, 期望 *UpstreamError", err)
	}
	if ue.Class != ErrClassClient {
		t.Errorf("Class = %v, 期望 ErrClassClient", ue.Class)
	}
	if ue.Class.Retryable() {
		t.Error("客户端错误不应触发重试")
	}
}

func TestVolc_响应体模型名映射回对外别名(t *testing.T) {
	v := newTestVolc()
	in := []byte(`{"id":"chatcmpl-1","model":"deepseek-v3-241226","choices":[]}`)

	out, err := v.TransformResponse(EndpointChat, in)
	if err != nil {
		t.Fatalf("TransformResponse 失败: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	// 多别名指向同一上游名时取字典序最小者，保证结果确定
	if got["model"] != "deepseek-v3" {
		t.Errorf("model = %v, 期望 deepseek-v3", got["model"])
	}
}

func TestVolc_流式DONE哨兵与非JSON内容原样透传(t *testing.T) {
	v := newTestVolc()
	for _, in := range []string{"[DONE]", "", ": keep-alive"} {
		out, err := v.TransformStreamChunk([]byte(in))
		if err != nil {
			t.Fatalf("TransformStreamChunk(%q) 失败: %v", in, err)
		}
		if string(out) != in {
			t.Errorf("TransformStreamChunk(%q) = %q, 期望原样返回", in, out)
		}
	}
}

func TestVolc_流式chunk同样做模型名映射(t *testing.T) {
	v := newTestVolc()
	out, err := v.TransformStreamChunk([]byte(`{"id":"x","model":"doubao-pro-32k-241215","choices":[{"delta":{"content":"a"}}]}`))
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["model"] != "doubao-pro-32k" {
		t.Errorf("model = %v, 期望 doubao-pro-32k", got["model"])
	}
}

func TestVolc_认证头(t *testing.T) {
	v := newTestVolc()
	h := v.BuildAuthHeaders("sk-secret")
	if h.Get("Authorization") != "Bearer sk-secret" {
		t.Errorf("Authorization = %q", h.Get("Authorization"))
	}
	if h.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", h.Get("Content-Type"))
	}
}

func TestVolc_解析非流式用量(t *testing.T) {
	v := newTestVolc()
	u, ok := v.ParseUsage(EndpointChat, []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`))
	if !ok {
		t.Fatal("应解析出 usage")
	}
	if u.PromptTokens != 10 || u.CompletionTokens != 20 || u.Total() != 30 {
		t.Errorf("usage = %+v", u)
	}
}

func TestVolc_解析推理模型思维链用量(t *testing.T) {
	v := newTestVolc()
	// 真实上游响应形状（deepseek-v4-flash）: 88 + 141 = 229，
	// reasoning_tokens 已含在 completion_tokens 内。
	u, ok := v.ParseUsage(EndpointChat, []byte(`{"usage":{
		"prompt_tokens":88,"completion_tokens":141,"total_tokens":229,
		"completion_tokens_details":{"reasoning_tokens":124}}}`))
	if !ok {
		t.Fatal("应解析出 usage")
	}
	if u.ReasoningTokens != 124 {
		t.Errorf("ReasoningTokens = %d, 期望 124", u.ReasoningTokens)
	}
	// 关键不变量: 思维链不得叠加进计费口径，否则会重复计费
	if u.Total() != 229 {
		t.Errorf("Total() = %d, 期望 229（思维链不额外累加）", u.Total())
	}
}

func TestVolc_无思维链字段时reasoning为零(t *testing.T) {
	v := newTestVolc()
	u, ok := v.ParseUsage(EndpointChat, []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`))
	if !ok {
		t.Fatal("应解析出 usage")
	}
	if u.ReasoningTokens != 0 {
		t.Errorf("ReasoningTokens = %d, 非推理模型应为 0", u.ReasoningTokens)
	}
}

func TestVolc_流式中间chunk无usage属正常情况(t *testing.T) {
	v := newTestVolc()
	if _, ok := v.ParseUsage(EndpointChat, []byte(`{"choices":[{"delta":{"content":"hi"}}],"usage":null}`)); ok {
		t.Error("中间 chunk 不应返回 usage")
	}
}

func TestVolc_缺total_tokens时按分项求和兜底(t *testing.T) {
	v := newTestVolc()
	u, ok := v.ParseUsage(EndpointChat, []byte(`{"usage":{"prompt_tokens":7,"completion_tokens":5}}`))
	if !ok {
		t.Fatal("应解析出 usage")
	}
	if u.Total() != 12 {
		t.Errorf("Total() = %d, 期望 12", u.Total())
	}
}

func TestVolc_图片生成按张数计次(t *testing.T) {
	v := newTestVolc()
	u, ok := v.ParseUsage(EndpointImages, []byte(`{"created":1,"data":[{"url":"a"},{"url":"b"}]}`))
	if !ok {
		t.Fatal("应解析出用量")
	}
	if u.CountUnits != 2 {
		t.Errorf("CountUnits = %d, 期望 2", u.CountUnits)
	}
}

func TestVolc_错误码映射矩阵(t *testing.T) {
	v := newTestVolc()
	tests := []struct {
		name          string
		status        int
		body          string
		wantClass     ErrorClass
		wantClient    int
		wantCode      string
		wantRetryable bool
	}{
		{
			name:   "401 Key 无效 → 禁用 Key 并重试",
			status: 401, body: `{"error":{"message":"invalid api key","code":"AuthenticationError"}}`,
			wantClass: ErrClassAuth, wantClient: 401, wantCode: "invalid_auth", wantRetryable: true,
		},
		{
			name:   "403 权限不足 → 禁用 Key",
			status: 403, body: `{"error":{"message":"access denied","code":"AccessDenied"}}`,
			wantClass: ErrClassAuth, wantClient: 403, wantCode: "forbidden", wantRetryable: true,
		},
		{
			name:   "403 额度耗尽 → 归为配额类而非鉴权类",
			status: 403, body: `{"error":{"message":"account overdue","code":"AccountOverdue"}}`,
			wantClass: ErrClassQuota, wantClient: 503, wantCode: "service_busy", wantRetryable: true,
		},
		{
			name:   "404 模型不存在 → 不换 Key",
			status: 404, body: `{"error":{"message":"model not found","code":"ModelNotFound"}}`,
			wantClass: ErrClassClient, wantClient: 404, wantCode: "model_not_found", wantRetryable: false,
		},
		{
			name:   "429 纯限流 → 冷却后换 Key，对外报 503",
			status: 429, body: `{"error":{"message":"rate limit exceeded","code":"RateLimitExceeded"}}`,
			wantClass: ErrClassRateLimit, wantClient: 503, wantCode: "service_busy", wantRetryable: true,
		},
		{
			name:   "429 额度耗尽 → 必须区分于纯限流",
			status: 429, body: `{"error":{"message":"You exceeded the free quota","code":"QuotaExceeded"}}`,
			wantClass: ErrClassQuota, wantClient: 503, wantCode: "service_busy", wantRetryable: true,
		},
		{
			name:   "429 中文额度耗尽文案",
			status: 429, body: `{"error":{"message":"该模型免费额度已用完","code":"Throttling"}}`,
			wantClass: ErrClassQuota, wantClient: 503, wantCode: "service_busy", wantRetryable: true,
		},
		{
			name:   "500 → 换 Key 重试",
			status: 500, body: `{"error":{"message":"internal"}}`,
			wantClass: ErrClassServer, wantClient: 502, wantCode: "bad_gateway", wantRetryable: true,
		},
		{
			name:   "503 → 换 Key 重试",
			status: 503, body: `upstream unavailable`,
			wantClass: ErrClassServer, wantClient: 502, wantCode: "bad_gateway", wantRetryable: true,
		},
		{
			name:   "504 → 保留超时语义",
			status: 504, body: ``,
			wantClass: ErrClassServer, wantClient: 504, wantCode: "gateway_timeout", wantRetryable: true,
		},
		{
			name:   "400 普通参数错误",
			status: 400, body: `{"error":{"message":"bad temperature","code":"InvalidParameter"}}`,
			wantClass: ErrClassClient, wantClient: 400, wantCode: "invalid_request", wantRetryable: false,
		},
		{
			name:   "400 实为 Key 问题 → 归为鉴权类",
			status: 400, body: `{"error":{"message":"InvalidApiKey"}}`,
			wantClass: ErrClassAuth, wantClient: 401, wantCode: "invalid_auth", wantRetryable: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := v.MapError(tc.status, []byte(tc.body))
			if e == nil {
				t.Fatal("期望非 nil 错误")
			}
			if e.Class != tc.wantClass {
				t.Errorf("Class = %v, 期望 %v", e.Class, tc.wantClass)
			}
			if e.ClientStatus != tc.wantClient {
				t.Errorf("ClientStatus = %d, 期望 %d", e.ClientStatus, tc.wantClient)
			}
			if e.Code != tc.wantCode {
				t.Errorf("Code = %q, 期望 %q", e.Code, tc.wantCode)
			}
			if e.Class.Retryable() != tc.wantRetryable {
				t.Errorf("Retryable = %v, 期望 %v", e.Class.Retryable(), tc.wantRetryable)
			}
			if e.Message == "" {
				t.Error("Message 不应为空")
			}
		})
	}
}

func TestVolc_成功状态码不产生错误(t *testing.T) {
	v := newTestVolc()
	for _, s := range []int{200, 201, 204} {
		if e := v.MapError(s, nil); e != nil {
			t.Errorf("MapError(%d) = %v, 期望 nil", s, e)
		}
	}
}

func TestVolc_非JSON错误体也能提取消息(t *testing.T) {
	v := newTestVolc()
	e := v.MapError(http.StatusBadGateway, []byte("<html>502 Bad Gateway</html>"))
	if e.Message == "" {
		t.Error("应从非 JSON 响应体中提取到片段")
	}
}
