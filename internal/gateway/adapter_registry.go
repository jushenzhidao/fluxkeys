package gateway

import (
	"fmt"

	"github.com/fluxkeys/fluxkeys/internal/adapter"
)

// AdapterRegistry 管理多个 provider 的适配器。
type AdapterRegistry struct {
	adapters map[string]adapter.Adapter
}

// NewAdapterRegistry 创建一个新的适配器注册表。
func NewAdapterRegistry() *AdapterRegistry {
	return &AdapterRegistry{
		adapters: make(map[string]adapter.Adapter),
	}
}

// Register 注册一个 provider 的适配器。
func (r *AdapterRegistry) Register(provider string, ad adapter.Adapter) {
	r.adapters[provider] = ad
}

// Get 获取指定 provider 的适配器。
func (r *AdapterRegistry) Get(provider string) (adapter.Adapter, error) {
	ad, ok := r.adapters[provider]
	if !ok {
		return nil, fmt.Errorf("未找到 provider %q 的适配器", provider)
	}
	return ad, nil
}
