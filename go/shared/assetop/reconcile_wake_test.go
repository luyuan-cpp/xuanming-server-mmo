package assetop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	assetpb "proto/common/asset"
)

// 本文件覆盖落地唤醒的 Loop 一侧(Loop.WakePlayer,设计 §5.3):
// 它与退避重投走同一条 Claim CAS + ProcessOne,并与 Tick 共用 Workers 预算。
// 订阅、去重、有界并发在 shared/assetopwake 里另测。

// wakeStore 在 fakeStore 之上实现可选的 PlayerPendingLister。
type wakeStore struct {
	*fakeStore

	pending    []uint64
	pendingErr error
	listCalls  int
	gotPlayer  uint64
	gotLimit   int
}

func (s *wakeStore) ListPendingByPlayer(_ context.Context, playerID uint64, limit int) ([]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	s.gotPlayer, s.gotLimit = playerID, limit
	if s.pendingErr != nil {
		return nil, s.pendingErr
	}
	// 故意不按 limit 截断:WakePlayer 自己也必须封顶,不能只信 Store。
	return append([]uint64(nil), s.pending...), nil
}

// newWakeStore 建一个「这些行都未到期」的 Store:ListDue 为空,唤醒照样能领到 ——
// Claim 只看 status 与租约,不看 next_attempt_ms。
func newWakeStore(ops ...Op) *wakeStore {
	base := newFakeStore(ops...)
	base.due = nil
	s := &wakeStore{fakeStore: base}
	for _, op := range ops {
		s.pending = append(s.pending, op.OpID)
	}
	return s
}

func appliedApplier() *fakeApplier {
	return &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) { return resultApplied(true), nil }}
}

// Store 没实现 PlayerPendingLister:唤醒是空操作,不是错误(唤醒只是加速)。
func TestWakePlayerWithoutListerIsNoop(t *testing.T) {
	store := newFakeStore(testOp(1))
	applier := appliedApplier()
	loop, m := newTestLoop(t, store, applier, nil)

	n, err := loop.WakePlayer(context.Background(), 42)
	if err != nil || n != 0 {
		t.Fatalf("未实现按玩家列出时应回 (0, nil),实际 (%d, %v)", n, err)
	}
	if applier.calls.Load() != 0 {
		t.Fatalf("不该投递,实际 %d 次", applier.calls.Load())
	}
	if got := testutil.ToFloat64(m.wakeTotal.WithLabelValues("none")); got != 1 {
		t.Fatalf("wake none 指标应为 1,实际 %v", got)
	}
}

// player_id 为 0 是发布方的 bug,不去查库。
func TestWakePlayerZeroPlayerIDIsNoop(t *testing.T) {
	store := newWakeStore(testOp(1))
	loop, m := newTestLoop(t, store, appliedApplier(), nil)

	if n, err := loop.WakePlayer(context.Background(), 0); err != nil || n != 0 {
		t.Fatalf("player_id=0 应回 (0, nil),实际 (%d, %v)", n, err)
	}
	if store.listCalls != 0 {
		t.Fatalf("player_id=0 不该查库,实际查了 %d 次", store.listCalls)
	}
	if got := testutil.ToFloat64(m.wakeTotal.WithLabelValues("none")); got != 1 {
		t.Fatalf("wake none 指标应为 1,实际 %v", got)
	}
}

// 多行未决:逐行 Claim + ProcessOne,结局与退避重投一致;未到期的行同样领得到。
func TestWakePlayerProcessesAllPendingRows(t *testing.T) {
	store := newWakeStore(testOp(1), testOp(2), testOp(3))
	applier := appliedApplier()
	loop, m := newTestLoop(t, store, applier, nil)
	loop.Ledger = sqlLedgerFixture(true, testOp(1), testOp(2), testOp(3))

	n, err := loop.WakePlayer(context.Background(), 42)
	if err != nil {
		t.Fatalf("WakePlayer 出错: %v", err)
	}
	if n != 3 {
		t.Fatalf("应处理 3 行,实际 %d", n)
	}
	if store.gotPlayer != 42 || store.gotLimit != maxWakeOpsPerPlayer {
		t.Fatalf("应按 player_id=42、limit=%d 列出,实际 player=%d limit=%d", maxWakeOpsPerPlayer, store.gotPlayer, store.gotLimit)
	}
	finalized, _ := store.snapshot()
	if len(finalized) != 3 {
		t.Fatalf("三行都应终结,实际 %+v", finalized)
	}
	store.mu.Lock()
	for _, f := range finalized {
		if tok := store.claimTokens[f.Op.OpID]; tok == 0 || f.Op.LeaseToken != tok {
			t.Errorf("op_id=%d 终结带的令牌 %d 与领取时的 %d 不一致", f.Op.OpID, f.Op.LeaseToken, tok)
		}
	}
	store.mu.Unlock()
	if got := testutil.ToFloat64(m.claimTotal.WithLabelValues("claimed")); got != 3 {
		t.Fatalf("claimed 指标应为 3,实际 %v", got)
	}
	if got := testutil.ToFloat64(m.wakeTotal.WithLabelValues("woke")); got != 1 {
		t.Fatalf("wake woke 指标应为 1(按次计,不按行),实际 %v", got)
	}
}

// 某一行已被退避重投的副本领走:跳过它,不投递,其余照常。
func TestWakePlayerSkipsRowsClaimedElsewhere(t *testing.T) {
	store := newWakeStore(testOp(1), testOp(2), testOp(3))
	store.lost[2] = true
	applier := appliedApplier()
	loop, m := newTestLoop(t, store, applier, nil)
	loop.Ledger = sqlLedgerFixture(true, testOp(1), testOp(3))

	n, err := loop.WakePlayer(context.Background(), 42)
	if err != nil {
		t.Fatalf("WakePlayer 出错: %v", err)
	}
	if n != 2 || applier.calls.Load() != 2 {
		t.Fatalf("应只处理并投递 2 行,实际处理 %d、投递 %d", n, applier.calls.Load())
	}
	finalized, _ := store.snapshot()
	for _, f := range finalized {
		if f.Op.OpID == 2 {
			t.Fatalf("被别人领走的行不得由唤醒终结: %+v", finalized)
		}
	}
	if got := testutil.ToFloat64(m.claimTotal.WithLabelValues("lost")); got != 1 {
		t.Fatalf("claim lost 指标应为 1,实际 %v", got)
	}
}

// 所有行都没领到(或玩家没有未决行):计 none,不计 woke。
func TestWakePlayerNothingClaimedCountsNone(t *testing.T) {
	store := newWakeStore(testOp(1))
	store.lost[1] = true
	loop, m := newTestLoop(t, store, appliedApplier(), nil)

	if n, err := loop.WakePlayer(context.Background(), 42); err != nil || n != 0 {
		t.Fatalf("应回 (0, nil),实际 (%d, %v)", n, err)
	}
	if got := testutil.ToFloat64(m.wakeTotal.WithLabelValues("none")); got != 1 {
		t.Fatalf("wake none 指标应为 1,实际 %v", got)
	}
	if got := testutil.ToFloat64(m.wakeTotal.WithLabelValues("woke")); got != 0 {
		t.Fatalf("没处理任何行不得计 woke,实际 %v", got)
	}
}

// 列出失败:回错误并计数,不投递。
func TestWakePlayerListErrorReported(t *testing.T) {
	store := newWakeStore(testOp(1))
	store.pendingErr = errors.New("连接断了")
	applier := appliedApplier()
	loop, m := newTestLoop(t, store, applier, nil)

	if _, err := loop.WakePlayer(context.Background(), 42); err == nil {
		t.Fatal("列出失败必须回错误")
	}
	if applier.calls.Load() != 0 {
		t.Fatalf("列出失败不该投递,实际 %d 次", applier.calls.Load())
	}
	if got := testutil.ToFloat64(m.wakeTotal.WithLabelValues("error")); got != 1 {
		t.Fatalf("wake error 指标应为 1,实际 %v", got)
	}
	if got := testutil.ToFloat64(m.storeErrorsTotal.WithLabelValues("wake_list")); got != 1 {
		t.Fatalf("wake_list 出错指标应为 1,实际 %v", got)
	}
}

// Store 多回了行也只处理 32 条:多出来的照常等自己的 next_attempt_ms。
func TestWakePlayerCapsRowsPerWake(t *testing.T) {
	ops := make([]Op, 0, 40)
	for i := uint64(1); i <= 40; i++ {
		ops = append(ops, testOp(i))
	}
	store := newWakeStore(ops...)
	applier := appliedApplier()
	loop, _ := newTestLoop(t, store, applier, nil)
	loop.Ledger = sqlLedgerFixture(true, ops...)

	n, err := loop.WakePlayer(context.Background(), 42)
	if err != nil {
		t.Fatalf("WakePlayer 出错: %v", err)
	}
	if n != maxWakeOpsPerPlayer || int(applier.calls.Load()) != maxWakeOpsPerPlayer {
		t.Fatalf("一次唤醒最多处理 %d 行,实际处理 %d、投递 %d", maxWakeOpsPerPlayer, n, applier.calls.Load())
	}
}

// 唤醒与 Tick 共用 Workers 预算:名额被占满时唤醒拿不到名额,预算用完就放弃,不越过上限去投递。
func TestWakePlayerSharesWorkerBudget(t *testing.T) {
	store := newWakeStore(testOp(1), testOp(2))
	applier := appliedApplier()
	loop, _ := newTestLoop(t, store, applier, func(c *LoopConfig) { c.Workers = 1 })

	// 模拟 Tick 的 worker 正占着唯一的名额。
	loop.slots <- struct{}{}
	defer func() { <-loop.slots }()

	// 用取消而不是短超时来结束等待:不足一个 OpBudget 的截止时刻会被 WakePlayer 当成配置错误直接拒掉,
	// 那就测不到「等名额」这一段了。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := time.AfterFunc(50*time.Millisecond, cancel)
	defer stop.Stop()
	n, err := loop.WakePlayer(ctx, 42)
	if err != nil {
		t.Fatalf("预算用完不是错误,实际 %v", err)
	}
	if n != 0 || applier.calls.Load() != 0 {
		t.Fatalf("名额被占满时不得投递,实际处理 %d、投递 %d", n, applier.calls.Load())
	}
	store.mu.Lock()
	claimed := len(store.claimTokens)
	store.mu.Unlock()
	if claimed != 0 {
		t.Fatalf("拿不到名额时连 Claim 都不该发,实际领了 %d 行", claimed)
	}
}

// 总预算连一行都装不下(唤醒超时配得比 OpBudget 还小):回错误、计 error,连库都不查 ——
// 这样的唤醒永远领不到行,不报出来就会静默退化成「从不唤醒」。
func TestWakePlayerRejectsBudgetBelowOpBudget(t *testing.T) {
	store := newWakeStore(testOp(1))
	applier := appliedApplier()
	loop, m := newTestLoop(t, store, applier, nil) // 默认 OpBudget 2.5s

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := loop.WakePlayer(ctx, 42); err == nil {
		t.Fatal("预算不足一个 OpBudget 必须回错误")
	}
	if store.listCalls != 0 || applier.calls.Load() != 0 {
		t.Fatalf("预算不足时不该查库或投递,实际查库 %d 次、投递 %d 次", store.listCalls, applier.calls.Load())
	}
	if got := testutil.ToFloat64(m.wakeTotal.WithLabelValues("error")); got != 1 {
		t.Fatalf("wake error 指标应为 1,实际 %v", got)
	}
}

// 父 ctx 只决定要不要开始下一行:剩余预算不够一整个 OpBudget 就不再领取。已经开始的行照常走完,
// 没轮到的行连 Claim 都不发 —— 领下来却被截在半路,行会带着租约没人处理,或被记成一次传输失败。
func TestWakePlayerStopsClaimingWhenBudgetShort(t *testing.T) {
	store := newWakeStore(testOp(1), testOp(2), testOp(3))
	// 每行投递耗时 400ms(替身不看 ctx):第一行开始时剩约 1.85s >= OpBudget 1.5s;
	// 它做完之后最多剩 1.45s,第二行不再开始。时序不依赖机器快慢,只依赖 sleep 至少睡够。
	// 投递预算 OpBudget-settleBudget = 800ms,装得下这 400ms 再加一次读已提交账本。
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		time.Sleep(400 * time.Millisecond)
		return resultApplied(true), nil
	}}
	loop, m := newTestLoop(t, store, applier, func(c *LoopConfig) { c.OpBudget = 1500 * time.Millisecond })
	loop.Ledger = sqlLedgerFixture(true, testOp(1), testOp(2), testOp(3))

	ctx, cancel := context.WithTimeout(context.Background(), 1850*time.Millisecond)
	defer cancel()
	n, err := loop.WakePlayer(ctx, 42)
	if err != nil {
		t.Fatalf("预算用完不是错误,实际 %v", err)
	}
	if n != 1 || applier.calls.Load() != 1 {
		t.Fatalf("应只处理并投递第一行,实际处理 %d、投递 %d", n, applier.calls.Load())
	}
	store.mu.Lock()
	claimed := len(store.claimTokens)
	store.mu.Unlock()
	if claimed != 1 {
		t.Fatalf("预算不够的行不得领取,实际领了 %d 行", claimed)
	}
	finalized, _ := store.snapshot()
	if len(finalized) != 1 || finalized[0].Op.OpID != 1 {
		t.Fatalf("已开始的第一行应照常终结,实际 %+v", finalized)
	}
	if got := testutil.ToFloat64(m.wakeTotal.WithLabelValues("woke")); got != 1 {
		t.Fatalf("wake woke 指标应为 1,实际 %v", got)
	}
}
