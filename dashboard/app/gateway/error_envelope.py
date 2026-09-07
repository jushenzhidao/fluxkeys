"""网关错误信封（OpenAI 风格）的解析。

单独成文件而不是留在 ``client.py`` 里：这些都是不碰 httpx、不碰配置的纯函数，
输入是已解码的 payload、输出是数据。放在一起会让 ``client.py`` 同时承担
「发请求」和「解析响应体格式」两件事，而后者是随网关契约变化的部分，
改动频率与前者完全不同。
"""

from __future__ import annotations

from typing import Any, Final

_REDACTED: Final[str] = "***"


def extract_gateway_error(payload: Any) -> tuple[str, str]:
    """从网关的 OpenAI 风格错误结构里取出文案与错误码。"""
    if isinstance(payload, dict):
        err = payload.get("error")
        if isinstance(err, dict):
            message = err.get("message")
            code = err.get("code")
            return (
                message if isinstance(message, str) else "",
                code if isinstance(code, str) else "",
            )
    return "", ""


def redact(text: str, secrets: tuple[str, ...]) -> str:
    """抹掉网关文案里回显的管理密钥。

    网关的部分错误文案会把收到的凭据拼进 message。之前唯一没泄漏的原因是
    401 分支整句替换成了看板自己的文案 —— 而 400/422 这些新透传的分支是原样
    转发 message 的，一旦网关某条文案带上密钥，它就会直接出现在浏览器里。

    不依赖「网关不会这么写」：那是另一个仓库的另一个人的自由，而这里付出的
    代价只是一次字符串替换。
    """
    for secret in secrets:
        if secret and secret in text:
            text = text.replace(secret, _REDACTED)
    return text


# 错误响应里需要转发给前端的结构化字段。
#
# 白名单而非「除 error 外全带上」：后者会把网关将来新增的任何字段自动暴露到
# 浏览器，包括可能含内部细节的调试字段。加一个字段的成本是改这里一行，
# 而漏掉一次泄漏的成本要高得多。
_ERROR_EXTRA_KEYS: Final[frozenset[str]] = frozenset(
    {
        # provider 校验失败的逐字段问题清单（admin_provider.go:697）。
        "failures",
        # 同上的非阻断告警（:698）。
        "warnings",
        # 跨量纲被拒时的字段级差异（:719）。
        "diff",
        # 乐观锁冲突时网关侧的当前版本号，前端用它写「当前已是 #130」。
        "current_version",
        "expected_version",
    }
)


def extract_error_extras(payload: Any) -> dict[str, Any]:
    """取出错误响应里 ``error`` 信封之外的结构化字段。

    这些字段与 ``error`` 同级而非嵌在里面（见 ``writeProviderInvalid``），
    只解析 ``error`` 会把它们整体丢掉，前端就只剩一句「校验未通过」——
    而运维需要知道的恰好是「哪个字段、为什么」。
    """
    if not isinstance(payload, dict):
        return {}
    return {k: v for k, v in payload.items() if k in _ERROR_EXTRA_KEYS}
