package store

// provider 配置读写的守卫测试。
//
// 需要真实 Postgres（复用 store_test.go 的 newTestStore，无库时 skip）——
// 这里要验的恰好是 SQL 层面的行为: deleted_at 的置位与清除、版本链单调、
// 禁改字段拦截。用 fake 替代连接就把被测对象换成了 fake 自己。

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testProviderConfig(name string) ProviderConfig {
	return ProviderConfig{
		Name:            name,
		Enabled:         true,
		BaseURL:         "https://api.example.com/v1",
		QuotaKind:       "token",
		QuotaLimit:      1_000_000,
		QuotaWindow:     24 * time.Hour,
		ModelMapping:    map[string]string{"gpt-4o": "upstream-model"},
		CountModels:     []string{},
		ReasoningModels: []string{},
	}
}

func mustCreateProvider(t *testing.T, s *Store, ctx context.Context, cfg ProviderConfig) int64 {
	t.Helper()
	v, err := s.CreateProvider(ctx, ProviderWrite{
		Config: cfg, Action: "create", Actor: "test",
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	return v
}

// 回滚到停用前的版本必须同时恢复 deleted_at，否则会出现「回滚返回成功、
// 版本号递增、快照写着未删除，而 provider 依旧查不到」的静默不生效。
func TestRollbackProvider_回滚到停用前版本须清除软删除标记(t *testing.T) {
	s, ctx := newTestStore(t)

	cfg := testProviderConfig("rb_undelete")
	liveVersion := mustCreateProvider(t, s, ctx, cfg)

	if _, err := s.DeleteProvider(ctx, cfg.Name, "停用测试", "test", nil); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}

	// 停用后默认视图应查不到。
	if _, err := s.GetProviderConfig(ctx, cfg.Name); err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatalf("停用后读取: %v", err)
	}

	newVersion, snap, err := s.RollbackProvider(ctx, cfg.Name, liveVersion, "恢复", "test", nil)
	if err != nil {
		t.Fatalf("RollbackProvider: %v", err)
	}
	if newVersion <= liveVersion {
		t.Errorf("回滚后版本号 = %d，应大于目标版本 %d（版本链只能向前）", newVersion, liveVersion)
	}
	if snap.Deleted {
		t.Errorf("目标快照 Deleted = true，期望 false")
	}

	got, err := s.GetProviderConfig(ctx, cfg.Name)
	if err != nil {
		t.Fatalf("回滚后读取 provider 失败: %v（回滚声称成功却查不到，正是要防的静默不生效）", err)
	}
	if got.DeletedAt != nil {
		t.Errorf("回滚后 deleted_at = %v，期望 nil —— 行仍是软删除态，provider 不会出现在路由里",
			got.DeletedAt)
	}
	if !got.Enabled {
		t.Errorf("回滚后 enabled = false，期望 true")
	}
}

// 回滚到一个本身是停用态的版本，应保持停用，且不覆盖原有停用时间。
func TestRollbackProvider_回滚到停用态版本须保持停用(t *testing.T) {
	s, ctx := newTestStore(t)

	cfg := testProviderConfig("rb_keepdel")
	mustCreateProvider(t, s, ctx, cfg)

	delVersion, err := s.DeleteProvider(ctx, cfg.Name, "停用", "test", nil)
	if err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}

	// 先恢复，再回滚到那个「停用态」版本。
	if _, _, err := s.RollbackProvider(ctx, cfg.Name, delVersion-1, "恢复", "test", nil); err != nil {
		// delVersion-1 未必存在，改用列表取第一个版本。
		versions, lerr := s.ListConfigVersions(ctx, cfg.Name, 200, 0)
		if lerr != nil || len(versions) == 0 {
			t.Fatalf("取版本历史失败: %v / %v", err, lerr)
		}
		first := versions[len(versions)-1].ID
		if _, _, err2 := s.RollbackProvider(ctx, cfg.Name, first, "恢复", "test", nil); err2 != nil {
			t.Fatalf("恢复失败: %v", err2)
		}
	}

	if _, _, err := s.RollbackProvider(ctx, cfg.Name, delVersion, "再停用", "test", nil); err != nil {
		t.Fatalf("回滚到停用态版本: %v", err)
	}

	list, err := s.ListProviderConfigs(ctx, true)
	if err != nil {
		t.Fatalf("ListProviderConfigs: %v", err)
	}
	var found *ProviderConfig
	for i := range list {
		if list[i].Name == cfg.Name {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatalf("includeDeleted=true 时应能读到 %s", cfg.Name)
	}
	if found.DeletedAt == nil {
		t.Errorf("回滚到停用态版本后 deleted_at = nil，期望非空 —— "+
			"快照里 deleted=true 却没落到表上，provider 会被意外复活。got=%+v", found.Name)
	}
}

// 跨量纲回滚永远不该成功: quota_kind 决定 Redis 配额 key 的量纲段。
func TestRollbackProvider_跨量纲被拒(t *testing.T) {
	s, ctx := newTestStore(t)

	cfg := testProviderConfig("rb_kind")
	tokenVersion := mustCreateProvider(t, s, ctx, cfg)

	// 直接改表制造一个 count 量纲的当前态 —— 正常路径不允许改 quota_kind，
	// 这里要构造的恰好是「历史版本量纲与当前不一致」这个状态。
	if _, err := s.pool.Exec(ctx,
		`UPDATE provider_configs SET quota_kind = 'count' WHERE name = $1`, cfg.Name); err != nil {
		t.Fatalf("构造 count 量纲当前态: %v", err)
	}

	_, _, err := s.RollbackProvider(ctx, cfg.Name, tokenVersion, "换量纲", "test", nil)
	if !errors.Is(err, ErrQuotaKindImmutable) {
		t.Fatalf("回滚 err = %v，期望 ErrQuotaKindImmutable —— "+
			"跨量纲回滚重试一万次也不会成功，必须与可重试的版本冲突区分开", err)
	}
}

// version 0 是 seed 前的合法值，故 ExpectedVersion 用指针而非「0 表示跳过」。
func TestUpdateProvider_版本冲突可区分且不落库(t *testing.T) {
	s, ctx := newTestStore(t)

	cfg := testProviderConfig("upd_conflict")
	version := mustCreateProvider(t, s, ctx, cfg)

	stale := version - 1
	next := cfg
	next.QuotaLimit = 2_000_000
	_, err := s.UpdateProvider(ctx, ProviderWrite{
		Config: next, Action: "update", Actor: "test", ExpectedVersion: &stale,
	})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("过期 expected_version 的 err = %v，期望 ErrVersionConflict", err)
	}

	got, err := s.GetProviderConfig(ctx, cfg.Name)
	if err != nil {
		t.Fatalf("GetProviderConfig: %v", err)
	}
	if got.QuotaLimit != cfg.QuotaLimit {
		t.Errorf("冲突被拒后 quota_limit = %d，期望保持 %d —— 被拒的请求不得落库",
			got.QuotaLimit, cfg.QuotaLimit)
	}
}

// RefreshHour 是 *int: 必须区分「0 点刷新」与「无刷新点」。
// 用 -1 之类哨兵值会在某天被当成合法小时数。
func TestProviderConfig_RefreshHour零点与无刷新点可区分(t *testing.T) {
	s, ctx := newTestStore(t)

	zero := 0
	atZero := testProviderConfig("rh_zero")
	atZero.RefreshHour = &zero
	mustCreateProvider(t, s, ctx, atZero)

	none := testProviderConfig("rh_none")
	none.RefreshHour = nil
	mustCreateProvider(t, s, ctx, none)

	gotZero, err := s.GetProviderConfig(ctx, atZero.Name)
	if err != nil {
		t.Fatalf("读取 rh_zero: %v", err)
	}
	if gotZero.RefreshHour == nil {
		t.Fatalf("0 点刷新被读成 nil —— 「0 点刷新」与「无刷新点」混为一谈")
	}
	if *gotZero.RefreshHour != 0 {
		t.Errorf("refresh_hour = %d，期望 0", *gotZero.RefreshHour)
	}

	gotNone, err := s.GetProviderConfig(ctx, none.Name)
	if err != nil {
		t.Fatalf("读取 rh_none: %v", err)
	}
	if gotNone.RefreshHour != nil {
		t.Errorf("无刷新点被读成 %d，期望 nil", *gotNone.RefreshHour)
	}
}

// quota_window 以纳秒整数存储，不存 '24h' 文本 ——
// 文本解析失败会静默退化为 0，而周期 0 意味着额度永不刷新且全程无报错。
func TestProviderConfig_QuotaWindow纳秒往返不失真(t *testing.T) {
	s, ctx := newTestStore(t)

	cfg := testProviderConfig("qw_roundtrip")
	cfg.QuotaWindow = 36*time.Hour + 30*time.Minute
	mustCreateProvider(t, s, ctx, cfg)

	got, err := s.GetProviderConfig(ctx, cfg.Name)
	if err != nil {
		t.Fatalf("GetProviderConfig: %v", err)
	}
	if got.QuotaWindow != cfg.QuotaWindow {
		t.Errorf("quota_window = %v，期望 %v", got.QuotaWindow, cfg.QuotaWindow)
	}
}
