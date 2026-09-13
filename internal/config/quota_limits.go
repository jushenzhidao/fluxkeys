// Package config 定义 FluxKeys 网关的全部可配置项。
//
// 配置来源优先级: 环境变量 > YAML 文件 > 内置默认值。
package config

import (
	"strings"
)

// 本文件是「配额水位」规则的唯一权威实现。
// 调用方一律走 LimitsFor / IsCountProvider，不要自行读 ratio 字段做乘法 ——
// 复制品不会跟着这里的规则改，而两边算出来的都是正常数字，对不上也无人察觉。

// TokenHard 返回 Token 型配额的硬水位。
func (q Quota) TokenHard() int64 { return int64(float64(q.TokenLimit) * q.TokenHardRatio) }

// TokenSoft 返回 Token 型软水位。
func (q Quota) TokenSoft() int64 { return int64(float64(q.TokenLimit) * q.TokenSoftRatio) }

// CountHard 返回次数型硬水位。
func (q Quota) CountHard() int64 { return int64(float64(q.CountLimit) * q.CountHardRatio) }

// CountSoft 返回次数型软水位。
func (q Quota) CountSoft() int64 { return int64(float64(q.CountLimit) * q.CountSoftRatio) }

// LimitsFor 返回指定 provider 在该配额类型下的软硬水位。
//
// 优先采用 provider 自己的 quota_limit，仅在其未配置（<=0）时回退到全局
// quota.token_limit / count_limit。比例（hard_ratio / soft_ratio）始终取全局值 ——
// 它表达的是「留多少安全余量」这一运维策略，与上游是谁无关。
//
// 必须按 provider 取额度: 各家上游的单 Key 限额天差地别（火山按 token 给
// 数百万，商汤公测按次给一千多）。用全局值会同时错向两边 ——
// 对额度小的上游是超发（真实额度耗尽后网关仍在放行，请求全部撞上游 429），
// 对额度大的上游是白白闲置。而且这类偏差不会报错，只会表现为「配了
// quota_limit 但完全没用」，极难从日志看出来。
func (c *Config) LimitsFor(provider string, kindCount bool) (hard, soft int64) {
	limit := int64(0)
	if p, ok := c.Providers[provider]; ok {
		limit = p.QuotaLimit
	}
	if limit <= 0 {
		if kindCount {
			limit = c.Quota.CountLimit
		} else {
			limit = c.Quota.TokenLimit
		}
	}
	if kindCount {
		return int64(float64(limit) * c.Quota.CountHardRatio),
			int64(float64(limit) * c.Quota.CountSoftRatio)
	}
	return int64(float64(limit) * c.Quota.TokenHardRatio),
		int64(float64(limit) * c.Quota.TokenSoftRatio)
}

// IsCountProvider 判断 provider 的配额口径是否为「按次」。
//
// 单独给出这个方法而不是让调用方各自读 QuotaKind: 归档、展示、对账都需要
// 知道「这个 provider 的额度该拿哪个量纲去比」。少一处判断就会拿 token 数
// 去除以按次额度 —— 两个数都真实存在、都非零，比值也在 [0,1] 里，没有任何
// 一层会报错，只会让历史打分读到一个毫无意义的小数。
func (c *Config) IsCountProvider(provider string) bool {
	p, ok := c.Providers[provider]
	if !ok {
		return false
	}
	return strings.EqualFold(p.QuotaKind, "count")
}
