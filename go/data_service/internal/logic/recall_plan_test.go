package logic

import (
	"math"
	"testing"

	"data_service/internal/store"

	dspb "proto/data_service"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func itemFilter() recallFilter {
	return recallFilter{Kind: dspb.RecallKind_RECALL_KIND_ITEM, ItemConfigID: 10, TimeStart: 1, TimeEnd: 2}
}

func goldFilter() recallFilter {
	return recallFilter{Kind: dspb.RecallKind_RECALL_KIND_CURRENCY, CurrencyType: 0, TimeStart: 1, TimeEnd: 2}
}

// 只有收益进计划:转出、销毁这类流水没有东西可回收。
func TestBuildRecallPlan_OnlyGainsCount(t *testing.T) {
	rows := []*store.TransactionLogRow{
		{TxID: 1, ToPlayer: 1, CurrencyDelta: 100},
		{TxID: 2, FromPlayer: 1, CurrencyDelta: -40},
		{TxID: 3, ToPlayer: 2, CurrencyDelta: 0},
		{TxID: 4, ToPlayer: 2, ItemConfigID: 10, ItemQuantity: 3},
		{TxID: 5, ToPlayer: 2, CurrencyType: 1, CurrencyDelta: 7},
	}
	plan, err := buildRecallPlan(goldFilter(), rows, nil)
	require.NoError(t, err)
	require.Len(t, plan.Ops, 1)
	assert.Equal(t, uint64(1), plan.Ops[0].PlayerID)
	assert.Equal(t, uint64(100), plan.Ops[0].Requested)
	assert.Equal(t, []uint64{1}, plan.Ops[0].SourceTxIDs)
	assert.Equal(t, uint32(1), plan.SourceRows)
}

// QueryLog 按 from_player OR to_player 查。带名单时不能回收到名单外的交易对方。
func TestBuildRecallPlan_PlayerListOnlyRecallsListedReceivers(t *testing.T) {
	filter := itemFilter()
	filter.PlayerIDs = []uint64{1}
	rows := []*store.TransactionLogRow{
		{TxID: 1, FromPlayer: 1, ToPlayer: 2, ItemConfigID: 10, ItemQuantity: 5},
		{TxID: 2, FromPlayer: 2, ToPlayer: 1, ItemConfigID: 10, ItemQuantity: 3},
	}
	plan, err := buildRecallPlan(filter, rows, nil)
	require.NoError(t, err)
	require.Len(t, plan.Ops, 1)
	assert.Equal(t, uint64(1), plan.Ops[0].PlayerID)
	assert.Equal(t, uint64(3), plan.Ops[0].Requested)
}

// 带名单时同一条流水可能查出来两次,只能算一次。
func TestBuildRecallPlan_DuplicateRowsCountOnce(t *testing.T) {
	filter := itemFilter()
	filter.PlayerIDs = []uint64{1, 2}
	row := &store.TransactionLogRow{TxID: 1, FromPlayer: 1, ToPlayer: 2, ItemConfigID: 10, ItemQuantity: 5}
	plan, err := buildRecallPlan(filter, []*store.TransactionLogRow{row, row}, nil)
	require.NoError(t, err)
	require.Len(t, plan.Ops, 1)
	assert.Equal(t, uint64(5), plan.Ops[0].Requested)
	assert.Equal(t, uint32(1), plan.SourceRows)
}

func TestBuildRecallPlan_HintsAreDistinctAndZeroIsSkipped(t *testing.T) {
	rows := []*store.TransactionLogRow{
		{TxID: 1, ToPlayer: 1, ItemConfigID: 10, ItemQuantity: 2, ItemUUID: 900},
		{TxID: 2, ToPlayer: 1, ItemConfigID: 10, ItemQuantity: 3, ItemUUID: 900},
		{TxID: 3, ToPlayer: 1, ItemConfigID: 10, ItemQuantity: 4},
	}
	plan, err := buildRecallPlan(itemFilter(), rows, nil)
	require.NoError(t, err)
	require.Len(t, plan.Ops, 1)
	assert.Equal(t, uint64(9), plan.Ops[0].Requested)
	assert.Equal(t, []uint64{900}, plan.Ops[0].Hints, "scene 拒绝 0 和重复的线索")
}

func manyInstanceRows(n int) []*store.TransactionLogRow {
	rows := make([]*store.TransactionLogRow, 0, n)
	for i := range n {
		rows = append(rows, &store.TransactionLogRow{TxID: uint64(i + 1), ToPlayer: 1, ItemConfigID: 10, ItemQuantity: 1, ItemUUID: uint64(1000 + i)})
	}
	return rows
}

// 超过线索上限拆成多行,每行的线索不超过 scene 的上限,数量和线索对得上。
func TestBuildRecallPlan_SplitsWhenHintsOverflow(t *testing.T) {
	plan, err := buildRecallPlan(itemFilter(), manyInstanceRows(maxRecallHintsPerOp+1), nil)
	require.NoError(t, err)
	require.Len(t, plan.Ops, 2)
	assert.Len(t, plan.Ops[0].Hints, maxRecallHintsPerOp)
	assert.Equal(t, uint64(maxRecallHintsPerOp), plan.Ops[0].Requested)
	assert.Len(t, plan.Ops[1].Hints, 1)
	assert.Equal(t, uint64(1), plan.Ops[1].Requested)
}

// 一名玩家的行数超过一条流能同时未决的上限:派不出去,冻结之前就拒绝。
func TestBuildRecallPlan_TooManyOpsForOnePlayerIsRejected(t *testing.T) {
	_, err := buildRecallPlan(itemFilter(), manyInstanceRows(maxRecallHintsPerOp*maxRecallOpsPerPlayer+1), nil)
	require.ErrorIs(t, err, errRecallPlanTooLarge)
}

// 货币汇总超过 scene 的 INT64_MAX 上限不能拆:欠款是合并的。
func TestBuildRecallPlan_CurrencyOverflowIsRejected(t *testing.T) {
	rows := []*store.TransactionLogRow{
		{TxID: 1, ToPlayer: 1, CurrencyDelta: math.MaxInt64},
		{TxID: 2, ToPlayer: 1, CurrencyDelta: 1},
	}
	_, err := buildRecallPlan(goldFilter(), rows, nil)
	require.ErrorIs(t, err, errRecallPlanTooLarge)
}

// 道具数量进 uint32:超了拆行,不能截断。
func TestBuildRecallPlan_ItemCountNeverTruncates(t *testing.T) {
	rows := []*store.TransactionLogRow{
		{TxID: 1, ToPlayer: 1, ItemConfigID: 10, ItemQuantity: math.MaxUint32},
		{TxID: 2, ToPlayer: 1, ItemConfigID: 10, ItemQuantity: 1},
	}
	plan, err := buildRecallPlan(itemFilter(), rows, nil)
	require.NoError(t, err)
	require.Len(t, plan.Ops, 2)
	assert.Equal(t, uint64(math.MaxUint32), plan.Ops[0].Requested)
	assert.Equal(t, uint64(1), plan.Ops[1].Requested)
	assert.Equal(t, uint64(math.MaxUint32)+1, plan.AmountRequested)
}

// 摘要不随查询顺序变;过滤条件、金额、流水、线索变了都要变。
func TestRecallPlanDigest_StableAcrossOrderAndSensitiveToContent(t *testing.T) {
	rows := []*store.TransactionLogRow{
		{TxID: 1, ToPlayer: 1, ItemConfigID: 10, ItemQuantity: 2, ItemUUID: 900},
		{TxID: 2, ToPlayer: 2, ItemConfigID: 10, ItemQuantity: 3},
	}
	reversed := []*store.TransactionLogRow{rows[1], rows[0]}
	first, err := buildRecallPlan(itemFilter(), rows, nil)
	require.NoError(t, err)
	second, err := buildRecallPlan(itemFilter(), reversed, nil)
	require.NoError(t, err)
	assert.Equal(t, first.DigestHex(), second.DigestHex())

	different := func(mutate func(*store.TransactionLogRow)) string {
		changed := *rows[0]
		mutate(&changed)
		plan, err := buildRecallPlan(itemFilter(), []*store.TransactionLogRow{&changed, rows[1]}, nil)
		require.NoError(t, err)
		return plan.DigestHex()
	}
	assert.NotEqual(t, first.DigestHex(), different(func(r *store.TransactionLogRow) { r.ItemQuantity = 9 }), "数量")
	assert.NotEqual(t, first.DigestHex(), different(func(r *store.TransactionLogRow) { r.TxID = 7 }), "流水")
	assert.NotEqual(t, first.DigestHex(), different(func(r *store.TransactionLogRow) { r.ItemUUID = 901 }), "线索")

	wider := itemFilter()
	wider.TimeEnd = 3
	other, err := buildRecallPlan(wider, rows, nil)
	require.NoError(t, err)
	assert.NotEqual(t, first.DigestHex(), other.DigestHex(), "过滤条件")
}

func TestRecallFilterDigest_IgnoresListOrder(t *testing.T) {
	a := recallFilter{Kind: dspb.RecallKind_RECALL_KIND_ITEM, ItemConfigID: 10, TimeStart: 1, TimeEnd: 2, PlayerIDs: []uint64{1, 2}, TxTypes: []uint32{8, 9}}
	b := a
	b.PlayerIDs = []uint64{2, 1}
	b.TxTypes = []uint32{9, 8}
	assert.Equal(t, a.digest(), b.digest())
	b.PlayerIDs = []uint64{2, 1, 3}
	assert.NotEqual(t, a.digest(), b.digest())
}
