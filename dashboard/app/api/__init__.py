"""看板 HTTP 路由层。

分包原则：一个关注点一个 router。
- auth: 登录/登出
- admin_proxy: Key 管理与配置重载的转发
- provider_proxy: provider 配置管理的转发
- reports: 只读报表
- static_files: 前端产物

admin_proxy 与 provider_proxy 同为写转发、共用那三个辅助函数，拆开只因两组
资源各自的端点数已够独立成文件。
"""

from . import admin_proxy, auth, provider_proxy, reports, static_files

__all__ = ["admin_proxy", "auth", "provider_proxy", "reports", "static_files"]
