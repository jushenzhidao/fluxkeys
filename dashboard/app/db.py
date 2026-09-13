"""Postgres 只读访问层。

约定：
- 所有 SQL 一律使用 ``$1`` 占位符参数化，绝不做字符串拼接（防注入）。
- 排序字段这类无法参数化的部分，只允许来自白名单映射，不接受用户原样输入。
- 连接池全局复用，由 FastAPI 生命周期管理。
- 池不可用时抛 :class:`StoreUnavailable`，由上层转成 503 而非 500。
"""

from __future__ import annotations

from collections.abc import Sequence
from datetime import date, datetime
from typing import Any, Final

import asyncpg

from .config import Settings, asyncpg_dsn


class StoreUnavailable(RuntimeError):
    """数据库不可用。上层据此返回 503 并给出可读提示。"""


# Key 列表允许的排序字段白名单。键是对外参数，值是 SQL 片段。
# 绝不能把外部字符串直接拼进 ORDER BY。
KEY_SORT_FIELDS: Final[dict[str, str]] = {
    "key_id": "k.key_id",
    "health": "k.health_score",
    "pool": "k.pool",
    "status": "k.status",
    "last_used": "k.last_used_at",
    "today_tokens": "today_tokens",
}


# 「一次错误」的唯一定义。所有报表（总览/趋势/按 Key/按用户/按出口）都必须
# 引用本片段，禁止再手写谓词 —— 否则各处错误率口径会各自漂移，
# 而「总数对得上、分项加不上」这类不一致极难排查。
#
# 口径：HTTP 4xx/5xx（status_code >= 400）或携带非空 error_code。
# usage_records.status_code / error_code 均为 NOT NULL
# （schema.sql 中 DEFAULT 0 / DEFAULT ''），因此不存在 SQL 三值逻辑把 NULL
# 判成「非错误」的情形，无需再加 IS NOT NULL 兜底。
_ERROR_CONDITION: Final[str] = "({alias}status_code >= 400 OR {alias}error_code <> '')"


def error_condition(alias: str = "") -> str:
    """返回「该行算一次错误」的 SQL 片段，已带外层括号。

    ``alias`` 是表别名前缀（含结尾的点，如 ``"r."``）；单表查询传空串。
    可直接嵌入 ``FILTER (WHERE ...)`` / ``WHERE`` / ``HAVING``。
    """
    return _ERROR_CONDITION.format(alias=alias)


class Database:
    """asyncpg 连接池的薄封装。"""

    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._pool: asyncpg.Pool | None = None
        self._error: str = ""

    # ------------------------------------------------------------ 生命周期

    async def connect(self) -> None:
        """建立连接池。失败不抛出，记录错误让 /healthz 可读。

        看板是旁路组件，Postgres 暂时不可用时也应能启动并回报状态，
        而不是让容器反复重启。
        """
        try:
            self._pool = await asyncpg.create_pool(
                dsn=asyncpg_dsn(self._settings.postgres_dsn),
                min_size=self._settings.pg_min_size,
                max_size=self._settings.pg_max_size,
                command_timeout=15.0,
                # 只读会话：即便 SQL 写错也无法改动网关数据。
                server_settings={
                    "application_name": "fluxkeys-dashboard",
                    "default_transaction_read_only": "on",
                },
            )
            self._error = ""
        except Exception as exc:
            self._pool = None
            self._error = str(exc)

    async def close(self) -> None:
        if self._pool is not None:
            await self._pool.close()
            self._pool = None

    @property
    def available(self) -> bool:
        return self._pool is not None

    @property
    def error(self) -> str:
        return self._error

    async def ping(self) -> bool:
        """探活。顺带在启动期失败后做一次懒重连。"""
        if self._pool is None:
            await self.connect()
        if self._pool is None:
            return False
        try:
            await self._pool.fetchval("SELECT 1")
            return True
        except Exception as exc:
            self._error = str(exc)
            return False

    # ------------------------------------------------------------ 查询

    async def fetch(self, sql: str, *args: Any) -> list[asyncpg.Record]:
        """执行查询并返回全部行。表不存在时返回空列表。"""
        pool = self._require_pool()
        try:
            return list(await pool.fetch(sql, *args))
        except asyncpg.UndefinedTableError:
            # schema 尚未迁移。看板不负责建表，按「无数据」处理。
            return []
        except Exception as exc:
            raise StoreUnavailable(f"查询失败: {exc}") from exc

    async def fetchrow(self, sql: str, *args: Any) -> asyncpg.Record | None:
        rows = await self.fetch(sql, *args)
        return rows[0] if rows else None

    async def fetchval(self, sql: str, *args: Any, default: Any = None) -> Any:
        row = await self.fetchrow(sql, *args)
        if row is None:
            return default
        value = row[0]
        return default if value is None else value

    def _require_pool(self) -> asyncpg.Pool:
        if self._pool is None:
            raise StoreUnavailable(f"Postgres 未连接: {self._error or '尚未初始化'}")
        return self._pool


# ================================================================ 查询函数
# 说明：这些函数只拼接白名单片段，全部业务参数都走占位符。


async def overview_usage(db: Database, day: date) -> asyncpg.Record | None:
    """某配额日的全局用量汇总。"""
    return await db.fetchrow(
        f"""
        SELECT
            COALESCE(SUM(total_tokens), 0)::bigint       AS total_tokens,
            COALESCE(SUM(prompt_tokens), 0)::bigint      AS prompt_tokens,
            COALESCE(SUM(completion_tokens), 0)::bigint   AS completion_tokens,
            COALESCE(SUM(count_units), 0)::bigint        AS count_units,
            COUNT(*)::bigint                              AS requests,
            COUNT(*) FILTER (
                WHERE {error_condition()}
            )::bigint                                     AS errors,
            COALESCE(AVG(latency_ms), 0)::double precision AS avg_latency_ms,
            COUNT(DISTINCT upstream_key_id) FILTER (
                WHERE upstream_key_id <> ''
            )::bigint                                     AS used_keys
        FROM usage_records
        WHERE quota_day = $1
        """,
        day,
    )


async def key_pool_distribution(db: Database) -> list[asyncpg.Record]:
    return await db.fetch(
        "SELECT pool AS name, COUNT(*)::bigint AS count FROM upstream_keys "
        "GROUP BY pool ORDER BY count DESC"
    )


async def key_status_distribution(db: Database) -> list[asyncpg.Record]:
    return await db.fetch(
        "SELECT status AS name, COUNT(*)::bigint AS count FROM upstream_keys "
        "GROUP BY status ORDER BY count DESC"
    )


async def key_counts(db: Database) -> asyncpg.Record | None:
    return await db.fetchrow(
        """
        SELECT
            COUNT(*)::bigint AS total,
            COUNT(*) FILTER (WHERE status = 'active')::bigint AS active,
            COUNT(*) FILTER (
                WHERE refresh_state <> 'confirmed'
            )::bigint AS unconfirmed_refresh
        FROM upstream_keys
        """
    )


async def list_keys(
    db: Database,
    *,
    day: date,
    status: str | None,
    pool: str | None,
    sort: str,
    desc: bool,
    limit: int,
    offset: int,
) -> list[asyncpg.Record]:
    """Key 列表。

    ``sort`` 必须是 :data:`KEY_SORT_FIELDS` 的键，调用方需先校验。
    水位相关排序（ratio/remaining）依赖 Redis 热态，在 Python 侧完成。
    """
    column = KEY_SORT_FIELDS.get(sort, KEY_SORT_FIELDS["key_id"])
    direction = "DESC" if desc else "ASC"
    # column/direction 均来自白名单常量，不含外部输入。
    sql = f"""
        WITH today AS (
            SELECT upstream_key_id,
                   COALESCE(SUM(total_tokens), 0)::bigint AS today_tokens,
                   COUNT(*)::bigint                        AS today_requests,
                   COUNT(*) FILTER (
                       WHERE {error_condition()}
                   )::bigint                               AS today_errors
            FROM usage_records
            WHERE quota_day = $1
            GROUP BY upstream_key_id
        )
        SELECT k.key_id, k.pool, k.status, k.persona_id, k.egress_ip,
               k.health_score, k.refresh_state, k.refresh_confirmed_at,
               k.last_error, k.last_used_at,
               COALESCE(t.today_tokens, 0)   AS today_tokens,
               COALESCE(t.today_requests, 0) AS today_requests,
               COALESCE(t.today_errors, 0)   AS today_errors
        FROM upstream_keys k
        LEFT JOIN today t ON t.upstream_key_id = k.key_id
        WHERE ($2::text IS NULL OR k.status = $2)
          AND ($3::text IS NULL OR k.pool = $3)
        ORDER BY {column} {direction} NULLS LAST, k.key_id ASC
        LIMIT $4 OFFSET $5
    """
    return await db.fetch(sql, day, status, pool, limit, offset)


async def count_keys(db: Database, status: str | None, pool: str | None) -> int:
    return int(
        await db.fetchval(
            "SELECT COUNT(*)::bigint FROM upstream_keys "
            "WHERE ($1::text IS NULL OR status = $1) "
            "AND ($2::text IS NULL OR pool = $2)",
            status,
            pool,
            default=0,
        )
    )


async def get_key(db: Database, key_id: str, day: date) -> asyncpg.Record | None:
    return await db.fetchrow(
        f"""
        WITH today AS (
            SELECT COALESCE(SUM(total_tokens), 0)::bigint AS today_tokens,
                   COUNT(*)::bigint                        AS today_requests,
                   COUNT(*) FILTER (
                       WHERE {error_condition()}
                   )::bigint                               AS today_errors
            FROM usage_records
            WHERE quota_day = $2 AND upstream_key_id = $1
        )
        SELECT k.key_id, k.pool, k.status, k.persona_id, k.egress_ip,
               k.health_score, k.refresh_state, k.refresh_confirmed_at,
               k.last_error, k.last_used_at,
               t.today_tokens, t.today_requests, t.today_errors
        FROM upstream_keys k CROSS JOIN today t
        WHERE k.key_id = $1
        """,
        key_id,
        day,
    )


async def key_trend(db: Database, key_id: str, days: Sequence[date]) -> list[asyncpg.Record]:
    """单 Key 的按配额日用量趋势。``days`` 为空时返回空。"""
    if not days:
        return []
    return await db.fetch(
        f"""
        SELECT quota_day,
               COALESCE(SUM(total_tokens), 0)::bigint      AS total_tokens,
               COALESCE(SUM(prompt_tokens), 0)::bigint     AS prompt_tokens,
               COALESCE(SUM(completion_tokens), 0)::bigint AS completion_tokens,
               COALESCE(SUM(count_units), 0)::bigint       AS count_units,
               COUNT(*)::bigint                             AS requests,
               COUNT(*) FILTER (
                   WHERE {error_condition()}
               )::bigint                                    AS errors,
               COALESCE(AVG(latency_ms), 0)::double precision AS avg_latency_ms
        FROM usage_records
        WHERE upstream_key_id = $1 AND quota_day = ANY($2::date[])
        GROUP BY quota_day
        ORDER BY quota_day
        """,
        key_id,
        list(days),
    )


async def key_model_usage(
    db: Database, key_id: str, days: Sequence[date], limit: int = 10
) -> list[asyncpg.Record]:
    if not days:
        return []
    return await db.fetch(
        """
        SELECT model,
               COUNT(*)::bigint                        AS requests,
               COALESCE(SUM(total_tokens), 0)::bigint  AS total_tokens
        FROM usage_records
        WHERE upstream_key_id = $1 AND quota_day = ANY($2::date[])
        GROUP BY model
        ORDER BY requests DESC
        LIMIT $3
        """,
        key_id,
        list(days),
        limit,
    )


async def key_error_buckets(
    db: Database, key_id: str, days: Sequence[date], limit: int = 10
) -> list[asyncpg.Record]:
    if not days:
        return []
    return await db.fetch(
        f"""
        SELECT COALESCE(NULLIF(error_code, ''), status_code::text) AS label,
               COUNT(*)::bigint AS count
        FROM usage_records
        WHERE upstream_key_id = $1 AND quota_day = ANY($2::date[])
          AND {error_condition()}
        GROUP BY label
        ORDER BY count DESC
        LIMIT $3
        """,
        key_id,
        list(days),
        limit,
    )


async def usage_trend(db: Database, days: Sequence[date]) -> list[asyncpg.Record]:
    """全局按配额日聚合的用量趋势。"""
    if not days:
        return []
    return await db.fetch(
        f"""
        SELECT quota_day,
               COALESCE(SUM(total_tokens), 0)::bigint      AS total_tokens,
               COALESCE(SUM(prompt_tokens), 0)::bigint     AS prompt_tokens,
               COALESCE(SUM(completion_tokens), 0)::bigint AS completion_tokens,
               COALESCE(SUM(count_units), 0)::bigint       AS count_units,
               COUNT(*)::bigint                             AS requests,
               COUNT(*) FILTER (
                   WHERE {error_condition()}
               )::bigint                                    AS errors,
               COUNT(DISTINCT upstream_key_id) FILTER (
                   WHERE upstream_key_id <> ''
               )::bigint                                    AS active_keys,
               COALESCE(AVG(latency_ms), 0)::double precision AS avg_latency_ms
        FROM usage_records
        WHERE quota_day = ANY($1::date[])
        GROUP BY quota_day
        ORDER BY quota_day
        """,
        list(days),
    )


async def usage_by_user(
    db: Database, start_day: date, end_day: date, limit: int
) -> list[asyncpg.Record]:
    """按用户计费口径聚合用量（P1-9）。

    用 LEFT JOIN users：``user_id`` 可能为 NULL（未鉴权或用户已删除），
    这些流量归到「未归属」而不是被丢掉——计费对账必须能看到全量。
    """
    return await db.fetch(
        f"""
        SELECT u.id                                        AS user_id,
               COALESCE(u.name, '(未归属)')                AS user_name,
               u.email,
               COALESCE(u.status, '')                      AS status,
               COALESCE(u.daily_token_limit, 0)::bigint    AS daily_token_limit,
               COALESCE(SUM(r.total_tokens), 0)::bigint      AS total_tokens,
               COALESCE(SUM(r.prompt_tokens), 0)::bigint     AS prompt_tokens,
               COALESCE(SUM(r.completion_tokens), 0)::bigint AS completion_tokens,
               COALESCE(SUM(r.count_units), 0)::bigint       AS count_units,
               COUNT(*)::bigint                              AS requests,
               COUNT(*) FILTER (
                   WHERE {error_condition("r.")}
               )::bigint                                     AS errors,
               COALESCE(AVG(r.latency_ms), 0)::double precision AS avg_latency_ms,
               COUNT(DISTINCT r.quota_day)::bigint           AS active_days,
               MIN(r.quota_day)                              AS first_day,
               MAX(r.quota_day)                              AS last_day
        FROM usage_records r
        LEFT JOIN users u ON u.id = r.user_id
        WHERE r.quota_day BETWEEN $1 AND $2
        GROUP BY u.id, u.name, u.email, u.status, u.daily_token_limit
        ORDER BY total_tokens DESC
        LIMIT $3
        """,
        start_day,
        end_day,
        limit,
    )


async def usage_by_user_daily(
    db: Database, start_day: date, end_day: date, user_ids: Sequence[int]
) -> list[asyncpg.Record]:
    """给定用户集合的逐配额日用量。``user_ids`` 为空则只查未归属流量。"""
    return await db.fetch(
        f"""
        SELECT r.user_id, r.quota_day,
               COALESCE(SUM(r.total_tokens), 0)::bigint AS total_tokens,
               COUNT(*)::bigint                          AS requests,
               COUNT(*) FILTER (
                   WHERE {error_condition("r.")}
               )::bigint                                 AS errors
        FROM usage_records r
        WHERE r.quota_day BETWEEN $1 AND $2
          AND (r.user_id = ANY($3::bigint[]) OR r.user_id IS NULL)
        GROUP BY r.user_id, r.quota_day
        ORDER BY r.quota_day
        """,
        start_day,
        end_day,
        list(user_ids),
    )


async def error_summary(db: Database, since: datetime) -> asyncpg.Record | None:
    return await db.fetchrow(
        f"""
        SELECT COUNT(*)::bigint AS requests,
               COUNT(*) FILTER (
                   WHERE {error_condition()}
               )::bigint AS errors
        FROM usage_records
        WHERE created_at >= $1
        """,
        since,
    )


async def errors_by_code(db: Database, since: datetime, limit: int = 20) -> list[asyncpg.Record]:
    return await db.fetch(
        f"""
        SELECT COALESCE(NULLIF(error_code, ''), 'http_' || status_code::text) AS label,
               COUNT(*)::bigint AS count
        FROM usage_records
        WHERE created_at >= $1 AND {error_condition()}
        GROUP BY label
        ORDER BY count DESC
        LIMIT $2
        """,
        since,
        limit,
    )


async def errors_by_status(db: Database, since: datetime, limit: int = 20) -> list[asyncpg.Record]:
    return await db.fetch(
        f"""
        SELECT status_code::text AS label, COUNT(*)::bigint AS count
        FROM usage_records
        WHERE created_at >= $1 AND {error_condition()}
        GROUP BY status_code
        ORDER BY count DESC
        LIMIT $2
        """,
        since,
        limit,
    )


async def errors_by_key(db: Database, since: datetime, limit: int = 20) -> list[asyncpg.Record]:
    return await db.fetch(
        f"""
        SELECT r.upstream_key_id AS key_id,
               COALESCE(k.pool, '')   AS pool,
               COALESCE(k.status, '') AS status,
               COUNT(*)::bigint       AS requests,
               COUNT(*) FILTER (
                   WHERE {error_condition("r.")}
               )::bigint              AS errors,
               COALESCE((
                   SELECT COALESCE(NULLIF(e.error_code, ''), 'http_' || e.status_code::text)
                   FROM usage_records e
                   WHERE e.upstream_key_id = r.upstream_key_id AND e.created_at >= $1
                     AND {error_condition("e.")}
                   GROUP BY 1 ORDER BY COUNT(*) DESC LIMIT 1
               ), '') AS top_error
        FROM usage_records r
        LEFT JOIN upstream_keys k ON k.key_id = r.upstream_key_id
        WHERE r.created_at >= $1 AND r.upstream_key_id <> ''
        GROUP BY r.upstream_key_id, k.pool, k.status
        HAVING COUNT(*) FILTER (WHERE {error_condition("r.")}) > 0
        ORDER BY errors DESC
        LIMIT $2
        """,
        since,
        limit,
    )


async def errors_timeline(db: Database, since: datetime) -> list[asyncpg.Record]:
    return await db.fetch(
        f"""
        SELECT date_trunc('hour', created_at) AS hour,
               COUNT(*)::bigint AS requests,
               COUNT(*) FILTER (
                   WHERE {error_condition()}
               )::bigint AS errors
        FROM usage_records
        WHERE created_at >= $1
        GROUP BY hour
        ORDER BY hour
        """,
        since,
    )


async def egress_usage(db: Database, day: date) -> dict[str, dict[str, int]]:
    """按出口 IP 聚合当日用量与 Key 归属，返回 ``{addr: {字段: 值}}``。

    只查用量与归属，**不查出口自身的状态**。state / reputation / max_keys 需要
    从网关 `GET /admin/ips` 取 —— `egress_ips` 表在网关代码里从未被写入，
    读它得到的永远是初始值，会把已封禁的出口显示成健康。

    `active_keys` 仍从库里算: 网关只知道「这个出口绑了几个 Key」，不知道其中
    有多少已被禁用。两个数字都要给运维看 —— 差值即「占着出口容量但不可用」
    的 Key 数，是需要清理的对象。
    """
    rows = await db.fetch(
        f"""
        WITH bound AS (
            SELECT egress_ip,
                   COUNT(*)::bigint AS db_bound_keys,
                   COUNT(*) FILTER (WHERE status = 'active')::bigint AS active_keys
            FROM upstream_keys
            WHERE egress_ip <> ''
            GROUP BY egress_ip
        ), today AS (
            SELECT egress_ip,
                   COALESCE(SUM(total_tokens), 0)::bigint AS today_tokens,
                   COUNT(*)::bigint                        AS today_requests,
                   COUNT(*) FILTER (
                       WHERE {error_condition()}
                   )::bigint                               AS today_errors
            FROM usage_records
            WHERE quota_day = $1 AND egress_ip <> ''
            GROUP BY egress_ip
        )
        SELECT COALESCE(b.egress_ip, t.egress_ip)  AS addr,
               COALESCE(b.db_bound_keys, 0)        AS db_bound_keys,
               COALESCE(b.active_keys, 0)          AS active_keys,
               COALESCE(t.today_tokens, 0)         AS today_tokens,
               COALESCE(t.today_requests, 0)       AS today_requests,
               COALESCE(t.today_errors, 0)         AS today_errors
        FROM bound b
        FULL OUTER JOIN today t ON t.egress_ip = b.egress_ip
        ORDER BY 1
        """,
        day,
    )
    return {
        str(r["addr"]): {
            "db_bound_keys": int(r["db_bound_keys"]),
            "active_keys": int(r["active_keys"]),
            "today_tokens": int(r["today_tokens"]),
            "today_requests": int(r["today_requests"]),
            "today_errors": int(r["today_errors"]),
        }
        for r in rows
        if r["addr"]
    }


async def unbound_key_count(db: Database) -> int:
    return int(
        await db.fetchval(
            "SELECT COUNT(*)::bigint FROM upstream_keys WHERE egress_ip = ''",
            default=0,
        )
    )


# 孤儿出口的判定已移到 service.egress()：以网关出口池为基准，
# 而非查 egress_ips 表 —— 那张表网关从不写入，拿它做基准会把所有
# 实际在用的地址都判成孤儿。


async def active_key_ids(db: Database, limit: int = 2000) -> list[str]:
    rows = await db.fetch(
        "SELECT key_id FROM upstream_keys WHERE status = 'active' ORDER BY key_id LIMIT $1",
        limit,
    )
    return [str(r["key_id"]) for r in rows]


async def all_key_rows(db: Database, limit: int = 2000) -> list[asyncpg.Record]:
    return await db.fetch(
        "SELECT key_id, pool, status, egress_ip, refresh_state, refresh_confirmed_at, "
        "last_error FROM upstream_keys ORDER BY key_id LIMIT $1",
        limit,
    )


async def recent_drifts(db: Database, limit: int = 50) -> list[asyncpg.Record]:
    return await db.fetch(
        """
        SELECT upstream_key_id AS key_id, billing_kind, quota_day, drift, created_at
        FROM quota_drift_logs
        ORDER BY created_at DESC
        LIMIT $1
        """,
        limit,
    )


async def refresh_state_distribution(db: Database) -> list[asyncpg.Record]:
    return await db.fetch(
        "SELECT refresh_state AS name, COUNT(*)::bigint AS count FROM upstream_keys "
        "GROUP BY refresh_state ORDER BY count DESC"
    )


async def key_used_on_day(db: Database, day: date) -> dict[str, int]:
    rows = await db.fetch(
        "SELECT upstream_key_id, COALESCE(SUM(total_tokens), 0)::bigint AS used "
        "FROM usage_records WHERE quota_day = $1 AND upstream_key_id <> '' "
        "GROUP BY upstream_key_id",
        day,
    )
    return {str(r["upstream_key_id"]): int(r["used"]) for r in rows}


async def behavior_hour_histogram(
    db: Database, start_day: date, end_day: date, min_requests: int
) -> list[asyncpg.Record]:
    """按 Key × 小时聚合请求数，用于构造 24 维时段分布向量。

    小时取自 ``created_at`` 的本地小时。这是离线报表口径，不做实时计算。
    """
    return await db.fetch(
        """
        SELECT upstream_key_id,
               EXTRACT(HOUR FROM created_at)::int AS hour,
               COUNT(*)::bigint AS requests
        FROM usage_records
        WHERE quota_day BETWEEN $1 AND $2 AND upstream_key_id <> ''
          AND upstream_key_id IN (
              SELECT upstream_key_id FROM usage_records
              WHERE quota_day BETWEEN $1 AND $2 AND upstream_key_id <> ''
              GROUP BY upstream_key_id HAVING COUNT(*) >= $3
          )
        GROUP BY upstream_key_id, hour
        """,
        start_day,
        end_day,
        min_requests,
    )


async def behavior_model_histogram(
    db: Database, start_day: date, end_day: date, min_requests: int
) -> list[asyncpg.Record]:
    """按 Key × 模型聚合请求数，用于构造模型偏好分布向量。"""
    return await db.fetch(
        """
        SELECT upstream_key_id, model, COUNT(*)::bigint AS requests
        FROM usage_records
        WHERE quota_day BETWEEN $1 AND $2 AND upstream_key_id <> ''
          AND upstream_key_id IN (
              SELECT upstream_key_id FROM usage_records
              WHERE quota_day BETWEEN $1 AND $2 AND upstream_key_id <> ''
              GROUP BY upstream_key_id HAVING COUNT(*) >= $3
          )
        GROUP BY upstream_key_id, model
        """,
        start_day,
        end_day,
        min_requests,
    )


async def key_egress_map(db: Database) -> dict[str, str]:
    rows = await db.fetch("SELECT key_id, egress_ip FROM upstream_keys")
    return {str(r["key_id"]): str(r["egress_ip"]) for r in rows}
