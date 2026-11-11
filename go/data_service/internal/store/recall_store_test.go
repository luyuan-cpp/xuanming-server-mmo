package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	assetpb "proto/common/asset"
	"shared/assetop"
	"shared/workshard"

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
			uint32(4), uint64(0), uint32(27004), uint64(1000), uint64(2000), recallStatusPending, uint64(3000), uint64(4242)}
	}

	state, err := scanRecallOp(row(payload), 77)
	require.NoError(t, err)
	require.NoError(t, state.OpErr)
	assert.True(t, state.IsPending())
	assert.Equal(t, uint64(3000), state.LeaseUntilMs)
	assert.Equal(t, uint64(4242), state.LeaseToken, "行上的令牌,Claim 回读拿它核对")
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

// Claim 回读之后只下发「我刚领到的未决行」:还是 PENDING,令牌也还是自己的。终结不清令牌,所以只看令牌不够。
func TestRecallOpState_HeldByOnlyThePendingLeaseHolder(t *testing.T) {
	cases := []struct {
		name   string
		status uint32
		token  uint64
		held   bool
	}{
		{"自己领到的未决行", recallStatusPending, 77, true},
		{"租约被别的副本接管", recallStatusPending, 78, false},
		{"领取之后被人工终结,令牌没换", recallStatusPartial, 77, false},
		{"领取之后被重投循环终结", recallStatusApplied, 77, false},
		{"没人领过", recallStatusPending, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.held, RecallOpState{Status: c.status, LeaseToken: c.token}.heldBy(77))
		})
	}
}

// 交人工处置的登记在碰库之前就拒绝残缺的入参:db 是 nil,走到 SQL 就会 panic。
func TestRecallRegisterReviewHandled_InvalidInputTouchesNothing(t *testing.T) {
	s := &RecallStore{}
	valid := RecallReviewHandled{PlayerID: 9, CorrelationID: 601, TxIDs: []uint64{11}, Operator: "tester", Reason: "工单"}
	cases := map[string]func(*RecallReviewHandled){
		"没有玩家":   func(h *RecallReviewHandled) { h.PlayerID = 0 },
		"没有指令号":  func(h *RecallReviewHandled) { h.CorrelationID = 0 },
		"没有流水":   func(h *RecallReviewHandled) { h.TxIDs = nil },
		"流水超过上限": func(h *RecallReviewHandled) { h.TxIDs = make([]uint64, maxRecallReviewHandledSources+1) },
		"没有操作人":  func(h *RecallReviewHandled) { h.Operator = "" },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			h := valid
			edit(&h)
			registered, err := s.RegisterReviewHandled(context.Background(), h, 1)
			require.ErrorContains(t, err, "invalid registration")
			assert.False(t, registered)
		})
	}
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

// 回收重投的到期扫描按玩家分片(缺口清单第 25 条第二批):两段都带同一个分片条件,位置在 ORDER BY / LIMIT 之前;
// 占位符个数与 ListDueOwned 传的实参一致(PENDING、now、now、FreshAttemptLimit、片号……、LIMIT);零值条件是恒真 1=1。纯函数,不连库。
func TestRecallListDueQueryPutsShardFilterBeforeLimit(t *testing.T) {
	const kind = "gm-recall-assetop"
	owned := workshard.SQLFilter(func(key string) bool { return key == workshard.ShardKey(kind, 9) }, kind, "player_id")
	for _, comparison := range []string{"<", ">="} {
		query := recallListDueQuery(comparison, owned)
		assert.True(t, strings.HasSuffix(query, " AND attempts "+comparison+" ? AND MOD(player_id,64) IN(?) ORDER BY next_attempt_ms,op_id LIMIT ?"),
			"%s 段的分片条件必须在 ORDER BY / LIMIT 之前:%s", comparison, query)
		assert.Equal(t, 4+len(owned.Args())+1, strings.Count(query, "?"), "%s 段:4 个到期条件 + 片号 + LIMIT", comparison)
		assert.NotContains(t, strings.ToUpper(query), "FOR UPDATE", "%s 段是候选读,必须是普通读", comparison)
		assert.True(t, strings.HasSuffix(recallListDueQuery(comparison, workshard.Filter{}), " ? AND 1=1 ORDER BY next_attempt_ms,op_id LIMIT ?"),
			"%s 段:零值条件不过滤", comparison)
	}
}

// limit 非正时不发 SQL:store 的 db 是 nil,走到查询就会 panic。
func TestRecallListDueOwned_NonPositiveLimitSkipsSQL(t *testing.T) {
	s := &RecallStore{}
	for _, limit := range []int{0, -1} {
		ids, err := s.ListDueOwned(context.Background(), 1, limit, workshard.Filter{})
		require.NoError(t, err)
		assert.Empty(t, ids)
		ids, err = s.ListDue(context.Background(), 1, limit)
		require.NoError(t, err)
		assert.Empty(t, ids)
	}
}
