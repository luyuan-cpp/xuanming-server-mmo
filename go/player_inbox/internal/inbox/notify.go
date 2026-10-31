package inbox

import (
	"context"
	"errors"
	"strconv"

	"player_inbox/internal/metrics"

	event "proto/common/event"
	kafkapb "proto/contracts/kafka"
	inboxpb "proto/player_inbox"
	smpb "proto/scene_manager"

	"shared/kafkacmd"
	"shared/redisslot"
	"shared/scenenode"

	"github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 在线提醒(设计 §6.3)。写法照 go/match/internal/team/notify.go 的 refreshScene:
// 读 player:{id}:location → 节点镜像查 node_id 与 uuid → SceneCommand{DispatchEvent, PlayerInboxNotifyEvent}
// 写 scene-cmd_g<N>(kafkacmd 唯一寻址入口),key = player_id,target_instance_id = uuid(AGENTS §7 #2 #3)。
//
// **只是提醒**:Kafka 从末尾消费、玩家不在本节点就丢,所以是「至多一次」。兜底有两层 ——
//   - 落地拉取:目标节点若在入箱提交之前已经落地,location 已指向它,提交之后读 location 必然投到它;
//     落地若发生在提交之后,落地时的 Fetch 一定看得见这一条;
//   - 在线有界兜底(scene 侧默认 30 分钟)。

// NotifyResult 是一次提醒的结局。Settled 为真的四种可以清 notify_due_ms:送达、离线、等待跨 zone 落点、
// 位置记录残缺(后三者交给落地拉取:下一次落地一定在本次提交之后,落地 Fetch 看得见);
// 其余留给补发循环,按退避重试(评审 L6,见 Store.ResendNotify);其中 NotifyNodeMissing 悬满
// notifyNodeMissingSettleAfter 之后由补发落定(notifyGivesUp)。
type NotifyResult uint8

const (
	// NotifyError:读 location / 序列化 / 写 Kafka 失败,或提醒器未装配。留给补发。
	NotifyError NotifyResult = iota
	// NotifySent:SceneCommand 已写进 Kafka。
	NotifySent
	// NotifyOffline:没有 location(玩家离线,或还没进过场景)。视为成功:下次落地会拉。
	NotifyOffline
	// NotifyAwaitingPlacement:跨 zone 交接已放行、目标 zone 还没落点。视为成功:目标落地会无条件拉。
	NotifyAwaitingPlacement
	// NotifyNoNode:不可恢复、视为落定(评审 L6):location 既无节点也无 epoch(位置记录残缺)。再怎么重试也送不到,
	// 留着只会一直把积压告警顶亮;交给落地拉取与 scene 侧的在线有界兜底(§7.3)。
	// 「镜像里查不到节点」不归这一类(评审 R3),见 NotifyNodeMissing。
	NotifyNoNode
	// NotifyEmptyUUID:节点注册里没有 uuid,不能填 target_instance_id(防僵尸契约,fail-closed 不发)。留给补发。
	NotifyEmptyUUID
	// NotifyNodesUnsynced:scene 节点镜像还没完成首次全量同步(启动窗口里的正常现象)。留给补发。
	NotifyNodesUnsynced
	// NotifyNodeAmbiguous:同一 (zone, node) 身份在镜像里有多条注册(租约 / 部署分叉,scenenode 已打 ERROR),
	// 旧租约过期后自愈。留给补发。
	NotifyNodeAmbiguous
	// NotifyNodeMissing:镜像已完成首次同步,却查不到 location 指向的节点(评审 R3)。**不能**断定节点已下线:
	// scenenode.Watcher 在 watch 断开重连期间仍报 Synced()、镜像停在上一版;新节点注册到镜像收到 PUT 之间有延迟;
	// etcd 租约抖动会先 DELETE 再重新注册,而节点上的玩家一直在线。所以留给补发、按退避重试;
	// 悬满 notifyNodeMissingSettleAfter 仍查不到才由补发落定(那时只剩「location 停在一个真没了的节点上」)。
	NotifyNodeMissing
)

// Settled 报告这次提醒是否可以立即清 notify_due_ms(入箱后那次提醒与补发都按它)。
// NotifyNodeMissing 不在其中:它要悬满一段时间才落定,由补发按 notifyGivesUp 判。
func (r NotifyResult) Settled() bool {
	return r == NotifySent || r == NotifyOffline || r == NotifyAwaitingPlacement || r == NotifyNoNode
}

// String 是 player_inbox_notify_total{result} 的标签。
func (r NotifyResult) String() string {
	switch r {
	case NotifySent:
		return "sent"
	case NotifyOffline:
		return "offline"
	case NotifyAwaitingPlacement:
		return "awaiting_placement"
	case NotifyNoNode:
		return "no_node"
	case NotifyEmptyUUID:
		return "empty_uuid"
	case NotifyNodesUnsynced:
		return "nodes_unsynced"
	case NotifyNodeAmbiguous:
		return "node_ambiguous"
	case NotifyNodeMissing:
		return "node_missing"
	default:
		return "error"
	}
}

// Notifier 发一次在线提醒。实现必须尊重 ctx 并且不 panic;结局只用于决定是否清 notify_due_ms。
type Notifier interface {
	Notify(ctx context.Context, playerID uint64, domain inboxpb.PlayerInboxDomain) NotifyResult
}

// NodeDirectory 是 scene 节点镜像(scenenode.Watcher)的最小接口:单测可塞假实现。
type NodeDirectory interface {
	Synced() bool
	EntryOf(zoneID uint32, nodeID string) (scenenode.NodeEntry, error)
}

// MessageWriter 是 kafka.Writer 的最小接口。
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// KafkaNotifier 是生产用的 Notifier。构造后只读,可并发使用。
type KafkaNotifier struct {
	// Locations 读核心 Redis 上的 player:{id}:location;键不存在必须返回 (nil, nil)。
	Locations scenenode.LocationReader
	// Nodes 是 SceneNodeService.rpc/ 前缀的节点镜像。
	Nodes NodeDirectory
	// Writer 写 Kafka;必须配 kafkacmd.CommandPartitionBalancer(控制面命令按显式分区发)。
	Writer MessageWriter
	// EventID 是 PlayerInboxNotifyEvent 的全局事件号(生成在 player_inbox/generated/pb/game)。
	EventID uint32
}

var errNotifierIncomplete = errors.New("inbox: KafkaNotifier missing Locations / Nodes / Writer / EventID")

// Notify 实现 Notifier。
func (n *KafkaNotifier) Notify(ctx context.Context, playerID uint64, domain inboxpb.PlayerInboxDomain) NotifyResult {
	result, err := n.notify(ctx, playerID, domain)
	metrics.ObserveNotify(result.String())
	if err != nil {
		logx.WithContext(ctx).Errorf("[player_inbox] Notify not delivered player=%d domain=%d result=%s settled=%v: %v",
			playerID, domain, result, result.Settled(), err)
	}
	return result
}

func (n *KafkaNotifier) notify(ctx context.Context, playerID uint64, domain inboxpb.PlayerInboxDomain) (NotifyResult, error) {
	if n == nil || n.Locations == nil || n.Nodes == nil || n.Writer == nil || n.EventID == 0 {
		return NotifyError, errNotifierIncomplete
	}
	raw, err := n.Locations.GetLocation(ctx, redisslot.LocationKey(playerID))
	if err != nil {
		return NotifyError, err
	}
	if len(raw) == 0 {
		return NotifyOffline, nil
	}
	loc := &smpb.PlayerLocation{}
	if err := proto.Unmarshal(raw, loc); err != nil {
		// 位置键写坏了是数据面事故,不折成「离线」(同 scenenode.Locator.Resolve)。
		return NotifyError, err
	}
	if loc.GetNodeId() == "" {
		if loc.GetOwnerEpoch() != 0 {
			return NotifyAwaitingPlacement, nil
		}
		// 位置记录残缺是数据面异常,打 ERROR;但重试也修不好它,落定交给落地拉取。
		return NotifyNoNode, errors.New("location has neither node_id nor owner_epoch; settled, left to the landing fetch")
	}
	if !n.Nodes.Synced() {
		return NotifyNodesUnsynced, nil // 镜像还没完成首次全量同步:启动窗口里的正常现象,留给补发
	}
	entry, err := n.Nodes.EntryOf(loc.GetZoneId(), loc.GetNodeId())
	if errors.Is(err, scenenode.ErrNodeAmbiguous) {
		return NotifyNodeAmbiguous, nil // EntryOf 已打 ERROR;旧租约过期后自愈,留给补发
	}
	if err != nil {
		// 镜像已同步却查不到:可能是 watch 重连期间的旧镜像、注册延迟或租约抖动(玩家仍在线),不当成「节点已下线」
		// 直接落定;留给补发按退避重试,悬太久才落定(评审 R3,notifyGivesUp)。
		return NotifyNodeMissing, nil
	}
	if entry.NodeUuid == "" {
		return NotifyEmptyUUID, errors.New("scene node registration has empty uuid; refusing to send without target_instance_id")
	}
	payload, err := proto.Marshal(&event.PlayerInboxNotifyEvent{PlayerId: playerID, Domain: uint32(domain)})
	if err != nil {
		return NotifyError, err
	}
	value, err := proto.Marshal(&kafkapb.SceneCommand{
		CommandType:      kafkapb.SceneCommand_DispatchEvent,
		PlayerId:         playerID,
		TargetSceneId:    entry.NodeId,
		Payload:          payload,
		TargetInstanceId: entry.NodeUuid,
		EventId:          proto.Uint32(n.EventID),
	})
	if err != nil {
		return NotifyError, err
	}
	msg, err := kafkacmd.SceneCommandMessage(strconv.FormatUint(uint64(entry.NodeId), 10),
		strconv.FormatUint(playerID, 10), value)
	if err != nil {
		return NotifyError, err
	}
	if err := n.Writer.WriteMessages(ctx, msg); err != nil {
		return NotifyError, err
	}
	return NotifySent, nil
}
