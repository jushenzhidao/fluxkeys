"""行为相似度自检（离线报表）。

火山商务反馈的封禁根因是「用户行为规律相似」——多个 Key 若在请求时段分布和
模型偏好上高度一致，风控就能把它们聚成一簇，判定为同一主体批量注册。

本模块把每个 Key 的行为压成两个概率向量：

1. **时段分布**：24 维，第 i 维 = 该 Key 在 i 点发起的请求占比；
2. **模型偏好**：维度 = 出现过的模型集合，值为各模型请求占比。

综合相似度 = 0.6 × 时段余弦 + 0.4 × 模型余弦。时段权重更高，因为「什么时候
用」比「用什么模型」更难伪装，也是风控最容易抓的特征。

**这是离线报表，不在请求热路径上计算**（V4 已把实时相似度判定移出热路径）。
纯函数实现，不依赖 numpy，1000 个 Key 的两两比较量级在秒内可完成。
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field

# 综合相似度中时段分布与模型偏好的权重。
HOUR_WEIGHT = 0.6
MODEL_WEIGHT = 0.4

# 请求数低于此值的 Key 行为样本太少，比较结果没有统计意义。
DEFAULT_MIN_REQUESTS = 20


@dataclass(slots=True)
class BehaviorVector:
    """单个 Key 的行为向量。"""

    key_id: str
    requests: int = 0
    # 24 维原始请求计数，索引即小时。
    hour_counts: list[float] = field(default_factory=lambda: [0.0] * 24)
    # 模型 -> 请求数
    model_counts: dict[str, float] = field(default_factory=dict)

    @property
    def hour_hist(self) -> list[float]:
        """归一化后的 24 维时段分布。总量为 0 时返回全零。"""
        return _normalize_list(self.hour_counts)

    @property
    def peak_hour(self) -> int | None:
        """请求量最高的小时。无请求时返回 None。"""
        if not any(self.hour_counts):
            return None
        return max(range(24), key=lambda h: self.hour_counts[h])

    @property
    def entropy(self) -> float:
        """时段分布的香农熵（以 2 为底，单位 bit）。

        熵越低说明请求越集中在少数时段，越像脚本定时跑；均匀分布的 24 维
        向量熵为 log2(24) ≈ 4.585。这是一个辅助指标，供人工判断。
        """
        hist = self.hour_hist
        total = sum(hist)
        if total <= 0:
            return 0.0
        acc = 0.0
        for value in hist:
            if value > 0:
                acc -= value * math.log2(value)
        return acc

    def top_models(self, limit: int = 3) -> list[str]:
        return [
            name
            for name, _ in sorted(self.model_counts.items(), key=lambda kv: (-kv[1], kv[0]))[:limit]
        ]


def _normalize_list(values: list[float]) -> list[float]:
    total = sum(values)
    if total <= 0:
        return [0.0] * len(values)
    return [v / total for v in values]


def cosine(a: dict[str, float], b: dict[str, float]) -> float:
    """稀疏向量余弦相似度。任一向量为零向量时返回 0。

    结果裁剪到 ``[0, 1]``：输入均为非负计数，理论上不会出现负值，
    裁剪只用于消除浮点误差导致的 1.0000000002。
    """
    if not a or not b:
        return 0.0
    # 遍历较短的一方求内积
    if len(a) > len(b):
        a, b = b, a
    dot = 0.0
    for key, value in a.items():
        other = b.get(key)
        if other:
            dot += value * other
    if dot == 0.0:
        return 0.0
    norm_a = math.sqrt(sum(v * v for v in a.values()))
    norm_b = math.sqrt(sum(v * v for v in b.values()))
    if norm_a == 0.0 or norm_b == 0.0:
        return 0.0
    return min(1.0, max(0.0, dot / (norm_a * norm_b)))


def cosine_list(a: list[float], b: list[float]) -> float:
    """等长稠密向量余弦相似度。"""
    if len(a) != len(b) or not a:
        return 0.0
    # strict=True 让「等长」这个前提显式化：上面已校验，此处若不等应当立即失败
    # 而非静默截断成较短的一方，那会让相似度偏高。
    dot = sum(x * y for x, y in zip(a, b, strict=True))
    if dot == 0.0:
        return 0.0
    norm_a = math.sqrt(sum(x * x for x in a))
    norm_b = math.sqrt(sum(y * y for y in b))
    if norm_a == 0.0 or norm_b == 0.0:
        return 0.0
    return min(1.0, max(0.0, dot / (norm_a * norm_b)))


@dataclass(slots=True)
class PairSimilarity:
    """一对 Key 的相似度结果。"""

    key_a: str
    key_b: str
    hour_similarity: float
    model_similarity: float
    similarity: float


def compare(a: BehaviorVector, b: BehaviorVector) -> PairSimilarity:
    """计算两个 Key 的加权相似度。"""
    hour_sim = cosine_list(a.hour_hist, b.hour_hist)
    model_sim = cosine(_normalize_dict(a.model_counts), _normalize_dict(b.model_counts))
    combined = HOUR_WEIGHT * hour_sim + MODEL_WEIGHT * model_sim
    # key_a/key_b 按字典序固定，保证同一对 Key 的输出稳定可比。
    first, second = a.key_id, b.key_id
    if first > second:
        first, second = second, first
    return PairSimilarity(
        key_a=first,
        key_b=second,
        hour_similarity=round(hour_sim, 6),
        model_similarity=round(model_sim, 6),
        similarity=round(min(1.0, max(0.0, combined)), 6),
    )


def _normalize_dict(counts: dict[str, float]) -> dict[str, float]:
    total = sum(counts.values())
    if total <= 0:
        return {}
    return {k: v / total for k, v in counts.items()}


@dataclass(slots=True)
class SimilarityReport:
    """相似度自检报告的计算结果（不含 IO）。"""

    pairs: list[PairSimilarity] = field(default_factory=list)
    compared_pairs: int = 0
    alert_pairs: int = 0
    max_similarity: float = 0.0
    avg_similarity: float = 0.0


def analyze(
    vectors: list[BehaviorVector],
    *,
    threshold: float = 0.6,
    min_requests: int = DEFAULT_MIN_REQUESTS,
    top_n: int = 50,
) -> SimilarityReport:
    """两两比较所有 Key，返回超过阈值的 Key 对。

    - 样本不足（请求数 < ``min_requests``）的 Key 直接排除，避免偶然重合造成误报。
    - 不足 2 个可比 Key 时返回空报告，而不是抛异常。
    - ``pairs`` 按相似度降序，最多 ``top_n`` 条。
    """
    usable = [v for v in vectors if v.requests >= min_requests]
    report = SimilarityReport()
    if len(usable) < 2:
        return report

    total = 0.0
    alerts: list[PairSimilarity] = []
    for i in range(len(usable)):
        for j in range(i + 1, len(usable)):
            pair = compare(usable[i], usable[j])
            report.compared_pairs += 1
            total += pair.similarity
            report.max_similarity = max(report.max_similarity, pair.similarity)
            if pair.similarity > threshold:
                alerts.append(pair)

    if report.compared_pairs:
        report.avg_similarity = round(total / report.compared_pairs, 6)
    report.alert_pairs = len(alerts)
    alerts.sort(key=lambda p: (-p.similarity, p.key_a, p.key_b))
    report.pairs = alerts[:top_n]
    return report


def build_vectors(
    hour_rows: list[tuple[str, int, int]],
    model_rows: list[tuple[str, str, int]],
) -> list[BehaviorVector]:
    """把数据库聚合结果组装成行为向量。

    ``hour_rows`` 为 ``(key_id, hour, requests)``，``hour`` 超出 0-23 的脏数据被忽略。
    ``model_rows`` 为 ``(key_id, model, requests)``。
    Key 的 ``requests`` 以时段维度求和为准（两个维度理论上应一致）。
    """
    vectors: dict[str, BehaviorVector] = {}

    for key_id, hour, requests in hour_rows:
        if not key_id or not 0 <= hour <= 23:
            continue
        vec = vectors.setdefault(key_id, BehaviorVector(key_id=key_id))
        vec.hour_counts[hour] += float(requests)
        vec.requests += int(requests)

    for key_id, model, requests in model_rows:
        if not key_id:
            continue
        vec = vectors.setdefault(key_id, BehaviorVector(key_id=key_id))
        name = model or "(未指定)"
        vec.model_counts[name] = vec.model_counts.get(name, 0.0) + float(requests)

    return [vectors[k] for k in sorted(vectors)]
