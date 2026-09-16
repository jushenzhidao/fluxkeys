# FluxKeys 交付总览

火山引擎多 Key 智能调度网关。从架构评审到可运行系统的完整落地。

## 交付路径

```
V3 方案（1665 行）
  → 架构评审 → fail（5 项 P0 + 5 项 P1 + 8 项 P2）   docs/architecture-review.md
  → V4 架构收敛（逐条决策）                          docs/architecture-v4.md
  → 编码前可行性验证（4 项实测）                     docs/feasibility-report.md
  → 编码实现（Go 网关 + Python 看板）
  → 端到端联调 → 发现并修复 5 个缺陷                 docs/integration-report.md
```

关键取舍是**先验证再编码**。可行性报告在写业务代码之前就用实测数据确认了四件事：200 goroutine 抢 100 份额度恰好放行 100 次、租约泄漏可回收、Linux 容器内每 Key 绑定的源 IP 精确生效、Acquire+Commit 0.85ms（20 QPS 目标利用率 < 2%）。最后一项直接证伪了 V3 为 3000 QPS 做的全部性能设计。

## 规模

| | 文件 | 行数 |
|---|---|---|
| Go | 106 | 35,867（其中测试 17,972） |
| Python 看板 | 38 | 8,719 |

Go 测试用例 527 个，Python 310 passed / 18 skipped（跳过项为需真实 PG/Redis 的集成用例）。Go 直接依赖只有 4 个：pgx、go-redis、prometheus client、yaml。

## 架构要点

**配额写路径唯一化（P0-1）**。所有预扣/修正/释放只能走 Redis Lua。进程内存不持有可写配额状态，只做只读快照用于打分排序，无权参与准入。`used + prededuct <= hard` 在 Redis 侧恒成立。

**租约模型（P0-2）**。预扣登记带 TTL 的租约到 ZSET，三条回收路径：正常 Commit 修正、超时 Reap、崩溃恢复，外加每小时对账。SSE 客户端断连不会造成永久泄漏。

**配额日与自然日解耦（P0-3）**。火山配额在每日 12:00 刷新，故 12:00 之前归属前一自然日。所有用量归档、历史打分、报表统计都按 `quota_day` 而非自然日。

**刷新探测确认制（P0-4）**。系统不假设火山何时刷新，只探测「是否已刷新」——发一个最小成本的真实请求，200 视为已恢复，配额类错误视为未恢复。`stagger_offset` 的语义是「恢复调度的时刻」，不是「控制火山刷新的时刻」。

**单实例部署（P0-5）**。EIP 绑定在特定 ECS 弹性网卡上，Pod 漂移后无法使用该机器的辅助 IP 出口，多副本下「Key-IP 终身绑定」物理上不可实现。K8s 移出 MVP。

**每 Key 独立 Transport（P1-8）**。用 `net.Dialer.LocalAddr` 钉死 TCP 源地址，并**禁用 HTTP/2**——多路复用会让多个 Key 共享同一 TLS/JA3 指纹，等于在传输层焊死「同一台机器」的证据。

**反过度设计（P1-7）**。真实流量 20 QPS，V3 的分桶采样、RCU 无锁热更新、位图、5 秒全量预计算全部删除。100 个 Key 朴素遍历打分是微秒级。

## 端到端联调发现的 5 个缺陷

单元测试与静态检查全绿，但实机跑通后发现 5 个问题。共同特征是**编译通过、测试通过、健康检查 200，但系统不干活**。

1. **无 Key 导入入口** — `store.UpsertVolcKey` 早已实现且正确，但没有任何调用方。全新部署 Key 池永远为空，所有业务请求 503。已补 `POST /admin/keys`。
2. **桩 probe 导致每日停服两小时** — 刷新探测 probe 恒返回 false，注释自称「保守的安全默认值」。读状态机源码可证伪：12:00 进窗口后全池置 pending 立即停止承接流量，探测恒 false 卡在 probing 到 13:59。即每天 12:00–14:00 全站 503。
3. **启动不装载 Key 池** — `Start()` 只起快照循环不读 Key 表，后台 reload 间隔 5 分钟且首轮还带错峰延迟。重启后数分钟全部 503，而健康检查一路 200。
4. **后台循环跑两遍且生命周期错位** — 更要紧的是租约回收器挂在被信号取消的 ctx 上，收到退出信号瞬间就死，而此时优雅关闭刚开始、仍有流式请求在归还租约。回收器比网关先死。
5. **PostgreSQL EXCLUDED 被 VALUES 兜底污染** — `ON CONFLICT ... WHEN EXCLUDED.status = ''` 判断「调用方是否指定状态」，但 VALUES 侧有 `COALESCE` 兜底，EXCLUDED 拿到的是兜底后的值，判断恒为假。后果是例行导入会静默复活已封禁的 Key，并抹掉画像与出口 IP 绑定。

详见 `docs/integration-report.md`。

## 已验证行为

以下行为在端到端形态下逐项验证过（当时假上游以本地全栈形态运行；该形态已收敛为
`test/mockark` 的进程内夹具，等价的离线链路验证现由 `go test ./test/...` 承接）：

- 8 次请求（7 非流式 + 1 流式）全部 200，落在 **8 个不同 Key** 上，合计 1419 tokens
- 流式 SSE 完整走到 `data: [DONE]`
- **`prededuct` 全为 0，`leases_active=0`，`leases_reaped_total=0`** —— P0-2 不变量成立且未依赖超时兜底
- 并发不超刷（集成测试）：80 并发下成功 52 / 拒绝 28，用量 5143 严格低于水位 6000；网关记账与上游独立记账完全一致
- secret 加密落库，`volc_keys` 无明文、`audit_logs` 无明文（两项检查均 0 行）
- 鉴权边界 4/4：管理接口无凭据/错凭据 401，业务接口无 Key/伪造 Key 401
- Python 看板 `/api/overview` 与 `/api/keys` 数据与网关侧一致
- 优雅关闭：停止接收新请求 → 进行中请求已完成 → 后台任务已退出 → 已退出

## 容量规划

吞吐上限不由计算能力决定，而由反封禁自我节流决定：

```
吞吐上限 = 当前可调度 Key 数 / min_request_interval
```

默认 100 Key / 5s = 20 QPS，与设计流量配套。但只导入 8 个 Key 时上限是 1.6 QPS；启用画像后可调度量随时段浮动（实测 8 个 active 里 3 个被时段过滤，实际 5 个 = 1.0 QPS）。

启动日志会打印 `active_keys` / `schedulable_now` / `max_qps_now`。503 时把拒绝原因分类写进日志（已排除/不健康/间隔不足/超硬水位/非活跃时段），但对客户端只回笼统文案——拒绝原因分布是内部容量信息。

这点很关键：「间隔不足」与「配额耗尽」的处置方向完全相反，前者要加 Key 或降速，后者要等 12:00 刷新。误判会让人枯等一整天。

## 上线前必做

1. **策略路由**。云厂商绑定辅助私网 IP 后，OS 不会自动配置到网卡也不建路由表，此时按 Key 绑定出口会**静默失效**（不报错，只是不生效，所有 Key 共享主 IP）。必须执行 `deploy/setup-egress.sh` 并保持 `egress.verify_on_start=true`——启动自检失败即拒绝启动，宁可起不来也不要带着失效的反封禁设计接流量。出口地址既可逐条写进 `EGRESS_IPS`，也可设 `EGRESS_IPS_SOURCE=scan` 让网关扫描本机网卡（过滤规则与分档计划见 `deploy/README.md`）。
2. **切换真实上游**。`EGRESS_MODE=multi_ip`；volc 地址默认就是真实火山（不再需要设 `VOLC_BASE_URL`——它一旦非空就会盖掉配置文件里的 provider 地址，只在确实要指向别处时才设）。
3. **业务端口前置 TLS**。compose 的网关端口绑 0.0.0.0，生产需在前面加 Nginx/Caddy 终止 TLS。
4. **导入 Key 并确认容量**。用 `POST /admin/keys` 批量导入后，检查启动日志的 `max_qps_now` 是否满足预期流量。
5. **付费渠道 fallback 默认关闭**。启用时配置校验强制要求设置 `daily_budget_cents`。

## 主要文件

```
cmd/gateway/          main.go 装配 / background.go 后台任务 / adapters.go 适配层
internal/quota/       Lua 脚本 + 租约 + quota_day + 刷新探测（配额唯一权威入口）
internal/scheduler/   五维打分 + 加权随机 + 健康状态机
internal/egress/      可插拔出口层（direct / multi_ip）
internal/gateway/     HTTP 服务 + 代理 + SSE + 限流 + 管理接口
internal/store/       Postgres + AES-GCM 加密 + 异步用量流水
internal/persona/     行为画像（时段窗口 + 节奏因子）
test/mockark/         假上游夹具（集成测试进程内启动；cmd/ 是手工联调入口，不属产品）
dashboard/            FastAPI 只读报表 + 轻前端
deploy/               Prometheus + Grafana + alerts + setup-egress.sh
.github/workflows/    ci.yml + release.yml
```
