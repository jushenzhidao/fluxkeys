package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/quota"
)

// testCipherKey 是测试专用主密钥（32 字节 hex）。
const testCipherKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

// newTestStore 连接测试库并清空数据。无可用 Postgres 时跳过，
// 与 internal/quota 的处理方式保持一致 —— CI 里缺依赖应当是 skip 而非 fail。
// testDSN 返回测试库连接串。优先 POSTGRES_TEST_DSN，避免误连开发库。
func testDSN() string {
	if dsn := os.Getenv("POSTGRES_TEST_DSN"); dsn != "" {
		return dsn
	}
	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://fluxkeys:fluxkeys@127.0.0.1:15434/fluxkeys?sslmode=disable"
}

func newTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()

	dsn := testDSN()

	c, err := NewCipher(testCipherKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	ctx := context.Background()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	s, err := NewWithOptions(cctx, config.Postgres{
		DSN: dsn, MaxConns: 8, MinConns: 1, AutoMigrate: true,
	}, Options{
		Cipher: c, UsageBuffer: 1024,
		UsageFlushInterval: 50 * time.Millisecond, UsageBatchSize: 64,
	})
	if err != nil {
		t.Skipf("跳过: 无可用 Postgres (%s): %v", dsn, err)
	}

	truncate(t, s)
	t.Cleanup(func() {
		truncate(t, s)
		s.Close()
	})
	return s, ctx
}

func truncate(t *testing.T, s *Store) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(), `
		TRUNCATE usage_records, key_daily_history, audit_logs, quota_drift_logs,
		         user_api_keys, users, upstream_keys, egress_ips,
		         provider_configs, config_versions RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("清空测试表: %v", err)
	}
}

func mustUser(t *testing.T, s *Store, ctx context.Context, name string) *User {
	t.Helper()
	u, err := s.CreateUser(ctx, &User{
		Name: name, Email: name + "@example.com",
		DailyTokenLimit: 1_000_000, RPMLimit: 60, TPMLimit: 100_000,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

// ---------- 加解密 ----------

func TestNewCipher_拒绝非法主密钥(t *testing.T) {
	cases := []struct{ name, key string }{
		{"空值", ""},
		{"非hex", "zzzz"},
		{"长度不足", "00112233"},
		{"长度过长", testCipherKey + "ff"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewCipher(c.key); err == nil {
				t.Fatalf("期望报错，实际通过: %q", c.key)
			}
		})
	}
}

func TestNewCipherFromEnv_缺失环境变量时报错而非降级明文(t *testing.T) {
	t.Setenv(EncryptionKeyEnv, "")
	_, err := NewCipherFromEnv()
	if !errors.Is(err, ErrNoEncryptionKey) {
		t.Fatalf("期望 ErrNoEncryptionKey，实际 %v", err)
	}
}

func TestEncryptSecret_往返一致且相同明文密文不同(t *testing.T) {
	c, err := NewCipher(testCipherKey)
	if err != nil {
		t.Fatal(err)
	}
	plain := "ak-volc-secret-0001"

	e1, err := c.EncryptSecret(plain)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := c.EncryptSecret(plain)
	if err != nil {
		t.Fatal(err)
	}
	if e1 == e2 {
		t.Fatal("相同明文两次加密结果相同，nonce 未随机化")
	}
	if strings.Contains(e1, plain) {
		t.Fatal("密文中出现明文")
	}
	for _, e := range []string{e1, e2} {
		got, err := c.DecryptSecret(e)
		if err != nil {
			t.Fatal(err)
		}
		if got != plain {
			t.Fatalf("解密不一致: 期望 %q 实际 %q", plain, got)
		}
	}
}

func TestDecryptSecret_密钥不匹配或密文被篡改时失败(t *testing.T) {
	c1, _ := NewCipher(testCipherKey)
	c2, _ := NewCipher(strings.Repeat("ab", 32))

	enc, err := c1.EncryptSecret("secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.DecryptSecret(enc); err == nil {
		t.Fatal("换密钥解密应失败")
	}

	tampered := enc[:len(enc)-4] + "AAAA"
	if _, err := c1.DecryptSecret(tampered); err == nil {
		t.Fatal("篡改密文解密应失败")
	}
	if _, err := c1.DecryptSecret("not-base64!!"); err == nil {
		t.Fatal("非法 base64 应失败")
	}
	if _, err := c1.DecryptSecret(""); err == nil {
		t.Fatal("空密文应失败")
	}
}

func TestNewUserKey_格式与唯一性(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		k, err := NewUserKey()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(k, UserKeyPrefix) {
			t.Fatalf("前缀错误: %q", k)
		}
		if len(k) != len(UserKeyPrefix)+32 {
			t.Fatalf("长度错误: %q (%d)", k, len(k))
		}
		if seen[k] {
			t.Fatalf("生成重复 Key: %q", k)
		}
		seen[k] = true
	}
}

func TestHashUserKey_稳定且不可逆(t *testing.T) {
	k := "fk-0123456789abcdef0123456789abcdef"
	h1, h2 := HashUserKey(k), HashUserKey(k)
	if h1 != h2 {
		t.Fatal("同一 Key 两次哈希不一致")
	}
	if len(h1) != 64 {
		t.Fatalf("SHA-256 hex 应为 64 字符，实际 %d", len(h1))
	}
	if strings.Contains(h1, k) {
		t.Fatal("哈希中包含明文")
	}
	if HashUserKey(k) == HashUserKey(k+"x") {
		t.Fatal("不同 Key 哈希碰撞")
	}
}

// ---------- 用户与鉴权 ----------

func TestCreateUser_与ListUsers(t *testing.T) {
	s, ctx := newTestStore(t)

	if _, err := s.CreateUser(ctx, &User{Name: ""}); err == nil {
		t.Fatal("空用户名应报错")
	}

	a := mustUser(t, s, ctx, "alice")
	b := mustUser(t, s, ctx, "bob")
	if a.ID == 0 || b.ID == 0 || a.ID == b.ID {
		t.Fatalf("用户 ID 异常: %d %d", a.ID, b.ID)
	}
	if a.Status != "active" {
		t.Fatalf("默认状态应为 active，实际 %q", a.Status)
	}

	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("期望 2 个用户，实际 %d", len(users))
	}

	got, err := s.GetUser(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "alice" || got.RPMLimit != 60 {
		t.Fatalf("读回用户不符: %+v", got)
	}
	if _, err := s.GetUser(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，实际 %v", err)
	}
}

func TestAuthenticateUserKey_成功路径返回限额(t *testing.T) {
	s, ctx := newTestStore(t)
	u := mustUser(t, s, ctx, "alice")

	plain, rec, err := s.CreateUserAPIKey(ctx, u.ID, "默认")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, UserKeyPrefix) {
		t.Fatalf("明文格式错误: %q", plain)
	}
	if rec.KeyPrefix != plain[:8] {
		t.Fatalf("前缀不符: %q vs %q", rec.KeyPrefix, plain[:8])
	}

	// 明文绝不落库: 库里只应有哈希
	var stored string
	if err := s.pool.QueryRow(ctx,
		`SELECT key_hash FROM user_api_keys WHERE id = $1`, rec.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == plain {
		t.Fatal("库中存了明文 Key")
	}
	if stored != HashUserKey(plain) {
		t.Fatal("库中哈希与计算值不符")
	}

	ac, err := s.AuthenticateUserKey(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	if ac.UserID != u.ID || ac.KeyID != rec.ID {
		t.Fatalf("身份不符: %+v", ac)
	}
	if ac.RPMLimit != 60 || ac.TPMLimit != 100_000 || ac.DailyTokenLimit != 1_000_000 {
		t.Fatalf("限额未随身份返回: %+v", ac)
	}
}

func TestAuthenticateUserKey_失败路径区分三种原因(t *testing.T) {
	s, ctx := newTestStore(t)
	u := mustUser(t, s, ctx, "alice")

	if _, err := s.AuthenticateUserKey(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("空 Key 期望 ErrNotFound，实际 %v", err)
	}
	if _, err := s.AuthenticateUserKey(ctx, "fk-doesnotexist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的 Key 期望 ErrNotFound，实际 %v", err)
	}

	// 已吊销
	revoked, rec, err := s.CreateUserAPIKey(ctx, u.ID, "待吊销")
	if err != nil {
		t.Fatal(err)
	}
	// 归属校验: 用错误的 user_id 吊销必须失败且不产生任何效果
	if err := s.RevokeUserAPIKey(ctx, u.ID+999, rec.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨用户吊销期望 ErrNotFound，实际 %v", err)
	}
	if _, err := s.AuthenticateUserKey(ctx, revoked); err != nil {
		t.Fatalf("跨用户吊销失败后 Key 应仍然有效，实际 %v", err)
	}
	if err := s.RevokeUserAPIKey(ctx, u.ID, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateUserKey(ctx, revoked); !errors.Is(err, ErrKeyRevoked) {
		t.Fatalf("已吊销 Key 期望 ErrKeyRevoked，实际 %v", err)
	}
	// 重复吊销应报 ErrNotFound（已非 active）
	if err := s.RevokeUserAPIKey(ctx, u.ID, rec.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复吊销期望 ErrNotFound，实际 %v", err)
	}

	// 用户停用
	live, _, err := s.CreateUserAPIKey(ctx, u.ID, "正常")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE users SET status='suspended' WHERE id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateUserKey(ctx, live); !errors.Is(err, ErrUserSuspended) {
		t.Fatalf("用户停用期望 ErrUserSuspended，实际 %v", err)
	}
}

func TestAuthenticateUserKey_并发鉴权(t *testing.T) {
	s, ctx := newTestStore(t)
	u := mustUser(t, s, ctx, "alice")
	plain, _, err := s.CreateUserAPIKey(ctx, u.ID, "并发")
	if err != nil {
		t.Fatal(err)
	}

	const n = 64
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.AuthenticateUserKey(ctx, plain); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("并发鉴权失败: %v", err)
	}
}

func TestListUserAPIKeys_与TouchLastUsed(t *testing.T) {
	s, ctx := newTestStore(t)
	u := mustUser(t, s, ctx, "alice")

	_, rec, err := s.CreateUserAPIKey(ctx, u.ID, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateUserAPIKey(ctx, u.ID, "k2"); err != nil {
		t.Fatal(err)
	}

	keys, err := s.ListUserAPIKeys(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("期望 2 条，实际 %d", len(keys))
	}
	if keys[0].LastUsedAt != nil {
		t.Fatal("新建 Key 的 last_used_at 应为 NULL")
	}

	if err := s.TouchUserAPIKey(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	keys, _ = s.ListUserAPIKeys(ctx, u.ID)
	if keys[0].LastUsedAt == nil {
		t.Fatal("Touch 后 last_used_at 应有值")
	}
}

// ---------- 上游 Key ----------

func TestUpsertUpstreamKey_加密存储且可解密读回(t *testing.T) {
	s, ctx := newTestStore(t)

	const secret = "ak-volc-super-secret"
	k, err := s.UpsertUpstreamKey(ctx, &UpstreamKey{
		KeyID: "volc_001", Secret: secret, Pool: "hot",
		EgressIP: "172.16.0.2", PersonaID: "p_01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if k.SecretEnc == secret || k.SecretEnc == "" {
		t.Fatalf("secret_enc 未加密: %q", k.SecretEnc)
	}
	if k.Status != KeyStatusActive || k.HealthScore != 100 || k.RefreshState != RefreshIdle {
		t.Fatalf("默认值不符: %+v", k)
	}

	got, err := s.GetUpstreamKey(ctx, "volc_001")
	if err != nil {
		t.Fatal(err)
	}
	if got.Secret != secret {
		t.Fatalf("解密后密钥不符: %q", got.Secret)
	}

	if _, err := s.GetUpstreamKey(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，实际 %v", err)
	}
	if _, err := s.UpsertUpstreamKey(ctx, &UpstreamKey{KeyID: ""}); err == nil {
		t.Fatal("空 key_id 应报错")
	}
}

func TestUpsertUpstreamKey_空Secret保留原密文(t *testing.T) {
	s, ctx := newTestStore(t)

	if _, err := s.UpsertUpstreamKey(ctx, &UpstreamKey{KeyID: "volc_001", Secret: "orig", Pool: "hot"}); err != nil {
		t.Fatal(err)
	}
	// 只改 pool，不带 Secret
	if _, err := s.UpsertUpstreamKey(ctx, &UpstreamKey{KeyID: "volc_001", Pool: "warm"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetUpstreamKey(ctx, "volc_001")
	if err != nil {
		t.Fatal(err)
	}
	if got.Secret != "orig" {
		t.Fatalf("原密钥被覆盖: %q", got.Secret)
	}
	if got.Pool != "warm" {
		t.Fatalf("pool 未更新: %q", got.Pool)
	}
}

func TestUpsertUpstreamKey_例行导入不复活banned也不改画像与出口(t *testing.T) {
	// 回归测试。曾经的实现在 ON CONFLICT 里用 EXCLUDED.status = '' 判断
	// 「调用方是否指定了状态」，而 VALUES 侧对 status 做了 COALESCE 兜底 ——
	// EXCLUDED 拿到的是兜底后的 'active'，判断永远为假。
	//
	// 后果: 运维重跑一份只含 key_id/pool 的导入清单，会把已封禁的 Key
	// 静默复活并重新投入流量，同时抹掉画像与出口 IP 绑定。
	s, ctx := newTestStore(t)

	if _, err := s.UpsertUpstreamKey(ctx, &UpstreamKey{
		KeyID: "volc_001", Secret: "s1", Pool: "hot",
		PersonaID: "p_night", EgressIP: "172.16.0.9",
	}); err != nil {
		t.Fatal(err)
	}
	// 该 Key 被封禁
	if _, err := s.UpsertUpstreamKey(ctx, &UpstreamKey{
		KeyID: "volc_001", Status: KeyStatusBanned,
	}); err != nil {
		t.Fatal(err)
	}

	// 例行导入: 只带 key_id 与 pool，其余留空
	if _, err := s.UpsertUpstreamKey(ctx, &UpstreamKey{KeyID: "volc_001", Pool: "cold"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetUpstreamKey(ctx, "volc_001")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != KeyStatusBanned {
		t.Fatalf("banned Key 被例行导入复活为 %q", got.Status)
	}
	if got.PersonaID != "p_night" {
		t.Fatalf("画像被抹掉: %q", got.PersonaID)
	}
	if got.EgressIP != "172.16.0.9" {
		t.Fatalf("出口 IP 绑定被抹掉: %q", got.EgressIP)
	}
	if got.Pool != "cold" {
		t.Fatalf("显式指定的 pool 未生效: %q", got.Pool)
	}

	// 显式指定时必须能改回来，否则封禁 Key 永远无法恢复
	if _, err := s.UpsertUpstreamKey(ctx, &UpstreamKey{
		KeyID: "volc_001", Status: KeyStatusActive, PersonaID: "p_day",
	}); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetUpstreamKey(ctx, "volc_001")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != KeyStatusActive || got.PersonaID != "p_day" {
		t.Fatalf("显式指定未覆盖: status=%q persona=%q", got.Status, got.PersonaID)
	}
}

func TestListUpstreamKeys_过滤与按需解密(t *testing.T) {
	s, ctx := newTestStore(t)

	seed := []UpstreamKey{
		{KeyID: "volc_001", Secret: "s1", Pool: "hot", Status: KeyStatusActive},
		{KeyID: "volc_002", Secret: "s2", Pool: "hot", Status: KeyStatusCooldown},
		{KeyID: "volc_003", Secret: "s3", Pool: "cold", Status: KeyStatusActive},
	}
	for i := range seed {
		if _, err := s.UpsertUpstreamKey(ctx, &seed[i]); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.ListUpstreamKeys(ctx, UpstreamKeyFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("期望 3 个，实际 %d", len(all))
	}
	// 默认不解密，避免明文密钥无谓地进入内存
	if all[0].Secret != "" {
		t.Fatalf("未请求解密却填充了明文: %q", all[0].Secret)
	}

	hotActive, err := s.ListUpstreamKeys(ctx, UpstreamKeyFilter{
		Pool: "hot", Status: KeyStatusActive, WithSecret: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hotActive) != 1 || hotActive[0].KeyID != "volc_001" {
		t.Fatalf("过滤结果不符: %+v", hotActive)
	}
	if hotActive[0].Secret != "s1" {
		t.Fatalf("WithSecret 未解密: %q", hotActive[0].Secret)
	}

	limited, err := s.ListUpstreamKeys(ctx, UpstreamKeyFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 {
		t.Fatalf("Limit 未生效: %d", len(limited))
	}
}

func TestUpdateUpstreamKeyState_局部更新不影响其他列(t *testing.T) {
	s, ctx := newTestStore(t)
	if _, err := s.UpsertUpstreamKey(ctx, &UpstreamKey{
		KeyID: "volc_001", Secret: "s1", Pool: "hot", EgressIP: "172.16.0.2",
	}); err != nil {
		t.Fatal(err)
	}

	status, health := KeyStatusCooldown, 55
	if err := s.UpdateUpstreamKeyState(ctx, "volc_001", UpstreamKeyState{
		Status: &status, HealthScore: &health, TouchLastUsed: true,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetUpstreamKey(ctx, "volc_001")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != KeyStatusCooldown || got.HealthScore != 55 {
		t.Fatalf("更新未生效: %+v", got)
	}
	if got.EgressIP != "172.16.0.2" || got.Secret != "s1" {
		t.Fatalf("未指定的列被改动: %+v", got)
	}
	if got.LastUsedAt == nil {
		t.Fatal("TouchLastUsed 未生效")
	}

	// 空更新是 no-op，不应报错
	if err := s.UpdateUpstreamKeyState(ctx, "volc_001", UpstreamKeyState{}); err != nil {
		t.Fatalf("空更新应为 no-op: %v", err)
	}
	if err := s.UpdateUpstreamKeyState(ctx, "nope", UpstreamKeyState{Status: &status}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，实际 %v", err)
	}
}

func TestUpdateRefreshState_确认时写入时间戳(t *testing.T) {
	s, ctx := newTestStore(t)
	if _, err := s.UpsertUpstreamKey(ctx, &UpstreamKey{KeyID: "volc_001", Secret: "s1"}); err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateRefreshState(ctx, "volc_001", RefreshProbing, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetUpstreamKey(ctx, "volc_001")
	if got.RefreshState != RefreshProbing {
		t.Fatalf("状态未更新: %q", got.RefreshState)
	}
	if got.RefreshConfirmedAt != nil {
		t.Fatal("probing 阶段不应写入确认时间")
	}

	if err := s.UpdateRefreshState(ctx, "volc_001", RefreshConfirmed, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetUpstreamKey(ctx, "volc_001")
	if got.RefreshConfirmedAt == nil {
		t.Fatal("confirmed 应写入确认时间")
	}

	if err := s.UpdateRefreshState(ctx, "volc_001", RefreshFailed, "额度未刷新"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetUpstreamKey(ctx, "volc_001")
	if got.LastError != "额度未刷新" {
		t.Fatalf("last_error 未写入: %q", got.LastError)
	}
	if err := s.UpdateRefreshState(ctx, "nope", RefreshProbing, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，实际 %v", err)
	}
}

// ---------- 用量流水 ----------

func TestInsertUsageRecord_异步批量落库(t *testing.T) {
	s, ctx := newTestStore(t)
	u := mustUser(t, s, ctx, "alice")
	_, key, err := s.CreateUserAPIKey(ctx, u.ID, "k")
	if err != nil {
		t.Fatal(err)
	}
	day := quota.QuotaDayTime(time.Now())

	const n = 300
	for i := 0; i < n; i++ {
		if err := s.InsertUsageRecord(ctx, UsageRecord{
			RequestID: fmt.Sprintf("req-%d", i), UserID: u.ID, UserAPIKeyID: key.ID,
			UpstreamKeyID: "volc_001", Model: "deepseek-v3", QuotaDay: day,
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
			EstimatedTokens: 200, StatusCode: 200, LatencyMS: 42,
		}); err != nil {
			t.Fatalf("第 %d 条投递失败: %v", i, err)
		}
	}

	if err := s.FlushUsage(ctx); err != nil {
		t.Fatal(err)
	}

	var count int64
	var sum int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*), COALESCE(SUM(total_tokens),0) FROM usage_records`).Scan(&count, &sum); err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Fatalf("期望 %d 条流水，实际 %d", n, count)
	}
	if sum != n*150 {
		t.Fatalf("token 汇总不符: %d", sum)
	}

	st := s.UsageStats()
	if st.Written != n || st.Dropped != 0 || st.Failed != 0 {
		t.Fatalf("统计不符: %+v", st)
	}
}

func TestInsertUsageRecord_校验与默认值(t *testing.T) {
	s, ctx := newTestStore(t)

	if err := s.InsertUsageRecord(ctx, UsageRecord{RequestID: "r1"}); err == nil {
		t.Fatal("quota_day 为零值应报错")
	}

	day := quota.QuotaDayTime(time.Now())
	// user_id = 0 表示无归属（如内部探测请求），应写成 NULL 而非违反外键
	if err := s.InsertUsageRecord(ctx, UsageRecord{
		RequestID: "probe-1", UpstreamKeyID: "volc_001", QuotaDay: day, StatusCode: 200,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushUsage(ctx); err != nil {
		t.Fatal(err)
	}

	var provider, kind string
	var userID *int64
	if err := s.pool.QueryRow(ctx,
		`SELECT provider, billing_kind, user_id FROM usage_records WHERE request_id='probe-1'`).
		Scan(&provider, &kind, &userID); err != nil {
		t.Fatal(err)
	}
	if provider != "volc" || kind != "token" {
		t.Fatalf("默认值未填充: %q %q", provider, kind)
	}
	if userID != nil {
		t.Fatalf("user_id=0 应写为 NULL，实际 %v", *userID)
	}
	if s.UsageStats().Failed != 0 {
		t.Fatal("无归属流水写入失败，可能是外键约束未绕开")
	}
}

func TestInsertUsageRecord_并发投递不丢不错(t *testing.T) {
	s, ctx := newTestStore(t)
	day := quota.QuotaDayTime(time.Now())

	const goroutines, perG = 20, 25
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				_ = s.InsertUsageRecord(ctx, UsageRecord{
					RequestID:     fmt.Sprintf("r-%d-%d", g, i),
					UpstreamKeyID: fmt.Sprintf("volc_%03d", g%5),
					QuotaDay:      day, TotalTokens: 10, StatusCode: 200,
				})
			}
		}(g)
	}
	wg.Wait()

	if err := s.FlushUsage(ctx); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM usage_records`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != goroutines*perG {
		t.Fatalf("期望 %d 条，实际 %d（丢弃 %d）",
			goroutines*perG, count, s.UsageStats().Dropped)
	}
}

func TestInsertUsageRecord_缓冲区满时丢弃而非阻塞热路径(t *testing.T) {
	s, ctx := newTestStore(t)

	// 直接塞满一个极小容量的写入器，验证 enqueue 不会阻塞
	w := &usageWriter{ch: make(chan UsageRecord, 2), store: s, batch: 100, interval: time.Hour, done: make(chan struct{})}
	day := quota.QuotaDayTime(time.Now())

	var dropErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10; i++ {
			if err := w.enqueue(UsageRecord{RequestID: "x", QuotaDay: day}); err != nil {
				dropErr = err
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("缓冲区满时 enqueue 发生阻塞，会把数据库压力传导到请求热路径")
	}
	if dropErr == nil {
		t.Fatal("期望缓冲区满时返回丢弃错误")
	}
	if w.dropped.Load() != 8 {
		t.Fatalf("期望丢弃 8 条，实际 %d", w.dropped.Load())
	}
	_ = ctx
}

func TestClose_退出前flush完毕(t *testing.T) {
	dsn := testDSN()
	c, _ := NewCipher(testCipherKey)
	ctx := context.Background()

	// interval 故意设得很长，确保数据只可能由 Close 时的 flush 落库
	s, err := NewWithOptions(ctx, config.Postgres{DSN: dsn, MaxConns: 4, AutoMigrate: true},
		Options{Cipher: c, UsageFlushInterval: time.Hour, UsageBatchSize: 1000})
	if err != nil {
		t.Skipf("跳过: 无可用 Postgres: %v", err)
	}
	truncate(t, s)

	day := quota.QuotaDayTime(time.Now())
	const n = 50
	for i := 0; i < n; i++ {
		if err := s.InsertUsageRecord(ctx, UsageRecord{
			RequestID: fmt.Sprintf("close-%d", i), UpstreamKeyID: "volc_001",
			QuotaDay: day, TotalTokens: 1, StatusCode: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 用一个新连接验证数据确实落库了
	s2, err := NewWithOptions(ctx, config.Postgres{DSN: dsn, MaxConns: 2}, Options{Cipher: c})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		truncate(t, s2)
		s2.Close()
	}()

	var count int64
	if err := s2.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM usage_records WHERE request_id LIKE 'close-%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Fatalf("Close 未 flush 完毕: 期望 %d 条，实际 %d", n, count)
	}

	// Close 后再投递应报错而非 panic（向已关闭 channel 发送会 panic）
	if err := s.InsertUsageRecord(ctx, UsageRecord{RequestID: "after", QuotaDay: day}); err == nil {
		t.Fatal("Close 后投递应报错")
	}
	// 重复 Close 应安全
	if err := s.Close(); err != nil {
		t.Fatalf("重复 Close 应安全: %v", err)
	}
}

// ---------- 历史归档 / 审计 / 对账 ----------

func TestUpsertKeyDailyHistory_与GetKeyHistory(t *testing.T) {
	s, ctx := newTestStore(t)
	day := quota.QuotaDayTime(time.Now())

	if err := s.UpsertKeyDailyHistory(ctx, &KeyDailyHistory{UpstreamKeyID: ""}); err == nil {
		t.Fatal("空 key_id 应报错")
	}
	if err := s.UpsertKeyDailyHistory(ctx, &KeyDailyHistory{UpstreamKeyID: "volc_001"}); err == nil {
		t.Fatal("空 provider 应报错")
	}
	if err := s.UpsertKeyDailyHistory(ctx, &KeyDailyHistory{UpstreamKeyID: "volc_001", Provider: "volc"}); err == nil {
		t.Fatal("零值 quota_day 应报错")
	}

	// token_ratio 留空时应由 used/limit 推导
	if err := s.UpsertKeyDailyHistory(ctx, &KeyDailyHistory{
		UpstreamKeyID: "volc_001", Provider: "volc", QuotaDay: day,
		TokenUsed: 1_000_000, TokenLimit: 5_000_000, RequestCount: 120,
	}); err != nil {
		t.Fatal(err)
	}
	hist, err := s.GetKeyHistory(ctx, []string{"volc_001", "volc_002"}, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(hist))
	}
	if got := hist[HistoryKey{UpstreamKeyID: "volc_001", Provider: "volc"}].TokenRatio; got != 0.2 {
		t.Fatalf("token_ratio 推导错误: %v", got)
	}

	// 重复 upsert 覆盖而非累加
	if err := s.UpsertKeyDailyHistory(ctx, &KeyDailyHistory{
		UpstreamKeyID: "volc_001", Provider: "volc", QuotaDay: day,
		TokenUsed: 2_000_000, TokenLimit: 5_000_000,
		ConsecutiveLightDays: 3,
	}); err != nil {
		t.Fatal(err)
	}
	hist, _ = s.GetKeyHistory(ctx, []string{"volc_001"}, day)
	if hist[HistoryKey{UpstreamKeyID: "volc_001", Provider: "volc"}].TokenUsed != 2_000_000 {
		t.Fatalf("upsert 应覆盖: %d", hist[HistoryKey{UpstreamKeyID: "volc_001", Provider: "volc"}].TokenUsed)
	}
	if hist[HistoryKey{UpstreamKeyID: "volc_001", Provider: "volc"}].ConsecutiveLightDays != 3 {
		t.Fatalf("连续低消耗天数未更新: %d", hist[HistoryKey{UpstreamKeyID: "volc_001", Provider: "volc"}].ConsecutiveLightDays)
	}

	empty, err := s.GetKeyHistory(ctx, nil, day)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空入参应返回空 map: %v %v", empty, err)
	}
}

func TestAggregateUsageByKey_与SumUserTokens(t *testing.T) {
	s, ctx := newTestStore(t)
	u := mustUser(t, s, ctx, "alice")
	day := quota.QuotaDayTime(time.Now())

	// k_shared 刻意在两个 provider 下各有流水: 汇总必须拆成两行。
	// 早期实现按 key 分组再取 MAX(provider)，会把两边的量合并成一行、
	// 并按字典序挑一个 provider 名 —— 用量被记到错误的上游账上。
	recs := []UsageRecord{
		{RequestID: "a1", UserID: u.ID, UpstreamKeyID: "volc_001", Provider: "volc", QuotaDay: day, TotalTokens: 100, StatusCode: 200},
		{RequestID: "a2", UserID: u.ID, UpstreamKeyID: "volc_001", Provider: "volc", QuotaDay: day, TotalTokens: 200, StatusCode: 500, ErrorCode: "upstream"},
		{RequestID: "a3", UserID: u.ID, UpstreamKeyID: "volc_002", Provider: "volc", QuotaDay: day, TotalTokens: 50, StatusCode: 200},
		{RequestID: "a4", UserID: u.ID, UpstreamKeyID: "k_shared", Provider: "volc", QuotaDay: day, TotalTokens: 70, StatusCode: 200},
		{RequestID: "a5", UserID: u.ID, UpstreamKeyID: "k_shared", Provider: "sensenova", QuotaDay: day, CountUnits: 3, StatusCode: 200},
	}
	for _, r := range recs {
		if err := s.InsertUsageRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushUsage(ctx); err != nil {
		t.Fatal(err)
	}

	agg, err := s.AggregateUsageByKey(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	byPair := map[HistoryKey]KeyDailyHistory{}
	for _, h := range agg {
		byPair[HistoryKey{UpstreamKeyID: h.UpstreamKeyID, Provider: h.Provider}] = h
	}
	if len(agg) != 4 {
		t.Fatalf("期望 4 个 (key,provider) 组合，实际 %d: %+v", len(agg), agg)
	}
	k1 := byPair[HistoryKey{UpstreamKeyID: "volc_001", Provider: "volc"}]
	if k1.TokenUsed != 300 || k1.RequestCount != 2 || k1.ErrorCount != 1 {
		t.Fatalf("volc_001 汇总不符: %+v", k1)
	}
	sv := byPair[HistoryKey{UpstreamKeyID: "k_shared", Provider: "volc"}]
	ss := byPair[HistoryKey{UpstreamKeyID: "k_shared", Provider: "sensenova"}]
	if sv.TokenUsed != 70 || sv.CountUsed != 0 {
		t.Fatalf("k_shared@volc 应只含 volc 的量: %+v", sv)
	}
	if ss.CountUsed != 3 || ss.TokenUsed != 0 {
		t.Fatalf("k_shared@sensenova 应只含 sensenova 的量: %+v", ss)
	}

	// 用户级汇总跨 provider 求和、且不区分成败: 100+200+50+70=420。
	// a2 是 500 但 token 已被上游计费，必须计入日限额，否则失败请求
	// 成了绕过限额的免费通道。a5 走 count 计费不产生 token，不参与此和。
	total, err := s.SumUserTokens(ctx, u.ID, day)
	if err != nil {
		t.Fatal(err)
	}
	if total != 420 {
		t.Fatalf("用户用量汇总不符: 期望 420，实际 %d", total)
	}
	if total, _ := s.SumUserTokens(ctx, 999999, day); total != 0 {
		t.Fatalf("无流水用户应为 0，实际 %d", total)
	}
}

func TestInsertAuditLog(t *testing.T) {
	s, ctx := newTestStore(t)

	if err := s.InsertAuditLog(ctx, AuditLog{Action: ""}); err == nil {
		t.Fatal("空 action 应报错")
	}
	if err := s.InsertAuditLog(ctx, AuditLog{
		Actor: "admin", Action: "volc_key.ban", Target: "volc_001",
		Detail: map[string]any{"reason": "连续 429", "count": 5},
	}); err != nil {
		t.Fatal(err)
	}
	// Detail 为空也应可写入
	if err := s.InsertAuditLog(ctx, AuditLog{Actor: "admin", Action: "user.create"}); err != nil {
		t.Fatal(err)
	}

	var count int
	var reason string
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_logs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("期望 2 条审计，实际 %d", count)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT detail->>'reason' FROM audit_logs WHERE target='volc_001'`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "连续 429" {
		t.Fatalf("JSONB detail 不符: %q", reason)
	}
}

func TestInsertQuotaDrift(t *testing.T) {
	s, ctx := newTestStore(t)
	day := quota.QuotaDayTime(time.Now())

	if err := s.InsertQuotaDrift(ctx, QuotaDrift{UpstreamKeyID: ""}); err == nil {
		t.Fatal("空 key_id 应报错")
	}
	if err := s.InsertQuotaDrift(ctx, QuotaDrift{UpstreamKeyID: "volc_001"}); err == nil {
		t.Fatal("零值 quota_day 应报错")
	}
	if err := s.InsertQuotaDrift(ctx, QuotaDrift{
		UpstreamKeyID: "volc_001", BillingKind: "token", QuotaDay: day, Drift: 1,
	}); err == nil {
		t.Fatal("空 provider 应报错")
	}
	// 负偏差同样要能记录: prededuct 少于租约之和也是异常
	for _, d := range []int64{5000, -3000} {
		if err := s.InsertQuotaDrift(ctx, QuotaDrift{
			UpstreamKeyID: "volc_001", Provider: "volc",
			BillingKind: "token", QuotaDay: day, Drift: d,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM quota_drift_logs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("期望 2 条，实际 %d", count)
	}
	// 断言 provider 真的落到了库里。只数行数的话，漏写 provider 列这类
	// 缺陷会被 NOT NULL 约束挡在数据库层、表现为「写入报错」而非「写错」，
	// 而生产里唯一调用方对写失败只 warn，缺陷就此静默。
	var gotProvider string
	if err := s.pool.QueryRow(ctx,
		`SELECT DISTINCT provider FROM quota_drift_logs`).Scan(&gotProvider); err != nil {
		t.Fatal(err)
	}
	if gotProvider != "volc" {
		t.Fatalf("provider 落库不符: %q", gotProvider)
	}
}

func TestMigrate_幂等(t *testing.T) {
	s, ctx := newTestStore(t)
	for i := 0; i < 3; i++ {
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("第 %d 次迁移失败: %v", i+1, err)
		}
	}
}

// TestInsertUsageRecord_构造context取消后仍能落库 锁定一条容易被"修好"成 bug 的性质。
//
// 背景: golangci-lint 的 contextcheck 会指出流水 flush 用了
// context.Background() 而非从 New 传入的 ctx，建议"传 context"。这个建议
// 在这里是反的 —— New 收到的 ctx 通常只覆盖启动阶段（本测试与真实 main
// 里都是带超时的），若 flush 继承它，超时之后所有用量记录都会静默写不进去，
// 而 usage_records 是计费与对账的事实来源。
//
// 流水写入器的生命周期由 Close() 界定，不由构造时的 ctx 界定。
func TestInsertUsageRecord_构造context取消后仍能落库(t *testing.T) {
	dsn := testDSN()
	c, err := NewCipher(testCipherKey)
	if err != nil {
		t.Fatal(err)
	}

	// 故意用一个随即取消的 ctx 构造 Store
	ctx, cancel := context.WithCancel(context.Background())
	s, err := NewWithOptions(ctx, config.Postgres{DSN: dsn, MaxConns: 4, AutoMigrate: true},
		Options{Cipher: c, UsageFlushInterval: 30 * time.Millisecond, UsageBatchSize: 10})
	if err != nil {
		cancel()
		t.Skipf("跳过: 无可用 Postgres: %v", err)
	}
	truncate(t, s)
	cancel() // 构造用的 ctx 就此失效

	bg := context.Background()
	day := quota.QuotaDayTime(time.Now())
	const n = 25
	for i := 0; i < n; i++ {
		if err := s.InsertUsageRecord(bg, UsageRecord{
			RequestID: fmt.Sprintf("ctxdead-%d", i), UpstreamKeyID: "volc_001",
			QuotaDay: day, TotalTokens: 7, StatusCode: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := NewWithOptions(bg, config.Postgres{DSN: dsn, MaxConns: 2}, Options{Cipher: c})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		truncate(t, s2)
		s2.Close()
	}()

	var count int64
	if err := s2.pool.QueryRow(bg,
		`SELECT COUNT(*) FROM usage_records WHERE request_id LIKE 'ctxdead-%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Fatalf("构造 context 取消后流水丢失: 期望 %d 条，实际 %d —— "+
			"flush 不应继承构造期 context", n, count)
	}
}
