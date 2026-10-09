package logic

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	smpb "proto/scene_manager"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/ownerepoch"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// 玩家归属 epoch(owner_epoch)与交接落盘标记(handoff)—— scene_manager 侧
//
// 两把键的格式契约在 shared/ownerepoch(Go 与 C++ 两边一字不差)。本文件只放
// scene_manager 自己的读写:
//
//  1. 铸造 + CAS 写 location 的 Lua(placePlayerLocation 用);
//  2. 路由 / 重定向投递失败后的**单调**回滚(GO-2 根治,cross-zone-scene-travel.md §12.8):
//     location 退回本次写入之前的值,铸造过的 epoch 再 INCR 一格(N+1 → N+2),从不退回旧值;
//     凭标记铸造的回滚以「所凭标记原样还在」为前提,同时在恢复出的 location 里写回滚回执、把
//     标记转写到新 epoch。例外 keep:没铸造的落点,以及同物理节点 epoch 0 的铸造(§12.6.9(c)),
//     只退 location、epoch 键不动;
//  3. 换手门第二道:handoff 标记比对(cross-zone-scene-travel.md CZ-4)。
//
// 不变量(scene-owner-reentry-barrier.md §3.3):epoch 只在 EnterScene 这一个
// 闸口推进,而且**只在持有者真的换了的时候**推进:首次落点、过了换手门的跨节点
// 交接、放行跨 zone 交接、等待落点的第二条腿。新值随路由事件带给目标节点;C++
// 存盘用 Lua 比对「缓存值 == Redis 当前值」,老 epoch 的写一律被拒。
// epoch 只经 INCR 前进 —— 路由失败的回滚也是 INCR,任何值都不会被铸两次(补种只按 location
// 记录值补回缺失的键,见 enterscenelogic.go sameNodeZeroMint 一段)。除下面「唯一例外」里的同节点 epoch 0
// 铸造外,bump 回滚是另一种「持有者没换也推进」的情形:它让被回滚掉的那个值作废,仍持有玩家的源 scene
// 凭 location 里的回滚回执采纳新值。
//
// 持有者没换就不许推进 —— 同节点换图、同落点重连、dev 旁路下无标记的跨节点
// 交接都只做「epoch 不变才写 location」的 CAS。原因:持有节点要等 Kafka → gate →
// PlayerEnterGameNode 才知道新值,这段窗口(几十 ms 到秒级)里它任何一次周期 /
// 退出存盘都会被 C++ 的 CAS 拒掉,继而把**合法持有者**当成被废黜、不存盘销毁
// 实体(踢人 + 回档)。Go 这边写 location 也走同一把 epoch 的 CAS,两条通道不
// 各写各的。
//
// 唯一例外(2026-09-21,cross-zone-scene-travel.md §12.6.9):同物理节点、owner_epoch 键读到 0
// **且** location 记的 epoch 也是 0(存量玩家,从未铸造过)时也铸造。上面「窗口内存盘被拒」的前提是
// 持有节点缓存着非 0 值;缓存为 0 时 C++ 走不带 guard 的旧 Save,新值拒不了它。不铸的话这类玩家
// 一直停在 0,帮会 B4c 的资产 RPC 对 epoch==0 一律回 RETRY。键读到 0 而 location 记着 N≠0(键被
// 单独淘汰)时不铸造,先按 N 补种(SET NX)。推导与残余见 enterscenelogic.go 的 sameNodeZeroMint。
// ---------------------------------------------------------------------------

// luaMintEpochAndSetLocation:「当前 epoch == ARGV[1] 才 INCR 并写 location」。
//
//	KEYS[1] = player:{id}:owner_epoch
//	KEYS[2] = player:{id}:location
//	KEYS[3] = player:{id}:handoff
//	ARGV[1] = 调用方读到的当前 epoch(十进制;键不存在按 "0")
//	ARGV[2] = 已带上 epoch+1 的 PlayerLocation 字节
//	ARGV[3] = 本次放行所凭的 handoff 标记原文;空串 = 本次不凭标记(首次落点 / 等待落点)
//
// 返回 INCR 之后的新 epoch;epoch CAS 不过返回 0;标记已不是预检时那一份返回 -1。
// 两种失败都什么都不改。键不存在时 GET 给 false,与 "0" 归一后比对(首次铸造)。
// 回滚只 INCR、从不把键退回旧值(见 luaRollbackPlayerPlacement),所以不存在「回滚之后
// 再铸出同一个值」。
//
// 重放识别(epoch CAS 不过、`return 0` 之前那一段):go-zero 给 go-redis 设了 MaxRetries=3,
// go-redis 对读超时 / EOF 会把**同一条** EVAL 原样重发。首发其实已经执行(epoch 已是
// ARGV[1]+1、location 已写成 ARGV[2])、只是应答丢了时,重发在这里读到 cur ≠ ARGV[1],
// 原本会回 0 被当成「并发推进」(errOwnerEpochConflict → 19):状态已经推进,调用方却既不发
// 重定向 / 路由、也不回滚 —— 与「推重定向失败 + 回滚失败」同一个终态(源端静默销毁实体、
// 客户端挂在哑连接上),但只需要一次 Redis 读超时。所以在回 0 之前再认一次「本请求已生效」:
//
//	ARGV[3] 非空(本请求凭标记放行)且 cur == ARGV[1]+1 且 location == ARGV[2] 且标记仍 == ARGV[3]
//
// 满足就返回 **-cur**(cur == 本请求本该铸出的新 epoch;取负只为让调用方区分「重放识别命中」与
// 「首发成功」以便计数,decodeMintReply 还原后按铸造成功继续:扣旧场景人数、发重定向 / 路由)。
// 负值不会与 -1(标记已撤回)/ 0(epoch 冲突)撞:凭标记放行时观察到的 epoch ≥ 1(epoch 为 0 的
// 标记从不写出),新 epoch ≥ 2,取负 ≤ -2;万一真有 epoch 0 的标记,-1 会被读成「已撤回」,
// 是拒绝方向,不会误放行。三条合起来能证明是本请求自己的写入:epoch 恰好只前进一格;location 原文带
// UpdateTime 与完整目标;标记仍是本请求所凭的那一份(源端的原子取证脚本删掉本族标记之后不再认,与上面
// 「标记检查与铸造同一原子步骤」的承诺一致 —— 源端删标记后读 epoch / location 做裁决,此时识别与否都不
// 改变它的结论,只影响本请求回 0 还是回成功)。本请求的落点已被 bump 回滚之后(epoch 已是
// ARGV[1]+2),原样重发同样回 0:「恰好只前进一格」不成立,不会把回滚掉的落点重新认成已生效。
//
// 不带标记的铸造(首次落点 / 等待落点)**不做**识别,维持回 0:UpdateTime 是秒级,同一秒内
// 目标相同的两个并发请求序列化字节完全一样,没有标记就区分不开「我已生效」与「别人抢先写了
// 同样的值」。残余:凭**同一份**标记、同一秒、同一目标的两个并发请求(只有上游重复提交才会
// 出现)两个都会被判成功 —— 归属终态正是两者都想要的那一个,代价是旧场景人数多扣一次、
// 重定向 / 路由多发一条(客户端与 gate 对重复 124 去重)。
// tonumber 比较:epoch 远小于 2^53,双精度下精确;cur 被写坏(非数字)时 tonumber 得 nil,
// 比较为假,照旧回 0。
//
// 标记为什么要在这段 Lua 里再比一次:源 scene 等应答超时(或收到失败应答)后要决定
// 「解冻继续玩」还是「我已被放行、销毁实体」。它的做法是在一段原子脚本里先删掉本次交接这一族
// 标记(按 saved_at_ms 认,含回滚转写出来的那份)、再读 owner_epoch 与 location(§12.8)——
// 只要标记检查与铸造是同一个原子步骤,删除之后就不可能再有凭这份标记的放行,于是「epoch 没变」
// 就严格等价于「没被放行」;被回滚时 epoch 变成 N+2,另由 location 里的回滚回执证明(见
// luaRollbackPlayerPlacement)。若只在预检里读标记,一个卡了很久的 EnterScene 可以在源端解冻
// 之后才铸造,同一名玩家在两个 zone 同时活着。
const luaMintEpochAndSetLocation = `
local cur = redis.call("GET", KEYS[1])
if cur == false then
    cur = "0"
end
if cur ~= ARGV[1] then
    if ARGV[3] ~= "" and tonumber(cur) == tonumber(ARGV[1]) + 1
        and redis.call("GET", KEYS[2]) == ARGV[2]
        and redis.call("GET", KEYS[3]) == ARGV[3] then
        return -tonumber(cur)
    end
    return 0
end
if ARGV[3] ~= "" and redis.call("GET", KEYS[3]) ~= ARGV[3] then
    return -1
end
local minted = redis.call("INCR", KEYS[1])
redis.call("SET", KEYS[2], ARGV[2])
return minted
`

// luaSetLocationIfEpoch:「当前 epoch == ARGV[1] 才写 location」,**不铸造**。
// 持有者没换的落点(同节点换图、dev 旁路下无标记的跨节点交接)用它:location 里
// 记的 epoch 就是 ARGV[1] 本身。KEYS / ARGV 布局与 luaMintEpochAndSetLocation 相同。
//
// 返回 1 = 已写;0 = epoch 被并发请求推进过,什么都没改。不能像铸造版那样返回
// epoch 本身:期望值为 0(旧版从未铸造)时成功与冲突就分不开了。
const luaSetLocationIfEpoch = `
local cur = redis.call("GET", KEYS[1])
if cur == false then
    cur = "0"
end
if cur ~= ARGV[1] then
    return 0
end
redis.call("SET", KEYS[2], ARGV[2])
return 1
`

// luaRollbackPlayerPlacement:推路由 / 重定向失败后撤掉本次落点(GO-2 根治,cross-zone-scene-travel.md
// §12.8)。location 退回本次写入之前的值;owner_epoch **只进不退**。
//
//	KEYS[1] = player:{id}:location
//	KEYS[2] = player:{id}:owner_epoch
//	KEYS[3] = player:{id}:handoff
//	ARGV[1] = 本次写入的 location 精确字节(当前值必须仍是它)
//	ARGV[2] = 回滚后的 location 字节;空串 = 本次之前没有 location,DEL
//	ARGV[3] = 本次落点写进 location 的 epoch(当前值必须仍是它;键不存在按 "0")
//	ARGV[4] = "bump":本次铸造过,回滚再 INCR 一格(N+1 → N+2);
//	          "keep":本次没铸造,或同物理节点 epoch 0 的铸造,epoch 键不动
//	ARGV[5] = 本次铸造所凭的 handoff 标记原文;空串 = 不凭标记
//	ARGV[6] = 转写后的标记 "{N+2}:{原标记冒号后的原文}";空串 = 不转写(不凭标记 / keep)
//	ARGV[7] = 转写标记的 TTL 秒数(ownerepoch.HandoffTTL)
//
// ARGV[2] 由调用方预先算好(planRouteRollback):bump 时恢复出的 location 已带 owner_epoch = N+2 与
// rollback_receipt = ARGV[5];keep 时是旧 location 的原字节。
//
// 返回值:
//
//	-after(≤ -2) bump 已回滚,after 是 INCR 的结果(必等于 ARGV[3]+1,Go 侧仍交叉校验);
//	1            keep 已回滚;
//	2            bump 已经回滚过(go-redis 重发,见下「重放识别」);
//	3            凭标记铸造,但所凭标记已不是那一份:**一个字节都不动**,保留本次落点;
//	0            location 或 epoch 已被并发请求推进,什么都没改(否则会覆盖后来者刚提交的归属)。
//
// 为什么 epoch 不退回旧值(旧写法把键 SET 回 N,两条已确认伤害):
//   - 伤害 1:「kafka-go 报错但 broker 其实已投递」时,目标节点 B 拿着 N+1 载入、可能已落盘并发出
//     DBTask(N+1);db 的 epoch 守卫只升不降,回滚后仍持有玩家的源端以 N 发出的 DBTask 被永久判 stale;
//   - 伤害 2:下一次铸造又铸出 N+1,同一个值被铸两次 —— 幽灵 B 手里的 N+1 与下一任持有者的 N+1 一样,
//     存盘 CAS 与 db 守卫都分不开两者。
//
// 单调回滚之后,回滚后的任何合法持有者(采纳了 N+2 的源端,或之后任何新落点)手里的 epoch 都大于任何被
// 回滚掉的值。代价:源端手里的 N 也过期了,它必须凭回执采纳 N+2 才能继续持有(见下)。
//
// 为什么凭标记的回滚要求标记原样还在(伤害 3,令牌互斥):目标节点载入时的 A2′(C++ exit_release_mark.h
// kLuaInheritClear)先核对 owner_epoch == N+1、再删掉 ≤ N+1 的标记,确认之后才建实体。两段是同一 Redis 上
// 的原子 Lua,只能先后执行:A2′ 在先 → 标记已删,这里回 3、归属留在 B,源端读到 (N+1, 指向 B) 销毁自己;
// 这里在先 → epoch 已是 N+2,A2′ 回 -1,B 拒建实体、发不出 DBTask(N+1)。「报错但已投递」不再造成双持有
// 与回档。同一条也收住「回滚晚于源端取证」:源端原子删掉本族标记之后,迟到的回滚只能回 3。
// 不凭标记的铸造(首次落点、第二条腿、死节点接管)没有令牌可查,这一支做不到互斥(§12.8 第十三节残余)。
//
// 回执与转写 —— 源端怎么知道自己被回滚了:
//   - 回执 = 所凭标记原文,写在恢复出的 location 里(PlayerLocation.rollback_receipt),与 INCR 同一段
//     Lua 原子落地。源 scene 的 ResolveTravelOutcome 只认与自己本次标记原文逐字节相同的回执,并且先在
//     同一原子脚本里删掉本次交接这一族标记,才采纳 N+2;
//   - 标记转写成 "{N+2}:{同一 saved_at_ms}":它说的「t 时刻的状态已落盘、持有者不再写」依然为真。源端
//     落到销毁侧(应答与取证都迟到、玩家重登)时,改派 / 重登凭它过换手门(marker.Epoch == 观察值),
//     不会卡在 18;后缀逐字节不变,源端按 saved_at_ms 的本族删除才删得到它。
//
// 分支顺序与写入纪律(改之前必读,任何一条被改掉都会重新打开「带着活标记解冻」的回档口子):
//   - 先比 location 与 epoch,再查令牌。首发回 -after 之后源端可能随即删掉转写标记,重发必须走到
//     「重放识别」回 2,不能因为标记不在了变成 3;
//   - 只在原标记原样还在时转写;**重放分支一个字节都不写**,尤其不重写转写标记 —— 否则源端删除之后,
//     一次迟到的重发会把标记重新写活;
//   - 源端必须用原子脚本删掉本族标记之后才能采纳;应答里的 owner_epoch_after_rollback 永不作为采纳凭证。
//
// 重放识别(返回 2):go-redis 对读超时 / EOF 会把同一条 EVAL 原样重发(go-zero MaxRetries=3)。首发已执行
// 时状态是 (epoch = ARGV[3]+1, location = ARGV[2])。能同时造出这两样的只有本请求自己的回滚:ARGV[2] 里带着
// N+2 与旧 UpdateTime,凭标记时还带唯一的回执;铸造只写 UpdateTime = now 的新字节,造不出 ARGV[2](本请求
// 铸造 EVAL 的重发在回滚之后到达时,epoch 已是它观察值 +2,铸造 Lua 回 0,也不写)。
// ARGV[2] 为空(DEL)时的误报要「首发从未执行 + 别人铸出 ARGV[3]+1 + LeaveScene 又删掉 location」同时成立,
// 后果只是人数多还一次(已登记的残余,LeaveScene 要等约 30s 的断线租约)。
// keep 回滚永不返回 2:另一个不铸造的落点在同一秒把同一 scene/node/zone/epoch 写回,字节与 ARGV[2] 完全
// 相同(UpdateTime 秒级,多副本间还有时钟偏差),证明不了是本请求所为,误报会让调用方多还一次人数。
//
// tonumber 比较:epoch 远小于 2^53,双精度下精确;cur 被写坏(非数字)时得 nil,比较为假,回 0。
const luaRollbackPlayerPlacement = `
local loc = redis.call("GET", KEYS[1])
local cur = redis.call("GET", KEYS[2])
if cur == false then
    cur = "0"
end
if loc == ARGV[1] and cur == ARGV[3] then
    if ARGV[5] ~= "" and redis.call("GET", KEYS[3]) ~= ARGV[5] then
        return 3
    end
    local after = 0
    if ARGV[4] == "bump" then
        after = redis.call("INCR", KEYS[2])
    end
    if ARGV[2] == "" then
        redis.call("DEL", KEYS[1])
    else
        redis.call("SET", KEYS[1], ARGV[2])
    end
    if ARGV[6] ~= "" then
        redis.call("SET", KEYS[3], ARGV[6], "EX", ARGV[7])
    end
    if ARGV[4] == "bump" then
        return -after
    end
    return 1
end
if ARGV[4] == "bump" and tonumber(cur) == tonumber(ARGV[3]) + 1 then
    if (ARGV[2] == "" and loc == false) or (ARGV[2] ~= "" and loc == ARGV[2]) then
        return 2
    end
end
return 0
`

// rollbackPlayerPlacement 的结果标签(scene_manager_enter_scene_rollback_total{outcome}):
//
//	rolled_back / already_rolled_back / superseded / redis_error  见 rollbackPlayerPlacement;
//	marker_gone  凭标记铸造的回滚发现所凭标记已不是那一份(目标节点 A2′ 已消费令牌、源端已取证或撤回),
//	             一个字节都没动,本次落点保留;
//	plan_error   回滚计划构造失败(planRouteRollback,按构造不可达),没有回滚。
const (
	rollbackOutcomeRolledBack        = "rolled_back"
	rollbackOutcomeAlreadyRolledBack = "already_rolled_back"
	rollbackOutcomeSuperseded        = "superseded"
	rollbackOutcomeRedisError        = "redis_error"
	rollbackOutcomeMarkerGone        = "marker_gone"
	rollbackOutcomePlanError         = "plan_error"
)

// errOwnerEpochConflict 表示铸造 epoch 时 CAS 失败:有并发的 EnterScene 抢先推进了
// epoch。Redis 未被改动,调用方翻成 constants.ErrOwnerEpochConflict(可重试)。
var errOwnerEpochConflict = errors.New("owner epoch advanced by a concurrent EnterScene")

// errHandoffWithdrawn 表示预检时读到的 handoff 标记在落点那一刻已经不在(或被换过):
// 源 scene 撤回了交接(应答超时 / 玩家取消),它即将解冻继续持有玩家。Redis 未被改动,
// 调用方翻成 constants.ErrHandoffPending(可重试)。
var errHandoffWithdrawn = errors.New("handoff marker withdrawn by the source scene")

// currentOwnerEpoch 读当前归属 epoch,不铸造。键不存在返回 0(旧版 / 从未分配)。
// 值损坏(非十进制整数)按错误返回:那是存储被写坏,不是可以猜的状态。
func currentOwnerEpoch(svcCtx *svc.ServiceContext, playerID uint64) (uint64, error) {
	raw, err := svcCtx.Redis.Get(ownerepoch.OwnerEpochKey(playerID))
	if err != nil {
		return 0, err
	}
	return ownerepoch.ParseEpoch(raw)
}

// rollbackMode 是回滚对 owner_epoch 键的处理方式,即回滚 Lua 的 ARGV[4]。
type rollbackMode uint8

const (
	// rollbackModeKeep:epoch 键不动,location 按旧的原字节退回。用于本次没铸造的落点(同物理节点换图、
	// dev 旁路),以及同物理节点 epoch 0 的铸造(§12.6.9(c):路由的目标就是持有节点本身,投递成功时它
	// 缓存的就是铸出的 1,再 INCR 会废黜合法持有者)。
	rollbackModeKeep rollbackMode = iota
	// rollbackModeBump:本次铸造过(N → N+1),回滚再 INCR 一格到 N+2,只升不降。
	rollbackModeBump
)

// String 是 Lua 实参与日志里的写法,两边必须一致。
func (m rollbackMode) String() string {
	if m == rollbackModeBump {
		return "bump"
	}
	return "keep"
}

// routeRollbackPlan 是一次路由失败回滚要交给 Lua 的全部内容,由 planRouteRollback 一次算好。
// 首发与 go-redis 的原样重发用的是同一份字节 —— 重放识别靠 location 与 restoredRaw 逐字节相等。
type routeRollbackPlan struct {
	mode rollbackMode
	// restoredRaw 是回滚后的 location 原文;"" = DEL(本次之前没有 location)。
	restoredRaw string
	// epochAfter 是回滚后 owner_epoch 的值:bump = placed.epoch+1;keep = placed.epoch。
	epochAfter uint64
	// requiredMarker 是本次铸造所凭的 handoff 标记原文;"" = 不凭标记。Lua 里它必须原样还在才回滚,
	// bump 时它也就是写进恢复出的 location 的回滚回执。
	requiredMarker string
	// forwardMarker 只在 bump 且凭标记时非空:"{epochAfter}:" + requiredMarker 第一个冒号之后的**原文**。
	forwardMarker string
}

// planRouteRollback 决定路由 / 重定向失败后怎么撤掉本次落点(取代旧的 rollbackEpochFor:旧写法把 epoch
// 退回旧值,会同值重铸并让合法持有者的 DBTask 被永久判 stale,见 luaRollbackPlayerPlacement)。
//
//   - old / oldRaw:本次 EnterScene 开头读到的旧 location 与其原文(陈旧 zone / 死节点接管时调用方已把两者
//     一起清空,回滚 = DEL);oldZoneID:调用方按 scene:{id}:zone 解析出的旧 zone,是权威值。
//   - requiredMarker:本次铸造所凭的 handoff 标记原文(placementGuard.requiredMarker)。
//   - keepMintedEpoch:同物理节点 epoch 0 的铸造(enterscenelogic.go sameNodeZeroMint)。
//
// keep(!placed.minted 或 keepMintedEpoch):restoredRaw = oldRaw 原字节,epochAfter = placed.epoch。epoch 从不
// 回退、也不会把同一个值铸两次,与单调方案一致。
//
// bump(其余铸造过的落点):epochAfter = placed.epoch+1(placed.epoch ≥ 1,所以 ≥ 2)。old == nil(首次落点、
// 死节点接管、陈旧 zone)时 restoredRaw = "",回滚 = DEL;old != nil 时克隆旧 location、改下面三处后 Marshal
// 一次(首发与 go-redis 重发用同一份字节):
//   - OwnerEpoch = epochAfter:同节点落点的补种按 location 记录值补回缺失的键,这里必须写新值,否则键被
//     淘汰后补种会把单调性写坏;
//   - RollbackReceipt = requiredMarker:总是覆盖,不凭标记时就清掉旧回执;
//   - ZoneId 为 0 时回填 oldZoneID:不回填的话,C++ 判「location 指回本节点本 zone」必失败,只能多销毁一次
//     (顺带的语义变化:之后的 samePlacement 判定对这条记录从「按跨节点 fail-closed」变成按权威 zone 判)。
//
// UpdateTime 与 PendingSceneConfId 原样保留,等待落点的有效期判断不受影响。
//
// forwardMarker 冒号后的部分按原文字节拼,不经 ParseHandoff / String 重新格式化:转写标记与原标记的
// saved_at_ms 后缀必须逐字节相同,C++ 源端按后缀删除本族标记才认得出它。
//
// 返回 error 的情况按构造都不可达;调用方记 plan_error、不回滚、不退人数(fail-closed:本次落点留在 Redis,
// 源端取证读到 epoch 已变、location 不指回自己,走销毁侧):
//   - old 与 oldRaw 不成对;
//   - keep 却凭标记(令牌互斥只对 bump 成立:keep 不 INCR,A2′ 拦不住已投递的目标节点);
//   - 铸造过的落点 epoch 为 0;
//   - bump 凭标记却没有旧 location(凭标记放行必有旧 location);
//   - 所凭标记解析不了;
//   - 恢复出的 location 序列化失败。
func planRouteRollback(old *smpb.PlayerLocation, oldRaw string, oldZoneID uint32, placed placedLocation,
	requiredMarker string, keepMintedEpoch bool) (routeRollbackPlan, error) {
	if (old == nil) != (oldRaw == "") {
		return routeRollbackPlan{}, fmt.Errorf("旧 location 的解码值与原文不成对: old_nil=%v raw_len=%d", old == nil, len(oldRaw))
	}
	if !placed.minted || keepMintedEpoch {
		if requiredMarker != "" {
			return routeRollbackPlan{}, fmt.Errorf("keep 回滚不能凭 handoff 标记 %q", requiredMarker)
		}
		return routeRollbackPlan{mode: rollbackModeKeep, restoredRaw: oldRaw, epochAfter: placed.epoch}, nil
	}
	if placed.epoch == 0 {
		return routeRollbackPlan{}, errors.New("铸造过的落点 epoch 不可能为 0")
	}
	plan := routeRollbackPlan{mode: rollbackModeBump, epochAfter: placed.epoch + 1, requiredMarker: requiredMarker}
	if requiredMarker != "" {
		if old == nil {
			return routeRollbackPlan{}, fmt.Errorf("凭 handoff 标记 %q 的铸造却没有旧 location", requiredMarker)
		}
		if _, err := ownerepoch.ParseHandoff(requiredMarker); err != nil {
			return routeRollbackPlan{}, fmt.Errorf("所凭 handoff 标记解析失败: %w", err)
		}
		_, savedAtText, _ := strings.Cut(requiredMarker, ":")
		plan.forwardMarker = strconv.FormatUint(plan.epochAfter, 10) + ":" + savedAtText
	}
	if old == nil {
		return plan, nil
	}
	restored := proto.Clone(old).(*smpb.PlayerLocation)
	restored.OwnerEpoch = plan.epochAfter
	restored.RollbackReceipt = requiredMarker
	if restored.ZoneId == 0 {
		restored.ZoneId = oldZoneID
	}
	data, err := proto.Marshal(restored)
	if err != nil {
		return routeRollbackPlan{}, fmt.Errorf("序列化回滚后的 location 失败: %w", err)
	}
	plan.restoredRaw = string(data)
	return plan, nil
}

// rollbackVerdict 是一次回滚 EVAL 返回值的解读。
type rollbackVerdict struct {
	// outcome 取 rollbackOutcome*,原样计入 scene_manager_enter_scene_rollback_total。
	outcome string
	// restored = location 此刻已处于回滚后的状态,调用方据此退人数。
	restored bool
	// echo 填进 EnterSceneResponse.owner_epoch_after_rollback;0 = 本应答不对 epoch 做任何断言。
	echo uint64
	// drift = 返回值与计划的模式 / epoch 对不上:Lua 与 Go 的约定被改岔了。仍按已回滚处理(Lua 确实走了
	// 回滚分支),但不回显 epoch。
	drift bool
}

// classifyRollbackReply 把 luaRollbackPlayerPlacement 的返回值翻成结论。纯函数,单测见 TestClassifyRollbackReply。
//
//	≤ -2  bump 且 -signed == epochAfter → rolled_back,回显 epochAfter;否则 rolled_back + drift,不回显
//	1     rolled_back;bump 计划收到 1 记 drift
//	2     bump → already_rolled_back,回显 epochAfter;keep 按构造不可能 → 与未知值同归 redis_error
//	3     marker_gone,没回滚
//	0     superseded,没回滚
//	其余  redis_error(结果未知),没回滚 —— 不混进 superseded,那会把约定漂移藏成「并发推进」
func classifyRollbackReply(signed int64, plan routeRollbackPlan) rollbackVerdict {
	bump := plan.mode == rollbackModeBump
	switch {
	case signed <= -2:
		if bump && uint64(-signed) == plan.epochAfter {
			return rollbackVerdict{outcome: rollbackOutcomeRolledBack, restored: true, echo: plan.epochAfter}
		}
		return rollbackVerdict{outcome: rollbackOutcomeRolledBack, restored: true, drift: true}
	case signed == 1:
		return rollbackVerdict{outcome: rollbackOutcomeRolledBack, restored: true, drift: bump}
	case signed == 2 && bump:
		return rollbackVerdict{outcome: rollbackOutcomeAlreadyRolledBack, restored: true, echo: plan.epochAfter}
	case signed == 3:
		return rollbackVerdict{outcome: rollbackOutcomeMarkerGone}
	case signed == 0:
		return rollbackVerdict{outcome: rollbackOutcomeSuperseded}
	default:
		return rollbackVerdict{outcome: rollbackOutcomeRedisError}
	}
}

// rollbackPlacementAfterPushFailure = planRouteRollback + rollbackPlayerPlacement,两条回滚调用点(同 zone
// 第 7 步、跨 zone 第一条腿)共用。计划构造失败记 plan_error 并返回 (false, 0):不回滚、不退人数、不回显。
func rollbackPlacementAfterPushFailure(svcCtx *svc.ServiceContext, log logx.Logger, playerID uint64,
	old *smpb.PlayerLocation, oldRaw string, oldZoneID uint32, placed placedLocation,
	requiredMarker string, keepMintedEpoch bool) (restored bool, echo uint64) {
	plan, err := planRouteRollback(old, oldRaw, oldZoneID, placed, requiredMarker, keepMintedEpoch)
	if err != nil {
		metrics.ObserveEnterSceneRollback(rollbackOutcomePlanError)
		log.Errorf("[RouteRollback] outcome=%s 回滚计划构造失败(按构造不可达),不回滚、不退人数,本次落点留在 Redis: player=%d epoch=%d minted=%v required_marker=%q err=%v",
			rollbackOutcomePlanError, playerID, placed.epoch, placed.minted, requiredMarker, err)
		return false, 0
	}
	return rollbackPlayerPlacement(svcCtx, log, playerID, placed, plan)
}

// rollbackPlayerPlacement 按计划执行回滚 Lua(location 退回、铸造过的 epoch 再前进一格、凭标记时写回执并
// 转写标记),返回 (restored, echo):
//
//	rolled_back          本次执行完成了回滚                                → true;bump 回显 epochAfter
//	already_rolled_back  go-redis 重发时首发已经回滚过(见 Lua)             → true(人数照还,否则漏还);回显 epochAfter
//	marker_gone          凭标记铸造,所凭标记已不是那一份,什么都没动       → false
//	superseded           位置或 epoch 已被并发请求推进,什么都没动          → false
//	redis_error          go-redis 自带重试耗尽仍出错,或返回值无法解读      → false
//
// restored 决定调用方要不要把人数也退回去;echo 只进 ErrKafkaRoute 应答供日志排障,**绝不是采纳凭证**。
// 每种结果计一次 scene_manager_enter_scene_rollback_total{outcome},日志统一前缀 [RouteRollback]。
//
// marker_gone 之后本次落点留在 Redis(B 或等待落点上,epoch = N+1)。两种来源的终态都自洽:目标节点的 A2′
// 已消费令牌 → 玩家在目标节点正常游戏;源端已取证 / 撤回 → 源端读到 (N+1, 不指回自己) 自行销毁,location
// 停在从未载入的目标上(与「回滚晚于源端取证」同一终态)。分辨方法:查目标节点有没有 inherit /
// HandlePlayerAsyncLoaded 日志,以及源 scene 的裁决日志(runbook)。
//
// redis_error 时本函数不知道回滚执行了没有(读超时那一类可能已执行),Redis 里可能留着本次
// 落点:epoch 已推进、location 指向新落点 / 等待落点。有意**不做应用层重试**:
//   - go-redis 已按 MaxRetries=3 重试过;持续出错时再试多半同样失败,只会把 handler 往源端
//     看门狗(30s)推;
//   - zrpc 服务端超时之后,这次应答本来就到不了源端;
//   - 回滚晚于源端裁决落地更糟:location 会指回一个已经不在的实体(凭标记的回滚会因标记已被源端删掉而回
//     marker_gone,不凭标记的没有这层保护)。
//
// redis_error 之后谁来收尾,取决于调用点(两者都由源端的原子取证脚本 —— 删本族标记 + 读 epoch / location ——
// 裁决。回滚实际没执行时判成「已被放行」,走下面两支;读超时一类、回滚其实已执行时,凭标记的那一支留下了
// 回执,源端凭回执采纳解冻(判定表 B5),下面两支都不发生):
//   - 跨 zone 第一条腿(handleCrossZoneRedirect):源 scene 会重置客户端(失败 tip + 踢线 34),
//     玩家重登落到等待落点;
//   - 同 zone 路由失败(rollbackEnterSceneAfterRouteFailure):源端**不**重置客户端,源实体被
//     销毁而 gate 会话仍绑在源节点上 —— 玩家挂在哑连接上,要等客户端自己断线重登(已知限制,
//     cross-zone-scene-travel.md §12)。
func rollbackPlayerPlacement(svcCtx *svc.ServiceContext, log logx.Logger, playerID uint64,
	placed placedLocation, plan routeRollbackPlan) (restored bool, echo uint64) {
	result, err := svcCtx.Redis.Eval(luaRollbackPlayerPlacement,
		[]string{getPlayerLocationKey(playerID), ownerepoch.OwnerEpochKey(playerID), ownerepoch.HandoffKey(playerID)},
		placed.raw, plan.restoredRaw, strconv.FormatUint(placed.epoch, 10), plan.mode.String(),
		plan.requiredMarker, plan.forwardMarker, strconv.FormatInt(int64(ownerepoch.HandoffTTL.Seconds()), 10))
	if err != nil {
		metrics.ObserveEnterSceneRollback(rollbackOutcomeRedisError)
		log.Errorf("[RouteRollback] outcome=%s 回滚玩家位置/epoch 时 Redis 出错,不确定是否已执行,本次落点可能仍在: player=%d mode=%s epoch=%d epoch_after=%d required_marker=%q err=%v",
			rollbackOutcomeRedisError, playerID, plan.mode, placed.epoch, plan.epochAfter, plan.requiredMarker, err)
		return false, 0
	}
	signed, parseErr := strconv.ParseInt(fmt.Sprint(result), 10, 64)
	if parseErr != nil {
		metrics.ObserveEnterSceneRollback(rollbackOutcomeRedisError)
		log.Errorf("[RouteRollback] outcome=%s 回滚 Lua 返回值不是整数,按结果未知处理: player=%d mode=%s epoch=%d reply=%v",
			rollbackOutcomeRedisError, playerID, plan.mode, placed.epoch, result)
		return false, 0
	}
	verdict := classifyRollbackReply(signed, plan)
	metrics.ObserveEnterSceneRollback(verdict.outcome)
	if verdict.drift {
		log.Errorf("[RouteRollback] 回滚 Lua 的返回值与计划对不上(Lua 与 Go 的约定被改岔了),按已回滚处理、不回显 epoch: player=%d mode=%s epoch=%d epoch_after=%d reply=%d",
			playerID, plan.mode, placed.epoch, plan.epochAfter, signed)
	}
	switch verdict.outcome {
	case rollbackOutcomeRolledBack:
		if plan.mode == rollbackModeBump && !verdict.drift {
			log.Infof("[RouteRollback] outcome=%s mode=%s player=%d epoch=%d->%d receipt=%q forwarded=%q",
				rollbackOutcomeRolledBack, plan.mode, playerID, placed.epoch, plan.epochAfter, plan.requiredMarker, plan.forwardMarker)
		}
	case rollbackOutcomeAlreadyRolledBack:
		log.Infof("[RouteRollback] outcome=%s 首发回滚已生效(go-redis 重发),按已回滚处理: player=%d mode=%s epoch=%d->%d",
			rollbackOutcomeAlreadyRolledBack, playerID, plan.mode, placed.epoch, plan.epochAfter)
	case rollbackOutcomeMarkerGone:
		// go-zero logx 没有 WARN 级,用 Error 级让它在日志台上与 Kafka 失败那一行排在一起。
		log.Errorf("[RouteRollback] outcome=%s 所凭交接标记已不在(目标节点已消费令牌,或源 scene 已取证 / 撤回),保留本次落点、不退人数: player=%d epoch=%d required_marker=%q",
			rollbackOutcomeMarkerGone, playerID, placed.epoch, plan.requiredMarker)
	case rollbackOutcomeSuperseded:
		log.Errorf("[RouteRollback] outcome=%s 玩家位置或 epoch 已被并发请求推进,跳过旧状态覆盖与人数回滚: player=%d mode=%s epoch=%d",
			rollbackOutcomeSuperseded, playerID, plan.mode, placed.epoch)
	default:
		log.Errorf("[RouteRollback] outcome=%s 回滚 Lua 返回了约定之外的值,按结果未知处理: player=%d mode=%s epoch=%d reply=%d",
			verdict.outcome, playerID, plan.mode, placed.epoch, signed)
	}
	return verdict.restored, verdict.echo
}

// handoffVerdict 是一次 handoff 标记比对的结论。
type handoffVerdict struct {
	// committed = 源 scene 已为当前 epoch 写出落盘标记,可以放行。
	committed bool
	// epoch 是拿来比对的 owner_epoch —— 调用方在本次 EnterScene 开头观察到的值。
	epoch uint64
	// marker 是读到的标记原文(日志用;空串 = 没有标记)。
	marker string
}

// checkHandoffCommitted 读 player:{id}:handoff,判定「源 scene 是否已经把
// observedEpoch 这一归属代际的最终态落地」。
//
// 判据只有一条:handoff.epoch == observedEpoch。标记里的 epoch 是源 scene 存盘
// 时持有的值,等于观察值才能证明是**当前持有者本人**在落地之后写的;小于它是
// 更早一代留下的旧标记(标记 EX 300s,过时的会自然消失,但比对不能靠 TTL)。
// 路由失败回滚转写出来的标记("{N+2}:{同一 saved_at_ms}",见 luaRollbackPlayerPlacement)按同一条判据
// 放行,判据不改:它说的「t 时刻的状态已落盘、持有者不再写」依然为真(源 scene 仍冻结,或已被判销毁);
// 源端凭回执采纳 N+2 之前,会在同一原子脚本里先删掉它。
//
// observedEpoch 必须是调用方在本次 EnterScene 开头读到的那个值,并且**原样**作为
// 后面落点 Lua 的 CAS 期望值。预检与落点锚在同一个 epoch 上,才谈得上「预检之后
// 被并发请求推进 → 落点被拒」;若落点时重新 GET,两个并发 EnterScene 会各自读到
// 对方推进后的值、各自 CAS 成功,同一名玩家被派到两个节点(gameplay 层双主)。
//
// 标记缺失 / 写坏 / 读失败,一律不放行(fail-closed)。读失败额外返回 err,让
// 调用方按 Redis 故障而不是「源未落盘」回复。
//
// epoch == 0(旧版写入的 location,侧车键从未铸造)不做特殊放宽:源 scene 手里
// 缓存的也是 0,它落盘后写出的标记就是 "0:{ms}",同样能比对通过;没有标记就
// 说明它还没落盘,与非 0 代际的语义完全一致。
func checkHandoffCommitted(svcCtx *svc.ServiceContext, playerID uint64, observedEpoch uint64) (handoffVerdict, error) {
	markerRaw, err := svcCtx.Redis.Get(ownerepoch.HandoffKey(playerID))
	if err != nil {
		return handoffVerdict{}, fmt.Errorf("读 handoff 标记失败: %w", err)
	}
	verdict := handoffVerdict{epoch: observedEpoch, marker: markerRaw}
	marker, parseErr := ownerepoch.ParseHandoff(markerRaw)
	if parseErr != nil {
		if !errors.Is(parseErr, ownerepoch.ErrNoHandoff) {
			// 标记被写坏了:没有可信证据,不放行。它会随 TTL 消失,或被源 scene
			// 下一次落盘覆盖;这里只能大声报,不能猜。
			logx.Errorf("[Handoff] 标记格式非法,按未落盘处理: player=%d raw=%q err=%v", playerID, markerRaw, parseErr)
		}
		return verdict, nil
	}
	verdict.committed = marker.Epoch == observedEpoch
	return verdict, nil
}
