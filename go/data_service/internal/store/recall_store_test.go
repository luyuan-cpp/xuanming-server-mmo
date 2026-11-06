package store

import (
	"context"
	"fmt"
	"testing"

	assetpb "proto/common/asset"
	"shared/assetop"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
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

func TestRecallResolveManuallyIfQuiet_FullAppliedIsRefused(t *testing.T) {
	s := &RecallStore{}
	_, err := s.ResolveManuallyIfQuiet(context.Background(), assetop.ManualResolution{OpID: 1, Final: assetop.StatusApplied, Operator: "tester"}, assetop.QuietWitness{}, 1)
	require.ErrorIs(t, err, ErrRecallFinalInvalid)
}

// recallOpScan 按 recallOpColumns 的列序回一行假数据,给 scanRecallOp 用。
type recallOpScan []any

func (r recallOpScan) Scan(dest ...any) error {
	if len(dest) != len(r) {
		return fmt.Errorf("scan %d columns, row has %d", len(dest), len(r))
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *uint64:
			*p = r[i].(uint64)
		case *uint32:
			*p = r[i].(uint32)
		case *[]byte:
			*p = r[i].([]byte)
		default:
			return fmt.Errorf("column %d: unexpected destination %T", i, d)
		}
	}
	return nil
}

// Claim 下发与人工终结读证据经同一个 scanRecallOp 还原请求:correlation_id 是 op_id,流号、纪元、载荷原样取列值。
// 载荷解不开不算读失败,填 OpErr,由调用方决定(Claim 推迟成毒行,人工终结按证据不足)。
func TestScanRecallOp_RebuildsTheDeliveredRequest(t *testing.T) {
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(recallOpForTest().Bundle)
	require.NoError(t, err)
	row := func(raw []byte) recallOpScan {
		return recallOpScan{uint64(9), uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GM_RECALL), uint64(3), uint64(1700), RecallTxType, raw,
			uint32(4), uint64(0), uint32(27004), uint64(1000), uint64(2000), recallStatusPending, uint64(3000)}
	}

	state, err := scanRecallOp(row(payload), 77)
	require.NoError(t, err)
	require.NoError(t, state.OpErr)
	assert.True(t, state.IsPending())
	assert.Equal(t, uint64(3000), state.LeaseUntilMs)
	op := state.Op
	assert.Equal(t, uint64(77), op.OpID)
	assert.Equal(t, uint64(77), op.CorrelationID, "scene 流水与收据都按 op_id 对到这一行")
	assert.Equal(t, assetpb.AssetOpStream_ASSET_OP_STREAM_GM_RECALL, op.Stream)
	assert.Equal(t, []uint64{9, 3, 1700}, []uint64{op.PlayerID, op.Seq, op.StreamEpoch})
	assert.Equal(t, RecallTxType, op.TxType)
	assert.Equal(t, []uint64{1000, 2000}, []uint64{op.CreatedMs, op.LastSendMs})
	assert.Equal(t, uint32(27004), op.LastReason)
	assert.Zero(t, op.LeaseToken, "令牌只属于领取方")
	assert.True(t, proto.Equal(recallOpForTest().Bundle, op.Bundle))

	state, err = scanRecallOp(row([]byte{0xff, 0xff}), 77)
	require.NoError(t, err, "载荷解不开不是读失败")
	require.Error(t, state.OpErr)
	assert.Nil(t, state.Op.Bundle)
	assert.Equal(t, uint64(3), state.Op.Seq, "其余列照常扣出,人工终结还要拿它们判")
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
