package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// P1-9: V3 完全没有用户模型，对外服务的网关无法限制单个用户的用量，
// 一个失控客户端就能把 1000 个 Key 的额度在几小时内刷光。本文件补上
// 用户级 RPM / TPM 限流。
//
// 选用令牌桶而非固定窗口计数: 固定窗口存在边界突发问题（窗口切换瞬间可
// 通过 2 倍配额），对「避免超刷」这个首要目标是实质风险。令牌桶天然平滑。
//
// 全部逻辑在 Lua 中原子完成 —— 读取余量与扣减必须不可分割，否则并发下
// 多个请求会各自读到充足余量而全部放行。

// luaTokenBucket 是令牌桶的原子实现。
//
// KEYS[1] = 桶的 hash key
// ARGV[1] = 容量 (burst)
// ARGV[2] = 每秒填充速率
// ARGV[3] = 本次请求需要的令牌数
// ARGV[4] = 当前时间（毫秒）
// ARGV[5] = key 的 TTL（秒）
//
// 返回 {allowed(0/1), remaining, retry_after_ms}
const luaTokenBucket = `
local capacity = tonumber(ARGV[1])
local rate     = tonumber(ARGV[2])
local need     = tonumber(ARGV[3])
local now_ms   = tonumber(ARGV[4])
local ttl      = tonumber(ARGV[5])

if capacity <= 0 or rate <= 0 then
  return {1, -1, 0}
end

local data = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(data[1])
local ts     = tonumber(data[2])

if tokens == nil or ts == nil then
  -- 首次访问按满桶初始化，避免新用户第一个请求就被拒
  tokens = capacity
  ts = now_ms
end

-- 按经过时间补充令牌
local elapsed = now_ms - ts
if elapsed < 0 then elapsed = 0 end
tokens = tokens + (elapsed / 1000.0) * rate
if tokens > capacity then tokens = capacity end

local allowed = 0
local retry_after = 0
if tokens >= need then
  tokens = tokens - need
  allowed = 1
else
  -- 攒够所需令牌还差多少毫秒
  retry_after = math.ceil(((need - tokens) / rate) * 1000)
end

redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now_ms)
redis.call('EXPIRE', KEYS[1], ttl)

return {allowed, math.floor(tokens), retry_after}
`

// RateLimiter 是基于 Redis 的用户级令牌桶限流器。
type RateLimiter struct {
	rdb *redis.Client
	sha string
	now func() time.Time
	// failOpen 决定 Redis 故障时的行为。
	//
	// 取 true（放行）: 限流是用量保护而非安全边界，Redis 抖动时拒绝全部请求
	// 会把一次依赖故障放大成完全不可用。真正的超刷防线是配额层的 Lua 预扣，
	// 它同样依赖 Redis —— Redis 挂了配额层会直接拒绝，无需限流层重复兜底。
	failOpen bool
}

// NewRateLimiter 创建限流器并预加载脚本。
func NewRateLimiter(ctx context.Context, rdb *redis.Client) (*RateLimiter, error) {
	sha, err := rdb.ScriptLoad(ctx, luaTokenBucket).Result()
	if err != nil {
		return nil, fmt.Errorf("gateway: 加载限流脚本: %w", err)
	}
	return &RateLimiter{rdb: rdb, sha: sha, now: time.Now, failOpen: true}, nil
}

// SetClock 替换时钟，仅供测试。
func (l *RateLimiter) SetClock(f func() time.Time) { l.now = f }

// Result 是一次限流判定的结果。
type Result struct {
	Allowed   bool
	Remaining int64
	// RetryAfter 是建议的重试等待时长。
	RetryAfter time.Duration
	// Dimension 标明触发限流的维度，用于指标与错误消息。
	Dimension string
}

// AllowRequest 检查 RPM 维度。
//
// 桶容量取 max(limit/6, 1)，即允许 10 秒的额度作为突发余量。取满分钟容量
// 会让用户在一秒内打满整分钟配额，与「平滑」的初衷相悖。
func (l *RateLimiter) AllowRequest(ctx context.Context, userID int64, rpmLimit int) (Result, error) {
	if rpmLimit <= 0 {
		return Result{Allowed: true, Remaining: -1}, nil
	}
	capacity := int64(rpmLimit) / 6
	if capacity < 1 {
		capacity = 1
	}
	rate := float64(rpmLimit) / 60.0
	return l.consume(ctx, fmt.Sprintf("user:rl:rpm:%d", userID), capacity, rate, 1, "rpm")
}

// AllowTokens 检查 TPM 维度。
//
// tokens 传入的是预估用量。这里存在一个无法回避的取舍: 真实用量只有请求
// 结束后才知道，若按真实用量事后扣减，超限只能在下一个请求才被发现。故按
// 预估量预扣，并在 RefundTokens 中归还差额。
func (l *RateLimiter) AllowTokens(ctx context.Context, userID int64, tpmLimit, tokens int64) (Result, error) {
	if tpmLimit <= 0 {
		return Result{Allowed: true, Remaining: -1}, nil
	}
	if tokens <= 0 {
		tokens = 1
	}
	capacity := tpmLimit / 6
	if capacity < 1 {
		capacity = 1
	}
	// 单请求预估量超过桶容量时会永远无法通过。放宽到能容纳该请求，
	// 否则大请求会被永久拒绝而非排队。
	if tokens > capacity {
		capacity = tokens
	}
	rate := float64(tpmLimit) / 60.0
	return l.consume(ctx, fmt.Sprintf("user:rl:tpm:%d", userID), capacity, rate, tokens, "tpm")
}

// RefundTokens 归还预扣与实际用量之间的差额。
//
// 不归还的后果是 TPM 被系统性高估（预扣含 1.2 倍放大系数），用户会在远低于
// 名义限额时就被限流。这里用「加回令牌」而非重新计算，保持原子性。
func (l *RateLimiter) RefundTokens(ctx context.Context, userID int64, tpmLimit, refund int64) error {
	if tpmLimit <= 0 || refund <= 0 {
		return nil
	}
	capacity := tpmLimit / 6
	if capacity < 1 {
		capacity = 1
	}
	key := fmt.Sprintf("user:rl:tpm:%d", userID)
	// HINCRBYFLOAT 后钳制到容量上限。轻微超出容量在下次 consume 时会被钳制，
	// 故此处不额外加锁。
	if err := l.rdb.HIncrByFloat(ctx, key, "tokens", float64(refund)).Err(); err != nil {
		return fmt.Errorf("gateway: 归还 TPM 令牌: %w", err)
	}
	return nil
}

func (l *RateLimiter) consume(ctx context.Context, key string, capacity int64, rate float64, need int64, dim string) (Result, error) {
	nowMS := l.now().UnixMilli()
	// TTL 取 2 分钟: 覆盖一分钟窗口且足够回填，同时让闲置用户的桶自动过期
	res, err := l.rdb.EvalSha(ctx, l.sha, []string{key},
		capacity, rate, need, nowMS, 120).Result()
	if err != nil {
		if l.failOpen {
			// Redis 不可用时放行，真正的超刷防线在配额层
			return Result{Allowed: true, Remaining: -1, Dimension: dim}, err
		}
		return Result{Dimension: dim}, err
	}

	vals, ok := res.([]interface{})
	if !ok || len(vals) < 3 {
		return Result{Allowed: true, Remaining: -1, Dimension: dim},
			fmt.Errorf("gateway: 限流脚本返回异常 %#v", res)
	}
	allowed, _ := vals[0].(int64)
	remaining, _ := vals[1].(int64)
	retryMS, _ := vals[2].(int64)

	return Result{
		Allowed:    allowed == 1,
		Remaining:  remaining,
		RetryAfter: time.Duration(retryMS) * time.Millisecond,
		Dimension:  dim,
	}, nil
}
