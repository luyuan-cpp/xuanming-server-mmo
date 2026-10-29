package logic

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gproto "google.golang.org/protobuf/proto"

	"proto/scene_manager"
	"scene_manager/internal/svc"
	"shared/ownerepoch"
)

// ---------------------------------------------------------------------------
// 跨语言 Lua 金样(GO-2 根治,cross-zone-scene-travel.md §12.8)
//
// GO-2 的安全性建立在三段跑在同一个 Redis 上的原子 Lua 之间的先后关系上,它们分属两种语言:
//
//   - scene_manager 的回滚 luaRollbackPlayerPlacement(本包 owner_epoch.go,直接引用包内常量);
//   - 目标 scene 载入时的 A2′ kLuaInheritClear(C++ exit_release_mark.h);
//   - 源 scene 的原子取证 kLuaJudgeTravelOutcome(C++ player_lifecycle.h 的 travel_outcome)。
//
// C++ 那两段没有 Go 侧的生成物,这里**逐字复制**,在 miniredis 上与 Go 的回滚 / 铸造 Lua 一起跑:
// 锁住「A2′ 与回滚互斥」「取证只删本族标记并读得到回执」这两条跨语言契约。
// **修改须两边同步**:改了 C++ 的任何一段,必须同步改这里的副本(TestSourceJudgeScriptAndInheritClearCopiesMatchCpp
// 在仓库完整时会逐字比对并报出漂移;只拷了 go/ 的构建环境里找不到 C++ 源码则跳过比对)。
// kLuaJudgeTravelOutcome 的副本去掉了首行 "#!lua":miniredis 内置的 gopher-lua 不认 shebang(真 Redis ≥ 7 由它
// 让脚本在只读副本 / MISCONF 下整体被拒,那一条性质只能靠 runbook 实跑验证)。
// ---------------------------------------------------------------------------

// C++ 单一 helper + 具名脚本的逐字金样；读取器只接受这些固定原始字面量。
const cppHandoffFenceHelpers = `
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
const cppLuaInheritClear = cppHandoffFenceHelpers + `
if #ARGV~=2 or not types3() then return redis.error_reply('HANDOFF confirm shape') end
local expected=fence(ARGV[2])
local raw=prior()
if not expected or expected.k~='H' or expected.e~=ARGV[1] or raw==nil then return redis.error_reply('HANDOFF confirm record') end
if redis.call('GET',KEYS[1])~=ARGV[1] or raw~=ARGV[2] then return -1 end
return 0
`
const cppLuaRegisterFence = cppHandoffFenceHelpers + `
if #ARGV~=3 or not types3() then return redis.error_reply('HANDOFF register shape') end
local candidate=fence(ARGV[2])
local raw,old=prior()
if not uint(ARGV[1]) or not candidate or candidate.k~='H' or candidate.e~=ARGV[1] or raw==nil then
    return redis.error_reply('HANDOFF register record') end
if redis.call('GET',KEYS[1])~=ARGV[1] then return -1 end
if raw==ARGV[2] then return 1 end
if raw~=ARGV[3] then return -2 end
if old then
    local order=cmp(candidate.e,old.e)
    if order<0 or (order==0 and (old.n=='0' or candidate.i~=old.i or cmp(candidate.n,old.n)<=0)) then return -2 end
end
local mark=redis.call('GET',KEYS[3])
if mark then
    local e,t=string.match(mark,'^(%d+):(%d+)$')
    if not uint(e) or not uint(t) or cmp(e,candidate.e)>0 then return redis.error_reply('HANDOFF register marker') end
end
redis.call('HSET',KEYS[2],'handoff_fence',ARGV[2])
if mark then redis.call('DEL',KEYS[3]) end
return 1
`
const cppLuaJudgeTravelOutcome = cppHandoffFenceHelpers + `
if #KEYS~=4 or #ARGV~=4 then return redis.error_reply('HANDOFF outcome shape') end
-- 将前三键映射为 owner/barrier/mark，第四键是只读 location。
local location=KEYS[4]
if redis.call('TYPE',location).ok~='none' and redis.call('TYPE',location).ok~='string' then
    return redis.error_reply('HANDOFF location type') end
if not types3() then return redis.error_reply('HANDOFF outcome keys') end
local held,published,next=fence(ARGV[1]),fence(ARGV[2]),fence(ARGV[3])
local raw=prior()
if not held or not published or not next or raw==nil or not sameholder(held,published)
    or not sameholder(held,next) or held.k~='H' or next.k~='R'
    or (published.k~='F' and ARGV[2]~=ARGV[1]) or (published.k=='F' and published.t==held.t)
    or next.t==held.t or next.t==published.t then return redis.error_reply('HANDOFF outcome record') end
if raw~=ARGV[1] and raw~=ARGV[2] and raw~=ARGV[3] then return redis.error_reply('HANDOFF outcome superseded') end
local owner=redis.call('GET',KEYS[1])
if not uint(owner) then return redis.error_reply('HANDOFF outcome owner') end
local mark=redis.call('GET',KEYS[3])
if mark then
    local e,t=string.match(mark,'^(%d+):(%d+)$')
    if not uint(e) or not uint(t) then return redis.error_reply('HANDOFF outcome marker') end
end
local stamp=string.match(mark or '', '^%d+:(%d+)$')
redis.call('HSET',KEYS[2],'handoff_fence',ARGV[3])
if stamp==ARGV[4] then redis.call('DEL',KEYS[3]) end
return {owner,redis.call('GET',location)}
`
const cppLuaRevokeFence = cppHandoffFenceHelpers + `
if #ARGV~=4 or not types3() then return redis.error_reply('HANDOFF revoke shape') end
local held,published,next=fence(ARGV[1]),fence(ARGV[2]),fence(ARGV[3])
local raw=prior()
if not held or not published or not next or raw==nil or held.k~='H'
    or not sameholder(held,published) or not sameholder(held,next)
    or (published.k~='F' and ARGV[2]~=ARGV[1]) or (published.k=='F' and published.t==held.t)
    or (next.k~='H' and next.k~='R') or next.t==held.t or next.t==published.t then
    return redis.error_reply('HANDOFF revoke record') end
local owner=redis.call('GET',KEYS[1])
if not uint(owner) then return redis.error_reply('HANDOFF revoke owner') end
if next.k=='H' and owner~=held.e then return 0 end
if raw~=ARGV[1] and raw~=ARGV[2] and raw~=ARGV[3] then return 0 end
local mark=redis.call('GET',KEYS[3])
if mark then
    local e,t=string.match(mark,'^(%d+):(%d+)$')
    if not uint(e) or not uint(t) then return redis.error_reply('HANDOFF revoke marker') end
end
local stamp=string.match(mark or '', '^%d+:(%d+)$')
redis.call('HSET',KEYS[2],'handoff_fence',ARGV[3])
if mark and ARGV[4]~='' and stamp==ARGV[4] then redis.call('DEL',KEYS[3]) end
return 1
`
const cppLuaReleaseFence = cppHandoffFenceHelpers + `
if #ARGV~=5 or not types3() then return redis.error_reply('HANDOFF release shape') end
local held,next=fence(ARGV[4]),fence(ARGV[5])
local raw=prior()
if not held or not next or raw==nil or held.k~='H' or next.k~='F' or not sameholder(held,next)
    or held.t==next.t or held.e~=ARGV[1] or not marker(ARGV[2],ARGV[1]) or ARGV[3]~='300' then
    return redis.error_reply('HANDOFF release record') end
if redis.call('GET',KEYS[1])~=ARGV[1] then return 0 end
if raw==ARGV[5] then return 1 end
if raw~=ARGV[4] then return -2 end
redis.call('HSET',KEYS[2],'handoff_fence',ARGV[5])
redis.call('SET',KEYS[3],ARGV[2],'EX',ARGV[3])
return 1
`

// 会话结束凭证(location-release-binding.md §3.3,2026-10-23):原件来自 scene 的 HGET 观察,H / F / R 皆可。
const cppLuaCredentialFence = cppHandoffFenceHelpers + `
if #ARGV~=5 or not types3() then return redis.error_reply('HANDOFF credential shape') end
local observed,next=fence(ARGV[4]),fence(ARGV[5])
local raw=prior()
if not observed or not next or raw==nil or next.k~='F' or not sameholder(observed,next)
    or observed.t==next.t or observed.e~=ARGV[1] or not marker(ARGV[2],ARGV[1]) or ARGV[3]~='300' then
    return redis.error_reply('HANDOFF credential record') end
if redis.call('GET',KEYS[1])~=ARGV[1] then return 0 end
if raw==ARGV[5] then return 1 end
if raw~=ARGV[4] then return -2 end
redis.call('HSET',KEYS[2],'handoff_fence',ARGV[5])
redis.call('SET',KEYS[3],ARGV[2],'EX',ARGV[3])
return 1
`
const cppLuaPublishFence = cppHandoffFenceHelpers + `
if #ARGV~=5 or not types3() then return redis.error_reply('HANDOFF_SQL shape') end
local held,next=fence(ARGV[4]),fence(ARGV[5])
local raw=prior()
if not held or not next or raw==nil or held.k~='H' or next.k~='F' or not sameholder(held,next)
    or held.t==next.t or held.e~=ARGV[1] or not marker(ARGV[2],ARGV[1]) or ARGV[3]~='300' then
    return redis.error_reply('HANDOFF_SQL record') end
if redis.call('GET',KEYS[1])~=ARGV[1] then return -1 end
if raw==ARGV[5] then return 1 end
if raw~=ARGV[4] then return -2 end
local ack=redis.call('HMGET',KEYS[2],'pending','player_database','player_database_1')
if not version(ack[1]) or (ack[2] and not version(ack[2])) or (ack[3] and not version(ack[3]))
    or string.sub(ack[1],1,20)~=string.rep('0',20-#ARGV[1])..ARGV[1] then
    return redis.error_reply('HANDOFF_SQL stored version') end
if ack[2]~=ack[1] or ack[3]~=ack[1] then return 0 end
redis.call('HSET',KEYS[2],'handoff_fence',ARGV[5])
redis.call('SET',KEYS[3],ARGV[2],'EX',ARGV[3])
return 1
`
const cppLuaResumeFence = cppHandoffFenceHelpers + `
if #KEYS~=4 or #ARGV~=4 then return redis.error_reply('HANDOFF resume shape') end
local location=KEYS[4]
local t=redis.call('TYPE',location).ok
if t~='none' and t~='string' then return redis.error_reply('HANDOFF resume location') end
if not types3() then return redis.error_reply('HANDOFF resume keys') end
local old,next=fence(ARGV[1]),fence(ARGV[2])
local raw=prior()
if not old or not next or raw==nil or old.k~='R' or next.k~='H'
    or old.i~=next.i or old.n~=next.n or old.a~=next.a or old.t==next.t
    or next.e~=ARGV[3] or (next.e~=old.e and next.e~=plus2(old.e)) then return redis.error_reply('HANDOFF resume record') end
if redis.call('GET',KEYS[1])~=ARGV[3] or (redis.call('GET',location) or '')~=ARGV[4] then return 0 end
if redis.call('EXISTS',KEYS[3])~=0 then return 0 end
if raw==ARGV[2] then return 1 end
if raw~=ARGV[1] then return 0 end
redis.call('HSET',KEYS[2],'handoff_fence',ARGV[2])
return 1
`

const cppLuaCancelRegistration = cppHandoffFenceHelpers + `
if #ARGV~=4 or not types3() then return redis.error_reply('HANDOFF cancel registration shape') end
local candidate,next=fence(ARGV[2]),fence(ARGV[4])
local raw,old=prior()
if not uint(ARGV[1]) or not candidate or not next or raw==nil or candidate.e~=ARGV[1]
    or candidate.k~='H' or next.k~='R' or not sameholder(candidate,next) or candidate.t==next.t then
    return redis.error_reply('HANDOFF cancel registration record') end
local owner=redis.call('GET',KEYS[1])
if not uint(owner) or cmp(owner,candidate.e)<0 then return redis.error_reply('HANDOFF cancel registration owner') end
if raw==ARGV[4] then return 1 end
if raw~=ARGV[2] then
    if raw~=ARGV[3] then return 0 end
    if old then
        local order=cmp(candidate.e,old.e)
        if order<0 or (order==0 and (old.n=='0' or candidate.i~=old.i or cmp(candidate.n,old.n)<=0)) then return 0 end
    end
end
local mark=redis.call('GET',KEYS[3])
if mark then
    local e,t=string.match(mark,'^(%d+):(%d+)$')
    if not uint(e) or not uint(t) then return redis.error_reply('HANDOFF cancel registration marker') end
end
redis.call('HSET',KEYS[2],'handoff_fence',ARGV[4])
return 1
`

var crossLangRepoRoot = filepath.Join("..", "..", "..", "..")

func cppFenceRaw(t *testing.T, source, name string) string {
	t.Helper()
	start := strings.Index(source, name+" = ")
	require.GreaterOrEqual(t, start, 0, name)
	start += len(name + " = ")
	raw := strings.Index(source[start:], "R\"(")
	require.GreaterOrEqual(t, raw, 0, name)
	start += raw + 3
	end := strings.Index(source[start:], ")\";")
	require.GreaterOrEqual(t, end, 0, name)
	return source[start : start+end]
}
func TestSourceJudgeScriptAndInheritClearCopiesMatchCpp(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(crossLangRepoRoot, "cpp/libs/services/scene/player/system/handoff_sql_barrier.h"))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("仅Go源码包无C++源；行为金样仍执行")
	}
	require.NoError(t, err)
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	prefix := cppFenceRaw(t, source, "kFenceLuaHelpers")
	require.Equal(t, cppHandoffFenceHelpers, prefix)
	require.Equal(t, cppLuaCancelRegistration, prefix+cppFenceRaw(t, source, "kCancelRegistrationLua"))
	require.Equal(t, cppLuaInheritClear, prefix+cppFenceRaw(t, source, "kConfirmFenceLua"), "kConfirmFenceLua")
	require.Equal(t, cppLuaRegisterFence, prefix+cppFenceRaw(t, source, "kRegisterFenceLua"), "kRegisterFenceLua")
	require.Equal(t, cppLuaJudgeTravelOutcome, prefix+cppFenceRaw(t, source, "kJudgeOutcomeLua"), "kJudgeOutcomeLua")
	require.Equal(t, cppLuaRevokeFence, prefix+cppFenceRaw(t, source, "kRevokeFenceLua"), "kRevokeFenceLua")
	require.Equal(t, cppLuaReleaseFence, prefix+cppFenceRaw(t, source, "kReleaseMarkLua"), "kReleaseMarkLua")
	require.Equal(t, cppLuaCredentialFence, prefix+cppFenceRaw(t, source, "kCredentialMarkLua"), "kCredentialMarkLua")
	require.Equal(t, cppLuaPublishFence, prefix+cppFenceRaw(t, source, "kPublishMarkLua"), "kPublishMarkLua")
	require.Equal(t, cppLuaResumeFence, prefix+cppFenceRaw(t, source, "kResumeFenceLua"), "kResumeFenceLua")
}

// 令牌互斥(§12.8 第六节):目标节点 B 载入时的 A2′ 与 scene_manager 的凭标记回滚是同一 Redis 上的两段原子 Lua,
// 只能先后执行,两种先后都不会出现双持有。
func TestRollbackAndInheritClearAreMutuallyExclusive(t *testing.T) {
	log := NewEnterSceneLogic(context.Background(), &svc.ServiceContext{}).Logger

	// setup 摆出「源节点 10 持有 epoch 1、已写标记 1:…;凭它铸出 2,落点是节点 20(目标 B)」,返回落点与回滚计划。
	setup := func(t *testing.T, playerID uint64) (*svc.ServiceContext, *miniredis.Miniredis, placedLocation, routeRollbackPlan) {
		t.Helper()
		sc, mr := newTestSvcCtxWithWorldScenes(t)
		require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, 7381, "10", testZoneId))
		require.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
		oldRaw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
		oldLoc, err := GetPlayerLocation(context.Background(), sc, playerID)
		require.NoError(t, err)
		writeHandoffMarker(t, sc, playerID, 1)
		marker, _ := sc.Redis.Get(ownerepoch.HandoffKey(playerID))
		placed, err := placePlayerLocation(sc, playerID, 7382, "20", testZoneId,
			placementGuard{observedEpoch: 1, mint: true, requiredMarker: marker})
		require.NoError(t, err)
		require.Equal(t, uint64(2), placed.epoch)
		plan, err := planRouteRollback(oldLoc, oldRaw, testZoneId, placed, marker, false)
		require.NoError(t, err)
		require.Equal(t, "3:1757000000000", plan.forwardMarker)
		return sc, mr, placed, plan
	}
	// inheritClear 以 N 执行一次 C++ 的 A2′,返回它的结果码。
	inheritClear := func(t *testing.T, sc *svc.ServiceContext, playerID uint64, n uint64) string {
		t.Helper()
		owner := strconv.FormatUint(n, 10)
		barrier := fmt.Sprintf("player:{%d}:save_barrier", playerID)
		held := owner + "|destination|1|" + strings.Repeat("a", 32) + "|H|" + strings.Repeat("b", 32)
		prior, _ := sc.Redis.Hget(barrier, "handoff_fence")
		keys := []string{ownerepoch.OwnerEpochKey(playerID), barrier, ownerepoch.HandoffKey(playerID)}
		result, err := sc.Redis.Eval(cppLuaRegisterFence, keys, owner, held, prior)
		require.NoError(t, err)
		if fmt.Sprint(result) != "1" {
			return fmt.Sprint(result)
		}
		result, err = sc.Redis.Eval(cppLuaInheritClear, keys, owner, held)
		require.NoError(t, err)
		return fmt.Sprint(result)
	}

	t.Run("inherit_clear_first_consumes_the_token", func(t *testing.T) {
		const playerID = uint64(6381)
		sc, mr, placed, plan := setup(t, playerID)

		// B 载入(N = 2):owner_epoch 核对通过,删掉代际 1 的标记(比 N 旧,结果码 1)。
		assert.Equal(t, "0", inheritClear(t, sc, playerID, 2))
		assert.False(t, mr.Exists(ownerepoch.HandoffKey(playerID)))

		markerGoneBefore := enterSceneRollbackCount(t, rollbackOutcomeMarkerGone)
		restored, echo := rollbackPlayerPlacement(sc, log, playerID, placed, plan)
		assert.False(t, restored, "令牌已被 B 消费:回滚一个字节都不动")
		assert.Equal(t, uint64(0), echo, "没回滚就不回显")
		assert.Equal(t, markerGoneBefore+1, enterSceneRollbackCount(t, rollbackOutcomeMarkerGone))
		assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "归属留在 B")
		raw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
		assert.Equal(t, placed.raw, raw, "location 仍是本次落点:源端取证读到 (2, 指向 B) 自行销毁")
		assert.False(t, mr.Exists(ownerepoch.HandoffKey(playerID)), "marker_gone 不写转写标记")
	})

	t.Run("rollback_first_makes_inherit_clear_refuse", func(t *testing.T) {
		const playerID = uint64(6382)
		sc, mr, placed, plan := setup(t, playerID)

		restored, echo := rollbackPlayerPlacement(sc, log, playerID, placed, plan)
		require.True(t, restored)
		require.Equal(t, uint64(3), echo)

		// 迟到投递的路由让 B 载入(N = 2):owner_epoch 已是 3,A2′ 回 -1,B 拒建实体、发不出 DBTask(2)。
		assert.Equal(t, "-1", inheritClear(t, sc, playerID, 2))
		forwarded, _ := sc.Redis.Get(ownerepoch.HandoffKey(playerID))
		assert.Equal(t, plan.forwardMarker, forwarded, "核对不过一个标记都不删:转写标记留给源端取证 / 改派")
		assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID))

		// 源端之后按回滚后的 epoch 重载(同落点重连,N = 3)时,A2′ 删掉转写标记(代际恰是 N,结果码 2)。
		assert.Equal(t, "0", inheritClear(t, sc, playerID, 3))
		assert.False(t, mr.Exists(ownerepoch.HandoffKey(playerID)))
	})
}

// 源端原子取证(§12.8 3.1):只删本次交接这一族标记(原标记与回滚转写出来的那份),别人的不碰;
// 同一段脚本里读到的 location 带着与本次标记原文逐字节相同的回滚回执。
func TestSourceJudgeScriptDeletesOnlyItsOwnLineageAndSeesReceipt(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const savedAt = "1757000000000"
	judge := func(t *testing.T, playerID uint64) []any {
		t.Helper()
		barrier := fmt.Sprintf("player:{%d}:save_barrier", playerID)
		held := "5|source|1|" + strings.Repeat("a", 32) + "|H|" + strings.Repeat("b", 32)
		published := strings.Replace(held, "|H|"+strings.Repeat("b", 32), "|F|"+strings.Repeat("c", 32), 1)
		revoked := strings.Replace(held, "|H|"+strings.Repeat("b", 32), "|R|"+strings.Repeat("d", 32), 1)
		if !mr.Exists(ownerepoch.OwnerEpochKey(playerID)) {
			require.NoError(t, mr.Set(ownerepoch.OwnerEpochKey(playerID), "5"))
		}
		mr.HSet(barrier, "handoff_fence", published)
		result, err := sc.Redis.Eval(cppLuaJudgeTravelOutcome,
			[]string{ownerepoch.OwnerEpochKey(playerID), barrier, ownerepoch.HandoffKey(playerID), getPlayerLocationKey(playerID)},
			held, published, revoked, savedAt)
		require.NoError(t, err)
		reply, ok := result.([]any)
		require.True(t, ok, "应答必须是数组,实际 %T", result)
		require.Len(t, reply, 2, "两元素:owner_epoch、location")
		return reply
	}

	t.Run("deletes_only_its_own_lineage", func(t *testing.T) {
		cases := []struct {
			mark    string
			deleted bool
		}{
			{"5:" + savedAt, true},
			{"7:" + savedAt, true},
			{"6:1757000009999", false},
			{"5:" + savedAt + "1", false},
		}
		for i, c := range cases {
			playerID := uint64(6391 + i)
			require.NoError(t, sc.Redis.Set(ownerepoch.HandoffKey(playerID), c.mark))
			judge(t, playerID)
			assert.Equal(t, !c.deleted, mr.Exists(ownerepoch.HandoffKey(playerID)), "mark=%s", c.mark)
		}
	})

	t.Run("missing_keys_are_nil_not_errors", func(t *testing.T) {
		reply := judge(t, 6397)
		assert.Equal(t, "5", reply[0], "资格要求有效原owner，缺location仍可取证")
		assert.Nil(t, reply[1], "location 缺键:nil(C++ 按解析失败)")
	})

	t.Run("rollback_receipt_is_visible_after_lineage_delete", func(t *testing.T) {
		const playerID = uint64(6398)
		marker := "5:" + savedAt
		old := &scene_manager.PlayerLocation{
			SceneId: 7398, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 5, UpdateTime: 1_757_000_000,
		}
		oldBytes, err := gproto.Marshal(old)
		require.NoError(t, err)
		require.NoError(t, sc.Redis.Set(getPlayerLocationKey(playerID), string(oldBytes)))
		require.NoError(t, sc.Redis.Set(ownerepoch.OwnerEpochKey(playerID), "5"))
		require.NoError(t, sc.Redis.Set(ownerepoch.HandoffKey(playerID), marker))

		// 凭 "5:t" 铸出 6(落点节点 20),推路由失败:bump 回滚到 7,回执 "5:t",标记转写成 "7:t"。
		placed, err := placePlayerLocation(sc, playerID, 7399, "20", testZoneId,
			placementGuard{observedEpoch: 5, mint: true, requiredMarker: marker})
		require.NoError(t, err)
		require.Equal(t, uint64(6), placed.epoch)
		plan, err := planRouteRollback(old, string(oldBytes), testZoneId, placed, marker, false)
		require.NoError(t, err)
		restored, echo := rollbackPlayerPlacement(sc, NewEnterSceneLogic(context.Background(), sc).Logger, playerID, placed, plan)
		require.True(t, restored)
		require.Equal(t, uint64(7), echo)
		forwarded, _ := sc.Redis.Get(ownerepoch.HandoffKey(playerID))
		require.Equal(t, "7:"+savedAt, forwarded)

		reply := judge(t, playerID)
		assert.False(t, mr.Exists(ownerepoch.HandoffKey(playerID)), "转写标记与原标记同一族:一并删掉")
		assert.Equal(t, "7", reply[0])
		locRaw, ok := reply[1].(string)
		require.True(t, ok, "location 必须是字符串,实际 %T", reply[1])
		assert.Equal(t, plan.restoredRaw, locRaw)
		loc := &scene_manager.PlayerLocation{}
		require.NoError(t, gproto.Unmarshal([]byte(locRaw), loc))
		assert.Equal(t, marker, loc.RollbackReceipt,
			"回执 == 本次标记原文:C++ 按 HandoffRedisValue(markEpoch, requestedAtMs) 逐字节比对")
		assert.Equal(t, uint64(7), loc.OwnerEpoch, "location 记的 epoch 与键一致(ClassifyOwnership 的交叉校验)")
		assert.Equal(t, "10", loc.NodeId, "location 指回源节点")
		assert.Equal(t, testZoneId, loc.ZoneId)

		// 源端采纳之后,任何凭这一族标记的铸造都过不了门:标记已被取证原子删掉(铸造 Lua 回 -1)。
		result, err := sc.Redis.Eval(luaMintEpochAndSetLocation,
			[]string{ownerepoch.OwnerEpochKey(playerID), getPlayerLocationKey(playerID), ownerepoch.HandoffKey(playerID)},
			"7", "late-bytes", "7:"+savedAt)
		require.NoError(t, err)
		assert.Equal(t, "-1", fmt.Sprint(result))
		assert.Equal(t, "7", ownerEpochRaw(t, sc, playerID))
	})
}

func TestHandoffFenceRejectsLateRegistrationPublishAndRevocation(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const id = uint64(63991)
	barrier := fmt.Sprintf("player:{%d}:save_barrier", id)
	keys := []string{ownerepoch.OwnerEpochKey(id), barrier, ownerepoch.HandoffKey(id)}
	require.NoError(t, mr.Set(keys[0], "7"))
	makeFence := func(e, s, n, a, k, token string) string {
		return strings.Join([]string{e, s, n, strings.Repeat(a, 32), k, strings.Repeat(token, 32)}, "|")
	}
	held := makeFence("7", "scene", "1", "a", "H", "b")
	newer := makeFence("7", "scene", "2", "c", "H", "d")
	eval := func(script string, args ...any) string {
		t.Helper()
		v, err := sc.Redis.Eval(script, keys, args...)
		require.NoError(t, err)
		return fmt.Sprint(v)
	}
	require.Equal(t, "1", eval(cppLuaRegisterFence, "7", held, ""))
	require.Equal(t, "1", eval(cppLuaRegisterFence, "7", newer, held))
	// 旧加载 HGET 故意晚到，取得的是新 prior；提交时 N 下限仍必须拒绝旧加载。
	before := mr.Dump()
	require.Equal(t, "-2", eval(cppLuaRegisterFence, "7", held, newer))
	require.Equal(t, before, mr.Dump())
	require.Equal(t, "-2", eval(cppLuaRegisterFence, "7", makeFence("7", "scene", "2", "e", "H", "f"), newer))
	require.Equal(t, "-2", eval(cppLuaRegisterFence, "7", makeFence("7", "other", "3", "a", "H", "b"), newer))
	published := makeFence("7", "scene", "2", "c", "F", "e")
	revoked := makeFence("7", "scene", "2", "c", "R", "f")
	require.False(t, mr.Exists(keys[2]))
	require.Equal(t, "1", eval(cppLuaRevokeFence, newer, published, revoked, "1000"))
	before = mr.Dump()
	require.Equal(t, "-2", eval(cppLuaRegisterFence, "7", newer, revoked), "同N不能在撤销后复活")
	require.Equal(t, "-2", eval(cppLuaReleaseFence, "7", "7:1000", "300", newer, published), "原H已撤销:资格被取代")
	require.Equal(t, "-2", eval(cppLuaPublishFence, "7", "7:1000", "300", newer, published))
	require.Equal(t, before, mr.Dump(), "mark缺席时撤资格也须挡住迟到SET")
	// 已知恢复floor保N；未知floor同E无资格，新SM owner推进后才可建立新instance。
	unknown := "7||0|" + strings.Repeat("0", 32) + "|R|" + strings.Repeat("a", 32)
	mr.HSet(barrier, "handoff_fence", unknown)
	require.Equal(t, "-2", eval(cppLuaRegisterFence, "7", makeFence("7", "scene", "99", "a", "H", "b"), unknown))
	require.NoError(t, mr.Set(keys[0], "8"))
	require.Equal(t, "1", eval(cppLuaRegisterFence, "8", makeFence("8", "new-instance", "1", "a", "H", "b"), unknown))
}

func TestPendingRegistrationCancellationPreventsLateRegisterEvenWithoutMark(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const id = uint64(63992)
	barrier := fmt.Sprintf("player:{%d}:save_barrier", id)
	keys := []string{ownerepoch.OwnerEpochKey(id), barrier, ownerepoch.HandoffKey(id)}
	require.NoError(t, mr.Set(keys[0], "7"))
	held := "7|scene|1|" + strings.Repeat("a", 32) + "|H|" + strings.Repeat("b", 32)
	revoked := "7|scene|1|" + strings.Repeat("a", 32) + "|R|" + strings.Repeat("c", 32)
	v, err := sc.Redis.Eval(cppLuaCancelRegistration, keys, "7", held, "", revoked)
	require.NoError(t, err)
	require.Equal(t, "1", fmt.Sprint(v))
	before := mr.Dump()
	v, err = sc.Redis.Eval(cppLuaRegisterFence, keys, "7", held, "")
	require.NoError(t, err)
	require.Equal(t, "-2", fmt.Sprint(v))
	require.Equal(t, before, mr.Dump())
	require.False(t, mr.Exists(keys[2]))
	require.Equal(t, revoked, mr.HGet(barrier, "handoff_fence"))
}

func TestStableHandoffTransitionCandidateRecoversLostReplies(t *testing.T) {
	for _, target := range []string{"7", "9"} {
		t.Run(target, func(t *testing.T) {
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			const id = uint64(63993)
			barrier := fmt.Sprintf("player:{%d}:save_barrier", id)
			keys := []string{ownerepoch.OwnerEpochKey(id), barrier, ownerepoch.HandoffKey(id), getPlayerLocationKey(id)}
			require.NoError(t, mr.Set(keys[0], target))
			require.NoError(t, mr.Set(keys[3], "original-location"))
			held := "7|scene|1|" + strings.Repeat("a", 32) + "|H|" + strings.Repeat("b", 32)
			frozen := "7|scene|1|" + strings.Repeat("a", 32) + "|F|" + strings.Repeat("c", 32)
			revoked := "7|scene|1|" + strings.Repeat("a", 32) + "|R|" + strings.Repeat("d", 32)
			candidate := target + "|scene|1|" + strings.Repeat("a", 32) + "|H|" + strings.Repeat("e", 32)
			mr.HSet(barrier, "handoff_fence", revoked)
			for i := 0; i < 2; i++ {
				v, err := sc.Redis.Eval(cppLuaResumeFence, keys, revoked, candidate, target, "original-location")
				require.NoError(t, err)
				require.Equal(t, "1", fmt.Sprint(v))
			}
			// 已提交但首回包被调用方丢弃，再重放原candidate成功；旧H无法借新状态发布。
			// owner 未变(7)时旧 H 已被恢复出的 candidate 取代 → -2;owner 已推进到 9 时先按 owner 拒 → 0。
			wantRelease := "-2"
			if target != "7" {
				wantRelease = "0"
			}
			before := mr.Dump()
			v, err := sc.Redis.Eval(cppLuaReleaseFence, keys[:3], "7", "7:1000", "300", held, frozen)
			require.NoError(t, err)
			require.Equal(t, wantRelease, fmt.Sprint(v))
			require.Equal(t, before, mr.Dump())
		})
	}
}

func TestHandoffDamagedOwnershipAndMarkerRejectBeforeAnyMutation(t *testing.T) {
	for _, bad := range []string{"owner", "mark", "revoked-published", "other-held-published", "same-token-published"} {
		t.Run(bad, func(t *testing.T) {
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			const id = uint64(63994)
			barrier := fmt.Sprintf("player:{%d}:save_barrier", id)
			keys := []string{ownerepoch.OwnerEpochKey(id), barrier, ownerepoch.HandoffKey(id), getPlayerLocationKey(id)}
			held := "7|scene|1|" + strings.Repeat("a", 32) + "|H|" + strings.Repeat("b", 32)
			frozen := "7|scene|1|" + strings.Repeat("a", 32) + "|F|" + strings.Repeat("c", 32)
			revoked := "7|scene|1|" + strings.Repeat("a", 32) + "|R|" + strings.Repeat("d", 32)
			require.NoError(t, mr.Set(keys[0], "7"))
			mr.HSet(barrier, "handoff_fence", frozen)
			published := frozen
			switch bad {
			case "owner":
				require.NoError(t, mr.Set(keys[0], "18446744073709551616"))
			case "mark":
				require.NoError(t, mr.Set(keys[2], "bad"))
			case "revoked-published":
				published = "7|scene|1|" + strings.Repeat("a", 32) + "|R|" + strings.Repeat("e", 32)
			case "other-held-published":
				published = "7|scene|1|" + strings.Repeat("a", 32) + "|H|" + strings.Repeat("e", 32)
			case "same-token-published":
				published = "7|scene|1|" + strings.Repeat("a", 32) + "|F|" + strings.Repeat("b", 32)
			}
			mr.HSet(barrier, "handoff_fence", published)
			before := mr.Dump()
			_, err := sc.Redis.Eval(cppLuaJudgeTravelOutcome, keys, held, published, revoked, "1000")
			require.Error(t, err)
			require.Equal(t, before, mr.Dump())
			_, err = sc.Redis.Eval(cppLuaRevokeFence, keys[:3], held, published, revoked, "1000")
			require.Error(t, err)
			require.Equal(t, before, mr.Dump())
		})
	}
}

// r2-lc#1 / r2-lc#2(data-overwrite-rollback-audit-r2-20261019.md §3.6):zone Redis 存活探测摘掉无应答的旧连接后,
// 内核仍可能把旧连接上未确认的标记写重传到 Redis,晚于新连接上的取证 / 同节点重载登记才执行。这里按 Redis 实际执行
// 顺序重放(迟到的那一条最后执行),断言 scene_manager 的两个消费方 —— 换手门 checkHandoffCommitted 与 LeaveScene
// 的释放证据 markerReleasesEpoch —— 在新持有期间都拿不到当前代际的标记。拒绝只靠 save_barrier.handoff_fence 原件的
// CAS,不看墙钟、不看连接顺序。C++ 侧用两条真实连接的同序复现见
// CurrencyTest.HandoffMarkFramesDelayedOnReplacedConnectionCannotReviveCredentialAgainstRealRedis。
func TestLateMarkWritesFromReplacedConnectionNeverOpenHandoffGate(t *testing.T) {
	const epoch = uint64(5)
	makeFence := func(generation, admission, state, token string) string {
		return strings.Join([]string{"5", "scene-x", generation, strings.Repeat(admission, 32), state, strings.Repeat(token, 32)}, "|")
	}
	setup := func(t *testing.T, id uint64) (*svc.ServiceContext, *miniredis.Miniredis, []string) {
		t.Helper()
		sc, mr := newTestSvcCtxWithWorldScenes(t)
		keys := []string{ownerepoch.OwnerEpochKey(id), fmt.Sprintf("player:{%d}:save_barrier", id),
			ownerepoch.HandoffKey(id), getPlayerLocationKey(id)}
		require.NoError(t, mr.Set(keys[0], "5"))
		return sc, mr, keys
	}
	eval := func(t *testing.T, sc *svc.ServiceContext, script string, keys []string, args ...any) any {
		t.Helper()
		v, err := sc.Redis.Eval(script, keys, args...)
		require.NoError(t, err)
		return v
	}
	requireGateClosed := func(t *testing.T, sc *svc.ServiceContext, mr *miniredis.Miniredis, id uint64) {
		t.Helper()
		require.False(t, mr.Exists(ownerepoch.HandoffKey(id)), "新持有期间不得出现当前代际的标记")
		verdict, err := checkHandoffCommitted(sc, id, epoch)
		require.NoError(t, err)
		require.False(t, verdict.committed, "换手门放行 = 不存盘就铸 E+1,目标读到冻结时的父快照")
		raw, _ := sc.Redis.Get(ownerepoch.HandoffKey(id))
		require.False(t, markerReleasesEpoch(raw, epoch), "LeaveScene 不得把迟到标记当释放证据删 location")
	}

	t.Run("lc1_publish_lands_after_judge_unchanged", func(t *testing.T) {
		const id = uint64(63995)
		sc, mr, keys := setup(t, id)
		version := "00000000000000000005:00000000000000000001"
		held, published := makeFence("1", "a", "H", "b"), makeFence("1", "a", "F", "c")
		revoked, resumed := makeFence("1", "a", "R", "d"), makeFence("1", "a", "H", "e")
		mr.HSet(keys[1], "pending", version, "player_database", version, "player_database_1", version, "handoff_fence", held)
		require.NoError(t, mr.Set(keys[3], "location-bytes"))
		// ① 双 SQL ACK 已齐,BeginTravelHandoff 的 kPublishMarkLua("5:1000")还滞留在旧连接里。
		// ② 重连后看门狗在新连接上取证:标记尚未到达、owner 未变 → kUnchanged;同一脚本已把资格原子撤成 R。
		reply, ok := eval(t, sc, cppLuaJudgeTravelOutcome, keys, held, published, revoked, "1000").([]any)
		require.True(t, ok)
		require.Equal(t, "5", reply[0])
		// ③ ResumeTravelAfterRevocation:R → 新 H 确认后才解冻。
		require.Equal(t, "1", fmt.Sprint(eval(t, sc, cppLuaResumeFence, keys, revoked, resumed, "5", "location-bytes")))
		// ④ 旧连接上的发布此刻才执行:原 H 已不是现任资格,拒绝且不改任何字节。
		before := mr.Dump()
		require.Equal(t, "-2", fmt.Sprint(eval(t, sc, cppLuaPublishFence, keys[:3], "5", "5:1000", "300", held, published)))
		require.Equal(t, before, mr.Dump())
		requireGateClosed(t, sc, mr, id)
		// 解冻后的新持有者自己的下一次交接照常放行:围栏只拒旧资格,不伤活性。
		require.Equal(t, "1", fmt.Sprint(eval(t, sc, cppLuaPublishFence, keys[:3], "5", "5:2000", "300", resumed,
			makeFence("1", "a", "F", "f"))))
		verdict, err := checkHandoffCommitted(sc, id, epoch)
		require.NoError(t, err)
		require.True(t, verdict.committed)
	})

	// A1′、A2′ 放弃补写、改派、改派踢线前补写都走同一段 kReleaseMarkLua,只消费各自缓存的原 H;这里以 A1′ 为代表。
	t.Run("lc2_release_lands_after_same_node_reload", func(t *testing.T) {
		const id = uint64(63996)
		sc, mr, keys := setup(t, id)
		held, released := makeFence("1", "a", "H", "b"), makeFence("1", "a", "F", "c")
		reloaded := makeFence("2", "7", "H", "8") // 同节点短重连:同 E、同 S,SQL 确认的新 admission generation
		require.Equal(t, "1", fmt.Sprint(eval(t, sc, cppLuaRegisterFence, keys[:3], "5", held, "")))
		// ① 退出存盘落地,A1′("5:1000")滞留在旧连接里,实体随即销毁。
		// ② 玩家 30s 内短重连回同一节点:不铸新 epoch,新连接上登记 N=2(原子清继承标记)并确认。
		require.Equal(t, "1", fmt.Sprint(eval(t, sc, cppLuaRegisterFence, keys[:3], "5", reloaded, held)))
		require.Equal(t, "0", fmt.Sprint(eval(t, sc, cppLuaInheritClear, keys[:3], "5", reloaded)))
		// ③ 旧 A1′ 此刻才执行:-2 = 原 H 已被新登记轮换,未写,不改任何字节。
		before := mr.Dump()
		require.Equal(t, "-2", fmt.Sprint(eval(t, sc, cppLuaReleaseFence, keys[:3], "5", "5:1000", "300", held, released)))
		require.Equal(t, before, mr.Dump())
		require.Equal(t, reloaded, mr.HGet(keys[1], "handoff_fence"))
		requireGateClosed(t, sc, mr, id)
	})

	// 会话结束凭证(location-release-binding.md §3.3,2026-10-23):本节点完全不持有时没有缓存的原 H,scene 先 HGET 读现任
	// 原件、核对(本进程实例 / 期望 E / 仍完全不持有)之后以它 CAS(kCredentialMarkLua)。读到原件之后 CAS 帧滞留在旧连接、
	// 期间同节点短重连登记新 N:迟到的凭证必回 -2,新持有期间换手门与 LeaveScene 都拿不到证据。
	t.Run("session_ended_credential_lands_after_same_node_reload", func(t *testing.T) {
		const id = uint64(63997)
		sc, mr, keys := setup(t, id)
		held, released := makeFence("1", "a", "H", "b"), makeFence("1", "a", "F", "c")
		require.Equal(t, "1", fmt.Sprint(eval(t, sc, cppLuaRegisterFence, keys[:3], "5", held, "")))
		require.Equal(t, "1", fmt.Sprint(eval(t, sc, cppLuaReleaseFence, keys[:3], "5", "5:1000", "300", held, released)))
		mr.Del(keys[2]) // A1′ 的标记 TTL 到期,location 仍指向本节点
		// ① 会话结束释放到达,本节点完全不持有:HGET 读到原件 F,凭证 CAS 帧滞留在旧连接。
		observed := mr.HGet(keys[1], "handoff_fence")
		require.Equal(t, released, observed)
		// ② 玩家短重连回同一节点:新连接上登记 N=2 并确认。
		reloaded := makeFence("2", "7", "H", "8")
		require.Equal(t, "1", fmt.Sprint(eval(t, sc, cppLuaRegisterFence, keys[:3], "5", reloaded, observed)))
		require.Equal(t, "0", fmt.Sprint(eval(t, sc, cppLuaInheritClear, keys[:3], "5", reloaded)))
		// ③ 迟到的凭证帧此刻才执行:原件已被轮换,-2,不改任何字节。
		before := mr.Dump()
		require.Equal(t, "-2", fmt.Sprint(eval(t, sc, cppLuaCredentialFence, keys[:3], "5", "5:2000", "300",
			observed, makeFence("1", "a", "F", "d"))))
		require.Equal(t, before, mr.Dump())
		require.Equal(t, reloaded, mr.HGet(keys[1], "handoff_fence"))
		requireGateClosed(t, sc, mr, id)
	})

	// 反面:持有节点活着而不持有(A1′ 写过、标记已过期),拿不到凭证时 location 永远删不掉(§15 起等待超时不授权删除)。
	// 凭证以读到的本节点原件 F 写出后,换手门放行、LeaveScene 拿到 E 代释放证据。
	t.Run("session_ended_credential_releases_idle_holder", func(t *testing.T) {
		const id = uint64(63998)
		sc, mr, keys := setup(t, id)
		held, released := makeFence("1", "a", "H", "b"), makeFence("1", "a", "F", "c")
		require.Equal(t, "1", fmt.Sprint(eval(t, sc, cppLuaRegisterFence, keys[:3], "5", held, "")))
		require.Equal(t, "1", fmt.Sprint(eval(t, sc, cppLuaReleaseFence, keys[:3], "5", "5:1000", "300", held, released)))
		mr.Del(keys[2])
		requireGateClosed(t, sc, mr, id)
		observed := mr.HGet(keys[1], "handoff_fence")
		require.Equal(t, "1", fmt.Sprint(eval(t, sc, cppLuaCredentialFence, keys[:3], "5", "5:2000", "300",
			observed, makeFence("1", "a", "F", "d"))))
		verdict, err := checkHandoffCommitted(sc, id, epoch)
		require.NoError(t, err)
		require.True(t, verdict.committed)
		raw, _ := sc.Redis.Get(ownerepoch.HandoffKey(id))
		require.True(t, markerReleasesEpoch(raw, epoch))
	})
}
