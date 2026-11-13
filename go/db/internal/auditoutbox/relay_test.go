package auditoutbox

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	storepb "proto/player_store"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/workshard"
)

const (
	txlogKind    = storepb.PlayerAuditKind_PLAYER_AUDIT_KIND_TRANSACTION_LOG
	snapshotKind = storepb.PlayerAuditKind_PLAYER_AUDIT_KIND_PLAYER_SNAPSHOT
	testStorage  = uint32(1000000)
)

// fakeOutbox 是内存里的一个存档库发件箱,带一张和 GET_LOCK 语义相同的锁表(每片至多一个持有者,不等待)。
// 几个 Relay 共用同一个 fakeOutbox,就是几个 go/db 副本连同一个库。
type fakeOutbox struct {
	mu    sync.Mutex
	rows  map[unitKind][]Row
	locks map[uint32]string
	// probes 记每片被探测的次数。
	probes    map[uint32]int
	deleteErr error
}

// rowKey 是一行的主键里片号、kind 之外的部分。
type rowKey struct{ createdMs, playerID, recordID uint64 }

func keyOf(row Row) rowKey { return rowKey{row.CreatedMs, row.PlayerID, row.RecordID} }

type unitKind struct {
	shard uint32
	kind  storepb.PlayerAuditKind
}

func newFakeOutbox() *fakeOutbox {
	return &fakeOutbox{rows: map[unitKind][]Row{}, locks: map[uint32]string{}, probes: map[uint32]int{}}
}

// add 按主键序插入行。
func (f *fakeOutbox) add(shard uint32, kind storepb.PlayerAuditKind, rows ...Row) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := unitKind{shard: shard, kind: kind}
	f.rows[key] = append(f.rows[key], rows...)
	slices.SortFunc(f.rows[key], comparePK)
}

func comparePK(a, b Row) int {
	switch {
	case a.CreatedMs != b.CreatedMs:
		return cmpUint(a.CreatedMs, b.CreatedMs)
	case a.PlayerID != b.PlayerID:
		return cmpUint(a.PlayerID, b.PlayerID)
	default:
		return cmpUint(a.RecordID, b.RecordID)
	}
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (f *fakeOutbox) remaining(shard uint32, kind storepb.PlayerAuditKind) []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []uint64
	for _, row := range f.rows[unitKind{shard: shard, kind: kind}] {
		ids = append(ids, row.RecordID)
	}
	return ids
}

func (f *fakeOutbox) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, rows := range f.rows {
		n += len(rows)
	}
	return n
}

func (f *fakeOutbox) probeCount(shard uint32) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probes[shard]
}

// store 返回 holder 这个副本看到的 Store。
func (f *fakeOutbox) store(holder string) Store { return &fakeStore{outbox: f, holder: holder} }

type fakeStore struct {
	outbox *fakeOutbox
	holder string
}

func (s *fakeStore) Oldest(_ context.Context, shard uint32, kind storepb.PlayerAuditKind, from uint64) (uint64, bool, error) {
	f := s.outbox
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes[shard]++
	for _, row := range f.rows[unitKind{shard: shard, kind: kind}] {
		if row.CreatedMs >= from {
			return row.CreatedMs, true, nil
		}
	}
	return 0, false, nil
}

func (s *fakeStore) LockUnit(_ context.Context, shard uint32) (UnitLock, bool, error) {
	f := s.outbox
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, busy := f.locks[shard]; busy {
		return nil, false, nil
	}
	f.locks[shard] = s.holder
	return &fakeLock{outbox: f, shard: shard}, true, nil
}

type fakeLock struct {
	outbox   *fakeOutbox
	shard    uint32
	released bool
}

func (l *fakeLock) Read(_ context.Context, kind storepb.PlayerAuditKind, from uint64, limit int) ([]Row, error) {
	f := l.outbox
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Row
	for _, row := range f.rows[unitKind{shard: l.shard, kind: kind}] {
		if row.CreatedMs >= from && len(out) < limit {
			out = append(out, row)
		}
	}
	return out, nil
}

func (l *fakeLock) Delete(_ context.Context, kind storepb.PlayerAuditKind, rows []Row) error {
	f := l.outbox
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	gone := make(map[rowKey]struct{}, len(rows))
	for _, row := range rows {
		gone[keyOf(row)] = struct{}{}
	}
	key := unitKind{shard: l.shard, kind: kind}
	f.rows[key] = slices.DeleteFunc(f.rows[key], func(row Row) bool {
		_, deleted := gone[keyOf(row)]
		return deleted
	})
	return nil
}

func (l *fakeLock) Release() {
	f := l.outbox
	f.mu.Lock()
	defer f.mu.Unlock()
	if l.released {
		panic("lock released twice")
	}
	l.released = true
	delete(f.locks, l.shard)
}

// fakeProducer 记下每条记录被确认的次数。fail 回非 nil 的行算没投成。
type fakeProducer struct {
	mu      sync.Mutex
	acked   map[uint64]int
	fail    func(kind storepb.PlayerAuditKind, row Row) error
	calls   int
	started chan struct{} // 非 nil:每次调用先通知一次
	proceed chan struct{} // 非 nil:通知之后等它放行
}

func newFakeProducer() *fakeProducer { return &fakeProducer{acked: map[uint64]int{}} }

func (p *fakeProducer) Produce(_ context.Context, kind storepb.PlayerAuditKind, rows []Row) []error {
	if p.started != nil {
		p.started <- struct{}{}
	}
	if p.proceed != nil {
		<-p.proceed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	errs := make([]error, len(rows))
	for i, row := range rows {
		if p.fail != nil {
			if err := p.fail(kind, row); err != nil {
				errs[i] = err
				continue
			}
		}
		p.acked[row.RecordID]++
	}
	return errs
}

func (p *fakeProducer) ackCount(recordID uint64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acked[recordID]
}

// fakeClock 是单测的墙钟,只在测试 goroutine 里推进。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newClock() *fakeClock { return &fakeClock{now: time.UnixMilli(1_800_000_000_000)} }

// rowsAt 造 n 行,created_ms 都是 createdMs,记录号从 firstRecord 起。
func rowsAt(createdMs uint64, firstRecord uint64, n int) []Row {
	rows := make([]Row, n)
	for i := range rows {
		rows[i] = Row{CreatedMs: createdMs, PlayerID: 7, RecordID: firstRecord + uint64(i), Payload: []byte("entry")}
	}
	return rows
}

func newTestRelay(t *testing.T, outbox *fakeOutbox, holder string, producer Producer, clock *fakeClock, mutate func(*Options)) *Relay {
	t.Helper()
	opts := Options{
		StorageIDs: func() []uint32 { return []uint32{testStorage} },
		Open:       func(context.Context, uint32) (Store, error) { return outbox.store(holder), nil },
		Producer:   producer,
		Interval:   time.Second,
		Now:        clock.Now,
		Jitter:     func() float64 { return 1 },
	}
	if mutate != nil {
		mutate(&opts)
	}
	relay, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return relay
}

// Kafka 没确认的行不删;确认之后才删,而且只删一次。
func TestRelay_DeletesOnlyAfterKafkaAcknowledges(t *testing.T) {
	clock := newClock()
	outbox := newFakeOutbox()
	nowMs := uint64(clock.Now().UnixMilli())
	outbox.add(5, txlogKind, rowsAt(nowMs-1000, 1, 3)...)
	producer := newFakeProducer()
	producer.fail = func(storepb.PlayerAuditKind, Row) error { return errors.New("broker down") }
	relay := newTestRelay(t, outbox, "a", producer, clock, nil)

	result := relay.Pass(context.Background())
	if !result.ProduceFailed || result.Forwarded != 0 {
		t.Fatalf("result = %+v, want a failed produce with nothing forwarded", result)
	}
	if got := outbox.remaining(5, txlogKind); !slices.Equal(got, []uint64{1, 2, 3}) {
		t.Fatalf("rows not acknowledged by Kafka must stay, remaining = %v", got)
	}
	if delay := relay.delayAfter(result); delay != backoffMin {
		t.Fatalf("first backoff = %v, want %v", delay, backoffMin)
	}

	producer.fail = nil
	result = relay.Pass(context.Background())
	if result.ProduceFailed || result.Forwarded != 3 {
		t.Fatalf("result = %+v, want 3 forwarded", result)
	}
	if got := outbox.remaining(5, txlogKind); len(got) != 0 {
		t.Fatalf("acknowledged rows must be deleted, remaining = %v", got)
	}
	for record := uint64(1); record <= 3; record++ {
		if n := producer.ackCount(record); n != 1 {
			t.Fatalf("record %d acknowledged %d times, want 1", record, n)
		}
	}
}

// 一批里个别行被拒:投成功的照删,被拒的留下,不退避;下一轮只重投被拒的那行。
func TestRelay_PartialFailureKeepsOnlyTheUnacknowledgedRows(t *testing.T) {
	clock := newClock()
	outbox := newFakeOutbox()
	nowMs := uint64(clock.Now().UnixMilli())
	outbox.add(2, txlogKind, rowsAt(nowMs-1000, 1, 3)...)
	producer := newFakeProducer()
	producer.fail = func(_ storepb.PlayerAuditKind, row Row) error {
		if row.RecordID == 2 {
			return errors.New("record rejected")
		}
		return nil
	}
	relay := newTestRelay(t, outbox, "a", producer, clock, nil)

	result := relay.Pass(context.Background())
	if result.ProduceFailed || result.Forwarded != 2 {
		t.Fatalf("result = %+v, want 2 forwarded and no backoff", result)
	}
	if got := outbox.remaining(2, txlogKind); !slices.Equal(got, []uint64{2}) {
		t.Fatalf("remaining = %v, want only the rejected record", got)
	}
	producer.fail = nil
	relay.Pass(context.Background())
	if got := outbox.remaining(2, txlogKind); len(got) != 0 {
		t.Fatalf("remaining = %v after the retry", got)
	}
	if producer.ackCount(1) != 1 || producer.ackCount(2) != 1 || producer.ackCount(3) != 1 {
		t.Fatalf("acks = %v, want each record exactly once", producer.acked)
	}
}

// 另一个实例持有一片的锁:这一片整轮不碰(不投、不删),别的片照常;锁放了之后这一片照常投。
func TestRelay_SkipsAUnitLockedByAnotherInstance(t *testing.T) {
	clock := newClock()
	outbox := newFakeOutbox()
	nowMs := uint64(clock.Now().UnixMilli())
	outbox.add(1, txlogKind, rowsAt(nowMs-1000, 10, 2)...)
	outbox.add(2, txlogKind, rowsAt(nowMs-1000, 20, 2)...)
	outbox.locks[1] = "other"
	producer := newFakeProducer()
	relay := newTestRelay(t, outbox, "a", producer, clock, nil)

	relay.Pass(context.Background())
	if got := outbox.remaining(1, txlogKind); !slices.Equal(got, []uint64{10, 11}) {
		t.Fatalf("a unit locked by another instance must not be touched, remaining = %v", got)
	}
	if producer.ackCount(10) != 0 || producer.ackCount(11) != 0 {
		t.Fatal("rows of a locked unit must not be produced")
	}
	if got := outbox.remaining(2, txlogKind); len(got) != 0 {
		t.Fatalf("unlocked unit must be forwarded, remaining = %v", got)
	}

	delete(outbox.locks, 1)
	relay.Pass(context.Background())
	if got := outbox.remaining(1, txlogKind); len(got) != 0 {
		t.Fatalf("after the other instance released the lock, remaining = %v", got)
	}
}

// 两个副本都认为自己负责全部片(分工表变化的窗口),并发转发:每片的锁保证每条记录只被投一次,发件箱最后清空。
func TestRelay_ConcurrentReplicasForwardEachRecordOnce(t *testing.T) {
	clock := newClock()
	outbox := newFakeOutbox()
	nowMs := uint64(clock.Now().UnixMilli())
	record := uint64(1)
	for shard := uint32(0); shard < 16; shard++ {
		outbox.add(shard, txlogKind, rowsAt(nowMs-1000, record, 30)...)
		record += 30
		outbox.add(shard, snapshotKind, rowsAt(nowMs-1000, record, 5)...)
		record += 5
	}
	producer := newFakeProducer()
	relays := []*Relay{
		newTestRelay(t, outbox, "a", producer, clock, nil),
		newTestRelay(t, outbox, "b", producer, clock, nil),
	}
	var wg sync.WaitGroup
	for _, relay := range relays {
		wg.Add(1)
		go func(relay *Relay) {
			defer wg.Done()
			for pass := 0; pass < 20 && outbox.total() > 0; pass++ {
				relay.Pass(context.Background())
			}
		}(relay)
	}
	wg.Wait()
	if n := outbox.total(); n != 0 {
		t.Fatalf("%d rows left after both replicas drained the outbox", n)
	}
	for id := uint64(1); id < record; id++ {
		if n := producer.ackCount(id); n != 1 {
			t.Fatalf("record %d produced %d times, want exactly once", id, n)
		}
	}
	if len(outbox.locks) != 0 {
		t.Fatalf("locks still held: %v", outbox.locks)
	}
}

// 按最高随机权重哈希分工:两个副本各做各的片,不重叠、不遗漏;不负责的片连探测都不做。
func TestRelay_ShardOwnershipSplitsUnitsWithoutOverlap(t *testing.T) {
	clock := newClock()
	outbox := newFakeOutbox()
	nowMs := uint64(clock.Now().UnixMilli())
	for shard := uint32(0); shard < 16; shard++ {
		outbox.add(shard, txlogKind, rowsAt(nowMs-1000, uint64(shard)*100+1, 1)...)
	}
	members := []string{"replica-a", "replica-b"}
	producers := map[string]*fakeProducer{"replica-a": newFakeProducer(), "replica-b": newFakeProducer()}
	for _, self := range members {
		relay := newTestRelay(t, outbox, self, producers[self], clock, func(o *Options) {
			o.Owns = func(key string) bool { return workshard.Owns(self, key, members) }
		})
		relay.Pass(context.Background())
	}
	if n := outbox.total(); n != 0 {
		t.Fatalf("%d rows left: some unit is owned by nobody", n)
	}
	for shard := uint32(0); shard < 16; shard++ {
		// 只有负责的副本探测这一片(每种记录一次):不负责的片不读库。
		if got := outbox.probeCount(shard); got != kindCount {
			t.Fatalf("shard %d probed %d times, want %d (only by its owner)", shard, got, kindCount)
		}
		owner := workshard.Owner(UnitKey(testStorage, shard), members)
		record := uint64(shard)*100 + 1
		for _, self := range members {
			want := 0
			if self == owner {
				want = 1
			}
			if got := producers[self].ackCount(record); got != want {
				t.Fatalf("shard %d: %s produced record %d %d times, want %d (owner %s)", shard, self, record, got, want, owner)
			}
		}
	}
}

// 维护排空:RELAY 关了之后不再开始新的单位;Wait 等在途的那个单位做完才返回。
func TestRelay_DrainWaitsForTheInFlightUnitThenStops(t *testing.T) {
	clock := newClock()
	outbox := newFakeOutbox()
	nowMs := uint64(clock.Now().UnixMilli())
	outbox.add(0, txlogKind, rowsAt(nowMs-1000, 1, 1)...)
	outbox.add(9, txlogKind, rowsAt(nowMs-1000, 2, 1)...)
	producer := newFakeProducer()
	producer.started = make(chan struct{})
	producer.proceed = make(chan struct{})
	session := drainproof.New()
	relay := newTestRelay(t, outbox, "a", producer, clock, func(o *Options) { o.Relay = session })

	passDone := make(chan PassResult)
	go func() { passDone <- relay.Pass(context.Background()) }()
	<-producer.started // 第 0 片的那一批正在投
	session.CloseAdmission()
	waitDone := make(chan error)
	go func() { waitDone <- session.Wait(context.Background()) }()
	select {
	case err := <-waitDone:
		t.Fatalf("drain returned (%v) while a unit was still being forwarded", err)
	case <-time.After(50 * time.Millisecond):
	}
	producer.proceed <- struct{}{}
	result := <-passDone
	if err := <-waitDone; err != nil {
		t.Fatalf("drain = %v", err)
	}
	if result.Forwarded != 1 {
		t.Fatalf("result = %+v, want only the in-flight unit forwarded", result)
	}
	if got := outbox.remaining(9, txlogKind); !slices.Equal(got, []uint64{2}) {
		t.Fatalf("no new unit may start after the drain closed the relay, remaining = %v", got)
	}
	probes := outbox.probeCount(9)
	relay.Pass(context.Background())
	if outbox.probeCount(9) != probes {
		t.Fatal("a pass after the drain must not touch the outbox")
	}
}

// 写者闸门冻结(搬库切换窗口):删不掉的行留着,不算 Kafka 故障;这个库暂停一会儿,不在窗口里反复投。
func TestRelay_FrozenWriterGatePausesTheStorage(t *testing.T) {
	clock := newClock()
	outbox := newFakeOutbox()
	nowMs := uint64(clock.Now().UnixMilli())
	outbox.add(4, txlogKind, rowsAt(nowMs-1000, 1, 2)...)
	outbox.deleteErr = authoritysql.ErrWriterFrozen
	producer := newFakeProducer()
	relay := newTestRelay(t, outbox, "a", producer, clock, nil)

	result := relay.Pass(context.Background())
	if result.ProduceFailed || result.Forwarded != 0 {
		t.Fatalf("result = %+v: a frozen gate is neither a Kafka failure nor a forward", result)
	}
	if got := outbox.remaining(4, txlogKind); len(got) != 2 {
		t.Fatalf("rows must stay while the gate is frozen, remaining = %v", got)
	}
	probes := outbox.probeCount(4)
	relay.Pass(context.Background())
	if outbox.probeCount(4) != probes {
		t.Fatal("a frozen storage must be paused, not probed again right away")
	}
	outbox.deleteErr = nil
	clock.Advance(frozenPause + time.Millisecond)
	relay.Pass(context.Background())
	if got := outbox.remaining(4, txlogKind); len(got) != 0 {
		t.Fatalf("after the pause the rows are forwarded, remaining = %v", got)
	}
}

// created_ms 早于游标的迟到行(提交晚于余量,或时钟跳变)由每 10 分钟一次的全扫接住。
func TestRelay_FullScanPicksUpRowsBehindTheCursor(t *testing.T) {
	clock := newClock()
	outbox := newFakeOutbox()
	producer := newFakeProducer()
	relay := newTestRelay(t, outbox, "a", producer, clock, nil)

	relay.Pass(context.Background()) // 空:游标推到「现在 − 余量」
	late := uint64(clock.Now().Add(-5 * time.Minute).UnixMilli())
	outbox.add(3, txlogKind, rowsAt(late, 1, 1)...)
	relay.Pass(context.Background())
	if got := outbox.remaining(3, txlogKind); len(got) != 1 {
		t.Fatalf("a row behind the cursor is not expected before the full scan, remaining = %v", got)
	}
	clock.Advance(fullScanEvery)
	relay.Pass(context.Background())
	if got := outbox.remaining(3, txlogKind); len(got) != 0 {
		t.Fatalf("the full scan must pick up the late row, remaining = %v", got)
	}
}

// 一个单位一轮最多投 maxBatchesPerUnit 批;还有剩时 Pass 报 More,下一轮不等。
func TestRelay_ReportsMoreWhenAUnitHasMoreThanOnePass(t *testing.T) {
	clock := newClock()
	outbox := newFakeOutbox()
	nowMs := uint64(clock.Now().UnixMilli())
	outbox.add(6, txlogKind, rowsAt(nowMs-1000, 1, maxBatchesPerUnit*txlogBatch+1)...)
	relay := newTestRelay(t, outbox, "a", newFakeProducer(), clock, nil)

	result := relay.Pass(context.Background())
	if !result.More || result.Forwarded != maxBatchesPerUnit*txlogBatch {
		t.Fatalf("result = %+v, want More with %d forwarded", result, maxBatchesPerUnit*txlogBatch)
	}
	if delay := relay.delayAfter(result); delay != 0 {
		t.Fatalf("delay after a full pass = %v, want 0", delay)
	}
	result = relay.Pass(context.Background())
	if result.More || result.Forwarded != 1 || outbox.total() != 0 {
		t.Fatalf("result = %+v, remaining %d", result, outbox.total())
	}
	if delay := relay.delayAfter(result); delay != time.Second {
		t.Fatalf("idle delay = %v, want the interval", delay)
	}
}

// 读失败只跳过那个库这一轮,别的库照常。
func TestRelay_ReadFailureSkipsOnlyThatStorage(t *testing.T) {
	clock := newClock()
	good, broken := newFakeOutbox(), uint32(1000001)
	nowMs := uint64(clock.Now().UnixMilli())
	good.add(0, txlogKind, rowsAt(nowMs-1000, 1, 1)...)
	relay := newTestRelay(t, good, "a", newFakeProducer(), clock, func(o *Options) {
		o.StorageIDs = func() []uint32 { return []uint32{broken, testStorage} }
		o.Open = func(_ context.Context, storageID uint32) (Store, error) {
			if storageID == broken {
				return nil, errors.New("store unavailable")
			}
			return good.store("a"), nil
		}
	})
	result := relay.Pass(context.Background())
	if result.Forwarded != 1 || good.total() != 0 {
		t.Fatalf("result = %+v: a broken storage must not stop the others", result)
	}
}

// Kafka 一直不可用时退避翻倍,封顶 backoffMax;投成功之后归零。
func TestRelay_BackoffDoublesUpToTheCapAndResets(t *testing.T) {
	relay := newTestRelay(t, newFakeOutbox(), "a", newFakeProducer(), newClock(), nil)
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if got := relay.delayAfter(PassResult{ProduceFailed: true}); got != w {
			t.Fatalf("backoff #%d = %v, want %v", i+1, got, w)
		}
	}
	if got := relay.delayAfter(PassResult{}); got != time.Second {
		t.Fatalf("delay after a good pass = %v, want the interval", got)
	}
	if got := relay.delayAfter(PassResult{ProduceFailed: true}); got != time.Second {
		t.Fatalf("backoff must restart from %v after a good pass, got %v", backoffMin, got)
	}
}

func TestNew_RejectsMissingDependencies(t *testing.T) {
	ok := Options{
		StorageIDs: func() []uint32 { return nil },
		Open:       func(context.Context, uint32) (Store, error) { return nil, nil },
		Producer:   newFakeProducer(),
		Interval:   time.Second,
	}
	cases := map[string]func(*Options){
		"no storage list": func(o *Options) { o.StorageIDs = nil },
		"no opener":       func(o *Options) { o.Open = nil },
		"no producer":     func(o *Options) { o.Producer = nil },
		"no interval":     func(o *Options) { o.Interval = 0 },
	}
	for name, mutate := range cases {
		opts := ok
		mutate(&opts)
		if _, err := New(opts); err == nil {
			t.Fatalf("%s: New must fail", name)
		}
	}
	if _, err := New(ok); err != nil {
		t.Fatal(err)
	}
}

func TestUnitKey(t *testing.T) {
	if got := UnitKey(1000000, 15); got != "audit-outbox:1000000:15" {
		t.Fatalf("UnitKey = %q", got)
	}
	if UnitKey(1000000, 1) == UnitKey(100000, 1) {
		t.Fatal("unit keys of different storages must differ")
	}
}
