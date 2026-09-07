# FluxKeys V4 架构决策（落地版）

> 版本: V4 · 日期: 2026-08-23
> 依据: docs/scheduler-solution.md (V3-Final) + docs/architecture-review.md (5 P0 / 5 P1)
> 状态: 已收敛，作为实施契约

---

## 0. 与 V3 的关系

V3 的业务理解（水位线、行为差异化、Key-IP 绑定、渠道适配）保留。V4 只做三件事：
1. 消除 V3 中互相矛盾的设计，每个问题收敛到**唯一**方案；
2. 补齐 V3 缺失的正确性机制（回收、对账、探测、崩溃恢复）；
3. 把性能目标从"想象的 3000 QPS"拉回"真实的 20 QPS"，删掉随之而来的过度设计。

---

## 1. P0 逐项收敛

### P0-1 配额写路径 → 唯一权威路径 = Redis Lua

**决策**：所有配额写操作（预扣 / 修正 / 释放）**只能**通过 Redis Lua 脚本执行。进程内存**不持有可写配额状态**，只做只读缓存（用于打分，容忍 1-2s 陈旧）。

理由：真实流量约 20 QPS，单 Redis 轻松承载（Lua 预扣实测 < 0.2ms）。用最终一致换性能在这里没有收益，却会直接破坏"不超刷"这个首要目标。

```
写路径（唯一）:  请求 → EVALSHA quota_acquire → Redis 原子返回 OK/DENY
读路径（打分）:  内存快照（后台 1s 全量 pipeline 刷新）→ 仅用于排序，不做准入判断
准入判断:        永远以 Lua 返回值为准，内存快照无权否决或放行
```

**关键不变量**：`used + prededuct <= hard_watermark` 在 Redis 侧恒成立。

### P0-2 prededuct 泄漏 → TTL 租约 + 对账 + 崩溃恢复

V3 的 prededuct 是"裸计数器"，只能加不能自动减，SSE 断连即永久泄漏。V4 改为**租约模型**：

```
每次预扣生成 lease_id，写入:
  ZSET volc:lease:{quota_day}   member=lease_id  score=expire_at
  HASH volc:lease:data:{lease_id} {key_id, amount, kind, created_at}

三条回收路径:
  1. 正常修正: quota_commit(lease_id, actual) → used += actual, prededuct -= amount, 删 lease
  2. 超时回收: 每 30s 扫 ZSET 中 score < now 的 lease → prededuct -= amount, 删 lease（不计 used）
  3. 崩溃恢复: 实例启动时不重建内存配额（内存本就无权威态），仅触发一次全量 lease 扫描
```

租约 TTL = `max(120s, 预估耗时 × 3)`，流式请求按 600s。

**对账**：每小时对每个活跃 Key 校验 `prededuct >= 0` 且 `prededuct == Σ 未过期 lease.amount`，偏差写告警并以 lease 求和为准强制修正。

### P0-3 配额日 → 引入 quota_day，与自然日解耦

火山 12:00 刷新，自然日 00:00 翻新，V3 直接用 `yyyyMMdd` 后缀导致 00:00-12:00 这 12 小时按满额度调度 → 必然超刷。

```go
// 配额日: 12:00 之前算作前一天
func QuotaDay(t time.Time) string {
    if t.Hour() < 12 { t = t.AddDate(0, 0, -1) }
    return t.Format("20060102")
}
```

所有配额 Key 一律用 `quota_day`。TTL 钉死为**该配额日结束后 24h**（即 `次日12:00 + 24h`），消除 V3 中"次日 12:30"的两义性。

### P0-4 刷新窗口 → 探测确认，而非时间推测

**决策**：系统**不假设**火山何时刷新，只探测"是否已刷新"。

```
11:30  进入 pre-refresh: 活跃 Key 降速至 30%，仅短请求
12:00  所有待刷新 Key 置为 probing 状态（停止承接正常流量）
       每个 Key 按 stagger_offset = hash(key_id+day) % 3600 秒错峰发探测请求
       探测 = 1 次最小成本请求（max_tokens=1）
       成功且响应正常 → 判定已刷新 → 本地配额清零 → 置 active（30 分钟内限速 50%）
       失败/仍报额度耗尽 → 保持 probing，5 分钟后重试（最多至 14:00）
14:00  仍未确认的 Key 标记 refresh_failed 并告警
```

**语义澄清**：`stagger_offset` 作用于**恢复调度的时刻**，不是"控制火山刷新时刻"。探测顺序按昨日消耗**从低到高**（消耗低的 Key 昨日仍有流量，探测更可靠；V3 写反了）。

**实现陷阱（已在开发中发现并修复）**：`stagger_offset` 必须按**秒**取模再放大回 `Duration`，不能直接对纳秒数取模。32 位哈希最大约 4.3e9，而 1 小时是 3.6e12 纳秒，哈希值恒小于模数，取模退化为恒等映射 —— 实测 100 个 Key 的偏移全部挤在开头 4 秒内，错峰完全失效，反而制造出"上百个账号同时发请求"的强机器特征，与反封禁目标完全背离。修复后改用 `fnv.New64a` 并按秒取模，100 个 Key 均匀铺满整个探测小时（每 10 分钟桶 12-24 个）。回归测试 `TestStaggerOffset_DeterministicAndSpread` 会断言分散度。

### P0-5 部署形态 → 单实例，K8s 移出 MVP

**决策**：V1 = **单进程单实例 + Redis 单节点(AOF) + Postgres 单节点**。

原因不只是成本：EIP 绑定在特定 ECS 的弹性网卡上，Pod 漂移后无法使用该机器的辅助 IP 出口，**"Key-IP 终身绑定" 在多 Pod 下物理上不可实现**。

多实例演进路径（非 MVP）：每实例独立 EIP 子池 + Key 按实例静态分组 + 一致性哈希路由，绝不做无状态多副本。

---

## 2. 关键 P1 收敛

| 项 | V3 问题 | V4 决策 |
|---|---|---|
| P1-6 辅助 IP | 称"自动生效" | 必须配置策略路由（`ip rule` + per-IP 路由表）。提供 `scripts/setup-egress.sh` 与 `curl --interface` 逐 IP 验证流程 |
| P1-7 性能目标 | P99<2ms / 3000 QPS | 改为**全局 20 QPS，峰值预留 100 QPS**。删除分桶采样、RCU、5s 全量预计算 —— 100 个 Key 朴素遍历打分是微秒级。注意真正的吞吐瓶颈不是计算而是 `min_request_interval` 自我节流，见 2.2 |
| P1-8 连接池 | "独立 Session" 与 "HTTP/2 共享" 矛盾 | **每 Key 独立 Transport**，禁用跨 Key 连接复用。共享 TLS 连接会让 10 个 Key 共享同一 JA3 指纹，等于在传输层焊死"同一台机器"证据 |
| P1-9 用户模型 | 完全缺失 | 补 Postgres: `users` / `user_api_keys` / `usage_records` / `volc_keys` / `egress_ips`，用户级 RPM/TPM 限流走 Redis 令牌桶 |
| P1-10 fallback | 无预算护栏 | **MVP 默认关闭**付费渠道 fallback。开启需显式配置日预算上限，触顶即熔断 |

其余 P2：预扣估算改为 `(prompt_tokens_est + max_tokens) * multiplier`（V3 漏算 prompt）；Redis 故障降级为全局限流 50% + 拒绝新预扣；行为相似度只做每日离线自检，移出热路径。

推理模型另需单独修正。`max_tokens` 对思维链**不构成约束** —— 实测 `deepseek-v4-flash` 在 `max_tokens=64` 时 `completion_tokens` 达 121（1.89 倍），`max_tokens=16` 时达 141（8.8 倍），因为思维链长度由问题复杂度决定，与用户声明的上限无关。仅靠 `estimate_multiplier`（1.2）会让预扣被系统性击穿：真实用量越过硬水位后要等 Commit 才发现，额度已经超刷。

修正机制是「按比例放大 + 绝对下限」两段，命中 `upstream.reasoning_models`（默认按 `deepseek` / `thinking` / `-r1` 等子串匹配）时对**输出部分**生效，且在乘 `n` 之前完成（每份候选各产生一条独立思维链）：

| 参数 | 默认值 | 作用 |
|---|---|---|
| `quota.reasoning_output_multiplier` | 3.0 | 覆盖实测 1.89 倍并留余量。不取更大值是因为预扣过高会压低单 Key 并发 |
| `quota.reasoning_floor_tokens` | 1024 | 托底极小 `max_tokens`（16 × 3 = 48 仍远不够实测的 141），仅在**显式**传入 `max_tokens` 时套用 |

未显式指定 `max_tokens` 时不套下限：此时输出已按 `default_max_tokens`（4096）计，本身高于任何观测到的思维链长度，再放大只是白占额度。两项均支持环境变量覆盖，便于不重建镜像调优。

`completion_tokens` 已包含 `reasoning_tokens`（实测 88 + 141 = 229 ✓），Commit 侧不存在重复计费；适配层额外解析 `completion_tokens_details.reasoning_tokens` 并落库，用于定位「预扣为何不够」。

---

## 2.1 五维打分的权重取值（已量化，不再调整）

`Score = 35*S_quota + 25*S_history + 20*S_persona + 15*S_health - SoftPenalty`，`SoftPenalty = 20`（越过软水位时生效）。

这组数字被质疑过「配额维度是否太不灵敏」，按 `scoreQuota` 的实际实现（`remainRatio > 0.5` 时 `+10`、`<= 0.2` 时 `-10`）量化后的相对权重曲线是：

| 已用（相对硬水位） | S_quota | 总分 | 相对新 Key |
|---|---|---|---|
| 0% | 100 | 9500 | 100.0% |
| 50% | 50 | 7750 | 81.6% |
| 80% | 10 | 6350 | 66.8% |
| **85%** | 5 | 4175 | **43.9%** |
| 90% | 0 | 4000 | 42.1% |

**结论：保持现状。** 80% → 85% 存在一次断崖（66.8% → 43.9%），由 `SoftPenalty` 制造，正是需要的行为——软水位之前平缓衰减以充分利用额度，越过之后立刻大幅让位。已用 90% 的 Key 只剩新 Key 的 42.1% 权重，抑制足够。

两个备选方向已验证并否决：

- **降软水位（0.8 → 0.7）**：只影响 70%~80% 区间，对 85% 以上毫无影响。等于提前放弃 10% 额度，而额度本身是核心资源。
- **加大 SoftPenalty 到 60**：已用 85% 时权重压到 1.8%，90% 归零。这把软水位变成了硬水位——软水位的语义是「还能用但降优先级」，压到 1.8% 等于禁用，那应该直接调硬水位比例而不是用打分绕。

**其余三维构成 6000 分的固定底座（占新 Key 总分 63.2%），S_quota 最大摆动仅 3500 分（36.8%）。这个比例是有意的**：`scoreHistory` 的「昨日刷满 >95% 扣 15 分」与 persona 的时段过滤承担跨日均衡和行为拟真，而这才是防封禁主线。提高 `WeightQuota` 会让调度退化成「哪个 Key 剩得多用哪个」，反而制造出「额度一到就被榨干」的规律性特征——与首要目标背离。

---

## 2.2 吞吐上限由 Key 数量决定（容量规划必读）

真正的吞吐瓶颈不是打分计算，而是反封禁的自我节流：

```
吞吐上限 = 当前可调度 Key 数 / min_request_interval
```

`min_request_interval` 默认 5 秒。100 Key / 5s = **20 QPS**，正好匹配设计流量——这不是巧合，两个数字是配套的。

但这条约束有三个容易踩的点：

1. **Key 数不足时上限骤降**。只导入 8 个 Key 时上限是 1.6 QPS，超出部分被调度器以「间隔不足」拒绝并返回 503。
2. **启用画像后可调度量随时段浮动**。实测 8 个 active Key 中有 3 个因画像时段被过滤，实际可用只有 5 个（1.0 QPS）。用 `PoolSize()` 估算会得出整天不变的乐观数字，而真实的 503 恰恰集中在可调度量最低的那几个小时。故 `Scheduler.SchedulableAt(now)` 才是容量口径。
3. **503 的处置方向与「配额耗尽」完全相反**。配额耗尽要等 12:00 刷新，间隔不足要加 Key 或降速。误判会让人枯等一整天。

因此：
- 启动日志打印 `active_keys` / `schedulable_now` / `max_qps_now` 三个数
- 可调度量低于池子一半时告警并指明是画像时段过滤所致
- 503 时把调度器的拒绝原因分类（已排除 / 不健康 / 间隔不足 / 超硬水位 / 非活跃时段）写进**日志**
- 对客户端只回笼统文案「暂无可用容量，请稍后重试」——拒绝原因分布是内部容量信息，外泄等于告知对方现在有几个 Key、还剩多少额度

---

## 3. 技术栈（锁定）

| 组件 | 选型 | 说明 |
|---|---|---|
| 网关 | **Go 1.23** | 每 Key 独立 Transport、SSE 流式转发、精确出口 IP 控制，Go 的 `net.Dialer.LocalAddr` 是最干净的实现方式 |
| 热状态 | Redis 7 (AOF everysec) | 配额、租约、限流、Key 状态 |
| 持久化 | Postgres 16 | 用户、Key 元数据、用量流水、审计 |
| 看板 | **Python 3.12 / FastAPI** | 只读报表，与网关完全解耦 |
| 编排 | Docker Compose | 单机部署 |
| CI | GitHub Actions | lint + test + build + 镜像 |

---

## 4. 服务拓扑

```
                   ┌──────────────┐
   用户 ──OpenAI兼容──▶│ gateway (Go) │──每Key独立Transport+绑定LocalAddr──▶ 火山 Ark
                   └───┬───────┬──┘
                       │       │
              ┌────────▼─┐   ┌─▼──────────┐
              │  Redis   │   │ Postgres   │
              │ 配额/租约 │   │ 用户/流水   │
              └────────┬─┘   └─┬──────────┘
                       │       │
                   ┌───▼───────▼────┐
                   │ dashboard (Py) │  只读
                   └────────────────┘
```

---

## 5. 实施顺序

1. 可行性验证：Lua 原子预扣并发不超刷 / 配额日边界 / Egress 绑定
2. 网关核心：配额 → 调度 → Egress → Adapter → HTTP 层
3. Mock 上游 + 集成测试
4. 看板
5. Compose + CI
