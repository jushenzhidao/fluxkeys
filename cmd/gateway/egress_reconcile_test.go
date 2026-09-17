package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/gateway"
	"github.com/fluxkeys/fluxkeys/internal/store"
)

// 本文件的用例都打真实 Postgres：两处缺陷（适配层错误翻译、对账回写）
// 的判据都是「存储层的真实行为」，用替身只会复刻替身的假设。
// 无库时 skip，与 internal/store 的口径一致（CI 起 postgres 服务）。

// testPGCipherKey 与 internal/store/store_test.go 同一测试主密钥。
const testPGCipherKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

// newTestPGStore 连接测试库并清空数据，无可用 Postgres 时跳过。
func newTestPGStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()

	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("POSTGRES_DSN")
	}
	if dsn == "" {
		dsn = "postgres://fluxkeys:fluxkeys@127.0.0.1:15434/fluxkeys?sslmode=disable"
	}

	c, err := store.NewCipher(testPGCipherKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	ctx := context.Background()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	st, err := store.NewWithOptions(cctx, config.Postgres{
		DSN: dsn, MaxConns: 8, MinConns: 1, AutoMigrate: true,
	}, store.Options{
		Cipher: c, UsageBuffer: 1024,
		UsageFlushInterval: 50 * time.Millisecond, UsageBatchSize: 64,
	})
	if err != nil {
		t.Skipf("跳过: 无可用 Postgres (%s): %v", dsn, err)
	}

	truncatePG(t, st)
	t.Cleanup(func() {
		truncatePG(t, st)
		st.Close()
	})
	return st, ctx
}

func truncatePG(t *testing.T, st *store.Store) {
	t.Helper()
	_, err := st.Pool().Exec(context.Background(), `
		TRUNCATE usage_records, key_daily_history, audit_logs, quota_drift_logs,
		         user_api_keys, users, upstream_keys, egress_ips,
		         provider_configs, config_versions RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("清空测试表: %v", err)
	}
}

// mustPGUpstreamKey 预置一把上游 Key，egressIP 为空表示「尚无绑定记录」。
func mustPGUpstreamKey(t *testing.T, st *store.Store, ctx context.Context, keyID, egressIP string) {
	t.Helper()
	if _, err := st.UpsertUpstreamKey(ctx, &store.UpstreamKey{
		KeyID: keyID, Secret: "sk-test-" + keyID, EgressIP: egressIP,
	}); err != nil {
		t.Fatalf("预置 Key %s: %v", keyID, err)
	}
}

// ---------- KI-036: DELETE 不存在 Key 曾返回 500 而非 404 ----------

// 回归 livetest-ai KI-036（v0.1.7 实测，E2E-FK-DELETE-404）:
// 存储层 DeleteUpstreamKey 在 0 行时返回 store.ErrNotFound，而适配层没有翻译，
// handleAdminKeyDelete 的 errors.Is(err, ErrKeyNotFound) 永远落空，
// 「不存在 / 重复删除」全部落进 500 internal_error —— 自动化清理脚本
// 无法区分「已删过」与「服务端故障」，会误判为失败并重试。
//
// 同包的 RevokeUserAPIKey / PatchUpstreamKeyState 早有同款翻译，
// 本条把 DeleteUpstreamKey 补齐到同一模式。
func TestStoreAdapter_DeleteUpstreamKey_NotFound翻译为KeyNotFound(t *testing.T) {
	st, ctx := newTestPGStore(t)
	a := &storeAdapter{st: st}

	// 前提守卫: 两个哨兵必须互不相认，否则下面的断言恒真。
	if errors.Is(store.ErrNotFound, gateway.ErrKeyNotFound) {
		t.Fatal("测试前提失效: store.ErrNotFound 与 gateway.ErrKeyNotFound 本应互不相认")
	}

	// 从未存在的 Key
	err := a.DeleteUpstreamKey(ctx, "no-such-key-xyz")
	if !errors.Is(err, gateway.ErrKeyNotFound) {
		t.Fatalf("未翻译为 gateway.ErrKeyNotFound（对外应为 404）: %v", err)
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("原始原因 store.ErrNotFound 在包装后丢失，服务端无法排障: %v", err)
	}

	// 存在的 Key 正常删除，重复删除才走 404 —— 翻译不得误伤成功路径。
	mustPGUpstreamKey(t, st, ctx, "del_ok", "")
	if err := a.DeleteUpstreamKey(ctx, "del_ok"); err != nil {
		t.Fatalf("删除存在的 Key 失败: %v", err)
	}
	if err := a.DeleteUpstreamKey(ctx, "del_ok"); !errors.Is(err, gateway.ErrKeyNotFound) {
		t.Fatalf("重复删除应返回 ErrKeyNotFound（404）: %v", err)
	}
}

// ---------- KI-037: 惰性绑定只改内存不落库 ----------

// 回归 livetest-ai KI-037（E2E-FK-BIND-PERSIST）:
// 热路径的惰性绑定（首次分配 / 原出口被封后的重绑）只改内存，
// /admin/ips 与库长期对不上；重启后 restoreBindings 按库里的空值/旧值
// 恢复，Key 换出口 —— 与「一 Key 一 IP 终身绑定」相抵。
// 热路径落库（proxy.go）是第一道，本对账是它失败时的兜底:
// 「内存非空且与库值不一致」的绑定必须被回写库。
func TestReconcileEgressBindings_内存绑定回写库(t *testing.T) {
	st, ctx := newTestPGStore(t)

	mustPGUpstreamKey(t, st, ctx, "k_fresh", "")          // 库里无绑定记录
	mustPGUpstreamKey(t, st, ctx, "k_stale", "10.0.0.99") // 库里是旧值（原出口已被封、热路径重绑过）
	// k_unknown 只在内存有绑定、库中不存在: 对账不得为它写库
	// （ErrNotFound 属正常竞态，ReleaseBindings 负责回收它的内存侧）。

	pool, err := egress.NewPool(egress.ModeMultiIP,
		[]*egress.IP{egress.NewIP("127.0.0.1", "203.0.113.1", 10)}, 5*time.Second)
	if err != nil {
		t.Fatalf("构造出口池: %v", err)
	}
	for _, id := range []string{"k_fresh", "k_stale", "k_unknown"} {
		if err := pool.Adopt(id, "127.0.0.1", egress.PoolAny); err != nil {
			t.Fatalf("Adopt %s: %v", id, err)
		}
	}

	bg := newBackground(bgDeps{
		st:   st,
		pool: pool,
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	keys, err := st.ListUpstreamKeys(ctx, store.UpstreamKeyFilter{})
	if err != nil {
		t.Fatalf("列出 Key: %v", err)
	}
	if n := bg.reconcileEgressBindings(ctx, keys); n != 2 {
		t.Fatalf("回写条数 = %d, 期望 2（k_fresh 与 k_stale 各一条）", n)
	}

	for _, id := range []string{"k_fresh", "k_stale"} {
		k, err := st.GetUpstreamKey(ctx, id)
		if err != nil {
			t.Fatalf("读取 %s: %v", id, err)
		}
		if k.EgressIP != "127.0.0.1" {
			t.Errorf("%s 库中 egress_ip = %q, 期望 127.0.0.1（内存权威应已回写）", id, k.EgressIP)
		}
	}

	// 已一致后再跑一轮必须为零写: 每轮全量重写会无意义地刷 updated_at，
	// 也会把「真实的绑定变更时间」淹没在噪音里。
	keys2, err := st.ListUpstreamKeys(ctx, store.UpstreamKeyFilter{})
	if err != nil {
		t.Fatalf("列出 Key: %v", err)
	}
	if n := bg.reconcileEgressBindings(ctx, keys2); n != 0 {
		t.Errorf("第二轮回写 = %d, 期望 0（内存与库已一致）", n)
	}
}

// 内存里没有绑定的 Key（冷 Key、刚导入未承接请求）不得被回写 —
// 空值写库会把「尚未绑定」抹成一条假记录。
func TestReconcileEgressBindings_内存无绑定不写库(t *testing.T) {
	st, ctx := newTestPGStore(t)
	mustPGUpstreamKey(t, st, ctx, "k_cold", "")

	pool, err := egress.NewPool(egress.ModeMultiIP,
		[]*egress.IP{egress.NewIP("127.0.0.1", "203.0.113.1", 10)}, 5*time.Second)
	if err != nil {
		t.Fatalf("构造出口池: %v", err)
	}
	bg := newBackground(bgDeps{
		st:   st,
		pool: pool,
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	keys, err := st.ListUpstreamKeys(ctx, store.UpstreamKeyFilter{})
	if err != nil {
		t.Fatalf("列出 Key: %v", err)
	}
	if n := bg.reconcileEgressBindings(ctx, keys); n != 0 {
		t.Fatalf("回写条数 = %d, 期望 0（内存中无绑定）", n)
	}
	k, err := st.GetUpstreamKey(ctx, "k_cold")
	if err != nil {
		t.Fatal(err)
	}
	if k.EgressIP != "" {
		t.Errorf("k_cold 库中 egress_ip = %q, 期望仍为空（未绑定不应落库）", k.EgressIP)
	}
}
