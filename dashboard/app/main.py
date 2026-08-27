"""FluxKeys 管理控制台的 FastAPI 入口。

本文件**只做装配**：建连接池、装鉴权组件、挂中间件、挂路由。业务逻辑分别在
``api/``（路由）、``service.py``（聚合）、``security/``（鉴权）、
``gateway/``（转发）内。

两条贯穿全局的约束：

- **看板永不直接写库。** Postgres 连接是服务端强制只读，所有写操作一律 HTTP
  转发给网关（见 ``gateway/client.py``）。这是写路径唯一化的延伸。
- **ADMIN_API_KEY 绝不下发到浏览器。** 它只存在于本进程内，由转发层注入请求
  头；前端只持有会话 Cookie。

时间聚合口径统一为**配额日**（每日 12:00 刷新），不使用自然日。
"""

from __future__ import annotations

import logging
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from typing import Final

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

from . import __version__
from .api import admin_proxy, auth, reports, static_files
from .cache import Cache
from .config import _env_bool, get_settings
from .db import Database, StoreUnavailable
from .gateway import GatewayClient, GatewayError
from .security import (
    CSRFMiddleware,
    LoginRateLimiter,
    PasswordChecker,
    SessionAuthMiddleware,
    SessionSigner,
)
from .service import ReportService

logger = logging.getLogger(__name__)


@asynccontextmanager
async def lifespan(app: FastAPI) -> AsyncIterator[None]:
    """启动时建立连接池与鉴权组件，关闭时释放。

    数据源连接失败**不阻止启动**：看板是旁路组件，应能启动后通过 /healthz
    暴露问题，而不是让容器反复重启。

    这与「必填配置缺失则拒绝启动」并不矛盾，两者性质不同：数据库连不上是
    环境的瞬时状态、可能自行恢复；而口令/密钥缺失是配置错误、不会自愈，
    且静默降级的后果是管理控制台裸奔。后者的校验在 get_settings() 内。
    """
    settings = get_settings()
    database = Database(settings)
    cache = Cache(settings)
    await database.connect()
    await cache.connect()

    app.state.settings = settings
    app.state.database = database
    app.state.cache = cache

    # 单个 AsyncClient 复用连接池。每次请求新建会丢掉连接池，
    # 且在高频操作下耗尽本地端口。
    #
    # 必须在 ReportService 之前构造: 出口面板的状态列只能从网关的内存出口池
    # 读到（库里那张 egress_ips 表从未被写入），故只读服务也需要它。
    gateway_client = GatewayClient(
        settings.gateway_base_url,
        settings.gateway_admin_api_key,
        float(settings.gateway_timeout_import),
    )
    app.state.gateway_client = gateway_client

    app.state.service = ReportService(database, cache, settings, gateway_client)

    # 鉴权组件。口令在 PasswordChecker 内转成摘要，明文不再留在别处。
    app.state.session_signer = SessionSigner(settings.session_secret, settings.session_ttl)
    app.state.password_checker = PasswordChecker(settings.password)
    app.state.login_limiter = LoginRateLimiter(
        settings.login_max_attempts, settings.login_lockout_seconds
    )

    if not settings.cookie_secure:
        # 默认值匹配既有部署形态（compose 绑 127.0.0.1 + SSH 隧道，链路加密由
        # SSH 承担，此时 Secure=true 反而让隧道访问无法登录）。但必须显式告警，
        # 保证将来真的挂到 HTTPS 上时不会忘记打开。
        logger.warning(
            "DASHBOARD_COOKIE_SECURE 为 false，会话 Cookie 可能经明文传输。"
            "若已启用 HTTPS 请将其设为 true"
        )

    try:
        yield
    finally:
        await gateway_client.aclose()
        await cache.close()
        await database.close()


# 自动文档开关。
#
# 这里直读环境变量而不调 get_settings()：装配期还不该触发必填项校验，否则
# 「配置不全」会表现为导入模块即崩，连 uvicorn 的启动日志都出不来，运维只能
# 看到一段没有上下文的 traceback。校验统一放在 lifespan。
_DOCS_ON: Final[bool] = _env_bool("DASHBOARD_DOCS_ENABLED", False)

app = FastAPI(
    title="FluxKeys 管理控制台",
    description="火山引擎 Key 池的统计看板与管理控制台：配额水位、用量趋势、"
    "计费聚合、错误分布、反封禁自检，以及经网关转发的 Key 管理操作。",
    version=__version__,
    lifespan=lifespan,
    # 生产默认关闭自动文档，是鉴权之外的纵深防御 ——
    # /openapi.json 会把管理端点的请求体结构完整暴露出去。
    docs_url="/docs" if _DOCS_ON else None,
    redoc_url="/redoc" if _DOCS_ON else None,
    openapi_url="/openapi.json" if _DOCS_ON else None,
)

# 中间件的执行顺序与添加顺序相反：后添加的先执行。
#
# 要的效果是「先鉴权、再校验来源」，所以 CSRF 先添加、鉴权后添加。顺序反了的
# 后果是未登录的写请求会先收到 403 来源错误而不是 401，前端据 401 跳登录页的
# 逻辑就失效 —— 运维会看到「来源不被允许」而完全想不到自己只是没登录。
#
# 两者都从 app.state 惰性取依赖，理由同 _DOCS_ON 处的说明。
app.add_middleware(CSRFMiddleware)
app.add_middleware(SessionAuthMiddleware)

# 鉴权是**默认拦截 + 白名单豁免**（见 security/middleware.py 的 EXEMPT_PATHS）。
# 因此下面新增任何 router 都自动受保护，无需在此处额外挂依赖。
app.include_router(auth.router)
app.include_router(admin_proxy.router)
app.include_router(reports.router)
app.include_router(static_files.router)


@app.exception_handler(GatewayError)
async def _gateway_error_handler(request: Request, exc: GatewayError) -> JSONResponse:
    """把转发失败转成看板的 {detail} 结构。

    前端 fetchJSON 统一按 detail 取文案，不能裸传网关的 {"error": {...}}。
    """
    return JSONResponse(status_code=exc.status, content=exc.to_payload())


@app.exception_handler(StoreUnavailable)
async def _store_unavailable_handler(request: Request, exc: StoreUnavailable) -> JSONResponse:
    """数据库不可用返回 503 而非 500，便于上游探针区分「挂了」和「有 bug」。"""
    return JSONResponse(status_code=503, content={"detail": f"数据源暂不可用：{exc}"})


def run() -> None:  # pragma: no cover - 入口函数
    """以配置的端口启动服务。"""
    import uvicorn

    settings = get_settings()
    uvicorn.run(app, host="0.0.0.0", port=settings.port, log_level="info")


if __name__ == "__main__":  # pragma: no cover
    run()
