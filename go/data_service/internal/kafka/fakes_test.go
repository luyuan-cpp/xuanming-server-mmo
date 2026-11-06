package kafka

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"data_service/internal/store"

	rollbackpb "proto/common/rollback"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// eventLog 记录 "insert" / "commit" 的先后,用来断言"先落库、后提交"。
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// fakeReader 从 channel 取消息;FetchMessage 在 ctx 结束时返回 ctx.Err()(与 kafka-go 一致)。
type fakeReader struct {
	msgs    chan kafkago.Message
	log     *eventLog
	mu      sync.Mutex
	commits [][]kafkago.Message
	closed  bool

	// fetches 是 FetchMessage 被调用的次数(进入即计)。dispatcher 投递完第 N 条才会第 N+1 次调用,
	// 所以 fetches >= N+1 就证明前 N 条都已进了分区 worker 的通道 —— 用例据此确定性地排时序,不靠 sleep。
	fetches atomic.Int32
}

func newFakeReader(log *eventLog) *fakeReader {
	return &fakeReader{msgs: make(chan kafkago.Message, 64), log: log}
}

func (r *fakeReader) push(msgs ...kafkago.Message) {
	for _, m := range msgs {
		r.msgs <- m
	}
}

func (r *fakeReader) FetchMessage(ctx context.Context) (kafkago.Message, error) {
	r.fetches.Add(1)
	select {
	case m := <-r.msgs:
		return m, nil
	case <-ctx.Done():
		return kafkago.Message{}, ctx.Err()
	}
}

// CommitMessages 刻意不看 ctx、来者必记:真 kafka-go(v0.4.47)在 ctx 已取消时仍可能把提交请求放进队列
// (入队与 ctx.Done 同在一个 select 里随机选;入队后是否一定发出未核实),所以「ctx 取消之后不许再有提交」类用例
// (如 TestSnapshotConsumer_CancelledBatchNeverLetsQueuedMessagesCommit)靠它暴露越位提交。改成 ctx 取消即拒收会让那些用例失明。
func (r *fakeReader) CommitMessages(_ context.Context, msgs ...kafkago.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commits = append(r.commits, append([]kafkago.Message(nil), msgs...))
	if r.log != nil {
		r.log.add("commit")
	}
	return nil
}

func (r *fakeReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func (r *fakeReader) commitCalls() [][]kafkago.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]kafkago.Message, len(r.commits))
	copy(out, r.commits)
	return out
}

// committedMax 每个分区已提交的最高 offset;没提交过的分区不在 map 里。
func (r *fakeReader) committedMax() map[int]int64 {
	out := map[int]int64{}
	for _, call := range r.commitCalls() {
		for _, m := range call {
			if cur, ok := out[m.Partition]; !ok || m.Offset > cur {
				out[m.Partition] = m.Offset
			}
		}
	}
	return out
}

var errDBDown = errors.New("db down")

// fakeTxSink 记录每批行;failFirst 次先失败再成功;alwaysFail 永远失败。
type fakeTxSink struct {
	log        *eventLog
	mu         sync.Mutex
	batches    [][]*store.TransactionLogRow
	calls      int
	failFirst  int
	alwaysFail bool
}

func (s *fakeTxSink) InsertBatchIgnore(_ context.Context, rows []*store.TransactionLogRow) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.alwaysFail || s.failFirst > 0 {
		if s.failFirst > 0 {
			s.failFirst--
		}
		return 0, errDBDown
	}
	cp := make([]*store.TransactionLogRow, len(rows))
	for i, r := range rows {
		c := *r
		cp[i] = &c
	}
	s.batches = append(s.batches, cp)
	if s.log != nil {
		s.log.add("insert")
	}
	return int64(len(rows)), nil
}

func (s *fakeTxSink) batchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.batches)
}

func (s *fakeTxSink) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *fakeTxSink) batch(i int) []*store.TransactionLogRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.batches[i]
}

// fakeSnapSink 记录每行;duplicate=true 时返回 inserted=false。
//
// 批量 / 并发用例(snapshot_worker_test.go)另用下面几项。gate 同时挡单行与批量两个方法,且在 ctx 结束时
// 返回 ctx.Err() —— 真 sink 的 ExecContext 就是这样;不收 ctx 的 gate 会让"兄弟分区停机 / 关停"类用例死等。
type fakeSnapSink struct {
	log        *eventLog
	mu         sync.Mutex
	rows       []*store.SnapshotRow
	calls      int
	failFirst  int
	alwaysFail bool
	duplicate  bool

	batches         [][]*store.SnapshotRow // 成功的批量调用,每批一份拷贝(行同时追加进 rows,保持到达顺序)
	batchCalls      int                    // 批量调用次数(过了 gate 才计,含失败的)
	failBatch       bool                   // 批量调用恒失败(返回 batchErr,未设时 errDBDown)
	batchErr        error                  // failBatch 时返回的错误
	batchDuplicates int                    // 成功的批量调用回报 len(rows)-batchDuplicates 行新插入
	failGuids       map[uint64]bool        // 单行调用遇到这些 guid 恒失败
	gate            chan struct{}          // 非 nil:调用先等它可读(关闭 = 全部放行)
	gateOnly        map[uint64]bool        // 非空:只有含这些 guid 的调用才等 gate
	panicGuid       uint64                 // 非 0:含这个 guid 的调用直接 panic
	entered         int                    // 进入 sink 的调用次数(在 gate 之前计)
	inflight        int                    // 当前在 sink 里(含等 gate)的调用数
	peakInflight    int                    // inflight 的历史峰值
}

// enter 计入在途并按需等 gate。panic 注入在计数之前,免得 panic 之后 inflight 永远少减一次。
func (s *fakeSnapSink) enter(ctx context.Context, guids ...uint64) error {
	s.mu.Lock()
	panicGuid := s.panicGuid
	shouldPanic := false
	gated := s.gate != nil && len(s.gateOnly) == 0
	for _, g := range guids {
		if panicGuid != 0 && g == panicGuid {
			shouldPanic = true
		}
		if s.gate != nil && s.gateOnly[g] {
			gated = true
		}
	}
	gate := s.gate
	s.entered++
	if !shouldPanic {
		s.inflight++
		if s.inflight > s.peakInflight {
			s.peakInflight = s.inflight
		}
	}
	s.mu.Unlock()

	if shouldPanic {
		panic(fmt.Sprintf("fakeSnapSink: injected panic on guid %d", panicGuid))
	}
	if gated {
		select {
		case <-gate:
		case <-ctx.Done():
			s.leave()
			return ctx.Err()
		}
	}
	return nil
}

func (s *fakeSnapSink) leave() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight--
}

func (s *fakeSnapSink) InsertSnapshotIfGuidAbsent(ctx context.Context, row *store.SnapshotRow) (uint64, bool, error) {
	if err := s.enter(ctx, row.SnapshotGuid); err != nil {
		return 0, false, err
	}
	defer s.leave()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.alwaysFail || s.failFirst > 0 {
		if s.failFirst > 0 {
			s.failFirst--
		}
		return 0, false, errDBDown
	}
	if s.failGuids[row.SnapshotGuid] {
		return 0, false, errDBDown
	}
	c := *row
	s.rows = append(s.rows, &c)
	if s.log != nil {
		s.log.add("insert")
	}
	if s.duplicate {
		return 42, false, nil
	}
	return uint64(len(s.rows)), true, nil
}

func (s *fakeSnapSink) InsertSnapshotsIfGuidAbsent(ctx context.Context, rows []*store.SnapshotRow) (int64, error) {
	guids := make([]uint64, len(rows))
	for i, r := range rows {
		guids[i] = r.SnapshotGuid
	}
	if err := s.enter(ctx, guids...); err != nil {
		return 0, err
	}
	defer s.leave()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchCalls++
	if s.failBatch {
		if s.batchErr != nil {
			return 0, s.batchErr
		}
		return 0, errDBDown
	}
	cp := make([]*store.SnapshotRow, len(rows))
	for i, r := range rows {
		c := *r
		cp[i] = &c
	}
	s.batches = append(s.batches, cp)
	s.rows = append(s.rows, cp...)
	if s.log != nil {
		s.log.add("insert")
	}
	return int64(len(rows) - s.batchDuplicates), nil
}

func (s *fakeSnapSink) rowCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

func (s *fakeSnapSink) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *fakeSnapSink) row(i int) *store.SnapshotRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[i]
}

func (s *fakeSnapSink) batchCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.batchCalls
}

// batchesSnapshot 返回成功批量调用的副本(外层切片拷贝;行本身入库时已拷贝,用例只读)。
func (s *fakeSnapSink) batchesSnapshot() [][]*store.SnapshotRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]*store.SnapshotRow(nil), s.batches...)
}

// guidsInOrder 返回落库成功的所有行的 guid,按到达 sink 的顺序(单行与批量混排)。
func (s *fakeSnapSink) guidsInOrder() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint64, len(s.rows))
	for i, r := range s.rows {
		out[i] = r.SnapshotGuid
	}
	return out
}

func (s *fakeSnapSink) inflightNow() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight
}

func (s *fakeSnapSink) peak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peakInflight
}

func (s *fakeSnapSink) enteredCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entered
}

// ── message builders ──────────────────────────────────────────────

func txMsg(t *testing.T, partition int, offset int64, entry *rollbackpb.TransactionLogEntry) kafkago.Message {
	t.Helper()
	b, err := proto.Marshal(entry)
	require.NoError(t, err)
	return kafkago.Message{Partition: partition, Offset: offset, Key: []byte("p"), Value: b}
}

func snapMsg(t *testing.T, partition int, offset int64, entry *rollbackpb.PlayerSnapshotEntry) kafkago.Message {
	t.Helper()
	b, err := proto.Marshal(entry)
	require.NoError(t, err)
	return kafkago.Message{Partition: partition, Offset: offset, Key: []byte("p"), Value: b}
}

func badMsg(partition int, offset int64) kafkago.Message {
	// 0xFF 开头的 varint 永远不完整,proto.Unmarshal 必失败。
	return kafkago.Message{Partition: partition, Offset: offset, Key: []byte("p"), Value: []byte{0xFF, 0xFF, 0xFF}}
}

func validTx(txID uint64) *rollbackpb.TransactionLogEntry {
	return &rollbackpb.TransactionLogEntry{
		TxId:          txID,
		Timestamp:     1700000000 + txID,
		TxType:        rollbackpb.TransactionType_TX_TRADE,
		FromPlayer:    100,
		ToPlayer:      200,
		ItemUuid:      9000 + txID,
		ItemConfigId:  7,
		ItemQuantity:  3,
		CurrencyType:  1,
		CurrencyDelta: -50,
		BalanceBefore: 1000,
		BalanceAfter:  950,
		CorrelationId: 555,
		Extra:         `{"k":"v"}`,
		ZoneId:        4,
	}
}

func validSnap(guid uint64) *rollbackpb.PlayerSnapshotEntry {
	return &rollbackpb.PlayerSnapshotEntry{
		SnapshotId:         guid,
		PlayerId:           100,
		SnapshotTime:       1700000123,
		Trigger:            rollbackpb.SnapshotTrigger_SNAPSHOT_LOGIN,
		PlayerDatabaseBlob: []byte("blob-a"),
		SchemaVersion:      "v1",
		TotalBytes:         12,
		ZoneId:             4,
	}
}
