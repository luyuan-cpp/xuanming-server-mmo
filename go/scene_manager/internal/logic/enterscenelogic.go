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

	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

type EnterSceneLogic struct {
	ctx               context.Context
	svcCtx            *svc.ServiceContext
	assignGateForZone func(context.Context, *svc.ServiceContext, uint32) (*scene_manager.RedirectToGateInfo, error)
	logx.Logger
}

const (
	enterSceneDedupeTTLSeconds    = 60
	enterSceneDedupeDonePrefix    = "done:"
	enterSceneDedupePendingPrefix = "pending:"
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

const luaRestorePlayerLocationIfValue = `
if redis.call("GET", KEYS[1]) ~= ARGV[1] then
    return 0
end
if ARGV[2] == "" then
    redis.call("DEL", KEYS[1])
else
    redis.call("SET", KEYS[1], ARGV[2])
end
return 1
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
	if targetZoneId == 0 {
		if locErr == nil && currentLoc != nil && currentLoc.ZoneId != 0 {
			targetZoneId = currentLoc.ZoneId
		}
	}
	if targetZoneId == 0 {
		targetZoneId = in.GateZoneId
	}

	// 2. Resolve the target scene (sceneId + nodeId).
	resolveStart := time.Now()
	sceneId, nodeId, reserved, err := l.resolveSceneForEnter(in.SceneId, in.SceneConfId, targetZoneId)
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

	// 在任何旧场景扣减、ReleasePlayer、位置写入或 Gate 路由之前关闭不安全的
	// 交接窗口。当前 ReleasePlayer 只保证存盘任务入队，并不保证旧节点的数据
	// 已持久化；因此跨节点进入新场景会读到陈旧快照。已有位置记录的跨区重定向
	// 同样会让后续请求落到另一节点，不能继续假装重定向成功。
	crossZoneRedirect := in.GateZoneId != 0 && targetZoneId != 0 && in.GateZoneId != targetZoneId
	currentZoneID := uint32(0)
	if currentLoc != nil {
		currentZoneID = currentLoc.ZoneId
		if currentZoneID == 0 && currentLoc.SceneId != 0 {
			currentZoneID = GetSceneZone(l.svcCtx, currentLoc.SceneId)
		}
	}
	// NodeId 只在 zone 内唯一，不能只比较字符串。旧 zone 的 node "10" 与目标
	// zone 的 node "10" 仍是两个进程；若把它当同节点，会跳过 ReleasePlayer 并
	// 在没有落盘屏障时直接覆盖 location。仅以下两类能证明无需跨节点交接：
	//   1. scene_id/node_id 完全相同，且两边 zone 明确相同；
	//   2. 两边 zone 明确相同，且 node_id 相同的同节点切场景。
	// zone=0 的旧位置无法证明物理节点身份，按 fail-closed 处理，不回退到
	// 只比较 node_id 的歧义旧语义。
	samePlacement := currentLoc != nil && currentLoc.SceneId == sceneId && currentLoc.NodeId == nodeId &&
		currentZoneID != 0 && targetZoneId != 0 && currentZoneID == targetZoneId
	samePhysicalNode := currentLoc != nil && currentLoc.NodeId != "" && currentLoc.NodeId == nodeId &&
		currentZoneID != 0 && targetZoneId != 0 && currentZoneID == targetZoneId
	crossNodeHandoff := currentLoc != nil && !samePlacement && !samePhysicalNode
	existingLocationCrossZone := currentLoc != nil && crossZoneRedirect
	if !l.svcCtx.Config.AllowUnsafeCrossNodeHandoff && (crossNodeHandoff || existingLocationCrossZone) {
		// 自动选择大世界频道会在 resolveSceneForEnter 内原子预占人数；拒绝
		// 请求时必须成对释放，不能让失败请求污染调度负载。
		if reserved {
			DecrInstancePlayerCount(l.svcCtx, targetZoneId, sceneId)
		}
		metrics.ObserveEnterSceneRejected(targetZoneId, "unsafe_handoff")
		l.Logger.Errorf("拒绝不安全的场景交接: player=%d old_scene=%d old_node=%s old_zone=%d target_scene=%d target_node=%s target_zone=%d cross_zone=%t",
			in.PlayerId, currentLoc.GetSceneId(), currentLoc.GetNodeId(), currentLoc.GetZoneId(),
			sceneId, nodeId, targetZoneId, crossZoneRedirect)
		return errResp(constants.ErrUnsafeCrossNodeHandoff,
			"跨节点或已有位置的跨区切换缺少持久化交接屏障，已拒绝且未修改玩家状态"), nil
	}

	// 3. CROSS-ZONE CHECK: If gate is in a different zone, redirect the player.
	if crossZoneRedirect {
		crossStart := time.Now()
		// The reserve in step 2 may have already INCR'd the target scene's
		// counters; release them before bailing out so the redirect path
		// doesn't leak. The follow-up EnterScene from the new gate will
		// re-reserve from scratch.
		if reserved {
			DecrInstancePlayerCount(l.svcCtx, targetZoneId, sceneId)
		}
		// Redirect 只通知客户端换 Gate，并没有提交新的 PlayerLocation；旧场景
		// 人数必须保留到后续 EnterScene 真正完成，不能在这里提前扣减。这样
		// broker 失败也天然没有旧场景人数需要补偿。
		resp, redirErr := l.handleCrossZoneRedirect(in, targetZoneId)
		metrics.ObserveEnterSceneStage(targetZoneId, metrics.EnterSceneStageCrossZone, time.Since(crossStart))
		return resp, redirErr
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
				if err := l.routePlayerToGate(in, nodeId, sceneId); err != nil {
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
	// ⚠ 此分支在默认配置下不可达：上面的安全门禁已拒绝跨节点交接。只有
	// 开发环境显式打开 AllowUnsafeCrossNodeHandoff 才会进入旧流程；此时
	// release 与新节点 load 之间仍没有持久化屏障，新节点可能读到旧节点尚未
	// 落盘的 stale 状态。同步等待也堵不住：C++ ReleasePlayer RPC 只保证
	// HandleExitGameNode **入队**了存盘，不保证已落盘。
	//
	// 注意:db 写管道的 kafka key / applied-seq 只约束 db 服务自己消费到的
	// 分表写入顺序；它既不等待旧 Scene 的 PlayerAllData Redis 写完成，也不会
	// 重写新 Scene 实际加载的同一份 key，因此**不是**交接屏障或本窗口的兜底。
	// 若未来要重新开放生产跨节点切换，仍需 per-player 交接 epoch（旧节点
	// 落盘后写标记，新节点 load 前校验，未就绪则有界退避）的跨管道协议。
	if currentLoc != nil && currentLoc.NodeId != "" && !samePhysicalNode {
		relStart := time.Now()
		dispatchReleasePlayer(l.svcCtx, l.Logger, currentZoneID,
			currentLoc.NodeId, in.PlayerId, sceneId, nodeId)
		metrics.ObserveEnterSceneStage(targetZoneId, metrics.EnterSceneStageReleaseDispatch, time.Since(relStart))
	}

	// 6. Update Player Location (Source of Truth)
	updStart := time.Now()
	newLocRaw, updateErr := updatePlayerLocationWithRaw(l.svcCtx, in.PlayerId, sceneId, nodeId, targetZoneId)
	if updateErr != nil {
		metrics.ObserveEnterSceneStage(targetZoneId, metrics.EnterSceneStageUpdateLoc, time.Since(updStart))
		DecrInstancePlayerCount(l.svcCtx, targetZoneId, sceneId)
		l.Logger.Errorf("Failed to update player location: %v", updateErr)
		return errResp(constants.ErrUpdateLocation, "Failed to update location"), nil
	}
	metrics.ObserveEnterSceneStage(targetZoneId, metrics.EnterSceneStageUpdateLoc, time.Since(updStart))

	// 6b. Decrement old scene instance count after the new reservation is
	// committed so concurrent world-channel selection sees the in-flight enter.
	if currentLoc != nil && currentLoc.SceneId != 0 && currentLoc.SceneId != sceneId {
		DecrInstancePlayerCount(l.svcCtx, currentZoneID, currentLoc.SceneId)
	}

	// 7. Send Route Command to Gate. Broker ACK is the transaction commit
	// point: only after it succeeds may the response be cached as successful.
	if in.GateId != "" {
		routeStart := time.Now()
		if err := l.routePlayerToGate(in, nodeId, sceneId); err != nil {
			l.Logger.Errorf("Failed to route player %d to gate, rolling back location/counts: %v", in.PlayerId, err)
			l.rollbackEnterSceneAfterRouteFailure(in.PlayerId, currentLoc, currentLocRaw, newLocRaw,
				currentZoneID, targetZoneId, sceneId)
			metrics.ObserveEnterSceneStage(targetZoneId, metrics.EnterSceneStageRouteGate, time.Since(routeStart))
			return errResp(constants.ErrKafkaRoute, fmt.Sprintf("route player to gate failed: %v", err)), nil
		}
		metrics.ObserveEnterSceneStage(targetZoneId, metrics.EnterSceneStageRouteGate, time.Since(routeStart))
	} else {
		l.Logger.Infof("No GateID in EnterScene request for player %d", in.PlayerId)
	}

	l.Logger.Infof("Player %d entered scene %d on node %s (zone %d)", in.PlayerId, sceneId, nodeId, targetZoneId)
	return &scene_manager.EnterSceneResponse{ErrorCode: 0}, nil
}

// handleCrossZoneRedirect assigns a Gate in the target zone and returns redirect info.
// It also sends a RedirectToGateEvent to the current Gate via Kafka so the Gate can
// push the redirect to the client immediately.
func (l *EnterSceneLogic) handleCrossZoneRedirect(in *scene_manager.EnterSceneRequest, targetZoneId uint32) (*scene_manager.EnterSceneResponse, error) {
	l.Logger.Infof("Cross-zone detected: player %d gate_zone=%d target_zone=%d, redirecting",
		in.PlayerId, in.GateZoneId, targetZoneId)

	redirect, err := l.assignGateForZone(l.ctx, l.svcCtx, targetZoneId)
	if err != nil {
		l.Logger.Errorf("Cross-zone redirect failed for player %d: %v", in.PlayerId, err)
		return errResp(constants.ErrNoAvailableNode, "no gate available in target zone"), nil
	}

	// Redirect token 只有 broker ACK 后才能作为成功响应缓存，否则相同
	// request_id 会永久重放一份客户端从未收到的假成功。
	if in.GateId != "" {
		if err := l.sendRedirectToGate(in, redirect); err != nil {
			l.Logger.Errorf("Failed to push redirect to gate: %v", err)
			return errResp(constants.ErrKafkaRoute, fmt.Sprintf("redirect to gate failed: %v", err)), nil
		}
	}

	return &scene_manager.EnterSceneResponse{
		ErrorCode: 0,
		Redirect:  redirect,
	}, nil
}

// rollbackEnterSceneAfterRouteFailure restores the state visible before this
// request. Redis operations are best-effort because the route failure is
// already the primary error; every rollback failure is logged for repair.
func (l *EnterSceneLogic) rollbackEnterSceneAfterRouteFailure(playerID uint64, old *scene_manager.PlayerLocation, oldRaw, newRaw string, oldZoneID, newZoneID uint32, newSceneID uint64) {
	result, err := l.svcCtx.Redis.Eval(luaRestorePlayerLocationIfValue,
		[]string{getPlayerLocationKey(playerID)}, newRaw, oldRaw)
	if err != nil {
		l.Logger.Errorf("route 回滚玩家位置 CAS 失败: player=%d err=%v", playerID, err)
		return
	}
	if fmt.Sprint(result) != "1" {
		l.Logger.Errorf("route 回滚检测到玩家位置已被并发请求推进，跳过旧状态覆盖与人数回滚: player=%d", playerID)
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

	topic := GateTopicName(in.GateId)
	// 不用可取消 context：kafka-go 在 context 返回后仍可能继续投递批次。
	// 真实 writer 由 MaxAttempts=1 + WriteTimeout 提供有界终态。
	if err := l.svcCtx.Kafka.WriteMessages(context.Background(), kafkago.Message{
		Topic: topic,
		Key:   []byte(fmt.Sprintf("%d", in.PlayerId)),
		Value: bytes,
	}); err != nil {
		return fmt.Errorf("push to Kafka topic %s: %w", topic, err)
	}

	l.Logger.Infof("Pushed RedirectToGate to Kafka topic %s for player %d", topic, in.PlayerId)
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
func (l *EnterSceneLogic) routePlayerToGate(in *scene_manager.EnterSceneRequest, nodeId string, sceneId uint64) error {
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

	topic := GateTopicName(in.GateId)
	// 不用可取消 context：kafka-go 在 context 返回后仍可能继续投递批次。
	// 真实 writer 由 MaxAttempts=1 + WriteTimeout 提供有界终态。
	if err := l.svcCtx.Kafka.WriteMessages(context.Background(), kafkago.Message{
		Topic: topic,
		Key:   []byte(fmt.Sprintf("%d", in.PlayerId)),
		Value: bytes,
	}); err != nil {
		return fmt.Errorf("push to Kafka topic %s: %w", topic, err)
	}

	l.Logger.Infof("Pushed RoutePlayer to Kafka topic %s for player %d -> node %s", topic, in.PlayerId, nodeId)
	return nil
}
