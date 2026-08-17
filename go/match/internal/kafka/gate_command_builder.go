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
type gateCommandBuilder struct{}

// NewGateCommandBuilder returns a GateCommandBuilder for the match service.
func NewGateCommandBuilder() kafkautil.GateCommandBuilder {
	return &gateCommandBuilder{}
}

func (b *gateCommandBuilder) BuildPushCommand(sessionID uint32, gateInstanceID string,
	messageID uint32, body []byte,
) ([]byte, error) {
	mc := &basepb.MessageContent{
		MessageId:         messageID,
		SerializedMessage: body,
	}
	event := &kafkapb.PushToPlayerEvent{
		SessionId:      sessionID,
		MessageContent: mc,
	}
	payload, err := proto.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("marshal PushToPlayerEvent: %w", err)
	}

	cmd := &kafkapb.GateCommand{
		EventId:          uint32(game.ContractsKafkaPushToPlayerEventEventId),
		TargetInstanceId: gateInstanceID,
		Payload:          payload,
	}
	return proto.Marshal(cmd)
}

func (b *gateCommandBuilder) BuildBroadcastCommand(sessionList []uint32, gateInstanceID string,
	messageID uint32, body []byte,
) ([]byte, error) {
	mc := &basepb.MessageContent{
		MessageId:         messageID,
		SerializedMessage: body,
	}
	event := &kafkapb.BroadcastToPlayersEvent{
		MessageContent: mc,
	}

	if base, bm, ok := kafkautil.EncodeBitmapFields(sessionList); ok {
		event.SessionBitmapBase = base
		event.SessionBitmap = bm
	} else {
		event.SessionList = sessionList
	}

	payload, err := proto.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("marshal BroadcastToPlayersEvent: %w", err)
	}

	cmd := &kafkapb.GateCommand{
		EventId:          uint32(game.ContractsKafkaBroadcastToPlayersEventEventId),
		TargetInstanceId: gateInstanceID,
		Payload:          payload,
	}
	return proto.Marshal(cmd)
}

func (b *gateCommandBuilder) BuildBroadcastToSceneCommand(sceneID uint64, gateInstanceID string,
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
		TargetInstanceId: gateInstanceID,
		Payload:          payload,
	}
	return proto.Marshal(cmd)
}

func (b *gateCommandBuilder) BuildBroadcastToAllCommand(gateInstanceID string,
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
		TargetInstanceId: gateInstanceID,
		Payload:          payload,
	}
	return proto.Marshal(cmd)
}
