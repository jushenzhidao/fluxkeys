// Package store 是 FluxKeys 的 Postgres 持久化层。
//
// 职责划分（与 internal/quota 严格互补）:
//
//   - Redis 持有全部**热状态**（配额计数、租约、限流），是配额的唯一权威（P0-1）；
//   - Postgres 持有全部**冷状态**（用户、Key 元数据、用量流水、审计），
//     是计费与审计的事实来源（P1-9）。
//
// 关键约束: 本包的任何方法都不参与配额准入判断。用量流水是事后记录，
// 写入失败只影响对账，绝不能影响请求成败 —— 因此流水写入走异步批量通道。
package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fluxkeys/fluxkeys/internal/config"
)

//go:embed schema.sql
var schemaSQL string

// 常见错误。调用方应以 errors.Is 判断，而非匹配错误字符串。
var (
	// ErrNotFound 表示记录不存在。
	ErrNotFound = errors.New("store: 记录不存在")
	// ErrKeyRevoked 表示 API Key 已被吊销或禁用。
	ErrKeyRevoked = errors.New("store: API Key 已吊销")
	// ErrUserSuspended 表示用户已被停用。
	ErrUserSuspended = errors.New("store: 用户已停用")
)

// Store 封装 Postgres 连接池与异步流水写入器。
type Store struct {
	pool   *pgxpool.Pool
	cipher *Cipher

	usage *usageWriter
}

// Options 是 Store 的可选构造参数。
type Options struct {
	// Cipher 覆盖默认的"从环境变量读取主密钥"行为，主要供测试使用。
	Cipher *Cipher
	// UsageBuffer 是流水异步通道容量。<=0 时使用默认值。
	UsageBuffer int
	// UsageFlushInterval 是流水定时 flush 间隔。<=0 时使用默认值。
	UsageFlushInterval time.Duration
	// UsageBatchSize 是单次批量写入的最大条数。<=0 时使用默认值。
	UsageBatchSize int
}

// New 创建连接池并校验连通性。
//
// 主密钥缺失时直接返回错误 —— 宁可启动失败，也不允许明文存储火山 Key。
func New(ctx context.Context, cfg config.Postgres) (*Store, error) {
	return NewWithOptions(ctx, cfg, Options{})
}

// NewWithOptions 是 New 的可配置版本。
func NewWithOptions(ctx context.Context, cfg config.Postgres, opt Options) (*Store, error) {
	c := opt.Cipher
	if c == nil {
		var err error
		if c, err = NewCipherFromEnv(); err != nil {
			return nil, err
		}
	}

	pcfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("store: 解析 DSN: %w", err)
	}
	if cfg.MaxConns > 0 {
		pcfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		pcfg.MinConns = cfg.MinConns
	}

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("store: 建立连接池: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping postgres: %w", err)
	}

	s := &Store{pool: pool, cipher: c}
	s.usage = newUsageWriter(s, opt) //nolint:contextcheck // flush 刻意用 Background，理由见 usage.go 的 flush 注释

	if cfg.AutoMigrate {
		if err := s.Migrate(ctx); err != nil {
			// 此处 Close 只为释放刚建立的连接池与后台 goroutine。流水缓冲区
			// 必然为空（还没有任何请求），因此 Close 的 flush 错误无信息量，
			// 显式丢弃并保留原始的迁移失败原因。
			_ = s.Close()
			return nil, err
		}
	}
	return s, nil
}

// Pool 暴露底层连接池，供需要自定义查询的上层（如管理接口）使用。
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Cipher 返回密钥加解密器。
func (s *Store) Cipher() *Cipher { return s.cipher }

// Migrate 执行内嵌的 schema.sql。schema 全部语句均为幂等（IF NOT EXISTS），
// 可重复执行。
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("store: 执行 schema 迁移: %w", err)
	}
	return nil
}

// Close 先 flush 未落库的流水，再关闭连接池。
//
// 顺序不可颠倒: 先关池会导致缓冲区里的流水永久丢失。
func (s *Store) Close() error {
	var err error
	if s.usage != nil {
		err = s.usage.Close()
	}
	if s.pool != nil {
		s.pool.Close()
	}
	return err
}

// ---------- 用户 ----------

// CreateUser 新建用户。
func (s *Store) CreateUser(ctx context.Context, u *User) (*User, error) {
	if u == nil || u.Name == "" {
		return nil, errors.New("store: 用户名不能为空")
	}
	status := u.Status
	if status == "" {
		status = "active"
	}
	var email any
	if u.Email != "" {
		email = u.Email
	}

	out := *u
	row := s.pool.QueryRow(ctx, `
		INSERT INTO users (name, email, status, daily_token_limit, rpm_limit, tpm_limit)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, status, created_at, updated_at`,
		u.Name, email, status, u.DailyTokenLimit, u.RPMLimit, u.TPMLimit)
	if err := row.Scan(&out.ID, &out.Status, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return nil, fmt.Errorf("store: 创建用户: %w", err)
	}
	return &out, nil
}

// ListUsers 返回全部用户，按 id 升序。
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, COALESCE(email, ''), status,
		       daily_token_limit, rpm_limit, tpm_limit, created_at, updated_at
		FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: 列出用户: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.Email, &u.Status,
			&u.DailyTokenLimit, &u.RPMLimit, &u.TPMLimit, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: 扫描用户: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUser 按 id 读取用户。
func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, COALESCE(email, ''), status,
		       daily_token_limit, rpm_limit, tpm_limit, created_at, updated_at
		FROM users WHERE id = $1`, id).
		Scan(&u.ID, &u.Name, &u.Email, &u.Status,
			&u.DailyTokenLimit, &u.RPMLimit, &u.TPMLimit, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取用户: %w", err)
	}
	return &u, nil
}

// CreateUserAPIKey 为用户创建一条 API Key，返回明文。
//
// 明文只在此处返回一次，之后无法再取回 —— 库里只有 SHA-256 哈希。
func (s *Store) CreateUserAPIKey(ctx context.Context, userID int64, name string) (plaintext string, rec *UserAPIKey, err error) {
	plaintext, err = NewUserKey()
	if err != nil {
		return "", nil, err
	}
	hash := HashUserKey(plaintext)
	prefix := KeyPrefix(plaintext)

	rec = &UserAPIKey{UserID: userID, KeyHash: hash, KeyPrefix: prefix, Name: name}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO user_api_keys (user_id, key_hash, key_prefix, name)
		VALUES ($1, $2, $3, $4)
		RETURNING id, status, created_at`,
		userID, hash, prefix, name)
	if err = row.Scan(&rec.ID, &rec.Status, &rec.CreatedAt); err != nil {
		return "", nil, fmt.Errorf("store: 创建 API Key: %w", err)
	}
	return plaintext, rec, nil
}

// RevokeUserAPIKey 吊销一条 API Key。
//
// 带 userID 做归属校验: 吊销入口暴露在管理接口的 /admin/users/{id}/keys/{key_id}
// 路径上，不校验归属的话，拼错 user_id 也能吊掉别人的 Key —— 单条 WHERE
// 在一次往返内完成校验与更新，不留 TOCTOU 窗口。
//
// 只吊销 active 状态的 Key: 重复吊销返回 ErrNotFound，让调用方能区分
// 「这次真的吊销了」与「早就不是 active 了」—— 审计需要这个区分。
func (s *Store) RevokeUserAPIKey(ctx context.Context, userID, keyID int64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE user_api_keys SET status = 'revoked', revoked_at = now()
		WHERE id = $1 AND user_id = $2 AND status = 'active'`, keyID, userID)
	if err != nil {
		return fmt.Errorf("store: 吊销 API Key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListUserAPIKeys 列出某用户的全部 API Key（不含明文）。
func (s *Store) ListUserAPIKeys(ctx context.Context, userID int64) ([]UserAPIKey, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, key_hash, key_prefix, name, status, last_used_at, created_at, revoked_at
		FROM user_api_keys WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 列出 API Key: %w", err)
	}
	defer rows.Close()

	var out []UserAPIKey
	for rows.Next() {
		var k UserAPIKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.KeyHash, &k.KeyPrefix, &k.Name,
			&k.Status, &k.LastUsedAt, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, fmt.Errorf("store: 扫描 API Key: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// AuthenticateUserKey 校验明文 API Key，返回调用方身份与限额。
//
// 明文绝不参与查询条件之外的任何用途，也不写日志。区分三种失败:
// 不存在(ErrNotFound) / 已吊销(ErrKeyRevoked) / 用户停用(ErrUserSuspended)，
// 便于上层返回精确的错误信息，同时对外仍统一为 401。
func (s *Store) AuthenticateUserKey(ctx context.Context, plaintextKey string) (*AuthContext, error) {
	if plaintextKey == "" {
		return nil, ErrNotFound
	}
	hash := HashUserKey(plaintextKey)

	var (
		ac        AuthContext
		keyStatus string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT k.id, k.key_prefix, k.status,
		       u.id, u.name, u.status, u.rpm_limit, u.tpm_limit, u.daily_token_limit
		FROM user_api_keys k JOIN users u ON u.id = k.user_id
		WHERE k.key_hash = $1`, hash).
		Scan(&ac.KeyID, &ac.KeyPrefix, &keyStatus,
			&ac.UserID, &ac.UserName, &ac.Status, &ac.RPMLimit, &ac.TPMLimit, &ac.DailyTokenLimit)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 鉴权查询: %w", err)
	}
	if keyStatus != "active" {
		return nil, ErrKeyRevoked
	}
	if ac.Status != "active" {
		return nil, ErrUserSuspended
	}
	return &ac, nil
}

// TouchUserAPIKey 异步更新 last_used_at。
//
// 刻意不在鉴权路径上同步执行: 每个请求一次 UPDATE 会让同一行成为热点，
// 而 last_used_at 的精度对业务毫无价值。
func (s *Store) TouchUserAPIKey(ctx context.Context, keyID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE user_api_keys SET last_used_at = now() WHERE id = $1`, keyID)
	if err != nil {
		return fmt.Errorf("store: 更新 API Key 使用时间: %w", err)
	}
	return nil
}
