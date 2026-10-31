package logic

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"

	"proto/scene_manager"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/handofffence"
	"shared/ownerepoch"
)

// 无资格持有者的带围栏撤销(docs/design/location-release-binding.md §17)。
//
// r2-lc#2 之后,会话结束凭证只能由持有节点 X 以「本进程实例在 E 下登记的资格原件」做 CAS 写出(§3.3)。location 记着 (X, E),
// X 却从未在 E 下登记成功(入场登记在写 handoff_fence 之前被拒、交接回滚之后的采纳没做成)时,X 拿不出资格,判
// no_qualification;§15 起等待超时不再授权删 location,X 活着又不再被派到这名玩家时,location 永久卡住、玩家永久进不了场。
//
// 收口按租约的标准做法:由发放租约的权威方回收。owner_epoch 是围栏令牌、scene_manager 是它的铸造者,所以由 scene_manager 先推进
// 令牌(INCR owner_epoch)、再交出位置(删 location);旧持有者之后任何带 E 的写都被存储侧的 owner CAS 拒绝。不需要 X 配合,
// 也不依赖 X 的应答、连接顺序或墙钟。
//
// 正证(§17.3):玩家实体只在 E 下的入场登记(kRegisterFenceLua 写出 E 下的 H)或交接采纳(kResumeFenceLua 写出 E 下的 H)成功之后
// 才建出或继续持有(不变量 I-1);owner_epoch 为 E 期间,一切资格写只能写出 E 下的原件。所以「资格原件缺失,或其 E ≠ location 的 E,
// 且没有 E 代标记」与 location、owner_epoch 在同一次原子检查里成立,就证明此刻没有任何进程以 E 持有该玩家,也就没有 E 下未落盘的进度。
// 前提是共享核心 Redis 不淘汰键(noeviction,handoff hash 不设 TTL):原件「缺失」只能是从来没人登记过。
//
// 宽限(§17.4 第 4 条)只决定什么时候去检查正证,不参与安全性:在途的入场登记 / 交接采纳若先落地,资格原件变了,撤销 Lua 回
// fence_changed;若撤销先落地,它们的 Lua 比 owner 失败,那次入场失败、客户端重登走新铸造。两种先后都不会出现两个持有者。
//
// 调用点只有两个,都在已经请过持有节点释放的流程里(§17.4 第 4 条):LeaveScene 判 pending 时(revokeUnqualifiedHolderOnLeave),
// EnterScene 换手门以 18 拒绝、持有者释放门判 eligible 之后(revokeUnqualifiedHolderOnLogin)。宽限从这份 location 的等待计时起点算
// (leave_release.go noteLeaveReleaseWait,Redis TIME,两个调用点共用一把键)。

// unqualifiedRevokeMetricVerdict 是撤销成功时记的指标取值:LeaveScene 一侧记在 scene_manager_leave_holder_release_total,
// EnterScene 一侧记在 scene_manager_enter_holder_release_total。只在撤销 Lua 真正推进围栏、删掉 location 时记一次。
const unqualifiedRevokeMetricVerdict = "revoked_unqualified"

// unqualifiedRevokeVerdict 是发撤销 Lua 之前的预判(Go 侧,只读),决定要不要发。名字只进日志。
// 底层类型 uint8 数的是结论种类数(现 5 种)。
type unqualifiedRevokeVerdict uint8

const (
	// unqualifiedRevokeNotDue:从这份 location 的等待计时起点算,还没过宽限。
	unqualifiedRevokeNotDue unqualifiedRevokeVerdict = iota
	// unqualifiedRevokeEpochUnproven:E 为 0,或 location 记的 E 与 owner_epoch 键不一致 —— 不是本机制认识的形态,不动。
	unqualifiedRevokeEpochUnproven
	// unqualifiedRevokeFenceMalformed:资格原件写坏了,看不出它属于哪一代,不当证据(fail-closed)。
	unqualifiedRevokeFenceMalformed
	// unqualifiedRevokeQualified:资格原件属于 E —— 有进程在 E 下登记过。正常持有、已释放而标记过期、登记被撤销都在这里,
	// 交给会话结束凭证(§3.3)与 GO-3(owner_replaced.go),本机制不碰。
	unqualifiedRevokeQualified
	// unqualifiedRevokeEligible:原件缺失或属于别的 E,发撤销 Lua 原子复核。
	unqualifiedRevokeEligible

	unqualifiedRevokeVerdictCount
)

var unqualifiedRevokeVerdictNames = [...]string{
	unqualifiedRevokeNotDue:         "not_due",
	unqualifiedRevokeEpochUnproven:  "epoch_unproven",
	unqualifiedRevokeFenceMalformed: "fence_malformed",
	unqualifiedRevokeQualified:      "qualified",
	unqualifiedRevokeEligible:       "eligible",
}

// 名表长度与 unqualifiedRevokeVerdictCount 不一致时编译失败(漏加 / 多加一行)。
var _ [unqualifiedRevokeVerdictCount]struct{} = [len(unqualifiedRevokeVerdictNames)]struct{}{}

func (v unqualifiedRevokeVerdict) String() string {
	if v < unqualifiedRevokeVerdictCount {
		return unqualifiedRevokeVerdictNames[v]
	}
	return "unknown"
}

// unqualifiedRevokeFacts 是 decideUnqualifiedRevoke 的输入。零值取「不撤销」的一侧:漏填只会少撤一次,location 照旧等释放证据。
type unqualifiedRevokeFacts struct {
	graceElapsed  bool   // 等待计时已过宽限
	epoch         uint64 // 期望 E:本次请求观察到的 owner_epoch(撤销 Lua 再原子复核键值)
	locationEpoch uint64 // location 记的 owner_epoch
	fencePresent  bool   // handoff hash 里有 handoff_fence
	fenceValid    bool   // 原件可解析(handofffence.ParseHandoffFence,与 C++ ParseFence 同口径)
	fenceEpoch    uint64 // 原件的 E(fenceValid 时才有意义)
}

// decideUnqualifiedRevoke 是 §17.4 的预判表,按顺序判,零副作用。前两行不看资格原件:调用方可以先只用它们判一次,
// 判出 not_due / epoch_unproven 时不必去读原件。撤销 Lua 会把第 2 行之后的条件全部再原子复核一遍。
func decideUnqualifiedRevoke(f unqualifiedRevokeFacts) unqualifiedRevokeVerdict {
	switch {
	case !f.graceElapsed:
		return unqualifiedRevokeNotDue
	case f.epoch == 0 || f.locationEpoch != f.epoch:
		return unqualifiedRevokeEpochUnproven
	case !f.fencePresent:
		return unqualifiedRevokeEligible
	case !f.fenceValid:
		return unqualifiedRevokeFenceMalformed
	case f.fenceEpoch == f.epoch:
		return unqualifiedRevokeQualified
	default:
		return unqualifiedRevokeEligible
	}
}

// 撤销 Lua 的结果名(只进日志与单测,不进指标 label)。
const (
	unqualifiedRevokeOutcomeRevoked       = "revoked"         // 1:围栏推进一格,location 已删、人数已减
	unqualifiedRevokeOutcomeAlreadyGone   = "already_gone"    // 0:location 已不在(别的副本 / 别的调用已处理,或 go-redis 原样重发)
	unqualifiedRevokeOutcomeSuperseded    = "superseded"      // -1:location 原字节或 §16 修订(侧车 / floor)已变
	unqualifiedRevokeOutcomeEpochMoved    = "epoch_moved"     // -2:owner_epoch 已不是 E(缺键同:不能 INCR 出 1)
	unqualifiedRevokeOutcomeFenceChanged  = "fence_changed"   // -3:资格原件与观察时不同(在途的登记 / 采纳先落地)
	unqualifiedRevokeOutcomeFenceNotProof = "fence_not_proof" // -4:原件属于 E 或写坏了(按 C++ 同口径解析)
	unqualifiedRevokeOutcomeMarkerPresent = "marker_present"  // -5:有 E 代或写坏的标记,走释放证据路径
)

// luaCppHandoffFenceHelpers 是 C++ handoff_sql_barrier.h kFenceLuaHelpers 的逐字副本(修改须两边同步;
// TestUnqualifiedRevokeFenceParsingMatchesCpp 逐字比对)。撤销 Lua 只用其中的 uint() 与 fence():「资格原件属于哪一代、
// 写没写坏」必须与 C++ 登记 / 释放 / 凭证脚本同一口径;Go 侧的 handofffence.ParseHandoffFence 只用于预判与日志。
// 其余几个函数(types3 / prior 等)按 C++ 的键序写死,这里不调用。
const luaCppHandoffFenceHelpers = `
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

// luaRevokeUnqualifiedHolder:原子复核「没有任何进程以 E 持有该玩家」之后推进围栏并删 location(§17.5)。
//
//	KEYS[1] = player:{id}:location
//	KEYS[2] = player:{id}:owner_epoch
//	KEYS[3] = player:{id}:handoff_fence(只读 handoff_fence 字段)
//	KEYS[4] = player:{id}:handoff(已落盘标记)
//	KEYS[5] = player:{id}:leave_release_wait(撤销成功时一并删掉)
//	KEYS[6] = player:{id}:location_revision(§16 侧车,撤销成功时一并删掉)
//	KEYS[7] = player:{id}:location_revision_floor(只比不删:floor 不随位置删除)
//	ARGV[1] = 调用方读到、并据此判定的 location 原字节
//	ARGV[2] = 期望 E(规范十进制,非 0)
//	ARGV[3] = 调用方读到的 handoff_fence 原件;空串 = 读时不存在
//	ARGV[4] = 调用方随 location 一起读到的 location_revision 原文;空串 = 读时不存在
//	ARGV[5] = 期望的 location_revision_floor(本次请求已观察的 SQL 原修订);空串 = 不核
//
// 返回 1 = 已撤销;0 = location 已不在;-1 = location 原字节 / 修订已变;-2 = owner_epoch 已不是 E;-3 = 资格原件与观察时不同;
// -4 = 原件属于 E 或写坏了;-5 = 有 E 代(或写坏的)标记。不是 1 时一个字节都没写。参数形状不对回 error_reply(Go 与 Lua 的
// 约定被改岔,不猜一个值去 INCR)。
//
//   - 为什么先 INCR 再删:INCR 是第一条写,它报错(值不是整数)时整段一个字节都没写;Redis 脚本出错不回滚已执行的写。
//   - 为什么 INCR 而不是按墙钟地板(G3a)取值:撤销不把新值交给任何节点,只为让 E 下的写全部失效;下一次落点照常走铸造,
//     由墙钟地板与 WAITAOF 兜 zone Redis 回退(zone-redis-regression-guard-20261022.md §4.3)。WAITAOF 等的是复制偏移,
//     铸造确认落盘时,排在它之前的撤销一定也已落盘。
//   - 标记的代际去掉前导零后按串比,与 luaDeletePlayerLocationIfUnchanged 的释放模式同法;owner_epoch 按串严格相等。
//   - 删 location、等待计时、侧车在同一次提交里;摘场景成员是第二步,在控制域上(runUnqualifiedRevoke),幂等。go-redis
//     读超时后原样重发时,重发看到 location 已不在回 0,不会再推进。
const luaRevokeUnqualifiedHolder = luaCppHandoffFenceHelpers + `
if #KEYS ~= 7 or #ARGV ~= 5 or not uint(ARGV[2]) then
    return redis.error_reply('UNQUALIFIED_REVOKE shape')
end
local cur = redis.call('GET', KEYS[1])
if cur == false then
    return 0
end
if cur ~= ARGV[1] or (redis.call('GET', KEYS[6]) or '') ~= ARGV[4] then
    return -1
end
if ARGV[5] ~= '' and redis.call('GET', KEYS[7]) ~= ARGV[5] then
    return -1
end
if redis.call('GET', KEYS[2]) ~= ARGV[2] then
    return -2
end
local held = redis.call('HGET', KEYS[3], 'handoff_fence') or ''
if held ~= ARGV[3] then
    return -3
end
if held ~= '' then
    local parsed = fence(held)
    if not parsed or parsed.e == ARGV[2] then
        return -4
    end
end
local mark = redis.call('GET', KEYS[4])
if mark then
    local e = string.match(mark, '^(%d+):%d+$')
    if e == nil or (string.gsub(e, '^0+(%d)', '%1')) == ARGV[2] then
        return -5
    end
end
redis.call('INCR', KEYS[2])
redis.call('DEL', KEYS[1], KEYS[5], KEYS[6])
return 1
`

// unqualifiedRevokeTarget 是一次撤销要原子复核的观察值,全部来自调用方本次请求的同一轮读取。
type unqualifiedRevokeTarget struct {
	playerID      uint64
	loc           *scene_manager.PlayerLocation // 解码后的 location(非 nil)
	locationRaw   string                        // location 原字节
	revisionTag   string                        // §16 的 location_revision 侧车原文(随 location 一起读到的)
	ownerRevision uint64                        // 本次请求已观察到的 SQL 原修订(floor 期望);0 = 不核
	zoneID        uint32                        // location 所在 zone,只用于找节点成员集合;0 按 scene:{id}:zone 补
	epoch         uint64                        // 本次请求观察到的 owner_epoch(期望 E)
	waited        time.Duration                 // 这份 location 的等待计时(Redis TIME)
}

// unqualifiedRevokeResult 是一次检查的结果。outcome 只在发了撤销 Lua 时非空;== revoked 才删掉了 location。
type unqualifiedRevokeResult struct {
	verdict      unqualifiedRevokeVerdict
	outcome      string
	grace        time.Duration
	fencePresent bool
	fenceValid   bool
	fence        handofffence.HandoffFence // fenceValid 时是读到的资格原件,只进日志(不打印原文)
}

// revokeUnqualifiedHolder 判定并在合格时执行撤销。只在调用方已请过持有节点释放之后调用(§17.4 第 4 条)。
// err 只表示 Redis 读写失败(或 Lua 报参数错),调用方一律按没撤销处理。
func revokeUnqualifiedHolder(svcCtx *svc.ServiceContext, target unqualifiedRevokeTarget) (unqualifiedRevokeResult, error) {
	result := unqualifiedRevokeResult{grace: svcCtx.Config.EffectiveUnqualifiedHolderRevokeGrace()}
	facts := unqualifiedRevokeFacts{
		graceElapsed:  target.waited >= result.grace,
		epoch:         target.epoch,
		locationEpoch: target.loc.GetOwnerEpoch(),
	}
	// 预判表的前两行不看资格原件:宽限未过、代际对不上时不读,重试期间不给 Redis 多加一次读。
	if result.verdict = decideUnqualifiedRevoke(facts); result.verdict != unqualifiedRevokeEligible {
		return result, nil
	}
	raw, present, err := readHandoffFenceRaw(svcCtx, target.playerID)
	if err != nil {
		return result, err
	}
	facts.fencePresent, result.fencePresent = present, present
	if present {
		if fence, parseErr := handofffence.ParseHandoffFence(raw); parseErr == nil {
			facts.fenceValid, facts.fenceEpoch = true, fence.OwnerEpoch
			result.fenceValid, result.fence = true, fence
		}
	}
	if result.verdict = decideUnqualifiedRevoke(facts); result.verdict != unqualifiedRevokeEligible {
		return result, nil
	}
	result.outcome, err = runUnqualifiedRevoke(svcCtx, target, raw)
	return result, err
}

// readHandoffFenceRaw 读玩家屏障 hash 里交接资格的原文。没有该字段返回 present=false、err=nil。
// 与 owner_replaced.go 的 readHandoffFence 读同一个字段;这里要原文,撤销 Lua 按原字节复核。
func readHandoffFenceRaw(svcCtx *svc.ServiceContext, playerID uint64) (raw string, present bool, err error) {
	raw, err = playerRedisHget(svcCtx, handofffence.Key(playerID), handofffence.HandoffFenceField)
	if errors.Is(err, goredis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读交接资格失败: player=%d: %w", playerID, err)
	}
	return raw, true, nil
}

// unqualifiedRevokeKeys 按 luaRevokeUnqualifiedHolder 的键序组键,全部同 {id} slot。
func unqualifiedRevokeKeys(playerID uint64) []string {
	return []string{
		getPlayerLocationKey(playerID),
		ownerepoch.OwnerEpochKey(playerID),
		handofffence.Key(playerID),
		ownerepoch.HandoffKey(playerID),
		leaveReleaseWaitKey(playerID),
		locationRevisionKey(playerID),
		locationRevisionFloorKey(playerID),
	}
}

// runUnqualifiedRevoke 执行撤销 Lua,把返回码翻成结果名。observedFence 是调用方读到的资格原件("" = 不存在)。
//
// location 已删(本次撤销,或 go-redis 重发时首发已撤销)后摘场景成员。摘成员失败时同时返回结果名与错误:
// location 已经没了,但调用方要按「没撤销」走重试,下一次 LeaveScene 看到 location 不在会再摘一次。
func runUnqualifiedRevoke(svcCtx *svc.ServiceContext, target unqualifiedRevokeTarget, observedFence string) (string, error) {
	result, err := playerRedisEval(svcCtx, luaRevokeUnqualifiedHolder, unqualifiedRevokeKeys(target.playerID), target.locationRaw,
		strconv.FormatUint(target.epoch, 10), observedFence, target.revisionTag, optionalRevisionText(target.ownerRevision))
	if err != nil {
		return "", fmt.Errorf("撤销无资格持有者失败: player=%d owner_epoch=%d: %w", target.playerID, target.epoch, err)
	}
	removeMember := func(outcome string) (string, error) {
		if err := removeScenePlayer(svcCtx, target.playerID, target.loc.GetSceneId(), target.zoneID); err != nil {
			return outcome, fmt.Errorf("撤销后摘场景成员失败: player=%d scene=%d: %w", target.playerID, target.loc.GetSceneId(), err)
		}
		return outcome, nil
	}
	switch fmt.Sprint(result) {
	case "1":
		return removeMember(unqualifiedRevokeOutcomeRevoked)
	case "0":
		return removeMember(unqualifiedRevokeOutcomeAlreadyGone)
	case "-1":
		return unqualifiedRevokeOutcomeSuperseded, nil
	case "-2":
		return unqualifiedRevokeOutcomeEpochMoved, nil
	case "-3":
		return unqualifiedRevokeOutcomeFenceChanged, nil
	case "-4":
		return unqualifiedRevokeOutcomeFenceNotProof, nil
	case "-5":
		return unqualifiedRevokeOutcomeMarkerPresent, nil
	}
	return "", fmt.Errorf("撤销无资格持有者返回值非法: player=%d owner_epoch=%d result=%v", target.playerID, target.epoch, result)
}

// logUnqualifiedRevoke 给一次检查打一行。宽限未过不打(LeaveScene 的 [LeaveRelease]、EnterScene 的 [EnterHolderRelease] 已各有一行);
// 撤销成功、原件写坏、Redis 出错打 ERROR(go-zero logx 没有 WARN 级;撤销说明持有节点在 E 下的入场登记没做成、location 卡过了宽限,
// 值得逐条看)。只打原件的 E / 状态,不打原文。site 是 leave / enter。
func logUnqualifiedRevoke(logger logx.Logger, site string, target unqualifiedRevokeTarget, result unqualifiedRevokeResult, err error) {
	if err == nil && result.verdict == unqualifiedRevokeNotDue {
		return
	}
	fenceEpoch, fenceState := "none", "none"
	if result.fenceValid {
		fenceEpoch, fenceState = strconv.FormatUint(result.fence.OwnerEpoch, 10), string(rune(result.fence.State))
	} else if result.fencePresent {
		fenceEpoch, fenceState = "malformed", "malformed"
	}
	args := []any{target.playerID, site, target.zoneID, target.loc.GetNodeId(), target.loc.GetSceneId(), target.epoch,
		target.loc.GetOwnerEpoch(), fenceEpoch, fenceState, target.waited.Truncate(time.Second), result.grace, result.verdict, result.outcome}
	switch {
	case err != nil:
		logger.Errorf("[UnqualifiedRevoke] check failed; keeping the location: "+unqualifiedRevokeLogFields+" err=%v", append(args, err)...)
	case result.outcome == unqualifiedRevokeOutcomeRevoked:
		logger.Errorf("[UnqualifiedRevoke] holder never qualified for the current owner_epoch; fenced it off (owner_epoch +1) and deleted the location: "+
			unqualifiedRevokeLogFields, args...)
	case result.verdict == unqualifiedRevokeFenceMalformed:
		logger.Errorf("[UnqualifiedRevoke] handoff fence is malformed; not revoking: "+unqualifiedRevokeLogFields, args...)
	default:
		logger.Infof("[UnqualifiedRevoke] not revoking: "+unqualifiedRevokeLogFields, args...)
	}
}

// unqualifiedRevokeLogFields 是 logUnqualifiedRevoke 各行共用的字段段(13 个参数,顺序见那里的 args)。
const unqualifiedRevokeLogFields = "player=%d site=%s holder_zone=%d holder_node=%s scene=%d owner_epoch=%d location_epoch=%d " +
	"fence_epoch=%s fence_state=%s waited=%s grace=%s verdict=%s outcome=%s"

// revokeUnqualifiedHolderOnLeave 是 LeaveScene 一侧的调用点:只在判 pending(离场身份判 deleted、本次请求在请持有节点释放)时检查。
// 撤销成功返回 true:location 已删,这次清理完成,不再请释放。其余一律返回 false,调用方照常保留 location、请持有节点释放、
// 回 FailedPrecondition。legacy 离场身份(pending_unproven)不请释放,也不撤销。
func (l *LeaveSceneLogic) revokeUnqualifiedHolderOnLeave(in *scene_manager.LeaveSceneRequest, loc *scene_manager.PlayerLocation,
	raw, tag string, ownerRevision uint64, j holderReleaseJudgement) bool {
	if j.verdict != leaveReleasePending {
		return false
	}
	target := unqualifiedRevokeTarget{playerID: in.PlayerId, loc: loc, locationRaw: raw, revisionTag: tag,
		ownerRevision: ownerRevision, zoneID: j.zoneID, epoch: j.epoch, waited: j.waited}
	result, err := revokeUnqualifiedHolder(l.svcCtx, target)
	logUnqualifiedRevoke(l.Logger, "leave", target, result, err)
	if err != nil || result.outcome != unqualifiedRevokeOutcomeRevoked {
		return false
	}
	metrics.ObserveLeaveHolderRelease(unqualifiedRevokeMetricVerdict)
	return true
}

// revokeUnqualifiedHolderOnLogin 是 EnterScene 持有者释放门一侧的调用点:换手门以 18 拒绝、requestHolderReleaseForLogin 判 eligible
// 并已同步请过持有节点之后调用(只替最新的登录请求;scene 自发的请求不到这里)。先登记 / 读出等待计时(与 LeaveScene 共用一把键,
// 同一份 location 同一个起点),过了宽限再检查。撤销与否都照回 18:撤销之后的重登看到没有 location,按无持有者落点铸造。
func (l *EnterSceneLogic) revokeUnqualifiedHolderOnLogin(in *scene_manager.EnterSceneRequest, loc *scene_manager.PlayerLocation,
	raw, tag string, zoneID uint32, observedEpoch uint64) {
	budget, _ := l.svcCtx.Config.EffectiveLeaveSceneHolderReleaseWait()
	target := unqualifiedRevokeTarget{playerID: in.PlayerId, loc: loc, locationRaw: raw, revisionTag: tag,
		ownerRevision: in.OwnerRevision, zoneID: zoneID, epoch: observedEpoch,
		waited: noteLeaveReleaseWait(l.svcCtx, in.PlayerId, leaveReleaseWaitScope(observedEpoch, loc), budget)}
	result, err := revokeUnqualifiedHolder(l.svcCtx, target)
	logUnqualifiedRevoke(l.Logger, "enter", target, result, err)
	if err == nil && result.outcome == unqualifiedRevokeOutcomeRevoked {
		metrics.ObserveEnterHolderRelease(unqualifiedRevokeMetricVerdict)
	}
}
