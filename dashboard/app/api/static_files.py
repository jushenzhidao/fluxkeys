"""前端静态资源路由。

这些路由**一律不豁免鉴权**：它们含全部 API 调用路径、字段结构与管理操作的
请求体格式，未登录者没有理由读到。登录页样式内联在 ``/login`` 内，不依赖这里
的任何文件，因此静态资源可以做到零豁免。

注册方式是**表驱动**而非逐个手写 handler。
理由不是少写几行，而是防一类具体的缺陷：前端新增文件时，逐个手写要改两处
（加 handler + 加测试），漏掉注册的后果是浏览器里 404、界面整块渲染不出来，
而后端测试全绿 —— 这正是本项目已经踩过的「装配完好但不干活」。表驱动把
「新增一个资源」收敛成往 ``_ASSETS`` 加一行，且下面的一致性测试会拿
``_ASSETS`` 与磁盘上的实际文件互相核对。
"""

from __future__ import annotations

from pathlib import Path
from typing import Final

from fastapi import APIRouter, HTTPException
from fastapi.responses import FileResponse

router = APIRouter(include_in_schema=False)

STATIC_DIR: Final[Path] = Path(__file__).resolve().parent.parent / "static"

_JS: Final[str] = "application/javascript"
_CSS: Final[str] = "text/css"

# 对外路径 -> (磁盘文件名, media_type)。
#
# 顺序无关紧要，但按「基础看板 / 管理控制台」分组便于阅读。
_ASSETS: Final[dict[str, tuple[str, str]]] = {
    # 基础看板
    "/app.js": ("app.js", _JS),
    "/app.css": ("app.css", _CSS),
    # 管理控制台。这 5 个是 index.html 显式引用的，缺任何一个界面都不完整。
    "/admin.css": ("admin.css", _CSS),
    "/admin-icons.js": ("admin-icons.js", _JS),
    "/admin-ui.js": ("admin-ui.js", _JS),
    "/admin.js": ("admin.js", _JS),
    "/admin-import.js": ("admin-import.js", _JS),
}


def _serve(filename: str, media_type: str) -> FileResponse:
    """返回静态文件。

    文件缺失时抛 404 而非让 FileResponse 在响应阶段抛 RuntimeError ——
    后者发生在响应已开始之后，客户端只会看到一个截断的连接，日志里也没有
    可读的原因。
    """
    path = STATIC_DIR / filename
    if not path.is_file():
        raise HTTPException(status_code=404, detail=f"静态资源不存在：{filename}")
    return FileResponse(path, media_type=media_type)


def _register(url_path: str, filename: str, media_type: str) -> None:
    """把一个静态资源挂到 router 上。"""

    async def handler() -> FileResponse:
        return _serve(filename, media_type)

    # 显式给 name，否则所有 handler 同名 'handler'，
    # 出问题时 traceback 与路由表都无法区分是哪个资源。
    router.add_api_route(
        url_path,
        handler,
        methods=["GET"],
        include_in_schema=False,
        name=f"static_{filename.replace('.', '_').replace('-', '_')}",
    )


for _url, (_file, _mt) in _ASSETS.items():
    _register(_url, _file, _mt)


@router.get("/", include_in_schema=False)
async def index() -> FileResponse:
    """单页看板。未登录时由鉴权中间件 302 到 /login。"""
    return _serve("index.html", "text/html")
