package kafka

// 两条审计消费者(player_snapshot、transaction_log)共用的「按分区派发」骨架。
// 2026-11-06 从 snapshot_worker.go 抽出,流水消费者也改成按分区并行落库(缺口清单第 15 条,
// docs/design/audit-pipeline-horizontal-scale-20261106.md §2.B)。
//
//	kafka-go group Reader(每实例一个)
//	        │ FetchMessage
//	   dispatcher(runPartitionWorkers 所在 goroutine):map[partition]通道,分区 worker 懒创建
//	        │
//	   worker p0 / p1 / …   每个 worker:FIFO 通道 → 攒批 → 先落库、后提交本分区本批最高 offset
//	        │
//	   落库名额(实例级 MaxInflightFlushes,runFlushAttempt)→ 库
//
// 不变量(「不丢」的全部依据):任何分区的任何一次提交 V,都满足「该分区 offset < V 的有效消息都已落库」。
//   - 同一分区只进同一个 worker 的通道,通道 FIFO 是分区内顺序的唯一来源;
//   - worker 只在落库成功后提交、只提交已落库前缀;落库没成功(含 ctx 取消导致的安静退出)的批一律不提交,
//     而且这个 worker 立刻退出,**绝不**再处理后面的消息 —— 后面的批一旦提交,就越过了没落库的这一批;
//   - 一个分区停下(重试用尽 / panic)就取消整个 Run,supervisor 冷却 ResumeAfter 后从各分区的断点重建;
//   - 维护排空(ownedReader 返回 errFetchDrained)时关掉各分区通道,worker 把通道里剩下的落完、提交后退出;
//     其它原因结束(进程关停、兄弟分区停机)时取消 worker,手里没落库的批不提交,重建 / 重启后重放,由主键 / 唯一键去重。
// 返回前一定等所有 worker 退出:supervisor 随后才 Close reader,不会出现「reader 已关、worker 还在提交」。

import (
	"context"
	"errors"
	"sync"
	"time"

	"shared/safego"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
)

// errPartitionWorkerAborted 是分区 worker panic 的公共原因。各消费者自己的 aborted 错误都包着它
// (errSnapshotWorkerAborted、errTxLogWorkerAborted),supervisor 按它 hold 排空框架:panic 时这一批的结局未知。
var errPartitionWorkerAborted = errors.New("kafka partition worker aborted (panic)")

// partitionWorkerStarter 给一个分区起 worker,返回它的 FIFO 输入通道。由各消费者实现,内部用 goPartitionWorker。
type partitionWorkerStarter func(runCtx context.Context, partition int, wg *sync.WaitGroup, stop *runStop) chan<- kafkago.Message

// runPartitionWorkers 阻塞派发,直到 ctx 结束、排空完成或任一分区 worker 停下;返回第一个停下的原因
// (ctx 结束、排空完成时为 nil)。返回前一定等所有分区 worker 退出。name 只用于日志。
func runPartitionWorkers(ctx context.Context, name string, reader MessageReader, fetchBackoff time.Duration, start partitionWorkerStarter) error {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	stop := &runStop{cancel: cancelRun}
	var wg sync.WaitGroup
	// dispatcher panic 时也先取消并 join 分区 worker,再让 supervisor 关闭 reader。
	defer func() { cancelRun(); wg.Wait() }()
	workers := make(map[int]chan<- kafkago.Message)
	graceful := false

	for runCtx.Err() == nil {
		msg, err := reader.FetchMessage(runCtx)
		if err != nil {
			if errors.Is(err, errFetchDrained) {
				graceful = true
				break
			}
			if runCtx.Err() != nil {
				break
			}
			logx.Errorf("[%s] fetch failed, retry in %s: %v", name, fetchBackoff, err)
			sleepCtx(runCtx, fetchBackoff)
			continue
		}
		in := workers[msg.Partition]
		if in == nil {
			in = start(runCtx, msg.Partition, &wg, stop)
			workers[msg.Partition] = in
		}
		select {
		case in <- msg:
		case <-runCtx.Done():
			// 某个分区已停(或进程关停):这条不投递、不提交,重建 / 重启后从断点重放。
		}
	}
	if graceful {
		for _, in := range workers {
			close(in)
		}
	} else {
		cancelRun()
	}
	wg.Wait()
	return stop.firstErr()
}

// goPartitionWorker 在 safego 点位 point(常量,不拼分区号:point 是 Prometheus label)上起一个分区 worker。
// run 返回错误就停下整个 Run;panic 展开时(早于 safego 的 recover)以 aborted 停下整个 Run ——
// 不让一个死掉的分区在其它分区继续跑时静默停摆。
func goPartitionWorker(runCtx context.Context, point string, aborted error, wg *sync.WaitGroup, stop *runStop, run func(context.Context) error) {
	wg.Add(1)
	safego.Go(point, func() {
		defer wg.Done()
		completed := false
		defer func() {
			if !completed {
				stop.fail(aborted)
			}
		}()
		err := run(runCtx)
		completed = true
		if err != nil {
			stop.fail(err)
		}
	})
}

// runStop 记第一次失败并取消整个 Run。err 在 wg.Wait 之后读:worker 的 fail 发生在它自己的 wg.Done 之前
// (两个 defer 的注册顺序保证),wg.Wait 给出 happens-before。
type runStop struct {
	once   sync.Once
	err    error
	cancel context.CancelFunc
}

func (s *runStop) fail(err error) {
	s.once.Do(func() { s.err = err })
	s.cancel()
}

func (s *runStop) firstErr() error { return s.err }

// runFlushAttempt 跑一次 DB 尝试:取实例级落库名额(sem,容量 = MaxInflightFlushes)→ op → 归还
// (defer:sink panic 时也归还)→ 记单次尝试耗时。有它,「本实例同时写几个分区」才有界,
// 池里总留得出连接给 GM 查询 / 回档读。
// 取名额期间 ctx 结束(关停 / 兄弟分区停机)直接返回 ctx 的原因,不碰库、不计耗时。
// 耗时只算拿到名额之后这一段:名额排队与重试退避都不算,flush_seconds 慢才等于库慢。
func runFlushAttempt(ctx context.Context, sem chan struct{}, consumer string, op func() error) error {
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-sem }()
	if err := ctx.Err(); err != nil {
		// 名额与 ctx.Done 同时就绪时 select 随机选,选中名额也不能再去碰库。
		return err
	}
	start := time.Now()
	err := op()
	observeFlushAttempt(ctx, consumer, start, err)
	return err
}
