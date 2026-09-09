package kafkautil

import (
	"context"
	"fmt"
	"strconv"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"

	"shared/kafkacmd"
)

// PlayerGateInfo holds the minimum session info needed to route a push to the right Gate.
type PlayerGateInfo struct {
	PlayerID       uint64
	SessionID      uint32
	GateID         string
	GateInstanceID string
}

// GateCommandBuilder abstracts GateCommand + event payload construction.
// Each service provides its own implementation using its local proto imports.
//
// 每个方法都多收一个 gateNodeID:控制面命令改成"一个类型一个 topic + 按
// node_id % P 定分区"之后(docs/design/control-plane-topic-partitioning-20260908.md),
// 同一个分区上坐着几百个 gate,GateCommand.target_gate_id 就从"可填可不填的冗余"
// 变成了消费端的**第一级过滤**。留 0 等于把便宜的数字比对关掉,只剩每条消息一次
// 字符串比对(target_instance_id)。
type GateCommandBuilder interface {
	// BuildPushCommand builds a serialized GateCommand wrapping a PushToPlayerEvent.
	BuildPushCommand(gateNodeID uint32, sessionID uint32, gateInstanceID string,
		messageID uint32, body []byte) ([]byte, error)

	// BuildBroadcastCommand builds a serialized GateCommand wrapping a BroadcastToPlayersEvent.
	// When len(sessionList) >= BitmapThreshold and bitmap is smaller, bitmap encoding is used automatically.
	BuildBroadcastCommand(gateNodeID uint32, sessionList []uint32, gateInstanceID string,
		messageID uint32, body []byte) ([]byte, error)

	// BuildBroadcastToSceneCommand builds a serialized GateCommand wrapping a BroadcastToSceneEvent.
	BuildBroadcastToSceneCommand(gateNodeID uint32, sceneID uint64, gateInstanceID string,
		messageID uint32, body []byte) ([]byte, error)

	// BuildBroadcastToAllCommand builds a serialized GateCommand wrapping a BroadcastToAllEvent.
	BuildBroadcastToAllCommand(gateNodeID uint32, gateInstanceID string,
		messageID uint32, body []byte) ([]byte, error)
}

// PushToPlayer sends a single message to a player's client via Kafka -> Gate -> TCP.
func PushToPlayer(ctx context.Context, w *kafkago.Writer, builder GateCommandBuilder,
	info PlayerGateInfo, messageID uint32, body []byte,
) error {
	// 不变量 2(CLAUDE.md §7):发往 gate 的命令必须带目标实例 uuid,
	// 空值 = 消费端 ValidateCommandTarget 的防僵尸过滤被关闭 —— gate 业务 node_id
	// 会被回收复用,老 gate 未彻底退出时会把发给新 gate 的命令一并消费。
	// 这里是所有 Go 服务推送的唯一共享收口,必须 fail-closed。
	if info.GateInstanceID == "" {
		return fmt.Errorf("push to player %d rejected: empty gate_instance_id (gate=%s), anti-zombie filtering would be disabled",
			info.PlayerID, info.GateID)
	}
	// 拿不到数字节点号就没法算分区,必须 fail-closed:发到 0 号分区只会被
	// node_id 是 P 的倍数的那些 gate 读到再丢弃,等于静默丢消息。
	gateNodeID, err := kafkacmd.ParseNodeID(info.GateID)
	if err != nil {
		return fmt.Errorf("push to player %d rejected: %w", info.PlayerID, err)
	}
	cmdBytes, err := builder.BuildPushCommand(uint32(gateNodeID), info.SessionID, info.GateInstanceID, messageID, body)
	if err != nil {
		return fmt.Errorf("build push command: %w", err)
	}

	return w.WriteMessages(ctx, kafkago.Message{
		Topic:     kafkacmd.GateCommandTopic(),
		Partition: kafkacmd.GateCommandPartition(gateNodeID),
		Key:       []byte(strconv.FormatUint(info.PlayerID, 10)),
		Value:     cmdBytes,
	})
}

// BroadcastToPlayers groups players by (gate_id, gate_instance_id) and sends one
// Kafka message per group. The message body is serialized once per group.
//
// 分组键必须带 instance id:gate 业务 node_id 会被回收复用,滚动重启窗口内同一个
// gate_id 下可能同时存在新旧两代实例的会话。旧实现按 gate_id 分组并取**组内第一个
// 玩家**的 instance id —— 另一代实例的玩家被折进同一条命令,消费端
// ValidateCommandTarget 按 instance 过滤时那一半玩家的消息静默丢失。
func BroadcastToPlayers(ctx context.Context, w *kafkago.Writer, builder GateCommandBuilder,
	players []PlayerGateInfo, messageID uint32, body []byte,
) error {
	type gateGroup struct {
		gateID     string
		instanceID string
		sessions   []uint32
	}
	grouped := make(map[string]*gateGroup)
	var firstErr error
	for _, p := range players {
		// 不变量 2:空 instance id 的条目不允许出去(理由见 PushToPlayer)。
		// 单个坏条目不拖垮整批,但必须向调用方报错,不能静默降级。
		if p.GateInstanceID == "" {
			logx.Errorf("BroadcastToPlayers: player %d dropped: empty gate_instance_id (gate=%s)",
				p.PlayerID, p.GateID)
			if firstErr == nil {
				firstErr = fmt.Errorf("player %d has empty gate_instance_id", p.PlayerID)
			}
			continue
		}
		key := p.GateID + "\x00" + p.GateInstanceID
		g, ok := grouped[key]
		if !ok {
			g = &gateGroup{gateID: p.GateID, instanceID: p.GateInstanceID}
			grouped[key] = g
		}
		g.sessions = append(g.sessions, p.SessionID)
	}

	for _, g := range grouped {
		gateNodeID, err := kafkacmd.ParseNodeID(g.gateID)
		if err != nil {
			logx.Errorf("BroadcastToPlayers: gate %q dropped: %v", g.gateID, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		cmdBytes, err := builder.BuildBroadcastCommand(uint32(gateNodeID), g.sessions, g.instanceID, messageID, body)
		if err != nil {
			logx.Errorf("BroadcastToPlayers: build command for gate %s: %v", g.gateID, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		if err := w.WriteMessages(ctx, kafkago.Message{
			Topic:     kafkacmd.GateCommandTopic(),
			Partition: kafkacmd.GateCommandPartition(gateNodeID),
			Key:       []byte(g.gateID),
			Value:     cmdBytes,
		}); err != nil {
			logx.Errorf("BroadcastToPlayers: send to gate %s (%s/%d) failed: %v",
				g.gateID, kafkacmd.GateCommandTopic(), kafkacmd.GateCommandPartition(gateNodeID), err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

// GateInfo holds the minimum info needed to address a Gate node via Kafka.
type GateInfo struct {
	GateID         string
	GateInstanceID string
}

// BroadcastToScene sends a scene-wide broadcast to specific gates via Kafka.
// The caller provides the list of gates that have players in the scene.
func BroadcastToScene(ctx context.Context, w *kafkago.Writer, builder GateCommandBuilder,
	sceneID uint64, gates []GateInfo, messageID uint32, body []byte,
) error {
	var firstErr error
	for _, g := range gates {
		// 不变量 2:理由见 PushToPlayer。
		if g.GateInstanceID == "" {
			logx.Errorf("BroadcastToScene: gate-%s dropped: empty gate_instance_id", g.GateID)
			if firstErr == nil {
				firstErr = fmt.Errorf("gate %s has empty gate_instance_id", g.GateID)
			}
			continue
		}
		gateNodeID, err := kafkacmd.ParseNodeID(g.GateID)
		if err != nil {
			logx.Errorf("BroadcastToScene: gate %q dropped: %v", g.GateID, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		cmdBytes, err := builder.BuildBroadcastToSceneCommand(uint32(gateNodeID), sceneID, g.GateInstanceID, messageID, body)
		if err != nil {
			logx.Errorf("BroadcastToScene: build command for gate %s: %v", g.GateID, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		if err := w.WriteMessages(ctx, kafkago.Message{
			Topic:     kafkacmd.GateCommandTopic(),
			Partition: kafkacmd.GateCommandPartition(gateNodeID),
			Key:       []byte(g.GateID),
			Value:     cmdBytes,
		}); err != nil {
			logx.Errorf("BroadcastToScene: send to gate %s (%s/%d) failed: %v",
				g.GateID, kafkacmd.GateCommandTopic(), kafkacmd.GateCommandPartition(gateNodeID), err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// BroadcastToAll sends a server-wide broadcast to all provided gates via Kafka.
func BroadcastToAll(ctx context.Context, w *kafkago.Writer, builder GateCommandBuilder,
	gates []GateInfo, messageID uint32, body []byte,
) error {
	var firstErr error
	for _, g := range gates {
		// 不变量 2:理由见 PushToPlayer。
		if g.GateInstanceID == "" {
			logx.Errorf("BroadcastToAll: gate-%s dropped: empty gate_instance_id", g.GateID)
			if firstErr == nil {
				firstErr = fmt.Errorf("gate %s has empty gate_instance_id", g.GateID)
			}
			continue
		}
		gateNodeID, err := kafkacmd.ParseNodeID(g.GateID)
		if err != nil {
			logx.Errorf("BroadcastToAll: gate %q dropped: %v", g.GateID, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		cmdBytes, err := builder.BuildBroadcastToAllCommand(uint32(gateNodeID), g.GateInstanceID, messageID, body)
		if err != nil {
			logx.Errorf("BroadcastToAll: build command for gate %s: %v", g.GateID, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		if err := w.WriteMessages(ctx, kafkago.Message{
			Topic:     kafkacmd.GateCommandTopic(),
			Partition: kafkacmd.GateCommandPartition(gateNodeID),
			Key:       []byte(g.GateID),
			Value:     cmdBytes,
		}); err != nil {
			logx.Errorf("BroadcastToAll: send to gate %s (%s/%d) failed: %v",
				g.GateID, kafkacmd.GateCommandTopic(), kafkacmd.GateCommandPartition(gateNodeID), err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// BitmapThreshold is the minimum session count to consider bitmap encoding.
const BitmapThreshold = 32

// EncodeBitmapFields computes bitmap encoding for a session list.
// Returns (base, bitmap, useBitmap). When useBitmap is false, caller should use plain list.
func EncodeBitmapFields(sessions []uint32) (base uint32, bitmap []byte, useBitmap bool) {
	if len(sessions) < BitmapThreshold {
		return 0, nil, false
	}

	minID := sessions[0]
	maxID := sessions[0]
	for _, s := range sessions[1:] {
		if s < minID {
			minID = s
		}
		if s > maxID {
			maxID = s
		}
	}

	span := maxID - minID + 1
	bitmapBytes := (span + 7) / 8

	// Use bitmap when it is smaller than varint list (~3 bytes per uint32 varint)
	if int(bitmapBytes)+6 >= len(sessions)*3 {
		return 0, nil, false
	}

	bm := make([]byte, bitmapBytes)
	for _, s := range sessions {
		offset := s - minID
		bm[offset/8] |= 1 << (offset % 8)
	}
	return minID, bm, true
}
