package quota

// P0-1: 配额的唯一权威写路径。所有预扣/提交/释放都必须经由这些 Lua 脚本，
// 进程内存不得持有可写配额状态。
//
// P0-2: 预扣采用「租约（lease）」模型而非裸计数器 —— 每次预扣登记一条带
// 到期时间的租约，使得 SSE 断连、进程崩溃等场景下的 prededuct 可被回收。

// luaAcquire 原子预扣。
//
// KEYS[1] 配额 Hash        volc:quota:{kind}:{key_id}:{quota_day}
// KEYS[2] 租约 ZSET        volc:lease:{quota_day}
// KEYS[3] 租约数据 Hash    volc:lease:data:{lease_id}
//
// ARGV[1] amount        本次预扣量
// ARGV[2] hard_limit    硬水位线
// ARGV[3] soft_limit    软水位线
// ARGV[4] lease_id
// ARGV[5] expire_at     租约到期时间戳(秒)
// ARGV[6] key_id
// ARGV[7] kind          token / count
// ARGV[8] now           当前时间戳(秒)
// ARGV[9] quota_ttl     配额 Hash 的 TTL(秒)
//
// 返回 {code, used, prededuct, remaining}
//
//	code: 1=OK 且水位正常, 2=OK 但已过软水位(应降权), 0=拒绝(超硬水位)
const luaAcquire = `
local quotaKey  = KEYS[1]
local leaseZSet = KEYS[2]
local leaseData = KEYS[3]

local amount    = tonumber(ARGV[1])
local hardLimit = tonumber(ARGV[2])
local softLimit = tonumber(ARGV[3])
local leaseID   = ARGV[4]
local expireAt  = tonumber(ARGV[5])
local keyID     = ARGV[6]
local kind      = ARGV[7]
local now       = tonumber(ARGV[8])
local quotaTTL  = tonumber(ARGV[9])

local used = tonumber(redis.call('HGET', quotaKey, 'used') or '0')
local pre  = tonumber(redis.call('HGET', quotaKey, 'prededuct') or '0')

-- 核心不变量: used + prededuct + amount <= hardLimit
local projected = used + pre + amount
if projected > hardLimit then
  return {0, used, pre, hardLimit - used - pre}
end

redis.call('HINCRBY', quotaKey, 'prededuct', amount)
redis.call('HSET', quotaKey, 'hard_limit', hardLimit, 'soft_limit', softLimit, 'last_updated', now)
redis.call('EXPIRE', quotaKey, quotaTTL)

-- 登记租约，供超时回收与对账使用
redis.call('ZADD', leaseZSet, expireAt, leaseID)
redis.call('HSET', leaseData,
  'key_id', keyID, 'kind', kind, 'amount', amount,
  'quota_key', quotaKey, 'created_at', now)
redis.call('EXPIRE', leaseData, quotaTTL)
redis.call('EXPIRE', leaseZSet, quotaTTL)

local code = 1
if projected > softLimit then code = 2 end
return {code, used, pre + amount, hardLimit - used - pre - amount}
`

// luaCommit 请求完成后的配额修正。
//
// KEYS[1] 租约 ZSET
// KEYS[2] 租约数据 Hash
// ARGV[1] lease_id
// ARGV[2] actual   实际消耗量
// ARGV[3] now
//
// 返回 {code, used, prededuct}
//
//	code: 1=已提交, 0=租约不存在(已被超时回收，actual 仍然计入 used 以防少算)
//
// 释放预扣、累计实际用量。二者在同一原子块内完成，不存在中间态。
const luaCommit = `
local leaseZSet = KEYS[1]
local leaseData = KEYS[2]

local leaseID = ARGV[1]
local actual  = tonumber(ARGV[2])
local now     = tonumber(ARGV[3])

local quotaKey = redis.call('HGET', leaseData, 'quota_key')
if not quotaKey then
  -- 租约已被回收器清理: prededuct 已还原，此处只补记实际用量，避免少算导致超刷
  return {0, 0, 0}
end

local amount = tonumber(redis.call('HGET', leaseData, 'amount') or '0')

redis.call('HINCRBY', quotaKey, 'used', actual)
local pre = redis.call('HINCRBY', quotaKey, 'prededuct', -amount)
-- 防御性钳位: prededuct 不允许为负
if pre < 0 then
  redis.call('HSET', quotaKey, 'prededuct', 0)
  pre = 0
end
redis.call('HSET', quotaKey, 'last_updated', now)

redis.call('ZREM', leaseZSet, leaseID)
redis.call('DEL', leaseData)

local used = tonumber(redis.call('HGET', quotaKey, 'used') or '0')
return {1, used, pre}
`

// luaRelease 请求失败时释放预扣，不计入 used。
//
// KEYS[1] 租约 ZSET
// KEYS[2] 租约数据 Hash
// ARGV[1] lease_id
const luaRelease = `
local leaseZSet = KEYS[1]
local leaseData = KEYS[2]
local leaseID   = ARGV[1]

local quotaKey = redis.call('HGET', leaseData, 'quota_key')
if not quotaKey then
  return {0}
end

local amount = tonumber(redis.call('HGET', leaseData, 'amount') or '0')
local pre = redis.call('HINCRBY', quotaKey, 'prededuct', -amount)
if pre < 0 then
  redis.call('HSET', quotaKey, 'prededuct', 0)
end

redis.call('ZREM', leaseZSet, leaseID)
redis.call('DEL', leaseData)
return {1}
`

// luaReap 回收过期租约。
//
// P0-2 的核心: 用户中途断开 SSE、实例宕机等情况下 Commit 永远不会执行，
// 若无此回收器，prededuct 将被永久占用，数日后出现「配额虚耗殆尽但火山侧
// 实际未用」的假枯竭。
//
// KEYS[1] 租约 ZSET
// ARGV[1] now
// ARGV[2] batch  单批最大回收数
//
// 返回已回收的租约数
const luaReap = `
local leaseZSet = KEYS[1]
local now   = tonumber(ARGV[1])
local batch = tonumber(ARGV[2])

local expired = redis.call('ZRANGEBYSCORE', leaseZSet, '-inf', now, 'LIMIT', 0, batch)
local n = 0

for _, leaseID in ipairs(expired) do
  local leaseData = 'volc:lease:data:' .. leaseID
  local quotaKey = redis.call('HGET', leaseData, 'quota_key')
  if quotaKey then
    local amount = tonumber(redis.call('HGET', leaseData, 'amount') or '0')
    local pre = redis.call('HINCRBY', quotaKey, 'prededuct', -amount)
    if pre < 0 then
      redis.call('HSET', quotaKey, 'prededuct', 0)
    end
    redis.call('DEL', leaseData)
  end
  redis.call('ZREM', leaseZSet, leaseID)
  n = n + 1
end

return n
`

// luaReconcile 对账: 以未过期租约之和为准，强制修正 prededuct。
//
// 用于兜底 Lua 之外的任何异常路径（如 Redis 主从切换丢写）。
//
// KEYS[1] 配额 Hash
// ARGV[1] expected  该 Key 未过期租约金额之和
// 返回 {drift, corrected}
const luaReconcile = `
local quotaKey = KEYS[1]
local expected = tonumber(ARGV[1])

local pre = tonumber(redis.call('HGET', quotaKey, 'prededuct') or '0')
local drift = pre - expected
if drift ~= 0 then
  redis.call('HSET', quotaKey, 'prededuct', expected)
end
return {drift, expected}
`

// luaRefreshReset 探测确认某 Key 已在火山侧刷新后，清零本地配额计数。
//
// P0-4: 只有在探测确认刷新完成后才允许调用，绝不按时间推测。
//
// KEYS[1] 新配额日的配额 Hash
// ARGV[1] hard_limit
// ARGV[2] soft_limit
// ARGV[3] now
// ARGV[4] ttl
const luaRefreshReset = `
local quotaKey = KEYS[1]
redis.call('HSET', quotaKey,
  'used', 0, 'prededuct', 0,
  'hard_limit', ARGV[1], 'soft_limit', ARGV[2],
  'last_updated', ARGV[3], 'refreshed', 1)
redis.call('EXPIRE', quotaKey, tonumber(ARGV[4]))
return 1
`
