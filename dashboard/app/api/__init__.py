"""看板 HTTP 路由层。

分包原则：一个关注点一个 router。
- auth: 登录/登出
- admin_proxy: 写操作转发（唯一的写入口）
- reports: 只读报表
- static_files: 前端产物
"""

from . import admin_proxy, auth, reports, static_files

__all__ = ["admin_proxy", "auth", "reports", "static_files"]
