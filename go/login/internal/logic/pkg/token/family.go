package token

import (
	"encoding/base64"

	"github.com/redis/go-redis/v9"
)

const (
	refreshFamilyPrefix = "refresh_family:"
	familyByteLen       = 16
)

func validFamilyID(id string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	return err == nil && len(raw) == familyByteLen && base64.RawURLEncoding.EncodeToString(raw) == id
}

// 新认证的 token、family 与账号索引同次提交；所有类型检查都在首次写入前。
var issueFamilyScript = redis.NewScript(`
local kind = redis.call("TYPE", KEYS[3]).ok
if kind ~= "none" and kind ~= "zset" then
    return redis.error_reply("invalid account refresh index")
end
if redis.call("EXISTS", KEYS[1], KEYS[2], KEYS[4]) ~= 0 then
    return redis.error_reply("token identity collision")
end
local oldTTL = redis.call("PTTL", KEYS[3])
redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[4])
redis.call("SET", KEYS[2], ARGV[1], "PX", ARGV[5])
redis.call("HSET", KEYS[4], "account", ARGV[2], "head", ARGV[8], "created_at", ARGV[3], "rotated_at", ARGV[3])
redis.call("PEXPIRE", KEYS[4], ARGV[5])
redis.call("ZREMRANGEBYSCORE", KEYS[3], "-inf", ARGV[6])
redis.call("ZADD", KEYS[3], ARGV[7], ARGV[8])
redis.call("PEXPIRE", KEYS[3], math.max(oldTTL, tonumber(ARGV[5])))
return 1
`)

// family 内严格一次性轮换；旧无 family 的 refresh 只在本次 CAS 成功时收编。
// KEYS：旧 refresh、账号索引、新 access、新 refresh、family。
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
if redis.call("EXISTS", KEYS[3], KEYS[4]) ~= 0 then
    return redis.error_reply("token identity collision")
end
if ARGV[10] == "1" then
    if redis.call("EXISTS", KEYS[5]) ~= 0 then
        return 0
    end
else
    if redis.call("TYPE", KEYS[5]).ok ~= "hash" or
       redis.call("HGET", KEYS[5], "account") ~= ARGV[9] or
       redis.call("HGET", KEYS[5], "head") ~= ARGV[2] or
       redis.call("PTTL", KEYS[5]) <= 0 then
        return 0
    end
end
local oldTTL = redis.call("PTTL", KEYS[2])
redis.call("DEL", KEYS[1])
redis.call("ZREM", KEYS[2], ARGV[2])
redis.call("SET", KEYS[3], ARGV[3], "PX", ARGV[4])
redis.call("SET", KEYS[4], ARGV[3], "PX", ARGV[5])
if ARGV[10] == "1" then
    redis.call("HSET", KEYS[5], "created_at", ARGV[11])
end
redis.call("HSET", KEYS[5], "account", ARGV[9], "head", ARGV[8], "rotated_at", ARGV[11])
redis.call("PEXPIRE", KEYS[5], ARGV[5])
redis.call("ZREMRANGEBYSCORE", KEYS[2], "-inf", ARGV[6])
redis.call("ZADD", KEYS[2], ARGV[7], ARGV[8])
redis.call("PEXPIRE", KEYS[2], math.max(oldTTL, tonumber(ARGV[5])))
return 1
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
// ARGV：账号、保留数量（0 表示全撤销）、refresh 前缀、family 前缀。
var retireRefreshScript = redis.NewScript(`
local kind = redis.call("TYPE", KEYS[1]).ok
if kind == "none" then return 0 end
if kind ~= "zset" then return redis.error_reply("invalid account refresh index") end
local count = redis.call("ZCARD", KEYS[1])
local keep = tonumber(ARGV[2])
if count <= keep then return 0 end
local tokens = redis.call("ZRANGE", KEYS[1], 0, count - keep - 1)
local families = {}
for i, token in ipairs(tokens) do
    local raw = redis.call("GET", ARGV[3] .. token)
    if raw then
        local ok, data = pcall(cjson.decode, raw)
        if not ok or type(data) ~= "table" or data.account ~= ARGV[1] then
            return redis.error_reply("invalid refresh ownership")
        end
        local family = data.family_id
        if family ~= nil then
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
end
for i, token in ipairs(tokens) do
    redis.call("DEL", ARGV[3] .. token)
    if families[i] then redis.call("DEL", families[i]) end
    redis.call("ZREM", KEYS[1], token)
end
return #tokens
`)
