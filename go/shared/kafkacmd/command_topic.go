// Package kafkacmd 只放控制面命令的**寻址**:topic 名、分区映射、以及让
// kafka-go 尊重显式分区的 Balancer。
//
// 为什么不放在 shared/kafkautil 里:kafkautil 还装着 topic 预建
// (topic_init.go,依赖 sarama 的 ClusterAdmin)。scene_manager / player_locator
// 只需要"往哪个分区发",不该因此被拖进一个 admin 客户端依赖 —— 它们的 go.mod
// 里本来就没有 sarama。寻址是每个生产者都要的,预建是运维路径,拆开。
package kafkacmd

// 寻址契约本体(Go 侧真源;C++ 侧那份在
// cpp/libs/engine/core/node/system/node/node_command_topic.h)。
// 两份实现由 testdata/command_partition_vectors.json 对拍,见 command_topic_test.go
// 与 cpp/tests/kafka_command_test。
//
// 设计文档:docs/design/control-plane-topic-partitioning-20260908.md
//
// 老方案是"一个节点一个 topic"(gate-5 / scene-77),两个问题:
//   P1 正确性:topic 名和 consumer group 名里都带 node_id,而 node_id 被 C++ 的
//      node_allocator 按"最小空闲"立刻回收。冻结但仍是 group 成员的前任 A 与继任者 B
//      同时在 gate-group-5 里,Kafka 只把分区判给一个;判给 A 时 A 把 B 的命令
//      消费掉再按 target_instance_id 丢弃,B 一条也收不到(登录绑定/进世界/踢人/
//      战斗绑定与结算静默消失)。
//   P2 规模:目标 ~10 万 scene 进程、每天重启 = 一个集群里 10 万个 topic,
//      controller 元数据、每分区内存、rebalance 时间全部塌方。
//
// 新方案:一个节点类型一个 topic + 固定分区数 P,**partition = node_id % P**。
// 生产者显式指定分区(不靠 key 哈希),消费者 assign 自己那个分区(不进 group)。
//
// 为什么不是"每个 shard 一个 Kafka 集群":全国同服 —— 任意分片的玩家要能和任意
// 分片的玩家一起玩,全局池的 match/battle 必须够得着任意 gate/scene。控制面必须
// 是一个可达的集群。

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	// DefaultCommandTopicPartitions 是 P 的默认值,必须与 C++ 的
	// node_command_topic.h::kDefaultCommandTopicPartitions 逐字相同。
	//
	// 改这个数 = 改不可变契约(与 db_task 同一条纪律,
	// docs/design/db-task-kafka-partition-contract.md):必须换代号、换一批新 topic,
	// 绝不在同名 topic 上扩分区 —— 扩分区会把 node_id % P 重映射,命令落到没人
	// assign 的分区上,静默全丢而且 Kafka 不报错。
	DefaultCommandTopicPartitions int32 = 256

	// DefaultCommandTopicGeneration 是代号。与 data_service 的
	// transaction_log_topic_g1 同形:**第一代也带后缀**,这样 broker 的 auto-create
	// (num.partitions=1)抢先建出来的裸名字 topic 与契约 topic 互不相干。
	DefaultCommandTopicGeneration uint32 = 1

	// GateCommandTopicBase / SceneCommandTopicBase 是基名,有效名再加 _g<代号>。
	GateCommandTopicBase  = "gate-cmd"
	SceneCommandTopicBase = "scene-cmd"

	// 环境变量覆盖。给"换代号"这种运维动作留的口子:改一次分区数要同时动
	// C++ yaml、Go 这两个变量、docker-compose 与 k8s 的 topic 预建,四处必须同拍。
	EnvCommandTopicPartitions = "KAFKA_COMMAND_TOPIC_PARTITIONS"
	EnvCommandTopicGeneration = "KAFKA_COMMAND_TOPIC_GENERATION"

	// commandTopicMarker 是"这是个控制面命令 topic"的判据:基名以 -cmd 结尾、
	// 后面跟 _g<数字>。CommandPartitionBalancer 用它区分"必须按显式分区发"的消息
	// 和共用同一个 Writer 的其它 topic。
	commandTopicMarker = "-cmd_g"
)

// CommandTopicContract 是本进程实际使用的分区契约。
type CommandTopicContract struct {
	Partitions int32
	Generation uint32
}

var (
	commandContractOnce sync.Once
	commandContract     CommandTopicContract
)

// CommandContract 读一次环境变量并缓存。非法值记 ERROR 后回落到默认值:
// 控制面寻址不能因为一个打错的环境变量就整体停摆,但必须留下痕迹。
func CommandContract() CommandTopicContract {
	commandContractOnce.Do(func() {
		commandContract = CommandTopicContract{
			Partitions: DefaultCommandTopicPartitions,
			Generation: DefaultCommandTopicGeneration,
		}
		if raw := strings.TrimSpace(os.Getenv(EnvCommandTopicPartitions)); raw != "" {
			v, err := strconv.ParseInt(raw, 10, 32)
			if err != nil || v <= 0 {
				logx.Errorf("kafkacmd: invalid %s=%q, falling back to %d",
					EnvCommandTopicPartitions, raw, DefaultCommandTopicPartitions)
			} else {
				commandContract.Partitions = int32(v)
			}
		}
		if raw := strings.TrimSpace(os.Getenv(EnvCommandTopicGeneration)); raw != "" {
			v, err := strconv.ParseUint(raw, 10, 32)
			if err != nil || v == 0 {
				logx.Errorf("kafkacmd: invalid %s=%q, falling back to %d",
					EnvCommandTopicGeneration, raw, DefaultCommandTopicGeneration)
			} else {
				commandContract.Generation = uint32(v)
			}
		}
		logx.Infof("kafkacmd: control-plane command topic contract partitions=%d generation=%d",
			commandContract.Partitions, commandContract.Generation)
	})
	return commandContract
}

// NormalizeCommandTopicPartitions:0 一律当"没配"。规则必须与 C++ 侧逐字一致
// (proto3 没有 presence,配置里不写就是 0)。
func NormalizeCommandTopicPartitions(configured int32) int32 {
	if configured <= 0 {
		return DefaultCommandTopicPartitions
	}
	return configured
}

// NormalizeCommandTopicGeneration:同上。
func NormalizeCommandTopicGeneration(configured uint32) uint32 {
	if configured == 0 {
		return DefaultCommandTopicGeneration
	}
	return configured
}

// CommandTopicBaseName: "gate" -> "gate-cmd"。
func CommandTopicBaseName(nodeShortName string) string {
	if nodeShortName == "" {
		return ""
	}
	return nodeShortName + "-cmd"
}

// CommandTopicName 拼有效名 `<基名>_g<代号>`。基名为空返回空,由调用方 fail-closed,
// 不拼出一个只有后缀的名字(与 data_service 的 topicForGeneration 同样处理)。
func CommandTopicName(base string, generation uint32) string {
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s_g%d", base, NormalizeCommandTopicGeneration(generation))
}

// CommandPartitionForNode 是寻址函数本体。
//
// 用取模而不是哈希:两端实现必须逐位一致,取模是唯一不会因语言/库版本漂移的映射;
// node_id 由分配器顺序发放,分布本来就均匀。
func CommandPartitionForNode(nodeID uint64, partitions int32) int {
	return int(nodeID % uint64(NormalizeCommandTopicPartitions(partitions)))
}

// IsCommandTopic 判断一个 topic 名是不是控制面命令 topic(`<x>-cmd_g<数字>`)。
func IsCommandTopic(topic string) bool {
	idx := strings.LastIndex(topic, commandTopicMarker)
	if idx <= 0 {
		return false
	}
	suffix := topic[idx+len(commandTopicMarker):]
	if suffix == "" {
		return false
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// GateCommandTopic / SceneCommandTopic 是当前代号下的有效 topic 名。
func GateCommandTopic() string {
	return CommandTopicName(GateCommandTopicBase, CommandContract().Generation)
}

func SceneCommandTopic() string {
	return CommandTopicName(SceneCommandTopicBase, CommandContract().Generation)
}

// GateCommandPartition / SceneCommandPartition 按当前契约算分区号。
func GateCommandPartition(nodeID uint64) int {
	return CommandPartitionForNode(nodeID, CommandContract().Partitions)
}

func SceneCommandPartition(nodeID uint64) int {
	return CommandPartitionForNode(nodeID, CommandContract().Partitions)
}

// ParseNodeID 把 "5" 这种字符串形态的路由号转成数字。
// 控制面里 gate_id / scene_node_id 在 Redis 与 proto 里都是字符串形态,
// 而分区号必须从数字算,解析失败就是无法寻址,必须 fail-closed 而不是发到 0 号分区。
func ParseNodeID(id string) (uint64, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(id), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid node id %q: %w", id, err)
	}
	return v, nil
}

// GateCommandMessage 组一条发往指定 gate 的 Kafka 消息(topic + 显式分区一起给)。
//
// 唯一入口:topic 名和分区号是一对不可分的东西,任何一处单独拼都会漂移。
func GateCommandMessage(gateID string, key string, value []byte) (kafkago.Message, error) {
	nodeID, err := ParseNodeID(gateID)
	if err != nil {
		return kafkago.Message{}, err
	}
	return kafkago.Message{
		Topic:     GateCommandTopic(),
		Partition: GateCommandPartition(nodeID),
		Key:       []byte(key),
		Value:     value,
	}, nil
}

// SceneCommandMessage 同上,发往 scene 节点。
func SceneCommandMessage(sceneNodeID string, key string, value []byte) (kafkago.Message, error) {
	nodeID, err := ParseNodeID(sceneNodeID)
	if err != nil {
		return kafkago.Message{}, err
	}
	return kafkago.Message{
		Topic:     SceneCommandTopic(),
		Partition: SceneCommandPartition(nodeID),
		Key:       []byte(key),
		Value:     value,
	}, nil
}

// CommandPartitionBalancer 让 kafka-go 的 Writer 尊重 Message.Partition。
//
// kafka-go 的 Writer 永远走 Balancer 选分区,Message.Partition 在写入路径上是被
// **忽略**的(writer.go: balancer.Balance(msg, partitions...))。控制面命令的落点
// 不能由 key 哈希决定 —— 它必须精确等于 node_id % P,否则消费端 assign 的那个分区
// 上根本没有这条消息。所以这里把 Message.Partition 透传出去。
//
// 只对控制面命令 topic 生效;同一个 Writer 上的其它 topic 交给 Fallback
// (各服务原来配的是 &kafka.Hash{}),行为不变。
type CommandPartitionBalancer struct {
	Fallback kafkago.Balancer
}

func (b *CommandPartitionBalancer) Balance(msg kafkago.Message, partitions ...int) int {
	if IsCommandTopic(msg.Topic) {
		for _, p := range partitions {
			if p == msg.Partition {
				return p
			}
		}
		// broker 上的分区数与契约不符(最常见的原因:topic 被生产者 auto-create 成
		// 1 分区,topic 预建 Job 没跑过)。这条消息无论落到哪个分区都不会被目标节点
		// 读到,但沉默是最坏的结果 —— 至少留一行能直接定位的 ERROR。
		logx.Errorf("kafkacmd: command topic %s partition %d not in broker partition set %v; "+
			"分区契约不符,目标节点不会收到这条命令。先跑 topic 预建"+
			"(deploy/docker-compose.yml kafka-topic-init / k8s_deploy.ps1 -Command infra-kafka-topics),"+
			"若是有意改分区数请换 %s 代号。",
			msg.Topic, msg.Partition, partitions, EnvCommandTopicGeneration)
	}
	fallback := b.Fallback
	if fallback == nil {
		// 各生产服务原本就配的是 Hash;不给 Fallback 时保持同一行为,
		// 不要退化成 kafka-go 默认的 RoundRobin(那会打散 key 的保序语义)。
		// 用共享实例:kafkago.Hash 内部有 mutex,可以跨 goroutine 复用,
		// 每条消息 new 一个只是白分配。
		fallback = defaultCommandFallbackBalancer
	}
	return fallback.Balance(msg, partitions...)
}

var defaultCommandFallbackBalancer kafkago.Balancer = &kafkago.Hash{}
