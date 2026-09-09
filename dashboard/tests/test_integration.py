"""集成测试：连真实 Postgres / Redis。

无依赖时通过 ``live_db`` / ``live_cache`` 夹具 **skip 而非失败**。
这些测试只做只读校验，不写入任何业务数据（Redis 用独立 DB 15，且只读）。
"""

from __future__ import annotations

from collections.abc import Awaitable
from datetime import datetime
from typing import Any, cast

import pytest

from app.cache import Cache
from app.db import Database
from app.service import ReportService

pytestmark = pytest.mark.integration


def _aw(value: Any) -> Awaitable[Any]:
    """把 redis-py 命令的返回值标注为可等待对象。

    redis-py 的命令签名是同步/异步联合类型，异步客户端下实际返回可等待对象，
    但 mypy 无法自行收窄，直接 await 会报 misc 错误。
    """
    return cast("Awaitable[Any]", value)


class TestPostgresReadOnly:
    async def test_ping(self, live_db: Database) -> None:
        assert await live_db.ping() is True

    async def test_session_is_read_only(self, live_db: Database) -> None:
        """连接必须是只读会话：看板不该有能力改动网关数据。"""
        value = await live_db.fetchval("SHOW transaction_read_only")
        assert value == "on"

    async def test_writes_are_actually_rejected(self, live_db: Database) -> None:
        """只读不是靠一个标志位自证，要真的写一次看它被拒。

        断言 ``SHOW transaction_read_only`` 只证明参数设上了，不证明写操作
        真的走不通。这条直接尝试三类写入 —— 这是「写路径唯一化」的底线：
        看板的所有写操作只能 HTTP 转发给网关，自身永不直接写库。

        它同时是一条防退化测试：若哪天有人为了图方便去掉
        ``default_transaction_read_only``，这条会立刻变红。
        """
        import asyncpg

        pool = live_db._require_pool()
        async with pool.acquire() as conn:
            for sql in (
                "CREATE TABLE dashboard_should_not_write (id int)",
                "INSERT INTO upstream_keys (key_id, secret_enc) VALUES ('__probe__', 'x')",
                "UPDATE upstream_keys SET pool = 'hot' WHERE key_id = '__probe__'",
            ):
                with pytest.raises(asyncpg.PostgresError) as excinfo:
                    await conn.execute(sql)
                # 必须是只读事务拒绝，而不是「表不存在」之类的偶然失败 ——
                # 后者会让这条测试在 schema 变化后变成假绿。
                assert isinstance(excinfo.value, asyncpg.ReadOnlySQLTransactionError), (
                    f"SQL 未被只读约束拒绝，而是 {type(excinfo.value).__name__}: {sql}"
                )

    async def test_read_only_cannot_be_disabled_in_session(self, live_db: Database) -> None:
        """连接内改不回可写：只读由服务端强制，不是客户端自律。"""
        import asyncpg

        pool = live_db._require_pool()
        async with pool.acquire() as conn:
            with pytest.raises(asyncpg.PostgresError):
                await conn.execute("SET transaction_read_only = off")
                await conn.execute("CREATE TABLE dashboard_bypass_probe (id int)")

    async def test_missing_table_returns_empty_not_error(self, live_db: Database) -> None:
        """表不存在时按「无数据」处理，看板不负责建表。"""
        rows = await live_db.fetch("SELECT 1 FROM table_that_should_not_exist")
        assert rows == []


class TestRedisReadOnly:
    async def test_ping(self, live_cache: Cache) -> None:
        assert await live_cache.ping() is True

    async def test_snapshot_of_unknown_key(self, live_cache: Cache, fixed_now: datetime) -> None:
        from app.quotaday import quota_day

        snap = await live_cache.snapshot("__nonexistent_key__", quota_day(fixed_now))
        assert snap.hot is False
        assert snap.used == 0

    async def test_snapshots_batch_on_real_redis(
        self, live_cache: Cache, fixed_now: datetime
    ) -> None:
        """批量读取要为每个 Key 都返回条目，缺失的标记 hot=False。"""
        from app.quotaday import quota_day

        result = await live_cache.snapshots(
            ["__missing_a__", "__missing_b__"], quota_day(fixed_now)
        )
        assert set(result) == {"__missing_a__", "__missing_b__"}
        assert all(not s.hot for s in result.values())

    async def test_lease_aggregate_parses_real_zset(
        self, live_cache: Cache, fixed_now: datetime
    ) -> None:
        """租约聚合要能在真实 Redis 上正确解析 ZSET + Hash。

        解析逻辑只有实际写入才能验证，因此用一个绝不会出现在生产的配额日
        （1999-01-01）做隔离，测试结束后清理，不触碰任何真实 Key。
        """
        from datetime import date

        from app.cache import lease_data_key, lease_zset

        day = date(1999, 1, 1)
        now_ts = int(fixed_now.timestamp())
        client = live_cache._client
        assert client is not None

        zset = lease_zset(day)
        keys = [zset, *(lease_data_key(x) for x in ("t_a", "t_b", "t_expired"))]
        try:
            # 两条未过期 + 一条已过期
            await _aw(
                client.zadd(
                    zset,
                    {"t_a": now_ts + 600, "t_b": now_ts + 600, "t_expired": now_ts - 600},
                )
            )
            for lease_id, amount in (("t_a", "1000"), ("t_b", "500"), ("t_expired", "9999")):
                await _aw(
                    client.hset(
                        lease_data_key(lease_id),
                        mapping={"key_id": "k1", "kind": "token", "amount": amount},
                    )
                )

            agg = await live_cache.lease_aggregate(day, now_ts)
            assert agg.total == 3
            assert agg.expired == 1
            # 已过期租约不计入求和，否则会把已回收的预扣误判成泄漏
            assert agg.amount_of("k1", "token") == 1500
            assert agg.count_of("k1", "token") == 2
            assert agg.amount_of("unknown") == 0
        finally:
            await _aw(client.delete(*keys))


class TestServiceAgainstLiveStores:
    """真实数据源上跑一遍全部报表，确保 SQL 在 Postgres 上语法正确。

    这是 test_api.py 的假数据库无法覆盖的部分：那里只验证组装逻辑，
    这里验证 SQL 本身能被 Postgres 接受。
    """

    async def test_overview(self, live_service: ReportService) -> None:
        resp = await live_service.overview()
        assert resp.total_tokens >= 0
        assert 0.0 <= resp.error_rate <= 1.0
        assert 0.0 <= resp.quota_day_progress <= 1.0

    async def test_list_keys_all_sorts(self, live_service: ReportService) -> None:
        for sort in ["key_id", "health", "pool", "status", "last_used", "today_tokens"]:
            resp = await live_service.list_keys(sort=sort, desc=True, limit=10)
            assert resp.total >= 0

    async def test_list_keys_hot_sorts(self, live_service: ReportService) -> None:
        for sort in ["ratio", "remaining", "used"]:
            resp = await live_service.list_keys(sort=sort, desc=True, limit=10)
            assert len(resp.items) <= 10

    async def test_list_keys_with_filters(self, live_service: ReportService) -> None:
        resp = await live_service.list_keys(status="active", pool="hot", limit=5)
        assert resp.total >= 0

    async def test_usage_trend(self, live_service: ReportService) -> None:
        resp = await live_service.usage_trend(7)
        assert len(resp.points) == 7

    async def test_usage_by_user(self, live_service: ReportService) -> None:
        resp = await live_service.usage_by_user(30, 50)
        assert resp.start_day <= resp.end_day

    async def test_errors(self, live_service: ReportService) -> None:
        resp = await live_service.errors(24)
        assert 0.0 <= resp.error_rate <= 1.0

    async def test_egress(self, live_service: ReportService) -> None:
        resp = await live_service.egress()
        assert resp.total == len(resp.items)

    async def test_quota_health(self, live_service: ReportService) -> None:
        resp = await live_service.quota_health()
        assert resp.checked_keys >= 0

    async def test_refresh_status(self, live_service: ReportService) -> None:
        resp = await live_service.refresh_status()
        assert resp.window_start < resp.window_end

    async def test_similarity(self, live_service: ReportService) -> None:
        resp = await live_service.similarity(7)
        assert 0.0 <= resp.max_similarity <= 1.0
        assert resp.compared_pairs >= 0

    async def test_key_detail_of_first_key(self, live_service: ReportService) -> None:
        listing = await live_service.list_keys(limit=1)
        if not listing.items:
            pytest.skip("upstream_keys 表为空，无法测试 Key 详情")
        detail = await live_service.key_detail(listing.items[0].key_id, 7)
        assert detail is not None
        assert len(detail.trend) == 7

    async def test_key_detail_missing_returns_none(self, live_service: ReportService) -> None:
        assert await live_service.key_detail("__nope__", 7) is None
