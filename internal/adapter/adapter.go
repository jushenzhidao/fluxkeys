// Package adapter 定义渠道适配层。
//
// 对应 docs/scheduler-solution.md 11.3 的 6 个方法：请求转换、响应转换、
// 流式 chunk 转换、认证头构造、用量解析、错误映射。
//
// 设计约束（P1-10）: MVP 只实现火山渠道。付费渠道 fallback 在没有预算护栏
// 之前一律不实现 —— 一个失控的 fallback 一天能烧掉数月预算，风险远高于
// 「配额耗尽时返回 503」这个可接受的降级行为。接口留出扩展位，但不留实现。
package adapter

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Provider 是渠道标识。
type Provider string

const (
	ProviderVolc Provider = "volc"
)

// Endpoint 区分不同的业务端点，决定 URL 路径与计费方式。
type Endpoint string

const (
	EndpointChat       Endpoint = "chat"
	EndpointImages     Endpoint = "images"
	EndpointEmbeddings Endpoint = "embeddings"
)

// Usage 是统一的用量结构。
//
// 这是配额修正的唯一输入。字段命名与 OpenAI 一致，便于原样透传。
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	// CountUnits 是按次计费的消耗次数（如 Seedream 生成的图片数）。
	CountUnits int64 `json:"-"`
}

// Total 返回用于配额修正的总量。上游未给 total_tokens 时按分项求和兜底。
func (u Usage) Total() int64 {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.PromptTokens + u.CompletionTokens
}

// Empty 判断是否为空用量（上游没返回 usage）。
func (u Usage) Empty() bool {
	return u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 && u.CountUnits == 0
}

// ErrorClass 是跨渠道统一的错误分类，驱动重试与 Key 状态机决策。
//
// 分类而非原始状态码才是决策依据: 不同渠道对同一语义可能用不同状态码，
// 上层重试逻辑只应关心「该不该换 Key」「该不该禁用 Key」。
type ErrorClass int

const (
	// ErrClassNone 表示成功。
	ErrClassNone ErrorClass = iota
	// ErrClassClient 是用户请求本身的问题（400/404），换 Key 无用，直接透传。
	ErrClassClient
	// ErrClassAuth 是 Key 无效或被封（401/403），必须禁用该 Key 并换 Key。
	ErrClassAuth
	// ErrClassRateLimit 是速率限制（429），该 Key 冷却后换 Key 重试。
	ErrClassRateLimit
	// ErrClassQuota 是上游侧额度耗尽，须立即将该 Key 的本地水位打满并换 Key。
	ErrClassQuota
	// ErrClassServer 是上游服务端错误（5xx），换 Key 重试。
	ErrClassServer
	// ErrClassNetwork 是连接失败或超时，换 Key 重试。
	ErrClassNetwork
)

// String 返回分类的可读名称，用于日志与指标标签。
func (c ErrorClass) String() string {
	switch c {
	case ErrClassNone:
		return "none"
	case ErrClassClient:
		return "client"
	case ErrClassAuth:
		return "auth"
	case ErrClassRateLimit:
		return "rate_limit"
	case ErrClassQuota:
		return "quota"
	case ErrClassServer:
		return "server"
	case ErrClassNetwork:
		return "network"
	}
	return "unknown"
}

// Retryable 判断该错误是否值得换一个 Key 重试。
//
// 客户端错误不可重试 —— 参数错误或模型不存在，换多少个 Key 结果都一样，
// 重试只会白白消耗额度并放大上游压力。
func (c ErrorClass) Retryable() bool {
	switch c {
	case ErrClassAuth, ErrClassRateLimit, ErrClassQuota, ErrClassServer, ErrClassNetwork:
		return true
	}
	return false
}

// UpstreamError 是归一化后的上游错误。
type UpstreamError struct {
	// Class 是统一分类，重试决策依据。
	Class ErrorClass
	// StatusCode 是上游返回的原始状态码（网络错误时为 0）。
	StatusCode int
	// ClientStatus 是应当返回给用户的状态码。
	ClientStatus int
	// Code 是对外错误码，取值见 docs/scheduler-solution.md 15.3。
	Code string
	// Message 是错误描述。
	Message string
	// Body 是上游原始响应体，用于透传与排障。
	Body []byte
}

func (e *UpstreamError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("upstream error: class=%s status=%d code=%s msg=%s",
		e.Class, e.StatusCode, e.Code, e.Message)
}

// Adapter 是渠道适配器接口，对应文档 11.3 的六个方法。
type Adapter interface {
	// Provider 返回渠道标识。
	Provider() Provider

	// TransformRequest 将通用（OpenAI 格式）请求体转为渠道特定请求体，
	// 并返回该请求应当访问的上游相对路径。
	TransformRequest(ep Endpoint, body []byte) (path string, out []byte, err error)

	// TransformResponse 将渠道响应体转为通用响应体。
	// 返回的 model 为对外模型名（需把上游实际模型名映射回去）。
	TransformResponse(ep Endpoint, body []byte) (out []byte, err error)

	// TransformStreamChunk 将渠道 SSE 数据行转为通用 SSE 数据行。
	// data 是 "data: " 之后的原始内容，不含前缀与换行。
	TransformStreamChunk(data []byte) (out []byte, err error)

	// BuildAuthHeaders 构造该渠道的认证头。
	BuildAuthHeaders(secret string) http.Header

	// ParseUsage 从响应体或流式最后一个 chunk 中解析用量。
	// ok 为 false 表示该载荷不含 usage（流式的中间 chunk 属正常情况）。
	ParseUsage(ep Endpoint, payload []byte) (u Usage, ok bool)

	// MapError 将渠道状态码与响应体映射为统一错误。
	// statusCode < 400 时返回 nil。
	MapError(statusCode int, body []byte) *UpstreamError
}

// openAIErrorBody 是 OpenAI 风格的错误响应结构，火山与之兼容。
type openAIErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
		Param   string `json:"param"`
	} `json:"error"`
}

// parseErrorBody 尽力从响应体中提取错误消息与错误码。
// 上游返回非 JSON（如网关的 HTML 错误页）时不应导致解析失败。
func parseErrorBody(body []byte) (msg, code string) {
	var b openAIErrorBody
	if err := json.Unmarshal(body, &b); err == nil {
		msg = b.Error.Message
		switch v := b.Error.Code.(type) {
		case string:
			code = v
		case float64:
			code = fmt.Sprintf("%d", int64(v))
		}
		if code == "" {
			code = b.Error.Type
		}
	}
	if msg == "" {
		const maxSnippet = 256
		if len(body) > maxSnippet {
			msg = string(body[:maxSnippet])
		} else {
			msg = string(body)
		}
	}
	return msg, code
}

// NewClientError 构造一个不触发重试的客户端错误，供网关层自身校验失败时使用。
func NewClientError(status int, code, msg string) *UpstreamError {
	return &UpstreamError{
		Class:        ErrClassClient,
		StatusCode:   status,
		ClientStatus: status,
		Code:         code,
		Message:      msg,
	}
}

// NewNetworkError 构造网络层错误（连接失败、超时、上游中断）。
func NewNetworkError(msg string) *UpstreamError {
	return &UpstreamError{
		Class:        ErrClassNetwork,
		StatusCode:   0,
		ClientStatus: http.StatusBadGateway,
		Code:         "bad_gateway",
		Message:      msg,
	}
}
