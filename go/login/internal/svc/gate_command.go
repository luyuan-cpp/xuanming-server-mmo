package svc

import (
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"

	game "login/generated/pb/game"
	basepb "proto/common/base"
	kafkapb "proto/contracts/kafka"
	"shared/kafkacmd"
)

// 本文件是 login 发往 gate 的命令(控制面的 BindSession / KickPlayer,以及经 gate 转给客户端的
// PushToPlayer tip 通知)的**唯一**寻址收口。
//
// 背景:控制面命令从"一个节点一个 topic"(gate-5)改成"一个节点类型一个 topic +
// partition = node_id % P"(gate-cmd_g1 的第 5 号分区),
// 见 docs/design/control-plane-topic-partitioning-20260908.md。
//
// 为什么收成一个函数而不是在每个方法里各写一遍:
//   - topic 名与分区号必须一起算。分开写必然漂移,而漂移的表现是消息落到没人
//     assign 的分区上、Kafka 一个错都不报。
//   - fail-closed 守卫(空 instance id / 非数字 gate id / node id 为 0)必须对**每一条**
//     命令都成立。写在这里,新增命令类型时想漏也漏不掉 —— PushTip 就是后加的第三条,
//     它同样只填业务字段,寻址与守卫一行没抄。

// gateCommandMessage 是一条命令的完整投递描述:寻址 + 载荷。
type gateCommandMessage struct {
	Topic        string
	Partition    int32
	PartitionKey string
	Payload      []byte
}

// buildGateCommandMessage 填齐目标字段、做三道 fail-closed 守卫、算出 topic+分区并序列化。
//
// kind 只用于错误信息(排障时要一眼看出是哪条命令被拒了)。
// cmd 由调用方填业务字段(event_id / session_id / player_id / …),
// 目标字段 target_gate_id 与 target_instance_id 一律由这里覆盖填写。
func buildGateCommandMessage(kind string, gateID string, gateInstanceID string,
	cmd *kafkapb.GateCommand,
) (gateCommandMessage, error) {
	// 守卫一(不变量 2,CLAUDE.md §7;审计 R08):发往 gate 的命令必须带目标实例 uuid。
	// 空值 = 消费端 ValidateCommandTarget 的防僵尸过滤被关闭 —— gate 的路由 node_id
	// 会被 node_allocator 立刻回收复用,老 gate 没彻底退出时会把发给新 gate 的命令
	// 一并消费掉再执行。共享推送路径(shared/kafkautil/gate_push.go:57)早就是
	// fail-closed 的,login 这两条(审计当时的 Bind / Kick)是审计点名的最后缺口;
	// 后加的 PushTip 同样受此守卫。
	if gateInstanceID == "" {
		return gateCommandMessage{}, fmt.Errorf(
			"%s to gate %q rejected: empty gate_instance_id, anti-zombie filtering would be disabled",
			kind, gateID)
	}

	// 守卫二:拿不到数字节点号就没法算分区,必须 fail-closed。
	// 默认发到 0 号分区是最坏的选择:0 号分区上坐着 node_id 是 P 的倍数的那些 gate,
	// 命令会被它们读到再按 instance id 丢弃 —— 静默丢消息,没有任何错误可查。
	gateNodeID, err := kafkacmd.ParseNodeID(gateID)
	if err != nil {
		return gateCommandMessage{}, fmt.Errorf("%s rejected: %w", kind, err)
	}

	// 守卫三:node_id 0 也算"没有 gate id"。node_allocator 从 1 开始发号
	// (node_allocator.cpp:76),0 只会来自 SessionDetails.gate_node_id 这个字段
	// 压根没被填过 —— proto3 没有 presence,"没填"和"填了 0"在线上长得一模一样。
	// 放过去的后果是双重的:落到 0 号分区(那上面坐着 node_id 是 P 的倍数的 gate),
	// 而且 target_gate_id=0 会让消费端的整数过滤直接放行,等于同时废掉寻址与过滤。
	// 这一条是 login 特有的:别的 Go 生产者拿到的 gate id 来自 Redis 会话记录,
	// 不是一个可能没填的数字字段。
	if gateNodeID == 0 {
		return gateCommandMessage{}, fmt.Errorf(
			"%s rejected: gate node id is 0 (unset gate_node_id); node ids are allocated from 1 and 0 would disable target filtering", kind)
	}

	// target_gate_id 是消费端**第一级**(整数)过滤的依据。同一个分区上坐着几百个
	// gate,留 0 会让 ValidateCommandTarget 直接放行,每条消息都退化成一次
	// target_instance_id 字符串比较(设计文档 §4 的读放大预算就靠这一级挡掉 389/390)。
	cmd.TargetGateId = uint32(gateNodeID)
	cmd.TargetInstanceId = gateInstanceID

	data, err := proto.Marshal(cmd)
	if err != nil {
		return gateCommandMessage{}, fmt.Errorf("marshal gate %s command: %w", kind, err)
	}

	return gateCommandMessage{
		Topic:     kafkacmd.GateCommandTopic(),
		Partition: int32(kafkacmd.GateCommandPartition(gateNodeID)),
		// key 不再决定落点(分区是显式指定的),保留是为了 broker 侧与日志的可读性,
		// 以及同一玩家的多条命令在同一分区内仍按发送顺序排列。
		PartitionKey: strconv.FormatUint(cmd.PlayerId, 10),
		Payload:      data,
	}, nil
}

// buildBindSessionCommand / buildKickPlayerCommand / buildPushTipCommand 是各条命令各自的组装入口。
// 单独拆出来是为了让单测能盯住**生产路径本身**(事件号、目标字段、topic、分区),
// 而不是在测试里再抄一遍字段字面量 —— 抄一遍的测试只能证明抄得对。
//
// Bind 的业务字段整份序列化成 BindSessionEvent 放进 payload:gate 的生成路由
// (gate_kafka_command_router.cpp)的 DispatchGateKafkaCommand 按白名单解包 payload 后直接调用 handler，信封路径只拼得出
// session_id / player_id / enter_gs_type,带不到 session_version 与 login_generation。
// login_generation 必须随 Bind 到 gate:Bind 先于本次路由到达时,gate 会立即按上一次的
// lastSceneRoute 补发登录进场,补发要带本次登录的代次(scene-enter-session-fence.md §4)。
// 新旧 gate 都认得 payload 里的 BindSessionEvent(事件已在 gate 白名单里，BindSessionEvent=32,未知字段原样跳过)。
// 信封字段照填(与 PushTip 同做法):分区 key 取自信封 PlayerId,broker 侧排障也读信封。
func buildBindSessionCommand(gateID string, gateInstanceID string,
	sessionID uint32, playerID uint64, sessionVersion uint32, enterGsType uint32, loginGeneration uint64,
) (gateCommandMessage, error) {
	eventPayload, err := proto.Marshal(&kafkapb.BindSessionEvent{
		SessionId:       sessionID,
		PlayerId:        playerID,
		SessionVersion:  sessionVersion,
		EnterGsType:     enterGsType,
		LoginGeneration: loginGeneration,
	})
	if err != nil {
		return gateCommandMessage{}, fmt.Errorf("marshal BindSessionEvent (session=%d): %w", sessionID, err)
	}
	return buildGateCommandMessage("bind session", gateID, gateInstanceID, &kafkapb.GateCommand{
		SessionId:   sessionID,
		PlayerId:    playerID,
		EventId:     uint32(game.ContractsKafkaBindSessionEventEventId),
		EnterGsType: enterGsType,
		Payload:     eventPayload,
	})
}

// Kick 与 Bind 同理,业务字段整份序列化成 KickPlayerEvent 放进 payload:踢线者的登录代次只能经 payload 到 gate
// (信封没有对应字段,生成路由的信封分支只按同名同型映射得到 session_id / player_id)。gate 的 ClassifyKick
// 用 player_id 只踢当前绑定该玩家的会话,用代次在会话已见过不小于它的登录时不踢
// (docs/design/scene-enter-chain-residuals.md §2.2 ③④)。
// 旧 gate 按白名单解出 KickPlayerEvent,未知的 2 / 3 号字段被忽略,仍按 session_id 踢,不丢踢线。
// 信封的 SessionId / PlayerId / EventId 照填:分区 key 取自信封 PlayerId,broker 侧排障也读信封。
func buildKickPlayerCommand(gateID string, gateInstanceID string,
	sessionID uint32, playerID uint64, loginGeneration uint64,
) (gateCommandMessage, error) {
	eventPayload, err := proto.Marshal(&kafkapb.KickPlayerEvent{
		SessionId:       sessionID,
		PlayerId:        playerID,
		LoginGeneration: loginGeneration,
	})
	if err != nil {
		return gateCommandMessage{}, fmt.Errorf("marshal KickPlayerEvent (session=%d): %w", sessionID, err)
	}
	return buildGateCommandMessage("kick", gateID, gateInstanceID, &kafkapb.GateCommand{
		SessionId: sessionID,
		PlayerId:  playerID,
		EventId:   uint32(game.ContractsKafkaKickPlayerEventEventId),
		Payload:   eventPayload,
	})
}

// buildPushTipCommand 组装"经 gate 给某个会话的客户端推一条 tip"的命令。
//
// 三层信封,由内到外:
//
//	TipInfoMessage{id}                                   客户端按 id 查文案(id 由导表器发号,调用方只传枚举值)
//	MessageContent{message_id, serialized}               gate 原样写给客户端 TCP;message_id = SendTipToClient
//	PushToPlayerEvent{session_id, player_id, content}    gate 按 session_id 找连接,按 player_id 核对会话当前玩家
//
// 最外层仍是 GateCommand,寻址与守卫走 buildGateCommandMessage,与 Bind / Kick 同一条路。
// gate 侧只认 payload 里的 session_id 与 player_id;GateCommand 上的 SessionId / PlayerId 照 Kick 那样填,
// 一是分区 key 取自 PlayerId(同一玩家的命令在分区内保序),二是 broker 侧排障可读。
//
// 消息号与事件号一律引用生成常量,不写数字字面量。
func buildPushTipCommand(gateID string, gateInstanceID string,
	sessionID uint32, playerID uint64, tipID uint32,
) (gateCommandMessage, error) {
	// 目标玩家身份栅栏(gate_event.proto PushToPlayerEvent.player_id):session_id 会被复用,
	// 0 在 gate 侧是「不校验」的兼容位,发 0 等于关掉栅栏,tip 可能弹给复用同号的另一个玩家。
	// 与共享推送收口 kafkautil.PushToPlayer 同口径 fail-closed。
	if playerID == 0 {
		return gateCommandMessage{}, fmt.Errorf(
			"push tip (tip=%d session=%d) rejected: zero player_id, gate identity fence would be disabled", tipID, sessionID)
	}
	return buildPushTipMessage("push tip", gateID, gateInstanceID, sessionID, playerID, playerID, tipID)
}

// buildPushTipToUnboundSessionCommand 组装发给**尚未绑定玩家**的会话的 tip:BindSession 还没发出时,gate 上的会话没有绑定这名玩家,
// 带真实 player_id 必被身份栅栏丢弃(ClassifyPush 判 kDrop),客户端只能干等 60 秒的进场超时。
//
// 这是「生产者拒发 0」的唯一例外:PushToPlayerEvent.player_id 留 0,gate 按兼容位放行;信封 PlayerId 照填,分区 key 与 broker 侧排障都读它。
// 关掉栅栏的代价可接受:内容是固定 tip、不含私有数据;target_instance_id 已挡住 node_id 复用,残余只有同一 gate 实例内的 session 序号回绕。
// 别的命令不要走这里:只要 Bind 已发出(同分区、排在前面),就必须用 buildPushTipCommand 带上真实 player_id。
func buildPushTipToUnboundSessionCommand(gateID string, gateInstanceID string,
	sessionID uint32, playerID uint64, tipID uint32,
) (gateCommandMessage, error) {
	if playerID == 0 {
		return gateCommandMessage{}, fmt.Errorf(
			"push tip to unbound session (tip=%d session=%d) rejected: zero player_id, the enter chain always knows its player", tipID, sessionID)
	}
	return buildPushTipMessage("push tip to unbound session", gateID, gateInstanceID, sessionID, playerID, 0, tipID)
}

// buildPushTipMessage 组三层信封。envelopePlayerID 填 GateCommand.PlayerId;fencePlayerID 填 PushToPlayerEvent.player_id,
// 即 gate 身份栅栏核对的目标玩家,0 = 不校验。
func buildPushTipMessage(kind string, gateID string, gateInstanceID string,
	sessionID uint32, envelopePlayerID uint64, fencePlayerID uint64, tipID uint32,
) (gateCommandMessage, error) {
	tipBody, err := proto.Marshal(&basepb.TipInfoMessage{Id: tipID})
	if err != nil {
		return gateCommandMessage{}, fmt.Errorf("marshal push tip body (tip=%d): %w", tipID, err)
	}
	eventPayload, err := proto.Marshal(&kafkapb.PushToPlayerEvent{
		SessionId: sessionID,
		PlayerId:  fencePlayerID,
		MessageContent: &basepb.MessageContent{
			MessageId:         uint32(game.SceneClientPlayerCommonSendTipToClientMessageId),
			SerializedMessage: tipBody,
		},
	})
	if err != nil {
		return gateCommandMessage{}, fmt.Errorf("marshal PushToPlayerEvent (tip=%d): %w", tipID, err)
	}
	return buildGateCommandMessage(kind, gateID, gateInstanceID, &kafkapb.GateCommand{
		SessionId: sessionID,
		PlayerId:  envelopePlayerID,
		EventId:   uint32(game.ContractsKafkaPushToPlayerEventEventId),
		Payload:   eventPayload,
	})
}

// PushTipToUnboundSession 经 gate 给尚未绑定玩家的会话推一条 tip,用于 BindSession 发出之前的进场失败提示
// (entergamelogic.go notifyEnterGameFailed)。身份栅栏为什么可以放开见 buildPushTipToUnboundSessionCommand;
// 其余约定同 PushTipToSession。
func (s *ServiceContext) PushTipToUnboundSession(gateID string, gateInstanceID string, sessionID uint32, playerID uint64, tipID uint32) error {
	msg, err := buildPushTipToUnboundSessionCommand(gateID, gateInstanceID, sessionID, playerID, tipID)
	if err != nil {
		return err
	}

	if err := s.KafkaClient.SendToTopicPartition(msg.Topic, msg.Partition, msg.Payload, msg.PartitionKey); err != nil {
		return fmt.Errorf("send gate push tip to unbound session command to %s/%d: %w", msg.Topic, msg.Partition, err)
	}

	return nil
}
