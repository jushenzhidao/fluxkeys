# 多上游改造设计

## 1. 问题定位

当前架构在以下五处**硬编码火山引擎**，无法支持其他上游：

| 层级 | 硬编码点 | 影响范围 |
|------|---------|---------|
| **Config** | `Upstream.VolcBaseURL` 单一 base URL | 只能配一个上游 |
| **Adapter** | Server.adapter 单例 | 请求转发无法按渠道分叉 |
| **Quota** | `RefreshHour=12` 包级常量 + Redis key `volc:quota:*` | 刷新逻辑与 key 命名与火山绑死 |
| **Scheduler** | 无 provider 维度，Select 不过滤渠道 | 会把商汤 key 调度给火山请求 |
| **Store** | 表名 `volc_keys`、字段 `provider` 虽存在但未用 | 查询无渠道过滤 |

## 2. 核心矛盾

**火山 vs 商汤的配额规则根本不同**：

| 维度 | 火山引擎 | 商汤 SenseNova |
|------|---------|--------------|
| 计量单位 | Token（500万/日） | 调用次数（500次） |
| 刷新周期 | 每日 12:00 固定 | 5小时滑动窗口 |
| 配额探测 | 需主动探测确认刷新 | 无固定刷新点，429 后等待窗口滚动 |
| Redis key | `volc:quota:token:key_001:20260831` | `sensenova:quota:count:key_001:<window_id>` |

**quota 层必须参数化，而非仅多态**。

## 3. 改造方案

### 3.1 Config 层：providers 配置段

```yaml
# 新增 providers 配置段，替代 Upstream.VolcBaseURL
providers:
  volc:
    base_url: "https://ark.cn-beijing.volces.com"
    quota_kind: token
    quota_limit: 5000000
    quota_window: 24h
    refresh_hour: 12     # 固定刷新点（火山专有）
    model_mapping:
      deepseek-v3: deepseek-v3-241226
    count_models: []
    reasoning_models: [deepseek-v4-flash]
  
  sensenova:
    base_url: "https://token.sensenova.cn/v1"
    quota_kind: count
    quota_limit: 500
    quota_window: 5h     # 滑动窗口（商汤专有）
    refresh_hour: null   # 无固定刷新点
    model_mapping:
      deepseek-v4: deepseek-v4-flash
    count_models: [deepseek-v4-flash]
    reasoning_models: [deepseek-v4-flash]

# Upstream 段简化为通用重试配置
upstream:
  max_retries: 3
  retry_base_delay: 200ms
  retry_jitter: 100ms
```

**配置字段说明**：

- `quota_kind`: `token` | `count`，决定预扣单位
- `quota_limit`: 配额上限（token 数或次数）
- `quota_window`: 配额周期，`24h` 或 `5h`
- `refresh_hour`: 固定刷新点（火山 12 点），商汤填 `null`
- `model_mapping`: 对外名 → 上游名
- `count_models`: 按次计费模型列表
- `reasoning_models`: 推理模型列表

### 3.2 Adapter 层：Registry + SenseNova 实现

**adapter/registry.go**（新建）：

```go
type Registry struct {
    adapters map[Provider]Adapter
    mu       sync.RWMutex
}

func NewRegistry() *Registry {
    return &Registry{adapters: make(map[Provider]Adapter)}
}

func (r *Registry) Register(p Provider, a Adapter) {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.adapters[p] = a
}

func (r *Registry) Get(p Provider) (Adapter, bool) {
    r.mu.RLock()
    defer r.mu.RUnlock()
    a, ok := r.adapters[p]
    return a, ok
}
```

**adapter/sensenova.go**（新建）：

```go
type SenseNova struct {
    forward map[string]string
    reverse map[string]string
}

func NewSenseNova(mapping map[string]string) *SenseNova {
    // 与 Volc 结构相同，复用逻辑
}

func (s *SenseNova) Provider() Provider { return Provider("sensenova") }

func (s *SenseNova) MapError(statusCode int, body []byte) *UpstreamError {
    // 商汤错误格式：error.code 是数字 16，无 error.type
    // 401: code=16 message="Forbidden" → ErrClassAuth
    // 429: 需实测确认是否有额度耗尽与限流的区分标记
}
```

**gateway/server.go** 改造：

```go
// 旧：单例 adapter
- adapter adapter.Adapter

// 新：注册表
+ registry *adapter.Registry
```

**proxy.go** 改造：

```go
// 旧：s.adapter.TransformRequest(...)
// 新：
ad, ok := s.registry.Get(cand.Provider)
if !ok {
    return attemptResult{...}
}
upstreamPath, upstreamBody, terr := ad.TransformRequest(plan.Endpoint, plan.Body)
```

### 3.3 Quota 层：provider 参数化

**quota/manager.go** 改造：

```go
// 旧：硬编码前缀
- func quotaKey(kind Kind, keyID, day string) string {
-     return fmt.Sprintf("volc:quota:%s:%s:%s", kind, keyID, day)
- }

// 新：provider 参数
+ func quotaKey(provider, kind, keyID, day string) string {
+     return fmt.Sprintf("%s:quota:%s:%s:%s", provider, kind, keyID, day)
+ }

// 所有调用处增加 provider 参数
func (m *Manager) Acquire(ctx context.Context, provider string, keyID string, kind Kind, ...) (Decision, *Lease, error)
func (m *Manager) Get(ctx context.Context, provider string, keyID string, kind Kind) (Snapshot, error)
```

**quota/quotaday.go** 改造：

```go
// 旧：包级常量
- const RefreshHour = 12

// 新：按 provider 配置查询
+ type RefreshPolicy struct {
+     RefreshHour *int          // nil 表示无固定刷新点
+     Window      time.Duration // 5h 或 24h
+ }
+
+ func QuotaDay(t time.Time, policy RefreshPolicy) string {
+     if policy.RefreshHour != nil && t.Hour() < *policy.RefreshHour {
+         t = t.AddDate(0, 0, -1)
+     }
+     return t.Format("20060102")
+ }
```

**商汤滑动窗口实现**：

```go
// sensenova 专用：5小时窗口 ID
func SenseNovaWindowID(t time.Time) string {
    // 将时间戳按 5 小时分桶：0-5h, 5-10h, ...
    // 窗口 ID = floor(unix_timestamp / 18000) * 18000
    bucket := t.Unix() / 18000 * 18000
    return fmt.Sprintf("%d", bucket)
}
```

### 3.4 Scheduler 层：按 provider 过滤

**scheduler/scheduler.go** 改造：

```go
type Request struct {
    Model    string
    Kind     quota.Kind
+   Provider string  // 新增：调度时按渠道过滤
    Now      time.Time
    Exclude  map[string]bool
}

func (s *Scheduler) Select(ctx context.Context, req Request) (*Candidate, error) {
    // 过滤时增加 provider 判断
    for _, e := range keys {
+       if e.provider != req.Provider {
+           reasons.wrongProvider++
+           continue
+       }
        // ... 其余过滤条件
    }
}
```

**keyEntry** 增加 provider 字段：

```go
type keyEntry struct {
    keyID     string
    secret    string
+   provider  string  // 从 DB 读取
    egressIP  string
    pool      string
    // ...
}
```

### 3.5 Store 层：表重命名 + provider 索引

**schema.sql** 改造（**破坏性变更，不兼容旧版**）：

```sql
-- 旧表名 volc_keys 改为 upstream_keys
DROP TABLE IF EXISTS volc_keys CASCADE;
DROP TABLE IF EXISTS key_daily_history CASCADE;
DROP TABLE IF EXISTS usage_records CASCADE;

CREATE TABLE IF NOT EXISTS upstream_keys (
    id            BIGSERIAL PRIMARY KEY,
    key_id        TEXT        NOT NULL UNIQUE,
    secret_enc    TEXT        NOT NULL,
    provider      TEXT        NOT NULL,  -- 'volc' | 'sensenova'
    pool          TEXT        NOT NULL DEFAULT 'cold',
    status        TEXT        NOT NULL DEFAULT 'active',
    persona_id    TEXT        NOT NULL DEFAULT '',
    egress_ip     TEXT        NOT NULL DEFAULT '',
    health_score  INTEGER     NOT NULL DEFAULT 100,
    refresh_state TEXT        NOT NULL DEFAULT 'idle',
    refresh_confirmed_at TIMESTAMPTZ,
    last_error    TEXT        NOT NULL DEFAULT '',
    last_used_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_upstream_keys_provider_status ON upstream_keys(provider, status);
CREATE INDEX idx_upstream_keys_pool_status ON upstream_keys(pool, status);

-- usage_records 的 volc_key_id 改为 upstream_key_id
CREATE TABLE IF NOT EXISTS usage_records (
    -- ...
    upstream_key_id TEXT NOT NULL DEFAULT '',
    provider        TEXT NOT NULL DEFAULT '',
    -- ...
);
CREATE INDEX idx_usage_provider_day ON usage_records(provider, quota_day);
CREATE INDEX idx_usage_key_day ON usage_records(upstream_key_id, quota_day);

-- key_daily_history 同理
CREATE TABLE IF NOT EXISTS key_daily_history (
    upstream_key_id TEXT NOT NULL,
    provider        TEXT NOT NULL,
    quota_day       DATE NOT NULL,
    -- ...
    PRIMARY KEY (upstream_key_id, quota_day)
);
```

**store/models.go** 改造：

```go
// 旧
- type VolcKey struct { ... }
- func (s *Store) ListVolcKeys(...) ([]VolcKey, error)

// 新
+ type UpstreamKey struct {
+     KeyID    string
+     Provider string
+     Secret   string
+     // ...
+ }
+ func (s *Store) ListUpstreamKeys(ctx, filter UpstreamKeyFilter) ([]UpstreamKey, error)
+
+ type UpstreamKeyFilter struct {
+     Provider string  // 按渠道过滤
+     Status   string
+     Limit    int
+ }
```

## 4. 迁移策略

**无兼容，直接重建**：

1. 备份旧表：`pg_dump fluxkeys > backup.sql`
2. 删除旧表：`DROP TABLE volc_keys, usage_records, key_daily_history CASCADE;`
3. 应用新 schema：`psql < internal/store/schema.sql`
4. 手动导入 key：
   ```sql
   INSERT INTO upstream_keys (key_id, secret_enc, provider, pool, egress_ip)
   VALUES ('volc_001', '<encrypted>', 'volc', 'hot', '154.40.45.34');
   ```

## 5. 测试计划

### 5.1 单元测试
- adapter 层：商汤错误分类（401/429）
- quota 层：滑动窗口 ID 生成、跨 provider key 隔离
- scheduler 层：按 provider 过滤正确性

### 5.2 集成测试
- 火山 + 商汤双上游并存
- 调度器不会把商汤 key 给火山请求
- 配额独立计数（volc:quota:* 与 sensenova:quota:* 不串）

### 5.3 真机测试
- 154.40.45.34 部署双上游
- 10 个商汤 key + 双 IP 出口
- 压测 5h 窗口的滚动刷新行为

## 6. 风险与兜底

| 风险 | 影响 | 兜底方案 |
|------|------|---------|
| 商汤 429 无额度耗尽标记 | 无法区分限流与配额 | 统一当限流处理，冷却后重试 |
| 5h 窗口边界并发 | 窗口切换时短暂超刷 | 预扣时额外保留 10% buffer |
| provider 拼写错误 | 调度器找不到 key | 启动时校验所有 key 的 provider 在配置中存在 |

## 7. 实施顺序

1. ✅ **调研 SenseNova**（已完成）
2. **Config 层**：providers 配置段 + 解析
3. **Adapter 层**：Registry + SenseNova 实现
4. **Quota 层**：provider 参数化 + 滑动窗口
5. **Scheduler 层**：provider 过滤
6. **Store 层**：表重命名 + 迁移脚本
7. **Gateway 层**：路由改造（Select 传 provider）
8. **测试 + 部署**

---

**设计原则**：

- 不保留任何火山专有概念在通用层
- 每个 provider 的特殊逻辑封装在自己的 adapter 与配置内
- quota 层只提供参数化接口，刷新策略由 provider 配置驱动
