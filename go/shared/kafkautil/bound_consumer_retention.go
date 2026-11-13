package kafkautil

import (
	"context"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

// 保留期删掉了本组还没处理的记录时不静默跳过(设计见 docs/design/kafka-bound-consumer-group.md §7)。
//
// kafka-go 的分区 reader 遇 OffsetOutOfRange(读的位点早于日志起点)会自己跳到日志起点(v0.4.47 reader.go 的 run,
// 经 ErrorLogger 打 `skipping from offset X to Y`),(重新)初始化时位点早于起点也会静默改到起点(initialize,不出声)。
// owner 打开分区时核过组内已提交位点在保留范围内(dialPartitionReader),但代次中途被保留期追上(消费者卡住、积压逼近
// retention.bytes)、或 reader 断线重连时起点已被推过,下一条记录都会越过应到的位点,而本组毫不知情。
// 所以 consumePartition 每取到一条都核它是不是紧接着上一条;有缺口就读一次日志起点:
//   - 起点 <= 应到位点:缺口不是保留期删的(事务控制记录、压缩占的位点),照常接纳;
//   - 起点 > 应到位点,且这段位点归本组(组在该分区提交过,或本 owner 已从这里取到过记录):中间那段没被本组处理就删了 ——
//     记 HOLD、停止取新、计 kafka_bound_consumer_retention_loss_total、打 ERROR,不接纳、不提交,按手册人工确认丢失范围;
//   - 组在该分区从没提交过、本 owner 也还没取到过一条(新组从日志起点读,打开之后起点才被推过去):组的起点本来就是日志起点,
//     没有越过本组的已提交位点,只打 ERROR、照常接纳。

// admitGap 核一次取到的记录 delivered 前面、从应到位点 expected 起的缺口。返回 true = 照常接纳;
// false = 已 HOLD,或本代次已不再消费(读日志起点失败时有界退避后重读,读到之前不接纳)。
func (c *BoundConsumerGroup) admitGap(fetchContext context.Context, partition *boundPartition, expected, delivered int64) bool {
	var delay time.Duration
	for {
		start, err := c.readLogStart(fetchContext, partition.topic, partition.partition)
		if err == nil {
			return c.judgeGap(partition, expected, delivered, start)
		}
		if fetchContext.Err() != nil {
			return false
		}
		delay = nextBoundRetryDelay(delay)
		observeBoundRetry(c.config.Group.ID, boundStageFetch)
		logx.Errorf("[kafka-bound] group=%s topic=%s partition=%d: delivered offset %d skips expected offset %d; reading the log start to check retention failed, retry in about %v: %v",
			c.config.Group.ID, partition.topic, partition.partition, delivered, expected, delay, err)
		if !c.sleep(fetchContext, delay) {
			return false
		}
	}
}

// judgeGap 按日志起点 start 判缺口,见文件头。
func (c *BoundConsumerGroup) judgeGap(partition *boundPartition, expected, delivered, start int64) bool {
	group := c.config.Group.ID
	if start <= expected {
		logx.Infof("[kafka-bound] group=%s topic=%s partition=%d: delivered offset %d skips expected offset %d but the log start is %d (not retention: transaction markers or compaction); accepted",
			group, partition.topic, partition.partition, delivered, expected, start)
		return true
	}
	if !partition.groupCommitted && expected == partition.initial {
		logx.Errorf("[kafka-bound] group=%s topic=%s partition=%d: the log start moved from %d to %d before this group ever committed or read the partition; the group starts from the log start, accepted",
			group, partition.topic, partition.partition, expected, start)
		return true
	}
	lost := min(start, delivered) - expected
	observeBoundRetentionLoss(group)
	logx.Errorf("[kafka-bound] group=%s topic=%s partition=%d: RECORDS DELETED BY RETENTION BEFORE THIS GROUP PROCESSED THEM: expected offset %d, log start %d, next delivered %d (about %d record(s)); the consumer HOLDs and stops fetching instead of skipping them, see docs/ops/kafka-bound-consumer-runbook.md",
		group, partition.topic, partition.partition, expected, start, delivered, lost)
	c.fail(fmt.Errorf("%w: %s[%d] 应到 %d,日志起点已是 %d,取到 %d", errBoundRetentionSkipped, partition.topic, partition.partition, expected, start, delivered))
	c.StopFetching()
	return false
}
