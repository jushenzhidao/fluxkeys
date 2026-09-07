package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fluxkeys/fluxkeys/internal/quota"
)

// provider 配置的读取侧。写入侧见 providers_write.go。
//
// 本文件的方法全部只读 provider_configs / config_versions / 用量表，
// 不做任何校验判断 —— 校验属于 config.Validate 与 admin 层的职责。

// provider 配置相关错误。调用方以 errors.Is 判断。
var (
	// ErrVersionConflict 表示乐观锁失败: 期望版本与当前版本不符。
	//
	// 与 ErrNotFound 分开是必需的: 两者在 UPDATE 层面都是「影响 0 行」，
	// 混在一起会让运维在「provider 名打错」与「配置已被别人改过」之间
	// 无从下手。
	ErrVersionConflict = errors.New("store: provider 配置版本冲突")

	// ErrProviderExists 表示同名 provider 已存在（含已软删除的）。
	//
	// 已软删除的也算占名: 它仍持有历史配额 key 与归档记录，复用其名会把
	// 新旧两段账目混成一体，而两段的额度体系可能完全不同。
	ErrProviderExists = errors.New("store: provider 已存在")

	// ErrNameImmutable 表示试图修改 provider 名。
	ErrNameImmutable = errors.New("store: provider 名不可修改")

	// ErrQuotaKindImmutable 表示试图修改 quota_kind。
	//
	// 它决定 Redis key 的 {kind} 段与归档量纲。count 改 token 后，Redis 里
	// 按次累加的计数会被当作 token 数解释（已用 800 次 → 800/5000000），
	// 水位瞬间显示为几乎空闲，调度器把实际已耗尽的 Key 排到最优先 ——
	// 全程没有任何错误日志。而两个量纲之间不存在确定的换算关系，
	// 跨存储做原子换算的代价远超收益，故物理禁改。
	ErrQuotaKindImmutable = errors.New("store: quota_kind 不可修改")
)

// snapshotSchema 是 config_versions.snapshot 的格式版本号。
//
// 必须有: 一年后给 provider 加了新字段，回滚到今天的旧快照时要能识别
// 「这个快照没有该字段，用默认值」，而不是把零值当成用户的选择写进去。
const snapshotSchema = 1

// ProviderConfig 是一个 provider 的当前生效配置，对应 provider_configs 一行。
type ProviderConfig struct {
	Name       string
	Enabled    bool
	BaseURL    string
	QuotaKind  string
	QuotaLimit int64
	// QuotaWindow 在库里存纳秒整数。不存 "24h" 文本是因为文本要在读取时
	// 解析，而解析失败会静默退化为 0 —— 周期为 0 意味着额度永不刷新。
	QuotaWindow time.Duration
	// RefreshHour 为 nil 表示无固定刷新点，与「0 点刷新」是两件事。
	RefreshHour     *int
	ModelMapping    map[string]string
	CountModels     []string
	ReasoningModels []string
	AdapterKind     string
	// CredentialEnv 只是环境变量名，不含凭据原文。
	CredentialEnv string
	Version       int64
	DeletedAt     *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// IsCountKind 报告该 provider 的配额口径是否为「按次」。
//
// 判定直接读本行的 quota_kind 而非绕回 config.IsCountProvider: 本表就是
// provider 配置的真相来源，两者取值必然相同，而少一次跨包查表就少一处
// 「用 token 数去除以按次额度」的可能 —— 那个比值两端都非零、也落在
// [0,1] 里，没有任何一层会报错。
func (p ProviderConfig) IsCountKind() bool { return p.QuotaKind == "count" }

// ProviderListItem 是列表视图: 当前配置 + 今日用量。
type ProviderListItem struct {
	ProviderConfig
	// TodayUsed 是本配额日的已用量，量纲随 QuotaKind 变化 ——
	// 按次计费返回调用次数，token 计费返回 token 数。
	TodayUsed int64
	// TodayRequests 是本配额日的请求数，与量纲无关。
	TodayRequests int64
}

// ConfigVersion 是一条配置版本历史，对应 config_versions 一行。
type ConfigVersion struct {
	ID            int64
	ProviderName  string
	Action        string
	ChangedFields []string
	// Snapshot 是该版本生效时该 provider 的完整配置 JSON。
	Snapshot json.RawMessage
	Reason   string
	// RolledBackFrom 非 nil 时表示本版本由回滚生成，值为来源版本号。
	RolledBackFrom *int64
	CreatedAt      time.Time
	CreatedBy      string
}

// providerColumns 是 provider_configs 的读取列清单。
//
// model_mapping 显式转 text 后由 Go 侧 unmarshal: 让 pgx 直接扫进 map 依赖
// 编解码器对 jsonb 的推断，而这里的目标类型是确定的，转 text 更不易出错。
const providerColumns = `name, enabled, base_url, quota_kind, quota_limit,
	quota_window_nanos, refresh_hour, model_mapping::text,
	count_models, reasoning_models, adapter_kind, credential_env,
	version, deleted_at, created_at, updated_at`

// scanProvider 按 providerColumns 的顺序扫描一行。
func scanProvider(row pgx.Row) (ProviderConfig, error) {
	var (
		p          ProviderConfig
		windowNS   int64
		mappingRaw string
	)
	err := row.Scan(&p.Name, &p.Enabled, &p.BaseURL, &p.QuotaKind, &p.QuotaLimit,
		&windowNS, &p.RefreshHour, &mappingRaw,
		&p.CountModels, &p.ReasoningModels, &p.AdapterKind, &p.CredentialEnv,
		&p.Version, &p.DeletedAt, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return ProviderConfig{}, err
	}
	p.QuotaWindow = time.Duration(windowNS)
	if mappingRaw != "" {
		if err := json.Unmarshal([]byte(mappingRaw), &p.ModelMapping); err != nil {
			return ProviderConfig{}, fmt.Errorf("store: 解析 model_mapping(%s): %w", p.Name, err)
		}
	}
	if p.ModelMapping == nil {
		// 回 nil 会让上层构造 adapter 时拿到 nil map，而 adapter 构造函数
		// 对 nil mapping 的行为是 panic 的前置条件之一（见 registry）。
		p.ModelMapping = map[string]string{}
	}
	if p.CountModels == nil {
		p.CountModels = []string{}
	}
	if p.ReasoningModels == nil {
		p.ReasoningModels = []string{}
	}
	return p, nil
}

// ListProviderConfigs 返回全部未软删除的 provider 配置，按名字升序。
//
// includeDeleted 为 true 时连软删除的一起返回 —— 重名校验必须看到它们。
func (s *Store) ListProviderConfigs(ctx context.Context, includeDeleted bool) ([]ProviderConfig, error) {
	q := `SELECT ` + providerColumns + ` FROM provider_configs`
	if !includeDeleted {
		q += ` WHERE deleted_at IS NULL`
	}
	q += ` ORDER BY name`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: 列出 provider 配置: %w", err)
	}
	defer rows.Close()

	var out []ProviderConfig
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描 provider 配置: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProviderConfig 读取单个 provider 的当前配置。不存在时返回 ErrNotFound。
//
// 软删除的行仍可读出（DeletedAt 非 nil）: 历史归档与版本回溯都需要它，
// 由调用方按 DeletedAt 决定如何呈现。
func (s *Store) GetProviderConfig(ctx context.Context, name string) (ProviderConfig, error) {
	p, err := scanProvider(s.pool.QueryRow(ctx,
		`SELECT `+providerColumns+` FROM provider_configs WHERE name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return ProviderConfig{}, ErrNotFound
	}
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("store: 读取 provider 配置: %w", err)
	}
	return p, nil
}

// CountProviderConfigs 返回未软删除的 provider 数量，供空库 seed 判定使用。
func (s *Store) CountProviderConfigs(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM provider_configs WHERE deleted_at IS NULL`).Scan(&n)
	if err != nil {
		// 表不存在时这里会报错，而调用方绝不能把它当成「表为空」去 seed：
		// auto_migrate=false 的部署需要的是一句明确的「请先执行迁移」。
		return 0, fmt.Errorf("store: 统计 provider 配置: %w", err)
	}
	return n, nil
}

// ListProvidersWithUsage 返回当前生效态 + 本配额日用量与请求数。
//
// 用量量纲按每个 provider 自己的 quota_kind 选: 按次计费的 provider 返回
// count_units 之和，token 计费返回 total_tokens 之和。一律返回 token 口径
// 会让按次 provider 的「已用」恒为 0（它们的 total_tokens 本就不记账），
// 界面上看起来永远空闲 —— 这是阶段一缺陷 #11 的复发面。
//
// 数据源取 usage_records 而非 key_daily_history: 归档是按日跑的批处理，
// 当日那一格要到次日才写入，而这里要的正是「今天用了多少」。
func (s *Store) ListProvidersWithUsage(ctx context.Context, now time.Time) ([]ProviderListItem, error) {
	cfgs, err := s.ListProviderConfigs(ctx, false)
	if err != nil {
		return nil, err
	}
	if len(cfgs) == 0 {
		return nil, nil
	}

	day := quota.QuotaDayTime(now)
	rows, err := s.pool.Query(ctx, `
		SELECT provider,
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(count_units), 0),
		       count(*)
		FROM usage_records
		WHERE quota_day = $1
		GROUP BY provider`, day)
	if err != nil {
		return nil, fmt.Errorf("store: 汇总 provider 今日用量: %w", err)
	}
	defer rows.Close()

	type agg struct{ tokens, counts, requests int64 }
	byProvider := make(map[string]agg, len(cfgs))
	for rows.Next() {
		var (
			name string
			a    agg
		)
		if err := rows.Scan(&name, &a.tokens, &a.counts, &a.requests); err != nil {
			return nil, fmt.Errorf("store: 扫描 provider 用量: %w", err)
		}
		byProvider[name] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 汇总 provider 今日用量: %w", err)
	}

	out := make([]ProviderListItem, 0, len(cfgs))
	for _, c := range cfgs {
		a := byProvider[c.Name]
		item := ProviderListItem{ProviderConfig: c, TodayRequests: a.requests}
		if c.IsCountKind() {
			item.TodayUsed = a.counts
		} else {
			item.TodayUsed = a.tokens
		}
		out = append(out, item)
	}
	return out, nil
}

// GetProviderPeakUsage 返回近 days 个配额日内**单 Key 单日**的最高用量。
//
// 供界面在编辑 quota_limit 时作参照: quota_limit 是单 Key 上限，因此这里
// 取的是 MAX(单行用量) 而非当日全 provider 汇总 —— 后者与上限不同量纲，
// 拿来对比只会误导运维把上限调到比实际已用还低。
//
// 量纲同样按 quota_kind 选，理由见 ListProvidersWithUsage。
func (s *Store) GetProviderPeakUsage(ctx context.Context, name string, days int, now time.Time) (int64, error) {
	if days <= 0 {
		days = 7
	}
	cfg, err := s.GetProviderConfig(ctx, name)
	if err != nil {
		return 0, err
	}

	col := "token_used"
	if cfg.IsCountKind() {
		col = "count_used"
	}
	since := quota.QuotaDayTime(now).AddDate(0, 0, -(days - 1))

	var peak int64
	// 列名由上面的白名单二选一决定，不含任何外部输入，故直接拼接安全。
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(`+col+`), 0) FROM key_daily_history
		WHERE provider = $1 AND quota_day >= $2`, name, since).Scan(&peak)
	if err != nil {
		return 0, fmt.Errorf("store: 读取 provider 峰值用量: %w", err)
	}
	return peak, nil
}

// ProviderHasTraffic 报告该 provider 是否有过流量。
//
// 两个数据源取或: usage_records 可能已被归档清理，key_daily_history 只有
// 跑过归档的日子才有行。只查其一都会漏，而漏判会让「有流量后禁改」的
// 守卫在最需要它的时候失效。
func (s *Store) ProviderHasTraffic(ctx context.Context, name string) (bool, error) {
	var has bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM usage_records      WHERE provider = $1)
		    OR EXISTS (SELECT 1 FROM key_daily_history  WHERE provider = $1)`,
		name).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("store: 判定 provider 流量: %w", err)
	}
	return has, nil
}

// ListConfigVersions 列出某 provider 的版本历史，按版本号倒序。
//
// before 为游标（返回 id < before 的行），0 表示从最新开始。用游标而非
// offset: 版本号天然单调，翻页期间新增版本不会让某一页漏行或重复。
func (s *Store) ListConfigVersions(ctx context.Context, providerName string, limit int, before int64) ([]ConfigVersion, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT id, provider_name, action, changed_fields, snapshot::text,
	             reason, rolled_back_from, created_at, created_by
	      FROM config_versions WHERE provider_name = $1`
	args := []any{providerName}
	if before > 0 {
		q += ` AND id < $2`
		args = append(args, before)
	}
	q += fmt.Sprintf(` ORDER BY id DESC LIMIT %d`, limit)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 列出配置版本: %w", err)
	}
	defer rows.Close()

	var out []ConfigVersion
	for rows.Next() {
		v, err := scanConfigVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetConfigVersion 读取某个版本的完整快照。不存在时返回 ErrNotFound。
func (s *Store) GetConfigVersion(ctx context.Context, id int64) (ConfigVersion, error) {
	v, err := scanConfigVersion(s.pool.QueryRow(ctx, `
		SELECT id, provider_name, action, changed_fields, snapshot::text,
		       reason, rolled_back_from, created_at, created_by
		FROM config_versions WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ConfigVersion{}, ErrNotFound
	}
	if err != nil {
		return ConfigVersion{}, err
	}
	return v, nil
}

func scanConfigVersion(row pgx.Row) (ConfigVersion, error) {
	var (
		v   ConfigVersion
		raw string
	)
	err := row.Scan(&v.ID, &v.ProviderName, &v.Action, &v.ChangedFields, &raw,
		&v.Reason, &v.RolledBackFrom, &v.CreatedAt, &v.CreatedBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ConfigVersion{}, err
		}
		return ConfigVersion{}, fmt.Errorf("store: 扫描配置版本: %w", err)
	}
	v.Snapshot = json.RawMessage(raw)
	if v.ChangedFields == nil {
		v.ChangedFields = []string{}
	}
	return v, nil
}
