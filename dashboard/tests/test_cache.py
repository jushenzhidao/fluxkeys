"""Redis 访问层的单元测试。

Key 命名规则必须与 Go 侧 ``internal/quota/manager.go`` 完全一致，否则看板会
读到空热态却不报错 —— 这是最难发现的一类问题，所以单独测。
"""

from __future__ import annotations

from dataclasses import replace
from datetime import date, datetime
from zoneinfo import ZoneInfo

import pytest

from app.cache import (
    KIND_COUNT,
    KIND_TOKEN,
    Cache,
    QuotaSnapshot,
    lease_data_key,
    lease_zset,
    quota_key,
)
from app.config import Settings

SH = ZoneInfo("Asia/Shanghai")


class TestKeyNaming:
    def test_quota_key_matches_go_format(self) -> None:
        """对应 Go: fmt.Sprintf("volc:quota:%s:%s:%s", kind, keyID, day)"""
        assert (
            quota_key(KIND_TOKEN, "volc_001", date(2026, 8, 23))
            == "volc:quota:token:volc_001:20260823"
        )

    def test_quota_key_count_kind(self) -> None:
        assert (
            quota_key(KIND_COUNT, "volc_009", date(2026, 1, 1))
            == "volc:quota:count:volc_009:20260101"
        )

    def test_lease_zset_matches_go_format(self) -> None:
        """对应 Go: "volc:lease:" + day"""
        assert lease_zset(date(2026, 12, 31)) == "volc:lease:20261231"

    def test_lease_data_key_matches_go_format(self) -> None:
        assert lease_data_key("abc123") == "volc:lease:data:abc123"


class TestQuotaSnapshot:
    def _settings_default(self) -> int:
        return 4_500_000

    def test_parses_full_hash(self) -> None:
        snap = QuotaSnapshot(
            "k1",
            KIND_TOKEN,
            {
                "used": "1000000",
                "prededuct": "200000",
                "hard_limit": "4500000",
                "soft_limit": "4000000",
                "last_updated": "1756000000",
            },
            self._settings_default(),
        )
        assert snap.used == 1_000_000
        assert snap.prededuct == 200_000
        assert snap.remaining == 3_300_000
        assert snap.ratio == pytest.approx(1_200_000 / 4_500_000)
        assert snap.hot is True
        assert isinstance(snap.last_updated, datetime)

    def test_missing_hash_yields_cold_snapshot(self) -> None:
        """Redis 无该 Key 时返回空快照，hot=False，不抛异常。"""
        snap = QuotaSnapshot("k1", KIND_TOKEN, None, self._settings_default())
        assert snap.hot is False
        assert snap.used == 0
        assert snap.prededuct == 0
        # hard_limit 用配置兜底，避免 ratio 恒为 0 被误读成「水位很低」
        assert snap.hard == 4_500_000
        assert snap.ratio == 0.0
        assert snap.remaining == 4_500_000
        assert snap.last_updated is None

    def test_garbage_values_default_to_zero(self) -> None:
        snap = QuotaSnapshot(
            "k1", KIND_TOKEN, {"used": "abc", "prededuct": ""}, self._settings_default()
        )
        assert snap.used == 0
        assert snap.prededuct == 0

    def test_remaining_never_negative(self) -> None:
        """超刷场景下剩余量必须钉在 0，不能是负数。"""
        snap = QuotaSnapshot(
            "k1",
            KIND_TOKEN,
            {"used": "5000000", "prededuct": "100000", "hard_limit": "4500000"},
            self._settings_default(),
        )
        assert snap.remaining == 0
        assert snap.ratio > 1.0

    def test_zero_hard_limit_treated_as_full(self) -> None:
        """hard_limit 为 0 时视为已满而不是除零。"""
        snap = QuotaSnapshot("k1", KIND_TOKEN, {"used": "0", "hard_limit": "0"}, 0)
        assert snap.ratio == 1.0
        assert snap.remaining == 0

    def test_negative_prededuct_is_parsed(self) -> None:
        """prededuct 为负说明网关侧有 bug，看板要如实反映而非吞掉。"""
        snap = QuotaSnapshot("k1", KIND_TOKEN, {"prededuct": "-500"}, self._settings_default())
        assert snap.prededuct == -500

    def test_as_dict_keys_match_model(self) -> None:
        snap = QuotaSnapshot("k1", KIND_TOKEN, {}, self._settings_default())
        data = snap.as_dict()
        assert set(data) == {
            "kind",
            "used",
            "prededuct",
            "hard_limit",
            "soft_limit",
            "remaining",
            "ratio",
            "last_updated",
            "hot",
        }


class TestCacheDegradation:
    """Redis 未连接时的降级行为：返回空数据而非抛异常。"""

    @pytest.fixture
    def offline_cache(self, settings: Settings) -> Cache:
        # 不调用 connect()，模拟 Redis 完全不可用
        return Cache(settings)

    async def test_snapshot_without_client(self, offline_cache: Cache) -> None:
        snap = await offline_cache.snapshot("k1", date(2026, 8, 23))
        assert snap.hot is False
        assert snap.used == 0

    async def test_snapshots_returns_entry_per_key(self, offline_cache: Cache) -> None:
        result = await offline_cache.snapshots(["k1", "k2"], date(2026, 8, 23))
        assert set(result) == {"k1", "k2"}
        assert all(not s.hot for s in result.values())

    async def test_snapshots_empty_input(self, offline_cache: Cache) -> None:
        assert await offline_cache.snapshots([], date(2026, 8, 23)) == {}

    async def test_lease_aggregate_without_client(self, offline_cache: Cache) -> None:
        agg = await offline_cache.lease_aggregate(date(2026, 8, 23), 1756000000)
        assert agg.total == 0
        assert agg.expired == 0
        assert agg.amount_of("k1") == 0
        assert agg.count_of("k1") == 0

    async def test_ping_fails_gracefully(self, settings: Settings) -> None:
        """指向不存在的端口时 ping 返回 False 而不抛异常。"""
        broken = replace(settings, redis_addr="127.0.0.1:1")
        cache = Cache(broken)
        await cache.connect()
        assert await cache.ping() is False
        assert cache.error
        await cache.close()
