# FluxKeys 看板镜像：多阶段构建，非 root 用户运行。

# ---------- 构建阶段：装依赖到独立前缀，避免把编译工具带进运行镜像 ----------
FROM python:3.12-slim AS builder

ENV PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

WORKDIR /build

# asyncpg 在无 wheel 的平台上需要编译，构建阶段单独装 gcc。
RUN apt-get update \
    && apt-get install -y --no-install-recommends gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install --prefix=/install -r requirements.txt

# ---------- 运行阶段 ----------
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    DASHBOARD_PORT=8081 \
    DASHBOARD_TZ=Asia/Shanghai \
    PATH="/usr/local/bin:$PATH"

# tzdata 是必需的：配额日以 Asia/Shanghai 为基准，缺少时区库会退化成 UTC，
# 导致 12:00 边界判断偏移 8 小时，所有报表口径出错。
RUN apt-get update \
    && apt-get install -y --no-install-recommends tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 dashboard \
    && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin dashboard

COPY --from=builder /install /usr/local

WORKDIR /app
COPY --chown=10001:10001 app ./app

USER 10001:10001

EXPOSE 8081

# 只读服务，单 worker 足够（看板 QPS 极低）。
CMD ["sh", "-c", "exec uvicorn app.main:app --host 0.0.0.0 --port ${DASHBOARD_PORT} --no-server-header"]
