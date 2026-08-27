# FluxKeys 统计看板

Python 3.12 + FastAPI 实现的**只读**统计看板，与 Go 网关完全解耦：只读 Postgres
和 Redis，不写入任何数据、不执行迁移、不提供任何写接口。

## 核心口径：配额日

火山额度每日 **12:00** 刷新，因此所有用量聚合都以**配额日**为口径：
**12:00 之前的时刻归属前一个自然日**。

```
配额日 2026-08-23  =  2026-08-23 12:00  ~  2026-08-24 12:00
```

实现见 `app/quotaday.py`，与 Go 侧 `internal/quota/quotaday.go` 是同一套定义。
若用自然日聚合，跨 12:00 的统计会把同一份额度拆成两天（架构文档 P0-3）。

时区由 `DASHBOARD_TZ` 控制（默认 `Asia/Shanghai`），**必须与网关进程时区一致**，
否则 Redis 配额 Key 的日期后缀会对不上。

## 配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `POSTGRES_DSN` | `postgres://fluxkeys:fluxkeys@127.0.0.1:5432/fluxkeys?sslmode=disable` | 只读会话 |
| `REDIS_ADDR` | `127.0.0.1:6379` | 与网关同一变量名 |
| `REDIS_PASSWORD` / `REDIS_DB` | 空 / `0` | |
| `DASHBOARD_PORT` | `8081` | |
| `DASHBOARD_TZ` | `Asia/Shanghai` | 配额日计算基准 |
| `QUOTA_TOKEN_HARD` | `4500000` | Redis 未写 `hard_limit` 时的兜底值 |
| `SIMILARITY_ALERT_THRESHOLD` | `0.6` | 行为相似度告警阈值 |

## 接口

| 路径 | 说明 |
|---|---|
| `GET /api/overview` | 全局概览：今日用量、活跃 Key、池分布、平均水位、错误率 |
| `GET /api/keys` | Key 列表，支持按水位/用量排序与状态筛选 |
| `GET /api/keys/{key_id}` | 单 Key 详情 + 近 N 配额日趋势 |
| `GET /api/usage/trend?days=7` | 用量趋势（按配额日） |
| `GET /api/usage/by-user?days=30` | 按用户计费口径聚合（P1-9） |
| `GET /api/errors?hours=24` | 错误分布（按错误码、状态码、Key） |
| `GET /api/egress` | 出口 IP 状态与负载 |
| `GET /api/quota/health` | 租约泄漏、接近硬水位、对账偏差 |
| `GET /api/refresh/status` | 刷新状态分布与未确认风险 Key |
| `GET /api/behavior/similarity` | 行为相似度离线自检 |
| `GET /healthz` | 探活，下游不可用时返回 `degraded` 但仍 200 |

`/` 是单页看板（原生 JS + Chart.js CDN，无构建工具）。

### 排序字段

`/api/keys?sort=` 支持两类：

- **落库口径**（下推 SQL）：`key_id` `health` `pool` `status` `last_used` `today_tokens`
- **热态口径**（Redis，Python 侧排序）：`ratio` `remaining` `used`

非白名单值返回 400，不会拼进 SQL。

## 运行

```bash
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python -r requirements-dev.txt
.venv/bin/python -m uvicorn app.main:app --port 8081
```

Docker（多阶段，非 root uid 10001）：

```bash
docker build -t fluxkeys-dashboard .
docker run -p 8081:8081 \
  -e POSTGRES_DSN=... -e REDIS_ADDR=redis:6379 fluxkeys-dashboard
```

## 测试

```bash
.venv/bin/python -m pytest
```

集成测试（`-m integration`）在 Postgres / Redis 不可用时自动 **skip**。
指定测试数据源：`TEST_POSTGRES_DSN` / `TEST_REDIS_ADDR` / `TEST_REDIS_DB`。

## 行为相似度说明

反封禁核心指标。每个 Key 压成两个概率向量：24 维请求时段分布 + 模型偏好分布，
综合相似度 = `0.6 × 时段余弦 + 0.4 × 模型余弦`。超过阈值（默认 0.6）的 Key 对
标记告警；若同时绑定同一出口 IP 则升级为 `critical`。

余弦对量级不敏感 —— 风控看的是行为「形状」，不是绝对请求量。

这是**离线报表**，不参与请求热路径的调度决策。

## 界面配色

金融/额度类看板，遵循中国用户直觉：**消耗上升/水位高 = 红色，余量充足 = 绿色**
（与欧美相反）。数字千分位，token 量按万/百万/亿压缩。
