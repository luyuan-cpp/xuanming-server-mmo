package logic

import (
	"errors"
	"fmt"
	"strconv"

	smpb "proto/scene_manager"
	"scene_manager/internal/svc"
	"shared/ownerepoch"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---------------------------------------------------------------------------
// 玩家归属 epoch(owner_epoch)与交接落盘标记(handoff)—— scene_manager 侧
//
// 两把键的格式契约在 shared/ownerepoch(Go 与 C++ 两边一字不差)。本文件只放
// scene_manager 自己的读写:
//
//  1. 铸造 + CAS 写 location 的 Lua(placePlayerLocation 用);
//  2. 路由失败后的精确值回滚(location 与 epoch 一起退回);
//  3. 换手门第二道:handoff 标记比对(cross-zone-scene-travel.md CZ-4)。
//
// 不变量(scene-owner-reentry-barrier.md §3.3):epoch 只在 EnterScene 这一个
// 闸口推进,而且**只在持有者真的换了的时候**推进:首次落点、过了换手门的跨节点
// 交接、放行跨 zone 交接、等待落点的第二条腿。新值随路由事件带给目标节点;C++
// 存盘用 Lua 比对「缓存值 == Redis 当前值」,老 epoch 的写一律被拒。
//
// 持有者没换就不许推进 —— 同节点换图、同落点重连、dev 旁路下无标记的跨节点
// 交接都只做「epoch 不变才写 location」的 CAS。原因:持有节点要等 Kafka → gate →
// PlayerEnterGameNode 才知道新值,这段窗口(几十 ms 到秒级)里它任何一次周期 /
// 退出存盘都会被 C++ 的 CAS 拒掉,继而把**合法持有者**当成被废黜、不存盘销毁
// 实体(踢人 + 回档)。Go 这边写 location 也走同一把 epoch 的 CAS,两条通道不
// 各写各的。
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
// 两种失败都什么都不改。键不存在时 GET 给 false,与 "0" 归一后比对,这样首次铸造与
// 「回滚到 0」之后的再铸造走同一条路。
//
// 标记为什么要在这段 Lua 里再比一次:源 scene 等应答超时(或收到失败应答)后要决定
// 「解冻继续玩」还是「我已被放行、销毁实体」。它的做法是先 DEL 自己写的标记、再 GET
// owner_epoch —— 只要标记检查与铸造是同一个原子步骤,DEL 之后就不可能再有凭这份标记的
// 放行,于是「epoch 没变」就严格等价于「没被放行」。若只在预检里读标记,一个卡了很久的
// EnterScene 可以在源端解冻之后才铸造,同一名玩家在两个 zone 同时活着。
const luaMintEpochAndSetLocation = `
local cur = redis.call("GET", KEYS[1])
if cur == false then
    cur = "0"
end
if cur ~= ARGV[1] then
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

// luaRollbackPlayerPlacement:把 location 与 owner_epoch 一起退回本次写入之前的值。
//
//	KEYS[1] = player:{id}:location
//	KEYS[2] = player:{id}:owner_epoch
//	ARGV[1] = 本次写入的 location 精确字节(当前值必须仍是它)
//	ARGV[2] = 回滚后的 location 字节;空串 = 本次之前没有 location,DEL
//	ARGV[3] = 本次落点写进 location 的 epoch(当前值必须仍是它;键不存在按 "0")
//	ARGV[4] = 回滚后的 epoch;与 ARGV[3] 相同 = 本次没铸造,epoch 键不动
//
// 两个键都做 exact-value 比对:任一被并发请求推进过,本次就**一个字节都不动**
// (返回 0),否则会把后来者刚提交的归属覆盖掉。
//
// 为什么 epoch 也要退:路由失败意味着目标节点没有收到新 epoch,而**源节点仍然
// 持有玩家**(跨 zone 传送里源 scene 拿到失败应答后解冻继续跑)。它手里是旧
// epoch;不退回去,它之后每一次存盘都被 C++ 的 CAS 拒掉,进而销毁实体。
const luaRollbackPlayerPlacement = `
if redis.call("GET", KEYS[1]) ~= ARGV[1] then
    return 0
end
local cur = redis.call("GET", KEYS[2])
if cur == false then
    cur = "0"
end
if cur ~= ARGV[3] then
    return 0
end
if ARGV[2] == "" then
    redis.call("DEL", KEYS[1])
else
    redis.call("SET", KEYS[1], ARGV[2])
end
if ARGV[4] ~= ARGV[3] then
    redis.call("SET", KEYS[2], ARGV[4])
end
return 1
`

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

// rollbackEpochFor 决定路由失败回滚时 epoch 应退回的值。
//
// 本次没铸造(placed.minted == false)→ 原样返回 placed.epoch,回滚只动 location。
// 铸造过 → 优先用旧 location 里记录的 epoch,那才是**仍持有玩家的节点**手里缓存
// 的值;旧 location 没有 epoch(旧版写入 / 本次之前没有 location)时退回
// placed.epoch-1:INCR 是原子的,拿到 N 就意味着写入前的值恰好是 N-1。
//
// 为什么铸造过的 epoch 要退(而不是保持单调):走到回滚的铸造只有三种来源 ——
// 首次落点 / 等待落点(没有持有者,退不退都无害),以及过了标记门的交接(源已
// 落盘并冻结)。第三种里源 scene 拿到失败应答后要解冻继续跑,它手里是旧值,不退
// 回去它之后每次存盘都被 CAS 拒。残余窗口:kafka-go 报错但 broker 其实已投递,
// 目标节点拿着被收回的 N 载入 —— 它的存盘会被拒并自毁,location 已指回源节点,
// 玩家重连即恢复,不会双主。
func rollbackEpochFor(old *smpb.PlayerLocation, placed placedLocation) uint64 {
	if !placed.minted {
		return placed.epoch
	}
	if old != nil && old.GetOwnerEpoch() != 0 {
		return old.GetOwnerEpoch()
	}
	if placed.epoch == 0 {
		return 0
	}
	return placed.epoch - 1
}

// rollbackPlayerPlacement 用精确值 CAS 把本次落点(location + epoch)退回去。
// 返回是否真的回滚了;false 表示位置已被并发请求推进,本次什么都没动。
// Redis 出错按「没回滚」返回并记日志:路由失败已经是主错误,回滚只能尽力。
func rollbackPlayerPlacement(svcCtx *svc.ServiceContext, log logx.Logger, playerID uint64,
	placed placedLocation, oldRaw string, restoreEpoch uint64) bool {
	result, err := svcCtx.Redis.Eval(luaRollbackPlayerPlacement,
		[]string{getPlayerLocationKey(playerID), ownerepoch.OwnerEpochKey(playerID)},
		placed.raw, oldRaw, strconv.FormatUint(placed.epoch, 10), strconv.FormatUint(restoreEpoch, 10))
	if err != nil {
		log.Errorf("route 回滚玩家位置/epoch CAS 失败: player=%d epoch=%d err=%v", playerID, placed.epoch, err)
		return false
	}
	if fmt.Sprint(result) != "1" {
		log.Errorf("route 回滚检测到玩家位置或 epoch 已被并发请求推进,跳过旧状态覆盖与人数回滚: player=%d epoch=%d",
			playerID, placed.epoch)
		return false
	}
	return true
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
