// Package config 定义 FluxKeys 网关的全部可配置项。
//
// 配置来源优先级: 环境变量 > YAML 文件 > 内置默认值。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// 本文件负责 YAML 加载与配置文本解析。

// Load 读取配置文件并叠加环境变量覆盖。path 为空时仅使用默认值 + 环境变量。
func Load(path string) (*Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: 读取 %s: %w", path, err)
		}
		// 严格模式: 未知字段直接报错而非静默丢弃。
		//
		// 静默丢弃的代价在生产上是隐蔽且昂贵的 —— 把 admin.api_key 误写成
		// admin.token 会让整组管理路由不注册（server.go 以 APIKey 为空作为
		// 禁用信号），把 egress.ips 误写成 bind_ips 会让多出口静默降级为
		// direct 单出口。两者都表现为「配置写了但功能不存在」，且启动日志
		// 一切正常，只能靠逐行比对结构体标签才能发现。
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("config: 解析 %s: %w", path, err)
		}

		// providers 必须整体替换默认值，不能合并。
		//
		// yaml 对 map 是逐键合并的，于是 Default() 里的 "volc" 会残留在
		// 只配了 sensenova 的部署上。危害是双重的:
		//
		//  1. 模型静默路由到未部署的上游 —— seedream-3.0 命中 volc 的
		//     count_models，请求被送去一个从未导入过 Key 的 provider，
		//     表现为「配额耗尽」而非「模型不存在」，排查方向完全错。
		//  2. ResolveProvider 的「只有一个 provider 时自动推断」永久失效，
		//     因为 map 里永远有两个键。单上游部署被迫每次显式写 provider，
		//     而这正是 DefaultProvider 想消除的噪音。
		//
		// 判据用「文件里是否出现 providers 键」而非「解析后是否非空」:
		// 后者无法区分「没配」与「配成空 map」，而显式配空 map 应当报错
		// （Validate 会拦），不该悄悄回落到 volc 默认值。
		var probe struct {
			Providers map[string]Provider `yaml:"providers"`
		}
		if err := yaml.Unmarshal(data, &probe); err == nil && probe.Providers != nil {
			cfg.Providers = probe.Providers
		}
	}

	applyEnv(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// DefaultMaxKeysPerIP 是未指定 max_keys 时单个出口 IP 的 Key 承载上限。
//
// 取 10 是保守值，适用于未做分层、也未收窄行为画像的部署。
// 画像窄化后（见 internal/persona）同 IP 的并发密度大幅下降，
// warm/cold 档可显著调高 —— 但这必须显式配置，不做隐式放宽。
//
// 导出给它处复用（装配层为「扫描本机地址」模式的分档计划兜底）：这个数字同时
// 决定了默认容量与单 IP 的账号密度，只应有一处定义 —— 两边各写一份而日后漂移，
// 表现是「同一台机器上两条地址来源给出不同的承载能力」，且不会有任何报错。
const DefaultMaxKeysPerIP = 10

// parseEgressIPs 解析 EGRESS_IPS 环境变量。
//
// 单项语法: <addr>[=<public_ip>][|<pool>[|<max_keys>]]
//
//	172.16.0.11                              仅私网地址
//	172.16.0.11=203.0.113.11                 带公网地址
//	172.16.0.11=203.0.113.11|hot             限定 hot 档
//	172.16.0.11=203.0.113.11|cold|100        限定 cold 档且承载 100 个 Key
//
// 用 '|' 而非 ':' 分隔是为了避免与 IPv6 地址的冒号冲突。
//
// 注意本变量一旦设置会**整体覆盖** YAML 里的 egress.ips，包括分层配置。
// 若已在 YAML 中做了分层，就不要再设置 EGRESS_IPS，否则分层会被静默清掉。
func parseEgressIPs(s string) []EgressIP {
	var out []EgressIP
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// 先切出档位与容量，剩余部分才是 addr[=public]
		pool, maxKeys := "", DefaultMaxKeysPerIP
		if i := strings.Index(part, "|"); i >= 0 {
			rest := part[i+1:]
			part = strings.TrimSpace(part[:i])
			if j := strings.Index(rest, "|"); j >= 0 {
				pool = strings.TrimSpace(rest[:j])
				if n, err := strconv.Atoi(strings.TrimSpace(rest[j+1:])); err == nil && n > 0 {
					maxKeys = n
				}
			} else {
				pool = strings.TrimSpace(rest)
			}
		}

		addr, public := part, ""
		if i := strings.Index(part, "="); i >= 0 {
			addr, public = strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])
		}
		if addr == "" {
			continue
		}
		out = append(out, EgressIP{
			Addr: addr, PublicIP: public, MaxKeys: maxKeys, Pool: pool,
		})
	}
	return out
}

// parseClockOrZero 解析 "HH:MM" 为当日偏移；无法解析时返回 0，交由调用方
// 的 window > 0 判断跳过校验 —— 时钟格式本身的校验不属于这里的职责。
func parseClockOrZero(s string) time.Duration {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
}
