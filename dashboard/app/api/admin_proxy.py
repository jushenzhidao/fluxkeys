"""管理操作转发端点。

这一层只做三件事：限长读取请求体、调转发客户端、把错误转成 ``{detail}``。
业务判定（状态机、幂等、乐观并发）全部在网关侧 —— 看板重复一份校验就会
出现两套规则各自演化，最终对同一个请求给出不同结论。
"""

from __future__ import annotations

from typing import Final

from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse, Response

from ..gateway.client import GatewayClient, GatewayError

router = APIRouter(prefix="/api/admin", tags=["dashboard-proxy"])

# 请求体上限，与网关 admin.go 的 MaxBytesReader 对齐。
#
# 必须对齐：看板放过一个 10MB 请求、转发到网关才被拒，等于白白耗费一次
# 完整传输，而运维看到的错误发生在第二跳，排查方向会被带偏。
_MAX_BODY_BYTES: Final[int] = 8 << 20


async def _read_body(request: Request) -> bytes:
    """读取请求体并限长。超限抛 GatewayError(413)。"""
    body = await request.body()
    if len(body) > _MAX_BODY_BYTES:
        raise GatewayError(
            413,
            f"请求体超过 {_MAX_BODY_BYTES // (1 << 20)}MB 上限",
        )
    return body


def _actor(request: Request) -> str:
    """本次操作的审计身份。

    共享口令模型下无法区分具体人员，如实上报为 dashboard 而不是编造一个
    人名 —— 审计里出现假身份比没有身份更糟。至少保留「经由看板」这个事实，
    与网关直连调用区分开。
    """
    return "dashboard"


def _client(request: Request) -> GatewayClient:
    """取转发客户端。

    缺失时抛 GatewayError 而非让 AttributeError 冒出去：后者会变成一个没有
    任何说明的 500，运维看到的是 'State' object has no attribute
    'gateway_client' —— 与真实原因（lifespan 未执行）毫无关联。
    """
    client: GatewayClient | None = getattr(request.app.state, "gateway_client", None)
    if client is None:
        raise GatewayError(503, "转发客户端尚未初始化，看板可能未正常完成启动")
    return client


@router.post("/keys/import", summary="导入 Key（转发至网关 POST /admin/keys）")
async def import_keys(request: Request) -> Response:
    body = await _read_body(request)
    result = await _client(request).import_keys(body, _actor(request))
    return JSONResponse(status_code=result.status, content=result.payload)


@router.patch("/keys/{key_id}", summary="调整 Key 状态或池（转发至网关 PATCH）")
async def patch_key(key_id: str, request: Request) -> Response:
    body = await _read_body(request)
    result = await _client(request).patch_key(key_id, body, _actor(request))
    return JSONResponse(status_code=result.status, content=result.payload)


@router.put("/keys/{key_id}/ip", summary="变更出口 IP（转发至网关 PUT）")
async def rebind_key_ip(key_id: str, request: Request) -> Response:
    body = await _read_body(request)
    result = await _client(request).rebind_key_ip(key_id, body, _actor(request))
    return JSONResponse(status_code=result.status, content=result.payload)
