"""报表业务层：把 Postgres 落库数据与 Redis 热态拼成响应模型。

分工约定：
- ``db.py`` 只做 SQL，``cache.py`` 只做 Redis，本模块只做组装与派生计算；
- 所有聚合都以**配额日**为口径（12:00 刷新），不使用自然日；
- 任何数据源为空都要产出结构完整的响应，绝不抛异常。
"""

from __future__ import annotations

import asyncio
from collections.abc import Iterable
from datetime import date, datetime, timedelta
from typing import Any

from . import db as q
from . import models as m
from .cache import KIND_COUNT, KIND_TOKEN, Cache, QuotaSnapshot
from .config import Settings
from .db import Database
from .gateway import GatewayClient, GatewayError
from .quotaday import (
    quota_day,
    quota_day_progress,
    quota_day_start,
    recent_quota_days,
)
from .similarity import DEFAULT_MIN_REQUESTS, analyze, build_vectors

# 配额水位接近硬水位的告警阈值。
NEAR_HARD_RATIO = 0.85
# prededuct 占硬水位比例超过此值，且与租约求和差额显著，视为疑似租约泄漏。
LEASE_LEAK_RATIO = 0.15
# 单次相似度报表最多取样的 Key 数，控制 O(n²) 比较量。
SIMILARITY_MAX_KEYS = 400
# 按水位排序时需要在 Python 侧拿到全量 Key，这里限定一次扫描的上限。
HOT_SORT_SCAN_LIMIT = 2000


def _num(value: Any, default: int = 0) -> int:
    """把 Record 字段安全转成 int。"""
    if value is None:
        return default
    try:
        return int(value)
    except (TypeError, ValueError):
        return default


def _ratio(part: int, whole: int) -> float:
    """安全求比例。分母为 0 时返回 0，而不是抛 ZeroDivisionError。"""
    if whole <= 0:
        return 0.0
    return round(part / whole, 6)


def _ts(value: Any) -> datetime | None:
    """把网关 JSON 里的 RFC3339 时间串转成 datetime，无法解析则返回 None。

    Go 侧这些字段带 ``omitempty``，未被封的出口根本不会出现该键，取到 None
    是正常路径而非异常。零值时间（``0001-01-01T00:00:00Z``）也当作"无"——
    Go 的 ``time.Time`` 零值在个别序列化路径下仍会被写出。

    解析失败一律降级为 None 而不抛异常：出口面板是运维排障的入口，
    不能因为一个时间字段格式意外就整页 500。
    """
    if value in (None, ""):
        return None
    if isinstance(value, datetime):
        return value
    try:
        # Go 用 "Z" 表示 UTC，Python 3.11 前的 fromisoformat 不认
        parsed = datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    except (TypeError, ValueError):
        return None
    return None if parsed.year <= 1 else parsed


def _quota_model(snap: QuotaSnapshot) -> m.KeyQuota:
    return m.KeyQuota(**snap.as_dict())


def _buckets(rows: Iterable[Any]) -> list[m.PoolBucket]:
    return [m.PoolBucket(name=str(r["name"] or "(空)"), count=_num(r["count"])) for r in rows]


class ReportService:
    """只读报表服务。"""

    def __init__(
        self,
        database: Database,
        cache: Cache,
        settings: Settings,
        gateway: GatewayClient | None = None,
    ) -> None:
        self._db = database
        self._cache = cache
        self._settings = settings
        # 出口 IP 的实时状态只存在于网关的内存出口池里，库中无处可查，
        # 故只读服务在此破例持有一个网关读客户端。
        # 仍不用它做任何写操作 —— 写路径唯一化的约束不变。
        self._gateway = gateway

    # ------------------------------------------------------------ 工具

    def now(self) -> datetime:
        return self._settings.now()

    def today(self) -> date:
        return quota_day(self.now())

    # ------------------------------------------------------------ 概览

    async def overview(self) -> m.OverviewResp:
        now = self.now()
        day = quota_day(now)

        # 六个数据源相互独立，gather 并发把首屏延迟从「各查询之和」
        # 压到「最慢者」。看板与网关共用 Postgres，串行瀑布在库抖动时
        # 会被逐项放大。
        usage, counts, pools, statuses, key_ids = await asyncio.gather(
            q.overview_usage(self._db, day),
            q.key_counts(self._db),
            q.key_pool_distribution(self._db),
            q.key_status_distribution(self._db),
            q.active_key_ids(self._db),
        )
        # snapshots 依赖 key_ids，quota_health 内部另拉全量，二者再并发一轮。
        snaps, health = await asyncio.gather(
            self._cache.snapshots(key_ids, day, KIND_TOKEN),
            # 告警数与 quota/health 口径一致，避免两个页面数字打架。
            self.quota_health(limit=200),
        )

        ratios = [s.ratio for s in snaps.values()]
        capacity = sum(s.hard for s in snaps.values())
        remaining = sum(s.remaining for s in snaps.values())

        requests = _num(usage["requests"]) if usage else 0
        errors = _num(usage["errors"]) if usage else 0

        return m.OverviewResp(
            quota_day=day,
            quota_day_progress=round(quota_day_progress(now), 4),
            generated_at=now,
            total_tokens=_num(usage["total_tokens"]) if usage else 0,
            prompt_tokens=_num(usage["prompt_tokens"]) if usage else 0,
            completion_tokens=_num(usage["completion_tokens"]) if usage else 0,
            total_requests=requests,
            total_count_units=_num(usage["count_units"]) if usage else 0,
            total_keys=_num(counts["total"]) if counts else 0,
            active_keys=_num(counts["active"]) if counts else 0,
            used_keys=_num(usage["used_keys"]) if usage else 0,
            pools=_buckets(pools),
            statuses=_buckets(statuses),
            avg_quota_ratio=round(sum(ratios) / len(ratios), 6) if ratios else 0.0,
            max_quota_ratio=round(max(ratios), 6) if ratios else 0.0,
            remaining_tokens=remaining,
            capacity_tokens=capacity,
            error_rate=_ratio(errors, requests),
            error_requests=errors,
            avg_latency_ms=int(usage["avg_latency_ms"]) if usage else 0,
            unconfirmed_refresh=_num(counts["unconfirmed_refresh"]) if counts else 0,
            alert_count=len(health.alerts),
        )

    # ------------------------------------------------------------ Key 列表

    async def list_keys(
        self,
        *,
        status: str | None = None,
        pool: str | None = None,
        sort: str = "key_id",
        desc: bool = False,
        limit: int = 100,
        offset: int = 0,
    ) -> m.KeyListResp:
        """Key 列表。

        水位类排序（``ratio`` / ``remaining`` / ``used``）的数据在 Redis，
        无法下推到 SQL，因此这类排序在 Python 侧对当前页之外的全量做处理：
        先按 key_id 取全量再排序切片。Key 规模是千级，代价可接受。
        """
        day = self.today()
        hot_sort = sort in {"ratio", "remaining", "used"}

        if hot_sort:
            rows = await q.list_keys(
                self._db,
                day=day,
                status=status,
                pool=pool,
                sort="key_id",
                desc=False,
                limit=HOT_SORT_SCAN_LIMIT,
                offset=0,
            )
        else:
            sql_sort = sort if sort in q.KEY_SORT_FIELDS else "key_id"
            rows = await q.list_keys(
                self._db,
                day=day,
                status=status,
                pool=pool,
                sort=sql_sort,
                desc=desc,
                limit=limit,
                offset=offset,
            )

        total = await q.count_keys(self._db, status, pool)
        key_ids = [str(r["key_id"]) for r in rows]
        token_snaps, count_snaps = await asyncio.gather(
            self._cache.snapshots(key_ids, day, KIND_TOKEN),
            self._cache.snapshots(key_ids, day, KIND_COUNT),
        )

        items = [self._key_row(r, token_snaps, count_snaps) for r in rows]

        if hot_sort:

            def sort_value(row: m.KeyRow) -> float:
                if sort == "remaining":
                    return float(row.token.remaining)
                if sort == "used":
                    return float(row.token.used)
                return row.token.ratio

            items.sort(key=sort_value, reverse=desc)
            items = items[offset : offset + limit]

        return m.KeyListResp(total=total, quota_day=day, items=items)

    def _key_row(
        self,
        row: Any,
        token_snaps: dict[str, QuotaSnapshot],
        count_snaps: dict[str, QuotaSnapshot],
    ) -> m.KeyRow:
        key_id = str(row["key_id"])
        token = token_snaps.get(key_id)
        count = count_snaps.get(key_id)
        return m.KeyRow(
            key_id=key_id,
            pool=str(row["pool"] or ""),
            status=str(row["status"] or ""),
            persona_id=str(row["persona_id"] or ""),
            egress_ip=str(row["egress_ip"] or ""),
            health_score=_num(row["health_score"]),
            refresh_state=str(row["refresh_state"] or "idle"),
            refresh_confirmed_at=row["refresh_confirmed_at"],
            last_error=str(row["last_error"] or ""),
            last_used_at=row["last_used_at"],
            token=_quota_model(token) if token else m.KeyQuota(),
            count=_quota_model(count) if count else m.KeyQuota(kind=KIND_COUNT),
            today_tokens=_num(row["today_tokens"]),
            today_requests=_num(row["today_requests"]),
            today_errors=_num(row["today_errors"]),
        )

    # ------------------------------------------------------------ Key 详情

    async def key_detail(self, key_id: str, days: int = 7) -> m.KeyDetailResp | None:
        """单 Key 详情。Key 不存在时返回 None，由路由层转 404。"""
        now = self.now()
        day = quota_day(now)
        row = await q.get_key(self._db, key_id, day)
        if row is None:
            return None

        window = recent_quota_days(now, days)
        (
            token_snaps,
            count_snaps,
            trend_rows,
            model_rows,
            error_rows,
            leases,
        ) = await asyncio.gather(
            self._cache.snapshots([key_id], day, KIND_TOKEN),
            self._cache.snapshots([key_id], day, KIND_COUNT),
            q.key_trend(self._db, key_id, window),
            q.key_model_usage(self._db, key_id, window),
            q.key_error_buckets(self._db, key_id, window),
            self._cache.lease_aggregate(day, int(now.timestamp())),
        )

        by_day = {r["quota_day"]: r for r in trend_rows}
        trend = [
            m.KeyDayUsage(
                quota_day=d,
                total_tokens=_num(by_day[d]["total_tokens"]) if d in by_day else 0,
                prompt_tokens=_num(by_day[d]["prompt_tokens"]) if d in by_day else 0,
                completion_tokens=(_num(by_day[d]["completion_tokens"]) if d in by_day else 0),
                count_units=_num(by_day[d]["count_units"]) if d in by_day else 0,
                requests=_num(by_day[d]["requests"]) if d in by_day else 0,
                errors=_num(by_day[d]["errors"]) if d in by_day else 0,
                avg_latency_ms=(int(by_day[d]["avg_latency_ms"]) if d in by_day else 0),
            )
            for d in window
        ]

        total_errors = sum(_num(r["count"]) for r in error_rows)
        return m.KeyDetailResp(
            key=self._key_row(row, token_snaps, count_snaps),
            trend=trend,
            models=[
                m.ModelUsage(
                    model=str(r["model"] or "(未指定)"),
                    requests=_num(r["requests"]),
                    total_tokens=_num(r["total_tokens"]),
                )
                for r in model_rows
            ],
            recent_errors=[
                m.ErrorBucket(
                    label=str(r["label"] or ""),
                    count=_num(r["count"]),
                    ratio=_ratio(_num(r["count"]), total_errors),
                )
                for r in error_rows
            ],
            lease_count=leases.count_of(key_id, KIND_TOKEN),
            lease_amount=leases.amount_of(key_id, KIND_TOKEN),
        )

    # ------------------------------------------------------------ 用量趋势

    async def usage_trend(self, days: int = 7) -> m.TrendResp:
        now = self.now()
        window = recent_quota_days(now, days)
        rows = await q.usage_trend(self._db, window)
        by_day = {r["quota_day"]: r for r in rows}

        points: list[m.TrendPoint] = []
        for d in window:
            row = by_day.get(d)
            requests = _num(row["requests"]) if row else 0
            errors = _num(row["errors"]) if row else 0
            points.append(
                m.TrendPoint(
                    quota_day=d,
                    total_tokens=_num(row["total_tokens"]) if row else 0,
                    prompt_tokens=_num(row["prompt_tokens"]) if row else 0,
                    completion_tokens=_num(row["completion_tokens"]) if row else 0,
                    count_units=_num(row["count_units"]) if row else 0,
                    requests=requests,
                    errors=errors,
                    active_keys=_num(row["active_keys"]) if row else 0,
                    avg_latency_ms=int(row["avg_latency_ms"]) if row else 0,
                    error_rate=_ratio(errors, requests),
                )
            )

        return m.TrendResp(days=days, quota_day=quota_day(now), points=points)

    # ------------------------------------------------------------ 按用户聚合

    async def usage_by_user(self, days: int = 30, limit: int = 100) -> m.UserUsageResp:
        """按用户计费口径聚合用量（P1-9）。

        含 ``user_id IS NULL`` 的未归属流量，计费对账需要看到全量。
        """
        now = self.now()
        window = recent_quota_days(now, days)
        end_day = quota_day(now)
        start_day = window[0] if window else end_day

        rows = await q.usage_by_user(self._db, start_day, end_day, limit)
        user_ids = [_num(r["user_id"]) for r in rows if r["user_id"] is not None]
        daily_rows = await q.usage_by_user_daily(self._db, start_day, end_day, user_ids)

        daily_map: dict[int | None, list[m.TrendPoint]] = {}
        for r in daily_rows:
            uid = _num(r["user_id"]) if r["user_id"] is not None else None
            requests = _num(r["requests"])
            errors = _num(r["errors"])
            daily_map.setdefault(uid, []).append(
                m.TrendPoint(
                    quota_day=r["quota_day"],
                    total_tokens=_num(r["total_tokens"]),
                    requests=requests,
                    errors=errors,
                    error_rate=_ratio(errors, requests),
                )
            )

        items: list[m.UserUsageRow] = []
        for r in rows:
            uid = _num(r["user_id"]) if r["user_id"] is not None else None
            requests = _num(r["requests"])
            errors = _num(r["errors"])
            items.append(
                m.UserUsageRow(
                    user_id=uid,
                    user_name=str(r["user_name"] or "(未归属)"),
                    email=r["email"],
                    status=str(r["status"] or ""),
                    daily_token_limit=_num(r["daily_token_limit"]),
                    total_tokens=_num(r["total_tokens"]),
                    prompt_tokens=_num(r["prompt_tokens"]),
                    completion_tokens=_num(r["completion_tokens"]),
                    count_units=_num(r["count_units"]),
                    requests=requests,
                    errors=errors,
                    error_rate=_ratio(errors, requests),
                    avg_latency_ms=int(r["avg_latency_ms"] or 0),
                    active_days=_num(r["active_days"]),
                    first_day=r["first_day"],
                    last_day=r["last_day"],
                    daily=daily_map.get(uid, []),
                )
            )

        return m.UserUsageResp(
            days=days,
            start_day=start_day,
            end_day=end_day,
            total_tokens=sum(i.total_tokens for i in items),
            total_requests=sum(i.requests for i in items),
            items=items,
        )

    # ------------------------------------------------------------ 错误

    async def errors(self, hours: int = 24) -> m.ErrorsResp:
        now = self.now()
        since = now - timedelta(hours=max(1, hours))

        summary = await q.error_summary(self._db, since)
        by_code = await q.errors_by_code(self._db, since)
        by_status = await q.errors_by_status(self._db, since)
        by_key = await q.errors_by_key(self._db, since)
        timeline = await q.errors_timeline(self._db, since)

        requests = _num(summary["requests"]) if summary else 0
        errors = _num(summary["errors"]) if summary else 0

        return m.ErrorsResp(
            hours=hours,
            since=since,
            total_requests=requests,
            total_errors=errors,
            error_rate=_ratio(errors, requests),
            by_code=[
                m.ErrorBucket(
                    label=str(r["label"] or ""),
                    count=_num(r["count"]),
                    ratio=_ratio(_num(r["count"]), errors),
                )
                for r in by_code
            ],
            by_status=[
                m.ErrorBucket(
                    label=str(r["label"] or ""),
                    count=_num(r["count"]),
                    ratio=_ratio(_num(r["count"]), errors),
                )
                for r in by_status
            ],
            by_key=[
                m.ErrorKeyRow(
                    key_id=str(r["key_id"]),
                    pool=str(r["pool"] or ""),
                    status=str(r["status"] or ""),
                    requests=_num(r["requests"]),
                    errors=_num(r["errors"]),
                    error_rate=_ratio(_num(r["errors"]), _num(r["requests"])),
                    top_error=str(r["top_error"] or ""),
                )
                for r in by_key
            ],
            timeline=[
                m.ErrorTimePoint(
                    hour=r["hour"],
                    requests=_num(r["requests"]),
                    errors=_num(r["errors"]),
                    error_rate=_ratio(_num(r["errors"]), _num(r["requests"])),
                )
                for r in timeline
            ],
        )

    # ------------------------------------------------------------ 出口 IP

    async def egress(self, actor: str = "dashboard") -> m.EgressResp:
        """出口 IP 状态。

        数据来自两处，各自是对应字段的唯一事实来源:

        - **状态**（state / reputation / max_keys / pool / bound_keys）取自网关
          `GET /admin/ips`，即 `egress.Pool` 的内存快照。
        - **用量**（today_tokens / requests / errors）取自 `usage_records`，
          网关不保留历史流水。

        不从 `egress_ips` 表读状态。那张表在网关代码里从未被写入（全仓库只有
        schema 定义与测试种子），`state` 永远是 'active'、`reputation` 永远
        是 100 —— 一个已被封禁的出口会被显示成健康，运维据此判断会直接出错。
        """
        day = self.today()
        usage = await q.egress_usage(self._db, day)
        unbound = await q.unbound_key_count(self._db)

        live, stale_reason = await self._live_egress()

        items: list[m.EgressRow] = []
        seen: set[str] = set()
        for ip in live:
            addr = str(ip.get("addr") or "")
            if not addr:
                continue
            seen.add(addr)
            u = usage.get(addr, {})
            requests = _num(u.get("today_requests"))
            errors = _num(u.get("today_errors"))
            max_keys = _num(ip.get("max_keys"))
            bound = _num(ip.get("bound_keys"))
            items.append(
                m.EgressRow(
                    addr=addr,
                    public_ip=str(ip.get("public_ip") or ""),
                    # region / isp 是静态标注信息，网关不持有。
                    # 保留字段但留空，避免前端列结构随数据源变化。
                    region="",
                    isp="",
                    state=str(ip.get("state") or ""),
                    reputation=_num(ip.get("reputation")),
                    max_keys=max_keys,
                    pool=str(ip.get("pool") or ""),
                    bound_keys=bound,
                    # db_bound_keys 与 bound_keys 的差额是有价值的信号:
                    # 两者不等说明内存绑定与库不一致（重启未回灌、或改库没同步网关）。
                    db_bound_keys=_num(u.get("db_bound_keys")),
                    # 网关不区分 Key 自身的启用状态，active_keys 只能来自库。
                    active_keys=_num(u.get("active_keys")),
                    load_ratio=_ratio(bound, max_keys),
                    today_tokens=_num(u.get("today_tokens")),
                    today_requests=requests,
                    today_errors=errors,
                    error_rate=_ratio(errors, requests),
                    # 网关用 omitempty 序列化，未被封的出口不会带这三个键。
                    banned_at=_ts(ip.get("banned_at")),
                    ban_count=_num(ip.get("ban_count")),
                    unban_at=_ts(ip.get("unban_at")),
                )
            )

        # 流水里出现过但已不在出口池中的地址。
        #
        # 与旧口径（未在 egress_ips 注册）不同: 这里指的是「配置里已移除但今天
        # 仍有请求从它发出」，通常是刚改了 EGRESS_IPS 还没重启，或该 IP 被摘除
        # 后仍有连接在复用。两者都需要运维知道。
        orphans = sorted(a for a in usage if a not in seen)

        return m.EgressResp(
            quota_day=day,
            total=len(items),
            unbound_keys=unbound,
            orphan_ips=orphans,
            stale=bool(stale_reason),
            stale_reason=stale_reason,
            items=items,
        )

    async def _live_egress(self) -> tuple[list[dict[str, Any]], str]:
        """取网关的出口快照。失败时返回空列表与降级原因，不抛异常。

        出口面板只是若干指标之一，网关不可达时应当把这一块标注为「取不到」
        而让其余面板照常渲染。抛异常会让整个 /api/egress 返回 5xx，
        前端拿不到 quota_day 与 unbound_keys 这些本可用的字段。
        """
        if self._gateway is None:
            return [], "看板未配置网关地址，无法读取出口实时状态"
        try:
            resp = await self._gateway.fetch_egress_ips("dashboard")
        except GatewayError as exc:
            return [], f"读取网关出口状态失败：{exc.detail}"

        payload = resp.payload
        if not isinstance(payload, dict):
            return [], "网关返回的出口状态格式异常"
        per_ip = payload.get("per_ip")
        if not isinstance(per_ip, list):
            return [], "网关返回的出口状态缺少 per_ip"
        return [x for x in per_ip if isinstance(x, dict)], ""

    # ------------------------------------------------------------ 配额健康

    async def quota_health(self, limit: int = 50) -> m.QuotaHealthResp:
        """配额健康自检。

        三类问题：
        1. **租约泄漏**：``prededuct`` 明显高于未过期租约求和，说明有预扣未被
           Commit/Release/Reap 回收（P0-2 要防的就是这个）；
        2. **接近硬水位**：水位 ≥ 85%，继续调度有超刷风险；
        3. **对账偏差**：``quota_drift_logs`` 里的历史偏差记录。
        """
        now = self.now()
        day = quota_day(now)
        key_ids = await q.active_key_ids(self._db)
        snaps, leases, drift_rows, key_rows = await asyncio.gather(
            self._cache.snapshots(key_ids, day, KIND_TOKEN),
            self._cache.lease_aggregate(day, int(now.timestamp())),
            q.recent_drifts(self._db, limit),
            q.all_key_rows(self._db),
        )

        alerts: list[m.QuotaAlert] = []
        leaks: list[m.LeaseLeakRow] = []
        near_hard: list[m.NearHardRow] = []
        missing_hot: list[str] = []

        pool_of = {str(r["key_id"]): str(r["pool"] or "") for r in key_rows}

        for key_id in key_ids:
            snap = snaps.get(key_id)
            if snap is None:
                continue
            if not snap.hot:
                missing_hot.append(key_id)
                continue

            lease_amount = leases.amount_of(key_id, KIND_TOKEN)
            lease_count = leases.count_of(key_id, KIND_TOKEN)
            gap = snap.prededuct - lease_amount
            pre_ratio = _ratio(snap.prededuct, snap.hard)

            # 预扣显著高于租约求和，且占硬水位有一定比重时才告警，
            # 避免把「租约刚建立、ZSET 尚未可见」的瞬时状态误报成泄漏。
            if gap > 0 and pre_ratio >= LEASE_LEAK_RATIO:
                leaks.append(
                    m.LeaseLeakRow(
                        key_id=key_id,
                        prededuct=snap.prededuct,
                        lease_amount=lease_amount,
                        lease_count=lease_count,
                        gap=gap,
                        ratio=pre_ratio,
                    )
                )
                alerts.append(
                    m.QuotaAlert(
                        level="critical" if pre_ratio >= 0.3 else "warn",
                        kind="lease_leak",
                        key_id=key_id,
                        message=(
                            f"预扣 {snap.prededuct} 高于未过期租约总量 "
                            f"{lease_amount}，差额 {gap}，疑似租约泄漏"
                        ),
                        value=pre_ratio,
                    )
                )

            if snap.ratio >= NEAR_HARD_RATIO:
                near_hard.append(
                    m.NearHardRow(
                        key_id=key_id,
                        pool=pool_of.get(key_id, ""),
                        used=snap.used,
                        prededuct=snap.prededuct,
                        hard_limit=snap.hard,
                        ratio=round(snap.ratio, 6),
                    )
                )
                alerts.append(
                    m.QuotaAlert(
                        level="critical" if snap.ratio >= 0.98 else "warn",
                        kind="near_hard",
                        key_id=key_id,
                        message=f"配额水位 {snap.ratio:.1%}，接近硬水位",
                        value=round(snap.ratio, 6),
                    )
                )

        drifts = [
            m.DriftRow(
                key_id=str(r["key_id"]),
                billing_kind=str(r["billing_kind"] or "token"),
                quota_day=r["quota_day"],
                drift=_num(r["drift"]),
                created_at=r["created_at"],
            )
            for r in drift_rows
        ]
        for d in drifts[:limit]:
            if d.drift != 0:
                alerts.append(
                    m.QuotaAlert(
                        level="warn",
                        kind="drift",
                        key_id=d.key_id,
                        message=f"配额日 {d.quota_day} 对账偏差 {d.drift}",
                        value=float(d.drift),
                    )
                )

        if leases.expired > 0:
            alerts.append(
                m.QuotaAlert(
                    level="warn",
                    kind="lease_leak",
                    message=(
                        f"租约集合中有 {leases.expired} 条已过期未回收，请检查网关回收器是否在运行"
                    ),
                    value=float(leases.expired),
                )
            )

        leaks.sort(key=lambda x: -x.gap)
        near_hard.sort(key=lambda x: -x.ratio)
        alerts.sort(key=lambda a: (a.level != "critical", -a.value))

        return m.QuotaHealthResp(
            quota_day=day,
            checked_keys=len(key_ids),
            lease_zset_size=leases.total,
            expired_leases=leases.expired,
            alerts=alerts[:limit],
            lease_leaks=leaks[:limit],
            near_hard=near_hard[:limit],
            drifts=drifts,
            missing_hot_keys=missing_hot[:limit],
        )

    # ------------------------------------------------------------ 刷新状态

    async def refresh_status(self, limit: int = 100) -> m.RefreshStatusResp:
        """配额刷新窗口状态。

        风险判定：``refresh_state != 'confirmed'`` 却仍有用量的 Key —— 系统
        无法确认火山侧是否已刷新，本地计数可能已被清零，继续放量就是超刷。
        """
        now = self.now()
        day = quota_day(now)
        states = await q.refresh_state_distribution(self._db)
        rows = await q.all_key_rows(self._db)
        used_map = await q.key_used_on_day(self._db, day)
        key_ids = [str(r["key_id"]) for r in rows]
        snaps = await self._cache.snapshots(key_ids, day, KIND_TOKEN)

        confirmed = 0
        unconfirmed_keys: list[m.RefreshKeyRow] = []
        risky: list[m.RefreshKeyRow] = []

        for r in rows:
            key_id = str(r["key_id"])
            state = str(r["refresh_state"] or "idle")
            snap = snaps.get(key_id)
            item = m.RefreshKeyRow(
                key_id=key_id,
                pool=str(r["pool"] or ""),
                status=str(r["status"] or ""),
                refresh_state=state,
                refresh_confirmed_at=r["refresh_confirmed_at"],
                last_error=str(r["last_error"] or ""),
                used_after_refresh=used_map.get(key_id, 0),
                hot_used=snap.used if snap else 0,
            )
            if state == "confirmed":
                confirmed += 1
                continue
            unconfirmed_keys.append(item)
            if item.used_after_refresh > 0 or item.hot_used > 0:
                risky.append(item)

        unconfirmed_keys.sort(key=lambda x: -x.used_after_refresh)
        risky.sort(key=lambda x: -x.used_after_refresh)

        window_start = quota_day_start(now)
        # 探测窗口 12:00-14:00，与网关 Refresh.WindowEnd 默认值一致。
        window_end = window_start + timedelta(hours=2)

        return m.RefreshStatusResp(
            quota_day=day,
            in_window=window_start <= now < window_end,
            window_start=window_start,
            window_end=window_end,
            states=_buckets(states),
            confirmed=confirmed,
            unconfirmed=len(unconfirmed_keys),
            unconfirmed_keys=unconfirmed_keys[:limit],
            risky_keys=risky[:limit],
        )

    # ------------------------------------------------------------ 行为相似度

    async def similarity(
        self,
        days: int = 7,
        threshold: float | None = None,
        min_requests: int = DEFAULT_MIN_REQUESTS,
        top_n: int = 50,
    ) -> m.SimilarityResp:
        """行为相似度离线自检报告。"""
        now = self.now()
        window = recent_quota_days(now, days)
        end_day = quota_day(now)
        start_day = window[0] if window else end_day
        limit = threshold if threshold is not None else self._settings.similarity_alert

        hour_rows = await q.behavior_hour_histogram(self._db, start_day, end_day, min_requests)
        model_rows = await q.behavior_model_histogram(self._db, start_day, end_day, min_requests)

        vectors = build_vectors(
            [(str(r["volc_key_id"]), _num(r["hour"]), _num(r["requests"])) for r in hour_rows],
            [
                (str(r["volc_key_id"]), str(r["model"] or ""), _num(r["requests"]))
                for r in model_rows
            ],
        )
        # 控制 O(n²)：请求量最大的若干 Key 最有代表性，也最值得关注。
        vectors.sort(key=lambda v: -v.requests)
        vectors = vectors[:SIMILARITY_MAX_KEYS]

        report = analyze(vectors, threshold=limit, min_requests=min_requests, top_n=top_n)
        egress_map = await q.key_egress_map(self._db)

        pairs = [
            m.SimilarPair(
                key_a=p.key_a,
                key_b=p.key_b,
                similarity=p.similarity,
                hour_similarity=p.hour_similarity,
                model_similarity=p.model_similarity,
                egress_a=egress_map.get(p.key_a, ""),
                egress_b=egress_map.get(p.key_b, ""),
                same_egress=bool(
                    egress_map.get(p.key_a) and egress_map.get(p.key_a) == egress_map.get(p.key_b)
                ),
                # 同出口 IP 且行为高度相似，是最容易被风控聚簇的组合。
                level=(
                    "critical"
                    if p.similarity >= 0.8
                    or (
                        p.similarity > limit
                        and egress_map.get(p.key_a)
                        and egress_map.get(p.key_a) == egress_map.get(p.key_b)
                    )
                    else "warn"
                ),
            )
            for p in report.pairs
        ]

        profiles = [
            m.KeyBehaviorProfile(
                key_id=v.key_id,
                requests=v.requests,
                hour_hist=[round(x, 6) for x in v.hour_hist],
                top_models=v.top_models(),
                peak_hour=v.peak_hour,
                entropy=round(v.entropy, 4),
            )
            for v in vectors[:top_n]
        ]

        note = ""
        if report.compared_pairs == 0:
            note = f"可比 Key 不足（需请求数 ≥ {min_requests}），样本积累后报告才有意义"

        return m.SimilarityResp(
            days=days,
            start_day=start_day,
            end_day=end_day,
            threshold=limit,
            sampled_keys=len(vectors),
            compared_pairs=report.compared_pairs,
            alert_pairs=report.alert_pairs,
            max_similarity=report.max_similarity,
            avg_similarity=report.avg_similarity,
            pairs=pairs,
            profiles=profiles,
            note=note,
        )

    # ------------------------------------------------------------ 健康检查

    async def health(self, version: str) -> m.HealthResp:
        pg_ok = await self._db.ping()
        redis_ok = await self._cache.ping()
        status = "ok" if pg_ok and redis_ok else "degraded"
        return m.HealthResp(
            status=status,
            postgres="ok" if pg_ok else f"error: {self._db.error or '不可用'}",
            redis="ok" if redis_ok else f"error: {self._cache.error or '不可用'}",
            quota_day=self.today(),
            version=version,
        )
