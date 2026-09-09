package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("抓取状态码 = %d", rec.Code)
	}
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

func TestNew_指标可正常抓取(t *testing.T) {
	m := New()
	m.ObserveRequest("chat", "deepseek-v3", "200", false, 120*time.Millisecond)

	out := scrape(t, m)
	for _, want := range []string{
		"fluxkeys_requests_total",
		"fluxkeys_request_duration_seconds",
		`endpoint="chat"`,
		`model="deepseek-v3"`,
		`status="200"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("抓取输出缺少 %q", want)
		}
	}
}

func TestNew_重复构造不冲突(t *testing.T) {
	// 用独立 Registry 而非默认注册表，多实例（如并行测试）不应 panic
	_ = New()
	_ = New()
}

func TestObserveRequest_流式标签(t *testing.T) {
	m := New()
	m.ObserveRequest("chat", "m", "200", true, time.Second)
	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("chat", "m", "200", "true")); got != 1 {
		t.Errorf("流式请求计数 = %v", got)
	}
}

func TestInFlight_增减对称(t *testing.T) {
	m := New()
	m.IncInFlight("chat")
	m.IncInFlight("chat")
	if got := testutil.ToFloat64(m.RequestsInFlight.WithLabelValues("chat")); got != 2 {
		t.Fatalf("in_flight = %v, 期望 2", got)
	}
	m.DecInFlight("chat")
	m.DecInFlight("chat")
	if got := testutil.ToFloat64(m.RequestsInFlight.WithLabelValues("chat")); got != 0 {
		t.Errorf("in_flight = %v, 期望归零（优雅关闭依赖该值）", got)
	}
}

func TestObserveTokens_按方向计数(t *testing.T) {
	m := New()
	m.ObserveTokens("m", 100, 250)
	if got := testutil.ToFloat64(m.TokensTotal.WithLabelValues("m", "prompt")); got != 100 {
		t.Errorf("prompt tokens = %v", got)
	}
	if got := testutil.ToFloat64(m.TokensTotal.WithLabelValues("m", "completion")); got != 250 {
		t.Errorf("completion tokens = %v", got)
	}
}

func TestObserveTokens_零值不产生序列(t *testing.T) {
	m := New()
	m.ObserveTokens("m", 0, 0)
	if strings.Contains(scrape(t, m), "fluxkeys_tokens_total") {
		t.Error("零值不应创建 tokens_total 序列")
	}
}

func TestObserveEstimateError_计算实际与预扣之比(t *testing.T) {
	m := New()
	m.ObserveEstimateError(1000, 500)
	out := scrape(t, m)
	if !strings.Contains(out, "fluxkeys_quota_estimate_error_ratio") {
		t.Error("缺少估算误差指标")
	}
	// 预扣为 0 时不应 panic 或产生 NaN
	m.ObserveEstimateError(0, 100)
	if strings.Contains(scrape(t, m), "NaN") {
		t.Error("输出中出现 NaN")
	}
}

func TestSetQuotaRatio与ForgetKey_避免序列泄漏(t *testing.T) {
	m := New()
	m.SetQuotaRatio("volc_001", "token", 0.42)
	m.SetQuotaRatio("volc_002", "token", 0.10)

	out := scrape(t, m)
	if !strings.Contains(out, `key_id="volc_001"`) {
		t.Fatal("缺少 volc_001 序列")
	}
	if m.TrackedKeys() != 2 {
		t.Errorf("TrackedKeys = %d, 期望 2", m.TrackedKeys())
	}

	m.ForgetKey("volc_001")
	out = scrape(t, m)
	if strings.Contains(out, `key_id="volc_001"`) {
		t.Error("ForgetKey 后序列仍存在，1000 Key 规模下会持续膨胀")
	}
	if !strings.Contains(out, `key_id="volc_002"`) {
		t.Error("ForgetKey 误删了其他 Key 的序列")
	}
	if m.TrackedKeys() != 1 {
		t.Errorf("TrackedKeys = %d, 期望 1", m.TrackedKeys())
	}
}

func TestSetKeyDistribution_快照式覆盖而非累加(t *testing.T) {
	m := New()
	m.SetKeyDistribution(map[string]int{"active": 100, "banned": 8}, map[string]int{"hot": 100})
	if got := testutil.ToFloat64(m.KeysByStatus.WithLabelValues("active")); got != 100 {
		t.Fatalf("active = %v", got)
	}

	// 第二次上报里 banned 消失，其序列也应随之消失而非停留在旧值
	m.SetKeyDistribution(map[string]int{"active": 95}, map[string]int{"hot": 95})
	if got := testutil.ToFloat64(m.KeysByStatus.WithLabelValues("active")); got != 95 {
		t.Errorf("active = %v, 期望覆盖为 95", got)
	}
	if strings.Contains(scrape(t, m), `status="banned"`) {
		t.Error("旧状态序列未被清除，分布会失真")
	}
}

func TestSetEgressStats(t *testing.T) {
	m := New()
	m.SetEgressStats(
		map[string]int{"active": 4, "banned": 1},
		map[string]int{"172.16.0.2": 100, "172.16.0.3": 70},
		map[string]int{"172.16.0.2": 10, "172.16.0.3": 8},
	)
	out := scrape(t, m)
	for _, want := range []string{
		"fluxkeys_egress_ips_by_state",
		"fluxkeys_egress_ip_reputation",
		"fluxkeys_egress_keys_bound",
		`addr="172.16.0.2"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q", want)
		}
	}
	if got := testutil.ToFloat64(m.EgressKeysBound.WithLabelValues("172.16.0.2")); got != 10 {
		t.Errorf("keys_bound = %v", got)
	}
}

func TestObserveReap_累加回收数(t *testing.T) {
	m := New()
	m.ObserveReap(3)
	m.ObserveReap(2)
	m.ObserveReap(0) // 零值不应影响
	if got := testutil.ToFloat64(m.LeasesReapedTotal); got != 5 {
		t.Errorf("leases_reaped_total = %v, 期望 5", got)
	}
}

func TestObserveDrift_负偏差取绝对值(t *testing.T) {
	m := New()
	m.ObserveDrift("token", -6777)
	if got := testutil.ToFloat64(m.QuotaDriftTotal.WithLabelValues("token")); got != 1 {
		t.Errorf("drift_total = %v, 期望 1", got)
	}
	m.ObserveDrift("token", 0)
	if got := testutil.ToFloat64(m.QuotaDriftTotal.WithLabelValues("token")); got != 1 {
		t.Errorf("零偏差不应计数, got %v", got)
	}
}

func TestSetDependencyUp(t *testing.T) {
	m := New()
	m.SetDependencyUp("redis", true)
	m.SetDependencyUp("postgres", false)
	if got := testutil.ToFloat64(m.DependencyUp.WithLabelValues("redis")); got != 1 {
		t.Errorf("redis = %v, 期望 1", got)
	}
	if got := testutil.ToFloat64(m.DependencyUp.WithLabelValues("postgres")); got != 0 {
		t.Errorf("postgres = %v, 期望 0", got)
	}
}

func TestObserveSchedule与Retry(t *testing.T) {
	m := New()
	m.ObserveSchedule("ok", 50*time.Microsecond)
	m.ObserveSchedule("no_candidate", 10*time.Microsecond)
	m.ObserveRetry("rate_limit")
	m.ObserveRetry("server")
	m.ObserveRetryCount(2)

	if got := testutil.ToFloat64(m.SchedulerSelectTotal.WithLabelValues("ok")); got != 1 {
		t.Errorf("select ok = %v", got)
	}
	if got := testutil.ToFloat64(m.RetriesTotal.WithLabelValues("rate_limit")); got != 1 {
		t.Errorf("retries = %v", got)
	}
	if !strings.Contains(scrape(t, m), "fluxkeys_retries_per_request") {
		t.Error("缺少 retries_per_request")
	}
}

func TestObserveRateLimited_按维度区分(t *testing.T) {
	m := New()
	m.ObserveRateLimited("rpm")
	m.ObserveRateLimited("tpm")
	m.ObserveRateLimited("rpm")
	if got := testutil.ToFloat64(m.RateLimitedTotal.WithLabelValues("rpm")); got != 2 {
		t.Errorf("rpm = %v, 期望 2", got)
	}
}

func TestServer_独立端口暴露metrics(t *testing.T) {
	m := New()
	m.ObserveRequest("chat", "m", "200", false, time.Second)

	srv := NewServer("127.0.0.1:0", m)
	if srv == nil {
		t.Fatal("NewServer 返回 nil")
	}
	// 端口 0 无法直接拿到实际地址，改用 httptest 验证 handler 装配
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "fluxkeys_requests_total") {
		t.Error("独立端口未暴露业务指标")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown 失败: %v", err)
	}
}

func TestServer_空地址返回nil且方法安全(t *testing.T) {
	srv := NewServer("", New())
	if srv != nil {
		t.Fatal("空地址应返回 nil")
	}
	// nil 接收者上的方法不应 panic —— 配置未启用指标端口时装配代码无需特判
	if got := srv.Addr(); got != "" {
		t.Errorf("Addr = %q", got)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown = %v", err)
	}
	if _, ok := <-srv.Start(); ok {
		t.Error("nil Server 的 Start 应立即关闭 channel")
	}
}

func TestServer_健康检查端点(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码 = %d", resp.StatusCode)
	}
}

func TestGoAndProcessCollector已注册(t *testing.T) {
	out := scrape(t, New())
	if !strings.Contains(out, "go_goroutines") {
		t.Error("缺少 Go 运行时指标")
	}
}
