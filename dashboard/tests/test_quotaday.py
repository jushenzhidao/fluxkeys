"""配额日逻辑单元测试。

这是整个看板最容易出错的地方：一旦配额日算错，所有用量报表都会在 12:00
前后出现「凭空多一天」或「用量拆成两半」的错误。因此边界必须逐个钉死。
"""

from __future__ import annotations

from datetime import date, datetime
from zoneinfo import ZoneInfo

import pytest

from app.quotaday import (
    REFRESH_HOUR,
    day_to_key,
    quota_day,
    quota_day_end,
    quota_day_key,
    quota_day_progress,
    quota_day_start,
    recent_quota_days,
)

SH = ZoneInfo("Asia/Shanghai")


class TestQuotaDayBoundary:
    """12:00 边界。刷新时刻本身归属**当日**，前一秒归属前一日。"""

    @pytest.mark.parametrize(
        ("moment", "expected"),
        [
            # 11:59:59 —— 仍在前一配额日
            (datetime(2026, 8, 23, 11, 59, 59), date(2026, 8, 22)),
            # 12:00:00 —— 刷新时刻，进入新配额日
            (datetime(2026, 8, 23, 12, 0, 0), date(2026, 8, 23)),
            # 12:00:01
            (datetime(2026, 8, 23, 12, 0, 1), date(2026, 8, 23)),
            # 00:00:00 —— 自然日刚翻新，但配额日仍是前一天
            (datetime(2026, 8, 23, 0, 0, 0), date(2026, 8, 22)),
            # 23:59:59 —— 与 12:00 同属一个配额日
            (datetime(2026, 8, 23, 23, 59, 59), date(2026, 8, 23)),
        ],
    )
    def test_boundary(self, moment: datetime, expected: date) -> None:
        assert quota_day(moment.replace(tzinfo=SH)) == expected

    def test_noon_and_prior_second_differ_by_one_day(self) -> None:
        """跨过 12:00 的相邻两秒，配额日必须恰好差一天。"""
        before = datetime(2026, 8, 23, 11, 59, 59, tzinfo=SH)
        after = datetime(2026, 8, 23, 12, 0, 0, tzinfo=SH)
        assert (quota_day(after) - quota_day(before)).days == 1

    def test_same_quota_day_spans_two_calendar_days(self) -> None:
        """同一配额日横跨两个自然日：当日 12:00 到次日 11:59。"""
        evening = datetime(2026, 8, 23, 20, 0, tzinfo=SH)
        next_morning = datetime(2026, 8, 24, 8, 0, tzinfo=SH)
        assert quota_day(evening) == quota_day(next_morning) == date(2026, 8, 23)

    def test_refresh_hour_is_twelve(self) -> None:
        """刷新小时必须与 Go 侧 quota.RefreshHour 一致。"""
        assert REFRESH_HOUR == 12


class TestQuotaDayGoContract:
    """与 Go 侧 ``internal/quota/quotaday.go`` 的契约对照。

    Go 的 QuotaDayTime 规则只有一句：``if t.Hour() < RefreshHour { t = t.AddDate(0,0,-1) }``，
    然后取该时刻的 Y/M/D。下面期望值全部按该规则**手工推导后硬编码**，
    不经由 Python 的 quota_day 生成 —— 否则测试自证，抓不到两侧漂移。

    秒级边界（11:59:59 / 12:00:00 / 12:00:01 / 00:00:00 / 23:59:59）已由
    TestQuotaDayBoundary 钉死；这里补上「24 个整点的完整归属划分」，
    覆盖其余 19 个整点，避免只在个别点对上、中间某小时错位却漏检。
    """

    def test_go_hour_partition_exhaustive(self) -> None:
        # 期望值 = 「hour < 12 归前一日」这一 Go 规则的独立复述，非函数反推。
        for hour in range(24):
            moment = datetime(2026, 8, 23, hour, 0, 0, tzinfo=SH)
            expected = date(2026, 8, 22) if hour < 12 else date(2026, 8, 23)
            assert quota_day(moment) == expected, f"{hour:02d}:00 归属错误"

    def test_go_rule_is_calendar_day_subtraction(self) -> None:
        """退一日必须是日历减法（跨月/跨年/闰日都对），而非固定 24h。"""
        assert quota_day(datetime(2026, 9, 1, 0, 0, 0, tzinfo=SH)) == date(2026, 8, 31)
        assert quota_day(datetime(2026, 1, 1, 11, 59, 59, tzinfo=SH)) == date(2025, 12, 31)
        assert quota_day(datetime(2024, 3, 1, 5, 0, 0, tzinfo=SH)) == date(2024, 2, 29)


class TestMonthYearRollover:
    """跨月与跨年。用 date 减法而非字符串拼接，这里验证不会出现 0 日/13 月。"""

    @pytest.mark.parametrize(
        ("moment", "expected"),
        [
            # 跨月：9 月 1 日上午归属 8 月 31 日
            (datetime(2026, 9, 1, 3, 0), date(2026, 8, 31)),
            (datetime(2026, 9, 1, 12, 0), date(2026, 9, 1)),
            # 跨月且前月为 30 天：7 月 1 日上午归属 6 月 30 日
            (datetime(2026, 7, 1, 11, 0), date(2026, 6, 30)),
            # 跨月且前月为 28 天：3 月 1 日上午归属 2 月 28 日（2026 非闰年）
            (datetime(2026, 3, 1, 6, 0), date(2026, 2, 28)),
            # 闰年 2 月 29：2024 年 3 月 1 日上午归属 2 月 29 日
            (datetime(2024, 3, 1, 6, 0), date(2024, 2, 29)),
            # 闰年 2 月 29 日上午归属 2 月 28 日
            (datetime(2024, 2, 29, 9, 0), date(2024, 2, 28)),
            # 跨年：元旦上午归属去年 12 月 31 日
            (datetime(2026, 1, 1, 0, 30), date(2025, 12, 31)),
            (datetime(2026, 1, 1, 11, 59), date(2025, 12, 31)),
            (datetime(2026, 1, 1, 12, 0), date(2026, 1, 1)),
        ],
    )
    def test_rollover(self, moment: datetime, expected: date) -> None:
        assert quota_day(moment.replace(tzinfo=SH)) == expected

    def test_redis_key_suffix_format(self) -> None:
        """Redis Key 后缀必须是 8 位 yyyyMMdd，与 Go 的 "20060102" 对齐。"""
        moment = datetime(2026, 1, 1, 3, 0, tzinfo=SH)
        assert quota_day_key(moment) == "20251231"
        assert day_to_key(date(2026, 9, 5)) == "20260905"


class TestQuotaDayWindow:
    """配额日的起止时刻。"""

    def test_start_is_noon_of_quota_day(self) -> None:
        moment = datetime(2026, 8, 24, 5, 0, tzinfo=SH)
        assert quota_day_start(moment) == datetime(2026, 8, 23, 12, 0, tzinfo=SH)

    def test_end_is_next_noon(self) -> None:
        moment = datetime(2026, 8, 23, 20, 0, tzinfo=SH)
        assert quota_day_end(moment) == datetime(2026, 8, 24, 12, 0, tzinfo=SH)

    def test_window_length_is_24h(self) -> None:
        moment = datetime(2026, 1, 1, 3, 0, tzinfo=SH)
        span = quota_day_end(moment) - quota_day_start(moment)
        assert span.total_seconds() == 86400

    def test_window_covers_moment(self) -> None:
        """任意时刻都必须落在自己所属配额日的窗口内（左闭右开）。"""
        for hour in range(24):
            moment = datetime(2026, 8, 23, hour, 30, tzinfo=SH)
            assert quota_day_start(moment) <= moment < quota_day_end(moment)

    def test_year_boundary_window(self) -> None:
        moment = datetime(2026, 1, 1, 2, 0, tzinfo=SH)
        assert quota_day_start(moment) == datetime(2025, 12, 31, 12, 0, tzinfo=SH)
        assert quota_day_end(moment) == datetime(2026, 1, 1, 12, 0, tzinfo=SH)


class TestRecentQuotaDays:
    def test_ascending_and_inclusive(self) -> None:
        moment = datetime(2026, 3, 2, 15, 0, tzinfo=SH)
        assert recent_quota_days(moment, 3) == [
            date(2026, 2, 28),
            date(2026, 3, 1),
            date(2026, 3, 2),
        ]

    def test_crosses_year(self) -> None:
        moment = datetime(2026, 1, 2, 13, 0, tzinfo=SH)
        assert recent_quota_days(moment, 4) == [
            date(2025, 12, 30),
            date(2025, 12, 31),
            date(2026, 1, 1),
            date(2026, 1, 2),
        ]

    def test_before_noon_excludes_current_calendar_day(self) -> None:
        """上午查询时，最后一个配额日应是前一个自然日。"""
        moment = datetime(2026, 3, 2, 9, 0, tzinfo=SH)
        assert recent_quota_days(moment, 2) == [date(2026, 2, 28), date(2026, 3, 1)]

    @pytest.mark.parametrize("days", [0, -1, -30])
    def test_non_positive_returns_empty(self, days: int) -> None:
        """非正数不抛异常，交由上层作为空数据处理。"""
        assert recent_quota_days(datetime(2026, 3, 2, 15, 0, tzinfo=SH), days) == []


class TestQuotaDayProgress:
    def test_at_refresh_is_zero(self) -> None:
        assert quota_day_progress(datetime(2026, 8, 23, 12, 0, tzinfo=SH)) == 0.0

    def test_midpoint_is_half(self) -> None:
        progress = quota_day_progress(datetime(2026, 8, 24, 0, 0, tzinfo=SH))
        assert progress == pytest.approx(0.5)

    def test_clamped_to_unit_interval(self) -> None:
        for hour in range(24):
            value = quota_day_progress(datetime(2026, 8, 23, hour, tzinfo=SH))
            assert 0.0 <= value <= 1.0
