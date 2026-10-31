package kafka

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	basepb "proto/common/base"
	kafkapb "proto/contracts/kafka"

	"match/generated/pb/game"
	"shared/kafkautil"
)

// gateCommandBuilder implements kafkautil.GateCommandBuilder using local proto imports.
// 与 friend/guild 的同名实现逐字相同,仅 import 的 per-service game 包不同。
//
// TargetGateId 必须填,理由见 go/friend/internal/kafka/gate_command_builder.go 的同名注释
// 与 docs/design/control-plane-topic-partitioning-20260908.md。
type gateCommandBuilder struct{}

// NewGateCommandBuilder returns a GateCommandBuilder for the match service.
func NewGateCommandBuilder() kafkautil.GateCommandBuilder {
	return &gateCommandBuilder{}
}

func (b *gateCommandBuilder) BuildPushCommand(gateNodeID uint32, sessionID uint32, playerID uint64, gateInstanceID string,
	messageID uint32, body []byte,
) ([]byte, error) {
	mc := &basepb.MessageContent{
		MessageId:         messageID,
		SerializedMessage: body,
	}
	// player_id 是 gate 的目标玩家身份栅栏,gate 只按 payload 投递;
	// 信封上的 PlayerId / SessionId 只供 broker 侧排障读取。
	event := &kafkapb.PushToPlayerEvent{
		SessionId:      sessionID,
		MessageContent: mc,
		PlayerId:       playerID,
	}
	payload, err := proto.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("marshal PushToPlayerEvent: %w", err)
	}

	cmd := &kafkapb.GateCommand{
		EventId:          uint32(game.ContractsKafkaPushToPlayerEventEventId),
		PlayerId:         playerID,
		SessionId:        sessionID,
		TargetGateId:     gateNodeID,
		TargetInstanceId: gateInstanceID,
		Payload:          payload,
	}
	return proto.Marshal(cmd)
}

func (b *gateCommandBuilder) BuildBroadcastCommand(gateNodeID uint32, targets []kafkautil.BroadcastTarget, gateInstanceID string,
	messageID uint32, body []byte,
) ([]byte, error) {
	mc := &basepb.MessageContent{
		MessageId:         messageID,
		SerializedMessage: body,
	}
	// 四个目标字段只由共享编码器产出:player_list 与 session 的对应全靠顺序契约,不能另写一份。
	sessionList, bitmapBase, bitmap, playerList := kafkautil.EncodeBroadcastTargets(targets)
	event := &kafkapb.BroadcastToPlayersEvent{
		SessionList:       sessionList,
		MessageContent:    mc,
		SessionBitmapBase: bitmapBase,
		SessionBitmap:     bitmap,
		PlayerList:        playerList,
	}

	payload, err := proto.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("marshal BroadcastToPlayersEvent: %w", err)
	}

	cmd := &kafkapb.GateCommand{
		EventId:          uint32(game.ContractsKafkaBroadcastToPlayersEventEventId),
		TargetGateId:     gateNodeID,
		TargetInstanceId: gateInstanceID,
		Payload:          payload,
	}
	return proto.Marshal(cmd)
}

func (b *gateCommandBuilder) BuildBroadcastToSceneCommand(gateNodeID uint32, sceneID uint64, gateInstanceID string,
	messageID uint32, body []byte,
) ([]byte, error) {
	mc := &basepb.MessageContent{
		MessageId:         messageID,
		SerializedMessage: body,
	}
	event := &kafkapb.BroadcastToSceneEvent{
		SceneId:        sceneID,
		MessageContent: mc,
	}
	payload, err := proto.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("marshal BroadcastToSceneEvent: %w", err)
	}

	cmd := &kafkapb.GateCommand{
		EventId:          uint32(game.ContractsKafkaBroadcastToSceneEventEventId),
		TargetGateId:     gateNodeID,
		TargetInstanceId: gateInstanceID,
		Payload:          payload,
	}
	return proto.Marshal(cmd)
}

func (b *gateCommandBuilder) BuildBroadcastToAllCommand(gateNodeID uint32, gateInstanceID string,
	messageID uint32, body []byte,
) ([]byte, error) {
	mc := &basepb.MessageContent{
		MessageId:         messageID,
		SerializedMessage: body,
	}
	event := &kafkapb.BroadcastToAllEvent{
		MessageContent: mc,
	}
	payload, err := proto.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("marshal BroadcastToAllEvent: %w", err)
	}

	cmd := &kafkapb.GateCommand{
		EventId:          uint32(game.ContractsKafkaBroadcastToAllEventEventId),
		TargetGateId:     gateNodeID,
		TargetInstanceId: gateInstanceID,
		Payload:          payload,
	}
	return proto.Marshal(cmd)
}
