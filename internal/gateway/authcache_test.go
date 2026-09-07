package gateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthCache_命中后不再回源(t *testing.T) {
	c := newAuthCache(30 * time.Second)
	var calls atomic.Int64
	fetch := func(ctx context.Context) (*UserContext, error) {
		calls.Add(1)
		return &UserContext{UserID: 1}, nil
	}

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		uc, _, err := c.authenticate(ctx, "tok-a", fetch)
		if err != nil || uc.UserID != 1 {
			t.Fatalf("authenticate: uc=%+v err=%v", uc, err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("回源 %d 次, 期望 1", n)
	}
}

func TestAuthCache_TTL过期后重新回源(t *testing.T) {
	c := newAuthCache(30 * time.Second)
	base := time.Now()
	c.now = func() time.Time { return base }

	var calls atomic.Int64
	fetch := func(ctx context.Context) (*UserContext, error) {
		calls.Add(1)
		return &UserContext{UserID: 2}, nil
	}

	ctx := context.Background()
	if _, fromCache, _ := c.authenticate(ctx, "tok-b", fetch); fromCache {
		t.Error("首次不应命中缓存")
	}
	c.now = func() time.Time { return base.Add(31 * time.Second) }
	if _, fromCache, _ := c.authenticate(ctx, "tok-b", fetch); fromCache {
		t.Error("过期后不应命中缓存")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("回源 %d 次, 期望 2", n)
	}
}

func TestAuthCache_失败不缓存(t *testing.T) {
	// 负缓存会把「主从延迟导致的暂时查不到」放大成 TTL 级持续 401，
	// 也会让撞库流量把缓存撑大。失败必须每次回源。
	c := newAuthCache(30 * time.Second)
	var calls atomic.Int64
	fetch := func(ctx context.Context) (*UserContext, error) {
		calls.Add(1)
		return nil, ErrUnauthorized
	}

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, _, err := c.authenticate(ctx, "tok-bad", fetch); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("期望 ErrUnauthorized, got %v", err)
		}
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("失败被缓存了: 回源 %d 次, 期望 3", n)
	}
}

func TestAuthCache_并发未命中只回源一次(t *testing.T) {
	c := newAuthCache(30 * time.Second)
	var calls atomic.Int64
	release := make(chan struct{})
	fetch := func(ctx context.Context) (*UserContext, error) {
		calls.Add(1)
		<-release // 卡住回源，让并发请求都挂到 inflight 上
		return &UserContext{UserID: 3}, nil
	}

	ctx := context.Background()
	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = c.authenticate(ctx, "tok-c", fetch)
		}(i)
	}
	// 等全部 goroutine 挂上后放行
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("请求 %d 失败: %v", i, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("并发去重失效: 回源 %d 次, 期望 1", got)
	}
}

func TestAuthCache_invalidate立即生效(t *testing.T) {
	c := newAuthCache(30 * time.Second)
	var calls atomic.Int64
	fetch := func(ctx context.Context) (*UserContext, error) {
		calls.Add(1)
		return &UserContext{UserID: 4}, nil
	}

	ctx := context.Background()
	_, _, _ = c.authenticate(ctx, "tok-d", fetch)
	c.invalidate()
	_, fromCache, _ := c.authenticate(ctx, "tok-d", fetch)
	if fromCache {
		t.Error("invalidate 后不应命中缓存")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("回源 %d 次, 期望 2", n)
	}
}

func TestAuthCache_返回副本而非共享指针(t *testing.T) {
	// 调用方可能就地改 UserContext（如叠加临时限额），共享指针会让
	// 一个请求的修改污染所有后续命中。
	c := newAuthCache(30 * time.Second)
	fetch := func(ctx context.Context) (*UserContext, error) {
		return &UserContext{UserID: 5, RPMLimit: 100}, nil
	}

	ctx := context.Background()
	uc1, _, _ := c.authenticate(ctx, "tok-e", fetch)
	uc1.RPMLimit = 999
	uc2, _, _ := c.authenticate(ctx, "tok-e", fetch)
	if uc2.RPMLimit != 100 {
		t.Errorf("缓存条目被调用方污染: RPMLimit = %d", uc2.RPMLimit)
	}
}
