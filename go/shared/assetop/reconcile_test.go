package assetop

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"
)

const testNowMs uint64 = 1_700_000_000_000

func fixedNow() time.Time { return time.UnixMilli(int64(testNowMs)) }

type finalizeCall struct {
	Op     Op
	Status Status
	Res    Result
}

type rescheduleCall struct {
	Op     Op
	NextMs uint64
	Res    Result
}

// fakeStore 是业务 outbox 表的替身:只记录被要求做了什么,不模拟 SQL 语义。
type fakeStore struct {
	mu sync.Mutex

	due     []uint64
	rows    map[uint64]Op
	lost    map[uint64]bool // Claim 返回 false(被别人领走 / 已终结)
	poison  map[uint64]bool // Claim 返回 ErrPoisonRow
	listErr error
	finErr  map[uint64]error

	finalized   []finalizeCall
	rescheduled []rescheduleCall
	claimTokens map[uint64]uint64
	oldest      map[assetpb.AssetOpStream]uint64
}

func newFakeStore(ops ...Op) *fakeStore {
	s := &fakeStore{
		rows:        map[uint64]Op{},
		lost:        map[uint64]bool{},
		poison:      map[uint64]bool{},
		finErr:      map[uint64]error{},
		claimTokens: map[uint64]uint64{},
	}
	for _, op := range ops {
		s.rows[op.OpID] = op
		s.due = append(s.due, op.OpID)
	}
	return s
}

func (s *fakeStore) ListDue(_ context.Context, _ uint64, limit int) ([]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := append([]uint64(nil), s.due...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeStore) Claim(_ context.Context, opID, _, _, token uint64) (Op, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poison[opID] {
		return Op{}, false, ErrPoisonRow
	}
	if s.lost[opID] {
		return Op{}, false, nil
	}
	op, ok := s.rows[opID]
	if !ok {
		return Op{}, false, nil
	}
	op.LeaseToken = token
	s.claimTokens[opID] = token
	return op, true, nil
}

func (s *fakeStore) Finalize(_ context.Context, op Op, status Status, res Result, _ uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.finErr[op.OpID]; err != nil {
		return false, err
	}
	s.finalized = append(s.finalized, finalizeCall{Op: op, Status: status, Res: res})
	return true, nil
}

func (s *fakeStore) Reschedule(_ context.Context, op Op, nextAttemptMs uint64, res Result, _ uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rescheduled = append(s.rescheduled, rescheduleCall{Op: op, NextMs: nextAttemptMs, Res: res})
	return nil
}

func (s *fakeStore) snapshot() ([]finalizeCall, []rescheduleCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]finalizeCall(nil), s.finalized...), append([]rescheduleCall(nil), s.rescheduled...)
}

// fakeAgeStore 在 fakeStore 之上实现可选的 PendingAgeReader。
type fakeAgeStore struct{ *fakeStore }

func (s fakeAgeStore) OldestPendingCreatedMs(_ context.Context, stream assetpb.AssetOpStream) (uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	createdMs, ok := s.oldest[stream]
	return createdMs, ok, nil
}

type fakeApplier struct {
	fn func(rpc RPC, req *assetpb.AssetOpRequest) (Result, error)

	calls       atomic.Int32
	inflight    atomic.Int32
	maxInflight atomic.Int32
	lastRPC     atomic.Int32
}

func (a *fakeApplier) Do(_ context.Context, rpc RPC, req *assetpb.AssetOpRequest) (Result, error) {
	a.calls.Add(1)
	a.lastRPC.Store(int32(rpc))
	cur := a.inflight.Add(1)
	for {
		peak := a.maxInflight.Load()
		if cur <= peak || a.maxInflight.CompareAndSwap(peak, cur) {
			break
		}
	}
	defer a.inflight.Add(-1)
	return a.fn(rpc, req)
}

func testOp(opID uint64) Op {
	return Op{
		OpID:          opID,
		PlayerID:      42,
		Stream:        assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT,
		Seq:           opID,
		StreamEpoch:   1700000000000,
		CorrelationID: opID,
		TxType:        24,
		Bundle: &assetpb.AssetBundle{
			Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 30}},
		},
	}
}

func newTestLoop(t *testing.T, store Store, applier Applier, mutate func(*LoopConfig)) (*Loop, *Metrics) {
	t.Helper()
	cfg := DefaultLoopConfig()
	cfg.Interval = 10 * time.Millisecond
	if mutate != nil {
		mutate(&cfg)
	}
	m := NewMetrics(prometheus.NewRegistry(), "test")
	loop, err := NewLoop(cfg, store, applier, m, fixedNow)
	if err != nil {
		t.Fatalf("NewLoop 失败: %v", err)
	}
	// 退避抖动固定成下界,断言才能写死。
	loop.Rand = func() float64 { return 0 }
	return loop, m
}

func resultApplied(durable bool) Result {
	return Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: durable}
}

func TestTickFinalizesDurableApplied(t *testing.T) {
	store := newFakeStore(testOp(1))
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return resultApplied(true), nil
	}}
	loop, m := newTestLoop(t, store, applier, nil)

	if got := loop.Tick(context.Background()); got != 1 {
		t.Fatalf("应处理 1 行,实际 %d", got)
	}
	finalized, rescheduled := store.snapshot()
	if len(finalized) != 1 || finalized[0].Status != StatusApplied {
		t.Fatalf("应当以 Applied 终结一次,实际 %+v", finalized)
	}
	if len(rescheduled) != 0 {
		t.Fatalf("终结的行不该再重排,实际 %+v", rescheduled)
	}
	if got := testutil.ToFloat64(m.finalizeTotal.WithLabelValues("guild_debit", "applied")); got != 1 {
		t.Fatalf("finalize 指标应为 1,实际 %v", got)
	}
	if RPC(applier.lastRPC.Load()) != RPCDebit {
		t.Fatalf("GUILD_DEBIT 流应当发 Debit,实际 %s", RPC(applier.lastRPC.Load()))
	}
}

// 过了业务截止时间就改发中止,而不是继续扣钱。
func TestProcessOneSwitchesToAbortAfterDeadline(t *testing.T) {
	op := testOp(1)
	op.DeadlineMs = testNowMs - 1
	store := newFakeStore(op)
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true}, nil
	}}
	loop, _ := newTestLoop(t, store, applier, nil)

	processed, err := loop.ProcessOne(context.Background(), op)
	if err != nil {
		t.Fatalf("ProcessOne 出错: %v", err)
	}
	if RPC(applier.lastRPC.Load()) != RPCAbort {
		t.Fatalf("到期行应当发 AbortDebit,实际 %s", RPC(applier.lastRPC.Load()))
	}
	if processed.Status != StatusAborted {
		t.Fatalf("中止占位应当落 Aborted,实际 %s", processed.Status)
	}
}

func TestProcessOneAwaitsDurable(t *testing.T) {
	op := testOp(1)
	store := newFakeStore(op)
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return resultApplied(false), nil
	}}
	loop, m := newTestLoop(t, store, applier, nil)

	if _, err := loop.ProcessOne(context.Background(), op); err != nil {
		t.Fatalf("ProcessOne 出错: %v", err)
	}
	finalized, rescheduled := store.snapshot()
	if len(finalized) != 0 {
		t.Fatalf("未落盘不得终结,实际 %+v", finalized)
	}
	if len(rescheduled) != 1 || rescheduled[0].NextMs != testNowMs+500 {
		t.Fatalf("应当 500ms 后重查,实际 %+v", rescheduled)
	}
	if got := testutil.ToFloat64(m.rescheduleTotal.WithLabelValues("guild_debit", "await_durable")); got != 1 {
		t.Fatalf("await_durable 重排指标应为 1,实际 %v", got)
	}
}

func TestProcessOneRetryUsesBackoff(t *testing.T) {
	op := testOp(1)
	op.Attempts = 3
	store := newFakeStore(op)
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY, Reason: ReasonInBattle}, nil
	}}
	loop, m := newTestLoop(t, store, applier, nil)

	if _, err := loop.ProcessOne(context.Background(), op); err != nil {
		t.Fatalf("ProcessOne 出错: %v", err)
	}
	_, rescheduled := store.snapshot()
	if len(rescheduled) != 1 {
		t.Fatalf("应当重排一次,实际 %+v", rescheduled)
	}
	if delay := rescheduled[0].NextMs - testNowMs; delay < 6400 || delay > 9600 {
		t.Fatalf("attempts=3 的退避 %dms 不在 [6400, 9600]", delay)
	}
	if got := testutil.ToFloat64(m.rescheduleTotal.WithLabelValues("guild_debit", "retry")); got != 1 {
		t.Fatalf("retry 重排指标应为 1,实际 %v", got)
	}
}

// UNKNOWN 必须告警 + 长退避,且**绝不终结**。
func TestProcessOneUnknownAlertsWithoutFinalizing(t *testing.T) {
	op := testOp(1)
	store := newFakeStore(op)
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN, Reason: ReasonAuthFailed}, nil
	}}
	loop, m := newTestLoop(t, store, applier, nil)

	if _, err := loop.ProcessOne(context.Background(), op); err != nil {
		t.Fatalf("ProcessOne 出错: %v", err)
	}
	finalized, rescheduled := store.snapshot()
	if len(finalized) != 0 {
		t.Fatalf("UNKNOWN 不得终结,实际 %+v", finalized)
	}
	if len(rescheduled) != 1 || rescheduled[0].NextMs != testNowMs+60000 {
		t.Fatalf("UNKNOWN 应当 60s 后重试,实际 %+v", rescheduled)
	}
	if got := testutil.ToFloat64(m.unknownTotal.WithLabelValues("guild_debit")); got != 1 {
		t.Fatalf("unknown 指标应为 1,实际 %v", got)
	}
}

// 一行终结失败不该拖垮整批。
func TestTickContinuesAfterFinalizeError(t *testing.T) {
	store := newFakeStore(testOp(1), testOp(2))
	store.finErr[1] = errors.New("死锁")
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return resultApplied(true), nil
	}}
	loop, m := newTestLoop(t, store, applier, func(c *LoopConfig) { c.Workers = 1 })

	if got := loop.Tick(context.Background()); got != 2 {
		t.Fatalf("两行都应被处理,实际 %d", got)
	}
	finalized, _ := store.snapshot()
	if len(finalized) != 1 || finalized[0].Op.OpID != 2 {
		t.Fatalf("第二行应当照常终结,实际 %+v", finalized)
	}
	if got := testutil.ToFloat64(m.storeErrorsTotal.WithLabelValues("finalize")); got != 1 {
		t.Fatalf("finalize 出错指标应为 1,实际 %v", got)
	}
}

// 重排必须带回**本次领取**的令牌,否则租约已被别人抢走时还会把自己的结果写回去。
func TestRescheduleCarriesLeaseToken(t *testing.T) {
	store := newFakeStore(testOp(1))
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return resultApplied(false), nil
	}}
	loop, _ := newTestLoop(t, store, applier, nil)

	loop.Tick(context.Background())

	_, rescheduled := store.snapshot()
	if len(rescheduled) != 1 {
		t.Fatalf("应当重排一次,实际 %+v", rescheduled)
	}
	store.mu.Lock()
	token := store.claimTokens[1]
	store.mu.Unlock()
	if token == 0 {
		t.Fatal("领取令牌不得为 0")
	}
	if rescheduled[0].Op.LeaseToken != token {
		t.Fatalf("重排带的令牌 %d 与领取时的 %d 不一致", rescheduled[0].Op.LeaseToken, token)
	}
}

// 列出来之后被别人领走:跳过,不投递。
func TestListDueThenClaimLostSkips(t *testing.T) {
	store := newFakeStore(testOp(1))
	store.lost[1] = true
	// 被别人领走的行一旦被投递,下面的 calls 断言就会炸;不在 worker goroutine 里
	// 调 t.Fatal(那不是测试自己的 goroutine)。
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return Result{}, errors.New("不该被调用")
	}}
	loop, m := newTestLoop(t, store, applier, nil)

	if got := loop.Tick(context.Background()); got != 0 {
		t.Fatalf("不该处理任何行,实际 %d", got)
	}
	if applier.calls.Load() != 0 {
		t.Fatalf("不该调用 applier,实际 %d 次", applier.calls.Load())
	}
	if got := testutil.ToFloat64(m.claimTotal.WithLabelValues("lost")); got != 1 {
		t.Fatalf("lost 指标应为 1,实际 %v", got)
	}
}

// 毒行(payload 解不开)跳过,下一行照常。
func TestPoisonRowSkipped(t *testing.T) {
	store := newFakeStore(testOp(1), testOp(2))
	store.poison[1] = true
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return resultApplied(true), nil
	}}
	loop, m := newTestLoop(t, store, applier, func(c *LoopConfig) { c.Workers = 1 })

	if got := loop.Tick(context.Background()); got != 1 {
		t.Fatalf("只应处理第二行,实际 %d", got)
	}
	finalized, _ := store.snapshot()
	if len(finalized) != 1 || finalized[0].Op.OpID != 2 {
		t.Fatalf("第二行应当照常终结,实际 %+v", finalized)
	}
	if got := testutil.ToFloat64(m.storeErrorsTotal.WithLabelValues("decode")); got != 1 {
		t.Fatalf("decode 指标应为 1,实际 %v", got)
	}
	if got := testutil.ToFloat64(m.claimTotal.WithLabelValues("poison")); got != 1 {
		t.Fatalf("poison 指标应为 1,实际 %v", got)
	}
}

// 并发必须封顶在 Workers:scene 的 sync poller 只有 8 条,打满会连带拖垮进场与开战。
func TestWorkersBounded(t *testing.T) {
	ops := make([]Op, 0, 12)
	for i := uint64(1); i <= 12; i++ {
		ops = append(ops, testOp(i))
	}
	store := newFakeStore(ops...)

	release := make(chan struct{})
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		<-release
		return resultApplied(true), nil
	}}
	loop, _ := newTestLoop(t, store, applier, func(c *LoopConfig) { c.Workers = 3 })

	done := make(chan int, 1)
	go func() { done <- loop.Tick(context.Background()) }()

	// 等到确实有请求在途,再放行:这样"同时在途数"才有意义。
	deadline := time.Now().Add(2 * time.Second)
	for applier.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := applier.maxInflight.Load(); got > 3 {
		t.Fatalf("同时在途 %d 条,超过 Workers=3", got)
	}
	close(release)

	select {
	case got := <-done:
		if got != 12 {
			t.Fatalf("应处理 12 行,实际 %d", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Tick 没有在预期时间内返回")
	}
	if got := applier.maxInflight.Load(); got > 3 {
		t.Fatalf("同时在途 %d 条,超过 Workers=3", got)
	}
}

func TestNewLoopRejectsBadConfig(t *testing.T) {
	store := newFakeStore()
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) { return Result{}, nil }}

	cases := []struct {
		name   string
		mutate func(*LoopConfig)
	}{
		{"预算超过租约", func(c *LoopConfig) { c.OpBudget = 9 * time.Second }},
		{"Workers 为 0", func(c *LoopConfig) { c.Workers = 0 }},
		{"Workers 超上限", func(c *LoopConfig) { c.Workers = maxWorkers + 1 }},
		{"Batch 小于 Workers", func(c *LoopConfig) { c.Batch = 1; c.Workers = 4 }},
		{"退避区间反了", func(c *LoopConfig) { c.BaseBackoff = time.Minute; c.MaxBackoff = time.Second }},
		{"Interval 非正", func(c *LoopConfig) { c.Interval = 0 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := DefaultLoopConfig()
			c.mutate(&cfg)
			if _, err := NewLoop(cfg, store, applier, nil, fixedNow); err == nil {
				t.Fatal("非法配置必须被拒")
			}
		})
	}
}

// 离线但账本里已有结局:直接终结,不必等玩家上线。
func TestLedgerReadFinalizesSeenOffline(t *testing.T) {
	op := testOp(1)
	op.Attempts = 3
	store := newFakeStore(op)
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE, Local: true}, nil
	}}
	loop, m := newTestLoop(t, store, applier, nil)
	loop.Ledger = &fakeLedger{ledger: ledgerWithApplied(op.Stream, op.StreamEpoch, op.Seq, false)}

	processed, err := loop.ProcessOne(context.Background(), op)
	if err != nil {
		t.Fatalf("ProcessOne 出错: %v", err)
	}
	if !processed.Finalized || processed.Status != StatusApplied {
		t.Fatalf("应当按已落盘账本终结为 Applied,实际 %+v", processed)
	}
	if got := testutil.ToFloat64(m.ledgerReadTotal.WithLabelValues("finalized")); got != 1 {
		t.Fatalf("ledger_read finalized 指标应为 1,实际 %v", got)
	}
}

// 离线且账本里没见过这个 seq:保持 PENDING 继续等,**不得**自行判中止(不变量 I7)。
func TestLedgerReadUnseenStaysPending(t *testing.T) {
	op := testOp(1)
	op.Attempts = 5
	store := newFakeStore(op)
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE, Local: true}, nil
	}}
	loop, m := newTestLoop(t, store, applier, nil)
	loop.Ledger = &fakeLedger{ledger: ledgerWithApplied(op.Stream, op.StreamEpoch, 99, false)}

	if _, err := loop.ProcessOne(context.Background(), op); err != nil {
		t.Fatalf("ProcessOne 出错: %v", err)
	}
	finalized, rescheduled := store.snapshot()
	if len(finalized) != 0 {
		t.Fatalf("未见的 seq 不得终结,实际 %+v", finalized)
	}
	if len(rescheduled) != 1 {
		t.Fatalf("应当退避重试,实际 %+v", rescheduled)
	}
	if got := testutil.ToFloat64(m.ledgerReadTotal.WithLabelValues("unseen")); got != 1 {
		t.Fatalf("ledger_read unseen 指标应为 1,实际 %v", got)
	}
}

// 尝试次数不够时不去读账本:刚离线的玩家很快会回来,没必要压 data_service。
func TestLedgerReadSkippedBeforeMinAttempts(t *testing.T) {
	op := testOp(1)
	op.Attempts = 1
	store := newFakeStore(op)
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE, Local: true}, nil
	}}
	loop, _ := newTestLoop(t, store, applier, nil)
	ledger := &fakeLedger{ledger: ledgerWithApplied(op.Stream, op.StreamEpoch, op.Seq, false)}
	loop.Ledger = ledger

	if _, err := loop.ProcessOne(context.Background(), op); err != nil {
		t.Fatalf("ProcessOne 出错: %v", err)
	}
	if ledger.reads.Load() != 0 {
		t.Fatalf("不该读账本,实际读了 %d 次", ledger.reads.Load())
	}
}

// 部分发放要落 AppliedPartial,业务方据此**不做**对侧账。
func TestPartialNotBookedCounterSide(t *testing.T) {
	op := testOp(1)
	op.Stream = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT
	store := newFakeStore(op)
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return Result{
			Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED,
			Reason:  ReasonPartialApplied,
			Durable: true,
			Partial: true,
		}, nil
	}}
	loop, m := newTestLoop(t, store, applier, nil)

	processed, err := loop.ProcessOne(context.Background(), op)
	if err != nil {
		t.Fatalf("ProcessOne 出错: %v", err)
	}
	if processed.Status != StatusAppliedPartial {
		t.Fatalf("应当落 AppliedPartial,实际 %s", processed.Status)
	}
	finalized, _ := store.snapshot()
	if len(finalized) != 1 || finalized[0].Status != StatusAppliedPartial {
		t.Fatalf("Store 收到的状态不对: %+v", finalized)
	}
	if got := testutil.ToFloat64(m.finalizeTotal.WithLabelValues("guild_credit", "applied_partial")); got != 1 {
		t.Fatalf("applied_partial 终结指标应为 1,实际 %v", got)
	}
}

// 曾见部分发放之后,Reason 为 0 的重排(本地 NOT_HERE / 传输失败 / 坏流号)
// 不得把行上的 27007 抹掉:partial_seqs 环被挤掉后它是唯一证据(规格 §4.33)。
func TestReschedulePreservesPartialReason(t *testing.T) {
	op := testOp(1)
	op.Stream = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT
	op.LastReason = ReasonPartialApplied
	store := newFakeStore(op)
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		// 玩家过图 / 离线:本地合成的 NOT_HERE,Reason 为 0。
		return Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE, Local: true}, nil
	}}
	loop, _ := newTestLoop(t, store, applier, nil)

	processed, err := loop.ProcessOne(context.Background(), op)
	if err != nil {
		t.Fatalf("ProcessOne 出错: %v", err)
	}
	_, rescheduled := store.snapshot()
	if len(rescheduled) != 1 {
		t.Fatalf("应当重排一次,实际 %+v", rescheduled)
	}
	if rescheduled[0].Res.Reason != ReasonPartialApplied {
		t.Fatalf("写回行上的 last_reason 应当仍是 %d,实际 %d", ReasonPartialApplied, rescheduled[0].Res.Reason)
	}
	// 返回给同步路径的仍是本次真实答复,不许被粘性改写。
	if processed.Result.Reason != 0 {
		t.Fatalf("返回值应当保留本次答复的 reason=0,实际 %d", processed.Result.Reason)
	}
}

// partial_seqs 环被挤掉之后 scene 只会回一个普通的 APPLIED(不带 partial、reason 为 0);
// 行上的 27007 必须仍然把它判成部分发放,否则半额发放会被按全额入对侧账。
func TestLastReasonStillMarksPartialAfterRingEviction(t *testing.T) {
	op := testOp(1)
	op.Stream = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT
	op.LastReason = ReasonPartialApplied
	store := newFakeStore(op)
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return resultApplied(true), nil
	}}
	loop, _ := newTestLoop(t, store, applier, nil)

	processed, err := loop.ProcessOne(context.Background(), op)
	if err != nil {
		t.Fatalf("ProcessOne 出错: %v", err)
	}
	if processed.Status != StatusAppliedPartial {
		t.Fatalf("行上有 27007 时必须落 AppliedPartial,实际 %s", processed.Status)
	}
	finalized, _ := store.snapshot()
	if len(finalized) != 1 || finalized[0].Status != StatusAppliedPartial {
		t.Fatalf("Store 收到的状态不对: %+v", finalized)
	}
}

// ListDue 出错只计数,不 panic 也不静默。
func TestTickListErrorCounted(t *testing.T) {
	store := newFakeStore(testOp(1))
	store.listErr = errors.New("连接断了")
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) { return Result{}, nil }}
	loop, m := newTestLoop(t, store, applier, nil)

	if got := loop.Tick(context.Background()); got != 0 {
		t.Fatalf("列表失败时不该处理任何行,实际 %d", got)
	}
	if got := testutil.ToFloat64(m.storeErrorsTotal.WithLabelValues("list")); got != 1 {
		t.Fatalf("list 出错指标应为 1,实际 %v", got)
	}
}

// 最老未决行年龄:Store 实现了可选接口才刷新。
func TestRefreshPendingAge(t *testing.T) {
	base := newFakeStore()
	base.oldest = map[assetpb.AssetOpStream]uint64{
		assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT: testNowMs - 30_000,
	}
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) { return Result{}, nil }}
	loop, m := newTestLoop(t, fakeAgeStore{base}, applier, func(c *LoopConfig) {
		c.Streams = []assetpb.AssetOpStream{
			assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT,
			assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT,
		}
	})

	loop.refreshPendingAge(context.Background())

	if got := testutil.ToFloat64(m.pendingOldestAge.WithLabelValues("guild_debit")); got != 30 {
		t.Fatalf("最老未决行年龄应为 30s,实际 %v", got)
	}
	if got := testutil.ToFloat64(m.pendingOldestAge.WithLabelValues("guild_credit")); got != 0 {
		t.Fatalf("没有未决行时应为 0,实际 %v", got)
	}
}

// fakeLedger 一律用指针:它内部有计数器,拷贝会让断言看错次数。
type fakeLedger struct {
	ledger *componentpb.PlayerAssetOpLedgerComp
	err    error
	reads  atomic.Int32
}

func (f *fakeLedger) ReadPersistedLedger(context.Context, uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	f.reads.Add(1)
	return f.ledger, f.err
}
