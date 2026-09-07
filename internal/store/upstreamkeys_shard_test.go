package store

import (
	"context"
	"testing"
)

// 本文件钉住机器分片（shard）的三条契约:
//
//  1. 过滤是严格相等 —— 带 shard 查询绝不返回别人的 Key，也不返回未指派的。
//  2. 例行导入（UpsertUpstreamKey 不带 shard）不得抹掉已有归属。
//  3. AssignShard 只改指定的那批，且返回真实行数。
//
// 为什么必须用真实 Postgres 而非 fake: 契约 2 依赖 ON CONFLICT 里
// `CASE WHEN $15 THEN EXCLUDED.shard ELSE upstream_keys.shard END` 的求值
// 时机。手写 fake 只会复现我对这段 SQL 的理解，理解错了就两边一起错。
// 分片被误抹的后果不是报错而是「Key 静默地不被任何实例装载」——
// 每台机器单看都正常，只是这个账号再也不出流量。

// seedShardKey 预置一个带归属的活跃 Key。
func seedShardKey(t *testing.T, s *Store, ctx context.Context, keyID, shard string) {
	t.Helper()
	if _, err := s.UpsertUpstreamKey(ctx, &UpstreamKey{
		KeyID: keyID, Secret: "sk-" + keyID,
		Status: KeyStatusActive, Pool: "hot", Shard: shard,
	}); err != nil {
		t.Fatalf("预置 Key %s: %v", keyID, err)
	}
}

// keyIDSet 把结果集抽成便于断言的集合。
func keyIDSet(keys []UpstreamKey) map[string]bool {
	got := make(map[string]bool, len(keys))
	for _, k := range keys {
		got[k.KeyID] = true
	}
	return got
}

func TestListUpstreamKeys_分片过滤严格相等(t *testing.T) {
	s, ctx := newTestStore(t)
	seedShardKey(t, s, ctx, "volc_a1", "node-a")
	seedShardKey(t, s, ctx, "volc_a2", "node-a")
	seedShardKey(t, s, ctx, "volc_b1", "node-b")
	seedShardKey(t, s, ctx, "volc_none", "") // 未指派

	keys, err := s.ListUpstreamKeys(ctx, UpstreamKeyFilter{Shard: "node-a"})
	if err != nil {
		t.Fatalf("ListUpstreamKeys: %v", err)
	}
	got := keyIDSet(keys)

	if !got["volc_a1"] || !got["volc_a2"] {
		t.Errorf("本分片 Key 缺失: %v", got)
	}
	// 拿到别的分片的 Key 是最危险的形态: 两台机器会同时调度同一个
	// 上游账号，账号级的日/分钟额度会被双倍消耗直到被上游封禁。
	if got["volc_b1"] {
		t.Error("返回了 node-b 的 Key，跨分片隔离失效")
	}
	// 未指派的不该被任何实例静默接管 —— 归属是不可逆的出口绑定决定，
	// 必须由运维显式做（启动时有 warnUnshardedKeys 告警兜底可观测性）。
	if got["volc_none"] {
		t.Error("返回了未指派归属的 Key，应仅由运维显式指派后才装载")
	}
}

func TestListUpstreamKeys_空分片视为不过滤(t *testing.T) {
	// 单机部署不配 shard，此时必须装载全部 Key。
	// 若把空串也当成一个具体分片去匹配，单机模式会一个 Key 都装不到,
	// 表现为「启动正常但所有请求 503」。
	s, ctx := newTestStore(t)
	seedShardKey(t, s, ctx, "volc_a1", "node-a")
	seedShardKey(t, s, ctx, "volc_none", "")

	keys, err := s.ListUpstreamKeys(ctx, UpstreamKeyFilter{})
	if err != nil {
		t.Fatalf("ListUpstreamKeys: %v", err)
	}
	got := keyIDSet(keys)
	if !got["volc_a1"] || !got["volc_none"] {
		t.Errorf("空 Shard 应返回全部 Key，实际: %v", got)
	}
}

func TestUpsertUpstreamKey_例行导入不抹掉已有归属(t *testing.T) {
	s, ctx := newTestStore(t)
	seedShardKey(t, s, ctx, "volc_a1", "node-a")

	// 模拟运维用一份不含 shard 字段的清单做例行覆盖导入。
	if _, err := s.UpsertUpstreamKey(ctx, &UpstreamKey{
		KeyID: "volc_a1", Secret: "sk-volc_a1",
		Status: KeyStatusActive, Pool: "warm", // 只想改档位
	}); err != nil {
		t.Fatalf("重复导入: %v", err)
	}

	k, err := s.GetUpstreamKey(ctx, "volc_a1")
	if err != nil {
		t.Fatalf("GetUpstreamKey: %v", err)
	}
	if k.Shard != "node-a" {
		t.Errorf("shard 被导入抹成 %q，期望仍是 node-a", k.Shard)
	}
	if k.Pool != "warm" {
		t.Errorf("pool 未更新，实际 %q", k.Pool)
	}
}

func TestUpsertUpstreamKey_显式指定分片可覆盖(t *testing.T) {
	// 与上一条互补: 不抹掉不等于改不了，显式给值必须生效，
	// 否则运维迁移 Key 时会以为改了、实际没改。
	s, ctx := newTestStore(t)
	seedShardKey(t, s, ctx, "volc_a1", "node-a")
	seedShardKey(t, s, ctx, "volc_a1", "node-b")

	k, _ := s.GetUpstreamKey(ctx, "volc_a1")
	if k.Shard != "node-b" {
		t.Errorf("shard = %q, 期望被显式覆盖为 node-b", k.Shard)
	}
}

func TestAssignShard_只改指定批次并返回真实行数(t *testing.T) {
	s, ctx := newTestStore(t)
	seedShardKey(t, s, ctx, "volc_a1", "")
	seedShardKey(t, s, ctx, "volc_a2", "")
	seedShardKey(t, s, ctx, "volc_b1", "node-b")

	n, err := s.AssignShard(ctx, "node-a", []string{"volc_a1", "volc_a2"})
	if err != nil {
		t.Fatalf("AssignShard: %v", err)
	}
	if n != 2 {
		t.Errorf("影响行数 = %d, 期望 2", n)
	}

	for _, id := range []string{"volc_a1", "volc_a2"} {
		k, _ := s.GetUpstreamKey(ctx, id)
		if k.Shard != "node-a" {
			t.Errorf("%s.Shard = %q, 期望 node-a", id, k.Shard)
		}
	}
	// 未列入的 Key 不能被顺带改动 —— 误改归属等于把在跑的账号
	// 从当前机器上摘掉，且出口绑定随之失效。
	k, _ := s.GetUpstreamKey(ctx, "volc_b1")
	if k.Shard != "node-b" {
		t.Errorf("volc_b1.Shard 被误改为 %q", k.Shard)
	}
}

func TestAssignShard_空列表返回零且不改动任何Key(t *testing.T) {
	// 这条只锁「空输入是安全的空操作」这一外部可观测行为。
	//
	// 它**不能**证明提前返回的守卫存在: `key_id = ANY($2)` 传空切片时
	// Postgres 合法地匹配零行，所以把守卫删掉本测试依然通过（已用变异
	// 测试确认存活）。守卫的价值是省一次无意义往返 + 让「没传」在代码层
	// 就短路，属于实现细节，本测试不越界去锁它。
	//
	// 早先我以为空 IN 列表会退化成全表 UPDATE，那是错的 —— 真按那个
	// 假想去写断言，只会得到一条永远为真的空断言。
	s, ctx := newTestStore(t)
	seedShardKey(t, s, ctx, "volc_a1", "node-a")

	n, err := s.AssignShard(ctx, "node-x", nil)
	if err != nil {
		t.Fatalf("AssignShard(nil): %v", err)
	}
	if n != 0 {
		t.Errorf("影响行数 = %d, 期望 0", n)
	}
	k, _ := s.GetUpstreamKey(ctx, "volc_a1")
	if k.Shard != "node-a" {
		t.Errorf("空列表却改动了 Key，shard = %q", k.Shard)
	}
}
