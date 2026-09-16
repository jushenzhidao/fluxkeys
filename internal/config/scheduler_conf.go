// Package config 定义 FluxKeys 网关的全部可配置项。
//
// 配置来源优先级: 环境变量 > YAML 文件 > 内置默认值。
package config

import (
	"fmt"
)

// 本文件承载调度配置的派生计算与自洽性校验。
// 这些规则的共同点是：都要把份额、档位 IP 数、max_keys 三者放在一起看。

// maxReqPerHourPerIP 是单个出口 IP 每小时的请求上限。
//
// 约 0.8 QPS。真人使用 AI 助手的强度远低于此，取这个值是留足余量的同时
// 仍能拦住「某档位份额远超其 IP 数所能承担」这类配置错误。
const maxReqPerHourPerIP = 3000

// validatePoolShareCapacity 检查各档份额是否与该档的出口 IP 数匹配。
//
// 拦的是这类错误: 给 hot 档配 70% 份额，但它只有 4 个 IP —— 20 QPS 下
// 每 IP 每小时要发 12600 次请求，是真人强度的 4 倍。配比本身不提高容量，
// 它只是重新分配压力，配错会把密度问题从一个档位搬到另一个档位。
//
// 仅在 multi_ip 且已配份额时检查。direct 模式没有出口概念，
// 未配份额时全档等权，压力自然按 Key 数量分布，不会失配。
func (c *Config) validatePoolShareCapacity() error {
	if c.Scheduler.NormalizedPoolShares() == nil || c.Egress.Mode != "multi_ip" {
		return nil
	}
	// 扫描模式下各档实际有几个出口，要等装配完成才知道（分档计划 + 本机扫到的
	// 地址数，含「接住剩余」那一档的动态数量）。这里不猜，由装配层在
	// PlanTierIPs 之后调 ValidateEgressCapacity 做同一项校验。
	//
	// 不能让这一项在扫描模式下直接失效: 它是防「某档份额远超其出口承载」的
	// 唯一自动闸门，静默跳过等于开一个开关就丢掉了一层保护。
	if c.Egress.Scanning() {
		return nil
	}
	perPool, generic := countPoolsByEgressIPs(c.Egress.IPs)
	return c.validateShareDensity(perPool, generic)
}

// countPoolsByEgressIPs 统计各档的出口数与通用出口数。
//
// 未标档位的 IP 是通用的，对所有档位等价可用，故单独返回由调用方加到每一档。
func countPoolsByEgressIPs(ips []EgressIP) (perPool map[string]int, generic int) {
	perPool = make(map[string]int, 3)
	for _, ip := range ips {
		if ip.Pool == "" {
			generic++
			continue
		}
		perPool[ip.Pool]++
	}
	return perPool, generic
}

// ValidateEgressCapacity 用**实际**的各档出口数校验份额密度。
//
// 导出给它处调用，是因为扫描模式下这个数字只有装配完成才知道: 分档计划给出
// 每档几个地址，「接住剩余」那一档还要加上本机实际扫到的余额。在配置校验阶段
// 推算这个数只能靠猜，而猜错的代价是拦住一个本来合法的部署。
func (c *Config) ValidateEgressCapacity(perPool map[string]int, generic int) error {
	return c.validateShareDensity(perPool, generic)
}

// validateShareDensity 是份额密度校验的本体，两条地址来源共用。
//
// 拦的是这类错误: 给 hot 档配 70% 份额，但它只有 4 个 IP —— 20 QPS 下
// 每 IP 每小时要发 12600 次请求，是真人强度的 4 倍。配比本身不提高容量，
// 它只是重新分配压力，配错会把密度问题从一个档位搬到另一个档位。
func (c *Config) validateShareDensity(perPool map[string]int, generic int) error {
	shares := c.Scheduler.NormalizedPoolShares()
	if shares == nil || c.Egress.Mode != "multi_ip" {
		return nil
	}
	// 没有任何出口标了档位 → 没在用出口分层，份额与出口数的匹配无从谈起
	// （所有出口对所有档位等价可用）。这时份额只影响 Key 的选取偏好，
	// 不会造成某个出口被过度使用，故跳过检查。
	if len(perPool) == 0 {
		return nil
	}

	// 用预期峰值 QPS 推算，而不是 Scheduler.MaxQPS()。
	//
	// MaxQPS 算的是「全部 Key 同时按最小间隔发请求」的理论上限 ——
	// 那是系统能承受的天花板，不是实际负载。用它做校验会把任何配置都判超标
	// （1000 个 Key ÷ 60s 间隔 = 16 QPS/Key 累计上千 QPS，除以 4 个 IP 必然爆表）。
	//
	// 真实约束是用户的峰值流量。未配置时跳过检查 —— 没有这个数字，
	// 任何推算都是凭空假设，宁可不查也不要给出错误的拦截。
	if c.Scheduler.ExpectedPeakQPS <= 0 {
		return nil
	}
	reqPerHour := c.Scheduler.ExpectedPeakQPS * 3600

	for pool, share := range shares {
		if share <= 0 {
			continue
		}
		ips := perPool[pool] + generic
		if ips == 0 {
			return fmt.Errorf(
				"config: scheduler.pool_shares[%s]=%.0f%% 但出口配置里没有该档位的出口，"+
					"这些流量会全部走回退路径（或直接失败）", pool, share*100)
		}
		perIP := reqPerHour * share / float64(ips)
		if perIP > maxReqPerHourPerIP {
			return fmt.Errorf(
				"config: scheduler.pool_shares[%s]=%.0f%% 与该档 %d 个出口不匹配 —— "+
					"每出口每小时 %.0f 次请求，超过上限 %d（约 0.8 QPS，真人强度）。"+
					"要么下调该档份额，要么给该档增加出口",
				pool, share*100, ips, perIP, maxReqPerHourPerIP)
		}
	}
	return nil
}

// NormalizedPoolShares 返回归一化后的份额，总和为 1。
//
// 返回 nil 表示未启用配比（未配置，或所有份额均为非正数）。
// 调用方应据此退化为全档等权的加权随机。
func (s Scheduler) NormalizedPoolShares() map[string]float64 {
	if len(s.PoolShares) == 0 {
		return nil
	}
	var sum float64
	for _, v := range s.PoolShares {
		if v > 0 {
			sum += v
		}
	}
	if sum <= 0 {
		return nil
	}
	out := make(map[string]float64, len(s.PoolShares))
	for k, v := range s.PoolShares {
		if v > 0 {
			out[k] = v / sum
		}
	}
	return out
}

// MaxQPS 返回当前配置下的理论吞吐上限。
//
// keyCount 传实际装载的活跃 Key 数而非 ActivePoolSize —— 后者只是目标值，
// 新部署时可能只导入了几个 Key，用目标值算会得出一个偏乐观的数字。
//
// MinRequestInterval 为 0 表示不做节流，此时不存在这个上限。
func (s Scheduler) MaxQPS(keyCount int) float64 {
	if s.MinRequestInterval <= 0 || keyCount <= 0 {
		return 0
	}
	return float64(keyCount) / s.MinRequestInterval.Seconds()
}
