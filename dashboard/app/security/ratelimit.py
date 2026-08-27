"""登录失败限流。

看板是特权入口且共享口令只有一个，不限流等于给爆破留门。

状态放**进程内存**而非 Redis：写 Redis 会破坏看板「只读旁路组件」的定位。
代价是多副本下攻击者可以轮询副本把尝试次数放大到 ``阈值 × 副本数`` ——
但这只是把 5 次放大到十几次，量级上仍远低于有效爆破所需，而收益是保住了
不变量。这个取舍是显式的，不是漏考虑。
"""

from __future__ import annotations

import time
from collections import OrderedDict
from dataclasses import dataclass, field

# 内存字典的条目上限。
#
# 必须有上限：键是客户端 IP，攻击者伪造源 IP 就能无限增长这张表，
# 把看板内存打满 —— 限流本身变成拒绝服务的入口。
MAX_TRACKED_CLIENTS = 10_000


@dataclass(slots=True)
class _Attempts:
    """单个客户端的失败记录。"""

    # 失败时刻列表，按时间递增。
    timestamps: list[float] = field(default_factory=list)
    # 锁定截止时刻，0 表示未锁定。
    locked_until: float = 0.0


class LoginRateLimiter:
    """按客户端标识做滑动窗口限流。"""

    def __init__(
        self,
        max_attempts: int,
        lockout_seconds: int,
        window_seconds: int = 300,
        max_clients: int = MAX_TRACKED_CLIENTS,
    ) -> None:
        self._max_attempts = max_attempts
        self._lockout = lockout_seconds
        self._window = window_seconds
        self._max_clients = max_clients
        # OrderedDict 用于 LRU 淘汰：超过上限时丢最久未活动的条目。
        self._clients: OrderedDict[str, _Attempts] = OrderedDict()

    def is_locked(self, client: str, now: float | None = None) -> bool:
        """判断该客户端当前是否处于锁定期。"""
        current = time.monotonic() if now is None else now
        record = self._clients.get(client)
        if record is None:
            return False
        if record.locked_until > current:
            return True
        # 锁定已到期，清零重新开始计数。
        if record.locked_until:
            record.locked_until = 0.0
            record.timestamps.clear()
        return False

    def record_failure(self, client: str, now: float | None = None) -> bool:
        """记录一次失败。返回该客户端是否因此进入锁定。"""
        current = time.monotonic() if now is None else now
        record = self._touch(client)

        # 丢弃窗口外的历史，实现滑动窗口。
        cutoff = current - self._window
        record.timestamps = [ts for ts in record.timestamps if ts > cutoff]
        record.timestamps.append(current)

        if len(record.timestamps) >= self._max_attempts:
            record.locked_until = current + self._lockout
            record.timestamps.clear()
            return True
        return False

    def record_success(self, client: str) -> None:
        """登录成功即清零该客户端的计数。"""
        self._clients.pop(client, None)

    def _touch(self, client: str) -> _Attempts:
        """取出并把该客户端移到 LRU 尾部，必要时淘汰最旧条目。"""
        record = self._clients.get(client)
        if record is None:
            record = _Attempts()
            self._clients[client] = record
            # 用 while 而非 if：max_clients 若被调小，一次要淘汰多条。
            while len(self._clients) > self._max_clients:
                self._clients.popitem(last=False)
        else:
            self._clients.move_to_end(client)
        return record


def client_key(host: str | None) -> str:
    """从连接信息推导限流键。

    只用 ``request.client.host``，**绝不读 X-Forwarded-For**：当前部署是
    compose 直接绑 127.0.0.1，没有反向代理，该头完全由客户端控制 ——
    信任它等于让攻击者每次换一个伪造 IP 就绕过限流。将来若真加了反向代理，
    需要显式配置「信任几层代理」再从右往左取，而不是无条件信任。
    """
    return host or "unknown"
