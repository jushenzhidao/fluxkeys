"""会话鉴权、登录限流与 CSRF 的测试。

重点不在「正确口令能登录」，而在几类会静默失效的行为：

1. 豁免清单漏掉 FastAPI 自动挂载的路由 —— 它们会暴露管理端点的请求体结构；
2. 鉴权组件缺失时中间件放行 —— 「没装好就等于不鉴权」是最危险的降级方向；
3. 令牌校验的顺序与边界 —— 先解析后验签等于把 json.loads 暴露给未验证输入；
4. 限流的键取自可伪造的头 —— 那样限流形同虚设。
"""

from __future__ import annotations

import json
import time
from collections.abc import Iterator

import pytest
from fastapi.testclient import TestClient

from app.api.deps import get_conf, get_service
from app.cache import Cache
from app.config import ConfigError, Settings
from app.db import Database
from app.main import app
from app.security import (
    COOKIE_NAME,
    EXEMPT_PATHS,
    LoginRateLimiter,
    PasswordChecker,
    SessionSigner,
)
from app.security.session import TOKEN_VERSION
from app.service import ReportService
from tests.test_api import EmptyDatabase, FakeCache

# 所有需要鉴权的业务报表端点。
PROTECTED_ENDPOINTS = [
    "/api/overview",
    "/api/keys",
    "/api/usage/trend",
    "/api/usage/by-user",
    "/api/errors",
    "/api/egress",
    "/api/quota/health",
    "/api/refresh/status",
    "/api/behavior/similarity",
]


def _build_client(settings: Settings, *, with_auth_state: bool = True) -> TestClient:
    """构造未携带任何 Cookie 的客户端。"""
    database: Database = EmptyDatabase(settings)
    cache: Cache = FakeCache(settings)
    app.dependency_overrides[get_service] = lambda: ReportService(database, cache, settings)
    app.dependency_overrides[get_conf] = lambda: settings

    if with_auth_state:
        app.state.settings = settings
        app.state.session_signer = SessionSigner(settings.session_secret, settings.session_ttl)
        app.state.password_checker = PasswordChecker(settings.password)
        app.state.login_limiter = LoginRateLimiter(
            settings.login_max_attempts, settings.login_lockout_seconds
        )
    return TestClient(app)


@pytest.fixture
def anon(settings: Settings) -> Iterator[TestClient]:
    """未登录客户端。"""
    client = _build_client(settings)
    yield client
    app.dependency_overrides.clear()


@pytest.fixture
def signer(settings: Settings) -> SessionSigner:
    return SessionSigner(settings.session_secret, settings.session_ttl)


# ---------------------------------------------------------------- 默认拦截


class TestDefaultDeny:
    """中间件默认拦截，白名单豁免。"""

    @pytest.mark.parametrize("url", PROTECTED_ENDPOINTS)
    def test_report_endpoints_require_session(self, anon: TestClient, url: str) -> None:
        resp = anon.get(url)
        assert resp.status_code == 401, f"{url} 未登录却返回 {resp.status_code}"
        assert resp.json()["detail"]

    def test_app_js_requires_session(self, anon: TestClient) -> None:
        """app.js 含全部 API 调用路径与字段结构，未登录者没有理由读到。"""
        assert anon.get("/app.js").status_code == 401

    def test_index_redirects_to_login(self, anon: TestClient) -> None:
        """页面请求给 302 而非 401，否则浏览器会停在一段 JSON 上。"""
        resp = anon.get("/", follow_redirects=False)
        assert resp.status_code == 302
        assert resp.headers["location"] == "/login"

    def test_api_gets_401_not_redirect(self, anon: TestClient) -> None:
        """API 请求必须是 401 JSON。

        返回 302 会让前端 fetch 拿到 200 的 HTML 登录页并试图当 JSON 解析，
        报出与「未登录」毫无关系的解析错误。
        """
        resp = anon.get("/api/overview", follow_redirects=False)
        assert resp.status_code == 401
        assert resp.headers["content-type"].startswith("application/json")

    def test_healthz_is_exempt(self, anon: TestClient) -> None:
        """compose healthcheck 在容器内调用，无法带 Cookie。"""
        assert anon.get("/healthz").status_code == 200

    def test_login_page_is_exempt(self, anon: TestClient) -> None:
        resp = anon.get("/login")
        assert resp.status_code == 200
        assert "口令" in resp.text

    def test_login_page_inlines_style(self, anon: TestClient) -> None:
        """登录页样式内联，不依赖 /app.css —— 从而无需豁免任何静态资源。"""
        text = anon.get("/login").text
        assert "<style>" in text
        assert "app.css" not in text
        assert anon.get("/app.css").status_code == 401

    def test_exempt_list_does_not_contain_docs_routes(self) -> None:
        """FastAPI 自动挂载的四个路由绝不能进豁免清单。

        这是最容易漏的一组：它们会把管理端点的请求体结构完整暴露出去。
        """
        for path in ("/openapi.json", "/docs", "/docs/oauth2-redirect", "/redoc"):
            assert path not in EXEMPT_PATHS

    def test_openapi_not_reachable_anonymously(self, anon: TestClient) -> None:
        """未登录访问 /openapi.json 必须拿不到 schema。

        默认配置下该路由未挂载（404）；即便被开启，也应被鉴权挡住（401）。
        两者都可接受，唯一不可接受的是 200。
        """
        for path in ("/openapi.json", "/docs", "/redoc"):
            code = anon.get(path).status_code
            assert code in {401, 404}, f"{path} 匿名可达，返回 {code}"

    def test_unknown_path_still_authenticated(self, anon: TestClient) -> None:
        """未知路径也要先过鉴权。

        若先返回 404，攻击者能靠状态码差异枚举出哪些管理路径存在。
        """
        assert anon.get("/api/admin/whatever").status_code == 401


class TestNeverFailOpen:
    """鉴权组件缺失时必须拒绝，绝不放行。"""

    def test_missing_signer_rejects_instead_of_allowing(self, settings: Settings) -> None:
        client = _build_client(settings, with_auth_state=False)
        # 显式清掉可能被别的测试留下的状态
        for attr in ("session_signer", "password_checker", "login_limiter"):
            if hasattr(app.state, attr):
                delattr(app.state, attr)
        try:
            resp = client.get("/api/overview")
            # 关键：不能是 200。组件没装好时放行等于完全没有鉴权。
            assert resp.status_code == 503
            assert resp.status_code != 200
        finally:
            app.dependency_overrides.clear()


# ---------------------------------------------------------------- 令牌


class TestSessionToken:
    def test_roundtrip(self, signer: SessionSigner) -> None:
        token = signer.issue()
        parsed = signer.verify(token)
        assert parsed is not None
        assert parsed.expires_at > parsed.issued_at

    def test_tampered_payload_rejected(self, signer: SessionSigner) -> None:
        body, sig = signer.issue().split(".")
        forged = f"{body[:-2]}xx.{sig}"
        assert signer.verify(forged) is None

    def test_signature_stripped_rejected(self, signer: SessionSigner) -> None:
        body = signer.issue().split(".")[0]
        assert signer.verify(body) is None
        assert signer.verify(f"{body}.") is None

    def test_rotating_secret_invalidates_old_tokens(self, settings: Settings) -> None:
        """轮换密钥即让全部已签发会话失效 —— 这是共享口令模型下唯一的踢人手段。"""
        old = SessionSigner(settings.session_secret, settings.session_ttl).issue()
        rotated = SessionSigner(settings.session_secret + "-rotated", settings.session_ttl)
        assert rotated.verify(old) is None

    def test_logout_does_not_invalidate_token_server_side(
        self, anon: TestClient, settings: Settings
    ) -> None:
        """登出是纯客户端行为：token 在服务端仍然有效。

        这条测试断言的是一个**已知边界**而非期望行为，写它的目的有两个：

        1. 把这个事实钉在测试里，避免后人误以为登出等于吊销。无状态签名会话
           在服务端没有任何记录可删（这是换取「看板保持只读」的代价，
           见 ADR-001 的 Consequences）；
        2. 如果将来真的引入了服务端吊销机制，这条会变红 —— 那时把它改成
           断言 401 并同步更新 ADR，而不是删掉了事。

        对应的缓解手段是 session_ttl 压到 2 小时 + 应急时轮换
        DASHBOARD_SESSION_SECRET，两者都有独立断言覆盖。
        """
        token = SessionSigner(settings.session_secret, settings.session_ttl).issue()

        # 用 headers 显式带 Cookie，不用 per-request cookies=（已被 starlette
        # 标记废弃，且它对「登出响应清掉 Cookie 后下一个请求还带不带」的行为
        # 有歧义 —— 而这里恰恰要模拟「客户端不听话、把旧 token 又发一次」）。
        auth = {"Cookie": f"{COOKIE_NAME}={token}"}

        # 登出：响应会带一个 max-age=0 的 Set-Cookie，让浏览器丢弃 Cookie
        resp = anon.post("/logout", headers=auth)
        assert resp.status_code == 204

        # 但重放同一个 token 仍然通得过 —— 服务端并不知道它已被「登出」
        replay = anon.get("/api/overview", headers=auth)
        assert replay.status_code != 401, (
            "若这里变成 401，说明已引入服务端吊销机制，"
            "请把本测试改为断言 401 并更新 ADR-001 的 Consequences"
        )

    def test_expiry_boundary_uses_gte(self, settings: Settings) -> None:
        """恰好到期即失效。用 > 会让令牌多活一秒，边界行为不可含糊。"""
        s = SessionSigner(settings.session_secret, ttl_seconds=100)
        now = int(time.time())
        token = s.issue(now=now)
        assert s.verify(token, now=now + 99) is not None
        assert s.verify(token, now=now + 100) is None

    def test_forged_never_expiring_payload_rejected(self, signer: SessionSigner) -> None:
        """自造一个 exp 极大的载荷但不会签名 —— 必须被拒。"""
        import base64

        payload = json.dumps(
            {"v": TOKEN_VERSION, "iat": 0, "exp": 99_999_999_999, "jti": "deadbeef"}
        ).encode()
        body = base64.urlsafe_b64encode(payload).rstrip(b"=").decode()
        assert signer.verify(f"{body}.{body}") is None

    def test_version_mismatch_rejected(self, settings: Settings) -> None:
        """版本位不符直接拒绝，而不是让旧格式以未定义行为通过。"""
        import base64
        import hashlib
        import hmac

        payload = json.dumps(
            {"v": TOKEN_VERSION + 1, "iat": 0, "exp": 99_999_999_999, "jti": "x"}
        ).encode()
        secret = settings.session_secret.encode()
        sig = hmac.new(secret, payload, hashlib.sha256).digest()
        token = (
            base64.urlsafe_b64encode(payload).rstrip(b"=").decode()
            + "."
            + base64.urlsafe_b64encode(sig).rstrip(b"=").decode()
        )
        # 签名是真的，只有版本位不符 —— 仍必须拒绝
        assert SessionSigner(settings.session_secret, settings.session_ttl).verify(token) is None

    def test_bool_exp_rejected(self, settings: Settings) -> None:
        """exp 写成 true 时不能被当成 1。bool 是 int 的子类，必须显式排除。"""
        import base64
        import hashlib
        import hmac

        payload = json.dumps({"v": TOKEN_VERSION, "iat": 0, "exp": True, "jti": "x"}).encode()
        sig = hmac.new(settings.session_secret.encode(), payload, hashlib.sha256).digest()
        token = (
            base64.urlsafe_b64encode(payload).rstrip(b"=").decode()
            + "."
            + base64.urlsafe_b64encode(sig).rstrip(b"=").decode()
        )
        assert SessionSigner(settings.session_secret, settings.session_ttl).verify(token) is None

    def test_garbage_input_rejected(self, signer: SessionSigner) -> None:
        for bad in ("", "...", "a.b.c", "not-base64!.sig", "@@@.@@@"):
            assert signer.verify(bad) is None

    def test_jti_differs_within_same_second(self, signer: SessionSigner) -> None:
        """同一秒签发的两个令牌必须不同，避免令牌值可预测。"""
        now = int(time.time())
        assert signer.issue(now=now) != signer.issue(now=now)


# ---------------------------------------------------------------- 登录


class TestLogin:
    def test_correct_password_sets_cookie(self, anon: TestClient, settings: Settings) -> None:
        resp = anon.post("/login", data={"password": settings.password})
        assert resp.status_code == 204

        raw = resp.headers["set-cookie"]
        assert COOKIE_NAME in raw
        assert "HttpOnly" in raw
        assert "SameSite=strict" in raw.replace("samesite", "SameSite")
        assert "Path=/" in raw
        assert f"Max-Age={settings.session_ttl}" in raw

    def test_cookie_secure_absent_when_disabled(self, anon: TestClient, settings: Settings) -> None:
        """默认关闭 Secure 以兼容 SSH 隧道下的 HTTP 访问。"""
        resp = anon.post("/login", data={"password": settings.password})
        assert "Secure" not in resp.headers["set-cookie"]

    def test_session_cookie_grants_access(self, anon: TestClient, settings: Settings) -> None:
        assert anon.get("/api/overview").status_code == 401
        anon.post("/login", data={"password": settings.password})
        assert anon.get("/api/overview").status_code == 200

    def test_wrong_password_401(self, anon: TestClient) -> None:
        resp = anon.post("/login", data={"password": "wrong-password-x"})
        assert resp.status_code == 401
        assert "set-cookie" not in resp.headers

    def test_json_body_also_accepted(self, anon: TestClient, settings: Settings) -> None:
        """接受 JSON 是为了让 curl 调试无需构造表单编码。"""
        resp = anon.post("/login", json={"password": settings.password})
        assert resp.status_code == 204

    def test_empty_password_401(self, anon: TestClient) -> None:
        assert anon.post("/login", data={}).status_code == 401

    def test_logout_clears_cookie(self, anon: TestClient, settings: Settings) -> None:
        anon.post("/login", data={"password": settings.password})
        resp = anon.post("/logout")
        assert resp.status_code == 204
        raw = resp.headers["set-cookie"]
        assert "Max-Age=0" in raw
        # 属性必须与签发时一致，否则浏览器视为另一个 Cookie，原会话不会被清除
        assert "Path=/" in raw
        assert "HttpOnly" in raw

    def test_password_comparison_is_constant_time(self, settings: Settings) -> None:
        """校验用的是 compare_digest，而非可短路的 ==。"""
        checker = PasswordChecker(settings.password)
        assert checker.verify(settings.password)
        assert not checker.verify(settings.password + "x")
        assert not checker.verify("")
        # 前缀相同但长度不同也必须失败
        assert not checker.verify(settings.password[:-1])


class TestLoginRateLimit:
    def test_locks_after_max_attempts(self, anon: TestClient, settings: Settings) -> None:
        for _ in range(settings.login_max_attempts):
            assert anon.post("/login", data={"password": "bad-password-1"}).status_code == 401
        resp = anon.post("/login", data={"password": "bad-password-1"})
        assert resp.status_code == 429

    def test_lockout_blocks_even_correct_password(
        self, anon: TestClient, settings: Settings
    ) -> None:
        """锁定期内即便口令正确也必须拒绝，否则限流可被绕过。"""
        for _ in range(settings.login_max_attempts):
            anon.post("/login", data={"password": "bad-password-1"})
        resp = anon.post("/login", data={"password": settings.password})
        assert resp.status_code == 429

    def test_lockout_message_hides_remaining_attempts(
        self, anon: TestClient, settings: Settings
    ) -> None:
        """不透露剩余次数 —— 告知「还剩 2 次」等于帮攻击者校准节奏。"""
        for _ in range(settings.login_max_attempts + 1):
            resp = anon.post("/login", data={"password": "bad-password-1"})
        detail = resp.json()["detail"]
        for leak in ("剩", "次数过多后", str(settings.login_max_attempts)):
            if leak == "次数过多后":
                continue
            assert leak not in detail or (leak == "剩" and "还剩" not in detail)

    def test_success_resets_counter(self, settings: Settings) -> None:
        limiter = LoginRateLimiter(max_attempts=3, lockout_seconds=900)
        limiter.record_failure("1.2.3.4")
        limiter.record_failure("1.2.3.4")
        limiter.record_success("1.2.3.4")
        # 计数已清零，再失败两次不应触发锁定
        assert not limiter.record_failure("1.2.3.4")
        assert not limiter.record_failure("1.2.3.4")

    def test_sliding_window_drops_old_failures(self) -> None:
        limiter = LoginRateLimiter(max_attempts=3, lockout_seconds=900, window_seconds=300)
        limiter.record_failure("ip", now=0.0)
        limiter.record_failure("ip", now=1.0)
        # 第三次落在窗口外，前两次应被丢弃，不触发锁定
        assert not limiter.record_failure("ip", now=400.0)

    def test_lockout_expires(self) -> None:
        limiter = LoginRateLimiter(max_attempts=2, lockout_seconds=100)
        limiter.record_failure("ip", now=0.0)
        assert limiter.record_failure("ip", now=1.0)
        assert limiter.is_locked("ip", now=50.0)
        assert not limiter.is_locked("ip", now=200.0)

    def test_client_table_is_bounded(self) -> None:
        """内存字典必须有上限。

        键是客户端 IP，攻击者伪造源 IP 就能无限增长这张表 ——
        限流本身会变成拒绝服务的入口。
        """
        limiter = LoginRateLimiter(max_attempts=5, lockout_seconds=900, max_clients=10)
        for i in range(100):
            limiter.record_failure(f"10.0.0.{i}")
        assert len(limiter._clients) <= 10

    def test_forwarded_for_header_is_not_trusted(
        self, anon: TestClient, settings: Settings
    ) -> None:
        """限流键绝不取自 X-Forwarded-For。

        当前无反向代理，该头完全由客户端控制 —— 信任它意味着攻击者每次
        换一个伪造 IP 就能绕过限流。这里每次请求都换一个伪造 IP，
        限流仍必须生效。
        """
        for i in range(settings.login_max_attempts):
            resp = anon.post(
                "/login",
                data={"password": "bad-password-1"},
                headers={"X-Forwarded-For": f"9.9.9.{i}"},
            )
            assert resp.status_code == 401
        resp = anon.post(
            "/login",
            data={"password": "bad-password-1"},
            headers={"X-Forwarded-For": "9.9.9.250"},
        )
        assert resp.status_code == 429, "伪造 X-Forwarded-For 绕过了限流"


# ---------------------------------------------------------------- CSRF


class TestCSRF:
    def test_cross_site_origin_rejected(self, anon: TestClient, settings: Settings) -> None:
        anon.post("/login", data={"password": settings.password})
        resp = anon.patch(
            "/api/admin/keys/volc_001",
            json={"status": "banned"},
            headers={"Origin": "https://evil.example"},
        )
        assert resp.status_code == 403

    def test_same_origin_allowed(self, anon: TestClient, settings: Settings) -> None:
        anon.post("/login", data={"password": settings.password})
        resp = anon.patch(
            "/api/admin/keys/volc_001",
            json={"status": "banned"},
            headers={"Origin": "http://testserver"},
        )
        # 通过 CSRF 与鉴权后会走到转发层，此时没有网关可连 —— 502/504 都算通过
        assert resp.status_code != 403

    def test_bad_referer_rejected(self, anon: TestClient, settings: Settings) -> None:
        anon.post("/login", data={"password": settings.password})
        resp = anon.patch(
            "/api/admin/keys/volc_001",
            json={"status": "banned"},
            headers={"Referer": "https://evil.example/page"},
        )
        assert resp.status_code == 403

    def test_malformed_referer_rejected(self, anon: TestClient, settings: Settings) -> None:
        """Referer 存在但格式畸形时必须拒绝，而不是当成「没有声称来源」放行。

        若按「解析结果为空就跳过校验」实现，构造一个畸形 Referer 就绕过了
        这道防线 —— 判据必须是「有没有声称来源」。
        """
        anon.post("/login", data={"password": settings.password})
        for bad in ("not-a-url", "///", "javascript:alert(1)"):
            resp = anon.patch(
                "/api/admin/keys/volc_001",
                json={"status": "banned"},
                headers={"Referer": bad},
            )
            assert resp.status_code == 403, f"畸形 Referer {bad!r} 未被拒绝"

    def test_missing_both_headers_allowed(self, anon: TestClient, settings: Settings) -> None:
        """两者都缺失则放行。

        CSRF 的前提是浏览器自动携带凭据，而浏览器发起的跨站请求必定带
        Origin。命令行客户端不带 Origin，也不会自动带上别人的 Cookie，
        不构成 CSRF。强行要求 Origin 只会堵死 curl 调试路径。
        """
        anon.post("/login", data={"password": settings.password})
        resp = anon.patch("/api/admin/keys/volc_001", json={"status": "banned"})
        assert resp.status_code != 403

    def test_get_requests_skip_csrf(self, anon: TestClient, settings: Settings) -> None:
        """安全方法不受 CSRF 校验影响。"""
        anon.post("/login", data={"password": settings.password})
        resp = anon.get("/api/overview", headers={"Origin": "https://evil.example"})
        assert resp.status_code == 200

    def test_auth_runs_before_csrf(self, anon: TestClient) -> None:
        """未登录的写请求应先拿到 401 而不是 403。

        顺序反了的话前端据 401 跳登录页的逻辑就失效 ——
        运维会看到「来源不被允许」而完全想不到自己只是没登录。
        """
        resp = anon.patch(
            "/api/admin/keys/volc_001",
            json={"status": "banned"},
            headers={"Origin": "https://evil.example"},
        )
        assert resp.status_code == 401


# ---------------------------------------------------------------- 配置


class TestConfigFailFast:
    """必填项缺失必须启动失败，而不是静默降级为无鉴权。"""

    def _clear(self, monkeypatch: pytest.MonkeyPatch) -> None:
        for name in (
            "DASHBOARD_PASSWORD",
            "DASHBOARD_SESSION_SECRET",
            "GATEWAY_ADMIN_API_KEY",
        ):
            monkeypatch.delenv(name, raising=False)

    def test_missing_password_refuses_start(self, monkeypatch: pytest.MonkeyPatch) -> None:
        from app.config import get_settings

        self._clear(monkeypatch)
        get_settings.cache_clear()
        with pytest.raises(ConfigError, match="DASHBOARD_PASSWORD"):
            get_settings()
        get_settings.cache_clear()

    def test_short_password_refuses_start(self, monkeypatch: pytest.MonkeyPatch) -> None:
        from app.config import get_settings

        self._clear(monkeypatch)
        monkeypatch.setenv("DASHBOARD_PASSWORD", "short")
        get_settings.cache_clear()
        with pytest.raises(ConfigError, match="长度不足"):
            get_settings()
        get_settings.cache_clear()

    def test_missing_session_secret_refuses_start(self, monkeypatch: pytest.MonkeyPatch) -> None:
        from app.config import get_settings

        self._clear(monkeypatch)
        monkeypatch.setenv("DASHBOARD_PASSWORD", "long-enough-password")
        get_settings.cache_clear()
        with pytest.raises(ConfigError, match="DASHBOARD_SESSION_SECRET"):
            get_settings()
        get_settings.cache_clear()

    def test_short_session_secret_refuses_start(self, monkeypatch: pytest.MonkeyPatch) -> None:
        from app.config import get_settings

        self._clear(monkeypatch)
        monkeypatch.setenv("DASHBOARD_PASSWORD", "long-enough-password")
        monkeypatch.setenv("DASHBOARD_SESSION_SECRET", "too-short")
        get_settings.cache_clear()
        with pytest.raises(ConfigError, match="长度不足"):
            get_settings()
        get_settings.cache_clear()

    def test_missing_admin_key_refuses_start(self, monkeypatch: pytest.MonkeyPatch) -> None:
        from app.config import get_settings

        self._clear(monkeypatch)
        monkeypatch.setenv("DASHBOARD_PASSWORD", "long-enough-password")
        monkeypatch.setenv("DASHBOARD_SESSION_SECRET", "a" * 32)
        get_settings.cache_clear()
        with pytest.raises(ConfigError, match="GATEWAY_ADMIN_API_KEY"):
            get_settings()
        get_settings.cache_clear()

    def test_all_present_succeeds(self, monkeypatch: pytest.MonkeyPatch) -> None:
        from app.config import get_settings

        monkeypatch.setenv("DASHBOARD_PASSWORD", "long-enough-password")
        monkeypatch.setenv("DASHBOARD_SESSION_SECRET", "a" * 32)
        monkeypatch.setenv("GATEWAY_ADMIN_API_KEY", "admin-key")
        get_settings.cache_clear()
        try:
            conf = get_settings()
            assert conf.gateway_admin_api_key == "admin-key"
            # 默认值应与 Spec 4.1 一致。
            #
            # session_ttl 不只是「多久要重新登录」，它同时是登出后 token 仍可
            # 被重放的最长时间（无状态会话无法吊销单个 token，见 ADR-001）。
            # 这条断言防的是后人为了「省得重新登录」把它调回 8 小时，而没有
            # 意识到自己把风险窗口放大了 4 倍。
            assert conf.session_ttl == 7_200
            assert conf.cookie_secure is False
            assert conf.gateway_timeout_import == 90
            assert conf.docs_enabled is False
            assert conf.allowed_origins == ()
        finally:
            get_settings.cache_clear()

    def test_wildcard_origin_rejected(self, monkeypatch: pytest.MonkeyPatch) -> None:
        """带凭据的请求配 * 等于关掉同源保护。"""
        from app.config import get_settings

        monkeypatch.setenv("DASHBOARD_PASSWORD", "long-enough-password")
        monkeypatch.setenv("DASHBOARD_SESSION_SECRET", "a" * 32)
        monkeypatch.setenv("GATEWAY_ADMIN_API_KEY", "admin-key")
        monkeypatch.setenv("DASHBOARD_ALLOWED_ORIGINS", "https://ok.example,*")
        get_settings.cache_clear()
        with pytest.raises(ConfigError, match=r"\*"):
            get_settings()
        get_settings.cache_clear()
