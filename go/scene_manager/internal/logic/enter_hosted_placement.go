package logic

import (
	"context"
	"fmt"
	"strconv"

	event "proto/common/event"
	kafkacontracts "proto/contracts/kafka"
	"proto/scene_manager"
	game "scene_manager/generated/pb/game"
	"scene_manager/internal/constants"
	"scene_manager/internal/metrics"
	"shared/kafkacmd"

	"google.golang.org/protobuf/proto"
)

// 托管落点(docs/design/player-connection-lost-hold.md §4.2、team-follow-server-trail.md §3.7):
// 离线托管中的角色(断线保留 + 挂机月卡有效,客户端已完全关掉)跟着队长换场景。请求由 scene 节点替他发,
// EnterSceneRequest.hosted_placement 置位、不带 gate。与普通换图走同一条 EnterScene:身份裁决、换手门、铸造、CAS 落点、
// 第 7b 步的 ReleasePlayer 都不变,只有两处不同 ——
//
//   - 提交点:没有 gate 可推路由,改为直接给目标 scene 节点发 PlayerHostedPlacementEvent(SceneCommand)。broker ACK 是提交点,
//     失败按推 gate 路由失败同一套回滚(落点退回、epoch 只进不退、人数还回去),scene 端收到 7 走既有的取证路径;
//   - 3b 归属查询照常做:目标节点建实体要 home_zone,与 gate 路由下发的是同一个值。
//
// 不支持跨 zone(没有 gate 可重定向,第二条腿也没有客户端去连),预检拒绝。

// 托管落点的拒绝原因(enter_scene_rejected_total 的 reason,固定字符串、低基数)。
const (
	// 请求形状不对:带了 gate、缺登录身份。按构造应恒 0(只有 scene 节点替托管角色发)。
	rejectReasonHostedPlacementMalformed = "hosted_placement_malformed"
	// 目标在别的 zone。scene 只在队长与自己同 zone 时才发,出现说明两边对 zone 的判断不一致。
	rejectReasonHostedPlacementCrossZone = "hosted_placement_cross_zone"
	// 本副本的节点镜像里拿不到目标节点唯一的实例号(未同步完、身份歧义、旧版注册不带 UUID):投递不了防僵尸的事件。
	rejectReasonHostedPlacementNoInstance = "hosted_placement_no_instance"
)

// rejectMalformedHostedPlacement 在任何去重、预占与落点写之前判托管落点的请求形状。返回非 nil 即拒绝,一个字节都没改。
// 不是托管落点时返回 nil。
func (l *EnterSceneLogic) rejectMalformedHostedPlacement(in *scene_manager.EnterSceneRequest) *scene_manager.EnterSceneResponse {
	if !in.HostedPlacement {
		return nil
	}
	reason := ""
	switch {
	case in.GateId != "" || in.GateInstanceId != "":
		// 带 gate 的请求有在线会话,走 gate 路由;两种提交点不能同时出现,否则目标节点会被派两次入场。
		reason = "hosted placement must not carry a gate"
	case in.SessionId == 0 || in.LoginGeneration == 0:
		// 断开的那次登录是角色在目标节点上的登录身份(角色登记要求非 0,断线保留与接回判据靠它),没有就建不出托管状态。
		reason = "hosted placement requires the login identity of the lost session"
	}
	if reason != "" {
		metrics.ObserveEnterSceneRejected(l.svcCtx.Config.ZoneId, rejectReasonHostedPlacementMalformed)
		l.Logger.Errorf("[HostedPlacement] malformed request rejected before any write: player=%d gate=%q gate_instance=%q session=%d login_generation=%d reason=%s",
			in.PlayerId, in.GateId, in.GateInstanceId, in.SessionId, in.LoginGeneration, reason)
		return errResp(constants.ErrInvalidGateID, reason)
	}
	if l.svcCtx.Kafka == nil {
		return errResp(constants.ErrKafkaRoute, "Kafka route writer is unavailable")
	}
	return nil
}

// rejectHostedPlacementAcrossZones 是跨 zone 分支入口处的拒绝:此刻还没有任何预占与写入。
func (l *EnterSceneLogic) rejectHostedPlacementAcrossZones(in *scene_manager.EnterSceneRequest, targetZoneID uint32) *scene_manager.EnterSceneResponse {
	metrics.ObserveEnterSceneRejected(targetZoneID, rejectReasonHostedPlacementCrossZone)
	l.Logger.Infof("[HostedPlacement] cross-zone hosted placement rejected before any write: player=%d gate_zone=%d target_zone=%d scene=%d corr=%d",
		in.PlayerId, in.GateZoneId, targetZoneID, in.SceneId, in.CorrelationId)
	return errResp(constants.ErrInvalidGateID, fmt.Sprintf("hosted placement cannot leave zone %d", in.GateZoneId))
}

// resolveHostedPlacementInstance 取目标 scene 节点在本副本镜像里唯一一条注册的实例号:托管落点事件的 target_instance_id
// (AGENTS §7 #2,节点号会被重启后的新进程复用,只有实例号分得清两代)。排在任何落点写之前;拿不到时返回 4
// (ErrInvalidNodeID,C++ 侧 SmRejectedBeforeAnyPlacementWrite 认这个码),调用方释放预占后原样返回。
func (l *EnterSceneLogic) resolveHostedPlacementInstance(in *scene_manager.EnterSceneRequest, zoneID uint32, nodeID string) (string, *scene_manager.EnterSceneResponse) {
	if _, err := strconv.ParseUint(nodeID, 10, 32); err != nil {
		return "", errResp(constants.ErrInvalidNodeID, fmt.Sprintf("invalid scene node id %q: %v", nodeID, err))
	}
	instance, ok := knownNodeRegisteredInstance(zoneID, nodeID)
	if !ok {
		metrics.ObserveEnterSceneRejected(zoneID, rejectReasonHostedPlacementNoInstance)
		l.Logger.Infof("[HostedPlacement] target scene node has no unique registered instance in this replica's mirror; rejected before any write (retryable): player=%d zone=%d node=%s scene=%d",
			in.PlayerId, zoneID, nodeID, in.SceneId)
		return "", errResp(constants.ErrInvalidNodeID, fmt.Sprintf("scene node %s has no unique registered instance", nodeID))
	}
	return instance, nil
}

// routePlacement 发本次落点的路由:带 gate 的推 RoutePlayerEvent 给 gate,托管落点直接推 PlayerHostedPlacementEvent 给目标
// scene 节点。两者都以 broker ACK 为提交点,调用方对两种失败做同一种处置(第 4 步只回 7,第 7 步回滚后回 7)。
// 调用方保证 in.GateId != "" 或 in.HostedPlacement(两者互斥,rejectMalformedHostedPlacement 已判)。
func (l *EnterSceneLogic) routePlacement(in *scene_manager.EnterSceneRequest, nodeID, hostedInstance string, sceneID uint64,
	homeZoneID uint32, ownerEpoch uint64) error {
	if in.HostedPlacement {
		return l.routeHostedPlayerToScene(in, nodeID, hostedInstance, sceneID, homeZoneID, ownerEpoch)
	}
	return l.routePlayerToGate(in, nodeID, sceneID, homeZoneID, ownerEpoch)
}

// routeHostedPlayerToScene 是 routePlayerToGate 在托管落点上的等价物。字段与 RoutePlayerEvent 一一对应,
// 会话号与登录代次是断开的那次登录(目标节点只记作登录身份,不绑定)。
// key = player_id(同一玩家保序,AGENTS §7 #3);topic / 分区由 kafkacmd 唯一入口按目标节点号算。
func (l *EnterSceneLogic) routeHostedPlayerToScene(in *scene_manager.EnterSceneRequest, nodeID, instanceID string, sceneID uint64,
	homeZoneID uint32, ownerEpoch uint64) error {
	targetNodeID, err := strconv.ParseUint(nodeID, 10, 32)
	if err != nil {
		return fmt.Errorf("invalid scene node id %q: %w", nodeID, err)
	}
	if instanceID == "" {
		// 调用方在落点写之前已经取到实例号;走到这里为空是调用方 bug,按投递失败回滚,不发一条关掉防僵尸过滤的命令。
		return fmt.Errorf("hosted placement to scene node %s without a target instance id", nodeID)
	}
	payload, err := proto.Marshal(&event.PlayerHostedPlacementEvent{
		PlayerId:        in.PlayerId,
		SceneId:         sceneID,
		HomeZoneId:      homeZoneID,
		OwnerEpoch:      ownerEpoch,
		SessionId:       in.SessionId,
		LoginGeneration: in.LoginGeneration,
		OwnerRevision:   in.OwnerRevision,
	})
	if err != nil {
		return fmt.Errorf("marshal PlayerHostedPlacementEvent: %w", err)
	}
	value, err := proto.Marshal(&kafkacontracts.SceneCommand{
		CommandType:      kafkacontracts.SceneCommand_DispatchEvent,
		PlayerId:         in.PlayerId,
		TargetSceneId:    uint32(targetNodeID),
		Payload:          payload,
		TargetInstanceId: instanceID,
		EventId:          proto.Uint32(uint32(game.PlayerHostedPlacementEventEventId)),
	})
	if err != nil {
		return fmt.Errorf("marshal SceneCommand: %w", err)
	}
	msg, err := kafkacmd.SceneCommandMessage(nodeID, strconv.FormatUint(in.PlayerId, 10), value)
	if err != nil {
		return fmt.Errorf("address hosted placement command: %w", err)
	}
	// 与 routePlayerToGate 同理不用可取消的 context:kafka-go 在 context 返回后仍可能继续投递批次,
	// 真实 writer 由 MaxAttempts=1 + WriteTimeout 给出有界终态。
	if err := l.svcCtx.Kafka.WriteMessages(context.Background(), msg); err != nil {
		return fmt.Errorf("push hosted placement to Kafka topic %s partition %d: %w", msg.Topic, msg.Partition, err)
	}
	l.Logger.Infof("[HostedPlacement] pushed hosted placement to Kafka %s/%d for player %d -> node %s instance %s scene %d home_zone=%d owner_epoch=%d session=%d login_generation=%d",
		msg.Topic, msg.Partition, in.PlayerId, nodeID, instanceID, sceneID, homeZoneID, ownerEpoch, in.SessionId, in.LoginGeneration)
	return nil
}
