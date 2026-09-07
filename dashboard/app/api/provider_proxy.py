"""provider 配置管理的转发端点。

与 ``admin_proxy.py`` 同一层次、同一套规则（见那边的模块 docstring）：只做
限长读取请求体、调转发客户端、把错误转成 ``{detail}``。**这一层不做任何
provider 字段校验、不判 quota_kind 能否改、不检测版本冲突** —— 那些全在网关
侧，看板重复一份就会出现两套规则各自演化，最终对同一个请求给出不同结论。

单独成文件而非并入 ``admin_proxy.py``：加上这一组会让那个文件突破单文件
可维护规模，而它们与 Key 管理是两组独立的资源。前缀 ``/api/admin/providers``
下的路由集中在此，读者不需要在一个混编文件里筛选。

不在此文件的两个相关端点：``POST /api/admin/reload-config`` 与
``PATCH /api/admin/config/default-provider`` 都在 ``admin_proxy.py``，因为它们
作用于整份配置而非某个 provider，网关侧的路径也不在 ``/admin/providers`` 下。

**转发目标是网关的实际路由（``internal/gateway/admin_provider.go:40-52``），
不是设计文档 §4.1 的清单** —— 两者有实质差异（网关用 ``PUT`` 全量替换而非
``PATCH``、版本历史按 provider 分组、没有 enable/disable）。按文档写会得到
一组 404。差异已列入交接报告。

``supported_providers`` 有两个来源，各服务一个场景：列表响应顶层字段
（与列表数据同一响应、必然同源，列表页用它）；独立的 ``/capabilities``
端点（不拉全量列表的轻量入口，新建表单用它）。两者在网关侧同源
（都出自 ``confsnap.SupportedProviders``）。
"""

from __future__ import annotations

from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse, Response

from .admin_proxy import _actor, _client, _read_body

router = APIRouter(prefix="/api/admin/providers", tags=["dashboard-proxy"])

# 复用 admin_proxy 的三个辅助函数而不是各写一份。
#
# 尤其是 _read_body 的 8MB 上限与 _client 的 503：两处各写一份的必然结果是
# 某次调整只改了一个文件，于是同一个看板对 Key 和 provider 给出不同的上限，
# 而这种不一致只会在运维踩到时才暴露。


@router.get("", summary="provider 列表（转发至网关 GET /admin/providers）")
async def list_providers(request: Request) -> Response:
    result = await _client(request).list_providers(_actor(request))
    return JSONResponse(status_code=result.status, content=result.payload)


@router.post("", summary="新增 provider（转发至网关 POST /admin/providers）")
async def create_provider(request: Request) -> Response:
    body = await _read_body(request)
    result = await _client(request).create_provider(body, _actor(request))
    return JSONResponse(status_code=result.status, content=result.payload)


@router.get("/capabilities", summary="受支持的 provider 取值（转发至网关 GET）")
async def get_provider_capabilities(request: Request) -> Response:
    """必须注册在 ``/{name}`` **之前**。

    FastAPI/Starlette 按注册顺序匹配，注册在后面会被 ``/{name}`` 吞掉 ——
    ``get_provider(name="capabilities")`` 拼出的转发路径与本端点逐字节相同，
    mock 侧看不出差别，只有真网关会把它当成一个不存在的 provider 名返回 404。
    路由顺序由 test_provider_proxy.py 的两条守卫测试钉住。
    """
    result = await _client(request).get_provider_capabilities(_actor(request))
    return JSONResponse(status_code=result.status, content=result.payload)


@router.get("/{name}", summary="provider 详情（转发至网关 GET）")
async def get_provider(name: str, request: Request) -> Response:
    result = await _client(request).get_provider(name, _actor(request))
    return JSONResponse(status_code=result.status, content=result.payload)


@router.put("/{name}", summary="全量替换 provider 配置（转发至网关 PUT）")
async def update_provider(name: str, request: Request) -> Response:
    """网关侧是全量替换，不是 PATCH 合并。

    看板不在此补一层「读当前值再合并」：那会让看板持有一份对「哪些零值是
    没填、哪些是要清空」的判断，而网关刻意把这件事留给调用方显式表达
    （admin_provider.go:227-233）。补上去等于在两侧各放一套语义。
    """
    body = await _read_body(request)
    result = await _client(request).update_provider(name, body, _actor(request))
    return JSONResponse(status_code=result.status, content=result.payload)


@router.delete("/{name}", summary="停用 provider（转发至网关 DELETE，软删除）")
async def delete_provider(name: str, request: Request) -> Response:
    body = await _read_body(request)
    result = await _client(request).delete_provider(name, body, _actor(request))
    return JSONResponse(status_code=result.status, content=result.payload)


@router.get("/{name}/versions", summary="配置版本历史（转发至网关 GET）")
async def list_provider_versions(name: str, request: Request) -> Response:
    """``limit`` / ``before`` 原样透传，不在看板侧校验。

    网关已对两个参数做正整数校验并各有文案（admin_provider.go:382-398）。
    看板再校验一遍就是第二套规则，且必然滞后于网关的演进。
    """
    result = await _client(request).list_provider_versions(name, request.url.query, _actor(request))
    return JSONResponse(status_code=result.status, content=result.payload)


@router.get("/{name}/versions/{version_id}", summary="配置版本详情（转发至网关 GET）")
async def get_provider_version(name: str, version_id: int, request: Request) -> Response:
    """``version_id`` 声明为 int，由 FastAPI 拦掉非数字。

    这是本文件唯一的例外，且它不是业务校验：非数字段落根本构造不出网关那条
    路由，转发过去只会拿回一句「版本号非法」，白走一趟网络。类型转换与
    「这个版本号是否属于该 provider」是两件事，后者仍在网关侧
    （admin_provider.go:442-447）。
    """
    result = await _client(request).get_provider_version(name, version_id, _actor(request))
    return JSONResponse(status_code=result.status, content=result.payload)


@router.post("/{name}/rollback", summary="回滚配置版本（转发至网关 POST）")
async def rollback_provider(name: str, request: Request) -> Response:
    body = await _read_body(request)
    result = await _client(request).rollback_provider(name, body, _actor(request))
    return JSONResponse(status_code=result.status, content=result.payload)


@router.post("/{name}/dry-run", summary="预演配置变更（转发至网关 POST）")
async def dry_run_provider(name: str, request: Request) -> Response:
    """网关侧预演永远返回 200，校验不通过体现在 body 的 ``valid: false``。

    看板原样透传该状态码，绝不把 ``valid: false`` 改写成 4xx —— 那会让前端
    无法区分「预演跑完了，结果不合格」与「预演本身没跑起来」。
    """
    body = await _read_body(request)
    result = await _client(request).dry_run_provider(name, body, _actor(request))
    return JSONResponse(status_code=result.status, content=result.payload)
