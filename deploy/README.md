# FluxKeys 部署手册

单机部署（架构 v4 P0-5：单进程单实例 + Redis 单节点 + Postgres 单节点）。

目录：

- [快速开始](#快速开始)：起完整栈并做冒烟测试
- [生产部署](#生产部署)：含出口 IP 配置
- [生产切换清单](#生产切换清单)
- [故障排查](#故障排查)
- [指标契约](#指标契约)

---

## 快速开始

`docker compose up` 起 Redis / Postgres / 网关 / 看板 / 监控栈，但**不再内置假上游**：
上游默认指向真实火山地址（`https://ark.cn-beijing.volces.com`），业务链路需要真实火山 Key。

```bash
# 1. 生成配置
cp .env.example .env

# 2. 填三个必填密钥
#    注意 FLUXKEYS_ENCRYPTION_KEY 必须是 32 字节 hex（64 字符），否则网关拒绝启动
openssl rand -hex 32   # -> FLUXKEYS_ENCRYPTION_KEY
openssl rand -hex 32   # -> ADMIN_API_KEY
openssl rand -hex 24   # -> POSTGRES_PASSWORD

# 3. 启动
docker compose up -d --build

# 4. 冒烟测试
bash scripts/smoke-test.sh
```

> 没有真实 Key 时想验证整条链路，改用进程内假上游的集成测试：
> `go test ./test/...` —— 假上游位于 `test/mockark`，随测试进程启动，
> 不依赖任何外部服务（Redis/Postgres 也由内存实现替代）。

访问入口：

| 服务 | 地址 | 说明 |
|---|---|---|
| 网关 | http://127.0.0.1:8080 | OpenAI 兼容 API |
| 指标 | http://127.0.0.1:9090/metrics | 仅绑回环 |
| 看板 | http://127.0.0.1:8000 | 只读报表，仅绑回环（容器内监听 8081）|
| Prometheus | http://127.0.0.1:9091 | 仅绑回环 |
| Grafana | http://127.0.0.1:3000 | 仅绑回环，默认 admin/admin |

> Postgres 与 Redis **默认不映射到宿主机**。它们都没有 TLS，暴露出去等于开放配额状态与全部业务数据的写入权限。
> 调试用 `docker compose exec postgres psql -U fluxkeys` / `docker compose exec redis redis-cli`。

### 关于 `FLUXKEYS_ENCRYPTION_KEY`

这个密钥用于加密 `volc_keys.secret_enc`。**丢了就永久解不开库里所有火山 Key**。

轮换必须「用旧密钥全量解密 → 用新密钥重新加密」，不能直接替换值。
`internal/store/crypto.go` 刻意在密钥缺失时让启动失败，而非静默降级为明文存储 ——
明文落库意味着数据库备份、慢查询日志、只读看板任一环节泄漏都等于全部 Key 泄漏。

### 初始化用户

网关对外提供服务需要用户 API Key（`user_api_keys` 表）。全新部署时该表为空，
此时 `/v1/chat/completions` 会返回 401 —— 这是正常的，不是故障。
`smoke-test.sh` 遇到 401 会判为「链路可达、鉴权生效」并提示。

创建用户与 Key 的具体接口由网关的 admin API 提供，用 `ADMIN_API_KEY` 鉴权。

---

## 生产部署

生产与本地有两处本质差异，缺一不可：

1. **出口 IP**：`EGRESS_MODE=multi_ip`，且必须先在宿主机配好策略路由
2. **网络模式**：gateway 走 host 网络（否则容器内看不到宿主机的辅助 IP）

### 第 1 步：配置多 EIP 出口（最关键）

这一步不做，「按 Key 绑定出口 IP」会**静默失效**。详见 [P1-6 的失效机理](#出口-ip-不生效)。

```bash
# 先演练，确认将要执行的操作
sudo bash scripts/setup-egress.sh --dry-run --ips '172.16.0.11=203.0.113.11,172.16.0.12=203.0.113.12'

# 正式执行 + 持久化（--persist 写 systemd unit，重启后自动恢复）
sudo bash scripts/setup-egress.sh --persist --ips '172.16.0.11=203.0.113.11,172.16.0.12=203.0.113.12'
```

脚本会做四件事，其中第 4 件和前三件一样重要：

1. 把辅助 IP 加到网卡上
2. 为每个 IP 建独立路由表（`table 100`、`101`…）与默认路由
3. 加 `ip rule from <ip> lookup <table>` 策略规则
4. **用 `curl --interface <ip>` 逐个探测，打印每个 IP 实际出口的公网地址**

第 4 步的判定标准有三条，全部满足才算通过：

- 每个 IP 都能成功出网
- 各 IP 的出口公网地址**互不相同**
- 若声明了期望 EIP，实际值与声明一致

输出示例（通过）：

```
  172.16.0.1       -> 203.0.113.1    (主地址，对照基准)
  172.16.0.11      -> 203.0.113.11   ✓ 与声明一致
  172.16.0.12      -> 203.0.113.12   ✓ 与声明一致
[ OK ]  验证通过：2 个出口 IP 各自使用独立公网出口
```

输出示例（失败，这正是静默失效的样子）：

```
  172.16.0.1       -> 203.0.113.1    (主地址，对照基准)
  172.16.0.11      -> 203.0.113.1    ✗ 与主地址出口相同，策略路由未生效！
  172.16.0.12      -> 203.0.113.1    ✗ 与主地址出口相同，策略路由未生效！
[FAIL]  出口公网地址不唯一：2 个 IP 只对应 1 个不同出口
```

脚本幂等，可反复执行。验证失败返回非零退出码，可直接用于部署门禁。

### 第 2 步：生成 `.env`

生产 compose 的**所有参数都有开箱默认值**（无 `.env` 也能一条命令起栈），
但占位默认密钥只可用于验证。正式部署一条命令生成强随机密钥：

```bash
bash scripts/gen-prod-env.sh
```

脚本幂等（已有 `.env` 拒绝覆盖），生成后唯一要人工填的是 `EGRESS_IPS`
（与机器网卡绑定，无法代填）。手工编辑可参考 `.env.example` 的「生产专用」段：

```bash
COMPOSE_PROFILES=monitoring,backup  # tls profile 见第 5 步
EGRESS_IPS=172.16.0.11=203.0.113.11,172.16.0.12=203.0.113.12
FLUXKEYS_IMAGE_TAG=v1.0.0       # 升级时用具体版本；首跑默认本地构建即可
```

其余关键项的默认行为都写死在生产 compose 文件里，无需配置：

- `EGRESS_MODE=multi_ip`（可显式改 direct 过渡）、`EGRESS_VERIFY_ON_START=true`、
  `REFRESH_ENABLED=true`
- `VOLC_BASE_URL` 默认**不注入**（留空 = 由配置文件的内置默认值决定，即真实火山
  地址 `https://ark.cn-beijing.volces.com`）。环境变量优先级高于 `-config` YAML，
  一旦注入就会盖掉配置文件里的地址 —— 夹具/联调把 volc 指向假上游时，必须确认
  它确实是空的，否则配置看着改对了、流量仍打到真实火山
- `ADMIN_API_KEY` 默认为空 = 管理接口与看板管理操作禁用（刻意不给仓库内置的
  已知密钥），由 gen 脚本生成；裸跑时可内联传递：
  `ADMIN_API_KEY=$(openssl rand -hex 32) docker compose up -d`

> **`REFRESH_ENABLED=true` 是硬要求。**
> 火山 12:00 刷新配额，而配额日边界也在 12:00（P0-3）。关闭刷新探测后，
> 系统会在跨过 12:00 时把所有 Key 当作「新一天、满额度」来调度，
> 但实际是否已刷新完全未知 —— 对已耗尽的 Key 持续发请求就是超刷，直接导致封号。
> 探测器的作用是「只在确认已刷新后才恢复调度」（P0-4）。
> 本地默认 `false` 是为了不干扰调试，**上线忘记打开是最常见的严重配置错误**。

#### 出口 IP 分层（Key 数超过 IP 数 × 10 时必需）

`EGRESS_IPS` 的单项语法为 `<addr>[=<public_ip>][|<pool>[|<max_keys>]]`，
用 `|` 而非 `:` 分隔以避免与 IPv6 地址冲突。`pool` 取 `hot` / `warm` / `cold`，
留空表示通用 IP（接受任何档位的 Key）。

决定 `max_keys` 的不是「一个 IP 上绑了多少 Key」，而是**这个 IP 在同一时刻
产生多少请求**。这取决于两件事：

1. **同时活跃的 Key 数**——由行为画像决定，窄化后约占绑定数的 20-40%。
2. **每个 Key 被选中的频率**——由 `scheduler.pool_shares` 决定。

两者相乘才是风控观测到的密度，本文称之为**等效密度**：

```
等效密度 = 画像峰值并发 × 该档单 Key 的相对请求频率
```

默认份额 `hot:warm:cold = 70:25:5` 下，实测单 Key 相对频率为
**hot 1.000 / warm 0.181 / cold 0.010**（1000 个 Key、20000 次调度采样）。
cold 档 700 个 Key 合计只拿 5% 的流量，摊到每个 Key 上仅为 hot 档的 **1%**。

##### 推荐配置：按档位分级 `max_keys`

| 档位 | max_keys | 满载时画像峰值 | 相对频率 | 等效密度 |
|---|---|---|---|---|
| hot | 10 | 5 | 1.000 | **5.0** ← 瓶颈 |
| warm | 50 | 17 | 0.181 | 3.1 |
| cold | 100 | 32 | 0.010 | 0.3 |

**hot 档是唯一的瓶颈**：它承接 70% 的流量，每个 Key 都在高频使用，必须保持
低密度。cold 档相反——单 Key 平均每 2 万次请求才被选中 1.4 次，即使绑 100 个
也几乎不产生并发。

##### 换算成绝对速率

等效密度是相对量，只能用来比较档位。真正的验收判据是**单个出口每秒发出多少
请求**——风控看的是这个绝对值，与其他 IP 无关。按实测总流量 20 QPS
（`TestSelect_份额决定单IP请求密度`，采样 2 万次调度）：

| 档位 | Key | IP | 单 IP | 单 Key | 满载单 IP |
|---|---|---|---|---|---|
| hot | 100 | 19 | 0.74 req/s | 503 req/h | — |
| warm | 200 | 5 | **1.01 req/s** ← 最高 | 91 req/h | — |
| cold | 700 | 8 | 0.12 req/s | 5.1 req/h | 508 req/h |

两个关键结论：

- **cold 档绑满 100 个 Key 时单出口约 508 req/h，仍只有 hot 档单出口
  （2647 req/h）的 1/5。**这是 `max_keys=100` 的直接依据——不是「反正它们
  不发请求」，而是实测频率差 99 倍，用低频换高密度是划算的。
- **真正最密的是 warm 档，不是 hot。**它只有 5 个 IP 却吃 25% 流量，
  而 hot 有 19 个 IP 吃 70%。若要进一步压低峰值，优先给 warm 加 IP。

上限取 2 req/s：这是留了一倍余量的经验值，测试对超限硬失败。总流量若从
20 QPS 涨到 40，所有速率同比翻倍，`warm` 档会率先触顶——**扩容时先看这张表
而不是 Key 数量。**

一台 32 IP 的机器容量是 **1000 个 Key**：

```bash
EGRESS_MODE=multi_ip
EGRESS_IPS=\
172.16.0.11|hot|10,172.16.0.12|hot|10,172.16.0.13|hot|10,172.16.0.14|hot|10,\
172.16.0.15|hot|10,172.16.0.16|hot|10,172.16.0.17|hot|10,172.16.0.18|hot|10,\
172.16.0.19|hot|10,172.16.0.20|hot|10,172.16.0.21|hot|10,172.16.0.22|hot|10,\
172.16.0.23|hot|10,172.16.0.24|hot|10,172.16.0.25|hot|10,172.16.0.26|hot|10,\
172.16.0.27|hot|10,172.16.0.28|hot|10,172.16.0.29|hot|10,\
172.16.0.30|warm|50,172.16.0.31|warm|50,172.16.0.32|warm|50,\
172.16.0.33|warm|50,172.16.0.34|warm|50,\
172.16.0.35|cold|100,172.16.0.36|cold|100,172.16.0.37|cold|100,\
172.16.0.38|cold|100,172.16.0.39|cold|100,172.16.0.40|cold|100,\
172.16.0.41|cold|100,172.16.0.42|cold|100
```

按 1:2:7 的 Key 分布（100 / 200 / 700）：hot 19 个 IP、warm 5 个、cold 8 个，
容量 190 / 250 / 800。

**各档 IP 数的下限由撤离余量决定**，而不是「装得下就行」。封掉一个出口后，
同档其余 IP 必须装得下它的 Key。`Bind` 会均摊，单出口负载约为 `keys/n`，故：

```
(n-1) × max_keys >= keys    →    n >= keys/max_keys + 1
```

实测验证（`TestCapacity_撤离余量的临界值`）：cold 档 700 Key / 100 上限时，
**7 个 IP 撤离全部失败、8 个全部成功**，临界点正是这个公式。

按下限只需 hot 11 + warm 5 + cold 8 = 24 个，**余出的 8 个全部补给 hot**——
它是唯一的密度瓶颈档，多给 IP 能直接摊薄单 IP 负载（11 个时 9.1/IP，
19 个时 5.3/IP）。

几个必须知道的约束：

- **`EGRESS_IPS` 一旦设置会整体覆盖 YAML 里的 `egress.ips`**，包括分层配置。
  两处不要同时用，否则 YAML 里的分层会被静默清掉。
- **单档位容量耗尽不会溢出到其他档位**，这是刻意的。溢出会让一个高频 Key
  落到承载了上百个 Key 的 cold 档 IP 上，分层收益归零。此时 `Bind` 返回的
  错误会指明是哪个档位满了。
- **冷 Key 转热会自动迁移出口**。`PATCH /admin/keys/{id}` 改动 `pool` 时，
  网关自动把该 Key 迁到新档位的 IP 并落库，响应里带 `egress_migration`：

  ```json
  {
    "changed": ["pool"],
    "pool": {"from": "cold", "to": "hot"},
    "egress_migration": {"applied": true, "ip_from": "172.16.0.30", "ip_to": "172.16.0.13"}
  }
  ```

  **`applied` 为 false 时必须人工介入**：`pool` 已改但出口没换，通常是目标档位
  IP 已满。此时该 Key 仍在旧出口上，需要先扩容该档位再手动调
  `PUT /admin/keys/{id}/ip`。这种情况返回 200 而非 5xx 是刻意的——`pool` 确实
  改成功了，返回 5xx 会让运维重试，而重试时 `pool` 已是新值不再触发迁移，
  反而永久卡在不一致状态且无人知晓。

  内部用 `Migrate` 而非 `Rebind`：后者语义是「原出口有问题」会扣信誉分，
  池间流转是正常运营动作，频繁扣分会把健康 IP 逐个推入 cooldown。
- **`max_keys` 能调高的前提是行为画像已收窄**（见 `internal/persona`）。
  画像决定同一 IP 上有多少 Key 会在同一时刻活跃。沿用宽时段画像却调高
  `max_keys`，等于直接提高被识别的概率。
- **移除 Key 用 `DELETE /admin/keys/{id}`**，不要直接 `DELETE FROM upstream_keys`。
  删行与出口解绑（`egress.Release`）必须在同一次调用里完成——只删库会让
  网关内存里的绑定「权威副本」不感知，表现为候选集恒空、新 Key 全部 502
  的容量假满（KI-035 根因）。`DELETE` 端点已把这条同事务语义封装好。

##### 出口地址不想手抄？改用扫描（`EGRESS_IPS_SOURCE=scan`）

`EGRESS_IPS` 要求逐条列出地址，而地址是云厂商按机器分配的：抄错、漏抄、扩容后
忘补，表现都是「某个档位没出口」或「容量比预期小」，且不会有任何配置错误提示。
扫描把「地址从哪来」自动化，**分层规则不变**：

```bash
# .env
EGRESS_IPS=                    # 必须清空：两者互斥，同时设置会被配置校验拒绝
EGRESS_IPS_SOURCE=scan
```

分档计划（每档几个地址、单出口承载多少 Key）在镜像内的
`configs/config.prod.yml` 的 `egress.scan.tiers`，预置的就是本文推荐形态
（19 hot×10 / 5 warm×50 / 8 cold×100；`cold` 那项写 `count: 0` 表示接住剩余地址）。

过滤规则同在 `egress.scan` 段，可按机器调整：

- 排除回环、链路本地 `169.254/16`、组播，以及 `docker0` / `br-*` / `veth` /
  `tun` 等虚拟网卡的地址。内置黑名单**始终生效**，自定义 `iface_deny` 是追加
  而非替换（否则一次 `iface_deny: [eth1]` 就会把容器网桥一并放进来 —— 出口池里
  混进内网地址不报错，只在上游侧表现为「一批账号从内网地址访问」）。
- `prefix_allow` / `prefix_deny` 按 CIDR 收窄范围，deny 优先。机器上还有管理网时，
  用 `prefix_allow` 只纳入出口网段最稳妥。
- 同一网卡上的 secondary 地址（辅助 IP 的典型形态）会被一并纳入；地址按**数值**
  排序后依次填入各档，顺序稳定 —— 重启后同一地址仍落在同一档，其上的 Key 密度
  假设不会漂移。
- **每个网卡的首地址（主 IP）也在候选内**。若该地址另有用途（SSH / 管理面），
  用 `prefix_deny` 把它排掉；确需纳入内置黑名单里的网卡，用 `egress.ips`
  显式指定（那条路径不经过滤）。

⚠️ 切换前先核对地址数：扫描到的可用地址**少于**分档计划要求时，网关**拒绝启动**，
不会缩水运行 —— 缩水会静默丢掉撤离余量，而那是「出口被封时其上 Key 有地方可去」
的唯一保障。多出来的地址不参与计划，启动日志会告警（计划里留一个 `count: 0`
的项即可接住它们）。

##### ⚠️ 改动 `pool_shares` 必须同步复核 `max_keys`

这两个配置是耦合的，改一个不改另一个会静默推高密度。

各档 `max_keys` 的取值依据是该档的**相对请求频率**，而频率完全由
`scheduler.pool_shares` 决定。把 cold 从 5% 调到 10%，它的相对频率翻倍，
等效密度也随之翻倍。

极端情况：若把 cold 份额调到 70%（与 hot 对调），cold 档单 Key 频率会升到
接近 1.0，此时 100 个 Key/IP 的等效密度是 **32**，远超上限 5.5。

推荐的 `max_keys` 已刻意留了余量（cold 取 100 而非模型允许的更大值），
足以吸收小幅份额调整。但份额若有量级变化，必须重新跑
`go test ./internal/egress/ -run TestCapacity` 复核。

顺带一提：cold 档的实际限制**不在密度**。按等效密度模型它能到 2000+ 才触顶，
真正约束它的是上面那条撤离余量公式——`max_keys` 越大，封掉一个 IP 时需要的
空位越多，反而要配更多 IP。高密度不是免费的。

#### 按档位分配流量（`scheduler.pool_shares`）

出口分层只解决「Key 落在哪个 IP 上」，**分层能否成立取决于各档的实际请求频率
是否真有差异**——这由调度器的流量配比决定。

```yaml
scheduler:
  pool_shares:      # 相对值，无需归一化
    hot: 70
    warm: 25
    cold: 5
  pool_fallback: true
```

默认已启用上述配比。语义是**先按份额抽档位，再在档内按五维分数加权随机**。

**为什么不用「给 pool 加分」实现。** 附加分会被其余四维（配额 / 历史 / 画像 /
健康）稀释，实际配比完全不可控——一个 cold 档但配额充裕、健康度满分的 Key
照样能压过 hot 档。而分层的前提恰恰是 cold 档必须**稳定地**只承接极少流量。

**不配置会怎样。** 退化为对全体候选做一次加权随机，此时 Key 数量多的档位
按数量占优：cold 档 700 个 Key 会拿走约 79% 的流量（实测），其单 IP 瞬时并发
反而成为全池最高。这正是分层要避免的结果，所以**不建议关闭**。

`pool_fallback` 控制目标档位无可用 Key 时的行为：

- `true`（默认，推荐）：回退到其他档位，在剩余档位间按份额重新归一化。
  保可用性，代价是极端情况下配比被打破。响应侧可通过 `PoolFellBack` 观测回退频率。
- `false`：直接返回 `ErrNoCandidate`。配比严格，但 hot 档 Key 全部进入非活跃
  时段时会整体 503——画像窄化后任一时刻只有约 1/7 的 Key 活跃，这并非小概率事件。

启动时会校验档位名（只接受 `hot` / `warm` / `cold`）。写成 `Hot` 或 `hott`
会直接拒绝启动，而不是让那份额永远抽不到 Key——后者是静默失效，极难排查。

#### 出口被封的自动识别（可选，默认关闭）

上游返回 401/403 时无法区分「这个 Key 被封」与「这个 IP 被封」，所以默认不做
自动判定——出口被封只能靠运维观察到「某个 IP 上的 Key 成批变 invalid」后手动处理。

要开启自动识别，配置两个参数：

```bash
EGRESS_BAN_DETECT_KEYS=3        # 窗口内多少个「不同」Key 鉴权失败才判定出口被封
EGRESS_BAN_DETECT_WINDOW=10m    # 观察窗口
```

判据是**不同 Key 的数量**而非失败次数。单个 Key 反复失败通常是它自己被上游禁用，
与出口无关；多个互不相干的 Key 从同一出口相继失败才指向出口被拉黑。

达到阈值后网关会自动：标记该出口为 banned → 把其上所有 Key 迁到**同档位**的健康
出口 → 落库。日志形如：

```
判定出口已被上游封禁，已撤离其上全部 Key egress_ip=172.16.0.13 auth_failed_keys=3 moved=10 failed=0
```

几个必须知道的点：

- **阈值低于 3 会被配置校验拒绝。** 阈值为 1 意味着任何单个 Key 被禁用都会连带撤离
  整个出口；2 也过于激进。误判的代价是一次性制造大批「换了出口的老账号」，
  比漏判更糟。
- **`failed` 不为 0 时必须人工介入。** 那些 Key 无处可去（同档位余量不足），
  仍绑在被封出口上，已不可用且不会自愈。日志会逐个列出 key_id。这也是为什么
  32 个 IP 要留 8 个备用——撤离需要落脚点。
- **窗口不宜过长或过短。** 过长会把跨越数小时、彼此无关的零星失败累积成误判；
  过短则真实封禁可能凑不满阈值——画像窄化后同一出口在任一时刻只有少数 Key 活跃，
  这一点尤其要注意。10 分钟是折中起点，上线后按实际的 Key 活跃密度调整。
- **成功请求会撤销该 Key 的失败记录。** 一次成功即证明「该出口 + 该 Key」可用，
  否则计数会随时间单调累积，最终把长期健康的出口误判为被封。

#### 被封出口的自动恢复（默认开启）

上游封禁通常是临时的——几小时到几天。而 `banned` 在状态机里是终态：
`MarkSuccess` 只把 `suspect` / `cooldown` 转回 `active`，`Assignable` 又要求
`active`，于是被封出口会**永久退出服务**。32 个 IP 逐个损耗且不可回收，
最终耗尽备用余量——每档的空位本是为「撤离」准备的，不是为「永久报废」准备的。

```bash
EGRESS_BAN_COOLDOWN=2h       # 首次被封后等待多久尝试恢复，0 = 永不恢复
EGRESS_BAN_COOLDOWN_MAX=24h  # 指数退避的上限
```

冷却期届满后**转入 `cooldown` 而非直接 `active`**，让 15 秒一轮的健康探测
先验证连通性，探测成功才由 `MarkSuccess` 转回 `active` 重新承载 Key。
直接转 `active` 等于赌它已经解封，赌错的代价是一批 Key 立刻踩到仍被封的出口。

**反复被封的出口按 2 的幂次延长等待**：第 1 次 2h，第 2 次 4h，第 3 次 8h，
直到 `MAX` 截断。同一出口反复被封说明它在上游眼里已经很脏，
按固定间隔重试只会不断制造失败请求。

几个要点：

- **`EGRESS_BAN_COOLDOWN_MAX` 必须 ≥ `EGRESS_BAN_COOLDOWN`**，否则退避被截断成
  恒等于 `MAX`——反复被封的脏出口与首次被封的等同处理，而前者最需要长冷却。
  配错会被启动校验拒绝。
- **解封在探测之前执行。** `checkEgress` 先 `TryUnbanAll` 再 `Verify`，顺序不可换：
  反过来的话被封出口即使探测成功也仍是 `banned`，自动恢复永远不会发生。
- **设为 0 表示永不恢复。** 只有确认封禁是永久的（IP 段被整体拉黑、准备换 IP）
  才这么配。写非法值（如 `EGRESS_BAN_COOLDOWN=两小时`）会保持默认而非当成 0——
  把笔误变成「出口永不恢复」是静默的容量泄漏。
- **`ban_count` 不清零。** 它累积记录该出口被封过几次，看板会显示。
  某个 IP 反复被封说明它该被替换，而不是继续放在池子里。

看板的「出口 IP 状态」面板显示 `banned_at` / `ban_count` / 预计解封时刻，
并区分「等自愈」与「需人工介入」两种状态。

#### 扩展到多台机器

每台机器持有自己的 32 个 IP 子池，Key 按实例静态分组，入口按 `key_id` 定位归属实例。
**归属关系必须落库固定，不能靠运行时哈希**——实例增减会导致哈希重算，Key 换出口
是风控最敏感的信号。仅出口层分片，Redis 与 Postgres 保持共享单点，否则同一用户
在两台机器上会各拿一份配额。

### 第 3 步：启动

仓库只有**一份**编排文件 `docker-compose.yml`（2026-09-17 起，原
`docker-compose.prod.yml` 与 `docker-compose.test.yml` 已合并进它，合并理由见该
文件头），所以命令里不再需要 `-f`：

```bash
docker compose up -d
bash scripts/smoke-test.sh
```

项目名与卷名没变（都是 `fluxkeys`），因此从旧的两文件形态切过来**不需要迁移数据** ——
`up -d` 会按新定义重建容器，命名卷原样保留。

优先拉 CI 发布的版本化镜像，拉不到时 `up` 会自动退回本地构建：

```bash
docker compose pull gateway
```

**调优档**：compose 形态的配置本来只有「环境变量 + 内置默认值」两个来源，而
`internal/config/env.go` 只映射了 17 个变量 —— `redis.pool_size`、
`postgres.max_conns`、`quota.reap_*`、`scheduler.*` 这些**没有 env 出口**，
在容器里根本调不了。所以调优档由**构建期打进镜像**：

| 档位 | 源文件（仓库） | 镜像内路径 | 定位 |
|---|---|---|---|
| `prod`（默认） | `configs/config.prod.yml` | `/etc/fluxkeys/config.prod.yml` | 只补容量，不动风控姿态（单 Key 间隔仍是 5s） |
| `loadtest` | `configs/config.loadtest.yml` | `/etc/fluxkeys/config.loadtest.yml` | 关闭单 Key 节流与档位配比，**仅压测**，先读文件头的代价说明 |

切换档位（`docker compose up -d` 会用新命令行重建容器）：

```bash
FLUXKEYS_TUNING=loadtest docker compose up -d
```

**档位文件不挂宿主机目录**（`Dockerfile` 里 `COPY configs/ /etc/fluxkeys/`）。这样做的收益
是镜像即「二进制 + 配置」的完整交付物：从 ghcr 拉的镜像自带与它匹配的档位，不会出现
「宿主机上的 YAML 与镜像里的二进制语义不符」；容器也不需要任何宿主机目录挂载，与
`read_only: true` / `cap_drop: ALL` 的安全基线一致。**代价是改档位内容必须重建镜像** ——
临时验证可用命令行覆盖并挂自己的文件：

```bash
docker compose run --rm -v "$PWD/configs/config.loadtest.yml:/tmp/x.yml:ro" gateway -config /tmp/x.yml
```

（`deploy/schema.sql`、`prometheus.yml`、`grafana/` 仍然挂载 —— 它们属于 Postgres /
Prometheus / Grafana 的官方镜像与初始化脚本，要内置就得自建镜像，不划算。）

优先级是 `环境变量 > YAML > 内置默认值`，所以密钥、`EGRESS_IPS`、`FLUXKEYS_SHARD_ID`
继续留在 `.env` 即可，两者不冲突。YAML 为严格模式（未知字段直接启动失败），
唯一的例外是 **map 型默认值**：要清空 `scheduler.pool_shares` 必须写 `null`，
写 `{}` 会被静默忽略并保留默认的 70/25/5。

`EGRESS_MODE`、`EGRESS_IPS`、`EGRESS_VERIFY_ON_START`、`REFRESH_ENABLED` 由 `.env`
提供，后两者默认为 `true` —— 关闭自检会带着失效的出口绑定继续跑，关闭刷新探测会在
00:00-12:00 按满额度调度已耗尽的 Key（直接超刷），只在你明确要放弃这两道保护时才设 `false`。

### 第 4 步：确认 Prometheus 抓取

生产形态下抓取目标无需手工调整 —— compose 挂载的是
`deploy/prometheus.yml`（target 已是 `127.0.0.1:9090`，网关与 Prometheus
同在 host 网络直连回环），不存在「忘了改 target 导致所有告警静默失效」的坑。

启动后确认目标全部 `up`：

```bash
curl -s http://127.0.0.1:9091/api/v1/targets | jq '.data.activeTargets[]|{job:.labels.job,health}'
```

改抓取配置后热加载：

```bash
curl -X POST http://127.0.0.1:9091/-/reload
```

### 第 5 步：对外暴露

网关默认监听 `0.0.0.0:8080`，**没有 TLS**。两种做法二选一：

1. **开 `tls` profile**（生产文件内置 Caddy，自动签发/续期证书）：
   `.env` 设置 `CADDY_DOMAIN` 与 `ACME_EMAIL` 后，
   `COMPOSE_PROFILES=monitoring,backup,tls`，Caddy 会反代 gateway 并把
   80/443 暴露公网。
2. **自建 Nginx/Caddy**（宿主机进程或自管容器）做 TLS 终止。

无论哪种，都需确认：

- 已创建用户 API Key。`user_api_keys` 为空时若还开放了公网入口，等于开放代理
- 防火墙只放通 TLS 端口，不直接暴露 8080
- 看板、Prometheus、Grafana 只绑回环（生产文件已保证），远程访问走 SSH 隧道：
  `ssh -L 3000:127.0.0.1:3000 user@host`

---

## 生产切换清单

上线前逐项确认：

- [ ] `scripts/setup-egress.sh` 执行通过，且**逐 IP 出口公网地址互不相同**
- [ ] `--persist` 已执行，`systemctl is-enabled fluxkeys-egress` 返回 enabled
- [ ] `sysctl net.ipv4.conf.all.rp_filter` 为 `2`（严格模式会丢回包）
- [ ] `.env` 中 `EGRESS_MODE=multi_ip`、`EGRESS_VERIFY_ON_START=true`
- [ ] `.env` 中 `REFRESH_ENABLED=true`
- [ ] 密钥已用 `scripts/gen-prod-env.sh` 生成（或已轮换 compose 里的占位默认值），
      且 `FLUXKEYS_ENCRYPTION_KEY` 已备份到密钥管理系统（丢失不可恢复）
- [ ] `.env` 中 `VOLC_BASE_URL` 未被设置（留空时地址取自配置文件的内置默认值，
      这才是本项要核对的状态）；仅当 volc 确实要指向别的上游时才显式赋值
- [ ] `COMPOSE_PROFILES` 按生产需要设置（如 `monitoring,backup`）
- [ ] `FLUXKEYS_IMAGE_TAG` 是具体版本号，不是 `latest` / `dev`（首跑本地构建的 `dev` 除外）
- [ ] `REDIS_PASSWORD` 已设置（非 compose 占位默认值）
- [ ] `.env` 未被提交（`git check-ignore .env` 应有输出）
- [ ] Redis `appendonly=yes`、`appendfsync=everysec`、`maxmemory-policy=noeviction`
- [ ] `deploy/prometheus.yml` 随生产文件挂载，抓取目标 `127.0.0.1:9090`
- [ ] Prometheus targets 全部 `up`
- [ ] 已创建至少一个用户 API Key
- [ ] 网关前已有 TLS 终止层
- [ ] `bash scripts/smoke-test.sh` 全部通过

验证 Redis 持久化配置：

```bash
docker compose exec redis redis-cli --no-auth-warning -a "$REDIS_PASSWORD" \
  config get appendonly appendfsync maxmemory-policy
```

期望 `yes` / `everysec` / `noeviction`。

> `noeviction` 不能改。配额键被 LRU 淘汰等于把已消耗的额度清零，会直接造成超刷。
> 内存满时宁可让写入报错（网关会降级为拒绝新预扣），也不能静默丢数据。

---

## 故障排查

### 出口 IP 不生效

**这是最需要理解的一类故障，因为它是静默的。**

失效机理：云厂商控制台绑定辅助私网 IP 后，操作系统内部**什么都不会发生** ——
辅助 IP 不在网卡上，也没有对应的策略路由。此时应用绑定源地址会出现两种结果：

| 情况 | 表现 | 危险程度 |
|---|---|---|
| 地址不在网卡上 | `bind: EADDRNOTAVAIL`，请求失败 | 低（能发现） |
| 地址在网卡上但无 `ip rule` | **请求照常成功，源地址被内核改回主 IP** | 高（无任何征兆） |

第二种情况下所有 Key 共用一个出口 IP，反作弊隔离完全失效，日志一切正常。
等到 Key 被批量封禁才发现问题。

诊断步骤：

```bash
# 1. 一键复检（最快）
sudo bash scripts/setup-egress.sh --verify-only

# 2. 地址是否在网卡上
ip -4 addr show dev eth0

# 3. 策略规则是否存在，且优先级在 main 表（32766）之前
ip rule show
# 期望看到:  10000: from 172.16.0.11 lookup 100

# 4. 路由表内容
ip route show table 100
# 期望看到:  default via <网关> dev eth0 src 172.16.0.11

# 5. 反向路径过滤（严格模式会丢回包，表现为"发出去了但收不到响应"）
sysctl net.ipv4.conf.all.rp_filter

# 6. 手工逐 IP 复现
curl -4 --interface 172.16.0.11 https://api.ipify.org; echo
curl -4 --interface 172.16.0.12 https://api.ipify.org; echo
# 两个必须返回不同的公网 IP
```

对应处置：

| 现象 | 原因 | 处置 |
|---|---|---|
| 所有 IP 出口公网地址相同 | `ip rule` 缺失，或优先级排在 32766 之后 | 重跑 `setup-egress.sh` |
| `bind: EADDRNOTAVAIL` | 地址不在网卡上 | 重跑脚本；确认云控制台已绑定该辅助 IP |
| 能发包收不到响应 | `rp_filter=1` | `sysctl -w net.ipv4.conf.all.rp_filter=2` |
| 重启后失效 | 未持久化 | 重跑并加 `--persist` |
| 容器内绑定失败 | gateway 未走 host 网络 | 用 `docker-compose.yml` |
| 出口 IP 数量对但公网地址与声明不符 | 控制台 EIP 关联关系与 `.env` 不一致 | 核对控制台，改 `EGRESS_IPS` |

配套监控：`fluxkeys_egress_ips_by_state{state!="active"}` 非零会触发 `FluxKeysEgressIPDown`。
但**这个指标抓不到「静默退回主 IP」** —— 那种情况下探测是成功的。
唯一可靠的检测手段是 `setup-egress.sh` 的逐 IP 公网地址比对，建议定期跑。

### 网关启动失败

```bash
docker compose logs --tail=100 gateway
```

| 日志关键字 | 原因 | 处置 |
|---|---|---|
| `未配置 FLUXKEYS_ENCRYPTION_KEY` | 密钥缺失 | `.env` 中设置 32 字节 hex |
| `需为 32 字节` | 密钥长度不对 | `openssl rand -hex 32` 重新生成 |
| `egress.mode=multi_ip 必须配置 egress.ips` | 缺 `EGRESS_IPS`（且未开扫描） | 补上地址清单，或设 `EGRESS_IPS_SOURCE=scan` 让网关自己扫本机地址 |
| `EGRESS_IPS 与 egress.ips_source=scan 同时设置` | 两种地址来源互斥 | 清空 `EGRESS_IPS`（用扫描），或把 `EGRESS_IPS_SOURCE` 改回 `config` |
| `未在本机发现任何可用出口地址` | 辅助 IP 未配置，或过滤条件过严 | 先跑 `scripts/setup-egress.sh` 绑定辅助 IP；再核对 `egress.scan` 的黑白名单 |
| `分档计划要求 N 个出口地址…本机只发现 M 个` | 机器地址数少于分档计划 | 按本机实际地址数改 `egress.scan.tiers`（各档数字见撤离余量公式） |
| `egress.mode 非法` | 拼写错误 | 只能是 `direct` 或 `multi_ip` |
| `reap_interval 必须短于 lease_ttl` | 配额参数不自洽 | 回收间隔必须短于租约 TTL，否则泄漏无法及时回收 |
| `软水位比例必须小于硬水位比例` | 水位配置反了 | 检查 soft/hard ratio |
| `启用 fallback 必须设置 daily_budget_cents` | 开了付费兜底没设预算 | 设预算上限，或关闭 fallback |
| 连接 Postgres/Redis 失败 | 依赖未就绪 | `docker compose ps` 看健康状态 |

### 配额接近水位

`FluxKeysQuotaNearHardLimit`（单 Key）/ `FluxKeysUsablePoolExhausted`（整池）。

硬水位是 limit 的 90%（留 50 万 token 缓冲），所以指标到 0.90 意味着实际用掉了 limit 的 81%。

单个 Key 到水位是正常轮转。需要关注的是**可用池塌陷**：

```bash
# 未达硬水位的 Key 数量
curl -s http://127.0.0.1:9090/metrics \
  | awk '/^fluxkeys_quota_used_ratio\{.*kind="token"/ {if ($NF < 0.9) n++} END {print n+0}'
```

排查方向：

1. 是否处于 12:00 刷新前的额度谷底（正常，会自行恢复）
2. 是否有 Key 因 `banned`/`invalid` 退出轮转 —— 看 `fluxkeys_keys_by_status`
3. 是否存在租约泄漏虚占额度 —— 见下节

### 可用池耗尽

同上。若排除前两项后仍然耗尽，说明 Key 总量不足以支撑当前流量，需要扩池。

### 封禁率上升

`FluxKeysBanRateHigh`（存量比例 > 5%）/ `FluxKeysBanRateSpike`（1 小时新增 ≥ 3 个）。

短时间连续封禁几乎可以确定是**系统性原因**，不是单 Key 问题。按优先级排查：

1. **出口 IP 隔离是否真的生效** —— 跑 `setup-egress.sh --verify-only`。
   P1-6 的静默失效会让所有 Key 共用主 IP，这是最常见的封号原因。
2. **是否共享了 TLS 指纹** —— P1-8 要求每 Key 独立 Transport。
   共享连接池会让多个 Key 共用同一 JA3 指纹，等于在传输层焊死「同一台机器」的证据。
3. **请求节奏是否机械** —— 纯指数退避本身就是机器特征，需叠加 persona 化抖动。

发现后应立即降低整体速率，避免连带烧掉更多 Key。

### 错误率上升

`FluxKeysErrorRateHigh`（网关 5xx > 5%）/ `FluxKeysRetryRateHigh`（P90 重试 ≥ 2 次）。

先分方向：

```bash
# 上游是否有问题
# 网关未导出 upstream 成功/失败计数器，用重试分类作为代理指标
curl -s http://127.0.0.1:9090/metrics | grep fluxkeys_retries_total
```

- `fluxkeys_retries_total` 同步上升 → 上游故障、出口 IP 被限流，或大量 Key 失效
  （按 `class` 标签可区分具体重试原因）
- 上游正常但网关 5xx 高 → 查网关自身：Redis 连接、租约获取失败

注意重试会放大失败量（`max_retries=3`），上游失败率高位时应考虑临时降速。

延迟告警 `FluxKeysLatencyHigh` 阈值定在 P99 > 30s，定得宽是刻意的 ——
大模型推理本身就慢，这里只抓明显异常。

### 租约泄漏

三条互补的告警：`FluxKeysLeaseReapRateHigh`、`FluxKeysLeaseLeakSuspected`、`FluxKeysQuotaDriftDetected`。

危害是**静默占额**：`prededuct` 只增不减时，Key 的可用额度被虚假占用，
调度器以为额度耗尽而跳过它，整池可用量凭空缩水。

```bash
# 活跃租约数（无流量时应为 0）
curl -s http://127.0.0.1:9090/metrics | grep '^fluxkeys_leases_active'

# 看 Redis 里的租约 ZSET。注意用配额日而非自然日
QD=$(date -d '-12 hours' +%Y%m%d 2>/dev/null || date -v-12H +%Y%m%d)
docker compose exec redis redis-cli --no-auth-warning -a "$REDIS_PASSWORD" \
  zrange "volc:lease:$QD" 0 -1 withscores

# 对账偏差明细
docker compose exec postgres psql -U fluxkeys -d fluxkeys \
  -c "SELECT * FROM quota_drift_logs ORDER BY created_at DESC LIMIT 20;"
```

排查顺序：

1. reaper 协程是否还活着（`reap_interval` 默认 30s）
2. ZSET 里的 `score` 是否为将来的时间戳（写错会导致永不过期）
3. **配额日切换后旧 day 的 ZSET 是否没被扫到** —— 这是最常见的原因。
   `quota_day` 与自然日解耦（12:00 边界），reaper 若只扫当前 day 就会漏掉跨界的租约。
4. 流式请求 TTL 是否够（`stream_lease_ttl` 默认 600s）

超时回收本身是兜底路径。正常情况下绝大多数租约应通过 `quota_commit` 释放，
`fluxkeys_lease_reaped_total` 持续增长说明请求在 commit 前就中断了 ——
常见于 SSE 客户端断连未被感知。

### 配额对账偏差

`FluxKeysQuotaDriftDetected`。对账每小时校验 `prededuct == Σ 未过期 lease.amount`。

偏差意味着**有代码路径绕过了 Redis Lua 原子写**，违反 P0-1 的唯一写路径约束。
系统会按 lease 求和强制修正，但根因必须定位 —— 否则「不超刷」这个首要不变量无法保证。

明细在 `quota_drift_logs` 表。

### 刷新未确认

`FluxKeysRefreshLikelyFailed`（北京 14:00-18:00 水位未回落）/
`FluxKeysNoTrafficAfterRefresh`（窗口后无成功预扣）。

> 这两条是**间接判定**。网关尚未导出刷新状态指标，详见[已知缺口](#已知缺口刷新状态指标)。
> 间接信号能抓到大部分刷新失败，但无法区分具体原因，需按下面的步骤人工确认。

**这是超刷风险的最强信号。**

告警触发时，先查最常见的原因：

```bash
docker compose exec gateway env | grep REFRESH_ENABLED
```

本地默认 `false`，上线忘记打开会让系统退回「按时间推测刷新」，即 V3 的超刷模式。

```bash
# Key 的刷新状态分布
docker compose exec postgres psql -U fluxkeys -d fluxkeys \
  -c "SELECT refresh_state, count(*) FROM volc_keys GROUP BY refresh_state;"
```

处置步骤：

1. 确认这些 Key 已被排除出活跃池（真实剩余额度未知，不能按满额度调度）
2. 手动对单 Key 发一次 `max_tokens=1` 探测确认状态
3. **若火山侧确实未刷新，保持隔离直到下一个窗口，不要手工清零本地配额** ——
   手工清零等于主动制造超刷

> 告警规则里的时间判定用的是 Prometheus `hour()`，它**按 UTC 求值**，
> 不受容器 `TZ` 影响。北京 12:00 = UTC 04:00，北京 14:00 = UTC 06:00。
> 改规则时务必换算，`deploy/alerts.yml` 里每处都标注了对应的北京时间。

### 指标抓不到

`FluxKeysTargetDown`。这条告警是所有业务告警的前提 ——
抓不到样本时 PromQL 返回空向量，其余告警全部静默失效。

```bash
curl -s http://127.0.0.1:9091/api/v1/targets | jq '.data.activeTargets[]|{scrapeUrl,health,lastError}'
```

生产形态下抓取目标固定为 `127.0.0.1:9090`（gateway 与 Prometheus 同在 host
网络直连回环，配置由 `deploy/prometheus.yml` 挂载）。若 target 仍 down，
按顺序排查：gateway 是否健康、9090 是否被其它进程占用、
`FLUXKEYS_METRICS_ADDR` 是否被环境覆盖。

### Grafana 面板全是 No data

数据源 `uid` 不匹配。dashboard JSON 里引用 `fluxkeys-prom`，
`provisioning/datasources/prometheus.yml` 里固定了这个 uid。

不固定 uid 的话每次重建 Grafana 都会生成随机值，面板会全部变成 No data 而不报任何错。
CI 的 `deploy-lint` job 会检查这个一致性。

```bash
curl -s -u admin:admin http://127.0.0.1:3000/api/datasources | jq '.[]|{uid,url}'
curl -s -u admin:admin -X POST http://127.0.0.1:3000/api/datasources/uid/fluxkeys-prom/health
```

### 端口冲突

管理面（只看回环）改 `.env` 里的 `*_HOST_PORT`：

```bash
DASHBOARD_HOST_PORT=18000
PROMETHEUS_HOST_PORT=19091
GRAFANA_HOST_PORT=13000
POSTGRES_HOST_PORT=15432
REDIS_HOST_PORT=16379
```

gateway 没有端口映射这一层 —— 它走 host 网络，8080 是**宿主机端口本身**，要改就改
`.env` 里的 `FLUXKEYS_ADDR`，同时确认没有第二个 gateway 进程在监听（旧栈没 `down`
掉、或手工跑过二进制，都会表现为「端口被占用」）。

集成测试栈与部署栈共用同一份编排文件，靠独立 project name + 换端口避让（容器名固定为
`fluxkeys-test-postgres-1`，`scripts/local-e2e-check.sh` 依赖它）：

```bash
COMPOSE_PROJECT_NAME=fluxkeys-test \
POSTGRES_PASSWORD=fluxkeys REDIS_PASSWORD=fluxkeys POSTGRES_HOST_PORT=15434 \
  docker compose up -d --wait postgres redis
```

假上游不在该栈里 —— 它是 `test/mockark` 的进程内夹具，不占端口。

### 冒烟测试报 502 或连接失败

环境里有 HTTP 代理时，代理无法访问 `127.0.0.1` 上的服务，curl 会收到 502 ——
看起来像网关挂了，实际是代理拦的。`smoke-test.sh` 已在入口 `unset` 代理变量并给
每个 curl 加 `--noproxy '*'`。若确实需要通过代理访问远端环境，用 `SMOKE_USE_PROXY=1`。

---

## 运维操作

### 备份

Redis 的 AOF 与 Postgres 的数据都在命名卷里。`docker compose down` 不会删（需 `-v`）。

生产文件内置 `backup` profile（每日 `pg_dump -Fc` 到 `backup-data` 卷，默认保留 14 天，
`COMPOSE_PROFILES` 加 `backup` 或 `--profile backup` 启用）。手工备份：

```bash
# Postgres 逻辑备份
docker compose exec -T postgres pg_dump -U fluxkeys fluxkeys | gzip > backup-$(date +%F).sql.gz

# Redis 触发一次 AOF 重写后拷贝
docker compose exec redis redis-cli --no-auth-warning -a "$REDIS_PASSWORD" bgrewriteaof
docker run --rm -v fluxkeys_redis-data:/data -v "$PWD:/backup" alpine:3.20.3 \
  tar czf /backup/redis-$(date +%F).tar.gz -C /data .
```

`FLUXKEYS_ENCRYPTION_KEY` 必须单独备份到密钥管理系统。
只有数据库备份而没有主密钥，`volc_keys` 里的密文是无法恢复的。

### 升级

```bash
# 改 .env 里的 FLUXKEYS_IMAGE_TAG，然后
docker compose pull gateway
docker compose build dashboard   # dashboard 无 CI 镜像，本地构建
docker compose up -d
bash scripts/smoke-test.sh
```

`stop_grace_period` 设为 40s（略大于 `shutdown_timeout=30s`），给流式请求收尾时间。

### 查看资源占用

```bash
docker stats --no-stream
```

资源限制按单机 4C8G 分配：gateway 2C/2G、postgres 1C/1.5G、redis 0.5C/1G，
其余各 0.25~0.5C。真实流量约 20 QPS（P1-7），这些是宽裕的上限而非需求。

---

## 指标契约

以下清单已与 `internal/metrics/metrics.go` 的实际实现**逐项核对**（2026-08-23）。
`deploy/alerts.yml` 与 Grafana dashboard 均基于此。网关改指标名时必须同步改配置。

| 指标 | 类型 | 标签 | 用途 |
|---|---|---|---|
| `fluxkeys_requests_total` | counter | `endpoint`,`model`,`status`,`stream` | 错误率 |
| `fluxkeys_request_duration_seconds` | histogram | `endpoint`,`model`,`stream` | 延迟分位 |
| `fluxkeys_stream_ttfb_seconds` | histogram | — | 流式首字节 |
| `fluxkeys_requests_in_flight` | gauge | `endpoint` | 处理中请求、优雅关闭 |
| `fluxkeys_tokens_total` | counter | `model`,`direction` | Token 消耗 |
| `fluxkeys_quota_used_ratio` | gauge | `key_id`,`kind` | 水位告警、可用池统计 |
| `fluxkeys_quota_acquire_total` | counter | `kind`,`result` | 配额准入拒绝率 |
| `fluxkeys_quota_estimate_error_ratio` | histogram | — | 预扣估算偏差 |
| `fluxkeys_quota_drift_total` | counter | `kind` | 对账偏差次数 |
| `fluxkeys_quota_drift_amount` | histogram | — | 对账偏差量 |
| `fluxkeys_leases_active` | gauge | — | 租约泄漏 |
| `fluxkeys_leases_reaped_total` | counter | — | 超时回收速率 |
| `fluxkeys_keys_by_status` | gauge | `status` | 封禁率、活跃池 |
| `fluxkeys_keys_by_pool` | gauge | `pool` | 池分布 |
| `fluxkeys_scheduler_select_total` | counter | `result` | 调度结果 |
| `fluxkeys_scheduler_select_duration_seconds` | histogram | — | 调度耗时 |
| `fluxkeys_retries_total` | counter | `class` | 重试原因分类 |
| `fluxkeys_retries_per_request` | histogram | — | 上游失败的代理指标 |
| `fluxkeys_rate_limited_total` | counter | `dimension` | 用户限流 |
| `fluxkeys_egress_ips_by_state` | gauge | `state` | 出口 IP 可用性 |
| `fluxkeys_egress_ip_reputation` | gauge | `addr` | IP 信誉 |
| `fluxkeys_egress_keys_bound` | gauge | `addr` | 每 IP 绑定的 Key 数 |
| `fluxkeys_dependency_up` | gauge | `dependency` | Redis/Postgres 可达性 |

`smoke-test.sh` 的第 6 项会检查关键指标是否已导出。

> **注意**：Prometheus 的 `*Vec` 类型指标在没有任何样本时不会输出 `# TYPE` 行。
> 因此全新启动、零流量时 `/metrics` 里看不到 `fluxkeys_requests_total` 等指标是**正常**的，
> 不代表未实现。无标签的 `Gauge`/`Counter`（如 `fluxkeys_leases_active`）会立即出现。

### 已知缺口：刷新状态指标

网关**尚未导出 Key 刷新状态**（P0-4）。这是当前监控体系最重要的缺口 ——
「配额刷新未确认」本应是超刷风险的最强信号，现在只能用间接方式近似。

现状：`deploy/alerts.yml` 的 `fluxkeys-refresh` 组用「配额日边界后水位未回落」
这个间接信号替代（`FluxKeysRefreshLikelyFailed`）。它能抓到大部分刷新失败，
但无法区分「探测器没跑」「火山未刷新」「租约泄漏虚占额度」这三种原因。

待补：建议导出 `fluxkeys_keys_refresh_state{state}` gauge，
`state` ∈ `idle`/`probing`/`confirmed`/`failed`（对应 `volc_keys.refresh_state`）。
补上后把 `alerts.yml` 里注释掉的 `FluxKeysRefreshUnconfirmed` 启用，
删掉间接判定那条。

同样缺失的还有 Key 封禁的 counter。当前 `FluxKeysBanRateSpike` 对
`fluxkeys_keys_by_status{status="banned"}` 这个 gauge 用 `increase()` 近似 ——
因为 Key 一旦 banned 不会自行恢复，数值单调递增，这个近似成立，但不如
counter 严谨。

### 告警清单

共 23 条，分 6 组。

**配额（fluxkeys-quota）**

| 告警 | 级别 | 触发条件 |
|---|---|---|
| `FluxKeysQuotaNearHardLimit` | warning | 单 Key 配额 > 硬水位 90%，持续 10m |
| `FluxKeysUsablePoolExhausted` | critical | 可用 Key < 10 个，持续 5m |
| `FluxKeysQuotaEstimateInsufficient` | warning | 预扣估算 P90 误差比 > 1，持续 15m |
| `FluxKeysQuotaDenyRateHigh` | warning | 配额准入拒绝率 > 20%，持续 10m |

**Key 池（fluxkeys-keys）**

| 告警 | 级别 | 触发条件 |
|---|---|---|
| `FluxKeysBanRateHigh` | critical | 封禁率 > 5%，持续 10m |
| `FluxKeysBanRateSpike` | critical | 1h 内新增封禁 ≥ 3 |
| `FluxKeysActiveKeysLow` | critical | active Key < 10 个，持续 5m |

**流量（fluxkeys-traffic）**

| 告警 | 级别 | 触发条件 |
|---|---|---|
| `FluxKeysErrorRateHigh` | warning | 网关 5xx > 5%，持续 5m |
| `FluxKeysRetryRateHigh` | critical | P90 重试 ≥ 2 次/请求，持续 10m |
| `FluxKeysRateLimitedHigh` | warning | 限流速率 > 1/s，持续 15m |
| `FluxKeysLatencyHigh` | warning | 非流式 P99 > 30s，持续 10m |
| `FluxKeysStreamTTFBHigh` | warning | 流式首字节 P95 > 10s，持续 10m |

**租约与对账（fluxkeys-lease）**

| 告警 | 级别 | 触发条件 |
|---|---|---|
| `FluxKeysLeaseReapRateHigh` | warning | 回收速率 > 0.05/s，持续 15m |
| `FluxKeysLeaseLeakSuspected` | critical | 无流量但有活跃租约，持续 20m |
| `FluxKeysQuotaDriftDetected` | critical | 2h 内出现对账偏差 |
| `FluxKeysRequestsStuck` | warning | 无流量但请求未收尾，持续 15m |

**刷新（fluxkeys-refresh）**

| 告警 | 级别 | 触发条件 |
|---|---|---|
| `FluxKeysRefreshLikelyFailed` | critical | 北京 14:00-18:00 水位未回落，持续 30m |
| `FluxKeysNoTrafficAfterRefresh` | warning | 刷新窗口后无成功预扣，持续 30m |

**基础设施（fluxkeys-infra）**

| 告警 | 级别 | 触发条件 |
|---|---|---|
| `FluxKeysTargetDown` | critical | 指标端点不可达，持续 2m |
| `FluxKeysDependencyDown` | critical | Redis/Postgres 不可达，持续 2m |
| `FluxKeysEgressIPDown` | critical | 有出口 IP 非 active，持续 3m |
| `FluxKeysEgressReputationLow` | warning | IP 信誉 < 60，持续 10m |
| `FluxKeysSchedulerSlow` | warning | 调度 P99 > 10ms，持续 10m |

MVP 未部署 Alertmanager，告警只在 Prometheus UI 的 Alerts 页展示，不外发通知。
接入时取消 `deploy/prometheus.yml` 里 `alerting` 段的注释。

告警规则有 26 个单元测试用例，改规则后必须跑：

```bash
docker run --rm -v "$PWD/deploy:/cfg:ro" --entrypoint promtool \
  prom/prometheus:v2.54.1 test rules /cfg/alerts_test.yml
```

用例覆盖三类实际写错过的地方：

1. **空向量陷阱** —— `count()` 无匹配时返回空向量而非 0，会让「所有 Key 都耗尽」
   这个最严重的情况反而不告警。用 `sum(... < bool ...)` 规避。
2. **零流量除零** —— 分母加 `clamp_min` 防 NaN。
3. **跨 UTC 午夜的时间窗** —— `hour()` 按 UTC 求值，与北京时间差 8 小时。

CI 的 `deploy-lint` job 会自动执行这些测试。

---

## 文件说明

| 文件 | 用途 |
|---|---|
| `Dockerfile` | Go 侧多阶段构建，`--target gateway`；构建期把 `configs/*.yml` 打进镜像的 `/etc/fluxkeys/` |
| `docker-compose.yml` | **唯一**编排文件：部署形态即生产形态（gateway 走 host 网络），含 monitoring / tls / backup 三个 profile；测试依赖栈也复用它（独立 project name + 换端口） |
| `configs/config.prod.yml` | 默认调优档 —— 只补容量（连接池 / 租约回收 / 活跃池），不动风控姿态；随镜像分发，改它要重建镜像 |
| `configs/config.loadtest.yml` | 高并发压测档 —— 关闭单 Key 节流与档位配比，`FLUXKEYS_TUNING=loadtest` 启用，代价见文件头 |
| `../.env.example` | 环境变量模板（仓库根目录，全项目唯一一份） |
| `deploy/prometheus.yml` | 抓取配置（target `127.0.0.1:9090` —— gateway 与 Prometheus 同在 host 网络，直连回环） |
| `deploy/alerts.yml` | 告警规则 |
| `deploy/alerts_test.yml` | 告警规则单元测试 |
| `deploy/grafana/` | 数据源与 dashboard 自动装载（数据源指向回环，uid 固定为 `fluxkeys-prom`） |
| `scripts/gen-prod-env.sh` | 一键生成生产 `.env`（强随机密钥，幂等防覆盖） |
| `scripts/setup-egress.sh` | 多 EIP 策略路由配置与验证 |
| `scripts/smoke-test.sh` | 端到端冒烟测试 |
