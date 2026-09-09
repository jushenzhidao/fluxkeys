// Package scheduler 从火山 Key 池中选出本次请求应使用的 Key。
//
// # 算法
//
//	Score = w1*S_quota + w2*S_history + w3*S_persona + w4*S_health - softPenalty
//	选择 = 按 Score 加权随机（而非取最高分）
//
// 取加权随机而非最高分的理由: 若总选最高分，配额曲线会呈现"一个 Key 被榨干、
// 下一个 Key 接棒"的阶梯状，这在风控侧是极其醒目的批量特征。加权随机让
// 1000 个 Key 的消耗曲线自然发散。
//
// # 相对 V3 的收敛（P1-7）
//
// V3 为调度设计了分数分桶采样、RCU 无锁快照、活跃 Key 位图，目标 P99 < 1ms
// @ 3000 QPS。V4 把这些全部删除:
//
//	真实流量 20 QPS，活跃池 100 个 Key。朴素遍历 100 个 Key 打分是微秒量级
//	（见 BenchmarkSelect），相对 3 秒级的上游 LLM 调用完全不可测。分桶采样
//	引入的桶边界维护、RCU 引入的内存序推理，其复杂度代价远超收益，且每一处
//	都是并发 bug 的滋生地。
//
// # 与配额层的边界（P0-1，最重要的约束）
//
// 本包持有的配额快照是**只读、容忍陈旧（1-2s）的排序依据**，绝不参与准入。
// 准入唯一权威是 quota.Acquire 的 Lua 返回值。调度器选出 Key 只意味着
// "这个 Key 看起来最合适"，调用方仍必须 Acquire 成功才能真正发请求；
// Acquire 被拒时应把该 Key 加入 exclude 重新 Select。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/persona"
	"github.com/fluxkeys/fluxkeys/internal/quota"
	"github.com/fluxkeys/fluxkeys/internal/store"
)

// ErrNoCandidate 表示当前无可用 Key。
var ErrNoCandidate = errors.New("scheduler: 无可用 Key")

// Request 是一次调度请求的输入。
type Request struct {
	// Provider 是上游服务商，调度器按此过滤 Key 池。
	Provider string
	// Model 是对外模型名，用于 persona 偏好匹配。
	Model string
	// Kind 是计费类型，决定读取哪一套配额快照。
	Kind quota.Kind
	// Now 是决策时刻。零值时由调度器填入当前时间。
	Now time.Time
	// Exclude 是本次请求已尝试过的 Key，用于重试换 Key。
	Exclude map[string]bool
}

// Candidate 是调度结果。
type Candidate struct {
	KeyID string
	// Secret 是解密后的火山密钥。
	Secret string
	// EgressIP 是该 Key 终身绑定的出口地址。
	EgressIP string
	// Pool 是该 Key 的档位（hot / warm / cold），供出口分层使用。
	Pool string
	// Persona 是该 Key 的行为画像，供上层做请求整形（如退避抖动）。
	Persona *persona.Persona
	// Score 是本次打分明细，便于排查选择原因。
	Score Score
	// Snapshot 是决策时读到的配额快照（可能陈旧，不可用于准入判断）。
	Snapshot quota.Snapshot
	// CandidateCount 是本次参与竞争的候选数，用于可观测性。
	CandidateCount int
	// PickedPool 是本次抽样命中的档位，空串表示未启用配比。
	//
	// 与 Pool 的区别: 正常情况下两者相同，但 PoolFallback 生效时
	// PickedPool 记录的是「原本该抽中的档」，用于观测回退频率。
	PickedPool string
	// PoolFellBack 标记本次是否发生了跨档回退。
	PoolFellBack bool
}

// 档位名。与 volc_keys.pool 的取值、以及 egress 侧的出口分层一一对应。
const (
	PoolHot  = "hot"
	PoolWarm = "warm"
	PoolCold = "cold"
)

// keyEntry 是 Key 的静态元数据（来自 Postgres，由 Reload 刷新）。
type keyEntry struct {
	keyID     string
	secret    string
	provider  string // 上游服务商
	egressIP  string
	pool      string
	dbStatus  string
	persona   *persona.Persona
	hardLimit int64
	softLimit int64
}

// poolOf 返回该 Key 的档位，空值归入 cold。
//
// 与 schema.sql 的 `pool TEXT NOT NULL DEFAULT 'cold'` 保持一致:
// 元数据缺失时按最保守的档位对待 —— 宁可少给流量，也不要把来源不明的
// Key 误当成 hot 而让它承接大量请求。
func (e keyEntry) poolOf() string {
	if e.pool == "" {
		return PoolCold
	}
	return e.pool
}

// Store 是调度器对持久化层的最小依赖，便于测试替换。
type Store interface {
	ListUpstreamKeys(ctx context.Context, filter store.UpstreamKeyFilter) ([]store.UpstreamKey, error)
	GetKeyHistory(ctx context.Context, keyIDs []string, quotaDay time.Time) (map[store.HistoryKey]store.KeyDailyHistory, error)
}

// QuotaReader 是调度器对配额层的最小依赖（只读！）。
//
// 这个接口刻意只暴露读方法: 从类型上就断绝调度器写配额的可能，
// 让 P0-1 的约束由编译器而非注释来保证。
type QuotaReader interface {
	GetMany(ctx context.Context, provider string, keyIDs []string, kind quota.Kind) (map[string]quota.Snapshot, error)
}

// EgressReader 是调度器对出口层的最小依赖（只读！）。
//
// 存在的理由: MinRequestInterval 只约束单个 Key，约束不了出口。一个 IP 上
// 25 个 Key 各自满足 5 秒间隔，IP 层面仍可达 5 QPS —— 而风控看到的是 IP。
// 分级配比把大部分流量集中到 hot 档后，这个缺口尤其明显。
//
// 与 QuotaReader 同理，接口刻意只读: 出口状态的写入归 gateway 的请求路径，
// 调度器只筛选，不改出口。
type EgressReader interface {
	// LastUsedOn 返回该 Key 绑定的出口最近一次发起请求的时刻。
	// Key 未绑定出口（direct 模式）时返回零值，调用方须视为「无约束」。
	LastUsedOn(keyID string) time.Time
}

// ConfigSource 提供调度所需的配置读取。
//
// 定成接口而非直接持 config 值，是为了让配置能热切。此前 Scheduler 持的是
// config.Scheduler 值拷贝，New 也按值传 —— 热切后调度器手里仍是启动那一刻的
// 副本，权重、画像开关、水位改了全都不生效，而管理页面会照常显示「配置已
// 生效」。操作成功但系统行为与用户心智模型不符，是这里最不能接受的失效。
//
// 由调用方（cmd/gateway 的装配层）用配置快照实现，调度器不认识 confsnap，
// 也就不会因此间接依赖 adapter。
type ConfigSource interface {
	// Scheduler 返回当前生效的调度配置。
	Scheduler() config.Scheduler
	// Quota 返回当前生效的全局配额配置。
	Quota() config.Quota
	// LimitsFor 返回指定 provider 在该量纲下的软硬水位。
	LimitsFor(provider string, kindCount bool) (hard, soft int64)
	// IsCountProvider 判断该 provider 的额度量纲是否为「按次」。
	IsCountProvider(provider string) bool
}

// StaticConfig 是固定不变的配置源，包一份 *config.Config。
//
// 仅供测试与一次性工具使用。生产装配必须传基于配置快照的实现 ——
// 用这个等于把启动时的配置钉死，正是 ConfigSource 要解决的问题。
//
// 水位与量纲一律委托给 config.Config 的既有方法，不在这里重算: 那两处
// 「provider 额度优先、比例取全局」的规则改了以后，复制品不会跟着改，
// 而两边算出来的都是正常数字，对不上也没人会发现。
type StaticConfig struct{ Cfg *config.Config }

func (s StaticConfig) Scheduler() config.Scheduler { return s.Cfg.Scheduler }
func (s StaticConfig) Quota() config.Quota         { return s.Cfg.Quota }

func (s StaticConfig) LimitsFor(provider string, kindCount bool) (int64, int64) {
	return s.Cfg.LimitsFor(provider, kindCount)
}

func (s StaticConfig) IsCountProvider(provider string) bool {
	return s.Cfg.IsCountProvider(provider)
}

// Scheduler 是调度引擎。
type Scheduler struct {
	conf   ConfigSource
	st     Store
	qr     QuotaReader
	health *healthTable

	// eg 为 nil 时不做出口级节流。出口池的构造晚于调度器（它依赖配置解析
	// 出的 IP 列表），故用 SetEgressReader 注入而非 New 的参数。
	egMu sync.RWMutex
	eg   EgressReader

	now func() time.Time

	mu      sync.RWMutex
	keys    []keyEntry                                 // 活跃池静态元数据
	history map[store.HistoryKey]store.KeyDailyHistory // 昨日归档
	snaps   map[quota.Kind]map[string]quota.Snapshot   // 配额快照（只读用于排序）

	rndMu sync.Mutex
	rnd   *rand.Rand

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// New 创建调度器。调用方需自行调用 Reload 装载 Key，并按需 Start 后台刷新。
func New(conf ConfigSource, st Store, qr QuotaReader) *Scheduler {
	now := time.Now
	return &Scheduler{
		conf:    conf,
		st:      st,
		qr:      qr,
		health:  newHealthTable(now),
		now:     now,
		history: map[store.HistoryKey]store.KeyDailyHistory{},
		snaps:   map[quota.Kind]map[string]quota.Snapshot{},
		rnd:     rand.New(rand.NewSource(time.Now().UnixNano())),
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
}

// SetClock 替换时钟，仅供测试使用。
func (s *Scheduler) SetClock(f func() time.Time) {
	s.now = f
	s.health.now = f
}

// SetEgressReader 注入出口只读视图，启用出口级最小间隔约束。
//
// 不注入时（direct 模式或未配置 EgressMinInterval）调度器行为不变。
func (s *Scheduler) SetEgressReader(eg EgressReader) {
	s.egMu.Lock()
	defer s.egMu.Unlock()
	s.eg = eg
}

// egressReader 返回当前的出口视图，未注入时为 nil。
func (s *Scheduler) egressReader() EgressReader {
	s.egMu.RLock()
	defer s.egMu.RUnlock()
	return s.eg
}

// SetRandSource 固定随机源，仅供测试使用。
func (s *Scheduler) SetRandSource(seed int64) {
	s.rndMu.Lock()
	s.rnd = rand.New(rand.NewSource(seed))
	s.rndMu.Unlock()
}

// Reload 从 Postgres 重新装载活跃 Key 池与昨日归档。
//
// 装载而非每次请求查库: Key 元数据变化频率是"天"级（人工增删 Key），
// 而请求是 20 QPS。每请求查一次库纯属浪费。
func (s *Scheduler) Reload(ctx context.Context) error {
	conf := s.conf
	rows, err := s.st.ListUpstreamKeys(ctx, store.UpstreamKeyFilter{
		Status:     store.KeyStatusActive,
		WithSecret: true,
		Limit:      conf.Scheduler().ActivePoolSize,
	})
	if err != nil {
		return fmt.Errorf("scheduler: 装载 Key 池: %w", err)
	}

	entries := make([]keyEntry, 0, len(rows))
	keyIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		// 水位必须按该 Key 所属 provider 的量纲取。
		//
		// 原先一律用 TokenHard()/TokenSoft()，对按次计费的 provider 就是量纲
		// 错配: 拿「500 万 token 的 95%」当成「1200 次的硬水位」。这个数不会
		// 让任何一层报错 —— snap.Hard 是正数，Used+Prededuct 永远远小于它，
		// 于是 :434 的硬水位过滤对按次 provider 完全失效，超额只能等 Acquire
		// 的 Lua 兜底，而调度打分里的 Ratio 也一路失真。
		//
		// 这是上一阶段那个缺陷的同源残留: 当时修了归档与打分的 ratio 口径，
		// 漏了 Key 池装载这一层。
		hard, soft := conf.LimitsFor(r.Provider, conf.IsCountProvider(r.Provider))
		entries = append(entries, keyEntry{
			keyID:     r.KeyID,
			secret:    r.Secret,
			provider:  r.Provider,
			egressIP:  r.EgressIP,
			pool:      r.Pool,
			dbStatus:  r.Status,
			persona:   persona.For(r.KeyID),
			hardLimit: hard,
			softLimit: soft,
		})
		keyIDs = append(keyIDs, r.KeyID)
		s.health.seed(r.KeyID, r.HealthScore, KeyStatus(r.Status))
	}

	// 昨日归档: 用于 S_history。查询失败不应阻塞调度 —— 缺历史时
	// S_history 退化为中性分，比整体拒绝服务好得多。
	yesterday := quota.QuotaDayTime(s.now().AddDate(0, 0, -1))
	hist, err := s.st.GetKeyHistory(ctx, keyIDs, yesterday)
	if err != nil {
		hist = map[store.HistoryKey]store.KeyDailyHistory{}
	}

	s.mu.Lock()
	s.keys = entries
	s.history = hist
	s.mu.Unlock()
	return nil
}

// Start 启动后台配额快照刷新。
//
// P0-1 再次强调: 这个快照**只用于打分排序**。它必然是陈旧的（最长 1 个
// SnapshotInterval），任何基于它的准入判断都会在并发下超刷。
// 准入永远只看 quota.Acquire 的 Lua 返回值。
func (s *Scheduler) Start(ctx context.Context) {
	// 刷新间隔在启动时定一次。ticker 周期改了要重启进程，属冷配置。
	interval := s.conf.Quota().SnapshotInterval
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		defer close(s.doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		s.RefreshSnapshot(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stopCh:
				return
			case <-ticker.C:
				s.RefreshSnapshot(ctx)
			}
		}
	}()
}

// Stop 停止后台刷新并等待退出。
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		select {
		case <-s.doneCh:
		case <-time.After(5 * time.Second):
		}
	})
}

// RefreshSnapshot 批量刷新配额快照（单次 pipeline）。
func (s *Scheduler) RefreshSnapshot(ctx context.Context) {
	s.mu.RLock()
	// 按 provider 分组 key IDs
	keysByProvider := make(map[string][]string)
	for _, e := range s.keys {
		keysByProvider[e.provider] = append(keysByProvider[e.provider], e.keyID)
	}
	s.mu.RUnlock()

	if len(keysByProvider) == 0 {
		return
	}

	for _, kind := range []quota.Kind{quota.KindToken, quota.KindCount} {
		// 以上一轮快照为基底做增量覆盖: 某个 provider 读取失败时，它名下的
		// Key 保留旧值，而不是被整体清空。快照本就允许陈旧且不参与准入（P0-1），
		// 因此这里不需要任何降级动作。
		s.mu.RLock()
		allSnaps := make(map[string]quota.Snapshot, len(s.snaps[kind]))
		for k, v := range s.snaps[kind] {
			allSnaps[k] = v
		}
		s.mu.RUnlock()

		for provider, ids := range keysByProvider {
			m, err := s.qr.GetMany(ctx, provider, ids, kind)
			if err != nil {
				continue
			}
			for k, v := range m {
				allSnaps[k] = v
			}
		}
		s.mu.Lock()
		s.snaps[kind] = allSnaps
		s.mu.Unlock()
	}
}

// Select 为一次请求挑选 Key。
//
// exclude 中的 Key 会被跳过，用于"上一个 Key 被 Acquire 拒绝 / 上游报错"
// 后的重试换 Key。
func (s *Scheduler) Select(ctx context.Context, req Request) (*Candidate, error) {
	if req.Now.IsZero() {
		req.Now = s.now()
	}
	if req.Kind == "" {
		req.Kind = quota.KindToken
	}

	s.mu.RLock()
	keys := s.keys
	history := s.history
	snaps := s.snaps[req.Kind]
	s.mu.RUnlock()

	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: Key 池为空", ErrNoCandidate)
	}

	// 循环外取一次: 出口视图在进程生命周期内基本不变，
	// 逐 Key 加读锁只会在热路径上白白抢锁。
	eg := s.egressReader()

	// 调度配置同样循环外取一次。逐 Key 取会让同一次 Select 里前几个 Key 按
	// 旧权重打分、后几个按新权重打分 —— 打出来的名次不对应任何一份真实配置，
	// 而结果看起来完全正常。
	scfg := s.conf.Scheduler()

	// 100 个 Key 的朴素遍历。不分桶、不预计算、不加位图（P1-7）。
	candidates := make([]scored, 0, len(keys))
	var reasons rejectReasons

	for _, e := range keys {
		if req.Exclude[e.keyID] {
			reasons.excluded++
			continue
		}

		// Provider 过滤: 只选择匹配的上游 Key
		if e.provider != req.Provider {
			reasons.wrongProvider++
			continue
		}

		hs, ok := s.health.available(e.keyID)
		if !ok {
			reasons.unhealthy++
			continue
		}

		// 最小请求间隔: 同一 Key 短时间内连续被选中，会形成远超人类操作
		// 频率的请求密度。按画像节奏放大间隔，让不同 Key 的密度也各不相同。
		if scfg.MinRequestInterval > 0 && !hs.LastSelectedAt.IsZero() {
			gap := time.Duration(float64(scfg.MinRequestInterval) * e.persona.PaceFactor())
			if req.Now.Sub(hs.LastSelectedAt) < gap {
				reasons.tooSoon++
				continue
			}
		}

		// 出口级最小间隔: 上面那条只管单个 Key，管不住出口 —— 一个 IP 上
		// 25 个 Key 各自守着 5 秒间隔，IP 层面仍可达 5 QPS。而风控看到的是
		// IP: 同一地址每秒冒出几个请求，无论背后几个账号都不像真人。
		//
		// 分级配比把大部分流量压到 hot 档后这个缺口最明显 —— hot 档 Key 少、
		// 权重高，同一出口被连续选中的概率远高于均匀分配时。
		if eg != nil && scfg.EgressMinInterval > 0 {
			if last := eg.LastUsedOn(e.keyID); !last.IsZero() &&
				req.Now.Sub(last) < scfg.EgressMinInterval {
				reasons.egressTooSoon++
				continue
			}
		}

		snap := snaps[e.keyID]
		snap.KeyID, snap.Kind = e.keyID, req.Kind
		if snap.Hard <= 0 {
			// 快照缺失时用配置水位补齐，避免 Remaining()/Ratio() 误判为 0 剩余。
			snap.Hard, snap.Soft = e.hardLimit, e.softLimit
		}

		// 硬水位: 直接淘汰。
		//
		// 这是一个**排序层面的优化过滤**，不是准入判断 —— 快照陈旧可能让
		// 已超水位的 Key 漏过，那种情况由 Acquire 的 Lua 兜住并返回 Denied。
		if snap.Used+snap.Prededuct >= snap.Hard {
			reasons.overHard++
			continue
		}

		personaScore, drop := scorePersona(e.persona, req, scfg.EnablePersona)
		if drop {
			reasons.offHours++
			continue
		}

		h, hasHist := history[store.HistoryKey{UpstreamKeyID: e.keyID, Provider: e.provider}]
		sc := Score{
			KeyID:   e.keyID,
			Quota:   scoreQuota(snap),
			History: scoreHistory(h, hasHist),
			Persona: personaScore,
			Health:  scoreHealth(hs),
		}
		// 软水位: 扣分但仍可选。这是"降权"而非"淘汰"的关键区别 ——
		// 全池都过软水位时系统仍需可用。
		if snap.Used+snap.Prededuct >= snap.Soft && snap.Soft > 0 {
			sc.SoftPenalty = scfg.SoftPenalty
		}

		sc.Total = scfg.WeightQuota*sc.Quota/100 +
			scfg.WeightHistory*sc.History/25 +
			scfg.WeightPersona*sc.Persona/100 +
			scfg.WeightHealth*sc.Health/100 -
			sc.SoftPenalty

		candidates = append(candidates, scored{entry: e, score: sc, snap: snap})
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoCandidate, reasons.String())
	}

	chosen, pickedPool, fellBack, err := s.pick(scfg, candidates)
	if err != nil {
		return nil, err
	}
	s.health.markSelected(chosen.entry.keyID, req.Now)

	return &Candidate{
		KeyID:          chosen.entry.keyID,
		Secret:         chosen.entry.secret,
		EgressIP:       chosen.entry.egressIP,
		Pool:           chosen.entry.poolOf(),
		Persona:        chosen.entry.persona,
		Score:          chosen.score,
		Snapshot:       chosen.snap,
		CandidateCount: len(candidates),
		PickedPool:     pickedPool,
		PoolFellBack:   fellBack,
	}, nil
}

// scored 是一个通过全部过滤的候选 Key 及其打分。
type scored struct {
	entry keyEntry
	score Score
	snap  quota.Snapshot
}

// selectWeight 是该候选参与加权随机的权重。
//
// 总分被罚成非正的 Key 给一个极小权重而非 0，保证「全池都很差」时仍有
// Key 可选，而不是整体 503。
func (c scored) selectWeight() float64 {
	if c.score.Total < minSelectWeight {
		return minSelectWeight
	}
	return c.score.Total
}

// minSelectWeight 是候选的权重下限，见 scored.selectWeight。
const minSelectWeight = 0.01

// pick 从候选中选出一个 Key。
//
// 返回 (选中项, 抽中的档位, 是否跨档回退, error)。
//
// 两级抽样:
//
//  1. 按 PoolShares 抽档位。这一步决定各档承担的流量比例。
//  2. 在该档内按分数加权随机。这一步保留原有的五维打分语义。
//
// 未配置 PoolShares 时退化为对全体候选做一次加权随机。
//
// 为什么不用「给 pool 加分」实现:
// 附加分会被其余四维稀释，实际配比完全不可控 —— 一个 cold 档但配额充裕、
// 健康度满分的 Key 照样能压过 hot 档。而分层的前提恰恰是 cold 档必须
// **稳定地**只承接极少流量，否则它那高 max_keys 的密度假设就崩了。
// scfg 由 Select 传入而非在这里重取: 抽样必须和打分用同一份配置，否则会出现
// 「按新权重打了分、按旧配比抽档位」这种谁都对不上的组合。
func (s *Scheduler) pick(scfg config.Scheduler, candidates []scored) (scored, string, bool, error) {
	shares := scfg.NormalizedPoolShares()
	if len(shares) == 0 {
		return weightedPick(candidates, s.randFloat()), "", false, nil
	}

	byPool := make(map[string][]scored, len(shares))
	var inShares int
	for _, c := range candidates {
		p := c.entry.poolOf()
		byPool[p] = append(byPool[p], c)
		if _, ok := shares[p]; ok {
			inShares++
		}
	}
	if inShares == 0 {
		// 所有候选的档位都不在配比表里（例如 pool 被填了未知值）。
		// 此时无从配比，退化为全体加权随机而非拒绝服务 ——
		// 配置错误不该表现为「服务不可用」这种难以定位的形态。
		return weightedPick(candidates, s.randFloat()), "", false, nil
	}

	// 第一级: 按份额抽档位。始终在**完整**份额上抽样，
	// 这样「抽中了空档」这件事才是可观测的，而不是被静默抹平。
	target := sampleShare(shares, s.randFloat())
	if g := byPool[target]; len(g) > 0 {
		return weightedPick(g, s.randFloat()), target, false, nil
	}

	// 目标档位此刻无可用 Key（全部在非活跃时段、超水位或不健康）。
	if !scfg.PoolFallback {
		return scored{}, target, false, fmt.Errorf(
			"%w: 档位 %q 无可用 Key 且已禁用跨档回退", ErrNoCandidate, target)
	}

	// 回退: 在其余有候选的档位之间按份额重新归一化后抽样。
	//
	// 不直接对全体候选做加权随机 —— 那样会让 cold 档（Key 数量最多）
	// 凭数量优势吃掉大部分回退流量，正是分层要避免的结果。
	var live float64
	for p, w := range shares {
		if p != target && len(byPool[p]) > 0 {
			live += w
		}
	}
	if live > 0 {
		r := s.randFloat() * live
		order := poolOrder(shares)
		for _, p := range order {
			if p == target || len(byPool[p]) == 0 {
				continue
			}
			r -= shares[p]
			if r <= 0 {
				return weightedPick(byPool[p], s.randFloat()), target, true, nil
			}
		}
		// 浮点累加误差走完循环: 取最后一个非空且非 target 的档位。
		for i := len(order) - 1; i >= 0; i-- {
			if p := order[i]; p != target && len(byPool[p]) > 0 {
				return weightedPick(byPool[p], s.randFloat()), target, true, nil
			}
		}
	}

	// 配比表内的档位全空，但候选非空 —— 说明候选都在表外的档位上。
	return weightedPick(candidates, s.randFloat()), target, true, nil
}

// sampleShare 按归一化份额抽一个档位。
//
// shares 的值之和为 1（由 NormalizedPoolShares 保证）。
func sampleShare(shares map[string]float64, r01 float64) string {
	order := poolOrder(shares)
	r := r01
	for _, p := range order {
		r -= shares[p]
		if r <= 0 {
			return p
		}
	}
	return order[len(order)-1] // 浮点误差兜底
}

// poolOrder 返回档位的确定性遍历顺序。
//
// map 遍历顺序在 Go 中是随机的，直接遍历会让「同一随机数落到哪个档位」
// 不可复现，测试无法固定预期，线上也无法复盘某次选择。
func poolOrder(shares map[string]float64) []string {
	out := make([]string, 0, len(shares))
	for _, p := range []string{PoolHot, PoolWarm, PoolCold} {
		if _, ok := shares[p]; ok {
			out = append(out, p)
		}
	}
	// 配置里可能有自定义档位名，附在标准三档之后，按字典序保证确定性。
	var extra []string
	for p := range shares {
		if p != PoolHot && p != PoolWarm && p != PoolCold {
			extra = append(extra, p)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// weightedPick 在候选集内按分数加权随机取一个。
//
// r01 是 [0,1) 的随机数，由调用方提供 —— 便于 pick 复用同一个随机源，
// 也让单元测试能固定落点。
func weightedPick(candidates []scored, r01 float64) scored {
	var total float64
	for _, c := range candidates {
		total += c.selectWeight()
	}

	// 落点 r 在 [0, total) 上均匀分布，逐个累减定位。
	r := r01 * total
	for _, c := range candidates {
		r -= c.selectWeight()
		if r <= 0 {
			return c
		}
	}
	// 兜底: 浮点累加误差导致走完循环时取最后一个。
	return candidates[len(candidates)-1]
}

// randFloat 返回 [0,1) 的随机数。
func (s *Scheduler) randFloat() float64 {
	s.rndMu.Lock()
	defer s.rndMu.Unlock()
	return s.rnd.Float64()
}

// rejectReasons 汇总各 Key 被淘汰的原因，让 "无可用 Key" 这个错误可诊断。
type rejectReasons struct {
	excluded      int
	wrongProvider int // Provider 不匹配
	unhealthy     int
	tooSoon       int
	// egressTooSoon 与 tooSoon 分开统计: 前者说明出口密度到顶（需要加 IP 或
	// 放宽 EgressMinInterval），后者说明单 Key 太热（需要扩池）。
	// 合并计数会让这两种完全不同的容量问题无法区分。
	egressTooSoon int
	overHard      int
	offHours      int
}

func (r rejectReasons) String() string {
	return fmt.Sprintf("已排除=%d Provider不匹配=%d 不健康=%d 间隔不足=%d 出口间隔不足=%d 超硬水位=%d 非活跃时段=%d",
		r.excluded, r.wrongProvider, r.unhealthy, r.tooSoon, r.egressTooSoon, r.overHard, r.offHours)
}

// MarkSuccess 记录一次成功请求。
func (s *Scheduler) MarkSuccess(keyID string) KeyHealth {
	return s.health.markSuccess(keyID)
}

// MarkFailure 按失败类型记录一次失败。
func (s *Scheduler) MarkFailure(keyID string, kind FailureKind) KeyHealth {
	return s.health.markFailure(keyID, kind)
}

// Health 返回某 Key 的健康状态快照。
func (s *Scheduler) Health(keyID string) KeyHealth { return s.health.get(keyID) }

// HealthAll 返回全部 Key 的健康状态，供监控与看板使用。
func (s *Scheduler) HealthAll() map[string]KeyHealth { return s.health.snapshotAll() }

// SetKeyStatus 由管理接口/刷新探测器显式设置 Key 状态。
func (s *Scheduler) SetKeyStatus(keyID string, status KeyStatus) {
	s.health.setStatus(keyID, status)
}

// PoolSize 返回当前活跃池中的 Key 数量。
func (s *Scheduler) PoolSize() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keys)
}

// KeyIDSet 返回活跃池中全部 Key 的 ID 集合。
//
// 供出口池清理下线 Key 的传输资源（egress.RetainClients）等「按存活集合
// 收缩」的消费方使用。返回新 map，调用方可自由持有。
func (s *Scheduler) KeyIDSet() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]bool, len(s.keys))
	for _, e := range s.keys {
		out[e.keyID] = true
	}
	return out
}

// SchedulableAt 返回在给定时刻通过画像时段过滤的 Key 数量。
//
// 与 PoolSize 的差别是容量口径: PoolSize 是「装载了多少 Key」，这个是
// 「此刻真正能派出去多少」。启用画像后两者可以差出几倍 —— 画像按小时
// 划分活跃窗口，深夜时段可调度量会明显低于池子总量。
//
// 用它算吞吐上限才有意义。拿 PoolSize 算会得出一个整天不变的乐观数字，
// 而真实的 503 恰恰集中在可调度量最低的那几个小时。
func (s *Scheduler) SchedulableAt(now time.Time) int {
	if !s.conf.Scheduler().EnablePersona {
		return s.PoolSize()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, e := range s.keys {
		if e.persona == nil || e.persona.IsActiveAt(now) {
			n++
		}
	}
	return n
}

// ScoreAll 返回当前全部 Key 的打分明细，供看板展示与问题排查。
// 不改变任何状态（不记录 LastSelectedAt）。
func (s *Scheduler) ScoreAll(req Request) []Score {
	if req.Now.IsZero() {
		req.Now = s.now()
	}
	if req.Kind == "" {
		req.Kind = quota.KindToken
	}

	s.mu.RLock()
	keys := s.keys
	history := s.history
	snaps := s.snaps[req.Kind]
	s.mu.RUnlock()

	// 与 Select 同理: 一次调用内的打分口径必须统一，否则看板会展示出一张
	// 各行权重不一致的分数表，而排查问题的人正是靠它对齐线上行为。
	scfg := s.conf.Scheduler()

	out := make([]Score, 0, len(keys))
	for _, e := range keys {
		hs := s.health.get(e.keyID)
		snap := snaps[e.keyID]
		if snap.Hard <= 0 {
			snap.Hard, snap.Soft = e.hardLimit, e.softLimit
		}
		personaScore, _ := scorePersona(e.persona, req, scfg.EnablePersona)
		h, hasHist := history[store.HistoryKey{UpstreamKeyID: e.keyID, Provider: e.provider}]

		sc := Score{
			KeyID:   e.keyID,
			Quota:   scoreQuota(snap),
			History: scoreHistory(h, hasHist),
			Persona: personaScore,
			Health:  scoreHealth(hs),
		}
		if snap.Soft > 0 && snap.Used+snap.Prededuct >= snap.Soft {
			sc.SoftPenalty = scfg.SoftPenalty
		}
		sc.Total = scfg.WeightQuota*sc.Quota/100 +
			scfg.WeightHistory*sc.History/25 +
			scfg.WeightPersona*sc.Persona/100 +
			scfg.WeightHealth*sc.Health/100 -
			sc.SoftPenalty
		out = append(out, sc)
	}
	return out
}
