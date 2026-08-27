"""登录与登出端点。"""

from __future__ import annotations

from typing import Final, Literal

from fastapi import APIRouter, Request
from fastapi.responses import HTMLResponse, JSONResponse, Response

from ..config import Settings
from ..security import COOKIE_NAME, client_key
from .login_page import render_login_page

router = APIRouter(tags=["dashboard-auth"])

# 会话 Cookie 的固定属性。
#
# HttpOnly 恒开：看板前端有 700 多行原生 JS 并从 CDN 加载图表库，
# 一旦 CDN 被投毒或出现 XSS，没有 HttpOnly 就等于会话直接被读走。
#
# SameSite=Strict 而非 Lax：看板是纯内部运维面，没有任何跨站跳转进来的
# 正常场景。Lax 允许顶层 GET 携带 Cookie，Strict 能连「从外部链接点进来
# 即已登录」这种信息泄漏一并挡掉。
_SAMESITE: Final[Literal["strict"]] = "strict"


def _set_session_cookie(response: Response, token: str, conf: Settings) -> None:
    response.set_cookie(
        COOKIE_NAME,
        token,
        max_age=conf.session_ttl,
        httponly=True,
        samesite=_SAMESITE,
        secure=conf.cookie_secure,
        path="/",
    )


@router.get("/login", include_in_schema=False)
async def login_page(request: Request) -> HTMLResponse:
    """登录页。

    样式内联在 HTML 内，不依赖 /app.css —— 这样静态资源无需任何豁免，
    未登录者读不到任何前端产物。
    """
    return HTMLResponse(render_login_page())


async def _extract_password(request: Request) -> str:
    """从请求体取口令，兼容表单与 JSON。

    刻意不用 FastAPI 的 ``Form(...)``：那需要额外安装 python-multipart，
    而本项目 venv 内并没有它 —— 用了会在导入期就抛 RuntimeError，
    表现是整个看板起不来。为一个字段引入一个依赖不划算，手工解析即可。

    同时接受 JSON 是为了让 curl 调试无需构造表单编码。
    """
    raw = await request.body()
    if not raw:
        return ""

    content_type = request.headers.get("content-type", "")
    if "application/json" in content_type:
        import json

        try:
            parsed = json.loads(raw)
        except ValueError:
            return ""
        if isinstance(parsed, dict):
            value = parsed.get("password")
            return value if isinstance(value, str) else ""
        return ""

    # 默认按 application/x-www-form-urlencoded 解析。
    from urllib.parse import parse_qs

    try:
        fields = parse_qs(raw.decode("utf-8"))
    except UnicodeDecodeError:
        return ""
    values = fields.get("password") or []
    return values[0] if values else ""


@router.post("/login", summary="提交口令并签发会话")
async def login(request: Request) -> Response:
    """校验口令并下发会话 Cookie。"""
    password = await _extract_password(request)
    conf: Settings = request.app.state.settings
    limiter = request.app.state.login_limiter
    checker = request.app.state.password_checker
    signer = request.app.state.session_signer

    client = client_key(request.client.host if request.client else None)

    if limiter.is_locked(client):
        # 不透露剩余尝试次数或锁定剩余时间 ——
        # 告知「还剩 2 次」等于帮攻击者校准节奏。
        return JSONResponse(
            status_code=429,
            content={"detail": "失败次数过多，请稍后再试"},
        )

    if not checker.verify(password):
        limiter.record_failure(client)
        # 文案不区分「口令错」与其他原因，避免成为口令探测的信息源。
        return JSONResponse(status_code=401, content={"detail": "口令错误"})

    limiter.record_success(client)
    response = Response(status_code=204)
    _set_session_cookie(response, signer.issue(), conf)
    return response


@router.post(
    "/logout",
    summary="清除本浏览器的会话 Cookie（不使 token 在服务端失效）",
)
async def logout(request: Request) -> Response:
    """登出。

    删 Cookie 时必须带上与签发时**完全一致**的 path / samesite / secure：
    属性不匹配时浏览器会认为是另一个 Cookie，原会话不会被清除，
    表现为「点了退出但刷新还是登录态」。

    **这是纯客户端行为，不是服务端吊销。**

    会话是无状态 HMAC 签名（这样看板才能保持只读，见 ADR-001），服务端没有
    任何会话记录可删。因此本接口只让浏览器丢弃 Cookie，那个 token 本身在
    DASHBOARD_SESSION_TTL 到期前仍然完全有效 —— 任何拿到过它的人（浏览器
    历史、代理日志、共享机器上的另一个进程、抓包）都能继续使用。

    两条对应的处置：
      - 常态：共享机器上用完请**关闭浏览器**，只点登出不够；
      - 应急（口令泄漏 / 怀疑 Cookie 泄漏）：改 DASHBOARD_SESSION_SECRET
        并重启全部副本，这会让所有既有会话立即失效。
    """
    conf: Settings = request.app.state.settings
    response = Response(status_code=204)
    response.set_cookie(
        COOKIE_NAME,
        "",
        max_age=0,
        httponly=True,
        samesite=_SAMESITE,
        secure=conf.cookie_secure,
        path="/",
    )
    return response
