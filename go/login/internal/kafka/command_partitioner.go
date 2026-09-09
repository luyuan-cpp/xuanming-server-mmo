package kafka

import (
	"fmt"

	"github.com/IBM/sarama"
	"github.com/zeromicro/go-zero/core/logx"

	"shared/kafkacmd"
)

// NewCommandAwarePartitioner 是控制面命令 topic 的分区器,装在**非事务的
// plainProducer** 上(见 key_ordered_producer.go 的 plainCfg)。
//
// 为什么需要它:控制面命令改成"一个节点类型一个 topic + partition = node_id % P"
// 之后(docs/design/control-plane-topic-partitioning-20260908.md),消息落在哪个分区
// 不能由 key 哈希决定 —— 消费端每个节点只 assign 自己那一个分区,算错分区 = 目标
// 节点永远读不到这条命令,而且 Kafka 一个错都不报。sarama 默认的
// NewHashPartitioner 会**覆盖** ProducerMessage.Partition,所以必须换掉。
//
// 这是 go/shared/kafkacmd 里 CommandPartitionBalancer(kafka-go 侧)的 sarama 对应物:
// login 用的是 sarama,shared 那个 Balancer 接的是 kafka-go 的 Writer,接口不通用,
// 但两边的规则必须逐字相同 —— 命令 topic 走显式分区,其它 topic 保持原行为。
//
// 作用域刻意收窄到"命令 topic"这一类:sarama 是**按 topic 各建一个 Partitioner**
// 的,所以 db_task 那类 topic 拿到的仍然是与改造前一模一样的
// sarama.NewHashPartitioner 实例,行为零变化。
func NewCommandAwarePartitioner(topic string) sarama.Partitioner {
	if kafkacmd.IsCommandTopic(topic) {
		return &commandPartitioner{topic: topic}
	}
	// 非命令 topic:原样保持 sarama.NewConfig() 的默认分区器。
	return sarama.NewHashPartitioner(topic)
}

// commandPartitioner 把调用方算好的 ProducerMessage.Partition 原样透传出去。
type commandPartitioner struct {
	topic string
}

func (p *commandPartitioner) Partition(msg *sarama.ProducerMessage, numPartitions int32) (int32, error) {
	if msg.Partition < 0 || msg.Partition >= numPartitions {
		// broker 上的分区数与契约不符,最常见的原因是 topic 没被预建、被某个抢先
		// 发消息的生产者 auto-create 成了 1 分区。这条命令无论落到哪个分区都不会被
		// 目标节点读到,所以**不降级、直接报错**:静默丢控制面命令的表现是"登录卡在
		// 分配场景"这类最难查的故障,宁可让调用方拿到错误。
		logx.Errorf("kafka: command topic %s partition %d out of broker range [0,%d); "+
			"分区契约不符,目标节点不会收到这条命令。先跑 topic 预建"+
			"(deploy/docker-compose.yml kafka-topic-init / k8s_deploy.ps1 -Command infra-kafka-topics),"+
			"若是有意改分区数请换 %s 代号。",
			p.topic, msg.Partition, numPartitions, kafkacmd.EnvCommandTopicGeneration)
		return 0, fmt.Errorf("command topic %s: partition %d out of broker range [0,%d)",
			p.topic, msg.Partition, numPartitions)
	}
	return msg.Partition, nil
}

// RequiresConsistency 返回 true,与 sarama 的 ManualPartitioner / HashPartitioner 一致:
// 它让 sarama 取**全部**分区(而不是只取当前可写的分区)来做下标映射,
// 否则某个分区暂时不可写时整个分区表会错位,node_id % P 算出来的号就指到别的分区上。
func (p *commandPartitioner) RequiresConsistency() bool { return true }
