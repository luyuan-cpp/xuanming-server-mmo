package store

import (
	"context"
	"testing"

	assetpb "proto/common/asset"
	"shared/assetop"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recallOpForTest() assetop.Op {
	return assetop.Op{OpID: 1, PlayerID: 9, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_GM_RECALL, Seq: 1, StreamEpoch: 2, CorrelationID: 1,
		Bundle: &assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 0, Amount: 30}}}}
}

// 这几条在碰库之前就要拒绝。store 的 db 是 nil,走到 SQL 就会 panic:这个用例同时钉住「拒绝排在任何写之前」。
func TestRecallFinalize_RefusesWithoutCommittedProof(t *testing.T) {
	s := &RecallStore{}
	_, err := s.Finalize(context.Background(), recallOpForTest(), assetop.StatusApplied, assetop.Result{
		Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true,
	}, 1)
	require.ErrorContains(t, err, "SQL commit proof")
}

func TestRecallFinalize_AppliedWithoutMatchingLineIsRefused(t *testing.T) {
	s := &RecallStore{}
	committed := assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true, SQLCommitted: true}
	for name, snapshot := range map[string]*assetpb.AssetSnapshot{
		"没有明细":  nil,
		"数量对不上": {RecallLines: []*assetpb.AssetRecallLine{{Requested: 31, Taken: 31}}},
		"总和对不上": {RecallLines: []*assetpb.AssetRecallLine{{Requested: 30, Taken: 1}}},
	} {
		t.Run(name, func(t *testing.T) {
			res := committed
			res.Snapshot = snapshot
			_, err := s.Finalize(context.Background(), recallOpForTest(), assetop.StatusApplied, res, 1)
			require.ErrorContains(t, err, "recall line")
		})
	}
}

// 回收不开自动放弃:放弃等于玩家躲够时间就把赃物带回来。走到这里是装配错了。
func TestRecallFinalize_AbandonIsNotAFinalStatus(t *testing.T) {
	s := &RecallStore{}
	_, err := s.Finalize(context.Background(), recallOpForTest(), assetop.StatusAbandoned, assetop.Result{SQLCommitted: true}, 1)
	require.ErrorContains(t, err, "not allowed")
}

func TestRecallResolveManually_FullAppliedIsRefused(t *testing.T) {
	s := &RecallStore{}
	_, err := s.ResolveManually(context.Background(), assetop.ManualResolution{OpID: 1, Final: assetop.StatusApplied, Operator: "tester"}, 1)
	require.ErrorIs(t, err, ErrRecallFinalInvalid)
}

func TestRecallManualFinal_OnlyTakesOutcomesThatClaimNothing(t *testing.T) {
	for _, status := range []uint32{recallStatusRejected, recallStatusAborted, recallStatusPartial} {
		_, ok := RecallManualFinal(status)
		assert.True(t, ok, "status=%d", status)
	}
	for _, status := range []uint32{0, recallStatusPending, recallStatusApplied, recallStatusPlanned, recallStatusCancelled, recallStatusRoleGone, 99} {
		_, ok := RecallManualFinal(status)
		assert.False(t, ok, "status=%d", status)
	}
}

// recallSeqTables 是字面量,没有经过 NewSeqTables。表名错了 AllocateSeq 才报错,这里提前钉住。
func TestRecallSeqTablesAreValid(t *testing.T) {
	_, err := assetop.NewSeqTables(recallSeqTables.SeqTable, recallSeqTables.OpTable, recallSeqTables.PendingStatus)
	require.NoError(t, err)
}

func TestRecallStatusLabel(t *testing.T) {
	assert.Equal(t, "applied", RecallStatusLabel(recallStatusApplied))
	assert.Equal(t, "role_gone", RecallStatusLabel(recallStatusRoleGone))
	assert.Equal(t, "unknown", RecallStatusLabel(99))
}

func TestRecallOpRowStates(t *testing.T) {
	for status, open := range map[uint32]bool{
		recallStatusPlanned: true, recallStatusPending: true,
		recallStatusApplied: false, recallStatusRejected: false, recallStatusAborted: false,
		recallStatusPartial: false, recallStatusCancelled: false, recallStatusRoleGone: false,
	} {
		assert.Equal(t, open, (&RecallOpRow{Status: status}).IsOpen(), "status=%d", status)
	}
	assert.True(t, (&RecallOpRow{Status: recallStatusApplied}).IsApplied())
	assert.False(t, (&RecallOpRow{Status: recallStatusPartial}).IsApplied(), "部分应用要人工核对,不算已应用")
}

func TestRecallPlaceholders(t *testing.T) {
	assert.Equal(t, "?,?,?", placeholders(3))
	assert.Equal(t, "?", placeholders(1))
}
