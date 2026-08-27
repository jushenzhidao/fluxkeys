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

-- 火山 Key 池。密钥密文存储。
CREATE TABLE IF NOT EXISTS volc_keys (
    id            BIGSERIAL PRIMARY KEY,
    key_id        TEXT        NOT NULL UNIQUE,  -- 业务标识，如 volc_001
    secret_enc    TEXT        NOT NULL,         -- 加密后的密钥
    provider      TEXT        NOT NULL DEFAULT 'volc',
    pool          TEXT        NOT NULL DEFAULT 'cold',   -- hot / warm / cold
    status        TEXT        NOT NULL DEFAULT 'active', -- active / cooldown / banned / invalid
    persona_id    TEXT        NOT NULL DEFAULT '',
    egress_ip     TEXT        NOT NULL DEFAULT '',
    health_score  INTEGER     NOT NULL DEFAULT 100,
    -- 刷新探测状态（P0-4）
    refresh_state TEXT        NOT NULL DEFAULT 'idle',   -- idle / probing / confirmed / failed
    refresh_confirmed_at TIMESTAMPTZ,
    last_error    TEXT        NOT NULL DEFAULT '',
    last_used_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_volc_keys_pool_status ON volc_keys(pool, status);
CREATE INDEX IF NOT EXISTS idx_volc_keys_status ON volc_keys(status);

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
    volc_key_id       TEXT        NOT NULL DEFAULT '',
    egress_ip         TEXT        NOT NULL DEFAULT '',
    provider          TEXT        NOT NULL DEFAULT 'volc',
    model             TEXT        NOT NULL DEFAULT '',
    billing_kind      TEXT        NOT NULL DEFAULT 'token',  -- token / count
    -- 配额日而非自然日，与 12:00 刷新周期对齐（P0-3）
    quota_day         DATE        NOT NULL,
    prompt_tokens     BIGINT      NOT NULL DEFAULT 0,
    completion_tokens BIGINT      NOT NULL DEFAULT 0,
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
CREATE INDEX IF NOT EXISTS idx_usage_user_day  ON usage_records(user_id, quota_day);
CREATE INDEX IF NOT EXISTS idx_usage_key_day   ON usage_records(volc_key_id, quota_day);
CREATE INDEX IF NOT EXISTS idx_usage_created   ON usage_records(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_request   ON usage_records(request_id);

-- Key 每日归档，供跨日记忆打分使用（S_history）
CREATE TABLE IF NOT EXISTS key_daily_history (
    volc_key_id  TEXT   NOT NULL,
    quota_day    DATE   NOT NULL,
    token_used   BIGINT NOT NULL DEFAULT 0,
    count_used   INTEGER NOT NULL DEFAULT 0,
    token_limit  BIGINT NOT NULL DEFAULT 0,
    token_ratio  DOUBLE PRECISION NOT NULL DEFAULT 0,
    request_count INTEGER NOT NULL DEFAULT 0,
    error_count  INTEGER NOT NULL DEFAULT 0,
    consecutive_light_days INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (volc_key_id, quota_day)
);

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
    id          BIGSERIAL PRIMARY KEY,
    volc_key_id TEXT        NOT NULL,
    billing_kind TEXT       NOT NULL,
    quota_day   DATE        NOT NULL,
    drift       BIGINT      NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_drift_created ON quota_drift_logs(created_at DESC);
