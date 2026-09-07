package scheduler

import (
	"github.com/fluxkeys/fluxkeys/internal/persona"
	"github.com/fluxkeys/fluxkeys/internal/quota"
	"github.com/fluxkeys/fluxkeys/internal/store"
)

// Score 是一次打分的明细，保留各维度分量便于排查"为什么选了这个 Key"。
type Score struct {
	KeyID string

	Quota   float64 // S_quota   [0,100]
	History float64 // S_history [-15,40]
	Persona float64 // S_persona [0,100]
	Health  float64 // S_health  [0,100]

	SoftPenalty float64 // 越过软水位的扣分
	Total       float64 // 加权总分（下限截断到 0）
}

// scoreQuota 计算实时剩余配额分。
//
// 依据 docs/scheduler-solution.md 7.2 节:
//
//	剩余比例 > 0.5   : +10 鼓励使用（额度用不完本身是浪费）
//	0.2 < 比例 <= 0.5: 正常
//	比例 <= 0.2      : -10 进入紧张模式
//
// 注意: 这里读的是**内存快照**（容忍 1-2s 陈旧），只用于排序。
// 准入与否完全由 quota.Acquire 的 Lua 返回值决定（P0-1）。
func scoreQuota(snap quota.Snapshot) float64 {
	if snap.Hard <= 0 {
		// 快照缺失（Key 今日还没有任何请求，Redis 里没有这个 hash）。
		// 视为满额: 这类 Key 恰恰是最该被用的。
		return 100
	}
	remainRatio := float64(snap.Remaining()) / float64(snap.Hard)
	if remainRatio < 0 {
		remainRatio = 0
	}
	if remainRatio > 1 {
		remainRatio = 1
	}

	s := remainRatio * 100
	switch {
	case remainRatio > 0.5:
		s += 10
	case remainRatio <= 0.2:
		s -= 10
	}
	return clamp(s, 0, 100)
}

// scoreHistory 计算跨日历史分。
//
// 目的是让配额消耗在 1000 个 Key 之间长期均衡: 昨日用得少的今天多用一点。
// 更重要的是那条 -15 的惩罚 —— 昨日刷满（>95%）的 Key 今天必须让位，
// 连续每日刷满是账号被盯上的最强特征。
func scoreHistory(h store.KeyDailyHistory, hasHistory bool) float64 {
	if !hasHistory {
		// 无历史（新 Key 或昨日完全没用）→ 给满额的基础分。
		return 25
	}
	ratio := h.TokenRatio
	if ratio <= 0 && h.TokenLimit > 0 {
		// 与 store 层的回退保持同一口径: 按次计费的上游 token_used 恒为 0，
		// 只看 token 会让这些 Key 永远拿到满额历史分 —— 正好与 -15 那条
		// 「昨日刷满必须让位」的反封禁规则相反，越是被刷穿的 Key 越优先。
		used := h.TokenUsed
		if used == 0 && h.CountUsed > 0 {
			used = int64(h.CountUsed)
		}
		ratio = float64(used) / float64(h.TokenLimit)
	}

	if ratio > 0.95 {
		return -15
	}

	base := (1 - ratio) * 25
	bonus := float64(h.ConsecutiveLightDays) * 3
	if bonus > 15 {
		bonus = 15
	}
	return clamp(base+bonus, -15, 40)
}

// scorePersona 计算行为匹配分。
//
// 返回 (分数, 是否淘汰)。不在活跃时段直接淘汰 —— 一个"夜猫子"Key 在早上
// 8 点持续发请求，是比配额曲线更显眼的机器特征。
func scorePersona(p *persona.Persona, req Request, enabled bool) (float64, bool) {
	if !enabled || p == nil {
		return 100, false
	}
	if !p.IsActiveAt(req.Now) {
		return 0, true
	}
	// 时段内满分打底，再按模型偏好度调整。偏好度最低 0.3，
	// 因此即使是画像不偏好的模型也不会被这一维彻底压死。
	affinity := p.ModelAffinity(req.Model)
	return clamp(60+40*affinity, 0, 100), false
}

// scoreHealth 计算健康度分。
func scoreHealth(hs KeyHealth) float64 {
	return clamp(float64(hs.Score), 0, 100)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
