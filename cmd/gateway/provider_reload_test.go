package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/store"
)

// 本文件守「DB → 运行期配置」这座桥的两条不变量：
//
//	① enabled=false 的行绝不能进 cfg.Providers（否则「已停用的仍在承接流量」）；
//	② 一个可用的 provider 都没有时必须**拒绝换入空配置**（否则全部请求 404
//	   而健康检查全绿 —— 本项目最贵的一类失效）。
//
// 另外把「未装配时如实报不支持」也钉住：降级必须是可识别的错误，
// 不能被静默吞成 nil。

func row(name string, enabled bool, mutate func(*store.ProviderConfig)) store.ProviderConfig {
	p := store.ProviderConfig{
		Name:         name,
		Enabled:      enabled,
		BaseURL:      "https://example.com",
		QuotaKind:    "token",
		QuotaLimit:   5_000_000,
		QuotaWindow:  24 * time.Hour,
		CountModels:  []string{},
		AdapterKind:  name,
		RefreshHour:  nil,
		ModelMapping: map[string]string{},
	}
	if mutate != nil {
		mutate(&p)
	}
	return p
}

func TestProvidersFromConfigs_停用的行被整个排除(t *testing.T) {
	rows := []store.ProviderConfig{
		row("volc", true, nil),
		row("sensenova", false, nil), // 停用
	}
	got, skipped, err := providersFromConfigs(rows)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if _, ok := got["sensenova"]; ok {
		t.Error("停用的 provider 不得进入 cfg.Providers（停用必须等价于不存在）")
	}
	if _, ok := got["volc"]; !ok {
		t.Error("启用的 provider 应该在里面")
	}
	if len(skipped) != 1 || skipped[0] != "sensenova" {
		t.Errorf("skipped = %v，应只含 sensenova", skipped)
	}
}

func TestProvidersFromConfigs_全部停用时拒绝换入空配置(t *testing.T) {
	rows := []store.ProviderConfig{
		row("volc", false, nil),
		row("sensenova", false, nil),
	}
	got, skipped, err := providersFromConfigs(rows)
	if err == nil {
		t.Fatal("全部停用时应拒绝：空 provider 集合会让全部请求 404 而健康检查仍绿")
	}
	if got != nil {
		t.Error("拒绝时不应返回任何 provider 集合")
	}
	if len(skipped) != 2 {
		t.Errorf("skipped = %v，应含两行，供日志区分「没配」与「都停用了」", skipped)
	}
}

func TestProvidersFromConfigs_库为空的报错要能区分于都停用(t *testing.T) {
	_, _, err := providersFromConfigs(nil)
	if err == nil {
		t.Fatal("空表也应拒绝")
	}
	// 报错里必须同时给出「读数」与「停用数」，否则运维分不清是没配还是全停了。
	if !strings.Contains(err.Error(), "读数 0 行") {
		t.Errorf("报错应点明读数: %v", err)
	}
}

func TestProvidersFromConfigs_字段逐一映射且_nil_map_归一(t *testing.T) {
	hour := 12
	rows := []store.ProviderConfig{row("volc", true, func(p *store.ProviderConfig) {
		p.QuotaKind = "count"
		p.QuotaLimit = 1400
		p.QuotaWindow = 5 * time.Hour
		p.RefreshHour = &hour
		p.ModelMapping = nil // 库里可能是 NULL
		p.CountModels = []string{"doubao-seedream-5-0-260128"}
		p.ReasoningModels = []string{"deepseek"}
		p.BaseURL = "https://token.sensenova.cn"
	})}

	got, _, err := providersFromConfigs(rows)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	p := got["volc"]
	if p.BaseURL != "https://token.sensenova.cn" || p.QuotaKind != "count" ||
		p.QuotaLimit != 1400 || p.QuotaWindow != 5*time.Hour {
		t.Errorf("基础字段映射错误: %+v", p)
	}
	if p.RefreshHour == nil || *p.RefreshHour != 12 {
		t.Errorf("refresh_hour 应映射为 12: %+v", p.RefreshHour)
	}
	if p.ModelMapping == nil {
		t.Error("nil map 必须归一成空 map：否则 JSON 出 null、下游遍历语义不同")
	}
	if len(p.CountModels) != 1 || p.CountModels[0] != "doubao-seedream-5-0-260128" {
		t.Errorf("count_models 映射错误: %v", p.CountModels)
	}
	if len(p.ReasoningModels) != 1 || p.ReasoningModels[0] != "deepseek" {
		t.Errorf("reasoning_models 映射错误: %v", p.ReasoningModels)
	}
}

func TestProvidersFromConfigs_无固定刷新点时_refresh_hour_保持_nil(t *testing.T) {
	got, _, err := providersFromConfigs([]store.ProviderConfig{
		row("sensenova", true, func(p *store.ProviderConfig) { p.RefreshHour = nil }),
	})
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got["sensenova"].RefreshHour != nil {
		t.Error("nil 必须保持 nil —— nil 表示「无固定刷新点」，与「0 点刷新」是两件事")
	}
}

func TestReloadProviderConfig_未装配时如实报不支持(t *testing.T) {
	// 这不是崩溃路径也不是静默 no-op：必须返回可识别的错误，让管理面把
	// reloaded=false 与这句话带回给运维。
	var a storeAdapter
	_, err := a.ReloadProviderConfig(context.Background())
	if !errors.Is(err, errReloadUnavailable) {
		t.Fatalf("期望 errReloadUnavailable，得到 %v", err)
	}
}

func TestReloadProviderConfig_缺基准配置时同样降级(t *testing.T) {
	a := storeAdapter{snaps: mustHolder(t, DefaultTestConfig(t))}
	_, err := a.ReloadProviderConfig(context.Background())
	if !errors.Is(err, errReloadUnavailable) {
		t.Fatalf("期望 errReloadUnavailable，得到 %v", err)
	}
}

// DefaultTestConfig 给快照 Holder 用的最小合法配置。
func DefaultTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("默认配置应自洽: %v", err)
	}
	return cfg
}
