package token

import (
	"github.com/redis/go-redis/v9"
)

const familyByteLen = 16

// 新认证的 token、family 与账号索引同次提交；所有类型检查都在首次写入前。
// 驱动对读超时的 EVALSHA 会原样重发:三个键的内容与本次一致只可能是同一次签发(payload 含签发秒与家族号,head 含随机数),按已写入返回。
var issueFamilyScript = redis.NewScript(`
local kind = redis.call("TYPE", KEYS[3]).ok
if kind ~= "none" and kind ~= "zset" then
    return redis.error_reply("invalid account refresh index")
end
if redis.call("EXISTS", KEYS[1], KEYS[2], KEYS[4]) ~= 0 then
    if redis.call("TYPE", KEYS[1]).ok == "string" and redis.call("GET", KEYS[1]) == ARGV[1] and
       redis.call("TYPE", KEYS[2]).ok == "string" and redis.call("GET", KEYS[2]) == ARGV[1] and
       redis.call("TYPE", KEYS[4]).ok == "hash" and redis.call("HGET", KEYS[4], "head") == ARGV[8] and
       redis.call("HGET", KEYS[4], "account") == ARGV[2] then
        return 1
    end
    return redis.error_reply("token identity collision")
end
local oldTTL = redis.call("PTTL", KEYS[3])
redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[4])
redis.call("SET", KEYS[2], ARGV[1], "PX", ARGV[5])
redis.call("HSET", KEYS[4], "account", ARGV[2], "head", ARGV[8], "prev", "", "created_at", ARGV[3], "rotated_at", ARGV[3])
redis.call("PEXPIRE", KEYS[4], ARGV[5])
redis.call("ZREMRANGEBYSCORE", KEYS[3], "-inf", ARGV[6])
redis.call("ZADD", KEYS[3], ARGV[7], ARGV[8])
redis.call("PEXPIRE", KEYS[3], math.max(oldTTL, tonumber(ARGV[5])))
return 1
`)

// 轮换:作废旧 refresh、写新的一对、推进家族记录、续账号索引、写轮换记录是同一次提交。
//
// 旧 token 按值比对:Refresh 先 GET 出 payload 再交给脚本,两步之间被并发的另一次刷新消费、被替换或已过期,整段不写。
// 驱动对读超时的 EVALSHA 原样重发时也一样:第二次看到旧 token 已不在,回 0,Refresh 随后走重放判定,拿回首发写下的那一对。
//
// KEYS:旧 refresh、账号索引、新 access、新 refresh、family、轮换记录。
// ARGV:1 旧 refresh 当前的值,2 旧 secret,3 新 payload,4 access TTL 毫秒,5 refresh TTL 毫秒,6 索引死成员 score 上界,
// 7 新 refresh 过期 unix 秒,8 新 refresh secret,9 账号,10 现在 unix 秒,11 宽限窗口毫秒(0 = 不写记录),
// 12 新 access secret,13 新 access 过期 unix 秒。
// 返回 refreshRotated / refreshNotLive / refreshFamilyGone / refreshNotHead。后三个都什么都没写。
var rotateRefreshScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) ~= ARGV[1] or redis.call("PTTL", KEYS[1]) <= 0 then
    return 0
end
local kind = redis.call("TYPE", KEYS[2]).ok
if kind ~= "none" and kind ~= "zset" then
    return redis.error_reply("invalid account refresh index")
end
if kind ~= "zset" or not redis.call("ZSCORE", KEYS[2], ARGV[2]) then
    return 0
end
if redis.call("EXISTS", KEYS[3], KEYS[4], KEYS[6]) ~= 0 then
    return redis.error_reply("token identity collision")
end
local familyKind = redis.call("TYPE", KEYS[5]).ok
if familyKind == "none" then
    return 2
end
if familyKind ~= "hash" or redis.call("HGET", KEYS[5], "account") ~= ARGV[9] or redis.call("PTTL", KEYS[5]) <= 0 then
    return redis.error_reply("invalid refresh family")
end
if redis.call("HGET", KEYS[5], "head") ~= ARGV[2] then
    return 3
end
local oldTTL = redis.call("PTTL", KEYS[2])
redis.call("DEL", KEYS[1])
redis.call("ZREM", KEYS[2], ARGV[2])
redis.call("SET", KEYS[3], ARGV[3], "PX", ARGV[4])
redis.call("SET", KEYS[4], ARGV[3], "PX", ARGV[5])
redis.call("HSET", KEYS[5], "head", ARGV[8], "prev", ARGV[2], "rotated_at", ARGV[10])
redis.call("PEXPIRE", KEYS[5], ARGV[5])
redis.call("ZREMRANGEBYSCORE", KEYS[2], "-inf", ARGV[6])
redis.call("ZADD", KEYS[2], ARGV[7], ARGV[8])
redis.call("PEXPIRE", KEYS[2], math.max(oldTTL, tonumber(ARGV[5])))
if tonumber(ARGV[11]) > 0 then
    redis.call("HSET", KEYS[6], "access", ARGV[12], "refresh", ARGV[8], "access_expire", ARGV[13], "refresh_expire", ARGV[7], "rotated_at", ARGV[10])
    redis.call("PEXPIRE", KEYS[6], ARGV[11])
end
return 1
`)

// 判定一枚已不在的旧 refresh。判定与吊销在同一段脚本里:读到的 head 不会在吊销前变,不存在「判定完家族已被轮换」的窗口。
// 吊销只需删掉家族记录(同家族的 access 校验要读它),顺带删掉头的键,把头从账号索引摘掉。
//
// KEYS:family、这枚旧 refresh 的轮换记录。
// ARGV:1 旧 secret,2 当前宽限窗口毫秒,3 窗口硬上限毫秒,4 refresh 键前缀,5 账号索引键前缀,6 只观测("1")。
// 两个前缀都带账号 tag,现拼出来的键与 KEYS 同槽。
// 返回数组,第一项是 replay* 常量:
//
//	replaySamePair:{1, 账号, access secret, refresh secret, access 过期, refresh 过期, 轮换时刻}
//	replayReuseRevoked / replayReuseObserved:{码, 账号, predecessor|older, 家族最近轮换时刻, 家族创建时刻}
//	其余:{码}
//
// 全部检查都在第一次写之前。
var replayRefreshScript = redis.NewScript(`
local recordKind = redis.call("TYPE", KEYS[2]).ok
if recordKind ~= "none" and recordKind ~= "hash" then
    return {-1}
end
local familyKind = redis.call("TYPE", KEYS[1]).ok
if familyKind == "none" then
    if recordKind == "hash" then
        return {2}
    end
    return {3}
end
if familyKind ~= "hash" or redis.call("PTTL", KEYS[1]) <= 0 then
    return {-1}
end
local account = redis.call("HGET", KEYS[1], "account")
local head = redis.call("HGET", KEYS[1], "head")
if not account or not head then
    return {-1}
end
if head == ARGV[1] then
    return {3}
end
if recordKind == "hash" then
    local ttl = redis.call("PTTL", KEYS[2])
    if ttl < 0 or ttl > tonumber(ARGV[3]) then
        return {-1}
    end
    if ttl > tonumber(ARGV[2]) then
        return {3}
    end
    local successor = redis.call("HGET", KEYS[2], "refresh")
    if not successor then
        return {-1}
    end
    if head == successor then
        local successorTTL = redis.call("PTTL", ARGV[4] .. successor)
        if successorTTL == -1 then
            return {-1}
        end
        if successorTTL == -2 then
            return {0}
        end
        return {1, account, redis.call("HGET", KEYS[2], "access"), successor,
            redis.call("HGET", KEYS[2], "access_expire"), redis.call("HGET", KEYS[2], "refresh_expire"),
            redis.call("HGET", KEYS[2], "rotated_at")}
    end
end
local replayed = "older"
if redis.call("HGET", KEYS[1], "prev") == ARGV[1] then
    replayed = "predecessor"
end
local rotatedAt = redis.call("HGET", KEYS[1], "rotated_at") or "0"
local createdAt = redis.call("HGET", KEYS[1], "created_at") or "0"
if ARGV[6] == "1" then
    return {5, account, replayed, rotatedAt, createdAt}
end
local index = ARGV[5] .. account
redis.call("DEL", ARGV[4] .. head, KEYS[1])
if redis.call("TYPE", index).ok == "zset" then
    redis.call("ZREM", index, head)
end
return {4, account, replayed, rotatedAt, createdAt}
`)

// 同 family 的旧 access 在正常轮换后继续有效；family 撤销后全部立即拒绝。
// 同时核当前 head 与账号索引，损坏或脱离索引的 family 不能变成不可撤销凭证。
var validateFamilyAccessScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) ~= ARGV[1] or redis.call("PTTL", KEYS[1]) <= 0 or
   redis.call("TYPE", KEYS[2]).ok ~= "hash" or
   redis.call("HGET", KEYS[2], "account") ~= ARGV[2] or
   redis.call("PTTL", KEYS[2]) <= 0 then
    return 0
end
local head = redis.call("HGET", KEYS[2], "head")
if not head or #head ~= 43 or not string.match(head, "^[%w_-]+$") or
   redis.call("TYPE", KEYS[3]).ok ~= "zset" or
   not redis.call("ZSCORE", KEYS[3], head) then
    return 0
end
if redis.call("PTTL", ARGV[4] .. head) <= 0 then return 0 end
local raw = redis.call("GET", ARGV[4] .. head)
if not raw then return 0 end
local ok, data = pcall(cjson.decode, raw)
if not ok or type(data) ~= "table" or data.account ~= ARGV[2] or data.family_id ~= ARGV[3] then
    return 0
end
return 1
`)

// 淘汰与整账号撤销共用同一原子提交。先核全部候选，再删除；仅 head 仍指向该候选时撤 family。
// ARGV：账号、保留数量（0 表示全撤销）、refresh 前缀、family 前缀、不淘汰的 secret（刚签发的那枚，全撤销时为空）。
// 候选键由前缀现拼、不在 KEYS 里；两个前缀都带账号 tag，现拼的键与 KEYS[1] 同槽（Manager.retire）。
var retireRefreshScript = redis.NewScript(`
local kind = redis.call("TYPE", KEYS[1]).ok
if kind == "none" then return 0 end
if kind ~= "zset" then return redis.error_reply("invalid account refresh index") end
local count = redis.call("ZCARD", KEYS[1])
local keep = tonumber(ARGV[2])
if count <= keep then return 0 end
local candidates = redis.call("ZRANGE", KEYS[1], 0, count - keep)
local tokens = {}
for _, token in ipairs(candidates) do
    if token ~= ARGV[5] and #tokens < count - keep then
        tokens[#tokens + 1] = token
    end
end
local families = {}
for i, token in ipairs(tokens) do
    local raw = redis.call("GET", ARGV[3] .. token)
    if raw then
        local ok, data = pcall(cjson.decode, raw)
        if not ok or type(data) ~= "table" or data.account ~= ARGV[1] then
            return redis.error_reply("invalid refresh ownership")
        end
        local family = data.family_id
        if type(family) ~= "string" or #family ~= 22 or not string.match(family, "^[%w_-]+$") or
           not string.match(string.sub(family, -1), "^[AQgw]$") then
            return redis.error_reply("invalid refresh family identity")
        end
        if redis.call("PTTL", ARGV[3] .. token) <= 0 then
            return redis.error_reply("invalid refresh expiration")
        end
        local key = ARGV[4] .. family
        local familyType = redis.call("TYPE", key).ok
        if familyType ~= "none" then
            if familyType ~= "hash" or redis.call("HGET", key, "account") ~= ARGV[1] or redis.call("PTTL", key) <= 0 then
                return redis.error_reply("invalid refresh family ownership")
            end
            local head = redis.call("HGET", key, "head")
            if not head or #head ~= 43 or not string.match(head, "^[%w_-]+$") or
               not redis.call("ZSCORE", KEYS[1], head) or redis.call("PTTL", ARGV[3] .. head) <= 0 then
                return redis.error_reply("invalid refresh family head")
            end
            local headRaw = redis.call("GET", ARGV[3] .. head)
            local headOK, headData = false, nil
            if headRaw then headOK, headData = pcall(cjson.decode, headRaw) end
            if not headOK or type(headData) ~= "table" or headData.account ~= ARGV[1] or headData.family_id ~= family then
                return redis.error_reply("invalid refresh family head ownership")
            end
            if head == token then families[i] = key end
        end
    end
end
for i, token in ipairs(tokens) do
    redis.call("DEL", ARGV[3] .. token)
    if families[i] then redis.call("DEL", families[i]) end
    redis.call("ZREM", KEYS[1], token)
end
return #tokens
`)
