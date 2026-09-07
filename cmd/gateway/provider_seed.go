package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/store"
)

// provider 配置的首次入库。
//
// 迁库前 provider 配置只存在于 config.prod.yaml。迁库后真相来源是
// provider_configs 表，但已有部署的表是空的 —— 直接切过去会让进程带着
// 零个 provider 正常启动: healthz 绿、readyz 绿、日志无错，
// 而所有业务请求都因为「没有可用 provider」而失败。这是最贵的一类故障，
// 因为一切迹象都指向「服务是好的，问题在别处」。
//
// 故约定: 表为空时以 YAML 为种子写入，非空时 YAML 完全不参与 ——
// 不做「以 YAML 覆盖库」也不做字段级合并。库非空说明已有人在界面上改过配置，
// 而 YAML 大概率是那次改动之前的旧版本，覆盖等于静默回退运维的改动。

// seedProviderConfigs 在 provider_configs 为空时以 YAML 配置为种子写入。
//
// 返回是否执行了 seed。表非空时直接返回 false，不做任何写入。
func seedProviderConfigs(ctx context.Context, st *store.Store, cfg *config.Config, log *slog.Logger) (bool, error) {
	n, err := st.CountProviderConfigs(ctx)
	if err != nil {
		// 计数失败必须中断启动，不能当成「表是空的」继续往下走。
		// 把一次连接抖动当成空表，紧接着的 seed 会在已有配置之上再写一遍，
		// 而那批写入各自带着新版本号，历史从此出现两条并行的版本链。
		return false, fmt.Errorf("统计 provider 配置行数: %w", err)
	}
	if n > 0 {
		log.Info("provider 配置已在库中，跳过 YAML 种子写入", "count", n)
		return false, nil
	}

	if len(cfg.Providers) == 0 {
		// 库空且 YAML 也空 —— 此时启动等于起一个无法承接任何流量的进程。
		// 宁可起不来: 启动失败会立刻被发现，而「healthz 绿但全部请求失败」
		// 平均要花掉数小时才能定位到「根本没有 provider」这个原因。
		return false, errors.New(
			"provider_configs 表为空且配置文件中未声明任何 provider，" +
				"启动后所有请求都将因无可用 provider 而失败。" +
				"请先在配置文件的 providers 段声明 provider，或直接向 provider_configs 表写入")
	}

	// 按名字排序后写入。map 遍历顺序随机，会让 config_versions 的版本号
	// 与 provider 的对应关系每次部署都不同 —— 排查历史时无从对照。
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	seeded := make([]string, 0, len(names))
	for _, name := range names {
		p := cfg.Providers[name]
		pc := store.ProviderConfig{
			Name:            name,
			Enabled:         true,
			BaseURL:         p.BaseURL,
			QuotaKind:       p.QuotaKind,
			QuotaLimit:      p.QuotaLimit,
			QuotaWindow:     p.QuotaWindow,
			RefreshHour:     p.RefreshHour,
			ModelMapping:    p.ModelMapping,
			CountModels:     p.CountModels,
			ReasoningModels: p.ReasoningModels,
			AdapterKind:     name,
			// 只存环境变量名，不存值。命名沿用既有约定（provider 名大写 + _API_KEY），
			// 值仍由进程启动时从环境读取。
			CredentialEnv: strings.ToUpper(name) + "_API_KEY",
		}

		version, err := st.CreateProvider(ctx, store.ProviderWrite{
			Config: pc,
			Action: "seed",
			// seed 的「变更字段」是全部字段，与空配置对比即得。
			ChangedFields: store.DiffFields(store.ProviderConfig{}, pc),
			Reason:        "首次启动时从 config 文件导入",
			Actor:         "system",
		})
		if err != nil {
			// 中途失败即中断: 每个 provider 各自一个事务，部分成功是可能的。
			// 继续写剩下的会得到一份不完整的配置集，而进程照常启动 ——
			// 于是只有一部分模型能路由，另一部分静默 404。
			return false, fmt.Errorf("导入 provider %q 配置: %w", name, err)
		}
		log.Info("已从配置文件导入 provider", "provider", name, "version", version,
			"quota_kind", pc.QuotaKind, "quota_limit", pc.QuotaLimit)
		seeded = append(seeded, name)
	}

	// 审计放在全部成功之后写一条，而不是每个 provider 一条:
	// 这是一次「初始化」动作而非 N 次配置变更，逐条写会让审计流水里
	// 出现一批看起来像人为改动的记录。
	if err := st.InsertAuditLog(ctx, store.AuditLog{
		Actor:  "system",
		Action: "seed_from_yaml",
		Target: "provider_configs",
		Detail: map[string]any{
			"providers": seeded,
			"count":     len(seeded),
			"source":    "config file",
			"note":      "provider_configs 表为空，已以配置文件为种子首次写入",
		},
	}); err != nil {
		// 只告警不中断: 配置已经落库且正确，此时因为审计写失败而拒绝启动，
		// 是用一个更严重的故障去响应一个更轻的问题。
		log.Warn("写入 seed 审计日志失败", "error", err)
	}

	log.Info("provider 配置种子写入完成", "count", len(seeded), "providers", seeded)
	return true, nil
}
