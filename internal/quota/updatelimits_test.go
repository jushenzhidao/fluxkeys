package quota

import (
	"context"
	"sync"
	"testing"
	"time"
)

// 守卫: 存量 provider 改额度后，存活的 Refresher 必须把新水位写回。
//
// 断言落在 MarkRefreshed 的实际效果（Redis 里持久化的 Hard）而不是
// r.cfg.TokenLimits 字段值。原因是字段断言只能证明「值被换了」，证明不了
// 「换掉的值真的被用在写回上」—— 而这个缺陷的危害恰恰全部发生在写回那一刻:
// 界面、库、调度都显示新额度，直到当天刷新窗口 confirm 把 hard_limit 覆盖成
// 旧值。中间隔着几小时，几乎无法归因。
func TestUpdateLimits_存活实例确认刷新时写回新水位(t *testing.T) {
	m, ctx := newTestManager(t)

	probe := func(ctx context.Context, keyID string) (bool, error) { return true, nil }
	list := func(ctx context.Context) ([]string, error) { return []string{"key_1"}, nil }

	r := NewRefresher("volc", testRefresherConfig(), m, probe, list, quietLogger())

	// 该 Key 先消耗一些额度，好让「确认后清零并重置水位」这件事可观测。
	lim := Limits{Hard: 10_000, Soft: 8_000}
	_, lease, _ := m.Acquire(ctx, "volc", "key_1", KindToken, 5_000, lim, time.Minute)
	_ = m.Commit(ctx, lease, 5_000)

	// 模拟热切: 管理页面把该 provider 的额度调高一倍。
	// 注意这里没有重建 Refresher —— 重建会清空状态机，正是要避免的做法。
	const 新硬水位 = 9_000_000
	const 新软水位 = 8_000_000
	const 新计次硬 = 180
	r.UpdateLimits(
		Limits{Hard: 新硬水位, Soft: 新软水位},
		Limits{Hard: 新计次硬, Soft: 160},
	)

	r.SetClock(func() time.Time { return at(13, 30) })
	m.SetClock(func() time.Time { return at(13, 30) })

	if err := r.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if r.State("key_1") != RefreshConfirmed {
		t.Fatalf("探测成功后应为 confirmed, got %s", r.State("key_1"))
	}

	snap, err := m.Get(ctx, "volc", "key_1", KindToken)
	if err != nil {
		t.Fatalf("读 token 水位: %v", err)
	}
	if snap.Hard != 新硬水位 {
		t.Errorf("确认刷新后写回的 token 硬水位 = %d, 期望 %d; "+
			"存活 refresher 用的仍是构造时的旧水位，额度改动会在刷新窗口被覆盖回旧值",
			snap.Hard, 新硬水位)
	}
	if snap.Soft != 新软水位 {
		t.Errorf("确认刷新后写回的 token 软水位 = %d, 期望 %d", snap.Soft, 新软水位)
	}

	// count 量纲同样要跟上。漏掉它会让按次计费的 provider 改了额度不生效，
	// 而 token 侧看起来是对的 —— 症状会被归因成「count 那条路另有 bug」。
	csnap, err := m.Get(ctx, "volc", "key_1", KindCount)
	if err != nil {
		t.Fatalf("读 count 水位: %v", err)
	}
	if csnap.Hard != 新计次硬 {
		t.Errorf("确认刷新后写回的 count 硬水位 = %d, 期望 %d", csnap.Hard, 新计次硬)
	}
}

// 就地更新不能以清空状态机为代价 —— 那正是不走「停掉重建」这条路的理由。
func TestUpdateLimits_不影响状态机(t *testing.T) {
	m, ctx := newTestManager(t)

	probe := func(ctx context.Context, keyID string) (bool, error) { return true, nil }
	list := func(ctx context.Context) ([]string, error) { return []string{"key_1"}, nil }

	r := NewRefresher("volc", testRefresherConfig(), m, probe, list, quietLogger())
	r.SetClock(func() time.Time { return at(13, 30) })
	m.SetClock(func() time.Time { return at(13, 30) })

	if err := r.Tick(ctx); err != nil {
		t.Fatalf("首次 Tick: %v", err)
	}
	before := r.State("key_1")
	if before != RefreshConfirmed {
		t.Fatalf("前置条件不成立: 期望 confirmed, got %s", before)
	}
	rampBefore := r.Schedulable("key_1")

	r.UpdateLimits(Limits{Hard: 1, Soft: 1}, Limits{Hard: 1, Soft: 1})

	if got := r.State("key_1"); got != before {
		t.Errorf("UpdateLimits 后状态 = %s, 期望保持 %s; "+
			"就地更新不应重置状态机（重置会让 Key 从 confirmed 退回 idle 重走 pending，中断刷新流程）",
			got, before)
	}
	if r.Schedulable("key_1") != rampBefore {
		t.Error("UpdateLimits 后可调度性发生变化，说明限速期被重置")
	}
}

// UpdateLimits 由 reconcile 协程调用，confirm 在探测协程里读同一份字段。
// 这个用例的判据是 -race 干净: 裸读 r.cfg 会与写侧构成数据竞争。
func TestUpdateLimits_与确认流程并发无竞态(t *testing.T) {
	m, ctx := newTestManager(t)

	probe := func(ctx context.Context, keyID string) (bool, error) { return true, nil }
	list := func(ctx context.Context) ([]string, error) {
		return []string{"key_1", "key_2", "key_3"}, nil
	}

	r := NewRefresher("volc", testRefresherConfig(), m, probe, list, quietLogger())
	r.SetClock(func() time.Time { return at(13, 30) })
	m.SetClock(func() time.Time { return at(13, 30) })

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读侧: 持续跑确认流程，内部会读 TokenLimits / CountLimits。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = r.Tick(ctx)
			}
		}
	}()

	// 写侧: 持续推新水位进去。跑完即收尾 —— 必须先让读侧退出再 Wait，
	// 反过来（Wait 完再 close）会死锁: 读侧等 stop、Wait 等读侧。
	for i := int64(1); i <= 200; i++ {
		r.UpdateLimits(
			Limits{Hard: 1_000_000 + i, Soft: 900_000 + i},
			Limits{Hard: 100 + i, Soft: 90 + i},
		)
	}
	close(stop)
	wg.Wait()
}
