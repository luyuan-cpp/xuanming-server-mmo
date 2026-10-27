// Package kafka 是 data_service 的两条落库消费者:
//
//   - transaction_log_topic → transaction_log 表(攒批 INSERT IGNORE,tx_id 主键天然幂等);
//   - player_snapshot_topic → player_snapshot 表(按分区并发、可配批量(默认逐条),按 snapshot_guid 去重,source=1;
//     见 snapshot_worker.go)。
//
// 生产者是 C++ scene(全局单 topic、裸 proto、key = player_id)。形态照
// go/match/internal/kafka/result_consumer.go:kafka-go reader、FirstOffset、同步提交。
//
// 与 match 的"软数据、重试用尽就跳过"**相反**:这两张表是审计数据,DB 写不进去时
// 消费者有界重试后**停下且不提交 offset**(宁可积压不许丢),并把健康标志翻成不健康。
// 只有"消息本身坏了"(解不出 proto / 主键为 0 / 超过 MEDIUMBLOB 上限)才跳过并提交:
// 那种消息留在 topic 里只会每次重启都撞一遍。
//
// 本包不 import internal/logic / svc(避免成环),落库能力以小接口注入,单测用 fake。
package kafka

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"data_service/internal/metrics"
	"data_service/internal/store"

	databasepb "proto/common/database"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/kafkautil"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
)

// MessageReader 是 *kafkago.Reader 的最小面,便于单测注入 fake。
type MessageReader interface {
	FetchMessage(ctx context.Context) (kafkago.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafkago.Message) error
	Close() error
}

// Health 是一条消费者的健康标志。Healthy()=false 有两种含义,靠 Stopped() 区分:
// 还没启动 / 已正常退出,或者因 DB 故障停下(StoppedOnError)。
type Health struct {
	name    string
	running atomic.Bool
	failed  atomic.Bool

	mu      sync.Mutex
	lastErr error
}

func newHealth(name string) *Health {
	return &Health{name: name}
}

// Name 是消费者名("transaction_log" | "player_snapshot"),同时是指标 label。
func (h *Health) Name() string { return h.name }

// Healthy 表示消费循环仍在跑且没有因故障停下。
func (h *Health) Healthy() bool { return h.running.Load() && !h.failed.Load() }

// StoppedOnError 表示消费循环因 DB 故障主动停下(offset 未提交,重启即从断点补消费)。
func (h *Health) StoppedOnError() bool { return h.failed.Load() }

// LastError 是最近一次让消费者停下的错误;nil 表示从未故障。
func (h *Health) LastError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastErr
}

// markRunning 进入消费循环。**同时清掉上一轮的故障标记**:supervisor 重建 reader 并
// 恢复消费之后,进程确实又健康了。不清的话 Healthy() 会因为一次早已恢复的 DB 抖动
// 在整个进程生命周期里恒为 false,把"现在真的停了"这个信号淹掉。
func (h *Health) markRunning() {
	h.mu.Lock()
	h.lastErr = nil
	h.mu.Unlock()
	h.failed.Store(false)
	h.running.Store(true)
	metrics.SetKafkaConsumerUp(h.name, true)
}

func (h *Health) markStopped() {
	h.running.Store(false)
	metrics.SetKafkaConsumerUp(h.name, false)
}

func (h *Health) markFailed(err error) {
	h.mu.Lock()
	h.lastErr = err
	h.mu.Unlock()
	h.failed.Store(true)
	h.markStopped()
}

// sleepCtx 可中断的 sleep;ctx 结束返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// highestPerPartition 把一批消息压成"每个分区最高 offset 的那一条"。
// kafka-go 的 CommitMessages 按每条消息的 offset+1 提交,只交最高的那条即可覆盖整批,
// 少交 N-1 次 offset 也少一次误提交低位的机会。
func highestPerPartition(msgs []kafkago.Message) []kafkago.Message {
	if len(msgs) == 0 {
		return nil
	}
	best := make(map[int]kafkago.Message, 4)
	for _, m := range msgs {
		if cur, ok := best[m.Partition]; !ok || m.Offset > cur.Offset {
			best[m.Partition] = m
		}
	}
	out := make([]kafkago.Message, 0, len(best))
	for _, m := range best {
		out = append(out, m)
	}
	return out
}

// observeFlushAttempt 记一次 DB 尝试的耗时与结局(两条消费者共用)。ctx 已结束导致的失败不记:
// 关停 / 兄弟分区停机不是库慢,记进去会让 flush_seconds{result="error"} 在每次发布时冒一下。
func observeFlushAttempt(ctx context.Context, consumer string, start time.Time, err error) {
	if err != nil && ctx.Err() != nil {
		return
	}
	result := metrics.KafkaResultOK
	if err != nil {
		result = metrics.KafkaResultError
	}
	metrics.ObserveKafkaConsumerFlushAttempt(consumer, result, time.Since(start))
}

// isReaderOffsetTraceLine 判断 kafka-go 普通 Logger(不是 ErrorLogger)上的一行是不是分区 reader (重新)初始化时的位点记录。
// 按 format 子串放行两行(kafka-go v0.4.47 reader.go,已按 tag 源码核对,两行都只经 Logger):
// (*reader).run 每次初始化前的 "initializing kafka reader for partition %d of %s starting at offset %d"(传入的起读位点)
// 与 (*reader).initialize 里的 "the kafka reader for partition %d of %s is seeking to offset %d"(实际起读位点)。
// Logger 上其余输出一律不放行(全集未核实,白名单与全集无关)。其中 fetch 超时的 "no messages received ..." 一行
// 也只经 Logger:空闲分区上它会反复出现(频率未核实),这是用白名单而不是整个接出来的原因。
func isReaderOffsetTraceLine(format string) bool {
	return strings.Contains(format, "starting at offset") || strings.Contains(format, "seeking to offset")
}

// newReader 建 kafka-go reader。NewReader 不拨号,连不上只会让 FetchMessage 报错并按
// 退避重试,所以这里不是启动失败点。
func newReader(brokers []string, topic, group string, relay *drainproof.Session) (*kafkautil.BoundConsumerGroup, error) {
	return kafkautil.NewBoundMessageReader(kafkautil.BoundConsumerGroupConfig{
		Group: kafkago.ConsumerGroupConfig{ID: group, Brokers: brokers, Topics: []string{topic}, StartOffset: kafkago.FirstOffset, Timeout: 10 * time.Second},
		Reader: kafkago.ReaderConfig{
			Brokers: brokers,
			// 新消费组从最早开始:data_service 晚于 scene 启动时,先发的流水/快照不能丢。
			StartOffset: kafkago.FirstOffset,
			MinBytes:    1,
			MaxBytes:    16 << 20, // 快照单条几百 KB,给足
			MaxWait:     500 * time.Millisecond,
			// CommitInterval=0:同步提交,由消费循环自己决定何时提交(插库成功之后)。
			CommitInterval: 0,
			// 保留期删掉未消费的消息之后 kafka-go 会自动跳到日志起点。它留下什么记录分两种情形
			// (v0.4.47 reader.go,已按 tag 源码核对;组 / 非组模式的分区 reader 同走 (*reader).run):
			//   - 运行中被保留期追上(fetch 返回 OffsetOutOfRange 且 offset < first):经 ErrorLogger 打
			//     `... skipping from offset X to Y (N messages)`,是该情形的精确记录;
			//   - 分区 reader 每次(重新)initialize(进程重启 / STOPPED 冷却后重建 / rebalance / 断线重连)时,已提交位点早于
			//     日志起点会在 initialize 里**静默**改到起点,ErrorLogger 不出声。这条路径靠下面 Logger 放行的两行:
			//     `starting at offset A`(传入位点;负数 = 这个组没提交过,不算丢)与 `seeking to offset B`(实际起读位点),
			//     A >= 0 且 B > A 时 B − A 就是被跳过的条数。DB 故障 → 停机 → 冷却 → 重建走的正是这条。
			// 两个都不配,这一跳完全静默,而 lag 探针的 retention_gap 会在消费者越过缺口、提交之后自行归零
			// (探针的 `AUDIT DATA LOST` 行只是采样到的下界)。只加日志,不改消费行为。
			ErrorLogger: kafkago.LoggerFunc(func(format string, args ...interface{}) {
				logx.Errorf("[kafka-go] topic=%s group=%s: "+format, append([]interface{}{topic, group}, args...)...)
			}),
			// 只放行上面说的两行位点记录,其余丢弃(见 isReaderOffsetTraceLine)。量:每个分区每次(重新)初始化两行;
			// broker 不可达期间每次初始化重试多一行 `starting at offset`,与 ErrorLogger 上已有的 `error initializing` 行同频。
			Logger: kafkago.LoggerFunc(func(format string, args ...interface{}) {
				if isReaderOffsetTraceLine(format) {
					logx.Infof("[kafka-go] topic=%s group=%s: "+format, append([]interface{}{topic, group}, args...)...)
				}
			}),
		},
	}, relay)
}

// ConsumerOwner 归属同一 boot 的真实 Fetch、SQL 与 offset 提交；supervisor 重建不替换它。
// ingress 只控制拉新，relay 保留给已接收消息的 SQL/提交；未知结果不能因重建或取消清零。
type ConsumerOwner struct {
	relay          *drainproof.Session
	ingress        *drainproof.Session
	fetchCtx       context.Context
	cancelFetch    context.CancelFunc
	mu             sync.Mutex
	draining       bool
	started        bool
	unknown        error
	sqlUnknown     error
	accepted       map[messagePosition]func()
	committed      map[partitionPosition]int64
	acceptedCount  uint64
	settledCount   uint64
	closedReaders  uint64
	readerEvidence [][]byte
	readers        map[string]configuredReader
	changed        chan struct{}
	errors         chan error
}

type configuredReader struct {
	reader       MessageReader
	group, topic string
}
type partitionPosition struct {
	topic     string
	partition int
}
type messagePosition struct {
	partitionPosition
	offset int64
}

var errFetchDrained = errors.New("Kafka 新 fetch 已永久关闭")

func NewConsumerOwner(relay, ingress *drainproof.Session) (*ConsumerOwner, error) {
	if relay == nil || ingress == nil || relay == ingress {
		return nil, drainproof.ErrHold
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &ConsumerOwner{relay: relay, ingress: ingress, fetchCtx: ctx, cancelFetch: cancel,
		accepted: make(map[messagePosition]func()), committed: make(map[partitionPosition]int64),
		errors: make(chan error, 1), readers: make(map[string]configuredReader), changed: make(chan struct{})}, nil
}

func (o *ConsumerOwner) Errors() <-chan error { return o.errors }

// RecordUnknown 供同一启动 owner 的真实 panic/未知结果使用，不允许重建时清除。
func (o *ConsumerOwner) RecordUnknown(err error) { o.hold(err) }
func (o *ConsumerOwner) Draining() bool          { o.mu.Lock(); defer o.mu.Unlock(); return o.draining }
func (o *ConsumerOwner) BeginDrain() {
	o.mu.Lock()
	o.draining = true
	readers := make([]MessageReader, 0, len(o.readers))
	for _, entry := range o.readers {
		readers = append(readers, entry.reader)
	}
	close(o.changed)
	o.changed = make(chan struct{})
	o.mu.Unlock()
	o.ingress.CloseAdmission()
	o.cancelFetch()
	for _, reader := range readers {
		if owner, ok := reader.(interface{ StopFetching() }); ok {
			owner.StopFetching()
		}
	}
}
func (o *ConsumerOwner) attach() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.draining || o.started {
		return drainproof.ErrClosed
	}
	o.started = true
	return nil
}
func (o *ConsumerOwner) hold(err error) {
	if err == nil {
		err = drainproof.ErrHold
	}
	o.mu.Lock()
	first := o.unknown == nil
	if first {
		o.unknown = err
	}
	o.mu.Unlock()
	if first {
		o.relay.Hold()
		o.errors <- err
	}
	o.BeginDrain()
}
func (o *ConsumerOwner) sqlFailure() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sqlUnknown
}
func (o *ConsumerOwner) observeSQL(err error) {
	if errors.Is(err, authoritysql.ErrOutcomeUnknown) {
		o.mu.Lock()
		if o.sqlUnknown == nil {
			o.sqlUnknown = err
		}
		o.mu.Unlock()
		o.hold(err)
	}
}

// accepted 只登记已经实际返回给现有 consumer 的消息；finish 必须晚于同分区真实 Commit 成功。
func (o *ConsumerOwner) accept(msg kafkago.Message, onFinish func()) (bool, error) {
	key := messagePosition{partitionPosition: partitionPosition{topic: msg.Topic, partition: msg.Partition}, offset: msg.Offset}
	o.mu.Lock()
	defer o.mu.Unlock()
	// 同 T/P/O 的确定性重投沿用原票据，不能每次重建再制造一份未决接纳或增长回调列表。
	if o.accepted[key] != nil {
		return false, nil
	}
	finish, err := o.relay.Enter()
	if err != nil {
		return false, err
	}
	o.accepted[key] = func() { onFinish(); finish() }
	o.acceptedCount++
	return true, nil
}
func (o *ConsumerOwner) commit(msgs []kafkago.Message) {
	var finished []func()
	o.mu.Lock()
	for _, msg := range msgs {
		part := partitionPosition{topic: msg.Topic, partition: msg.Partition}
		if previous, ok := o.committed[part]; !ok || msg.Offset > previous {
			o.committed[part] = msg.Offset
		}
		for key, callback := range o.accepted {
			if key.partitionPosition == part && key.offset <= msg.Offset {
				finished = append(finished, callback)
				o.settledCount++
				delete(o.accepted, key)
			}
		}
	}
	o.mu.Unlock()
	for _, finish := range finished {
		finish()
	}
}

// SettledDigest 只证明本 owner 实际接收消息的 SQL/提交前沿；不作为 P8 分区分配就绪证明。
func (o *ConsumerOwner) SettledDigest(ctx context.Context) ([]byte, error) {
	if err := o.ingress.Wait(ctx); err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.unknown != nil {
		return nil, errors.Join(drainproof.ErrHold, o.unknown)
	}
	if len(o.accepted) != 0 {
		return nil, fmt.Errorf("%w: Kafka 仍有 %d 个未确定提交的位置", drainproof.ErrHold, len(o.accepted))
	}
	keys := make([]partitionPosition, 0, len(o.committed))
	for key := range o.committed {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].topic != keys[j].topic {
			return keys[i].topic < keys[j].topic
		}
		return keys[i].partition < keys[j].partition
	})
	h := sha256.New()
	var value [8]byte
	for _, count := range []uint64{o.acceptedCount, o.settledCount, o.closedReaders} {
		binary.BigEndian.PutUint64(value[:], count)
		h.Write(value[:])
	}
	for _, key := range keys {
		binary.BigEndian.PutUint64(value[:], uint64(len(key.topic)))
		h.Write(value[:])
		h.Write([]byte(key.topic))
		binary.BigEndian.PutUint64(value[:], uint64(key.partition))
		h.Write(value[:])
		binary.BigEndian.PutUint64(value[:], uint64(o.committed[key]))
		h.Write(value[:])
	}
	sort.Slice(o.readerEvidence, func(i, j int) bool { return bytes.Compare(o.readerEvidence[i], o.readerEvidence[j]) < 0 })
	for _, proof := range o.readerEvidence {
		h.Write(proof)
	}
	return h.Sum(nil), nil
}

func (o *ConsumerOwner) bindReader(name, group, topic string, reader MessageReader) {
	o.mu.Lock()
	o.readers[name] = configuredReader{reader: reader, group: group, topic: topic}
	draining := o.draining
	close(o.changed)
	o.changed = make(chan struct{})
	o.mu.Unlock()
	if draining {
		if owner, ok := reader.(interface{ StopFetching() }); ok {
			owner.StopFetching()
		}
	}
}

// PreparedFrontier 仅接受实际 consumer-group owner 的正式原件；普通 Reader 没有该证明时明确 HOLD。
func (o *ConsumerOwner) PreparedFrontier(ctx context.Context, runtime *drainproof.Runtime, role string) ([]byte, []byte, error) {
	var entries []configuredReader
	for {
		o.mu.Lock()
		if o.draining || o.unknown != nil {
			o.mu.Unlock()
			return nil, nil, drainproof.ErrHold
		}
		if len(o.readers) == 2 {
			for _, name := range []string{ConsumerTxLog, ConsumerSnapshot} {
				entry, ok := o.readers[name]
				if !ok {
					o.mu.Unlock()
					return nil, nil, drainproof.ErrHold
				}
				entries = append(entries, entry)
			}
			o.mu.Unlock()
			break
		}
		changed := o.changed
		o.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-changed:
		}
	}
	if runtime == nil || runtime.Plan == nil {
		return nil, nil, drainproof.ErrHold
	}
	var reports [][]byte
	for _, entry := range entries {
		provider, ok := entry.reader.(interface {
			ExpectedPartitions(*drainproof.Runtime) ([]*databasepb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure, error)
			PreparedFrontier(context.Context, *drainproof.Runtime, string, []*databasepb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure) ([]byte, []byte, error)
		})
		if !ok {
			return nil, nil, fmt.Errorf("%w: DS Reader 缺保留攒批语义的实际 assigned/generation/commit 前沿 API", drainproof.ErrHold)
		}
		expected, err := provider.ExpectedPartitions(runtime)
		if err != nil {
			return nil, nil, err
		}
		_, raw, err := provider.PreparedFrontier(ctx, runtime, role, expected)
		if err != nil {
			return nil, nil, err
		}
		reports = append(reports, raw)
	}
	return kafkautil.MergeConsumerReadiness(reports...)
}

// WithReady 将同 boot 的未知状态、全部实际组代次核验与 root open 放在同一临界区。
func (o *ConsumerOwner) WithReady(ctx context.Context, raw []byte, commit func() ([]byte, error)) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.draining || o.unknown != nil || len(o.readers) != 2 {
		return nil, drainproof.ErrHold
	}
	var owners []*kafkautil.BoundConsumerGroup
	for _, name := range []string{ConsumerTxLog, ConsumerSnapshot} {
		entry, ok := o.readers[name]
		if !ok {
			return nil, drainproof.ErrHold
		}
		owner, ok := entry.reader.(*kafkautil.BoundConsumerGroup)
		if !ok {
			return nil, drainproof.ErrHold
		}
		owners = append(owners, owner)
	}
	return kafkautil.WithConsumerReadiness(ctx, raw, owners, commit)
}

type ownedRunner struct {
	runner
	ctx    context.Context
	cancel context.CancelFunc
}

func (r *ownedRunner) Run(context.Context) error { defer r.cancel(); return r.runner.Run(r.ctx) }

type ownedReader struct {
	reader    MessageReader
	owner     *ConsumerOwner
	txlog     bool
	stopRun   context.CancelFunc
	pending   atomic.Int64
	closeOnce sync.Once
	closeErr  error
	activeMu  sync.Mutex
	active    map[messagePosition]kafkago.Message
	groupDone chan struct{}
	groupErr  error
}

func (r *ownedReader) drainFetch(ctx context.Context) (kafkago.Message, error) {
	if !r.txlog {
		return kafkago.Message{}, errFetchDrained
	}
	if r.pending.Load() == 0 {
		r.stopRun()
		return kafkago.Message{}, context.Canceled
	}
	// 现有 TxLogConsumer 的批截止到点后会走原 flush；不取消它手里的 SQL/offset 上下文。
	if _, ok := ctx.Deadline(); !ok {
		r.owner.hold(errors.New("txlog 已接受但未提交的批已离开原处理栈"))
		r.stopRun()
		return kafkago.Message{}, drainproof.ErrHold
	}
	<-ctx.Done()
	return kafkago.Message{}, ctx.Err()
}

func (r *ownedReader) FetchMessage(ctx context.Context) (kafkago.Message, error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.owner.hold(errors.New("Kafka Fetch panic，接收边界未知"))
			panic(recovered)
		}
	}()
	finishFetch, err := r.owner.ingress.Enter()
	if err != nil {
		return r.drainFetch(ctx)
	}
	defer finishFetch()
	fetchCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.owner.fetchCtx, cancel)
	msg, err := r.reader.FetchMessage(fetchCtx)
	stop()
	cancel()
	if err != nil {
		if r.owner.Draining() {
			return r.drainFetch(ctx)
		}
		return msg, err
	}
	fresh, err := r.owner.accept(msg, func() { r.pending.Add(-1) })
	if err != nil {
		r.owner.hold(err)
		return kafkago.Message{}, err
	}
	if fresh {
		r.pending.Add(1)
	}
	r.activeMu.Lock()
	if r.active == nil {
		r.active = make(map[messagePosition]kafkago.Message)
	}
	position := messagePosition{partitionPosition: partitionPosition{topic: msg.Topic, partition: msg.Partition}, offset: msg.Offset}
	r.active[position] = kafkago.Message{Topic: msg.Topic, Partition: msg.Partition, Offset: msg.Offset}
	r.activeMu.Unlock()
	return msg, nil
}
func (r *ownedReader) CommitMessages(ctx context.Context, msgs ...kafkago.Message) error {
	commitCtx := ctx
	cancel := func() {}
	if ctx.Err() != nil {
		// 调用点已确认这是持久前缀；兄弟 worker 取消不能撤销该 SQL 或跳过原 offset 确认。
		commitCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	}
	defer cancel()
	err := r.reader.CommitMessages(commitCtx, msgs...)
	if err != nil {
		r.owner.hold(err)
		return err
	}
	r.activeMu.Lock()
	for _, message := range msgs {
		for position := range r.active {
			if position.topic == message.Topic && position.partition == message.Partition && position.offset <= message.Offset {
				delete(r.active, position)
			}
		}
	}
	r.activeMu.Unlock()
	r.owner.commit(msgs)
	return nil
}
func (r *ownedReader) Close() error {
	r.closeOnce.Do(func() {
		var raw []byte
		if reporter, ok := r.reader.(interface {
			CloseAndReport(context.Context) ([]byte, error)
		}); ok {
			raw, r.closeErr = reporter.CloseAndReport(context.Background())
		} else {
			r.closeErr = r.reader.Close()
		}
		if r.groupDone != nil {
			<-r.groupDone
			r.closeErr = errors.Join(r.closeErr, r.groupErr)
		}
		if r.closeErr != nil {
			r.owner.hold(r.closeErr)
			return
		}
		r.owner.mu.Lock()
		r.owner.closedReaders++
		if len(raw) > 0 {
			sum := sha256.Sum256(raw)
			r.owner.readerEvidence = append(r.owner.readerEvidence, append([]byte(nil), sum[:]...))
		}
		r.owner.mu.Unlock()
	})
	return r.closeErr
}

func (o *ConsumerOwner) effect(ctx context.Context, call func() error) (err error) {
	if err = o.sqlFailure(); err != nil {
		return err
	}
	finish, err := o.relay.Enter()
	if err != nil {
		return err
	}
	returned := false
	defer func() {
		if !returned {
			o.hold(errors.New("Kafka SQL owner panic，服务器结果未知"))
		}
		finish()
	}()
	err = call()
	returned = true
	o.observeSQL(err)
	return err
}

type ownedTxLogSink struct {
	owner *ConsumerOwner
	sink  TxLogSink
}

func (s *ownedTxLogSink) InsertBatchIgnore(ctx context.Context, rows []*store.TransactionLogRow) (out int64, err error) {
	err = s.owner.effect(ctx, func() error { var e error; out, e = s.sink.InsertBatchIgnore(ctx, rows); return e })
	return
}

type ownedSnapshotSink struct {
	owner *ConsumerOwner
	sink  SnapshotSink
}

func (s *ownedSnapshotSink) InsertSnapshotIfGuidAbsent(ctx context.Context, row *store.SnapshotRow) (id uint64, inserted bool, err error) {
	err = s.owner.effect(ctx, func() error { var e error; id, inserted, e = s.sink.InsertSnapshotIfGuidAbsent(ctx, row); return e })
	return
}
func (s *ownedSnapshotSink) InsertSnapshotsIfGuidAbsent(ctx context.Context, rows []*store.SnapshotRow) (out int64, err error) {
	err = s.owner.effect(ctx, func() error { var e error; out, e = s.sink.InsertSnapshotsIfGuidAbsent(ctx, rows); return e })
	return
}

// startGroup 只在唯一工厂第一次创建此 Reader 时调用；业务 consumer 重建不会重开组或丢票据。
func (r *ownedReader) startGroup(ctx context.Context, group *kafkautil.BoundConsumerGroup) {
	r.groupDone = make(chan struct{})
	go func() {
		defer close(r.groupDone)
		defer func() {
			if recovered := recover(); recovered != nil {
				r.groupErr = errors.New("Kafka group owner panic，结果未知")
				r.owner.RecordUnknown(r.groupErr)
			}
		}()
		r.groupErr = group.Run(ctx)
		if r.groupErr != nil {
			r.owner.RecordUnknown(r.groupErr)
		}
	}()
}

// finishAttempt 在原有批/分区 worker 全部返回后报告未提交前缀。
// 成功 SQL 的前缀已由 CommitMessages 确认；driver 明确 UNKNOWN 或任何提交不明时只能报告 UNKNOWN。
func (r *ownedReader) finishAttempt(result error) error {
	r.activeMu.Lock()
	messages := make([]kafkago.Message, 0, len(r.active))
	for _, message := range r.active {
		messages = append(messages, message)
	}
	r.activeMu.Unlock()
	if len(messages) == 0 {
		return nil
	}
	outcome := kafkautil.ProcessingOutcomeUnknown
	r.owner.mu.Lock()
	unknown := r.owner.unknown
	r.owner.mu.Unlock()
	if errors.Is(result, ErrConsumerStoppedOnDB) && unknown == nil {
		outcome = kafkautil.ProcessingNotCommitted
	}
	group, ok := r.reader.(*kafkautil.BoundConsumerGroup)
	if !ok {
		return drainproof.ErrHold
	}
	reportErr := group.ReportProcessingFailure(context.Background(), outcome, messages...)
	if outcome == kafkautil.ProcessingOutcomeUnknown || reportErr != nil {
		r.owner.RecordUnknown(errors.Join(drainproof.ErrHold, result, reportErr))
		return errors.Join(drainproof.ErrHold, result, reportErr)
	}
	r.activeMu.Lock()
	clear(r.active)
	r.activeMu.Unlock()
	return nil
}
