package kafka

// 待重搬的重试收据(R7 复核)。
//
// 收据被认领进本实例的 processing 之后,出口都是单脚本原子搬移(moveRetryReceipt):claim 撤销后原样搬回 p{origin}
// (requeueRevokedRetry、consumeOneOwnedRetry 的撤销分支)、放错 p 键挪回原分区(misrouted)、失败写回 ready
// (deferFailedTask 的重试分支)、进死信(moveClaimedRetryToDead、deadLetterRetry)。搬移因 Redis 出错没做成时,收据
// 留在本实例 processing:本实例一直续租,reclaimOnce 跳过自己,别的实例也不回收,以前只能等重启(start 收回自己)或停机
// release 才回到收件箱。db 的存活探针只看 gRPC 端口,Redis 故障不会让 Pod 重启;而 Redis 故障恰好会让 claim 失败、
// 再平衡,队列里的 owner 重试成批走撤销搬回,于是故障一过,这一批都滞留到下次发布。owner 重试的 Kafka 位点早已提交,
// 收据就是那笔存盘唯一的副本。
//
// 这张表把这些收据登记下来,重试协程每拍在分拣与认领之前逐条重搬(retryStrandedReceipts,每拍上限 strandedRetryPerTick):
//   - 搬成:摘掉,补记原出口成功时的计数(onMoved);
//   - moved=false(收据已不在 processing:之前那次其实执行了、被回收、或已 ack):摘掉;
//   - 出错:留到下一拍,本拍不再继续(Redis 多半仍不可用)。
// 登记条件是「搬移返回 error 且本消费者 / worker 的 ctx 未取消」。moved=false 且 err==nil 不登记:多半是 go-redis 重发
// EVAL 时首发其实已经执行。表里的收据都已结束执行(调过或即将调 finishRetry),重搬不会与执行并发;processing 里字节相同的
// 重复副本搬走任一份都等价(LREM 1)。
//
// Stop 不需要额外处理:第⑤步 release 把整张 processing 还回收件箱(失败写回的 RetryCount 更新随之丢失,这一次尝试不计)。
// 进程崩溃时表随之丢失,靠租约过期后别的实例回收兜底。db_retry_stranded_receipts 是表长;持续 > 0 说明 Redis 一直拒绝
// 搬移(例如目的键类型不对),处置见 runbook。
//
// 并发:worker(requeueRevokedRetry、deferFailedTask、deadLetterRetry)与重试协程(其余出口)登记,只有重试协程取出重搬;
// 一把互斥锁。全部方法对 nil 接收者安全(直接拼结构体的单测没有这张表):add 返回 false,调用方退回原来的日志。

import (
	"context"
	"sync"

	"db/internal/metrics"

	"github.com/zeromicro/go-zero/core/logx"
)

// strandedRetryPerTick 是一拍最多重搬的条数(每条一次 moveRetryReceipt 脚本,脚本里要 LRANGE 本实例 processing)。
const strandedRetryPerTick = 100

// strandedRetry 是一条待重搬的收据。receipt 是 processing 里的原字节;replacement 是要放进 dest 的字节(撤销搬回、
// misrouted 与原字节相同;失败写回是 RetryCount 已更新的载荷;进死信是死信载荷)。
type strandedRetry struct {
	processingKey string
	dest          string
	receipt       []byte
	replacement   []byte
	// what 只用于日志:出口与任务标识。
	what string
	// onMoved 在重搬成功后调用一次,补记原出口成功时的计数;可为 nil。
	onMoved func()
}

type strandedRetries struct {
	mu    sync.Mutex
	items []strandedRetry
}

func newStrandedRetries() *strandedRetries { return &strandedRetries{} }

// add 登记一条待重搬的收据,receipt / replacement 复制一份。nil 接收者返回 false。
func (s *strandedRetries) add(r strandedRetry) bool {
	if s == nil {
		return false
	}
	r.receipt = append([]byte(nil), r.receipt...)
	r.replacement = append([]byte(nil), r.replacement...)
	s.mu.Lock()
	s.items = append(s.items, r)
	n := len(s.items)
	s.mu.Unlock()
	metrics.SetRetryStrandedReceipts(n)
	return true
}

// take 取出最早登记的至多 limit 条。
func (s *strandedRetries) take(limit int) []strandedRetry {
	if s == nil || limit <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := min(limit, len(s.items))
	out := append([]strandedRetry(nil), s.items[:n]...)
	s.items = append(s.items[:0:0], s.items[n:]...)
	return out
}

// restore 把没处理完的放回队首(保持登记顺序)。
func (s *strandedRetries) restore(rs []strandedRetry) {
	if s == nil || len(rs) == 0 {
		return
	}
	s.mu.Lock()
	s.items = append(append([]strandedRetry(nil), rs...), s.items...)
	s.mu.Unlock()
}

// size 是此刻的表长。
func (s *strandedRetries) size() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

// retryStrandedReceipts 是重试协程每拍的第一步:逐条重搬待重搬表里的收据(语义见文件头),最后更新 db_retry_stranded_receipts。
func (c *KeyOrderedKafkaConsumer) retryStrandedReceipts() {
	s := c.stranded
	if s == nil {
		return
	}
	defer func() { metrics.SetRetryStrandedReceipts(s.size()) }()
	batch := s.take(strandedRetryPerTick)
	for i, r := range batch {
		if c.ctx.Err() != nil || c.pullStopped() {
			// 停机:不再重搬,收据在本实例 processing,Stop 第⑤步 release 归还收件箱。
			s.restore(batch[i:])
			return
		}
		moved, err := moveRetryReceipt(c.ctx, c.redisClient, r.processingKey, r.dest, r.receipt, r.replacement)
		if err != nil {
			s.restore(batch[i:])
			if c.ctx.Err() == nil {
				logx.Errorf("stranded retry receipt still cannot be moved; retrying on the next tick: topic=%s dest=%s what=%s stranded=%d err=%v",
					c.topic, r.dest, r.what, s.size(), err)
			}
			return
		}
		if !moved {
			logx.Infof("stranded retry receipt is no longer in processing (moved, reclaimed or acked meanwhile); dropped from the table: topic=%s dest=%s what=%s",
				c.topic, r.dest, r.what)
			continue
		}
		if r.onMoved != nil {
			r.onMoved()
		}
		logx.Infof("stranded retry receipt moved: topic=%s dest=%s what=%s", c.topic, r.dest, r.what)
	}
}

// drainStranded只在worker/retry/owner循环join后执行，不依赖已取消的c.ctx或租约超时。
func (c *KeyOrderedKafkaConsumer) drainStranded(ctx context.Context) error {
	for c.stranded.size() > 0 {
		batch := c.stranded.take(strandedRetryPerTick)
		for i, r := range batch {
			_, err := moveRetryReceipt(ctx, c.redisClient, r.processingKey, r.dest, r.receipt, r.replacement)
			if err != nil {
				c.stranded.restore(batch[i:])
				return err
			}
			if r.onMoved != nil {
				r.onMoved()
			}
		}
	}
	metrics.SetRetryStrandedReceipts(0)
	return nil
}
