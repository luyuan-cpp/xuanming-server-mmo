//go:build integration

package store_test

// GM 批量回收 outbox 的真 SQL 状态机。跑法同 schema_integration_test.go:go test -tags=integration ./internal/store/...

import (
	"context"
	"testing"
	"time"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	assetpb "proto/common/asset"
	rfpb "proto/role_fence"
	"shared/assetop"
	"shared/rolefence"
	"shared/workshard"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const (
	recallPending   = uint32(1)
	recallApplied   = uint32(2)
	recallRejected  = uint32(3)
	recallAborted   = uint32(4)
	recallPartial   = uint32(5)
	recallPlanned   = uint32(6)
	recallCancelled = uint32(7)
	recallRoleGone  = uint32(8)
)

type recallFenceStates map[uint64]*rfpb.RoleFenceState

func (r recallFenceStates) GetRoleFenceState(_ context.Context, player uint64) (*rfpb.RoleFenceState, error) {
	state, ok := r[player]
	if !ok {
		return nil, rolefence.ErrRoleNotFound
	}
	return state, nil
}

func activeFence(player uint64) *rfpb.RoleFenceState {
	return &rfpb.RoleFenceState{PlayerId: player, Account: "acct", OwnerRevision: 1, Version: 1, HomeZone: 1}
}

type recallFixture struct {
	db    *storetest.DB
	store *store.RecallStore
	roles recallFenceStates
	ctx   context.Context
	next  uint64
}

func newRecallFixture(t *testing.T) *recallFixture {
	t.Helper()
	db := storetest.NewMigratedDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	require.NoError(t, rolefence.EnsureSchema(ctx, db.Raw))
	roles := recallFenceStates{}
	s := store.NewRecallStoreForTest(db.Raw)
	require.NoError(t, s.Configure(store.RowIDsForTest, roles))
	return &recallFixture{db: db, store: s, roles: roles, ctx: ctx}
}

func goldBundle(t *testing.T, amount uint64) []byte {
	t.Helper()
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(&assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 0, Amount: amount}}})
	require.NoError(t, err)
	return raw
}

// createBatch 冻结一批,每名玩家一行,流水号从 sources 依次取。
func (f *recallFixture) createBatch(t *testing.T, requestID uint64, amount uint64, players ...uint64) []*store.RecallOpPlan {
	t.Helper()
	ops := make([]*store.RecallOpPlan, 0, len(players))
	for _, player := range players {
		f.next++
		f.roles[player] = activeFence(player)
		ops = append(ops, &store.RecallOpPlan{OpID: requestID*1000 + f.next, PlayerID: player, Kind: 1, Requested: amount,
			Payload: goldBundle(t, amount), SourceTxIDs: []uint64{requestID*100000 + f.next}})
	}
	require.NoError(t, f.store.CreateBatch(f.ctx, &store.RecallBatchRow{
		RequestID: requestID, RequestDigest: []byte("request"), PlanDigest: []byte("plan"), Kind: 1,
		TimeStart: 1, TimeEnd: 2, Operator: "tester", Reason: "工单", PeerAddr: "127.0.0.1:1",
		PlayerCount: uint32(len(players)), OpCount: uint32(len(ops)), SourceRows: uint32(len(ops)), AmountRequested: amount * uint64(len(ops)),
	}, ops, 1000))
	return ops
}

func (f *recallFixture) opStatus(t *testing.T, requestID uint64) map[uint64]uint32 {
	t.Helper()
	rows, err := f.store.ListOps(f.ctx, requestID)
	require.NoError(t, err)
	out := make(map[uint64]uint32, len(rows))
	for _, row := range rows {
		out[row.OpID] = row.Status
	}
	return out
}

// claim 派发并领取一行,返回可以交给 Finalize 的 Op。
func (f *recallFixture) claim(t *testing.T, plan *store.RecallOpPlan) assetop.Op {
	t.Helper()
	result, err := f.store.Dispatch(f.ctx, store.RecallPlanned{OpID: plan.OpID, PlayerID: plan.PlayerID}, 2000)
	require.NoError(t, err)
	require.Equal(t, store.RecallDispatched, result)
	op, claimed, err := f.store.Claim(f.ctx, plan.OpID, 3000, 4000, 5000, 77)
	require.NoError(t, err)
	require.True(t, claimed)
	return op
}

// witness 读一行此刻的租约与发送时刻,当作人工终结读证据时的见证(logic.ResolveRecallOp 的第一步)。
func (f *recallFixture) witness(t *testing.T, opID uint64) assetop.QuietWitness {
	t.Helper()
	state, err := f.store.ReadOpState(f.ctx, opID)
	require.NoError(t, err)
	return assetop.QuietWitness{LeaseUntilMs: state.LeaseUntilMs, LastSendMs: state.Op.LastSendMs}
}

func appliedResult(line *assetpb.AssetRecallLine) assetop.Result {
	return assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true, SQLCommitted: true,
		Snapshot: &assetpb.AssetSnapshot{RecallLines: []*assetpb.AssetRecallLine{line}}}
}

func TestRecallStore_LifecycleWritesLinesCountersAndAudit(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 100, 901, 902)

	batch, err := f.store.ReadBatch(f.ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), batch.OpenOps)
	assert.False(t, batch.Finished())
	assert.EqualValues(t, 1, f.db.Count(t, "rollback_audit_log", "reason LIKE ?", "batch_recall ACCEPTED%"))

	// PLANNED 行不能被领:还没有 seq。
	_, claimed, err := f.store.Claim(f.ctx, ops[0].OpID, 3000, 4000, 5000, 77)
	require.NoError(t, err)
	assert.False(t, claimed)

	first := f.claim(t, ops[0])
	assert.Equal(t, uint64(1), first.Seq)
	assert.Equal(t, ops[0].OpID, first.CorrelationID, "correlation_id 是 op_id,scene 流水靠它对账")
	assert.Equal(t, assetpb.AssetOpStream_ASSET_OP_STREAM_GM_RECALL, first.Stream)
	require.Len(t, first.Bundle.GetCurrencies(), 1)

	finalized, err := f.store.Finalize(f.ctx, first, assetop.StatusApplied, appliedResult(&assetpb.AssetRecallLine{Requested: 100, Taken: 60, Debt: 40}), 6000)
	require.NoError(t, err)
	assert.True(t, finalized)
	// 重复终结不做第二遍。
	finalized, err = f.store.Finalize(f.ctx, first, assetop.StatusApplied, appliedResult(&assetpb.AssetRecallLine{Requested: 100, Taken: 60, Debt: 40}), 6000)
	require.NoError(t, err)
	assert.False(t, finalized, "这是一次且仅一次的最后一道闸")

	batch, err = f.store.ReadBatch(f.ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), batch.OpenOps)
	assert.Equal(t, uint32(1), batch.OpsComplete, "差额转欠款算全额兜底")
	assert.Equal(t, uint64(60), batch.AmountTaken)
	assert.Equal(t, uint64(40), batch.AmountDebt)
	assert.False(t, batch.Finished())
	assert.Zero(t, f.db.Count(t, "rollback_audit_log", "reason LIKE ?", "batch_recall FINISHED%"))

	second := f.claim(t, ops[1])
	finalized, err = f.store.Finalize(f.ctx, second, assetop.StatusRejected, assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true, SQLCommitted: true, Reason: 27004}, 7000)
	require.NoError(t, err)
	assert.True(t, finalized)

	batch, err = f.store.ReadBatch(f.ctx, 1)
	require.NoError(t, err)
	assert.True(t, batch.Finished())
	assert.Zero(t, batch.OpenOps)
	assert.Equal(t, uint32(1), batch.OpsFailed)
	assert.Equal(t, uint64(100), batch.AmountShortfall, "被拒的一行整行算缺口")

	var affected, failed uint32
	require.NoError(t, f.db.Raw.QueryRowContext(f.ctx, "SELECT players_affected,players_failed FROM rollback_audit_log WHERE reason LIKE 'batch_recall FINISHED%'").Scan(&affected, &failed))
	assert.Equal(t, uint32(1), affected)
	assert.Equal(t, uint32(1), failed)

	rows, err := f.store.ListOps(f.ctx, 1)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, recallApplied, rows[0].Status)
	assert.Equal(t, recallRejected, rows[1].Status)
}

func TestRecallStore_ShortfallCountsSeparately(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 100, 901)
	op := f.claim(t, ops[0])
	_, err := f.store.Finalize(f.ctx, op, assetop.StatusApplied, appliedResult(&assetpb.AssetRecallLine{Requested: 100, Taken: 30, Shortfall: 70}), 6000)
	require.NoError(t, err)

	batch, err := f.store.ReadBatch(f.ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), batch.OpsShortfall)
	assert.Zero(t, batch.OpsComplete)
	assert.Equal(t, uint64(70), batch.AmountShortfall)
}

// 一条流水只能回收一次:跨批次撞主键整批回滚,什么都不留。
func TestRecallStore_SourceCanBeConsumedOnce(t *testing.T) {
	f := newRecallFixture(t)
	f.roles[901] = activeFence(901)
	plan := func(opID uint64) []*store.RecallOpPlan {
		return []*store.RecallOpPlan{{OpID: opID, PlayerID: 901, Kind: 1, Requested: 5, Payload: goldBundle(t, 5), SourceTxIDs: []uint64{4242}}}
	}
	batch := func(id uint64) *store.RecallBatchRow {
		return &store.RecallBatchRow{RequestID: id, RequestDigest: []byte("r"), PlanDigest: []byte("p"), Kind: 1, TimeStart: 1, TimeEnd: 2, Operator: "t", Reason: "r", OpCount: 1, SourceRows: 1, PlayerCount: 1}
	}
	require.NoError(t, f.store.CreateBatch(f.ctx, batch(1), plan(100), 1000))

	consumed, err := f.store.ConsumedSources(f.ctx, []uint64{4242, 4243})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]bool{4242: true}, consumed)

	err = f.store.CreateBatch(f.ctx, batch(2), plan(200), 1000)
	require.ErrorIs(t, err, store.ErrRecallSourceConsumed)
	_, err = f.store.ReadBatch(f.ctx, 2)
	require.ErrorIs(t, err, store.ErrRecallBatchNotFound, "撞主键整批回滚")
	assert.Zero(t, f.db.Count(t, "gm_recall_op", "op_id = ?", 200))

	err = f.store.CreateBatch(f.ctx, batch(1), []*store.RecallOpPlan{{OpID: 300, PlayerID: 901, Kind: 1, Requested: 5, Payload: goldBundle(t, 5), SourceTxIDs: []uint64{4243}}}, 1000)
	require.ErrorIs(t, err, store.ErrRecallBatchExists)
}

// 冻结中的角色派不动,行留在 PLANNED;解冻之后接着派。
func TestRecallStore_FrozenRoleDefersDispatch(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901)
	frozen := activeFence(901)
	frozen.FreezeToken = 7
	frozen.Version = 2
	f.roles[901] = frozen

	result, err := f.store.Dispatch(f.ctx, store.RecallPlanned{OpID: ops[0].OpID, PlayerID: 901}, 2000)
	require.NoError(t, err)
	assert.Equal(t, store.RecallDeferred, result)
	assert.Equal(t, recallPlanned, f.opStatus(t, 1)[ops[0].OpID])

	thawed := activeFence(901)
	thawed.Version = 3
	f.roles[901] = thawed
	result, err = f.store.Dispatch(f.ctx, store.RecallPlanned{OpID: ops[0].OpID, PlayerID: 901}, 2000)
	require.NoError(t, err)
	assert.Equal(t, store.RecallDispatched, result)
	assert.Equal(t, recallPending, f.opStatus(t, 1)[ops[0].OpID])
}

// 已删除或目录里没有的角色无从回收:终结成 ROLE_GONE,批次照常往前走。
func TestRecallStore_MissingRoleClosesTheRow(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901, 902)
	delete(f.roles, 901)
	deleted := activeFence(902)
	deleted.Account = ""
	deleted.Version = 2
	f.roles[902] = deleted

	for _, op := range ops {
		result, err := f.store.Dispatch(f.ctx, store.RecallPlanned{OpID: op.OpID, PlayerID: op.PlayerID}, 2000)
		require.NoError(t, err)
		assert.Equal(t, store.RecallRoleGone, result)
		assert.Equal(t, recallRoleGone, f.opStatus(t, 1)[op.OpID])
	}
	batch, err := f.store.ReadBatch(f.ctx, 1)
	require.NoError(t, err)
	assert.True(t, batch.Finished())
	assert.Equal(t, uint32(2), batch.OpsFailed)
	assert.Equal(t, uint64(10), batch.AmountShortfall)
}

// 取消:没派发的直接 CANCELLED;已派发的改截止时刻,下一次投递发中止。幂等。
func TestRecallStore_CancelStopsWhatHasNotTakenEffect(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901, 902)
	f.claim(t, ops[0])
	res, err := f.store.ListDue(f.ctx, 3500, 10)
	require.NoError(t, err)
	assert.Empty(t, res, "租约内不到期")
	require.NoError(t, f.store.Reschedule(f.ctx, assetop.Op{OpID: ops[0].OpID, LeaseToken: 77}, 3500, assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE, Local: true}, 3100))

	require.NoError(t, f.store.CancelBatch(f.ctx, 1, "tester", "误操作", 3600))
	require.NoError(t, f.store.CancelBatch(f.ctx, 1, "tester", "误操作", 3700), "重复取消幂等")

	status := f.opStatus(t, 1)
	assert.Equal(t, recallPending, status[ops[0].OpID])
	assert.Equal(t, recallCancelled, status[ops[1].OpID])
	batch, err := f.store.ReadBatch(f.ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, uint64(3600), batch.CancelledMs)
	assert.Equal(t, uint32(1), batch.OpenOps)
	assert.Equal(t, uint32(1), batch.OpsFailed)
	assert.EqualValues(t, 1, f.db.Count(t, "rollback_audit_log", "reason LIKE ?", "batch_recall CANCELLED%"), "重复取消不重复写审计")

	op, claimed, err := f.store.Claim(f.ctx, ops[0].OpID, 4000, 5000, 6000, 78)
	require.NoError(t, err)
	require.True(t, claimed)
	assert.Equal(t, uint64(3600), op.DeadlineMs, "Loop 按它改发中止")

	finalized, err := f.store.Finalize(f.ctx, op, assetop.StatusAborted, assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true, SQLCommitted: true}, 7000)
	require.NoError(t, err)
	assert.True(t, finalized)
	assert.Equal(t, recallAborted, f.opStatus(t, 1)[ops[0].OpID])
	batch, err = f.store.ReadBatch(f.ctx, 1)
	require.NoError(t, err)
	assert.True(t, batch.Finished())

	// 取消之后迟到的派发:行已经不是 PLANNED,seq 不消耗。
	result, err := f.store.Dispatch(f.ctx, store.RecallPlanned{OpID: ops[1].OpID, PlayerID: 902}, 8000)
	require.NoError(t, err)
	assert.Equal(t, store.RecallLost, result)
	assert.Zero(t, f.db.Count(t, "gm_recall_op", "player_id = ? AND seq = 1 AND stream_epoch <> 0", 902))
}

// 派发还没提交时取消:那一行不算没派发。先数后改的话 open_ops 多减,批次提前结束,那一行以后再也终结不了。
func TestRecallStore_CancelRacingDispatchKeepsCountersRight(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901, 902)
	tx, err := f.db.Raw.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(f.ctx, "UPDATE gm_recall_op SET status=?,seq=1,stream_epoch=9 WHERE op_id=?", recallPending, ops[0].OpID)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- f.store.CancelBatch(f.ctx, 1, "tester", "误操作", 3600) }()
	// 等取消撞上行锁。没撞上也不算错,只是这一轮没覆盖到竞争。
	time.Sleep(300 * time.Millisecond)
	require.NoError(t, tx.Commit())
	require.NoError(t, <-done)

	batch, err := f.store.ReadBatch(f.ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), batch.OpenOps, "派发成功的那一行还没有结局")
	assert.Equal(t, uint32(1), batch.OpsFailed)
	assert.Equal(t, uint64(5), batch.AmountShortfall)
	assert.False(t, batch.Finished())
	status := f.opStatus(t, 1)
	assert.Equal(t, recallPending, status[ops[0].OpID])
	assert.Equal(t, recallCancelled, status[ops[1].OpID])
	assert.EqualValues(t, 1, f.db.Count(t, "gm_recall_op", "op_id = ? AND deadline_ms = 3600", ops[0].OpID), "派发成功的行要改发中止")
}

// 确定没回收到东西的行,流水放回去,下一批还能回收。回收过的和可能回收过的不放。
func TestRecallStore_SourcesAreReleasedOnlyWhenNothingWasRecalled(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901, 902, 903, 904, 905)
	sourceOf := func(op *store.RecallOpPlan) uint64 { return op.SourceTxIDs[0] }
	consumed := func(op *store.RecallOpPlan) bool {
		got, err := f.store.ConsumedSources(f.ctx, op.SourceTxIDs)
		require.NoError(t, err)
		return got[sourceOf(op)]
	}

	applied := f.claim(t, ops[0])
	_, err := f.store.Finalize(f.ctx, applied, assetop.StatusApplied, appliedResult(&assetpb.AssetRecallLine{Requested: 5, Taken: 2, Debt: 3}), 6000)
	require.NoError(t, err)
	assert.True(t, consumed(ops[0]), "已应用的不能再回收")

	rejected := f.claim(t, ops[1])
	_, err = f.store.Finalize(f.ctx, rejected, assetop.StatusRejected, assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true, SQLCommitted: true}, 6000)
	require.NoError(t, err)
	assert.False(t, consumed(ops[1]), "scene 判拒,资产没动")

	partial := f.claim(t, ops[2])
	resolved, err := f.store.ResolveManuallyIfQuiet(f.ctx, assetop.ManualResolution{OpID: partial.OpID, Final: assetop.StatusAppliedPartial, Operator: "tester", Reason: "账本判不了"},
		f.witness(t, partial.OpID), 6000)
	require.NoError(t, err)
	require.True(t, resolved)
	assert.True(t, consumed(ops[2]), "可能已经扣过,再回收就是重复")

	delete(f.roles, 904)
	result, err := f.store.Dispatch(f.ctx, store.RecallPlanned{OpID: ops[3].OpID, PlayerID: 904}, 6000)
	require.NoError(t, err)
	require.Equal(t, store.RecallRoleGone, result)
	assert.False(t, consumed(ops[3]))

	require.NoError(t, f.store.CancelBatch(f.ctx, 1, "tester", "误操作", 7000))
	assert.Equal(t, recallCancelled, f.opStatus(t, 1)[ops[4].OpID])
	assert.False(t, consumed(ops[4]), "取消的没回收到,改对条件之后还要能回收")

	batch, err := f.store.ReadBatch(f.ctx, 1)
	require.NoError(t, err)
	assert.True(t, batch.Finished())
}

func TestRecallStore_RescheduleWithWrongTokenIsLeaseLost(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901)
	f.claim(t, ops[0])
	err := f.store.Reschedule(f.ctx, assetop.Op{OpID: ops[0].OpID, LeaseToken: 999}, 3500, assetop.Result{}, 3100)
	require.ErrorIs(t, err, assetop.ErrLeaseLost)
}

// 护栏:PLANNED、PENDING 都挡过户。终局之后放开。
func TestRecallStore_PendingForRoleBlocksUntilSettled(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901)
	blocker := func() string {
		tx, err := f.db.Raw.BeginTx(f.ctx, nil)
		require.NoError(t, err)
		defer tx.Rollback()
		value, err := f.store.PendingForRole(f.ctx, tx, &rfpb.RoleFenceState{PlayerId: 901})
		require.NoError(t, err)
		return value
	}
	assert.NotEmpty(t, blocker(), "PLANNED 要挡")
	op := f.claim(t, ops[0])
	assert.NotEmpty(t, blocker(), "PENDING 要挡")
	_, err := f.store.Finalize(f.ctx, op, assetop.StatusApplied, appliedResult(&assetpb.AssetRecallLine{Requested: 5, Taken: 5}), 6000)
	require.NoError(t, err)
	assert.Empty(t, blocker())
}

// 人工终结只改还在 PENDING 的行,一次且仅一次,结果留痕。
func TestRecallStore_ResolveManuallyIfQuiet(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901)
	f.claim(t, ops[0])
	witness := f.witness(t, ops[0].OpID)
	resolved, err := f.store.ResolveManuallyIfQuiet(f.ctx, assetop.ManualResolution{OpID: ops[0].OpID, Final: assetop.StatusAborted, Operator: "tester", Reason: "账本已拒绝"}, witness, 6000)
	require.NoError(t, err)
	assert.True(t, resolved)
	resolved, err = f.store.ResolveManuallyIfQuiet(f.ctx, assetop.ManualResolution{OpID: ops[0].OpID, Final: assetop.StatusAborted, Operator: "tester", Reason: "重复"}, witness, 6000)
	require.NoError(t, err)
	assert.False(t, resolved)
	assert.EqualValues(t, 1, f.db.Count(t, "gm_recall_op", "op_id = ? AND resolved_by = 'tester'", ops[0].OpID))
}

// 人工终结和 ROLE_GONE 不是 scene 的答复:durable、last_outcome、last_reason 留着最后一次投递时 scene 实际回的值(事后核对用);
// 只有自动终结把终结所凭的落盘答复写进去、置 durable=1。
func TestRecallStore_ManualResolutionKeepsTheLastSceneAnswer(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901, 902, 903)
	const lastReason = uint32(27001)
	unknown := uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN)

	f.claim(t, ops[0])
	require.NoError(t, f.store.Reschedule(f.ctx, assetop.Op{OpID: ops[0].OpID, LeaseToken: 77}, 3500,
		assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN, Reason: lastReason}, 3100))
	resolved, err := f.store.ResolveManuallyIfQuiet(f.ctx, assetop.ManualResolution{OpID: ops[0].OpID, Final: assetop.StatusAppliedPartial,
		Operator: "tester", Reason: "账本判不了"}, f.witness(t, ops[0].OpID), 6000)
	require.NoError(t, err)
	require.True(t, resolved)
	assert.EqualValues(t, 1, f.db.Count(t, "gm_recall_op", "op_id = ? AND status = ? AND durable = 0 AND last_outcome = ? AND last_reason = ? AND resolved_by = 'tester'",
		ops[0].OpID, recallPartial, unknown, lastReason), "人工终结保留最后一次 scene 答复,不置 durable")

	delete(f.roles, 902)
	result, err := f.store.Dispatch(f.ctx, store.RecallPlanned{OpID: ops[1].OpID, PlayerID: 902}, 2000)
	require.NoError(t, err)
	require.Equal(t, store.RecallRoleGone, result)
	assert.EqualValues(t, 1, f.db.Count(t, "gm_recall_op", "op_id = ? AND status = ? AND durable = 0 AND last_outcome = 0 AND last_reason = 0",
		ops[1].OpID, recallRoleGone), "ROLE_GONE 从没投出去过,也不是 scene 的答复")

	applied := f.claim(t, ops[2])
	finalized, err := f.store.Finalize(f.ctx, applied, assetop.StatusApplied, appliedResult(&assetpb.AssetRecallLine{Requested: 5, Taken: 5}), 7000)
	require.NoError(t, err)
	require.True(t, finalized)
	assert.EqualValues(t, 1, f.db.Count(t, "gm_recall_op", "op_id = ? AND status = ? AND durable = 1 AND last_outcome = ? AND resolved_by = ''",
		ops[2].OpID, recallApplied, uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED)), "自动终结写终结所凭的落盘答复")
}

// 读证据之后重投循环又领了这一行:见证对不上,终结落空,批次计数与来源都不动;按新读的值再来才终结。
func TestRecallStore_ResolveManuallyIfQuiet_MissesAfterAClaim(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901)
	f.claim(t, ops[0])
	// 玩家不在线,没发出请求:租约清回 0,last_send_ms 不动。
	require.NoError(t, f.store.Reschedule(f.ctx, assetop.Op{OpID: ops[0].OpID, LeaseToken: 77}, 3500,
		assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE, Local: true}, 3100))
	witness := f.witness(t, ops[0].OpID)

	_, claimed, err := f.store.Claim(f.ctx, ops[0].OpID, 4000, 5000, 6000, 78)
	require.NoError(t, err)
	require.True(t, claimed)

	manual := assetop.ManualResolution{OpID: ops[0].OpID, Final: assetop.StatusAborted, Operator: "tester", Reason: "账本已拒绝"}
	resolved, err := f.store.ResolveManuallyIfQuiet(f.ctx, manual, witness, 6000)
	require.NoError(t, err)
	assert.False(t, resolved, "读证据之后有人领过,证据作废")
	assert.Equal(t, recallPending, f.opStatus(t, 1)[ops[0].OpID])
	assert.Zero(t, f.db.Count(t, "gm_recall_op", "op_id = ? AND resolved_by = 'tester'", ops[0].OpID))
	batch, err := f.store.ReadBatch(f.ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), batch.OpenOps, "落空不动批次计数")
	consumed, err := f.store.ConsumedSources(f.ctx, ops[0].SourceTxIDs)
	require.NoError(t, err)
	assert.True(t, consumed[ops[0].SourceTxIDs[0]], "落空不放回来源")

	resolved, err = f.store.ResolveManuallyIfQuiet(f.ctx, manual, f.witness(t, ops[0].OpID), 7000)
	require.NoError(t, err)
	assert.True(t, resolved, "见证与库里一致才终结")
	assert.Equal(t, recallAborted, f.opStatus(t, 1)[ops[0].OpID])
	consumed, err = f.store.ConsumedSources(f.ctx, ops[0].SourceTxIDs)
	require.NoError(t, err)
	assert.False(t, consumed[ops[0].SourceTxIDs[0]], "确定没回收到东西,来源放回去")
	batch, err = f.store.ReadBatch(f.ctx, 1)
	require.NoError(t, err)
	assert.True(t, batch.Finished())
}

// 领取、发出请求、正常重排之后租约又回到 0:只比租约看不出来,last_send_ms 被推进了,照样落空。
func TestRecallStore_ResolveManuallyIfQuiet_MissesAfterASend(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901)
	f.claim(t, ops[0])
	require.NoError(t, f.store.Reschedule(f.ctx, assetop.Op{OpID: ops[0].OpID, LeaseToken: 77}, 3500,
		assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE, Local: true}, 3100))
	witness := f.witness(t, ops[0].OpID)

	_, claimed, err := f.store.Claim(f.ctx, ops[0].OpID, 4000, 5000, 6000, 78)
	require.NoError(t, err)
	require.True(t, claimed)
	// 这一次真的发出去了,scene 回 UNKNOWN:重排把租约清回 0,把 last_send_ms 推到这一轮的开始时刻。
	require.NoError(t, f.store.Reschedule(f.ctx, assetop.Op{OpID: ops[0].OpID, LeaseToken: 78}, 9000,
		assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN}, 4100))
	after := f.witness(t, ops[0].OpID)
	require.Equal(t, witness.LeaseUntilMs, after.LeaseUntilMs, "租约又回到读证据时的值")
	require.NotEqual(t, witness.LastSendMs, after.LastSendMs)

	resolved, err := f.store.ResolveManuallyIfQuiet(f.ctx, assetop.ManualResolution{OpID: ops[0].OpID, Final: assetop.StatusAppliedPartial,
		Operator: "tester", Reason: "账本判不了"}, witness, 6000)
	require.NoError(t, err)
	assert.False(t, resolved, "读证据之后发过请求,证据作废")
	assert.Equal(t, recallPending, f.opStatus(t, 1)[ops[0].OpID])
}

// 人工终结读证据看到的请求,就是 Claim 下发的那一份(同一个 scanRecallOp),收据身份才对得上。没有这一行回 ErrRecallOpNotFound。
func TestRecallStore_ReadOpStateIsTheClaimedRequest(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901)
	planned, err := f.store.ReadOpState(f.ctx, ops[0].OpID)
	require.NoError(t, err)
	assert.False(t, planned.IsPending(), "PLANNED 还没有 seq,不能人工终结")

	claimed := f.claim(t, ops[0])
	state, err := f.store.ReadOpState(f.ctx, ops[0].OpID)
	require.NoError(t, err)
	require.NoError(t, state.OpErr)
	assert.True(t, state.IsPending())
	assert.Equal(t, uint64(4000), state.LeaseUntilMs, "租约是 claim 写下的")
	assert.Equal(t, uint64(77), state.LeaseToken, "行上的令牌是 claim 写下的;Op.LeaseToken 只属于领取方")
	assert.True(t, proto.Equal(claimed.Bundle, state.Op.Bundle))
	claimed.Bundle, claimed.LeaseToken, state.Op.Bundle = nil, 0, nil
	assert.Equal(t, claimed, state.Op)

	_, err = f.store.ReadOpState(f.ctx, 999_999)
	require.ErrorIs(t, err, store.ErrRecallOpNotFound)
}

// 派发(PLANNED → PENDING)写 last_send_ms 初值(go/shared/assetop/abandon.go 文件头 last_send_ms 契约第 1 条)。
// 原来这一列从派发到第一次真实投递都是 0,从没投出去过的行(玩家一直不在线)永远判不了安静:人工终结只能关成 PARTIAL(少回收),
// 自动放弃也永远跳过它。现在派发时刻就是初值,过了安全间隔、没人持租约就判得了安静。
func TestRecallStore_DispatchInitializesLastSendMs(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901)
	result, err := f.store.Dispatch(f.ctx, store.RecallPlanned{OpID: ops[0].OpID, PlayerID: ops[0].PlayerID}, 2000)
	require.NoError(t, err)
	require.Equal(t, store.RecallDispatched, result)

	state, err := f.store.ReadOpState(f.ctx, ops[0].OpID)
	require.NoError(t, err)
	require.True(t, state.IsPending())
	assert.Equal(t, uint64(2000), state.Op.LastSendMs, "派发时刻就是初值,不再是 0")

	gapMs := uint64(assetop.ManualQuietFloor / time.Millisecond)
	quiet, why := assetop.RowQuiet(state.Op.LastSendMs, state.LeaseUntilMs, 2000+gapMs+1, assetop.ManualQuietFloor)
	assert.True(t, quiet, "从没投出去、过了安全间隔、没人持租约:应判安静(%s)", why)
	quiet, _ = assetop.RowQuiet(state.Op.LastSendMs, state.LeaseUntilMs, 2000+gapMs-1, assetop.ManualQuietFloor)
	assert.False(t, quiet, "离派发不足安全间隔:仍不安静")
}

// needs_review 指令交人工处置的登记:全部流水在一个事务里按已消费写进 gm_recall_source(op_id = 0,request_id = 审计行号),
// 同一事务写一行审计(操作人、理由、指令)。再登记一次幂等,不重复写。
func TestRecallStore_RegisterReviewHandledConsumesSourcesOnce(t *testing.T) {
	f := newRecallFixture(t)
	handled := store.RecallReviewHandled{PlayerID: 901, CorrelationID: 601, TxIDs: []uint64{700001, 700002},
		Operator: "tester", Reason: "工单 123 已挂欠款", Detail: "kind=1 item=0 currency=0"}
	registered, err := f.store.RegisterReviewHandled(f.ctx, handled, 5000)
	require.NoError(t, err)
	assert.True(t, registered)

	consumed, err := f.store.ConsumedSources(f.ctx, handled.TxIDs)
	require.NoError(t, err)
	assert.Equal(t, map[uint64]bool{700001: true, 700002: true}, consumed, "登记之后按已消费算,以后的批次和预览都剔除这条指令")
	state, err := f.store.SourceState(f.ctx, handled.TxIDs)
	require.NoError(t, err)
	assert.Equal(t, store.RecallSourceState{Manual: true}, state)
	audit := "operator = 'tester' AND reason LIKE '%REVIEW_HANDLED player=901 correlation=601 sources=2%'"
	assert.EqualValues(t, 1, f.db.Count(t, "rollback_audit_log", audit))
	assert.EqualValues(t, 2, f.db.Count(t, "gm_recall_source",
		"op_id = 0 AND player_id = 901 AND request_id = (SELECT id FROM rollback_audit_log WHERE "+audit+")"),
		"request_id 是审计行号,按它找得到操作人和理由")

	registered, err = f.store.RegisterReviewHandled(f.ctx, handled, 6000)
	require.NoError(t, err)
	assert.False(t, registered, "已登记过:幂等")
	assert.EqualValues(t, 1, f.db.Count(t, "rollback_audit_log", "reason LIKE '%REVIEW_HANDLED%'"), "幂等不重复写审计")
}

// 已被回收批次消费的流水不能再登记:整笔回滚,一条都不写,也不写审计。
func TestRecallStore_RegisterReviewHandledRefusesSourcesOfABatch(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901)
	handled := store.RecallReviewHandled{PlayerID: 901, CorrelationID: 601, TxIDs: []uint64{ops[0].SourceTxIDs[0], 700003},
		Operator: "tester", Reason: "工单"}
	registered, err := f.store.RegisterReviewHandled(f.ctx, handled, 5000)
	require.ErrorIs(t, err, store.ErrRecallSourceConsumed)
	assert.False(t, registered)
	assert.Zero(t, f.db.Count(t, "gm_recall_source", "tx_id = ?", 700003), "整笔回滚")
	assert.Zero(t, f.db.Count(t, "rollback_audit_log", "reason LIKE '%REVIEW_HANDLED%'"))
	state, err := f.store.SourceState(f.ctx, handled.TxIDs)
	require.NoError(t, err)
	assert.Equal(t, store.RecallSourceState{Batch: true}, state)
}

// 人工登记的流水没有放回路径:批次的行被拒绝(放回它自己的流水)、批次被取消(放回没派发的行的流水),都碰不到它。
func TestRecallStore_ReviewHandledSourcesAreNeverReleased(t *testing.T) {
	f := newRecallFixture(t)
	registered, err := f.store.RegisterReviewHandled(f.ctx, store.RecallReviewHandled{PlayerID: 901, CorrelationID: 601,
		TxIDs: []uint64{700011}, Operator: "tester", Reason: "工单"}, 1000)
	require.NoError(t, err)
	require.True(t, registered)

	ops := f.createBatch(t, 1, 5, 901, 902)
	rejected := f.claim(t, ops[0])
	_, err = f.store.Finalize(f.ctx, rejected, assetop.StatusRejected,
		assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true, SQLCommitted: true}, 6000)
	require.NoError(t, err)
	require.NoError(t, f.store.CancelBatch(f.ctx, 1, "tester", "误操作", 7000))

	consumed, err := f.store.ConsumedSources(f.ctx, []uint64{700011, ops[0].SourceTxIDs[0], ops[1].SourceTxIDs[0]})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]bool{700011: true}, consumed, "批次的流水放回去了,人工登记的还在")
}

// 领取的 CAS 之后、回读之前这一行被人工终结了(settle 不换令牌):回读看到的不再是自己的未决行,不下发。
// 租约被别的副本接管(令牌不是自己的)同样不下发。两条语句之间的窗口用 ClaimedOpForTest 确定地复现。
func TestRecallStore_ClaimDeliversOnlyItsOwnPendingRow(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901, 902)
	f.claim(t, ops[0])
	resolved, err := f.store.ResolveManuallyIfQuiet(f.ctx, assetop.ManualResolution{OpID: ops[0].OpID, Final: assetop.StatusAppliedPartial,
		Operator: "tester", Reason: "账本判不了"}, f.witness(t, ops[0].OpID), 5000)
	require.NoError(t, err)
	require.True(t, resolved)
	op, delivered, err := f.store.ClaimedOpForTest(f.ctx, ops[0].OpID, 9000, 77)
	require.NoError(t, err)
	assert.False(t, delivered, "已终结的行不再下发")
	assert.Zero(t, op.OpID)

	f.claim(t, ops[1])
	_, delivered, err = f.store.ClaimedOpForTest(f.ctx, ops[1].OpID, 9000, 78)
	require.NoError(t, err)
	assert.False(t, delivered, "令牌不是自己的:租约已被别的副本接管")
	op, delivered, err = f.store.ClaimedOpForTest(f.ctx, ops[1].OpID, 9000, 77)
	require.NoError(t, err)
	assert.True(t, delivered)
	assert.Equal(t, uint64(77), op.LeaseToken)
	assert.Equal(t, ops[1].OpID, op.CorrelationID)
}

// 回收重投的到期扫描按玩家分片(缺口清单第 25 条第二批,片号 = player_id % 64):分片条件在两段里都生效、都在 LIMIT 之前,
// 别的片的行再早到期也不占本副本的名额;零值条件与 ListDue 相同;一片都不归本副本时什么都不回。
func TestRecallStore_ListDueOwnedReadsOnlyOwnedShards(t *testing.T) {
	f := newRecallFixture(t)
	const kind = "gm-recall-assetop"
	const other, mine uint64 = 64*150 + 7, 64*150 + 8 // 片 7 与片 8
	ops := f.createBatch(t, 1, 5, other, mine)
	for i, at := range []uint64{2000, 2100} { // 别的片先派发、先到期,占住队头
		result, err := f.store.Dispatch(f.ctx, store.RecallPlanned{OpID: ops[i].OpID, PlayerID: ops[i].PlayerID}, at)
		require.NoError(t, err)
		require.Equal(t, store.RecallDispatched, result)
	}
	// 本片那一行是老行(attempts >= FreshAttemptLimit),走第二段。
	_, err := f.db.Raw.ExecContext(f.ctx, "UPDATE gm_recall_op SET attempts=? WHERE op_id=?", assetop.FreshAttemptLimit, ops[1].OpID)
	require.NoError(t, err)

	owned := workshard.SQLFilter(func(key string) bool { return key == workshard.ShardKey(kind, workshard.ShardOf(mine)) }, kind, "player_id")
	ids, err := f.store.ListDueOwned(f.ctx, 3500, 1, owned)
	require.NoError(t, err)
	assert.Equal(t, []uint64{ops[1].OpID}, ids, "第一段的别的片新行不占名额,第二段同样只取本片")

	all, err := f.store.ListDue(f.ctx, 3500, 10)
	require.NoError(t, err)
	assert.Equal(t, []uint64{ops[0].OpID, ops[1].OpID}, all, "不分片:新行在前、老行补缺口")
	unsharded, err := f.store.ListDueOwned(f.ctx, 3500, 10, workshard.Filter{})
	require.NoError(t, err)
	assert.Equal(t, all, unsharded, "ListDue 就是不带条件的 ListDueOwned")

	none := workshard.SQLFilter(func(string) bool { return false }, kind, "player_id")
	ids, err = f.store.ListDueOwned(f.ctx, 3500, 10, none)
	require.NoError(t, err)
	assert.Empty(t, ids, "一片都不归本副本:什么都不回")
}

// 载荷解不开的行推迟成毒行:只清租约,last_send_ms 不动。推迟带 status = PENDING:回读之后、推迟之前这一行若被人工终结
// (令牌没换),推迟落空,终态行的 next_attempt_ms 不被改写。
func TestRecallStore_PoisonDeferOnlyTouchesThePendingLease(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901)
	_, err := f.db.Raw.ExecContext(f.ctx, "UPDATE gm_recall_op SET payload=? WHERE op_id=?", []byte{0xff, 0xff}, ops[0].OpID)
	require.NoError(t, err)
	result, err := f.store.Dispatch(f.ctx, store.RecallPlanned{OpID: ops[0].OpID, PlayerID: 901}, 2000)
	require.NoError(t, err)
	require.Equal(t, store.RecallDispatched, result)

	_, claimed, err := f.store.Claim(f.ctx, ops[0].OpID, 3000, 4000, 9000, 77)
	require.ErrorIs(t, err, assetop.ErrPoisonRow)
	assert.False(t, claimed)
	assert.EqualValues(t, 1, f.db.Count(t, "gm_recall_op",
		"op_id = ? AND status = ? AND next_attempt_ms = 9000 AND lease_until_ms = 0 AND last_send_ms = 2000", ops[0].OpID, recallPending))

	resolved, err := f.store.ResolveManuallyIfQuiet(f.ctx, assetop.ManualResolution{OpID: ops[0].OpID, Final: assetop.StatusAppliedPartial,
		Operator: "tester", Reason: "载荷坏,账本判不了"}, f.witness(t, ops[0].OpID), 10000)
	require.NoError(t, err)
	require.True(t, resolved)
	require.NoError(t, f.store.DeferPoisonForTest(f.ctx, ops[0].OpID, 77, 12000))
	assert.EqualValues(t, 1, f.db.Count(t, "gm_recall_op", "op_id = ? AND status = ? AND next_attempt_ms = 9000", ops[0].OpID, recallPartial),
		"终态行不被推迟改写")
}

// GM 回收派发按玩家分片(缺口清单第 25 条第四批,设计 §14.2):别的片的 PLANNED 行更早到期也不占本副本的名额;零值条件就是
// 原来的不分片扫描;一片都不归本副本时什么都不回;没到期的、已派发成 PENDING 的行都不回。
func TestRecallStore_ListPlannedOwnedReadsOnlyOwnedShards(t *testing.T) {
	f := newRecallFixture(t)
	const kind = "gm-recall-dispatch"
	const other, mine, later uint64 = 64*160 + 3, 64*160 + 4, 64*161 + 4 // 片 3、片 4、片 4
	ops := f.createBatch(t, 1, 5, other, mine, later)
	// 三行建批时都在 1000 到期:别的片那一行提前到 500 占住队头,later 推迟到 5000(读的时刻 3000 还没到期)。
	_, err := f.db.Raw.ExecContext(f.ctx, "UPDATE gm_recall_op SET next_attempt_ms=500 WHERE op_id=?", ops[0].OpID)
	require.NoError(t, err)
	require.NoError(t, f.store.DeferPlanned(f.ctx, ops[2].OpID, 5000, 1000))

	owned := workshard.SQLFilter(func(key string) bool { return key == workshard.ShardKey(kind, workshard.ShardOf(mine)) }, kind, "player_id")
	planned, err := f.store.ListPlannedOwned(f.ctx, 3000, 1, owned)
	require.NoError(t, err)
	assert.Equal(t, []store.RecallPlanned{{OpID: ops[1].OpID, PlayerID: mine}}, planned, "别的片更早到期的行不占名额,本片没到期的行不回")

	all, err := f.store.ListPlannedOwned(f.ctx, 3000, 10, workshard.Filter{})
	require.NoError(t, err)
	assert.Equal(t, []store.RecallPlanned{{OpID: ops[0].OpID, PlayerID: other}, {OpID: ops[1].OpID, PlayerID: mine}}, all,
		"零值条件:全部片按到期先后,没到期的不回")

	none := workshard.SQLFilter(func(string) bool { return false }, kind, "player_id")
	planned, err = f.store.ListPlannedOwned(f.ctx, 3000, 10, none)
	require.NoError(t, err)
	assert.Empty(t, planned, "一片都不归本副本:什么都不回")

	result, err := f.store.Dispatch(f.ctx, store.RecallPlanned{OpID: ops[1].OpID, PlayerID: mine}, 3000)
	require.NoError(t, err)
	require.Equal(t, store.RecallDispatched, result)
	planned, err = f.store.ListPlannedOwned(f.ctx, 3000, 10, owned)
	require.NoError(t, err)
	assert.Empty(t, planned, "派发成 PENDING 之后不再出现在派发扫描里;本片剩下那一行还没到期")
}
