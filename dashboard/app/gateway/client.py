"""网关转发客户端。

看板自身**永不直接写库**：Postgres 连接是服务端强制只读，所有写操作一律
HTTP 转发给网关。这是「写路径唯一化」的延伸，也是本模块存在的全部理由。

``ADMIN_API_KEY`` 只存在于看板服务端进程内，由本模块在转发时注入请求头，
绝不下发到浏览器 —— 前端只持有会话 Cookie。
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Final

import httpx

from .error_envelope import extract_error_extras, extract_gateway_error, redact
from .provider_forward import ProviderForwardMixin

# 分维度超时，不用单一总超时。
#
# connect 单独设短：网关不可达应当快速失败，而不是让运维等满整个读超时。
_CONNECT_TIMEOUT: Final[float] = 3.0
_WRITE_TIMEOUT: Final[float] = 10.0
_POOL_TIMEOUT: Final[float] = 3.0
# 单行 UPDATE + 内存状态设置，很快；给足余量。
_DEFAULT_READ_TIMEOUT: Final[float] = 15.0


class GatewayError(Exception):
    """转发失败。``status`` 是看板应当对外返回的状态码。

    文案分三类且各不相同，因为处置动作完全不同 —— 一句笼统的
    「网关错误」会让运维把「该重启容器」和「该刷新确认」混为一谈。
    """

    def __init__(
        self,
        status: int,
        detail: str,
        gateway_code: str = "",
        gateway_status: int = 0,
        extra: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(detail)
        self.status = status
        self.detail = detail
        self.gateway_code = gateway_code
        self.gateway_status = gateway_status
        # 网关错误响应里 error 信封之外的同级字段（failures / diff / warnings）。
        #
        # provider 校验失败逐字段回问题（admin_provider.go:686-702），跨量纲
        # 变更被拒时附字段级差异（:708-721）。压平成一句 detail 会让运维在
        # 十几个字段里靠猜定位，而这些结构正是为了免掉这一步才存在的。
        self.extra: dict[str, Any] = extra or {}

    def to_payload(self) -> dict[str, Any]:
        """转成看板的错误结构。

        前端 fetchJSON 统一按 ``detail`` 取文案，所以必须转译成 ``{detail}``
        而不是裸传网关的 ``{"error": {...}}`` —— 后者会让前端对管理端点走
        一套解析、对报表端点走另一套，在原生 JS 里再分叉一条错误路径。
        ``gateway_code`` 保留，前端据它分派文案（同一状态码下有多种处置动作，
        只看状态码只能给模糊提示）。
        """
        payload: dict[str, Any] = {"detail": self.detail}
        if self.gateway_code:
            payload["gateway_code"] = self.gateway_code
        if self.gateway_status:
            payload["gateway_status"] = self.gateway_status
        payload.update(self.extra)
        return payload


@dataclass(frozen=True, slots=True)
class GatewayResponse:
    """转发成功后网关的原始响应。"""

    status: int
    payload: Any


class GatewayClient(ProviderForwardMixin):
    """管理接口转发客户端。

    持有**单个** ``AsyncClient``，由 FastAPI 的 lifespan 管理生命周期。
    不每次请求新建：新建会丢掉连接池，且在高频操作下耗尽本地端口。
    """

    def __init__(
        self,
        base_url: str,
        admin_api_key: str,
        import_read_timeout: float,
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self._admin_api_key = admin_api_key
        self._import_read_timeout = import_read_timeout
        self._client = client or httpx.AsyncClient(
            timeout=httpx.Timeout(
                connect=_CONNECT_TIMEOUT,
                read=_DEFAULT_READ_TIMEOUT,
                write=_WRITE_TIMEOUT,
                pool=_POOL_TIMEOUT,
            )
        )

    async def aclose(self) -> None:
        await self._client.aclose()

    async def patch_key(self, key_id: str, body: bytes, actor: str) -> GatewayResponse:
        """转发 PATCH /admin/keys/{key_id}。"""
        return await self._forward("PATCH", f"/admin/keys/{key_id}", body, actor)

    async def rebind_key_ip(self, key_id: str, body: bytes, actor: str) -> GatewayResponse:
        """转发 PUT /admin/keys/{key_id}/ip。"""
        return await self._forward("PUT", f"/admin/keys/{key_id}/ip", body, actor)

    async def fetch_egress_ips(self, actor: str) -> GatewayResponse:
        """转发 GET /admin/ips，取出口池的**实时**状态快照。

        为什么必须走网关而不查 `egress_ips` 表: 那张表在网关代码里从未被写入
        （只有 schema 定义），`state` / `reputation` 永远停在初始值。出口的封禁
        与信誉是 `egress.Pool` 的内存状态，只有网关自己知道。查库会让看板把一个
        已被封禁的出口显示成 active —— 比没有面板更危险。
        """
        return await self._forward("GET", "/admin/ips", b"", actor)

    async def import_keys(self, body: bytes, actor: str) -> GatewayResponse:
        """转发 POST /admin/keys。

        用显著更长的读超时：网关侧是**逐条 upsert**，每条一次数据库往返、
        一次 AES-GCM 加密并建立出口绑定。1000 个 Key 按每条 20-50ms 估需
        20-50 秒，用默认 15 秒会让大批量导入在网关**已经写入部分数据**后
        被判定超时 —— 运维看到「失败」而库里已有几百条，这是最糟的状态。
        """
        return await self._forward(
            "POST", "/admin/keys", body, actor, read_timeout=self._import_read_timeout
        )

    async def _forward(
        self,
        method: str,
        path: str,
        body: bytes,
        actor: str,
        read_timeout: float | None = None,
    ) -> GatewayResponse:
        headers = {
            # 管理密钥在此注入。它从不经过浏览器。
            "Authorization": f"Bearer {self._admin_api_key}",
            "Content-Type": "application/json",
            # 共享密钥无法区分人员，把会话身份透传给网关审计。
            "X-Admin-Actor": actor,
        }
        timeout = httpx.Timeout(
            connect=_CONNECT_TIMEOUT,
            read=read_timeout or _DEFAULT_READ_TIMEOUT,
            write=_WRITE_TIMEOUT,
            pool=_POOL_TIMEOUT,
        )

        try:
            resp = await self._client.request(
                method,
                f"{self._base_url}{path}",
                # 原样透传字节，不在看板侧反序列化再重新序列化 ——
                # 后者会把 8MB 数据在内存里翻倍，且 JSON 重序列化可能改变
                # 字段顺序与数值表示（大整数、浮点精度），让网关收到的内容
                # 与运维提交的不完全一致。
                content=body,
                headers=headers,
                timeout=timeout,
            )
        except httpx.ConnectTimeout as exc:
            raise GatewayError(504, "连接网关超时，请检查 gateway 容器状态") from exc
        except httpx.ReadTimeout as exc:
            # 这条文案是重点。
            #
            # POST /admin/keys 是逐条 upsert 且**不是单个事务**，超时时网关
            # 很可能已写入部分 Key。此时提示「请重试」会让运维重复导入 ——
            # 虽然 upsert 幂等，但会掩盖「上次到底成功了多少」，且第二次同样
            # 可能超时。正确的引导是先刷新列表确认实际状态。
            raise GatewayError(
                504,
                "请求已发出但未在超时内返回，操作可能已生效。请刷新列表确认实际状态，不要直接重试",
            ) from exc
        except httpx.ConnectError as exc:
            raise GatewayError(502, "网关服务不可达，请检查 gateway 容器状态") from exc
        except httpx.HTTPError as exc:
            raise GatewayError(502, f"转发到网关失败：{exc}") from exc

        return self._interpret(resp)

    def _interpret(self, resp: httpx.Response) -> GatewayResponse:
        """把网关响应转成看板语义，必要时抛 GatewayError。

        改为实例方法（原为 staticmethod）以便拿到管理密钥做脱敏 —— 透传网关
        文案的分支变多后，「网关文案里绝不含密钥」这个假设不再值得依赖。
        """
        try:
            payload: Any = resp.json()
        except ValueError:
            payload = None

        if resp.status_code < 400:
            return GatewayResponse(status=resp.status_code, payload=payload)

        message, code = extract_gateway_error(payload)
        message = redact(message, (self._admin_api_key,))
        extra = extract_error_extras(payload)

        # 401 必须转成 500，绝不透传。
        #
        # 网关返回 401 意味着**看板持有的 ADMIN_API_KEY 配错了**，这是看板
        # 侧的配置错误，不是用户未登录。透传 401 会让前端误判为「会话过期」
        # 并跳登录页，运维于是陷入「反复登录仍失败」的死循环，而真正该做的
        # 是去核对两侧的 ADMIN_API_KEY 是否同一个值。
        if resp.status_code == 401:
            raise GatewayError(
                500,
                "看板持有的网关管理密钥无效，请核对 GATEWAY_ADMIN_API_KEY "
                "与网关侧 ADMIN_API_KEY 是否一致",
                code,
                resp.status_code,
            )
        # 405 同理属看板 bug：说明看板发错了方法，不是用户的问题。
        if resp.status_code == 405:
            raise GatewayError(500, "看板向网关发送了不被允许的方法", code, resp.status_code)

        # 400 / 404 / 409 是调用方真正需要看到的业务错误，透传网关文案。
        #
        # 422 与 501 是 provider 配置端点引入的：
        #
        # - **422** 用于跨量纲变更被拒（admin_provider.go:704-721）。它与 409 的
        #   区别是刻意的：409 意味着「别人先改了，刷新重试即可」，而跨量纲变更
        #   重试一万次也不会成功，正确的补救是新建 provider。收敛成 502 会让这
        #   条永久失败被显示成「网关返回异常」，运维于是去查网关而不是改做法。
        # - **501** 表示该部署未启用配置管理（providerStore 类型断言失败）。压成
        #   502 会让「这个部署没这功能」显示成「网关出错了」，运维会去重启一个
        #   本来就正常的容器。
        if resp.status_code in {400, 404, 409, 413, 422, 429, 501, 503}:
            raise GatewayError(resp.status_code, message, code, resp.status_code, extra)

        # 其余 5xx 收敛为 502：语义上看板是网关的代理。
        return _raise_bad_gateway(message, code, resp.status_code, extra)


def _raise_bad_gateway(
    message: str, code: str, status: int, extra: dict[str, Any] | None = None
) -> GatewayResponse:
    raise GatewayError(502, message or "网关返回异常", code, status, extra)
