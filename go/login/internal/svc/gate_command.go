package svc

import (
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"

	game "login/generated/pb/game"
	kafkapb "proto/contracts/kafka"
	"shared/kafkacmd"
)

// 本文件是 login 发往 gate 的控制面命令(BindSession / KickPlayer)的**唯一**寻址收口。
//
// 背景:控制面命令从"一个节点一个 topic"(gate-5)改成"一个节点类型一个 topic +
// partition = node_id % P"(gate-cmd_g1 的第 5 号分区),
// 见 docs/design/control-plane-topic-partitioning-20260908.md。
// login 是最后一个还在写老 topic 的生产者,切完之后
// Kafka.DisableLegacyPerNodeTopic 才能置 true、老的 group 订阅路径才能关掉
// (在那之前 R05/R09 的"僵尸前任把继任者饿死"在老路径上仍然成立)。
//
// 为什么收成一个函数而不是在两个方法里各写一遍:
//   - topic 名与分区号必须一起算。分开写必然漂移,而漂移的表现是消息落到没人
//     assign 的分区上、Kafka 一个错都不报。
//   - 两道 fail-closed 守卫(空 instance id / 非数字 gate id)必须对**每一条**
//     命令都成立。写在这里,新增命令类型时想漏也漏不掉。

// gateCommandMessage 是一条命令的完整投递描述:寻址 + 载荷。
type gateCommandMessage struct {
	Topic        string
	Partition    int32
	PartitionKey string
	Payload      []byte
}

// buildGateCommandMessage 填齐目标字段、做两道守卫、算出 topic+分区并序列化。
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
	// fail-closed 的,login 这两条是审计点名的最后缺口。
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

// buildBindSessionCommand / buildKickPlayerCommand 是两条命令各自的组装入口。
// 单独拆出来是为了让单测能盯住**生产路径本身**(事件号、目标字段、topic、分区),
// 而不是在测试里再抄一遍字段字面量 —— 抄一遍的测试只能证明抄得对。
func buildBindSessionCommand(gateID string, gateInstanceID string,
	sessionID uint32, playerID uint64, enterGsType uint32,
) (gateCommandMessage, error) {
	return buildGateCommandMessage("bind session", gateID, gateInstanceID, &kafkapb.GateCommand{
		SessionId:   sessionID,
		PlayerId:    playerID,
		EventId:     uint32(game.ContractsKafkaBindSessionEventEventId),
		EnterGsType: enterGsType,
	})
}

func buildKickPlayerCommand(gateID string, gateInstanceID string,
	sessionID uint32, playerID uint64,
) (gateCommandMessage, error) {
	return buildGateCommandMessage("kick", gateID, gateInstanceID, &kafkapb.GateCommand{
		SessionId: sessionID,
		PlayerId:  playerID,
		EventId:   uint32(game.ContractsKafkaKickPlayerEventEventId),
	})
}
