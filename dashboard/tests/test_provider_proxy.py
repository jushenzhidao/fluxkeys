"""provider 配置转发层测试。

沿用 ``test_gateway_proxy.py`` 的 MockTransport 手法（见那边的模块 docstring）：
断言的是**看板如何转发与解释**，逐条编程的响应比真网关更能覆盖 422/501 这些
分支。

覆盖重点，按重要性排序：

1. **``error.code`` 与结构化字段完整透传** —— 前端靠 ``gateway_code`` 分派文案，
   靠 ``failures`` 定位到具体字段。压平成一句 detail 等于让界面只能说「失败」；
2. 转发路径与方法与网关实际路由一致（不是与设计文档一致，两者有实质差异）；
3. actor 一律 ``dashboard``，管理密钥由服务端注入；
4. 413 限长与 Key 那侧同一上限；
5. **不在 Python 侧做业务判定** —— 网关说合法就合法。
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


def _make_client(
    settings: Settings,
    handler: Callable[[httpx.Request], httpx.Response],
) -> TestClient:
    """构造一个转发到 MockTransport 的已登录客户端。"""
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


@pytest.fixture(autouse=True)
def _cleanup() -> Iterator[None]:
    yield
    app.dependency_overrides.clear()
    if hasattr(app.state, "gateway_client"):
        delattr(app.state, "gateway_client")


def _capture(
    status: int = 200, payload: object | None = None
) -> tuple[dict[str, object], Callable[[httpx.Request], httpx.Response]]:
    """返回 (捕获字典, handler)。"""
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        # raw_path 是真正发到网关的字节；path 已被 httpx 解码，断不出编码问题。
        seen["raw_path"] = request.url.raw_path.split(b"?")[0]
        seen["query"] = str(request.url.query.decode())
        seen["actor"] = request.headers.get("x-admin-actor")
        seen["auth"] = request.headers.get("authorization")
        seen["body"] = request.content
        return httpx.Response(status, json=payload if payload is not None else {"ok": True})

    return seen, handler


# ============================================================ 转发路径与方法
#
# 路径取自 internal/gateway/admin_provider.go:40-51 的实际注册，
# **不是** docs/provider-config-hotreload.md §4.1 的清单 —— 两者有实质差异
# （文档写 PATCH/enable/disable/config-versions，网关实现的是
# PUT/DELETE/providers-scoped versions）。按文档写会得到一组 404。


class TestForwardRouting:
    def test_list_forwards_to_admin_providers(self, settings: Settings) -> None:
        seen, handler = _capture(payload={"providers": [], "count": 0})
        resp = _make_client(settings, handler).get("/api/admin/providers")

        assert resp.status_code == 200
        assert seen["method"] == "GET"
        assert seen["path"] == "/admin/providers"

    def test_create_forwards_with_post(self, settings: Settings) -> None:
        seen, handler = _capture(status=201, payload={"version": 3})
        resp = _make_client(settings, handler).post(
            "/api/admin/providers", json={"name": "moonshot"}
        )

        assert resp.status_code == 201, "网关的 201 被改写了"
        assert seen["method"] == "POST"
        assert seen["path"] == "/admin/providers"

    def test_get_detail_forwards_with_name(self, settings: Settings) -> None:
        seen, handler = _capture(payload={"provider": {"name": "volc"}})
        _make_client(settings, handler).get("/api/admin/providers/volc")

        assert seen["method"] == "GET"
        assert seen["path"] == "/admin/providers/volc"

    def test_update_uses_put_not_patch(self, settings: Settings) -> None:
        """网关侧是 PUT 全量替换（admin_provider.go:44）。

        转成 PATCH 会 404，而 404 出现在第二跳 —— 前端只会显示
        「目标不存在，列表已过期」，完全指不到「方法不对」这个真实原因。
        """
        seen, handler = _capture()
        _make_client(settings, handler).put("/api/admin/providers/volc", json={"quota_limit": 2000})

        assert seen["method"] == "PUT"
        assert seen["path"] == "/admin/providers/volc"

    def test_delete_forwards_with_delete(self, settings: Settings) -> None:
        seen, handler = _capture(payload={"soft_delete": True})
        _make_client(settings, handler).request(
            "DELETE", "/api/admin/providers/volc", json={"reason": "下线"}
        )

        assert seen["method"] == "DELETE"
        assert seen["path"] == "/admin/providers/volc"

    def test_versions_forwards_under_provider(self, settings: Settings) -> None:
        seen, handler = _capture(payload={"versions": []})
        _make_client(settings, handler).get("/api/admin/providers/volc/versions")

        assert seen["path"] == "/admin/providers/volc/versions"

    def test_version_detail_forwards_id(self, settings: Settings) -> None:
        seen, handler = _capture(payload={"version": {}})
        _make_client(settings, handler).get("/api/admin/providers/volc/versions/42")

        assert seen["path"] == "/admin/providers/volc/versions/42"

    def test_rollback_and_dryrun_paths(self, settings: Settings) -> None:
        seen, handler = _capture()
        client = _make_client(settings, handler)

        client.post("/api/admin/providers/volc/rollback", json={"target_version_id": 4})
        assert seen["path"] == "/admin/providers/volc/rollback"

        client.post("/api/admin/providers/volc/dry-run", json={"quota_limit": 1})
        assert seen["path"] == "/admin/providers/volc/dry-run"

    def test_reload_config_is_not_under_providers(self, settings: Settings) -> None:
        """网关的重载端点是 /admin/reload-config，不在 providers 之下。"""
        seen, handler = _capture(payload={"reloaded": True, "active_version": 7})
        resp = _make_client(settings, handler).post("/api/admin/reload-config", json={})

        assert resp.status_code == 200
        assert seen["path"] == "/admin/reload-config"

    def test_versions_query_passed_through(self, settings: Settings) -> None:
        """limit / before 原样透传，看板不解析重组。

        网关已对两者做正整数校验并各有文案（admin_provider.go:382-398）。
        """
        seen, handler = _capture(payload={"versions": []})
        _make_client(settings, handler).get("/api/admin/providers/volc/versions?limit=10&before=99")

        assert seen["query"] == "limit=10&before=99"

    def test_provider_name_url_encoded(self, settings: Settings) -> None:
        """名字里的特殊字符必须编码，不能改变转发 URL 的解析。

        断言 ``raw_path`` 而非 ``path``：后者是 httpx 解码后的视图，``a%23b``
        与真正危险的裸 ``a#b`` 在那里长得一模一样，断不出区别。线上真正发出去
        的是 ``raw_path``，未编码的 ``#`` 会让路径其余部分变成 fragment 而被
        整段丢弃。
        """
        seen, handler = _capture()
        _make_client(settings, handler).get("/api/admin/providers/a%23b")

        assert seen["raw_path"] == b"/admin/providers/a%23b"
        assert seen["query"] == ""

    def test_query_injection_via_name_blocked(self, settings: Settings) -> None:
        """名字里的 ``?`` 不能凭空造出查询参数。"""
        seen, handler = _capture()
        _make_client(settings, handler).get("/api/admin/providers/x%3Flimit%3D9999")

        assert seen["query"] == "", "provider 名注入出了查询参数"
        assert seen["raw_path"] == b"/admin/providers/x%3Flimit%3D9999"


# ============================================================ 审计身份与密钥


class TestActorAndKey:
    def test_actor_is_dashboard_and_key_injected(self, settings: Settings) -> None:
        seen, handler = _capture()
        _make_client(settings, handler).put("/api/admin/providers/volc", json={})

        assert seen["actor"] == "dashboard"
        assert seen["auth"] == f"Bearer {settings.gateway_admin_api_key}"

    def test_admin_key_absent_from_provider_responses(self, settings: Settings) -> None:
        """即便网关回显了密钥，看板也不得传出去。"""

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(
                422,
                json={
                    "error": {
                        "message": f"密钥 {settings.gateway_admin_api_key} 无效",
                        "type": "invalid_request_error",
                        "code": "quota_kind_immutable",
                    }
                },
            )

        resp = _make_client(settings, handler).put("/api/admin/providers/volc", json={})
        assert settings.gateway_admin_api_key not in resp.text

    def test_body_passed_through_verbatim(self, settings: Settings) -> None:
        """请求体原样透传，不反序列化再重新序列化。"""
        seen, handler = _capture()
        raw = b'{"quota_limit":12345678901234567890,"name":"volc"}'
        _make_client(settings, handler).request(
            "PUT",
            "/api/admin/providers/volc",
            content=raw,
            headers={"Content-Type": "application/json"},
        )
        assert seen["body"] == raw, "请求体被改写了"


# ================================================ capabilities 与 default-provider
#
# 这两条是后补的端点。capabilities 的风险不在转发本身（它没有请求体、没有
# 路径参数），而在**注册顺序**：它与 /{name} 争夺同一个位置。


class TestCapabilities:
    def test_forwards_to_capabilities_path(self, settings: Settings) -> None:
        seen, handler = _capture(payload={"supported_providers": ["volc", "sensenova"]})
        resp = _make_client(settings, handler).get("/api/admin/providers/capabilities")

        assert resp.status_code == 200
        assert seen["method"] == "GET"
        assert seen["path"] == "/admin/providers/capabilities"
        assert resp.json()["supported_providers"] == ["volc", "sensenova"]

    def test_capabilities_handled_by_its_own_endpoint(self) -> None:
        """/capabilities 必须由 get_provider_capabilities 处理，不是 get_provider。

        为什么不能靠断言转发路径来测这件事：路由被 ``/{name}`` 吞掉时，
        ``get_provider(name="capabilities")`` 拼出的转发路径是
        ``/admin/providers/capabilities`` —— 与正确实现**逐字节相同**。mock 那侧
        看到的请求完全一样，任何基于「handler 收到什么」的断言都会照样通过。

        （这是实测结论：本条测试的前一版就是那么写的，把 capabilities 挪到
        ``/{name}`` 之后跑，它依然绿。）

        真正能区分的是「哪个 endpoint 函数会处理这个路径」。直接按 Starlette 的
        匹配规则问路由表，不发请求 —— 加中间件那条路走不通：``app`` 是模块级
        单例，本文件更早的测试已经启动过它，此时 ``@app.middleware`` 会抛
        ``Cannot add middleware after an application has started``。
        """
        from starlette.routing import Match

        from app.api.provider_proxy import get_provider_capabilities

        scope = {
            "type": "http",
            "method": "GET",
            "path": "/api/admin/providers/capabilities",
            "path_params": {},
            "headers": [],
        }
        # 按注册顺序取第一个完全匹配的，这就是 Starlette 实际会选中的那条。
        winner = next(
            (
                r
                for r in app.routes
                if r.matches(scope)[0] == Match.FULL  # type: ignore[attr-defined]
            ),
            None,
        )
        endpoint = getattr(winner, "endpoint", None)

        assert endpoint is get_provider_capabilities, (
            f"/capabilities 被 /{{name}} 吞掉了，实际会由 {getattr(endpoint, '__name__', '?')} 处理"
        )

    def test_capabilities_route_registered_before_name_route(self) -> None:
        """直接断言注册顺序，不依赖请求行为。

        上一条测的是可观察行为，这一条测的是成因。两条都留着：行为测试在
        路由被吞掉时才失败，而顺序断言在有人挪动函数位置时立刻失败，
        且失败信息直接指向原因。
        """
        from app.api import provider_proxy

        # router.routes 里的 path 已带上 prefix，断言用完整路径。
        paths = [getattr(r, "path", "") for r in provider_proxy.router.routes]
        cap = "/api/admin/providers/capabilities"
        name = "/api/admin/providers/{name}"
        assert cap in paths, f"capabilities 路由未注册，当前: {paths}"
        assert paths.index(cap) < paths.index(name), (
            f"capabilities 必须注册在 /{{name}} 之前，当前顺序: {paths}"
        )

    def test_capabilities_sends_no_body(self, settings: Settings) -> None:
        seen, handler = _capture(payload={"supported_providers": []})
        _make_client(settings, handler).get("/api/admin/providers/capabilities")

        assert seen["body"] == b""
        assert seen["actor"] == "dashboard"

    def test_capabilities_5xx_uses_same_error_path(self, settings: Settings) -> None:
        """capabilities 的错误走同一条 error_envelope，不另写一套。

        它也可能返回 501（部署未启用配置管理），此时 error.code 必须保留 ——
        前端靠它区分「这个部署没这功能」与「网关挂了」。
        """
        handler = lambda r: httpx.Response(  # noqa: E731
            501, json={"error": {"message": "当前部署未启用配置管理", "code": "not_implemented"}}
        )
        resp = _make_client(settings, handler).get("/api/admin/providers/capabilities")

        assert resp.status_code == 501, "501 被收敛了"
        assert resp.json()["gateway_code"] == "not_implemented"


class TestDefaultProvider:
    def test_forwards_to_config_default_provider(self, settings: Settings) -> None:
        """路径在 /admin/config 下，不在 /admin/providers 下（文档 §4.1.2）。"""
        seen, handler = _capture(payload={"active_version": 129})
        resp = _make_client(settings, handler).patch(
            "/api/admin/config/default-provider",
            json={"expected_version": 128, "name": "sensenova", "reason": "切默认"},
        )

        assert resp.status_code == 200
        assert seen["method"] == "PATCH", "网关侧是 PATCH（§4.1.2）"
        assert seen["path"] == "/admin/config/default-provider"

    def test_body_passed_through_verbatim(self, settings: Settings) -> None:
        seen, handler = _capture()
        raw = b'{"expected_version":128,"name":"sensenova","reason":"x"}'
        _make_client(settings, handler).request(
            "PATCH",
            "/api/admin/config/default-provider",
            content=raw,
            headers={"Content-Type": "application/json"},
        )
        assert seen["body"] == raw, "请求体被改写了"
        assert seen["actor"] == "dashboard"

    def test_no_python_side_enabled_check(self, settings: Settings) -> None:
        """看板不检查目标 provider 是否存在或 enabled —— 那是网关的判断。

        断言方式：请求一个明显不存在的 provider 名，确认它**照样被转发**而不是
        在看板侧被拦掉。看板拦掉的话，网关那套「目标必须 enabled」的规则就有了
        第二个实现，而这一份读到的是另一个时点的快照。
        """
        seen, handler = _capture()
        _make_client(settings, handler).patch(
            "/api/admin/config/default-provider",
            json={"expected_version": 1, "name": "does-not-exist-at-all"},
        )
        assert seen["path"] == "/admin/config/default-provider", "请求没被转发出去"

    def test_invalid_state_transition_code_survives(self, settings: Settings) -> None:
        """目标未 enabled 时网关返回 409 + invalid_state_transition（§4.1.2 规则 2）。

        与 version_conflict 同为 409，前端靠 code 区分：前者要先启用目标，
        后者刷新重试即可。压平成一句 detail 等于让运维两条路都试一遍。
        """
        handler = lambda r: httpx.Response(  # noqa: E731
            409,
            json={
                "error": {
                    "message": "目标 provider 未启用，请先启用后再设为默认",
                    "code": "invalid_state_transition",
                }
            },
        )
        resp = _make_client(settings, handler).patch(
            "/api/admin/config/default-provider", json={"expected_version": 1, "name": "volc"}
        )

        assert resp.status_code == 409
        assert resp.json()["gateway_code"] == "invalid_state_transition"

    def test_version_conflict_carries_current_version(self, settings: Settings) -> None:
        """乐观锁冲突时 current_version 必须透传，前端用它写「当前已是 #N」。"""
        handler = lambda r: httpx.Response(  # noqa: E731
            409,
            json={
                "error": {"message": "配置已被他人修改", "code": "version_conflict"},
                "current_version": 130,
                "expected_version": 128,
            },
        )
        resp = _make_client(settings, handler).patch(
            "/api/admin/config/default-provider", json={"expected_version": 128, "name": "volc"}
        )

        body = resp.json()
        assert body["gateway_code"] == "version_conflict"
        assert body["current_version"] == 130

    def test_oversized_body_rejected(self, settings: Settings) -> None:
        """与 Key 那侧同一个 8MB 上限，不在本端点另设一个。"""
        seen, handler = _capture()
        resp = _make_client(settings, handler).request(
            "PATCH",
            "/api/admin/config/default-provider",
            content=b"x" * (8 * 1024 * 1024 + 1),
            headers={"Content-Type": "application/json"},
        )

        assert resp.status_code == 413
        assert "path" not in seen, "超限请求仍被转发到了网关"
