package kafkautil

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"

	"shared/kafkacmd"
)

// PlayerGateInfo holds the minimum session info needed to route a push to the right Gate.
//
// PlayerID 必填:它随推送下发,是 gate 的目标玩家身份栅栏(见 PushToPlayer),
// 为 0 的条目会被 PushToPlayer / BroadcastToPlayers 拒绝。
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
	//
	// playerID 写进 PushToPlayerEvent.player_id(gate 投递前核对「会话当前玩家 == player_id」),
	// 信封 GateCommand 的 player_id / session_id 同时照填供 broker 侧排障。
	// 调用方保证 playerID 非 0(PushToPlayer 已 fail-closed 拒绝 0)。
	BuildPushCommand(gateNodeID uint32, sessionID uint32, playerID uint64, gateInstanceID string,
		messageID uint32, body []byte) ([]byte, error)

	// BuildBroadcastCommand builds a serialized GateCommand wrapping a BroadcastToPlayersEvent.
	//
	// 目标的四个字段(session_list / session_bitmap_base / session_bitmap / player_list)
	// 必须经 EncodeBroadcastTargets 编码,实现里不得另写一份:player_list 与 session 的对应
	// 全靠顺序契约,两份编码就是两份真相。
	BuildBroadcastCommand(gateNodeID uint32, targets []BroadcastTarget, gateInstanceID string,
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
	// 目标玩家身份栅栏(docs/design/routing-identity-audit-20260908.md R13):gate 写 socket 前
	// 核对「会话当前玩家 == player_id」。session_id 单独不足以定位玩家 —— 高位的 gate node_id
	// 会被立刻回收复用,低位序号在单个 gate 实例内也会回绕。player_id 为 0 在 gate 侧是
	// 「不校验」的兼容位,生产者发 0 等于亲手关掉栅栏,推送可能写进另一个玩家的连接。
	// 与下面的实例守卫同理,共享收口必须 fail-closed。
	if info.PlayerID == 0 {
		return fmt.Errorf("push to session %d rejected: zero player_id (gate=%s), gate identity fence would be disabled",
			info.SessionID, info.GateID)
	}
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
	cmdBytes, err := builder.BuildPushCommand(uint32(gateNodeID), info.SessionID, info.PlayerID, info.GateInstanceID, messageID, body)
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
//
// 每个目标都带着自己的 player_id 下发(gate 逐个核对身份栅栏,理由见 PushToPlayer);
// player_id 为 0 的条目逐个丢弃并报错,全部无效时一条消息都不写。
func BroadcastToPlayers(ctx context.Context, w *kafkago.Writer, builder GateCommandBuilder,
	players []PlayerGateInfo, messageID uint32, body []byte,
) error {
	type gateGroup struct {
		gateID     string
		instanceID string
		targets    []BroadcastTarget
	}
	grouped := make(map[string]*gateGroup)
	var firstErr error
	for _, p := range players {
		// 身份栅栏:player_id 为 0 的条目不允许出去(理由见 PushToPlayer),处置与下面的实例守卫相同。
		if p.PlayerID == 0 {
			logx.Errorf("BroadcastToPlayers: session %d dropped: zero player_id (gate=%s)",
				p.SessionID, p.GateID)
			if firstErr == nil {
				firstErr = fmt.Errorf("session %d on gate %s has zero player_id", p.SessionID, p.GateID)
			}
			continue
		}
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
		g.targets = append(g.targets, BroadcastTarget{SessionID: p.SessionID, PlayerID: p.PlayerID})
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
		cmdBytes, err := builder.BuildBroadcastCommand(uint32(gateNodeID), g.targets, g.instanceID, messageID, body)
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

// BroadcastTarget 是广播的一个投递目标:gate 上的会话,以及该会话此刻应当绑定的玩家。
// PlayerID 是 gate 身份栅栏的输入(理由见 PushToPlayer)。
type BroadcastTarget struct {
	SessionID uint32
	PlayerID  uint64
}

// BitmapThreshold is the minimum (deduplicated) target count to consider bitmap encoding.
const BitmapThreshold = 32

// EncodeBroadcastTargets 把一组广播目标编成 BroadcastToPlayersEvent 的四个目标字段。
//
// 与 C++ 的 cpp/libs/engine/core/network/broadcast_target_codec.h 是同一份契约(gate 的 RPC
// 与 Kafka 两条入口都按它的 ForEach 解码),逐字段对拍见 testdata/broadcast_target_vectors.json:
//   - 顺序:按 SessionID 稳定升序;同一 session 出现多次只保留输入里的第一个。去重是对齐的前提:
//     bitmap 对同一 session 只置一位,player_list 却会多一项,之后每一项都错位。被丢弃的重复项
//     收不到这条广播 —— 同一 gate 实例上一个 session 只属于一个玩家,重复只会来自过期的会话记录。
//   - 形态:去重后目标数 ≥ BitmapThreshold,且 bitmap 字节数 + 6 < 目标数 × 3(每个 session_id
//     按约 3 字节 varint 估算)时用 bitmap,否则用 session_list。bitmap 的第 offset 位(第 offset/8
//     字节的第 offset%8 位,字节内低位在前)置位表示 session = bitmapBase + offset。
//   - player_list[i] 对应「按 session_id 升序」的第 i 个目标,两种形态完全相同。
//
// 两种形态二选一:用 bitmap 时 sessionList 为 nil;用 list 时 bitmapBase 为 0、bitmap 为 nil。
// 不修改 targets;空输入返回全零值。纯函数,可并发调用。
func EncodeBroadcastTargets(targets []BroadcastTarget) (sessionList []uint32, bitmapBase uint32, bitmap []byte, playerList []uint64) {
	if len(targets) == 0 {
		return nil, 0, nil, nil
	}
	ordered := slices.Clone(targets)
	slices.SortStableFunc(ordered, func(a, b BroadcastTarget) int {
		return cmp.Compare(a.SessionID, b.SessionID)
	})
	// 稳定排序后同一 session 的各项相邻且保持输入顺序,CompactFunc 保留每段的第一个。
	ordered = slices.CompactFunc(ordered, func(a, b BroadcastTarget) bool {
		return a.SessionID == b.SessionID
	})

	playerList = make([]uint64, len(ordered))
	for i, target := range ordered {
		playerList[i] = target.PlayerID
	}

	// 选哪种形态只看 session 部分:player_list 两种形态一模一样,换编码省不掉它。
	if len(ordered) >= BitmapThreshold {
		minID := ordered[0].SessionID
		maxID := ordered[len(ordered)-1].SessionID
		// 跨度按 uint64 算:uint32 下 [0, MaxUint32] 的跨度会回绕成 0,算出一个装不下目标的空 bitmap。
		span := uint64(maxID) - uint64(minID) + 1
		bitmapBytes := (span + 7) / 8
		if bitmapBytes+6 < uint64(len(ordered))*3 {
			bitmap = make([]byte, bitmapBytes)
			for _, target := range ordered {
				offset := target.SessionID - minID
				bitmap[offset/8] |= 1 << (offset % 8)
			}
			return nil, minID, bitmap, playerList
		}
	}

	sessionList = make([]uint32, len(ordered))
	for i, target := range ordered {
		sessionList[i] = target.SessionID
	}
	return sessionList, 0, nil, playerList
}
