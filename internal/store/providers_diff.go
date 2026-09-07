package store

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"time"
)

// 字段级差异。dry-run 的预演结果与回滚拒绝的原因说明共用这一个结构 ——
// 两端讲的是同一件事（「哪个字段、从什么变成什么」），拆成两套只会让
// 前端各写一份解析，而其中一份迟早与后端漂移。

// FieldDiff 是单个字段的变更。
type FieldDiff struct {
	Field  string `json:"field"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// DiffProviders 返回 before → after 的字段级差异。
//
// 不比较 name / quota_kind: 它们物理禁改，出现在 diff 里只会误导运维
// 以为可以改。也不比较 version / created_at / updated_at 这类事实字段。
func DiffProviders(before, after ProviderConfig) []FieldDiff {
	out := make([]FieldDiff, 0, 8)
	add := func(field string, b, a any) {
		if !reflect.DeepEqual(b, a) {
			out = append(out, FieldDiff{Field: field, Before: b, After: a})
		}
	}

	add("enabled", before.Enabled, after.Enabled)
	add("base_url", before.BaseURL, after.BaseURL)
	add("quota_limit", before.QuotaLimit, after.QuotaLimit)
	// 用可读文本而非纳秒整数: 运维看 "5h" 能立刻判断对错，
	// 看 18000000000000 只能去按计算器。
	add("quota_window", before.QuotaWindow.String(), after.QuotaWindow.String())
	// refresh_hour 用 *int 直接比: nil（无刷新点）与 0（0 点刷新）在
	// DeepEqual 下是两个值，这正是需要的 —— 把它们压成同一个数会让
	// 「取消刷新点」与「改到 0 点」在历史里完全无法区分。
	add("refresh_hour", before.RefreshHour, after.RefreshHour)
	add("model_mapping", normalizeMapping(before.ModelMapping), normalizeMapping(after.ModelMapping))
	add("count_models", normalizeList(before.CountModels), normalizeList(after.CountModels))
	add("reasoning_models", normalizeList(before.ReasoningModels), normalizeList(after.ReasoningModels))
	add("adapter_kind", before.AdapterKind, after.AdapterKind)
	add("credential_env", before.CredentialEnv, after.CredentialEnv)

	return out
}

// DiffFields 返回发生变化的字段名，供 config_versions.changed_fields 使用。
func DiffFields(before, after ProviderConfig) []string {
	diffs := DiffProviders(before, after)
	out := make([]string, 0, len(diffs))
	for _, d := range diffs {
		out = append(out, d.Field)
	}
	return out
}

// normalizeMapping 把 nil map 归一化为空 map，避免 nil 与 {} 被判为差异。
func normalizeMapping(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// normalizeList 把 nil slice 归一化为空 slice。
func normalizeList(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// QuotaWindowFromNanos 把纳秒整数转为 time.Duration。
//
// 负数与溢出值一律归零并由上层校验拒绝: time.Duration 是 int64 纳秒，
// 从 JSON 反序列化时若来源是 float64（JSON 数字的默认落点），超过 2^53
// 的值会丢精度。这里不做静默截断 —— 返回 0 会被 quota_window > 0 的
// 校验挡下，让问题在提交时暴露，而不是变成一个「周期短得离谱」的配置。
//
// 导出是给装配层用的: API 请求体里的纳秒整数必须经由同一个换算落库，
// 装配层直接写 time.Duration(v) 会让这道归零保护形同虚设。
func QuotaWindowFromNanos(ns int64) time.Duration {
	if ns <= 0 || ns > int64(math.MaxInt64) {
		return 0
	}
	return time.Duration(ns)
}

// ToConfig 把快照还原为 ProviderConfig。
//
// RefreshHour 的 *int 在 JSON 里以 null / 数字两种形态存在，
// encoding/json 对 *int 的处理天然能区分二者，故直接透传 ——
// 这里不做任何「零值即无」的折叠，那正是哨兵值方案的老问题。
func (s ProviderSnapshot) ToConfig() ProviderConfig {
	return ProviderConfig{
		Name:            s.Name,
		Enabled:         s.Enabled,
		BaseURL:         s.BaseURL,
		QuotaKind:       s.QuotaKind,
		QuotaLimit:      s.QuotaLimit,
		QuotaWindow:     QuotaWindowFromNanos(s.QuotaWindowNS),
		RefreshHour:     s.RefreshHour,
		ModelMapping:    normalizeMapping(s.ModelMapping),
		CountModels:     normalizeList(s.CountModels),
		ReasoningModels: normalizeList(s.ReasoningModels),
		AdapterKind:     s.AdapterKind,
		CredentialEnv:   s.CredentialEnv,
	}
}

// DecodeProviderSnapshot 解析 config_versions.snapshot。
//
// 快照格式版本高于当前程序认识的版本时直接报错，不做「尽力而为」的解析:
// 未来版本可能改变某字段的语义（而非只是新增），按旧语义读会得到一份
// 看起来合法、实际错误的配置。
func DecodeProviderSnapshot(raw json.RawMessage) (ProviderSnapshot, error) {
	var s ProviderSnapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return ProviderSnapshot{}, fmt.Errorf("store: 解析 provider 快照: %w", err)
	}
	if s.Schema > snapshotSchema {
		return ProviderSnapshot{}, fmt.Errorf(
			"store: 快照格式版本 %d 高于本程序支持的 %d，请先升级网关再回滚",
			s.Schema, snapshotSchema)
	}
	return s, nil
}
