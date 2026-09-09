package kafka

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	basepb "proto/common/base"
	kafkapb "proto/contracts/kafka"

	"guild/generated/pb/game"
	"shared/kafkautil"
)

// gateCommandBuilder implements kafkautil.GateCommandBuilder using local proto imports.
//
// TargetGateId 必须填,理由见 go/friend/internal/kafka/gate_command_builder.go 的同名注释
// 与 docs/design/control-plane-topic-partitioning-20260908.md。
type gateCommandBuilder struct{}

// NewGateCommandBuilder returns a GateCommandBuilder for the guild service.
func NewGateCommandBuilder() kafkautil.GateCommandBuilder {
	return &gateCommandBuilder{}
}

func (b *gateCommandBuilder) BuildPushCommand(gateNodeID uint32, sessionID uint32, gateInstanceID string,
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
		TargetGateId:     gateNodeID,
		TargetInstanceId: gateInstanceID,
		Payload:          payload,
	}
	return proto.Marshal(cmd)
}

func (b *gateCommandBuilder) BuildBroadcastCommand(gateNodeID uint32, sessionList []uint32, gateInstanceID string,
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
