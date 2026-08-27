# 看板管理控制台：鉴权与转发方案

> 状态: 待实现 · 日期 2026-08-23 · 配套 ADR-001
> 适用范围: `dashboard/` (Python/FastAPI) 与 `internal/gateway/` (Go) 的增量改动

---

## 0. 现状核实（含对既有陈述的两处修正）

实测手段与结论：

| 项 | 核实方式 | 结论 |
|---|---|---|
| 看板端点数量 | 用真实 `app` 对象枚举 `app.routes` | **18 个路由，不是 14 个**（见修正一） |
| 看板是否有写操作 | 枚举全部路由的 `methods` | 确认全为 `GET`/`HEAD`，无任何非 GET 端点 |
| Postgres 只读 | 连真实库跑 `UPDATE`/`INSERT`/`CREATE TABLE` | 全部被 `ReadOnlySQLTransactionError` 拒绝 |
| 只读能否被绕过 | 连接内 `SET transaction_read_only = off` 后再写 | 仍被拒绝，服务端强制 |
| `itsdangerous` | venv 内 import | **缺失**（见修正二） |
| `httpx` | venv 内 import | 存在，但只在 dev 依赖，**运行镜像内没有** |

### 修正一：看板对外暴露的是 18 个路由，不是 14 个

14 个业务 GET 之外还有：`/healthz`、`/`、`/app.js`、`/app.css`，以及 FastAPI 自动挂载的 `/openapi.json`、`/docs`、`/docs/oauth2-redirect`、`/redoc`。

这不是数字上的吹毛求疵：**`/openapi.json` 与 `/docs` 会把新增的管理端点结构完整暴露出去**。如果鉴权只按「14 个业务端点」的清单去加，这三个路由会成为漏网之鱼，攻击者能直接读到管理接口的请求体格式。豁免清单必须显式处理它们。

### 修正二：`itsdangerous` 未安装，Starlette 的 `SessionMiddleware` 当前不可用

`starlette 0.41.3` 已装，但 `from starlette.middleware.sessions import SessionMiddleware` 抛 `ModuleNotFoundError: No module named 'itsdangerous'`——该中间件把 `itsdangerous` 作为可选依赖。所以「直接用 Starlette 内建会话」不是零成本选项，它和自研签名一样都要新增依赖。这个事实直接影响 1.1 的选型结论。

### 另需注意：`httpx` 目前进不了运行镜像

`requirements.txt` 只有 5 个包，`httpx` 在 `requirements-dev.txt` 与 `pyproject.toml` 的 `[dev]` 里。`Dockerfile` 构建阶段执行的是 `pip install -r requirements.txt`，因此**镜像内没有 httpx**。转发层若用 httpx，必须把它提升为运行时依赖，否则容器启动后首次转发才会炸 `ImportError`——又是一个「装配完好但不干活」的缺陷。

---

## 1. 会话鉴权方案

### 1.1 会话签发与校验：选型对比

| 维度 | A. stdlib HMAC 自签名 | B. itsdangerous / Starlette SessionMiddleware | C. 服务端会话表 + 随机 SID |
|---|---|---|---|
| 新增依赖 | 无（`hmac`/`hashlib`/`secrets`/`base64`） | `itsdangerous`（当前缺失，需新增） | 无，但需存储 |
| 会话状态 | 无状态，自包含 | 无状态，自包含 | 有状态 |
| 存哪 | 不存 | 不存 | Redis 或 Postgres |
| 与只读约束冲突 | 无 | 无 | **冲突**：写 Postgres 违反只读；写 Redis 破坏「看板只读」定位 |
| 重启后登录保持 | 保持 | 保持 | 取决于存储 |
| 主动踢出单个会话 | 做不到（只能轮换密钥全体失效） | 同 A | 可以 |
| 代码量 | 约 40 行 | 约 10 行 | 约 80 行 + 存储 |
| 审计面 | 全部逻辑在项目内可读 | 依赖库行为 | 同 A |

**结论：选 A（stdlib HMAC 自签名）。**

理由不是「少一个依赖」这种泛泛之谈，而是三条具体的：

1. **B 的主要卖点在本项目不成立。** 选 B 通常是因为「框架自带、无需自研」，但实测 `itsdangerous` 缺失，B 和 A 都要动依赖清单。B 省下的代码约 30 行，代价是引入一个仅为签名 Cookie 而存在的依赖。而看板的 Dockerfile 构建阶段要装 gcc 编译 asyncpg，依赖清单每加一项都要重新过一遍构建，这个成本不算零。

2. **C 与只读不变量直接冲突。** 看板的 Postgres 连接是服务端强制只读（已实测无法绕过），会话表根本写不进去。改走 Redis 则把看板从「只读旁路组件」变成有写状态的组件，与 P0-1 的收敛方向相反。为了一个共享口令登录去破坏这条不变量，不划算。

3. **C 唯一的独有能力（踢出单个会话）在共享口令模型下没有意义。** 所有人用同一个口令，没有「单个会话」的身份概念，要作废就是全体作废——而 A 通过轮换 `DASHBOARD_SESSION_SECRET` 就能做到（已实测：换密钥后旧令牌校验失败）。

令牌格式与实现（已实测 8 项通过：正常校验、篡改载荷、去签名、换密钥、过期边界、伪造永不过期载荷、口令正确/错误）：

```
token = base64url(payload_json) + "." + base64url(hmac_sha256(secret, body))
payload = {"v":1, "iat":<签发秒>, "exp":<过期秒>, "jti":<随机8字节>}
```

实测令牌长度 126 字节，远在 Cookie 4096 字节预算内。

三个实现细节，写错任何一条方案就失去意义：

- **签名比对必须用 `hmac.compare_digest`。** 用 `==` 比较会按字节短路返回，攻击者可据响应时间逐字节爆破签名。
- **先验签名，再解析载荷。** 顺序颠倒等于把 `json.loads` 暴露给未经验证的输入。
- **`v` 字段是版本位。** 将来改载荷结构时靠它拒绝旧格式，而不是让旧令牌以未定义行为通过。`exp` 判定用 `now >= exp`（已实测边界）。

`jti` 不用于服务端查重（无状态，无处可查），它的作用是让同一秒内为同一口令签发的两个令牌不相同，避免令牌值可预测。

### 1.2 Cookie 属性

```
Set-Cookie: fk_session=<token>; HttpOnly; SameSite=Strict; Path=/; Max-Age=7200[; Secure]
```

| 属性 | 取值 | 理由 |
|---|---|---|
| `HttpOnly` | 恒开 | JS 读不到。看板前端有 706 行原生 JS 并从 CDN 加载 Chart.js，一旦 CDN 被投毒或出现 XSS，没有 `HttpOnly` 就等于会话直接被读走 |
| `SameSite` | `Strict` | 看板是纯内部运维面，没有任何跨站跳转进来的正常场景。选 `Strict` 而非 `Lax`：`Lax` 允许顶层 GET 携带 Cookie，而管理动作虽是 POST，`Strict` 能连「从外部链接点进来即已登录」这种信息泄漏一并挡掉 |
| `Path` | `/` | 静态资源与 API 同源，无需按路径细分 |
| `Max-Age` | 7200（2 小时） | 这个值同时是「登出后 token 仍可被重放的最长时间」（见 1.3 的代价一节），故不取一个工作日。不设 `Session` 级：浏览器不关就永久有效，特权入口不应如此 |
| `Secure` | **由 `DASHBOARD_COOKIE_SECURE` 控制，默认 `false`** | 见下 |

`Secure` 在 HTTP 本地开发下的处理是唯一需要妥协的点：置 `Secure` 后浏览器在 `http://127.0.0.1` 上**不回传** Cookie，本地开发直接无法登录。三种做法里：

- 硬编码 `Secure=True` —— 本地不可用，开发者会去注释掉代码，最终生产也可能被注释掉，最危险
- 硬编码 `Secure=False` —— 生产上 Cookie 可能走明文
- **按配置项决定，默认 `false`，但启动时若检测到 `Secure=false` 打印显式告警** —— 采用这条

之所以默认 `false` 而非 `true`：当前部署形态是 compose 绑 `127.0.0.1` + SSH 隧道访问（`docker-compose.yml` 注释已写明 `ssh -L 8000:127.0.0.1:8081`），链路加密由 SSH 承担，此时 `Secure=true` 反而让隧道访问失效。默认值应匹配既有部署形态，同时用启动告警保证「将来真的挂到 HTTPS 上时不会忘记打开」。

### 1.3 会话状态存哪

**不存。** 理由已在 1.1 展开：只读约束让 Postgres 不可写，写 Redis 会破坏看板的只读定位。

对「看板可能多副本」的回应：无状态签名天然支持多副本，**前提是所有副本共享同一个 `DASHBOARD_SESSION_SECRET`**。这一点必须写进部署文档——若让各副本自行随机生成密钥，用户会在副本间跳转时反复掉登录，且现象随负载均衡的落点随机出现，极难排查。

对「重启后是否该保持登录」的回应：**应该保持**。看板重启是常规运维动作（改配置、升级镜像），若重启即掉线，运维会在最需要看板的时刻（刚改完东西）被踢出去。无状态签名天然满足。代价是「重启无法清除所有会话」——需要清除时轮换密钥，这是显式动作，比隐式副作用更好。

#### 代价：登出无法在服务端失效

前面三条都是收益，这一条是必须一并接受的代价，不要只读上半节。

`POST /logout` 只能做 `set_cookie(max_age=0)`——**纯客户端行为**。它让浏览器丢弃 Cookie，但那个 HMAC 签名的 token 在服务端仍然完全有效，直到 `DASHBOARD_SESSION_TTL` 自然到期。服务端没有任何会话记录可删，这正是「不存」的另一面。

实测：
```
POST /logout                    -> 204
用 curl -b 重放登出前的 Cookie  -> /api/overview 仍返回 200
```

任何拿到过该 token 的人（浏览器历史、代理日志、共享机器上的另一个进程、抓包）在窗口内都能继续使用。

两条缓解手段，都不破坏只读约束：

| 场景 | 动作 | 效果 |
|---|---|---|
| 常态 | `DASHBOARD_SESSION_TTL` 定为 7200（2 小时）；共享机器上用完**关闭浏览器** | 把重放窗口压到可接受，而非一个工作日 |
| 应急（口令泄漏 / 怀疑 Cookie 泄漏） | 改 `DASHBOARD_SESSION_SECRET` 并重启**全部副本** | 全量吊销既有会话（实测改密钥后旧 token 返回 401） |

密钥本身就是世代号，不需要额外实现版本字段。但注意它与多副本约束的交互：**所有副本必须共用同一密钥，所以应急吊销时每个副本都要改并重启，漏掉一个则该副本上的旧 token 仍然有效。**

`DASHBOARD_SESSION_TTL` 的语义因此不只是「多久要重新登录」，而是「登出后 token 仍可被重放的最长时间」。调大它等于按同样倍数放大风险窗口，`.env.example` 里已写明这点。

### 1.4 口令存储与比对

**存哈希，不存明文；比对用常量时间。**

```python
# 启动时：由 DASHBOARD_PASSWORD 计算 sha256 摘要，明文不留在模块级变量里
_PW_DIGEST = hashlib.sha256(password.encode()).digest()

# 登录时
def _password_ok(candidate: str) -> bool:
    return hmac.compare_digest(hashlib.sha256(candidate.encode()).digest(), _PW_DIGEST)
```

两点说明，避免被误读为「安全强度不足」：

- **这里刻意不用 bcrypt/argon2。** 慢哈希的价值在于「哈希值泄漏后抬高离线爆破成本」。本场景的哈希只存在于进程内存，且明文口令本来就在同一进程的环境变量里——攻击者能读到哈希就已经能读到明文，慢哈希没有增益，只增加依赖和登录延迟。这与「存用户密码到数据库」是不同的威胁模型。
- **常量时间比对是必需的。** 即便哈希不落盘，`==` 的短路行为仍会通过响应时间泄漏信息。先取 sha256 再 `compare_digest`，顺带保证两侧长度一致。

启动时校验：`DASHBOARD_PASSWORD` 缺失或长度不足 12 字符则**启动失败**，参照 `FLUXKEYS_ENCRYPTION_KEY` 的做法。不静默降级为「无鉴权」——那正是当前的危险状态，一次配置疏漏就会悄悄退回去。

### 1.5 登录失败限流

**必须做。** 看板是特权入口，共享口令又只有一个，不限流等于给爆破留门。

限流状态放**进程内存**（不放 Redis）：

```
键: 客户端 IP
窗口: 5 分钟滑动
阈值: 连续失败 5 次 → 锁定该 IP 15 分钟
成功登录 → 立即清零该 IP 计数
```

放内存而非 Redis 的理由：写 Redis 会破坏看板的只读定位（同 1.1 的 C 方案问题）。单实例下内存完全够用；多副本下攻击者可以靠轮询副本放大尝试次数，但这只是把 5 次放大到 `5 × 副本数`，量级上仍远低于有效爆破所需，而代价是保住了不变量。这个取舍需要在实现时写进注释，避免后人误以为是漏考虑。

两个实现要点：

- **客户端 IP 的取法。** 当前部署是 compose 直接绑 `127.0.0.1`，无反向代理，取 `request.client.host` 即可。**绝不能无条件信任 `X-Forwarded-For`**——没有代理时该头完全由客户端控制，攻击者每次换一个伪造 IP 就能绕过限流。将来若真的加了反向代理，需要显式配置「信任几层代理」再从右往左取。
- **锁定期间的响应。** 返回 `429` 且不透露剩余尝试次数——告知「还剩 2 次」等于帮攻击者校准节奏。
- 内存字典需设上限（如 1 万条）并按 LRU 淘汰，否则攻击者用伪造源 IP 就能把看板内存打满。

### 1.6 鉴权豁免清单

**采用白名单豁免、其余全需鉴权**，用中间件统一拦截，而不是给每个端点挂 `Depends`。

理由与网关侧「`Admin.APIKey` 为空时不注册路由」同源：逐端点挂依赖时，新增一个端点忘了挂就是裸奔，而这种遗漏不会有任何报错。中间件默认拦截则相反——新增端点自动受保护，要放行必须显式加进豁免清单。

豁免清单：

| 路径 | 是否豁免 | 理由 |
|---|---|---|
| `/healthz` | 豁免 | compose healthcheck 在容器内调用，无法带 Cookie。已确认其响应只含 status/version 与依赖可达性，不含用量数据 |
| `/login`（新增，GET+POST） | 豁免 | 登录入口本身 |
| `/app.css`、`/login` 所需的最小 CSS | 豁免 | 否则登录页无样式 |
| `/app.js` | **不豁免** | 706 行业务逻辑，含全部 API 调用路径与字段结构。未登录者没有理由读到 |
| `/` | 不豁免 | 未登录时 302 到 `/login` |
| 14 个 `/api/*` | 不豁免 | 用量与 Key 数据 |
| `/openapi.json`、`/docs`、`/docs/oauth2-redirect`、`/redoc` | **不豁免** | 见修正一。这是最容易漏的一组 |

补充建议（advisory）：生产环境可直接关掉自动文档（`FastAPI(docs_url=None, redoc_url=None, openapi_url=None)`），按配置项开关。鉴权已能挡住，关掉是纵深防御，二者不互斥。

静态资源的处理：登录页只依赖 `/app.css`，故只豁免 CSS。若嫌 CSS 也不该外露，可把登录页样式内联进 `/login` 的 HTML，实现零静态资源豁免——推荐这条，登录页样式量很小。

---

## 2. 转发层设计

### 2.1 HTTP 客户端选型：httpx

| 维度 | httpx | aiohttp |
|---|---|---|
| 已在项目内 | 是（dev 依赖，测试用它调 FastAPI） | 否 |
| 运行镜像内 | **否，需提升为运行时依赖** | 否，需新增 |
| 与 FastAPI 生态 | Starlette `TestClient` 即基于 httpx | 无关系 |
| 连接池复用 | `AsyncClient` 长生命周期持有 | 同 |

**选 httpx。** 决定性理由是它已经是项目的测试客户端（`requirements-dev.txt` 里就有），团队对其 API 已熟悉，且版本已锁定 `0.28.1`。引入 aiohttp 等于让项目里同时存在两套 HTTP 客户端语义。

必须做的配套改动：**把 `httpx==0.28.1` 从 dev 依赖提升到 `requirements.txt`**。当前 `Dockerfile` 只 `pip install -r requirements.txt`，不改就是运行时 `ImportError`。

客户端生命周期：在 `lifespan` 里创建**单个** `AsyncClient` 存入 `app.state`，关闭时 `aclose()`。不要每次请求新建——新建会丢掉连接池，且在高频操作下耗尽本地端口。

### 2.2 超时设置

分维度设置，不用单一总超时：

```python
httpx.Timeout(connect=3.0, read=<按端点>, write=10.0, pool=3.0)
```

| 端点 | read 超时 | 依据 |
|---|---|---|
| `PUT /admin/keys/{id}/ip` | 15s | `Rebind` 是内存操作 + 关闭旧连接，很快；给足余量 |
| `PATCH /admin/keys/{id}` | 15s | 单行 UPDATE + 内存状态设置 |
| `POST /admin/keys` | **90s** | 见下 |

`POST /admin/keys` 需要显著更长的 read 超时，因为网关侧是**逐条 upsert**（`admin.go` 的循环，每条一次 `UpsertVolcKey`，每条还要做一次 AES-GCM 加密并 `egress.Bind`），导入 1000 个 Key 就是 1000 次数据库往返。按每条 20-50ms 估，1000 条需 20-50 秒。设 15s 会让大批量导入在网关**已经写入部分数据**后被看板判定超时——运维看到「失败」，实际库里已有几百条，这是最糟的状态。

`connect=3.0` 单独设短：网关不可达应当快速失败，而不是等 90 秒。

### 2.3 错误透传：转译而非裸传

网关错误结构（`server.go` 的 `errorEnvelope`）：

```json
{"error": {"message": "...", "type": "...", "code": "..."}}
```

看板既有错误结构（`main.py` 的 `StoreUnavailable` handler 与 FastAPI 默认）：

```json
{"detail": "..."}
```

**决策：转译为看板的 `{"detail": ...}` 结构，同时保留网关原始 `code`。**

```json
{"detail": "<人类可读文案>", "gateway_code": "<原始 code>", "gateway_status": 503}
```

理由：前端 `app.js` 的 `fetchJSON` 已经统一按 `body.detail` 取错误文案（第 88-99 行实测）。裸传网关结构会让前端对管理端点走一套、对报表端点走另一套解析逻辑，706 行原生 JS 里再分叉一条错误路径，是净负担。保留 `gateway_code` 供排障，不参与前端展示决策。

状态码映射：

| 网关返回 | 看板对外 | 说明 |
|---|---|---|
| 200 / 201 | 原样 | |
| 400 | 400 | 请求体问题，透传网关文案 |
| 401 | **500** | 关键：网关返回 401 意味着**看板持有的 `ADMIN_API_KEY` 错了**，这是看板的配置错误，不是用户未登录。透传 401 会让前端误判为「会话过期」并跳登录页，运维将陷入「反复登录仍失败」的死循环 |
| 405 | 500 | 看板发错了方法，属看板 bug |
| 503 | 503 | 网关容量问题，原样传 |
| 其他 5xx | 502 | 上游故障，语义上看板是网关的代理 |

401 那条是本节最容易写错的地方，必须落实到代码注释里。

### 2.4 转发失败的反馈

三类失败要给出不同文案，因为处置动作完全不同——这条沿用 `integration-report.md` 里 503 文案误导那次教训：

| 情况 | 异常 | 对外 | 文案 |
|---|---|---|---|
| 网关不可达 | `httpx.ConnectError` | 502 | 「网关服务不可达，请检查 gateway 容器状态」 |
| 连接超时 | `httpx.ConnectTimeout` | 504 | 同上 |
| 读超时 | `httpx.ReadTimeout` | 504 | **「请求已发出但未在超时内返回，操作可能已生效，请刷新后确认，不要直接重试」** |
| 网关 5xx | — | 502 | 透传网关文案 |

读超时的文案是重点。`POST /admin/keys` 是逐条 upsert 且**不是单个事务**，超时时网关很可能已写入部分 Key。此时提示「请重试」会让运维重复导入——虽然 upsert 幂等（`integration-report.md` 已验证），但会掩盖「上次到底成功了多少」，且第二次同样可能超时。正确的引导是先刷新 Key 列表确认实际状态。

大请求体的处理：

- 看板侧对 `POST /admin/keys` 的请求体设 **8MB 上限**，与网关 `admin.go` 的 `http.MaxBytesReader(w, r.Body, 8<<20)` 对齐。不对齐的后果是看板放过了 10MB 请求，转发到网关才被拒，白白耗费一次传输
- 转发时**流式透传**请求体，不在看板侧 `await request.json()` 再重新序列化。后者会把 8MB 数据在看板内存里翻倍，且 JSON 重序列化可能改变字段顺序与数值表示（如大整数、浮点精度），让网关收到的内容与运维提交的不完全一致
- 超过上限返回 413 并明确指出上限值

### 2.5 CSRF 防护

**结论：`SameSite=Strict` 为主，叠加 Origin/Referer 校验作为第二道。不引入 CSRF token。**

先说清 `SameSite=Strict` 挡住了什么、没挡住什么：

- 挡住：所有跨站发起的请求都不携带 `fk_session`。攻击者构造的恶意页面无论用 form POST、fetch 还是 img 标签，Cookie 都不会被带上，请求在鉴权层即失败
- 没挡住：同站内的其他内容。但看板是单一应用独占源，没有用户生成内容，不存在「同站不同应用」的场景
- 没挡住：浏览器不支持 `SameSite` 的老版本。当前部署面向内部运维，浏览器可控

为什么仍要叠加 Origin 校验而不是就此收手：`SameSite` 的有效性完全依赖浏览器正确实现，而这是**单一防线**。Origin/Referer 校验的成本极低（约 10 行），且防的是不同层面的问题（请求来源），两者失效模式不重叠。对所有非 GET 请求：

```
若 Origin 存在且不在允许列表 → 403
若 Origin 缺失但 Referer 存在且主机不匹配 → 403
两者都缺失 → 放行（curl 等非浏览器客户端；它们本来就不受 CSRF 影响）
```

「两者都缺失则放行」这条需要解释：CSRF 的前提是**浏览器自动携带凭据**，而浏览器发起的跨站请求必定带 `Origin`。命令行客户端不带 `Origin`，但它们也不会自动带上别人的 Cookie，不构成 CSRF。若强行要求 `Origin` 必须存在，会把 `curl` 调试路径堵死而不带来实际安全收益。

为什么不做 CSRF token：无状态会话下，token 要么另存服务端（回到 1.1 的 C 方案，破坏只读不变量），要么用 double-submit cookie 模式。后者在 `SameSite=Strict` 已生效的前提下，防护增量接近于零——能读写 Cookie 的攻击者早已突破同源限制。而代价是前端 706 行 JS 里每个写请求都要取 token 并塞进头，且登录页要多一轮握手。收益与复杂度不成比例。

允许的 Origin 来源：由 `DASHBOARD_ALLOWED_ORIGINS` 配置（逗号分隔），缺省为空表示只接受同源（由 `Host` 头推导）。**不要配 `*`**。

---

## 3. 新增 `PATCH /admin/keys/{key_id}` 契约

### 3.1 路由注册与冲突规避（本节结论经算法验证）

**这是最容易出事的一处**，因为 `/admin/keys/` 已作为子树注册给 `handleAdminKeyByID`。

本机无 Go 工具链，故不凭记忆断言。做法是从 `go1.23.4` 源码取到 `net/http` 的 `ServeMux` 文档与 `pattern.go` 的冲突判定算法，逐条移植后验证。移植版先用 Go 文档自身断言的案例自校验（如文档明确「`GET /` 与 `/index.html` 冲突」、「`/images/thumbnails/` 比 `/images/` 更具体，两者可同时注册」），全部复现一致后再判定本项目的模式对。

Go 1.23.4 的规则原文（`server.go`）：

> If two or more patterns match a request, then the most specific pattern takes precedence. A pattern P1 is more specific than P2 if P1 matches a strict subset of P2's requests... If neither is more specific, then the patterns conflict.

> A trailing slash in a path acts as an anonymous "..." wildcard.

验证结果：

| 新模式 | 对既有 `/admin/keys/` | 结论 |
|---|---|---|
| `PATCH /admin/keys/{key_id}` | `moreSpecific` | **不冲突，可共存，且新模式优先** |
| `PATCH /admin/keys/{key_id}` | 对 `PUT /admin/keys/{key_id}/ip` 为 `disjoint` | 互不干扰 |

关键机制：`/admin/keys/` 的尾斜杠等价于匿名 `...` 多段通配，而 `{key_id}` 是单段通配。按 `compareSegments`，多段通配比单段更一般，故 `PATCH /admin/keys/{key_id}` 是严格子集 → `moreSpecific` → 优先级更高，不 panic。

因此 `PATCH /admin/keys/volc_001` 会命中新 handler，而 `PUT /admin/keys/volc_001/ip` 仍命中原有 `handleAdminKeyByID`（两段路径，单段通配匹配不到）。

**实现要求**：

1. 新增注册必须写成 `s.mux.Handle("PATCH /admin/keys/{key_id}", s.adminChain(s.handleAdminKeyPatch))`，用 Go 1.22+ 增强模式带上方法与命名通配，通过 `r.PathValue("key_id")` 取值。**不要**沿用 `strings.TrimPrefix` 手工切路径——那样必须挂在 `/admin/keys/` 上，与既有 handler 争夺同一模式。
2. 注册顺序不影响结果（优先级由模式具体性决定，非注册顺序），但仍建议紧挨 `/admin/keys/` 那行，便于阅读时看到两者关系。
3. **必须补一条注册期回归测试**：断言 `New()` 不 panic，且 `PATCH /admin/keys/x` 与 `PUT /admin/keys/x/ip` 各自路由到预期 handler。理由——路由冲突是**注册时 panic**，即进程起不来；这类故障若只在生产暴露，代价远高于一条测试。
4. `key_id` 为空的情况：`PATCH /admin/keys/` 不会命中新模式（单段通配不匹配尾斜杠，已验证为 `disjoint`），会落到既有 `handleAdminKeyByID` 并返回其 404 文案。可接受，但建议在 `handleAdminKeyByID` 的错误文案里补充提示。

### 3.2 请求体

```
PATCH /admin/keys/{key_id}
Authorization: Bearer <ADMIN_API_KEY>
X-Admin-Actor: <操作者标识>
Content-Type: application/json
```

```json
{
  "status": "banned",
  "pool": "cold",
  "persona_id": "p_night",
  "expected_status": "active",
  "reason": "火山侧提示异常，先隔离观察"
}
```

| 字段 | 类型 | 必填 | 语义 |
|---|---|---|---|
| `status` | string | 否 | 目标状态，取值 `active`/`cooldown`/`banned`/`invalid` |
| `pool` | string | 否 | 目标池，取值 `hot`/`warm`/`cold` |
| `persona_id` | string | 否 | 行为画像标识 |
| `expected_status` | string | 否 | 乐观并发控制，见 3.4 |
| `reason` | string | 否 | 变更原因，只进审计不进业务表 |

语义规则：

- **字段缺席 = 不改该字段**，与 `null` 等价处理。三个可改字段全部缺席时返回 400（「没有需要变更的字段」），不返回 200——否则调用方无法区分「改了」和「什么都没改」。
- **`egress_ip` 刻意不在本端点内。** 出口 IP 变更必须走既有 `PUT /admin/keys/{id}/ip`，因为它需要 `egress.Rebind()` 丢弃旧连接这一副作用。若在 PATCH 里放开 `egress_ip`，会出现「库里 IP 改了但连接仍走旧 IP」的静默不一致——绑定变更形同虚设。这条与 `admin.go` 现有注释的判断一致。
- **`health_score` 不开放。** 健康分是运行时观测值（`health.go` 注释已说明只在单实例内存中有意义），人工改写会立刻被下一次成功/失败请求覆盖，给运维「改了但没用」的错觉。要恢复一个 Key 应改 `status`。

**实现陷阱（务必避开）**：判断「调用方是否提供了某字段」不能靠 Go 零值。`status: ""` 与字段缺席在 `encoding/json` 解成 `string` 后不可区分。必须用 `*string` 指针（`nil` = 缺席）或 `json.RawMessage`。这正是 `integration-report.md` 第二节 `EXCLUDED` 被 `VALUES` 兜底污染那个 bug 的同类形态——两者都是「无法区分未提供与空值」，后果都是静默改写不该改的字段。`store.VolcKeyState` 已经用了 `*string`，直接复用。

### 3.3 状态机约束

允许的转换：

| 从 → 到 | `active` | `cooldown` | `banned` | `invalid` |
|---|---|---|---|---|
| `active` | 幂等 | 允许 | 允许 | 允许 |
| `cooldown` | 允许 | 幂等 | 允许 | 允许 |
| `banned` | **需 `force`** | 允许 | 幂等 | 允许 |
| `invalid` | **需 `force`** | 允许 | 允许 | 幂等 |

`banned`/`invalid` → `active` 需要额外确认，请求体加 `"force": true`，否则返回 409 并说明原因。

理由：`health.go` 把这两个状态当**终态**处理（`markSuccess` 的注释明确「banned/invalid 是终态，不自动恢复」，`available()` 对二者直接返回不可用）。`invalid` 是 `FailureAuth` 触发的——即上游明确拒绝了该 Key 的鉴权。把这样一个 Key 直接放回流量，若鉴权问题未解决，它会立刻再次失败并被扣健康分，同时向火山侧多贡献一次异常请求。而封禁与异常请求恰恰是本项目最敏感的信号。要求显式 `force` 是让运维确认「我知道这是终态且已排查过」。

不做的约束：不限制 `active → banned` 这类收紧方向。隔离一个可疑 Key 必须随时可做，加确认步骤只会延误处置。

`pool` 转换无状态机约束——已核实 `pool` 字段在调度器中仅被 `Reload` 读入 `keyEntry.pool` 后未参与任何打分或过滤（`scheduler.go` 全文仅两处引用），当前是纯标签。**这一点要在实现时写进注释**，避免后人误以为改 pool 会立即改变调度行为。

### 3.4 幂等性

**端点整体幂等**：相同请求体重复执行，最终状态一致。已用真实 Postgres 验证「同值重放」返回相同结果且不产生额外变更。

- 相同目标值重复提交返回 200，不报错。理由同 `POST /admin/keys` 的 upsert 语义：运维重跑脚本应当安全
- 幂等**不代表无副作用**：每次调用都写一条审计。这是有意的——「谁试过改这个 Key」本身是排障信息，去重会丢掉「反复尝试」这一信号
- 可选的乐观并发控制：`expected_status` 不匹配当前状态时返回 409。SQL 在单条语句内完成「条件匹配 + 取旧值 + 写新值」：

```sql
UPDATE volc_keys AS t
   SET status = COALESCE($2, t.status),
       pool   = COALESCE($3, t.pool),
       persona_id = COALESCE($4, t.persona_id),
       updated_at = now()
  FROM volc_keys AS old
 WHERE t.key_id = old.key_id
   AND t.key_id = $1
   -- 条件必须挂在 t 上，绝不能写成 old.status = $5。见下方警告。
   AND ($5::text IS NULL OR t.status = $5)
RETURNING old.status AS prev_status, old.pool AS prev_pool,
          t.status AS new_status, t.pool AS new_pool
```

实测四种情形均正确：正常改写返回新旧值、`expected_status` 不匹配返回 0 行、Key 不存在返回 0 行、单字段更新不影响其他字段。

> **警告：乐观并发条件绝不能挂在自连接的 `old` 上。**
>
> 本文档早期版本写的是 `AND ($5::text IS NULL OR old.status = $5)`。这在**顺序调用下完全正确**（上面四种情形全过），但**并发下静默失效**。
>
> 机制：两个并发 PATCH 争同一行时，后到者阻塞在行锁上。前者提交后，PostgreSQL 的 EvalPlanQual 重检只对**更新目标行**重新求值 WHERE，而 `FROM` 侧的 `old` 关系仍沿用语句开始时的快照。于是后到者解除阻塞后看到的 `old.status` 还是它进来时的旧值，条件照样成立，更新被放行。
>
> 两个真实会话实测：
> ```
> 条件挂 old：A 改 banned 提交 → B 仍成功改 invalid，最终值 invalid   ← 两个写入互相覆盖
> 条件挂 t  ：A 改 banned 提交 → B 被拒绝（UPDATE 0），最终值 banned  ← 正确
> ```
>
> 写在 `t.status` 上时，WHERE 读到的就是本行更新前的值，语义与判定意图一致；`old` 只保留给 `RETURNING` 取旧值。
>
> 这类缺陷属于「沉默逻辑错误」：不报错、不崩溃，只在并发下悄悄放过两个写入，而顺序测试全绿——`expected_status` 会退化成纯装饰。回归测试 `TestPatchVolcKeyState_并发下乐观并发控制严格成立` 起 8 个并发全部声称 `expected_status=active`，断言恰好 1 成功 7 冲突；改回 `old` 写法这条会红（实测成功数 = 2）。

0 行需要区分两种原因（否则 404 与 409 无法分辨）：再查一次该 `key_id` 是否存在——存在则是 409（状态不匹配），不存在则 404。

**与内存状态的同步顺序**：先写库，成功后再 `sched.SetKeyStatus()`。顺序不可换——先改内存后写库失败，会出现「内存已封禁但库里仍 active」，而下一次 `Reload`（5 分钟一轮或导入触发）会用库里的值 `seed` 回来，封禁悄悄失效。

注意 `healthTable.seed()` 对已存在的 Key 不覆盖（「已有实时观测值，不被库中的旧快照覆盖」），所以内存必须显式 `SetKeyStatus` 而不能指望 `Reload` 生效。

### 3.5 响应

成功 200：

```json
{
  "key_id": "volc_001",
  "changed": ["status", "pool"],
  "status": {"from": "active", "to": "banned"},
  "pool": {"from": "hot", "to": "cold"},
  "persona_id": {"from": "p_day", "to": "p_day"}
}
```

回显新旧值而非仅回显新值：运维需要确认「改动是否如预期」，而 `from` 是唯一能证明「改之前确实是那个值」的凭据。`changed` 数组让调用方一眼看出实际生效的字段。

错误码：

| 状态 | `code` | 触发 |
|---|---|---|
| 400 | `invalid_request` | 请求体非法 / 无可变更字段 / 枚举值非法 |
| 404 | `invalid_request` | `key_id` 不存在 |
| 409 | `invalid_request` | `expected_status` 不匹配，或终态转 `active` 未带 `force` |
| 500 | `internal_error` | 数据库错误 |

沿用 `s.writeError` 的既有结构，不引入新错误格式。

### 3.6 审计

`action` = `patch_volc_key`，`target` = `key_id`，`detail` 内容：

```json
{
  "changed": ["status", "pool"],
  "status_from": "active", "status_to": "banned",
  "pool_from": "hot", "pool_to": "cold",
  "forced": false,
  "expected_status": "active",
  "reason": "火山侧提示异常，先隔离观察",
  "request_id": "req_..."
}
```

三条要求：

- **记新旧值双方**。只记新值的审计无法回答「这个 Key 是什么时候从 active 变成 banned 的」——而这正是 `admin.go` 开头注释给出的审计存在理由
- **`forced` 必须单独成字段**，便于日后筛出所有「强制复活终态 Key」的操作
- **绝不记 secret**。本端点请求体本就不含 secret，但要在代码注释里钉住这条，防止将来扩展字段时带进来

审计失败只告警不阻断，与 `s.audit` 既有行为一致。

---

## 4. 新增配置项清单

### 4.1 看板侧（`dashboard/app/config.py`）

| 环境变量 | 默认值 | 必填 | 缺失/非法时的行为 |
|---|---|---|---|
| `DASHBOARD_PASSWORD` | 无 | **是** | **启动失败**，日志指明「未配置看板口令，管理控制台拒绝启动」 |
| `DASHBOARD_SESSION_SECRET` | 无 | **是** | **启动失败**（见下方说明，这里刻意不自动生成） |
| `GATEWAY_ADMIN_API_KEY` | 无 | **是** | **启动失败**，无此项则转发必然 401 |
| `GATEWAY_BASE_URL` | `http://gateway:8080` | 否 | 用默认值。compose 内服务名可达 |
| `DASHBOARD_COOKIE_SECURE` | `false` | 否 | 用默认值，并在启动日志打印告警 |
| `DASHBOARD_SESSION_TTL` | `7200` | 否 | 非法值回退默认，与既有 `_env_int` 行为一致。**注意这不只是「多久要重新登录」，它同时是登出后 token 可被重放的最长时间**，调大即放大风险窗口（见 1.3） |
| `DASHBOARD_ALLOWED_ORIGINS` | 空 | 否 | 空表示只接受同源 |
| `DASHBOARD_LOGIN_MAX_ATTEMPTS` | `5` | 否 | 回退默认 |
| `DASHBOARD_LOGIN_LOCKOUT_SECONDS` | `900` | 否 | 回退默认 |
| `GATEWAY_TIMEOUT_IMPORT` | `90` | 否 | 回退默认 |
| `DASHBOARD_DOCS_ENABLED` | `false` | 否 | 生产默认关闭自动文档 |

三项必填的一致处理：缺失时**抛异常终止启动**，参照 `FLUXKEYS_ENCRYPTION_KEY` 的既有约定。

需要特别说明的是，这与看板现有的「连接失败不阻止启动」原则**并不矛盾**。两者针对的是不同性质的问题：

- 数据库/Redis 连不上是**环境的瞬时状态**，可能自行恢复，所以启动后用 `/healthz` 暴露、让容器留在运行状态更利于排障（`db.py` 的 `connect()` 注释已说明）
- 口令/密钥缺失是**配置错误**，不会自行恢复，且静默降级的后果是管理控制台裸奔或全体登录失败。这类必须 fail fast

`DASHBOARD_SESSION_SECRET` 为何不自动随机生成：自动生成会让「重启不掉线」和「多副本共享」两项设计目标同时失效，而且失效方式是隐蔽的——功能看起来正常，只是用户偶尔莫名掉线。宁可启动失败，让人一次性配好。

建议校验：`DASHBOARD_SESSION_SECRET` 长度不足 32 字符即拒绝启动，并在文档中给出生成命令 `python -c "import secrets; print(secrets.token_urlsafe(32))"`。

`get_settings()` 当前有 `@lru_cache(maxsize=1)`，新增必填项的校验应放在 `get_settings()` 内，这样进程内只校验一次，且任何调用方都无法拿到未校验的配置。

### 4.2 网关侧

**网关无需新增任何配置项。** `PATCH /admin/keys/{key_id}` 复用既有 `Admin.APIKey`，沿用「`APIKey` 为空则整组管理路由不注册」的现有约定——新端点自动继承这一保护，无需额外开关。

刻意不加 `ADMIN_ALLOW_PATCH` 这类细粒度开关：它会制造一种「管理接口可以部分开启」的错觉，而实际风险边界是「管理接口是否可达」这一条。多一个开关多一处配错的机会。

### 4.3 compose 与 .env.example 配套改动

`docker-compose.yml` 的 `dashboard.environment` 需补：

```yaml
      DASHBOARD_PASSWORD: ${DASHBOARD_PASSWORD:?DASHBOARD_PASSWORD 未设置}
      DASHBOARD_SESSION_SECRET: ${DASHBOARD_SESSION_SECRET:?DASHBOARD_SESSION_SECRET 未设置}
      GATEWAY_ADMIN_API_KEY: ${ADMIN_API_KEY:?ADMIN_API_KEY 未设置}
      GATEWAY_BASE_URL: http://gateway:8080
```

用 `:?` 语法与既有 `POSTGRES_PASSWORD` 保持一致，让 compose 在启动前就报错。

注意 `GATEWAY_ADMIN_API_KEY` 复用根级 `ADMIN_API_KEY` 变量——网关与看板必须用同一个值，拆成两个变量必然出现两边不一致而 401。

`.env.example` 需补 `DASHBOARD_PASSWORD` 与 `DASHBOARD_SESSION_SECRET` 两行（占位符沿用 `CHANGE_ME_` 前缀风格）。

端口绑定**保持 `127.0.0.1` 不变**。加了鉴权不等于可以对外暴露：鉴权是纵深防御的一层，不是取代网络隔离。

---

## 5. 前端管理界面：图标与样式约束

### 5.1 图标方案：内联 SVG sprite，锁定一套

**方案：在 `index.html` 底部内联一个 `<svg>` sprite，用 `<symbol id="icon-*">` 定义，引用处写 `<svg class="icon"><use href="#icon-ban"></use></svg>`。**

图标来源锁定 **Lucide**（MIT 许可，24×24 网格，`stroke` 风格与看板现有深色主题一致）。只从中挑选所需的 6-8 个图标，手工复制其 path 进 sprite，**不引入 npm 包、不引入 CDN**。

选它的理由：

| 方案 | 评价 |
|---|---|
| emoji | **禁止**（P0 规则）。且 emoji 跨平台渲染不一致，深色主题下彩色 emoji 与克制的配色冲突 |
| 图标字体（Font Awesome 等） | 需额外网络请求，字体文件体积远大于 8 个图标的实际需要，且字体加载失败时显示豆腐块 |
| 独立 `.svg` 文件 | 每个图标一次 HTTP 请求；且现有 `main.py` 是逐文件手写路由（`/app.js`、`/app.css`），每加一个图标要加一条路由，或者引入 `StaticFiles` 挂载——后者又扩大了鉴权豁免面 |
| **内联 SVG sprite** | **零额外请求、零新增路由、无构建步骤、可被 CSS 变量着色** |

关键优势是 `currentColor`：sprite 里的 `stroke="currentColor"` 会继承 CSS 的 `color`，因此图标颜色自动跟随既有 `--up` / `--ok` / `--warn` 变量，无需为图标单独定义颜色。

需要的图标（全部来自 Lucide，按功能命名而非按外观命名）：

```
icon-import      上传/导入 Key
icon-network     出口 IP 变更
icon-ban         封禁
icon-restore     恢复
icon-layers      改池子
icon-alert       危险操作确认
icon-check       操作成功
icon-logout      退出登录
```

统一尺寸类：

```css
.icon { width: 15px; height: 15px; stroke: currentColor; fill: none;
        stroke-width: 2; vertical-align: -2px; }
```

**一致性要求**：全项目只用这一套。禁止在某些按钮用 SVG、另一些用 emoji 或叉号/对勾之类的 Unicode 符号字符。已知现有 `app.js` 用了上下箭头字符做表格排序指示（`.arrow` 相关）——这属于既有代码，本次不强制改，但**新增的管理界面一律用 sprite**，且若后续统一，方向是把箭头也并入 sprite。

### 5.2 配色约束

- **禁止紫色→粉色渐变**，不使用任何渐变作为主视觉
- **禁止硬编码颜色值**（`#fff`/`#000` 除外），全部走 CSS 变量
- 危险操作（封禁、强制复活）用既有 `--up`（语义为「消耗上升/告警」的红）；成功用 `--ok`；确认弹层背景用 `--panel-2`，边框用 `--border`
- 新增管理界面**不引入新的颜色变量**——现有 `:root` 里的 15 个变量（`--bg` `--panel` `--panel-2` `--border` `--text` `--text-dim` `--text-faint` `--up` `--up-soft` `--ok` `--ok-soft` `--warn` `--warn-soft` `--info` `--accent`）已覆盖所需语义。若确实需要，须新增到 `:root` 而非就地写值

已核实 `app.css` 存在若干裸色值（分布在 `.gauge .bar` 的进度槽底色、`.alert.critical` 的背景、四个 `.tag-*` 的边框色、`.err-banner` 的文字色）。这些是既有代码，本次不纳入改动范围，但**新增样式不得沿用这种写法**。若要顺手收敛，应把它们提为 `:root` 变量而非就地保留。

### 5.3 危险操作的确认交互

封禁与「强制复活终态 Key」必须二次确认。确认弹层要显示：

- 目标 `key_id`
- 当前状态 → 目标状态
- 对于强制复活：明确提示「该 Key 处于终态，通常意味着上游已拒绝其鉴权或已被封禁。若根因未排查，恢复后会立即再次失败并向上游贡献异常请求」

不用 `window.confirm()`：它无法承载上面这段说明，且样式不可控，与深色主题割裂。用现有 `.panel` / `.border` 变量手写一个轻量弹层即可，无需引入组件库。

---

## 6. 实施顺序与验收要点

建议顺序（每步可独立验证，避免大爆炸式改动）：

1. **网关 `PATCH` 端点** —— 先做这个，它与看板改动完全解耦。验收：注册期不 panic 的回归测试 + 状态机各转换的单测 + `PUT .../ip` 仍正常路由
2. **看板依赖与配置** —— `httpx` 提升为运行时依赖、新增配置项与必填校验。验收：缺任一必填项时容器启动失败且日志明确
3. **看板鉴权中间件与登录页** —— 验收：18 个路由逐一确认鉴权状态（特别是 `/openapi.json` `/docs` `/redoc`）；登录限流生效；Cookie 属性正确
4. **看板转发层** —— 验收：三个转发端点各自的成功路径；网关停掉时的错误文案；401 被转成 500 而非透传
5. **前端管理界面** —— 验收：sprite 图标渲染；无 emoji；无硬编码色值；危险操作二次确认

必须新增的测试（对应本项目已有的「装配完好但不干活」教训）：

- 网关：路由注册不冲突（这是 panic 级问题）
- 网关：`status` 字段缺席时不改动原状态（防重演 `EXCLUDED` 那类 bug）
- 看板：只读连接池仍拒绝写操作（防有人为了图方便偷偷去掉 `default_transaction_read_only`）
- 看板：未登录访问 `/openapi.json` 返回 302/401
- 看板：`ADMIN_API_KEY` 不出现在任何响应体或前端产物中

最后一条建议用 grep 式断言实现，与 `integration-report.md` 里「明文泄漏检查 = 0」的做法一致。
