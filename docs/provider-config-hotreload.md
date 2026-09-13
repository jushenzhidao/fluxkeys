# Provider 配置热加载、版本化与变更审计 —— 架构设计

> 版本锚定：Go 1.23.0（`go.mod:3`，`toolchain go1.23.4`）。模块路径 `github.com/fluxkeys/fluxkeys`。
> PostgreSQL 走 `pgxpool`，schema 经 `//go:embed schema.sql`（`internal/store/store.go:26`）+ 启动 `auto_migrate`（`store.go:99`）。
> 新增 admin 路由使用 Go 1.22+ method-pattern 写法，与 `internal/gateway/server.go:263` 现有 `"PATCH /admin/keys/{key_id}"` 保持一致。
>
> **本文档只做设计。**DDL 与接口签名可直接取用，但不含 `.go` 实现代码。施工单见第 7 节。
> 配套界面设计见 `docs/provider-config-ui.md`（designer 产出），本文档的 API 契约与其 §4.4 两步提交、§7 版本历史、§8 `expected_version` 冲突约定对齐。

---

## 本期交付的能力边界（给批准这一期的人先读）

这一节不谈技术，只用来对齐"provider 增删改"这个功能实际交付什么、不交付什么。技术依据见 §3.2.1。

**本期能做的 —— 都在页面上完成，不需要改代码，保存后立即生效，不用重启网关**

- 改上游地址 `base_url`：厂商换域名、换区域节点、临时切到备用入口，改完下一个请求就走新地址。
- 改模型名映射 `model_mapping`：对外暴露的模型名与上游真实模型名的对应关系，可以新增一条、改一条、删一条。
- 改配额上限 `quota_limit`：调大调小当天额度，正在跑的配额刷新任务会就地改用新额度，不用等到第二天。
- 改计费口径相关的模型清单 `count_models` / `reasoning_models`：哪些模型按次计费、哪些模型带思维链。
- 启用 / 停用某个 provider：停用后数据面立刻不再往它发请求，但它仍留在管理列表里，随时可以再启用。
- 回滚到任意历史版本：每次保存都留一份全量快照，选一个版本就能整体回到那个时刻的配置。
- 全程审计：谁、什么时候、把哪个字段从什么改成了什么，都留在变更记录里，可按 provider 和时间查。

**本期不能做的**

**在页面上接入一个全新的厂商。** 分两种情况：要配的厂商如果网关代码里已经实现过（当前是火山 volc、商汤 sensenova 这两家），新增、改配置、启停都没问题；如果要接的是代码里还没有对接实现的第三方厂商，必须先由开发写完这家厂商的对接代码、发一次版本，之后才能在页面上配置它。这种情况下页面会直接拒绝保存，并明确说明"这个厂商还没有对接实现"，同时列出当前支持哪几家 —— 不让运维反复试。

**为什么不能 —— 这不是少做了一块，是配置表达不了**

配置能表达的只有"名字对应关系"这一层，也就是把我们对外的模型名换成厂商那边的模型名。但各家厂商真正的差异不在名字上：

- **请求怎么发**：请求体的字段名和嵌套结构各家不同，同一个参数放的位置就可能不一样。
- **怎么证明身份**：鉴权信息放哪个请求头、用什么格式、要不要签名，各家一套。
- **出错怎么读**：厂商返回的错误码和错误结构各不相同，要先翻译成统一的错误语义，上层才能判断该重试还是该换 Key。
- **流式回包怎么拆**：流式响应的分帧格式有差异，思维链内容（`reasoning_content`）在返回结构里的位置也不统一。

这四类差异是逐厂商写在代码里的判断与转换逻辑，不是几个配置项能描述的形状。要让它们变成"可配置"，等于在网关里再造一套描述语言，成本高于每接一家厂商写一次对接代码，出问题时也更难查。所以本期的定位是：**已对接厂商的配置完全自助化，新厂商接入仍走一次代码发布。**

---

## 0. 现状核对结论与对 team-lead 数据的偏差

设计前逐条验证了交接数据。**准确项**：148 处 `cfg.` 的 11 文件分布完全吻合；`internal/config` 零并发保护（无 `sync`/`atomic` 引入）；`audit_logs` 结构够用；admin 路由无任何 config 端点；dashboard 对 provider 配置完全只读；3 个 `*config.Config` 长生命周期持有点（`server.go:52`、`cmd/gateway/adapters.go:44`、`cmd/gateway/background.go:32`）存在。

**偏差 5 项，全部纳入本设计：**

| # | 交接数据 | 实际情况 | 影响 |
|---|---|---|---|
| D1 | `server.go:439` 为热区（疑为 /healthz 或路由注册） | 实际在 `Start()` 内，仅拼启动日志的 provider 名列表 | **冷区，不改造**。误改会把日志逻辑拖进快照路径 |
| D2 | 11 处热区清单 | 遗漏 `background.go:192` `for provider := range b.cfg.Providers`（`reapLeases`） | **真热区**。新增 provider 后不遍历它，其过期租约永不回收 —— 正是 P0-2 要防的额度泄漏，且全程静默 |
| D3 | 11 处热区清单 | 遗漏 `background.go:485` `for provider := range b.cfg.Providers`（按 provider 建 `Refresher`） | **真热区，但换快照解决不了**：`Refresher` 是长生命周期后台任务且持 `RefresherConfig` 值拷贝（`internal/quota/refresh.go:62-78`），新增要起、停用要停、改额度要就地更新，需生命周期编排（见 §2.5） |
| D4 | — | **adapter registry 完全在快照体系外** | 最严重。见下 |
| D5 | — | `internal/scheduler/scheduler.go:240` 存在与阶段一缺陷 #11 同类的量纲错位 | 既存缺陷，本次必须一并修，否则热加载 `quota_limit` 对调度打分无效 |

### D4 详述：adapter registry 是本次最大的缺口

`adapter.NewVolc(p.ModelMapping)`（`internal/adapter/volc.go:26`）在构造时把 mapping 展开成 `forward`/`reverse` 两张 map 并**固化**。`UpstreamModel`(volc.go:67)、`PublicModel`(volc.go:74)、`TransformRequest`(volc.go:83) 读的都是这两张固化 map。

后果：热加载改了 `model_mapping`，`config.UpstreamModel`(config.go:1072) 用新映射，而 adapter 的 `TransformRequest` 仍用旧映射改写请求体 model 字段 —— **这恰好就是用户决策 3 要根除的"新 base_url + 旧 model_mapping"混合态**，且不报错，只是把请求发给一个错的上游模型名。

装配点有两处，都硬编码 `switch name`，`default` 直接 `return fmt.Errorf("未知的 provider: %s", name)`：`cmd/gateway/main.go:261-273` 与 `internal/gateway/server.go:192-206`。含义：**通过 admin API 新增一个未在 switch 里登记的 provider 名，会让热加载失败、重启后启动也失败。**`Registry.Register`(registry.go:27) 在名字不匹配或 adapter 为 nil 时 **panic**，热路径绝不能直接复用；`registry.go:10` 注释也明说"不支持运行时动态注册"。

**已验证的关键事实：`internal/adapter` 不 import `internal/config`**（grep 零命中）。因此可以在上层把 adapter 实例与 `*config.Config` 组合进同一个快照对象，不产生循环依赖 —— 这是 §2 方案成立的前提。

### D5 详述：scheduler 的水位量纲错位

`scheduler.go:240` `hard, soft := s.quotaC.TokenHard(), s.quotaC.TokenSoft()` 在 `Reload` 里给**每一个 Key** 都套**全局 token 水位**，完全没走 `LimitsFor(provider, kindCount)`。对按次计费的 sensenova（`quota_limit: 1400`）会用 5000000×0.9 的 token 水位打分，量纲整体错位。该值经 `keyEntry.hardLimit/softLimit`(scheduler.go:104-105) 流向 `snap.Hard/snap.Soft`（scheduler.go:427、754）参与配额打分。

另外 `Scheduler` 持有的是 `config.Scheduler` / `config.Quota` **值拷贝**（scheduler.go:150-151），不在 3 个持有点内，热加载同样吃不到 —— 意味着改 `quota_limit` 对调度打分零效果。

### 另一处隐藏持有点

`newProbe`(main.go:489) 闭包捕获 `cfg`，是**第 4 个长生命周期持有点**。其中 `for _, p := range cfg.Providers { adapterMapping = p.ModelMapping; break }`(main.go:493) 取"第一个 provider"的 mapping 且硬编码 `adapter.NewVolc`，带 TODO 注释，多 provider 下本就是错的。

---

## 1. PG 表 schema（含版本化）

### 1.1 三个设计问题的取舍

**问题一：一行一 provider，还是一行一快照？**

结论：**两张表分工 —— `provider_configs` 一行一 provider（当前态），`config_versions` 一行一全局快照（历史）。**

单表做不到两件事同时成立：热加载需要"一次读出全部 provider 的一致视图"，回滚需要"某一时刻的全局一致视图"。若只用一行一 provider + 每行自己的版本号，回滚就退化成逐 provider 回滚，跨 provider 的关联改动（如把 default_provider 从 volc 切到 sensenova 同时调 sensenova 上限）无法原子回退，且"当前生效版本号"没有单一定义 —— 而 designer 的 §7.1 表格明确要求列出全局的「版本 / 生效中」。

**问题二：存全量快照还是存 diff？**

结论：**存全量快照（JSONB），额外冗余一个 `changed_fields text[]` 供列表展示。**

理由三条：① provider 数量是个位数，单个快照 JSON 约 2-6 KB，一天改 10 次一年也才 20 MB 级，存储成本不构成理由；② diff 链的回滚必须从基线重放，任何一环写错或迁移期字段改名，重放结果就是**静默错的配置** —— 这正是本项目最贵的失效类型；③ 全量快照让"查看历史版本"和"回滚"都退化为一次单行读取，无重放逻辑即无重放 bug。

`changed_fields` 是纯展示冗余，即使它写错也只影响列表摘要，不影响回滚正确性 —— 这个不对称是刻意的。

**问题三：怎么回滚？**

结论：**回滚 = 以历史版本的 `snapshot` 内容创建一个新版本**，不删除、不修改任何中间版本，`action='rollback'` + `rolled_back_from` 记来源。与 designer §7 文案（"回滚会以 #124 的配置内容创建一个新版本 #129"）一致。

不做"把 current 指针挪回去"：那样版本历史会出现空洞，且"当前生效的是 #124"与"#125-#128 曾经生效过"两个事实无法从单表读出。回滚本身留痕，也让回滚可再回滚。

### 1.2 DDL（可直接追加到 `internal/store/schema.sql` 末尾的"增量迁移"分节）

全部语句幂等，遵循 `schema.sql:148-153` 的约定。**不使用 `ALTER TABLE` 改动任何现有表** —— 本期只新增两张表，`audit_logs` 原样复用。

```sql
-- ============================================================
-- provider 配置真相来源（一行一 provider，当前态）
-- 说明：provider 名是 Redis 配额 key 前缀与归档维度，故为主键且禁改。
-- ============================================================
CREATE TABLE IF NOT EXISTS provider_configs (
    name              TEXT PRIMARY KEY,
    enabled           BOOLEAN     NOT NULL DEFAULT TRUE,
    base_url          TEXT        NOT NULL,
    quota_kind        TEXT        NOT NULL,
    quota_limit       BIGINT      NOT NULL,
    quota_window      TEXT        NOT NULL DEFAULT '',
    refresh_hour      SMALLINT,
    model_mapping     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    count_models      JSONB       NOT NULL DEFAULT '[]'::jsonb,
    reasoning_models  JSONB       NOT NULL DEFAULT '[]'::jsonb,
    credential_env    TEXT        NOT NULL DEFAULT '',
    version           BIGINT      NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT provider_configs_quota_kind_chk CHECK (quota_kind IN ('token', 'count')),
    CONSTRAINT provider_configs_quota_limit_chk CHECK (quota_limit > 0),
    CONSTRAINT provider_configs_refresh_hour_chk
        CHECK (refresh_hour IS NULL OR (refresh_hour >= 0 AND refresh_hour <= 23)),
    CONSTRAINT provider_configs_name_chk CHECK (name ~ '^[a-z][a-z0-9_]{0,31}$')
);

-- 只有 enabled 的 provider 参与路由；数量个位数，此索引主要用于表达意图
CREATE INDEX IF NOT EXISTS idx_provider_configs_enabled
    ON provider_configs(enabled) WHERE enabled;

-- ============================================================
-- 配置版本历史（一行一全局快照）
-- snapshot 存"该版本生效时全部 provider 的完整配置"，回滚即取此列重放为新版本。
-- ============================================================
CREATE TABLE IF NOT EXISTS config_versions (
    version           BIGSERIAL PRIMARY KEY,
    is_current        BOOLEAN     NOT NULL DEFAULT FALSE,
    action            TEXT        NOT NULL,
    target_provider   TEXT        NOT NULL DEFAULT '',
    changed_fields    TEXT[]      NOT NULL DEFAULT '{}',
    snapshot          JSONB       NOT NULL,
    reason            TEXT        NOT NULL DEFAULT '',
    actor             TEXT        NOT NULL DEFAULT '',
    rolled_back_from  BIGINT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT config_versions_action_chk
        CHECK (action IN ('seed', 'create', 'update', 'enable', 'disable', 'rollback'))
);

CREATE INDEX IF NOT EXISTS idx_config_versions_created
    ON config_versions(created_at DESC);

-- 全局至多一个 is_current=true。用部分唯一索引在 DB 层兜住，
-- 避免应用层"先 update false 再 update true"中途失败留下两个生效版本。
CREATE UNIQUE INDEX IF NOT EXISTS idx_config_versions_current_uniq
    ON config_versions((is_current)) WHERE is_current;
```

`snapshot` 的 JSONB 结构（与 §3 的 YAML 迁出字段一一对应）：

```json
{
  "schema": 1,
  "default_provider": "sensenova",
  "providers": {
    "sensenova": {
      "enabled": true,
      "base_url": "https://token.sensenova.cn",
      "quota_kind": "count",
      "quota_limit": 1400,
      "quota_window": "5h",
      "refresh_hour": null,
      "model_mapping": { "deepseek-v3": "DeepSeek-V3" },
      "count_models": ["deepseek-v3"],
      "reasoning_models": ["deepseek-r1"],
      "credential_env": "SENSENOVA_API_KEY"
    }
  },
  "quota_watermark": {
    "token_soft_ratio": 0.8,
    "token_hard_ratio": 0.9,
    "count_soft_ratio": 0.8,
    "count_hard_ratio": 0.9
  }
}
```

`schema` 字段是快照格式版本号，**必须有**：一年后加了新 provider 字段，回滚到今天的旧快照时要能识别"这个快照没有该字段，用默认值"而不是当成空值写进去。这是 diff 方案避不开、全量方案也仍需显式处理的一处。

**关于 `credential_env`**：designer 在 §4.2 字段 3 与遗留问题 1 中假定此处填**环境变量名**而非密钥原文，理由是配置会进版本历史、历史对所有持看板口令的人可见。**架构确认采纳该假定**：本表只存变量名，网关启动/热加载时按名读 `os.Getenv`。上游 Key 的密文仍走既有 `upstream_keys` 表 + `store.Cipher`，与本表无关。若变量名在环境中不存在，dry-run 阶段就报错拒绝（见 §6.3）。

### 1.3 一次变更的事务边界

一次写操作在**单个 PG 事务**内完成三件事，缺一则版本历史与当前态会漂移：

1. `INSERT INTO config_versions (...) RETURNING version` —— 拿到新版本号
2. `UPDATE config_versions SET is_current = FALSE WHERE is_current` 然后把新行置 `TRUE`
3. `INSERT ... ON CONFLICT (name) DO UPDATE` 写 `provider_configs`，并把该行 `version` 设为新版本号

`audit_logs` 的写入放在**事务提交之后**，沿用 `s.audit()` 现有的"失败只告警不阻断"语义（`internal/gateway/adminapi/admin.go` 的 `audit`）。理由：`config_versions` 已经是不可抵赖的变更记录，`audit_logs` 是统一检索入口而非唯一凭据，让审计写失败去回滚一次已生效的配置变更是本末倒置。

### 1.4 回滚的具体动作

回滚到 #124 时：读 `config_versions` 中 version=124 的 `snapshot`，用它作为 §1.3 的输入生成新版本 #129，`action='rollback'`、`rolled_back_from=124`。`provider_configs` 按 snapshot 内容整体重建（snapshot 里没有的 provider 置 `enabled=false` 而**不删除**，理由见 §6.1）。

---

## 2. 配置快照层设计

### 2.1 快照对象：为什么必须把 adapter 装进去

用户决策 3 要求"请求内配置自始一致"。仅把 `*config.Config` 放进 `atomic.Pointer` 是不够的 —— D4 已证明 adapter 固化了 `model_mapping`。若两者分开替换，就会出现 config 是新的、adapter 是旧的，请求体被改写成错误的上游模型名，**不报错，只是算错**。

因此快照是一个**不可分割的三元组**：

```go
// 新增文件：internal/config/snapshot.go（包 config）
// Snapshot 是一次配置生效的完整视图，创建后不可变。
type Snapshot struct {
    Version  int64
    Config   *Config
    Adapters AdapterSet   // 接口，见下
}

// AdapterSet 由上层注入，config 包只定义接口，不 import internal/adapter。
type AdapterSet interface {
    Get(provider string) (any, bool)
    Providers() []string
}
```

**依赖方向说明**：已验证 `internal/adapter` 不 import `internal/config`（grep 零命中）。为避免反向引入 `config → adapter` 依赖，`config` 包只定义 `AdapterSet` 接口，具体类型由 `internal/gateway` 或 `cmd/gateway` 装配时注入。`Get` 返回 `any` 由调用方断言为 `adapter.Adapter`——这点不优雅，但比引入包循环或把 `Snapshot` 塞进 gateway 包（导致 `background`/`scheduler` 反向依赖 gateway）都更可接受。

替代方案（推荐，若接受新建包）：**把 `Snapshot` 放在新包 `internal/confsnap`**，它同时 import `internal/config` 与 `internal/adapter`，类型完全具体，无需 `any`。依赖方向为 `confsnap → {config, adapter}`，`gateway`/`background`/`scheduler` → `confsnap`。这是更干净的选择，实现者若无阻碍应优先此方案。下文按 `confsnap` 表述。

### 2.2 Holder：atomic.Pointer 的位置与接口

```go
// 新增文件：internal/confsnap/holder.go
type Snapshot struct {
    Version  int64
    Cfg      *config.Config
    Adapters *adapter.Registry
    LoadedAt time.Time
}

type Holder struct {
    ptr atomic.Pointer[Snapshot]
}

func NewHolder(initial *Snapshot) *Holder
func (h *Holder) Current() *Snapshot          // 无锁读，热路径调用
func (h *Holder) Store(s *Snapshot)           // 原子整体替换
func (h *Holder) Version() int64              // 供 /readyz 与一致性检查
```

**放在哪**：独立包 `internal/confsnap`，不放 `internal/gateway`。原因是 `cmd/gateway/background.go`、`internal/scheduler` 都要读快照，若 Holder 在 gateway 包内会形成 `background → gateway` 的反向依赖。

**为什么是 `*Snapshot` 而非 `Snapshot`**：`atomic.Pointer` 换的是指针，读侧拿到的是同一个不可变对象的地址，零拷贝。**约定：`Snapshot` 及其字段一律视为只读**，任何持有者都不得改写 `s.Cfg.Providers` —— 这条约定无法由编译器保证，必须写进代码注释（`Config` 是可变结构体，Go 没有 const）。

**构造函数**：`func Build(cfg *config.Config, version int64) (*Snapshot, error)` 负责按 `cfg.Providers` 装配一个全新 Registry。它必须**自己构造新 Registry 而不复用旧的**，且不能调 `adapter.Registry.Register` 的 panic 路径 —— 见 §2.6。

### 2.3 请求入口取快照与向下传递

**取快照的唯一位置**：`internal/gateway/handlers.go:71` 之前。当前该行是 `provider := s.cfg.ProviderForModel(meta.Model)`，改为先 `snap := s.snaps.Current()`，再 `snap.Cfg.ProviderForModel(...)`。

**为什么在这**：这是所有代理类请求解析出 model 之后、任何配置决策之前的第一个点。三个 handler（chat / images / embeddings）都经此路径。

**向下传递的载体**：`requestPlan`（`internal/gateway/proxy.go:53`）新增一个字段：

```go
type requestPlan struct {
    // ... 现有字段不动
    Snap *confsnap.Snapshot   // 本请求的配置快照，execute 全程只读此字段
}
```

`requestPlan` 已经在承载 `Endpoint`/`Provider`/`Model`/`QuotaKind` 等每请求不变量，且已经贯穿 `execute` → `attempt` 全链路，是天然载体。**不用 `context.Value`**：那会让配置读取变成隐式的、编译器无法检查的，而配置读错正是本项目最贵的失效类型；显式字段能让"这里读了配置"在 code review 中可见。

**重试循环的关键约束**：`execute`(proxy.go:75) 的 `for attempt := 0; attempt < maxAttempts; attempt++`(proxy.go:83) 内部**不得重新取快照**。plan 在进入循环前就已带好 `Snap`，循环内每次 `attempt` 都读同一个。这样即使重试期间发生了热加载，本请求的 3 次 attempt 用的 base_url、model_mapping、adapter 完全一致。

`maxAttempts := s.cfg.Upstream.MaxRetries + 1`(proxy.go:78) 属冷配置（`Upstream` 不在热加载范围），可继续读 `s.cfg`，但为一致性建议一并改为 `plan.Snap.Cfg.Upstream.MaxRetries`——同一个函数里两种读法会让后续维护者困惑。

### 2.4 热区读取点逐处改法（13 处：交接的 11 处减 1 处误判、加 3 处遗漏）

分三类：**R = 请求内读取**（改为读 `plan.Snap`）；**B = 后台任务读取**（改为每轮循环开头取一次 `Current()`）；**L = 生命周期编排**（换快照不够，需 reconcile）。

| # | 位置 | 当前代码 | 类 | 改法 |
|---|---|---|---|---|
| H1 | `handlers.go:71` | `s.cfg.ProviderForModel(meta.Model)` | R | **取快照的源头**。`snap := s.snaps.Current()` → `snap.Cfg.ProviderForModel(...)`，并把 `snap` 写入 `plan.Snap` |
| H2 | `handlers.go:90-93` | `s.cfg.Quota.DefaultMaxTokens` / `EstimateMultiplier` + `s.reasoningEstimateFor(...)` | R | 改读 `snap.Cfg.Quota.*`；`reasoningEstimateFor` 增加 `snap` 入参 |
| H3 | `handlers.go:292-299` | `for providerName, p := range s.cfg.Providers`（`/v1/models`） | R | 取一次 `Current()`，遍历 `snap.Cfg.Providers`。**同时按 `enabled` 过滤** —— 停用的 provider 不应出现在模型列表里，否则客户端会拿到一个必然失败的模型名 |
| H4 | `proxy.go:330` | `provider, ok := s.cfg.Providers[plan.Provider]` → `provider.BaseURL`(336) | R | `provider, ok := plan.Snap.Cfg.Providers[plan.Provider]`。**必须**，否则重试期间热加载会让两次 attempt 打到不同 base_url |
| H5 | `proxy.go:307` | `ad, err := s.adapters.Get(plan.Provider)` | R | `plan.Snap.Adapters.Get(plan.Provider)`。**这是 D4 的根治点** —— adapter 与 base_url 同源同快照，混合态从此不可能出现 |
| H6 | `proxy.go:616-617` | `s.cfg.LimitsFor(provider, kind == quota.KindCount)`（`limitsFor`） | R | `limitsFor` 增加 `snap` 入参，读 `snap.Cfg.LimitsFor(...)` |
| H7 | `server.go:570` | `s.cfg.IsCountModel(provider, model)`（`quotaKindFor`） | R | `quotaKindFor` 增加 `snap` 入参。**此处决定 Redis key 的 `{kind}` 段**，一次请求内前后取值不一致会导致扣费写到一个 key、归档读另一个 key，静默漏计 |
| H8 | `server.go:580` | `s.cfg.IsReasoningModel(...)` + `s.cfg.Quota.ReasoningOutputMultiplier` / `ReasoningFloorTokens` | R | 同上，`reasoningEstimateFor` 增加 `snap` 入参 |
| H9 | `admin.go:287` | `it.Provider = s.cfg.ResolveProvider(...)`（Key 导入） | R | 取一次 `Current()`；同一个 handler 内与 H10 **必须共用同一快照** |
| H10 | `admin.go:302` | `if _, exists := s.cfg.Providers[it.Provider]; !exists` | R | 同 H9 的快照。若分两次取，可能出现"校验时 provider 存在、写库时已被停用" |
| H11 | `background.go:192` | `for provider := range b.cfg.Providers`（`reapLeases`） | B | **交接遗漏（D2）**。每轮 reap 开头取一次 `Current()`，遍历其 Providers。漏改则新 provider 的过期租约永不回收 |
| H12 | `background.go:401-416` | `b.cfg.ResolveProvider("")` / `IsCountProvider` / `LimitsFor` | B | 该批处理开头取一次快照，三处共用。三者必须同源：`kindCount` 与 `LimitsFor` 取自不同快照就是量纲错位 |
| H13 | `background.go:483-484` | `tHard, tSoft := b.cfg.LimitsFor(provider, false)` / `cHard, cSoft := ...(provider, true)` | B | 与 H14 共用同一快照 |
| H14 | `background.go:485` | `for provider := range b.cfg.Providers`（按 provider 建 `Refresher`） | **L** | **交接遗漏（D3）**。见 §2.5，换快照解决不了，需 reconcile 编排 |
| H15 | `main.go:489-511`（`newProbe`） | 闭包捕获 `cfg`；`for _, p := range cfg.Providers { ...; break }`；`cfg.Providers[key.Provider].BaseURL` | **L** | 第 4 个持有点。改为闭包捕获 `*confsnap.Holder`，每次探测执行时取 `Current()`；**并修掉"取第一个 provider 的 mapping"这个既存 bug**，改为按 `key.Provider` 从快照 Adapters 取对应 adapter |

**冷区确认不改**：`server.go:439`（启动日志，D1 误判）、`main.go:83-84`（启动日志）、`server.go:192-206` 与 `main.go:261-273`（启动装配，但要改为调用 `confsnap.Build`，见 §7）、`main.go:448` 与 `background.go:351`（`EgressVerifyTarget`，出口 IP 属冷配置）、`proxy.go:78`（`Upstream.MaxRetries`）、`store.go` 全部 6 处（DB 连接参数）。

### 2.5 H14 特例：Refresher 的生命周期 reconcile

`background.go:485` 为每个 provider 起一个 `Refresher` 长期后台任务（`startRefresher`，`background.go:457`）。这是本次唯一需要"响应式"处理的热区点，其余 12 处都是被动读快照。

**先纠正本文档早期草稿的一处错误。**早期写的是"只改 `base_url` / `quota_limit` 不需要重起，Refresher 每轮从快照读即可"——**这句是错的**，读代码后确认：

`Refresher` 持的是 `RefresherConfig` **值拷贝**（`internal/quota/refresh.go:62-78`），且 `internal/quota` 包不 import `internal/config`、也拿不到快照。其中 `TokenLimits` / `CountLimits` 是构造时由 `b.cfg.LimitsFor(provider, ...)` 算好后固化进去的（`background.go:502-505`）。**它永远不会重新读取配置。**

后果：改了 `quota_limit` 或水位系数后，Refresher 在下一个刷新窗口探测确认成功时，会调用 `MarkRefreshed`（`refresh.go:252`、`refresh.go:255`）把 Key 的 hard/soft **写回旧额度**。这与 D5（scheduler 值拷贝）是同一类失效，但更隐蔽——只在刷新窗口内（默认 12:00-14:00）、且探测确认成功时才显形，其余时间一切正常。界面会显示"配置已生效"，而每天中午它把额度改回去。

### 2.5.1 reconcile 需要三个动作，不是两个

| 触发条件 | 动作 | 为什么不能合并 |
|---|---|---|
| 新快照有、`refreshers` map 里没有的 provider | **起**一个新 Refresher | — |
| `refreshers` map 里有、新快照里没有或已 `enabled: false` 的 provider | **cancel** 对应子 ctx | — |
| 存量 provider 的 `quota_limit` 或水位系数变化 | 调 `Refresher.UpdateLimits(token, count Limits)` **就地更新**，不重建 | 重建会清空状态机（`states` / `rampUntil` / `lastProbe`），若恰好发生在刷新窗口内，全部 Key 从 confirmed 退回 idle 再走一遍 pending → probing，等于中断一次正在进行的刷新流程 |

`UpdateLimits` 是需要给 `internal/quota` 新增的一个方法（带 `r.mu` 写锁）。它安全的原因是：`TokenLimits` / `CountLimits` 这两个字段**只在 `MarkRefreshed` 处被读**（`refresh.go:252,255`），不参与状态机推进，就地换值不会让状态机进入不一致状态。`WindowStart` / `WindowEnd` / `ProbeInterval` / `RampDuration` 参与状态机判定，且来自全局 `refresh.*` 配置，**不在本期热加载范围内**（用户决策 2 只热加载 provider 相关项），因此 `UpdateLimits` 只更新 limits 两项、不碰这四项。但"不在热加载范围"不等于"改了没影响"——见 §2.5.4 的 `rc` 分叉。

### 2.5.2 创建与停止的时机（team-lead 要求写明）

**创建时机**：在热加载 `Swap` **成功之后**触发，不在事务提交时、也不在 `Build` 阶段。理由：`Build` 失败时不该有任何副作用发生；而 `Swap` 之前起 Refresher，它会读到还没生效的配置。顺序固定为 `Build` → `Validate` → `Swap` → `reconcile`。

**停止时机**：provider 变为 `enabled: false` 时立即 cancel，**不等它被物理删除**（本设计没有 DELETE，停用即终态，见 §6.1）。

**这条的落点不在 `reconcileRefreshers`，而在 DB→config 的桥。**这一点我早期表述得不准确，读代码后修正如下：

`config.Provider`（`internal/config/config.go:315-332`）**没有 `Enabled` 字段**，`Enabled` 只存在于 `store.ProviderConfig`(`internal/store/providers.go:57`) 与 `gateway.ProviderConfigView`(`internal/gateway/adminapi/provider_deps.go`) 两层。所以 `reconcileRefreshers` 里 `for provider := range cfg.Providers` 物理上**读不到** `enabled` —— 它没法做这个判断，也不该做。

正确分工：**DB→config 的桥必须把 `enabled: false` 的 provider 整个排除在 `cfg.Providers` 之外**，让"停用"在快照层就等价于"不存在"。这样 `reconcileRefreshers` 现有的键集合比对（`background.go:542`、`background.go:603`）自动正确，无需改动。

**为什么必须在桥这一层过滤，而不是在下游各处判 `enabled`**：`cfg.Providers` 有 148 处读取点（§2 的 H1-H14 加 137 处冷区）。若把停用项放进快照再要求每个读取点自己跳过，等于新增 148 处漏判机会，而漏判的表现是"已停用的 provider 仍在承接流量"且不报错。在桥这一层过滤一次，`ResolveProvider` / `ProviderForModel` / `/v1/models` / 调度 / 归档全部自动正确。

**这也顺带解决 `confsnap.Build` 的一个连带问题**：`newAdapterFor`(`internal/confsnap/snapshot.go:82-92`) 对未登记的 provider 名返回 error，整个 `Build` 随之失败。若停用项进了 `cfg.Providers`，那么一个"已停用且 adapter_kind 不受支持"的历史遗留行会让**每一次热加载都失败**，且失败原因指向一个运维已经停用、认为与自己无关的 provider。过滤掉停用项后这条路径不存在。

**给桥的验证要求（反向验证，team-lead 要求）**：先故意用"键集合 = 全部 provider（含停用）"实现一遍，确认停用后仍能观测到该 provider 的探测日志（`newProbe` 发的是真实请求，不是桩，会真实消耗上游配额），再改成桥层过滤。只验正向的话，两种实现都"看起来能停"——因为测试里往往只测了物理删除的情形。

**实现要点**：`b.every`（`background.go:140`）当前接收 `startRefresher` 传入的同一个 ctx，要做 per-provider cancel 必须给每个 provider 派生子 ctx 并把 `CancelFunc` 存进 `refreshers map[string]context.CancelFunc`。`b.every` 内部有 `b.wg.Add(1)` 且在 `ctx.Done()` 时 `return`（`background.go:150-166`），因此 cancel 子 ctx 能干净退出、不会泄漏 goroutine，也不影响 `wg.Wait()` 的关闭语义。

**不要持久化 Refresher 状态。**停用再启用同一个 provider 时，新 Refresher 的 `states` map 是空的，所有 Key 回到 `RefreshIdle` → `Schedulable` 返回 true（`refresh.go:143-150`）——这正是想要的结果。若有人试图"保留状态以免丢失进度"，反而会把停用期间早已过期的 pending 状态带回来，让这些 Key 永久不可调度。

### 2.5.3 为什么这处最容易出错

它是唯一一个"配置变更需要触发副作用"的点，而副作用的失败（起不起来、停不掉、limits 没更新）**不影响请求路径**，因此没有任何用户可见的报错——只会表现为若干天后有人发现某个 provider 的 Key 额度从没被刷新过，或者额度每天中午被改回旧值。

因此强制要求两条可观测手段：

1. 一条明确的 reconcile 日志：`refresher reconcile: started=[...] stopped=[...] limits_updated=[...]`。三个列表都要打，`limits_updated` 尤其不能省——它对应的正是上面那个"每天中午改回旧值"的失效。
2. `/readyz`（`server.go:429`）里暴露 refresher 数量，与快照中 provider 数量比对（停用项已在桥层被排除，见 §2.5.2），不等即为异常。

### 2.5.4 `rc` 分叉：存活与新建的 refresher 可能跑在不同探测窗口

team-lead 核实 reconcile 实现时发现的一条失效路径，比 `limits_updated` 更隐蔽。**现象为真，但归因需要修正，且实际比"两个窗口不一致"更严重。**

`rc`（`WindowStart`/`WindowEnd`/`ProbeInterval`/`RampDuration`）在 `background.go:506-511` 每次 reconcile 都从当次快照重算，但已存活的 refresher 因 `background.go:544-548` 的 `continue` 被跳过，保留它构造那一刻的 `rc`。于是改了 `refresh.window_start` 又新增一个 provider 时，同一进程内两个 refresher 跑在不同窗口上。

**归因修正**：这个分叉的成因不是"`refresh.*` 不热加载"，恰恰相反——是 `rc` **确实被重算了**（`:506-511` 无条件执行），但只对新建的生效。真正不热加载的是**已存活 refresher 内部的那份值拷贝**。两者的区别决定了后果的严重程度：

若 `refresh.*` 完全不参与热加载，改它就是纯粹的"不生效"，重启后统一对齐，无中间态。而现在它是**部分生效**——新建的用新值、存活的用旧值。`refresh.window_start` 从 12:00 改到 10:00 后新增一个 provider，新 provider 的 Key 在 10:00 进入 pending（`refresh.go:174-200`），**进入 pending 就立即停止承接流量**（`Schedulable` 只放行 idle / confirmed，`refresh.go:143-150`），而老 provider 的 Key 仍在 12:00 才动。两小时内两批 Key 的可调度性由不同规则决定，且日志里两条"刷新探测器已启动"打的都是各自构造时的窗口（`background.go:592-596`），看上去都"正常"。

**裁决：写清后果，不做 rc 变更时的全量重建。**理由与 §2.5.1 一致——重建会清空状态机（`states`/`rampUntil`/`lastProbe`），若恰好发生在窗口内，全部 Key 从 confirmed 退回 idle 再走一遍 pending → probing，等于用一次"必然中断刷新流程"去换一个"仅在改了 `refresh.*` 且同时增删 provider 时才出现"的不一致。代价方向不对。

**但必须补一条启动期不变量**，否则"写清后果"落不到实处：`reconcileRefreshers` 在跳过存活 refresher 前，比对当次算出的 `rc` 与该 refresher 构造时的 `rc`（存进 `refreshers` map 的 value 里，与 `CancelFunc` 一起），**不一致就打一条 WARN**：

```
refresher rc 分叉: provider=%s 存活窗口=%s-%s 当前配置窗口=%s-%s
（refresh.* 变更不热加载，需重启网关使全部 refresher 对齐）
```

这条日志是这个已知缺陷唯一的可观测出口。没有它，"部分生效"这件事在运行期完全不可见——而 §8.4 之所以删掉"部分生效"这个概念，是因为它在**配置写入路径**上不该存在；这里是**后台任务生命周期**，它真实存在，不能靠删概念解决，只能让它可见。

### 2.6 Registry 重建的安全约束

`adapter.Registry.Register`(registry.go:27) 在 provider 名不匹配或 adapter 为 nil 时 **panic**，注释（registry.go:10）明说不支持运行时动态注册。热加载路径**绝不能触发 panic** —— 一次配置写错导致整个网关进程崩掉，比拒绝这次变更糟得多。

因此 `confsnap.Build` 必须：

1. **每次构造全新 Registry**，不复用、不修改现有 Registry。旧快照的 Registry 随旧 Snapshot 一起被 GC，读侧仍持有它的请求不受影响（这是 `atomic.Pointer` 换指针的天然好处）。
2. **在调用 `Register` 之前自行完成全部前置校验**（provider 名格式、adapter_kind 已知、mapping 非 nil），使 panic 的前置条件不可能成立。
3. **返回 error 而非 panic**。构造失败时 `Store` 不被调用，旧快照继续生效，admin API 返回 400/409 并说明原因。

`Registry` 本身**不需要改**：它的 `mu.RLock()` 读锁对单个 Registry 实例的并发读已经安全，而我们从不并发写一个已发布的 Registry。

### 2.7 Scheduler：值拷贝持有点与 D5 缺陷的合并处理

`Scheduler` 持有 `cfg config.Scheduler` / `quotaC config.Quota` 值拷贝（scheduler.go:150-151），不是 `*config.Config`，所以它既不在 4 个持有点内，也吃不到热加载。而 `Reload`(scheduler.go:227) 是现有唯一的热加载通道。

两个问题必须一起改，否则热加载功能上线后会立刻暴露：

1. **D5 量纲错位**：`scheduler.go:240` 的 `hard, soft := s.quotaC.TokenHard(), s.quotaC.TokenSoft()` 改为按 Key 所属 provider 取 `snap.Cfg.LimitsFor(key.Provider, kindCount)`，其中 `kindCount = snap.Cfg.IsCountProvider(key.Provider)`。
2. **配置来源**：`Scheduler` 改为持有 `*confsnap.Holder`，`Reload` 每轮开头取一次 `Current()`。`s.cfg.ActivePoolSize` 继续从快照的 `Scheduler` 节读（该节属冷配置，但从快照读不额外花钱且保持一致）。

不改的话，用户在界面上把 sensenova 的 `quota_limit` 从 1400 改成 2000 并看到"已热生效"，而调度打分仍在用 token 量纲的全局水位 —— **界面说生效了，实际没生效，且没有任何报错**。这正是本项目最贵的失效类型，不能留。

---

## 3. YAML 引导配置边界

### 3.1 留在 YAML 的字段（引导配置，改动需重启）

| YAML 节 | 保留理由 |
|---|---|
| `server` | 监听端口、超时。改动本质上要重启进程 |
| `redis` | 连接串、池参数。热切 Redis 会让在飞的配额操作落到两个实例 |
| `postgres` | 连接串、`auto_migrate`。配置读取自身依赖它，鸡生蛋 |
| `admin` | `api_key`。它是配置写入口的鉴权凭据，放进 DB 意味着"改错了就再也改不回来"；且 `cfg.Admin.APIKey` 为空时整组 admin 路由不注册（`admin.go:10-20`），这个开关必须在启动时确定 |
| `egress` | 出口 IP 池。用户决策 2 明确划为冷配置 |
| `upstream` | 重试次数、超时。属网关自身行为，非 provider 配置 |
| `scheduler` | 池大小、租约 TTL、回收间隔。改动涉及后台任务节奏 |
| `fallback` | 降级策略 |
| `quota` 的**估算与水位系数** | `default_max_tokens`、`estimate_multiplier`、`reasoning_*`、四个水位 ratio。注意：用户决策 2 把"水位系数"划入热加载范围，见 §3.3 的处理 |

### 3.2 迁到 DB 的字段（热加载）

整个 `providers` 节 + `default_provider`：

`base_url`、`quota_kind`、`quota_limit`、`quota_window`、`refresh_hour`、`model_mapping`、`count_models`、`reasoning_models`，加上新增的 `enabled`、`adapter_kind`、`credential_env`。

### 3.2.1 `adapter_kind` 当前是死字段，且 provider 名被 adapter 契约锁死

team-lead 提出要给"不可启用的停用项"加标记时，我去核实这类坏行怎么产生的，发现问题比"标记一下"要根本。**这一节的结论会收紧新建 provider 的能力边界，designer 和 fe-provider 必须知道。**

核实到的三件事：

1. **`adapter_kind` 全链路存在但无人读取。**它有 DB 列（`schema.sql:217`）、有 store 字段（`providers.go:69`）、进 diff（`providers_diff.go:47`）、seed 时被写成 provider 名（`provider_seed.go:75`）、API 视图里也有（`provider_deps.go:39`）。但 `newAdapterFor`(`confsnap/snapshot.go:82-92`) **按 `name` 分派，从不读 `adapter_kind`**，也没有任何一处校验它的取值。它当前是纯粹的死字段。

2. **adapter 的 `Provider()` 返回硬编码常量。**`Volc.Provider()` 返回 `ProviderVolc`（`volc.go:45`），`SenseNova.Provider()` 返回字面量 `"sensenova"`（`sensenova.go:43`），都与传入的 mapping 无关。

3. **`Registry.Register` 要求注册名 == `adapter.Provider()`，不等则 panic**（`registry.go:31-34`）。

三者合起来的后果：**provider 的 `name` 不是一个自由字段，它必须恰好等于某个已实现 adapter 的硬编码 `Provider()` 值。**当前只有 `volc` 和 `sensenova` 两个合法取值。

这比 team-lead 描述的"不受支持的 adapter_kind"宽得多——不是"某些类型不支持"，而是**新建 provider 这个功能实际上只能建出这两个名字，而这两个名字通常已经存在了**。换言之，本期交付的"新增 provider"能力在没有配套代码改动的前提下几乎无处可用。

**裁决：不在本期扩展这个能力，但必须把边界前移到创建时拒绝，而不是留到 `Build` 失败。**

具体要求三条：

- **`POST /admin/providers` 必须校验 `name` 在受支持列表内**（`{volc, sensenova}`，由 `newAdapterFor` 的 `switch` 导出为一个 `confsnap.SupportedProviders()`，**不要在 handler 里另抄一份字面量** —— 抄一份就等于新增一处会与 `switch` 失同步的地方）。不通过则 `400` + `code: unsupported_provider`，message 明确列出当前支持的取值。
- **拒绝理由必须说清这是代码级限制**，不是配置问题。文案给 designer：`provider 名必须是 volc 或 sensenova 之一。新增其他上游需要先实现对应的 adapter（internal/adapter/），不能仅通过配置完成。` 这里**要**写出判据（与 §6.4 疑似密钥那条相反）—— 那条不写判据是防规避，这条写清判据是因为运维无法自行绕过，含糊只会让他反复试。
- **`adapter_kind` 本期保持只读**：seed 写入、diff 记录、界面展示，但**不接受 API 传入修改**。理由：让它可改会造成"`adapter_kind=volc` 而 `name=sensenova`"的组合，而 `newAdapterFor` 按 name 分派、`Register` 按 `Provider()` 校验，这个组合的实际行为与 `adapter_kind` 无关 —— 一个能改但不起作用的字段，是比死字段更糟的东西。把它变活的正确做法是让 `newAdapterFor` 改按 `adapter_kind` 分派并解开 `Provider()` 的硬编码，那是独立一期的事。

**对 team-lead 要求的"不可启用标记"的处理**：上述创建期校验生效后，**通过 API 建不出坏行**。但 seed 路径仍可能产生 —— 空库首次启动时从 YAML 导入（§3.5），若历史 YAML 里有第三个 provider 名，`provider_seed.go:75` 会把它原样写进库。所以标记仍然要做：

**列表项若 `name` 不在 `SupportedProviders()` 内，标记为"不可启用"并禁用启用按钮**，说明原因是该 provider 类型当前版本未实现 adapter。不给一个看起来能点、点了必然失败的按钮 —— 否则运维会反复尝试、反复吃同一个拒绝，并合理地怀疑是系统 bug。

这类行**永久留在列表里且无法删除**（本设计无物理 DELETE，且 `name` 物理禁改，见 §6.1），这是有意接受的：坏数据留着不动是安全的，但必须让它看起来就是不可救的。

**seed 侧同时要做的**：`provider_seed.go` 遇到不受支持的 provider 名时，**跳过并打 WARN**（而不是写入后让它变成一个永久的坏行）。已存在的坏行不追溯清理。

### 3.3 水位系数的归属：留 YAML，但纳入快照

用户决策 2 把"水位系数"列入热加载范围，但四个 ratio（`token_soft_ratio` 等）在 `config.Quota` 下、是**全局**而非 per-provider 的。折中方案：

**保持在 YAML，但不单独开 DB 表。**理由：它们是全局调参而非 provider 属性，为 4 个浮点数建表并加一套 CRUD 是过度设计；而它们又确实需要热调（调水位是压测期的常规动作）。

处理方式：`provider_config_versions.snapshot` 里**冗余记录当前生效的四个 ratio**（前文 §1.3 的 snapshot 结构已含 `quota_watermark`），但**不提供修改入口**。作用是让版本历史能回答"当时的水位是多少"——排查历史配额异常时这是必需的上下文。真要改 ratio 仍走 YAML + 重启。

若后续确有热调需求，扩展路径是给 `provider_configs` 加四个可空的 per-provider override 列（`ALTER TABLE ... ADD COLUMN IF NOT EXISTS token_soft_ratio DOUBLE PRECISION`），NULL 表示继承全局。**本期不做**，但表结构留了这个口子。

### 3.4 YAML 中 `providers` 节的过渡期语义

`config.Load`(config.go:698) 目前用二次 `probe` 解析判断"文件里是否出现 providers 键"，出现则**整体替换**而非合并 —— 这个行为要保留，但语义改为：

**YAML 的 `providers` 节降级为 seed 数据源，只在空库首次启动时被读取。**

`dec.KnownFields(true)` 的严格模式必须保留，因此不能直接删掉 `Config.Providers` 字段（否则所有现存 YAML 都会因未知字段而启动失败）。保留字段 + 改变用途，是唯一不破坏现有部署的路径。

### 3.5 空库首次启动：从 YAML 一次性 seed

**做 seed，不做"空配置启动"。**

理由：若空库启动时 provider 集合为空，网关会起来但任何请求都路由失败（`ProviderForModel` 找不到 provider），而运维看到的是进程健康、`/healthz` 正常。这是又一个静默失效。而 seed 让首次启动后行为与迁移前完全一致，是最小意外原则。

流程（在 `main.go` 中 `st.Migrate(ctx)` 之后、构造 registry 之前）：

1. `SELECT count(*) FROM provider_configs` —— 为 0 才 seed，非 0 直接跳过
2. 若 YAML 有 `providers` 节，用它 seed；若也没有，用 `config.Default()`(config.go:585) 内置的 `volc` provider seed
3. seed 写入走 §1.4 的同一个事务，`action='seed'`、`actor='system'`、`reason='首次启动从引导配置导入'`，生成版本 #1
4. seed 之后**必须**跑一次完整的 `Validate()`。若 YAML 里的 providers 配置本身不合法（历史遗留），要在启动时明确失败，而不是 seed 进一份坏配置

**幂等性**：判断条件是"表为空"而非"某个 flag"。这样重复启动不会重复 seed，而运维若真要重新 seed，删空表即可（有版本历史兜底，不会丢数据）。

**一个必须写进实现注释的坑**：seed 判断必须在 `Migrate` 之后。若 `auto_migrate=false`（生产可能关掉），表不存在时 `SELECT count(*)` 会直接报错——此时应该是明确的启动失败（提示"请先执行迁移"），而不是被当成"表为空"去 seed。

---

## 4. Admin API 契约

### 4.1 路由注册与鉴权

全部端点挂在既有 `adminChain`（`server.go:304`）下，注册位置紧随 `server.go:265` 之后，仍在 `if cfg.Admin.APIKey != ""` 分支内 —— 即 **admin key 为空时这组路由整体不注册**，沿用 `admin.go:10-20` 的既有约定。鉴权头 `Authorization: Bearer <admin_key>`，操作人身份取 `X-Admin-Actor`（`adminActor`，admin.go:26），缺省 `"admin"`。

路由写法用 Go 1.22+ method pattern，与 `server.go:263` 的 `"PATCH /admin/keys/{key_id}"` 一致：

```go
s.mux.Handle("GET /admin/providers",                    s.adminChain(s.handleProviderList))
s.mux.Handle("POST /admin/providers",                   s.adminChain(s.handleProviderCreate))
s.mux.Handle("GET /admin/providers/{name}",             s.adminChain(s.handleProviderGet))
s.mux.Handle("PATCH /admin/providers/{name}",           s.adminChain(s.handleProviderPatch))
s.mux.Handle("POST /admin/providers/{name}/enable",     s.adminChain(s.handleProviderEnable))
s.mux.Handle("POST /admin/providers/{name}/disable",    s.adminChain(s.handleProviderDisable))
s.mux.Handle("POST /admin/providers/validate",          s.adminChain(s.handleProviderValidate))
s.mux.Handle("GET /admin/config/versions",              s.adminChain(s.handleConfigVersions))
s.mux.Handle("GET /admin/config/versions/{version}",    s.adminChain(s.handleConfigVersionGet))
s.mux.Handle("POST /admin/config/rollback",             s.adminChain(s.handleConfigRollback))
s.mux.Handle("PATCH /admin/config/default-provider",    s.adminChain(s.handleConfigDefaultProvider))
```

**没有 DELETE 端点。**删除一律走 `/disable`，理由见 §1.3 的 `enabled` 说明与 §6.1。这不是遗漏，实现者不要"补全" RESTful 语义。

**响应格式沿用既有 `writeJSON`（server.go:545）**，即直接返回业务对象，不套 `{code, data, message}` 外层 —— 现有全部 admin 端点（admin.go:93/125/228/446/495）都是这个风格，本期不引入第二种约定。

### 4.1.1 错误响应与错误码（修正：不用 422）

错误走既有 `writeError(w, r, status, code, msg)`（`server.go:518`），它输出 OpenAI 风格三段信封：

```json
{ "error": { "message": "...", "type": "invalid_request_error", "code": "..." } }
```

**关键事实（核实后修正了本文档早期草稿）**：`type` 字段由 `errorTypeFor(status)`（`server.go:531`）**从状态码自动派生**，调用方无法指定；能自由取值的是 **`code`**。项目现有 code 取值：`invalid_request`、`internal_error`、`invalid_auth`、`provider_not_found`、`service_busy`、`volc_banned`、`volc_invalid`。

因此 designer 提的"三种情况要可区分"这个要求成立，但**解决手段不是引入新状态码，而是用 `code` 区分** —— 项目已经有这个机制，且 `admin_patch.go:100-102` 正是这么做的（`ErrPreconditionFailed` → 409 + `invalid_request`）。

**422 全项目零使用**（网关侧 grep 无命中；`dashboard/tests/test_api.py:568` 那几处 422 是 FastAPI 对 query 参数的内建校验，不是本项目主动写的）。所以 designer 说"422 在本项目其他地方表示请求体形态不合法"这个前提**不准确** —— 网关侧根本没用过它。但结论仍然对：**不该引入 422**，因为引入它会给这套 API 造出一个网关里独一无二的状态码，运维和 dashboard 都没有既成直觉。

修正后的错误码表：

| 情况 | HTTP | `code` | 运维的下一步 |
|---|---|---|---|
| 请求体形态不合法（JSON 解析失败、字段类型错、缺必填） | 400 | `invalid_request` | 改参数重试 |
| provider 不存在 | 404 | `provider_not_found` | 检查名字 |
| `expected_version` 不匹配（并发冲突） | 409 | `version_conflict` | **刷新后重新编辑** |
| 改动被字段规则拒绝（改 name、有流量改 `quota_kind`、疑似密钥原文等） | 409 | `immutable_field` | **改用新建 provider + 重新导入 Key** |
| 跨量纲回滚未带确认位 | 409 | `quota_kind_mismatch` | **改用编辑表单单改 quota_limit，别走回滚** |
| 停用会造成坏状态（停用默认 provider、enabled 归零） | 409 | `invalid_state_transition` | 先改默认 provider / 先启用别的 |
| 新建时 `name` 不在受支持列表内 | 400 | `unsupported_provider` | **无法在本页解决** —— 需先实现对应 adapter（§3.2.1）。这是唯一一个运维自己无解的错误，文案必须写清判据与支持列表 |
| 校验通过但热加载失败 | 200 + `reloaded:false` | — | 查该实例日志 |

三种 409 用 `code` 区分，dashboard 据 `code` 分派文案，不靠状态码猜。这满足 designer 的诉求（三者处置动作不同、界面要能给准确的下一步），又不引入项目里没有的状态码。**实现者注意：`writeError` 的 `code` 参数此前只用过 `invalid_request` 这个泛化值，本期新增的四个 code 是有意的细化，不要图省事全填 `invalid_request`。**

**通则：拒绝文案是否写出判据，看运维能不能自行绕过。**

本文档里有两条方向相反的文案要求，容易被实现者当成前后矛盾，这里把判据固定下来，后续新增拒绝场景照它决策，不要再逐条讨论：

| 判据 | 文案怎么写 | 本文档中的实例 |
|---|---|---|
| 运维**能**绕过判据（改一改输入就能骗过检查） | **不写判据**，只说被拒绝和该走哪条正路 | §6.4 检查 9b 疑似密钥原文 —— 写明"长度 > 40 且含小写"就等于教人把密钥截短或改大小写塞进去，检查随即失效 |
| 运维**绕不过**判据（缺的是代码，不是输入） | **必须写清判据**，并把合法取值列全 | §3.2.1 `unsupported_provider` —— 支持哪几家由代码决定，含糊只会让他把厂商名反复试一遍，每次都拿到同一个拒绝 |

判断方法：假设文案已经贴在运维面前，问一句"他看完能不能在本页把这次提交塞过去"。能，就说明判据本身是防线，写出来等于自废；不能，就说明判据是事实说明，藏起来只制造无效重试。

界面侧对应处理：拒绝文案统一在 `docs/provider-config-ui.md` §8.3 的错误列表里渲染；疑似密钥那条的成品文案与"不写判据"的理由见该文档 §4.2（`:379-383`）。两侧文案必须同进同退 —— 服务端 message 与界面文案对同一个 `code` 给出的判据披露程度要一致，否则运维从两个地方读到两套说法。

**通则的推论：写清判据之后，必须同时给出全部合法路径。**

"写清判据"只完成一半。运维绕不过判据，说明他缺的不是输入而是正确操作路径 —— 只告诉他"塞不过去"，会把无效重试换成无从下手，同样卡住。所以这一档的文案要写成三段：**被拒的是什么与后果 → 每一条合法路径怎么走**。

路径必须列全，而不是只给最可能的那一条。判据是**同一个提交内容能对应几种意图**：若服务端分不出意图，文案就得把每种意图的出路都写上，因为服务端替运维猜意图必然猜错一部分人。

本文档里最典型的一例是 §7.6.1 的跨 provider 导入（`immutable_field`）：`key_id` 已存在但 `provider` 不同这一个提交，对应两种完全不同的意图 —— ① 正在走"新建 provider + 重新导入"（出路：换新 key_id，旧 Key 留在原 provider 下停用）；② 只想改密钥或池位、`provider` 字段是顺手填错的（出路：把 `provider` 改回原值，或删掉该字段）。两者在请求体上完全一样，服务端无从分辨。**只写 ① 会让第二种人以为必须新建一批 Key，只写 ② 会让第一种人卡在原地。**

这条推论由 designer 在界面侧提出并落文（`docs/provider-config-ui.md` §8.5 三段式文案），架构侧确认为通则的一部分：适用于所有"绕不过"档的 code，不限于这一处。服务端 message 也要给出这两条路径 —— 否则两侧披露程度不齐，违反上一段的同进同退要求。

### 4.1.2 PATCH /admin/config/default-provider

采纳 designer 的论证：不做这个端点，页面就能制造一个页面自己修不了的坏状态（停用了默认 provider → 未指定 provider 的请求落到无候选 Key 的分支 → 只能改 YAML 加重启）。这轮的目标就是把配置搬出 YAML，留一个必须回 YAML 才能修的洞不合适。

请求体：

```json
{ "expected_version": 128, "name": "sensenova", "reason": "把默认切到 sensenova 以便停用 volc" }
```

服务端规则：

1. 走与其他写操作**同一套** `expected_version` 乐观锁（409 + `version_conflict`）
2. **独立校验目标 provider 存在且 `enabled`** —— 界面已不渲染停用行的按钮，但那不能是唯一防线（designer 明确要求，我同意：界面约束和服务端约束必须各自成立）。违反返回 409 + `invalid_state_transition`
3. 进配置版本历史，`action = 'set_default'`，审计 `action = 'provider_config.set_default'`
4. 与 §4.6 的 disable 前置检查形成闭环：不能停用默认 provider，要停就先改默认

`provider_config_versions` 的 `action` CHECK 约束（§1.2）需补 `'set_default'`：

```sql
-- §1.2 的 chk_pcv_action 改为：
CHECK (action IN ('seed','create','update','enable','disable','rollback','set_default'))
```

`config_versions.action` 的这个新取值对应 designer §7.1 动作徽章的 `设为默认`。

**`default_provider` 的存储位置**：它不是 provider 的属性，存 `provider_config_state` 表（§1.2 已有该单行表）新增一列：

```sql
ALTER TABLE provider_config_state ADD COLUMN IF NOT EXISTS default_provider TEXT NOT NULL DEFAULT '';
```

`snapshot` JSONB 里已含 `default_provider`（§1.2），回滚时一并恢复 —— 这一点原设计已经对了。

### 4.2 GET /admin/providers —— 列表

响应 200：

```json
{
  "active_version": 128,
  "supported_providers": ["volc", "sensenova"],
  "providers": [
    {
      "name": "sensenova",
      "enabled": true,
      "base_url": "https://token.sensenova.cn",
      "quota_kind": "count",
      "quota_limit": 1400,
      "quota_window": "5h",
      "refresh_hour": null,
      "model_mapping": {"deepseek-v3": "DeepSeek-V3"},
      "count_models": ["deepseek-v3"],
      "reasoning_models": ["deepseek-r1"],
      "adapter_kind": "openai_compatible",
      "credential_env": "SENSENOVA_API_KEY",
      "credential_present": true,
      "version": 128,
      "is_default": true,
      "has_traffic": true,
      "created_at": "2026-08-20T06:11:03Z",
      "updated_at": "2026-08-29T06:22:41Z"
    }
  ]
}
```

**`supported_providers` 挂在列表接口的顶层，不单独开端点**（回答 designer 的接口问题）。

它是**响应顶层字段而非数组元素字段**，这一点决定了 designer 担心的空状态问题不存在：`providers` 为空数组时 `supported_providers` 照样返回完整取值。第一次新建 provider 恰好发生在列表为空时，选择器此刻必须已有选项 —— 顶层字段天然满足，不需要为空状态另开一次请求。

不单独开端点的理由：单独端点会引入一个新的时序问题 —— 两次请求之间若发生热加载，选择器的选项与列表数据来自不同快照，而这恰好是本设计用请求内单快照要根除的那类不一致。同一个响应里带出来，两者必然同源。

取值来自 `confsnap.SupportedProviders()`（§7.7），该函数由 `newAdapterFor` 的 `switch` 导出，是唯一真相来源。**handler 禁止另抄字面量** —— 抄一份的失效表现是"代码支持某厂商但界面不给选"，且不报错。designer 已明确界面侧 `volc、sensenova` 那串不硬编码、从此字段拼装，两侧口径一致。

顺序约定：**按 `switch` 的 case 书写顺序返回，不排序**。运维看到的顺序稳定，且新增 adapter 时新项出现在末尾而不是插进中间。

三个派生字段的用途：

- `credential_present`：`os.Getenv(credential_env) != ""`。**只回布尔，永不回值。**界面据此提示"环境变量未设置"。
- `has_traffic`：该 provider 在 `usage_records` 或 `key_daily_history` 中是否有记录。**这是 §6 不可变字段判定的依据**，界面据此禁用某些字段的编辑。
- `is_default`：是否为 `default_provider`。designer 用它渲染列表里的 `默认` 徽章（互斥单值，只有一行为 true）并决定是否渲染「设为默认」按钮。

**`credential_present` 的一条重要约束**（designer §3.8 提出，我同意并记在这里，因为它是后端能力边界而非界面偏好）：这个字段为 `false` 时，**修复动作不在本 API 的能力范围内**。设置环境变量的值必须在部署环境操作并重启网关 —— `credential_env` 存的是变量名，值从进程环境读，进程环境不能热改。

因此 dashboard 的提示文案必须写清"要在部署环境设置该变量的值并重启网关，不在本页热生效范围内"。少了这句，运维看到红徽章的第一反应是在页面上把密钥值填进去，而页面结构上做不到 —— 那这个徽章就只是报警不给出路。**这也是为什么 `credential_env` 存变量名而不存值**（§6.2 的密钥原文校验）：值进配置表就会进版本历史，无法收回。

时间戳一律 RFC3339 UTC。**`created_at` 是 UTC，与 CST 差 8 小时** —— dashboard 侧展示需自行换算，不要在网关侧做时区转换（与既有端点保持一致）。

### 4.3 POST /admin/providers —— 新增

请求体：

```json
{
  "name": "moonshot",
  "base_url": "https://api.moonshot.cn/v1",
  "quota_kind": "token",
  "quota_limit": 5000000,
  "quota_window": "24h",
  "refresh_hour": null,
  "model_mapping": {"kimi-k2": "moonshot-v1-128k"},
  "count_models": [],
  "reasoning_models": [],
  "adapter_kind": "openai_compatible",
  "credential_env": "MOONSHOT_API_KEY",
  "enabled": false,
  "reason": "接入 moonshot 作为 sensenova 的备份通道"
}
```

`reason` 必填（非空、≤500 字符），与 designer §4.2 字段 12 一致。`enabled` 默认 `false` —— **新增的 provider 默认停用**，让运维能先建好配置、单独验证凭据与 mapping，再显式启用。默认启用意味着一保存就立刻承接流量，配置写错的后果无缓冲。

响应 201：`{"name": "moonshot", "version": 129, "reloaded": true}`

错误：400（校验失败，body 同 §4.5 的 dry-run 失败结构）、409（name 已存在）。

### 4.4 PATCH /admin/providers/{name} —— 修改

请求体只含要改的字段 + 两个必填控制字段：

```json
{
  "expected_version": 128,
  "reason": "上调 sensenova 日额度至 2000",
  "quota_limit": 2000
}
```

`expected_version` 必填。与 `provider_configs.version` 不等时返回 **409**，body：

```json
{
  "error": "version_conflict",
  "message": "该 provider 的配置在你编辑期间已被改动",
  "current_version": 130,
  "expected_version": 128
}
```

对应 designer §8 的冲突文案（`provider-config-ui.md:672,678`）。**不做 last-write-wins**：两个运维同时改配额，静默覆盖掉对方的改动是典型的"不报错只算错"。

**PATCH 语义的一处明确约定**：`model_mapping` 字段若出现，是**整体替换**而非合并。与 `config.Load` 对 providers 节的处理（config.go:698 附近，整体替换而非合并）保持一致。若做成合并，"删掉一条 mapping"就没有表达方式了。此约定必须写进 API 文档与界面提示。

`refresh_hour` 需要区分"不传"（不改）与"传 null"（清空为不按小时刷新）。实现上用 `*json.RawMessage` 或 `json.Unmarshal` 到指针的指针；**这是本组端点最容易写错的一处**，漏掉会导致"想清空却清不掉"或"没想改却被清空"。

响应 200：`{"name":"sensenova","version":131,"reloaded":true,"changed_fields":["quota_limit"]}`

### 4.5 POST /admin/providers/validate —— dry-run 校验

不写库、不热加载，只跑校验并返回结果。请求体与 §4.3/§4.4 相同，额外带 `"target": "create" | "update"`；`update` 时需带 `name`。

**校验实现复用 `confsnap.Build`**：把候选配置合入当前快照的副本，构建一个临时 Snapshot（含临时 registry），跑 `Config.Validate()`(config.go:897)。这样 dry-run 与真实生效走**完全相同的代码路径**，不存在"dry-run 过了但真提交失败"的漂移。临时快照构建完即丢弃，不 `Swap`。

响应 200（校验通过）：

```json
{
  "ok": true,
  "diff": [
    {"field": "quota_limit", "before": 1400, "after": 2000}
  ],
  "warnings": [
    {"code": "quota_limit_increased", "message": "上调额度不会追溯已扣减的用量，当日水位按新上限重算"}
  ]
}
```

响应 200（校验不通过，注意仍是 200 —— dry-run 的"校验失败"是正常业务结果，不是请求错误）：

```json
{
  "ok": false,
  "diff": [],
  "failures": [
    {"field": "quota_kind", "code": "immutable_with_traffic",
     "message": "sensenova 已有流量，quota_kind 从 count 改为 token 会让 Redis 中按 count 累加的计数被当作 token 读取，配额水位整体失真且全程不报错"},
    {"field": "model_mapping.deepseek-v3", "code": "empty_upstream_model",
     "message": "上游模型名不能为空"}
  ]
}
```

`failures` 数组结构参照 `admin.go` 批量导入的 `failures []failure` 风格（单个失败不中断整批校验，一次返回全部问题）。**必须一次返回全部失败项**，逐个返回会让运维改一个提交一次，反复踩坑。

### 4.6 enable / disable

`POST /admin/providers/{name}/enable`、`POST /admin/providers/{name}/disable`，body `{"expected_version":128,"reason":"..."}`。

`disable` 的前置检查（返回 409）：

- 该 provider 是 `default_provider` → 拒绝，提示"请先把默认 provider 切到其他 provider"
- 停用后 `enabled` 的 provider 数为 0 → 拒绝，"至少保留一个启用的 provider"
- 该 provider 尚有未回收的活跃租约 → **不拒绝，但在响应里 warn 并说明"在途请求仍会完成，租约将由 reap 正常回收"**

`disable` 后的行为边界（必须写进 API 文档，否则实现者会各写一套）：不再出现在 `/v1/models`；不再接受新 Key 导入；不再被调度派发；**但 `reapLeases` 仍遍历它**（§2.4 B1），且历史用量与版本记录完整保留。

### 4.7 版本列表与详情

`GET /admin/config/versions?limit=50&before=129`：游标分页（`before` 为版本号），不用 offset —— 版本号天然单调，游标分页在持续写入时不会漏行或重复。designer §8 明确要求"加载更多"而非无限滚动（`provider-config-ui.md:671`）。

```json
{
  "active_version": 129,
  "items": [
    {"version":129,"action":"rollback","target":"sensenova",
     "changed_fields":["quota_limit"],"rolled_back_from":124,
     "reason":"#128 的上限调整导致 Key 大批退出派发","actor":"zhang",
     "created_at":"2026-08-29T06:22:41Z"}
  ],
  "has_more": true
}
```

`GET /admin/config/versions/{version}` 返回该版本的完整 `snapshot`。**响应中 `credential_env` 字段照原样返回（它只是变量名）；但若历史快照中存在疑似密钥原文（长度 > 20 且不匹配环境变量名格式），网关侧就地脱敏为 `"[REDACTED]"`** —— 呼应 designer §7.2（`provider-config-ui.md:565`）。脱敏在网关侧做而不只在前端做：前端脱敏挡不住直接调 API。

### 4.8 POST /admin/config/rollback

```json
{
  "target_version": 124,
  "expected_version": 129,
  "reason": "#128 的上限调整导致 Key 大批退出派发，先回滚"
}
```

`expected_version` 必须等于当前 `active_version`，否则 409。

**回滚前置校验（复用 §4.5 的同一条路径）**：把目标快照当作候选配置跑一次 `Build` + `Validate`。若目标版本引用的 `credential_env` 现在已不存在于环境中，或 `adapter_kind` 已不被支持，回滚必须被拒绝而不是回滚出一个跑不起来的配置。

**跨量纲回滚：默认拒绝，不是"待确认"。**（本节按 designer 的意见修正了早期草稿的语义。）

若目标版本与当前版本任一 provider 的 `quota_kind` 不同，**直接返回 409 + `code: quota_kind_mismatch`**，不落库、不生成新版本。运维往往只想回滚 `quota_limit`，没意识到顺带换了量纲 —— 这是 designer §7.3 标记为"回滚里最危险的情形"（`provider-config-ui.md:577-585`）。

早期草稿把它写成"响应 200 + `applied: false` + 带 `confirm_quota_kind_change: true` 重试"。**这个语义是错的**，理由是 designer 提出的：`applied: false` 的 200 会把这条路径读成"待确认的正常流程"，进而诱导实现者把该标志位当成通用放行开关。正确的语义是：

- **默认拒绝**——不带该字段就是 409，这是终态，不是中间态
- 该标志位**只对 `quota_kind` 这一项差异生效**，且**只放行 `quota_kind` 这一项检查**。它不放宽 `credential_env` 缺失、`adapter_kind` 不支持、`expected_version` 冲突中的任何一条 —— 那些各自独立判定
- **实现者不得把它变成"前端确认过就放行"的语义**。dashboard 永不发送该字段为 true（designer 明确确认：界面上没有任何入口可以置真，跨量纲回滚在前端就被拦下、请求不会发出）。因此它的实际作用是一道**服务端默认安全网**，挡住未来的新客户端、手写 curl、以及本页面因回归而漏掉这道检查的情形
- 将来若真有迁移脚本需要换量纲，**那条路径必须自己论证 Redis 命名空间与归档量纲怎么处理**，而不是靠置一个前端本就不会置的标志位拿放行

换言之：这个字段存在的意义是"让绕过必须是显式且被记录的"，不是"提供一个确认步骤"。带该字段成功执行时，审计 `detail` 必须记 `"quota_kind_override": true` 与变更前后的量纲 —— 走了这条路必须在流水里留痕。

回滚成功响应：`{"version":130,"rolled_back_from":124,"reloaded":true,"changed_fields":["quota_limit"]}`

### 4.9 热加载的触发与收敛

写操作成功提交事务后，**当前进程立即同步重载**（`Build` + `Swap`），`reloaded` 字段反映这一步是否成功。若 `Swap` 前的 `Build` 失败（理论上不该发生，因为 dry-run 已过），返回 200 但 `reloaded: false` 并附 `reload_error` —— 配置已落库，进程仍用旧快照，运维需要知道这个不一致。

**其他实例的收敛靠轮询**：后台任务每 `N` 秒（建议 10s，复用 `background` 的 tick）`SELECT active_version FROM provider_config_state`，与 `Holder.Version()` 不等则重载。轮询而非 PG `LISTEN/NOTIFY`：实例数是个位数，10s 收敛延迟完全可接受，而 `LISTEN/NOTIFY` 要处理连接断开后的漏通知补偿，复杂度不划算。

`/readyz`（server.go:429）响应新增 `config_version` 字段，让运维能直接看到各实例的收敛状态，支撑 designer §8 的"某实例未确认重载"文案（`provider-config-ui.md:659`）。

---

## 5. 审计字段约定

复用既有 `audit_logs` 表（`schema.sql:126-134`）与 `InsertAuditLog`（`internal/store/history.go:152`）/ `storeAdapter.Audit`（`cmd/gateway/adapters.go:254`），**不新增审计表**。`config_versions` 已经承担"变更内容的完整记录"，`audit_logs` 承担"跨资源的统一操作流水"，两者职责不重叠。

### 5.1 四个字段各写什么

| 字段 | 写入内容 | 说明 |
|---|---|---|
| `actor` | `adminActor(r)` 的返回值（`admin.go:26`，取 `X-Admin-Actor`，缺省 `"admin"`） | 与既有 Key/User 管理端点完全一致，不另起一套 |
| `action` | `provider.create` / `provider.update` / `provider.enable` / `provider.disable` / `provider.set_default` / `config.rollback` / `config.seed` / `config.reload_failed` | 点分命名空间。现有 action 值风格需在实现时对齐 —— 若既有值是 `key_import` 这类下划线风格，则改用 `provider_create` 保持一致，**不要在同一张表里混两种命名风格** |
| `target` | provider 名（如 `sensenova`）；`config.rollback` 与 `config.seed` 写 `"-"` 表示全局；`provider.set_default` 写新的默认 provider 名 | `target` 是排查时的主要过滤维度，写 provider 名比写版本号有用（版本号在 detail 里） |
| `detail` | JSONB，结构见下 | |

`action == ""` 会被 `InsertAuditLog` 直接拒绝（history.go 内校验），实现时不要漏传。

### 5.2 `detail` 的 JSONB 结构

固定四个键，**不把整份快照塞进 detail**（快照已在 `config_versions.snapshot`，重复存会让 `audit_logs` 膨胀且两处可能不一致）：

```json
{
  "version": 131,
  "reason": "上调 sensenova 日额度至 2000",
  "changes": [
    {"field": "quota_limit", "before": 1400, "after": 2000}
  ],
  "risk": ["quota_limit_increased"]
}
```

| 键 | 说明 |
|---|---|
| `version` | 本次变更生成的版本号。**这是 `audit_logs` 与 `config_versions` 的唯一关联键**，排查时凭它去查完整快照 |
| `reason` | 运维填的变更原因原文，冗余一份在此，让"只看审计流水"也能读懂 |
| `changes` | 字段级 before/after 数组。`create` 动作 `before` 为 `null`；`rollback` 只列实际发生变化的字段 |
| `risk` | 触发过的告警码数组（与 §4.5 的 `warnings[].code` 同一套词表），无告警则为 `[]` |

**`changes` 中的敏感值处理**：`credential_env` 只存变量名，本身不敏感，可原样记录 before/after。但**若某次提交的 `credential_env` 值疑似密钥原文**（长度 > 20 且不匹配 `^[A-Z][A-Z0-9_]*$`），审计里记为 `"[REDACTED]"` —— 与 §4.7 的脱敏规则同源，避免运维误粘密钥后密钥永久留在审计表里。

`rollback` 的 detail 额外带一个键：

```json
{
  "version": 130,
  "rolled_back_from": 124,
  "reason": "...",
  "changes": [...],
  "risk": ["quota_kind_reverted"]
}
```

### 5.3 写入时机与失败处理

事务提交成功**之后**写（§1.4）。沿用 `s.audit(...)`（`admin.go:34`）的"失败只告警不阻断"，与既有全部写操作一致。`InsertAuditLog` 是同步写（history.go 注释：管理操作频率极低，操作已执行但审计丢了在合规上不可接受）—— 这个同步语义对配置变更同样成立，不要为了响应延迟改成异步。

**dry-run（`/validate`）不写审计。**它不改变任何状态，写审计只会淹没真实变更记录。

---

## 6. 不可变字段清单与校验规则

本节是"保守优先"原则落地最密集的地方。判定分三档：

- **绝对禁改**：任何情况下 API 都拒绝（409 + `immutable_field`，见 §4.1.1）
- **有流量后禁改**：无流量时可改，有流量后拒绝
- **可改但强告警**：允许，dry-run 返回 warning，需运维在界面上看过后果再确认

"有流量"的判定：该 provider 在 `usage_records` 中近 7 天有记录，**或** `key_daily_history` 中有任何归档记录，**或** Redis 中存在 `{provider}:quota:*` 键。三者取或——只查其一都会漏：PG 可能已归档清理，Redis 可能已过期。

### 6.1 绝对禁改

| 字段 | 理由 |
|---|---|
| `name` | 三重依赖：① Redis 配额 key 前缀 `{provider}:quota:{kind}:{key_id}:{day}`，改名等于把现有配额计数全部孤立，新名从 0 开始计——**当日额度瞬间翻倍且不报错**；② `usage_records` / `key_daily_history` 的归档维度，改名造成账目断层；③ `upstream_keys.provider` 外键语义。API 层面：PATCH 请求体中出现 `name` 直接 409 + `immutable_field`。**正确做法：新建 provider → 用既有 `POST /admin/keys/import` 在新 provider 下重新导入 Key（`key_id` 必须与旧的不同，见 §8.2.1）→ 停用旧 provider。**不做 Key 迁移，理由见 §8.1 ②——旧 provider 的历史用量按旧维度留在归档里是正确的，迁移改写它才是账目失真 |
| `created_at` | 事实字段 |
| 物理删除 | 见 §1.3。**API 无 DELETE 端点**，这本身就是约束 |

`name` 的格式校验（新建时）：`^[a-z][a-z0-9_]{1,31}$`，且不得与已存在的 provider 名重复（**包括已停用的**——停用的 provider 仍持有历史配额 key 与归档记录，复用其名会把新旧数据混在一起）。DB 层有 CHECK 约束兜底（§1.2），但 API 层必须先校验并给出人类可读的错误。

### 6.2 有流量后禁改

| 字段 | 理由 |
|---|---|
| `quota_kind` | **本清单里最危险的一项。**它决定两件事：Redis key 的 `{kind}` 段，以及归档时分子分母的口径。`count` 改 `token` 后，Redis 里按次累加的计数（如 1400 里已用 800 次）会被当作 token 数解释（800 tokens / 5000000 上限 = 0.016%），水位瞬间显示为几乎空闲，调度器把这些实际已耗尽的 Key 排到最优先——**这就是阶段一缺陷 #11 的复现路径，且全程无任何错误日志**。Redis 中的计数不会换算，也无法换算（次数与 token 数没有确定的转换关系）。<br>**规则**：无流量可自由改；有流量返回 **409 + `code: immutable_field`**（dry-run 的 `errors[].code` 用更具体的 `immutable_with_traffic`），message 必须说清后果。正确做法与改名相同：新建一个 provider |
| `adapter_kind` | adapter 决定请求/响应的改写方式。运行中切换会让在途请求的响应用错的 adapter 解析。有流量时禁改；无流量可改 |

### 6.3 可改但强告警

| 字段 | 告警码 | 告警内容 |
|---|---|---|
| `base_url` | `base_url_changed` | 在途请求（含重试）仍走旧 URL 直至完成——这是设计如此（请求内快照），不是 bug。新 URL 从下一个请求起生效 |
| `quota_limit` 上调 | `quota_limit_increased` | Redis 中已累计的用量不变，水位按新上限重算，可能让此前被判定为耗尽的 Key 立即重新进入派发。若上游实际额度并未提升，会导致超发 |
| `quota_limit` 下调 | `quota_limit_decreased` | 当前用量可能已超过新上限，这些 Key 会立即退出派发。若下调幅度大，可能导致可用 Key 数骤降为 0 —— dry-run 应估算"按当前用量，新上限下有几个 Key 会立即退出" |
| `model_mapping` 移除条目 | `model_removed` | 被移除的公开模型名将返回 404。dry-run 需查近 24h `usage_records` 是否有该模型的调用量并在告警中给出次数 |
| `model_mapping` 改上游名 | `model_upstream_changed` | 在途请求用旧映射，新请求用新映射。**若新上游模型名不存在于上游，所有请求会失败** —— 网关无法预先验证，这是告警而非阻断 |
| `count_models` 变更 | `count_models_changed` | 该列表决定单次请求走哪种配额口径（`IsCountModel`，config.go:1028 → `quotaKindFor`，server.go:570）。把一个模型移出/移入会改变其扣减方式，历史数据不会追溯换算 |
| `reasoning_models` 变更 | `reasoning_models_changed` | 影响预估用量（`reasoningEstimateFor`，server.go:580），只影响预留额度不影响最终结算，风险最低 |
| `quota_window` | `quota_window_changed` | 影响配额 key 的 `{day}` 分桶周期。改动后当前窗口内的计数不会重新分桶 |
| `refresh_hour` | `refresh_hour_changed` | 影响刷新时刻。改成已过去的小时数会让本日不再刷新 |
| `credential_env` | `credential_env_changed` | 若新变量名在环境中不存在，**dry-run 直接报 error 而非 warning**（见 §6.4） |
| `default_provider` 指向被停用的 provider | — | **error，阻断** |

### 6.4 提交前的前置检查清单（dry-run 必须全跑）

按顺序执行，全部通过才 `ok: true`。**一次返回全部失败项**（参照 `admin.go` 批量导入的 `failures []failure` 风格），不要遇到第一个就返回。

1. `expected_version` 与当前 `active_version` 一致（不一致 → 409，且后续检查不必再跑）
2. `reason` 非空且 ≤ 500 字符
3. `name` 格式合法、未与任何已存在（含停用）provider 重名（仅 create）
4. 请求体中不含 §6.1 的绝对禁改字段
5. `quota_kind` / `adapter_kind` 变更时，查该 provider 是否有流量（§6 开头的三重判定）
6. `quota_kind ∈ {token, count}`、`quota_limit > 0`、`quota_window` 能被 `time.ParseDuration` 解析、`refresh_hour ∈ [0,23] ∪ {null}`
7. `base_url` 是合法 URL 且 scheme 为 `https`（内网 http 上游若确有需要，走 YAML 白名单，不在此处开口子）
8. `model_mapping` 的 key 与 value 均非空字符串；`count_models` / `reasoning_models` 中的每一项都必须出现在 `model_mapping` 的 key 集合中 —— **不在映射里的模型名永远不会被匹配到，是个静默的空配置**
9. `credential_env` 非空且 `os.Getenv(credential_env) != ""`。变量不存在 → **error**：一个没有凭据的 enabled provider 会让所有派往它的请求 401，而运维只会看到"配置保存成功"
9b. **`credential_env` 疑似密钥原文 → error，阻断提交**（长度 > 40，或含小写字母 —— 环境变量名惯例为大写加下划线）。这是 designer 与我共同确认为 error 而非 warning 的一项：密钥一旦进入 `provider_config_versions` 就无法收回（历史只读，且 `audit_logs` 二次留存），不可逆错误不该由一次勾选把关。误拒一个形态奇怪的合法变量名，代价只是改名重提一次。<br>**API 不提供"我知道风险，继续提交"的旁路。**<br>**message 里不要写出判据**（不写"长度不能超过 40"或"不能含小写"）—— 讲清判据会引导人去规避判据。给正确形态的例子比给判据有用，例如：`该字段填环境变量名而非密钥值，如 SENSENOVA_API_KEY`。
10. `adapter_kind` 在已支持列表内（§7.4）
11. 停用操作：**不得停用 `default_provider`**（409 + `invalid_state_transition`，提示"请先用 `PATCH /admin/config/default-provider` 把默认切到其他 provider"）；不得让 enabled provider 数降为 0
11b. **设为默认操作**：目标 provider 必须存在且 `enabled`。界面已不渲染停用行的「设为默认」按钮，但**服务端必须独立判定** —— 界面约束与服务端约束各自成立，不互为前提。违反返回 409 + `invalid_state_transition`
12. 启用操作：`enabled: true` 且该 provider 无可用上游 Key → warning（非阻断，允许先建配置后导 Key）
13. **跑完整的 `config.Validate()`**（config.go:897）—— 含 `validatePoolShareCapacity`、水位比例校验等既有规则。这一步兜住所有本清单没显式列出的既有约束
14. **跑 `confsnap.Build`**（含 adapter registry 构造）—— 兜住"provider 名/adapter_kind 组合会导致 `Register` panic"这类问题。这一步必须在 dry-run 里跑，否则 panic 会发生在真实提交时把进程带崩

第 13、14 步是**复用而非重写**：dry-run 与真实生效走同一条构造路径，是"dry-run 结果可信"的唯一保证。

---

## 7. 改造清单（后端施工单）

标注约定：**[新]** = 新增文件/函数，**[改]** = 修改现有代码，**[不改]** = 明确不要动（防止过度改造）。

建议按 7.1 → 7.9 顺序施工，每步可独立编译通过。

### 7.1 [新] `internal/store/schema.sql` 追加 DDL

追加 §1.2 的三段 DDL 到文件末尾的"增量迁移"分节之后。全部幂等，由既有 `auto_migrate`（`store.go:99`）执行，**不需要新的迁移机制**。

[不改] 现有 8 张表的任何定义。本期不 `ALTER` 任何既有表，`audit_logs` 原样复用。

### 7.2 [新] `internal/store/provider_config.go`

新文件，仿 `internal/store/history.go` 的风格（同步写、显式错误、`ErrNotFound` 复用）。需要的方法：

```go
type ProviderRow struct { /* 对应 provider_configs 全部列 */ }
type ConfigVersionRow struct { /* 对应 provider_config_versions 全部列 */ }

// 读
func (s *Store) ListProviderConfigs(ctx context.Context) ([]ProviderRow, error)
func (s *Store) GetProviderConfig(ctx context.Context, name string) (ProviderRow, error) // 无则 ErrNotFound
func (s *Store) ActiveConfigVersion(ctx context.Context) (int64, error)
func (s *Store) CountProviderConfigs(ctx context.Context) (int, error)                   // seed 判定
func (s *Store) ListConfigVersions(ctx context.Context, limit int, before int64) ([]ConfigVersionRow, bool, error)
func (s *Store) GetConfigVersion(ctx context.Context, version int64) (ConfigVersionRow, error)

// 写（内部开事务，实现 §1.4 的三件事）
type ApplyConfigInput struct {
    Action        string           // create/update/enable/disable/rollback/seed
    Target        string
    Rows          []ProviderRow    // 变更后的全量 provider 集合
    Snapshot      []byte           // JSONB
    ChangedFields []string
    Reason, Actor string
    RolledBackFrom *int64
    ExpectedVersion *int64         // nil 表示跳过乐观锁（仅 seed）
}
// 返回新版本号。ExpectedVersion 不匹配返回 ErrVersionConflict。
func (s *Store) ApplyProviderConfig(ctx context.Context, in ApplyConfigInput) (int64, error)

// 流量判定（§6 的三重判定中的 PG 两项）
func (s *Store) ProviderHasTraffic(ctx context.Context, name string) (bool, error)
```

[新] `var ErrVersionConflict = errors.New(...)`，与既有 `ErrNotFound` / `ErrKeyRevoked` 同风格，调用方用 `errors.Is` 判断。

### 7.3 [新] `internal/confsnap/` 包（snapshot.go + holder.go + builder.go）

按 §2.2 的签名实现。三条硬约束写进包注释：

1. `Snapshot` 发布后一律只读，禁止改写 `Cfg.Providers`
2. `Build` 失败绝不触碰 `Holder`
3. `Build` 内不得触发 `adapter.Registry.Register` 的 panic 路径（先校验后注册）

依赖方向 `confsnap → {config, adapter, store}`。**验证过 `internal/adapter` 不 import `internal/config`**，故此组合不产生循环依赖。

### 7.4 [新] adapter 工厂：消灭硬编码 switch

当前 `main.go:261-273` 与 `server.go:192-206` 两处都是 `switch name { case "volc": ... case "sensenova": ... default: return error }`。**这是热加载的硬阻塞**：通过 API 新增一个 provider，两处 switch 都不认识它，热加载失败，且重启后启动也失败。

[新] `internal/adapter/factory.go`：

```go
// Kind 是 adapter 的行为类型，与 provider 名解耦。
// provider 名可以任意新增，只要 adapter_kind 落在已支持集合内。
const (
    KindOpenAICompatible = "openai_compatible" // volc 及大多数上游
    KindSenseNova        = "sensenova"          // 需要特殊处理的
)

func SupportedKinds() []string
func New(kind, providerName string, mapping map[string]string) (Adapter, error) // 未知 kind 返回 error，不 panic
```

`provider_configs.adapter_kind` 提供这个 kind。迁移期 seed 时按名映射（`volc` → `openai_compatible`，`sensenova` → `sensenova`）以保持行为完全不变。

[改] `internal/adapter/registry.go`：**签名不改、panic 行为不改**（`Register` 的 panic 是启动期的正确行为）。仅新增一个不 panic 的构造入口：

```go
// BuildRegistry 从 kind/mapping 集合构造一个全新 Registry。
// 全部校验前置，任何问题返回 error 而非 panic。供热加载路径使用。
func BuildRegistry(specs []Spec) (*Registry, error)
```

[不改] `registry.go:10` 的"不支持运行时动态注册"注释所描述的行为 —— 我们不动态注册，而是每次构造全新实例。注释可补一句说明 `BuildRegistry` 的用法。

### 7.5 [改] `cmd/gateway/main.go`

| 位置 | 改动 |
|---|---|
| `main.go:149-152` 之后 | [新] seed 逻辑：`CountProviderConfigs` 为 0 时按 §3.5 从 YAML（或 `config.Default()`）seed，`action='seed'` |
| `main.go:261-273` | [改] 删除硬编码 switch，改为从 DB 读 `ListProviderConfigs` → `confsnap.NewBuilder(cfg).Build(version, rows)` 得到初始 Snapshot → `confsnap.NewHolder(snap)` |
| `main.go:274-286` | [改] `gateway.Deps` 传 `Snaps *confsnap.Holder` 替代/追加于 `Config` |
| `main.go:253-256` | [改] `bgDeps` 传 `Snaps` |
| `main.go:489-511`（`newProbe`） | [改] 签名收 `Holder`；**修掉 TODO**：按 `key.Provider` 从 `snap.Adapters.Get()` 取 adapter，不再取"第一个 provider 的 mapping"（C2） |
| `main.go:83-84`、`main.go:448` | [不改] 启动日志与 `EgressVerifyTarget`，冷区 |

### 7.6 [改] `internal/gateway/`

| 文件:行 | 改动 |
|---|---|
| `server.go:33-48`（`Deps`）、`51-76`（`Server`） | [改] 新增 `snaps *confsnap.Holder` 字段。**`cfg *config.Config` 保留** —— 冷配置（Admin/Upstream/Server）仍从它读，删掉会牵动大量无关代码 |
| `server.go:192-206` | [改] registry 自动构造分支改走 `confsnap`；`Deps.Snaps` 已传入时直接用 |
| `server.go:265` 后 | [新] §4.1 的 10 条路由 |
| `server.go:429`（`/readyz`） | [改] 增加 `config_version` 与 `config_lag` |
| `server.go:570`、`580` | [改] `quotaKindFor` / `reasoningEstimateFor` 增加 `snap` 入参（A8、A9） |
| `server.go:438-440` | **[不改]** 启动日志，D1 误判，冷区 |
| `handlers.go:71` | [改] 取快照的唯一位置（A1） |
| `handlers.go:90-93`、`292-299` | [改] A2、A3（A3 另加 enabled 过滤） |
| `proxy.go:53-70`（`requestPlan`） | [改] 新增 `Snap *confsnap.Snapshot` 字段 |
| `proxy.go:75-83`（`execute`） | [改] 循环外持有快照，**循环内严禁重新取**（A7 注释） |
| `proxy.go:307`、`330`、`617` | [改] A4、A5、A6 |
| `admin.go:287`、`302` | [改] A10、A11（同一批次复用同一 snap，并拒绝向停用 provider 导 Key） |
| [新] `internal/gateway/adminapi/admin_provider.go` | §4 全部 handler（**11 个**，含 `PATCH /admin/config/default-provider`）。**与 `adminapi/admin.go` 分文件**，合进去会突破单文件可维护规模。<br>**错误码务必按 §4.1.1 细化**：三种 409 分别用 `version_conflict` / `immutable_field` / `quota_kind_mismatch` / `invalid_state_transition`，不要图省事全填 `invalid_request`（现有代码的泛化用法不是本期的标准） |

### 7.7 [改] `cmd/gateway/background.go` 与 `adapters.go`

| 位置 | 改动 |
|---|---|
| `background.go:31-46`（`bgDeps`/`background`） | [改] `cfg` → `snaps *confsnap.Holder`；冷配置仍需 `cfg` 则两者并存 |
| `background.go:192` | [改] B1，**遍历含 disabled 的全部 provider**（租约泄漏） |
| `background.go:401`、`408`、`416` | [改] B2-B4，三处共用同一 tick 快照 |
| `background.go:483-484` | [改] B5 |
| `background.go:457-518`（`startRefresher`） | [改] **C1，refresher reconcile**（最复杂的一处，见 §2.5）。建议抽出 `refresherSet` 结构，持 `map[string]context.CancelFunc` + `map[string]*quota.Refresher`（后者用于 `UpdateLimits`）。**三种动作都要实现：起 / 停 / 就地更新 limits**，只做前两种会留下"每天中午把额度改回旧值"的静默失效 |
| `internal/quota/refresh.go` | [改] 新增 `func (r *Refresher) UpdateLimits(token, count Limits)`，带 `r.mu` 写锁。这是本期唯一需要动 `internal/quota` 的地方 —— 该包不 import config，也不应该 import，limits 由 `background` 从快照算好后推给它 |
| DB→`config.Config` 的桥（`confsnap` 装配侧） | [改] **必须排除 `enabled: false` 的 provider**，让"停用"在快照层等价于"不存在"（§2.5.2）。`config.Provider` 无 `Enabled` 字段，下游 148 处读取点无法自行判断；在此过滤一次，`reconcileRefreshers` 的键集合比对、`ResolveProvider`、`/v1/models`、调度、归档全部自动正确。**连带收益**：避免一个"已停用且 adapter_kind 不受支持"的历史行让每次 `confsnap.Build` 都失败（`snapshot.go:82-92` 对未知 provider 名返回 error） |
| `reconcileRefreshers`（`background.go:496`） | [改] 除三个动作外，补 `rc` 分叉 WARN（§2.5.4）：`refreshers` map 的 value 除 `CancelFunc` 外须存该实例构造时的 `rc`，跳过存活实例前与当次 `rc` 比对，不一致打 WARN |
| `internal/confsnap/snapshot.go` | [改] 导出 `func SupportedProviders() []string`，**由 `newAdapterFor` 的 `switch` 生成，不另列字面量**（§3.2.1）。这是 provider 名合法集合的唯一真相来源，加新 adapter 时只改这一处 |
| `cmd/gateway/provider_seed.go:75` | [改] seed 遇到 `name` 不在 `SupportedProviders()` 内时**跳过并打 WARN**，不写入库。当前会原样写入（`AdapterKind: name`），产生一行永久无法启用也无法删除的坏数据。已存在的坏行不追溯清理 |
| `background.go:351` | [不改] `EgressVerifyTarget`，冷区 |
| [新] `background.go` 新 tick | 10s 轮询 `ActiveConfigVersion` 比对本地版本，不等则重载（§4.9） |
| `adapters.go:43-54`（`quotaLimits`） | [改] B6，持有 `Holder` 替代 `*config.Config` |

### 7.6.1 [改] `internal/gateway/adminapi/admin.go` —— Key 导入的跨 provider 前置检查

| 位置 | 改动 |
|---|---|
| `POST /admin/keys/import` handler（`admin.go:377` 附近） | [改] 导入每把 Key 前按 `key_id` 查现存行；若存在且 `provider` 与本次目标不同 → **409 + `immutable_field`**，message 写清"该 key_id 已属于 provider X"（§8.2.1）。**判据要写清且必须给全两条出路**（运维绕不过，照 §4.1.1 通则及其推论）。<br>**注意不要写成"key_id 已存在就拒绝"** —— 相同 provider 的重复导入是既有的幂等行为，必须保留 |
| `errorEnvelope`（`server.go:522-528`） | [不改] **禁止为本期需求给它加业务字段。**它是 OpenAI 风格错误体，被数据面 `/v1/chat/completions` 共用（`handlers.go:37,48,57,65,80,133,213` 等 9 处、`admin.go` 20 处、`server.go` 6 处）。加字段会改变数据面错误响应形状，影响所有上游 SDK 客户端 —— 代价与收益完全不成比例。<br>**冲突的 key_id 与其现属 provider 直接写进 `message` 文本**（"`volc_001` 已存在，当前挂在 provider `volc` 下"）。界面按整段文本渲染，不解析结构化字段（§4.1.1 的答复） |
| `internal/store/upstreamkeys.go:90,96` | [不改] `ON CONFLICT (key_id) DO UPDATE` + `provider = EXCLUDED.provider` 保持原样。`DO UPDATE` 表达不了"冲突即拒绝"，改造需触发器或先查后写，超出本期范围；本期在 handler 层堵（§8.2.1 裁决二） |
| `deploy/init.sql:19` | [不改] 其 `uq_provider_key` 复合唯一与 `schema.sql:38` 的单列唯一冲突，**以 `schema.sql` 为准**（§8.2.1 裁决一）。本期不动这份过期副本，仅记录 |
| `internal/store/schema.sql` 的 `upstream_keys` 约束 | [不改] **禁止改成 `(provider, key_id)` 复合唯一。**该方案已撤回：与 `ON CONFLICT (key_id)` 不兼容，且 `ADD CONSTRAINT` 遇既存重复行会让 `auto_migrate` 在启动钩子里失败、网关起不来（§8.2.1 已撤回小节） |
| [新] `docs/` 下的排查脚本（非 `schema.sql`） | [新] §8.2.1 那条**只读**排查 SQL，交付运维手工执行。**不进 `Migrate`**、不作为发布门禁 |

### 7.8 [改] `internal/scheduler/scheduler.go`

| 位置 | 改动 |
|---|---|
| `scheduler.go:150-151` | [改] 两个值拷贝字段替换为 `snaps *confsnap.Holder` |
| `scheduler.go:177`（`New`） | [改] 签名相应调整 |
| `scheduler.go:227`（`Reload`） | [改] 开头取一次快照 |
| **`scheduler.go:240`** | [改] **D5 缺陷修复**：`s.quotaC.TokenHard()/TokenSoft()` → 按每 Key 的 provider 求 `LimitsFor(key.Provider, IsCountProvider(key.Provider))` |

[不改] `internal/quota` 全包 —— 已验证不 import config。

### 7.9 [改] `internal/config/config.go` 与 dashboard

| 位置 | 改动 |
|---|---|
| `config.go:315-332`（`Provider`） | [改] 新增 `Enabled bool`、`AdapterKind string`、`CredentialEnv string` 三个字段（带 yaml tag，保持 `KnownFields(true)` 严格模式下 YAML 仍可解析） |
| `config.go:698`（`Load`） | [不改] 整体替换而非合并的行为保留；`providers` 节语义降级为 seed 源（§3.4），代码不变、注释更新 |
| `config.go:897`（`Validate`） | [改] 补充 §6.4 中第 6/7/8 项的字段级校验（若尚未覆盖）。**其余既有规则一律不动** |
| 热区方法（47/205/231/1028/1047/1072/1087） | [不改] 方法体不动。它们仍在 `*Config` 上，只是调用方改为从 snapshot 取 `Cfg` |
| `dashboard/app/api/admin_proxy.py` | [改] 新增 §4 的 **11 条**转发（现只有 3 条写转发：`admin_proxy.py:60/67/74`）。转发时透传 `X-Admin-Actor`。**必须按 `error.code` 而非状态码分派文案** —— 三种 409 的运维处置动作各不相同（§4.1.1）。**永不发送 `confirm_quota_kind_change: true`**（§4.8） |

### 7.10 验证步骤（施工完成后端到端跑一遍）

1. `go build ./...` + `go vet ./...`
2. `go test ./internal/config/... ./internal/confsnap/... ./internal/scheduler/...`
3. **空库首次启动**：清空 `provider_configs`，用现有生产配置启动（`config.prod.yaml` 原在 real_upstream_test/，已于 2026-09-13 移除且本仓内无副本；重跑本步需先按 `deploy/README.md` 重建该配置），确认 seed 出 `sensenova` 且版本号为 1，`GET /admin/providers` 返回内容与 YAML 完全一致
4. **热加载生效**：`PATCH` 改 `quota_limit`，不重启，确认 `/admin/providers` 的 `version` 与 `loaded_version` 同步推进，且**调度打分使用了新上限**（这一步专门验 D5，`quota_limit` 改了但调度不认是最容易残留的缺陷）
5. **model_mapping 热加载**：改一条 mapping，确认新请求的上游 model 名按新映射改写（这一步专门验 D4；若 adapter 没跟着重建，此处会静默用旧映射，是本次最难发现的失效）
6. **请求内一致性**：在 `attempt` 之间人为触发一次热加载（可临时加日志或用 debugger），确认同一请求的多次重试用的是同一 base_url + 同一 adapter
7. **禁改校验**：对有流量的 provider 尝试改 `quota_kind`，确认返回 **409 + `code: immutable_field`**（不是 422，见 §4.1.1）且 message 说清后果
8. **乐观锁**：两个请求带同一 `expected_version` 并发 PATCH，确认一个 201/200、一个 409
9. **回滚**：回滚到一个 `quota_kind` 不同的版本，确认不带 `confirm_quota_kind_change` 时返回 **409 + `code: quota_kind_mismatch`**；并确认 dashboard 从未发送该字段为 true（§4.8 的默认拒绝语义）
10. **refresher reconcile**（三个动作分别验，见 §2.5.2）：
    - **起**：新增并启用一个 provider，确认日志出现 `refresher reconcile: started=[新provider]`
    - **停**：停用它（`enabled: false`，注意此时它**仍在 `provider_configs` 表里**），确认日志出现 `stopped=[...]`，且不再有对该 provider 的探测请求。**这条要做反向验证**（§2.5.2）：先故意让桥把停用项也放进 `cfg.Providers`，确认此时停用后仍能观测到探测日志（真实请求、真实烧上游配额），再改成桥层过滤。只验正向的话两种实现都"看起来能停"，因为测试往往只覆盖了物理删除的情形
    - **停用项不进快照**：停用一个 provider 后，确认 `GET /v1/models` 不再列出它的模型、`ResolveProvider` 不再解析到它、调度不再选它的 Key。这几项应当**自动成立**（桥层已过滤），若需要在这些位置各自加 `enabled` 判断，说明过滤放错了层——那等于新增 148 处漏判机会（§2.5.2）
    - **rc 分叉可见**：改 `refresh.window_start` 后新增一个 provider，确认出现 §2.5.4 那条 WARN。这是该已知缺陷唯一的可观测出口，缺了它"部分生效"在运行期完全不可见
    - **就地更新**：改一个存量 provider 的 `quota_limit`，确认日志出现 `limits_updated=[...]`，且 Refresher **未被重建**（状态机未清空）。这一条对应"每天中午把额度改回旧值"的失效，是三者里最容易漏的
    - 三者失败都完全静默，必须靠日志验证；另核对 `/readyz` 的 refresher 数量与**快照中的 provider 数量**相等（停用项已在桥层排除，所以快照里的就是启用的，不需要再筛一次 —— 见 §2.5.2）
11. **审计**：上述每次写操作后查 `audit_logs`，确认 `actor`/`action`/`target`/`detail.version` 齐全，且能凭 `detail.version` 在 `provider_config_versions` 找到对应快照
12. **默认 provider 闭环**（专门验 designer 指出的坏状态）：设 A 为默认 → 尝试停用 A，确认 409 + `invalid_state_transition` → 把默认切到 B → 再停用 A，确认成功。**再单独验一次服务端独立防线**：直接 curl 把一个已停用的 provider 设为默认（绕过界面），确认 409
13. **错误码可分派**：确认 `version_conflict` / `immutable_field` / `quota_kind_mismatch` / `invalid_state_transition` 四种 409 的 `error.code` 各不相同 —— dashboard 靠它分派文案，若都填 `invalid_request`，界面只能给模糊提示（§4.1.1）
14. **密钥原文拦截**：`credential_env` 填一个长度 > 40 的含小写字符串，确认返回 error 且**拒绝文案里没有出现判据**（不出现"40"、不出现"小写"），只给正确形态示例
15. **不受支持的 provider 名**（§3.2.1）：
    - `POST /admin/providers` 用 `name: "openai"` 提交，确认 **400 + `unsupported_provider`**，且拒绝文案**写出了**支持列表与"需先实现 adapter"这层原因（这条与步骤 14 相反：那条不写判据是防规避，这条必须写清，因为运维无法自行绕过，含糊只会让他反复试）
    - 确认拒绝发生在**创建时**，而不是等到 `confsnap.Build` 失败 —— 后者会让这次热加载整体失败并牵连其他 provider
    - `SupportedProviders()` 必须由 `newAdapterFor` 的 `switch` 导出。**反向验证**：给 `newAdapterFor` 加一个 `case`，确认新名字**不改 handler 代码**就能通过校验；若还要改别处，说明有人抄了一份字面量
    - **seed 侧**：在 YAML 里放一个不受支持的 provider 名，空库启动，确认它被**跳过并打 WARN**，而不是写入库变成永久坏行
    - **不可启用标记**：手工往库里插一行 `name` 不受支持且 `enabled: false` 的记录（模拟历史遗留），确认列表把它标为"不可启用"且不渲染启用按钮
    - **`supported_providers` 随列表返回且空状态可用**（§4.2）：清空 `provider_configs` 后调 `GET /admin/providers`，确认 `providers` 为空数组但 `supported_providers` 仍返回完整取值 —— 第一次新建正好发生在列表为空时，此刻选择器必须已有选项
16. **key_id 跨 provider 改写拦截**（§8.2.1，这条验的是既存暗路）：
    - 先导入一把 Key（provider=volc, key_id=K1），再用同一个 `key_id: K1` 但 `provider: sensenova` 调 `POST /admin/keys/import`，确认返回 **409 + `immutable_field`**，且 message 写清"该 key_id 已属于 volc"
    - **反向验证**：临时去掉该前置检查，确认此时导入会**静默成功**并把 `upstream_keys.provider` 改写成 sensenova（`upstreamkeys.go:96`），且 Redis 配额 key 前缀与归档维度都没跟着变 —— 确认这条暗路真实存在，再把检查加回去。只验正向的话，无法区分"检查生效"与"本来就不会发生"
    - 确认幂等导入未被破坏：同一把 Key 用**相同** provider 重复导入仍正常更新（不能把这条检查写成"key_id 已存在就拒绝"）

---

## 8. 遗留问题与后续扩展

本节分两类：**8.1 有意不做**（已裁决，不是缺失功能，不要"补全"）与 **8.2 已知约束**（接受的代价）。

### 8.1 有意不做的功能（team-lead 已裁决）

这几项都不是遗漏。写在这里是为了防止后来者当成缺失功能补上——与 §4.8 "不提供部分回滚"是同样的处理方式。

**① 水位系数的 per-provider override —— 不做，只留 DDL 口子。**

当前所有 provider 共用 `token_hard_ratio` / `count_hard_ratio` 这套全局系数，没有实际需求驱动差异化。判据是：水位系数属于**调优参数**，不属于 provider 契约——它不进 Redis key 结构、不进归档维度、改错了也只影响准入激进程度而不改变数据口径。本期核心目标是"配置能在页面改且热生效"，把调优参数一并搬进来会扩大表结构和校验面而没有对应收益。

留的口子：`provider_configs` 表将来加 4 个可空列（`token_hard_ratio` 等），`NULL` 表示继承全局。快照构造时 `COALESCE(provider.ratio, global.ratio)`。**本期这 4 列不建**——建了不用的列会诱导实现者去填它。

**② Key 的 provider 迁移端点 —— 不做，替代路径是重新导入。**

表面上"改 provider 名的正确做法是新建 + 迁 Key + 停用旧的"，其中"迁 Key"缺一个端点。但这个判断本身不成立：

- 迁移涉及跨两个存储的原子性：Redis 配额 key 要整体换命名空间（`{provider}:quota:{kind}:{key_id}:{day}`），而 `usage_records` / `key_daily_history` 里的历史行仍带旧 provider 维度。迁移中途失败会留下一半在新命名空间、一半在旧的状态，且**不报错**——这和跨量纲回滚是同一类问题（§4.8），代价远超收益
- 更关键的是**这个操作的正确形态本来就不是"迁移"**。upstream_keys 存的是上游厂商发的凭据，一把 volc 的 Key 在语义上不可能"变成" sensenova 的 Key。真实场景永远是"在新 provider 下重新导入一批 Key"，而 `POST /admin/keys/import` 已经支持

所以这不是缺功能，是路径不同。历史用量按旧 provider 维度留在归档里是**正确的**，不该被迁移改写——那才是账目失真。

**但这条替代路径有个陷阱，必须一起交付**（team-lead 判为本轮最高优先级的文案问题）：

`§5.1`/`§5.2`/`§6.1`/`§7.4` 与界面 §8.5 都把运维指向"新建 provider + 重新导入"，这是禁改 `name`/`quota_kind` 的唯一出路。而运维照做时最自然的动作，就是**拿原来那批 Key 原样再导一遍** —— 那批 Key 的 `key_id` 通常不变，于是每一把都走进 §8.2.1 那条静默改写：provider 列被覆盖、Redis 配额前缀与归档维度不跟着变。

也就是说：**我们为了避免隐式迁移而不做迁移端点，却在文档里把运维引到了唯一那条隐式迁移路径上。**

§7.6.1 的 handler 检查会拦住它（409），这是好的 —— 但拦住之后运维会卡住：他照文档做、被拒绝、而文档没告诉他下一步。所以引导文案必须**同时**说清两件事，缺一条都不成立：

1. **新导入的 `key_id` 必须与旧的不同**（不是"建议不同"）
2. **为什么**：旧 Key 要带着它的历史用量留在旧 provider 下停用。这不是限制，是上面那条"历史按旧维度留存才正确"的直接推论 —— 复用 `key_id` 等于要求同一个身份同时属于两个 provider，而它的归档、配额命名空间、水位体系都是按 provider 分的

这是硬要求，不是建议补充。只写第 1 条会让它看起来像个任意规定，运维会想办法绕（比如先删旧 Key 再导 —— 那会连历史一起弄丢，比原缺陷更糟）。

### 8.2 已知约束（接受的代价）

| 约束 | 影响 | 判断 |
|---|---|---|
| 多实例 10s 收敛窗口 | 期间不同实例配额口径可能不一致 | 实例数个位数的运维工具场景可接受。若未来不可接受，走 pub/sub + 轮询兜底，而不是缩短轮询间隔 |
| `adapter_kind` 的扩展需发版 | 新增一类 adapter 仍要改代码 | 这是合理的边界：adapter 是代码逻辑不是配置。API 只保证"新增 provider 名"不需要发版 |
| `deploy/init.sql` 是过期副本 | 其 `upstream_keys` 定义与实际在跑的 `internal/store/schema.sql` 在**唯一性约束、列类型、可空性**三处不一致（`init.sql:12,19` vs `schema.sql:38,39`）。照它建库会让 `POST /admin/keys/import` 直接报约束不匹配 | **以 `schema.sql` 为准**，本期不动 `init.sql`（只被两个测试脚本引用，改它等于给未验证路径改行为）。详见 §8.2.1，建议后续单独一轮收敛 |
| `key_id` 可被跨 provider 静默改写 | `ON CONFLICT (key_id)` + `provider = EXCLUDED.provider`（`upstreamkeys.go:90,96`）使同名 key_id 换 provider 导入时静默改写归属，而 Redis 配额前缀与归档维度不跟着变 | **本期在 handler 层堵**（§7.6.1 前置检查 → 409），不改 SQL —— `DO UPDATE` 表达不了"冲突即拒绝"。详见 §8.2.1 裁决二 |
| 上述缺陷的**历史残留**无法被 handler 检查追溯 | 该缺陷是既存代码、已在生产跑过一段时间，线上可能已有被改写过归属的 Key：当日用量落新 provider 命名空间、历史留在旧的，账目分裂且当时无日志。加检查后这些行仍是错的 | **交付一条只读排查 SQL 给运维**（§8.2.1，用 `key_daily_history` 三列维度反查同一 key_id 出现过多个 provider 的行）。**不进 `Migrate`、不阻塞上线** —— 怎么处置取决于业务口径（哪段用量算哪个 provider 的账），不能由代码替运维决定 |
| 全局 `refresh.*` 配置**部分生效**（不是"不生效"） | 改 `window_start` / `window_end` / `probe_interval` / `post_refresh_ramp_minutes` 后：**已存活的 refresher 保留旧值，此后新建的 refresher 用新值**，两者可能跑在不同探测窗口上。必须**重启网关**才能让全部 refresher 对齐 | 符合用户决策 2（只热加载 provider 相关项），且这几项参与 Refresher 状态机判定，热改要处理"窗口中途变更"的中间态，不划算。**但"不生效"的表述是错的**——`rc` 每次 reconcile 都重算（`background.go:506-511`），只是不作用于存活实例。完整分析与强制要求的 WARN 日志见 §2.5.4 |

### 8.2.1 `upstream_keys` 唯一性口径分歧（designer 提出，架构裁决）

designer 报告的两条事实**核实全部为真**，且两者的交互比他描述的更糟：

| 事实 | 位置 | 内容 |
|---|---|---|
| 两份 schema 相反 | `internal/store/schema.sql:38` vs `deploy/init.sql:19` | 前者 `key_id TEXT NOT NULL UNIQUE`（单列唯一），后者 `CONSTRAINT uq_provider_key UNIQUE (provider, key_id)`（复合唯一） |
| upsert 无条件覆盖 provider | `internal/store/upstreamkeys.go:90,96` | 冲突目标是 `ON CONFLICT (key_id)` 单列，命中后 `provider = EXCLUDED.provider` 无 CASE 守卫 |
| 列类型也不一致 | `schema.sql:39` vs `init.sql:12` | `secret_enc TEXT NOT NULL` vs `secret_enc BYTEA`（可空）。同一列在两份脚本里类型与可空性都不同 |

**裁决一：`internal/store/schema.sql` 是唯一真相来源，`deploy/init.sql` 的复合唯一约束作废。**

判据不是"哪个设计更好"，而是**哪个实际在跑**。`schema.sql` 经 `//go:embed`（`store.go:26`）+ 启动 `auto_migrate`（`store.go:99`）执行，是网关自己维护的 schema；`init.sql` 现已无任何消费者（原先只被两个遗留脚本手工调用，二者已于 2026-09-13 移除）。而 `upstreamkeys.go:90` 的 `ON CONFLICT (key_id)` 只在单列唯一下成立 —— 若按 `init.sql` 的复合唯一建表，这条 upsert 会直接报 `there is no unique or exclusion constraint matching the ON CONFLICT specification`，导入功能整体不可用。也就是说**生产跑的一定是单列唯一**，否则现有导入早就炸了。`init.sql` 是过期副本，不是候选设计。

#### 已撤回的裁决：把 `upstream_keys` 改为 `(provider, key_id)` 复合唯一

team-lead 曾裁定"以 `key_daily_history` 的三列主键为正确水位，把 `upstream_keys` 改成复合唯一并同步改 `ON CONFLICT` 目标"，**该裁决已由 team-lead 本人撤回**。记录在此是为了防止后来者看到两份 schema 不一致时重走一遍。

撤回理由两条，第二条更严重：

1. **与既有 upsert 不兼容**。如上，`ON CONFLICT (key_id)` 在复合唯一下直接报约束不匹配。要改约束就必须同时改冲突目标，而这属于本期明确不动的 Key 管理链路。
2. **改约束的迁移会在启动期打死生产**。`ALTER TABLE ... ADD CONSTRAINT UNIQUE (provider, key_id)` 若遇到已存在的重复 `(provider, key_id)` 行会**直接失败**，而这条 DDL 会由 `auto_migrate` 在启动钩子里自动执行 —— 结果是 `Migrate` 报错、**网关起不来**。检测 SQL 是给运维看的，`Migrate` 是自动跑的，前者兜不住后者。这类"数据修正放进启动期自动迁移"的形状本身要避免。

顺带纠正一个推理错误（team-lead 自陈）：`key_daily_history` 的三列主键（`schema.sql:120`）**不是"更正确的水位"**。它是**归档表**，天然按 `(upstream_key_id, provider, quota_day)` 分维度，`schema.sql:118-119` 的注释写明了理由 —— 同一把 Key 换上游后两段历史属于不同额度体系，合并会让 `token_ratio` 失去意义。`upstream_keys` 是**实体表**，`key_id` 单列唯一是它的身份定义。两者维度不同，不构成矛盾。用归档表的维度去推实体表的身份，推错了。

**裁决一之补：既存残留的排查（team-lead 要求，本期必须交付）**

handler 层前置检查（裁决二）只能堵住未来，堵不住已经发生的。单列唯一 + 无守卫覆盖是**既存代码**、不是本期引入，所以线上可能已经存在被静默改写过归属的 Key：当日用量落新 provider 命名空间、历史留在旧的。加了检查之后这些行仍然是错的，且从此再没有任何东西提示它们存在。

因此交付**一条只读排查 SQL**，给运维手工执行：

```sql
-- 排查历史上被跨 provider 改写过归属的 Key（只读，不修改任何数据）
-- 判据：同一个 key_id 在归档表里出现过两个及以上不同 provider
SELECT h.upstream_key_id,
       array_agg(DISTINCT h.provider ORDER BY h.provider) AS providers_seen,
       min(h.quota_day) AS first_day,
       max(h.quota_day) AS last_day,
       k.provider       AS current_provider
FROM key_daily_history AS h
LEFT JOIN upstream_keys AS k ON k.key_id = h.upstream_key_id
GROUP BY h.upstream_key_id, k.provider
HAVING count(DISTINCT h.provider) > 1
ORDER BY h.upstream_key_id;
```

这条查得出来，**正因为归档表按三列分维度、改写前后的两份记录都保留了** —— 这是实体表与归档表维度不同带来的好处。`h.upstream_key_id` 与 `upstream_keys.key_id` 都是业务 key_id（`schema.sql:108` 与 `:38` 同为 TEXT），可直接 join；`LEFT JOIN` 是有意的：Key 可能已被删除，但历史仍在，那种行同样需要看到。

**三条硬约束**：

- **只读，且绝不进 `Migrate`**。这些行怎么处置取决于业务口径（哪一段用量算哪个 provider 的账），不能由代码替运维决定。进了自动迁移就变成启动期跑数据修正，正是上面刚撤回的那类错误。
- 结果不为空**不阻塞本期上线**。它是历史残留清单，不是本期引入的缺陷，也不是发布门禁。
- 放在运维文档或 `docs/` 下的排查脚本里，**不放进 `schema.sql`**（那份文件只放幂等 DDL）。

配套说明落 §8.2 已知约束表。

**裁决二：`provider = EXCLUDED.provider` 这条无条件覆盖，本期不改，但必须在界面上堵住。**

单列唯一 + 无条件覆盖的组合后果：用同一个 `key_id` 在另一个 provider 名下导入，不会报冲突，而是**把这把 Key 的 provider 列静默改写**。这正是 §8.1 ② "Key 的 provider 迁移不做端点"想避免的那件事 —— 它没有端点，却有一条从"重新导入"意外走进去的暗路。且 Redis 配额 key（`{provider}:quota:{kind}:{key_id}:{day}`）与归档维度（`background.go:406`）都不会跟着改，改写后该 Key 的当日用量落在新 provider 命名空间、历史留在旧的，账目从此分裂，全程无日志。

不在本期改 SQL 的理由：改成 `provider = CASE WHEN upstream_keys.provider = EXCLUDED.provider THEN ... ELSE 报错 END` 需要 upsert 具备"冲突且字段不符时整条失败"的语义，而 `ON CONFLICT DO UPDATE` 表达不了拒绝，只能靠触发器或先查后写 —— 都超出本期范围（本期不改 Key 管理链路）。

本期的处理是**把暗路暴露出来**，两条要求：

1. **`POST /admin/keys/import` 侧加前置检查**（be-api）：导入前按 `key_id` 查现存行，若存在且 `provider` 与本次导入的目标不同 → **409 + `immutable_field`**，message 写清"该 key_id 已属于 provider X，不能改挂到 Y；请换一个 key_id，或先删除旧记录"。这条属于"运维绕不过"档（缺的不是输入而是正确的操作路径），**判据要写清**，照 §4.1.1 的通则。
2. **界面在"重新导入"引导里说明**（designer / fe-provider）：改 provider 名的替代路径是"在新 provider 下重新导入 Key"，此处必须明确 **Key 的 key_id 也要是新的**，不能拿旧 key_id 原样导入，**并写清为什么**（旧 Key 带着历史用量留在旧 provider 下停用）。否则运维照引导操作，反而触发上面那条改写；而只说"要换 key_id"不说理由，他会去找绕过的办法——最可能的是先删旧 Key 再导，那会连历史一起弄丢，比原缺陷更糟。完整论证见 §8.1 ② 末段。
3. **交付只读排查 SQL**（运维）：handler 检查只堵未来。已发生的改写需靠 §8.2.1 "裁决一之补"那条 SQL 人工核对，**不进 `Migrate`、不阻塞上线**。

**裁决三：`deploy/init.sql` 的处置 —— 本期不删不改，但在 §8.2 记为已知约束。**

它与 `schema.sql` 在唯一性、列类型、可空性三处不一致，是个真实的踩坑源（照它建库则导入不可用）。但它不在本期改造面内，且当前只被两个测试脚本引用；本期动它等于给一条没验证过的路径改行为。**明确记录为"已知过期副本，以 `schema.sql` 为准"，并建议后续单独一轮收敛**（要么让 `init.sql` 只做扩展配置、表结构全交 `auto_migrate`，要么整体删除）。写在这里是为了防止以后有人看到两份不一致时，按"新的那份更准"这种直觉去改错方向。

### 8.3 已闭合（原遗留项）

| 原遗留项 | 结论 |
|---|---|
| ~~`default_provider` 的修改入口~~ | **已闭合。**designer 论证了缺它会由页面制造页面自己修不了的坏状态，已落为 §4.1.2 `PATCH /admin/config/default-provider`（乐观锁 + 目标必须 enabled + 进版本历史 `action='set_default'`），并与 §4.6 停用前置检查形成闭环 |
