package idsegment

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// capLog 收集日志,用来断言「范围违规被大声记了」。
type capLog struct {
	mu    sync.Mutex
	errs  []string
	infos []string
}

func (l *capLog) Errorf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, fmt.Sprintf(format, args...))
}

func (l *capLog) Infof(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, fmt.Sprintf(format, args...))
}

func (l *capLog) errorsContaining(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.errs {
		if strings.Contains(e, sub) {
			n++
		}
	}
	return n
}

// counterAlloc 是最简单的假服务端:从 1 起按 step 连续发段,计调用次数。
type counterAlloc struct {
	mu    sync.Mutex
	next  uint64
	calls int
	// steps 按调用顺序记下每次请求带的 step;动态 step 的测试靠它断言「预取带了新 step」。
	steps []uint32
	// gate 非 nil 时,第 blockFrom 次(从 1 数)起的调用都要等 gate 关闭才返回。
	gate      chan struct{}
	blockFrom int
}

func newCounterAlloc() *counterAlloc { return &counterAlloc{next: 1} }

func (a *counterAlloc) Allocate(ctx context.Context, _ string, step uint32) (uint64, uint64, error) {
	a.mu.Lock()
	a.calls++
	a.steps = append(a.steps, step)
	call := a.calls
	gate := a.gate
	blockFrom := a.blockFrom
	a.mu.Unlock()
	if gate != nil && call >= blockFrom {
		select {
		case <-gate:
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	lo := a.next
	a.next += uint64(step)
	return lo, a.next, nil
}

func (a *counterAlloc) Calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func (a *counterAlloc) Steps() []uint32 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.steps)
}

func mustNew(t *testing.T, rpc AllocateFunc, opt Options) *Client {
	t.Helper()
	if opt.Logger == nil {
		opt.Logger = &capLog{}
	}
	c, err := New(rpc, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestNew_RejectsInvalidOptions(t *testing.T) {
	ok := newCounterAlloc().Allocate
	cases := []struct {
		name string
		rpc  AllocateFunc
		opt  Options
	}{
		{"nil rpc", nil, Options{BizTag: "x", Step: 1}},
		{"empty biz tag", ok, Options{Step: 1}},
		{"zero step", ok, Options{BizTag: "x"}},
		{"prefetch > 1", ok, Options{BizTag: "x", Step: 1, PrefetchAt: 1.5}},
		{"max id above 2^55", ok, Options{BizTag: "x", Step: 1, MaxIDExclusive: DefaultMaxIDExclusive + 1}},
		{"min step above step", ok, Options{BizTag: "x", Step: 5, MinStep: 6}},
		{"max step below step", ok, Options{BizTag: "x", Step: 50, MaxStep: 40}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.rpc, tc.opt); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("want ErrInvalidOptions, got %v", err)
			}
		})
	}
}

// 顺序发号:id 从 1 起严格递增、无重复,跨段无缝。
func TestNext_SequentialUnique(t *testing.T) {
	alloc := newCounterAlloc()
	c := mustNew(t, alloc.Allocate, Options{BizTag: "player", Step: 10})
	ctx := context.Background()

	seen := make(map[uint64]struct{})
	var prev uint64
	for i := 0; i < 100; i++ {
		id, err := c.Next(ctx)
		if err != nil {
			t.Fatalf("Next #%d: %v", i, err)
		}
		if id != prev+1 {
			t.Fatalf("Next #%d = %d, want %d (ids must be contiguous from 1 with a contiguous fake)", i, id, prev+1)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = struct{}{}
		prev = id
	}
	st := c.Stats()
	if st.Issued != 100 || st.RangeViolations != 0 || st.Unavailable != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if st.HighWater < 100 {
		t.Fatalf("HighWater = %d, want >= 100", st.HighWater)
	}
}

// 预取在阈值处触发,而且单飞:预取在途时再多的 Next 也不会再发一次 RPC。
func TestPrefetch_TriggersAtThresholdAndSingleFlight(t *testing.T) {
	alloc := newCounterAlloc()
	alloc.gate = make(chan struct{})
	alloc.blockFrom = 2 // 第一段立刻给,预取那次卡住
	// step 10、PrefetchAt 0.2 → 剩余 ≤ 2 时预取,即发出第 8 个 id 之后。
	c := mustNew(t, alloc.Allocate, Options{BizTag: "player", Step: 10, PrefetchAt: 0.2})
	ctx := context.Background()

	for i := 1; i <= 7; i++ {
		if _, err := c.Next(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := alloc.Calls(); got != 1 {
		t.Fatalf("after 7 ids (3 remaining > threshold 2) calls = %d, want 1 (no prefetch yet)", got)
	}

	if _, err := c.Next(ctx); err != nil { // 第 8 个:剩余 2 → 触发预取
		t.Fatal(err)
	}
	waitFor(t, func() bool { return alloc.Calls() == 2 }, "prefetch RPC was not issued at the threshold")

	// 预取卡在 gate 上;把当前段剩下的 2 个发完,期间不许出现第 3 次 RPC。
	for i := 9; i <= 10; i++ {
		if _, err := c.Next(ctx); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := alloc.Calls(); got != 2 {
		t.Fatalf("calls = %d, want 2: prefetch must be single-flight", got)
	}
	if st := c.Stats(); st.CurrentRemaining != 0 || st.NextReady {
		t.Fatalf("stats = %+v, want current exhausted and next not ready", st)
	}

	// 放行预取:下一个 id 必须是第二段的 lo(11),且仍然只有 2 次 RPC。
	close(alloc.gate)
	alloc.gate = nil
	id, err := c.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id != 11 {
		t.Fatalf("first id of the prefetched range = %d, want 11", id)
	}
	if got := alloc.Calls(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

// 两段都空、服务端一直失败:Next 等到那次续段失败就返回 ErrSegmentUnavailable(带原因)。
func TestNext_ServerDown_ReturnsUnavailable(t *testing.T) {
	boom := errors.New("connection refused")
	log := &capLog{}
	c := mustNew(t, func(context.Context, string, uint32) (uint64, uint64, error) {
		return 0, 0, boom
	}, Options{BizTag: "player", Step: 10, RetryBackoff: 10 * time.Millisecond, Logger: log})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := c.Next(ctx)
	if !errors.Is(err, ErrSegmentUnavailable) || !errors.Is(err, boom) {
		t.Fatalf("want ErrSegmentUnavailable wrapping the rpc error, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Next must fail as soon as the in-flight fetch fails, took %v", time.Since(start))
	}
	if st := c.Stats(); st.Unavailable != 1 || st.FetchErrors != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if log.errorsContaining("allocate failed") == 0 {
		t.Fatal("fetch failure must be logged")
	}
}

// 服务端挂死(RPC 不返回):Next 阻塞到 ctx 截止,然后 ErrSegmentUnavailable。
func TestNext_ServerHangs_BlocksUntilDeadline(t *testing.T) {
	c := mustNew(t, func(ctx context.Context, _ string, _ uint32) (uint64, uint64, error) {
		<-ctx.Done()
		return 0, 0, ctx.Err()
	}, Options{BizTag: "player", Step: 10, FetchTimeout: time.Minute})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Next(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrSegmentUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want ErrSegmentUnavailable wrapping DeadlineExceeded, got %v", err)
	}
	if elapsed < 90*time.Millisecond {
		t.Fatalf("Next returned after %v: it must block on the in-flight fetch until the ctx deadline", elapsed)
	}
}

// 续段失败后先退避再试:失败一次、随后成功,第二次 RPC 不早于 RetryBackoff。
func TestNext_RetryBackoffAfterFailure(t *testing.T) {
	var mu sync.Mutex
	var calls []time.Time
	c := mustNew(t, func(context.Context, string, uint32) (uint64, uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, time.Now())
		if len(calls) == 1 {
			return 0, 0, errors.New("transient")
		}
		return 1, 11, nil
	}, Options{BizTag: "player", Step: 10, RetryBackoff: 80 * time.Millisecond})

	ctx := context.Background()
	if _, err := c.Next(ctx); !errors.Is(err, ErrSegmentUnavailable) {
		t.Fatalf("first Next should fail, got %v", err)
	}
	id, err := c.Next(ctx)
	if err != nil {
		t.Fatalf("second Next should succeed after backoff: %v", err)
	}
	if id != 1 {
		t.Fatalf("id = %d, want 1", id)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if gap := calls[1].Sub(calls[0]); gap < 70*time.Millisecond {
		t.Fatalf("second attempt came %v after the failure, want >= RetryBackoff", gap)
	}
}

// 范围校验:越界 / 空段 / 超上限 / 重叠 / 倒退一律拒绝,而且大声记日志、不发号。
func TestValidation_RejectsBadRanges(t *testing.T) {
	cases := []struct {
		name   string
		ranges [][2]uint64 // 依次返回;最后一个是坏的
		reason string
	}{
		{"lo below 1", [][2]uint64{{0, 10}}, "lo < 1"},
		{"empty range", [][2]uint64{{5, 5}}, "hi <= lo"},
		{"inverted range", [][2]uint64{{10, 5}}, "hi <= lo"},
		{"hi above cap", [][2]uint64{{DefaultMaxIDExclusive - 5, DefaultMaxIDExclusive + 5}}, "max_id_exclusive"},
		{"overlap with previous", [][2]uint64{{1, 11}, {5, 15}}, "regressed/overlapped"},
		{"regression", [][2]uint64{{100, 110}, {1, 11}}, "regressed/overlapped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			i := 0
			log := &capLog{}
			c := mustNew(t, func(context.Context, string, uint32) (uint64, uint64, error) {
				mu.Lock()
				defer mu.Unlock()
				if i >= len(tc.ranges) {
					return 0, 0, errors.New("no more scripted ranges")
				}
				r := tc.ranges[i]
				i++
				return r[0], r[1], nil
			}, Options{BizTag: "player", Step: 10, PrefetchAt: 1, RetryBackoff: time.Millisecond, Logger: log})
			ctx := context.Background()

			// 前面的好段要能正常发完(PrefetchAt=1 → 第一个 Next 就预取坏段)。
			var lastGood uint64
			for k := 0; k+1 < len(tc.ranges); k++ {
				for n := tc.ranges[k][0]; n < tc.ranges[k][1]; n++ {
					id, err := c.Next(ctx)
					if err != nil {
						t.Fatalf("good range Next: %v", err)
					}
					lastGood = id
				}
			}
			_, err := c.Next(ctx)
			if !errors.Is(err, ErrSegmentUnavailable) || !errors.Is(err, ErrRangeViolation) {
				t.Fatalf("want ErrSegmentUnavailable+ErrRangeViolation, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("error %q does not mention reason %q", err, tc.reason)
			}
			if log.errorsContaining("REJECTED range") == 0 {
				t.Fatal("range violation must be logged loudly")
			}
			st := c.Stats()
			if st.RangeViolations != 1 {
				t.Fatalf("RangeViolations = %d, want 1", st.RangeViolations)
			}
			if st.Issued != 0 && lastGood == 0 {
				t.Fatalf("ids were issued from a rejected range: %+v", st)
			}
		})
	}
}

// 64 个 goroutine × 10k:全部唯一,数量精确。
func TestConcurrent_64x10k_Unique(t *testing.T) {
	const (
		workers = 64
		perW    = 10_000
	)
	alloc := newCounterAlloc()
	c := mustNew(t, alloc.Allocate, Options{BizTag: "player", Step: 1000})
	ctx := context.Background()

	results := make([][]uint64, workers)
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			buf := make([]uint64, 0, perW)
			for i := 0; i < perW; i++ {
				id, err := c.Next(ctx)
				if err != nil {
					errCh <- err
					return
				}
				buf = append(buf, id)
			}
			results[w] = buf
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	seen := make(map[uint64]struct{}, workers*perW)
	for _, buf := range results {
		for _, id := range buf {
			if id == 0 {
				t.Fatal("id 0 issued")
			}
			if _, dup := seen[id]; dup {
				t.Fatalf("duplicate id %d", id)
			}
			seen[id] = struct{}{}
		}
	}
	if len(seen) != workers*perW {
		t.Fatalf("issued %d ids, want %d", len(seen), workers*perW)
	}
	if st := c.Stats(); st.Issued != workers*perW || st.RangeViolations != 0 || st.Unavailable != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

// Close 幂等;Close 后 Next / Warm 一律 ErrClosed;在途 RPC 被取消,Close 不会挂住。
func TestClose_Idempotent(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	c := mustNew(t, func(ctx context.Context, _ string, _ uint32) (uint64, uint64, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return 0, 0, ctx.Err()
	}, Options{BizTag: "player", Step: 10, FetchTimeout: time.Minute})

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = c.Next(ctx)
	}()
	<-entered

	done := make(chan struct{})
	go func() {
		c.Close()
		c.Close() // 第二次必须是空操作
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung: the in-flight fetch must be cancelled and awaited")
	}
	if _, err := c.Next(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Next after Close = %v, want ErrClosed", err)
	}
	if err := c.Warm(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Warm after Close = %v, want ErrClosed", err)
	}
}

// Warm 同步领第一段;之后的 Next 不再发 RPC(直到阈值)。失败只返回错误,不影响后续。
func TestWarm(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		alloc := newCounterAlloc()
		c := mustNew(t, alloc.Allocate, Options{BizTag: "guild", Step: 100})
		if err := c.Warm(context.Background()); err != nil {
			t.Fatal(err)
		}
		if alloc.Calls() != 1 {
			t.Fatalf("calls = %d, want 1", alloc.Calls())
		}
		if st := c.Stats(); !st.NextReady || st.HighWater != 101 {
			t.Fatalf("stats after Warm = %+v", st)
		}
		if err := c.Warm(context.Background()); err != nil || alloc.Calls() != 1 {
			t.Fatalf("second Warm must be a no-op: err=%v calls=%d", err, alloc.Calls())
		}
		id, err := c.Next(context.Background())
		if err != nil || id != 1 || alloc.Calls() != 1 {
			t.Fatalf("Next after Warm: id=%d err=%v calls=%d", id, err, alloc.Calls())
		}
	})
	t.Run("failure does not poison the client", func(t *testing.T) {
		var mu sync.Mutex
		n := 0
		c := mustNew(t, func(context.Context, string, uint32) (uint64, uint64, error) {
			mu.Lock()
			defer mu.Unlock()
			n++
			if n == 1 {
				return 0, 0, errors.New("unimplemented")
			}
			return 1, 101, nil
		}, Options{BizTag: "guild", Step: 100, RetryBackoff: time.Millisecond})
		if err := c.Warm(context.Background()); err == nil {
			t.Fatal("Warm must surface the fetch error")
		}
		if id, err := c.Next(context.Background()); err != nil || id != 1 {
			t.Fatalf("Next after failed Warm: id=%d err=%v", id, err)
		}
	})
}

// rpc panic 时在途状态必须被清掉、等待者必须被唤醒,而不是让所有 Next 永远挂死。
func TestFetchPanic_DoesNotWedgeClient(t *testing.T) {
	var mu sync.Mutex
	n := 0
	c := mustNew(t, func(context.Context, string, uint32) (uint64, uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 1 {
			panic("boom")
		}
		return 1, 11, nil
	}, Options{BizTag: "player", Step: 10, RetryBackoff: time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Next(ctx); !errors.Is(err, ErrSegmentUnavailable) {
		t.Fatalf("Next during panicked fetch = %v, want ErrSegmentUnavailable", err)
	}
	if id, err := c.Next(ctx); err != nil || id != 1 {
		t.Fatalf("Next after panicked fetch: id=%d err=%v", id, err)
	}
}

// MinStep / MaxStep 为 0 时取 [10, 1000];默认界夹不住 Step 时收成 Step 本身,
// 让显式给的 Step 永远合法。
func TestNew_StepBoundsDefaults(t *testing.T) {
	cases := []struct {
		step, wantMin, wantMax uint32
	}{
		{100, 10, 1000},
		{5, 5, 1000},
		{5000, 10, 5000},
	}
	for _, tc := range cases {
		c := mustNew(t, newCounterAlloc().Allocate, Options{BizTag: "x", Step: tc.step})
		st := c.Stats()
		if st.Step != tc.step || st.MinStep != tc.wantMin || st.MaxStep != tc.wantMax {
			t.Fatalf("Step=%d: got step=%d bounds=[%d, %d], want bounds [%d, %d]",
				tc.step, st.Step, st.MinStep, st.MaxStep, tc.wantMin, tc.wantMax)
		}
	}
}

// Conf 的 OrDefault 三件套必须与 New 对 0 值的处理一致,起服日志打的界才是客户端真用的界。
func TestConf_StepDefaults(t *testing.T) {
	cases := []struct {
		name                   string
		conf                   Conf
		step, wantMin, wantMax uint32
	}{
		{"zero conf", Conf{}, 100, 10, 1000},
		{"step below default min", Conf{Step: 5}, 5, 5, 1000},
		{"step above default max", Conf{Step: 5000}, 5000, 10, 5000},
		{"explicit bounds", Conf{Step: 100, MinStep: 20, MaxStep: 200}, 100, 20, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if s, lo, hi := tc.conf.StepOrDefault(), tc.conf.MinStepOrDefault(), tc.conf.MaxStepOrDefault(); s != tc.step || lo != tc.wantMin || hi != tc.wantMax {
				t.Fatalf("got step=%d bounds=[%d, %d], want %d [%d, %d]", s, lo, hi, tc.step, tc.wantMin, tc.wantMax)
			}
		})
	}
}

// 有号在手、服务端倒下:预取失败后的退避期内,连续 N 次 Next 不许再发起领段
// (否则库倒下期间每个建角请求都去打一次 data_service);退避期过后才允许再试一次。
func TestPrefetch_RespectsRetryBackoff(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	rpc := func(context.Context, string, uint32) (uint64, uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return 1, 101, nil
		}
		return 0, 0, errors.New("connection refused")
	}
	const backoff = 200 * time.Millisecond
	// PrefetchAt=1:段内每个 Next 都满足预取条件 —— 正好证明拦住它的是退避而不是阈值。
	c := mustNew(t, rpc, Options{BizTag: "player", Step: 100, PrefetchAt: 1, RetryBackoff: backoff})

	issueN(t, c, 1) // 领段 1,并立刻预取段 2(会失败)
	waitFor(t, func() bool { return c.Stats().FetchErrors == 1 }, "the prefetch did not fail")
	notBefore := notBeforeOf(c)

	issueN(t, c, 20)
	got := fetchesStarted(c)
	if !time.Now().Before(notBefore) {
		t.Skipf("machine too slow: 20 Next calls outlived the %v backoff window", backoff)
	}
	if got != 2 {
		t.Fatalf("fetches started = %d after 20 Next calls inside the backoff window, want 2 "+
			"(the first range + the one failed prefetch): prefetch must not bypass RetryBackoff", got)
	}

	time.Sleep(time.Until(notBefore) + 100*time.Millisecond)
	issueN(t, c, 1)
	if got := fetchesStarted(c); got != 3 {
		t.Fatalf("fetches started = %d after the backoff elapsed, want 3 (exactly one more attempt)", got)
	}
}

// ───────────────────────── 动态 step(Leaf 口径)─────────────────────────

// 一段不到 15 分钟用完 → step 翻倍;翻倍后的 step 由下一次预取带给服务端;新段的预取
// 阈值按新长度算。
func TestDynamicStep_DoublesAfterFastRangeAndPrefetchUsesIt(t *testing.T) {
	alloc := newCounterAlloc()
	clk := newFakeClock()
	// PrefetchAt 默认 0.1:段长 10 → 剩 1 个时预取;段长 20 → 剩 2 个时预取。
	c := mustNewWithClock(t, alloc.Allocate, Options{BizTag: "player", Step: 10, MinStep: 10, MaxStep: 1000}, clk)

	// 段 1 [1, 11):第 9 个号触发预取(此时 step 仍是 10),第 10 个号用完 → 20。
	issueN(t, c, 9)
	waitFor(t, func() bool { return alloc.Calls() == 2 }, "prefetch for range 2 was not issued")
	if got := c.Stats().Step; got != 10 {
		t.Fatalf("step changed to %d before the range was exhausted", got)
	}
	issueN(t, c, 1)
	if got := c.Stats().Step; got != 20 {
		t.Fatalf("step after a range that lasted 0s = %d, want 20 (doubled)", got)
	}

	// 段 2 [11, 21) 是翻倍**之前**用 step=10 领的,长度 10、阈值 1:第 9 个号时预取段 3,
	// 这次请求必须带 20。
	issueN(t, c, 9)
	waitFor(t, func() bool { return alloc.Calls() == 3 }, "prefetch for range 3 was not issued")
	if got := alloc.Steps(); !slices.Equal(got, []uint32{10, 10, 20}) {
		t.Fatalf("requested steps = %v, want [10 10 20]: the prefetch must carry the adjusted step", got)
	}
	issueN(t, c, 1) // 段 2 也是瞬间用完 → 40
	if got := c.Stats().Step; got != 40 {
		t.Fatalf("step after the second fast range = %d, want 40", got)
	}

	// 段 3 [21, 41) 长度 20 → 阈值 2:发到第 18 个才预取,第 17 个时不许有第 4 次领段。
	issueN(t, c, 17)
	if got := fetchesStarted(c); got != 3 {
		t.Fatalf("fetches started after 17/20 of range 3 = %d, want 3 (threshold must follow the range length)", got)
	}
	issueN(t, c, 1)
	if got := fetchesStarted(c); got != 4 {
		t.Fatalf("fetches started after 18/20 of range 3 = %d, want 4", got)
	}
	waitFor(t, func() bool { return alloc.Calls() == 4 }, "prefetch for range 4 was not issued")
	if got := alloc.Steps(); got[3] != 40 {
		t.Fatalf("range 4 requested with step %d, want 40", got[3])
	}
	if st := c.Stats(); st.HighWater != 81 { // 从 1 起:10 + 10 + 20 + 40
		t.Fatalf("HighWater = %d, want 81", st.HighWater)
	}
}

// 一段超过 30 分钟才用完 → step 减半,且减半后的 step 由下一次预取带出去。
func TestDynamicStep_HalvesAfterSlowRange(t *testing.T) {
	alloc := newCounterAlloc()
	clk := newFakeClock()
	c := mustNewWithClock(t, alloc.Allocate, Options{BizTag: "guild", Step: 40, MinStep: 10, MaxStep: 1000}, clk)

	issueN(t, c, 1) // 装上段 1,开始计时
	clk.Advance(30*time.Minute + time.Second)
	issueN(t, c, 39) // 用完
	if got := c.Stats().Step; got != 20 {
		t.Fatalf("step after a range that lasted 30m1s = %d, want 20 (halved)", got)
	}
	// 段 2 [41, 81) 是减半前用 40 领的,阈值 4:第 36 个号时预取段 3,必须带 20。
	issueN(t, c, 36)
	waitFor(t, func() bool { return alloc.Calls() == 3 }, "prefetch for range 3 was not issued")
	if got := alloc.Steps(); !slices.Equal(got, []uint32{40, 40, 20}) {
		t.Fatalf("requested steps = %v, want [40 40 20]", got)
	}
}

// [15, 30] 分钟(含两端)用完的段不改 step:门限是严格的 < 15 / > 30。
func TestDynamicStep_UnchangedBetweenThresholds(t *testing.T) {
	for _, lasted := range []time.Duration{15 * time.Minute, 20 * time.Minute, 30 * time.Minute} {
		t.Run(lasted.String(), func(t *testing.T) {
			alloc := newCounterAlloc()
			clk := newFakeClock()
			c := mustNewWithClock(t, alloc.Allocate, Options{BizTag: "player", Step: 40, MinStep: 10, MaxStep: 1000}, clk)
			issueN(t, c, 1)
			clk.Advance(lasted)
			issueN(t, c, 39)
			if got := c.Stats().Step; got != 40 {
				t.Fatalf("step after a range that lasted %v = %d, want unchanged 40", lasted, got)
			}
		})
	}
}

// 翻倍不超过 MaxStep、减半不低于 MinStep;顶在界上再快 / 再慢也不动。
func TestDynamicStep_ClampsAtBounds(t *testing.T) {
	t.Run("grow clamps at MaxStep", func(t *testing.T) {
		alloc := newCounterAlloc()
		clk := newFakeClock()
		c := mustNewWithClock(t, alloc.Allocate, Options{BizTag: "player", Step: 10, MinStep: 10, MaxStep: 15}, clk)
		issueN(t, c, 10) // 段 1 瞬间用完:10×2=20 → 夹到 15
		if got := c.Stats().Step; got != 15 {
			t.Fatalf("step = %d, want 15 (clamped at MaxStep)", got)
		}
		issueN(t, c, 10) // 段 2(翻倍前用 10 领的)也瞬间用完:15×2 → 仍 15
		if got := c.Stats().Step; got != 15 {
			t.Fatalf("step = %d, want to stay at MaxStep 15", got)
		}
	})
	t.Run("shrink clamps at MinStep", func(t *testing.T) {
		alloc := newCounterAlloc()
		clk := newFakeClock()
		c := mustNewWithClock(t, alloc.Allocate, Options{BizTag: "player", Step: 10, MinStep: 8, MaxStep: 100}, clk)
		for i := 0; i < 2; i++ { // 两段都慢:10/2=5 → 夹到 8;8/2=4 → 仍 8(两段都是用 10 领的)
			issueN(t, c, 1)
			clk.Advance(31 * time.Minute)
			issueN(t, c, 9)
			if got := c.Stats().Step; got != 8 {
				t.Fatalf("after slow range #%d step = %d, want 8 (clamped at MinStep)", i+1, got)
			}
		}
	})
}

// ───────────────────────── 测试辅助 ─────────────────────────

// fakeClock 是可手动拨动的墙钟。退避判断也走它,所以用它的测试要么不制造领段失败,
// 要么显式 Advance 越过 RetryBackoff,否则 Next 会在退避里睡到 ctx 到期。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

// mustNewWithClock 在任何 IO 之前把假钟装进去(New 不起 goroutine,这里没有竞争)。
func mustNewWithClock(t *testing.T, rpc AllocateFunc, opt Options, clk *fakeClock) *Client {
	t.Helper()
	c := mustNew(t, rpc, opt)
	c.now = clk.Now
	return c
}

// issueN 连发 n 个号,任何错误都 Fatal。
func issueN(t *testing.T, c *Client, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := c.Next(context.Background()); err != nil {
			t.Fatalf("Next #%d: %v", i+1, err)
		}
	}
}

// fetchesStarted 读已发起(含在途 / 失败)的领段次数:startFetchLocked 在 Next 持锁时
// 同步加一,所以「没有多发」可以确定性断言,不必 sleep 等 goroutine 跑到 rpc。
func fetchesStarted(c *Client) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fetchesStarted
}

func notBeforeOf(c *Client) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.notBefore
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(msg)
}
