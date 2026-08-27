# 可行性验证报告

> 日期: 2026-08-23 · 环境: Apple M1 Max / Go 1.23.4 / Redis 7.2.16 / Alpine 3.20 容器
> 目的: 在编写全量代码前，验证 V4 架构中三个高风险决策确实成立

---

## 结论

三项全部通过。V4 架构可以进入实施。

| # | 验证目标 | 对应缺陷 | 结果 |
|---|---|---|---|
| 1 | Redis Lua 原子预扣在高并发下不超刷 | P0-1 | 通过 |
| 2 | 租约模型能回收泄漏的 prededuct | P0-2 | 通过 |
| 3 | 配额日与 12:00 刷新对齐 | P0-3 | 通过 |
| 4 | 按 Key 绑定出口源 IP 真实生效 | P0-5 / P1-6 | 通过（Linux 容器内验证） |
| 5 | 20 QPS 目标下延迟余量充足 | P1-7 | 通过 |

---

## 1. 原子预扣不超刷（P0-1）

**验证方法**：200 个 goroutine 通过 `close(chan)` 同时发起预扣，每个申请 1000 单位，硬水位 100000 —— 总需求是可用额度的 2 倍。

**结果**：

```
放行 100 次 / 拒绝 100 次
used + prededuct = 100000，恰好等于硬水位，未突破
```

放行次数**精确等于** `hard/amount`，不多不少。这正是 V3「本地内存标记 + 异步刷 Redis」方案会失败的场景：多个并发请求各自读到陈旧的 `used`，同时判定尚有余额而全部放行。

混合场景（并发 Acquire + Commit，实际用量小于预扣量）下不变量同样成立，且全部提交后 `prededuct` 归零。

## 2. 租约回收（P0-2）

**验证方法**：模拟 5 个请求预扣后客户端断开（Commit 永不到达），推进时钟越过租约 TTL 后触发回收。

**结果**：

```
回收前 prededuct = 5000
租约未到期时 Reap 回收 0 条（不误伤进行中的请求）
到期后 Reap 回收 5 条，prededuct 归零，used 保持 0（泄漏不计入用量）
```

补充验证了一个容易被忽略的边界：**回收后迟到的 Commit 仍须补记实际用量**。若直接丢弃，会造成少算而在后续导致超刷。测试 `TestCommit_AfterReap_StillRecordsUsage` 确认 `used` 被正确补记为 750。

对账能力也已验证：人为向 Redis 注入 `prededuct=7777` 的漂移后，`Reconcile` 检出偏差 6777 并以未过期租约之和（1000）强制修正。

## 3. 配额日对齐（P0-3）

7 个时间边界用例全部通过，含跨月、跨年。关键用例 `TestQuotaDay_DiffersFromCalendarDayInMorningWindow` 直接复现 V3 缺陷：8 月 24 日上午 9 点，自然日为 `20260824`，配额日为 `20260823` —— 若按自然日存储，这 12 小时会按满额度调度而必然超刷。

TTL 语义也已钉死：13:00 时计算出的过期时刻为次次日 12:00，消除了 V3「次日 12:30」的两义性。

## 4. 出口 IP 绑定（P0-5 / P1-6）

这是最需要实证的一项，因为 P1-6 的失效模式是**静默**的：未配置策略路由时，绑定源地址的请求不会报错，只是全部走了主 IP。

macOS 宿主机无权添加环回别名，故在 Alpine 容器内（`--cap-add=NET_ADMIN`）添加真实别名地址后运行交叉编译的测试二进制：

```
inet 127.0.0.1/8 scope host lo
inet 127.0.0.2/8 scope host secondary lo
inet 127.0.0.3/8 scope host secondary lo

TestLocalAddrBinding_ActuallyControlsSourceIP  PASS
```

测试让 4 个 Key 各自通过绑定了不同 `LocalAddr` 的客户端请求同一 HTTP 服务，并在服务端断言 `r.RemoteAddr` 的源 IP **精确等于**该 Key 的绑定值。这证明 `net.Dialer.LocalAddr` 方案在真实 Linux 网络栈下有效。

同时 `TestVerify_DetectsUnroutableIP` 验证了启动自检能力：不存在的地址 `10.255.255.254` 被检出并标记失败，避免配置错误在线上静默失效。

另外验证了绑定的**确定性**：两个独立构造的 Pool 对 30 个 Key 给出完全一致的分配结果，即进程重启后「Key-IP 终身绑定」不会漂移。

## 5. 性能余量（P1-7）

```
BenchmarkAcquireCommit-10    3000    852169 ns/op
```

一次完整 Acquire + Commit 耗时 0.85ms，含 2 次 Redis 往返，且这是 macOS Docker 桥接网络的悲观值（生产同机 Redis 会显著更快）。

按 V4 修正后的目标 20 QPS 计算，配额路径的资源利用率不到 2%。**这从数据上确认了 P1-7 的判断**：V3 为 3000 QPS 设计的分桶采样、RCU 无锁热更新、5 秒全量分数预计算全部是过度设计，100 个 Key 的朴素遍历打分是微秒级操作。

---

## 复现方式

```bash
# 配额（需 Redis）
REDIS_ADDR=127.0.0.1:16399 go test ./internal/quota/ -v -count=1

# 出口绑定（宿主机上部分用例会跳过）
go test ./internal/egress/ -v -race

# 出口绑定完整验证（Linux 容器，需 NET_ADMIN）
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -o /tmp/egress.test ./internal/egress/
docker run --rm --cap-add=NET_ADMIN -v /tmp/egress.test:/egress.test alpine:3.20 sh -c \
  'ip addr add 127.0.0.2/8 dev lo; ip addr add 127.0.0.3/8 dev lo; /egress.test -test.v'
```
