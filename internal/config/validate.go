// Package config 定义 FluxKeys 网关的全部可配置项。
//
// 配置来源优先级: 环境变量 > YAML 文件 > 内置默认值。
package config

import (
	"fmt"
	"time"
)

// 本文件是启动前配置校验的唯一入口。
// 校验失败一律拒绝启动 —— 带着错配置跑起来，症状会在几小时后以超刷或封号的形式出现。

// Validate 校验配置的自洽性，尽早暴露错误而非在运行时才失败。
func (c *Config) Validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("config: server.addr 不能为空")
	}
	if c.Redis.Addr == "" {
		return fmt.Errorf("config: redis.addr 不能为空")
	}

	switch c.Egress.Mode {
	case "direct":
	case "multi_ip":
		if len(c.Egress.IPs) == 0 {
			return fmt.Errorf("config: egress.mode=multi_ip 必须配置 egress.ips")
		}
	default:
		return fmt.Errorf("config: egress.mode 非法 %q（应为 direct 或 multi_ip）", c.Egress.Mode)
	}

	// 阈值 1 意味着任何单个 Key 被上游禁用都会连带撤离整个出口 ——
	// 那正是本判据要避免的误判。2 同样过于激进（两个 Key 恰好同时出问题
	// 并不罕见），而误判的代价是一次性制造大批「换了出口的老账号」。
	if c.Egress.BanDetectKeys != 0 && c.Egress.BanDetectKeys < 3 {
		return fmt.Errorf(
			"config: egress.ban_detect_keys=%d 过低，会把单个 Key 被禁误判为出口被封"+
				"（应为 0 关闭，或 >=3）", c.Egress.BanDetectKeys)
	}
	if c.Egress.BanDetectKeys > 0 && c.Egress.BanDetectWindow <= 0 {
		return fmt.Errorf("config: 启用 egress.ban_detect_keys 必须设置 ban_detect_window")
	}

	// max 小于 base 会让指数退避彻底失效: backoff 先按 2 的幂次累乘，
	// 再被 maxWait 截断，于是每次封禁都只等 max —— 反复被封的脏出口
	// 与首次被封的出口等同处理，而前者恰恰最需要长时间冷却。
	// 这个错配不会报错也不影响功能，只是退避默默不生效。
	if c.Egress.BanCooldown > 0 && c.Egress.BanCooldownMax < c.Egress.BanCooldown {
		return fmt.Errorf(
			"config: egress.ban_cooldown_max=%s 小于 ban_cooldown=%s，指数退避会失效"+
				"（每次封禁都只等 max）", c.Egress.BanCooldownMax, c.Egress.BanCooldown)
	}

	// 档位名写错（如 "Hot" / "hot " / "hott"）不会报错，只会让那份额永远
	// 抽不到 Key，从而静默退化 —— 配比看着配了却完全没生效。
	for pool, share := range c.Scheduler.PoolShares {
		switch pool {
		case "hot", "warm", "cold":
		default:
			return fmt.Errorf(
				"config: scheduler.pool_shares 含未知档位 %q（应为 hot / warm / cold）", pool)
		}
		if share < 0 {
			return fmt.Errorf("config: scheduler.pool_shares[%s]=%v 不能为负", pool, share)
		}
	}
	// 配了 pool_shares 但全是 0 等同于没配。这几乎一定是笔误，
	// 而它的表现是「配比静默失效」，比直接报错难查得多。
	if len(c.Scheduler.PoolShares) > 0 && c.Scheduler.NormalizedPoolShares() == nil {
		return fmt.Errorf("config: scheduler.pool_shares 全为 0，至少一个档位需为正数")
	}
	if err := c.validatePoolShareCapacity(); err != nil {
		return err
	}

	if c.Quota.TokenHardRatio <= 0 || c.Quota.TokenHardRatio > 1 {
		return fmt.Errorf("config: quota.token_hard_ratio 应在 (0,1] 区间")
	}
	if c.Quota.TokenSoftRatio >= c.Quota.TokenHardRatio {
		return fmt.Errorf("config: 软水位比例必须小于硬水位比例")
	}
	if c.Quota.CountSoftRatio >= c.Quota.CountHardRatio {
		return fmt.Errorf("config: 次数型软水位比例必须小于硬水位比例")
	}
	if c.Quota.ReapInterval >= c.Quota.LeaseTTL {
		return fmt.Errorf("config: quota.reap_interval 必须短于 lease_ttl，否则泄漏无法及时回收")
	}
	if c.Quota.EstimateMultiplier < 1 {
		return fmt.Errorf("config: quota.estimate_multiplier 不应小于 1")
	}
	// 小于 1 会让推理模型的预扣低于常规模型，与该系数的设计意图相反。
	if c.Quota.ReasoningOutputMultiplier < 1 {
		return fmt.Errorf("config: quota.reasoning_output_multiplier 不应小于 1")
	}
	if c.Quota.ReasoningFloorTokens < 0 {
		return fmt.Errorf("config: quota.reasoning_floor_tokens 不应为负")
	}

	// P1-10: 开启付费渠道 fallback 必须同时设定预算上限
	if c.Fallback.Enabled && c.Fallback.DailyBudgetCents <= 0 {
		return fmt.Errorf("config: 启用 fallback 必须设置 fallback.daily_budget_cents 预算上限")
	}

	// 探测间隔下限。
	//
	// 探测是发往上游的真实请求，窗口内每轮会对所有未确认的 Key 各发一次。
	// 100 个活跃 Key 配 5s 间隔就是 20 QPS 的纯探测流量，且这些请求内容
	// 高度雷同 —— 正好是火山商务反馈的封禁根因「用户行为规律相似」。
	//
	// 这个下限只能放在配置校验里: 放在调用点兜底，一旦出现第二个调用点就
	// 会漏（此前 main.go 与 background.go 各有一份循环，只有一份做了限制）。
	if c.Refresh.Enabled && c.Refresh.ProbeInterval < 30*time.Second {
		return fmt.Errorf("config: refresh.probe_interval 不得短于 30s（当前 %s），"+
			"高频雷同探测本身就是封禁特征", c.Refresh.ProbeInterval)
	}

	// 探测间隔必须能在窗口内至少跑几轮，否则等于没探测。
	window := parseClockOrZero(c.Refresh.WindowEnd) - parseClockOrZero(c.Refresh.WindowStart)
	if c.Refresh.Enabled && window > 0 && c.Refresh.ProbeInterval*3 > window {
		return fmt.Errorf("config: refresh.probe_interval (%s) 过长，"+
			"在 %s~%s 窗口内不足 3 轮探测，刷新很可能整天都确认不了",
			c.Refresh.ProbeInterval, c.Refresh.WindowStart, c.Refresh.WindowEnd)
	}
	return nil
}
