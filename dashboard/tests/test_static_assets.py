"""静态资源路由测试。

这组测试补的是一个已经真实发生过的缺陷：前端新增了 5 个 admin 文件，
后端忘了注册路由，结果浏览器里全部 404、管理界面整块渲染不出来，而当时
238 条 pytest 全绿 —— 没有任何一条断言这些路径可达。

所以这里不只测「已注册的能拿到」，还要测**「index.html 引用的每一个本地资源
都必须已注册」**。后者才是能防住下次遗漏的那条：新增文件时只改前端不改后端，
它会立刻变红。
"""

from __future__ import annotations

import re
from collections.abc import Iterator

import pytest
from fastapi.testclient import TestClient

from app.api.deps import get_conf, get_service
from app.api.static_files import _ASSETS, STATIC_DIR
from app.config import Settings
from app.main import app
from app.security import COOKIE_NAME, SessionSigner
from app.service import ReportService
from tests.test_api import EmptyDatabase, FakeCache

# index.html 引用的本地资源（排除 CDN 与 #icon-* 这类片段引用）。
_LOCAL_REF = re.compile(r'(?:src|href)="(/[^"#]+)"')


def _client(settings: Settings, *, authenticated: bool) -> TestClient:
    database = EmptyDatabase(settings)
    cache = FakeCache(settings)
    app.dependency_overrides[get_service] = lambda: ReportService(database, cache, settings)
    app.dependency_overrides[get_conf] = lambda: settings

    signer = SessionSigner(settings.session_secret, settings.session_ttl)
    app.state.settings = settings
    app.state.session_signer = signer

    client = TestClient(app)
    if authenticated:
        client.cookies.set(COOKIE_NAME, signer.issue())
    return client


@pytest.fixture
def logged_in(settings: Settings) -> Iterator[TestClient]:
    client = _client(settings, authenticated=True)
    yield client
    app.dependency_overrides.clear()


@pytest.fixture
def anon(settings: Settings) -> Iterator[TestClient]:
    client = _client(settings, authenticated=False)
    yield client
    app.dependency_overrides.clear()


# 管理控制台的 5 个资源。单独列出而非只用 _ASSETS，是为了让「这 5 个必须存在」
# 成为测试代码里的显式事实 —— 只依赖 _ASSETS 的话，有人从表里删掉一行，
# 测试会跟着一起放过。
ADMIN_ASSETS = [
    "/admin.css",
    "/admin-icons.js",
    "/admin-ui.js",
    "/admin.js",
    "/admin-import.js",
]


class TestAdminAssetsReachable:
    @pytest.mark.parametrize("path", ADMIN_ASSETS)
    def test_served_when_logged_in(self, logged_in: TestClient, path: str) -> None:
        """登录后必须 200。缺失即管理界面渲染不出来。"""
        resp = logged_in.get(path)
        assert resp.status_code == 200, f"{path} 未注册或文件缺失 -> {resp.status_code}"
        assert resp.content, f"{path} 返回空内容"

    @pytest.mark.parametrize("path", ADMIN_ASSETS)
    def test_requires_auth(self, anon: TestClient, path: str) -> None:
        """未登录必须 401。

        这些文件含全部 API 调用路径与管理操作的请求体格式，
        与 app.js 同等对待，不能因为「只是静态资源」就放行。
        """
        assert anon.get(path).status_code == 401, f"{path} 未登录可读"

    @pytest.mark.parametrize("path", ADMIN_ASSETS)
    def test_content_type_correct(self, logged_in: TestClient, path: str) -> None:
        """CSS 不能以 JS 类型下发，否则浏览器按脚本解析直接报错。"""
        ctype = logged_in.get(path).headers["content-type"]
        expected = "text/css" if path.endswith(".css") else "application/javascript"
        assert expected in ctype, f"{path} content-type = {ctype}"


class TestBaseAssetsStillWork:
    """确认新增注册方式没有打坏原有资源（回归）。"""

    @pytest.mark.parametrize("path", ["/app.js", "/app.css"])
    def test_base_assets_served(self, logged_in: TestClient, path: str) -> None:
        assert logged_in.get(path).status_code == 200

    @pytest.mark.parametrize("path", ["/app.js", "/app.css"])
    def test_base_assets_require_auth(self, anon: TestClient, path: str) -> None:
        assert anon.get(path).status_code == 401

    def test_index_served_when_logged_in(self, logged_in: TestClient) -> None:
        resp = logged_in.get("/")
        assert resp.status_code == 200
        assert "text/html" in resp.headers["content-type"]

    def test_index_redirects_when_anonymous(self, anon: TestClient) -> None:
        resp = anon.get("/", follow_redirects=False)
        assert resp.status_code == 302
        assert resp.headers["location"] == "/login"


class TestRegistrationConsistency:
    """把「前端引用」与「后端注册」对上，这是防下次遗漏的关键一条。"""

    def test_every_local_ref_in_index_is_registered(self) -> None:
        """index.html 引用的每个本地资源都必须已注册路由。

        这条能直接抓住「前端加了文件、后端忘了注册」——
        即本次被打回的那个缺陷。
        """
        html = (STATIC_DIR / "index.html").read_text(encoding="utf-8")
        referenced = set(_LOCAL_REF.findall(html))
        registered = set(_ASSETS) | {"/"}

        missing = sorted(referenced - registered)
        assert not missing, f"index.html 引用了未注册的资源: {missing}"

    def test_every_registered_asset_exists_on_disk(self) -> None:
        """注册表里的每个文件都必须真实存在。

        防的是另一半：注册了但文件名拼错 —— 那样运行期才 404。
        """
        for url_path, (filename, _) in _ASSETS.items():
            assert (STATIC_DIR / filename).is_file(), f"{url_path} 指向的 {filename} 不存在"

    def test_admin_assets_are_all_registered(self) -> None:
        """5 个管理资源一个都不能少。"""
        for path in ADMIN_ASSETS:
            assert path in _ASSETS, f"{path} 未在 _ASSETS 注册表内"

    def test_missing_file_returns_404_not_crash(self, logged_in: TestClient) -> None:
        """注册了但文件被删时应是干净的 404，而不是响应中途崩掉。"""
        from app.api import static_files

        static_files._register(
            "/__probe_missing__.js", "definitely_absent.js", "application/javascript"
        )
        try:
            assert logged_in.get("/__probe_missing__.js").status_code == 404
        finally:
            # 移除探针路由，避免污染其他测试
            static_files.router.routes = [
                r
                for r in static_files.router.routes
                if getattr(r, "path", None) != "/__probe_missing__.js"
            ]
