-- FluxKeys 数据库初始化脚本
-- PostgreSQL 15+

-- ============================================================
-- 上游 Key 管理表
-- ============================================================

CREATE TABLE IF NOT EXISTS upstream_keys (
    id BIGSERIAL PRIMARY KEY,
    provider VARCHAR(50) NOT NULL,           -- provider 标识: volc, sensenova, qwen 等
    key_id VARCHAR(255) NOT NULL,            -- API Key（明文，用于匹配）
    secret_enc BYTEA,                        -- 加密后的完整 API Secret（AES-256-GCM）
    status VARCHAR(20) NOT NULL DEFAULT 'active',  -- active, banned, cooldown, suspect
    egress_ip VARCHAR(45),                   -- 绑定的出口 IP
    pool VARCHAR(20) DEFAULT '',             -- 档位: hot, warm, cold 或空串（通用）
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    
    CONSTRAINT uq_provider_key UNIQUE (provider, key_id)
);

CREATE INDEX IF NOT EXISTS idx_upstream_keys_provider_status 
    ON upstream_keys(provider, status);
CREATE INDEX IF NOT EXISTS idx_upstream_keys_egress_ip 
    ON upstream_keys(egress_ip) WHERE egress_ip IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_upstream_keys_secret 
    ON upstream_keys(secret_enc) WHERE secret_enc IS NOT NULL;

COMMENT ON TABLE upstream_keys IS '上游 API Key 管理表';
COMMENT ON COLUMN upstream_keys.provider IS 'Provider 标识';
COMMENT ON COLUMN upstream_keys.key_id IS 'API Key（明文标识，用于匹配和展示）';
COMMENT ON COLUMN upstream_keys.secret_enc IS '加密后的完整 API Secret（AES-256-GCM）';
COMMENT ON COLUMN upstream_keys.status IS 'Key 状态: active, banned, cooldown, suspect';
COMMENT ON COLUMN upstream_keys.egress_ip IS '绑定的出口 IP 地址';
COMMENT ON COLUMN upstream_keys.pool IS '档位标识: hot/warm/cold，空串表示通用';

-- ============================================================
-- 配额偏差记录表
-- ============================================================

CREATE TABLE IF NOT EXISTS quota_drift_logs (
    id BIGSERIAL PRIMARY KEY,
    provider VARCHAR(50) NOT NULL,
    upstream_key_id VARCHAR(255) NOT NULL,  -- 上游 Key 标识
    billing_kind VARCHAR(10) NOT NULL,      -- token 或 count
    quota_day DATE NOT NULL,                -- 配额日期
    drift BIGINT NOT NULL,                  -- 偏差量（正数表示 Redis 多计）
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_quota_drift_logs_provider_day 
    ON quota_drift_logs(provider, quota_day DESC);
CREATE INDEX IF NOT EXISTS idx_quota_drift_logs_key_day 
    ON quota_drift_logs(upstream_key_id, quota_day DESC);

COMMENT ON TABLE quota_drift_logs IS '配额偏差记录表，用于对账后的审计';
COMMENT ON COLUMN quota_drift_logs.drift IS '偏差量：正数表示 Redis 多计，负数表示 Redis 少计';

-- ============================================================
-- 出口 IP 封禁记录表
-- ============================================================

CREATE TABLE IF NOT EXISTS egress_ban_events (
    id BIGSERIAL PRIMARY KEY,
    provider VARCHAR(50) NOT NULL,
    egress_ip VARCHAR(45) NOT NULL,
    key_id VARCHAR(255) NOT NULL,
    ban_reason TEXT,                        -- 封禁原因
    banned_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    recovered_at TIMESTAMPTZ,               -- 恢复时间（NULL 表示尚未恢复）
    
    CONSTRAINT chk_ban_recovery CHECK (recovered_at IS NULL OR recovered_at >= banned_at)
);

CREATE INDEX IF NOT EXISTS idx_egress_ban_provider_ip 
    ON egress_ban_events(provider, egress_ip, banned_at DESC);
CREATE INDEX IF NOT EXISTS idx_egress_ban_key 
    ON egress_ban_events(key_id, banned_at DESC);

COMMENT ON TABLE egress_ban_events IS '出口 IP 封禁事件记录';
COMMENT ON COLUMN egress_ban_events.ban_reason IS '封禁原因描述';
COMMENT ON COLUMN egress_ban_events.recovered_at IS '恢复时间，NULL 表示尚未恢复';

-- ============================================================
-- 请求日志表（可选，用于审计）
-- ============================================================

CREATE TABLE IF NOT EXISTS request_logs (
    id BIGSERIAL PRIMARY KEY,
    provider VARCHAR(50) NOT NULL,
    key_id VARCHAR(255) NOT NULL,
    egress_ip VARCHAR(45),
    model VARCHAR(100) NOT NULL,
    status_code INTEGER NOT NULL,
    tokens_used INTEGER,
    latency_ms INTEGER,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 分区表索引（按日期分区，提升查询性能）
CREATE INDEX IF NOT EXISTS idx_request_logs_provider_time 
    ON request_logs(provider, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_key_time 
    ON request_logs(key_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_status 
    ON request_logs(status_code) WHERE status_code >= 400;

COMMENT ON TABLE request_logs IS '请求日志表（可选）';
COMMENT ON COLUMN request_logs.tokens_used IS '本次请求消耗的 token 数';
COMMENT ON COLUMN request_logs.latency_ms IS '请求延迟（毫秒）';

-- ============================================================
-- 配额快照表（用于历史趋势分析）
-- ============================================================

CREATE TABLE IF NOT EXISTS quota_snapshots (
    id BIGSERIAL PRIMARY KEY,
    provider VARCHAR(50) NOT NULL,
    key_id VARCHAR(255) NOT NULL,
    quota_day DATE NOT NULL,
    billing_kind VARCHAR(10) NOT NULL,
    token_used BIGINT NOT NULL DEFAULT 0,
    token_hard BIGINT NOT NULL DEFAULT 0,
    snapshot_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    
    CONSTRAINT uq_quota_snapshot UNIQUE (provider, key_id, quota_day, billing_kind, snapshot_at)
);

CREATE INDEX IF NOT EXISTS idx_quota_snapshots_provider_day 
    ON quota_snapshots(provider, quota_day DESC, snapshot_at DESC);

COMMENT ON TABLE quota_snapshots IS '配额快照表，定期记录配额状态用于趋势分析';

-- ============================================================
-- 初始化数据（可选）
-- ============================================================

-- 插入一些示例 provider 配置（实际配置在 config.yml 中）
-- 这里仅用于验证表结构

-- ============================================================
-- 版本信息
-- ============================================================

CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    description TEXT
);

INSERT INTO schema_version (version, description) 
VALUES (1, 'Initial schema for multi-provider architecture')
ON CONFLICT (version) DO NOTHING;

COMMENT ON TABLE schema_version IS '数据库 schema 版本管理';

-- ============================================================
-- 数据库性能优化
-- ============================================================

-- 启用自动 VACUUM
ALTER TABLE upstream_keys SET (autovacuum_enabled = true);
ALTER TABLE quota_drifts SET (autovacuum_enabled = true);
ALTER TABLE request_logs SET (autovacuum_enabled = true);

-- 设置统计信息收集
ALTER TABLE upstream_keys SET (autovacuum_analyze_scale_factor = 0.05);
ALTER TABLE request_logs SET (autovacuum_analyze_scale_factor = 0.02);

-- ============================================================
-- 完成
-- ============================================================

SELECT 'FluxKeys 数据库初始化完成！' AS status;
