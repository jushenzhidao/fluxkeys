package test

import (
	"testing"

	"github.com/fluxkeys/fluxkeys/internal/adapter"
	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/gateway"
)

// TestAdapterRegistry 验证多 provider 的适配器注册和获取
func TestAdapterRegistry(t *testing.T) {
	// 构造 adapter registry
	volcAdapter := adapter.NewVolc(map[string]string{
		"ep-20241226": "ep-20241226-xxxxx",
	})
	senseAdapter := adapter.NewSenseNova(map[string]string{
		"SenseChat-5": "SenseChat-5",
	})

	registry := gateway.NewAdapterRegistry()
	registry.Register("volc", volcAdapter)
	registry.Register("sensenova", senseAdapter)

	// 测试获取
	t.Run("GetVolcAdapter", func(t *testing.T) {
		ad, err := registry.Get("volc")
		if err != nil {
			t.Fatalf("获取 volc adapter 失败: %v", err)
		}
		if ad == nil {
			t.Fatal("volc adapter 为 nil")
		}
	})

	t.Run("GetSenseNovaAdapter", func(t *testing.T) {
		ad, err := registry.Get("sensenova")
		if err != nil {
			t.Fatalf("获取 sensenova adapter 失败: %v", err)
		}
		if ad == nil {
			t.Fatal("sensenova adapter 为 nil")
		}
	})

	t.Run("GetNonexistentAdapter", func(t *testing.T) {
		_, err := registry.Get("nonexistent")
		if err == nil {
			t.Fatal("应该返回错误")
		}
	})
}

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
