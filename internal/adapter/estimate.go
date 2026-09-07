package adapter

import (
	"encoding/json"
	"unicode/utf8"
)

// ChatRequestMeta 是从请求体中提取的、调度与配额需要的元信息。
type ChatRequestMeta struct {
	// Model 是对外模型名。
	Model string
	// Stream 标识是否流式。
	Stream bool
	// MaxTokens 是用户声明的最大输出长度，未指定则为 0。
	MaxTokens int64
	// PromptChars 是输入内容的字符数（rune 计），用于估算 prompt_tokens。
	PromptChars int64
	// N 是图片生成张数或候选数，用于按次计费的预扣。
	N int64
	// StreamOptionsPresent 标识用户是否已显式设置 stream_options。
	StreamOptionsPresent bool
	// IncludeUsage 标识用户是否已要求流式返回 usage。
	IncludeUsage bool
}

// 中文场景的字符/Token 比。
//
// 英文约 4 字符 1 token，中文约 1.5 字符 1 token。取 2.5 作为混合场景的
// 折中值：偏保守（估多于实），因为预扣估少会导致真实用量超过硬水位，
// 而预扣估多只是短暂占用额度，租约结束即释放。
const charsPerToken = 2.5

// EstimatePromptTokens 按字符数估算 prompt token 数。
func EstimatePromptTokens(chars int64) int64 {
	if chars <= 0 {
		return 0
	}
	n := int64(float64(chars)/charsPerToken) + 1
	return n
}

// ParseChatMeta 从 chat/completions 请求体中提取元信息。
//
// P2 修正: V3 的预扣只算了 max_tokens，漏掉了 prompt_tokens。长上下文
// 请求（如 32K 输入 + 512 输出）会被严重低估，实际用量可能是预扣的 60 倍，
// 直接击穿硬水位。此处必须把输入也纳入估算。
func ParseChatMeta(body []byte) (ChatRequestMeta, error) {
	var raw struct {
		Model     string          `json:"model"`
		Stream    *bool           `json:"stream"`
		MaxTokens *int64          `json:"max_tokens"`
		MaxCompat *int64          `json:"max_completion_tokens"`
		N         *int64          `json:"n"`
		Messages  json.RawMessage `json:"messages"`
		Input     json.RawMessage `json:"input"`
		Prompt    json.RawMessage `json:"prompt"`
		Tools     json.RawMessage `json:"tools"`
		StreamOpt *struct {
			IncludeUsage *bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ChatRequestMeta{}, NewClientError(400, "invalid_request", "请求体不是合法 JSON: "+err.Error())
	}

	m := ChatRequestMeta{Model: raw.Model}
	if raw.Stream != nil {
		m.Stream = *raw.Stream
	}
	switch {
	case raw.MaxTokens != nil:
		m.MaxTokens = *raw.MaxTokens
	case raw.MaxCompat != nil:
		m.MaxTokens = *raw.MaxCompat
	}
	if raw.N != nil {
		m.N = *raw.N
	}
	if raw.StreamOpt != nil {
		m.StreamOptionsPresent = true
		if raw.StreamOpt.IncludeUsage != nil {
			m.IncludeUsage = *raw.StreamOpt.IncludeUsage
		}
	}

	// messages / input / prompt / tools 都计入输入成本。tools 的 JSON Schema
	// 会被完整送入模型，长 schema 的 token 开销不容忽视。
	m.PromptChars = countChars(raw.Messages) + countChars(raw.Input) +
		countChars(raw.Prompt) + countChars(raw.Tools)

	return m, nil
}

// countChars 统计 JSON 片段中所有字符串字面量的字符数。
//
// 只数字符串内容而非整个 JSON 长度，避免把 role/content 这类结构性字段名
// 算进去导致高估。递归处理嵌套的多模态 content 数组。
func countChars(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// 解析失败时退化为按原始长度估算，保证不会返回 0 而低估预扣
		return int64(utf8.RuneCount(raw))
	}
	return countValue(v)
}

func countValue(v any) int64 {
	switch t := v.(type) {
	case string:
		return int64(utf8.RuneCountInString(t))
	case []any:
		var n int64
		for _, item := range t {
			n += countValue(item)
		}
		return n
	case map[string]any:
		var n int64
		for k, item := range t {
			// base64 图片等超长 data URL 不按字符计价，跳过以免估算爆炸
			if k == "url" || k == "b64_json" || k == "data" {
				if s, ok := item.(string); ok && len(s) > 512 {
					// 图片按固定成本近似（约 1000 token 量级）
					n += 2500
					continue
				}
			}
			n += countValue(item)
		}
		return n
	}
	return 0
}

// ReasoningEstimate 描述推理模型的输出预扣修正参数。
//
// 零值表示非推理模型，此时估算退化为原有公式。
type ReasoningEstimate struct {
	// Enabled 标识当前模型会输出思维链。
	Enabled bool
	// OutputMultiplier 是输出部分的额外放大系数。
	OutputMultiplier float64
	// FloorTokens 是输出部分的预扣下限。
	FloorTokens int64
}

// EstimateTokens 计算一次请求应当预扣的 token 数。
//
// 公式: (prompt_tokens_est + output_est) * multiplier
//
// 其中 output_est 对普通模型即 max_tokens；对推理模型则额外放大并托底，
// 因为 reasoning_content 的 token 不受 max_tokens 约束（详见 applyReasoning）。
//
// defaultMaxTokens 用于用户未指定 max_tokens 的情况 —— 此时输出长度完全
// 不可知，只能按配置的基准值预扣，靠 Commit 时的实际用量修正。
func EstimateTokens(m ChatRequestMeta, defaultMaxTokens int64, multiplier float64) int64 {
	return EstimateTokensFor(m, defaultMaxTokens, multiplier, ReasoningEstimate{})
}

// EstimateTokensFor 是 EstimateTokens 的带推理修正版本。
func EstimateTokensFor(m ChatRequestMeta, defaultMaxTokens int64, multiplier float64, r ReasoningEstimate) int64 {
	out := m.MaxTokens
	explicit := out > 0
	if out <= 0 {
		out = defaultMaxTokens
	}
	if out <= 0 {
		out = 4096
	}

	// 推理模型的输出放大必须在乘 n 之前完成: 每份候选都会各自产生一条
	// 独立的思维链，放大属于「单份输出成本」而非「总量的修正」。
	out = applyReasoning(out, explicit, r)

	// n > 1 时会生成多份输出，成本相应放大
	if m.N > 1 {
		out *= m.N
	}

	base := EstimatePromptTokens(m.PromptChars) + out
	if multiplier < 1 {
		multiplier = 1
	}
	est := int64(float64(base) * multiplier)
	if est <= 0 {
		est = 1
	}
	return est
}

// applyReasoning 修正推理模型的单份输出预扣量。
//
// 实测（deepseek-v4-flash，真实上游）:
//
//	max_tokens=16   → completion 141（8.8x）
//	max_tokens=64   → completion 121（1.89x）
//	max_tokens=256  → completion 214（0.84x）
//	max_tokens=1024 → completion 227（0.22x）
//
// 规律是思维链长度由问题复杂度决定，与 max_tokens 基本无关。因此:
//   - 按比例放大处理 max_tokens 处于中间量级的情况；
//   - 绝对下限处理 max_tokens 极小的情况（16 × 3 = 48 仍远不够）。
//
// 未显式指定 max_tokens 时不套用下限: 此时 out 已是配置基准值（默认 4096），
// 本身高于任何观测到的思维链长度，再放大只会白占额度、压低单 Key 并发。
func applyReasoning(out int64, explicit bool, r ReasoningEstimate) int64 {
	if !r.Enabled {
		return out
	}
	if r.OutputMultiplier > 1 {
		out = int64(float64(out) * r.OutputMultiplier)
	}
	if explicit && r.FloorTokens > out {
		out = r.FloorTokens
	}
	return out
}

// EstimateCountUnits 计算按次计费模型应预扣的次数。
func EstimateCountUnits(m ChatRequestMeta) int64 {
	if m.N > 0 {
		return m.N
	}
	return 1
}

// EnsureStreamUsage 为流式请求补齐 stream_options.include_usage=true。
//
// 这是流式配额修正的前提: 不设该选项，火山不会在最后一个 chunk 里返回
// usage，网关就只能拿估算值 Commit，误差会随流量累积成系统性偏差。
// 已由用户显式设置的不覆盖 —— 用户可能有意关闭。
func EnsureStreamUsage(body []byte) ([]byte, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	if _, exists := m["stream_options"]; exists {
		return body, false
	}
	m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}
