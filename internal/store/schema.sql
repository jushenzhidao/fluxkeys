-- FluxKeys 持久化模型
--
-- P1-9: V3 数据模型只有火山 Key/IP/配额，缺失用户、计费与审计，
-- 导致对外服务的网关无法回答「哪个用户本月用了多少 token」。
-- 本 schema 补齐用户维度。

CREATE TABLE IF NOT EXISTS users (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT        NOT NULL,
    email        TEXT        UNIQUE,
    status       TEXT        NOT NULL DEFAULT 'active',  -- active / suspended
    -- 用户级配额与限流（0 表示不限）
    daily_token_limit BIGINT NOT NULL DEFAULT 0,
    rpm_limit    INTEGER     NOT NULL DEFAULT 0,
    tpm_limit    BIGINT      NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 用户 API Key。仅存哈希，明文只在创建时返回一次。
CREATE TABLE IF NOT EXISTS user_api_keys (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key_hash    TEXT        NOT NULL UNIQUE,
    key_prefix  TEXT        NOT NULL,           -- 前 8 位，用于展示识别
    name        TEXT        NOT NULL DEFAULT '',
    status      TEXT        NOT NULL DEFAULT 'active',
    last_used_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_user_api_keys_hash ON user_api_keys(key_hash) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_user_api_keys_user ON user_api_keys(user_id);

-- 上游 Key 池（多 Provider 支持）。密钥密文存储。
CREATE TABLE IF NOT EXISTS upstream_keys (
    id            BIGSERIAL PRIMARY KEY,
    key_id        TEXT        NOT NULL UNIQUE,  -- 业务标识，如 volc_001, sensenova_001
    secret_enc    TEXT        NOT NULL,         -- 加密后的密钥
    provider      TEXT        NOT NULL,         -- volc / sensenova / ...
    pool          TEXT        NOT NULL DEFAULT 'cold',   -- hot / warm / cold
    status        TEXT        NOT NULL DEFAULT 'active', -- active / cooldown / banned / invalid
    persona_id    TEXT        NOT NULL DEFAULT '',
    egress_ip     TEXT        NOT NULL DEFAULT '',
    -- shard 是该 Key 的机器归属（多机部署分片）。
    -- Key 终身绑定出口 IP，IP 物理上属于某台机器，因此 Key 的机器归属
    -- 由 IP 决定且一经分配不再漂移 —— 跨机迁移等于换出口 IP，正是风控
    -- 最敏感的「老账号换 IP」信号。空串表示未分片（单机部署全量装载）。
    shard         TEXT        NOT NULL DEFAULT '',
    health_score  INTEGER     NOT NULL DEFAULT 100,
    -- 刷新探测状态（P0-4）
    refresh_state TEXT        NOT NULL DEFAULT 'idle',   -- idle / probing / confirmed / failed
    refresh_confirmed_at TIMESTAMPTZ,
    last_error    TEXT        NOT NULL DEFAULT '',
    last_used_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 增量迁移: CREATE TABLE IF NOT EXISTS 对已存在的表是空操作，不会补列。
-- 因此新增列必须在**任何引用它的语句之前**显式补上 —— 紧邻的
-- idx_upstream_keys_shard 就用到 shard，放到文件末尾的「增量迁移」段
-- 会导致该索引先于列创建，直接报 column "shard" does not exist，
-- 使任何既有部署升级后无法启动（新库因建表时已带该列而看不出问题）。
ALTER TABLE upstream_keys ADD COLUMN IF NOT EXISTS shard TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_upstream_keys_provider_status ON upstream_keys(provider, status);
CREATE INDEX IF NOT EXISTS idx_upstream_keys_pool_status ON upstream_keys(pool, status);
CREATE INDEX IF NOT EXISTS idx_upstream_keys_status ON upstream_keys(status);
CREATE INDEX IF NOT EXISTS idx_upstream_keys_shard ON upstream_keys(shard, status);

-- 出口 IP 注册表
CREATE TABLE IF NOT EXISTS egress_ips (
    id          BIGSERIAL PRIMARY KEY,
    addr        TEXT        NOT NULL UNIQUE,   -- 本机绑定地址（TCP 源地址）
    public_ip   TEXT        NOT NULL DEFAULT '',
    region      TEXT        NOT NULL DEFAULT '',
    isp         TEXT        NOT NULL DEFAULT '',
    max_keys    INTEGER     NOT NULL DEFAULT 10,
    state       TEXT        NOT NULL DEFAULT 'active',
    reputation  INTEGER     NOT NULL DEFAULT 100,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 用量流水。这是计费与审计的事实来源。
CREATE TABLE IF NOT EXISTS usage_records (
    id                BIGSERIAL PRIMARY KEY,
    request_id        TEXT        NOT NULL,
    user_id           BIGINT      REFERENCES users(id) ON DELETE SET NULL,
    user_api_key_id   BIGINT      REFERENCES user_api_keys(id) ON DELETE SET NULL,
    upstream_key_id   TEXT        NOT NULL DEFAULT '',
    egress_ip         TEXT        NOT NULL DEFAULT '',
    provider          TEXT        NOT NULL DEFAULT 'volc',
    model             TEXT        NOT NULL DEFAULT '',
    billing_kind      TEXT        NOT NULL DEFAULT 'token',  -- token / count
    -- 配额日而非自然日，与 12:00 刷新周期对齐（P0-3）
    quota_day         DATE        NOT NULL,
    prompt_tokens     BIGINT      NOT NULL DEFAULT 0,
    completion_tokens BIGINT      NOT NULL DEFAULT 0,
    -- 思维链用量。已含在 completion_tokens 内，不参与计费，仅用于
    -- 定位推理模型「预扣不足」的成本归因。
    reasoning_tokens  BIGINT      NOT NULL DEFAULT 0,
    total_tokens      BIGINT      NOT NULL DEFAULT 0,
    count_units       INTEGER     NOT NULL DEFAULT 0,
    estimated_tokens  BIGINT      NOT NULL DEFAULT 0,       -- 预扣量，用于评估估算准确度
    status_code       INTEGER     NOT NULL DEFAULT 0,
    is_stream         BOOLEAN     NOT NULL DEFAULT false,
    error_code        TEXT        NOT NULL DEFAULT '',
    retry_count       INTEGER     NOT NULL DEFAULT 0,
    latency_ms        INTEGER     NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 同理: 先补列再建索引。当前没有索引引用 reasoning_tokens，但保持同一
-- 规则可以避免「以后给新列加索引」时重新踩一遍顺序坑。
ALTER TABLE usage_records ADD COLUMN IF NOT EXISTS reasoning_tokens BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_usage_user_day  ON usage_records(user_id, quota_day);
CREATE INDEX IF NOT EXISTS idx_usage_key_day   ON usage_records(upstream_key_id, quota_day);
CREATE INDEX IF NOT EXISTS idx_usage_provider_day ON usage_records(provider, quota_day);
CREATE INDEX IF NOT EXISTS idx_usage_created   ON usage_records(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_request   ON usage_records(request_id);

-- Key 每日归档，供跨日记忆打分使用（S_history）
CREATE TABLE IF NOT EXISTS key_daily_history (
    upstream_key_id  TEXT   NOT NULL,
    provider         TEXT   NOT NULL,
    quota_day        DATE   NOT NULL,
    token_used       BIGINT NOT NULL DEFAULT 0,
    count_used       INTEGER NOT NULL DEFAULT 0,
    token_limit      BIGINT NOT NULL DEFAULT 0,
    token_ratio      DOUBLE PRECISION NOT NULL DEFAULT 0,
    request_count    INTEGER NOT NULL DEFAULT 0,
    error_count      INTEGER NOT NULL DEFAULT 0,
    consecutive_light_days INTEGER NOT NULL DEFAULT 0,
    -- provider 必须进主键: 同一 Key 迁到别的上游后，两段历史属于不同的
    -- 额度体系（水位不同），合并成一行会让 token_ratio 失去意义。
    PRIMARY KEY (upstream_key_id, provider, quota_day)
);
-- 调度按 (day, keyIDs) 反查，主键前缀顺序不匹配，补一条覆盖索引
CREATE INDEX IF NOT EXISTS idx_kdh_day_key ON key_daily_history(quota_day, upstream_key_id);

-- 管理操作审计
CREATE TABLE IF NOT EXISTS audit_logs (
    id         BIGSERIAL PRIMARY KEY,
    actor      TEXT        NOT NULL DEFAULT '',
    action     TEXT        NOT NULL,
    target     TEXT        NOT NULL DEFAULT '',
    detail     JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_logs(created_at DESC);

-- 配额对账偏差记录，用于追踪异常（P0-2）
CREATE TABLE IF NOT EXISTS quota_drift_logs (
    id              BIGSERIAL PRIMARY KEY,
    upstream_key_id TEXT        NOT NULL,
    provider        TEXT        NOT NULL,
    billing_kind    TEXT        NOT NULL,
    quota_day       DATE        NOT NULL,
    drift           BIGINT      NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_drift_created ON quota_drift_logs(created_at DESC);

-- ============================================================
-- 增量迁移
-- 上面的 CREATE TABLE IF NOT EXISTS 对已存在的表不生效，新增列
-- 必须在此显式声明，否则已部署实例升级后写入会因缺列而全部失败。
-- 所有语句必须幂等，本文件在每次启动时重复执行。
-- ============================================================

-- 注意: 补列语句（ALTER TABLE ... ADD COLUMN IF NOT EXISTS）**不在这里**。
-- 它们必须紧跟各自的 CREATE TABLE，位于任何引用新列的语句之前 ——
-- 放到本段会让「建索引」先于「补列」执行并发失败。见文件中相应位置的注释。

-- 归档主键补齐 provider。
--
-- 旧主键 (upstream_key_id, quota_day) 会把同一 Key 在不同上游的用量挤成
-- 一行，provider 列由最后一次 upsert 决定，历史打分因此读到错误上游的
-- 水位。已存在的行无法可靠拆分（源流水才是真相），交给下一次归档覆盖，
-- 这里只改约束。
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'key_daily_history'::regclass
           AND contype = 'p'
           AND pg_get_constraintdef(oid) = 'PRIMARY KEY (upstream_key_id, quota_day)'
    ) THEN
        -- 先清掉旧口径写下的行: 它们的 provider 与 token_limit 都不可信，
        -- 保留会让调度按错误水位打分，而错行又会挡住新口径的 upsert。
        DELETE FROM key_daily_history;
        ALTER TABLE key_daily_history DROP CONSTRAINT key_daily_history_pkey;
        ALTER TABLE key_daily_history
            ADD CONSTRAINT key_daily_history_pkey
            PRIMARY KEY (upstream_key_id, provider, quota_day);
    END IF;
END $$;

-- ============================================================
-- provider 配置迁库（配置热加载）
--
-- 两表分工:
--   provider_configs  一行一 provider，是当前生效态；
--   config_versions   一行一次变更的**全量快照**，是历史与回滚的依据。
--
-- 不存 diff: 回滚靠重放 diff，一旦某一环写错或字段改名，重放结果就是一份
-- 静默错掉的配置 —— 而配置错掉不报错、只让请求打到错的上游模型，是本项目
-- 最贵的失效类型。全量快照让「看历史」与「回滚」都退化为一次单行读取。
-- ============================================================

CREATE TABLE IF NOT EXISTS provider_configs (
    -- name 进 Redis 配额 key 前缀 {provider}:quota:{kind}:{key_id}:{day}，
    -- 同时是 usage_records / key_daily_history / upstream_keys 的归档维度。
    -- 改名等于把现有计数整体孤立、新名从 0 起算 —— 当日额度瞬间翻倍且不报错。
    -- 故为主键且物理禁改（API 层拒绝，见 admin_provider.go）。
    name               TEXT PRIMARY KEY,
    enabled            BOOLEAN     NOT NULL DEFAULT TRUE,
    base_url           TEXT        NOT NULL,
    -- quota_kind 决定 Redis key 的 {kind} 段与归档量纲，与 name 同级禁改。
    quota_kind         TEXT        NOT NULL,
    quota_limit        BIGINT      NOT NULL,
    -- 存纳秒整数而非 '24h' 文本。文本要在读取时解析，解析失败会静默退化为 0，
    -- 而配额周期为 0 意味着分桶键恒定、额度永不刷新 —— 全程没有任何报错。
    quota_window_nanos BIGINT      NOT NULL,
    -- 可空: 商汤无固定刷新点(NULL)、火山 12 点(12)。
    -- 不用 -1 之类哨兵值 —— 哨兵值总有一天会被某处当成合法小时数，
    -- 而 NULL 在反序列化时能与「0 点刷新」明确区分。
    refresh_hour       INT,
    model_mapping      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    count_models       TEXT[]      NOT NULL DEFAULT '{}',
    reasoning_models   TEXT[]      NOT NULL DEFAULT '{}',
    -- adapter 行为类型，与 provider 名解耦，供快照层构造 registry。
    -- 空串表示按 provider 名回落到既有装配逻辑。
    adapter_kind       TEXT        NOT NULL DEFAULT '',
    -- 只存环境变量名（如 SENSENOVA_API_KEY），进程读取时才解析。
    -- 本表内容会整份进 config_versions 历史、且历史对所有持看板口令的人可查，
    -- 凭据原文进来等于把上游密钥摊在运维随手能翻的表里，还会被
    -- audit_logs.detail 二次留存。密文仍走 upstream_keys + store.Cipher。
    credential_env     TEXT        NOT NULL DEFAULT '',
    -- 指向 config_versions.id，即当前生效的版本。
    version            BIGINT      NOT NULL DEFAULT 0,
    -- 软删除。物理删会让 usage_records / key_daily_history 里那批行变成
    -- 无法归因的孤儿数据，账目从此对不上。
    deleted_at         TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT provider_configs_quota_kind_chk   CHECK (quota_kind IN ('token', 'count')),
    CONSTRAINT provider_configs_quota_limit_chk  CHECK (quota_limit > 0),
    CONSTRAINT provider_configs_quota_window_chk CHECK (quota_window_nanos > 0),
    CONSTRAINT provider_configs_refresh_hour_chk
        CHECK (refresh_hour IS NULL OR (refresh_hour >= 0 AND refresh_hour <= 23)),
    CONSTRAINT provider_configs_name_chk CHECK (name ~ '^[a-z][a-z0-9_]{0,31}$')
);
-- 路由只看未删除的行。provider 数量是个位数，此索引主要用于表达意图。
CREATE INDEX IF NOT EXISTS idx_provider_configs_live
    ON provider_configs(name) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS config_versions (
    -- BIGSERIAL 是全局单调的，跨 provider 也不会重号。回滚生成的新版本号
    -- 继续递增，而不是把 provider_configs.version 改回旧值 —— 后者会让
    -- 版本链断裂，「当前生效的是哪个」与「哪些版本曾生效过」都读不出来。
    id               BIGSERIAL PRIMARY KEY,
    provider_name    TEXT        NOT NULL,
    action           TEXT        NOT NULL DEFAULT 'update',
    -- 纯展示冗余。写错只影响列表摘要，不影响回滚正确性 —— 这个不对称是刻意的。
    changed_fields   TEXT[]      NOT NULL DEFAULT '{}',
    snapshot         JSONB       NOT NULL,
    reason           TEXT        NOT NULL DEFAULT '',
    rolled_back_from BIGINT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by       TEXT        NOT NULL DEFAULT '',
    CONSTRAINT config_versions_action_chk
        CHECK (action IN ('seed', 'create', 'update', 'delete', 'rollback'))
);
-- 版本历史按 provider 倒序翻页，(provider_name, id DESC) 正好覆盖。
CREATE INDEX IF NOT EXISTS idx_config_versions_provider
    ON config_versions(provider_name, id DESC);
CREATE INDEX IF NOT EXISTS idx_config_versions_created
    ON config_versions(created_at DESC);
