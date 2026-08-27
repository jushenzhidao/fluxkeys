"""API 层测试。

用假 Database / Cache 注入，不依赖真实 Postgres / Redis。重点验证两件事：
1. **空数据不 500** —— 所有接口在数据源为空时都要返回结构完整的 200；
2. 参数校验与错误码正确（非法排序 400、Key 不存在 404、库不可用 503）。
"""

from __future__ import annotations

import os
from collections.abc import Iterator
from datetime import date, datetime
from typing import Any
from zoneinfo import ZoneInfo

import pytest
from fastapi.testclient import TestClient

from app.api.deps import get_conf, get_service
from app.cache import Cache
from app.config import Settings
from app.db import Database, StoreUnavailable
from app.main import app
from app.security import COOKIE_NAME, LoginRateLimiter, PasswordChecker, SessionSigner
from app.service import ReportService

SH = ZoneInfo("Asia/Shanghai")

# 所有 GET 报表接口，用于批量验证「空数据不 500」。
REPORT_ENDPOINTS = [
    "/api/overview",
    "/api/keys",
    "/api/usage/trend?days=7",
    "/api/usage/by-user?days=30",
    "/api/errors?hours=24",
    "/api/egress",
    "/api/quota/health",
    "/api/refresh/status",
    "/api/behavior/similarity?days=7",
]


class FakeRecord(dict):
    """模拟 asyncpg.Record：既能按键访问，也支持位置访问。"""

    def __getitem__(self, key: Any) -> Any:
        if isinstance(key, int):
            return list(self.values())[key]
        return super().__getitem__(key)


class EmptyDatabase(Database):
    """所有查询都返回空结果的假数据库，模拟全新部署。"""

    def __init__(self, settings: Settings) -> None:
        super().__init__(settings)
        self.available_flag = True

    async def connect(self) -> None:
        return None

    async def close(self) -> None:
        return None

    async def ping(self) -> bool:
        return self.available_flag

    async def fetch(self, sql: str, *args: Any) -> list[Any]:
        return []


class BrokenDatabase(EmptyDatabase):
    """查询一律抛 StoreUnavailable，验证 503 分支。"""

    async def ping(self) -> bool:
        return False

    async def fetch(self, sql: str, *args: Any) -> list[Any]:
        raise StoreUnavailable("connection refused")


class SeededDatabase(EmptyDatabase):
    """按 SQL 特征返回预置数据，覆盖有数据时的分支。"""

    async def fetch(self, sql: str, *args: Any) -> list[Any]:
        text = " ".join(sql.split())

        if "FROM usage_records WHERE quota_day = $1" in text and "used_keys" in text:
            return [
                FakeRecord(
                    total_tokens=3_000_000,
                    prompt_tokens=1_000_000,
                    completion_tokens=2_000_000,
                    count_units=12,
                    requests=1000,
                    errors=50,
                    avg_latency_ms=820.4,
                    used_keys=3,
                )
            ]
        if "COUNT(*) FILTER (WHERE status = 'active')" in text:
            return [FakeRecord(total=3, active=2, unconfirmed_refresh=1)]
        if "FROM volc_keys GROUP BY pool" in text:
            return [FakeRecord(name="hot", count=1), FakeRecord(name="cold", count=2)]
        if "FROM volc_keys GROUP BY status" in text:
            return [FakeRecord(name="active", count=2), FakeRecord(name="banned", count=1)]
        if "FROM volc_keys GROUP BY refresh_state" in text:
            return [
                FakeRecord(name="confirmed", count=2),
                FakeRecord(name="probing", count=1),
            ]
        if text.startswith("SELECT key_id FROM volc_keys WHERE status = 'active'"):
            return [FakeRecord(key_id="volc_001"), FakeRecord(key_id="volc_002")]
        if text.startswith("SELECT key_id, pool, status, egress_ip, refresh_state"):
            return [
                FakeRecord(
                    key_id="volc_001",
                    pool="hot",
                    status="active",
                    egress_ip="172.16.0.2",
                    refresh_state="confirmed",
                    refresh_confirmed_at=None,
                    last_error="",
                ),
                FakeRecord(
                    key_id="volc_002",
                    pool="cold",
                    status="active",
                    egress_ip="172.16.0.3",
                    refresh_state="probing",
                    refresh_confirmed_at=None,
                    last_error="quota exhausted",
                ),
            ]
        if "FROM volc_keys k" in text and "LEFT JOIN today" in text:
            return [
                FakeRecord(
                    key_id="volc_001",
                    pool="hot",
                    status="active",
                    persona_id="p1",
                    egress_ip="172.16.0.2",
                    health_score=95,
                    refresh_state="confirmed",
                    refresh_confirmed_at=None,
                    last_error="",
                    last_used_at=None,
                    today_tokens=2_000_000,
                    today_requests=600,
                    today_errors=10,
                )
            ]
        if text.startswith("SELECT COUNT(*)::bigint FROM volc_keys WHERE ($1::text IS NULL"):
            return [FakeRecord(count=2)]
        if "FROM volc_keys k CROSS JOIN today t" in text:
            return [
                FakeRecord(
                    key_id="volc_001",
                    pool="hot",
                    status="active",
                    persona_id="p1",
                    egress_ip="172.16.0.2",
                    health_score=95,
                    refresh_state="confirmed",
                    refresh_confirmed_at=None,
                    last_error="",
                    last_used_at=None,
                    today_tokens=2_000_000,
                    today_requests=600,
                    today_errors=10,
                )
            ]
        if "quota_day = ANY($2::date[])" in text and "avg_latency_ms" in text:
            return [
                FakeRecord(
                    quota_day=date(2026, 8, 23),
                    total_tokens=2_000_000,
                    prompt_tokens=800_000,
                    completion_tokens=1_200_000,
                    count_units=3,
                    requests=600,
                    errors=10,
                    avg_latency_ms=700.0,
                )
            ]
        if "quota_day = ANY($1::date[])" in text:
            return [
                FakeRecord(
                    quota_day=date(2026, 8, 23),
                    total_tokens=3_000_000,
                    prompt_tokens=1_000_000,
                    completion_tokens=2_000_000,
                    count_units=12,
                    requests=1000,
                    errors=50,
                    active_keys=3,
                    avg_latency_ms=820.0,
                )
            ]
        if "LEFT JOIN users u" in text:
            return [
                FakeRecord(
                    user_id=1,
                    user_name="张三",
                    email="a@example.com",
                    status="active",
                    daily_token_limit=1_000_000,
                    total_tokens=2_500_000,
                    prompt_tokens=1_000_000,
                    completion_tokens=1_500_000,
                    count_units=0,
                    requests=800,
                    errors=8,
                    avg_latency_ms=600.0,
                    active_days=5,
                    first_day=date(2026, 8, 19),
                    last_day=date(2026, 8, 23),
                ),
                FakeRecord(
                    user_id=None,
                    user_name="(未归属)",
                    email=None,
                    status="",
                    daily_token_limit=0,
                    total_tokens=500_000,
                    prompt_tokens=200_000,
                    completion_tokens=300_000,
                    count_units=0,
                    requests=200,
                    errors=42,
                    avg_latency_ms=900.0,
                    active_days=2,
                    first_day=date(2026, 8, 22),
                    last_day=date(2026, 8, 23),
                ),
            ]
        if "date_trunc('hour', created_at)" in text:
            return [
                FakeRecord(hour=datetime(2026, 8, 23, 19, tzinfo=SH), requests=400, errors=20),
                FakeRecord(hour=datetime(2026, 8, 23, 20, tzinfo=SH), requests=600, errors=30),
            ]
        if text.startswith("SELECT COUNT(*)::bigint AS requests"):
            return [FakeRecord(requests=1000, errors=50)]
        if "GROUP BY volc_key_id, hour" in text:
            # 两个 Key 时段分布完全一致 → 应触发相似度告警
            return [
                FakeRecord(volc_key_id="volc_001", hour=9, requests=30),
                FakeRecord(volc_key_id="volc_001", hour=10, requests=30),
                FakeRecord(volc_key_id="volc_002", hour=9, requests=30),
                FakeRecord(volc_key_id="volc_002", hour=10, requests=30),
            ]
        if "GROUP BY volc_key_id, model" in text:
            return [
                FakeRecord(volc_key_id="volc_001", model="deepseek-v3", requests=60),
                FakeRecord(volc_key_id="volc_002", model="deepseek-v3", requests=60),
            ]
        if text.startswith("SELECT key_id, egress_ip FROM volc_keys"):
            return [
                FakeRecord(key_id="volc_001", egress_ip="172.16.0.2"),
                FakeRecord(key_id="volc_002", egress_ip="172.16.0.2"),
            ]
        return []


class FakeCache(Cache):
    """可注入热态数据的假 Redis。"""

    def __init__(self, settings: Settings, data: dict[str, dict[str, str]] | None = None) -> None:
        super().__init__(settings)
        self.data = data or {}
        self.lease_total = 0
        self.lease_expired = 0
        self.lease_amounts: dict[tuple[str, str], int] = {}

    async def connect(self) -> None:
        return None

    async def close(self) -> None:
        return None

    async def ping(self) -> bool:
        return True

    async def snapshot(self, key_id: str, day: date, kind: str = "token") -> Any:
        from app.cache import QuotaSnapshot

        return QuotaSnapshot(key_id, kind, self.data.get(key_id), self._hard_default(kind))

    async def snapshots(self, key_ids: Any, day: date, kind: str = "token") -> Any:
        from app.cache import QuotaSnapshot

        return {
            key_id: QuotaSnapshot(key_id, kind, self.data.get(key_id), self._hard_default(kind))
            for key_id in key_ids
        }

    async def lease_aggregate(self, day: date, now_ts: int, limit: int = 5000) -> Any:
        from app.cache import LeaseAggregate

        agg = LeaseAggregate()
        agg.total = self.lease_total
        agg.expired = self.lease_expired
        agg.by_key_amount = dict(self.lease_amounts)
        agg.by_key_count = dict.fromkeys(self.lease_amounts, 1)
        return agg


def make_client(
    database: Database,
    cache: Cache,
    settings: Settings,
    *,
    authenticated: bool = True,
) -> TestClient:
    """构造注入了假数据源的 TestClient。

    ``authenticated`` 默认为 True 并预置一个有效会话 Cookie：报表接口现在
    全部需要鉴权，逐个测试自己登录一次只是重复噪声。鉴权本身的行为由
    test_auth.py 专门覆盖。
    """
    service = ReportService(database, cache, settings)
    app.dependency_overrides[get_service] = lambda: service
    app.dependency_overrides[get_conf] = lambda: settings

    # 直接装配鉴权组件。
    #
    # TestClient 默认不跑 lifespan，而中间件在组件缺失时会返回 503 ——
    # 那是刻意的「绝不 fail open」，所以这里必须显式把组件放进 state。
    signer = SessionSigner(settings.session_secret, settings.session_ttl)
    app.state.settings = settings
    app.state.session_signer = signer
    app.state.password_checker = PasswordChecker(settings.password)
    app.state.login_limiter = LoginRateLimiter(
        settings.login_max_attempts, settings.login_lockout_seconds
    )

    client = TestClient(app)
    if authenticated:
        client.cookies.set(COOKIE_NAME, signer.issue())
    return client


@pytest.fixture
def empty_client(settings: Settings) -> Iterator[TestClient]:
    client = make_client(EmptyDatabase(settings), FakeCache(settings), settings)
    yield client
    app.dependency_overrides.clear()


@pytest.fixture
def seeded_cache(settings: Settings) -> FakeCache:
    cache = FakeCache(
        settings,
        {
            "volc_001": {
                "used": "4_000_000".replace("_", ""),
                "prededuct": "300000",
                "hard_limit": "4500000",
                "soft_limit": "4000000",
                "last_updated": "1756000000",
            },
            "volc_002": {
                "used": "100000",
                "prededuct": "900000",
                "hard_limit": "4500000",
                "soft_limit": "4000000",
                "last_updated": "1756000000",
            },
        },
    )
    cache.lease_total = 3
    cache.lease_expired = 2
    # volc_001 的预扣与租约一致；volc_002 预扣 90 万但只有 10 万租约 → 泄漏
    cache.lease_amounts = {("volc_001", "token"): 300_000, ("volc_002", "token"): 100_000}
    return cache


@pytest.fixture
def seeded_client(settings: Settings, seeded_cache: FakeCache) -> Iterator[TestClient]:
    client = make_client(SeededDatabase(settings), seeded_cache, settings)
    yield client
    app.dependency_overrides.clear()


# ---------------------------------------------------------------- 空数据


class TestEmptyDataNeverFails:
    """核心质量要求：数据为空时所有接口都要 200，不能 500。"""

    @pytest.mark.parametrize("url", REPORT_ENDPOINTS)
    def test_endpoint_returns_200(self, empty_client: TestClient, url: str) -> None:
        resp = empty_client.get(url)
        assert resp.status_code == 200, f"{url} → {resp.status_code}: {resp.text}"
        assert isinstance(resp.json(), dict)

    def test_overview_zeros(self, empty_client: TestClient) -> None:
        body = empty_client.get("/api/overview").json()
        assert body["total_tokens"] == 0
        assert body["total_requests"] == 0
        assert body["error_rate"] == 0.0
        # 除零必须被安全处理
        assert body["avg_quota_ratio"] == 0.0
        assert body["pools"] == []
        assert body["quota_day"]

    def test_keys_empty_list(self, empty_client: TestClient) -> None:
        body = empty_client.get("/api/keys").json()
        assert body["total"] == 0
        assert body["items"] == []

    def test_trend_pads_missing_days(self, empty_client: TestClient) -> None:
        """无数据也要补齐每个配额日，前端曲线才不会断。"""
        body = empty_client.get("/api/usage/trend?days=7").json()
        assert len(body["points"]) == 7
        assert all(p["total_tokens"] == 0 for p in body["points"])
        days = [p["quota_day"] for p in body["points"]]
        assert days == sorted(days)

    def test_similarity_has_note_when_no_sample(self, empty_client: TestClient) -> None:
        body = empty_client.get("/api/behavior/similarity").json()
        assert body["compared_pairs"] == 0
        assert body["pairs"] == []
        assert body["note"]

    def test_quota_health_no_alerts(self, empty_client: TestClient) -> None:
        body = empty_client.get("/api/quota/health").json()
        assert body["alerts"] == []
        assert body["checked_keys"] == 0

    def test_key_detail_404(self, empty_client: TestClient) -> None:
        resp = empty_client.get("/api/keys/not_exist")
        assert resp.status_code == 404

    def test_healthz_ok(self, empty_client: TestClient) -> None:
        body = empty_client.get("/healthz").json()
        assert body["status"] in {"ok", "degraded"}
        assert body["quota_day"]


# ---------------------------------------------------------------- 有数据


class TestSeededData:
    def test_overview_aggregates(self, seeded_client: TestClient) -> None:
        body = seeded_client.get("/api/overview").json()
        assert body["total_tokens"] == 3_000_000
        assert body["total_requests"] == 1000
        assert body["error_rate"] == pytest.approx(0.05)
        assert body["active_keys"] == 2
        assert body["total_keys"] == 3
        assert body["unconfirmed_refresh"] == 1
        # volc_001: (400万+30万)/450万 ≈ 0.956, volc_002: 100万/450万 ≈ 0.222
        assert body["max_quota_ratio"] == pytest.approx(4_300_000 / 4_500_000)
        assert body["capacity_tokens"] == 9_000_000
        assert body["alert_count"] > 0

    def test_keys_row_merges_redis_hot_state(self, seeded_client: TestClient) -> None:
        body = seeded_client.get("/api/keys").json()
        row = body["items"][0]
        assert row["key_id"] == "volc_001"
        assert row["token"]["used"] == 4_000_000
        assert row["token"]["prededuct"] == 300_000
        assert row["token"]["remaining"] == 200_000
        assert row["token"]["hot"] is True
        assert row["today_tokens"] == 2_000_000

    def test_key_detail_trend_padded(self, seeded_client: TestClient) -> None:
        body = seeded_client.get("/api/keys/volc_001?days=7").json()
        assert body["key"]["key_id"] == "volc_001"
        assert len(body["trend"]) == 7
        assert body["lease_amount"] == 300_000

    def test_usage_by_user_includes_unattributed(self, seeded_client: TestClient) -> None:
        """未归属流量必须出现在结果里，计费对账不能丢数据。"""
        body = seeded_client.get("/api/usage/by-user?days=30").json()
        names = [i["user_name"] for i in body["items"]]
        assert "张三" in names
        assert "(未归属)" in names
        assert body["total_tokens"] == 3_000_000
        unattributed = next(i for i in body["items"] if i["user_id"] is None)
        assert unattributed["error_rate"] == pytest.approx(42 / 200)

    def test_quota_health_detects_lease_leak(self, seeded_client: TestClient) -> None:
        """volc_002 预扣 90 万但租约只有 10 万 → 泄漏告警。"""
        body = seeded_client.get("/api/quota/health").json()
        leaks = {row["key_id"]: row for row in body["lease_leaks"]}
        assert "volc_002" in leaks
        assert leaks["volc_002"]["gap"] == 800_000
        assert "volc_001" not in leaks
        assert body["expired_leases"] == 2
        kinds = {a["kind"] for a in body["alerts"]}
        assert "lease_leak" in kinds

    def test_quota_health_detects_near_hard(self, seeded_client: TestClient) -> None:
        body = seeded_client.get("/api/quota/health").json()
        near = {row["key_id"] for row in body["near_hard"]}
        assert "volc_001" in near  # 95.6% ≥ 85%
        assert "volc_002" not in near

    def test_refresh_status_flags_risky_keys(self, seeded_client: TestClient) -> None:
        body = seeded_client.get("/api/refresh/status").json()
        assert body["confirmed"] == 1
        assert body["unconfirmed"] == 1
        risky = [r["key_id"] for r in body["risky_keys"]]
        # volc_002 未确认刷新（probing）且 Redis 有 used → 高风险
        assert risky == ["volc_002"]

    def test_similarity_flags_identical_behavior(self, seeded_client: TestClient) -> None:
        body = seeded_client.get("/api/behavior/similarity?days=7").json()
        assert body["sampled_keys"] == 2
        assert body["compared_pairs"] == 1
        assert body["alert_pairs"] == 1
        pair = body["pairs"][0]
        assert pair["similarity"] == pytest.approx(1.0)
        # 两个 Key 同出口 IP 且行为相同 → critical
        assert pair["same_egress"] is True
        assert pair["level"] == "critical"

    def test_errors_summary(self, seeded_client: TestClient) -> None:
        body = seeded_client.get("/api/errors?hours=24").json()
        assert body["total_requests"] == 1000
        assert body["total_errors"] == 50
        assert body["error_rate"] == pytest.approx(0.05)
        assert len(body["timeline"]) == 2
        assert body["timeline"][0]["error_rate"] == pytest.approx(0.05)

    def test_trend_uses_quota_day(self, seeded_client: TestClient) -> None:
        body = seeded_client.get("/api/usage/trend?days=3").json()
        assert len(body["points"]) == 3
        filled = [p for p in body["points"] if p["total_tokens"] > 0]
        assert len(filled) == 1
        assert filled[0]["error_rate"] == pytest.approx(0.05)


# ---------------------------------------------------------------- 参数与错误


class TestValidation:
    def test_invalid_sort_is_400(self, empty_client: TestClient) -> None:
        """排序字段走白名单，非法值返回 400 而不是拼进 SQL。"""
        resp = empty_client.get("/api/keys?sort=key_id;DROP TABLE volc_keys")
        assert resp.status_code == 400

    def test_sql_injection_attempt_rejected(self, empty_client: TestClient) -> None:
        resp = empty_client.get("/api/keys?sort=1' OR '1'='1")
        assert resp.status_code == 400

    @pytest.mark.parametrize(
        "sort",
        [
            # 落库口径，下推 SQL
            "key_id",
            "health",
            "pool",
            "status",
            "last_used",
            "today_tokens",
            # 热态口径，Python 侧排序
            "ratio",
            "remaining",
            "used",
        ],
    )
    def test_all_documented_sorts_accepted(self, empty_client: TestClient, sort: str) -> None:
        assert empty_client.get(f"/api/keys?sort={sort}&order=desc").status_code == 200

    def test_invalid_order_is_422(self, empty_client: TestClient) -> None:
        assert empty_client.get("/api/keys?order=random").status_code == 422

    @pytest.mark.parametrize("days", [0, -1, 999])
    def test_trend_days_out_of_range(self, empty_client: TestClient, days: int) -> None:
        assert empty_client.get(f"/api/usage/trend?days={days}").status_code == 422

    def test_errors_hours_out_of_range(self, empty_client: TestClient) -> None:
        assert empty_client.get("/api/errors?hours=0").status_code == 422

    def test_similarity_threshold_out_of_range(self, empty_client: TestClient) -> None:
        assert empty_client.get("/api/behavior/similarity?threshold=1.5").status_code == 422

    def test_status_filter_is_parameterized(self, empty_client: TestClient) -> None:
        """恶意 status 值只会作为参数传入，不产生 500。"""
        resp = empty_client.get("/api/keys?status=active'; DROP TABLE users;--")
        assert resp.status_code == 200
        assert resp.json()["items"] == []


class TestStoreUnavailable:
    """数据库不可用返回 503，便于探针区分故障与 bug。"""

    @pytest.fixture
    def broken_client(self, settings: Settings) -> Iterator[TestClient]:
        client = make_client(BrokenDatabase(settings), FakeCache(settings), settings)
        yield client
        app.dependency_overrides.clear()

    @pytest.mark.parametrize("url", ["/api/overview", "/api/keys", "/api/usage/trend"])
    def test_returns_503(self, broken_client: TestClient, url: str) -> None:
        resp = broken_client.get(url)
        assert resp.status_code == 503
        assert "数据源" in resp.json()["detail"]

    def test_healthz_reports_degraded_but_200(self, broken_client: TestClient) -> None:
        """探活接口本身不能因为下游挂掉而失败。"""
        resp = broken_client.get("/healthz")
        assert resp.status_code == 200
        assert resp.json()["status"] == "degraded"


class TestStaticAssets:
    def test_index_served(self, empty_client: TestClient) -> None:
        resp = empty_client.get("/")
        assert resp.status_code == 200
        assert "FluxKeys" in resp.text

    def test_js_and_css_served(self, empty_client: TestClient) -> None:
        assert empty_client.get("/app.js").status_code == 200
        assert empty_client.get("/app.css").status_code == 200

    def test_openapi_disabled_by_default(self, empty_client: TestClient) -> None:
        """自动文档默认关闭。

        原断言「/openapi.json 可用」已随管理控制台改造失效：该端点会把管理
        接口的请求体结构完整暴露，生产默认关闭是鉴权之外的纵深防御。
        这里改为断言默认关闭 —— 若哪天有人把默认值翻回去，这条会变红。
        """
        assert empty_client.get("/openapi.json").status_code == 404

    def test_openapi_exposes_paths_when_enabled(self, settings: Settings) -> None:
        """显式开启时仍应能拿到完整 schema，保证开关本身是有效的。"""
        import importlib

        import app.main as main_module

        os.environ["DASHBOARD_DOCS_ENABLED"] = "true"
        try:
            reloaded = importlib.reload(main_module)
            signer = SessionSigner(settings.session_secret, settings.session_ttl)
            # 手工注入而不进 TestClient 的上下文管理器：进上下文会跑 lifespan，
            # 那会连数据库与 Redis，而本条只关心 schema 是否挂出来了。
            reloaded.app.state.settings = settings
            reloaded.app.state.session_signer = signer
            client = TestClient(reloaded.app)
            client.cookies.set(COOKIE_NAME, signer.issue())
            body = client.get("/openapi.json").json()
            assert "/api/overview" in body["paths"]
            # 管理转发端点也应在 schema 内，否则说明路由没挂上
            assert "/api/admin/keys/{key_id}" in body["paths"]
        finally:
            os.environ.pop("DASHBOARD_DOCS_ENABLED", None)
            # 重新载入以恢复默认关闭状态，避免污染后续测试。
            importlib.reload(main_module)


# ---------------------------------------------------------------- 出口封禁


class FakeGateway:
    """只实现 fetch_egress_ips 的网关替身。

    出口面板的 state / reputation / 封禁三字段全部来自网关的内存快照，
    库里没有（`egress_ips` 表在网关代码里从未被写入）。所以要覆盖这条链路
    只能替掉网关，替库是无效的。
    """

    def __init__(self, per_ip: list[dict[str, Any]]) -> None:
        self._per_ip = per_ip

    async def fetch_egress_ips(self, actor: str) -> Any:
        from app.gateway.client import GatewayResponse

        return GatewayResponse(status=200, payload={"per_ip": self._per_ip})


def _egress_client(settings: Settings, per_ip: list[dict[str, Any]]) -> Iterator[TestClient]:
    """构造一个出口状态可控的客户端。

    库用 EmptyDatabase: 出口面板的用量列取自 usage_records，本组用例只关心
    网关侧的状态字段，让用量全为 0 更容易定位问题。
    """
    client = make_client(EmptyDatabase(settings), FakeCache(settings), settings)
    service = app.dependency_overrides[get_service]()
    service._gateway = FakeGateway(per_ip)
    yield client
    app.dependency_overrides.clear()


class TestEgressBanFields:
    """被封出口的三个字段必须原样透传到看板。

    这三个字段是「出口何时会自愈」的唯一依据。漏掉任何一个，
    运维只能看到 banned 而不知道还要等多久，进而误以为需要人工介入。
    """

    def test_封禁出口透传时间与轮次(self, settings: Settings) -> None:
        per_ip = [
            {
                "addr": "172.16.0.11",
                "public_ip": "203.0.113.11",
                "state": "banned",
                "reputation": 40,
                "max_keys": 10,
                "pool": "hot",
                "bound_keys": 0,
                "banned_at": "2026-08-25T10:00:00+08:00",
                "unban_at": "2026-08-25T12:00:00+08:00",
                "ban_count": 2,
            }
        ]
        for c in _egress_client(settings, per_ip):
            body = c.get("/api/egress").json()
            assert body["total"] == 1
            row = body["items"][0]
            assert row["state"] == "banned"
            assert row["ban_count"] == 2
            # 时间必须是可解析的 ISO 串，前端要拿它算剩余分钟数
            assert row["banned_at"].startswith("2026-08-25T10:00:00")
            assert row["unban_at"].startswith("2026-08-25T12:00:00")

    def test_未封禁出口三字段为空(self, settings: Settings) -> None:
        # 网关用 omitempty 序列化，健康出口不会带这三个键。
        # 缺键时必须是 None / 0，不能报错也不能填假值。
        per_ip = [
            {
                "addr": "172.16.0.12",
                "state": "active",
                "reputation": 100,
                "max_keys": 10,
                "pool": "hot",
                "bound_keys": 8,
            }
        ]
        for c in _egress_client(settings, per_ip):
            row = c.get("/api/egress").json()["items"][0]
            assert row["state"] == "active"
            assert row["banned_at"] is None
            assert row["unban_at"] is None
            assert row["ban_count"] == 0

    def test_时间格式异常不影响其余字段(self, settings: Settings) -> None:
        # 网关升级换了时间格式、或字段被填成非时间字符串时，
        # 出口面板不能整个 500 —— 状态与负载信息仍然有价值。
        per_ip = [
            {
                "addr": "172.16.0.13",
                "state": "banned",
                "reputation": 30,
                "max_keys": 10,
                "pool": "warm",
                "bound_keys": 0,
                "banned_at": "not-a-timestamp",
                "unban_at": 12345,
                "ban_count": 1,
            }
        ]
        for c in _egress_client(settings, per_ip):
            resp = c.get("/api/egress")
            assert resp.status_code == 200
            row = resp.json()["items"][0]
            assert row["state"] == "banned"
            assert row["reputation"] == 30
            assert row["ban_count"] == 1
            assert row["banned_at"] is None
            assert row["unban_at"] is None


class TestEgressStale:
    """网关取不到出口状态时必须显式标注，而不是静默呈现健康。

    这是生产最常见的故障形态: 网关重启、看板配错地址、鉴权失效。
    此时 state 列没有数据，若不标注 stale，运维看到的是「一个出口都没被封」
    ——与「全部健康」在界面上完全无法区分，封禁事件会被整个漏掉。
    """

    def test_未配置网关时标注降级(self, settings: Settings) -> None:
        client = make_client(EmptyDatabase(settings), FakeCache(settings), settings)
        service = app.dependency_overrides[get_service]()
        service._gateway = None
        try:
            body = client.get("/api/egress").json()
            assert body["stale"] is True
            assert "未配置网关" in body["stale_reason"]
            assert body["items"] == []
            # 其余字段仍应正常返回 —— 降级只影响出口这一块
            assert "quota_day" in body
        finally:
            app.dependency_overrides.clear()

    def test_网关不可达时标注降级(self, settings: Settings) -> None:
        from app.gateway.client import GatewayError

        class DownGateway:
            async def fetch_egress_ips(self, actor: str) -> Any:
                raise GatewayError(status=502, detail="connect refused")

        client = make_client(EmptyDatabase(settings), FakeCache(settings), settings)
        service = app.dependency_overrides[get_service]()
        service._gateway = DownGateway()
        try:
            resp = client.get("/api/egress")
            # 不可达是预期状况，不该让整个接口 5xx
            assert resp.status_code == 200
            body = resp.json()
            assert body["stale"] is True
            assert "connect refused" in body["stale_reason"]
        finally:
            app.dependency_overrides.clear()

    def test_网关返回格式异常时标注降级(self, settings: Settings) -> None:
        from app.gateway.client import GatewayResponse

        class BadGateway:
            def __init__(self, payload: Any) -> None:
                self._payload = payload

            async def fetch_egress_ips(self, actor: str) -> Any:
                return GatewayResponse(status=200, payload=self._payload)

        for payload, why in [
            ("not-a-dict", "顶层非对象"),
            ({}, "缺 per_ip"),
            ({"per_ip": "oops"}, "per_ip 非数组"),
        ]:
            client = make_client(EmptyDatabase(settings), FakeCache(settings), settings)
            service = app.dependency_overrides[get_service]()
            service._gateway = BadGateway(payload)
            try:
                body = client.get("/api/egress").json()
                assert body["stale"] is True, why
                assert body["stale_reason"], why
            finally:
                app.dependency_overrides.clear()

    def test_正常取到状态时不标注降级(self, settings: Settings) -> None:
        # 对照用例: 证明 stale 不是恒为 true
        per_ip = [{"addr": "172.16.0.11", "state": "active", "reputation": 100}]
        for c in _egress_client(settings, per_ip):
            body = c.get("/api/egress").json()
            assert body["stale"] is False
            assert body["stale_reason"] == ""
            assert body["total"] == 1
