package adapter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// SenseNova 是商汤 SenseNova 的适配器。
//
// SenseNova 与 OpenAI 高度兼容，转换工作集中在:
//  1. 模型名映射: 对外暴露稳定别名，上游用实际模型名。
//  2. 错误码归一化: SenseNova 的错误响应结构与 OpenAI 兼容。
//  3. 配额规则差异: SenseNova 使用「调用次数 + 5 小时滑动窗口」而非日刷新。
type SenseNova struct {
	// forward 是 对外名 → 上游名 的映射。
	forward map[string]string
	// reverse 是 上游名 → 对外名 的反向映射，用于响应转换。
	reverse map[string]string
}

// NewSenseNova 构造 SenseNova 适配器。mapping 为 对外模型名 → SenseNova 实际模型名。
func NewSenseNova(mapping map[string]string) *SenseNova {
	s := &SenseNova{
		forward: make(map[string]string, len(mapping)),
		reverse: make(map[string]string, len(mapping)),
	}
	for public, upstream := range mapping {
		if public == "" || upstream == "" {
			continue
		}
		s.forward[public] = upstream
		// 多个别名映射到同一上游名时，反向只保留第一个（按字典序稳定）。
		if exist, ok := s.reverse[upstream]; !ok || public < exist {
			s.reverse[upstream] = public
		}
	}
	return s
}

// Provider 返回渠道标识。
func (s *SenseNova) Provider() Provider { return "sensenova" }

// 上游路径。SenseNova 的 OpenAI 兼容端点位于 /v1 下。
const (
	sensePathChat       = "/v1/chat/completions"
	sensePathImages     = "/v1/images/generations"
	sensePathEmbeddings = "/v1/embeddings"
)

// UpstreamPath 返回端点对应的上游相对路径。
func (s *SenseNova) UpstreamPath(ep Endpoint) string {
	switch ep {
	case EndpointImages:
		return sensePathImages
	case EndpointEmbeddings:
		return sensePathEmbeddings
	default:
		return sensePathChat
	}
}

// UpstreamModel 返回对外模型名在 SenseNova 侧的实际名称。
func (s *SenseNova) UpstreamModel(public string) string {
	if u, ok := s.forward[public]; ok {
		return u
	}
	return public
}

// PublicModel 返回 SenseNova 模型名对应的对外别名。
func (s *SenseNova) PublicModel(upstream string) string {
	if p, ok := s.reverse[upstream]; ok {
		return p
	}
	return upstream
}

// TransformRequest 做模型名替换，其余字段原样透传。
func (s *SenseNova) TransformRequest(ep Endpoint, body []byte) (string, []byte, error) {
	path := s.UpstreamPath(ep)

	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return path, nil, NewClientError(http.StatusBadRequest, "invalid_request", "请求体不是合法 JSON: "+err.Error())
	}

	raw, ok := m["model"]
	if !ok {
		return path, nil, NewClientError(http.StatusBadRequest, "invalid_request", "缺少 model 字段")
	}
	var public string
	if err := json.Unmarshal(raw, &public); err != nil {
		return path, nil, NewClientError(http.StatusBadRequest, "invalid_request", "model 字段必须是字符串")
	}

	upstream := s.UpstreamModel(public)
	if upstream == public {
		// 无需改写，避免重新序列化带来的字段顺序变化
		return path, body, nil
	}
	enc, err := json.Marshal(upstream)
	if err != nil {
		return path, nil, NewClientError(http.StatusBadRequest, "invalid_request", "model 字段无法编码")
	}
	m["model"] = enc

	out, err := json.Marshal(m)
	if err != nil {
		return path, nil, NewClientError(http.StatusBadRequest, "invalid_request", "请求体无法重新编码")
	}
	return path, out, nil
}

// TransformResponse 将响应中的上游模型名映射回对外别名。
func (s *SenseNova) TransformResponse(ep Endpoint, body []byte) ([]byte, error) {
	return s.rewriteModel(body), nil
}

// TransformStreamChunk 处理单条 SSE 数据。
func (s *SenseNova) TransformStreamChunk(data []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return data, nil
	}
	if trimmed[0] != '{' {
		return data, nil
	}
	return s.rewriteModel(data), nil
}

// rewriteModel 仅替换顶层 model 字段，其他内容保持不变。
func (s *SenseNova) rewriteModel(body []byte) []byte {
	if len(s.reverse) == 0 {
		return body
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	raw, ok := m["model"]
	if !ok {
		return body
	}
	var upstream string
	if err := json.Unmarshal(raw, &upstream); err != nil {
		return body
	}
	public := s.PublicModel(upstream)
	if public == upstream {
		return body
	}
	enc, err := json.Marshal(public)
	if err != nil {
		return body
	}
	m["model"] = enc
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// BuildAuthHeaders 构造 SenseNova 认证头。
func (s *SenseNova) BuildAuthHeaders(secret string) http.Header {
	h := make(http.Header, 2)
	h.Set("Authorization", "Bearer "+secret)
	h.Set("Content-Type", "application/json")
	return h
}

// ParseUsage 从响应体或流式 chunk 中提取用量。
// SenseNova 的 usage 结构与 OpenAI 完全兼容。
func (s *SenseNova) ParseUsage(ep Endpoint, payload []byte) (Usage, bool) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Usage{}, false
	}

	var env usageEnvelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return Usage{}, false
	}

	var u Usage
	if env.Usage != nil {
		u.PromptTokens = env.Usage.PromptTokens
		u.CompletionTokens = env.Usage.CompletionTokens
		u.TotalTokens = env.Usage.TotalTokens
		if d := env.Usage.CompletionDetails; d != nil {
			u.ReasoningTokens = d.ReasoningTokens
		}
	}

	// 图片生成按生成张数计费
	if ep == EndpointImages {
		u.CountUnits = int64(len(env.Data))
		if u.CountUnits == 0 {
			u.CountUnits = 1
		}
		return u, true
	}

	if u.Empty() {
		return Usage{}, false
	}
	return u, true
}

// SenseNova 配额相关的错误标识。
var senseQuotaMarkers = []string{
	"quota",
	"exceed",
	"insufficient",
	"balance",
	"limit",
	"额度",
	"超限",
	"余额",
}

// SenseNova Key 无效的标识。
var senseInvalidKeyMarkers = []string{
	"authentication",
	"invalid",
	"api key",
	"unauthorized",
	"access denied",
	"认证",
	"无效",
}

// MapError 将 SenseNova 状态码与响应体映射为统一错误。
func (s *SenseNova) MapError(statusCode int, body []byte) *UpstreamError {
	if statusCode > 0 && statusCode < 400 {
		return nil
	}

	msg, code := parseErrorBody(body)
	probe := strings.ToLower(msg + " " + code)

	e := &UpstreamError{
		StatusCode: statusCode,
		Message:    msg,
		Body:       body,
	}

	switch {
	case statusCode == http.StatusUnauthorized:
		e.Class = ErrClassAuth
		e.ClientStatus = http.StatusUnauthorized
		e.Code = "invalid_auth"

	case statusCode == http.StatusForbidden:
		// 403 可能是额度耗尽或 Key 无效
		if containsAny(probe, senseQuotaMarkers) {
			e.Class = ErrClassQuota
			e.ClientStatus = http.StatusServiceUnavailable
			e.Code = "service_busy"
		} else {
			e.Class = ErrClassAuth
			e.ClientStatus = http.StatusForbidden
			e.Code = "forbidden"
		}

	case statusCode == http.StatusNotFound:
		e.Class = ErrClassClient
		e.ClientStatus = http.StatusNotFound
		e.Code = "model_not_found"

	case statusCode == http.StatusTooManyRequests:
		if containsAny(probe, senseQuotaMarkers) {
			e.Class = ErrClassQuota
			e.ClientStatus = http.StatusServiceUnavailable
			e.Code = "service_busy"
		} else {
			e.Class = ErrClassRateLimit
			e.ClientStatus = http.StatusServiceUnavailable
			e.Code = "service_busy"
		}

	case statusCode == http.StatusGatewayTimeout:
		e.Class = ErrClassServer
		e.ClientStatus = http.StatusGatewayTimeout
		e.Code = "gateway_timeout"

	case statusCode >= 500:
		e.Class = ErrClassServer
		e.ClientStatus = http.StatusBadGateway
		e.Code = "bad_gateway"

	case statusCode == http.StatusBadRequest:
		if containsAny(probe, senseInvalidKeyMarkers) {
			e.Class = ErrClassAuth
			e.ClientStatus = http.StatusUnauthorized
			e.Code = "invalid_auth"
		} else {
			e.Class = ErrClassClient
			e.ClientStatus = http.StatusBadRequest
			e.Code = "invalid_request"
		}

	default:
		e.Class = ErrClassClient
		e.ClientStatus = statusCode
		e.Code = "invalid_request"
	}

	if e.Message == "" {
		e.Message = http.StatusText(statusCode)
	}
	return e
}
