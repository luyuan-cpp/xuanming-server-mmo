package assetop

import (
	"context"
	"sync"
	"testing"
	"time"

	assetpb "proto/common/asset"
)

// 本文件只验一件事:单行预算怎么在「投递」和「落库」之间分,以及落库那一步会不会被
// 父 ctx 拖死。这是钱路径上最贵的一条时序 —— scene 已经扣了钱、outbox 行却写不回去。

// ctxProbe 记下 Store 被调用那一刻拿到的 ctx 长什么样。只看 ctx:参数 fakeStore 已经记了。
type ctxProbe struct {
	calls       int
	err         error // 调用那一刻的 ctx.Err()
	hasDeadline bool
	remaining   time.Duration
}

func (p *ctxProbe) record(ctx context.Context) {
	p.calls++
	p.err = ctx.Err()
	if deadline, ok := ctx.Deadline(); ok {
		p.hasDeadline = true
		p.remaining = time.Until(deadline)
	}
}

// settleSpyStore 在 fakeStore 之上偷看落库那一步拿到的 ctx。
// 自己的锁叫 spyMu:fakeStore 里已经有一把 mu,同名会让人以为是同一把。
type settleSpyStore struct {
	*fakeStore

	spyMu      sync.Mutex
	finalize   ctxProbe
	reschedule ctxProbe
}

func (s *settleSpyStore) Finalize(ctx context.Context, op Op, status Status, res Result, nowMs uint64) (bool, error) {
	s.spyMu.Lock()
	s.finalize.record(ctx)
	s.spyMu.Unlock()
	return s.fakeStore.Finalize(ctx, op, status, res, nowMs)
}

func (s *settleSpyStore) Reschedule(ctx context.Context, op Op, nextAttemptMs uint64, res Result, nowMs uint64) error {
	s.spyMu.Lock()
	s.reschedule.record(ctx)
	s.spyMu.Unlock()
	return s.fakeStore.Reschedule(ctx, op, nextAttemptMs, res, nowMs)
}

func (s *settleSpyStore) probes() (finalize, reschedule ctxProbe) {
	s.spyMu.Lock()
	defer s.spyMu.Unlock()
	return s.finalize, s.reschedule
}

// budgetEatingApplier 一直等到自己的 ctx 过期才交出结果:
// 这就是「scene 拖到预算最后一刻才答复」的样子。
type budgetEatingApplier struct {
	mu          sync.Mutex
	hasDeadline bool
	remaining   time.Duration

	res            Result
	afterExhausted func() // 预算结束、scene 答复交付前同步触发父取消，消除等长 timer 观察竞态。
}

func (a *budgetEatingApplier) Do(ctx context.Context, _ RPC, _ *assetpb.AssetOpRequest) (Result, error) {
	a.mu.Lock()
	if deadline, ok := ctx.Deadline(); ok {
		a.hasDeadline = true
		a.remaining = time.Until(deadline)
	}
	a.mu.Unlock()
	<-ctx.Done()
	if a.afterExhausted != nil {
		a.afterExhausted()
	}
	// 预算耗尽的同一瞬间答复到了:结局有效,钱已经动了。
	return a.res, nil
}

func (a *budgetEatingApplier) budget() (time.Duration, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.remaining, a.hasDeadline
}

// 把投递预算压到 120ms,免得测试真等 1.8s;落库预留仍是 settleBudget,分配逻辑不变。
func tightBudget(c *LoopConfig) { c.OpBudget = settleBudget + 120*time.Millisecond }

// 投递吃满预算、父 ctx 已经过期之后,落库仍须拿到可用的 ctx(RequireSQLCommit 关闭,HEAD 口径)。
//
// 修复前这里是会丢钱的:scene 扣了钱,Finalize 拿着死 ctx 写不进去,行还是 PENDING,
// 下一轮重投又投一次。
func TestFinalizeKeepsBudgetAfterApplyExhaustsIt(t *testing.T) {
	op := testOp(1)
	store := &settleSpyStore{fakeStore: newFakeStore(op)}
	applier := &budgetEatingApplier{res: resultApplied(true)}
	loop, _ := newTestLoop(t, store, applier, tightBudget)

	// 父 ctx 只剩投递那一段:同步路径的调用方在进来之前已经花掉了前面一部分预算。
	// 投递吃满之后父 ctx 必然过期,正是修复前落库拿到死 ctx 的那条时序。
	parent, cancel := context.WithTimeout(context.Background(), loop.cfg.OpBudget-settleBudget)
	defer cancel()
	applier.afterExhausted = cancel // 等实际投递预算耗尽，再同步确认父取消。

	processed, err := loop.ProcessOne(parent, op)
	if err != nil {
		t.Fatalf("ProcessOne 出错: %v", err)
	}
	if parent.Err() == nil {
		t.Fatal("父 ctx 本应已经过期,否则这个用例什么也没验到")
	}

	finalize, _ := store.probes()
	if finalize.calls != 1 {
		t.Fatalf("应当落库一次,实际 %d 次", finalize.calls)
	}
	if finalize.err != nil {
		t.Fatalf("落库拿到的 ctx 已经不可用: %v", finalize.err)
	}
	if !finalize.hasDeadline {
		t.Fatal("落库的 ctx 必须自带超时:没有超时,一个卡住的库能把关停永远钉死")
	}
	if finalize.remaining <= settleBudget/2 || finalize.remaining > settleBudget {
		t.Fatalf("落库应当拿到完整的 %v 预留,实际只剩 %v", settleBudget, finalize.remaining)
	}
	if !processed.Finalized || processed.Status != StatusApplied {
		t.Fatalf("应当终结为 Applied,实际 %+v", processed)
	}
}

// RequireSQLCommit 打开时同一条时序:投递吃满预算之后已没有时间读 SQL 证据,不得终结,
// 但重排仍须拿到完整的落库预留(否则这一行会原地被反复领取投递)。
func TestAwaitSQLKeepsBudgetAfterApplyExhaustsIt(t *testing.T) {
	op := testOp(1)
	store := &settleSpyStore{fakeStore: newFakeStore(op)}
	applier := &budgetEatingApplier{res: resultApplied(true)}
	loop, _ := newTestLoop(t, store, applier, func(c *LoopConfig) {
		tightBudget(c)
		c.RequireSQLCommit = true
	})

	ledger := sqlLedgerFixture(true, op)
	loop.Ledger = ledger
	if err := loop.CheckWiring(); err != nil {
		t.Fatal(err)
	}

	// 父 ctx 只剩投递那一段:同步路径的调用方在进来之前已经花掉了前面一部分预算。
	// 投递吃满之后父 ctx 必然过期,正是修复前落库拿到死 ctx 的那条时序。
	parent, cancel := context.WithTimeout(context.Background(), loop.cfg.OpBudget-settleBudget)
	defer cancel()
	applier.afterExhausted = cancel // 不靠两个等长 timer 的调度顺序证明父取消。

	processed, err := loop.ProcessOne(parent, op)
	if err != nil {
		t.Fatalf("ProcessOne 出错: %v", err)
	}
	if parent.Err() == nil {
		t.Fatal("父 ctx 本应已经过期,否则这个用例什么也没验到")
	}

	if ledger.reads.Load() != 0 {
		t.Fatal("apply 预算已耗尽，不得读取 SQL 证据")
	}
	finalize, reschedule := store.probes()
	if finalize.calls != 0 || reschedule.calls != 1 {
		t.Fatalf("预算耗尽后未取得 SQL 证据，必须可靠重排: finalize=%d reschedule=%d", finalize.calls, reschedule.calls)
	}
	if reschedule.err != nil {
		t.Fatalf("重排拿到的 ctx 已经不可用: %v", reschedule.err)
	}
	if !reschedule.hasDeadline {
		t.Fatal("落库的 ctx 必须自带超时:没有超时,一个卡住的库能把关停永远钉死")
	}
	if reschedule.remaining <= settleBudget/2 || reschedule.remaining > settleBudget {
		t.Fatalf("重排应当拿到完整的 %v 预留,实际只剩 %v", settleBudget, reschedule.remaining)
	}
	if processed.Finalized {
		t.Fatalf("无 SQL 证据不得终结，实际 %+v", processed)
	}
}

// 投递拿到的是 OpBudget 扣掉落库预留,不是整个 OpBudget。
func TestApplyBudgetReservesSettleShare(t *testing.T) {
	op := testOp(1)
	store := &settleSpyStore{fakeStore: newFakeStore(op)}
	applier := &budgetEatingApplier{res: resultApplied(true)}
	loop, _ := newTestLoop(t, store, applier, tightBudget)
	want := loop.cfg.OpBudget - settleBudget

	// 父 ctx 不带截止:投递的截止时刻只可能来自本包自己切的那一份预算。
	if _, err := loop.ProcessOne(context.Background(), op); err != nil {
		t.Fatalf("ProcessOne 出错: %v", err)
	}
	got, ok := applier.budget()
	if !ok {
		t.Fatal("投递的 ctx 必须带截止时间")
	}
	if got > want || got < want-50*time.Millisecond {
		t.Fatalf("投递预算应当约等于 %v,实际 %v", want, got)
	}
}

// 已准入投递在 scene 答复交付边界取消父 ctx，终局仍要落库(RequireSQLCommit 关闭,HEAD 口径)。
func TestFinalizeSurvivesCanceledParent(t *testing.T) {
	op := testOp(1)
	store := &settleSpyStore{fakeStore: newFakeStore(op)}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	// scene 已经答复过了:结局到手,钱已经动了,这一步只剩写回去。
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		cancel() // 已准入的投递取得 scene 结局后，父调用方退出。
		return resultApplied(true), nil
	}}
	loop, _ := newTestLoop(t, store, applier, nil)

	processed, err := loop.ProcessOne(parent, op)
	if err != nil {
		t.Fatalf("ProcessOne 出错: %v", err)
	}
	if parent.Err() != context.Canceled {
		t.Fatal("scene 答复交付边界必须已同步取消父 ctx")
	}
	finalize, _ := store.probes()
	if finalize.calls != 1 || finalize.err != nil {
		t.Fatalf("父 ctx 取消不得连累落库,实际 calls=%d err=%v", finalize.calls, finalize.err)
	}
	if !finalize.hasDeadline {
		t.Fatal("落库的 ctx 必须自带超时")
	}
	if !processed.Finalized {
		t.Fatalf("应当终结,实际 %+v", processed)
	}
}

// RequireSQLCommit 打开且已准入投递耗尽 apply 预算时，不读取或伪造 SQL 证据；父取消不连累待办可靠写回。
func TestUnconfirmedResultSurvivesCanceledParentAsPending(t *testing.T) {
	op := testOp(1)
	store := &settleSpyStore{fakeStore: newFakeStore(op)}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 已准入的 scene 投递耗尽 apply 预算后交付结果，并同步取消父调用；不能再用该预算取 SQL 证据。
	applier := &budgetEatingApplier{res: resultApplied(true), afterExhausted: cancel}
	loop, _ := newTestLoop(t, store, applier, func(c *LoopConfig) {
		tightBudget(c)
		requireSQLCommit(c)
	})
	ledger := sqlLedgerFixture(true, op)
	loop.Ledger = ledger
	if err := loop.CheckWiring(); err != nil {
		t.Fatal(err)
	}

	processed, err := loop.ProcessOne(parent, op)
	if err != nil {
		t.Fatalf("ProcessOne 出错: %v", err)
	}
	if parent.Err() != context.Canceled {
		t.Fatal("scene 答复交付边界必须已同步取消父 ctx")
	}
	if ledger.reads.Load() != 0 || processed.Result.SQLCommitted {
		t.Fatal("已耗尽取证预算不得读取或伪造 SQL 已提交证据")
	}
	finalize, reschedule := store.probes()
	if finalize.calls != 0 || reschedule.calls != 1 || reschedule.err != nil {
		t.Fatalf("父 ctx 取消后应可靠保留未确认待办: finalize=%d reschedule=%d err=%v", finalize.calls, reschedule.calls, reschedule.err)
	}
	if !reschedule.hasDeadline {
		t.Fatal("落库的 ctx 必须自带超时")
	}
	if processed.Finalized {
		t.Fatalf("缺 SQL 证据不得终结,实际 %+v", processed)
	}
}

// 重排走同一份预留:父 ctx 取消时 attempts 也得推得动,否则那一行会原地被反复投递。
func TestRescheduleSurvivesCanceledParent(t *testing.T) {
	op := testOp(1)
	store := &settleSpyStore{fakeStore: newFakeStore(op)}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		cancel()                         // 已准入的 scene 结果交付后父调用方退出，重排仍有独立预算。
		return resultApplied(false), nil // 结局有了但没落盘 → await_durable 重排
	}}
	loop, _ := newTestLoop(t, store, applier, nil)

	if _, err := loop.ProcessOne(parent, op); err != nil {
		t.Fatalf("ProcessOne 出错: %v", err)
	}
	if parent.Err() != context.Canceled {
		t.Fatal("scene 答复交付边界必须已同步取消父 ctx")
	}
	_, reschedule := store.probes()
	if reschedule.calls != 1 || reschedule.err != nil {
		t.Fatalf("父 ctx 取消不得连累重排,实际 calls=%d err=%v", reschedule.calls, reschedule.err)
	}
	if !reschedule.hasDeadline {
		t.Fatal("重排的 ctx 必须自带超时")
	}
}

// 准入前取消与已接收结果的收尾不同：不得先派发，再借独立 settle 预算做新副作用。
func TestProcessOneRejectsAlreadyCanceledParent(t *testing.T) {
	op := testOp(1)
	store := &settleSpyStore{fakeStore: newFakeStore(op)}
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		t.Fatal("已取消的新操作不得派发 scene RPC")
		return Result{}, nil
	}}
	loop, _ := newTestLoop(t, store, applier, nil)
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	processed, err := loop.ProcessOne(parent, op)
	if err != context.Canceled || processed.Finalized || applier.calls.Load() != 0 {
		t.Fatalf("已取消操作必须在派发前拒绝: result=%+v err=%v calls=%d", processed, err, applier.calls.Load())
	}
	finalize, reschedule := store.probes()
	if finalize.calls != 0 || reschedule.calls != 0 {
		t.Fatalf("未准入操作不得终结或重排: finalize=%d reschedule=%d", finalize.calls, reschedule.calls)
	}
}
