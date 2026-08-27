"""无状态会话令牌：HMAC-SHA256 自签名。

令牌格式::

    token   = base64url(payload_json) + "." + base64url(hmac_sha256(secret, payload_json))
    payload = {"v": 1, "iat": <签发秒>, "exp": <过期秒>, "jti": <随机 8 字节>}

为什么自签名而不用服务端会话表：看板的 Postgres 连接是服务端强制只读
（已实测无法绕过），会话表根本写不进去；改走 Redis 则把看板从「只读旁路
组件」变成有写状态的组件，与写路径唯一化的收敛方向相反。服务端会话唯一的
独有能力是「踢出单个会话」，而共享口令模型下没有「单个会话」的身份概念，
要作废就是全体作废 —— 轮换 ``session_secret`` 即可做到。

也没有用 itsdangerous / Starlette SessionMiddleware：该中间件把
itsdangerous 作为可选依赖，而本项目 venv 内并未安装，选它同样要动依赖清单。
省下的约 30 行代码换来一个仅为签名 Cookie 而存在的依赖，不值。
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import secrets
import time
from dataclasses import dataclass
from typing import Any, Final

# 载荷版本位。将来改载荷结构时靠它拒绝旧格式，
# 而不是让旧令牌以未定义行为通过校验。
TOKEN_VERSION: Final[int] = 1

# Cookie 名。带 fk_ 前缀避免与同源部署的其他应用撞名。
COOKIE_NAME: Final[str] = "fk_session"


def _b64url_encode(raw: bytes) -> str:
    """无填充 base64url。去掉 '=' 是因为它在 Cookie 值里需要额外转义。"""
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def _b64url_decode(text: str) -> bytes:
    padding = "=" * (-len(text) % 4)
    return base64.urlsafe_b64decode(text + padding)


@dataclass(frozen=True, slots=True)
class SessionToken:
    """已通过校验的会话载荷。"""

    issued_at: int
    expires_at: int
    jti: str


class SessionSigner:
    """签发与校验会话令牌。"""

    def __init__(self, secret: str, ttl_seconds: int) -> None:
        if not secret:
            raise ValueError("会话密钥不能为空")
        self._secret = secret.encode("utf-8")
        self._ttl = ttl_seconds

    def issue(self, now: int | None = None) -> str:
        """签发一个新令牌。"""
        issued = int(time.time()) if now is None else now
        payload = {
            "v": TOKEN_VERSION,
            "iat": issued,
            "exp": issued + self._ttl,
            # jti 不用于服务端查重（无状态，无处可查）。
            # 它的作用是让同一秒内为同一口令签发的两个令牌不相同，
            # 避免令牌值可预测。
            "jti": secrets.token_hex(8),
        }
        body = json.dumps(payload, separators=(",", ":"), sort_keys=True).encode("utf-8")
        return f"{_b64url_encode(body)}.{_b64url_encode(self._sign(body))}"

    def verify(self, token: str, now: int | None = None) -> SessionToken | None:
        """校验令牌。任何一处不通过都返回 ``None``，不区分失败原因。

        不区分原因是有意的：向调用方（最终会传到浏览器）说明「签名错」还是
        「已过期」，等于告诉伪造者他离成功还有多远。
        """
        if not token or token.count(".") != 1:
            return None
        body_part, sig_part = token.split(".", 1)

        try:
            body = _b64url_decode(body_part)
            provided_sig = _b64url_decode(sig_part)
        except (ValueError, TypeError):
            return None

        # 先验签名，再解析载荷。
        #
        # 顺序颠倒等于把 json.loads 暴露给未经验证的输入 —— 攻击者可控的
        # 字节会先进解析器，而解析器的攻击面远大于一次 HMAC 比较。
        if not hmac.compare_digest(provided_sig, self._sign(body)):
            return None

        try:
            payload: Any = json.loads(body)
        except (ValueError, TypeError):
            return None
        if not isinstance(payload, dict):
            return None

        # 版本不符直接拒绝，不做兼容猜测。
        if payload.get("v") != TOKEN_VERSION:
            return None

        issued_at = payload.get("iat")
        expires_at = payload.get("exp")
        jti = payload.get("jti")
        # 逐项校验类型：伪造者可以把 exp 写成字符串或 null 来试探比较逻辑。
        #
        # 用内联的 isinstance 而非抽出的辅助函数，是为了让类型检查器能真正
        # 收窄类型 —— 抽成函数后检查器无法据其返回值推断 expires_at 非 None，
        # 后续的比较就成了「看起来可能拿 None 做比较」的代码。
        #
        # bool 必须显式排除：它是 int 的子类，否则 exp=true 会被当成 1。
        if (
            not isinstance(issued_at, int)
            or isinstance(issued_at, bool)
            or not isinstance(expires_at, int)
            or isinstance(expires_at, bool)
            or not isinstance(jti, str)
        ):
            return None

        current = int(time.time()) if now is None else now
        # 用 >= 而非 >：恰好到期的令牌应当失效。
        if current >= expires_at:
            return None

        return SessionToken(issued_at=issued_at, expires_at=expires_at, jti=jti)

    def _sign(self, body: bytes) -> bytes:
        return hmac.new(self._secret, body, hashlib.sha256).digest()
