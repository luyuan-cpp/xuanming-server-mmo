package kafka

import (
	"bytes"
	"sync/atomic"

	"db/internal/metrics"
	db_proto "proto/db"
	"shared/kafkautil"

	"github.com/IBM/sarama"
	"github.com/zeromicro/go-zero/core/logx"
)

// 分区契约的消费端观测(docs/design/db-task-kafka-partition-contract.md「分区函数契约」)。
//
// 每条从 Kafka 解码成功的 DBTask 都按契约核对一次:Kafka key 必须是 kafkautil.DBTaskKey(task.Key),
// 消息所在分区必须是 kafkautil.DBTaskPartition(key, N)。结果只进
// db_task_producer_partition_total{op,verdict} 并限量打日志,**不拒绝消息,不改 ack、游标、重试**:
//   - 拒写会丢玩家存盘;拒读会在 login 滚动发版窗口里打断仍在跑的旧版 login(它们按 FNV 分区)。
//   - 遗留重试载荷照旧按原规则 fail-closed(没有可信来源分区就进死信),observer 不替它补分区。
//   - 重试任务不经 ConsumeClaim,不在观测范围内。
//   - 计数按每次投递算:再平衡后的重投递会重复计入,比例类指标不受影响。
//
// 写的 verdict 不是 ok 说明有生产者破坏契约:同一玩家的写一旦在新旧生产者之间换分区,
// 游标里记录的分区就不可比,写被判 orderingIncomparable 隔离进死信 —— 所以按 critical 告警。
// 读不符说明还有旧版 login 在跑,这些读没有读后写屏障。

// producerPartitionLogBudget 是每进程、每种 op 最多打的不符日志条数,防止契约被破坏时刷屏。
const producerPartitionLogBudget = 20

// producerPartitionLogged 是各 op 已打的不符日志条数。下标:0=read,1=write,2=other。
var producerPartitionLogged [3]atomic.Int64

// observeProducerPartition 核对一条消息是否守分区契约,计数并在不符时限量打日志,返回 verdict。
//
// partitionCount 必须是 msg.Topic 的分区数。现在一个 handler 只消费一个 topic,
// 传 h.consumer.partitionCount 即可;以后如果一个 handler 消费两代 topic,要按 msg.Topic 取分区数,
// 否则写会被误判 partition_mismatch 并触发 critical 告警。
//
// 只读:不修改 msg / task,不访问 Redis / MySQL,可在 ConsumeClaim 的消费循环里同步调用。
func observeProducerPartition(msg *sarama.ConsumerMessage, task *db_proto.DBTask, partitionCount int32) string {
	op := producerPartitionOp(task)
	verdict, expected := classifyProducerPartition(msg, task, partitionCount)
	metrics.CountProducerPartition(op, verdict)
	if verdict != metrics.ProducerPartitionOK {
		logProducerPartitionMismatch(msg, task, op, verdict, expected)
	}
	return verdict
}

// producerPartitionOp 把 DBTask.op 归到 read / write / other 三类(指标 label 是封闭集合)。
func producerPartitionOp(task *db_proto.DBTask) string {
	if task == nil {
		return metrics.ProducerPartitionOpOther
	}
	switch task.Op {
	case db_proto.DBOp_DB_OP_READ:
		return metrics.ProducerPartitionOpRead
	case db_proto.DBOp_DB_OP_WRITE:
		return metrics.ProducerPartitionOpWrite
	default:
		return metrics.ProducerPartitionOpOther
	}
}

// classifyProducerPartition 返回 verdict 与契约分区(算不出时为 -1)。
// 先核对 key 再核对分区:key 不合规时消息落在哪个分区都不能说明生产者用的是哪个函数。
func classifyProducerPartition(msg *sarama.ConsumerMessage, task *db_proto.DBTask, partitionCount int32) (string, int32) {
	if msg == nil || task == nil || partitionCount <= 0 {
		return metrics.ProducerPartitionInvalid, -1
	}
	var buf [20]byte // uint64 十进制最多 20 位
	want := kafkautil.AppendDBTaskKey(buf[:0], task.Key)
	if !bytes.Equal(msg.Key, want) {
		// nil 或空 key 也落在这一支。expected 用规范 key 算出来,只供日志排障。
		expected, err := kafkautil.DBTaskPartition(want, partitionCount)
		if err != nil {
			expected = -1
		}
		return metrics.ProducerPartitionKeyMismatch, expected
	}
	expected, err := kafkautil.DBTaskPartition(msg.Key, partitionCount)
	if err != nil {
		return metrics.ProducerPartitionInvalid, -1
	}
	if expected != msg.Partition {
		return metrics.ProducerPartitionMismatch, expected
	}
	return metrics.ProducerPartitionOK, expected
}

// allowProducerPartitionLog 判断该 op 是否还有日志额度。
func allowProducerPartitionLog(op string) bool {
	idx := 2
	switch op {
	case metrics.ProducerPartitionOpRead:
		idx = 0
	case metrics.ProducerPartitionOpWrite:
		idx = 1
	}
	return producerPartitionLogged[idx].Add(1) <= producerPartitionLogBudget
}

func logProducerPartitionMismatch(msg *sarama.ConsumerMessage, task *db_proto.DBTask, op, verdict string, expected int32) {
	if !allowProducerPartitionLog(op) {
		return
	}
	topic, partition, offset := "", int32(-1), int64(-1)
	var kafkaKey []byte
	if msg != nil {
		topic, partition, offset, kafkaKey = msg.Topic, msg.Partition, msg.Offset, msg.Key
	}
	var taskKey uint64
	msgType := ""
	if task != nil {
		taskKey, msgType = task.Key, task.MsgType
	}
	if op == metrics.ProducerPartitionOpRead {
		logx.Infof("db_task read outside the partition contract (old login?): verdict=%s topic=%s partition=%d expectedPartition=%d offset=%d kafkaKey=%q taskKey=%d",
			verdict, topic, partition, expected, offset, kafkaKey, taskKey)
		return
	}
	logx.Errorf("DATA-ORDERING: db_task producer violated the partition contract: verdict=%s op=%s topic=%s partition=%d expectedPartition=%d offset=%d kafkaKey=%q taskKey=%d msgType=%s",
		verdict, op, topic, partition, expected, offset, kafkaKey, taskKey, msgType)
}
