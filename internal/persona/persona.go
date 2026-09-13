// Package persona 为每个火山 Key 生成确定性的行为画像。
//
// 设计目标（反封禁）: 1000 个 Key 若行为完全一致 —— 同一作息、同一模型偏好、
// 同一请求节奏 —— 那么它们在风控侧就是同一个批量脚本的 1000 个分身。画像的
// 作用是让每个 Key 看起来像一个独立的人在用。
//
// 相对 V3 的收敛（P1-7）:
//
//	V3 设计了 12 维指纹 + 请求时实时计算 Key 之间的行为相似度（要求 < 0.6）。
//	这里只保留 3 维: 作息时段、模型偏好、节奏抖动。原因:
//
//	  1. 有效性递减: 风控能观测到的主要就是"何时来、要什么、多快来"。
//	     UA/时区/语言这类头部字段可以静态设定，不需要建模成画像维度。
//	  2. 实时相似度是 O(n²) 且落在请求热路径上。100 个 Key 每次请求算 4950 对
//	     相似度，为了一个"离线自检就够"的指标。V4 已将其移出热路径，改为
//	     每日离线检查。
//	  3. 维度越多越难保证"互不重叠"，反而容易生成一批看起来同样机械的画像。
//
// 画像完全由 key_id 哈希导出，因此:
//   - 无状态: 进程重启后同一 Key 的画像不变，不需要持久化；
//   - 可复现: 出问题时能凭 key_id 复算出当时的画像用于排查。
package persona

import (
	"fmt"
	"hash/fnv"
	"time"
)

// Rhythm 描述 Key 的请求节奏类型。
type Rhythm string

const (
	// RhythmBursty 阵发型: 集中一段时间高频请求，然后长时间静默。
	RhythmBursty Rhythm = "bursty"
	// RhythmSteady 稳定型: 请求间隔较均匀。
	RhythmSteady Rhythm = "steady"
	// RhythmSparse 稀疏型: 低频零散请求。
	RhythmSparse Rhythm = "sparse"
)

// Chronotype 描述 Key 的作息相位。
type Chronotype string

const (
	ChronoEarlyBird Chronotype = "early_bird" // 早起型
	ChronoStandard  Chronotype = "standard"   // 标准工作时间
	ChronoNightOwl  Chronotype = "night_owl"  // 夜猫子
	ChronoAllDay    Chronotype = "all_day"    // 全天候（少数）
)

// ActiveWindow 是一个活跃时段 [StartHour, EndHour)，按小时计。
//
// EndHour 可以小于 StartHour，表示跨零点（如 22:00-03:00）。
type ActiveWindow struct {
	StartHour int
	EndHour   int
}

// Contains 判断给定小时是否落在时段内。
func (w ActiveWindow) Contains(hour int) bool {
	if w.StartHour == w.EndHour {
		return true // 退化为全天
	}
	if w.StartHour < w.EndHour {
		return hour >= w.StartHour && hour < w.EndHour
	}
	// 跨零点
	return hour >= w.StartHour || hour < w.EndHour
}

// Persona 是一个 Key 的行为画像。所有字段都是 key_id 的确定性函数。
type Persona struct {
	// ID 是画像的可读标识，形如 night_owl_bursty_07，用于日志与审计。
	ID string
	// KeyID 是画像所属的 Key。
	KeyID string

	// Chronotype 作息相位。
	Chronotype Chronotype
	// Windows 是活跃时段列表。工作日型通常有两段（上下午），夜猫子只有一段。
	Windows []ActiveWindow

	// PreferredModels 是偏好模型，按偏好度降序。
	PreferredModels []string
	// Loyalty 是对首选模型的忠诚度 [0,1]。1 表示几乎只用首选模型。
	Loyalty float64

	// Rhythm 请求节奏类型。
	Rhythm Rhythm
	// JitterRatio 是请求间隔抖动比例 [0.1, 0.6]，越大越不规律。
	JitterRatio float64
	// DailyRequestBudget 是画像"人设"下的日请求量预期，用于节奏偏离检测。
	DailyRequestBudget int
}

// 候选模型池。画像从中挑选偏好序列。
//
// 这里只写"对外模型名"，实际请求前由 config.UpstreamModel 做映射。
var modelPool = []string{
	"deepseek-v3",
	"doubao-pro-32k",
	"doubao-lite-32k",
	"deepseek-r1",
	"doubao-pro-4k",
}

// windowTemplate 是一个作息模板。
type windowTemplate struct {
	chrono  Chronotype
	windows []ActiveWindow
}

// windowDurations 是候选活跃时长（小时）。
//
// 下界 3 小时受可用性约束: 时段过窄会让调度器在低谷时刻找不到活跃 Key。
// 实测 200 个 Key 时各时段最少 31 个活跃、50 个 Key 时最少 6 个，仍有余量。
var windowDurations = []int{3, 4, 5}

// windowTemplates 由「起点小时 × 时长」生成，共 24×3 = 72 个。
//
// 为什么放弃原先手写的 9 个宽模板:
//
//	风控能观测到的密度是「同一出口 IP 在同一时刻有多少个 Key 在活动」，
//	而非「这个 IP 上一共绑了多少 Key」。原先 9 个模板日均活跃 9.6 小时，
//	其中 {9,18} 单段占 9 小时、all_day 占满 24 小时，模板间大面积重叠 ——
//	实测 MaxKeys=10 时单 IP 峰值已有 7 个 Key 同时活跃，最差 9/10。
//	即「同一 IP 下的 Key 作息完全错开」这一设计意图并未真正落实，
//	10 个 Key/IP 的低密度只是名义上的。
//
//	换成 72 个窄模板后日均活跃降至 3 小时，单 IP 峰值并发下降约 76%:
//	50 个 Key/IP 的峰值并发（8）反低于原先 10 个 Key/IP 的峰值（7）。
//	这是把 MaxKeys 从 10 提高到 25-50 的前提 —— 出口 IP 需求随之等比下降。
//
//	窄化同时改善行为相似度: 同 IP 内 25 个 Key 的两两时段余弦均值
//	0.394 → 0.135，超 0.6 告警的 Key 对 34.3% → 8.7%，
//	时段向量完全相同的 Key 对 8.7% → 0.7%。
//
// 起点与时长双维度抖动是刻意的: 只按起点分 24 档会让同起点的 Key 拿到
// 完全相同的时段向量，双维度才能把「完全相同」压到接近零。
var windowTemplates = buildWindowTemplates()

func buildWindowTemplates() []windowTemplate {
	out := make([]windowTemplate, 0, 24*len(windowDurations))
	for start := 0; start < 24; start++ {
		for _, d := range windowDurations {
			out = append(out, windowTemplate{
				chrono:  chronoOf(start),
				windows: []ActiveWindow{{start, (start + d) % 24}},
			})
		}
	}
	return out
}

// chronoOf 按活跃起点推导作息相位。
//
// 相位只用于日志与审计的可读性（画像 ID 形如 night_owl_bursty_07），
// 不参与调度决策 —— 调度只看 Windows。
func chronoOf(start int) Chronotype {
	switch {
	case start >= 5 && start < 9:
		return ChronoEarlyBird
	case start >= 9 && start < 18:
		return ChronoStandard
	case start >= 18 && start < 24:
		return ChronoNightOwl
	default:
		// 0-4 点起活跃，作息与常人完全错开，归为全天候类。
		return ChronoAllDay
	}
}

var rhythms = []Rhythm{RhythmBursty, RhythmSteady, RhythmSparse}

// hashSeed 由 key_id 与用途标签导出一个稳定的 64 位种子。
//
// 每个维度用不同的 salt，否则各维度会强相关（比如"作息越晚的 Key 节奏越稀疏"），
// 反而形成可被聚类识别的规律。
func hashSeed(keyID, salt string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(salt))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(keyID))
	return h.Sum64()
}

// For 返回 keyID 的行为画像。纯函数，可并发调用。
func For(keyID string) *Persona {
	tpl := windowTemplates[hashSeed(keyID, "chrono")%uint64(len(windowTemplates))]

	p := &Persona{
		KeyID:      keyID,
		Chronotype: tpl.chrono,
		Windows:    tpl.windows,
	}

	// 模型偏好: 用 key 的哈希做一次确定性洗牌，取前 3 个作为偏好序列。
	p.PreferredModels = shuffledModels(keyID)

	// 忠诚度 0.55 ~ 0.95。低于 0.5 就谈不上"偏好"了。
	// 先在整数域算完再除，避免 0.55+0.40 这类浮点累加误差越出上界。
	p.Loyalty = float64(55+hashSeed(keyID, "loyalty")%41) / 100.0

	p.Rhythm = rhythms[hashSeed(keyID, "rhythm")%uint64(len(rhythms))]

	// 抖动 0.10 ~ 0.60。节奏越阵发，抖动天然越大。同样在整数域完成计算。
	jitter := 10 + int(hashSeed(keyID, "jitter")%36)
	switch p.Rhythm {
	case RhythmBursty:
		jitter += 15
	case RhythmSparse:
		jitter += 5
	}
	p.JitterRatio = float64(min(jitter, 60)) / 100.0

	// 日请求预算按节奏分档，并在档内抖动，避免出现整齐的数值。
	switch p.Rhythm {
	case RhythmBursty:
		p.DailyRequestBudget = 400 + int(hashSeed(keyID, "budget")%400)
	case RhythmSteady:
		p.DailyRequestBudget = 200 + int(hashSeed(keyID, "budget")%300)
	default:
		p.DailyRequestBudget = 50 + int(hashSeed(keyID, "budget")%150)
	}

	p.ID = fmt.Sprintf("%s_%s_%02d", p.Chronotype, p.Rhythm, hashSeed(keyID, "tag")%100)
	return p
}

func shuffledModels(keyID string) []string {
	models := make([]string, len(modelPool))
	copy(models, modelPool)

	// Fisher-Yates，随机源来自 key_id 哈希的确定性推进。
	seed := hashSeed(keyID, "model")
	for i := len(models) - 1; i > 0; i-- {
		seed = seed*6364136223846793005 + 1442695040888963407 // LCG 推进
		j := int((seed >> 33) % uint64(i+1))
		models[i], models[j] = models[j], models[i]
	}
	if len(models) > 3 {
		models = models[:3]
	}
	return models
}

// IsActiveAt 判断该画像在给定时刻是否处于活跃时段。
//
// 调度器用它做**硬过滤**: 不在活跃时段的 Key 直接淘汰（S_persona = 0）。
// 让一个"夜猫子"Key 在早上 8 点持续发请求，是最容易被识别的机器特征之一。
func (p *Persona) IsActiveAt(t time.Time) bool {
	if p == nil {
		return true
	}
	h := t.Hour()
	for _, w := range p.Windows {
		if w.Contains(h) {
			return true
		}
	}
	return false
}

// ModelAffinity 返回该画像对某模型的偏好度 [0,1]。
//
// 首选模型得 Loyalty，次选按位次衰减，池外模型得一个低但非零的底分 ——
// 底分不为零是刻意的: 用户请求什么模型由用户决定，画像不该把某个 Key
// 完全排除在某模型之外，那会导致小众模型的请求无 Key 可用。
func (p *Persona) ModelAffinity(model string) float64 {
	if p == nil || model == "" {
		return 1
	}
	for i, m := range p.PreferredModels {
		if m == model {
			switch i {
			case 0:
				return p.Loyalty
			case 1:
				return p.Loyalty * 0.7
			default:
				return p.Loyalty * 0.5
			}
		}
	}
	return 0.3
}

// PaceFactor 返回节奏因子，用于把最小请求间隔按画像放大。
//
// 稀疏型 Key 的请求间隔应当明显长于阵发型 —— 否则所有 Key 都以同一个
// 最小间隔贴着跑，节奏维度就白设了。
func (p *Persona) PaceFactor() float64 {
	if p == nil {
		return 1
	}
	switch p.Rhythm {
	case RhythmBursty:
		return 0.6
	case RhythmSteady:
		return 1.0
	default:
		return 2.0
	}
}
