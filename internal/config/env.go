// Package config 定义 FluxKeys 网关的全部可配置项。
//
// 配置来源优先级: 环境变量 > YAML 文件 > 内置默认值。
package config

import (
	"os"
	"strconv"
	"time"
)

// 本文件是环境变量覆盖的唯一入口。
// 优先级：环境变量 > YAML 文件 > 内置默认值。

func applyEnv(cfg *Config) {
	if v := os.Getenv("FLUXKEYS_ADDR"); v != "" {
		cfg.Server.Addr = v
	}
	if v := os.Getenv("FLUXKEYS_METRICS_ADDR"); v != "" {
		cfg.Server.MetricsAddr = v
	}
	// 分片标识走环境变量是刻意的: 多机部署时同一份配置文件与镜像分发到
	// 每台机器，唯一的差异就是这个值。写进 YAML 就意味着每台机器一份配置。
	if v := os.Getenv("FLUXKEYS_SHARD_ID"); v != "" {
		cfg.Server.ShardID = v
	}
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		cfg.Redis.Addr = v
	}
	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		cfg.Redis.Password = v
	}
	if v := os.Getenv("REDIS_DB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Redis.DB = n
		}
	}
	if v := os.Getenv("POSTGRES_DSN"); v != "" {
		cfg.Postgres.DSN = v
	}
	// 推理模型预扣的两个调优旋钮暴露为环境变量：真实输出倍率随模型版本
	// 漂移，需要能在不重建镜像的前提下调整。非法值忽略，交由 Validate 兜底。
	if v := os.Getenv("QUOTA_REASONING_OUTPUT_MULTIPLIER"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Quota.ReasoningOutputMultiplier = f
		}
	}
	if v := os.Getenv("QUOTA_REASONING_FLOOR_TOKENS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Quota.ReasoningFloorTokens = n
		}
	}
	if v := os.Getenv("VOLC_BASE_URL"); v != "" {
		// 这条不是兼容垫片，而是**唯一**的 provider 级 base_url 环境变量覆盖 ——
		// 本地与 CI 用它把 volc 指向 mockark 假上游（compose 默认即
		// http://mockark:18080），生产用它指回真实火山地址。
		//
		// 没有通用的 PROVIDER_<name>_BASE_URL 机制，所以删掉它会同时打断
		// 「离线可跑」与 CI 链路。其他 provider 的地址只能写进 YAML
		// 或经 /admin/providers 改（见 docs/provider-config-hotreload.md）。
		if p, ok := cfg.Providers["volc"]; ok {
			p.BaseURL = v
			cfg.Providers["volc"] = p
		}
	}
	if v := os.Getenv("EGRESS_MODE"); v != "" {
		cfg.Egress.Mode = v
	}
	// EGRESS_IPS 形如 "172.16.0.2=1.2.3.4,172.16.0.3=1.2.3.5"
	if v := os.Getenv("EGRESS_IPS"); v != "" {
		cfg.Egress.IPs = parseEgressIPs(v)
	}
	if v := os.Getenv("EGRESS_VERIFY_ON_START"); v != "" {
		cfg.Egress.VerifyOnStart = v == "true" || v == "1"
	}
	if v := os.Getenv("EGRESS_BAN_DETECT_KEYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Egress.BanDetectKeys = n
		}
	}
	if v := os.Getenv("EGRESS_BAN_DETECT_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Egress.BanDetectWindow = d
		}
	}
	// 允许显式设为 0 关闭自动恢复。
	//
	// 与上面几项不同，这里不能用「解析失败就保持默认」——默认值是 2h（已启用），
	// 而运维写 EGRESS_BAN_COOLDOWN=0 的意图恰恰是关掉它。time.ParseDuration("0")
	// 返回 0 且无错误，所以直接赋值即可；写了非法值则保持默认，避免把
	// 一个笔误变成「出口永不恢复」这种静默的容量泄漏。
	if v := os.Getenv("EGRESS_BAN_COOLDOWN"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Egress.BanCooldown = d
		}
	}
	if v := os.Getenv("EGRESS_BAN_COOLDOWN_MAX"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Egress.BanCooldownMax = d
		}
	}
	if v := os.Getenv("ADMIN_API_KEY"); v != "" {
		cfg.Admin.APIKey = v
	}
	if v := os.Getenv("REFRESH_ENABLED"); v != "" {
		cfg.Refresh.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("QUOTA_TOKEN_LIMIT"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Quota.TokenLimit = n
		}
	}
}
