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

## 快速开始

默认配置指向内置的 Mock 火山服务，**无需任何真实 Key 即可跑通全链路**：

```bash
cp deploy/.env.example .env      # 按提示填入随机密钥
docker compose up -d
./scripts/smoke-test.sh          # 冒烟测试
```

访问：

- 网关 `http://localhost:8080`
- 统计看板 `http://localhost:8000`
- 指标 `http://localhost:9090/metrics`
- Grafana `http://localhost:3000`

调用示例：

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $USER_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v3","messages":[{"role":"user","content":"你好"}]}'
```

## 切换到生产

```bash
EGRESS_MODE=multi_ip
EGRESS_IPS=172.16.0.2=1.2.3.4,172.16.0.3=1.2.3.5
VOLC_BASE_URL=https://ark.cn-beijing.volces.com
```

**出口 IP 需要额外配置**。云厂商绑定辅助私网 IP 后，操作系统不会自动配置到网卡也不会建立策略路由，此时所有流量仍走主 IP，按 Key 绑定出口会**静默失效**（不报错，只是不生效）。必须先执行：

```bash
sudo ./scripts/setup-egress.sh   # 配置网卡 + 策略路由，并逐 IP 验证真实出口
```

网关启动时会自动校验出口连通性（`EGRESS_VERIFY_ON_START=true`），配置错误会在日志中直接暴露。详见 [deploy/README.md](deploy/README.md)。

## 技术栈

- **网关**：Go 1.23（每 Key 独立 Transport、SSE 流式转发、`net.Dialer.LocalAddr` 精确控制出口）
- **热状态**：Redis 7，AOF everysec
- **持久化**：Postgres 16（用户、Key 元数据、用量流水、审计）
- **看板**：Python 3.12 + FastAPI，只读
- **部署**：Docker Compose 单机 · **CI**：GitHub Actions

## 项目结构

```
cmd/gateway        网关主程序
cmd/mockark        Mock 火山 Ark 服务（离线验证全链路）
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
