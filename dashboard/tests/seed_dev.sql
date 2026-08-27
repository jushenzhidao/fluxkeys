-- 开发/验证用示例数据。仅供本地手工验证看板，不参与自动化测试。
--
-- 关键验证点：配额日边界。下面刻意插入两条跨 12:00 的流水，
-- 它们的 created_at 分属两个自然日，但 quota_day 相同 —— 看板必须按
-- quota_day 聚合，否则会把同一份额度拆成两天。

TRUNCATE usage_records, quota_drift_logs, user_api_keys, users, volc_keys, egress_ips RESTART IDENTITY CASCADE;

INSERT INTO users (name, email, status, daily_token_limit, rpm_limit, tpm_limit) VALUES
  ('张三', 'zhangsan@example.com', 'active',  1000000, 60, 100000),
  ('李四', 'lisi@example.com',     'active',        0, 30,  50000),
  ('王五', 'wangwu@example.com',   'suspended', 500000, 10,  20000);

INSERT INTO user_api_keys (user_id, key_hash, key_prefix, name) VALUES
  (1, 'hash_a', 'sk-aaaa', '生产'),
  (2, 'hash_b', 'sk-bbbb', '测试'),
  (3, 'hash_c', 'sk-cccc', '默认');

INSERT INTO egress_ips (addr, public_ip, region, isp, max_keys, state, reputation) VALUES
  ('172.16.0.2', '1.2.3.4', 'cn-beijing', '电信', 10, 'active',   100),
  ('172.16.0.3', '1.2.3.5', 'cn-beijing', '联通', 10, 'active',    85),
  ('172.16.0.4', '1.2.3.6', 'cn-shanghai','移动',  5, 'degraded',  40);

INSERT INTO volc_keys
  (key_id, secret_enc, pool, status, persona_id, egress_ip, health_score, refresh_state, refresh_confirmed_at, last_error, last_used_at)
VALUES
  -- 水位极高，应触发 near_hard 告警
  ('volc_001', 'enc', 'hot',  'active',   'p_morning', '172.16.0.2', 98,  'confirmed', now(), '',                    now()),
  -- 与 volc_001 行为高度相似且同出口 IP，应触发 critical 相似度告警
  ('volc_002', 'enc', 'hot',  'active',   'p_morning', '172.16.0.2', 95,  'confirmed', now(), '',                    now()),
  -- 刷新未确认却有用量，应出现在 risky_keys
  ('volc_003', 'enc', 'warm', 'active',   'p_night',   '172.16.0.3', 80,  'probing',   NULL,  'quota exhausted',     now()),
  ('volc_004', 'enc', 'cold', 'active',   'p_random',  '172.16.0.3', 100, 'idle',      NULL,  '',                    NULL),
  ('volc_005', 'enc', 'cold', 'cooldown', 'p_random',  '172.16.0.4', 45,  'failed',    NULL,  'rate limited',        now()),
  ('volc_006', 'enc', 'cold', 'banned',   'p_random',  '',            0,  'idle',      NULL,  'account suspended',   now());

-- 跨 12:00 边界的两条流水：自然日不同，配额日相同
INSERT INTO usage_records
  (request_id, user_id, user_api_key_id, volc_key_id, egress_ip, model, billing_kind,
   quota_day, prompt_tokens, completion_tokens, total_tokens, estimated_tokens,
   status_code, latency_ms, created_at)
VALUES
  ('req_boundary_a', 1, 1, 'volc_001', '172.16.0.2', 'deepseek-v3', 'token',
   CURRENT_DATE, 1000, 2000, 3000, 3600, 200, 700,
   CURRENT_DATE + TIME '23:30'),
  ('req_boundary_b', 1, 1, 'volc_001', '172.16.0.2', 'deepseek-v3', 'token',
   CURRENT_DATE, 1000, 2000, 3000, 3600, 200, 700,
   CURRENT_DATE + INTERVAL '1 day' + TIME '11:30');

-- volc_001 / volc_002 时段与模型分布刻意做成一致，用于验证相似度告警
INSERT INTO usage_records
  (request_id, user_id, user_api_key_id, volc_key_id, egress_ip, model, billing_kind,
   quota_day, prompt_tokens, completion_tokens, total_tokens, estimated_tokens,
   status_code, latency_ms, created_at)
SELECT
  'req_sim_' || k.key_id || '_' || g,
  1, 1, k.key_id, '172.16.0.2', 'deepseek-v3', 'token',
  CURRENT_DATE, 500, 1500, 2000, 2400, 200, 650,
  CURRENT_DATE + TIME '14:00' + (g || ' minutes')::interval
FROM (VALUES ('volc_001'), ('volc_002')) AS k(key_id),
     generate_series(1, 30) AS g;

-- volc_003 夜间集中请求 + 部分错误
INSERT INTO usage_records
  (request_id, user_id, user_api_key_id, volc_key_id, egress_ip, model, billing_kind,
   quota_day, prompt_tokens, completion_tokens, total_tokens, estimated_tokens,
   status_code, error_code, latency_ms, created_at)
SELECT
  'req_night_' || g,
  2, 2, 'volc_003', '172.16.0.3', 'doubao-pro', 'token',
  CURRENT_DATE, 300, 700, 1000, 1200,
  CASE WHEN g % 5 = 0 THEN 429 ELSE 200 END,
  CASE WHEN g % 5 = 0 THEN 'rate_limit_exceeded' ELSE '' END,
  900, CURRENT_DATE + TIME '22:00' + (g || ' minutes')::interval
FROM generate_series(1, 25) AS g;

-- 未归属流量（user_id 为空），验证计费口径不丢数据
INSERT INTO usage_records
  (request_id, volc_key_id, egress_ip, model, billing_kind, quota_day,
   prompt_tokens, completion_tokens, total_tokens, status_code, latency_ms, created_at)
VALUES
  ('req_anon_1', 'volc_004', '172.16.0.3', 'deepseek-v3', 'token', CURRENT_DATE,
   100, 200, 300, 200, 500, CURRENT_DATE + TIME '15:00'),
  ('req_anon_2', 'volc_004', '172.16.0.3', 'deepseek-v3', 'token', CURRENT_DATE,
   100, 200, 300, 500, 500, CURRENT_DATE + TIME '15:05');

-- 按次计费（Seedream）
INSERT INTO usage_records
  (request_id, user_id, volc_key_id, egress_ip, model, billing_kind, quota_day,
   count_units, status_code, latency_ms, created_at)
VALUES
  ('req_count_1', 3, 'volc_005', '172.16.0.4', 'seedream-3.0', 'count', CURRENT_DATE,
   1, 200, 3000, CURRENT_DATE + TIME '16:00');

-- 历史配额日，用于验证趋势曲线
INSERT INTO usage_records
  (request_id, user_id, volc_key_id, egress_ip, model, billing_kind, quota_day,
   prompt_tokens, completion_tokens, total_tokens, status_code, latency_ms, created_at)
SELECT
  'req_hist_' || d || '_' || g,
  1, 'volc_001', '172.16.0.2', 'deepseek-v3', 'token',
  CURRENT_DATE - d,
  400, 600, 1000, 200, 600,
  CURRENT_DATE - d + TIME '15:00' + (g || ' minutes')::interval
FROM generate_series(1, 6) AS d, generate_series(1, 10) AS g;

-- 对账偏差记录
INSERT INTO quota_drift_logs (volc_key_id, billing_kind, quota_day, drift) VALUES
  ('volc_003', 'token', CURRENT_DATE, 12000),
  ('volc_005', 'token', CURRENT_DATE - 1, -3500);
