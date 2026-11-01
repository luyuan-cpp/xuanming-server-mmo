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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const (
	recallPending   = uint32(1)
	recallApplied   = uint32(2)
	recallRejected  = uint32(3)
	recallAborted   = uint32(4)
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
	_, err = f.store.ResolveManually(f.ctx, assetop.ManualResolution{OpID: partial.OpID, Final: assetop.StatusAppliedPartial, Operator: "tester", Reason: "可能扣过"}, 6000)
	require.NoError(t, err)
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

// 人工终结只改还在 PENDING 的行,结果留痕。
func TestRecallStore_ResolveManually(t *testing.T) {
	f := newRecallFixture(t)
	ops := f.createBatch(t, 1, 5, 901)
	f.claim(t, ops[0])
	resolved, err := f.store.ResolveManually(f.ctx, assetop.ManualResolution{OpID: ops[0].OpID, Final: assetop.StatusAborted, Operator: "tester", Reason: "流水里没有这笔"}, 6000)
	require.NoError(t, err)
	assert.True(t, resolved)
	resolved, err = f.store.ResolveManually(f.ctx, assetop.ManualResolution{OpID: ops[0].OpID, Final: assetop.StatusAborted, Operator: "tester", Reason: "重复"}, 6000)
	require.NoError(t, err)
	assert.False(t, resolved)
	assert.EqualValues(t, 1, f.db.Count(t, "gm_recall_op", "op_id = ? AND resolved_by = 'tester'", ops[0].OpID))
}
