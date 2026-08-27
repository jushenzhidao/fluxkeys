"""口令比对：存哈希、常量时间比较。"""

from __future__ import annotations

import hashlib
import hmac


class PasswordChecker:
    """校验登录口令。

    进程内只保留 sha256 摘要，不保留明文。

    这里刻意**不用 bcrypt / argon2**，不是安全强度不足而是威胁模型不同：
    慢哈希的价值在于「哈希值泄漏后抬高离线爆破成本」，而本场景的哈希只存在
    于进程内存，且明文口令本来就在同一进程的环境变量里 —— 能读到哈希的
    攻击者早已能读到明文，慢哈希没有任何增益，只增加依赖与登录延迟。
    这与「把用户密码存进数据库」是两回事。
    """

    __slots__ = ("_digest",)

    def __init__(self, password: str) -> None:
        if not password:
            raise ValueError("口令不能为空")
        self._digest = self._hash(password)

    def verify(self, candidate: str) -> bool:
        """常量时间比对。

        必须用 ``hmac.compare_digest`` 而非 ``==``：后者按字节短路返回，
        攻击者能据响应时间逐字节爆破。先取 sha256 再比对，顺带保证两侧
        长度一致，避免长度差本身成为侧信道。
        """
        return hmac.compare_digest(self._hash(candidate), self._digest)

    @staticmethod
    def _hash(value: str) -> bytes:
        return hashlib.sha256(value.encode("utf-8")).digest()
