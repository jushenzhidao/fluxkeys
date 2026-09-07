package adapter

import (
	"fmt"
	"sync"
)

// Registry 是适配器注册表，管理多个 provider 的适配器实例。
//
// 线程安全: 支持并发读取，但不支持运行时动态注册（启动时一次性装配）。
type Registry struct {
	adapters map[string]Adapter
	mu       sync.RWMutex
}

// NewRegistry 创建一个空的适配器注册表。
func NewRegistry() *Registry {
	return &Registry{
		adapters: make(map[string]Adapter),
	}
}

// Register 注册一个 provider 的适配器。
//
// provider 必须与 adapter.Provider() 返回值一致，否则 panic（启动时检查错误）。
// 重复注册同一个 provider 会覆盖旧的适配器。
func (r *Registry) Register(provider string, adapter Adapter) {
	if adapter == nil {
		panic(fmt.Sprintf("adapter.Registry: 不能注册 nil 适配器（provider=%s）", provider))
	}
	if string(adapter.Provider()) != provider {
		panic(fmt.Sprintf("adapter.Registry: provider 不匹配（注册=%s, 适配器返回=%s）",
			provider, adapter.Provider()))
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[provider] = adapter
}

// Get 获取指定 provider 的适配器。
//
// 找不到时返回 nil + error。调用方需判断并返回 404 "模型不存在"。
func (r *Registry) Get(provider string) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	adapter, ok := r.adapters[provider]
	if !ok {
		return nil, fmt.Errorf("adapter.Registry: 未注册的 provider=%s", provider)
	}
	return adapter, nil
}

// MustGet 获取指定 provider 的适配器，找不到时 panic。
//
// 仅用于启动时校验配置，运行时请求路径禁用。
func (r *Registry) MustGet(provider string) Adapter {
	adapter, err := r.Get(provider)
	if err != nil {
		panic(err)
	}
	return adapter
}

// Providers 返回所有已注册的 provider 列表（无序）。
func (r *Registry) Providers() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]string, 0, len(r.adapters))
	for p := range r.adapters {
		out = append(out, p)
	}
	return out
}
