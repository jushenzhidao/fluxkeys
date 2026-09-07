"""provider 配置管理端点的转发方法。

与 ``client.py`` 分文件，因为那边已近 240 行，再加这一组会突破单文件可维护
规模。拆分点选在这里是因为这组方法有一个共同特征：**它们全都只是 URL 拼装**。
真正的转发、超时、错误解释都在 ``_forward`` / ``_interpret`` 内，本文件不含
任何独立逻辑，因此单独成文件不会让读者需要在两处之间来回跳。

``ProviderForwardMixin`` 只依赖 ``self._forward``，不碰 ``_client`` /
``_base_url`` 等状态 —— 混入类若开始摸宿主的私有属性，拆文件就变成了把一个
类劈成两半，比不拆更糟。

**provider 名一律 URL 编码**。FastAPI 的 ``{name}`` 路径参数不匹配斜杠，所以
构造不出跨层级路径，但 ``%`` ``?`` ``#`` 这些字符仍会改变转发 URL 的解析结果
—— 编码一次比逐个论证「哪些字符是安全的」可靠。
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Protocol
from urllib.parse import quote

if TYPE_CHECKING:
    from .client import GatewayResponse


class _Forwarder(Protocol):
    """混入类对宿主的唯一要求。"""

    async def _forward(
        self,
        method: str,
        path: str,
        body: bytes,
        actor: str,
        read_timeout: float | None = None,
    ) -> GatewayResponse: ...


def _seg(name: str) -> str:
    """把 provider 名编码成单个路径段。"""
    return quote(name, safe="")


class ProviderForwardMixin:
    """provider 配置管理的转发方法。

    每个端点一个显式方法，与 ``patch_key`` / ``import_keys`` 的既有风格一致。
    不做「传 method 与 path 的通用转发方法」：那等于把网关的路由表搬到调用方，
    路由改名时看板这侧不会有任何编译期或测试期信号，只会在运行时 404。
    """

    async def list_providers(self: _Forwarder, actor: str) -> GatewayResponse:
        """转发 GET /admin/providers。

        响应顶层除 ``providers`` 数组外还带 ``active_version`` 与
        ``supported_providers``。列表页直接用响应内的取值 —— 与列表数据
        同一次响应，必然同源。这里无需为它做任何事：payload 原样透传。
        """
        return await self._forward("GET", "/admin/providers", b"", actor)

    async def get_provider_capabilities(self: _Forwarder, actor: str) -> GatewayResponse:
        """转发 GET /admin/providers/capabilities。

        轻量入口：只要 provider 取值集合、不拉全量列表（新建表单的下拉框）。
        列表页仍应优先用列表响应里的 ``supported_providers``（同源保证），
        本端点服务于「列表尚未加载」的场景。
        """
        return await self._forward("GET", "/admin/providers/capabilities", b"", actor)

    async def get_provider(self: _Forwarder, name: str, actor: str) -> GatewayResponse:
        """转发 GET /admin/providers/{name}。"""
        return await self._forward("GET", f"/admin/providers/{_seg(name)}", b"", actor)

    async def create_provider(self: _Forwarder, body: bytes, actor: str) -> GatewayResponse:
        """转发 POST /admin/providers。"""
        return await self._forward("POST", "/admin/providers", body, actor)

    async def update_provider(
        self: _Forwarder, name: str, body: bytes, actor: str
    ) -> GatewayResponse:
        """转发 PUT /admin/providers/{name}。

        网关侧是**全量替换**而非 PATCH（admin_provider.go:227-233）。看板不在
        这里做「读当前值再合并」的补偿 —— 那会让看板持有一份对「哪些零值是
        没填、哪些是要清空」的判断，而这正是网关刻意不做的那件事。
        """
        return await self._forward("PUT", f"/admin/providers/{_seg(name)}", body, actor)

    async def delete_provider(
        self: _Forwarder, name: str, body: bytes, actor: str
    ) -> GatewayResponse:
        """转发 DELETE /admin/providers/{name}（网关侧是软删除）。"""
        return await self._forward("DELETE", f"/admin/providers/{_seg(name)}", body, actor)

    async def list_provider_versions(
        self: _Forwarder, name: str, query: str, actor: str
    ) -> GatewayResponse:
        """转发 GET /admin/providers/{name}/versions。

        ``query`` 是已编码的查询串（``limit`` / ``before`` 游标）。原样透传而不
        在看板侧解析重组：网关已对两个参数做了正整数校验并给出各自的文案，
        看板再校验一遍就会出现两套规则，且看板这份必然滞后于网关的演进。
        """
        path = f"/admin/providers/{_seg(name)}/versions"
        return await self._forward("GET", f"{path}?{query}" if query else path, b"", actor)

    async def get_provider_version(
        self: _Forwarder, name: str, version_id: int, actor: str
    ) -> GatewayResponse:
        """转发 GET /admin/providers/{name}/versions/{version_id}。"""
        return await self._forward(
            "GET", f"/admin/providers/{_seg(name)}/versions/{version_id}", b"", actor
        )

    async def rollback_provider(
        self: _Forwarder, name: str, body: bytes, actor: str
    ) -> GatewayResponse:
        """转发 POST /admin/providers/{name}/rollback。"""
        return await self._forward("POST", f"/admin/providers/{_seg(name)}/rollback", body, actor)

    async def dry_run_provider(
        self: _Forwarder, name: str, body: bytes, actor: str
    ) -> GatewayResponse:
        """转发 POST /admin/providers/{name}/dry-run。

        网关侧预演永远返回 200，校验不通过体现在 body 的 ``valid: false`` +
        ``failures``（admin_provider.go:615-618）。看板不得把 ``valid: false``
        改写成 4xx —— 那会让前端无法区分「预演跑完了，结果不合格」与
        「预演本身没跑起来」。
        """
        return await self._forward("POST", f"/admin/providers/{_seg(name)}/dry-run", body, actor)

    async def set_default_provider(self: _Forwarder, body: bytes, actor: str) -> GatewayResponse:
        """转发 PATCH /admin/config/default-provider（文档 §4.1.2）。

        路径**不在** ``/admin/providers`` 之下，因为 ``default_provider`` 不是某个
        provider 的属性，而是整份配置的一个字段（文档 §4.1.2:597 明确它存在
        ``provider_config_state`` 表而非 provider 行上）。挂进 providers 前缀会
        让路径暗示一个不存在的从属关系。

        请求体带 ``expected_version`` / ``name`` / ``reason``，看板原样透传：
        「目标是否存在且 enabled」「版本号是否过期」全在网关判（§4.1.2 规则
        1-2）。看板尤其**不能**代为检查目标 provider 是否 enabled —— 那份判断
        依赖的是网关那一刻的快照，看板读到的必然是另一次请求的结果。
        """
        return await self._forward("PATCH", "/admin/config/default-provider", body, actor)

    async def reload_provider_config(self: _Forwarder, body: bytes, actor: str) -> GatewayResponse:
        """转发 POST /admin/reload-config。

        网关侧只重载**接到请求的那个实例**（admin_provider.go:621-630），响应里
        的 ``scope: current_instance`` 说的就是这件事。看板原样透传该字段，不
        改写成「已全局生效」—— 当前是单实例部署，两者恰好等价，但等价的前提
        写在网关的注释里而不是看板能保证的事。
        """
        return await self._forward("POST", "/admin/reload-config", body, actor)
