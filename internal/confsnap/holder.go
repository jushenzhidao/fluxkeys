package confsnap

import (
	"sync/atomic"

	"github.com/fluxkeys/fluxkeys/internal/config"
)

// Holder 持有当前生效的快照，提供无锁读与原子整体替换。
//
// 用 atomic.Pointer 而非 RWMutex: 读侧是热路径（每请求至少一次，148 处读取点
// 全都经过它），而写侧只在配置变更时发生。读写锁的读锁在高并发下仍有 cache
// line 争抢，原子指针读是一条 load 指令。
//
// 换的是指针而非结构体值: 读侧拿到的是同一个不可变对象的地址，零拷贝，
// 且保证「一次 Current() 拿到的 Cfg 与 Adapters 必然同源」。
type Holder struct {
	ptr atomic.Pointer[Snapshot]
}

// NewHolder 构造持有者。initial 为 nil 时 Current 返回 nil，调用方需自行判断 ——
// 但正常装配路径不应出现这种情况，见 main.go 的启动顺序。
func NewHolder(initial *Snapshot) *Holder {
	h := &Holder{}
	if initial != nil {
		h.ptr.Store(initial)
	}
	return h
}

// NewHolderFromConfig 是启动装配的便捷入口: 按 cfg 构建快照并直接持有。
func NewHolderFromConfig(cfg *config.Config, version int64) (*Holder, error) {
	snap, err := Build(cfg, version)
	if err != nil {
		return nil, err
	}
	return NewHolder(snap), nil
}

// Current 返回当前快照。无锁，可在热路径任意频次调用。
//
// 关键用法约束: 一次逻辑操作（一个用户请求、一轮后台任务）只调用一次，
// 把结果传下去。多次调用会拿到跨热切的不同快照 —— 那正是「新 base_url
// 配旧 model_mapping」混合态的成因。
func (h *Holder) Current() *Snapshot { return h.ptr.Load() }

// Store 原子替换整个快照。
//
// 必须传一个 Build 出来的完整快照，不得把旧快照改一改再存回来 ——
// 后者会让正在读旧快照的请求看到字段级撕裂。
func (h *Holder) Store(s *Snapshot) {
	if s == nil {
		return
	}
	h.ptr.Store(s)
}

// Version 返回当前生效的配置版本号，供 /readyz 与多实例收敛检查使用。
func (h *Holder) Version() int64 {
	if s := h.ptr.Load(); s != nil {
		return s.Version
	}
	return 0
}

// Cfg 返回当前快照的配置对象。
//
// 供只读冷配置、不需要与 adapter 同源的场景使用（如后台任务读间隔参数）。
// 请求路径上禁用 —— 那里必须走 Current() 拿整个快照。
func (h *Holder) Cfg() *config.Config {
	if s := h.ptr.Load(); s != nil {
		return s.Cfg
	}
	return nil
}
