# 端到端联调报告

编码完成后的实机验证记录。测试方式是**真实二进制 + 真实 Redis + 真实 Postgres + Mock 上游**，不是单元测试的 fake 环境。

## 结论

单元测试与静态检查全绿（`go build` / `go vet` / `go test ./...`），但端到端跑通后发现 **4 个功能缺陷 + 1 个数据正确性 bug**。它们有一个共同特征：

> 编译通过、测试通过、健康检查 200，但系统不干活。

这类缺陷单元测试查不出来，因为每个模块单独看都是对的，问题出在装配与生命周期上。

---

## 一、四个「装配完好但不可用」的缺陷

### 1. 没有火山 Key 的导入入口

`store.UpsertVolcKey` 早已实现且正确（含 AES-GCM 加密），但**没有任何调用方**——管理接口只有 `GET /admin/keys`（只读）和 `PUT /admin/keys/{id}/ip`，也没有 CLI 工具。

后果：全新部署时 Key 池永远为空，网关对所有业务请求返回 503。冒烟日志实证了这条路径（重试 4 次换 Key 均失败 → `service_busy`）。

绕过方案只能是手写 SQL 并自行复刻 AES-GCM 加密逻辑——一旦实现有偏差，密文将永远解不开。

**修复**：新增 `POST /admin/keys`
- 支持单对象与数组两种请求体，一份清单可导入上千个 Key
- 语义是 upsert，重复导入同一清单是幂等的
- 批内重复 key_id 显式报错而非静默后写覆盖前写
- 逐条 upsert，单条失败不回滚整批（导入 1000 个 Key 时因第 3 个格式错就全滚，运维只能反复试错）
- 导入后立即建立出口绑定 + 重载调度器
- 审计只记 key_id 与统计，绝不记 secret（已验证：明文泄漏检查 = 0）
- 全部失败时返回 400 而非 200，CI 脚本可据此判断

### 2. 桩 probe 导致每日定时停服两小时

`cmd/gateway/background.go` 的刷新探测 probe 恒返回 `(false, nil)`，注释自称这是「保守的安全默认值」。

读 `quota.Refresher` 源码可以证伪这个判断。`Schedulable()` 只在 `idle` / `confirmed` 两个状态放行调度，完整链路是：

```
12:00 进入窗口 → 全部 Key 从 idle 置为 pending
              → Schedulable() 立即返回 false，全池停止承接流量
              → 探测恒 false，卡在 probing
              → 13:59 全部标记 failed
              → 仅在窗口结束后才被清理
```

即恒 false 的桩**不是**「保守地不清零配额」，而是**每天 12:00–14:00 整个网关对所有请求返回 503**。

**修复**：替换为真实探测 `newProbe()`——发一个最小成本的真实请求，200 视为额度已恢复，配额类错误视为尚未恢复，其他错误按未恢复处理（此时才是保守的正确选择）。判定规则与上述推理链已写入代码注释。

### 3. 启动时不装载 Key 池

`Scheduler.Start()` 只启动配额快照循环，**不读 Key 表**。而后台 `key_reload` 间隔 5 分钟，首轮还带 `StaggerOffset` 错峰延迟。

后果：网关重启后在长达数分钟内对所有业务请求返回 503，而 `/healthz` 与 `/readyz` 一路 200——从外部看完全正常，只是不干活。

这比缺陷 1 更隐蔽：导入至少有 Reload 兜底，纯重启没有任何补救。

**修复**：`main.go` 中显式 `sched.Reload(ctx)`。装载失败只告警不阻止启动——Key 表暂时读不到时仍应把服务拉起来，让 `/readyz` 与后台重载反映真实状态，比整个进程起不来更容易运维。

### 4. 后台循环跑两遍，且生命周期错位

`main.go` 自建的 `runReapLoop` / `runRefreshLoop` 与 `background.go` 的 `lease_reap` / `refresh_probe` 重复，租约回收会跑两遍。

更要紧的是生命周期。`main.go` 的循环直接挂在被信号取消的 `ctx` 上：

> 收到退出信号的瞬间循环就退出，而此刻优雅关闭才刚开始，仍有流式请求在收尾并归还租约。**回收器比网关先死**，这批租约只能等下次启动后由超时路径兜底。

`background.go` 用 `context.WithCancel(context.WithoutCancel(parent))` 正是为解决这点。

**修复**：删除 `main.go` 的两个循环，统一走 `background`。关闭顺序固定为：

```
网关 Shutdown（等流式请求收尾）→ bg.stop() → 指标端点 Shutdown
```

顺序不可交换：先停后台，最后一批请求归还的租约就没有回收器处理了。指标端点最后关，让关闭过程本身的指标也有机会被采集。

---

## 二、数据正确性 bug：PostgreSQL EXCLUDED 被 VALUES 兜底污染

`UpsertVolcKey` 原本这样判断「调用方是否指定了状态」：

```sql
VALUES (..., COALESCE(NULLIF($5, ''), 'active'), ...)
ON CONFLICT (key_id) DO UPDATE SET
  status = CASE WHEN EXCLUDED.status = '' THEN volc_keys.status
                ELSE EXCLUDED.status END
```

**`EXCLUDED` 拿到的是 VALUES 子句求值之后的结果**。VALUES 侧写了 `COALESCE` 兜底，所以 `EXCLUDED.status` 永远是 `'active'`，`= ''` 的判断恒为假，CASE 永远走 ELSE。

后果：运维重跑一份只含 `key_id` / `pool` 的例行导入清单，会
- 把已 `banned` 的 Key 静默复活并重新投入流量
- 抹掉 `persona_id`（画像是打散行为规律的手段，而火山商务反馈封禁根因正是「用户行为规律相似」）
- 抹掉 `egress_ip` 终身绑定（换出口等于把一个有历史的老账号变成「换了地址的账号」，这是风控最敏感的信号）

**修复**：拆成两组参数——`$5` 传插入用的实际值，`$11` 单独传 `statusGiven` 布尔标记，不经过 VALUES 求值。`persona_id` / `egress_ip` / `secret_enc` 同法处理。

回归测试：`TestUpsertVolcKey_例行导入不复活banned也不改画像与出口`

> **教训**：`ON CONFLICT` 里判断「入参是否为空」绝不能依赖 `EXCLUDED`——只要 VALUES 侧存在任何兜底表达式，判断就会静默失效。

---

## 三、容量约束：此前无任何文档

实测中出现大量 503，日志显示 `class=quota`，但上游一条请求都没收到。根因是调度器在 Select 阶段就返回了「无可用 Key」。

真正的约束是：

```
吞吐上限 = 当前可调度 Key 数 / MinRequestInterval
```

`MinRequestInterval` 默认 5 秒（反封禁的自我节流）。100 Key / 5s = 20 QPS，正好匹配设计流量。但只导入 8 个 Key 时上限骤降到 1.6 QPS，超出部分被以「间隔不足」拒绝。

两个可诊断性问题：

**503 文案误导**。原文案是「所有 Key 配额耗尽或不可用」，会把运维引向「等 12:00 配额刷新」——而真实处置是加 Key 或降速。两种情况的处置完全相反，误判会让人枯等一整天。

调度器其实已有完整的拒绝原因分类（`rejectReasons`），但 gateway 把它整个丢掉了。

**修复**：
- 503 时把 `已排除=N 不健康=N 间隔不足=N 超硬水位=N 非活跃时段=N` 写进日志
- 对客户端仍只回笼统文案「暂无可用容量，请稍后重试」——拒绝原因分布是内部容量信息，外泄等于告知对方现在有几个 Key、还剩多少额度
- 新增 `config.Scheduler.MaxQPS(keyCount)`，启动时把容量上限算给运维看
- 空池启动时直接给出补救动作：`请通过 POST /admin/keys 导入火山 Key`

**容量口径修正**。启用画像后，可调度量随时段浮动。实测 8 个 active Key 中有 3 个因画像时段被过滤，实际只有 5 个可用：

```
active_keys=8  schedulable_now=5  max_qps_now=1.0
```

用 `PoolSize()` 算会得出整天不变的乐观数字（1.6），而真实的 503 恰恰集中在可调度量最低的那几个小时。新增 `Scheduler.SchedulableAt(now)`，并在可调度量低于池子一半时告警说明原因，避免运维以为 Key 丢了。

---

## 四、已验证通过的行为

用独立数据库（`fk_lead`）+ 独立 Redis DB 隔离后的完整链路：

| 验证项 | 结果 |
|---|---|
| 启动装配 | Redis 连接 → 数据库迁移 → Key 池装载 → 管理接口启用 → 网关监听 → 指标监听 |
| `quota_day` | `20260823`，12:00 分界正确（P0-3） |
| `/healthz` `/readyz` | 200，readyz 真实检查 Redis + Postgres 可达性 |
| 鉴权边界 | 管理接口无凭据/错误凭据 401，业务接口无 Key/伪造 Key 401 |
| Key 导入 | 批量 2 成功 / 2 拒绝（批内重复、空 key_id），幂等重跑正确区分 created/updated |
| secret 加密 | 落库为密文，审计与日志均无明文（泄漏检查 = 0） |
| 导入即可用 | 空池 503 → 导入后**立即** 200，无需等 5 分钟 |
| 启动即可用 | 启动后**首个**请求 200 |
| 非流式对话 | 200，用量精确记账（total = prompt + completion） |
| 流式 SSE | 完整走到 `data: [DONE]` |
| Key 轮换 | 4 次请求分别落在 v1 / v2 / v3 / v4 四个不同 Key |
| 用量流水落库 | 逐条与 Redis `used` 对齐 |
| 换 Key 重试 | 上游失败后真实换 Key，耗尽后返回 503 `service_busy` 而非 500 |
| **预扣无泄漏（P0-2）** | `prededuct` 全为 0，`leases_active=0`，`leases_reaped_total=0`——且未靠超时兜底 |
| 优雅关闭 | 停止接收新请求 → 所有进行中请求已完成 → 后台任务已全部退出 → 已退出 |
| Python 看板 | 135 passed, 16 skipped（16 skip 是无 Postgres 时的集成测试） |

---

## 五、环境坑（复现时需注意）

- **本机 `http_proxy` 会代理 127.0.0.1 的 POST 并返回 502**，看起来像网关挂了。所有本机 curl 必须加 `--noproxy '*'`。这一点已写进 `scripts/smoke-test.sh` 的注释。
- **端口冲突严重**：8080 / 8000 被 Bifrost 占用，18080 被 aikeymart 占用。冒烟脚本连到别人的服务上会得到 `is_bifrost_error` 这类完全无关的响应。
- **测试库串扰**：多方共用同一个 Postgres 时互相 `truncate`，表现为 API Key 突然 401、流水表突然为空。验证时须用独立库 + 独立 Redis DB。
- 沙箱内 30 并发 curl 会被 OOM kill（退出码 137），压测并发控制在 12 以内。
