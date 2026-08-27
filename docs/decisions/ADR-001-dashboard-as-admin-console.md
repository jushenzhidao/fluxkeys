# ADR-001: 看板升级为管理控制台

## Status

Accepted (2026-08-23)

## Background

看板当前是纯只读服务：18 个路由（14 个业务 GET + `/healthz` + 3 个静态资源，另有 FastAPI 自动挂载的 `/docs` `/redoc` `/openapi.json`），全部无鉴权，仅靠 compose 里绑 `127.0.0.1` 挡外网。Postgres 连接池在 `db.py:62` 设了 `default_transaction_read_only=on`，服务端强制只读。

三项高频运维动作目前只能靠 `curl` 直连网关管理接口完成：导入 Key、改 Key 出口 IP、调整 Key 状态（第三项网关侧尚无端点）。运维要手工拼 `Authorization: Bearer $ADMIN_API_KEY`，`ADMIN_API_KEY` 因此散落在各人的 shell 历史与脚本里。

同时 V4 P0-1 定下了一条不变量：**配额与业务数据的写路径唯一化**。网关是唯一的写入者，Redis Lua 是配额的唯一权威路径。加一个管理界面不应该破掉这条。

## Decision

看板升级为管理控制台，但**不获得写库能力**。四条要点：

1. **看板永不直接写库。** Postgres 连接保留 `default_transaction_read_only=on`。所有写操作由看板服务端以 HTTP 转发给网关管理接口，网关仍是唯一写入者。P0-1 的写路径唯一化因此保持成立。

2. **鉴权 = 共享口令 + HMAC 签名的会话 Cookie。** 不建用户表。会话状态不落任何存储，用 `hmac-sha256` 签名的自包含令牌（stdlib `hmac`/`hashlib`，不引入 `itsdangerous`）。Cookie 为 `HttpOnly` + `SameSite=Strict`。

3. **`ADMIN_API_KEY` 只存在于看板服务端进程内。** 浏览器只拿到会话 Cookie。转发时由服务端注入 `Authorization` 头。前端任何路径都拿不到管理密钥。

4. **网关管理接口继续对外暴露。** 看板与 `curl` 直连并存，不收紧为「只允许看板调用」。建用户与签发 API Key 不进看板，继续走直连。

配套新增网关端点 `PATCH /admin/keys/{key_id}`，承载 Key 状态与池子调整。

## Consequences

### 正面

- `ADMIN_API_KEY` 收敛到单一位置（看板服务端环境变量），不再散落在运维的 shell 历史里。轮换只需改一处。
- 审计链路完整：看板带 `X-Admin-Actor` 转发，网关照常写 `audit_logs`，「谁在什么时候改了哪个 Key 的出口 IP」可回答。不引入双写。
- 只读不变量由 Postgres 服务端强制，不依赖看板代码自觉。即便看板将来被注入了写 SQL，服务端仍会拒绝（已实测：`UPDATE` / `INSERT` / `CREATE TABLE` 均被 `ReadOnlySQLTransactionError` 拒绝，且连接内 `SET transaction_read_only = off` 后再写仍被拒）。
- 会话无状态，看板重启后已登录用户不掉线，多副本也无需共享会话存储。
- **应急吊销无需额外机制**：`DASHBOARD_SESSION_SECRET` 参与 HMAC 签名，改掉它所有既有 token 的签名校验立即失败。密钥本身就是世代号，不需要再实现一套版本字段。对共享口令模型来说「只能全量吊销」在语义上是匹配的——本来就无法区分具体人，口令泄漏时要做的正是让所有会话失效。（已实测：改密钥重启后重放旧 token 返回 401）
- 前端无构建步骤这一现状得以保留：方案不引入打包器。

### 负面

- **看板从「泄漏即读到用量数据」升级为「泄漏即可改 Key 状态」。** 这是本决策最实质的代价。风险敞口从只读信息扩大到写操作，所以口令强度、登录限流、继续绑 `127.0.0.1` 三者缺一不可。
- **共享口令无法区分人员。** `X-Admin-Actor` 是自报的，不可信，只能用于排障线索而非追责依据。真要追责需要引入用户表，本决策明确不做。
- **登出是客户端行为，不是服务端吊销。** `POST /logout` 只让浏览器丢弃 Cookie；那个 HMAC 签名的 token 在服务端仍然完全有效，直到 `DASHBOARD_SESSION_TTL` 到期。任何拿到过它的人（浏览器历史、代理日志、共享机器上的另一个进程、抓包）在窗口内都能继续用。

  这是无状态会话换取「看板保持只读」这条不变量的**直接代价**——服务端会话表在只读连接下根本写不进去（实测 `SET transaction_read_only = off` 后仍被拒），改走 Redis 则把看板从只读旁路变成有写状态的组件，与 P0-1 方向相反。

  两条缓解手段，都不破坏只读约束：
  - 常态：`DASHBOARD_SESSION_TTL` 定为 **7200（2 小时）**而非一个工作日，把重放窗口压到可接受。共享机器上用完须**关闭浏览器**，只点登出不够。
  - 应急：改 `DASHBOARD_SESSION_SECRET` 并重启**全部副本**，全量吊销既有会话。多副本必须共用同一密钥，所以漏掉任一副本，该副本上的旧 token 仍然有效。

  测试 `test_logout_does_not_invalidate_token_server_side` 把这个已知边界钉住了——它断言的是现状而非期望行为，如果将来引入服务端吊销机制该测试会变红，那时应改为断言 401 并同步更新本节。
- 多了一跳网络。网关不可达时看板的管理动作全部失败，而只读报表仍可用——两者可用性不再一致，排障时需要区分。
- 转发层引入新依赖 `httpx`（当前仅在 dev 依赖里，需提升到运行时依赖并写进 `requirements.txt`，否则镜像内 `import httpx` 会失败）。
- Key 导入可能是上千条的大请求体，看板成为中间转发者后，请求体在看板侧也要过一遍内存，需设上限并与网关侧的 8MB 对齐。
- 新增 `PATCH` 端点扩大了网关管理接口的攻击面，且状态机转换需要显式约束，否则会重演「例行导入静默复活 banned Key」那类事故。

### 中性

- FastAPI 自动挂载的 `/docs` `/redoc` `/openapi.json` 会连同管理端点一起暴露 API 结构，需纳入鉴权范围。

## Related

- `docs/architecture-v4.md` P0-1（配额写路径唯一化）——本决策的核心约束来源
- `docs/architecture-v4.md` P0-5（单实例部署）——会话方案无需跨副本共享，但仍按无状态设计
- `docs/integration-report.md` 二（`EXCLUDED` 被 `VALUES` 兜底污染）——`PATCH` 端点的部分更新语义必须避开同类陷阱
- `internal/gateway/server.go` `routes()`——`Admin.APIKey` 为空时不注册路由的既有约定，新端点沿用
