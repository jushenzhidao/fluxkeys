package httpcore

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestErrorTypeFor(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "authentication_error"},
		{http.StatusForbidden, "authentication_error"},
		{http.StatusTooManyRequests, "rate_limit_error"},
		{http.StatusInternalServerError, "server_error"},
		{http.StatusBadGateway, "server_error"},
		{http.StatusBadRequest, "invalid_request_error"},
		{http.StatusNotFound, "invalid_request_error"},
		// 未知状态码必须归入 default 而不是空串: 该字段用于客户端分支，
		// 空串会让客户端落到未定义分支。
		{http.StatusTeapot, "invalid_request_error"},
	}
	for _, c := range cases {
		if got := ErrorTypeFor(c.status); got != c.want {
			t.Errorf("ErrorTypeFor(%d) = %q, 期望 %q", c.status, got, c.want)
		}
	}
}

// 错误信封的字段层级是对外契约（客户端 SDK 按 error.message/type/code 解析），
// 用原始 JSON 断言而不是解成结构体 —— 解成结构体会掩盖层级被改平的情况。
func TestWriteError_信封形状与状态码(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)

	WriteError(rec, r, nil, http.StatusTooManyRequests, "rate_limited", "额度耗尽")

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("状态码 = %d, 期望 %d", rec.Code, http.StatusTooManyRequests)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}

	var raw map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v", err)
	}
	e, ok := raw["error"]
	if !ok {
		t.Fatal("响应缺少顶层 error 字段")
	}
	if e["message"] != "额度耗尽" {
		t.Errorf("error.message = %v", e["message"])
	}
	if e["code"] != "rate_limited" {
		t.Errorf("error.code = %v", e["code"])
	}
	if e["type"] != "rate_limit_error" {
		t.Errorf("error.type = %v", e["type"])
	}
}

// log 为 nil 时不得 panic: 测试路径会传 nil，而写响应失败本就不该升级成崩溃。
func TestWriteError_nilLogger不panic(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	WriteError(rec, r, nil, http.StatusBadRequest, "invalid_request", "x")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("状态码 = %d", rec.Code)
	}
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusCreated, map[string]any{"id": 7})

	if rec.Code != http.StatusCreated {
		t.Errorf("状态码 = %d", rec.Code)
	}
	var got map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v", err)
	}
	if got["id"] != 7 {
		t.Errorf("id = %d", got["id"])
	}
}

// 上游传入的 X-Request-Id 必须被复用 —— 否则跨系统链路断裂，
// 排障时网关与上游的日志无法关联到同一次请求。
func TestWithRequestID_复用上游请求ID(t *testing.T) {
	const inbound = "req_from_upstream"
	var (
		gotCtx  string
		gotResp string
	)
	h := WithRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCtx = RequestIDFromContext(r.Context())
		gotResp = w.Header().Get("X-Request-Id")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", inbound)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotCtx != inbound {
		t.Errorf("context 中的请求 ID = %q, 期望 %q", gotCtx, inbound)
	}
	if gotResp != inbound {
		t.Errorf("响应头 X-Request-Id = %q, 期望 %q", gotResp, inbound)
	}
}

func TestWithRequestID_缺失时生成并回写(t *testing.T) {
	var gotCtx string
	h := WithRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCtx = RequestIDFromContext(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if gotCtx == "" {
		t.Fatal("缺失时应生成请求 ID")
	}
	if rec.Header().Get("X-Request-Id") != gotCtx {
		t.Errorf("响应头与 context 中的请求 ID 不一致: %q vs %q",
			rec.Header().Get("X-Request-Id"), gotCtx)
	}
}

// 无请求 ID 时必须返回空串而不是 panic。
//
// 注意不能让它与「生成了 ID」的情形混淆: 调用方据空串决定是否记录该字段。
func TestRequestIDFromContext_缺失返回空串(t *testing.T) {
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Errorf("空 context 应返回空串，实际 %q", got)
	}
	// 别人的键不能读出本包的值 —— ctxKey 是私有类型，外部构造不出同类型的键。
	other := context.WithValue(context.Background(), struct{}{}, "x") //nolint:staticcheck
	if got := RequestIDFromContext(other); got != "" {
		t.Errorf("无关 context 应返回空串，实际 %q", got)
	}
}

func TestNewRequestID_格式与唯一性(t *testing.T) {
	re := regexp.MustCompile(`^req_[0-9a-f]{32}$`)
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := NewRequestID()
		if !re.MatchString(id) {
			t.Fatalf("格式不符: %q（期望 req_ + 32 位十六进制）", id)
		}
		if seen[id] {
			t.Fatalf("第 %d 次生成出现重复: %q", i, id)
		}
		seen[id] = true
	}
	if strings.Contains(NewRequestID(), "req_") == false {
		t.Fatal("前缀缺失")
	}
}

// WriteError 的编码失败路径不应改变已写出的状态码。
//
// 用一个一写就失败的 ResponseWriter 触发编码失败（写入时返回错误），
// 断言状态码仍是调用方指定的那个 —— 失败降级只影响日志，不影响响应语义。
func TestWriteError_编码失败不影响状态码(t *testing.T) {
	fw := &failingWriter{header: make(http.Header)}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	WriteError(fw, r, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)),
		http.StatusBadGateway, "bad_gateway", "上游不可达")

	if fw.status != http.StatusBadGateway {
		t.Errorf("状态码 = %d, 期望 %d", fw.status, http.StatusBadGateway)
	}
}

type failingWriter struct {
	header http.Header
	status int
}

func (f *failingWriter) Header() http.Header       { return f.header }
func (f *failingWriter) WriteHeader(status int)    { f.status = status }
func (f *failingWriter) Write([]byte) (int, error) { return 0, context.DeadlineExceeded }
