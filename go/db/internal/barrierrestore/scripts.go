package barrierrestore

import (
	"github.com/redis/go-redis/v9"
	"shared/savebarrier"
)

const luaHandoffFenceHelpers = `
local function bytecmp(a,b)
    if #a~=#b then if #a>#b then return 1 else return -1 end end
    for i=1,#a do
        local x,y=string.byte(a,i),string.byte(b,i)
        if x~=y then if x>y then return 1 else return -1 end end
    end
    return 0
end
local function uint(s, zero)
    if type(s)~='string' or #s==0 or #s>20 or not string.match(s,'^%d+$') then return false end
    if s=='0' then return zero==true end
    if string.sub(s,1,1)=='0' then return false end
    return bytecmp(s,'18446744073709551615')<=0
end
local function cmp(a,b)
    return bytecmp(a,b)
end
local function plus2(s)
    local out,carry='',2
    for i=#s,1,-1 do
        local value=string.byte(s,i)-48+carry
        out=string.char(48+value%10)..out
        carry=math.floor(value/10)
    end
    if carry>0 then out=string.char(48+carry)..out end
    if not uint(out) then return nil end
    return out
end
local function nonce(s,zero)
    return type(s)=='string' and #s==32 and not string.find(s,'[^0-9a-f]')
        and (zero or s~=string.rep('0',32))
end
local function fence(s)
    if type(s)~='string' then return nil end
    local e,i,n,a,k,t=string.match(s,'^([^|]*)|([^|]*)|([^|]*)|([^|]*)|([^|]*)|([^|]*)$')
    if not uint(e) or not uint(n,true) or not nonce(a,true) or not nonce(t)
        or #i>256 or string.find(i,'[%c%s]') or (k~='H' and k~='F' and k~='R') then return nil end
    if n=='0' then
        if k~='R' or i~='' or a~=string.rep('0',32) then return nil end
    elseif i=='' or a==string.rep('0',32) then return nil end
    return {e=e,i=i,n=n,a=a,k=k,t=t}
end
local function sameholder(a,b)
    return a.e==b.e and a.i==b.i and a.n==b.n and a.a==b.a
end
local function types3()
    if (#KEYS~=3 and #KEYS~=4) or KEYS[1]==KEYS[2] or KEYS[1]==KEYS[3] or KEYS[2]==KEYS[3] then return false end
    for i,key in ipairs(KEYS) do
        if key=='' then return false end
        for j=1,i-1 do if key==KEYS[j] then return false end end
    end
    local b=redis.call('TYPE',KEYS[2]).ok
    local m=redis.call('TYPE',KEYS[3]).ok
    return redis.call('TYPE',KEYS[1]).ok=='string' and (b=='none' or b=='hash')
        and (m=='none' or m=='string') and (b=='none' or redis.call('PTTL',KEYS[2])==-1)
end
local function prior()
    local raw=redis.call('HGET',KEYS[2],'handoff_fence')
    if raw==false then return '',nil end
    local parsed=fence(raw)
    if not parsed then return nil,nil end
    return raw,parsed
end
local function version(s)
    return type(s)=='string' and #s==41 and string.sub(s,21,21)==':'
        and string.match(s,'^%d+:%d+$')~=nil
        and bytecmp(string.sub(s,1,20),'18446744073709551615')<=0
        and bytecmp(string.sub(s,22),'18446744073709551615')<=0 and string.sub(s,22)~=string.rep('0',20)
end
local function marker(s,e)
    local owner,time=string.match(s or '','^(%d+):(%d+)$')
    return owner==e and uint(time)
end
`

// 所有检查先于第一次写。维护键只是本工具互斥及人工停写声明，业务写者不会自动读取它。
var restoreScript = redis.NewScript(savebarrier.LuaVersionHelpers + luaHandoffFenceHelpers + `
local function is_schema_gen(s)
    if type(s) ~= 'string' or #s == 0 or #s > 10 or not string.match(s,'^[1-9]%d*$') then return false end
    return #s < 10 or not version_gt(s,'4294967295')
end
if (#KEYS ~= 75 and #KEYS ~= 72) or #ARGV ~= 10 or not uint(ARGV[2]) or not is_version(ARGV[3])
    or not is_schema_gen(ARGV[8]) or not nonce(ARGV[10]) then return redis.error_reply('restore arguments invalid') end
local schema_gen=ARGV[8]
if redis.call('TYPE',KEYS[1]).ok ~= 'string' or redis.call('GET',KEYS[1]) ~= ARGV[1]
    or redis.call('PTTL',KEYS[1]) ~= -1 then return redis.error_reply('permanent maintenance marker required') end
if redis.call('TYPE',KEYS[2]).ok ~= 'string' or redis.call('GET',KEYS[2]) ~= ARGV[2]
    or redis.call('PTTL',KEYS[2]) ~= -1 then return redis.error_reply('owner changed or expires') end
if #KEYS == 75 then
for i=73,74 do
    if i == 74 and ARGV[7] == '' then
        -- 2026-10-20 修复(r2-mz#1):计划显式允许的本区空落点(AllowHomeFallback)按 home 回退,要求落点记录此刻
        -- 仍不存在,与写屏障同一原子段复核;home 仍须是上一轮核过的永久原值。空串只可能来自已校验的计划。
        if redis.call('EXISTS',KEYS[i]) ~= 0 then return redis.error_reply('authoritative route changed or expires') end
    elseif redis.call('TYPE',KEYS[i]).ok ~= 'string' or redis.call('GET',KEYS[i]) ~= ARGV[i-67]
        or redis.call('PTTL',KEYS[i]) ~= -1 then return redis.error_reply('authoritative route changed or expires') end
end
if redis.call('EXISTS',KEYS[75]) ~= 0 then return redis.error_reply('zone merge fence exists') end
end
for i=3,7 do
    if redis.call('EXISTS',KEYS[i]) ~= 0 then return redis.error_reply('cache evidence or handoff still exists') end
end
local t=redis.call('TYPE',KEYS[8]).ok
if t ~= 'none' and t ~= 'hash' then return redis.error_reply('barrier wrong type') end
if t == 'hash' then
    if ARGV[5] ~= '1' then return redis.error_reply('barrier exists before SQL migration') end
    for _,field in ipairs(redis.call('HKEYS',KEYS[8])) do
        -- schema_writer 是 C++ 严格屏障写入的写方令牌(混版本持久字段保护 §5.2),只供 Lua 判"上次是不是本实体写的";
        -- schema_regressed 是 warn 下被判倒退的写方令牌(防回包丢失的证据)。两者都接受,写入时清掉(见文末)。
        if field ~= 'pending' and field ~= 'player_database' and field ~= 'player_database_1' and field ~= 'schema_gen'
            and field ~= 'schema_writer' and field ~= 'schema_regressed' and field ~= 'handoff_fence' then
            return redis.error_reply('barrier or outbox conflicts')
        end
    end
    if redis.call('PTTL',KEYS[8]) ~= -1
        or redis.call('HGET',KEYS[8],'pending') ~= ARGV[3]
        or redis.call('HGET',KEYS[8],'player_database') ~= ARGV[3]
        or redis.call('HGET',KEYS[8],'player_database_1') ~= ARGV[3] then
        return redis.error_reply('barrier or outbox conflicts')
    end
    -- 已有的 schema_gen 只是索引不是权威(混版本持久字段保护 §3.3):恢复以 SQL 为准,直接用 ARGV[8](两行戳最小值)
    -- 覆盖,不保留旧值、也不因旧值格式损坏而拒绝 —— 保留更高的旧值会让恢复后的第一次加载被判倒退(§13 第 8 条)。
end
-- 即使原字段缺席也写永久 R 墓碑；恢复不是重新授权某个仍活的旧加载。
local prior=redis.call('HGET',KEYS[8],'handoff_fence')
local original=nil
if prior~=false then
    original=fence(prior)
    if not original or cmp(original.e,ARGV[2])>0 then return redis.error_reply('handoff floor invalid') end
end
if (prior or '')~=ARGV[9] then return redis.error_reply('handoff floor changed') end
local restored=ARGV[2]..'||0|'..string.rep('0',32)..'|R|'..ARGV[10]
if original then
    if original.t==ARGV[10] then return redis.error_reply('handoff token must rotate') end
    restored=original.e..'|'..original.i..'|'..original.n..'|'..original.a..'|R|'..ARGV[10]
end
if ARGV[4]~='0' and ARGV[4]~='1' then return redis.error_reply('restore mode invalid') end
for i=9,72 do
    local kind=redis.call('TYPE',KEYS[i]).ok
    if kind ~= 'none' and kind ~= 'zset' then return redis.error_reply('pending index wrong type') end
    if kind == 'zset' and redis.call('ZCARD',KEYS[i]) ~= 0 then return redis.error_reply('zone pending outbox not drained') end
end
if ARGV[4] == '1' then
    redis.call('HSET',KEYS[8],'pending',ARGV[3],'player_database',ARGV[3],'player_database_1',ARGV[3],'schema_gen',schema_gen,'handoff_fence',restored)
    -- 恢复以 SQL 为准:清掉写方令牌与 warn 倒退证据,下一次任何写方的严格屏障存盘都重新做一次世代比较(fail-closed)。
    -- 合规流程里恢复时没有在线写方,结果不变;万一还有截断数据的写方在线,它不能凭旧令牌跳过 C3。
    redis.call('HDEL',KEYS[8],'schema_writer','schema_regressed')
elseif ARGV[4] ~= '0' then return redis.error_reply('restore mode invalid') end
return 1
`)

// 新维护提升有独立阶段，不改旧restore协议。所有类型、原件与待投检查先于首写；
// check无副作用；revoke只撤销并清已与双SQL同内容的原缓存；finish只确认实际SQL目标版。
var promotionScript = redis.NewScript(savebarrier.LuaVersionHelpers + luaHandoffFenceHelpers + `
if (#KEYS~=74 and #KEYS~=76) or #ARGV~=#KEYS-54 or not uint(ARGV[2]) or not uint(ARGV[3]) or cmp(ARGV[3],ARGV[2])<=0
 or not is_version(ARGV[4]) or not is_version(ARGV[5]) or (ARGV[9]~='check' and ARGV[9]~='revoke' and ARGV[9]~='finish')
 or not string.match(ARGV[6],'^[1-9]%d*$') or #ARGV[6]>10 or bytecmp(ARGV[6],'4294967295')>0 then return redis.error_reply('promotion arguments invalid') end
local unique={}
for _,key in ipairs(KEYS) do if key=='' or unique[key] then return redis.error_reply('promotion keys duplicate') end unique[key]=true end
for i=73,#KEYS,2 do
 local a=i-54
 if #ARGV[a]~=32 or string.find(ARGV[a],'[^0-9a-f]') or ARGV[a]==string.rep('0',32)
  or ARGV[a+1]=='' or redis.call('TYPE',KEYS[i]).ok~='string' or redis.call('GET',KEYS[i])~=ARGV[a+1] or redis.call('PTTL',KEYS[i])~=-1
  or redis.call('TYPE',KEYS[i+1]).ok~='string' or redis.call('GET',KEYS[i+1])~=ARGV[a] or redis.call('PTTL',KEYS[i+1])~=-1 then return redis.error_reply('permanent maintenance target required') end
end
if redis.call('TYPE',KEYS[1]).ok~='string' or redis.call('GET',KEYS[1])~=ARGV[1] or redis.call('PTTL',KEYS[1])~=-1 then return redis.error_reply('permanent maintenance required') end
if redis.call('TYPE',KEYS[2]).ok~='string' or redis.call('PTTL',KEYS[2])~=-1 then return redis.error_reply('owner type or ttl invalid') end
local bkind=redis.call('TYPE',KEYS[8]).ok
if bkind~='hash' or redis.call('PTTL',KEYS[8])~=-1 then return redis.error_reply('converged permanent barrier required') end
if redis.call('TYPE',KEYS[7]).ok~='none' then return redis.error_reply('handoff still present') end
for i=3,5 do local k=redis.call('TYPE',KEYS[i]).ok if k~='none' and k~='string' then return redis.error_reply('cache wrong type') end end
local assetKind=redis.call('TYPE',KEYS[6]).ok
if assetKind~='none' and assetKind~='hash' then return redis.error_reply('asset evidence wrong type') end
if assetKind=='hash' and (redis.call('HLEN',KEYS[6])~=2 or redis.call('PTTL',KEYS[6])~=-1) then return redis.error_reply('asset evidence shape invalid') end
for i=9,72 do local k=redis.call('TYPE',KEYS[i]).ok if k~='none' and k~='zset' then return redis.error_reply('pending type invalid') end if k=='zset' and redis.call('ZCARD',KEYS[i])~=0 then return redis.error_reply('pending work not drained') end end
for _,field in ipairs(redis.call('HKEYS',KEYS[8])) do
 if field~='pending' and field~='player_database' and field~='player_database_1' and field~='schema_gen' and field~='schema_writer' and field~='schema_regressed' and field~='handoff_fence' and field~='verified_identity' then return redis.error_reply('outbox still present') end
end
local stamp=redis.call('HGET',KEYS[8],'verified_identity')
if stamp and (#stamp~=81 or string.sub(stamp,41,41)~=':' or string.find(string.sub(stamp,1,40),'[^0-9a-f]') or string.find(string.sub(stamp,42),'[^0-9a-f]')) then return redis.error_reply('verified identity malformed') end
local oldOwner=redis.call('GET',KEYS[2]);local oldFence=redis.call('HGET',KEYS[8],'handoff_fence') or ''
local proposed=fence(ARGV[8]);local original=nil
if ARGV[7]~='' then original=fence(ARGV[7]);if not original or cmp(original.e,ARGV[2])>0 then return redis.error_reply('original fence invalid') end end
if not proposed or proposed.k~='R' then return redis.error_reply('revocation invalid') end
if original then if not sameholder(original,proposed) or original.t==proposed.t then return redis.error_reply('revocation must preserve holder and rotate nonce') end
elseif proposed.e~=ARGV[3] or proposed.n~='0' then return redis.error_reply('missing prior revocation invalid') end
local already=oldOwner==ARGV[3] and oldFence==ARGV[8]
if not already and (oldOwner~=ARGV[2] or oldFence~=ARGV[7]) then return redis.error_reply('owner or cached revocation changed') end
local pending=redis.call('HGET',KEYS[8],'pending')
local a=redis.call('HGET',KEYS[8],'player_database');local b=redis.call('HGET',KEYS[8],'player_database_1')
if pending~=a or pending~=b or (pending~=ARGV[4] and not (already and pending==ARGV[5])) then return redis.error_reply('SQL ACK or pending not converged') end
for i=3,5 do
 local offset=10+(i-3)*2;local flag=ARGV[offset]
 if flag~='0' and flag~='1' then return redis.error_reply('cache presence invalid') end
 if already then if redis.call('EXISTS',KEYS[i])~=0 then return redis.error_reply('cache reappeared after revocation') end
 elseif flag=='0' then if redis.call('EXISTS',KEYS[i])~=0 then return redis.error_reply('unexpected cache') end
 elseif redis.call('GET',KEYS[i])~=ARGV[offset+1] then return redis.error_reply('cache bytes changed or missing') end
end
if ARGV[16]~='0' and ARGV[16]~='1' then return redis.error_reply('asset presence invalid') end
if already then if assetKind~='none' then return redis.error_reply('asset cache reappeared') end
elseif ARGV[16]=='0' then if assetKind~='none' then return redis.error_reply('unexpected asset evidence') end
elseif assetKind~='hash' or redis.call('HGET',KEYS[6],'version')~=ARGV[17] or redis.call('HGET',KEYS[6],'data')~=ARGV[18] or ARGV[17]~=ARGV[4] then return redis.error_reply('asset evidence changed') end
if ARGV[9]=='check' then if already then return redis.error_reply('new plan cannot adopt old revocation') end return 1 end
if ARGV[9]=='finish' then
 if not already then return redis.error_reply('revocation not confirmed') end
 redis.call('HSET',KEYS[8],'pending',ARGV[5],'player_database',ARGV[5],'player_database_1',ARGV[5],'schema_gen',ARGV[6])
 redis.call('HDEL',KEYS[8],'schema_writer','schema_regressed')
 return 1
end
if not already then
 redis.call('SET',KEYS[2],ARGV[3])
 redis.call('HSET',KEYS[8],'handoff_fence',ARGV[8])
 -- 仅原缓存已与SQL逐内容核对且同Lua再次比原bytes时才清；pending/ACK/outbox不删不伪造。
 redis.call('DEL',KEYS[3],KEYS[4],KEYS[5],KEYS[6])
end
return 1
`)

// 一物理目标只发一条EVAL；全部角色marker、guard及原角色状态均在首写前原子复核。
const maintenanceReleaseLua = `
local markers,guards,players=tonumber(ARGV[3]),tonumber(ARGV[4]),tonumber(ARGV[5])
if (ARGV[1]~='check' and ARGV[1]~='apply') or not markers or markers<1 or markers>2
 or not guards or guards<1 or guards>2 or not players or players<0 or players>1000
 or #KEYS~=2*markers+1+guards+2*players or #ARGV~=6+2*markers+guards+3*players then return redis.error_reply('release shape invalid') end
local expires=tonumber(ARGV[6])
local clock=redis.call('TIME')
if not expires or expires<=tonumber(clock[1])*1000+math.floor(tonumber(clock[2])/1000) then return redis.error_reply('release window expired') end
for i,key in ipairs(KEYS) do
 if key=='' then return redis.error_reply('release empty key') end
 for j=1,i-1 do if key==KEYS[j] then return redis.error_reply('release duplicate key') end end
end
local function permanent(key,want)
 return redis.call('TYPE',key).ok=='string' and redis.call('PTTL',key)==-1 and redis.call('GET',key)==want
end
for i=1,markers do
 local k,a=2*(i-1),6+2*(i-1)
 if not permanent(KEYS[k+1],ARGV[a+1]) or not permanent(KEYS[k+2],ARGV[a+2]) then return redis.error_reply('release maintenance target changed') end
end
local base=2*markers+1
local argbase=6+2*markers
local rt=redis.call('TYPE',KEYS[base]).ok
if rt~='none' and rt~='string' then return redis.error_reply('release receipt type') end
if rt=='string' and (ARGV[2]=='' or not permanent(KEYS[base],ARGV[2])) then return redis.error_reply('release receipt conflict or ttl') end
local present=0
for i=1,guards do
 local k=KEYS[base+i]
 if redis.call('TYPE',k).ok~='none' then
  if not permanent(k,ARGV[argbase+i]) then return redis.error_reply('release guard changed or ttl') end
  present=present+1
 end
end
if rt=='none' and present~=guards then return redis.error_reply('release missing guard without receipt') end
if rt=='string' and present~=0 then return redis.error_reply('release receipt with residual guard') end
for i=1,players do
 local k=base+guards+2*(i-1)
 local a=argbase+guards+3*(i-1)
 local owner,barrier=KEYS[k+1],KEYS[k+2]
 if not permanent(owner,ARGV[a+1]) or redis.call('TYPE',barrier).ok~='hash' or redis.call('PTTL',barrier)~=-1 then return redis.error_reply('release owner/barrier changed') end
 local values=redis.call('HMGET',barrier,'pending','player_database','player_database_1','handoff_fence')
 if values[1]~=ARGV[a+2] or values[2]~=ARGV[a+2] or values[3]~=ARGV[a+2] or (values[4] or '')~=ARGV[a+3] then return redis.error_reply('release SQL ACK/fence mismatch') end
 for _,field in ipairs(redis.call('HKEYS',barrier)) do
  if string.sub(field,1,7)=='outbox_' then return redis.error_reply('release pending outbox') end
 end
end
if rt=='string' then return 'committed' end
if ARGV[1]=='check' then return 'pending' end
if ARGV[2]=='' then return redis.error_reply('release apply needs immutable plan receipt') end
-- 首写之后只有固定string的SET和DEL，无后续可能WRONGTYPE的命令。
redis.call('SET',KEYS[base],ARGV[2])
for i=1,guards do redis.call('DEL',KEYS[base+i]) end
return 'committed'
`
