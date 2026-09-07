package adapter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// Volc 是火山引擎 Ark 的适配器。
//
// 火山 Ark 基本与 OpenAI 兼容，故转换工作集中在两处:
//  1. 模型名映射: 对外暴露稳定别名（deepseek-v3），上游用带版本后缀的实际
//     名称（deepseek-v3-241226）。响应里必须映射回别名，否则客户端会看到
//     一个它没请求过的模型名，SDK 层可能因此报错。
//  2. 错误码归一化: 火山把「额度耗尽」和「限流」都放在 429，必须靠响应体
//     里的 code 区分 —— 两者的处置完全不同（前者要打满本地水位，后者只需冷却）。
type Volc struct {
	// forward 是 对外名 → 上游名 的映射。
	forward map[string]string
	// reverse 是 上游名 → 对外名 的反向映射，用于响应转换。
	reverse map[string]string
}

// NewVolc 构造火山适配器。mapping 为 对外模型名 → 火山实际模型名。
func NewVolc(mapping map[string]string) *Volc {
	v := &Volc{
		forward: make(map[string]string, len(mapping)),
		reverse: make(map[string]string, len(mapping)),
	}
	for public, upstream := range mapping {
		if public == "" || upstream == "" {
			continue
		}
		v.forward[public] = upstream
		// 多个别名映射到同一上游名时，反向只保留第一个（按字典序稳定）。
		if exist, ok := v.reverse[upstream]; !ok || public < exist {
			v.reverse[upstream] = public
		}
	}
	return v
}

// Provider 返回渠道标识。
func (v *Volc) Provider() Provider { return ProviderVolc }

// 上游路径。火山 Ark 的 OpenAI 兼容端点位于 /api/v3 下。
const (
	pathChat       = "/api/v3/chat/completions"
	pathImages     = "/api/v3/images/generations"
	pathEmbeddings = "/api/v3/embeddings"
)

// UpstreamPath 返回端点对应的上游相对路径。
func (v *Volc) UpstreamPath(ep Endpoint) string {
	switch ep {
	case EndpointImages:
		return pathImages
	case EndpointEmbeddings:
		return pathEmbeddings
	default:
		return pathChat
	}
}

// UpstreamModel 返回对外模型名在火山侧的实际名称。
func (v *Volc) UpstreamModel(public string) string {
	if u, ok := v.forward[public]; ok {
		return u
	}
	return public
}

// PublicModel 返回火山模型名对应的对外别名。
func (v *Volc) PublicModel(upstream string) string {
	if p, ok := v.reverse[upstream]; ok {
		return p
	}
	return upstream
}

// TransformRequest 做模型名替换，其余字段原样透传。
//
// 用 map 而非结构体承载请求体，是为了不吞掉未知字段 —— 火山会持续新增
// thinking、cache 之类的参数，用结构体反序列化会静默丢弃它们。
func (v *Volc) TransformRequest(ep Endpoint, body []byte) (string, []byte, error) {
	path := v.UpstreamPath(ep)

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

	upstream := v.UpstreamModel(public)
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
func (v *Volc) TransformResponse(ep Endpoint, body []byte) ([]byte, error) {
	return v.rewriteModel(body), nil
}

// TransformStreamChunk 处理单条 SSE 数据。
//
// [DONE] 哨兵与非 JSON 内容原样返回 —— 流式转发的正确性优先于规范性，
// 无法解析的内容透传给客户端总比中断连接好。
func (v *Volc) TransformStreamChunk(data []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return data, nil
	}
	if trimmed[0] != '{' {
		return data, nil
	}
	return v.rewriteModel(data), nil
}

// rewriteModel 仅替换顶层 model 字段，其他内容保持不变。
// 任何解析失败都返回原始载荷，绝不因为改写失败而丢数据。
func (v *Volc) rewriteModel(body []byte) []byte {
	if len(v.reverse) == 0 {
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
	public := v.PublicModel(upstream)
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

// BuildAuthHeaders 构造火山认证头。
func (v *Volc) BuildAuthHeaders(secret string) http.Header {
	h := make(http.Header, 2)
	h.Set("Authorization", "Bearer "+secret)
	h.Set("Content-Type", "application/json")
	return h
}

// usageEnvelope 用于提取 usage 字段。
type usageEnvelope struct {
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
		// 推理模型的思维链用量。已实测确认它被计入 completion_tokens
		// （prompt 88 + completion 141 = total 229），故仅用于可观测性，
		// 不参与配额扣减，否则会重复计费。
		CompletionDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	// 图片生成按次计费，用 data 数组长度计量
	Data []json.RawMessage `json:"data"`
}

// ParseUsage 从响应体或流式 chunk 中提取用量。
func (v *Volc) ParseUsage(ep Endpoint, payload []byte) (Usage, bool) {
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

	// 图片生成按生成张数计费，与 token 用量互不相干。
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

// 火山在 429 上复用了多种语义，必须靠错误码文本区分额度耗尽与普通限流。
var quotaExhaustedMarkers = []string{
	"quotaexceeded",
	"exceededquota",
	"quota_exceeded",
	"insufficientbalance",
	"accountoverdue",
	"freetrialexpired",
	"exceeded the free quota",
	"额度已用完",
	"额度不足",
	"余额不足",
}

// 表明 Key 本身不可用（而非临时故障）的标志。
var invalidKeyMarkers = []string{
	"authenticationerror",
	"invalidapikey",
	"invalid api key",
	"api key not found",
	"accountdisabled",
	"accessdenied",
}

// MapError 将火山状态码与响应体映射为统一错误。
//
// 对应 docs/scheduler-solution.md 13.1 错误码处理矩阵。
func (v *Volc) MapError(statusCode int, body []byte) *UpstreamError {
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
		// 403 也可能是额度耗尽（火山对超额账号有时返回 403）
		if containsAny(probe, quotaExhaustedMarkers) {
			e.Class = ErrClassQuota
			e.ClientStatus = http.StatusServiceUnavailable
			e.Code = "service_busy"
		} else {
			e.Class = ErrClassAuth
			e.ClientStatus = http.StatusForbidden
			e.Code = "forbidden"
		}

	case statusCode == http.StatusNotFound:
		// 模型不存在，换 Key 无用
		e.Class = ErrClassClient
		e.ClientStatus = http.StatusNotFound
		e.Code = "model_not_found"

	case statusCode == http.StatusTooManyRequests:
		if containsAny(probe, quotaExhaustedMarkers) {
			// 额度耗尽: 上层须把该 Key 的本地水位打满，否则会反复选中它
			e.Class = ErrClassQuota
			e.ClientStatus = http.StatusServiceUnavailable
			e.Code = "service_busy"
		} else {
			// 纯速率限制: 冷却后仍可用。对用户侧改报 503，避免客户端把
			// 上游限流误当成自己触发了本网关的用户级限流而停止重试。
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
		if containsAny(probe, invalidKeyMarkers) {
			// 少数情况下火山用 400 报 Key 问题
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

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// 编译期断言 Volc 实现了 Adapter。
var _ Adapter = (*Volc)(nil)
