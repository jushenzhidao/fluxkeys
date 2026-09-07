"""provider 端点的错误码与结构化字段透传。

**这是本轮改动最要紧的一组断言。**

网关用 ``error.code`` 区分处置动作完全不同的几类拒绝：``version_conflict``
是「别人先改了，刷新重试即可」，``quota_kind_immutable`` 是「重试一万次也不会
成功，去新建 provider」。两者若都退化成一句「冲突」，运维会反复重试一个注定
失败的动作，每次都得到同一句话 —— 这正是转发层最容易悄悄造成的损害：
没有任何一层报错，但界面从此给不出正确的下一步。

同理，``failures`` / ``diff`` 与 ``error`` 信封**同级**而非嵌在里面
（``writeProviderInvalid``，admin_provider.go:686-702）。只解析 ``error`` 会把
它们整体丢掉，前端就只剩「校验未通过」，而运维需要的恰好是「哪个字段、为什么」。
"""

from __future__ import annotations

from collections.abc import Callable, Iterator

import httpx
import pytest
from fastapi.testclient import TestClient

from app.api.deps import get_conf, get_service
from app.config import Settings
from app.gateway import GatewayClient
from app.main import app
from app.security import COOKIE_NAME, SessionSigner
from app.service import ReportService
from tests.test_api import EmptyDatabase, FakeCache


def _client_for(
    settings: Settings, handler: Callable[[httpx.Request], httpx.Response]
) -> TestClient:
    gateway = GatewayClient(
        settings.gateway_base_url,
        settings.gateway_admin_api_key,
        float(settings.gateway_timeout_import),
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
    )
    app.dependency_overrides[get_service] = lambda: ReportService(
        EmptyDatabase(settings), FakeCache(settings), settings, gateway
    )
    app.dependency_overrides[get_conf] = lambda: settings
    signer = SessionSigner(settings.session_secret, settings.session_ttl)
    app.state.settings = settings
    app.state.session_signer = signer
    app.state.gateway_client = gateway
    client = TestClient(app)
    client.cookies.set(COOKIE_NAME, signer.issue())
    return client


def _err(status: int, code: str, message: str, **extra: object) -> Callable[..., httpx.Response]:
    def handler(request: httpx.Request) -> httpx.Response:
        body: dict[str, object] = {
            "error": {"message": message, "type": "invalid_request_error", "code": code}
        }
        body.update(extra)
        return httpx.Response(status, json=body)

    return handler


@pytest.fixture(autouse=True)
def _cleanup() -> Iterator[None]:
    yield
    app.dependency_overrides.clear()
    if hasattr(app.state, "gateway_client"):
        delattr(app.state, "gateway_client")


# ============================================================ error.code 分派


class TestErrorCodePassthrough:
    """每个 code 都必须原样到达前端。"""

    # (状态码, code) 取自网关实际实现（admin_provider.go:86-105、:708-721）。
    #
    # 注意 quota_kind_immutable 是 422 而非文档 §4.1.1 写的 409 —— 网关刻意用
    # 不同状态码把「刷新重试」与「永久失败」分开。这里按实现断言而非按文档，
    # 因为实现是唯一能被验证的一侧。
    @pytest.mark.parametrize(
        ("status", "code"),
        [
            (409, "version_conflict"),
            (409, "provider_exists"),
            (422, "quota_kind_immutable"),
            (400, "name_immutable"),
            (400, "provider_invalid"),
            (404, "provider_not_found"),
            (404, "version_not_found"),
            (501, "not_implemented"),
            (503, "reload_failed"),
        ],
    )
    def test_code_reaches_frontend(self, settings: Settings, status: int, code: str) -> None:
        client = _client_for(settings, _err(status, code, "网关的原始文案"))
        resp = client.put("/api/admin/providers/volc", json={"quota_limit": 1})

        assert resp.status_code == status, f"{code} 的状态码被改写了"
        body = resp.json()
        assert body["gateway_code"] == code, (
            f"{code} 丢了 —— 前端只能按状态码猜，而同一状态码下处置动作不同"
        )
        assert body["detail"] == "网关的原始文案"
        # 转成 {detail} 结构，不裸传网关信封
        assert "error" not in body

    def test_two_409s_are_distinguishable(self, settings: Settings) -> None:
        """同为 409 的两种拒绝必须能被区分。

        这条是整组测试的核心：若只按状态码分派，界面对
        「刷新重试就好」与「名字已被占用，得换个名」只能给同一句话。
        """
        # 两个客户端必须依次构造并各自立即发请求：_client_for 会写同一个
        # app.state.gateway_client，先建两个再发请求的话第二个会覆盖第一个，
        # 于是两次请求都走后建的那个 handler，测试变成自证。
        a = (
            _client_for(settings, _err(409, "version_conflict", "配置已被他人修改"))
            .put("/api/admin/providers/volc", json={})
            .json()
        )
        b = (
            _client_for(settings, _err(409, "provider_exists", "provider 名已被占用"))
            .post("/api/admin/providers", json={"name": "volc"})
            .json()
        )

        assert a["gateway_code"] != b["gateway_code"]
        assert a["gateway_code"] == "version_conflict"
        assert b["gateway_code"] == "provider_exists"

    def test_422_not_collapsed_into_502(self, settings: Settings) -> None:
        """跨量纲被拒是 422，不能被收敛成 502。

        收敛后运维看到「网关返回异常」，会去查网关或重启容器，
        而真正该做的是新建一个 provider —— 方向完全反了。
        """
        client = _client_for(settings, _err(422, "quota_kind_immutable", "quota_kind 不可修改"))
        resp = client.put("/api/admin/providers/volc", json={"quota_kind": "token"})

        assert resp.status_code == 422, "422 被收敛成 502 了"
        assert resp.json()["gateway_code"] == "quota_kind_immutable"

    def test_501_not_collapsed_into_502(self, settings: Settings) -> None:
        """未启用配置管理是 501，压成 502 会让运维去重启一个正常的容器。"""
        client = _client_for(settings, _err(501, "not_implemented", "当前部署未启用配置管理"))
        resp = client.get("/api/admin/providers")

        assert resp.status_code == 501
        assert resp.json()["gateway_code"] == "not_implemented"


# ============================================================ 结构化字段


class TestStructuredFieldsPassthrough:
    def test_failures_list_survives(self, settings: Settings) -> None:
        """逐字段问题清单必须完整到达前端。

        网关刻意一次返回全部失败项（避免运维改一个提交一次），
        转发层把它丢掉就等于取消了这个设计。
        """
        failures = [
            {"field": "quota_limit", "code": "must_be_positive", "message": "必须为正整数"},
            {
                "field": "model_mapping.deepseek-v3",
                "code": "empty_upstream_model",
                "message": "上游模型名不能为空",
            },
        ]
        client = _client_for(
            settings,
            _err(400, "provider_invalid", "provider 配置校验未通过", failures=failures),
        )
        body = client.post("/api/admin/providers", json={"name": "x"}).json()

        assert body["failures"] == failures, "逐字段清单被丢弃，界面只能说「校验失败」"
        # 两条都在，不是只留第一条
        assert len(body["failures"]) == 2

    def test_diff_survives_on_immutable_conflict(self, settings: Settings) -> None:
        """跨量纲被拒时的字段级差异必须保留。

        少了它，运维看不到「当前 count、目标 token」这个关键事实 ——
        而那正是他判断下一步的依据。
        """
        diff = [{"field": "quota_kind", "before": "count", "after": "token"}]
        client = _client_for(
            settings,
            _err(422, "quota_kind_immutable", "quota_kind 不可修改", diff=diff),
        )
        body = client.put("/api/admin/providers/volc", json={"quota_kind": "token"}).json()

        assert body["diff"] == diff

    def test_warnings_survive(self, settings: Settings) -> None:
        warnings = [{"field": "quota_limit", "code": "limit_below_peak", "message": "低于峰值"}]
        client = _client_for(
            settings,
            _err(400, "provider_invalid", "校验未通过", failures=[], warnings=warnings),
        )
        body = client.post("/api/admin/providers", json={}).json()

        assert body["warnings"] == warnings

    def test_unknown_fields_not_leaked(self, settings: Settings) -> None:
        """白名单之外的字段不透传。

        「除 error 外全带上」会把网关将来新增的任何字段自动暴露到浏览器，
        包括可能含内部细节的调试字段。
        """
        client = _client_for(
            settings,
            _err(
                500,
                "internal_error",
                "数据库错误",
                stack_trace="internal detail",
                dsn="postgres://user:pw@host/db",
            ),
        )
        body = client.put("/api/admin/providers/volc", json={}).json()

        assert "stack_trace" not in body
        assert "dsn" not in body

    def test_extras_kept_when_5xx_collapses_to_502(self, settings: Settings) -> None:
        """收敛成 502 时仍带上 code，供排障定位。"""
        client = _client_for(settings, _err(500, "internal_error", "创建 provider 失败"))
        resp = client.post("/api/admin/providers", json={})

        assert resp.status_code == 502
        assert resp.json()["gateway_code"] == "internal_error"
        assert resp.json()["gateway_status"] == 500


# ============================================================ 既有行为不回归
#
# error.code 的改动动了 _interpret 与 GatewayError，那是 Key 端点共用的代码。
# 这几条锁住 keys 那侧的既有语义没被顺手改掉。


class TestKeysBehaviorUnchanged:
    def test_401_still_becomes_500(self, settings: Settings) -> None:
        """401 转 500 这条不能因为新增透传而破功。"""
        client = _client_for(settings, _err(401, "invalid_auth", "管理密钥无效"))
        resp = client.patch("/api/admin/keys/volc_001", json={"status": "banned"})

        assert resp.status_code == 500, "401 被透传了，前端会误判为会话过期"

    def test_405_still_becomes_500(self, settings: Settings) -> None:
        client = _client_for(settings, _err(405, "invalid_request", "只接受 PUT"))
        resp = client.patch("/api/admin/keys/volc_001", json={})

        assert resp.status_code == 500

    def test_no_extras_means_no_extra_keys(self, settings: Settings) -> None:
        """网关没给结构化字段时不塞空字段。

        塞空数组会让前端拿到一个「有 failures 但是空的」响应，
        进而渲染出一个空的问题列表。
        """
        client = _client_for(settings, _err(409, "version_conflict", "冲突"))
        body = client.patch("/api/admin/keys/volc_001", json={}).json()

        assert set(body) == {"detail", "gateway_code", "gateway_status"}


# ============================================================ 限长与降级


class TestBodyLimitAndDegradation:
    def test_oversized_provider_body_rejected_with_413(self, settings: Settings) -> None:
        """上限与 Key 那侧同一个 8MB，不转发。"""
        forwarded = {"called": False}

        def handler(request: httpx.Request) -> httpx.Response:
            forwarded["called"] = True
            return httpx.Response(200, json={})

        client = _client_for(settings, handler)
        resp = client.request(
            "PUT",
            "/api/admin/providers/volc",
            content=b'{"pad":"' + b"x" * (9 << 20) + b'"}',
            headers={"Content-Type": "application/json"},
        )

        assert resp.status_code == 413
        assert "8MB" in resp.json()["detail"]
        assert not forwarded["called"], "超限请求仍被转发到网关"

    def test_missing_client_gives_503_not_bare_500(self, settings: Settings) -> None:
        """转发客户端缺失时是 503 且有说明，不是裸 500。"""
        client = _client_for(settings, lambda r: httpx.Response(200, json={}))
        delattr(app.state, "gateway_client")

        resp = client.get("/api/admin/providers")
        assert resp.status_code == 503
        assert "转发客户端" in resp.json()["detail"]

    def test_dry_run_valid_false_stays_200(self, settings: Settings) -> None:
        """预演的「校验不通过」是 200 + valid:false，看板不得改写成 4xx。

        改写会让前端无法区分「预演跑完了，结果不合格」与「预演本身没跑起来」。
        """

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(
                200,
                json={
                    "valid": False,
                    "failures": [{"field": "quota_limit", "code": "must_be_positive"}],
                    "applied": False,
                },
            )

        resp = _client_for(settings, handler).post(
            "/api/admin/providers/volc/dry-run", json={"quota_limit": 0}
        )

        assert resp.status_code == 200, "valid:false 被改写成了错误状态码"
        assert resp.json()["valid"] is False
        assert resp.json()["failures"]

    def test_read_timeout_warns_against_retry(self, settings: Settings) -> None:
        """写操作超时的文案必须引导「先刷新确认」而非「请重试」。

        配置写入后还要热加载，超时时很可能已落库。
        """

        def handler(request: httpx.Request) -> httpx.Response:
            raise httpx.ReadTimeout("read timed out")

        resp = _client_for(settings, handler).put("/api/admin/providers/volc", json={})

        assert resp.status_code == 504
        detail = resp.json()["detail"]
        assert "刷新" in detail
        assert "不要直接重试" in detail
