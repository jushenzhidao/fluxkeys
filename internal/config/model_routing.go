// Package config 定义 FluxKeys 网关的全部可配置项。
//
// 配置来源优先级: 环境变量 > YAML 文件 > 内置默认值。
package config

import (
	"sort"
	"strings"
)

// 本文件是「模型名 → provider / 上游模型名 / 计费量纲」的唯一解析处。
// 三类模型声明（model_mapping / count_models / reasoning_models）的匹配语义各不相同，
// 集中在此以免调用方各自实现出不一致的判定。

// IsCountModel 判断模型在指定 provider 上是否按次计费。
//
// 先把 model 归一化到上游名再匹配。调用方（gateway 的 quotaKindFor）传入的是
// 用户请求里的**对外名**，而 count_models 通常按**上游名**书写 —— 例如商汤配
// model_mapping: {gpt-4: SenseChat-5} 且 count_models: [SenseChat-5]。不归一化
// 时 "gpt-4" 匹配不上 "SenseChat-5"，按次计费的模型会被当成 token 型计量:
// 配额扣的是估算 token 而非调用次数，count 档位的额度永远扣不动，
// 而 token 档位被凭空消耗。两侧都能命中才是安全的。
func (c *Config) IsCountModel(provider, model string) bool {
	p, ok := c.Providers[provider]
	if !ok {
		return false
	}
	upstream := c.UpstreamModel(provider, model)
	for _, m := range p.CountModels {
		if strings.EqualFold(m, model) || strings.EqualFold(m, upstream) {
			return true
		}
	}
	return false
}

// IsReasoningModel 判断模型在指定 provider 上是否会输出思维链。
//
// 用子串匹配而非全等: 火山推理模型名带版本后缀（deepseek-v4-flash-ga-260731），
// 且新版本会持续发布，枚举全名会漏掉未来的型号 —— 而漏判的后果是预扣不足、
// 额度超刷，比误判（预扣偏高、并发略降）严重得多。
func (c *Config) IsReasoningModel(provider, model string) bool {
	if model == "" {
		return false
	}
	p, ok := c.Providers[provider]
	if !ok {
		return false
	}
	// 同 IsCountModel: 调用方传对外名，配置按上游名书写，两侧都要匹配。
	// 漏判在这里的代价是预扣不足、额度超刷。
	lower := strings.ToLower(model)
	lowerUp := strings.ToLower(c.UpstreamModel(provider, model))
	for _, m := range p.ReasoningModels {
		if m == "" {
			continue
		}
		needle := strings.ToLower(m)
		if strings.Contains(lower, needle) || strings.Contains(lowerUp, needle) {
			return true
		}
	}
	return false
}

// UpstreamModel 返回模型在指定 provider 上的实际名称。
func (c *Config) UpstreamModel(provider, model string) string {
	p, ok := c.Providers[provider]
	if !ok {
		return model
	}
	if v, ok := p.ModelMapping[model]; ok && v != "" {
		return v
	}
	return model
}

// ProviderForModel 从模型名推断 provider。
//
// 返回第一个声明支持该模型的 provider；找不到时返回空串，由调用方回
// 404「模型在所有上游中均不存在」。
//
// 遍历顺序必须是确定的。map 的遍历顺序在 Go 中是随机的，因此当两个 provider
// 声明了同一个对外模型名时，同一份配置、同一个请求会随机落到不同上游 ——
// 两边都能正常返回 200，只是结果来自不同厂商。「同一个模型回答风格不一致」
// 这种症状，几乎不会有人往路由上想。
//
// 定序规则是「按 provider 名排序取第一个」: 稳定、可预期，运维看配置就能
// 推断出唯一的落点，不需要知道运行时选了什么。
func (c *Config) ProviderForModel(model string) string {
	if model == "" {
		return ""
	}
	names := make([]string, 0, len(c.Providers))
	for name := range c.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if c.Providers[name].claims(model) {
			return name
		}
	}
	return ""
}

// claims 判断该 provider 是否认领某个模型名。
//
// 三类声明都要看。只查 model_mapping 会让「按次计费模型」与「推理模型」一律
// 404 —— 这两类模型在配置里通常不改名，因此不会出现在 model_mapping 中，
// 而 count_models / reasoning_models 里明明写了它们。
func (p Provider) claims(model string) bool {
	// 对外名
	if _, ok := p.ModelMapping[model]; ok {
		return true
	}
	// 上游名（允许调用方直接用上游模型名）
	for _, upstream := range p.ModelMapping {
		if upstream == model {
			return true
		}
	}
	// 按次计费模型
	for _, m := range p.CountModels {
		if strings.EqualFold(m, model) {
			return true
		}
	}
	// 推理模型。此前注释声称会一并遍历，实际漏了这一支 ——
	// 只在 reasoning_models 里声明过的模型会全部 404。
	for _, m := range p.ReasoningModels {
		if strings.EqualFold(m, model) {
			return true
		}
	}
	return false
}
