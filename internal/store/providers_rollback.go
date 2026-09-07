package store

// provider 配置的回滚。
//
// 与 providers_write.go 的 CRUD 分文件: CRUD 的输入是调用方给的完整配置，
// 而回滚的输入来自历史表，多出「目标版本是否可用」这一整类校验
// （属不属于本 provider、量纲变没变、停用态要不要一并恢复）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RollbackProvider 以目标版本的快照创建一个**新版本**，返回新版本号。
//
// 不把 provider_configs.version 改回旧值: 那样版本链会出现空洞，
// 「当前生效的是 #124」与「#125-#128 曾经生效过」两个事实就都读不出来了。
// 回滚本身留痕，也让回滚可再回滚。
//
// 目标版本的 quota_kind 与当前不一致时返回 ErrQuotaKindImmutable ——
// 跨量纲回滚是永远不该成功的操作，理由见该错误的说明。
func (s *Store) RollbackProvider(ctx context.Context, name string, targetVersionID int64, reason, actor string, expectedVersion *int64) (int64, ProviderSnapshot, error) {
	var (
		version int64
		target  ProviderSnapshot
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := scanProvider(tx.QueryRow(ctx,
			`SELECT `+providerColumns+` FROM provider_configs WHERE name = $1 FOR UPDATE`, name))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: 读取待回滚 provider: %w", err)
		}
		if expectedVersion != nil && *expectedVersion != cur.Version {
			return fmt.Errorf("%w: 当前 %d，期望 %d",
				ErrVersionConflict, cur.Version, *expectedVersion)
		}

		var (
			raw       string
			belongsTo string
		)
		err = tx.QueryRow(ctx,
			`SELECT snapshot::text, provider_name FROM config_versions WHERE id = $1`,
			targetVersionID).Scan(&raw, &belongsTo)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: 读取目标版本: %w", err)
		}
		if belongsTo != name {
			// 版本号是全局单调的，因此「存在但属于别的 provider」是可能的。
			// 不拦住的话会把 A 的配置整份写进 B —— 包括 base_url 与凭据变量名。
			return fmt.Errorf("store: 版本 %d 属于 provider %q，不属于 %q",
				targetVersionID, belongsTo, name)
		}
		if err := json.Unmarshal([]byte(raw), &target); err != nil {
			return fmt.Errorf("store: 解析目标版本快照: %w", err)
		}
		if target.QuotaKind != cur.QuotaKind {
			return fmt.Errorf("%w: 当前 %s，目标版本 %s",
				ErrQuotaKindImmutable, cur.QuotaKind, target.QuotaKind)
		}

		next := cur
		next.Enabled = target.Enabled
		next.BaseURL = target.BaseURL
		next.QuotaLimit = target.QuotaLimit
		next.QuotaWindow = QuotaWindowFromNanos(target.QuotaWindowNS)
		next.RefreshHour = target.RefreshHour
		next.ModelMapping = target.ModelMapping
		next.CountModels = target.CountModels
		next.ReasoningModels = target.ReasoningModels
		next.AdapterKind = target.AdapterKind
		next.CredentialEnv = target.CredentialEnv

		src := targetVersionID
		version, err = insertConfigVersion(ctx, tx, ProviderWrite{
			Config:         next,
			Action:         "rollback",
			ChangedFields:  DiffFields(cur, next),
			Reason:         reason,
			Actor:          actor,
			RolledBackFrom: &src,
		})
		if err != nil {
			return err
		}

		// deleted_at 必须跟着目标快照走，不能沿用当前值。
		//
		// 快照里存了 Deleted，所以「回滚到停用前的那一版」在语义上就包含
		// 「恢复启用」。若这里不写 deleted_at，那次回滚会返回成功、版本号
		// 照常递增、快照里明明白白写着 deleted:false，而表里的行仍是软删除态 ——
		// provider 依旧不出现在路由和列表里。运维看到的是一次成功的回滚
		// 配上一个查无此 provider 的结果，且没有任何错误可查。
		// COALESCE 让停用时间戳保持原值: 目标版本是停用态时，若当前行已经
		// 有 deleted_at 就沿用，「什么时候停的」这个事实不该被一次回滚覆盖。
		var deletedAt any
		if target.Deleted {
			deletedAt = cur.DeletedAt
		}

		_, err = tx.Exec(ctx, `
			UPDATE provider_configs SET
				enabled = $2, base_url = $3, quota_limit = $4, quota_window_nanos = $5,
				refresh_hour = $6, model_mapping = $7::jsonb, count_models = $8,
				reasoning_models = $9, adapter_kind = $10, credential_env = $11,
				version = $12,
				deleted_at = CASE WHEN $14::bool THEN COALESCE($13::timestamptz, now()) ELSE NULL END,
				updated_at = now()
			WHERE name = $1`,
			next.Name, next.Enabled, next.BaseURL, next.QuotaLimit, int64(next.QuotaWindow),
			next.RefreshHour, mustMappingJSON(next.ModelMapping), next.CountModels,
			next.ReasoningModels, next.AdapterKind, next.CredentialEnv, version,
			deletedAt, target.Deleted)
		if err != nil {
			return fmt.Errorf("store: 回滚 provider 配置: %w", err)
		}
		return nil
	})
	return version, target, err
}
