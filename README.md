# FluxKeys

火山引擎多 Key 智能调度网关。对外提供 OpenAI 兼容 API，内部把请求分散到大量免费 Key 上，在**不超刷**的前提下最大化可用额度，同时让每个 Key 的行为看起来像一个独立用户。

## 它解决什么问题

火山引擎每个账号每日赠送 500 万 Token，额度在**每日 12:00 刷新**。持有大量 Key 时会遇到三个真实问题：

1. **超刷即熔断** —— 免费额度耗尽后会触发"安心模式"，即使账户有余额也停服。所以额度必须精确管控，不能靠估算。
2. **行为相似即封号** —— 火山商务反馈封禁根因是"用户行为规律相似"。1000 个 Key 若表现出同一套作息、同一种请求节奏、同一个出口 IP，就是明显的池化特征。
3. **额度刷新不可控** —— 刷新发生在火山侧，系统无法控制其时刻，只能探测。

## 核心设计

| 机制 | 说明 |
|---|---|
| 配额单一权威写路径 | 预扣/修正/释放全部走 Redis Lua 原子操作，进程内存只做只读快照，无权做准入判断 |
| 租约式预扣 | 每次预扣登记带 TTL 的租约，SSE 断连或进程崩溃后由回收器释放，配套每小时对账 |
| 配额日（quota_day） | 12:00 之前归属前一自然日，与火山刷新周期对齐 |
| 刷新探测确认 | 只有探测到成功响应才认为已刷新并清零计数，绝不按时间推测 |
| Key-IP 终身绑定 | 每个 Key 绑定固定出口 IP，独立 Transport，禁用跨 Key 连接复用 |
| 行为差异化 | 由 key_id 哈希确定的作息时段/模型偏好/节奏抖动，不在活跃时段的 Key 直接淘汰 |

设计依据与推导过程见 [docs/architecture-v4.md](docs/architecture-v4.md)，编码前的可行性实证见 [docs/feasibility-report.md](docs/feasibility-report.md)。

## 部署

本仓只有**一份** compose 文件（`docker-compose.yml`），它同时是唯一的部署入口 ——
形态即生产形态：gateway 走 host 网络，因为 `multi_ip` 出口绑定必须看到宿主机网卡上的
辅助 IP。完整流程（含出口 IP 规划、迁移、排障）见 [deploy/README.md](deploy/README.md)，
最短路径：

```bash
bash scripts/gen-prod-env.sh          # 生成强随机密钥的 .env（权限 600）
# 编辑 .env，填 EGRESS_IPS —— 唯一无法代填的参数
sudo bash scripts/setup-egress.sh --persist --ips '...'   # 宿主机侧 IP + 策略路由
docker compose up -d
bash scripts/smoke-test.sh            # 冒烟：不只验容器 Running，验链路真的通
```

升级到已发布的镜像：

```bash
docker pull ghcr.io/<owner>/fluxkeys:<版本> && docker tag ghcr.io/<owner>/fluxkeys:<版本> fluxkeys/gateway:<版本>
FLUXKEYS_IMAGE_TAG=<版本> docker compose up -d
```

**出口 IP 必须额外配置**。云厂商绑定辅助私网 IP 后，操作系统不会自动配置到网卡也不会建立策略路由，此时所有流量仍走主 IP，按 Key 绑定出口会**静默失效**（不报错，只是不生效）。网关启动时会用每个 IP 作为源地址拨测一次上游，失败即拒绝启动 —— 这是有意的快速失败，不是故障。

采用 host 网络要接受三个代价：8080/9090 直接占用宿主机端口（同机不能跑第二份 gateway）；容器内没有服务名 DNS，依赖地址一律走 `127.0.0.1`；任何 host 网络容器都能访问回环上的 Redis/Postgres，密码是唯一的门。

## 本地验证（不需要 compose）

host 网络要求宿主机有辅助 IP，开发机通常没有，所以本地验证走这两条：

```bash
go test ./test/...                 # 离线全链路：进程内假上游，无需任何真实 Key
bash scripts/local-e2e-check.sh    # 真实 Redis/Postgres + 假上游的端到端（依赖栈起法见脚本头部）
```

访问地址（均在宿主机回环）：网关 `http://127.0.0.1:8080`、看板 `http://127.0.0.1:8000`（远程走 SSH 隧道）、指标 `http://127.0.0.1:9090/metrics`、Prometheus `9091`、Grafana `3000`。

调用示例：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $USER_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v3","messages":[{"role":"user","content":"你好"}]}'
```

## 技术栈

- **网关**：Go 1.23（每 Key 独立 Transport、SSE 流式转发、`net.Dialer.LocalAddr` 精确控制出口）
- **热状态**：Redis 7，AOF everysec
- **持久化**：Postgres 16（用户、Key 元数据、用量流水、审计）
- **看板**：Python 3.12 + FastAPI，只读
- **部署**：Docker Compose（单一编排文件，gateway 走 host 网络）· **CI**：GitHub Actions

## 项目结构

```
cmd/gateway        网关主程序
test/mockark       假上游夹具（集成测试进程内启动；另含手工联调入口 cmd/，非产品部件）
internal/quota     配额管理：Lua 原子操作、租约、配额日、刷新探测
internal/scheduler 五维打分调度引擎
internal/egress    出口 IP 池与 Key-IP 绑定
internal/adapter   渠道适配层
internal/gateway   HTTP 服务、鉴权、限流、流式转发
internal/store     Postgres 持久化
internal/persona   行为画像
dashboard/         Python 统计看板
deploy/            部署配置与文档
```

## 开发

```bash
export PATH=$HOME/.workbuddy/binaries/go/go/bin:$PATH
go test ./... -race                                    # 全量测试
REDIS_ADDR=127.0.0.1:6379 go test ./internal/quota/    # 配额测试需 Redis
```

出口 IP 绑定的完整验证需要 Linux 网络栈（宿主机部分用例会 skip）：

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -o /tmp/egress.test ./internal/egress/
docker run --rm --cap-add=NET_ADMIN -v /tmp/egress.test:/egress.test alpine:3.20 sh -c \
  'ip addr add 127.0.0.2/8 dev lo; ip addr add 127.0.0.3/8 dev lo; /egress.test -test.v'
```

## 合规提示

批量使用免费额度账号处于服务条款的灰色地带，存在批量封禁风险。本项目的行为差异化设计只降低被识别概率，不构成任何规避承诺。请自行评估账号资产的法律边界与最坏情况预案。
