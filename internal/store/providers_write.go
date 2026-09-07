package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// provider 配置的写入侧。
//
// 每次写操作在**单个事务**内完成两件事，缺一则当前态与版本历史会漂移:
//
//  1. INSERT INTO config_versions 拿到新版本号（BIGSERIAL 全局单调）；
//  2. 写 provider_configs 并把该行 version 指向新版本号。
//
// audit_logs 刻意放在事务之外、由 admin 层在提交成功后写: config_versions
// 已经是不可抵赖的变更记录，audit_logs 是跨资源的统一检索入口而非唯一凭据。
// 让审计写失败去回滚一次已生效的配置变更是本末倒置。

// ProviderSnapshot 是写入 config_versions.snapshot 的 JSON 结构。
//
// 字段名与 API 请求体一致，让「看历史版本」与「重新提交」用的是同一套词汇。
type ProviderSnapshot struct {
	// Schema 是快照格式版本。见 snapshotSchema 的说明。
	Schema          int               `json:"schema"`
	Name            string            `json:"name"`
	Enabled         bool              `json:"enabled"`
	BaseURL         string            `json:"base_url"`
	QuotaKind       string            `json:"quota_kind"`
	QuotaLimit      int64             `json:"quota_limit"`
	QuotaWindowNS   int64             `json:"quota_window_nanos"`
	RefreshHour     *int              `json:"refresh_hour"`
	ModelMapping    map[string]string `json:"model_mapping"`
	CountModels     []string          `json:"count_models"`
	ReasoningModels []string          `json:"reasoning_models"`
	AdapterKind     string            `json:"adapter_kind"`
	CredentialEnv   string            `json:"credential_env"`
	Deleted         bool              `json:"deleted"`
}

// Snapshot 把当前配置转为快照结构。
func (p ProviderConfig) Snapshot() ProviderSnapshot {
	mapping := p.ModelMapping
	if mapping == nil {
		mapping = map[string]string{}
	}
	count := p.CountModels
	if count == nil {
		count = []string{}
	}
	reasoning := p.ReasoningModels
	if reasoning == nil {
		reasoning = []string{}
	}
	return ProviderSnapshot{
		Schema:          snapshotSchema,
		Name:            p.Name,
		Enabled:         p.Enabled,
		BaseURL:         p.BaseURL,
		QuotaKind:       p.QuotaKind,
		QuotaLimit:      p.QuotaLimit,
		QuotaWindowNS:   int64(p.QuotaWindow),
		RefreshHour:     p.RefreshHour,
		ModelMapping:    mapping,
		CountModels:     count,
		ReasoningModels: reasoning,
		AdapterKind:     p.AdapterKind,
		CredentialEnv:   p.CredentialEnv,
		Deleted:         p.DeletedAt != nil,
	}
}

// ProviderWrite 是一次 provider 配置写入的入参。
type ProviderWrite struct {
	// Config 是变更后的完整配置。局部更新由调用方先读当前行再改字段，
	// 存储层只接受全量 —— 让存储层做字段级合并意味着它要知道「哪些字段
	// 是零值即不改」，而那正是 PATCH 语义反复写错的地方。
	Config ProviderConfig
	// Action 取 seed / create / update / delete / rollback。
	Action string
	// ChangedFields 是本次变化的字段名，纯展示用。
	ChangedFields []string
	Reason        string
	Actor         string
	// RolledBackFrom 非 nil 时表示本次由回滚生成，值为来源版本号。
	RolledBackFrom *int64
	// ExpectedVersion 非 nil 时启用乐观锁: 与库中当前 version 不等则拒绝。
	//
	// 用指针而非 0 值判空: 版本号 0 是 seed 之前的合法初值，用 0 表示
	// 「跳过检查」会让「期望版本为 0」这个合法请求静默绕过乐观锁。
	ExpectedVersion *int64
}

// CreateProvider 新增一个 provider，返回新版本号。
//
// 同名（含已软删除）时返回 ErrProviderExists。
func (s *Store) CreateProvider(ctx context.Context, in ProviderWrite) (int64, error) {
	if in.Config.Name == "" {
		return 0, errors.New("store: provider 名不能为空")
	}
	if in.Action == "" {
		in.Action = "create"
	}

	var version int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// 显式先查而非依赖主键冲突: 主键冲突分不出「重名」与「并发插入」，
		// 而软删除的行也占名，靠 ON CONFLICT 会把它悄悄复活成新配置，
		// 新旧两段账目从此混在一起。
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM provider_configs WHERE name = $1)`,
			in.Config.Name).Scan(&exists); err != nil {
			return fmt.Errorf("store: 检查 provider 重名: %w", err)
		}
		if exists {
			return ErrProviderExists
		}

		v, err := insertConfigVersion(ctx, tx, in)
		if err != nil {
			return err
		}
		version = v

		p := in.Config
		_, err = tx.Exec(ctx, `
			INSERT INTO provider_configs (
				name, enabled, base_url, quota_kind, quota_limit, quota_window_nanos,
				refresh_hour, model_mapping, count_models, reasoning_models,
				adapter_kind, credential_env, version)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$11,$12,$13)`,
			p.Name, p.Enabled, p.BaseURL, p.QuotaKind, p.QuotaLimit, int64(p.QuotaWindow),
			p.RefreshHour, mustMappingJSON(p.ModelMapping), p.CountModels, p.ReasoningModels,
			p.AdapterKind, p.CredentialEnv, version)
		if err != nil {
			return fmt.Errorf("store: 插入 provider 配置: %w", err)
		}
		return nil
	})
	return version, err
}

// UpdateProvider 更新一个 provider 并生成新版本，返回新版本号。
//
// name 与 quota_kind 在这里再拦一次（API 层已拦）。两层都拦不是冗余:
// 存储层是唯一无法绕过的入口，而未来可能有别的调用方（迁移脚本、回滚
// 路径）直接调它。这两个字段一旦改动，Redis 里的配额计数会整体错位而
// 全程不报错 —— 这类守卫值得放在最里层。
func (s *Store) UpdateProvider(ctx context.Context, in ProviderWrite) (int64, error) {
	if in.Action == "" {
		in.Action = "update"
	}

	var version int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := scanProvider(tx.QueryRow(ctx,
			`SELECT `+providerColumns+` FROM provider_configs WHERE name = $1 FOR UPDATE`,
			in.Config.Name))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: 读取待更新 provider: %w", err)
		}
		if cur.Name != in.Config.Name {
			return ErrNameImmutable
		}
		if in.Config.QuotaKind != cur.QuotaKind {
			return ErrQuotaKindImmutable
		}
		// 乐观锁判定必须在同一事务内、且在 FOR UPDATE 之后 ——
		// 先查再改会留下 TOCTOU 窗口，两个并发请求各自校验通过再互相覆盖，
		// 守卫看起来生效而实际形同虚设。
		if in.ExpectedVersion != nil && *in.ExpectedVersion != cur.Version {
			return fmt.Errorf("%w: 当前 %d，期望 %d",
				ErrVersionConflict, cur.Version, *in.ExpectedVersion)
		}

		// 变更字段清单在事务内、拿到 FOR UPDATE 锁下的 cur 之后才算。
		//
		// 调用方在事务外读一次再算是不行的: 那份 before 可能已被并发写覆盖，
		// 于是历史里记着「改了 base_url」而实际这次只改了 quota_limit。
		// 调用方显式传了就尊重（seed 之类场景有自己的语义）。
		if in.ChangedFields == nil {
			in.ChangedFields = DiffFields(cur, in.Config)
		}

		if version, err = insertConfigVersion(ctx, tx, in); err != nil {
			return err
		}

		p := in.Config
		_, err = tx.Exec(ctx, `
			UPDATE provider_configs SET
				enabled = $2, base_url = $3, quota_limit = $4, quota_window_nanos = $5,
				refresh_hour = $6, model_mapping = $7::jsonb, count_models = $8,
				reasoning_models = $9, adapter_kind = $10, credential_env = $11,
				version = $12, deleted_at = NULL, updated_at = now()
			WHERE name = $1`,
			p.Name, p.Enabled, p.BaseURL, p.QuotaLimit, int64(p.QuotaWindow),
			p.RefreshHour, mustMappingJSON(p.ModelMapping), p.CountModels,
			p.ReasoningModels, p.AdapterKind, p.CredentialEnv, version)
		if err != nil {
			return fmt.Errorf("store: 更新 provider 配置: %w", err)
		}
		return nil
	})
	return version, err
}

// DeleteProvider 软删除一个 provider，返回新版本号。
//
// 只置 deleted_at，不物理删。有流量的 provider 被物理删后，usage_records
// 与 key_daily_history 里那批行会变成无法归因的孤儿数据 —— 账目从此对不上，
// 而且这个损失是不可逆的。
func (s *Store) DeleteProvider(ctx context.Context, name, reason, actor string, expectedVersion *int64) (int64, error) {
	var version int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := scanProvider(tx.QueryRow(ctx,
			`SELECT `+providerColumns+` FROM provider_configs
			 WHERE name = $1 AND deleted_at IS NULL FOR UPDATE`, name))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: 读取待删除 provider: %w", err)
		}
		if expectedVersion != nil && *expectedVersion != cur.Version {
			return fmt.Errorf("%w: 当前 %d，期望 %d",
				ErrVersionConflict, cur.Version, *expectedVersion)
		}

		// 快照记停用**后**的状态，让回滚能把它原样恢复。
		//
		// DeletedAt 必须一并置上: Snapshot() 是按 DeletedAt != nil 推导
		// snapshot.deleted 的，而 cur 读的是删除前的行（DeletedAt 仍为 nil）。
		// 只改 Enabled 会写出一份 deleted:false 的快照，日后回滚到这个
		// 「停用版本」时 RollbackProvider 依据 deleted:false 把 deleted_at
		// 清空 —— 一个本该保持停用的 provider 被静默复活并重新进入路由。
		snap := cur
		snap.Enabled = false
		now := time.Now()
		snap.DeletedAt = &now
		version, err = insertConfigVersion(ctx, tx, ProviderWrite{
			Config:        snap,
			Action:        "delete",
			ChangedFields: []string{"deleted_at"},
			Reason:        reason,
			Actor:         actor,
		})
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			UPDATE provider_configs
			SET deleted_at = now(), enabled = FALSE, version = $2, updated_at = now()
			WHERE name = $1`, name, version)
		if err != nil {
			return fmt.Errorf("store: 软删除 provider: %w", err)
		}
		return nil
	})
	return version, err
}

// insertConfigVersion 写一条版本历史并返回新版本号。必须在事务内调用。
func insertConfigVersion(ctx context.Context, tx pgx.Tx, in ProviderWrite) (int64, error) {
	blob, err := json.Marshal(in.Config.Snapshot())
	if err != nil {
		return 0, fmt.Errorf("store: 序列化 provider 快照: %w", err)
	}
	changed := in.ChangedFields
	if changed == nil {
		changed = []string{}
	}

	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO config_versions (
			provider_name, action, changed_fields, snapshot, reason,
			rolled_back_from, created_by)
		VALUES ($1,$2,$3,$4::jsonb,$5,$6,$7)
		RETURNING id`,
		in.Config.Name, in.Action, changed, string(blob), in.Reason,
		in.RolledBackFrom, in.Actor).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: 写入配置版本: %w", err)
	}
	return id, nil
}

// inTx 在事务内执行 fn，出错回滚。
//
// 单独抽出来是因为 provider 的每个写操作都必须是「版本历史 + 当前态」的
// 原子对: 少了任何一半，界面上看到的版本号与实际生效的配置就会漂移，
// 而这个漂移不报错，只让运维在回滚时拿到一份对不上的历史。
func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 开启事务: %w", err)
	}
	defer func() {
		// Rollback 在已提交的事务上返回 ErrTxClosed，属预期，显式丢弃。
		_ = tx.Rollback(ctx)
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: 提交事务: %w", err)
	}
	return nil
}

// mustMappingJSON 把 model_mapping 序列化为 JSON 文本。
//
// nil 与空 map 都落成 "{}"，不落 "null": jsonb 列里的 null 读回来是 nil map，
// 而 nil mapping 会让 adapter 构造走进 panic 的前置条件。
func mustMappingJSON(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		// map[string]string 的序列化不会失败，兜底也只回空对象而非空串 ——
		// 空串写进 jsonb 列会直接报语法错误，把一个不可能发生的分支变成
		// 一次真实的写入失败。
		return "{}"
	}
	return string(b)
}
