// Package metrics 定义 FluxKeys 的 Prometheus 指标。
//
// 指标选取依据 docs/scheduler-solution.md 14.1，但按 V4 的真实规模做了裁剪：
// 20 QPS 的系统不需要为高基数标签付代价，也不需要毫秒级分位数直方图。
//
// 标签基数控制是本包的主要设计约束。按 key_id 打标签在 1000 个 Key 下会产生
// 上万条时间序列，因此：
//   - 请求维度指标只按 model / status / provider 打标签（低基数）；
//   - Key 维度只暴露聚合分布（按状态分桶计数），不逐 Key 展开；
//   - 唯一按 key_id 展开的是配额水位，且用 GaugeVec 并在 Key 淘汰时删除标签。
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "fluxkeys"

// Metrics 聚合所有指标，避免包级全局变量导致测试之间互相污染。
type Metrics struct {
	reg *prometheus.Registry

	// ===== 请求层 =====
	RequestsTotal   *prometheus.CounterVec
	RequestDuration *prometheus.HistogramVec
	// TTFB 是流式请求的首字节延迟，用户体验的主要感知指标。
	StreamTTFB       *prometheus.HistogramVec
	RequestsInFlight *prometheus.GaugeVec
	TokensTotal      *prometheus.CounterVec

	// ===== 调度与重试 =====
	SchedulerSelectTotal    *prometheus.CounterVec
	SchedulerSelectDuration prometheus.Histogram
	RetriesTotal            *prometheus.CounterVec
	// RetriesPerRequest 记录单次用户请求消耗的重试次数分布，
	// 均值持续偏高说明 Key 池健康度在恶化。
	RetriesPerRequest prometheus.Histogram

	// ===== 配额 =====
	QuotaUsedRatio     *prometheus.GaugeVec
	QuotaAcquireTotal  *prometheus.CounterVec
	QuotaEstimateError prometheus.Histogram
	LeasesReapedTotal  prometheus.Counter
	LeasesActive       prometheus.Gauge
	QuotaDriftTotal    *prometheus.CounterVec
	QuotaDriftAmount   prometheus.Histogram

	// ===== Key 池 =====
	KeysByStatus *prometheus.GaugeVec
	KeysByPool   *prometheus.GaugeVec

	// ===== 出口 IP =====
	EgressIPsByState   *prometheus.GaugeVec
	EgressIPReputation *prometheus.GaugeVec
	EgressKeysBound    *prometheus.GaugeVec

	// ===== 用户层 =====
	RateLimitedTotal *prometheus.CounterVec

	// ===== 依赖健康 =====
	DependencyUp *prometheus.GaugeVec

	// mu 保护 trackedKeys，用于在 Key 淘汰时清理标签避免序列泄漏。
	mu          sync.Mutex
	trackedKeys map[string]struct{}
}

// New 构造并注册全部指标。
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{reg: reg, trackedKeys: make(map[string]struct{})}

	// 延迟分桶覆盖 LLM 场景: 从 50ms（缓存命中）到 120s（长文本生成）。
	// 默认分桶上限 10s 对流式请求毫无意义。
	latencyBuckets := []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120}

	m.RequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "requests_total",
		Help: "网关处理的请求总数，按端点、模型、状态码、是否流式区分。",
	}, []string{"endpoint", "model", "status", "stream"})

	m.RequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: "request_duration_seconds",
		Help: "端到端请求耗时。", Buckets: latencyBuckets,
	}, []string{"endpoint", "model", "stream"})

	m.StreamTTFB = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: "stream_ttfb_seconds",
		Help:    "流式请求首字节延迟。",
		Buckets: []float64{0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10},
	}, []string{"model"})

	m.RequestsInFlight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "requests_in_flight",
		Help: "进行中的请求数。优雅关闭需等待该值归零。",
	}, []string{"endpoint"})

	m.TokensTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "tokens_total",
		Help: "实际消耗的 Token 数，按模型与方向（prompt/completion）区分。",
	}, []string{"model", "direction"})

	m.SchedulerSelectTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "scheduler_select_total",
		Help: "调度选 Key 的结果计数（ok / no_candidate / error）。",
	}, []string{"result"})

	m.SchedulerSelectDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Name: "scheduler_select_duration_seconds",
		Help: "调度选 Key 耗时。100 个 Key 朴素遍历应在微秒级（P1-7）。",
		// 分桶下探到 10µs —— 若这里出现毫秒级尾部，说明打分路径引入了 I/O
		Buckets: []float64{0.00001, 0.00005, 0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05},
	})

	m.RetriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "retries_total",
		Help: "换 Key 重试次数，按错误分类区分。",
	}, []string{"class"})

	m.RetriesPerRequest = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Name: "retries_per_request",
		Help: "单次用户请求消耗的重试次数分布。", Buckets: []float64{0, 1, 2, 3, 4, 5},
	})

	m.QuotaUsedRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "quota_used_ratio",
		Help: "单 Key 配额水位（含预扣）占硬水位的比例。",
	}, []string{"key_id", "kind"})

	m.QuotaAcquireTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "quota_acquire_total",
		Help: "配额预扣结果计数（granted / granted_low / denied / error）。",
	}, []string{"kind", "result"})

	m.QuotaEstimateError = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Name: "quota_estimate_error_ratio",
		Help: "预扣估算误差比（实际/预扣）。持续 <0.3 说明预扣过于保守而浪费额度，" +
			">1 说明估算不足有超刷风险。",
		Buckets: []float64{0.1, 0.25, 0.5, 0.75, 0.9, 1.0, 1.25, 1.5, 2, 5},
	})

	m.LeasesReapedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Name: "leases_reaped_total",
		Help: "被超时回收的租约数（P0-2）。持续非零说明有请求路径未正确结束租约。",
	})

	m.LeasesActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Name: "leases_active",
		Help: "当前未结束的租约数。",
	})

	m.QuotaDriftTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "quota_drift_total",
		Help: "对账检出偏差的次数（P0-2）。",
	}, []string{"kind"})

	m.QuotaDriftAmount = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Name: "quota_drift_amount",
		Help:    "对账偏差量分布。",
		Buckets: []float64{1, 10, 100, 1000, 10000, 100000, 1000000},
	})

	m.KeysByStatus = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "keys_by_status",
		Help: "Key 池按状态的数量分布（active/cooldown/banned/invalid/probing）。",
	}, []string{"status"})

	m.KeysByPool = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "keys_by_pool",
		Help: "Key 池按层级的数量分布（hot/warm/cold）。",
	}, []string{"pool"})

	m.EgressIPsByState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "egress_ips_by_state",
		Help: "出口 IP 按状态的数量分布。",
	}, []string{"state"})

	m.EgressIPReputation = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "egress_ip_reputation",
		Help: "单个出口 IP 的信誉分（0-100）。",
	}, []string{"addr"})

	m.EgressKeysBound = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "egress_keys_bound",
		Help: "每个出口 IP 绑定的 Key 数量。单 IP 承载过多 Key 会提高关联识别风险。",
	}, []string{"addr"})

	m.RateLimitedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "rate_limited_total",
		Help: "用户级限流拒绝次数，按限流维度区分（rpm/tpm/daily_token）。",
	}, []string{"dimension"})

	m.DependencyUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "dependency_up",
		Help: "依赖可达性（1 可达 / 0 不可达），供 /readyz 与告警使用。",
	}, []string{"dependency"})

	reg.MustRegister(
		m.RequestsTotal, m.RequestDuration, m.StreamTTFB, m.RequestsInFlight, m.TokensTotal,
		m.SchedulerSelectTotal, m.SchedulerSelectDuration, m.RetriesTotal, m.RetriesPerRequest,
		m.QuotaUsedRatio, m.QuotaAcquireTotal, m.QuotaEstimateError,
		m.LeasesReapedTotal, m.LeasesActive, m.QuotaDriftTotal, m.QuotaDriftAmount,
		m.KeysByStatus, m.KeysByPool,
		m.EgressIPsByState, m.EgressIPReputation, m.EgressKeysBound,
		m.RateLimitedTotal, m.DependencyUp,
	)
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return m
}

// Handler 返回 /metrics 的 HTTP 处理器。
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		// 采集出错时仍返回已成功采集的部分，避免单个坏 Collector 打瞎整个监控
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// ===== 便捷记录方法 =====
// 这些方法把标签拼装收拢在一处，避免调用方各写一份标签顺序而出错。

// ObserveRequest 记录一次完成的请求。
func (m *Metrics) ObserveRequest(endpoint, model, status string, stream bool, d time.Duration) {
	s := boolLabel(stream)
	m.RequestsTotal.WithLabelValues(endpoint, model, status, s).Inc()
	m.RequestDuration.WithLabelValues(endpoint, model, s).Observe(d.Seconds())
}

// ObserveTTFB 记录流式首字节延迟。
func (m *Metrics) ObserveTTFB(model string, d time.Duration) {
	m.StreamTTFB.WithLabelValues(model).Observe(d.Seconds())
}

// IncInFlight / DecInFlight 维护进行中请求计数。
func (m *Metrics) IncInFlight(endpoint string) { m.RequestsInFlight.WithLabelValues(endpoint).Inc() }
func (m *Metrics) DecInFlight(endpoint string) { m.RequestsInFlight.WithLabelValues(endpoint).Dec() }

// ObserveTokens 记录实际 Token 消耗。
func (m *Metrics) ObserveTokens(model string, prompt, completion int64) {
	if prompt > 0 {
		m.TokensTotal.WithLabelValues(model, "prompt").Add(float64(prompt))
	}
	if completion > 0 {
		m.TokensTotal.WithLabelValues(model, "completion").Add(float64(completion))
	}
}

// ObserveSchedule 记录一次调度结果。
func (m *Metrics) ObserveSchedule(result string, d time.Duration) {
	m.SchedulerSelectTotal.WithLabelValues(result).Inc()
	m.SchedulerSelectDuration.Observe(d.Seconds())
}

// ObserveRetry 记录一次重试。
func (m *Metrics) ObserveRetry(class string) { m.RetriesTotal.WithLabelValues(class).Inc() }

// ObserveRetryCount 记录单请求的总重试次数。
func (m *Metrics) ObserveRetryCount(n int) { m.RetriesPerRequest.Observe(float64(n)) }

// ObserveQuotaAcquire 记录预扣结果。
func (m *Metrics) ObserveQuotaAcquire(kind, result string) {
	m.QuotaAcquireTotal.WithLabelValues(kind, result).Inc()
}

// ObserveEstimateError 记录预扣估算误差比。
func (m *Metrics) ObserveEstimateError(estimated, actual int64) {
	if estimated <= 0 {
		return
	}
	m.QuotaEstimateError.Observe(float64(actual) / float64(estimated))
}

// SetQuotaRatio 更新单 Key 的配额水位。
func (m *Metrics) SetQuotaRatio(keyID, kind string, ratio float64) {
	m.mu.Lock()
	m.trackedKeys[keyID] = struct{}{}
	m.mu.Unlock()
	m.QuotaUsedRatio.WithLabelValues(keyID, kind).Set(ratio)
}

// ForgetKey 删除某 Key 的所有序列。
//
// Key 被永久废弃后若不清理，其时间序列会一直留在内存与抓取输出里 ——
// 1000 Key 规模下这类泄漏会持续膨胀。
func (m *Metrics) ForgetKey(keyID string) {
	m.mu.Lock()
	delete(m.trackedKeys, keyID)
	m.mu.Unlock()
	m.QuotaUsedRatio.DeletePartialMatch(prometheus.Labels{"key_id": keyID})
}

// TrackedKeys 返回当前有配额序列的 Key 数量，用于自检序列规模。
func (m *Metrics) TrackedKeys() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.trackedKeys)
}

// SetKeyDistribution 覆盖式更新 Key 状态与池层级分布。
//
// 用 Reset 而非增量更新: 状态分布是快照语义，增量更新一旦漏掉某次状态迁移
// 就会永久性偏差，而快照式覆盖天然自愈。
func (m *Metrics) SetKeyDistribution(byStatus, byPool map[string]int) {
	m.KeysByStatus.Reset()
	for k, v := range byStatus {
		m.KeysByStatus.WithLabelValues(k).Set(float64(v))
	}
	m.KeysByPool.Reset()
	for k, v := range byPool {
		m.KeysByPool.WithLabelValues(k).Set(float64(v))
	}
}

// SetEgressStats 更新出口 IP 相关指标。
func (m *Metrics) SetEgressStats(byState map[string]int, reputation map[string]int, bound map[string]int) {
	m.EgressIPsByState.Reset()
	for k, v := range byState {
		m.EgressIPsByState.WithLabelValues(k).Set(float64(v))
	}
	m.EgressIPReputation.Reset()
	for addr, rep := range reputation {
		m.EgressIPReputation.WithLabelValues(addr).Set(float64(rep))
	}
	m.EgressKeysBound.Reset()
	for addr, n := range bound {
		m.EgressKeysBound.WithLabelValues(addr).Set(float64(n))
	}
}

// ObserveReap 记录一次租约回收。
func (m *Metrics) ObserveReap(n int64) {
	if n > 0 {
		m.LeasesReapedTotal.Add(float64(n))
	}
}

// ObserveDrift 记录一次对账偏差。
func (m *Metrics) ObserveDrift(kind string, amount int64) {
	if amount == 0 {
		return
	}
	if amount < 0 {
		amount = -amount
	}
	m.QuotaDriftTotal.WithLabelValues(kind).Inc()
	m.QuotaDriftAmount.Observe(float64(amount))
}

// SetDependencyUp 更新依赖可达性。
func (m *Metrics) SetDependencyUp(name string, up bool) {
	v := 0.0
	if up {
		v = 1
	}
	m.DependencyUp.WithLabelValues(name).Set(v)
}

// ObserveRateLimited 记录一次用户级限流拒绝。
func (m *Metrics) ObserveRateLimited(dimension string) {
	m.RateLimitedTotal.WithLabelValues(dimension).Inc()
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
