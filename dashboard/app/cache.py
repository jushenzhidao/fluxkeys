"""Redis 只读访问层。

Key 命名与 Go 侧 ``internal/quota/manager.go`` 严格对应：

- 配额热态：``{provider}:quota:{kind}:{key_id}:{quota_day}``（Hash）
  字段 ``used`` / ``prededuct`` / ``hard_limit`` / ``soft_limit`` / ``last_updated``
- 租约集合：``{provider}:lease:{quota_day}``（ZSET，score = 到期时间戳）
- 租约明细：``{provider}:lease:data:{lease_id}``（Hash，字段 key_id / kind / amount）

其中 ``{quota_day}`` 是配额日的 ``yyyyMMdd``，**不是自然日**；``{provider}``
默认 ``volc`` —— 看板当前只展示火山池，但前缀必须走同一个常量而不是散落
硬编码，Go 侧曾因 Lua 里写死 'volc:' 导致其他 provider 的租约回收静默失效。

本模块只读：仅使用 HGETALL / ZRANGEBYSCORE / ZCARD 等读命令，不含任何写操作。
"""

from __future__ import annotations

from collections.abc import Awaitable, Iterable
from datetime import UTC, date, datetime
from typing import Any, cast
from zoneinfo import ZoneInfo

import redis.asyncio as aioredis

from .config import Settings, redis_url
from .quotaday import day_to_key

# 与 Go 侧 quota.Kind 对应
KIND_TOKEN = "token"
KIND_COUNT = "count"

# 看板当前展示的上游。多 provider 展示时把它升级为查询参数即可，
# 所有 key 构造函数都已接收 provider 形参。
DEFAULT_PROVIDER = "volc"


def quota_key(kind: str, key_id: str, day: date, provider: str = DEFAULT_PROVIDER) -> str:
    """拼出配额热态的 Redis Key。"""
    return f"{provider}:quota:{kind}:{key_id}:{day_to_key(day)}"


def lease_zset(day: date, provider: str = DEFAULT_PROVIDER) -> str:
    """拼出租约 ZSET 的 Redis Key。"""
    return f"{provider}:lease:{day_to_key(day)}"


def lease_data_key(lease_id: str, provider: str = DEFAULT_PROVIDER) -> str:
    return f"{provider}:lease:data:{lease_id}"


def _to_int(value: Any) -> int:
    """宽松地把 Redis 字段解析为整数。字段缺失或非法时返回 0。"""
    if value is None:
        return 0
    try:
        return int(str(value).strip())
    except (TypeError, ValueError):
        return 0


class QuotaSnapshot:
    """单个 Key 单类配额的只读快照。"""

    __slots__ = (
        "hard",
        "hot",
        "key_id",
        "kind",
        "last_updated",
        "prededuct",
        "soft",
        "used",
    )

    def __init__(
        self,
        key_id: str,
        kind: str,
        raw: dict[str, str] | None,
        hard_default: int,
        tz: ZoneInfo | None = None,
    ) -> None:
        self.key_id = key_id
        self.kind = kind
        self.hot = bool(raw)
        raw = raw or {}
        self.used = _to_int(raw.get("used"))
        self.prededuct = _to_int(raw.get("prededuct"))
        # Redis 未写入 hard_limit 时用配置兜底，避免 ratio 全为 0 误判「水位很低」。
        self.hard = _to_int(raw.get("hard_limit")) or hard_default
        self.soft = _to_int(raw.get("soft_limit"))
        ts = _to_int(raw.get("last_updated"))
        # 必须显式带时区：last_updated 是 Unix 时间戳，若按机器本地时区解释，
        # 容器内（UTC）与看板展示时区（Asia/Shanghai）会相差 8 小时。
        self.last_updated: datetime | None = (
            datetime.fromtimestamp(ts, tz=tz or UTC) if ts > 0 else None
        )

    @property
    def remaining(self) -> int:
        """相对硬水位的剩余可用量，下限 0。"""
        return max(0, self.hard - self.used - self.prededuct)

    @property
    def ratio(self) -> float:
        """已消耗比例（含预扣）。硬水位非正时视为已满，避免除零。"""
        if self.hard <= 0:
            return 1.0
        return (self.used + self.prededuct) / self.hard

    def as_dict(self) -> dict[str, Any]:
        return {
            "kind": self.kind,
            "used": self.used,
            "prededuct": self.prededuct,
            "hard_limit": self.hard,
            "soft_limit": self.soft,
            "remaining": self.remaining,
            "ratio": round(self.ratio, 6),
            "last_updated": self.last_updated,
            "hot": self.hot,
        }


class LeaseAggregate:
    """某配额日的租约汇总结果。"""

    __slots__ = ("by_key_amount", "by_key_count", "expired", "total")

    def __init__(self) -> None:
        self.total = 0
        self.expired = 0
        self.by_key_amount: dict[tuple[str, str], int] = {}
        self.by_key_count: dict[tuple[str, str], int] = {}

    def amount_of(self, key_id: str, kind: str = KIND_TOKEN) -> int:
        return self.by_key_amount.get((key_id, kind), 0)

    def count_of(self, key_id: str, kind: str = KIND_TOKEN) -> int:
        return self.by_key_count.get((key_id, kind), 0)


class Cache:
    """redis-py 异步客户端的薄封装。"""

    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._client: aioredis.Redis | None = None
        self._error: str = ""

    # ------------------------------------------------------------ 生命周期

    async def connect(self) -> None:
        """创建客户端。redis-py 是懒连接，这里只构造对象。"""
        try:
            self._client = aioredis.from_url(
                redis_url(self._settings),
                decode_responses=True,
                socket_timeout=5.0,
                socket_connect_timeout=3.0,
                max_connections=16,
            )
            self._error = ""
        except Exception as exc:
            self._client = None
            self._error = str(exc)

    async def close(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    @property
    def available(self) -> bool:
        return self._client is not None

    @property
    def error(self) -> str:
        return self._error

    async def ping(self) -> bool:
        if self._client is None:
            await self.connect()
        if self._client is None:
            return False
        try:
            await self._client.ping()
            self._error = ""
            return True
        except Exception as exc:
            self._error = str(exc)
            return False

    # ------------------------------------------------------------ 配额快照

    async def snapshot(self, key_id: str, day: date, kind: str = KIND_TOKEN) -> QuotaSnapshot:
        """读取单个 Key 的配额快照。Redis 不可用时返回空快照（hot=False）。"""
        raw: dict[str, str] | None = None
        if self._client is not None:
            try:
                # redis-py 的命令签名是同步/异步联合类型，异步客户端下实际返回
                # 可等待对象，mypy 无法自行收窄，故显式 cast。
                raw = await cast(
                    "Awaitable[dict[str, str]]",
                    self._client.hgetall(quota_key(kind, key_id, day)),
                )
            except Exception as exc:
                self._error = str(exc)
        return QuotaSnapshot(key_id, kind, raw, self._hard_default(kind), self._settings.tzinfo)

    async def snapshots(
        self, key_ids: Iterable[str], day: date, kind: str = KIND_TOKEN
    ) -> dict[str, QuotaSnapshot]:
        """批量读取配额快照，单次 pipeline 完成。

        Redis 不可用或部分 Key 无热态时，仍为每个 Key 返回一个空快照，
        保证上层可以无分支地渲染表格。
        """
        ids = list(key_ids)
        if not ids:
            return {}

        raws: list[dict[str, str] | None] = [None] * len(ids)
        if self._client is not None:
            try:
                async with self._client.pipeline(transaction=False) as pipe:
                    for key_id in ids:
                        pipe.hgetall(quota_key(kind, key_id, day))
                    results = await pipe.execute()
                for idx, item in enumerate(results):
                    if isinstance(item, dict):
                        raws[idx] = item
            except Exception as exc:
                self._error = str(exc)

        hard_default = self._hard_default(kind)
        tz = self._settings.tzinfo
        return {
            key_id: QuotaSnapshot(key_id, kind, raws[idx], hard_default, tz)
            for idx, key_id in enumerate(ids)
        }

    # ------------------------------------------------------------ 租约

    async def lease_aggregate(self, day: date, now_ts: int, limit: int = 5000) -> LeaseAggregate:
        """汇总某配额日的租约，按 (key_id, kind) 累加未过期租约的预扣量。

        这是判断 prededuct 是否泄漏的依据：正常情况下
        ``prededuct == Σ 未过期 lease.amount``，差额即泄漏量。
        """
        agg = LeaseAggregate()
        if self._client is None:
            return agg

        zset = lease_zset(day)
        try:
            agg.total = int(await self._client.zcard(zset))
            # score < now 的租约已过期但尚未被网关回收器处理，单独计数用于告警。
            agg.expired = int(await self._client.zcount(zset, "-inf", f"({now_ts}"))
            lease_ids = await self._client.zrangebyscore(zset, now_ts, "+inf", start=0, num=limit)
        except Exception as exc:
            self._error = str(exc)
            return agg

        if not lease_ids:
            return agg

        try:
            async with self._client.pipeline(transaction=False) as pipe:
                for lease_id in lease_ids:
                    pipe.hgetall(lease_data_key(str(lease_id)))
                rows = await pipe.execute()
        except Exception as exc:
            self._error = str(exc)
            return agg

        for row in rows:
            if not isinstance(row, dict) or not row:
                continue
            key_id = str(row.get("key_id", ""))
            if not key_id:
                continue
            kind = str(row.get("kind", KIND_TOKEN)) or KIND_TOKEN
            bucket = (key_id, kind)
            agg.by_key_amount[bucket] = agg.by_key_amount.get(bucket, 0) + _to_int(
                row.get("amount")
            )
            agg.by_key_count[bucket] = agg.by_key_count.get(bucket, 0) + 1
        return agg

    # ------------------------------------------------------------ 内部

    def _hard_default(self, kind: str) -> int:
        if kind == KIND_COUNT:
            return self._settings.count_hard_default
        return self._settings.token_hard_default
