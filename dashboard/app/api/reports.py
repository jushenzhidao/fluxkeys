"""只读报表端点。

这一层只做参数校验与调 service，聚合逻辑全在 ReportService 内 ——
路由处理器里写聚合会让同一份口径在多个端点各写一遍。
"""

from __future__ import annotations

from typing import Final

from fastapi import APIRouter, Depends, HTTPException, Query

from .. import __version__
from .. import models as m
from ..db import KEY_SORT_FIELDS
from ..service import ReportService
from ..similarity import DEFAULT_MIN_REQUESTS
from .deps import get_service

router = APIRouter(tags=["dashboard-reports"])

# Key 列表支持的排序字段：SQL 白名单 + 依赖 Redis 热态的水位字段。
#
# 必须是白名单：ORDER BY 无法参数化，直接拼外部字符串就是注入。
SORT_CHOICES: Final[frozenset[str]] = frozenset(KEY_SORT_FIELDS) | {
    "ratio",
    "remaining",
    "used",
}


@router.get("/api/overview", response_model=m.OverviewResp, summary="全局概览")
async def api_overview(
    service: ReportService = Depends(get_service),
) -> m.OverviewResp:
    """今日（配额日）总用量、活跃 Key 数、池分布、平均水位与错误率。"""
    return await service.overview()


@router.get("/api/keys", response_model=m.KeyListResp, summary="Key 列表")
async def api_keys(
    service: ReportService = Depends(get_service),
    status: str | None = Query(None, description="按 Key 状态筛选，如 active"),
    pool: str | None = Query(None, description="按池筛选：hot / warm / cold"),
    sort: str = Query("key_id", description="排序字段"),
    order: str = Query("asc", pattern="^(asc|desc)$", description="asc / desc"),
    limit: int = Query(100, ge=1, le=1000),
    offset: int = Query(0, ge=0),
) -> m.KeyListResp:
    """Key 列表：配额水位、健康分、出口 IP、池归属、刷新状态。

    ``sort`` 取值：``key_id`` / ``health`` / ``pool`` / ``status`` /
    ``last_used`` / ``today_tokens``（落库口径）以及
    ``ratio`` / ``remaining`` / ``used``（Redis 热态口径）。
    """
    if sort not in SORT_CHOICES:
        raise HTTPException(
            status_code=400,
            detail=f"sort 仅支持：{', '.join(sorted(SORT_CHOICES))}",
        )
    return await service.list_keys(
        status=status,
        pool=pool,
        sort=sort,
        desc=order == "desc",
        limit=limit,
        offset=offset,
    )


@router.get("/api/keys/{key_id}", response_model=m.KeyDetailResp, summary="Key 详情")
async def api_key_detail(
    key_id: str,
    service: ReportService = Depends(get_service),
    days: int = Query(7, ge=1, le=90, description="趋势窗口的配额日数"),
) -> m.KeyDetailResp:
    """单 Key 详情 + 近 N 配额日用量趋势 + 模型偏好 + 错误分布。"""
    detail = await service.key_detail(key_id, days)
    if detail is None:
        raise HTTPException(status_code=404, detail=f"Key 不存在：{key_id}")
    return detail


@router.get("/api/usage/trend", response_model=m.TrendResp, summary="用量趋势")
async def api_usage_trend(
    service: ReportService = Depends(get_service),
    days: int = Query(7, ge=1, le=180, description="配额日数"),
) -> m.TrendResp:
    """按**配额日**聚合的用量趋势。无数据的配额日补零，保证曲线连续。"""
    return await service.usage_trend(days)


@router.get("/api/usage/by-user", response_model=m.UserUsageResp, summary="按用户聚合用量")
async def api_usage_by_user(
    service: ReportService = Depends(get_service),
    days: int = Query(30, ge=1, le=365),
    limit: int = Query(100, ge=1, le=1000),
) -> m.UserUsageResp:
    """按用户计费口径聚合用量（P1-9）。

    包含 ``user_id`` 为空的未归属流量（显示为「(未归属)」），
    计费对账必须能看到全量，不能静默丢弃。
    """
    return await service.usage_by_user(days, limit)


@router.get("/api/errors", response_model=m.ErrorsResp, summary="错误分布")
async def api_errors(
    service: ReportService = Depends(get_service),
    hours: int = Query(24, ge=1, le=720, description="回溯小时数"),
) -> m.ErrorsResp:
    """按错误码、HTTP 状态、Key 维度的错误分布，附逐小时时间线。

    注意此接口按 ``created_at`` 自然时间回溯，与配额日聚合口径不同 ——
    排障关心的是「最近 N 小时」，而非配额周期。
    """
    return await service.errors(hours)


@router.get("/api/egress", response_model=m.EgressResp, summary="出口 IP 状态")
async def api_egress(service: ReportService = Depends(get_service)) -> m.EgressResp:
    """出口 IP 状态与负载分布，含未绑定 Key 数与未注册的孤儿 IP。"""
    return await service.egress()


@router.get("/api/quota/health", response_model=m.QuotaHealthResp, summary="配额健康自检")
async def api_quota_health(
    service: ReportService = Depends(get_service),
    limit: int = Query(50, ge=1, le=500),
) -> m.QuotaHealthResp:
    """配额健康自检：租约泄漏、接近硬水位、对账偏差。"""
    return await service.quota_health(limit)


@router.get("/api/refresh/status", response_model=m.RefreshStatusResp, summary="刷新窗口状态")
async def api_refresh_status(
    service: ReportService = Depends(get_service),
    limit: int = Query(100, ge=1, le=1000),
) -> m.RefreshStatusResp:
    """配额刷新窗口状态。

    ``risky_keys`` 是重点：刷新未确认却仍在消耗的 Key，本地计数可能已清零，
    继续放量就是超刷。
    """
    return await service.refresh_status(limit)


@router.get(
    "/api/behavior/similarity",
    response_model=m.SimilarityResp,
    summary="行为相似度自检",
)
async def api_similarity(
    service: ReportService = Depends(get_service),
    days: int = Query(7, ge=1, le=90),
    threshold: float | None = Query(None, ge=0.0, le=1.0, description="告警阈值"),
    min_requests: int = Query(DEFAULT_MIN_REQUESTS, ge=1, description="最小样本请求数"),
    top_n: int = Query(50, ge=1, le=500),
) -> m.SimilarityResp:
    """行为相似度离线自检报告。

    基于 ``usage_records`` 计算 Key 间行为向量的余弦相似度
    （24 维请求时段分布 + 模型偏好分布）。相似度超过阈值（默认 0.6）的
    Key 对会被标记告警 —— 火山商务反馈的封禁根因正是「用户行为规律相似」。

    这是**离线报表**，不参与请求热路径的调度决策。
    """
    return await service.similarity(days, threshold, min_requests, top_n)


@router.get("/healthz", response_model=m.HealthResp, summary="健康检查")
async def healthz(service: ReportService = Depends(get_service)) -> m.HealthResp:
    """探活。Postgres / Redis 任一不可用时 status 为 degraded，但仍返回 200。"""
    return await service.health(__version__)
