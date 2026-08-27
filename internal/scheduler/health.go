package scheduler

import (
	"sync"
	"time"
)

// FailureKind 区分失败类型。不同失败对健康度的含义完全不同 ——
// 429 是"这个 Key 被限了"，5xx 多半是上游抖动，401/403 则意味着 Key 已失效。
type FailureKind string

const (
	// Failure429 是上游限流。这是最需要在意的信号: 连续 429 往往是
	// 风控开始针对该 Key，继续压只会加速封禁。
	Failure429 FailureKind = "rate_limited"
	// Failure5xx 是上游服务端错误，通常与 Key 本身无关。
	Failure5xx FailureKind = "server_error"
	// FailureTimeout 是请求超时。
	FailureTimeout FailureKind = "timeout"
	// FailureQuota 是上游明确报额度耗尽。
	FailureQuota FailureKind = "quota_exhausted"
	// FailureAuth 是鉴权失败，Key 已失效或被封。不可恢复。
	FailureAuth FailureKind = "auth_failed"
)

// KeyStatus 是 Key 在调度器视角下的可用状态。
type KeyStatus string

const (
	StatusActive   KeyStatus = "active"
	StatusCooldown KeyStatus = "cooldown"
	StatusBanned   KeyStatus = "banned"
	StatusInvalid  KeyStatus = "invalid"
)

// 健康度调整量。取值参考 docs/scheduler-solution.md 7.2 节。
const (
	healthMax = 100
	healthMin = 0

	penalty429     = 10
	penalty5xx     = 5
	penaltyTimeout = 3
	penaltyQuota   = 8
	recoverSuccess = 2

	// consecutive429Cooldown 是触发冷却的连续 429 次数。
	consecutive429Cooldown = 3
	// cooldownDuration 是冷却时长。
	//
	// 取 10 分钟而非几秒: 429 之后立刻重试同一个 Key，本质上是在向风控
	// 证明"这是个不看反馈的脚本"。人类遇到限流会走开一会儿。
	cooldownDuration = 10 * time.Minute
	// healthCooldownFloor 是低于该健康度即进入冷却的阈值。
	healthCooldownFloor = 30
)

// KeyHealth 是单个 Key 的运行时健康状态。
type KeyHealth struct {
	KeyID           string
	Score           int
	Status          KeyStatus
	Consecutive429  int
	CooldownUntil   time.Time
	LastFailureKind FailureKind
	LastSelectedAt  time.Time
	Successes       int64
	Failures        int64
}

// healthTable 是内存中的健康度表。
//
// 为什么放内存而不放 Redis: 健康度是"本实例观测到的上游反馈"，只在
// 单实例内有意义（MVP 锁定单实例，P0-5），且丢失后从满分重新观测即可，
// 不需要持久化保证。放 Redis 只会给热路径加一次网络往返。
type healthTable struct {
	mu    sync.RWMutex
	items map[string]*KeyHealth
	now   func() time.Time
}

func newHealthTable(now func() time.Time) *healthTable {
	return &healthTable{items: map[string]*KeyHealth{}, now: now}
}

// ensure 返回（必要时创建）某 Key 的健康记录。调用方须持有写锁。
func (h *healthTable) ensure(keyID string) *KeyHealth {
	st, ok := h.items[keyID]
	if !ok {
		st = &KeyHealth{KeyID: keyID, Score: healthMax, Status: StatusActive}
		h.items[keyID] = st
	}
	return st
}

// get 返回健康记录的副本。
func (h *healthTable) get(keyID string) KeyHealth {
	h.mu.RLock()
	if st, ok := h.items[keyID]; ok {
		cp := *st
		h.mu.RUnlock()
		return cp
	}
	h.mu.RUnlock()
	return KeyHealth{KeyID: keyID, Score: healthMax, Status: StatusActive}
}

// seed 用库中的健康度初始化内存状态，仅在 Key 首次注册时生效。
func (h *healthTable) seed(keyID string, score int, status KeyStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.items[keyID]; ok {
		return // 已有实时观测值，不被库中的旧快照覆盖
	}
	if score <= 0 || score > healthMax {
		score = healthMax
	}
	if status == "" {
		status = StatusActive
	}
	h.items[keyID] = &KeyHealth{KeyID: keyID, Score: score, Status: status}
}

// markSuccess 记录一次成功，恢复健康度并清零连续 429 计数。
func (h *healthTable) markSuccess(keyID string) KeyHealth {
	h.mu.Lock()
	defer h.mu.Unlock()

	st := h.ensure(keyID)
	st.Successes++
	st.Consecutive429 = 0
	st.Score = min(st.Score+recoverSuccess, healthMax)
	// 成功即证明 Key 可用，从冷却中放出来。banned/invalid 是终态，不自动恢复。
	if st.Status == StatusCooldown {
		st.Status = StatusActive
		st.CooldownUntil = time.Time{}
	}
	return *st
}

// markFailure 按失败类型扣减健康度，必要时进入冷却或终态。
func (h *healthTable) markFailure(keyID string, kind FailureKind) KeyHealth {
	h.mu.Lock()
	defer h.mu.Unlock()

	st := h.ensure(keyID)
	st.Failures++
	st.LastFailureKind = kind
	now := h.now()

	switch kind {
	case Failure429:
		st.Consecutive429++
		// 连续 429 惩罚递增: 第 n 次扣 n*10，让"越撞越退"而非线性硬扛。
		st.Score -= penalty429 * st.Consecutive429
		if st.Consecutive429 >= consecutive429Cooldown {
			st.Status = StatusCooldown
			st.CooldownUntil = now.Add(cooldownDuration)
		}
	case Failure5xx:
		st.Consecutive429 = 0
		st.Score -= penalty5xx
	case FailureTimeout:
		st.Score -= penaltyTimeout
	case FailureQuota:
		// 上游报额度耗尽说明本地配额账与火山侧不一致，先冷却观察，
		// 由刷新探测器决定何时放回。
		st.Score -= penaltyQuota
		st.Status = StatusCooldown
		st.CooldownUntil = now.Add(cooldownDuration)
	case FailureAuth:
		// 鉴权失败不可能自愈，直接置终态并交由人工/管理接口处理。
		st.Score = healthMin
		st.Status = StatusInvalid
	default:
		st.Score -= penaltyTimeout
	}

	if st.Score < healthMin {
		st.Score = healthMin
	}
	if st.Score <= healthCooldownFloor && st.Status == StatusActive {
		st.Status = StatusCooldown
		st.CooldownUntil = now.Add(cooldownDuration)
	}
	return *st
}

// markSelected 记录选中时刻，用于最小请求间隔约束。
func (h *healthTable) markSelected(keyID string, at time.Time) {
	h.mu.Lock()
	h.ensure(keyID).LastSelectedAt = at
	h.mu.Unlock()
}

// available 判断 Key 当前是否可被调度，并顺带把已到期的冷却解除。
func (h *healthTable) available(keyID string) (KeyHealth, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	st := h.ensure(keyID)
	switch st.Status {
	case StatusBanned, StatusInvalid:
		return *st, false
	case StatusCooldown:
		if h.now().Before(st.CooldownUntil) {
			return *st, false
		}
		// 冷却到期: 回到 active，但不把健康度一次性拉满 —— 恢复要靠成功请求
		// 逐步累加，否则冷却过后又会立刻满负荷压上去。
		st.Status = StatusActive
		st.CooldownUntil = time.Time{}
		st.Consecutive429 = 0
		if st.Score < healthCooldownFloor+10 {
			st.Score = healthCooldownFloor + 10
		}
	}
	return *st, true
}

// setStatus 由管理接口/探测器显式设置状态（如封禁、恢复）。
func (h *healthTable) setStatus(keyID string, status KeyStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.ensure(keyID)
	st.Status = status
	if status == StatusActive {
		st.CooldownUntil = time.Time{}
		st.Consecutive429 = 0
		if st.Score < healthCooldownFloor+10 {
			st.Score = healthCooldownFloor + 10
		}
	}
}

// snapshotAll 返回全部健康记录的副本，供监控与看板读取。
func (h *healthTable) snapshotAll() map[string]KeyHealth {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]KeyHealth, len(h.items))
	for k, v := range h.items {
		out[k] = *v
	}
	return out
}
