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


@router.patch("/config/default-provider", summary="切换默认 provider（转发至网关 PATCH）")
async def set_default_provider(request: Request) -> Response:
    """挂在 /api/admin/config 下，不在 /api/admin/providers 下。

    与网关的 ``PATCH /admin/config/default-provider`` 对齐（文档 §4.1.2）。
    ``default_provider`` 是整份配置的一个字段而非某个 provider 的属性 ——
    文档 §4.1.2 明确它存 ``provider_config_state`` 表而不是 provider 行上。
    挂进 providers 前缀会让路径读起来像「某个 provider 的 default 属性」，
    而运维照这个理解去猜时会发现无从指定是哪个。

    请求体 ``{expected_version, name, reason}`` 原样透传。看板**不检查**目标
    provider 是否存在、是否 enabled、版本号是否过期 —— 全在网关判（§4.1.2
    规则 1-2）。这条尤其不能在看板补：`enabled` 状态取自网关那一刻的配置
    快照，看板另发一次请求读到的是另一个时点的结果，两者不一致时看板的判断
    只会拦掉本该放过的提交。
    """
    body = await _read_body(request)
    result = await _client(request).set_default_provider(body, _actor(request))
    return JSONResponse(status_code=result.status, content=result.payload)


@router.post("/reload-config", summary="重载 provider 配置（转发至网关 POST）")
async def reload_config(request: Request) -> Response:
    """挂在 /api/admin 而非 /api/admin/providers 下。

    对应网关的 ``POST /admin/reload-config`` —— 它重载的是整份配置快照，不是
    某个 provider 的操作。放进 providers 前缀会让路径暗示一个不存在的从属关系，
    而运维照着路径去猜「重载哪个 provider」时会发现无从指定。
    """
    body = await _read_body(request)
    result = await _client(request).reload_provider_config(body, _actor(request))
    return JSONResponse(status_code=result.status, content=result.payload)


# provider 配置那组转发端点在 api/provider_proxy.py。
#
# 曾有一版按 docs/provider-config-hotreload.md §4.1 的清单写在本文件里
# （PATCH /providers/{name}、/enable、/disable、/config/versions、
# /providers/validate）。那一版**已移除**：网关侧
# 实测没有这些路由（internal/gateway/admin_provider.go:40-51 用 PUT 全量替换、
# 用 DELETE 软删代替 disable、版本历史按 provider 分组），照文档转发会得到
# 一组 404 —— 而 404 出现在第二跳，前端只会显示「目标不存在，列表已过期」，
# 完全指不到「路由压根没注册」这个真实原因。
#
# 差异清单已报 team-lead 与 be-api，以网关实际路由为准的理由是：那是唯一
# 能被验证的一侧，文档与前端都还能改。
