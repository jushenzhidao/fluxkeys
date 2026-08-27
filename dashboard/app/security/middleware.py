"""会话鉴权与 CSRF 中间件。

**默认拦截、白名单豁免**，而不是给每个端点挂 ``Depends``。

理由与网关侧「Admin.APIKey 为空则整组路由不注册」同源：逐端点挂依赖时，
新增一个端点忘了挂就是裸奔，而这种遗漏不会有任何报错、任何测试变红。
中间件默认拦截则相反 —— 新增端点自动受保护，要放行必须显式加进白名单，
即「安全是默认值，开口子要动手」。
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from typing import Final
from urllib.parse import urlsplit

from starlette.middleware.base import BaseHTTPMiddleware
from starlette.requests import Request
from starlette.responses import JSONResponse, RedirectResponse, Response

from .session import COOKIE_NAME, SessionSigner

# 豁免鉴权的路径。
#
# 刻意用精确匹配而非前缀匹配：前缀匹配下 /login 会连带放行
# /login-something 这类将来可能新增的路径。
#
# 注意这里**没有** /openapi.json、/docs、/docs/oauth2-redirect、/redoc ——
# 这四个是 FastAPI 自动挂载的，最容易被漏掉，而它们会把管理端点的请求体
# 结构完整暴露给未登录者。也**没有** /app.js：那 700 多行业务逻辑含全部
# API 调用路径与字段结构，未登录者没有理由读到。
EXEMPT_PATHS: Final[frozenset[str]] = frozenset(
    {
        # compose healthcheck 在容器内调用，无法带 Cookie。
        # 已确认其响应只含 status/version 与依赖可达性，不含用量数据。
        "/healthz",
        # 登录入口本身。样式内联在 HTML 里，因此无需豁免任何静态资源。
        "/login",
        "/logout",
    }
)

# 需要 CSRF 校验的方法。GET/HEAD/OPTIONS 是安全方法，不改状态。
_UNSAFE_METHODS: Final[frozenset[str]] = frozenset({"POST", "PUT", "PATCH", "DELETE"})

# 前端页面请求（希望 302 到登录页）与 API 请求（希望 401 JSON）的区分依据。
_HTML_PATHS: Final[frozenset[str]] = frozenset({"/", "/index.html"})


class SessionAuthMiddleware(BaseHTTPMiddleware):
    """校验会话 Cookie，未通过则拒绝。

    签名器从 ``app.state`` 惰性取，不在构造期注入：装配期读配置会让必填项
    缺失表现为「导入模块即崩」，连启动日志都出不来。
    """

    async def dispatch(
        self, request: Request, call_next: Callable[[Request], Awaitable[Response]]
    ) -> Response:
        path = request.url.path
        if path in EXEMPT_PATHS:
            return await call_next(request)

        signer: SessionSigner | None = getattr(request.app.state, "session_signer", None)
        if signer is None:
            # lifespan 未执行（例如有人绕过 lifespan 直接挂载 app）时必须拒绝，
            # 绝不能放行 —— 「鉴权组件没装好就等于不鉴权」是最危险的降级方向。
            return JSONResponse(status_code=503, content={"detail": "鉴权组件尚未初始化"})

        token = request.cookies.get(COOKIE_NAME, "")
        if signer.verify(token) is None:
            return self._reject(request, path)
        return await call_next(request)

    @staticmethod
    def _reject(request: Request, path: str) -> Response:
        """未登录时的响应。

        页面请求 302 到登录页，其余一律 401 JSON。对 API 返回 302 会让
        前端 fetch 拿到一个 200 的 HTML 登录页并试图当 JSON 解析，
        报出与「未登录」毫无关系的解析错误。
        """
        if path in _HTML_PATHS and request.method in {"GET", "HEAD"}:
            return RedirectResponse(url="/login", status_code=302)
        return JSONResponse(status_code=401, content={"detail": "未登录或会话已过期"})


class CSRFMiddleware(BaseHTTPMiddleware):
    """对非安全方法做 Origin / Referer 校验。

    这是叠在 ``SameSite=Strict`` 之上的第二道防线，不引入 CSRF token。

    SameSite=Strict 已让跨站请求完全不携带会话 Cookie，但它的有效性完全
    依赖浏览器正确实现，而那是**单一防线**。Origin 校验成本极低且防的是
    另一个层面（请求来源），两者失效模式不重叠。

    不做 CSRF token 的理由：无状态会话下 token 要么另存服务端（回到会话表
    方案，破坏只读不变量），要么用 double-submit cookie —— 后者在
    SameSite=Strict 已生效时防护增量接近于零，而代价是每个写请求都要多取
    一次 token。收益与复杂度不成比例。
    """

    async def dispatch(
        self, request: Request, call_next: Callable[[Request], Awaitable[Response]]
    ) -> Response:
        if request.method not in _UNSAFE_METHODS:
            return await call_next(request)

        conf = getattr(request.app.state, "settings", None)
        allowed = {o.rstrip("/") for o in getattr(conf, "allowed_origins", ())}

        origin = request.headers.get("origin", "").strip()
        referer = request.headers.get("referer", "").strip()

        # Origin 优先，缺失时退而校验 Referer 的主机部分。
        #
        # 注意判据是「有没有声称来源」而不是「解析出的来源是否非空」：
        # Referer 存在但格式畸形时 _origin_of 返回空串，此时必须判定为不通过 ——
        # 若按空串跳过校验，构造一个畸形 Referer 就绕过了这道防线。
        if origin:
            claimed = origin
        elif referer:
            claimed = _origin_of(referer)
        else:
            # 两者都缺失则放行。
            return await call_next(request)

        if not self._origin_ok(claimed, request, allowed):
            return JSONResponse(status_code=403, content={"detail": "请求来源不被允许"})
        #
        # CSRF 的前提是**浏览器自动携带凭据**，而浏览器发起的跨站请求必定带
        # Origin。命令行客户端不带 Origin，但它们也不会自动带上别人的 Cookie，
        # 不构成 CSRF。强行要求 Origin 存在只会堵死 curl 调试路径，
        # 不带来实际安全收益。
        return await call_next(request)

    @staticmethod
    def _origin_ok(origin: str, request: Request, allowed: set[str]) -> bool:
        if not origin:
            return False
        candidate = origin.rstrip("/")
        if candidate in allowed:
            return True
        # 未显式配置时按同源判定，由 Host 头推导。
        host = request.headers.get("host", "")
        return bool(host) and urlsplit(candidate).netloc == host


def _origin_of(url: str) -> str:
    """从完整 URL 取出 scheme://host 部分。"""
    parts = urlsplit(url)
    if not parts.scheme or not parts.netloc:
        return ""
    return f"{parts.scheme}://{parts.netloc}"
