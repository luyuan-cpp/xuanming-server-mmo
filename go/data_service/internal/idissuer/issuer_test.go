package idissuer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// memStore 是内存里的一份预留记录,可以注入回退、丢失和写失败。
type memStore struct {
	mu       sync.Mutex
	value    uint64
	revision int64
	exists   bool
	readErr  error
	writeErr error
	// beforeWrite 在 CAS 比较之前调用,用来插入并发写。
	beforeWrite func()
	closed      bool
}

func (m *memStore) Read(context.Context) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readErr != nil {
		return Record{}, m.readErr
	}
	return Record{Value: m.value, Revision: m.revision, Exists: m.exists}, nil
}

func (m *memStore) CompareAndSwap(_ context.Context, prev Record, next uint64) (bool, error) {
	m.mu.Lock()
	hook := m.beforeWrite
	m.beforeWrite = nil
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return false, m.writeErr
	}
	if m.exists != prev.Exists || m.revision != prev.Revision {
		return false, nil
	}
	m.value, m.exists = next, true
	m.revision++
	return true, nil
}

func (m *memStore) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *memStore) set(value uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.value, m.exists = value, true
	m.revision++
}

func (m *memStore) drop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.value, m.exists = 0, false
	m.revision++
}

func (m *memStore) get() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.value
}

// 测试用的时刻:纪元后约 300 天。
var testNow = time.Unix(epochUnix+26_000_000, 0)

const testBlock = uint64(1) << 26 // 大于 MaxStep,比默认小,方便测试跨块

type harness struct {
	t         *testing.T
	primary   *memStore
	secondary *memStore
	now       time.Time
	mu        sync.Mutex
}

func newHarness(t *testing.T) *harness {
	return &harness{t: t, primary: &memStore{}, secondary: &memStore{}, now: testNow}
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) setClock(at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = at
}

func (h *harness) issuer(mut func(*Options)) *Issuer {
	h.t.Helper()
	opt := Options{
		Primary:        h.primary,
		Secondary:      h.secondary,
		BlockSize:      testBlock,
		AllowBootstrap: true,
		Tags:           []string{"player", "item"},
		Now:            h.clock,
	}
	if mut != nil {
		mut(&opt)
	}
	iss, err := New(opt)
	if err != nil {
		h.t.Fatal(err)
	}
	return iss
}

// allocated 记下发过的所有段,断言互不重叠。
type allocated struct {
	t      *testing.T
	ranges [][2]uint64
}

func (a *allocated) add(lo, hi uint64) {
	a.t.Helper()
	if lo == 0 || hi <= lo {
		a.t.Fatalf("invalid range [%d, %d)", lo, hi)
	}
	for _, r := range a.ranges {
		if lo < r[1] && r[0] < hi {
			a.t.Fatalf("range [%d, %d) overlaps earlier [%d, %d)", lo, hi, r[0], r[1])
		}
	}
	a.ranges = append(a.ranges, [2]uint64{lo, hi})
}

func (a *allocated) take(iss *Issuer, step uint32) {
	a.t.Helper()
	lo, hi, err := iss.Allocate(context.Background(), "player", step)
	if err != nil {
		a.t.Fatal(err)
	}
	a.add(lo, hi)
}

func TestAllocate_ContiguousWithinBlock(t *testing.T) {
	h := newHarness(t)
	iss := h.issuer(nil)
	lo1, hi1, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if hi1-lo1 != 100 {
		t.Fatalf("segment length = %d, want 100", hi1-lo1)
	}
	lo2, _, err := iss.Allocate(context.Background(), "item", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lo2 != hi1 {
		t.Fatalf("different tags must share one counter: lo2=%d hi1=%d", lo2, hi1)
	}
}

func TestAllocate_TruncatesAtBlockEnd(t *testing.T) {
	h := newHarness(t)
	iss := h.issuer(nil)
	lo, _, err := iss.Allocate(context.Background(), "player", 1)
	if err != nil {
		t.Fatal(err)
	}
	// 把当前块消耗到只剩 10 个号:发号器可以截短,调用方不要求 hi-lo==step。
	iss.mu.Lock()
	iss.cur.pos = iss.cur.end - 10
	iss.next = &block{pos: iss.cur.end + 1000, end: iss.cur.end + 2000, halfway: iss.cur.end + 1500}
	end := iss.cur.end
	iss.mu.Unlock()
	lo2, hi2, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if hi2 != end || hi2-lo2 != 10 {
		t.Fatalf("segment [%d, %d), want truncated at block end %d (first lo=%d)", lo2, hi2, end, lo)
	}
}

func TestAllocate_RejectsBadArguments(t *testing.T) {
	h := newHarness(t)
	iss := h.issuer(nil)
	cases := []struct {
		tag  string
		step uint32
		want error
	}{
		{"Bad Tag", 1, ErrInvalidTag},
		{"", 1, ErrInvalidTag},
		{"unknown", 1, ErrUnknownTag},
		{"player", MaxStep + 1, ErrInvalidStep},
	}
	for _, tc := range cases {
		if _, _, err := iss.Allocate(context.Background(), tc.tag, tc.step); !errors.Is(err, tc.want) {
			t.Errorf("Allocate(%q,%d) err=%v, want %v", tc.tag, tc.step, err, tc.want)
		}
	}
}

// 一块只有两份记录都写成功才能发。
func TestReserve_BothRecordsWrittenBeforeIssuing(t *testing.T) {
	h := newHarness(t)
	iss := h.issuer(nil)
	lo, hi, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if h.primary.get() < hi || h.secondary.get() < hi {
		t.Fatalf("records (%d,%d) must cover the issued range end %d", h.primary.get(), h.secondary.get(), hi)
	}
	if lo < uint64(26_000_000)<<seqBits {
		t.Fatalf("lo=%d below the time floor", lo)
	}
}

func TestReserve_SecondaryWriteFailureDiscardsBlock(t *testing.T) {
	h := newHarness(t)
	h.secondary.writeErr = errors.New("secondary down")
	iss := h.issuer(nil)
	if _, _, err := iss.Allocate(context.Background(), "player", 100); err == nil {
		t.Fatal("must not issue when the secondary record cannot be written")
	}
	// 主记录已经写过,留在高位只浪费值域;之后的块必须从它之上开始。
	if h.primary.get() == 0 {
		t.Fatal("the primary record is expected to be written first")
	}
	iss.mu.Lock()
	remaining := iss.cur.remaining()
	iss.mu.Unlock()
	if remaining != 0 {
		t.Fatal("the block must be discarded")
	}
}

// 第二份记录冲突时不能认领,必须作废重来。
func TestReserve_SecondaryConflictRestarts(t *testing.T) {
	h := newHarness(t)
	h.secondary.set(uint64(26_000_000) << seqBits)
	h.primary.set(uint64(26_000_000) << seqBits)
	h.secondary.beforeWrite = func() {
		// 另一个副本抢先写了更高的值。
		h.secondary.set(uint64(26_000_001) << seqBits)
		h.primary.set(uint64(26_000_001) << seqBits)
	}
	iss := h.issuer(nil)
	lo, _, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lo < uint64(26_000_001)<<seqBits {
		t.Fatalf("lo=%d must restart above the other replica's record", lo)
	}
}

func TestReserve_PersistentConflictGivesUp(t *testing.T) {
	h := newHarness(t)
	var keepConflicting func()
	keepConflicting = func() {
		h.primary.set(h.primary.get() + 1)
		h.primary.mu.Lock()
		h.primary.beforeWrite = keepConflicting
		h.primary.mu.Unlock()
	}
	h.primary.beforeWrite = keepConflicting
	iss := h.issuer(nil)
	if _, _, err := iss.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrContended) {
		t.Fatalf("err=%v, want ErrContended", err)
	}
}

// 下面几个用例对应设计 §6.2、§6.3 的故障表,全部断言发出去的段不重叠。

func TestRecovery_RestartReservesAboveOldBlock(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	first := h.issuer(nil)
	a.take(first, 100)
	// 进程崩溃:不调 Close,手里的块作废。新进程只能拿到更高的块。
	second := h.issuer(nil)
	a.take(second, 100)
	a.take(first, 100) // 崩溃前的进程假如还活着,手里的块仍然独占
}

func TestRecovery_PrimaryRollback(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	first := h.issuer(nil)
	a.take(first, 100)
	before := h.primary.get()
	// 主记录被旧备份覆盖,副记录还在。时钟也倒回去,确保靠的是副记录。
	h.primary.set(before - testBlock*10)
	h.setClock(testNow.Add(-time.Hour))
	second := h.issuer(nil)
	a.take(second, 100)
	if h.primary.get() <= before {
		t.Fatalf("primary=%d must be restored above %d", h.primary.get(), before)
	}
}

func TestRecovery_SecondaryRollback(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	first := h.issuer(nil)
	a.take(first, 100)
	h.secondary.set(h.secondary.get() - testBlock*10)
	h.setClock(testNow.Add(-time.Hour))
	second := h.issuer(nil)
	a.take(second, 100)
}

func TestRecovery_PrimaryWiped(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	first := h.issuer(func(o *Options) { o.AllowBootstrap = false })
	h.primary.set(uint64(26_000_000) << seqBits)
	h.secondary.set(uint64(26_000_000) << seqBits)
	a.take(first, 100)
	h.primary.drop()
	h.setClock(testNow.Add(-time.Hour))
	second := h.issuer(func(o *Options) { o.AllowBootstrap = false })
	a.take(second, 100)
	if !h.primary.exists {
		t.Fatal("the wiped record must be restored")
	}
}

func TestRecovery_SecondaryWiped(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	h.primary.set(uint64(26_000_000) << seqBits)
	h.secondary.set(uint64(26_000_000) << seqBits)
	first := h.issuer(func(o *Options) { o.AllowBootstrap = false })
	a.take(first, 100)
	h.secondary.drop()
	h.setClock(testNow.Add(-time.Hour))
	second := h.issuer(func(o *Options) { o.AllowBootstrap = false })
	a.take(second, 100)
}

// 评审给出的反例:时钟跑快把计数器抬到未来,之后一份记录回退、时钟恢复,再等真实时间追上来。
func TestRecovery_FastClockThenRollback(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	h.primary.set(uint64(26_000_000) << seqBits)
	h.secondary.set(uint64(26_000_000) << seqBits)
	// 时钟快 1 天,发出未来的号。
	h.setClock(testNow.Add(24 * time.Hour))
	fast := h.issuer(nil)
	a.take(fast, 100)
	// 时钟恢复,主记录回退到时钟跑快之前。
	h.setClock(testNow)
	h.primary.set(uint64(26_000_000) << seqBits)
	second := h.issuer(nil)
	a.take(second, 100)
	// 真实时间走到当时那一刻,再领一块。
	h.setClock(testNow.Add(24 * time.Hour))
	third := h.issuer(nil)
	a.take(third, 100)
	a.take(second, 100)
}

// 两份都丢:生产拒绝发号,不自动从头来。
func TestRecovery_BothMissingRefusedInProduction(t *testing.T) {
	h := newHarness(t)
	iss := h.issuer(func(o *Options) { o.AllowBootstrap = false })
	if _, _, err := iss.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrCounterMissing) {
		t.Fatalf("err=%v, want ErrCounterMissing", err)
	}
	if h.primary.exists || h.secondary.exists {
		t.Fatal("refusal must not write anything")
	}
}

func TestRecovery_ExplicitBootstrapThenIssue(t *testing.T) {
	h := newHarness(t)
	iss := h.issuer(func(o *Options) { o.AllowBootstrap = false })
	done, err := iss.Bootstrap(context.Background())
	if err != nil || !done {
		t.Fatalf("Bootstrap done=%v err=%v", done, err)
	}
	want := (uint64(26_000_000) + bootstrapMarginSeconds) << seqBits
	if h.primary.get() != want || h.secondary.get() != want {
		t.Fatalf("records=(%d,%d), want %d", h.primary.get(), h.secondary.get(), want)
	}
	// 记录已存在时再跑一次什么也不做。
	done, err = iss.Bootstrap(context.Background())
	if err != nil || done {
		t.Fatalf("second Bootstrap done=%v err=%v", done, err)
	}
	if _, _, err := iss.Allocate(context.Background(), "player", 100); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrap_DoesNothingWhenOneRecordExists(t *testing.T) {
	h := newHarness(t)
	h.primary.set(12345)
	iss := h.issuer(nil)
	done, err := iss.Bootstrap(context.Background())
	if err != nil || done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if h.secondary.exists {
		t.Fatal("must not create the missing record here")
	}
}

// 时钟回拨或跑慢:计数器只增不减,不会重号。
func TestClock_BackwardIsHarmless(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	first := h.issuer(nil)
	a.take(first, 100)
	h.setClock(testNow.Add(-30 * 24 * time.Hour))
	second := h.issuer(nil)
	a.take(second, 100)
}

// 时钟早于纪元:不采用时间下限,但不影响发号。
func TestClock_BeforeEpoch(t *testing.T) {
	h := newHarness(t)
	h.primary.set(5000)
	h.secondary.set(5000)
	h.setClock(time.Unix(epochUnix-100, 0))
	iss := h.issuer(nil)
	lo, _, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lo != 5000 {
		t.Fatalf("lo=%d, want 5000", lo)
	}
}

// 记录落后时钟很多:时间下限照样生效,不拒绝。一块号能用几个月,这在用量小的时候是常态。
// 反过来,这也是两份记录同时被旧备份覆盖时唯一的防线。
func TestClock_FloorAppliesHoweverFarAhead(t *testing.T) {
	h := newHarness(t)
	h.primary.set(uint64(1000) << seqBits)
	h.secondary.set(uint64(1000) << seqBits)
	iss := h.issuer(func(o *Options) { o.AllowBootstrap = false })
	lo, _, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lo != uint64(26_000_000)<<seqBits {
		t.Fatalf("lo=%d, want the time floor", lo)
	}
}

// 时钟超出可表示的范围:不能挡住发号,按记录继续。
func TestClock_BeyondRangeIgnored(t *testing.T) {
	h := newHarness(t)
	h.primary.set(5000)
	h.secondary.set(5000)
	h.setClock(time.Unix(epochUnix+int64(1)<<timeBits+100, 0))
	iss := h.issuer(nil)
	lo, _, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lo != 5000 {
		t.Fatalf("lo=%d, want 5000", lo)
	}
}

// 计数器比时钟超前很多(时钟曾跳到未来):照常发号,只上报。
func TestClock_CounterFarAheadIsReported(t *testing.T) {
	h := newHarness(t)
	ahead := (uint64(26_000_000) + maxAheadSeconds + 1) << seqBits
	h.primary.set(ahead)
	h.secondary.set(ahead)
	var mu sync.Mutex
	seen := map[string]int{}
	iss := h.issuer(func(o *Options) {
		o.AllowBootstrap = false
		o.OnReserve = func(result string) {
			mu.Lock()
			defer mu.Unlock()
			seen[result]++
		}
	})
	lo, _, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lo != ahead {
		t.Fatalf("lo=%d, want %d", lo, ahead)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen[ReserveAheadOfClock] != 1 || seen[ReserveOK] != 1 {
		t.Fatalf("observed %v", seen)
	}
}

func TestBootstrap_RefusesUnusableClock(t *testing.T) {
	h := newHarness(t)
	h.setClock(time.Unix(epochUnix-100, 0))
	iss := h.issuer(nil)
	if _, err := iss.Bootstrap(context.Background()); !errors.Is(err, ErrClockUnusable) {
		t.Fatalf("err=%v, want ErrClockUnusable", err)
	}
	if h.primary.exists || h.secondary.exists {
		t.Fatal("refusal must not write anything")
	}
}

// 预留失败之后,退避期内快速失败;过了退避期自动恢复,且发出的号仍在记录之下。
func TestReserve_RecoversAfterBackoff(t *testing.T) {
	h := newHarness(t)
	h.secondary.mu.Lock()
	h.secondary.writeErr = errors.New("secondary down")
	h.secondary.mu.Unlock()
	iss := h.issuer(nil)
	mono := time.Unix(1000, 0)
	var monoMu sync.Mutex
	iss.monoNow = func() time.Time {
		monoMu.Lock()
		defer monoMu.Unlock()
		return mono
	}
	if _, _, err := iss.Allocate(context.Background(), "player", 100); err == nil {
		t.Fatal("first allocate must fail")
	}
	if _, _, err := iss.Allocate(context.Background(), "player", 100); err == nil {
		t.Fatal("must fail fast inside the backoff window")
	}
	h.secondary.mu.Lock()
	h.secondary.writeErr = nil
	h.secondary.mu.Unlock()
	monoMu.Lock()
	mono = mono.Add(reserveRetryBackoff)
	monoMu.Unlock()
	_, hi, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if h.primary.get() < hi || h.secondary.get() < hi {
		t.Fatalf("records (%d,%d) must cover %d", h.primary.get(), h.secondary.get(), hi)
	}
}

// 用到一半时后台预取下一块,切块时不用等存储。
func TestAllocate_PrefetchesNextBlockAtHalfway(t *testing.T) {
	h := newHarness(t)
	iss := h.issuer(nil)
	a := &allocated{t: t}
	a.take(iss, 1)
	iss.mu.Lock()
	iss.cur.pos = iss.cur.halfway
	iss.mu.Unlock()
	a.take(iss, 1)
	iss.mu.Lock()
	res := iss.inflight
	iss.mu.Unlock()
	if res != nil {
		<-res.done
	}
	iss.mu.Lock()
	ready := iss.next != nil
	iss.mu.Unlock()
	if !ready {
		t.Fatal("the next block must be prefetched")
	}
	// 预取之后存储坏了,手里的号也发得完,还能切到下一块。
	h.primary.mu.Lock()
	h.primary.readErr = errors.New("primary down")
	h.primary.mu.Unlock()
	h.secondary.mu.Lock()
	h.secondary.readErr = errors.New("secondary down")
	h.secondary.mu.Unlock()
	iss.mu.Lock()
	iss.cur.pos = iss.cur.end - 1
	iss.mu.Unlock()
	a.take(iss, 1)
	a.take(iss, 1)
}

func TestRegion_InHighBits(t *testing.T) {
	h := newHarness(t)
	iss := h.issuer(func(o *Options) { o.Region = 5 })
	lo, hi, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lo>>regionShift != 5 || hi>>regionShift != 5 {
		t.Fatalf("range [%d, %d) must carry region 5", lo, hi)
	}
	if hi >= 1<<63 {
		t.Fatalf("hi=%d must stay below 2^63", hi)
	}
}

func TestRegion_MismatchRefused(t *testing.T) {
	h := newHarness(t)
	h.primary.set(uint64(2)<<regionShift | 100)
	h.secondary.set(uint64(2)<<regionShift | 100)
	iss := h.issuer(func(o *Options) { o.Region = 3 })
	if _, _, err := iss.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrRegionMismatch) {
		t.Fatalf("err=%v, want ErrRegionMismatch", err)
	}
}

func TestNew_RejectsBadOptions(t *testing.T) {
	p, s := &memStore{}, &memStore{}
	cases := []Options{
		{Secondary: s, Tags: []string{"a"}},
		{Primary: p, Tags: []string{"a"}},
		{Primary: p, Secondary: s},
		{Primary: p, Secondary: s, Tags: []string{"Bad Tag"}},
		{Primary: p, Secondary: s, Tags: []string{"a"}, Region: MaxRegion + 1},
		{Primary: p, Secondary: s, Tags: []string{"a"}, BlockSize: 1},
	}
	for i, opt := range cases {
		if _, err := New(opt); err == nil {
			t.Errorf("case %d must be rejected", i)
		}
	}
}

// 多个副本并发发号,全部段不重叠。
func TestConcurrent_ReplicasDoNotOverlap(t *testing.T) {
	h := newHarness(t)
	const replicas = 4
	const perReplica = 400
	type seg struct{ lo, hi uint64 }
	results := make(chan seg, replicas*perReplica)
	var wg sync.WaitGroup
	for r := 0; r < replicas; r++ {
		iss := h.issuer(nil)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < perReplica; n++ {
				// 冲突重试有上限,偶发失败是预期内的,调用方会重试;这里只收成功的段。
				lo, hi, err := iss.Allocate(context.Background(), "player", uint32(testBlock/64))
				if err != nil {
					time.Sleep(time.Millisecond)
					continue
				}
				results <- seg{lo, hi}
			}
		}()
	}
	wg.Wait()
	close(results)
	a := &allocated{t: t}
	for s := range results {
		a.add(s.lo, s.hi)
	}
	if len(a.ranges) == 0 {
		t.Fatal("no segment was issued")
	}
}

func TestClose_StopsAllocating(t *testing.T) {
	h := newHarness(t)
	iss := h.issuer(nil)
	if err := iss.Close(); err != nil {
		t.Fatal(err)
	}
	if !h.primary.closed || !h.secondary.closed {
		t.Fatal("Close must close both stores")
	}
	if _, _, err := iss.Allocate(context.Background(), "player", 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("err=%v, want ErrClosed", err)
	}
}
