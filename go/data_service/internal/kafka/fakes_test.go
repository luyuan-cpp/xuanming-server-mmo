package kafka

import (
	"context"
	"errors"
	"sync"
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
	select {
	case m := <-r.msgs:
		return m, nil
	case <-ctx.Done():
		return kafkago.Message{}, ctx.Err()
	}
}

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
type fakeSnapSink struct {
	log        *eventLog
	mu         sync.Mutex
	rows       []*store.SnapshotRow
	calls      int
	failFirst  int
	alwaysFail bool
	duplicate  bool
}

func (s *fakeSnapSink) InsertSnapshotIfGuidAbsent(_ context.Context, row *store.SnapshotRow) (uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.alwaysFail || s.failFirst > 0 {
		if s.failFirst > 0 {
			s.failFirst--
		}
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
		SnapshotId:           guid,
		PlayerId:             100,
		SnapshotTime:         1700000123,
		Trigger:              rollbackpb.SnapshotTrigger_SNAPSHOT_LOGIN,
		PlayerDatabaseBlob:   []byte("blob-a"),
		PlayerDatabase_1Blob: []byte("blob-b"),
		SchemaVersion:        "v1",
		TotalBytes:           12,
		ZoneId:               4,
	}
}
