"""配额日（quota day）计算。

火山引擎的免费额度在每日 **12:00** 刷新，而自然日在 00:00 翻新。若直接用自然日
做统计口径，00:00-12:00 这 12 小时会被算作「新的一天」，但火山侧此时仍在消耗
前一日的额度 —— 报表会把同一份额度拆到两天里，跨 12:00 的用量统计必然错乱。

这与 Go 侧 ``internal/quota/quotaday.go`` 是同一套定义，必须严格保持一致：
**12:00 之前的时刻归属于前一个自然日**。

注意时区：Go 网关用的是进程本地时区（``time.Now()``），本模块用
``DASHBOARD_TZ``（默认 ``Asia/Shanghai``）。两者必须一致，否则 Redis 配额 Key
的日期后缀会对不上。
"""

from __future__ import annotations

from datetime import date, datetime, timedelta
from zoneinfo import ZoneInfo

# 火山侧配额刷新的起始小时（12:00-13:00 刷新窗口）。
REFRESH_HOUR = 12

# Redis 配额 Key 里日期后缀的格式，与 Go 的 "20060102" 对应。
_REDIS_DAY_FORMAT = "%Y%m%d"


def quota_day(moment: datetime) -> date:
    """返回 ``moment`` 所属的配额日。

    12:00 之前归属前一个自然日。用 :class:`~datetime.date` 做减法而非对
    datetime 做减法，可以天然跨月、跨年，也避免夏令时切换导致的偏移。

    >>> quota_day(datetime(2026, 1, 1, 11, 59))
    datetime.date(2025, 12, 31)
    >>> quota_day(datetime(2026, 1, 1, 12, 0))
    datetime.date(2026, 1, 1)
    """
    day = moment.date()
    if moment.hour < REFRESH_HOUR:
        day -= timedelta(days=1)
    return day


def quota_day_key(moment: datetime) -> str:
    """返回配额日的 Redis Key 后缀，形如 ``20260823``。"""
    return quota_day(moment).strftime(_REDIS_DAY_FORMAT)


def day_to_key(day: date) -> str:
    """把配额日（date）转成 Redis Key 后缀。"""
    return day.strftime(_REDIS_DAY_FORMAT)


def quota_day_start(moment: datetime, tz: ZoneInfo | None = None) -> datetime:
    """返回 ``moment`` 所属配额日的起始时刻，即该自然日的 12:00。"""
    day = quota_day(moment)
    tzinfo = tz or moment.tzinfo
    return datetime(day.year, day.month, day.day, REFRESH_HOUR, tzinfo=tzinfo)


def quota_day_end(moment: datetime, tz: ZoneInfo | None = None) -> datetime:
    """返回 ``moment`` 所属配额日的结束时刻，即次日 12:00。"""
    start = quota_day_start(moment, tz)
    return start + timedelta(days=1)


def recent_quota_days(moment: datetime, days: int) -> list[date]:
    """返回截至 ``moment`` 所属配额日的最近 ``days`` 个配额日，按时间升序。

    ``days <= 0`` 时返回空列表，供上层做「无数据」处理而不是抛异常。
    """
    if days <= 0:
        return []
    today = quota_day(moment)
    return [today - timedelta(days=offset) for offset in range(days - 1, -1, -1)]


def quota_day_progress(moment: datetime, tz: ZoneInfo | None = None) -> float:
    """返回当前配额日已过去的比例，取值 ``[0, 1]``。

    看板用它判断「按当前速度推算，今日额度是否会提前耗尽」。
    """
    start = quota_day_start(moment, tz)
    elapsed = (moment - start).total_seconds()
    ratio = elapsed / 86400.0
    return min(1.0, max(0.0, ratio))
