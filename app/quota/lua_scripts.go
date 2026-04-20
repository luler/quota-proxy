package quota

// Lua 脚本定义

// TryReserveScript 预占名额脚本
// KEYS[1]: quota key
// ARGV[1]: limit
// ARGV[2]: ttl (seconds)
// 返回: {成功标志(1/0), success_count, pending_count, rejected_429_count}
const TryReserveScript = `
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])

local success = tonumber(redis.call('HGET', key, 'success') or 0)
local pending = tonumber(redis.call('HGET', key, 'pending') or 0)

if success + pending >= limit then
	local rejected429 = redis.call('HINCRBY', key, 'rejected_429', 1)
	redis.call('EXPIRE', key, ttl)
	return {0, success, pending, rejected429}
end

redis.call('HINCRBY', key, 'pending', 1)
redis.call('EXPIRE', key, ttl)
return {1, success, pending + 1, tonumber(redis.call('HGET', key, 'rejected_429') or 0)}
`

// ConfirmScript 确认成功脚本
// KEYS[1]: quota key
const ConfirmScript = `
local key = KEYS[1]
local pending = tonumber(redis.call('HGET', key, 'pending') or 0)
if pending > 0 then
	redis.call('HINCRBY', key, 'pending', -1)
end
redis.call('HINCRBY', key, 'success', 1)
return 1
`

// RollbackScript 回滚 pending 脚本
// KEYS[1]: quota key
const RollbackScript = `
local key = KEYS[1]
local pending = tonumber(redis.call('HGET', key, 'pending') or 0)
if pending > 0 then
	redis.call('HINCRBY', key, 'pending', -1)
end
return 1
`

// GetQuotaScript 获取配额状态脚本
// KEYS[1]: quota key
// 返回: {success, pending, rejected_429}
const GetQuotaScript = `
local key = KEYS[1]
local success = tonumber(redis.call('HGET', key, 'success') or 0)
local pending = tonumber(redis.call('HGET', key, 'pending') or 0)
local rejected429 = tonumber(redis.call('HGET', key, 'rejected_429') or 0)
return {success, pending, rejected429}
`

// RejectScript 耗尽剩余额度脚本
// KEYS[1]: quota key
// ARGV[1]: limit
// ARGV[2]: ttl (seconds)
const RejectScript = `
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
redis.call('HSET', key, 'success', limit)
redis.call('HSET', key, 'pending', 0)
redis.call('EXPIRE', key, ttl)
return 1
`

// ResetScript 重置配额脚本
// KEYS[1]: quota key
const ResetScript = `
local key = KEYS[1]
redis.call('DEL', key)
return 1
`
