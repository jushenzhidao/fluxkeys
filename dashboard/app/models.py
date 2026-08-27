"""看板 API 的响应模型。

所有模型都给出默认值，保证在数据为空（新部署、Redis 尚未写入热态）时仍能
序列化出结构完整的响应，而不是抛异常返回 500。
"""

from __future__ import annotations

from datetime import date, datetime

from pydantic import BaseModel, Field

# ---------------------------------------------------------------- 概览


class PoolBucket(BaseModel):
    """池 / 状态维度的计数。"""

    name: str
    count: int = 0


class OverviewResp(BaseModel):
    """全局概览。"""

    quota_day: date = Field(description="当前配额日（12:00 刷新口径）")
    quota_day_progress: float = Field(default=0.0, description="当前配额日已过去的比例 0~1")
    generated_at: datetime

    total_tokens: int = Field(default=0, description="今日配额日总 token 用量")
    prompt_tokens: int = 0
    completion_tokens: int = 0
    total_requests: int = Field(default=0, description="今日请求数")
    total_count_units: int = Field(default=0, description="今日按次计费用量")

    total_keys: int = Field(default=0, description="Key 总数")
    active_keys: int = Field(default=0, description="status=active 的 Key 数")
    used_keys: int = Field(default=0, description="今日有实际用量的 Key 数")

    pools: list[PoolBucket] = Field(default_factory=list, description="池分布")
    statuses: list[PoolBucket] = Field(default_factory=list, description="状态分布")

    avg_quota_ratio: float = Field(default=0.0, description="平均配额水位（含预扣）0~1")
    max_quota_ratio: float = Field(default=0.0, description="最高单 Key 水位")
    remaining_tokens: int = Field(default=0, description="全池剩余可用 token（相对硬水位）")
    capacity_tokens: int = Field(default=0, description="全池硬水位总量")

    error_rate: float = Field(default=0.0, description="今日错误率 0~1")
    error_requests: int = 0
    avg_latency_ms: int = 0

    unconfirmed_refresh: int = Field(default=0, description="刷新未确认的 Key 数（风险项）")
    alert_count: int = Field(default=0, description="配额健康告警条数")


# ---------------------------------------------------------------- Key


class KeyQuota(BaseModel):
    """单个 Key 在某类配额上的水位。"""

    kind: str = "token"
    used: int = 0
    prededuct: int = 0
    hard_limit: int = 0
    soft_limit: int = 0
    remaining: int = 0
    ratio: float = Field(default=0.0, description="(used+prededuct)/hard_limit")
    last_updated: datetime | None = None
    hot: bool = Field(default=False, description="Redis 中是否存在热态记录")


class KeyRow(BaseModel):
    """Key 列表中的一行。"""

    key_id: str
    pool: str = "cold"
    status: str = "active"
    persona_id: str = ""
    egress_ip: str = ""
    health_score: int = 100
    refresh_state: str = "idle"
    refresh_confirmed_at: datetime | None = None
    last_error: str = ""
    last_used_at: datetime | None = None

    token: KeyQuota = Field(default_factory=KeyQuota)
    count: KeyQuota = Field(default_factory=lambda: KeyQuota(kind="count"))

    today_tokens: int = Field(default=0, description="今日配额日已落库 token")
    today_requests: int = 0
    today_errors: int = 0


class KeyListResp(BaseModel):
    total: int = 0
    quota_day: date
    items: list[KeyRow] = Field(default_factory=list)


class KeyDayUsage(BaseModel):
    """单 Key 单配额日用量。"""

    quota_day: date
    total_tokens: int = 0
    prompt_tokens: int = 0
    completion_tokens: int = 0
    count_units: int = 0
    requests: int = 0
    errors: int = 0
    avg_latency_ms: int = 0


class ModelUsage(BaseModel):
    model: str
    requests: int = 0
    total_tokens: int = 0


class KeyDetailResp(BaseModel):
    key: KeyRow
    trend: list[KeyDayUsage] = Field(default_factory=list, description="近 N 配额日趋势")
    models: list[ModelUsage] = Field(default_factory=list, description="模型偏好分布")
    recent_errors: list[ErrorBucket] = Field(default_factory=list)
    lease_count: int = Field(default=0, description="当前未过期租约数")
    lease_amount: int = Field(default=0, description="当前未过期租约预扣总量")


# ---------------------------------------------------------------- 用量趋势


class TrendPoint(BaseModel):
    quota_day: date
    total_tokens: int = 0
    prompt_tokens: int = 0
    completion_tokens: int = 0
    count_units: int = 0
    requests: int = 0
    errors: int = 0
    active_keys: int = 0
    avg_latency_ms: int = 0
    error_rate: float = 0.0


class TrendResp(BaseModel):
    days: int
    quota_day: date
    points: list[TrendPoint] = Field(default_factory=list)


class UserUsageRow(BaseModel):
    """按用户计费口径聚合的一行。"""

    user_id: int | None = None
    user_name: str = "(未归属)"
    email: str | None = None
    status: str = ""
    daily_token_limit: int = 0
    total_tokens: int = 0
    prompt_tokens: int = 0
    completion_tokens: int = 0
    count_units: int = 0
    requests: int = 0
    errors: int = 0
    error_rate: float = 0.0
    avg_latency_ms: int = 0
    active_days: int = Field(default=0, description="有用量的配额日数")
    first_day: date | None = None
    last_day: date | None = None
    daily: list[TrendPoint] = Field(default_factory=list, description="该用户每日用量")


class UserUsageResp(BaseModel):
    days: int
    start_day: date
    end_day: date
    total_tokens: int = 0
    total_requests: int = 0
    items: list[UserUsageRow] = Field(default_factory=list)


# ---------------------------------------------------------------- 错误


class ErrorBucket(BaseModel):
    label: str
    count: int = 0
    ratio: float = 0.0


class ErrorKeyRow(BaseModel):
    key_id: str
    pool: str = ""
    status: str = ""
    errors: int = 0
    requests: int = 0
    error_rate: float = 0.0
    top_error: str = ""


class ErrorTimePoint(BaseModel):
    hour: datetime
    requests: int = 0
    errors: int = 0
    error_rate: float = 0.0


class ErrorsResp(BaseModel):
    hours: int
    since: datetime
    total_requests: int = 0
    total_errors: int = 0
    error_rate: float = 0.0
    by_code: list[ErrorBucket] = Field(default_factory=list)
    by_status: list[ErrorBucket] = Field(default_factory=list)
    by_key: list[ErrorKeyRow] = Field(default_factory=list)
    timeline: list[ErrorTimePoint] = Field(default_factory=list)


# ---------------------------------------------------------------- 出口 IP


class EgressRow(BaseModel):
    addr: str
    public_ip: str = ""
    region: str = ""
    isp: str = ""
    pool: str = Field(default="", description="档位 hot/warm/cold，空串表示不限档")
    state: str = "active"
    reputation: int = 100
    max_keys: int = 0
    bound_keys: int = Field(default=0, description="网关出口池中已绑定的 Key 数")
    db_bound_keys: int = Field(default=0, description="库中 egress_ip 指向该地址的 Key 数")
    active_keys: int = Field(default=0, description="其中状态 active 的 Key 数")
    load_ratio: float = Field(default=0.0, description="bound_keys/max_keys")
    today_tokens: int = 0
    today_requests: int = 0
    today_errors: int = 0
    error_rate: float = 0.0

    # 以下三项只在 state=banned 时有意义，是「等自愈」与「要人工介入」
    # 的唯一判据 —— 缺了它们，一次性被封与反复被封在面板上完全一样。
    banned_at: datetime | None = Field(default=None, description="最近一次被判定封禁的时刻")
    ban_count: int = Field(default=0, description="累计被封次数，决定退避档次")
    unban_at: datetime | None = Field(
        default=None,
        description=(
            "预计转入 cooldown 重新探测的时刻。"
            "state=banned 但此项为空表示未启用自动恢复，需人工处理"
        ),
    )


class EgressResp(BaseModel):
    quota_day: date
    total: int = 0
    unbound_keys: int = Field(default=0, description="未绑定出口 IP 的 Key 数")
    orphan_ips: list[str] = Field(
        default_factory=list,
        description="有用量记录但已不在网关出口池中的 IP（配置被移除或改名）",
    )
    # 网关不可达时状态列全是占位值。前端必须据此提示，
    # 否则「读不到状态」会被误当成「所有出口都健康」。
    stale: bool = Field(default=False, description="true 表示状态列不可信（网关未接入或不可达）")
    stale_reason: str = Field(default="", description="stale 为 true 时的原因")
    items: list[EgressRow] = Field(default_factory=list)


# ---------------------------------------------------------------- 配额健康


class QuotaAlert(BaseModel):
    """一条配额健康告警。"""

    level: str = Field(default="warn", description="warn / critical")
    kind: str = Field(description="告警类别：lease_leak / near_hard / drift / refresh")
    key_id: str = ""
    message: str = ""
    value: float = 0.0


class DriftRow(BaseModel):
    key_id: str
    billing_kind: str = "token"
    quota_day: date
    drift: int = 0
    created_at: datetime


class LeaseLeakRow(BaseModel):
    key_id: str
    prededuct: int = 0
    lease_amount: int = Field(default=0, description="未过期租约求和")
    lease_count: int = 0
    gap: int = Field(default=0, description="prededuct - lease_amount，> 0 说明有泄漏")
    ratio: float = Field(default=0.0, description="prededuct/hard_limit")


class NearHardRow(BaseModel):
    key_id: str
    pool: str = ""
    used: int = 0
    prededuct: int = 0
    hard_limit: int = 0
    ratio: float = 0.0


class QuotaHealthResp(BaseModel):
    quota_day: date
    checked_keys: int = 0
    lease_zset_size: int = 0
    expired_leases: int = Field(default=0, description="ZSET 中已过期但未回收的租约数")
    alerts: list[QuotaAlert] = Field(default_factory=list)
    lease_leaks: list[LeaseLeakRow] = Field(default_factory=list)
    near_hard: list[NearHardRow] = Field(default_factory=list)
    drifts: list[DriftRow] = Field(default_factory=list)
    missing_hot_keys: list[str] = Field(
        default_factory=list, description="active 但 Redis 无热态的 Key"
    )


# ---------------------------------------------------------------- 刷新状态


class RefreshKeyRow(BaseModel):
    key_id: str
    pool: str = ""
    status: str = ""
    refresh_state: str = "idle"
    refresh_confirmed_at: datetime | None = None
    last_error: str = ""
    used_after_refresh: int = Field(default=0, description="配额日内已消耗 token")
    hot_used: int = Field(default=0, description="Redis 热态 used")


class RefreshStatusResp(BaseModel):
    quota_day: date
    in_window: bool = Field(default=False, description="当前是否处于 12:00-14:00 探测窗口")
    window_start: datetime
    window_end: datetime
    states: list[PoolBucket] = Field(default_factory=list)
    confirmed: int = 0
    unconfirmed: int = 0
    unconfirmed_keys: list[RefreshKeyRow] = Field(
        default_factory=list, description="未确认刷新的 Key，可能已超刷"
    )
    risky_keys: list[RefreshKeyRow] = Field(
        default_factory=list, description="未确认却仍在消耗的 Key（高风险）"
    )


# ---------------------------------------------------------------- 行为相似度


class SimilarPair(BaseModel):
    key_a: str
    key_b: str
    similarity: float = 0.0
    hour_similarity: float = 0.0
    model_similarity: float = 0.0
    egress_a: str = ""
    egress_b: str = ""
    same_egress: bool = False
    level: str = "warn"


class KeyBehaviorProfile(BaseModel):
    key_id: str
    requests: int = 0
    hour_hist: list[float] = Field(default_factory=list, description="24 维请求时段分布")
    top_models: list[str] = Field(default_factory=list)
    peak_hour: int | None = None
    entropy: float = Field(default=0.0, description="时段分布熵，越低越集中越像机器")


class SimilarityResp(BaseModel):
    days: int
    start_day: date
    end_day: date
    threshold: float = 0.6
    sampled_keys: int = 0
    compared_pairs: int = 0
    alert_pairs: int = 0
    max_similarity: float = 0.0
    avg_similarity: float = 0.0
    pairs: list[SimilarPair] = Field(default_factory=list)
    profiles: list[KeyBehaviorProfile] = Field(default_factory=list)
    note: str = ""


# ---------------------------------------------------------------- 健康检查


class HealthResp(BaseModel):
    status: str = "ok"
    postgres: str = "unknown"
    redis: str = "unknown"
    quota_day: date
    version: str = ""


KeyDetailResp.model_rebuild()
