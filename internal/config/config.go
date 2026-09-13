// Package config 定义 FluxKeys 网关的全部可配置项。
//
// 配置来源优先级: 环境变量 > YAML 文件 > 内置默认值。
package config

import (
	"net/url"
	"sort"
)

// 本文件只放根配置类型 Config 及其直接访问器。
// 其余类型按领域分在 types.go / quota_limits.go / scheduler_conf.go，
// 加载与校验收敛在 load.go / env.go / validate.go。

// Config 是网关的根配置。
type Config struct {
	Server    Server              `yaml:"server"`
	Redis     Redis               `yaml:"redis"`
	Postgres  Postgres            `yaml:"postgres"`
	Quota     Quota               `yaml:"quota"`
	Egress    Egress              `yaml:"egress"`
	Providers map[string]Provider `yaml:"providers"` // 多上游配置（替代 Upstream）
	Upstream  UpstreamCommon      `yaml:"upstream"`  // 通用重试配置
	Scheduler Scheduler           `yaml:"scheduler"`
	Refresh   Refresh             `yaml:"refresh"`
	Fallback  Fallback            `yaml:"fallback"`
	Admin     Admin               `yaml:"admin"`

	// DefaultProvider 是省略 provider 时的兜底上游。
	//
	// 存在的理由: 导入 Key 与模型路由都需要一个 provider，但单上游部署下
	// 强制每次显式指定纯属噪音。留空时若只配了一个 provider 则自动采用它，
	// 多 provider 场景则必须显式指定，避免静默路由到错误上游。
	DefaultProvider string `yaml:"default_provider"`
}

// ResolveProvider 补全省略的 provider。
//
// 规则: 显式值原样返回；留空时仅在 DefaultProvider 已配置、或全局只有一个
// provider 时才推断，多 provider 且未配默认值时返回空串由调用方报错。
func (c *Config) ResolveProvider(provider string) string {
	if provider != "" {
		return provider
	}
	if c.DefaultProvider != "" {
		return c.DefaultProvider
	}
	if len(c.Providers) == 1 {
		for name := range c.Providers {
			return name
		}
	}
	return ""
}

// EgressVerifyTarget 返回出口自检应当拨测的 host:port。
//
// 优先取显式配置的 verify_target；未配置时从实际启用的 provider base_url
// 推导，而**不是**退回某个写死的厂商域名。
//
// 为什么不能写死: 自检的意义是「从这个出口能否连到我们真正要发请求的地方」。
// 目标一旦与真实上游不同，自检就变成了对无关域名的连通性测试 —— 它会在
// 真实上游被出口 IP 拉黑时依旧全绿，恰好在最需要它报警时失效。这个偏差
// 在多 provider 化后必然出现: 配置里是商汤，自检却在拨火山。
//
// 多 provider 时取 default_provider，无默认值则按名称排序取第一个，
// 保证同一份配置每次得到相同目标（便于运维对照日志）。
func (c *Config) EgressVerifyTarget() string {
	if c.Egress.VerifyTarget != "" {
		return c.Egress.VerifyTarget
	}
	if len(c.Providers) == 0 {
		return ""
	}

	name := c.ResolveProvider("")
	if _, ok := c.Providers[name]; !ok {
		names := make([]string, 0, len(c.Providers))
		for n := range c.Providers {
			names = append(names, n)
		}
		sort.Strings(names)
		name = names[0]
	}

	u, err := url.Parse(c.Providers[name].BaseURL)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "http" {
		return u.Hostname() + ":80"
	}
	return u.Hostname() + ":443"
}

// intPtr 返回 int 指针，用于默认配置中的可选字段。
func intPtr(v int) *int {
	return &v
}
