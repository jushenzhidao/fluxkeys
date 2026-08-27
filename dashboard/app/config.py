"""看板配置。全部来自环境变量，无本地配置文件。"""

from __future__ import annotations

import os
from dataclasses import dataclass
from datetime import datetime
from functools import lru_cache
from zoneinfo import ZoneInfo


@dataclass(frozen=True, slots=True)
class Settings:
    """看板运行期配置。

    看板是**只读**服务：数据库连接以只读方式使用，不做任何写入或迁移。
    """

    postgres_dsn: str
    redis_addr: str
    redis_password: str
    redis_db: int
    port: int
    timezone: str
    # 单 Key 的 Token 硬水位，仅在 Redis 未写入 hard_limit 时作为兜底基准。
    # 与网关 config.Quota 默认值保持一致：500 万 × 0.90。
    token_hard_default: int
    count_hard_default: int
    # Postgres 连接池上限。看板 QPS 极低，小池足够。
    pg_max_size: int
    pg_min_size: int
    # 行为相似度告警阈值。火山商务反馈的封禁根因是「用户行为规律相似」，
    # 超过该值的 Key 对需要人工介入调整 persona。
    similarity_alert: float

    # ---------------------------------------------------------- 管理控制台

    # 看板登录口令。进程内只保留 sha256 摘要，不留明文（见 app/security/password.py）。
    password: str
    # 会话签名密钥。轮换它即让全部已签发会话失效。
    #
    # 这是本方案唯一的会话吊销手段: 密钥参与 HMAC 签名，改掉它所有既有
    # token 的签名校验都会失败。口令泄漏或怀疑 Cookie 泄漏时，正确处置是
    # 改这个值并重启看板 —— 不是让用户点登出（登出只清浏览器 Cookie）。
    #
    # 多副本部署必须共用同一个值，因此应急吊销时**每个副本都要改并重启**，
    # 漏掉一个，那个副本上的旧 token 仍然有效。
    session_secret: str
    # 转发到网关时注入的管理密钥。绝不下发到浏览器。
    gateway_admin_api_key: str
    gateway_base_url: str
    # 会话有效期（秒）。默认 2 小时。
    #
    # 这个值不只是「多久要重新登录」，它同时是**登出后 token 仍可被重放的
    # 最长时间**。无状态签名会话无法在服务端吊销单个 token（见下方注释与
    # ADR-001），所以 TTL 就是风险窗口的实际长度：8 小时意味着早上登录一次、
    # 到下班前拿到过该 Cookie 的人都还能用它。
    #
    # 调大它等于按同样倍数放大这个窗口，不要因为「省得重新登录」就随手加。
    session_ttl: int
    # Cookie 的 Secure 属性。默认关闭以兼容 SSH 隧道下的 HTTP 本地访问，
    # 关闭时启动会打印告警（见 create_app）。
    cookie_secure: bool
    # 允许的 Origin 列表，空表示只接受同源。绝不配 "*"。
    allowed_origins: tuple[str, ...]
    # 登录失败限流：窗口内连续失败达阈值即锁定该 IP。
    login_max_attempts: int
    login_lockout_seconds: int
    # POST /admin/keys 的转发读超时。网关是逐条 upsert，1000 个 Key 需 20-50 秒。
    gateway_timeout_import: int
    # 自动文档开关。生产默认关闭，是鉴权之外的纵深防御。
    docs_enabled: bool

    @property
    def tzinfo(self) -> ZoneInfo:
        return ZoneInfo(self.timezone)

    def now(self) -> datetime:
        """返回配置时区下的当前时刻。所有时间聚合都必须以此为基准。"""
        return datetime.now(self.tzinfo)


def _env_int(name: str, default: int) -> int:
    raw = os.getenv(name)
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError:
        return default


def _env_float(name: str, default: float) -> float:
    raw = os.getenv(name)
    if not raw:
        return default
    try:
        return float(raw)
    except ValueError:
        return default


def _env_bool(name: str, default: bool) -> bool:
    raw = os.getenv(name)
    if not raw:
        return default
    return raw.strip().lower() in {"1", "true", "yes", "on"}


class ConfigError(RuntimeError):
    """必填配置缺失或非法。抛出即终止启动。"""


# 会话密钥的最小长度。短密钥可被离线暴力破解，届时任何人都能自签会话。
_MIN_SESSION_SECRET_LEN = 32
# 口令最小长度。管理控制台是特权入口，弱口令等于没有鉴权。
_MIN_PASSWORD_LEN = 12


def _require_secrets() -> tuple[str, str, str]:
    """校验三个必填项，缺失或过短则拒绝启动。

    为什么是 fail fast 而不是像数据库那样「连不上也先起来」：
    这两类问题的性质不同。数据库/Redis 连不上是**环境的瞬时状态**，
    可能自行恢复，所以启动后用 /healthz 暴露更利于排障；而口令/密钥缺失是
    **配置错误**，不会自行恢复，且静默降级的后果是管理控制台裸奔或全体
    登录失败。参照 FLUXKEYS_ENCRYPTION_KEY 的既有约定。

    ``DASHBOARD_SESSION_SECRET`` 刻意不自动随机生成：自动生成会让
    「重启不掉线」与「多副本共享会话」两项设计目标同时失效，而失效方式是
    隐蔽的 —— 功能看起来正常，只是用户偶尔莫名掉线，且现象随负载均衡落点
    随机出现，极难排查。宁可启动失败，让人一次性配好。
    """
    password = os.getenv("DASHBOARD_PASSWORD", "")
    if not password:
        raise ConfigError("未配置看板口令 DASHBOARD_PASSWORD，管理控制台拒绝启动")
    if len(password) < _MIN_PASSWORD_LEN:
        raise ConfigError(
            f"DASHBOARD_PASSWORD 长度不足 {_MIN_PASSWORD_LEN} 字符，"
            "管理控制台是特权入口，拒绝启动"
        )

    secret = os.getenv("DASHBOARD_SESSION_SECRET", "")
    if not secret:
        raise ConfigError(
            "未配置 DASHBOARD_SESSION_SECRET，拒绝启动。"
            '生成方式：python -c "import secrets; print(secrets.token_urlsafe(32))"'
        )
    if len(secret) < _MIN_SESSION_SECRET_LEN:
        raise ConfigError(
            f"DASHBOARD_SESSION_SECRET 长度不足 {_MIN_SESSION_SECRET_LEN} 字符，拒绝启动"
        )

    admin_key = os.getenv("GATEWAY_ADMIN_API_KEY", "")
    if not admin_key:
        raise ConfigError(
            "未配置 GATEWAY_ADMIN_API_KEY，拒绝启动 —— 无此项则所有转发必然 401，"
            "而 401 会被看板转成 500，运维只能看到无法解释的服务端错误"
        )
    return password, secret, admin_key


def _parse_origins(raw: str) -> tuple[str, ...]:
    """解析逗号分隔的 Origin 列表。

    显式拒绝 ``*``：带凭据的请求配通配符等于关掉同源保护，
    而看板是持有管理密钥的特权入口。
    """
    items = tuple(item.strip() for item in raw.split(",") if item.strip())
    if "*" in items:
        raise ConfigError("DASHBOARD_ALLOWED_ORIGINS 不允许配置 *")
    return items


@lru_cache(maxsize=1)
def get_settings() -> Settings:
    """读取并缓存配置。进程内只解析一次。

    必填项校验放在这里而非调用处：有 ``lru_cache`` 兜底，进程内只校验一次，
    且任何调用方都不可能拿到一份未校验的配置。
    """
    password, session_secret, admin_key = _require_secrets()
    return Settings(
        password=password,
        session_secret=session_secret,
        gateway_admin_api_key=admin_key,
        gateway_base_url=os.getenv("GATEWAY_BASE_URL", "http://gateway:8080").rstrip("/"),
        session_ttl=_env_int("DASHBOARD_SESSION_TTL", 7_200),
        cookie_secure=_env_bool("DASHBOARD_COOKIE_SECURE", False),
        allowed_origins=_parse_origins(os.getenv("DASHBOARD_ALLOWED_ORIGINS", "")),
        login_max_attempts=_env_int("DASHBOARD_LOGIN_MAX_ATTEMPTS", 5),
        login_lockout_seconds=_env_int("DASHBOARD_LOGIN_LOCKOUT_SECONDS", 900),
        gateway_timeout_import=_env_int("GATEWAY_TIMEOUT_IMPORT", 90),
        docs_enabled=_env_bool("DASHBOARD_DOCS_ENABLED", False),
        postgres_dsn=os.getenv(
            "POSTGRES_DSN",
            "postgres://fluxkeys:fluxkeys@127.0.0.1:5432/fluxkeys?sslmode=disable",
        ),
        redis_addr=os.getenv("REDIS_ADDR", "127.0.0.1:6379"),
        redis_password=os.getenv("REDIS_PASSWORD", ""),
        redis_db=_env_int("REDIS_DB", 0),
        port=_env_int("DASHBOARD_PORT", 8081),
        timezone=os.getenv("DASHBOARD_TZ", "Asia/Shanghai"),
        token_hard_default=_env_int("QUOTA_TOKEN_HARD", 4_500_000),
        count_hard_default=_env_int("QUOTA_COUNT_HARD", 90),
        pg_max_size=_env_int("DASHBOARD_PG_MAX_CONNS", 8),
        pg_min_size=_env_int("DASHBOARD_PG_MIN_CONNS", 1),
        similarity_alert=_env_float("SIMILARITY_ALERT_THRESHOLD", 0.6),
    )


def redis_url(settings: Settings) -> str:
    """把 ``host:port`` 形式的地址拼成 redis-py 需要的 URL。

    网关侧用 ``REDIS_ADDR=host:port``，这里保持同一环境变量以便共用 compose 配置。
    """
    addr = settings.redis_addr
    if addr.startswith(("redis://", "rediss://", "unix://")):
        return addr
    auth = f":{settings.redis_password}@" if settings.redis_password else ""
    return f"redis://{auth}{addr}/{settings.redis_db}"


def asyncpg_dsn(dsn: str) -> str:
    """把 Go 风格 DSN 转成 asyncpg 可接受的形式。

    asyncpg 不认 ``sslmode=disable`` 之外的部分 libpq 参数写法，但认 ``sslmode``；
    这里仅规范化 scheme，其余参数原样透传。
    """
    if dsn.startswith(("postgresql://", "postgres://")):
        return dsn
    return f"postgres://{dsn}"
