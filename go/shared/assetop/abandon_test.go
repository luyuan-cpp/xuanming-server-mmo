package assetop

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"

	"shared/scenenode"
)

// 自动放弃(abandon.go,设计 §6.3)。六个条件各给反例,外加一条全部成立的成功路径:
// 任何一条证据拿不到都必须跳过、计数,并且持租约的出口要么终结、要么还租约。

// abandonCutoffForTest 是默认 AbandonAfter(180 天)下、testNowMs 时刻的候选截止时刻。
var abandonCutoffForTest = testNowMs - uint64(180*24*time.Hour/time.Millisecond)

// abandonListCall 是一次 ListAbandonCandidates 的入参。
type abandonListCall struct {
	createdBeforeMs uint64
	afterOpID       uint64
	limit           int
}

// abandonStore 在 fakeStore 之上实现可选的 Abandoner。
type abandonStore struct {
	*fakeStore

	candidates    []uint64
	candidatesErr error
	abandonErr    error

	listCalls []abandonListCall
	abandoned []Op
	released  []Op
}

func (s *abandonStore) ListAbandonCandidates(_ context.Context, createdBeforeMs, afterOpID uint64, limit int) ([]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls = append(s.listCalls, abandonListCall{createdBeforeMs: createdBeforeMs, afterOpID: afterOpID, limit: limit})
	if s.candidatesErr != nil {
		return nil, s.candidatesErr
	}
	return append([]uint64(nil), s.candidates...), nil
}

func (s *abandonStore) Abandon(_ context.Context, op Op, _ uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.abandoned = append(s.abandoned, op)
	return s.abandonErr
}

func (s *abandonStore) ReleaseLease(_ context.Context, op Op, _ uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, op)
	return nil
}

// abandonRig 是一轮放弃的全部替身。默认装配下唯一的候选(op_id=1)满足全部六个条件。
type abandonRig struct {
	loop     *Loop
	m        *Metrics
	store    *abandonStore
	ledger   *fakeLedger
	resolver *fakeResolver
	applier  *fakeApplier
}

// 两个替身只用于自动放弃夹具；核验位是与 blob 同次返回的独立事实，不把所有 fakeLedger 默认为已核验。
type verifiedAbandonLedger struct {
	*fakeLedger
	verified bool
}

func (v *verifiedAbandonLedger) ReadVerifiedPersistedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, bool, error) {
	ledger, err := v.fakeLedger.ReadPersistedLedger(ctx, playerID)
	return ledger, v.verified && ledger != nil && err == nil, err
}

type verifiedSQLAbandonLedger struct {
	*splitLedger
	verified bool
}

func (v *verifiedSQLAbandonLedger) ReadVerifiedPersistedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, bool, error) {
	ledger, err := v.splitLedger.ReadPersistedLedger(ctx, playerID)
	return ledger, v.verified && ledger != nil && err == nil, err
}

// 只缺SQL能力的反例仍提供Redis核验能力，避免被另一道装配门禁遮住。
type verifiedBlobOnlyAbandonLedger struct {
	blobOnlyLedger
	verified bool
}

func (v *verifiedBlobOnlyAbandonLedger) ReadVerifiedPersistedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, bool, error) {
	ledger, err := v.blobOnlyLedger.ReadPersistedLedger(ctx, playerID)
	return ledger, v.verified && ledger != nil && err == nil, err
}

// abandonableOp 是一行「早该放弃」的待办:创建于截止时刻之前一小时,最后一次可能的发送在两小时前。
func abandonableOp() Op {
	op := testOp(1)
	op.Attempts = 500
	op.CreatedMs = abandonCutoffForTest - uint64(time.Hour/time.Millisecond)
	op.LastSendMs = testNowMs - uint64(2*time.Hour/time.Millisecond)
	return op
}

func newAbandonRig(t *testing.T, op Op) *abandonRig {
	t.Helper()
	store := &abandonStore{fakeStore: newFakeStore(op), candidates: []uint64{op.OpID}}
	// 放弃路径不投递:applier 被调就是 bug。
	applier := &fakeApplier{fn: func(RPC, *assetpb.AssetOpRequest) (Result, error) {
		return Result{}, errors.New("放弃路径不该投递")
	}}
	loop, m := newTestLoop(t, store, applier, nil)
	// 同纪元账本里只见过 seq 99,这一行的 seq 在窗内未见:「确定未见」。
	ledger := &fakeLedger{ledger: ledgerWithApplied(op.Stream, op.StreamEpoch, 99, false)}
	resolver := &fakeResolver{err: scenenode.ErrNotOnline}
	loop.Ledger = &verifiedAbandonLedger{fakeLedger: ledger, verified: true}
	loop.Locator = resolver
	return &abandonRig{loop: loop, m: m, store: store, ledger: ledger, resolver: resolver, applier: applier}
}

func (r *abandonRig) skipped(reason string) float64 {
	return testutil.ToFloat64(r.m.abandonSkippedTotal.WithLabelValues(reason))
}

// 六个条件全部成立:以领取时的令牌提交放弃,不还租约、不走 Finalize、不投递。
func TestAbandonOnceAbandonsWhenAllConditionsHold(t *testing.T) {
	rig := newAbandonRig(t, abandonableOp())

	n, err := rig.loop.AbandonOnce(context.Background(), testNowMs)
	if err != nil {
		t.Fatalf("AbandonOnce 出错: %v", err)
	}
	if n != 1 {
		t.Fatalf("应放弃 1 行,实际 %d", n)
	}
	if len(rig.store.listCalls) != 1 {
		t.Fatalf("应列一次候选,实际 %+v", rig.store.listCalls)
	}
	call := rig.store.listCalls[0]
	if call.createdBeforeMs != abandonCutoffForTest || call.afterOpID != 0 || call.limit != 200 {
		t.Fatalf("候选入参应为 (cutoff=%d, after=0, limit=200),实际 %+v", abandonCutoffForTest, call)
	}
	if len(rig.store.abandoned) != 1 {
		t.Fatalf("应提交一次放弃,实际 %+v", rig.store.abandoned)
	}
	if tok := rig.store.claimTokens[1]; tok == 0 || rig.store.abandoned[0].LeaseToken != tok {
		t.Fatalf("放弃必须带本次领取的令牌(条件 6):领取 %d,提交 %d", tok, rig.store.abandoned[0].LeaseToken)
	}
	if len(rig.store.released) != 0 {
		t.Fatalf("放弃成功后不该再还租约,实际 %+v", rig.store.released)
	}
	if finalized, _ := rig.store.snapshot(); len(finalized) != 0 {
		t.Fatalf("放弃不走 Finalize,实际 %+v", finalized)
	}
	if rig.applier.calls.Load() != 0 {
		t.Fatalf("放弃路径不得投递,实际 %d 次", rig.applier.calls.Load())
	}
	if got := testutil.ToFloat64(rig.m.abandonedTotal.WithLabelValues("guild_debit")); got != 1 {
		t.Fatalf("abandoned 指标应为 1,实际 %v", got)
	}
}

// 逐条反例:每一条都只破坏一个条件,其余保持成立。
func TestAbandonOnceSkipsWhenAnyConditionFails(t *testing.T) {
	cases := []struct {
		name  string
		setup func(op *Op, rig *abandonRig)
		// mutateOp 在建 rig 之前改行(Claim 回读的就是它)。
		mutateOp func(op *Op)
		reason   string
		// wantRelease:持租约的出口必须还租约;没领到租约或提交时租约已丢,则无租约可还。
		wantRelease  bool
		wantAbandon  int // Abandon 被调用的次数(只有条件 6 的反例会调到)
		wantFinalize bool
	}{
		// 条件 1:持租约复核时不再早于截止时刻(候选列表是非锁读,可能已过时)。
		{name: "条件1 复核时创建时刻不够老", mutateOp: func(op *Op) { op.CreatedMs = abandonCutoffForTest },
			reason: "not_candidate", wantRelease: true},
		{name: "条件1 Store 没回填 created_ms", mutateOp: func(op *Op) { op.CreatedMs = 0 },
			reason: "not_candidate", wantRelease: true},
		// 条件 2:没领到租约(被重投循环或唤醒领走)。
		{name: "条件2 租约被别人领走", setup: func(_ *Op, rig *abandonRig) { rig.store.lost[1] = true },
			reason: "lease_lost"},
		// 条件 3:玩家有位置,或给不出「没有位置」的证据。
		{name: "条件3 玩家在线", setup: func(_ *Op, rig *abandonRig) {
			rig.resolver.err = nil
			rig.resolver.targets = []scenenode.Target{{Endpoint: "scene-1:50051"}}
		}, reason: "located", wantRelease: true},
		{name: "条件3 等待跨 zone 落点按有位置", setup: func(_ *Op, rig *abandonRig) {
			rig.resolver.err = scenenode.ErrAwaitingPlacement
		}, reason: "located", wantRelease: true},
		{name: "条件3 读位置出错", setup: func(_ *Op, rig *abandonRig) {
			rig.resolver.err = errors.New("redis 不可用")
		}, reason: "locate_error", wantRelease: true},
		// 条件 4:距最后一次可能的发送不足安全间隔,或 Store 没维护 last_send_ms。
		{name: "条件4 半小时前刚发过", mutateOp: func(op *Op) { op.LastSendMs = testNowMs - uint64(30*time.Minute/time.Millisecond) },
			reason: "recent", wantRelease: true},
		{name: "条件4 last_send_ms 晚于 now", mutateOp: func(op *Op) { op.LastSendMs = testNowMs + 1000 },
			reason: "recent", wantRelease: true},
		{name: "条件4 Store 没维护 last_send_ms", mutateOp: func(op *Op) { op.LastSendMs = 0 },
			reason: "not_candidate", wantRelease: true},
		// 条件 5:账本已见 → 按已见结局正常终结;读失败或给不出「确定未见」→ 跳过。
		{name: "条件5 账本已见按已见终结", setup: func(op *Op, rig *abandonRig) {
			rig.ledger.ledger = sqlLedgerFixture(true, *op).ledger
		}, reason: "ledger_seen", wantFinalize: true},
		{name: "条件5 读账本失败", setup: func(_ *Op, rig *abandonRig) {
			rig.ledger.err = errors.New("data_service 超时")
		}, reason: "ledger_error", wantRelease: true},
		{name: "条件5 没有落盘 blob", setup: func(_ *Op, rig *abandonRig) { rig.ledger.ledger = nil },
			reason: "ledger_unknown", wantRelease: true},
		{name: "条件5 账本纪元比请求新", setup: func(op *Op, rig *abandonRig) {
			rig.ledger.ledger = ledgerWithApplied(op.Stream, op.StreamEpoch+1, 99, false)
		}, reason: "ledger_unknown", wantRelease: true},
		// 条件 6:提交时令牌对不上(租约在判定期间被接管)或事务失败。
		{name: "条件6 提交时租约已被接管", setup: func(_ *Op, rig *abandonRig) {
			rig.store.abandonErr = fmt.Errorf("%w: op_id=1", ErrLeaseLost)
		}, reason: "lease_lost", wantAbandon: 1},
		{name: "条件6 提交事务失败", setup: func(_ *Op, rig *abandonRig) {
			rig.store.abandonErr = errors.New("死锁")
		}, reason: "store_error", wantAbandon: 1, wantRelease: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op := abandonableOp()
			if c.mutateOp != nil {
				c.mutateOp(&op)
			}
			rig := newAbandonRig(t, op)
			if c.setup != nil {
				c.setup(&op, rig)
			}

			n, err := rig.loop.AbandonOnce(context.Background(), testNowMs)
			if err != nil {
				t.Fatalf("逐行跳过不该让整轮出错: %v", err)
			}
			if n != 0 {
				t.Fatalf("不得放弃,实际放弃 %d 行", n)
			}
			if got := testutil.ToFloat64(rig.m.abandonedTotal.WithLabelValues("guild_debit")); got != 0 {
				t.Fatalf("abandoned 指标应为 0,实际 %v", got)
			}
			if got := rig.skipped(c.reason); got != 1 {
				t.Fatalf("跳过原因 %s 应计 1,实际 %v", c.reason, got)
			}
			if len(rig.store.abandoned) != c.wantAbandon {
				t.Fatalf("Abandon 应被调用 %d 次,实际 %d", c.wantAbandon, len(rig.store.abandoned))
			}
			if released := len(rig.store.released) == 1; released != c.wantRelease || len(rig.store.released) > 1 {
				t.Fatalf("还租约期望 %v,实际 %+v", c.wantRelease, rig.store.released)
			}
			for _, r := range rig.store.released {
				if r.LeaseToken != rig.store.claimTokens[1] {
					t.Fatalf("还租约必须带本次领取的令牌:领取 %d,归还 %d", rig.store.claimTokens[1], r.LeaseToken)
				}
			}
			finalized, _ := rig.store.snapshot()
			if c.wantFinalize {
				if len(finalized) != 1 || finalized[0].Status != StatusApplied || !finalized[0].Res.Durable {
					t.Fatalf("账本已见应按已落盘结局终结为 Applied,实际 %+v", finalized)
				}
			} else if len(finalized) != 0 {
				t.Fatalf("不该终结,实际 %+v", finalized)
			}
			if rig.applier.calls.Load() != 0 {
				t.Fatalf("放弃路径不得投递,实际 %d 次", rig.applier.calls.Load())
			}
		})
	}
}

// newSQLAbandonRig 打开 RequireSQLCommit:Redis 父键 blob 用 rig.ledger,SQL 提交证据另给一份。
// 默认两份都「确定未见」(同纪元只见过 seq 99),即可以放弃。
func newSQLAbandonRig(t *testing.T, op Op) (*abandonRig, *splitLedger) {
	t.Helper()
	rig := newAbandonRig(t, op)
	rig.loop.cfg.RequireSQLCommit = true
	split := &splitLedger{blob: rig.ledger, sql: &fakeLedger{ledger: ledgerWithApplied(op.Stream, op.StreamEpoch, 99, false)}}
	rig.loop.Ledger = &verifiedSQLAbandonLedger{splitLedger: split, verified: true}
	return rig, split
}

// 开关打开时的条件 5(db-write-fencing-mysql.md §6.6):两份账本都确定未见才放弃;证据已见按证据终结;
// 证据未见而父键 blob 已见(证据落后,fence-inflight.md F6 的一物两得场景)不放弃也不终结;任一侧读失败不放弃。
func TestAbandonWithSQLCommitEvidence(t *testing.T) {
	cases := []struct {
		name         string
		setup        func(op Op, split *splitLedger)
		reason       string // 空 = 应当放弃
		wantFinalize bool
		wantBlobRead bool
	}{
		{name: "两份都未见才放弃", reason: "", wantBlobRead: true},
		{name: "证据落后于父键不得放弃", setup: func(op Op, s *splitLedger) {
			s.blob.ledger = ledgerWithApplied(op.Stream, op.StreamEpoch, op.Seq, false)
		}, reason: "sql_evidence_behind", wantBlobRead: true},
		{name: "证据已见按证据终结且不读父键", setup: func(op Op, s *splitLedger) {
			s.sql.ledger = sqlLedgerFixture(true, op).ledger
		}, reason: "ledger_seen", wantFinalize: true},
		{name: "证据读失败", setup: func(_ Op, s *splitLedger) { s.sql.err = errors.New("data_service 超时") },
			reason: "ledger_error"},
		{name: "证据缺失", setup: func(_ Op, s *splitLedger) { s.sql.ledger = nil },
			reason: "ledger_unknown"},
		{name: "证据纪元比请求新", setup: func(op Op, s *splitLedger) {
			s.sql.ledger = ledgerWithApplied(op.Stream, op.StreamEpoch+1, 99, false)
		}, reason: "ledger_unknown"},
		{name: "证据未见但父键读失败", setup: func(_ Op, s *splitLedger) { s.blob.err = errors.New("data_service 超时") },
			reason: "ledger_error", wantBlobRead: true},
		{name: "证据未见但父键缺失", setup: func(_ Op, s *splitLedger) { s.blob.ledger = nil },
			reason: "ledger_unknown", wantBlobRead: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op := abandonableOp()
			rig, split := newSQLAbandonRig(t, op)
			if c.setup != nil {
				c.setup(op, split)
			}

			n, err := rig.loop.AbandonOnce(context.Background(), testNowMs)
			if err != nil {
				t.Fatalf("逐行跳过不该让整轮出错: %v", err)
			}
			if c.reason == "" {
				if n != 1 || len(rig.store.abandoned) != 1 {
					t.Fatalf("两份账本都确定未见应放弃,实际 n=%d abandoned=%+v", n, rig.store.abandoned)
				}
				return
			}
			if n != 0 || len(rig.store.abandoned) != 0 {
				t.Fatalf("不得放弃,实际 n=%d abandoned=%+v", n, rig.store.abandoned)
			}
			if got := rig.skipped(c.reason); got != 1 {
				t.Fatalf("跳过原因 %s 应计 1,实际 %v", c.reason, got)
			}
			finalized, _ := rig.store.snapshot()
			if c.wantFinalize {
				if len(finalized) != 1 || finalized[0].Status != StatusApplied || !finalized[0].Res.Durable || !finalized[0].Res.SQLCommitted {
					t.Fatalf("证据已见应按证据终结为 Applied 并带 SQLCommitted,实际 %+v", finalized)
				}
				if len(rig.store.released) != 0 {
					t.Fatalf("终结后不该再还租约,实际 %+v", rig.store.released)
				}
			} else {
				if len(finalized) != 0 {
					t.Fatalf("不该终结,实际 %+v", finalized)
				}
				if len(rig.store.released) != 1 {
					t.Fatalf("持租约的跳过必须还租约,实际 %+v", rig.store.released)
				}
			}
			if read := split.blob.reads.Load() != 0; read != c.wantBlobRead {
				t.Fatalf("父键 blob 是否读取:期望 %t,实际读了 %d 次", c.wantBlobRead, split.blob.reads.Load())
			}
			if rig.applier.calls.Load() != 0 {
				t.Fatalf("放弃路径不得投递,实际 %d 次", rig.applier.calls.Load())
			}
		})
	}
}

// 开关打开却只挂了 blob 读取器:自动放弃整体不运行(连候选都不列),不退回只看 blob。
func TestAbandonNotRunningWithoutSQLLedgerWhenSwitchOn(t *testing.T) {
	rig := newAbandonRig(t, abandonableOp())
	rig.loop.cfg.RequireSQLCommit = true
	rig.loop.Ledger = &verifiedBlobOnlyAbandonLedger{blobOnlyLedger: blobOnlyLedger{inner: rig.ledger}, verified: true}
	n, err := rig.loop.AbandonOnce(context.Background(), testNowMs)
	if err != nil || n != 0 {
		t.Fatalf("装配不齐应回 (0, nil),实际 (%d, %v)", n, err)
	}
	if len(rig.store.listCalls) != 0 {
		t.Fatalf("装配不齐不该列候选,实际 %+v", rig.store.listCalls)
	}
}

// 没领到租约时不该去读位置与账本:那些读只在持租约之后才有意义。
func TestAbandonOnceLeaseLostReadsNothing(t *testing.T) {
	rig := newAbandonRig(t, abandonableOp())
	rig.store.lost[1] = true

	if _, err := rig.loop.AbandonOnce(context.Background(), testNowMs); err != nil {
		t.Fatalf("AbandonOnce 出错: %v", err)
	}
	if rig.resolver.callCount() != 0 || rig.ledger.reads.Load() != 0 {
		t.Fatalf("没领到租约不该读位置 / 账本,实际 resolve=%d ledger=%d", rig.resolver.callCount(), rig.ledger.reads.Load())
	}
}

// 装配不齐时整个功能不运行(连候选都不列):缺任何一条证据来源都不该放弃。
func TestAbandonOnceNotRunningWhenNotAssembled(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) (*Loop, *abandonStore)
	}{
		{"配置关闭", func(t *testing.T) (*Loop, *abandonStore) {
			store := &abandonStore{fakeStore: newFakeStore(abandonableOp()), candidates: []uint64{1}}
			loop, _ := newTestLoop(t, store, appliedApplier(), func(c *LoopConfig) { c.AbandonAfter = 0 })
			loop.Ledger = &fakeLedger{}
			loop.Locator = &fakeResolver{err: scenenode.ErrNotOnline}
			return loop, store
		}},
		{"没挂已落盘账本", func(t *testing.T) (*Loop, *abandonStore) {
			rig := newAbandonRig(t, abandonableOp())
			rig.loop.Ledger = nil
			return rig.loop, rig.store
		}},
		{"没挂玩家定位", func(t *testing.T) (*Loop, *abandonStore) {
			rig := newAbandonRig(t, abandonableOp())
			rig.loop.Locator = nil
			return rig.loop, rig.store
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			loop, store := c.build(t)
			n, err := loop.AbandonOnce(context.Background(), testNowMs)
			if err != nil || n != 0 {
				t.Fatalf("装配不齐应回 (0, nil),实际 (%d, %v)", n, err)
			}
			if len(store.listCalls) != 0 {
				t.Fatalf("装配不齐不该列候选,实际 %+v", store.listCalls)
			}
		})
	}

	// Store 没实现 Abandoner。
	loop, _ := newTestLoop(t, newFakeStore(abandonableOp()), appliedApplier(), nil)
	loop.Ledger = &verifiedAbandonLedger{fakeLedger: &fakeLedger{}, verified: true}
	loop.Locator = &fakeResolver{err: scenenode.ErrNotOnline}
	if n, err := loop.AbandonOnce(context.Background(), testNowMs); err != nil || n != 0 {
		t.Fatalf("Store 没实现 Abandoner 时应回 (0, nil),实际 (%d, %v)", n, err)
	}
}

// 时钟早于放弃门槛(截止时刻算不出来)时没有候选,不列表。
func TestAbandonOnceClockBeforeThreshold(t *testing.T) {
	rig := newAbandonRig(t, abandonableOp())
	if n, err := rig.loop.AbandonOnce(context.Background(), 1000); err != nil || n != 0 {
		t.Fatalf("应回 (0, nil),实际 (%d, %v)", n, err)
	}
	if len(rig.store.listCalls) != 0 {
		t.Fatalf("截止时刻为 0 时不该列候选,实际 %+v", rig.store.listCalls)
	}
}

// 列候选失败:整轮返回错误并计数。
func TestAbandonOnceListErrorReported(t *testing.T) {
	rig := newAbandonRig(t, abandonableOp())
	rig.store.candidatesErr = errors.New("连接断了")

	if _, err := rig.loop.AbandonOnce(context.Background(), testNowMs); err == nil {
		t.Fatal("列候选失败必须回错误")
	}
	if got := testutil.ToFloat64(rig.m.storeErrorsTotal.WithLabelValues("abandon_list")); got != 1 {
		t.Fatalf("abandon_list 出错指标应为 1,实际 %v", got)
	}
}

// 候选按 op_id 翻页:本页满了下一轮从本页最大 op_id 之后接着翻,不满则从头开始 ——
// 否则前一页若一直被跳过(玩家在线),后面的候选永远轮不到。
func TestAbandonOnceCursorPages(t *testing.T) {
	store := &abandonStore{fakeStore: newFakeStore(), candidates: []uint64{7, 3}}
	loop, _ := newTestLoop(t, store, appliedApplier(), func(c *LoopConfig) { c.AbandonBatch = 2 })
	loop.Ledger = &verifiedAbandonLedger{fakeLedger: &fakeLedger{}, verified: true}
	loop.Locator = &fakeResolver{err: scenenode.ErrNotOnline}

	// 第一页满(2 条,行都不存在 → Claim 落空,只计 lease_lost)。
	if _, err := loop.AbandonOnce(context.Background(), testNowMs); err != nil {
		t.Fatalf("第一轮出错: %v", err)
	}
	// 第二页不满。
	store.candidates = []uint64{9}
	if _, err := loop.AbandonOnce(context.Background(), testNowMs); err != nil {
		t.Fatalf("第二轮出错: %v", err)
	}
	// 第三轮应从头开始。
	if _, err := loop.AbandonOnce(context.Background(), testNowMs); err != nil {
		t.Fatalf("第三轮出错: %v", err)
	}

	if len(store.listCalls) != 3 {
		t.Fatalf("应列三次候选,实际 %+v", store.listCalls)
	}
	for i, want := range []uint64{0, 7, 0} {
		if got := store.listCalls[i].afterOpID; got != want {
			t.Fatalf("第 %d 轮应从 op_id>%d 开始,实际 %d", i+1, want, got)
		}
		if store.listCalls[i].limit != 2 {
			t.Fatalf("第 %d 轮 limit 应为 AbandonBatch=2,实际 %d", i+1, store.listCalls[i].limit)
		}
	}
}

func TestNextAbandonCursor(t *testing.T) {
	cases := []struct {
		name  string
		ids   []uint64
		batch int
		want  uint64
	}{
		{"空页从头开始", nil, 200, 0},
		{"不满一页从头开始", []uint64{5, 9}, 3, 0},
		{"满页取最大 op_id(不依赖返回顺序)", []uint64{3, 9, 5}, 3, 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nextAbandonCursor(c.ids, c.batch); got != c.want {
				t.Fatalf("nextAbandonCursor = %d,期望 %d", got, c.want)
			}
		})
	}
}

// 跳过原因会进指标 label,写飘了告警与看板就全废。
func TestAbandonSkipNames(t *testing.T) {
	want := map[abandonSkip]string{
		abandonSkipLocated:     "located",
		abandonSkipRecent:      "recent",
		abandonSkipLedgerSeen:  "ledger_seen",
		abandonSkipLedgerError: "ledger_error",
		abandonSkipLeaseLost:   "lease_lost",
		abandonSkip(0):         "invalid",
		abandonSkipCount:       "invalid",

		abandonSkipSQLEvidenceBehind: "sql_evidence_behind",
	}
	for r, name := range want {
		if got := r.String(); got != name {
			t.Fatalf("abandonSkip(%d) 名字应为 %q,实际 %q", r, name, got)
		}
	}
}

// 通过正式 AbandonOnce 验证默认核验闸；未提供能力、未核验与已见终局三种事实分开。
func TestAbandonRequiresVerifiedRedisCopy(t *testing.T) {
	for _, name := range []string{"缺少核验能力", "副本未核验", "已见终局不靠核验位"} {
		t.Run(name, func(t *testing.T) {
			op := abandonableOp()
			rig := newAbandonRig(t, op)
			if !rig.loop.cfg.RequireVerifiedRedisCopy {
				t.Fatal("默认核验闸必须开启，不能以关闭优化夹具")
			}
			switch name {
			case "缺少核验能力":
				rig.loop.Ledger = rig.ledger
			case "副本未核验":
				rig.loop.Ledger = &verifiedAbandonLedger{fakeLedger: rig.ledger, verified: false}
			case "已见终局不靠核验位":
				rig.ledger.ledger = sqlLedgerFixture(true, op).ledger
				rig.loop.Ledger = &verifiedAbandonLedger{fakeLedger: rig.ledger, verified: false}
			}
			_, err := rig.loop.AbandonOnce(context.Background(), testNowMs)
			if err != nil {
				t.Fatalf("核验边界不应使该轮失败：%v", err)
			}
			finalized, _ := rig.store.snapshot()
			if len(rig.store.abandoned) != 0 || rig.applier.calls.Load() != 0 {
				t.Fatalf("未核验不能放弃或投递：abandoned=%d apply=%d", len(rig.store.abandoned), rig.applier.calls.Load())
			}
			if name == "已见终局不靠核验位" {
				if len(finalized) != 1 {
					t.Fatalf("已见原件仍应终结，实际 %d", len(finalized))
				}
				return
			}
			if len(finalized) != 0 {
				t.Fatal("缺核验原件不得终结")
			}
			if name == "缺少核验能力" {
				if len(rig.store.listCalls) != 0 || rig.ledger.reads.Load() != 0 {
					t.Fatal("没有核验能力必须在列候选/读账本前拒绝")
				}
			} else if rig.skipped("redis_unverified") != 1 || len(rig.store.released) != 1 {
				t.Fatalf("未核验应计数并归还本次租约：skip=%v release=%d", rig.skipped("redis_unverified"), len(rig.store.released))
			}
		})
	}
}
