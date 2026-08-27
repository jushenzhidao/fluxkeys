"""FluxKeys 只读统计看板。

与 Go 网关完全解耦：只读 Postgres 与 Redis，不写入任何数据、不执行迁移。
"""

__all__ = ["__version__"]

__version__ = "0.1.0"
