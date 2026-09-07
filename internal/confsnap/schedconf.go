package confsnap

import "github.com/fluxkeys/fluxkeys/internal/config"

// SchedConfig 让调度器从配置快照读配置，实现 scheduler.ConfigSource。
//
// 定义在这里而不是 scheduler 包里: scheduler 不 import confsnap，也就不会
// 通过 confsnap 间接依赖 adapter —— 调度层没有任何理由认识上游适配器。
//
// 这里不缓存 *Snapshot，每次都走 Holder.Current()。调度器是长生命周期对象，
// 缓存一份就等于回到「持启动时副本」的老问题。
//
// 至于「一次操作内口径要统一」，由 scheduler 自己保证 —— 它在 Select /
// ScoreAll 入口各取一次 Scheduler() 后循环内复用。
type SchedConfig struct{ H *Holder }

func (s SchedConfig) Scheduler() config.Scheduler { return s.H.Cfg().Scheduler }
func (s SchedConfig) Quota() config.Quota         { return s.H.Cfg().Quota }

func (s SchedConfig) LimitsFor(provider string, kindCount bool) (int64, int64) {
	return s.H.Cfg().LimitsFor(provider, kindCount)
}

func (s SchedConfig) IsCountProvider(provider string) bool {
	return s.H.Cfg().IsCountProvider(provider)
}
