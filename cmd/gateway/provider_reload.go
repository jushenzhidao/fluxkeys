package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/confsnap"
	"github.com/fluxkeys/fluxkeys/internal/store"
)

// 本文件实现 adminapi.ProviderReloader —— 「DB → 运行期配置」的那座桥。
//
// # 为什么必须有它
//
// provider_configs 是 provider 配置的真相来源（设计口径），管理面
// （/admin/providers）与看板都写这张表；而运行期读的是 confsnap 快照。
// 缺这座桥时，写库会「成功而不生效」：
//
//   - POST /admin/providers 返回 201、版本号 +1、审计留痕，进程却仍用启动时的配置；
//   - POST /admin/reload-config 恒返回 501，运维拿不到任何「这条路走不通」的线索；
//   - 更贵的是 POST /admin/keys —— 它按运行期配置校验 provider，于是新建 provider
//     的 Key 会一律报「provider 'X' 未在配置中定义」，把排查引向「名字拼错了」。
//
// 这三条都实测过（见 livetest-ai 的 E2E-FLUXKEYS-001：E2E-FK-RELOAD-501 /
// E2E-FK-DB-TRUTH / E2E-FK-PROVIDER-KEY-IMPORT）。
//
// # 装配位置
//
// 挂在 storeAdapter 上，与 adminapi.ProviderReloader 的注释一致：它是装配层里
// 唯一同时握有存储与快照持有者的对象，而 gateway 侧只需要「触发一次并拿到版本号」。

// errReloadUnavailable 表示本进程未装配热加载所需的依赖。
//
// 保持成一个可识别的错误而不是 nil：调用方据此把响应里的 reloaded 置 false，
// 并把这句话原样带给运维 —— 「不支持」必须说出来，不能被降级成「已完成」。
var errReloadUnavailable = errors.New(
	"provider 配置热加载未装配（缺少快照持有者或基准配置）：变更已落库，但不会影响运行期")

// ReloadProviderConfig 从 provider_configs 重新装配配置快照并换入，返回生效版本号。
//
// 顺序固定为「读库 → 映射校验 → Build → 换入 → 对齐常驻协程」：
//
//   - Build 在校验失败时返回错误且**没有任何副作用** —— 一次写错的配置不该把
//     整个进程带崩（adapter.Registry.Register 对未登记的 provider 名会 panic，
//     所以前置校验必须发生在 Build 内部调用 Register 之前）；
//   - 换入放在对齐之前，因为 Swap 之前起的探测器会读到还没生效的配置。
func (a *storeAdapter) ReloadProviderConfig(ctx context.Context) (int64, error) {
	if a.snaps == nil || a.base == nil {
		return 0, errReloadUnavailable
	}

	// includeDeleted=false: 软删除的行（DELETE /admin/providers 走的就是软删）
	// 绝不能再进运行期配置，否则「删了还在服务」。
	rows, err := a.st.ListProviderConfigs(ctx, false)
	if err != nil {
		return 0, fmt.Errorf("读取 provider 配置: %w", err)
	}

	providers, skipped, err := providersFromConfigs(rows)
	if err != nil {
		return 0, err
	}
	if len(skipped) > 0 {
		a.logger().Info("热加载跳过已停用的 provider", "disabled", skipped, "count", len(skipped))
	}

	// 浅拷贝基准配置后只替换 providers 段：server / postgres / redis / quota /
	// egress / upstream / admin 这些冷配置原样继承（它们本就要重启才生效，
	// 顺手改掉会让「改了不生效」的范围从 provider 扩大到全局）。
	next := *a.base
	next.Providers = providers

	// default_provider 指向一个已不在集合里的名字时必须清掉：它会让
	// ResolveProvider 返回一个不存在的名字，而「只有一个 provider 时自动推断」
	// 这条兜底也会因为 DefaultProvider 非空而永久失效。
	if next.DefaultProvider != "" {
		if _, ok := providers[next.DefaultProvider]; !ok {
			a.logger().Warn("default_provider 不在本次热加载的 provider 集合中，已清空",
				"default_provider", next.DefaultProvider, "providers", keysOf(providers))
			next.DefaultProvider = ""
		}
	}

	version := a.snaps.Version() + 1
	snap, err := confsnap.Build(&next, version)
	if err != nil {
		return 0, fmt.Errorf("装配配置快照: %w", err)
	}
	a.snaps.Store(snap)

	// 换入之后再对齐常驻协程。这里失败**不回滚配置**也不返回错误：配置已经生效，
	// 只是刷新探测器要等下一轮才建起来；报成「热加载失败」会让运维以为白改了。
	if a.afterSwap != nil {
		if err := a.afterSwap(ctx); err != nil {
			a.logger().Error("配置已换入，但常驻协程对齐失败",
				"err", err, "version", version)
		}
	}
	return version, nil
}

// providersFromConfigs 把 provider_configs 的行映射成运行期 provider 配置。
//
// 两个刻意的行为：
//
//  1. enabled=false 的行**整个排除**在 cfg.Providers 之外，而不是放进去让下游判
//     enabled。「停用」在快照层就等价于「不存在」：cfg.Providers 有上百处读取点
//     （ResolveProvider / ProviderForModel / /v1/models / 调度 / 归档），逐处判
//     enabled 等于新增同样数量的漏判机会，而漏判的表现是「已停用的仍在承接流量」
//     且不报错。在这一层过滤一次，下游全部自动正确。
//  2. 一个已启用的 provider 都没有时**拒绝热加载**（返回错误、不换入空配置）：
//     空集合会让全部业务请求 404，而 /healthz 与 /readyz 依然全绿 —— 这正是本
//     项目最贵的一类失效。
//
// skipped 是排除掉的名字，供调用方记录，用来区分「库里没配」与「配了但都停用了」。
func providersFromConfigs(rows []store.ProviderConfig) (map[string]config.Provider, []string, error) {
	out := make(map[string]config.Provider, len(rows))
	var skipped []string

	for _, r := range rows {
		if !r.Enabled {
			skipped = append(skipped, r.Name)
			continue
		}
		p := config.Provider{
			BaseURL:         r.BaseURL,
			QuotaKind:       r.QuotaKind,
			QuotaLimit:      r.QuotaLimit,
			QuotaWindow:     r.QuotaWindow,
			RefreshHour:     r.RefreshHour,
			ModelMapping:    r.ModelMapping,
			CountModels:     r.CountModels,
			ReasoningModels: r.ReasoningModels,
		}
		// nil map 在 JSON 序列化与下游遍历上表现不同（一个出 null、一个出 []），
		// 归一成空 map 让「没有映射」只有一种表示。
		if p.ModelMapping == nil {
			p.ModelMapping = map[string]string{}
		}
		out[r.Name] = p
	}

	if len(out) == 0 {
		return nil, skipped, fmt.Errorf(
			"库中没有任何已启用的 provider（读数 %d 行，其中停用 %d 个），拒绝换入空配置："+
				"空 provider 集合会让全部业务请求 404，而健康检查仍全绿。"+
				"请先启用至少一个 provider，或检查是否误删",
			len(rows), len(skipped))
	}
	return out, skipped, nil
}

func (a *storeAdapter) logger() *slog.Logger {
	if a.log != nil {
		return a.log
	}
	return slog.Default()
}

func keysOf(m map[string]config.Provider) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
