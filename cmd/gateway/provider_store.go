package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/gateway"
	"github.com/fluxkeys/fluxkeys/internal/store"
)

// storeAdapter 的 provider 配置管理部分，实现 gateway.ProviderConfigStore。
//
// 单独成文件而非并入 adapters.go: 后者已近 400 行且承载热路径适配，
// 而这里是低频管理面。Go 允许同包跨文件定义方法，故 adapters.go 无需改动
// —— 这也让本任务与快照层的改动不争抢同一个文件。
//
// 本层只做类型翻译与错误翻译，不含业务判断。判断分两处:
// gateway 侧的 validateProviderInput（对外校验与文案）与 store 侧的
// 禁改字段拦截（不可绕过的最后一道）。中间再加一层只会让三处逐渐漂移。

// providerToView 把存储层配置转为 gateway 视图。
func providerToView(p store.ProviderConfig) gateway.ProviderConfigView {
	return gateway.ProviderConfigView{
		Name:       p.Name,
		Enabled:    p.Enabled,
		BaseURL:    p.BaseURL,
		QuotaKind:  p.QuotaKind,
		QuotaLimit: p.QuotaLimit,
		// time.Duration 本身就是 int64 纳秒，此处只是换类型不换单位。
		QuotaWindow:     int64(p.QuotaWindow),
		RefreshHour:     p.RefreshHour,
		ModelMapping:    p.ModelMapping,
		CountModels:     p.CountModels,
		ReasoningModels: p.ReasoningModels,
		AdapterKind:     p.AdapterKind,
		CredentialEnv:   p.CredentialEnv,
		Version:         p.Version,
		DeletedAt:       p.DeletedAt,
		CreatedAt:       p.CreatedAt,
		UpdatedAt:       p.UpdatedAt,
	}
}

// viewToProvider 把 gateway 视图转回存储层配置。
//
// 纳秒转 Duration 走 store 侧的换算而非直接 time.Duration(v):
// 负值与溢出值必须归零好被校验挡下，静默截断会得到一个「周期短得离谱」
// 但看起来合法的配置。
func viewToProvider(v gateway.ProviderConfigView) store.ProviderConfig {
	return store.ProviderConfig{
		Name:            v.Name,
		Enabled:         v.Enabled,
		BaseURL:         v.BaseURL,
		QuotaKind:       v.QuotaKind,
		QuotaLimit:      v.QuotaLimit,
		QuotaWindow:     store.QuotaWindowFromNanos(v.QuotaWindow),
		RefreshHour:     v.RefreshHour,
		ModelMapping:    v.ModelMapping,
		CountModels:     v.CountModels,
		ReasoningModels: v.ReasoningModels,
		AdapterKind:     v.AdapterKind,
		CredentialEnv:   v.CredentialEnv,
		Version:         v.Version,
	}
}

// translateProviderErr 把存储层错误翻译为 gateway 侧错误。
//
// 逐个显式映射而非直接透传: gateway 不 import store，它只认自己声明的
// 那几个错误值，透传会让所有失败都落到 default 分支变成 500 ——
// 版本冲突与跨量纲变更都会因此丢掉各自的语义。
func translateProviderErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("%w: %w", gateway.ErrProviderNotFound, err)
	case errors.Is(err, store.ErrProviderExists):
		return fmt.Errorf("%w: %w", gateway.ErrProviderExists, err)
	case errors.Is(err, store.ErrVersionConflict):
		return fmt.Errorf("%w: %w", gateway.ErrProviderVersionConflict, err)
	case errors.Is(err, store.ErrNameImmutable):
		return fmt.Errorf("%w: %w", gateway.ErrProviderNameImmutable, err)
	case errors.Is(err, store.ErrQuotaKindImmutable):
		return fmt.Errorf("%w: %w", gateway.ErrProviderQuotaKindImmutable, err)
	default:
		return err
	}
}

func (a *storeAdapter) ListProvidersWithUsage(ctx context.Context, now time.Time) ([]gateway.ProviderListEntry, error) {
	items, err := a.st.ListProvidersWithUsage(ctx, now)
	if err != nil {
		return nil, translateProviderErr(err)
	}

	out := make([]gateway.ProviderListEntry, 0, len(items))
	for _, it := range items {
		e := gateway.ProviderListEntry{
			ProviderConfigView: providerToView(it.ProviderConfig),
			TodayUsed:          it.TodayUsed,
			TodayRequests:      it.TodayRequests,
			// 只回布尔: 环境变量的值就是上游凭据本身，一旦进了响应体
			// 就会同时出现在浏览器内存、前端日志与任何抓包里。
			CredentialPresent: it.CredentialEnv != "" && os.Getenv(it.CredentialEnv) != "",
		}
		// 列表页不逐个查流量: N 个 provider 会变成 2N 次 EXISTS 查询。
		// 有用量即说明有流量，这个推断对列表场景足够；
		// 详情页的 has_traffic 才走精确查询（要覆盖「历史有、今日无」）。
		e.HasTraffic = it.TodayRequests > 0
		out = append(out, e)
	}
	return out, nil
}

func (a *storeAdapter) GetProviderConfig(ctx context.Context, name string) (gateway.ProviderConfigView, error) {
	p, err := a.st.GetProviderConfig(ctx, name)
	if err != nil {
		return gateway.ProviderConfigView{}, translateProviderErr(err)
	}
	return providerToView(p), nil
}

func (a *storeAdapter) ListProviderConfigs(ctx context.Context, includeDeleted bool) ([]gateway.ProviderConfigView, error) {
	items, err := a.st.ListProviderConfigs(ctx, includeDeleted)
	if err != nil {
		return nil, translateProviderErr(err)
	}
	out := make([]gateway.ProviderConfigView, 0, len(items))
	for _, p := range items {
		out = append(out, providerToView(p))
	}
	return out, nil
}

func (a *storeAdapter) GetProviderPeakUsage(ctx context.Context, name string, days int, now time.Time) (int64, error) {
	v, err := a.st.GetProviderPeakUsage(ctx, name, days, now)
	return v, translateProviderErr(err)
}

func (a *storeAdapter) ProviderHasTraffic(ctx context.Context, name string) (bool, error) {
	v, err := a.st.ProviderHasTraffic(ctx, name)
	return v, translateProviderErr(err)
}

func (a *storeAdapter) CreateProvider(ctx context.Context, in gateway.ProviderWriteInput) (int64, error) {
	cfg := viewToProvider(in.Config)
	v, err := a.st.CreateProvider(ctx, store.ProviderWrite{
		Config: cfg,
		Action: in.Action,
		// 新建时全部字段都是「新增」，与空配置对比即得完整字段清单。
		ChangedFields:   store.DiffFields(store.ProviderConfig{}, cfg),
		Reason:          in.Reason,
		Actor:           in.Actor,
		ExpectedVersion: in.ExpectedVersion,
	})
	return v, translateProviderErr(err)
}

func (a *storeAdapter) UpdateProvider(ctx context.Context, in gateway.ProviderWriteInput) (int64, error) {
	v, err := a.st.UpdateProvider(ctx, store.ProviderWrite{
		Config: viewToProvider(in.Config),
		Action: in.Action,
		// ChangedFields 留空由存储层在事务内计算: 它此时正持有 FOR UPDATE
		// 锁下的当前行，那份 before 才是与本次写入真正对应的。适配层在事务外
		// 读到的 before 可能已被并发改动，据此算出的字段清单会与实际变更不符。
		Reason:          in.Reason,
		Actor:           in.Actor,
		ExpectedVersion: in.ExpectedVersion,
	})
	return v, translateProviderErr(err)
}

func (a *storeAdapter) DeleteProvider(ctx context.Context, name, reason, actor string, expectedVersion *int64) (int64, error) {
	v, err := a.st.DeleteProvider(ctx, name, reason, actor, expectedVersion)
	return v, translateProviderErr(err)
}

func (a *storeAdapter) RollbackProvider(ctx context.Context, name string, targetVersionID int64, reason, actor string, expectedVersion *int64) (int64, gateway.ProviderConfigView, error) {
	v, snap, err := a.st.RollbackProvider(ctx, name, targetVersionID, reason, actor, expectedVersion)
	if err != nil {
		return 0, gateway.ProviderConfigView{}, translateProviderErr(err)
	}
	applied := providerToView(snap.ToConfig())
	// 快照里没有版本号（它记录的是配置内容，不是配置的元数据），
	// 补上本次回滚生成的新版本号，否则界面拿到的是 0，
	// 紧接着的一次编辑会带着 expected_version=0 提交并被乐观锁拒掉。
	applied.Version = v
	return v, applied, nil
}

func (a *storeAdapter) ListProviderVersions(ctx context.Context, name string, limit int, before int64) ([]gateway.ProviderVersionView, error) {
	items, err := a.st.ListConfigVersions(ctx, name, limit, before)
	if err != nil {
		return nil, translateProviderErr(err)
	}
	out := make([]gateway.ProviderVersionView, 0, len(items))
	for _, v := range items {
		// 列表不带快照: 每条快照是一份完整配置，一页 50 条会让响应体
		// 膨胀到几百 KB，而列表页只需要摘要。要看内容走单版本端点。
		out = append(out, versionToView(v, nil))
	}
	return out, nil
}

func (a *storeAdapter) GetProviderVersion(ctx context.Context, id int64) (gateway.ProviderVersionView, error) {
	v, err := a.st.GetConfigVersion(ctx, id)
	if err != nil {
		return gateway.ProviderVersionView{}, translateProviderErr(err)
	}

	// 快照解析失败不吞掉: 解析不了的快照是回滚不了的快照，
	// 静默返回一个空配置会让界面显示「这个版本什么都没配」，
	// 运维据此判断「回滚到这里是安全的」，而真相是这份历史已经读不出来了。
	snap, err := store.DecodeProviderSnapshot(v.Snapshot)
	if err != nil {
		return gateway.ProviderVersionView{}, err
	}
	view := providerToView(snap.ToConfig())
	return versionToView(v, &view), nil
}

// versionToView 把版本历史行转为 gateway 视图。
func versionToView(v store.ConfigVersion, snap *gateway.ProviderConfigView) gateway.ProviderVersionView {
	return gateway.ProviderVersionView{
		ID:             v.ID,
		ProviderName:   v.ProviderName,
		Action:         v.Action,
		ChangedFields:  v.ChangedFields,
		Reason:         v.Reason,
		RolledBackFrom: v.RolledBackFrom,
		CreatedAt:      v.CreatedAt,
		CreatedBy:      v.CreatedBy,
		Snapshot:       snap,
	}
}

// DiffProviderConfigs 复用存储层的差异计算。
//
// 不在此处另写一份: dry-run 展示的差异、跨量纲回滚拒绝时给出的差异、
// 以及落进 config_versions.changed_fields 的字段清单必须同源，
// 否则会出现「预演说改了 3 个字段、历史记录里只有 2 个」这种
// 运维无法判断谁是真相的局面。
func (a *storeAdapter) DiffProviderConfigs(before, after gateway.ProviderConfigView) []gateway.ProviderFieldDiff {
	diffs := store.DiffProviders(viewToProvider(before), viewToProvider(after))
	out := make([]gateway.ProviderFieldDiff, 0, len(diffs))
	for _, d := range diffs {
		out = append(out, gateway.ProviderFieldDiff{Field: d.Field, Before: d.Before, After: d.After})
	}
	return out
}
