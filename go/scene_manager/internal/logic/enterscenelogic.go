package logic

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	kafkacontracts "proto/contracts/kafka"
	game "scene_manager/generated/pb/game"
	"scene_manager/internal/constants"
	"scene_manager/internal/metrics"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"proto/scene_manager"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

type EnterSceneLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	// assignGateForZone(ctx, svcCtx, targetZoneId, playerId):挑目标 zone 的 gate 并签
	// 绑定持票者的票据。字段化只为单测替换,真实实现是 AssignGateForZone。
	assignGateForZone func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error)
	logx.Logger
}

const (
	enterSceneDedupeTTLSeconds    = 60
	enterSceneDedupeDonePrefix    = "done:"
	enterSceneDedupePendingPrefix = "pending:"
)

// enter_scene_rejected_total 的 reason 取值(本文件发出的、与归属交接 / 跨 zone 传送有关的几种)。
// 全部是固定字符串,低基数;玩家 id、标记原文只进日志。
//
// 换手门的 18 拆成三种,前缀统一为 handoff_pending:看总量用 reason=~"handoff_pending.*",
// 告警只盯后两种。不拆的话,生产配置(AllowUnsafeCrossNodeHandoff=false)下每一次跨节点换图的
// 第一跳都会 +1,这条指标的速率就只反映"换图有多频繁",看不出异常。
//
//	no_marker     预检时一份标记都没有。绝大多数是常规第一跳:源 scene 收到 18 才冻结 → 存盘 →
//	              写标记 → 重发(player_lifecycle.cpp StartTravelHandoff)。少数是源节点硬崩后留下
//	              的位置记录(标记永远不会出现,cross-zone-scene-travel.md §10.3 的已知限制)——
//	              这种要对照同期的放行量看:只拒不放才是异常。
//	stale_marker  有标记,但不是当前归属代际的(或写坏了)。源 scene 重发之后仍走到这里 = 它手里
//	              缓存的 epoch 已经不是 Redis 当前值(已被废黜),是要查的信号。也有常规来源:
//	              跨 zone 放行 / 疏散改派不删标记,旧标记在 300s TTL 内会被下一次跨节点换图的
//	              第一跳读到 —— 所以它的基线不是 0,但远低于 no_marker。
//	withdrawn     预检通过,落点 Lua 里再比时标记已被源 scene 撤回(应答超时 / 玩家退出)。
const (
	rejectReasonHandoffNoMarker    = "handoff_pending_no_marker"
	rejectReasonHandoffStaleMarker = "handoff_pending_stale_marker"
	rejectReasonHandoffWithdrawn   = "handoff_pending_withdrawn"
	// 跨 zone 传送指定的地图在目标 zone 没有任何世界频道,第一条腿在不可回头点之前拒绝。
	rejectReasonTravelMapUnavailable = "travel_map_unavailable"
	// 第二条腿:第一条腿记下的目标地图解析失败,已回落默认大世界(人照常落地,不是拒绝整个请求;
	// 记在这条指标上是因为"玩家指定的那张图"确实被拒了,而且它不该有稳定速率)。
	rejectReasonPendingMapFallback = "pending_map_fallback"
)

const luaDeleteEnterSceneDedupeIfValue = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
end
return 0
`

const luaCompleteEnterSceneDedupe = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
    redis.call("SET", KEYS[1], ARGV[2], "EX", ARGV[3])
    return 1
end
return 0
`

func NewEnterSceneLogic(ctx context.Context, svcCtx *svc.ServiceContext) *EnterSceneLogic {
	return &EnterSceneLogic{
		ctx:               ctx,
		svcCtx:            svcCtx,
		assignGateForZone: AssignGateForZone,
		Logger:            logx.WithContext(ctx),
	}
}

func errResp(code uint32, msg string) *scene_manager.EnterSceneResponse {
	return &scene_manager.EnterSceneResponse{ErrorCode: code, ErrorMessage: msg}
}

func enterSceneRequestFingerprint(in *scene_manager.EnterSceneRequest) (string, error) {
	payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(in)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum[:]), nil
}

// EnterScene routes a player into a scene, managed by SceneManager.
func (l *EnterSceneLogic) EnterScene(in *scene_manager.EnterSceneRequest) (response *scene_manager.EnterSceneResponse, returnErr error) {
	// 回显 player_id:scene 节点的异步应答回调靠它把结果对回发起玩家(传送 / 疏散要据此
	// 收尾)。放在 defer 里覆盖所有返回路径,包括去重缓存的重放。
	defer func() {
		if response != nil {
			response.PlayerId = in.PlayerId
		}
	}()
	// 能在任何 Redis 预占或 location 写入之前判定的路由错误先判掉。
	// broker 错误只能在真正发送时得知，后面由 exact-value CAS 回滚。
	if in.GateId != "" {
		if l.svcCtx.Kafka == nil {
			return errResp(constants.ErrKafkaRoute, "Kafka route writer is unavailable"), nil
		}
		if _, err := strconv.ParseUint(in.GateId, 10, 32); err != nil {
			return errResp(constants.ErrInvalidGateID, fmt.Sprintf("invalid gate id %q: %v", in.GateId, err)), nil
		}
		// CLAUDE.md §7 不变量 2:发往 {type}-{id} topic 的消息必须填 target_instance_id
		// （目标节点 UUID），空值等于关闭防僵尸过滤。gate-{id} 这个 topic 名只带业务
		// node_id，而 node_id 会被回收复用：老 gate 崩了、新 gate 顶上同一个 id，
		// 老 gate 若还没彻底退出就会把这条 GateCommand 一起消费掉，把玩家踢/重定向到
		// 一个已经不属于它的会话上。GateInstanceId 是唯一能区分两代进程的字段。
		//
		// fail-closed 是安全的:全部真实调用方都已填该字段 —— gate C++ 在
		// client_message_processor.cpp 两处 set_gate_instance_id(node_uuid),
		// login 透传 sessionDetails.GetGateInstanceId(),场景侧 4 个
		// EnterSceneRequest 构造点也都填了(此前审计记录里说"还有两处没填"
		// 是把 GsEnterSceneRequest(scene↔scene S2S)误认成了本 RPC 的请求)。
		if in.GateInstanceId == "" {
			return errResp(constants.ErrInvalidGateID,
				"missing gate_instance_id (required for gate anti-zombie filtering)"), nil
		}
	}

	var dedupeKey string
	var dedupeOwner string
	var dedupeFingerprint string
	dedupeOwned := false
	defer func() {
		if !dedupeOwned {
			return
		}
		// pending 只是进行中占位。失败终态必须释放；成功终态则以 CAS 覆盖成
		// 完整 protobuf 响应。唯一 owner 防止旧请求超过 TTL 后误删/覆盖后来者。
		if returnErr != nil || response == nil || response.ErrorCode != 0 {
			if err := l.deleteEnterSceneDedupeIfValue(dedupeKey, dedupeOwner); err != nil {
				l.Logger.Errorf("释放失败请求的 EnterScene 去重占位失败: key=%s err=%v", dedupeKey, err)
			}
			return
		}

		payload, err := proto.Marshal(response)
		if err != nil {
			l.Logger.Errorf("序列化 EnterScene 成功响应失败: key=%s err=%v", dedupeKey, err)
			_ = l.deleteEnterSceneDedupeIfValue(dedupeKey, dedupeOwner)
			return
		}
		cached := enterSceneDedupeDonePrefix + dedupeFingerprint + ":" + base64.StdEncoding.EncodeToString(payload)
		if _, err := l.svcCtx.Redis.Eval(luaCompleteEnterSceneDedupe, []string{dedupeKey},
			dedupeOwner, cached, enterSceneDedupeTTLSeconds); err != nil {
			l.Logger.Errorf("缓存 EnterScene 完整成功响应失败: key=%s err=%v", dedupeKey, err)
		}
	}()

	// 0. REQUEST-LEVEL DEDUPLICATION: If caller provided a request_id,
	//    use Redis SET NX to guarantee at-most-once processing.
	if in.RequestId != "" {
		dedupStart := time.Now()
		var fingerprintErr error
		dedupeFingerprint, fingerprintErr = enterSceneRequestFingerprint(in)
		if fingerprintErr != nil {
			metrics.ObserveEnterSceneStage(in.GateZoneId, metrics.EnterSceneStageDedup, time.Since(dedupStart))
			return errResp(constants.ErrEncodeEvent, fmt.Sprintf("EnterScene request fingerprint failed: %v", fingerprintErr)), nil
		}
		// request_id 的唯一性只在玩家作用域内成立；不同玩家可能由不同客户端
		// 生成相同值，绝不能因此重放另一名玩家的路由或 redirect token。
		dedupeKey = fmt.Sprintf("enter_scene:dedup:%d:%s", in.PlayerId, in.RequestId)
		dedupeOwner = enterSceneDedupePendingPrefix + dedupeFingerprint + ":" + uuid.NewString()
		for attempt := 0; attempt < 2 && !dedupeOwned; attempt++ {
			ok, err := l.svcCtx.Redis.SetnxEx(dedupeKey, dedupeOwner, enterSceneDedupeTTLSeconds)
			if err != nil {
				metrics.ObserveEnterSceneStage(in.GateZoneId, metrics.EnterSceneStageDedup, time.Since(dedupStart))
				return errResp(constants.ErrRedis, fmt.Sprintf("EnterScene dedupe unavailable: %v", err)), nil
			}
			if ok {
				dedupeOwned = true
				break
			}

			existing, getErr := l.svcCtx.Redis.Get(dedupeKey)
			if getErr != nil {
				// 键可能恰好在 SETNX 与 GET 之间过期；再尝试一次 SETNX。
				continue
			}
			if strings.HasPrefix(existing, enterSceneDedupePendingPrefix) {
				parts := strings.SplitN(strings.TrimPrefix(existing, enterSceneDedupePendingPrefix), ":", 2)
				if len(parts) == 2 && parts[0] != dedupeFingerprint {
					metrics.ObserveEnterSceneStage(in.GateZoneId, metrics.EnterSceneStageDedup, time.Since(dedupStart))
					return errResp(constants.ErrEnterSceneIdempotencyConflict,
						"同一 request_id 已用于不同的 EnterScene 请求"), nil
				}
				metrics.ObserveEnterSceneStage(in.GateZoneId, metrics.EnterSceneStageDedup, time.Since(dedupStart))
				return errResp(constants.ErrEnterSceneInProgress,
					"相同 request_id 的 EnterScene 仍在执行，请稍后重试"), nil
			}
			if strings.HasPrefix(existing, enterSceneDedupeDonePrefix) {
				parts := strings.SplitN(strings.TrimPrefix(existing, enterSceneDedupeDonePrefix), ":", 2)
				if len(parts) == 2 && parts[0] != dedupeFingerprint {
					metrics.ObserveEnterSceneStage(in.GateZoneId, metrics.EnterSceneStageDedup, time.Since(dedupStart))
					return errResp(constants.ErrEnterSceneIdempotencyConflict,
						"同一 request_id 已用于不同的 EnterScene 请求"), nil
				}
				var decodeErr error
				var payload []byte
				if len(parts) == 2 {
					payload, decodeErr = base64.StdEncoding.DecodeString(parts[1])
				} else {
					decodeErr = fmt.Errorf("legacy done entry has no request fingerprint")
				}
				cached := &scene_manager.EnterSceneResponse{}
				if decodeErr == nil {
					decodeErr = proto.Unmarshal(payload, cached)
				}
				if decodeErr == nil && cached.ErrorCode == 0 {
					metrics.ObserveEnterSceneStage(in.GateZoneId, metrics.EnterSceneStageDedup, time.Since(dedupStart))
					l.Logger.Infof("重放 EnterScene 完整成功响应: request_id=%s player=%d", in.RequestId, in.PlayerId)
					return cached, nil
				}
				l.Logger.Errorf("EnterScene 去重缓存损坏，将清理后重新执行: key=%s err=%v", dedupeKey, decodeErr)
			}

			// 未知旧格式或损坏的 done 只能按“读到的精确值”删除，不能 DEL
			// 掉并发请求刚写入的新 owner。
			if err := l.deleteEnterSceneDedupeIfValue(dedupeKey, existing); err != nil {
				metrics.ObserveEnterSceneStage(in.GateZoneId, metrics.EnterSceneStageDedup, time.Since(dedupStart))
				return errResp(constants.ErrRedis, fmt.Sprintf("EnterScene dedupe repair failed: %v", err)), nil
			}
		}
		metrics.ObserveEnterSceneStage(in.GateZoneId, metrics.EnterSceneStageDedup, time.Since(dedupStart))
		if !dedupeOwned {
			return errResp(constants.ErrEnterSceneInProgress,
				"相同 request_id 的 EnterScene 状态正在变化，请稍后重试"), nil
		}
	}

	// 1. Determine the target zone.
	//    - Caller provided zone_id: use it (explicit cross-zone teleport).
	//    - sceneId != 0, no zone_id: look up scene:{id}:zone (reconnect / follow).
	//    - sceneId == 0, no zone_id: fall back to existing PlayerLocation zone,
	//      then to GateZoneId (first login — gate zone = home zone).
	targetZoneId := in.ZoneId
	if targetZoneId == 0 && in.SceneId != 0 {
		targetZoneId = GetSceneZone(l.svcCtx, in.SceneId)
	}
	currentLoc, currentLocRaw, locErr := getPlayerLocationWithRaw(l.svcCtx, in.PlayerId)
	if locErr != nil {
		return errResp(constants.ErrRedis, fmt.Sprintf("读取玩家当前位置失败，已拒绝进入场景: %v", locErr)), nil
	}
	// 本次请求对归属 epoch 的**唯一一次**观察。换手门的标记比对、落点 Lua 的 CAS
	// 期望值、同落点重连下发的值,全部用它 —— 决策与提交锚在同一个 epoch 上,
	// 观察之后任何并发 EnterScene 的推进都会让本次落点 CAS 失败(见 placePlayerLocation)。
	observedEpoch, epochErr := currentOwnerEpoch(l.svcCtx, in.PlayerId)
	if epochErr != nil {
		return errResp(constants.ErrRedis, fmt.Sprintf("读取玩家归属 epoch 失败，已拒绝进入场景: %v", epochErr)), nil
	}
	currentZoneID := uint32(0)
	if currentLoc != nil {
		currentZoneID = currentLoc.ZoneId
		if currentZoneID == 0 && currentLoc.SceneId != 0 {
			currentZoneID = GetSceneZone(l.svcCtx, currentLoc.SceneId)
		}
	}
	// 合服 / 下线 zone 留下的陈旧 location:旧 zone 里已经没有任何活着的场景
	// 节点,就不存在「老节点还在存盘」这件事,下面两道交接门禁守护的对象已经
	// 消失。此时把它当作没有位置记录处理,否则玩家从两条腿(经源区 Gate 走
	// 重定向、经目标区 Gate 直接落点)都会被换手门(ErrHandoffPending:死掉的
	// 源永远写不出落盘标记)永久挡在门外。判定规则见 playerLocationOwnerGone;
	// 拿不准一律沿用原拒绝。
	if currentLoc != nil && l.playerLocationOwnerGone(currentLoc, currentZoneID) {
		l.Logger.Infof("忽略已下线 zone 的陈旧玩家位置: player=%d stale_zone=%d stale_node=%s stale_scene=%d gate_zone=%d target_zone=%d",
			in.PlayerId, currentZoneID, currentLoc.GetNodeId(), currentLoc.GetSceneId(), in.GateZoneId, targetZoneId)
		// raw 一并清空:route 失败回滚时 CAS 会把 key DEL 掉而不是把陈旧值写回去;
		// 正常落点则由第 6 步的 SET 直接覆盖。
		currentLoc, currentLocRaw, currentZoneID = nil, "", 0
	}
	// takenOverLoc:本次请求若是从一个已死节点手里接管玩家,记下那条旧位置,落点成功后要把
	// 旧场景的人数还回去。
	var takenOverLoc *scene_manager.PlayerLocation
	var takenOverZone uint32
	// 同一件事的**节点级**版本:zone 还活着,但位置记录指向的那一个节点已经确认死亡、
	// 且再入屏障已过。死掉的节点永远写不出 handoff 标记,不放行的话它名下的玩家会被
	// ErrHandoffPending 永久挡在门外,只能等运维手工清 location —— 一次单节点崩溃变成
	// 一批玩家进不了游戏。判定规则与「为什么这样是安全的」见 playerLocationOwnerDead。
	if currentLoc != nil && l.playerLocationOwnerDead(currentLoc, currentZoneID) {
		metrics.ObserveEnterSceneOwnerDeadTakeover(currentZoneID)
		l.Logger.Infof("位置记录的属主节点已确认死亡且再入屏障已过,按无持有者处理: player=%d dead_zone=%d dead_node=%s dead_scene=%d owner_epoch=%d gate_zone=%d target_zone=%d",
			in.PlayerId, currentZoneID, currentLoc.GetNodeId(), currentLoc.GetSceneId(), observedEpoch, in.GateZoneId, targetZoneId)
		// 与上面同理把 raw 清空。旧场景的人数留到本次落点**成功之后**再还(见函数末尾的
		// releaseTakenOverSceneCount):拒绝 / 回滚路径上不能动它。
		takenOverLoc, takenOverZone = currentLoc, currentZoneID
		currentLoc, currentLocRaw, currentZoneID = nil, "", 0
	}
	// 「等待落点」= 跨 zone 交接的第一条腿已经放行(location 只剩目标 zone 与
	// epoch,node_id 为空),目标 zone 还没落点。此刻**没有任何节点持有该玩家**:
	// 源 scene 在收到重定向应答后已销毁实体,目标节点还没被派到。两道换手门守护
	// 的对象(可能仍在写的旧持有者)不存在,所以第二条腿的落点、以及再次跨 zone
	// 重定向都直接放行,只按常规铸造 epoch + CAS 落点。
	awaitingPlacement := currentLoc != nil && currentLoc.GetNodeId() == "" && currentLoc.GetOwnerEpoch() != 0
	// 等待落点只在重定向票据有效期内指向目标 zone。票据过期仍未落地 = 传送失败
	// (CZ-8),之后的登录不再被这条记录牵去目标 zone,而是按 gate zone 走常规落点
	// (login 的 RedirectOnEnter 会把人送回 home,CZ-9「回家」)。没有持有者,在哪
	// 落点都安全;位置记录本身留着,由这次落点的 CAS 写覆盖。
	awaitingExpired := awaitingPlacement && awaitingPlacementExpired(currentLoc, time.Now())
	if targetZoneId == 0 {
		if currentLoc != nil && currentLoc.ZoneId != 0 && !awaitingExpired {
			targetZoneId = currentLoc.ZoneId
		}
	}
	if targetZoneId == 0 {
		targetZoneId = in.GateZoneId
	}

	// 2. CROSS-ZONE CHECK —— 必须在解析目标场景**之前**决定。
	//    Gate 不在目标 zone 时,本进程只负责把玩家送到目标 zone 的 Gate;目标
	//    场景由第二条腿上目标 zone 自己的 EnterScene 解析。若先解析再重定向,
	//    目标 zone 的过渡窗口(频道尚未建好 / 节点刚换代)会让第一条腿死在
	//    ErrNoAvailableNode 上,玩家永远到不了目标区 —— 这正是合服后源区
	//    玩家登录卡死的路径。这里不做任何预占,所以也没有需要成对释放的人数。
	crossZoneRedirect := in.GateZoneId != 0 && targetZoneId != 0 && in.GateZoneId != targetZoneId
	if crossZoneRedirect {
		// 重定向分两种,只有第二种动归属:
		//  a) 目标 zone 就是玩家现在所在的 zone(访客掉线后从别区 gate 登录、或等待
		//     落点的玩家换了入口):只是把**连接**送过去,归属没变 —— 不过门、不写
		//     location、不铸造;第二条腿在目标 zone 里按同落点重连 / 常规换手门处理。
		//  b) 玩家要**离开**现在所在的 zone:某个节点可能仍持有并在写这名玩家,放行
		//     的唯一凭据是源 scene 已为当前 epoch 写出「已落盘」标记(CZ-4 第二道门)。
		//     放行 = 把 location 改成目标 zone 的「等待落点」。等待落点的位置没有
		//     持有者,不用过门;陈旧位置已在上面被过滤成 nil。
		//  没有位置记录(干净登出后的首次落点)同样只送连接,由第二条腿铸造。
		//  currentZoneID == 0(旧位置无法确定 zone)≠ 任何目标 zone,按 b) 过门,fail-closed。
		leavingZone := currentLoc != nil && currentZoneID != targetZoneId
		// 指定了目标地图、且真的要动归属的放行,在不可回头点之前先**只读**看一眼这张图在目标 zone
		// 开没开(CZ-5:失败要回源 scene 解冻并回 tip)。过了这一步就要铸 epoch、写等待落点、发 124,
		// 源 scene 随即销毁实体 —— 地图问题留到第二条腿才发现,玩家已经没有"原地"可回。
		// 与上面「不先解析场景」的约束不冲突:那条约束防的是预占式解析把**登录**重定向卡死在目标区的
		// 过渡窗口里;login 发来的请求从不带 SceneConfId,只有 scene 替在线玩家发的跨 zone 传送会带,
		// 拒绝它的后果只是玩家留在原地收到一条 tip。只送连接(!leavingZone)的重定向不写等待落点,不查。
		if leavingZone && in.SceneConfId != 0 {
			if resp := l.rejectTravelToUnopenedMap(in, targetZoneId); resp != nil {
				return resp, nil
			}
		}
		// 目标地图随等待落点一起记下:第二条腿是目标 zone 的 login 发来的 EnterScene,不带地图。
		guard := placementGuard{observedEpoch: observedEpoch, mint: true, pendingSceneConfID: in.SceneConfId}
		if leavingZone && !awaitingPlacement {
			resp, grant := l.requireHandoffCommitted(in, currentLoc, currentZoneID, targetZoneId, observedEpoch, "跨区重定向")
			if resp != nil {
				return resp, nil
			}
			guard.mint, guard.requiredMarker = grant.committed, grant.marker
		}
		crossStart := time.Now()
		resp, redirErr := l.handleCrossZoneRedirect(in, targetZoneId, currentLoc, currentLocRaw, currentZoneID,
			crossZonePlacement{place: leavingZone, guard: guard})
		metrics.ObserveEnterSceneStage(targetZoneId, metrics.EnterSceneStageCrossZone, time.Since(crossStart))
		return resp, redirErr
	}

	// 3. Resolve the target scene (sceneId + nodeId).
	//    跨 zone 传送的第二条腿:请求本身不带地图(目标 zone 的 login 不知道玩家要去哪张图),
	//    用第一条腿记在「等待落点」里的目标地图。只在这条记录仍然有效、且就是指向本次落点的
	//    zone 时采用;请求自己指定了场景 / 地图则以请求为准。
	sceneConfID := in.SceneConfId
	// confFromPending:这次要解析的地图不是请求指定的,而是第一条腿记下的。它决定解析失败时能不能回落。
	confFromPending := false
	if awaitingPlacement && !awaitingExpired && currentZoneID == targetZoneId &&
		in.SceneId == 0 && sceneConfID == 0 {
		sceneConfID = currentLoc.GetPendingSceneConfId()
		confFromPending = sceneConfID != 0
	}
	resolveStart := time.Now()
	sceneId, nodeId, reserved, err := l.resolveSceneForEnter(in.SceneId, sceneConfID, targetZoneId)
	if err != nil && confFromPending {
		// 等待落点里记的地图解析不出来(两条腿之间频道被回收 / 节点全挂 / 满员,或第一条腿的只读检查
		// 因 Redis 抖动放过了一张没开的图):回落到默认大世界再试一次,**人必须能落地**。
		// 走到第二条腿的玩家已经被源 scene 销毁、没有"原地"可回;硬拒的话,等待落点在票据有效期
		// (redirectTokenTTLSeconds)内会把他每一次登录都牵回本 zone、读到同一个 pending 值、再失败
		// 一次,只能等过期。只对 pending 来的地图回落:请求自己指定的场景 / 地图解析失败仍然硬拒,
		// 那种请求的发起方还持有玩家,能把失败告诉他。
		// 第一次解析失败时 reserved 必为 false,没有需要成对释放的预占。
		metrics.ObserveEnterSceneRejected(targetZoneId, rejectReasonPendingMapFallback)
		l.Logger.Errorf("[Travel] 等待落点记下的目标地图不可用,回落默认大世界: player=%d target_zone=%d pending_scene_conf_id=%d err=%v",
			in.PlayerId, targetZoneId, sceneConfID, err)
		sceneId, nodeId, reserved, err = l.resolveSceneForEnter(0, 0, targetZoneId)
	}
	metrics.ObserveEnterSceneStage(targetZoneId, metrics.EnterSceneStageSceneResolve, time.Since(resolveStart))
	if err != nil {
		// 再入屏障未到是**可重试**的瞬时拒绝,不是"没有可用节点"。用独立错误码
		// 下发,上游才能区分「退避重试」与「这张图真的没节点了」。
		if errors.Is(err, ErrReentryBarrierPending) {
			return errResp(constants.ErrSceneReentryBarrier, err.Error()), nil
		}
		return errResp(constants.ErrNoAvailableNode, err.Error()), nil
	}
	if in.GateId != "" {
		if _, parseErr := strconv.ParseUint(nodeId, 10, 32); parseErr != nil {
			if reserved {
				DecrInstancePlayerCount(l.svcCtx, targetZoneId, sceneId)
			}
			return errResp(constants.ErrInvalidNodeID,
				fmt.Sprintf("invalid scene node id %q: %v", nodeId, parseErr)), nil
		}
	}

	// 在任何旧场景扣减、ReleasePlayer、位置写入或 Gate 路由之前过换手门:跨节点
	// 进入新场景时旧节点可能仍在存盘,新节点会读到陈旧快照。放行凭据是源 scene
	// 为当前 epoch 写出的「已落盘」标记(CZ-4 第二道门,见 requireHandoffCommitted);
	// 已有位置记录的跨区重定向在第 2 步过的是同一道门,走到这里的都是同 zone 落点。
	//
	// NodeId 只在 zone 内唯一，不能只比较字符串。旧 zone 的 node "10" 与目标
	// zone 的 node "10" 仍是两个进程；若把它当同节点，会跳过 ReleasePlayer 并
	// 在没有落盘屏障时直接覆盖 location。仅以下两类能证明无需跨节点交接：
	//   1. scene_id/node_id 完全相同，且两边 zone 明确相同；
	//   2. 两边 zone 明确相同，且 node_id 相同的同节点切场景。
	// zone=0 的旧位置无法证明物理节点身份，按 fail-closed 处理，不回退到
	// 只比较 node_id 的歧义旧语义。等待落点的位置(node_id 为空)没有持有者,
	// 不算跨节点交接。
	samePlacement := currentLoc != nil && currentLoc.SceneId == sceneId && currentLoc.NodeId == nodeId &&
		currentZoneID != 0 && targetZoneId != 0 && currentZoneID == targetZoneId
	samePhysicalNode := currentLoc != nil && currentLoc.NodeId != "" && currentLoc.NodeId == nodeId &&
		currentZoneID != 0 && targetZoneId != 0 && currentZoneID == targetZoneId
	crossNodeHandoff := currentLoc != nil && !awaitingPlacement && !samePlacement && !samePhysicalNode
	// guard.mint:持有者换了才铸造。同节点换图持有者没换 —— 铸了,持有节点要等路由
	// 事件绕一圈才知道新值,窗口内它的周期 / 退出存盘会被 C++ CAS 拒掉,合法持有者
	// 被当成废黜销毁(踢人 + 回档)。跨节点交接:过了标记门才铸;dev 旁路下无标记
	// 放行时不铸,保持旧的竞态语义,让旧节点 ReleasePlayer 的释放存盘照常落地
	// (铸了它必被拒,每次跨节点换图确定性回档)。
	guard := placementGuard{observedEpoch: observedEpoch, mint: !samePhysicalNode}
	if crossNodeHandoff {
		resp, grant := l.requireHandoffCommitted(in, currentLoc, currentZoneID, targetZoneId, observedEpoch, "场景交接")
		if resp != nil {
			// 自动选择大世界频道会在 resolveSceneForEnter 内原子预占人数；拒绝
			// 请求时必须成对释放，不能让失败请求污染调度负载。
			if reserved {
				DecrInstancePlayerCount(l.svcCtx, targetZoneId, sceneId)
			}
			return resp, nil
		}
		guard.mint, guard.requiredMarker = grant.committed, grant.marker
	}

	// 3b. 归属 zone:随 RoutePlayerEvent 下发,scene 节点据此选存盘 topic(CZ-3)。
	//     只有真的要发路由事件时才查;查不到(data_service 不可用)按可重试拒绝,
	//     不能带着 home_zone_id=0 把人派下去让节点落进程 zone 库。
	var homeZoneID uint32
	if in.GateId != "" {
		hz, hzResp := l.resolveHomeZone(in)
		if hzResp != nil {
			if reserved {
				DecrInstancePlayerCount(l.svcCtx, targetZoneId, sceneId)
			}
			return hzResp, nil
		}
		homeZoneID = hz
	}

	// 4. IDEMPOTENCY CHECK: Is player already in the same scene on the same node?
	//    Location is unchanged, but we must still route the gate so the new
	//    session/connection is wired to the correct scene node.
	if currentLoc != nil {
		if samePlacement {
			if reserved {
				DecrInstancePlayerCount(l.svcCtx, targetZoneId, sceneId)
			}
			l.Logger.Infof("Player %d already in scene %d, sending route to gate (reconnect)", in.PlayerId, sceneId)
			if in.GateId != "" {
				// 同一落点重连**不铸造** epoch:归属没有变,持有节点手里的缓存值
				// 必须继续等于 Redis 当前值。原样把观察到的值放进路由事件,节点若据此
				// 重建实体(实体已被 AFK 清掉的重连)拿到的也是同一代。
				if err := l.routePlayerToGate(in, nodeId, sceneId, homeZoneID, observedEpoch); err != nil {
					l.Logger.Errorf("Failed to route reconnecting player %d to gate: %v", in.PlayerId, err)
					return errResp(constants.ErrKafkaRoute, fmt.Sprintf("route player to gate failed: %v", err)), nil
				}
			}
			return &scene_manager.EnterSceneResponse{ErrorCode: 0}, nil
		}
	}

	if !reserved {
		reserveStart := time.Now()
		count, err := AtomicIncrPlayerCountIfSceneExists(l.svcCtx, sceneId)
		if err != nil {
			metrics.ObserveEnterSceneStage(targetZoneId, metrics.EnterSceneStageReserve, time.Since(reserveStart))
			return errResp(constants.ErrUpdateLocation, fmt.Sprintf("reserve scene player count failed: %v", err)), nil
		}
		if count < 0 {
			metrics.ObserveEnterSceneStage(targetZoneId, metrics.EnterSceneStageReserve, time.Since(reserveStart))
			return errResp(constants.ErrNoAvailableNode, fmt.Sprintf("scene %d disappeared during enter", sceneId)), nil
		}
		// AtomicIncrPlayerCountIfSceneExists only bumps the per-scene
		// counter — bump the per-node aggregate to match what
		// IncrInstancePlayerCount/ReserveBestWorldChannel do, so the
		// composite-load score stays consistent across both reservation
		// paths. Don't double-call IncrInstancePlayerCount here: that
		// would re-INCR the per-scene counter the Lua already moved.
		if nodeId != "" {
			l.svcCtx.Redis.Incr(nodePlayerCountKey(targetZoneId, nodeId))
		}
		metrics.ObserveEnterSceneStage(targetZoneId, metrics.EnterSceneStageReserve, time.Since(reserveStart))
	}

	// 5b. Cross-node switch: notify the old scene node to release the player so
	// it persists state and tears down the entity. Same-node switches skip this
	// because the C++ node reuses the in-memory entity directly.
	//
	// Round 14: dispatchReleasePlayer is fully async — caller returns immediately
	// while the release + retry chain runs in a background goroutine. Previously
	// the sync 500ms deadline pinned EnterScene latency at ~535ms under 45k-bot
	// load, cascading into 71k robot-side scene-ready timeouts. AFK cleanup on
	// the old node remains the final fallback.
	//
	// 走到这里的跨节点交接要么已经过了「源已落盘」标记门,要么是 dev 旁路
	// (AllowUnsafeCrossNodeHandoff)。前者源节点已经存盘、只等这条应答销毁实体,
	// ReleasePlayer 是补一刀的幂等通知;后者 release 与新节点 load 之间仍没有
	// 持久化屏障,新节点可能读到旧节点尚未落盘的 stale 状态 —— 这正是旁路只许
	// 开发用的原因。同步等待也堵不住：C++ ReleasePlayer RPC 只保证
	// HandleExitGameNode **入队**了存盘，不保证已落盘。
	//
	// 注意:db 写管道的 kafka key / applied-seq 只约束 db 服务自己消费到的
	// 分表写入顺序；它既不等待旧 Scene 的 PlayerAllData Redis 写完成，也不会
	// 重写新 Scene 实际加载的同一份 key，因此**不是**交接屏障或本窗口的兜底。
	// 真正的兜底是下面铸造的 owner_epoch:旧节点迟到的写会被 C++ 存盘 Lua 与
	// db 消费者按 epoch 拒掉。
	if currentLoc != nil && currentLoc.NodeId != "" && !samePhysicalNode {
		relStart := time.Now()
		dispatchReleasePlayer(l.svcCtx, l.Logger, currentZoneID,
			currentLoc.NodeId, in.PlayerId, sceneId, nodeId)
		metrics.ObserveEnterSceneStage(targetZoneId, metrics.EnterSceneStageReleaseDispatch, time.Since(relStart))
	}

	// 6. Update Player Location (Source of Truth):铸造新 epoch + CAS 写 location,
	//    一段 Lua 原子完成。CAS 失败 = 并发 EnterScene 抢先推进了归属,本次不发
	//    路由(否则两个节点各拿一个 epoch、只有后者合法,前者白 load 一次)。
	updStart := time.Now()
	placed, updateErr := placePlayerLocation(l.svcCtx, in.PlayerId, sceneId, nodeId, targetZoneId, guard)
	metrics.ObserveEnterSceneStage(targetZoneId, metrics.EnterSceneStageUpdateLoc, time.Since(updStart))
	if updateErr != nil {
		DecrInstancePlayerCount(l.svcCtx, targetZoneId, sceneId)
		if errors.Is(updateErr, errHandoffWithdrawn) {
			metrics.ObserveEnterSceneRejected(targetZoneId, rejectReasonHandoffWithdrawn)
			l.Logger.Errorf("[Handoff] 落点时交接标记已被源 scene 撤回,本次不发路由: player=%d target_scene=%d target_node=%s",
				in.PlayerId, sceneId, nodeId)
			return errResp(constants.ErrHandoffPending, "源场景已撤回交接，请稍后重试；未修改玩家状态"), nil
		}
		if errors.Is(updateErr, errOwnerEpochConflict) {
			metrics.ObserveEnterSceneRejected(targetZoneId, "epoch_conflict")
			l.Logger.Errorf("归属 epoch 被并发 EnterScene 抢先推进,本次不发路由: player=%d target_scene=%d target_node=%s",
				in.PlayerId, sceneId, nodeId)
			return errResp(constants.ErrOwnerEpochConflict,
				"concurrent EnterScene advanced the owner epoch; retry"), nil
		}
		l.Logger.Errorf("Failed to update player location: %v", updateErr)
		return errResp(constants.ErrUpdateLocation, "Failed to update location"), nil
	}

	// 6b. Decrement old scene instance count after the new reservation is
	// committed so concurrent world-channel selection sees the in-flight enter.
	if currentLoc != nil && currentLoc.SceneId != 0 && currentLoc.SceneId != sceneId {
		DecrInstancePlayerCount(l.svcCtx, currentZoneID, currentLoc.SceneId)
	}

	// 7. Send Route Command to Gate. Broker ACK is the transaction commit
	// point: only after it succeeds may the response be cached as successful.
	if in.GateId != "" {
		routeStart := time.Now()
		if err := l.routePlayerToGate(in, nodeId, sceneId, homeZoneID, placed.epoch); err != nil {
			l.Logger.Errorf("Failed to route player %d to gate, rolling back location/epoch/counts: %v", in.PlayerId, err)
			l.rollbackEnterSceneAfterRouteFailure(in.PlayerId, currentLoc, currentLocRaw, placed,
				currentZoneID, targetZoneId, sceneId)
			metrics.ObserveEnterSceneStage(targetZoneId, metrics.EnterSceneStageRouteGate, time.Since(routeStart))
			return errResp(constants.ErrKafkaRoute, fmt.Sprintf("route player to gate failed: %v", err)), nil
		}
		metrics.ObserveEnterSceneStage(targetZoneId, metrics.EnterSceneStageRouteGate, time.Since(routeStart))
	} else {
		l.Logger.Infof("No GateID in EnterScene request for player %d", in.PlayerId)
	}

	l.releaseTakenOverSceneCount(takenOverLoc, takenOverZone)

	l.Logger.Infof("Player %d entered scene %d on node %s (zone %d, home_zone %d, owner_epoch %d)",
		in.PlayerId, sceneId, nodeId, targetZoneId, homeZoneID, placed.epoch)
	return &scene_manager.EnterSceneResponse{ErrorCode: 0}, nil
}

// requireHandoffCommitted 是 CZ-4 的第二道换手门:玩家已有位置记录、且本次要把
// 他换到别的节点 / zone 时,源 scene 必须已经为**当前** owner_epoch 写出落盘标记
// (player:{id}:handoff),否则拒绝。返回 (nil, grant) 表示放行:grant.committed=true
// 是凭标记放行(源已落盘,落点要铸造新 epoch,且 grant.marker 要原样带进落点 Lua 再比
// 一次);false 是 dev 旁路放行(不铸造)。
//
// 拒绝是可重试的瞬时状态(与 ErrSceneReentryBarrier 同款):源 scene 的
// SavePlayerToRedis 落地回调之后才写标记,上游退避重试就会放行;它不改任何状态。
// site 只进日志,不进指标 label。指标 reason 按「预检时有没有读到标记」分成
// handoff_pending_no_marker / handoff_pending_stale_marker(含义与各自的基线见文件头的常量说明)。
//
// AllowUnsafeCrossNodeHandoff 是开发旁路:没有标记也放行,并且**不铸造** epoch,
// 只做「epoch 没被并发推进才写 location」的 CAS。旁路走的是旧的竞态语义 —— 先异步
// 通知旧节点 ReleasePlayer、随即改派;此时若铸造,旧节点的释放存盘必然晚于 INCR、
// 被 C++ 的 CAS 拒掉,每次跨节点换图都确定性丢掉最后一段进度,比旁路本来的
// 「可能读到旧快照」更糟。不铸造 = 新旧节点短暂同持一个 epoch,这正是 unsafe 的
// 含义,只许 dev 用。先看标记、后看旁路:旁路开着但源确实写了标记时照样走安全路径。
func (l *EnterSceneLogic) requireHandoffCommitted(in *scene_manager.EnterSceneRequest, currentLoc *scene_manager.PlayerLocation,
	currentZoneID, targetZoneId uint32, observedEpoch uint64, site string) (*scene_manager.EnterSceneResponse, handoffVerdict) {
	verdict, err := checkHandoffCommitted(l.svcCtx, in.PlayerId, observedEpoch)
	if err != nil {
		l.Logger.Errorf("[Handoff] %s 读交接状态失败,fail-closed 拒绝: player=%d err=%v", site, in.PlayerId, err)
		return errResp(constants.ErrRedis, fmt.Sprintf("读取玩家交接状态失败，已拒绝: %v", err)), handoffVerdict{}
	}
	if verdict.committed {
		return nil, verdict
	}
	if l.svcCtx.Config.AllowUnsafeCrossNodeHandoff {
		l.Logger.Infof("[Handoff] dev 旁路 AllowUnsafeCrossNodeHandoff=true,无落盘标记放行且不铸造 epoch(%s): player=%d owner_epoch=%d old_scene=%d old_node=%s old_zone=%d target_zone=%d",
			site, in.PlayerId, observedEpoch, currentLoc.GetSceneId(), currentLoc.GetNodeId(), currentZoneID, targetZoneId)
		return nil, handoffVerdict{epoch: observedEpoch}
	}
	// 没有标记 = 常规第一跳;有标记却不是当前代际 = 源 scene 重发之后仍过不了门的那一类。
	// 两者分开计数,后者才有告警价值(verdict.marker 是预检读到的标记原文,空串 = 没有标记)。
	rejectReason := rejectReasonHandoffNoMarker
	if verdict.marker != "" {
		rejectReason = rejectReasonHandoffStaleMarker
	}
	metrics.ObserveEnterSceneRejected(targetZoneId, rejectReason)
	// Infof 而不是 Errorf:这不是异常。scene 节点事先不知道目标场景在不在本节点,跨节点换图的
	// 第一次请求注定拿到 18,它据此「冻结 → 存盘 → 写标记 → 重发」(player_lifecycle.cpp 的
	// StartTravelHandoff)。真正的异常是重发之后仍被 18 拒,看 handoff_pending_stale_marker 的速率;
	// scene 侧对应的是 [TravelHandoff] 汇总行里的 aborted。
	l.Logger.Infof("[Handoff] %s 暂拒:源 scene 尚未为当前归属代际写出落盘标记(可重试): player=%d owner_epoch=%d handoff=%q old_scene=%d old_node=%s old_zone=%d gate_zone=%d target_zone=%d",
		site, in.PlayerId, verdict.epoch, verdict.marker, currentLoc.GetSceneId(), currentLoc.GetNodeId(),
		currentZoneID, in.GateZoneId, targetZoneId)
	return errResp(constants.ErrHandoffPending,
		fmt.Sprintf("源场景尚未落盘(owner_epoch=%d 的交接标记未就绪)，请稍后重试；未修改玩家状态", verdict.epoch)), handoffVerdict{}
}

// releaseTakenOverSceneCount 在「从已死节点接管玩家」的落点成功之后,把玩家在旧场景里占的那
// 一个人数还回去。
//
// 为什么必须还:dead-node reconcile 只销毁**副本实例**;大世界频道会沿用同一个 scene_id 迁到
// 活节点上,instance:{id}:player_count 原值保留,全仓没有任何重算路径。不还的话,死节点上
// 当时有 N 个玩家,这 N 个人陆续回来后那个频道的人数就永久多 N —— 按最小人数选频道时一直
// 躲着它,autoscale 也永远排不空它。(接管落回同一个频道时,前面的预占已经 +1,这里 -1,净 0。)
//
// 为什么先看 scene:{id}:node 还在不在:副本实例已被 reconcile 销毁时计数键也删了,直接
// Incrby(-1) 会把键重新建出来(钳成 0 后留下一条垃圾)。场景还在才还,DecrInstancePlayerCount
// 自带 <0 归零钳制。
func (l *EnterSceneLogic) releaseTakenOverSceneCount(loc *scene_manager.PlayerLocation, zoneID uint32) {
	if loc == nil || loc.GetSceneId() == 0 {
		return
	}
	if lookupSceneNode(l.svcCtx, loc.GetSceneId()) == "" {
		return
	}
	DecrInstancePlayerCount(l.svcCtx, zoneID, loc.GetSceneId())
}

// playerLocationOwnerDead 判定 loc 记录的**那一个节点**是否已经确认死亡且再入屏障已过
// (所在 zone 仍有别的活节点;整 zone 下线走 playerLocationOwnerGone)。
//
// 为什么可以据此免掉 handoff 标记(CZ-4 第二道门):那道门守护的是「可能仍在写这名玩家
// 的旧持有者」。节点死了就没有这个对象;而「看上去死了其实没死」的僵尸由另外两层兜住 ——
//
//   - 再入屏障(scene-owner-reentry-barrier.md §3.2):节点从 etcd 消失后 C++ 老进程还有
//     kDrainBudget(15s)的紧急疏散在存盘,屏障(默认 20s)盖过这段窗口;屏障未到本函数
//     返回 false,请求照旧被换手门以可重试的 18 暂拒。
//   - owner_epoch CAS(§3.3):放行后的落点一定铸造新 epoch(此时 currentLoc 已被当作
//     不存在,走首次落点的 mint=true),僵尸手里的旧 epoch 之后每一次存盘都被 C++ 的
//     Lua 原子拒绝并自毁,DBTask 由 db 的 applied-epoch 守卫拒绝。屏障靠时间、epoch
//     不靠时间,两层同时在位才敢放行 —— 这正是阶段 1 落完之后才补这条的原因。
//
// 代价(崩溃固有,与本函数无关):死节点没来得及落盘的那段进度丢失,新节点读到的是上一次
// 周期存盘。
//
// 「死亡」要的是**正面证据**,不是「看不到它」:节点必须已经从 etcd 注册表里消失
// (租约到期 / 主动注销,见 isNodeGoneFromRegistry)。只看 Redis 负载集不够 —— 负载集的
// 成员资格由 leader 周期刷新,还会被别的路径短暂改写,缺席只说明「这一刻没看到它」。
// 若据此接管,一个只是暂时缺席的活节点名下正在玩的玩家,下一次换图会被直接派到别的节点、
// 读到最长一个存盘周期之前的旧档(本该走「18 → 冻结 → 存盘 → 标记」的安全交接)。
// (2026-09-19:world_init 曾经在一次 CreateScene RPC 超时后就摘负载集且不写 death_at,
// 那条路径已改成不碰负载集、并同样以 isNodeGoneFromRegistry 为改派前提,见 ba2337b0d。
// 本判定不依赖那次修复:负载集缺席在任何原因下都不单独构成死亡证据。)
// 进程已死但租约未到期的那几十秒里,请求照旧被 18 暂拒,租约到期后 death_at + 屏障走完即放行。
//
// 任何一步拿不准都 fail-closed(返回 false,沿用换手门的拒绝):
//   - zone 无法确定、node_id 为空(等待落点另有规则)→ false;
//   - 本进程尚未完成首次 etcd 全量同步,或节点仍在注册表里 → false;
//   - 本副本亲眼看到它消失还不到一个屏障时长 → false(不依赖 leader 写的 death_at,
//     见 load_reporter.go nodeGoneObservedAt);
//   - (zone,node) 身份有歧义(node_id 被复用、两代进程并存)→ false。注意 IsNodeAlive 对
//     歧义身份返回的是 false(它服务的是"别把场景派给它"),这里**不能**把那个 false
//     读成"死了",必须先单独判歧义;
//   - IsNodeAlive 在 Redis 抖动时按存活返回 → false;
//   - death_at 读失败 / 值非法 / 屏障未到 → reentryBarrierBlocks 为真 → false。
func (l *EnterSceneLogic) playerLocationOwnerDead(loc *scene_manager.PlayerLocation, zoneID uint32) bool {
	if loc == nil || zoneID == 0 || loc.GetNodeId() == "" {
		return false
	}
	nodeID := loc.GetNodeId()
	if isKnownNodeIdentityAmbiguous(zoneID, nodeID) {
		return false
	}
	if !isNodeGoneFromRegistry(zoneID, nodeID) {
		return false
	}
	if localGoneBarrierBlocks(l.svcCtx, zoneID, nodeID) {
		return false
	}
	if IsNodeAlive(l.svcCtx, zoneID, nodeID) {
		return false
	}
	if reentryBarrierBlocks(l.svcCtx, zoneID, nodeID, barrierSiteDeadOwnerTakeover) {
		return false
	}
	return true
}

// playerLocationOwnerGone 判定 loc 记录的属主节点是否已经**不可能**再写这名
// 玩家的数据 —— 只有这时才允许把这条位置记录当作不存在,绕过交接门禁。
//
// 三个条件全部成立才算「已消失」,任何一个拿不准都 fail-closed(返回 false,
// 沿用换手门的拒绝):
//
//  1. loc 所在 zone 的节点负载集 scene_nodes:zone:{z}:load 为空 —— 整个 zone
//     没有任何活着的场景节点(合服后源区永久下线就是这个形态)。只看单个
//     node 不够:node_id 会被回收复用,老 zone 里换代顶上来的同号进程仍可能
//     在写;zone 级别的空集才是「没人在写」的证据。
//  2. IsNodeAlive(zone, node) 为 false —— 与 1 冗余,但它内部把 Redis 抖动
//     当作存活、把重复 (zone,node) 身份当作不可信,正好补上 1 与本调用之间
//     的状态变化窗口。
//  3. 再入屏障已过(node:zone:{z}:{n}:death_at 不存在或已超过屏障时长)——
//     节点刚从 etcd 消失时 C++ 老进程还有 kDrainBudget 的 emergency relocate
//     在 SavePlayerToRedis;这段时间内它虽已不在负载集,却仍在写。屏障就是
//     为这段窗口设的,这里不能绕过它。
//
// zone 无法确定(zone_id=0 且 scene:{id}:zone 也没了)时同样 fail-closed:
// 没有 zone 就无法证明物理节点身份,与门禁本身的口径一致。
func (l *EnterSceneLogic) playerLocationOwnerGone(loc *scene_manager.PlayerLocation, zoneID uint32) bool {
	if loc == nil || zoneID == 0 {
		return false
	}
	liveNodes, err := l.svcCtx.Redis.Zcard(nodeLoadKey(zoneID))
	if err != nil {
		l.Logger.Errorf("读取 zone 节点负载集失败,陈旧位置判定 fail-closed 沿用拒绝: zone=%d node=%s err=%v",
			zoneID, loc.GetNodeId(), err)
		return false
	}
	if liveNodes > 0 {
		return false
	}
	if IsNodeAlive(l.svcCtx, zoneID, loc.GetNodeId()) {
		return false
	}
	if loc.GetNodeId() != "" && reentryBarrierBlocks(l.svcCtx, zoneID, loc.GetNodeId(), barrierSiteStaleLocation) {
		return false
	}
	return true
}

// awaitingPlacementExpired 判定一条「等待落点」位置是否已经过了重定向票据的有效期。
// 票据过期 = 客户端已不可能凭它落到目标 zone(CZ-8),这条记录不再牵引后续登录的去向。
// 只比较秒级时间戳:UpdateTime 由 scene_manager 自己在放行时写入,不跨进程比时钟。
func awaitingPlacementExpired(loc *scene_manager.PlayerLocation, now time.Time) bool {
	return now.Unix()-int64(loc.GetUpdateTime()) > redirectTokenTTLSeconds
}

// rejectTravelToUnopenedMap 是跨 zone 传送第一条腿上对目标地图的**只读**检查:目标 zone 里这张图
// 一个世界频道都没登记过就拒绝(返回非 nil),此时一个字节都没改,源 scene 按失败应答解冻并回 tip。
//
// 只看频道集合(worldChannelsKey,world_channels:zone:{z}:{conf})是否为空,不做解析、不预占:
//   - 集合为空 = 这张图此刻没在目标 zone 开着。副本 / 镜像的 conf id、客户端乱填的 id、只在别的
//     zone 开的图都落在这里(频道集合只登记 World 表里的图,由世界频道初始化与自动扩缩容维护);
//   - 集合非空但频道暂时全不可用(节点刚换代、满员)照常放行:陈旧频道由第二条腿的解析懒修复,
//     真落不进去时第二条腿回落默认大世界。这里拒它,就把目标区的过渡窗口变成了"传送不可用"。
//
// Redis 读失败按放行处理(fail-open):这道检查只为给玩家一个更早、更准的拒绝,不是安全门;
// 归属安全由后面的换手门与 epoch CAS 保证,地图兜底由第二条腿的回落保证。
func (l *EnterSceneLogic) rejectTravelToUnopenedMap(in *scene_manager.EnterSceneRequest, targetZoneId uint32) *scene_manager.EnterSceneResponse {
	channels, err := l.svcCtx.Redis.Scard(worldChannelsKey(targetZoneId, in.SceneConfId))
	if err != nil {
		l.Logger.Errorf("[Travel] 读目标 zone 的世界频道集合失败,跳过地图预检(第二条腿有回落兜底): player=%d target_zone=%d scene_conf_id=%d err=%v",
			in.PlayerId, targetZoneId, in.SceneConfId, err)
		return nil
	}
	if channels > 0 {
		return nil
	}
	metrics.ObserveEnterSceneRejected(targetZoneId, rejectReasonTravelMapUnavailable)
	l.Logger.Errorf("[Travel] 跨 zone 传送被拒:目标 zone 没有这张图的世界频道,未改任何状态: player=%d gate_zone=%d target_zone=%d scene_conf_id=%d",
		in.PlayerId, in.GateZoneId, targetZoneId, in.SceneConfId)
	return errResp(constants.ErrNoAvailableNode,
		fmt.Sprintf("no world channel for conf %d in target zone %d; player state untouched", in.SceneConfId, targetZoneId))
}

// crossZonePlacement 描述跨 zone 重定向要不要动归属(由 EnterScene 第 2 步判定)。
type crossZonePlacement struct {
	// place=true:玩家要离开现在所在的 zone,把 location 改成目标 zone 的「等待落点」。
	// false:只送连接(目标 zone 就是玩家所在 zone,或根本没有位置记录),location 与
	// epoch 一个字节都不动。
	place bool
	// guard 是落点的并发前提(观察到的 epoch / 是否铸造 / 所凭的标记),原样交给 placePlayerLocation。
	guard placementGuard
}

// handleCrossZoneRedirect 是跨 zone 交接的第一条腿:挑目标 zone 的 Gate、签绑定
// 持票者的票据、(placement.place 时)把归属推进到「等待落点」,再把
// RedirectToGateEvent 推给当前 Gate。
//
// 顺序刻意是「先签票据(纯读 etcd,无副作用)→ 再提交 location → 最后发 Kafka」:
// 签不出票据(目标 zone 没有 gate)时一个字节都不改;Kafka 失败时只有 location
// 与 epoch 需要回滚。
//
// location 被**更新**成 {zone=目标, node_id="", scene_id=0, owner_epoch=N} 而不是删除
// (CZ-5):这样崩溃遗留能被 Offline-Return 规则识别,第二条腿也能凭 node_id 为空
// 知道「没有持有者」直接落点。源 scene 在收到本应答后销毁实体,所以从这一刻起
// 玩家不再算在旧场景人数里;旧场景的 ReleasePlayer 不需要(源自己就是发起方)。
func (l *EnterSceneLogic) handleCrossZoneRedirect(in *scene_manager.EnterSceneRequest, targetZoneId uint32,
	currentLoc *scene_manager.PlayerLocation, currentLocRaw string, currentZoneID uint32,
	placement crossZonePlacement) (*scene_manager.EnterSceneResponse, error) {
	l.Logger.Infof("Cross-zone detected: player %d gate_zone=%d target_zone=%d place=%v mint=%v, redirecting",
		in.PlayerId, in.GateZoneId, targetZoneId, placement.place, placement.guard.mint)

	redirect, err := l.assignGateForZone(l.ctx, l.svcCtx, targetZoneId, in.PlayerId)
	if err != nil {
		l.Logger.Errorf("Cross-zone redirect failed for player %d: %v", in.PlayerId, err)
		return errResp(constants.ErrNoAvailableNode, "no gate available in target zone"), nil
	}

	var placed placedLocation
	if placement.place {
		var placeErr error
		placed, placeErr = placePlayerLocation(l.svcCtx, in.PlayerId, 0, "", targetZoneId, placement.guard)
		if placeErr != nil {
			if errors.Is(placeErr, errHandoffWithdrawn) {
				metrics.ObserveEnterSceneRejected(targetZoneId, rejectReasonHandoffWithdrawn)
				l.Logger.Errorf("[Handoff] 跨区放行时交接标记已被源 scene 撤回,本次不发重定向: player=%d target_zone=%d",
					in.PlayerId, targetZoneId)
				return errResp(constants.ErrHandoffPending, "源场景已撤回交接，请稍后重试；未修改玩家状态"), nil
			}
			if errors.Is(placeErr, errOwnerEpochConflict) {
				metrics.ObserveEnterSceneRejected(targetZoneId, "epoch_conflict")
				l.Logger.Errorf("跨区放行时归属 epoch 被并发 EnterScene 抢先推进,本次不发重定向: player=%d target_zone=%d",
					in.PlayerId, targetZoneId)
				return errResp(constants.ErrOwnerEpochConflict,
					"concurrent EnterScene advanced the owner epoch; retry"), nil
			}
			l.Logger.Errorf("跨区放行写入等待落点位置失败: player=%d target_zone=%d err=%v", in.PlayerId, targetZoneId, placeErr)
			return errResp(constants.ErrUpdateLocation, "Failed to update location"), nil
		}
		if currentLoc != nil && currentLoc.SceneId != 0 {
			DecrInstancePlayerCount(l.svcCtx, currentZoneID, currentLoc.SceneId)
		}
	}

	// Redirect token 只有 broker ACK 后才能作为成功响应缓存，否则相同
	// request_id 会永久重放一份客户端从未收到的假成功。
	if in.GateId != "" {
		if err := l.sendRedirectToGate(in, redirect); err != nil {
			l.Logger.Errorf("Failed to push redirect to gate (placed=%v), rolling back location/epoch: %v", placement.place, err)
			if placement.place && rollbackPlayerPlacement(l.svcCtx, l.Logger, in.PlayerId, placed, currentLocRaw,
				rollbackEpochFor(currentLoc, placed)) && currentLoc != nil && currentLoc.SceneId != 0 {
				IncrInstancePlayerCount(l.svcCtx, currentZoneID, currentLoc.SceneId)
			}
			return errResp(constants.ErrKafkaRoute, fmt.Sprintf("redirect to gate failed: %v", err)), nil
		}
	}

	if placement.place {
		l.Logger.Infof("Cross-zone handoff released: player=%d target_zone=%d owner_epoch=%d minted=%v (awaiting placement)",
			in.PlayerId, targetZoneId, placed.epoch, placed.minted)
	} else {
		l.Logger.Infof("Cross-zone redirect only (ownership unchanged): player=%d target_zone=%d", in.PlayerId, targetZoneId)
	}
	return &scene_manager.EnterSceneResponse{
		ErrorCode: 0,
		Redirect:  redirect,
	}, nil
}

// rollbackEnterSceneAfterRouteFailure restores the state visible before this
// request. Redis operations are best-effort because the route failure is
// already the primary error; every rollback failure is logged for repair.
//
// location 与 owner_epoch 一起退回(理由见 luaRollbackPlayerPlacement):路由没发
// 出去,目标节点不会拿到新 epoch,而旧节点仍持有玩家并缓存着旧 epoch。
func (l *EnterSceneLogic) rollbackEnterSceneAfterRouteFailure(playerID uint64, old *scene_manager.PlayerLocation, oldRaw string,
	placed placedLocation, oldZoneID, newZoneID uint32, newSceneID uint64) {
	if !rollbackPlayerPlacement(l.svcCtx, l.Logger, playerID, placed, oldRaw, rollbackEpochFor(old, placed)) {
		return
	}

	DecrInstancePlayerCount(l.svcCtx, newZoneID, newSceneID)
	if old != nil && old.SceneId != 0 && old.SceneId != newSceneID {
		// 旧 PlayerLocation.zone_id 可能来自历史数据而为 0；调用方已经按
		// scene:{id}:zone 解析出旧 zone，必须用解析结果恢复 zone-scoped aggregate。
		IncrInstancePlayerCount(l.svcCtx, oldZoneID, old.SceneId)
	}
}

func (l *EnterSceneLogic) deleteEnterSceneDedupeIfValue(key, expected string) error {
	_, err := l.svcCtx.Redis.Eval(luaDeleteEnterSceneDedupeIfValue, []string{key}, expected)
	return err
}

// sendRedirectToGate pushes a RedirectToGateEvent to the current Gate via Kafka.
func (l *EnterSceneLogic) sendRedirectToGate(in *scene_manager.EnterSceneRequest, redirect *scene_manager.RedirectToGateInfo) error {
	targetGateId, err := strconv.ParseUint(in.GateId, 10, 32)
	if err != nil {
		return fmt.Errorf("invalid gate id %q: %w", in.GateId, err)
	}

	event := &kafkacontracts.RedirectToGateEvent{
		PlayerId:       in.PlayerId,
		SessionId:      in.SessionId,
		TargetGateIp:   redirect.TargetGateIp,
		TargetGatePort: redirect.TargetGatePort,
		TokenPayload:   redirect.TokenPayload,
		TokenSignature: redirect.TokenSignature,
		TokenDeadline:  redirect.TokenDeadline,
	}
	payload, err := proto.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal RedirectToGateEvent: %w", err)
	}

	cmd := &kafkacontracts.GateCommand{
		PlayerId:         in.PlayerId,
		SessionId:        in.SessionId,
		TargetGateId:     uint32(targetGateId),
		TargetInstanceId: in.GateInstanceId,
		Payload:          payload,
		EventId:          uint32(game.ContractsKafkaRedirectToGateEventEventId),
	}
	bytes, err := proto.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal GateCommand: %w", err)
	}

	msg, err := GateCommandMessageFor(in.GateId, fmt.Sprintf("%d", in.PlayerId), bytes)
	if err != nil {
		return fmt.Errorf("address RedirectToGate command: %w", err)
	}
	// 不用可取消 context：kafka-go 在 context 返回后仍可能继续投递批次。
	// 真实 writer 由 MaxAttempts=1 + WriteTimeout 提供有界终态。
	if err := l.svcCtx.Kafka.WriteMessages(context.Background(), msg); err != nil {
		return fmt.Errorf("push to Kafka topic %s partition %d: %w", msg.Topic, msg.Partition, err)
	}

	l.Logger.Infof("Pushed RedirectToGate to Kafka %s/%d for player %d (gate %s)",
		msg.Topic, msg.Partition, in.PlayerId, in.GateId)
	return nil
}

// resolveScene resolves the concrete (sceneId, nodeId) for an EnterScene request.
//   - sceneId != 0: direct lookup — the caller already knows the exact scene instance.
//   - sceneId == 0, sceneConfId != 0: auto-select least-loaded channel for the map.
//   - sceneId == 0, sceneConfId == 0: fallback to the first configured main scene.
func (l *EnterSceneLogic) resolveScene(sceneId uint64, sceneConfId uint64, zoneId uint32) (uint64, string, error) {
	// Case 1: exact scene instance specified.
	if sceneId != 0 {
		if mappedZone := GetSceneZone(l.svcCtx, sceneId); mappedZone != 0 && mappedZone != zoneId {
			return 0, "", fmt.Errorf("scene %d belongs to zone %d, requested zone %d", sceneId, mappedZone, zoneId)
		}
		key := fmt.Sprintf("scene:%d:node", sceneId)
		nodeId, err := l.svcCtx.Redis.Get(key)
		if err != nil {
			return 0, "", fmt.Errorf("scene lookup failed: %w", err)
		}
		// go-zero 的 Redis.Get 对「key 不存在」返回 ("", nil)（内部把 redis.Nil 吞掉），
		// 所以 err==nil 并不代表场景存在。必须在这里把「查无此场景」拦下来。
		//
		// 漏了这一句的后果不是"查不到"，而是"凭空造出一个幽灵场景"：
		// 空 nodeId 会一路走到下面的 IsNodeAlive(zone, "")——成员不存在返回 false——
		// 被当成「场景活着但它所在的节点死了」，于是进 dead-node 自愈分支，
		// 挑一个活节点并 Set(scene:{id}:node, newNode) 把映射无 TTL 地写出来。
		// 而 CreateScene 又被 sceneConfId==0 挡掉（follow / 客户端路径正是 0），
		// 结果是：映射有了、ECS 实体没有。随后 AtomicIncrPlayerCountIfSceneExists
		// 的存在性守卫（EXISTS scene:{id}:node）正好看到这把刚被伪造出来的 key，
		// 守卫失效并继续 INCR 出 player_count —— 一个不在任何活跃集合里、
		// 永远不会被回收的幽灵场景，玩家被派进去后无人接收。
		if nodeId == "" {
			return 0, "", fmt.Errorf("scene %d does not exist", sceneId)
		}
		// 重复 (zone,node) 注册是身份歧义，不是普通节点死亡。若继续走
		// dead-node 自愈会静默改写 scene ownership，等于在两个同号进程中
		// 随机选一个并把现有场景迁走；必须保持映射不动并拒绝本次请求。
		if isKnownNodeIdentityAmbiguous(zoneId, nodeId) {
			return 0, "", fmt.Errorf("scene %d maps to ambiguous node identity zone=%d node=%s", sceneId, zoneId, nodeId)
		}
		// Validate the mapped node is still alive; reassign if dead.
		if !IsNodeAlive(l.svcCtx, zoneId, nodeId) {
			// 再入屏障:节点刚从 etcd 消失不代表它已经停笔。C++ 老节点丢租约后
			// 还有 kDrainBudget(15s)的 emergency relocate,期间仍在
			// SavePlayerToRedis。此刻改写 scene:{id}:node 并让新节点
			// CreateScene + load,就是同一玩家双写 / 回档。
			// 屏障没走完时**一个字节都不改**,返回可重试错误让上游带着同样的
			// scene_conf_id 退避重试。见 docs/design/scene-owner-reentry-barrier.md §3.2。
			if reentryBarrierBlocks(l.svcCtx, zoneId, nodeId, barrierSiteResolveScene) {
				return 0, "", fmt.Errorf("scene %d 的属主节点 %s 刚判死,再入屏障未到: %w",
					sceneId, nodeId, ErrReentryBarrierPending)
			}
			// Pick a replacement of the correct purpose so a world scene
			// never gets moved onto an instance-only node (C++ EnterScene
			// would then reject the request on type mismatch).
			// 拿不到 scene_conf_id 就发不出 CreateScene（见下面的 if），
			// 那样只会改写映射而不建实体 —— 同样是造幽灵场景。宁可拒绝本次请求，
			// 让上游带着 conf id 重试，也不要留下一条指向空节点的映射。
			if sceneConfId == 0 {
				return 0, "", fmt.Errorf("scene %d is on dead node %s and cannot be recreated without scene_conf_id", sceneId, nodeId)
			}
			purpose := constants.NodePurposeInstance
			if IsWorldConf(sceneConfId) {
				purpose = constants.NodePurposeWorld
			}
			newNodeId, err := GetBestNodeForPurpose(l.ctx, l.svcCtx, zoneId, purpose)
			if err != nil {
				return 0, "", fmt.Errorf("scene %d mapped to dead node %s and no live nodes for purpose %d: %w", sceneId, nodeId, purpose, err)
			}
			l.Logger.Infof("resolveScene: scene %d was on dead node %s, reassigning to %s (purpose=%d)", sceneId, nodeId, newNodeId, purpose)
			l.svcCtx.Redis.Set(key, newNodeId)
			nodeId = newNodeId

			// Ensure the new node creates the ECS scene entity.
			if sceneConfId != 0 {
				if _, err := RequestNodeCreateScene(l.ctx, l.svcCtx, zoneId, nodeId, uint32(sceneConfId), sceneId); err != nil {
					l.Logger.Errorf("resolveScene: CreateScene on new node %s for scene %d failed: %v", nodeId, sceneId, err)
				}
			}
		}
		return sceneId, nodeId, nil
	}

	// Need a scene_conf_id to allocate.
	if sceneConfId == 0 {
		if wids := worldConfIds(); len(wids) > 0 {
			sceneConfId = wids[0]
		} else {
			return 0, "", fmt.Errorf("no scene_conf_id provided and no default world scene configured")
		}
	}

	// Case 2: auto-select least-loaded channel.
	sid, nid, err := GetBestWorldChannel(l.ctx, l.svcCtx, sceneConfId, zoneId)
	if err != nil || sid == 0 {
		return 0, "", fmt.Errorf("no available channel for conf %d in zone %d", sceneConfId, zoneId)
	}
	return sid, nid, nil
}

func (l *EnterSceneLogic) resolveSceneForEnter(sceneId uint64, sceneConfId uint64, zoneId uint32) (uint64, string, bool, error) {
	if sceneId != 0 {
		sid, nid, err := l.resolveScene(sceneId, sceneConfId, zoneId)
		return sid, nid, false, err
	}

	if sceneConfId == 0 {
		if wids := worldConfIds(); len(wids) > 0 {
			sceneConfId = wids[0]
		} else {
			return 0, "", false, fmt.Errorf("no scene_conf_id provided and no default world scene configured")
		}
	}

	sid, nid, err := ReserveBestWorldChannelForEnter(l.ctx, l.svcCtx, sceneConfId, zoneId)
	if err != nil || sid == 0 {
		return 0, "", false, fmt.Errorf("no available channel for conf %d in zone %d", sceneConfId, zoneId)
	}
	return sid, nid, true, nil
}

// routePlayerToGate builds a GateCommand and pushes it to the gate's Kafka topic.
//
// homeZoneID 与 ownerEpoch 必须随事件走,不能让 C++ 节点自己去 Redis / data_service
// 读:两次改派挨得近时,先派的节点若在后一次推进之后才读,会读到后一次的值并与
// 后派的节点一样自认为最新 —— 双主。事件里带的值才是「这一次路由决策」的值。
func (l *EnterSceneLogic) routePlayerToGate(in *scene_manager.EnterSceneRequest, nodeId string, sceneId uint64,
	homeZoneID uint32, ownerEpoch uint64) error {
	targetNodeId, err := strconv.ParseUint(nodeId, 10, 32)
	if err != nil {
		return fmt.Errorf("invalid scene node id %q: %w", nodeId, err)
	}
	targetGateId, err := strconv.ParseUint(in.GateId, 10, 32)
	if err != nil {
		return fmt.Errorf("invalid gate id %q: %w", in.GateId, err)
	}

	event := &kafkacontracts.RoutePlayerEvent{
		SessionId:    in.SessionId,
		TargetNodeId: uint32(targetNodeId),
		SceneId:      sceneId,
		PlayerId:     in.PlayerId,
		HomeZoneId:   homeZoneID,
		OwnerEpoch:   ownerEpoch,
	}
	payload, err := proto.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal RoutePlayerEvent: %w", err)
	}

	cmd := &kafkacontracts.GateCommand{
		PlayerId:         in.PlayerId,
		TargetNodeId:     uint32(targetNodeId),
		SessionId:        in.SessionId,
		Payload:          payload,
		TargetGateId:     uint32(targetGateId),
		TargetInstanceId: in.GateInstanceId,
		EventId:          uint32(game.ContractsKafkaRoutePlayerEventEventId),
	}
	bytes, err := proto.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal GateCommand: %w", err)
	}

	msg, err := GateCommandMessageFor(in.GateId, fmt.Sprintf("%d", in.PlayerId), bytes)
	if err != nil {
		return fmt.Errorf("address RoutePlayer command: %w", err)
	}
	// 不用可取消 context：kafka-go 在 context 返回后仍可能继续投递批次。
	// 真实 writer 由 MaxAttempts=1 + WriteTimeout 提供有界终态。
	if err := l.svcCtx.Kafka.WriteMessages(context.Background(), msg); err != nil {
		return fmt.Errorf("push to Kafka topic %s partition %d: %w", msg.Topic, msg.Partition, err)
	}

	l.Logger.Infof("Pushed RoutePlayer to Kafka %s/%d for player %d (gate %s) -> node %s home_zone=%d owner_epoch=%d",
		msg.Topic, msg.Partition, in.PlayerId, in.GateId, nodeId, homeZoneID, ownerEpoch)
	return nil
}
