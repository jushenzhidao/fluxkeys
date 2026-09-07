//go:build realredis

// 物理机真实 Redis 链路测试。
//
// 与 e2e_test.go 的分工: 那些用例用 memQuota 替身跑网关全链路，好处是快且
// 无外部依赖，代价是替身忽略了 provider 维度 —— 恰恰是多 provider 改造中
// 最容易出错的地方。这里换成真实 Redis + 真实 quota.Manager，专门验证
// Lua 脚本层面的 provider 隔离与租约生命周期。
//
// 运行:
//
//	go test -tags realredis ./test/ -run RealRedis -v
//
// 需要本机 127.0.0.1:6379 有可用 Redis。测试使用独立 DB（默认 15）并在
// 结束时只删自己写入的 key，不会 FLUSHDB。
package test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fluxkeys/fluxkeys/internal/quota"
)

const realRedisDB = 15

// newRealQuota 连到本机 Redis 并构造真实 Manager。
//
// 用独立 DB 而非 FLUSHDB 隔离: 开发机的 DB 0 可能有别的项目在用，
// 清库是那种"跑一次测试顺手毁掉同事数据"的操作。
func newRealQuota(t *testing.T) (*quota.Manager, *redis.Client) {
	t.Helper()

	addr := os.Getenv("FLUXKEYS_TEST_REDIS")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: realRedisDB})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("跳过: 本机 Redis 不可用 (%s): %v", addr, err)
	}

	qm, err := quota.NewManager(ctx, rdb)
	if err != nil {
		t.Fatalf("构造 quota.Manager（Lua 脚本加载）失败: %v", err)
	}
	return qm, rdb
}

// cleanupKeys 删除本次测试写入的 key。用 SCAN 而非 KEYS，避免阻塞。
func cleanupKeys(t *testing.T, rdb *redis.Client, patterns ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, pat := range patterns {
			var cursor uint64
			for {
				ks, next, err := rdb.Scan(ctx, cursor, pat, 200).Result()
				if err != nil {
					return
				}
				if len(ks) > 0 {
					rdb.Del(ctx, ks...)
				}
				if next == 0 {
					break
				}
				cursor = next
			}
		}
		rdb.Close()
	})
}

func TestRealRedis_不同provider的同名Key配额互不干扰(t *testing.T) {
	qm, rdb := newRealQuota(t)
	cleanupKeys(t, rdb, "volc:*", "sensenova:*")
	ctx := context.Background()

	// 两个上游用了同一个 key_id —— 现实中很常见，各家自己的编号体系
	// 本来就会撞。若 Redis key 没带 provider 前缀，两家会共用一份水位，
	// 一家跑满会把另一家一起锁死。
	const keyID = "shared-001"
	lim := quota.Limits{Hard: 1000, Soft: 800}

	_, leaseA, err := qm.Acquire(ctx, "volc", keyID, quota.KindToken, 600, lim, time.Minute)
	if err != nil {
		t.Fatalf("volc 预扣失败: %v", err)
	}
	if err := qm.Commit(ctx, leaseA, 600); err != nil {
		t.Fatalf("volc 提交失败: %v", err)
	}

	// 同名 key 在另一个 provider 下应当还是满额的。
	decB, leaseB, err := qm.Acquire(ctx, "sensenova", keyID, quota.KindToken, 600, lim, time.Minute)
	if err != nil {
		t.Fatalf("sensenova 预扣被 volc 的用量影响了（provider 隔离失效）: %v", err)
	}
	if decB == quota.Denied {
		t.Fatal("sensenova 应放行，实际被拒 —— 两个 provider 共用了同一份水位")
	}
	if err := qm.Commit(ctx, leaseB, 600); err != nil {
		t.Fatalf("sensenova 提交失败: %v", err)
	}

	snapA, err := qm.Get(ctx, "volc", keyID, quota.KindToken)
	if err != nil {
		t.Fatalf("读取 volc 快照: %v", err)
	}
	snapB, err := qm.Get(ctx, "sensenova", keyID, quota.KindToken)
	if err != nil {
		t.Fatalf("读取 sensenova 快照: %v", err)
	}
	if snapA.Used != 600 {
		t.Errorf("volc used = %d, 期望 600", snapA.Used)
	}
	if snapB.Used != 600 {
		t.Errorf("sensenova used = %d, 期望 600（若为 1200 说明水位被合并）", snapB.Used)
	}
}

func TestRealRedis_硬水位在真实Lua下正确拒绝(t *testing.T) {
	qm, rdb := newRealQuota(t)
	cleanupKeys(t, rdb, "volc:*")
	ctx := context.Background()

	const keyID = "hard-limit-001"
	lim := quota.Limits{Hard: 100, Soft: 90}

	_, l1, err := qm.Acquire(ctx, "volc", keyID, quota.KindToken, 100, lim, time.Minute)
	if err != nil {
		t.Fatalf("首次预扣应成功: %v", err)
	}

	// 第二次预扣: 前一笔还挂在 prededuct 上，未提交也必须计入准入判断。
	// 只看 used 不看 prededuct 是超刷的经典成因。
	if _, _, err := qm.Acquire(ctx, "volc", keyID, quota.KindToken, 1, lim, time.Minute); !errors.Is(err, quota.ErrInsufficient) {
		t.Fatalf("满额时应返回 ErrInsufficient，实际 %v", err)
	}

	// 释放后额度应当回到池子里。
	if err := qm.Release(ctx, l1); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	if _, l2, err := qm.Acquire(ctx, "volc", keyID, quota.KindToken, 100, lim, time.Minute); err != nil {
		t.Fatalf("释放后应可重新预扣: %v", err)
	} else {
		_ = qm.Release(ctx, l2)
	}
}

func TestRealRedis_并发预扣不超卖(t *testing.T) {
	qm, rdb := newRealQuota(t)
	cleanupKeys(t, rdb, "volc:*")
	ctx := context.Background()

	const (
		keyID   = "concurrent-001"
		hard    = 1000
		amount  = 10
		workers = 200 // 期望值的两倍，确保一半会被拒
	)
	lim := quota.Limits{Hard: hard, Soft: hard}

	var (
		mu      sync.Mutex
		granted []*quota.Lease
		denied  int
	)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, l, err := qm.Acquire(ctx, "volc", keyID, quota.KindToken, amount, lim, time.Minute)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				denied++
				return
			}
			granted = append(granted, l)
		}()
	}
	wg.Wait()

	// Lua 脚本是原子的，放行数必须恰好等于额度除以单笔用量。
	// 多一个就是超卖，少一个说明脚本误拒。
	wantGranted := hard / amount
	if len(granted) != wantGranted {
		t.Errorf("放行 %d 笔，期望恰好 %d 笔（超出即超卖）", len(granted), wantGranted)
	}
	if denied != workers-wantGranted {
		t.Errorf("拒绝 %d 笔，期望 %d 笔", denied, workers-wantGranted)
	}

	snap, err := qm.Get(ctx, "volc", keyID, quota.KindToken)
	if err != nil {
		t.Fatalf("读取快照: %v", err)
	}
	if snap.Prededuct != int64(wantGranted*amount) {
		t.Errorf("prededuct = %d, 期望 %d", snap.Prededuct, wantGranted*amount)
	}

	for _, l := range granted {
		if err := qm.Release(ctx, l); err != nil {
			t.Fatalf("释放租约 %s 失败: %v", l.ID, err)
		}
	}
	snap, _ = qm.Get(ctx, "volc", keyID, quota.KindToken)
	if snap.Prededuct != 0 {
		t.Errorf("全部释放后 prededuct = %d, 期望 0（残留会永久占额）", snap.Prededuct)
	}
}

func TestRealRedis_GetMany按provider批量读取(t *testing.T) {
	qm, rdb := newRealQuota(t)
	cleanupKeys(t, rdb, "volc:*", "sensenova:*")
	ctx := context.Background()

	lim := quota.Limits{Hard: 10000, Soft: 9000}
	ids := []string{"batch-a", "batch-b", "batch-c"}
	for i, id := range ids {
		amount := int64((i + 1) * 100)
		_, l, err := qm.Acquire(ctx, "volc", id, quota.KindToken, amount, lim, time.Minute)
		if err != nil {
			t.Fatalf("预扣 %s: %v", id, err)
		}
		if err := qm.Commit(ctx, l, amount); err != nil {
			t.Fatalf("提交 %s: %v", id, err)
		}
	}

	got, err := qm.GetMany(ctx, "volc", ids, quota.KindToken)
	if err != nil {
		t.Fatalf("GetMany 失败: %v", err)
	}
	for i, id := range ids {
		want := int64((i + 1) * 100)
		if got[id].Used != want {
			t.Errorf("%s used = %d, 期望 %d", id, got[id].Used, want)
		}
	}

	// 换 provider 读同一批 id，应当全是零 —— 证明批量读取也带 provider 维度。
	other, err := qm.GetMany(ctx, "sensenova", ids, quota.KindToken)
	if err != nil {
		t.Fatalf("GetMany(sensenova) 失败: %v", err)
	}
	for _, id := range ids {
		if other[id].Used != 0 {
			t.Errorf("sensenova 下 %s used = %d, 期望 0（读串了 provider）", id, other[id].Used)
		}
	}
}

func TestRealRedis_按次配额与Token配额独立计量(t *testing.T) {
	qm, rdb := newRealQuota(t)
	cleanupKeys(t, rdb, "volc:*")
	ctx := context.Background()

	const keyID = "dual-kind-001"
	// 按次模型（如 Seedream）额度很小，若与 Token 共用计数器会瞬间打满。
	countLim := quota.Limits{Hard: 5, Soft: 4}
	tokenLim := quota.Limits{Hard: 100000, Soft: 90000}

	for i := 0; i < 5; i++ {
		_, l, err := qm.Acquire(ctx, "volc", keyID, quota.KindCount, 1, countLim, time.Minute)
		if err != nil {
			t.Fatalf("第 %d 次按次预扣失败: %v", i+1, err)
		}
		if err := qm.Commit(ctx, l, 1); err != nil {
			t.Fatalf("第 %d 次按次提交失败: %v", i+1, err)
		}
	}
	if _, _, err := qm.Acquire(ctx, "volc", keyID, quota.KindCount, 1, countLim, time.Minute); !errors.Is(err, quota.ErrInsufficient) {
		t.Fatalf("按次配额用尽后应拒绝，实际 %v", err)
	}

	// 按次打满不应影响同一个 Key 的 Token 配额。
	_, l, err := qm.Acquire(ctx, "volc", keyID, quota.KindToken, 5000, tokenLim, time.Minute)
	if err != nil {
		t.Fatalf("按次用尽后 Token 配额应仍可用（两类配额串了）: %v", err)
	}
	if err := qm.Commit(ctx, l, 5000); err != nil {
		t.Fatalf("Token 提交失败: %v", err)
	}

	cnt, _ := qm.Get(ctx, "volc", keyID, quota.KindCount)
	tok, _ := qm.Get(ctx, "volc", keyID, quota.KindToken)
	if cnt.Used != 5 {
		t.Errorf("count used = %d, 期望 5", cnt.Used)
	}
	if tok.Used != 5000 {
		t.Errorf("token used = %d, 期望 5000", tok.Used)
	}
}

func TestRealRedis_过期租约被Reap回收(t *testing.T) {
	qm, rdb := newRealQuota(t)
	cleanupKeys(t, rdb, "volc:*")
	ctx := context.Background()

	const keyID = "reap-001"
	lim := quota.Limits{Hard: 1000, Soft: 900}

	// 模拟进程崩溃: 预扣后既不 Commit 也不 Release。
	// 没有 Reap，这笔预扣会永久占额，Key 的可用额度只会单调减少。
	if _, _, err := qm.Acquire(ctx, "volc", keyID, quota.KindToken, 400, lim, 1*time.Second); err != nil {
		t.Fatalf("预扣失败: %v", err)
	}
	before, _ := qm.Get(ctx, "volc", keyID, quota.KindToken)
	if before.Prededuct != 400 {
		t.Fatalf("预扣后 prededuct = %d, 期望 400", before.Prededuct)
	}

	time.Sleep(1500 * time.Millisecond)

	n, err := qm.Reap(ctx, "volc", 100)
	if err != nil {
		t.Fatalf("Reap 失败: %v", err)
	}
	if n != 1 {
		t.Errorf("回收 %d 笔，期望 1", n)
	}

	after, _ := qm.Get(ctx, "volc", keyID, quota.KindToken)
	if after.Prededuct != 0 {
		t.Errorf("Reap 后 prededuct = %d, 期望 0", after.Prededuct)
	}
	if after.Used != 0 {
		t.Errorf("Reap 不应把未提交的预扣计入 used，实际 %d", after.Used)
	}
}

func TestRealRedis_Redis中的key前缀带provider(t *testing.T) {
	qm, rdb := newRealQuota(t)
	cleanupKeys(t, rdb, "volc:*", "sensenova:*")
	ctx := context.Background()

	lim := quota.Limits{Hard: 1000, Soft: 900}
	_, l, err := qm.Acquire(ctx, "sensenova", "prefix-check", quota.KindToken, 10, lim, time.Minute)
	if err != nil {
		t.Fatalf("预扣失败: %v", err)
	}
	if err := qm.Commit(ctx, l, 10); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	// 直接查 Redis 里的实际 key 名。这条断言看着琐碎，但它是"多 provider
	// 隔离"这个结论的物理证据 —— 前缀写错时，上面几个用例可能因为
	// key_id 恰好不同而全部通过。
	keys, err := rdb.Keys(ctx, "sensenova:quota:token:prefix-check:*").Result()
	if err != nil {
		t.Fatalf("扫描 key 失败: %v", err)
	}
	if len(keys) == 0 {
		all, _ := rdb.Keys(ctx, "*prefix-check*").Result()
		t.Fatalf("未找到带 sensenova 前缀的配额 key，实际存在: %v", all)
	}
	t.Logf("配额 key 命名正确: %s", keys[0])

	// 反向确认: volc 前缀下不该有这个 key。
	stray, _ := rdb.Keys(ctx, "volc:quota:token:prefix-check:*").Result()
	if len(stray) > 0 {
		t.Errorf("sensenova 的用量写到了 volc 前缀下: %v", stray)
	}
}

// TestRealRedis_环境信息 打印本次真实测试的环境，便于报告归档。
func TestRealRedis_环境信息(t *testing.T) {
	_, rdb := newRealQuota(t)
	cleanupKeys(t, rdb)
	ctx := context.Background()

	info, err := rdb.Info(ctx, "server").Result()
	if err != nil {
		t.Fatalf("读取 Redis INFO: %v", err)
	}
	for _, line := range []string{"redis_version", "os", "arch_bits"} {
		for _, l := range splitLines(info) {
			if len(l) > len(line) && l[:len(line)] == line {
				t.Logf("%s", l)
			}
		}
	}
	t.Logf("测试使用 DB=%d", realRedisDB)
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
