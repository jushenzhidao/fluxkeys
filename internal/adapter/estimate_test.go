package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseChatMeta_提取基本字段(t *testing.T) {
	body := []byte(`{"model":"deepseek-v3","stream":true,"max_tokens":512,
		"messages":[{"role":"user","content":"你好世界"}]}`)

	m, err := ParseChatMeta(body)
	if err != nil {
		t.Fatalf("ParseChatMeta 失败: %v", err)
	}
	if m.Model != "deepseek-v3" {
		t.Errorf("Model = %q", m.Model)
	}
	if !m.Stream {
		t.Error("Stream 应为 true")
	}
	if m.MaxTokens != 512 {
		t.Errorf("MaxTokens = %d", m.MaxTokens)
	}
	// "user" + "你好世界" = 4 + 4 = 8 个字符
	if m.PromptChars != 8 {
		t.Errorf("PromptChars = %d, 期望 8", m.PromptChars)
	}
}

func TestParseChatMeta_兼容max_completion_tokens(t *testing.T) {
	m, err := ParseChatMeta([]byte(`{"model":"m","max_completion_tokens":256,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.MaxTokens != 256 {
		t.Errorf("MaxTokens = %d, 期望 256", m.MaxTokens)
	}
}

func TestParseChatMeta_统计多模态嵌套内容(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"描述这张图"},
		{"type":"image_url","image_url":{"url":"https://x/y.png"}}
	]}]}`)
	m, err := ParseChatMeta(body)
	if err != nil {
		t.Fatal(err)
	}
	if m.PromptChars == 0 {
		t.Error("嵌套 content 数组的字符应被统计")
	}
}

func TestParseChatMeta_超长base64图片按固定成本近似而非爆炸式高估(t *testing.T) {
	long := strings.Repeat("A", 100_000)
	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": long}},
			},
		}},
	})
	m, err := ParseChatMeta(body)
	if err != nil {
		t.Fatal(err)
	}
	// 若按字符直算会得到 10 万字符 → 4 万 token 的荒谬预扣
	if m.PromptChars > 10_000 {
		t.Errorf("PromptChars = %d, 超长 data URL 应按固定成本近似", m.PromptChars)
	}
}

func TestParseChatMeta_非法JSON返回客户端错误(t *testing.T) {
	_, err := ParseChatMeta([]byte(`{not json`))
	if err == nil {
		t.Fatal("期望报错")
	}
	if ue, ok := err.(*UpstreamError); !ok || ue.Class != ErrClassClient {
		t.Errorf("期望客户端错误, got %v", err)
	}
}

func TestEstimateTokens_预扣包含prompt与输出(t *testing.T) {
	// 这是 V3 的已知缺陷: 只算 max_tokens 会严重低估长上下文请求
	m := ChatRequestMeta{PromptChars: 25000, MaxTokens: 512}
	est := EstimateTokens(m, 4096, 1.2)

	promptEst := EstimatePromptTokens(25000) // 约 10000
	want := int64(float64(promptEst+512) * 1.2)
	if est != want {
		t.Errorf("EstimateTokens = %d, 期望 %d", est, want)
	}
	// 只按 max_tokens 算的话是 614，实际应在 12000 量级
	if est < 10000 {
		t.Errorf("长上下文预扣被低估: %d", est)
	}
}

func TestEstimateTokens_未指定max_tokens时用配置基准(t *testing.T) {
	m := ChatRequestMeta{PromptChars: 100}
	est := EstimateTokens(m, 4096, 1.0)
	if est < 4096 {
		t.Errorf("EstimateTokens = %d, 应至少包含默认 4096", est)
	}
}

func TestEstimateTokens_n大于1时放大输出成本(t *testing.T) {
	one := EstimateTokens(ChatRequestMeta{MaxTokens: 100, N: 1}, 4096, 1.0)
	three := EstimateTokens(ChatRequestMeta{MaxTokens: 100, N: 3}, 4096, 1.0)
	if three <= one {
		t.Errorf("n=3 的预扣 %d 应大于 n=1 的 %d", three, one)
	}
}

func TestEstimateTokens_倍数小于1时被钳制(t *testing.T) {
	m := ChatRequestMeta{MaxTokens: 1000}
	if got := EstimateTokens(m, 4096, 0.5); got < 1000 {
		t.Errorf("EstimateTokens = %d, 倍数不应缩小预扣", got)
	}
}

func TestEstimateTokens_结果恒为正(t *testing.T) {
	if got := EstimateTokens(ChatRequestMeta{}, 0, 1); got <= 0 {
		t.Errorf("EstimateTokens = %d, 必须为正（Acquire 拒绝非正数）", got)
	}
}

func TestEstimateCountUnits(t *testing.T) {
	if got := EstimateCountUnits(ChatRequestMeta{}); got != 1 {
		t.Errorf("默认应为 1, got %d", got)
	}
	if got := EstimateCountUnits(ChatRequestMeta{N: 4}); got != 4 {
		t.Errorf("应按 n 计次, got %d", got)
	}
}

func TestEnsureStreamUsage_补齐include_usage(t *testing.T) {
	out, changed := EnsureStreamUsage([]byte(`{"model":"m","stream":true}`))
	if !changed {
		t.Fatal("应补齐 stream_options")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	opt, ok := got["stream_options"].(map[string]any)
	if !ok || opt["include_usage"] != true {
		t.Errorf("stream_options = %v", got["stream_options"])
	}
}

func TestEnsureStreamUsage_不覆盖用户显式设置(t *testing.T) {
	in := []byte(`{"model":"m","stream":true,"stream_options":{"include_usage":false}}`)
	out, changed := EnsureStreamUsage(in)
	if changed {
		t.Error("用户已显式设置时不应覆盖")
	}
	if string(out) != string(in) {
		t.Errorf("应原样返回, got %s", out)
	}
}

func TestEstimatePromptTokens_零值与正值(t *testing.T) {
	if got := EstimatePromptTokens(0); got != 0 {
		t.Errorf("EstimatePromptTokens(0) = %d", got)
	}
	if got := EstimatePromptTokens(1000); got < 350 || got > 450 {
		t.Errorf("EstimatePromptTokens(1000) = %d, 期望约 400", got)
	}
}
