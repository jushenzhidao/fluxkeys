"""转发层测试。

用 httpx 的 MockTransport 而非起一个真网关：要断言的是**看板如何解释网关的
响应**，逐条编程的响应比真实服务更能覆盖 401/405/503 这些难以自然触发的分支。

覆盖重点：
1. 401 必须转成 500 —— 透传会让运维陷入「反复登录仍失败」的死循环；
2. 读超时的文案必须引导「先刷新确认」而非「请重试」；
3. 请求体原样透传，不重新序列化；
4. ADMIN_API_KEY 由服务端注入，绝不出现在响应里。
"""

from __future__ import annotations

import json
from collections.abc import Callable, Iterator

import httpx
import pytest
from fastapi.testclient import TestClient

from app.api.deps import get_conf, get_service
from app.config import Settings
from app.gateway import GatewayClient, GatewayError
from app.main import app
from app.security import COOKIE_NAME, SessionSigner
from app.service import ReportService
from tests.test_api import EmptyDatabase, FakeCache


# 网关侧的 OpenAI 风格错误结构。
def _gw_error(message: str, code: str = "invalid_request") -> dict[str, object]:
    return {"error": {"message": message, "type": "invalid_request_error", "code": code}}


def _make_client(
    settings: Settings,
    handler: Callable[[httpx.Request], httpx.Response],
) -> TestClient:
    """构造一个转发到 MockTransport 的已登录客户端。"""
    database = EmptyDatabase(settings)
    cache = FakeCache(settings)

    gateway = GatewayClient(
        settings.gateway_base_url,
        settings.gateway_admin_api_key,
        float(settings.gateway_timeout_import),
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
    )

    # gateway 必须同时给到 ReportService 与 app.state，与 main.py 的装配一致。
    #
    # 只挂 app.state 会让 /api/egress 静默走降级分支（service._gateway is None），
    # 于是断言「网关返回了什么」的用例全部变成断言降级路径 —— 恒真且毫无价值。
    app.dependency_overrides[get_service] = lambda: ReportService(
        database, cache, settings, gateway
    )
    app.dependency_overrides[get_conf] = lambda: settings

    signer = SessionSigner(settings.session_secret, settings.session_ttl)
    app.state.settings = settings
    app.state.session_signer = signer
    app.state.gateway_client = gateway

    client = TestClient(app)
    client.cookies.set(COOKIE_NAME, signer.issue())
    return client


@pytest.fixture(autouse=True)
def _cleanup() -> Iterator[None]:
    yield
    app.dependency_overrides.clear()
    if hasattr(app.state, "gateway_client"):
        delattr(app.state, "gateway_client")


# ---------------------------------------------------------------- 成功路径


class TestForwardSuccess:
    def test_patch_forwards_and_returns_payload(self, settings: Settings) -> None:
        captured: dict[str, object] = {}

        def handler(request: httpx.Request) -> httpx.Response:
            captured["method"] = request.method
            captured["url"] = str(request.url)
            captured["auth"] = request.headers.get("authorization")
            captured["actor"] = request.headers.get("x-admin-actor")
            captured["body"] = request.content
            return httpx.Response(
                200,
                json={
                    "key_id": "volc_001",
                    "changed": ["status"],
                    "status": {"from": "active", "to": "banned"},
                },
            )

        client = _make_client(settings, handler)
        resp = client.patch("/api/admin/keys/volc_001", json={"status": "banned"})

        assert resp.status_code == 200
        assert resp.json()["changed"] == ["status"]
        assert captured["method"] == "PATCH"
        assert captured["url"] == f"{settings.gateway_base_url}/admin/keys/volc_001"
        # 管理密钥由服务端注入
        assert captured["auth"] == f"Bearer {settings.gateway_admin_api_key}"
        assert captured["actor"] == "dashboard"

    def test_request_body_passed_through_verbatim(self, settings: Settings) -> None:
        """请求体原样透传，不反序列化再重新序列化。

        重新序列化会改变字段顺序与数值表示（大整数、浮点精度），
        让网关收到的内容与运维提交的不完全一致。
        """
        captured: dict[str, bytes] = {}

        def handler(request: httpx.Request) -> httpx.Response:
            captured["body"] = request.content
            return httpx.Response(200, json={"ok": True})

        client = _make_client(settings, handler)
        # 故意用非规范化的字段顺序与一个超出 float 精度的大整数
        raw = b'{"status":"banned","big":12345678901234567890,"reason":"x"}'
        client.request(
            "PATCH",
            "/api/admin/keys/volc_001",
            content=raw,
            headers={"Content-Type": "application/json"},
        )
        assert captured["body"] == raw, "请求体被改写了"

    def test_import_forwards_to_admin_keys(self, settings: Settings) -> None:
        captured: dict[str, str] = {}

        def handler(request: httpx.Request) -> httpx.Response:
            captured["url"] = str(request.url)
            return httpx.Response(200, json={"imported_count": 2, "failed_count": 0})

        client = _make_client(settings, handler)
        resp = client.post("/api/admin/keys/import", json=[{"key_id": "a"}, {"key_id": "b"}])
        assert resp.status_code == 200
        assert captured["url"] == f"{settings.gateway_base_url}/admin/keys"

    def test_rebind_ip_forwards_with_put(self, settings: Settings) -> None:
        captured: dict[str, str] = {}

        def handler(request: httpx.Request) -> httpx.Response:
            captured["method"] = request.method
            captured["url"] = str(request.url)
            return httpx.Response(200, json={"switch_status": "completed"})

        client = _make_client(settings, handler)
        resp = client.put("/api/admin/keys/volc_001/ip", json={})
        assert resp.status_code == 200
        assert captured["method"] == "PUT"
        assert captured["url"] == f"{settings.gateway_base_url}/admin/keys/volc_001/ip"

    def test_import_200_status_preserved(self, settings: Settings) -> None:
        """网关的 400（全部导入失败）应原样透传，便于脚本判断。"""

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(400, json={"imported_count": 0, "failed_count": 3})

        client = _make_client(settings, handler)
        resp = client.post("/api/admin/keys/import", json=[{"key_id": ""}])
        assert resp.status_code == 400


# ---------------------------------------------------------------- 错误映射


class TestErrorMapping:
    def test_401_becomes_500_not_passed_through(self, settings: Settings) -> None:
        """这是本文件最重要的一条。

        网关返回 401 意味着看板持有的 ADMIN_API_KEY 配错了，属看板配置错误。
        透传 401 会让前端误判为「会话过期」并跳登录页，运维于是陷入
        「反复登录仍失败」的死循环，而真正该做的是核对两侧的密钥。
        """

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(401, json=_gw_error("管理密钥无效", "invalid_auth"))

        client = _make_client(settings, handler)
        resp = client.patch("/api/admin/keys/volc_001", json={"status": "banned"})

        assert resp.status_code == 500, "401 被透传了，前端会误判为会话过期"
        detail = resp.json()["detail"]
        # 文案必须指向真正的处置动作
        assert "GATEWAY_ADMIN_API_KEY" in detail or "管理密钥" in detail

    def test_405_becomes_500(self, settings: Settings) -> None:
        """看板发错方法属看板 bug，不该让用户看到 405。"""

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(405, json=_gw_error("该端点只接受 PUT"))

        client = _make_client(settings, handler)
        resp = client.patch("/api/admin/keys/volc_001", json={"status": "banned"})
        assert resp.status_code == 500

    @pytest.mark.parametrize("status", [400, 404, 409])
    def test_business_errors_passed_through(self, settings: Settings, status: int) -> None:
        """400/404/409 是调用方真正需要看到的业务错误。"""

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(status, json=_gw_error("状态机不允许该转换"))

        client = _make_client(settings, handler)
        resp = client.patch("/api/admin/keys/volc_001", json={"status": "active"})
        assert resp.status_code == status
        assert resp.json()["detail"] == "状态机不允许该转换"

    def test_503_passed_through(self, settings: Settings) -> None:
        """网关容量问题原样传。"""

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(503, json=_gw_error("出口池无可用 IP", "service_busy"))

        client = _make_client(settings, handler)
        resp = client.put("/api/admin/keys/volc_001/ip", json={})
        assert resp.status_code == 503

    def test_other_5xx_becomes_502(self, settings: Settings) -> None:
        """其余 5xx 收敛为 502：语义上看板是网关的代理。"""

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(500, json=_gw_error("数据库错误", "internal_error"))

        client = _make_client(settings, handler)
        resp = client.patch("/api/admin/keys/volc_001", json={"status": "banned"})
        assert resp.status_code == 502

    def test_error_translated_to_detail_shape(self, settings: Settings) -> None:
        """必须转成 {detail}，不能裸传网关的 {"error": {...}}。

        前端 fetchJSON 统一按 body.detail 取文案。
        """

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(409, json=_gw_error("当前状态是终态", "invalid_request"))

        client = _make_client(settings, handler)
        body = client.patch("/api/admin/keys/volc_001", json={"status": "active"}).json()

        assert "detail" in body
        assert "error" not in body
        # gateway_code 保留供排障，不参与前端展示决策
        assert body["gateway_code"] == "invalid_request"
        assert body["gateway_status"] == 409

    def test_unreachable_gateway_becomes_502(self, settings: Settings) -> None:
        def handler(request: httpx.Request) -> httpx.Response:
            raise httpx.ConnectError("connection refused")

        client = _make_client(settings, handler)
        resp = client.patch("/api/admin/keys/volc_001", json={"status": "banned"})
        assert resp.status_code == 502
        assert "gateway" in resp.json()["detail"] or "不可达" in resp.json()["detail"]

    def test_connect_timeout_becomes_504(self, settings: Settings) -> None:
        def handler(request: httpx.Request) -> httpx.Response:
            raise httpx.ConnectTimeout("timed out")

        client = _make_client(settings, handler)
        resp = client.patch("/api/admin/keys/volc_001", json={"status": "banned"})
        assert resp.status_code == 504

    def test_read_timeout_message_warns_against_retry(self, settings: Settings) -> None:
        """读超时文案必须引导「先刷新确认」而非「请重试」。

        POST /admin/keys 是逐条 upsert 且不是单个事务，超时时网关很可能已写入
        部分 Key。提示重试会让运维重复导入 —— 虽然 upsert 幂等，但会掩盖
        「上次到底成功了多少」，且第二次同样可能超时。
        """

        def handler(request: httpx.Request) -> httpx.Response:
            raise httpx.ReadTimeout("read timed out")

        client = _make_client(settings, handler)
        resp = client.post("/api/admin/keys/import", json=[{"key_id": "a"}])

        assert resp.status_code == 504
        detail = resp.json()["detail"]
        assert "刷新" in detail, f"文案未引导刷新确认: {detail}"
        assert "不要直接重试" in detail, f"文案未劝阻重试: {detail}"

    def test_non_json_error_body_still_handled(self, settings: Settings) -> None:
        """网关返回 HTML 或空体时不能把看板自己搞崩。"""

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(502, text="<html>bad gateway</html>")

        client = _make_client(settings, handler)
        resp = client.patch("/api/admin/keys/volc_001", json={"status": "banned"})
        assert resp.status_code == 502
        assert resp.json()["detail"]


# ---------------------------------------------------------------- 密钥不外泄


class TestAdminKeyNeverLeaks:
    def test_admin_key_absent_from_all_responses(self, settings: Settings) -> None:
        """ADMIN_API_KEY 绝不出现在任何响应体里。

        用 grep 式断言，与联调报告里「明文泄漏检查 = 0」的做法一致。
        """

        def handler(request: httpx.Request) -> httpx.Response:
            # 即便网关（错误地）把密钥回显了，看板也不应把它传出去
            return httpx.Response(
                401, json=_gw_error(f"密钥 {settings.gateway_admin_api_key} 无效")
            )

        client = _make_client(settings, handler)
        resp = client.patch("/api/admin/keys/volc_001", json={"status": "banned"})
        assert settings.gateway_admin_api_key not in resp.text

    def test_admin_key_absent_from_login_page(self, settings: Settings) -> None:
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(200, json={})

        client = _make_client(settings, handler)
        assert settings.gateway_admin_api_key not in client.get("/login").text

    def test_admin_key_absent_from_frontend_assets(self, settings: Settings) -> None:
        """前端产物里不应有管理密钥。"""

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(200, json={})

        client = _make_client(settings, handler)
        for path in ("/app.js", "/app.css", "/"):
            resp = client.get(path)
            if resp.status_code == 200:
                assert settings.gateway_admin_api_key not in resp.text

    def test_session_cookie_is_not_the_admin_key(self, settings: Settings) -> None:
        """会话令牌与管理密钥必须无关。"""
        signer = SessionSigner(settings.session_secret, settings.session_ttl)
        assert settings.gateway_admin_api_key not in signer.issue()


# ---------------------------------------------------------------- 请求体上限


class TestBodyLimit:
    def test_oversized_body_rejected_with_413(self, settings: Settings) -> None:
        """上限与网关的 8MB 对齐。

        不对齐的后果是看板放过一个更大的请求、转发到网关才被拒，
        白白耗费一次完整传输，而错误发生在第二跳会带偏排查方向。
        """
        forwarded = {"called": False}

        def handler(request: httpx.Request) -> httpx.Response:
            forwarded["called"] = True
            return httpx.Response(200, json={})

        client = _make_client(settings, handler)
        payload = json.dumps([{"key_id": "k", "secret": "x" * (9 << 20)}]).encode()
        resp = client.post(
            "/api/admin/keys/import",
            content=payload,
            headers={"Content-Type": "application/json"},
        )
        assert resp.status_code == 413
        assert "8MB" in resp.json()["detail"]
        assert not forwarded["called"], "超限请求仍被转发到网关"


# ---------------------------------------------------------------- 超时配置


class TestTimeouts:
    @pytest.mark.anyio
    async def test_import_uses_longer_read_timeout(self, settings: Settings) -> None:
        """导入端点的 read 超时必须显著长于其他端点。

        网关是逐条 upsert，1000 个 Key 需 20-50 秒；用默认 15 秒会让大批量
        导入在网关已经写入部分数据后被判定超时。
        """
        seen: list[float | None] = []

        def handler(request: httpx.Request) -> httpx.Response:
            timeout = request.extensions.get("timeout") or {}
            seen.append(timeout.get("read"))
            return httpx.Response(200, json={})

        gc = GatewayClient(
            settings.gateway_base_url,
            settings.gateway_admin_api_key,
            float(settings.gateway_timeout_import),
            client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
        )
        try:
            await gc.import_keys(b"[]", "dashboard")
            await gc.patch_key("volc_001", b"{}", "dashboard")
        finally:
            await gc.aclose()

        assert seen[0] == float(settings.gateway_timeout_import)
        assert seen[1] == 15.0
        assert seen[0] > seen[1]


def test_gateway_error_payload_omits_empty_fields() -> None:
    """没有网关码时不应塞空字段，避免前端拿到一堆无意义的 key。"""
    err = GatewayError(502, "网关不可达")
    assert err.to_payload() == {"detail": "网关不可达"}


# ==================== 出口 IP 状态的数据来源 ====================
#
# 出口状态（state / reputation / pool / bound_keys）的唯一事实来源是网关内存，
# 不是 egress_ips 表 —— 那张表在网关代码里从未被写入，读它会把一个已封禁的
# 出口显示成 active/信誉 100，运维据此判断会直接出错。
#
# 这几个用例锁的就是这条链路: 网关可达时用它的值，不可达时**明确降级**而非
# 静默展示旧值或空值。


def _egress_payload(*items: dict[str, object]) -> dict[str, object]:
    """构造网关 GET /admin/ips 的响应。字段名与 egress.Stats 一致。"""
    return {
        "mode": "multi_ip",
        "total": len(items),
        "keys_bound": sum(int(i.get("bound_keys", 0) or 0) for i in items),
        "per_ip": list(items),
    }


def test_出口状态取自网关而非库(settings: Settings) -> None:
    """banned 状态必须如实呈现。

    这是整条改动的核心: 旧实现从 egress_ips 表读，那里永远是 active。
    """
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path == "/admin/ips"
        return httpx.Response(
            200,
            json=_egress_payload(
                {
                    "addr": "172.16.0.11",
                    "public_ip": "203.0.113.11",
                    "state": "banned",
                    "reputation": 40,
                    "bound_keys": 8,
                    "max_keys": 10,
                    "pool": "hot",
                },
                {
                    "addr": "172.16.0.12",
                    "public_ip": "203.0.113.12",
                    "state": "active",
                    "reputation": 100,
                    "bound_keys": 3,
                    "max_keys": 10,
                    "pool": "hot",
                },
            ),
        )

    client = _make_client(settings, handler)
    try:
        resp = client.get("/api/egress")
        assert resp.status_code == 200, resp.text
        body = resp.json()

        assert body["stale"] is False
        assert body["stale_reason"] == ""
        assert body["total"] == 2

        by_addr = {r["addr"]: r for r in body["items"]}
        banned = by_addr["172.16.0.11"]
        assert banned["state"] == "banned", "封禁状态被吞掉了 —— 运维会把已封出口当健康的用"
        assert banned["reputation"] == 40
        assert banned["pool"] == "hot"
        assert banned["bound_keys"] == 8
        # 负载率由 bound/max 算出，供前端标红
        assert 0.79 < banned["load_ratio"] < 0.81
    finally:
        app.dependency_overrides.clear()


def test_网关不可达时明确降级(settings: Settings) -> None:
    """拿不到实时状态时必须置 stale 并给出原因。

    静默返回空表或旧值都不可接受: 前者让运维以为「没有出口」，
    后者让他以为「一切正常」，两种误判都会延误处置。
    """
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(503, json=_gw_error("upstream unavailable"))

    client = _make_client(settings, handler)
    try:
        resp = client.get("/api/egress")
        # 出口面板降级不应让整个接口 5xx —— 其余字段仍有价值
        assert resp.status_code == 200, resp.text
        body = resp.json()

        assert body["stale"] is True
        assert body["stale_reason"], "降级必须给出原因，否则前端只能显示一张空表"
        assert body["items"] == []
        # quota_day 这类不依赖网关的字段仍应可用
        assert body["quota_day"]
    finally:
        app.dependency_overrides.clear()


def test_网关返回结构异常时降级(settings: Settings) -> None:
    """per_ip 缺失或类型不对时按降级处理，不得抛 500。"""
    for payload in ({"mode": "multi_ip"}, {"per_ip": "not-a-list"}, []):
        def handler(request: httpx.Request, p: object = payload) -> httpx.Response:
            return httpx.Response(200, json=p)

        client = _make_client(settings, handler)
        try:
            resp = client.get("/api/egress")
            assert resp.status_code == 200, resp.text
            body = resp.json()
            assert body["stale"] is True, f"payload={payload!r} 应判为降级"
            assert body["items"] == []
        finally:
            app.dependency_overrides.clear()


def test_出口缺少addr的条目被跳过(settings: Settings) -> None:
    """addr 是行标识，缺失的条目无法定位到具体出口，跳过而非渲染空行。"""
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json=_egress_payload(
                {"addr": "", "state": "active", "reputation": 100},
                {"addr": "172.16.0.11", "state": "active", "reputation": 100,
                 "bound_keys": 1, "max_keys": 10, "pool": "hot"},
            ),
        )

    client = _make_client(settings, handler)
    try:
        body = client.get("/api/egress").json()
        assert [r["addr"] for r in body["items"]] == ["172.16.0.11"]
        assert body["total"] == 1
    finally:
        app.dependency_overrides.clear()
