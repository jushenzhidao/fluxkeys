package test

import (
	"testing"

	"github.com/fluxkeys/fluxkeys/internal/config"
)

// 适配器注册表的用例已迁移到 internal/adapter/registry_test.go ——
// 原先这里打的是 internal/gateway 里一份无锁、无校验的副本，副本已删除。

// TestProviderForModel 验证模型到 provider 的映射
func TestProviderForModel(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"volc": {
				ModelMapping: map[string]string{
					"ep-20241226": "ep-20241226-xxxxx",
					"ep-20250108": "ep-20250108-xxxxx",
				},
			},
			"sensenova": {
				ModelMapping: map[string]string{
					"SenseChat-5":     "SenseChat-5",
					"SenseChat-Turbo": "SenseChat-Turbo",
				},
			},
		},
	}

	tests := []struct {
		model    string
		expected string
	}{
		{"ep-20241226", "volc"},
		{"ep-20250108", "volc"},
		{"SenseChat-5", "sensenova"},
		{"SenseChat-Turbo", "sensenova"},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			provider := cfg.ProviderForModel(tt.model)
			if provider != tt.expected {
				t.Errorf("模型 %s 期望 provider %s，实际 %s", tt.model, tt.expected, provider)
			}
		})
	}
}
