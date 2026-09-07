// Package confsnap 提供配置的原子快照。
//
// 存在理由: provider 配置要能在运行期热切，而 internal/config 内部零并发
// 保护（无 sync / atomic），148 处读取点全靠这一层的原子指针替换来保证安全。
//
// 三条硬约束，违反任何一条都会让热加载变成静默错配置:
//
//  1. Snapshot 发布后一律只读。禁止改写 s.Cfg.Providers 或 Cfg 的任何字段。
//     Go 没有 const，这条无法由编译器保证，只能靠约定 —— 一旦有人就地改
//     已发布的快照，正在读它的请求会看到撕裂的中间态。
//
//  2. Config 与 adapter registry 必须绑成同一个对象整体替换。adapter 在构造
//     时把 model_mapping 展开成两张固化 map（见 adapter/volc.go），分开替换
//     就会出现「config 是新的、adapter 是旧的」—— 请求体被改写成一个错误的
//     上游模型名，不报错，只是发错。这是本项目最贵的失效类型。
//
//  3. Build 失败绝不触碰 Holder。构造不出新快照时旧快照必须继续生效，
//     而不是把网关切到一个跑不起来的配置上。
package confsnap

import (
	"fmt"
	"sort"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/adapter"
	"github.com/fluxkeys/fluxkeys/internal/config"
)

// Snapshot 是一次配置生效的完整视图，创建后不可变。
//
// Cfg 与 Adapters 必须同源: Adapters 里每个 adapter 的 model_mapping 都取自
// 同一个 Cfg。请求路径上拿到一个 Snapshot 就等于拿到了自洽的全套配置，
// 不存在跨两次热切的混合态。
type Snapshot struct {
	// Version 是该快照对应的配置版本号。启动装配时为 0。
	Version int64
	// Cfg 是配置对象。只读 —— 见包注释约束 1。
	Cfg *config.Config
	// Adapters 是与 Cfg.Providers 同源构造的适配器注册表。
	Adapters *adapter.Registry
	// LoadedAt 是该快照的生效时刻，供 /readyz 展示收敛状态。
	LoadedAt time.Time
}

// Build 按 cfg 装配一份全新快照。
//
// 每次都构造全新 Registry 而不复用旧的: 旧快照的 Registry 随旧 Snapshot 一起
// 被 GC，此刻仍持有它的在途请求完全不受影响 —— 这是 atomic.Pointer 换指针
// 的天然好处，也是「请求内配置自始一致」得以成立的基础。
//
// 全部校验前置且返回 error 而非 panic。adapter.Registry.Register 在 provider
// 名不匹配或 adapter 为 nil 时会 panic，那是启动期的正确行为，但热加载路径
// 上一次配置写错就把整个进程带崩，比拒绝这次变更糟得多。
func Build(cfg *config.Config, version int64) (*Snapshot, error) {
	if cfg == nil {
		return nil, fmt.Errorf("confsnap: cfg 不能为 nil")
	}

	reg := adapter.NewRegistry()
	for name, p := range cfg.Providers {
		ad, err := newAdapterFor(name, p.ModelMapping)
		if err != nil {
			return nil, err
		}
		// 前置条件已在 newAdapterFor 内保证成立（ad 非 nil 且
		// ad.Provider() == name），Register 的 panic 路径不可能被触发。
		reg.Register(name, ad)
	}

	return &Snapshot{
		Version:  version,
		Cfg:      cfg,
		Adapters: reg,
		LoadedAt: time.Now(),
	}, nil
}

// adapterConstructors 是「代码里实际存在适配器」的唯一登记处。
//
// SupportedProviders 与 newAdapterFor 都从这张表出发 —— 分开维护两份清单
// 的必然结局是漂移: 管理接口宣称支持某 provider，热加载时却构造不出适配器。
var adapterConstructors = map[string]func(mapping map[string]string) adapter.Adapter{
	"volc":      func(m map[string]string) adapter.Adapter { return adapter.NewVolc(m) },
	"sensenova": func(m map[string]string) adapter.Adapter { return adapter.NewSenseNova(m) },
}

// SupportedProviders 返回代码支持的 provider 名，按字典序。
//
// 供管理接口（GET /admin/providers/capabilities）向前端暴露可选值:
// 新建 provider 的表单需要知道 name 字段的合法取值集合，而这个集合由
// 编译进来的适配器决定，不由任何配置决定。
func SupportedProviders() []string {
	out := make([]string, 0, len(adapterConstructors))
	for name := range adapterConstructors {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// newAdapterFor 按 provider 名构造适配器。
//
// 返回 error 而非 panic 是刻意的: 通过管理接口新增一个未登记的 provider 名时，
// 这里是唯一能把「不认识这个 provider」变成一条可读拒绝理由的地方。
func newAdapterFor(name string, mapping map[string]string) (adapter.Adapter, error) {
	ctor, ok := adapterConstructors[name]
	if !ok {
		return nil, fmt.Errorf("confsnap: 不支持的 provider=%s", name)
	}
	return ctor(mapping), nil
}
