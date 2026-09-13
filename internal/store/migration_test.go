package store

import (
	"context"
	"testing"
)

// 增量迁移必须能在「旧库」上跑通：表已存在、且缺少后来新增的列。
//
// 这是真实生产故障的回归用例。schema.sql 里补列语句
// （ALTER TABLE ... ADD COLUMN IF NOT EXISTS shard）原先排在文件末尾的
// 「增量迁移」段，而它前面第 62 行的
// `CREATE INDEX idx_upstream_keys_shard ON upstream_keys(shard, status)`
// 已经引用了 shard。对一个建表时还没有该列的既有库，迁移直接失败：
//
//	ERROR: column "shard" does not exist (SQLSTATE 42703)
//
// 后果是任何既有部署升级到新版本后**无法启动**（进程以退出码 1 反复重启），
// 而全新部署因为 CREATE TABLE 里就带了 shard 列，完全看不到这个问题 ——
// CI 用的正是全新库，所以这条路径长期没有被覆盖。
//
// 用例显式把列删掉来重建「旧库」状态，再跑一次迁移，断言它能自愈。
func TestMigrate_旧库缺列时能补上(t *testing.T) {
	s, ctx := newTestStore(t)

	// 先退回旧 schema 的形状: 去掉 shard 列（依赖它的索引会随列一起被删）
	if _, err := s.Pool().Exec(ctx, `ALTER TABLE upstream_keys DROP COLUMN IF EXISTS shard`); err != nil {
		t.Fatalf("构造旧库形态失败: %v", err)
	}
	if columnExists(t, ctx, s, "upstream_keys", "shard") {
		t.Fatal("前置条件不成立: shard 列仍然存在")
	}

	// 这一步在修复前会失败
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("旧库上执行迁移失败: %v\n"+
			"提示: 补列语句必须位于引用该列的语句（如建索引）之前", err)
	}

	if !columnExists(t, ctx, s, "upstream_keys", "shard") {
		t.Error("迁移完成后 shard 列仍不存在")
	}
	if !indexExists(t, ctx, s, "idx_upstream_keys_shard") {
		t.Error("迁移完成后 idx_upstream_keys_shard 索引仍不存在")
	}

	// 迁移是每次启动都执行的，必须幂等: 再跑一次不应报错
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("迁移不幂等，第二次执行失败: %v", err)
	}
}

func columnExists(t *testing.T, ctx context.Context, s *Store, table, column string) bool {
	t.Helper()
	var n int
	err := s.Pool().QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = $1 AND column_name = $2`, table, column).Scan(&n)
	if err != nil {
		t.Fatalf("查询列存在性失败: %v", err)
	}
	return n > 0
}

func indexExists(t *testing.T, ctx context.Context, s *Store, index string) bool {
	t.Helper()
	var n int
	err := s.Pool().QueryRow(ctx, `
		SELECT count(*) FROM pg_indexes WHERE indexname = $1`, index).Scan(&n)
	if err != nil {
		t.Fatalf("查询索引存在性失败: %v", err)
	}
	return n > 0
}
