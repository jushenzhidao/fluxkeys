package config

import "testing"

// 两个 provider 声明了同一个对外模型名时，路由必须稳定落在同一个上游。
//
// 旧实现遍历 map 取「第一个命中」，而 Go 的 map 遍历顺序是随机的 ——
// 同一份配置、同一个请求会随机打到不同厂商，两边都返回 200，
// 只是结果来源不同。「同一个模型回答风格不一样」没人会往路由上想。
//
// 迭代次数取 500: 两个 key 的 map 上，随机顺序命中任一方的概率各半，
// 500 次全落同一侧的概率约为 2^-499 —— 只要实现是不确定的，这个用例必然捕获。
func TestProviderForModel_同名模型路由稳定(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"zebra": {
				ModelMapping: map[string]string{"gpt-4": "zebra-model-v1"},
			},
			"alpha": {
				ModelMapping: map[string]string{"gpt-4": "alpha-model-v1"},
			},
		},
	}

	// 定序规则: 按 provider 名排序取第一个 → "alpha"
	const want = "alpha"
	for i := 0; i < 500; i++ {
		if got := cfg.ProviderForModel("gpt-4"); got != want {
			t.Fatalf("第 %d 次迭代路由到 %q，期望 %q —— 同名模型的路由不确定", i+1, got, want)
		}
	}
}

// 只用上游名调用时同样要能定位到 provider（两个 provider 都不以它作为对外名）。
func TestProviderForModel_按上游名定位(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"volc": {
				ModelMapping: map[string]string{"gpt-4": "deepseek-v3-241226"},
			},
		},
	}
	if got := cfg.ProviderForModel("deepseek-v3-241226"); got != "volc" {
		t.Errorf("按上游名定位 = %q, 期望 volc", got)
	}
}

// count_models 里声明的模型（按次计费，通常不改名）必须能被认领。
func TestProviderForModel_认领CountModels(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"volc": {
				ModelMapping: map[string]string{},
				CountModels:  []string{"seedream-3.0"},
			},
		},
	}
	if got := cfg.ProviderForModel("seedream-3.0"); got != "volc" {
		t.Errorf("count_models 中的模型 = %q, 期望 volc", got)
	}
}

// reasoning_models 里声明的模型同样要被认领。
//
// 这是旧实现的漏查: 注释声称「CountModels / ReasoningModels 里声明过的模型
// 同样算认领」，代码却只遍历了 CountModels。只在 reasoning_models 里写过的
// 模型会一律 404 —— 而调用方拿到的文案是「模型在所有上游中均不存在」，
// 与「配置里明明写了」直接矛盾。
func TestProviderForModel_认领ReasoningModels(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"volc": {
				ModelMapping:    map[string]string{},
				ReasoningModels: []string{"deepseek-v4-pro"},
			},
		},
	}
	if got := cfg.ProviderForModel("deepseek-v4-pro"); got != "volc" {
		t.Errorf("reasoning_models 中的模型 = %q, 期望 volc（旧实现漏查该分支）", got)
	}
	// 子串语义由 IsReasoningModel 负责，ProviderForModel 只做全等认领 ——
	// 拿带版本后缀的名字来问不应命中，否则会把任意含 "deepseek" 的串都认领。
	if got := cfg.ProviderForModel("deepseek-v4-pro-ga-260731"); got != "" {
		t.Errorf("带版本后缀的名字不应被全等认领，实际 = %q", got)
	}
}

func TestProviderForModel_未声明返回空(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"volc": {ModelMapping: map[string]string{"gpt-4": "x"}},
		},
	}
	if got := cfg.ProviderForModel("unknown-model"); got != "" {
		t.Errorf("未声明模型 = %q, 期望空串", got)
	}
	if got := cfg.ProviderForModel(""); got != "" {
		t.Errorf("空模型名 = %q, 期望空串", got)
	}
}
