"""行为相似度计算的单元测试。纯函数，无需外部依赖。"""

from __future__ import annotations

import math

import pytest

from app.similarity import (
    BehaviorVector,
    analyze,
    build_vectors,
    compare,
    cosine,
    cosine_list,
)


def make_vector(key_id: str, hours: dict[int, int], models: dict[str, int]) -> BehaviorVector:
    """构造行为向量的测试辅助函数。"""
    vec = BehaviorVector(key_id=key_id)
    for hour, count in hours.items():
        vec.hour_counts[hour] = float(count)
        vec.requests += count
    vec.model_counts = {k: float(v) for k, v in models.items()}
    return vec


class TestCosine:
    def test_identical_is_one(self) -> None:
        assert cosine_list([1.0, 2.0, 3.0], [1.0, 2.0, 3.0]) == pytest.approx(1.0)

    def test_proportional_is_one(self) -> None:
        """余弦只看方向：等比放缩后相似度仍为 1。"""
        assert cosine_list([1.0, 2.0], [10.0, 20.0]) == pytest.approx(1.0)

    def test_orthogonal_is_zero(self) -> None:
        assert cosine_list([1.0, 0.0], [0.0, 1.0]) == 0.0

    def test_zero_vector_is_zero_not_nan(self) -> None:
        """零向量不能除零产生 NaN。"""
        assert cosine_list([0.0, 0.0], [1.0, 1.0]) == 0.0
        assert cosine_list([], []) == 0.0

    def test_length_mismatch_is_zero(self) -> None:
        assert cosine_list([1.0], [1.0, 2.0]) == 0.0

    def test_sparse_disjoint_keys(self) -> None:
        assert cosine({"a": 1.0}, {"b": 1.0}) == 0.0

    def test_sparse_partial_overlap(self) -> None:
        value = cosine({"a": 1.0, "b": 1.0}, {"a": 1.0})
        assert value == pytest.approx(1 / math.sqrt(2))

    def test_sparse_empty(self) -> None:
        assert cosine({}, {"a": 1.0}) == 0.0

    def test_result_within_unit_interval(self) -> None:
        value = cosine_list([3.0, 4.0], [3.0, 4.0])
        assert 0.0 <= value <= 1.0


class TestCompare:
    def test_identical_behavior_is_fully_similar(self) -> None:
        a = make_vector("k1", {9: 10, 10: 20}, {"m1": 30})
        b = make_vector("k2", {9: 10, 10: 20}, {"m1": 30})
        pair = compare(a, b)
        assert pair.similarity == pytest.approx(1.0)
        assert pair.hour_similarity == pytest.approx(1.0)
        assert pair.model_similarity == pytest.approx(1.0)

    def test_disjoint_hours_and_models(self) -> None:
        a = make_vector("k1", {1: 10}, {"m1": 10})
        b = make_vector("k2", {13: 10}, {"m2": 10})
        assert compare(a, b).similarity == 0.0

    def test_same_hours_different_models(self) -> None:
        """时段权重 0.6，模型权重 0.4：时段全同、模型全异 → 0.6。"""
        a = make_vector("k1", {9: 10}, {"m1": 10})
        b = make_vector("k2", {9: 10}, {"m2": 10})
        assert compare(a, b).similarity == pytest.approx(0.6)

    def test_same_models_different_hours(self) -> None:
        a = make_vector("k1", {9: 10}, {"m1": 10})
        b = make_vector("k2", {21: 10}, {"m1": 10})
        assert compare(a, b).similarity == pytest.approx(0.4)

    def test_pair_order_is_stable(self) -> None:
        """无论传入顺序，key_a/key_b 都按字典序输出，便于去重比对。"""
        a = make_vector("z_key", {9: 10}, {"m": 10})
        b = make_vector("a_key", {9: 10}, {"m": 10})
        assert compare(a, b).key_a == "a_key"
        assert compare(b, a).key_a == "a_key"

    def test_scale_invariance(self) -> None:
        """一个 Key 请求量是另一个的 100 倍，只要分布一致就应判定相似。

        这是关键性质：风控看的是行为「形状」，不是绝对量级。
        """
        a = make_vector("k1", {9: 1, 10: 2}, {"m": 3})
        b = make_vector("k2", {9: 100, 10: 200}, {"m": 300})
        assert compare(a, b).similarity == pytest.approx(1.0)


class TestBehaviorVector:
    def test_hour_hist_normalized(self) -> None:
        vec = make_vector("k", {0: 1, 12: 3}, {})
        hist = vec.hour_hist
        assert sum(hist) == pytest.approx(1.0)
        assert hist[0] == pytest.approx(0.25)
        assert hist[12] == pytest.approx(0.75)

    def test_empty_hist_is_all_zero(self) -> None:
        vec = BehaviorVector(key_id="k")
        assert vec.hour_hist == [0.0] * 24
        assert vec.peak_hour is None
        assert vec.entropy == 0.0

    def test_peak_hour(self) -> None:
        assert make_vector("k", {3: 5, 18: 9}, {}).peak_hour == 18

    def test_entropy_single_hour_is_zero(self) -> None:
        """全部请求集中在一个小时 → 熵为 0，机器特征最明显。"""
        assert make_vector("k", {9: 100}, {}).entropy == pytest.approx(0.0)

    def test_entropy_uniform_is_max(self) -> None:
        """24 小时均匀分布 → 熵为 log2(24)。"""
        vec = make_vector("k", dict.fromkeys(range(24), 1), {})
        assert vec.entropy == pytest.approx(math.log2(24))

    def test_top_models_sorted_by_count(self) -> None:
        vec = make_vector("k", {9: 1}, {"a": 1, "b": 5, "c": 3})
        assert vec.top_models(2) == ["b", "c"]


class TestAnalyze:
    def test_empty_input(self) -> None:
        report = analyze([], threshold=0.6)
        assert report.compared_pairs == 0
        assert report.pairs == []
        assert report.max_similarity == 0.0

    def test_single_key_no_pairs(self) -> None:
        vec = make_vector("k1", {9: 50}, {"m": 50})
        assert analyze([vec], threshold=0.6).compared_pairs == 0

    def test_filters_low_sample_keys(self) -> None:
        """请求数不足的 Key 被排除，避免偶然重合误报。"""
        a = make_vector("k1", {9: 5}, {"m": 5})
        b = make_vector("k2", {9: 5}, {"m": 5})
        report = analyze([a, b], threshold=0.6, min_requests=20)
        assert report.compared_pairs == 0

    def test_detects_similar_pair(self) -> None:
        a = make_vector("k1", {9: 30, 10: 30}, {"m": 60})
        b = make_vector("k2", {9: 30, 10: 30}, {"m": 60})
        report = analyze([a, b], threshold=0.6, min_requests=20)
        assert report.compared_pairs == 1
        assert report.alert_pairs == 1
        assert report.pairs[0].similarity == pytest.approx(1.0)

    def test_ignores_dissimilar_pair(self) -> None:
        a = make_vector("k1", {2: 30}, {"m1": 30})
        b = make_vector("k2", {15: 30}, {"m2": 30})
        report = analyze([a, b], threshold=0.6, min_requests=20)
        assert report.compared_pairs == 1
        assert report.alert_pairs == 0
        assert report.pairs == []

    def test_pairs_count_is_combinatorial(self) -> None:
        vectors = [make_vector(f"k{i}", {i % 24: 30}, {"m": 30}) for i in range(5)]
        assert analyze(vectors, threshold=0.6, min_requests=20).compared_pairs == 10

    def test_pairs_sorted_desc_and_truncated(self) -> None:
        vectors = [
            make_vector("k1", {9: 30, 10: 30}, {"m": 60}),
            make_vector("k2", {9: 30, 10: 30}, {"m": 60}),
            make_vector("k3", {9: 30, 10: 29}, {"m": 59}),
        ]
        report = analyze(vectors, threshold=0.5, min_requests=20, top_n=2)
        assert len(report.pairs) == 2
        assert report.pairs[0].similarity >= report.pairs[1].similarity

    def test_avg_and_max(self) -> None:
        a = make_vector("k1", {9: 30}, {"m": 30})
        b = make_vector("k2", {9: 30}, {"m": 30})
        c = make_vector("k3", {20: 30}, {"x": 30})
        report = analyze([a, b, c], threshold=0.6, min_requests=20)
        assert report.max_similarity == pytest.approx(1.0)
        assert 0.0 < report.avg_similarity < 1.0


class TestBuildVectors:
    def test_assembles_two_dimensions(self) -> None:
        vectors = build_vectors(
            [("k1", 9, 10), ("k1", 10, 5), ("k2", 9, 3)],
            [("k1", "gpt", 15), ("k2", "gpt", 3)],
        )
        assert [v.key_id for v in vectors] == ["k1", "k2"]
        assert vectors[0].requests == 15
        assert vectors[0].hour_counts[9] == 10.0
        assert vectors[0].model_counts == {"gpt": 15.0}

    def test_skips_invalid_hour(self) -> None:
        """脏数据（hour 越界）被忽略，不污染 24 维向量。"""
        vectors = build_vectors([("k1", 24, 10), ("k1", -1, 5), ("k1", 9, 2)], [])
        assert vectors[0].requests == 2

    def test_skips_empty_key_id(self) -> None:
        assert build_vectors([("", 9, 10)], [("", "m", 10)]) == []

    def test_empty_model_name_normalized(self) -> None:
        vectors = build_vectors([("k1", 9, 1)], [("k1", "", 1)])
        assert "(未指定)" in vectors[0].model_counts

    def test_model_only_key_has_zero_requests(self) -> None:
        """只有模型维度没有时段维度的 Key 请求数为 0，会被 analyze 过滤。"""
        vectors = build_vectors([], [("k1", "m", 10)])
        assert vectors[0].requests == 0
        assert analyze(vectors, threshold=0.6).compared_pairs == 0
