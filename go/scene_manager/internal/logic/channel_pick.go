package logic

import (
	"fmt"
	"strconv"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
)

// 玩家主动选线的预检(docs/design/world-channel-switch.md §4.4)。
//
// 触发条件:EnterSceneRequest.client_channel_pick=true 且 scene_id != 0。这个标记只由 scene 节点的
// 客户端 EnterScene 入口在玩家指定了 scene_id 时置位;队伍跟随、镜像、疏散、登录、跨节点交接的
// **重发**都不置位,因此它们不受这里任何一条规则约束(上限、冷却、回收中都不拦)。
//
// 预检在 EnterScene 里排在跨 zone 重定向分支之后、解析目标场景之前:此时 location 已读、陈旧位置
// 已过滤,还没有任何预占、任何写入,也早于死节点改派和换手门。预检本身**纯只读**,所以每个拒绝点
// 都不需要成对释放任何东西。
//
// 它是放行权威的一部分,但不是原子的:人数上限是软上限(预检与预占不在同一原子步骤里,并发下
// 可能超出几个人),「回收中」的判定与 autoscaler 摘线之间也有毫秒级窗口(beginDrainWorldChannel
// 先 SREM 在役集合、隔几次 Redis 往返才 SADD 回收中集合;locateWorldChannel 的两次 SISMEMBER 都落在
// 这两步之间时两个集合都查不到,请求会被当成「不是分线」走原逻辑 —— 上限、冷却、conf 校验都不做)。
// 两者漏过去的后果都只是既有行为(多进几个人 / 进了回收中的线随后被改派),不涉及归属安全。
//
// 这个窗口来自摘线那两步不是原子的,根治在 world_autoscale.go(合成一次 SMOVE)。本文件能保证的是
// 读的顺序不把窗口放大,并且摘线一旦改成原子就不再有窗口(理由见 locateWorldChannel)。
// 同一窗口对线号的影响见 world_channel_directory.go 的 luaAllocWorldChannelLineNumbers。

// enter_scene_rejected_total 的 reason 取值(玩家主动选线的预检发出的五种,zone_id 是目标 zone)。
// 全部是固定字符串,低基数。它们是玩家操作触发的业务拒绝,有稳定的低速率属正常。
const (
	// 目标线在回收中集合里:已摘出选线集合,等人走空后销毁。
	rejectReasonChannelClosing = "channel_closing"
	// 请求带的 scene_conf_id 与这条线实际所属的地图对不上。
	rejectReasonChannelConfMismatch = "channel_conf_mismatch"
	// ChannelSwitch.Disabled=true:玩家主动切线被配置关闭。
	rejectReasonChannelSwitchDisabled = "channel_switch_disabled"
	// 目标线人数已到每线上限。
	rejectReasonChannelFull = "channel_full"
	// 距上一次主动切线还没过冷却。
	rejectReasonChannelSwitchCooldown = "channel_switch_cooldown"
)

// ChannelSwitchCooldownKeyFmt:STRING "1",存在 = 该玩家的切线冷却未过;TTL = 冷却秒数。
// 由 scene_manager 独占读写(预检只读判存在,EnterScene 收尾时写),不删,靠 TTL 过期。
const ChannelSwitchCooldownKeyFmt = "player:%d:channel_switch_cooldown"

func channelSwitchCooldownKey(playerID uint64) string {
	return fmt.Sprintf(ChannelSwitchCooldownKeyFmt, playerID)
}

// worldChannelMembership 是一个 scene_id 相对于「某 zone 的大世界分线」的身份。
type worldChannelMembership uint8

const (
	// worldChannelNotMember:不在任何一张大世界地图的在役 / 回收中集合里(副本、镜像,或不存在的 id)。
	worldChannelNotMember worldChannelMembership = iota
	// worldChannelActive:在某张图的在役集合里。
	worldChannelActive
	// worldChannelDraining:在某张图的回收中集合里。
	worldChannelDraining
)

// locateWorldChannel 查 sceneID 是不是 zoneID 里某张大世界地图的线,是的话属于哪张图、在役还是回收中。
//
// Redis 里没有 scene → conf 的反查键,只能逐张图 SISMEMBER。先查 hintConfID(请求带的 scene_conf_id,
// 正常切线它就是当前地图,两次往返即命中),查不到再遍历 World 表:请求带的 conf 可能与线所属的图
// 不一致,那正是调用方要识别并拒绝的情形,不能因为 hint 没命中就当成「不是分线」放过去。
//
// 每张图**先读在役、再读回收中,两个都读完才裁决**,回收中命中优先。线只会从在役单向移到回收中
// (beginDrainWorldChannel),所以:
//   - 顺序不能反。先读回收中、再读在役,会遇到「读回收中时线还没移过去、读在役时已经移走」而两头落空,
//     判成「不是分线」跳过整段预检;摘线即使是原子的也躲不掉。按现在的顺序,原子的移动最多让两边都读到。
//   - 在役命中也不能提前返回。同一个 id 同时在两个集合里(或读在役之后才被移走)时按回收中处理,
//     与目录发布同口径:回收中集合是排空状态的权威。
//
// 往返次数:每张图 2 次。命中 hint 时共 2 次;不是分线的目标(按 id 进副本)要查完整张 World 表,2 × 图数
// (当前 16 张图 → 34 次)。这条路径只有玩家手动按 id 进场景才走,不在登录热路径上。
//
// 只读。任何一次读失败都返回 error,由调用方 fail-closed。
func locateWorldChannel(svcCtx *svc.ServiceContext, zoneID uint32, sceneID, hintConfID uint64) (uint64, worldChannelMembership, error) {
	worldConfs := worldConfIds()
	candidates := make([]uint64, 0, len(worldConfs)+1)
	if hintConfID != 0 {
		candidates = append(candidates, hintConfID)
	}
	for _, confID := range worldConfs {
		if confID != hintConfID {
			candidates = append(candidates, confID)
		}
	}

	member := strconv.FormatUint(sceneID, 10)
	for _, confID := range candidates {
		active, err := svcCtx.Redis.Sismember(worldChannelsKey(zoneID, confID), member)
		if err != nil {
			return 0, worldChannelNotMember, fmt.Errorf("读在役线集合失败(zone=%d conf=%d): %w", zoneID, confID, err)
		}
		draining, err := svcCtx.Redis.Sismember(worldDrainingSetKey(zoneID, confID), member)
		if err != nil {
			return 0, worldChannelNotMember, fmt.Errorf("读回收中线集合失败(zone=%d conf=%d): %w", zoneID, confID, err)
		}
		if draining {
			return confID, worldChannelDraining, nil
		}
		if active {
			return confID, worldChannelActive, nil
		}
	}
	return 0, worldChannelNotMember, nil
}

// readScenePlayerCountChecked 与 readScenePlayerCount 只差一条:Redis 读失败时返回 error 而不是当 0。
// 选线预检要 fail-closed —— 读不到人数就当 0 放行,等于 Redis 抖动时上限失效。
// 键不存在、负数、非数字仍按 0(与目录展示同口径)。
func readScenePlayerCountChecked(svcCtx *svc.ServiceContext, sceneID uint64) (int64, error) {
	raw, err := svcCtx.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, sceneID))
	if err != nil {
		return 0, err
	}
	if raw == "" {
		return 0, nil
	}
	n, parseErr := strconv.ParseInt(raw, 10, 64)
	if parseErr != nil || n < 0 {
		return 0, nil
	}
	return n, nil
}

// isChannelSwitch 判定这次选线是不是一次「真正的切换」:玩家此刻落在某个具体节点的某个场景里,
// 而目标是另一个场景。只有真正的切换才查冷却、才起算冷却。
// 不算切换的三种:没有位置记录(首次落点;陈旧 / 属主已死的位置在 EnterScene 开头已被过滤成 nil)、
// 等待落点(跨 zone 传送途中,node_id 为空)、目标就是当前所在场景(同落点重连)。
func isChannelSwitch(currentLoc *scene_manager.PlayerLocation, targetSceneID uint64) bool {
	return currentLoc != nil && currentLoc.GetNodeId() != "" &&
		currentLoc.GetSceneId() != 0 && currentLoc.GetSceneId() != targetSceneID
}

// checkClientChannelPick 是玩家主动选线的只读预检。调用方保证 in.ClientChannelPick && in.SceneId != 0。
//
// 返回 (resp, chargeCooldown):
//   - resp != nil:拒绝,调用方原样返回它;此时没有任何状态被改动。
//   - resp == nil:放行,继续走 EnterScene 的原逻辑。chargeCooldown=true 表示这是一次真正的切换且
//     冷却开着,调用方要在本次请求收尾时按最终应答决定是否起算冷却(见 channelSwitchStartsCooldown)。
//
// 判定顺序(自上而下第一条命中):
//  1. 目标是不是本 zone 的大世界分线:回收中 → 22;都不在 → 不是分线,预检到此为止(按 id 进副本 /
//     镜像不受任何影响,包括下面的 Disabled);在役但与请求的 scene_conf_id 对不上 → 22。
//     为什么对不上要拒:resolveScene 在目标节点已死时会拿请求的 scene_conf_id 判用途并 CreateScene,
//     不能让一个对不上的值流进去把 A 图的线建成 B 图的场景。请求不带 conf(0)时没有可对的值,放行。
//  2. ChannelSwitch.Disabled → 22。
//  3. 人数 ≥ 每线上限 → 23。
//  4. 冷却:仅当这是一次真正的切换且冷却开着;冷却键还在 → 24。只读,不在这里占冷却。
//  5. 以上任何一步 Redis 读失败 → ErrRedis(fail-closed,与本文件所在流程的其它读失败一致)。
func (l *EnterSceneLogic) checkClientChannelPick(in *scene_manager.EnterSceneRequest, currentLoc *scene_manager.PlayerLocation,
	targetZoneId uint32) (*scene_manager.EnterSceneResponse, bool) {
	ownerConfID, membership, err := locateWorldChannel(l.svcCtx, targetZoneId, in.SceneId, in.SceneConfId)
	if err != nil {
		l.Logger.Errorf("[ChannelSwitch] 判定目标是否分线失败,fail-closed 拒绝: player=%d zone=%d scene=%d scene_conf_id=%d err=%v",
			in.PlayerId, targetZoneId, in.SceneId, in.SceneConfId, err)
		return errResp(constants.ErrRedis, fmt.Sprintf("读取分线集合失败，已拒绝切线: %v", err)), false
	}
	switch membership {
	case worldChannelNotMember:
		return nil, false
	case worldChannelDraining:
		return l.rejectChannelPick(in, targetZoneId, constants.ErrChannelUnavailable, rejectReasonChannelClosing,
			fmt.Sprintf("所选线路正在回收(scene=%d)，请选择其它线路；未修改玩家状态", in.SceneId)), false
	}
	if in.SceneConfId != 0 && in.SceneConfId != ownerConfID {
		return l.rejectChannelPick(in, targetZoneId, constants.ErrChannelUnavailable, rejectReasonChannelConfMismatch,
			fmt.Sprintf("所选线路(scene=%d)属于地图 %d，与请求的地图 %d 不一致；未修改玩家状态",
				in.SceneId, ownerConfID, in.SceneConfId)), false
	}

	cfg := l.svcCtx.Config.ChannelSwitch
	if cfg.Disabled {
		return l.rejectChannelPick(in, targetZoneId, constants.ErrChannelUnavailable, rejectReasonChannelSwitchDisabled,
			"切线功能已关闭；未修改玩家状态"), false
	}

	maxPlayers := l.svcCtx.Config.ChannelMaxPlayers()
	players, err := readScenePlayerCountChecked(l.svcCtx, in.SceneId)
	if err != nil {
		l.Logger.Errorf("[ChannelSwitch] 读目标线人数失败,fail-closed 拒绝: player=%d zone=%d scene=%d err=%v",
			in.PlayerId, targetZoneId, in.SceneId, err)
		return errResp(constants.ErrRedis, fmt.Sprintf("读取线路人数失败，已拒绝切线: %v", err)), false
	}
	if players >= maxPlayers {
		return l.rejectChannelPick(in, targetZoneId, constants.ErrChannelFull, rejectReasonChannelFull,
			fmt.Sprintf("所选线路人数已满(%d/%d)，请选择其它线路；未修改玩家状态", players, maxPlayers)), false
	}

	if !isChannelSwitch(currentLoc, in.SceneId) {
		return nil, false
	}
	cooldownSeconds := cfg.EffectiveCooldownSeconds()
	if cooldownSeconds <= 0 {
		return nil, false
	}
	cooling, err := l.svcCtx.Redis.Exists(channelSwitchCooldownKey(in.PlayerId))
	if err != nil {
		l.Logger.Errorf("[ChannelSwitch] 读切线冷却失败,fail-closed 拒绝: player=%d err=%v", in.PlayerId, err)
		return errResp(constants.ErrRedis, fmt.Sprintf("读取切线冷却失败，已拒绝切线: %v", err)), false
	}
	if cooling {
		return l.rejectChannelPick(in, targetZoneId, constants.ErrChannelSwitchCooldown, rejectReasonChannelSwitchCooldown,
			fmt.Sprintf("切线冷却中(间隔 %d 秒)，请稍后再试；未修改玩家状态", cooldownSeconds)), false
	}
	return nil, true
}

// rejectChannelPick 统一记一次选线预检拒绝:指标 + 日志 + 应答。
// Infof 而不是 Errorf:这些都是玩家操作触发的业务拒绝(点了一条满线 / 回收中的线 / 点得太快),不是异常。
// 应答文案只描述被选的线与请求本身,不带其他玩家的任何信息。
func (l *EnterSceneLogic) rejectChannelPick(in *scene_manager.EnterSceneRequest, targetZoneId uint32,
	code uint32, reason, message string) *scene_manager.EnterSceneResponse {
	metrics.ObserveEnterSceneRejected(targetZoneId, reason)
	l.Logger.Infof("[ChannelSwitch] 选线被拒: reason=%s player=%d zone=%d scene=%d scene_conf_id=%d",
		reason, in.PlayerId, targetZoneId, in.SceneId, in.SceneConfId)
	return errResp(code, message)
}

// channelSwitchStartsCooldown 判定一次通过了预检的切线请求,其最终应答是否起算冷却:
// 成功(0)或 ErrHandoffPending(18)。
//
// 为什么 18 也算:跨节点切线的第一跳必拿 18 —— scene 节点事先不知道目标线在不在本节点,收到 18 才
// 冻结 → 存盘 → 写交接标记 → **重发一条不带 client_channel_pick 的 EnterScene**(重发是按在途记录的
// scene_id / scene_conf_id 重新构造的)。真正落点的是那条重发,而它不带标记、不过预检,也就永远
// 不会起算冷却;所以冷却只能记在第一跳上。反过来若在预检入口就占冷却,同节点切线被别的原因拒绝时
// 玩家会白吃一次冷却。
//
// 17(再入屏障)、19(epoch 冲突)、7(路由失败,已回滚)等拒绝都不起算:玩家没有切成,
// 也没有进入交接流程。
func channelSwitchStartsCooldown(resp *scene_manager.EnterSceneResponse, err error) bool {
	if err != nil || resp == nil {
		return false
	}
	return resp.ErrorCode == 0 || resp.ErrorCode == constants.ErrHandoffPending
}

// startChannelSwitchCooldown 写切线冷却键(带过期的 SET,值 "1")。冷却关着时不写。
//
// 写失败只记日志:切线本身已经成功(或已进入交接),不能因为冷却没记上就把它改判成失败;
// 代价是这名玩家下一次切线不受冷却限制,有界。
func (l *EnterSceneLogic) startChannelSwitchCooldown(playerID uint64) {
	cooldownSeconds := l.svcCtx.Config.ChannelSwitch.EffectiveCooldownSeconds()
	if cooldownSeconds <= 0 {
		return
	}
	if err := l.svcCtx.Redis.Setex(channelSwitchCooldownKey(playerID), "1", int(cooldownSeconds)); err != nil {
		l.Logger.Errorf("[ChannelSwitch] 写切线冷却失败,本次切线不计冷却: player=%d cooldown=%ds err=%v",
			playerID, cooldownSeconds, err)
	}
}
