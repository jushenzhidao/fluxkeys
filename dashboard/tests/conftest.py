"""pytest 公共夹具。

设计原则：**无 Postgres / Redis 时 skip 而非失败**。CI 与本地开发常常没有
这两个依赖，看板测试不该因此变红。需要真实数据源的测试统一挂
``@pytest.mark.integration`` 并依赖 ``live_db`` 夹具。
"""

from __future__ import annotations

import os
from collections.abc import AsyncIterator
from datetime import datetime
from zoneinfo import ZoneInfo

import pytest

from app.cache import Cache
from app.config import Settings
from app.db import Database
from app.service import ReportService


def pytest_configure(config: pytest.Config) -> None:
    config.addinivalue_line("markers", "integration: 需要真实 Postgres / Redis 的集成测试")


# 测试用的管理控制台配置。
#
# 口令与密钥的长度必须满足 config 的下限校验，否则这里构造出的 Settings
# 与真实 get_settings() 能产出的取值集合不一致，测试就会放过真实配置下
# 才会出现的问题。
TEST_PASSWORD = "test-password-1234"
TEST_SESSION_SECRET = "test-session-secret-at-least-32-chars-long"
TEST_ADMIN_API_KEY = "test-admin-api-key"


@pytest.fixture(scope="session")
def settings() -> Settings:
    """测试用配置。时区固定，避免不同机器结果不一致。"""
    return Settings(
        postgres_dsn=os.getenv(
            "TEST_POSTGRES_DSN",
            os.getenv(
                "POSTGRES_DSN",
                "postgres://fluxkeys:fluxkeys@127.0.0.1:5432/fluxkeys?sslmode=disable",
            ),
        ),
        redis_addr=os.getenv("TEST_REDIS_ADDR", os.getenv("REDIS_ADDR", "127.0.0.1:6379")),
        redis_password="",
        redis_db=int(os.getenv("TEST_REDIS_DB", "15")),
        port=8081,
        timezone="Asia/Shanghai",
        token_hard_default=4_500_000,
        count_hard_default=90,
        pg_max_size=4,
        pg_min_size=1,
        similarity_alert=0.6,
        password=TEST_PASSWORD,
        session_secret=TEST_SESSION_SECRET,
        gateway_admin_api_key=TEST_ADMIN_API_KEY,
        gateway_base_url="http://gateway:8080",
        # 与 config.py 的默认值保持一致。测试 fixture 用固定值本身没问题，
        # 但「Max-Age 断言」那条测试会拿它比对响应头 —— 若两边不一致，
        # 测的就不是真实默认行为了。
        session_ttl=7_200,
        cookie_secure=False,
        allowed_origins=(),
        login_max_attempts=5,
        login_lockout_seconds=900,
        gateway_timeout_import=90,
        docs_enabled=False,
    )


@pytest.fixture
def fixed_now() -> datetime:
    """固定时刻，位于配额日窗口内（20:00 → 配额日 2026-08-23）。"""
    return datetime(2026, 8, 23, 20, 0, tzinfo=ZoneInfo("Asia/Shanghai"))


@pytest.fixture
async def live_db(settings: Settings) -> AsyncIterator[Database]:
    """真实 Postgres 连接。

    未配置环境变量则 skip（本地快跑、无 Docker 的 CI 阶段是合理场景）；
    但**配置了却连不上则直接 fail**。

    这个区分是必须的。曾经这里一律 skip，结果 18 个集成测试因为环境变量名
    写错（`POSTGRES_TEST_DSN` 而非 `TEST_POSTGRES_DSN`）退化到默认 DSN，
    认证失败后静默跳过 —— `pytest -q` 照样显示全绿，而这 18 条从未真正执行。
    只有加 `-rs` 才能看到 skip 数从 1 变成 18，没人会天天加这个参数。

    「配了但连不上」几乎总是配置错误或服务没起，那是需要立刻知道的失败，
    不是可以跳过的环境差异。
    """
    configured = bool(os.getenv("TEST_POSTGRES_DSN") or os.getenv("POSTGRES_DSN"))

    database = Database(settings)
    await database.connect()
    if not await database.ping():
        err = database.error
        await database.close()
        if configured:
            pytest.fail(
                f"已配置 Postgres DSN 但连接失败，这是配置错误而非环境缺失: {err}\n"
                f"检查变量名是否为 TEST_POSTGRES_DSN（不是 POSTGRES_TEST_DSN）、"
                f"端口与凭据是否正确。"
            )
        pytest.skip(f"未配置 TEST_POSTGRES_DSN，跳过集成测试（连接错误：{err}）")
    try:
        yield database
    finally:
        await database.close()


@pytest.fixture
async def live_cache(settings: Settings) -> AsyncIterator[Cache]:
    """真实 Redis 连接。

    与 live_db 同理：未配置则 skip，配置了却连不上则 fail。见 live_db 的注释。
    """
    configured = bool(os.getenv("TEST_REDIS_ADDR") or os.getenv("REDIS_ADDR"))

    cache = Cache(settings)
    await cache.connect()
    if not await cache.ping():
        err = cache.error
        await cache.close()
        if configured:
            pytest.fail(f"已配置 Redis 地址但连接失败，这是配置错误而非环境缺失: {err}")
        pytest.skip(f"未配置 TEST_REDIS_ADDR，跳过集成测试（连接错误：{err}）")
    try:
        yield cache
    finally:
        await cache.close()


@pytest.fixture
async def live_service(live_db: Database, live_cache: Cache, settings: Settings) -> ReportService:
    return ReportService(live_db, live_cache, settings)
