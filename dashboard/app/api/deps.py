"""路由层的依赖注入。

单独成文件是为了让 main.py 只做装配 —— 依赖解析属于路由层职责，
而各 router 都要用到它，放进任一 router 都会造成横向 import。
"""

from __future__ import annotations

from fastapi import HTTPException, Request

from ..config import Settings, get_settings
from ..service import ReportService


def get_service(request: Request) -> ReportService:
    """取报表服务。

    lifespan 未执行时返回 503 而非让 AttributeError 变成 500：
    后者的错误信息与真实原因（未完成初始化）毫无关联。
    """
    service: ReportService | None = getattr(request.app.state, "service", None)
    if service is None:  # pragma: no cover - lifespan 未执行时才可能发生
        raise HTTPException(status_code=503, detail="看板尚未初始化")
    return service


def get_conf(request: Request) -> Settings:
    """取运行期配置。优先用 app.state 里的实例，便于测试注入。"""
    return getattr(request.app.state, "settings", None) or get_settings()
