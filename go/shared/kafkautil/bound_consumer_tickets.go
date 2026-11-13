package kafkautil

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

// 收尾核对(设计见 docs/design/kafka-bound-consumer-group.md §6):两类票据留到 CloseAndReport,在 owner 离组之后
// 按组内已提交位点核一次结局。
//
//   - 交还给新属主的记录(handoff):排空时副本逐个离组,每次离组都结束其余副本的代次;owner 终止时还在途或退避中、
//     没提交的记录(含旧代次提交被拒的),不再原地重试。没提交就是确定结局:组内已提交位点停在它前面,owner 离组之后
//     它归新属主(还在跑的副本),新属主处理并提交越过它(回调与落库都幂等)。
//   - 确定性恢复票据(replay,reader 模式的 ProcessingNotCommitted):所在分区漂到别的副本、不再回到本 owner。
//
// 位点越过了它、且组在该分区确实提交过(位点 >= 0,同 R5 的 groupCommitted 判据:-1 说明组在这个分区从没提交过,
// 越过它的只可能是保留期),就是别的属主处理并提交了它 —— 结清,计入同一次排空的收据;预算内核不到的才 HOLD
// (先记 HOLD 再结束票据)。读位点只走 coordinator 的 OffsetFetch,不碰消费与提交。

// boundTicketConfirmPolls / boundTicketConfirmInterval:收尾核对最多读几轮、两轮之间隔多久(经 sleep 接缝,单测不等墙钟)。
// 约 30 秒:新属主要等一次再均衡(kafka-go 默认 JoinGroupBackoff 5s)、拉取、处理、提交,正常远在这之内;调用方的 ctx 先到期就以它为准。
const (
	boundTicketConfirmPolls    = 30
	boundTicketConfirmInterval = time.Second
)

var errBoundTicketUnconfirmed = errors.New("Kafka 交还 / 恢复票据在收尾核对里没核到组内已提交位点越过它们")

// boundHandoff 是 owner 终止时交还给新属主、还没核到结局的一条已接纳记录;finish 是它的 relay 票据。
type boundHandoff struct {
	topic      string
	partition  int
	offset     int64
	generation int32
	finish     func()
}

// boundHandoffObservation 是收尾核对结清的一条交还记录原件(只记 Kafka API 实际返回值)。
type boundHandoffObservation struct {
	Topic          string
	Partition      int
	Offset         int64
	Generation     int32 // 交还时的代次
	GroupCommitted int64 // 核到的组内已提交位点(> Offset)
}

// confirmOutstandingTickets 在 owner 离组(Run 已返回)之后核交还与恢复票据,见文件头。
// owner 已记 HOLD 时不再核(收据反正是 HOLD),只结束票据;预算用完或 ctx 先到期,还没核到的记 HOLD 再结束票据。
func (c *BoundConsumerGroup) confirmOutstandingTickets(ctx context.Context) {
	for poll := 1; ; poll++ {
		c.mu.Lock()
		failed := c.failure != nil
		wanted := c.outstandingTicketPartitionsLocked()
		c.mu.Unlock()
		if len(wanted) == 0 {
			return
		}
		if failed {
			c.dropOutstandingTickets(nil)
			return
		}
		committed, err := c.readCommittedPartitions(ctx, wanted)
		if err == nil {
			c.mu.Lock()
			finished := c.resolvePassedTicketsLocked(committed)
			remaining := len(c.handoffs) + len(c.replay)
			c.signalLocked()
			c.mu.Unlock()
			for _, finish := range finished {
				finish()
			}
			if remaining == 0 {
				return
			}
		} else {
			logx.Errorf("[kafka-bound] group=%s: reading the group's committed offsets for the drain receipt failed (poll %d/%d): %v",
				c.config.Group.ID, poll, boundTicketConfirmPolls, err)
		}
		if poll >= boundTicketConfirmPolls || !c.sleep(ctx, boundTicketConfirmInterval) {
			c.dropOutstandingTickets(errBoundTicketUnconfirmed)
			return
		}
	}
}

// outstandingTicketPartitionsLocked 列出还没核到结局的票据涉及的 topic → 分区(升序)。调用方持有 c.mu。
func (c *BoundConsumerGroup) outstandingTicketPartitionsLocked() map[string][]int32 {
	seen := map[string]map[int32]bool{}
	add := func(topic string, partition int) {
		if seen[topic] == nil {
			seen[topic] = map[int32]bool{}
		}
		seen[topic][int32(partition)] = true
	}
	for _, handoff := range c.handoffs {
		add(handoff.topic, handoff.partition)
	}
	for _, delivery := range c.replay {
		add(delivery.message.Topic, delivery.message.Partition)
	}
	out := make(map[string][]int32, len(seen))
	for topic, partitions := range seen {
		for partition := range partitions {
			out[topic] = append(out[topic], partition)
		}
		sort.Slice(out[topic], func(i, j int) bool { return out[topic][i] < out[topic][j] })
	}
	return out
}

// readCommittedPartitions 逐 topic 读本组的已提交位点;任何一个读失败整轮作废。
func (c *BoundConsumerGroup) readCommittedPartitions(ctx context.Context, wanted map[string][]int32) (map[string]map[int32]int64, error) {
	out := make(map[string]map[int32]int64, len(wanted))
	for topic, partitions := range wanted {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		committed, err := c.readCommitted(ctx, topic, partitions)
		if err != nil {
			return nil, err
		}
		out[topic] = committed
	}
	return out, nil
}

// resolvePassedTicketsLocked 结清组内已提交位点越过的票据,返回要在锁外调用的票据结束函数。调用方持有 c.mu。
func (c *BoundConsumerGroup) resolvePassedTicketsLocked(committed map[string]map[int32]int64) []func() {
	passed := func(topic string, partition int, offset int64) (int64, bool) {
		position, ok := committed[topic][int32(partition)]
		return position, ok && position >= 0 && position > offset
	}
	var finished []func()
	kept := c.handoffs[:0]
	for _, handoff := range c.handoffs {
		position, ok := passed(handoff.topic, handoff.partition, handoff.offset)
		if !ok {
			kept = append(kept, handoff)
			continue
		}
		c.resolvedHandoffs = append(c.resolvedHandoffs, boundHandoffObservation{Topic: handoff.topic, Partition: handoff.partition, Offset: handoff.offset, Generation: handoff.generation, GroupCommitted: position})
		finished = append(finished, handoff.finish)
	}
	clear(c.handoffs[len(kept):])
	c.handoffs = kept
	for key, delivery := range c.replay {
		if _, ok := passed(delivery.message.Topic, delivery.message.Partition, delivery.message.Offset); !ok {
			continue
		}
		delete(c.replay, key)
		finished = append(finished, delivery.finish)
		c.appendResolvedReplayLocked(boundReplayObservation{Topic: delivery.message.Topic, Partition: delivery.message.Partition, Offset: delivery.message.Offset, FirstGeneration: delivery.originGeneration, PayloadDigest: delivery.payloadDigest[:], Resolution: boundReplayGroupCommittedAtClose})
	}
	return finished
}

// dropOutstandingTickets 结束全部还没核到结局的交还与恢复票据。reason 非 nil 时先记 HOLD 再结束票据,
// 排空方不会在两步之间看到「没有在途、也没有未知」;reason 为 nil 表示 HOLD 已经记过。
func (c *BoundConsumerGroup) dropOutstandingTickets(reason error) {
	c.mu.Lock()
	var (
		finished []func()
		lines    []string
	)
	for _, handoff := range c.handoffs {
		finished = append(finished, handoff.finish)
		lines = append(lines, fmt.Sprintf("handoff %s[%d]@%d", handoff.topic, handoff.partition, handoff.offset))
	}
	c.handoffs = nil
	for key, delivery := range c.replay {
		finished = append(finished, delivery.finish)
		lines = append(lines, fmt.Sprintf("replay %s[%d]@%d", delivery.message.Topic, delivery.message.Partition, delivery.message.Offset))
		delete(c.replay, key)
	}
	c.mu.Unlock()
	if reason != nil && len(finished) > 0 {
		sort.Strings(lines)
		const shown = 16
		if len(lines) > shown {
			lines = append(lines[:shown], fmt.Sprintf("... %d more", len(lines)-shown))
		}
		detail := strings.Join(lines, ", ")
		logx.Errorf("[kafka-bound] group=%s: %d ticket(s) not confirmed by the group's committed offset within the drain budget; receipt HOLD: %s",
			c.config.Group.ID, len(finished), detail)
		c.fail(fmt.Errorf("%w: %s", reason, detail))
	}
	for _, finish := range finished {
		finish()
	}
}

// fetchGroupCommitted 是 readCommitted 的生产实现:每次新建一条只读探针连接(sarama,不自动建 topic),读完即关。
// 只在收尾核对时用,每秒至多一次、最多 30 次。
func (c *BoundConsumerGroup) fetchGroupCommitted(ctx context.Context, topic string, partitions []int32) (map[int32]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prober, err := NewGroupLagProber(c.config.Group.Brokers)
	if err != nil {
		return nil, err
	}
	committed, err := CommittedOffsets(prober, c.config.Group.ID, topic, partitions)
	if closeErr := prober.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	return committed, nil
}
