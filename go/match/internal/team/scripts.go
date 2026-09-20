package team

// SharedRedis 上的组队 Lua 脚本(设计文档 docs/design/team-system.md §C.5)。
//
// 统一模式:先 S_READ 一致性读 → Go 里跑纯函数规则(rules.go)→ S_COMMIT 按 ver CAS 写。
// 所有脚本里的字段名与 keys.go 的 recField* / indexField* 常量一致(store_test 用常量读写钉住)。
//
// 时钟:所有绝对时间(epoch 起种、过期判定、server_time_ms)一律取 SharedRedis 的 TIME
// (§C.4 唯一时钟源),不用各 match 实例的 Go 墙钟。Redis 7 默认 effects replication,
// 脚本里调 TIME 之后仍可写。
//
// 数字精度:毫秒时间戳 13 位,写回或拼进参数时一律 string.format("%.0f", x),
// 避免 Lua 默认 %.14g 在 14 位以上丢精度;team_id / player_id 在脚本里只当字符串比较,
// 从不 tonumber(uint64 超出 double 精度)。
//
// 返回数组里不放 nil/false(Lua 数组遇 nil 截断),缺失值一律用 "" 或 "0" 占位。

// scriptCommit S_COMMIT:唯一的写脚本。
//
//	KEYS[1]=team:rec:<tid>  KEYS[2]=team:<tid>
//	其后依次:nJ 个新加入成员的 team:player:<pid>
//	         nK 个保留成员的 team:player:<pid>
//	         nL 个移出成员的 team:player:<pid>
//	         nIA 个新增/刷新邀请的 team:invite:<pid>
//	         nID 个删除邀请的 team:invite:<pid>
//	ARGV[1]=expectedVer:"new" = 建队专用,记录必须不存在;其余必须是现有记录的十进制 ver
//	ARGV[2]=recPb(""=解散:删 KEYS[1]/KEYS[2])  ARGV[3]=projPb  ARGV[4]=ttlSec  ARGV[5]=tid
//	ARGV[6..10]=nJ,nK,nL,nIA,nID  ARGV[11..10+nIA]=各新增邀请的 expire_at_ms
//	ARGV[11+nIA]=每个被邀请人的待处理邀请上限
//
// 返回:
//
//	{1, newVer, tidAfter_1, epoch_1, ..., tidAfter_n, epoch_n}  n = nJ+nK+nL,按 J、K、L 顺序
//	{0}      版本冲突,或非建队操作遇到记录已不存在(绝不复活已解散队伍)
//	{-1, i}  第 i 个新成员已在别的队
//	{-2, i}  第 i 个保留成员的索引指向别的队
//	{-3, i}  第 i 个新增邀请的被邀请人待处理邀请已达上限
//
// 判定段只用 HGET / ZSCORE / ZCOUNT 只读命令,任何拒绝分支都不产生写入。
//
// epoch 起种(索引缺失)= TIME 毫秒数 + 1,严格大于同一毫秒内 S_READ 对缺失索引回报的 nowMs(见 scriptRead)。
//
// 与设计稿 §C.5 的两处偏离(均为收紧):
//  1. 移出成员:索引 tid 等于本队**或索引缺失**时都置 "0"(缺失时按 TIME 起种新 epoch),
//     并按 (tidAfter, epoch) 成对返回;索引指向别队时原样返回该队 tid 与 epoch。
//     这样调用方拿到的 epoch 永远与同一次原子读出的 tid 配对,不会把"别队的 epoch"
//     配上 team_id=0 的空视图推给客户端(§C.4 "epoch 相等则 team_id 必然相等")。
//  2. 新增邀请写入前先 ZREMRANGEBYSCORE 清掉该被邀请人已过期的反查项,索引不随过期项膨胀。
const scriptCommit = `
local cur = redis.call("HGET", KEYS[1], "ver")
if ARGV[1] == "new" then
  if cur then return {0} end
elseif (not cur) or cur ~= ARGV[1] then
  return {0}
end
local tid = ARGV[5]
local ttl = ARGV[4]
local nJ, nK, nL = tonumber(ARGV[6]), tonumber(ARGV[7]), tonumber(ARGV[8])
local nIA, nID = tonumber(ARGV[9]), tonumber(ARGV[10])
local inviteCap = tonumber(ARGV[11 + nIA])
local t = redis.call("TIME")
local nowms = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local nowStr = string.format("%.0f", nowms)
for i = 1, nJ do
  local v = redis.call("HGET", KEYS[2 + i], "tid")
  if v and v ~= "0" and v ~= tid then return {-1, i} end
end
for i = 1, nK do
  local v = redis.call("HGET", KEYS[2 + nJ + i], "tid")
  if v and v ~= tid then return {-2, i} end
end
local base = 2 + nJ + nK + nL
for i = 1, nIA do
  local key = KEYS[base + i]
  if not redis.call("ZSCORE", key, tid) then
    if redis.call("ZCOUNT", key, "(" .. nowStr, "+inf") >= inviteCap then
      return {-3, i}
    end
  end
end
local function setIdx(key, v)
  local old = redis.call("HGET", key, "tid")
  local e = tonumber(redis.call("HGET", key, "epoch") or "0")
  if old ~= v then
    if e == 0 then e = nowms + 1 else e = e + 1 end
    redis.call("HSET", key, "tid", v, "epoch", string.format("%.0f", e))
  end
  redis.call("EXPIRE", key, ttl)
  return string.format("%.0f", e)
end
local newVer = string.format("%.0f", (tonumber(cur) or 0) + 1)
local out = {1, newVer}
if ARGV[2] == "" then
  redis.call("DEL", KEYS[1], KEYS[2])
else
  redis.call("HSET", KEYS[1], "ver", newVer, "pb", ARGV[2])
  redis.call("EXPIRE", KEYS[1], ttl)
  redis.call("SET", KEYS[2], ARGV[3], "EX", ttl)
end
for i = 1, nJ + nK do
  local e = setIdx(KEYS[2 + i], tid)
  out[#out + 1] = tid
  out[#out + 1] = e
end
for i = 1, nL do
  local key = KEYS[2 + nJ + nK + i]
  local v = redis.call("HGET", key, "tid")
  if (not v) or v == tid then
    local e = setIdx(key, "0")
    out[#out + 1] = "0"
    out[#out + 1] = e
  else
    out[#out + 1] = v
    out[#out + 1] = redis.call("HGET", key, "epoch") or "0"
  end
end
for i = 1, nID do redis.call("ZREM", KEYS[base + nIA + i], tid) end
for i = 1, nIA do
  local key = KEYS[base + i]
  redis.call("ZREMRANGEBYSCORE", key, "-inf", nowStr)
  redis.call("ZADD", key, ARGV[10 + i], tid)
  redis.call("EXPIRE", key, "3600")
end
return out
`

// scriptRead S_READ:一致性读(玩家索引 + 某队记录 + Redis 时钟)。
//
//	KEYS[1]=team:player:<pid>  KEYS[2]=team:rec:<t>
//	返回 {tidNow, epoch, ver, pb, recTTL, nowMs}
//	tidNow:索引缺失为 ""; epoch 缺失为 nowMs(见下); ver/pb 记录缺失为 "";
//	recTTL 为整数(-2 不存在 / -1 无 TTL);nowMs 用字符串返回。
//
// 索引缺失(24h 整队空闲过期、被淘汰、从未组过队)时 epoch 回报 Redis 当前毫秒数,只读不写:
// 客户端手里的旧 epoch = key 消失前的起种毫秒数 + tid 变更次数,变更次数远小于起种以来经过的毫秒数
// (整队过期时至少 24h),所以 nowMs 大于它,空视图能按 §H.3 被接受;若回 "0" 会被一律丢弃,
// 界面卡在已不存在的队伍上。
// 之后任何起种都是 nowMs+1(S_COMMIT setIdx / S_HEAL_ORPHAN),Redis 串行执行脚本,起种时刻不早于这次读,
// 所以同一毫秒内也不会出现"epoch 相等、team_id 不同"(§C.4 冲突判定会让客户端反复重拉)。
const scriptRead = `
local t = redis.call("TIME")
local nowStr = string.format("%.0f", tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000))
local tidNow = redis.call("HGET", KEYS[1], "tid") or ""
local epoch = redis.call("HGET", KEYS[1], "epoch") or nowStr
local r = redis.call("HMGET", KEYS[2], "ver", "pb")
return {tidNow, epoch, r[1] or "", r[2] or "", redis.call("TTL", KEYS[2]), nowStr}
`

// scriptReadMembers S_READ_MEMBERS:给其他队员构建不经提交的视图。
//
//	KEYS[1]=team:rec:<tid>  KEYS[2..]=Go 按上一次读到的成员表传入的 team:player:<m>
//	返回 {ver, pb, nowMs, tid_1, epoch_1, tid_2, epoch_2, ...}
const scriptReadMembers = `
local r = redis.call("HMGET", KEYS[1], "ver", "pb")
local t = redis.call("TIME")
local out = {r[1] or "", r[2] or "", string.format("%.0f", tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000))}
for i = 2, #KEYS do
  out[#out + 1] = redis.call("HGET", KEYS[i], "tid") or ""
  out[#out + 1] = redis.call("HGET", KEYS[i], "epoch") or "0"
end
return out
`

// scriptInviteList S_INVITE_LIST:ListMyInvites 专用,单 key。
// 用 Redis 时钟剔除已过期项(score <= now),再列出剩余项。
//
//	KEYS[1]=team:invite:<pid>
//	返回 {nowMs, tid_1, score_1, tid_2, score_2, ...}(score 为 Redis 原字符串)
const scriptInviteList = `
local t = redis.call("TIME")
local nowms = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
redis.call("ZREMRANGEBYSCORE", KEYS[1], "-inf", string.format("%.0f", nowms))
local out = {string.format("%.0f", nowms)}
local z = redis.call("ZRANGE", KEYS[1], 0, -1, "WITHSCORES")
for i = 1, #z do out[#out + 1] = z[i] end
return out
`

// scriptInvitePrune S_INVITE_PRUNE:只有 score 没变(期间没被重邀刷新)才删,
// 避免删掉队长刚写入的新索引项。
//
//	KEYS[1]=team:invite:<pid>  ARGV[1]=tid  ARGV[2]=S_INVITE_LIST 看到的 score 原字符串
//	返回 1 删了 / 0 未删
const scriptInvitePrune = `
local s = redis.call("ZSCORE", KEYS[1], ARGV[1])
if s and s == ARGV[2] then
  redis.call("ZREM", KEYS[1], ARGV[1])
  return 1
end
return 0
`

// scriptTouch S_TOUCH:续期,不改 ver。
//
//	KEYS[1]=team:rec:<tid>  KEYS[2]=team:<tid>  KEYS[3..]=成员 team:player:<m>
//	ARGV[1]=expectedVer  ARGV[2]=ttlSec  ARGV[3]=projPb(由 expectedVer 那一版记录生成)  ARGV[4]=tid
//	返回 1 已续期 / 0 ver 不一致(含记录已不存在)
//
// 相对设计稿 §C.5 的补充(§C.6 "team 下一次提交或触碰时重写投影"):投影缺失时用
// ARGV[3] 重写;成员索引只在 tid 仍指向本队时续期,不给已去别队的索引续命。
const scriptTouch = `
local cur = redis.call("HGET", KEYS[1], "ver")
if (not cur) or cur ~= ARGV[1] then return 0 end
redis.call("EXPIRE", KEYS[1], ARGV[2])
if redis.call("EXISTS", KEYS[2]) == 1 then
  redis.call("EXPIRE", KEYS[2], ARGV[2])
else
  redis.call("SET", KEYS[2], ARGV[3], "EX", ARGV[2])
end
for i = 3, #KEYS do
  if redis.call("HGET", KEYS[i], "tid") == ARGV[4] then
    redis.call("EXPIRE", KEYS[i], ARGV[2])
  end
end
return 1
`

// scriptHealOrphan S_HEAL_ORPHAN:孤儿索引自愈(索引指向的记录已不存在)。
//
//	KEYS[1]=team:player:<pid>  KEYS[2]=team:rec:<tid>  ARGV[1]=tid  ARGV[2]=ttlSec
//	条件:记录不存在 且 索引 tid == ARGV[1];满足时 tid 置 "0"、epoch+1(0 则按 TIME 毫秒数 +1 起种,与 S_COMMIT 同口径),
//	返回 1;否则 0。
const scriptHealOrphan = `
if redis.call("EXISTS", KEYS[2]) == 1 then return 0 end
if redis.call("HGET", KEYS[1], "tid") ~= ARGV[1] then return 0 end
local e = tonumber(redis.call("HGET", KEYS[1], "epoch") or "0")
if e == 0 then
  local t = redis.call("TIME")
  e = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000) + 1
else
  e = e + 1
end
redis.call("HSET", KEYS[1], "tid", "0", "epoch", string.format("%.0f", e))
redis.call("EXPIRE", KEYS[1], ARGV[2])
return 1
`
