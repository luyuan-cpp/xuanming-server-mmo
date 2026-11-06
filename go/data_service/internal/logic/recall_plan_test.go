package logic

import (
	"math"
	"slices"
	"testing"

	"data_service/internal/store"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"
	dspb "proto/data_service"
	"shared/assetop"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func itemFilter() recallFilter {
	return recallFilter{Kind: dspb.RecallKind_RECALL_KIND_ITEM, ItemConfigID: 10, TimeStart: 1, TimeEnd: 2}
}

func goldFilter() recallFilter {
	return recallFilter{Kind: dspb.RecallKind_RECALL_KIND_CURRENCY, CurrencyType: 0, TimeStart: 1, TimeEnd: 2}
}

// ── 已提交账本的测试替身 ───────────────────────────────────────

const testCreditEpoch = uint64(1700000000000)

func newTestStream(stream assetpb.AssetOpStream) *componentpb.AssetOpStreamLedger {
	return &componentpb.AssetOpStreamLedger{Stream: stream, StreamEpoch: testCreditEpoch,
		SeenBits: make([]uint64, assetop.WindowWords), AppliedBits: make([]uint64, assetop.WindowWords)}
}

// markSeq 记一个 seq 的结局,位序与 scene 一致:第 i/64 个字的第 i%64 位,i = seq - watermark - 1。
func markSeq(stream *componentpb.AssetOpStreamLedger, seq uint64, applied bool) {
	i := seq - stream.GetWatermark() - 1
	stream.SeenBits[i/64] |= uint64(1) << (i % 64)
	if applied {
		stream.AppliedBits[i/64] |= uint64(1) << (i % 64)
	}
	if seq > stream.MaxSeq {
		stream.MaxSeq = seq
	}
}

// receiptStream 是一条流上 seq 1 的一张收据;seen / applied 决定位图上这个 seq 的结局。
func receiptStream(stream assetpb.AssetOpStream, correlationID uint64, seen, applied bool) *componentpb.AssetOpStreamLedger {
	s := newTestStream(stream)
	if seen {
		markSeq(s, 1, applied)
	}
	s.Receipts = []*componentpb.AssetOpReceipt{{Seq: 1, CorrelationId: correlationID}}
	return s
}

func ledgerOf(streams ...*componentpb.AssetOpStreamLedger) PlayerStoreLedger {
	return PlayerStoreLedger{Found: true, Released: true, Ledger: &componentpb.PlayerAssetOpLedgerComp{Streams: streams}}
}

// committedCredits 是一份已提交账本:每个指令号在发放流上各有一张已应用的收据。一条流的窗口只有 1024 位,
// 指令多时每 1000 张摊到下一条发放流上(最多三条流,3000 张)。
func committedCredits(correlationIDs ...uint64) PlayerStoreLedger {
	creditStreams := []assetpb.AssetOpStream{
		assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT,
		assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT,
		assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT,
	}
	var streams []*componentpb.AssetOpStreamLedger
	for start := 0; start < len(correlationIDs); start += 1000 {
		stream := newTestStream(creditStreams[start/1000])
		for i, id := range correlationIDs[start:min(start+1000, len(correlationIDs))] {
			seq := uint64(i + 1)
			markSeq(stream, seq, true)
			stream.Receipts = append(stream.Receipts, &componentpb.AssetOpReceipt{Seq: seq, CorrelationId: id})
		}
		streams = append(streams, stream)
	}
	return ledgerOf(streams...)
}

// proven 给还没有指令号的流水各配一个独立的指令号,并造出已提交账本证明每条指令应用了一次。
// 只关心汇总、拆行与摘要的用例用它把证据补齐。
func proven(rows ...*store.TransactionLogRow) recallInputs {
	byPlayer := make(map[uint64][]uint64)
	for _, row := range rows {
		if row.CorrelationID == 0 {
			row.CorrelationID = 500_000 + row.TxID
		}
		if row.ToPlayer != 0 && !slices.Contains(byPlayer[row.ToPlayer], row.CorrelationID) {
			byPlayer[row.ToPlayer] = append(byPlayer[row.ToPlayer], row.CorrelationID)
		}
	}
	ledgers := make(map[uint64]PlayerStoreLedger, len(byPlayer))
	for player, ids := range byPlayer {
		ledgers[player] = committedCredits(ids...)
	}
	return recallInputs{Rows: rows, Ledgers: ledgers}
}

func reviewReasons(plan *recallPlan) map[uint64]dspb.RecallReviewReason {
	out := make(map[uint64]dspb.RecallReviewReason, len(plan.Reviews))
	for _, review := range plan.Reviews {
		out[review.Row.TxID] = review.Reason
	}
	return out
}

// ── 汇总与拆行 ─────────────────────────────────────────────────

// 只有收益进计划:转出、销毁这类流水没有东西可回收。
func TestBuildRecallPlan_OnlyGainsCount(t *testing.T) {
	rows := []*store.TransactionLogRow{
		{TxID: 1, ToPlayer: 1, CurrencyDelta: 100},
		{TxID: 2, FromPlayer: 1, CurrencyDelta: -40},
		{TxID: 3, ToPlayer: 2, CurrencyDelta: 0},
		{TxID: 4, ToPlayer: 2, ItemConfigID: 10, ItemQuantity: 3},
		{TxID: 5, ToPlayer: 2, CurrencyType: 1, CurrencyDelta: 7},
	}
	plan, err := buildRecallPlan(goldFilter(), proven(rows...))
	require.NoError(t, err)
	require.Len(t, plan.Ops, 1)
	assert.Equal(t, uint64(1), plan.Ops[0].PlayerID)
	assert.Equal(t, uint64(100), plan.Ops[0].Requested)
	assert.Equal(t, []uint64{1}, plan.Ops[0].SourceTxIDs)
	assert.Equal(t, uint32(1), plan.SourceRows)
	assert.Empty(t, plan.Reviews, "不是收益的行不用人核对")
}

// QueryLog 按 from_player OR to_player 查。带名单时不能回收到名单外的交易对方。
func TestBuildRecallPlan_PlayerListOnlyRecallsListedReceivers(t *testing.T) {
	filter := itemFilter()
	filter.PlayerIDs = []uint64{1}
	rows := []*store.TransactionLogRow{
		{TxID: 1, FromPlayer: 1, ToPlayer: 2, ItemConfigID: 10, ItemQuantity: 5},
		{TxID: 2, FromPlayer: 2, ToPlayer: 1, ItemConfigID: 10, ItemQuantity: 3},
	}
	plan, err := buildRecallPlan(filter, proven(rows...))
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
	plan, err := buildRecallPlan(filter, proven(row, row))
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
	plan, err := buildRecallPlan(itemFilter(), proven(rows...))
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
	plan, err := buildRecallPlan(itemFilter(), proven(manyInstanceRows(maxRecallHintsPerOp+1)...))
	require.NoError(t, err)
	require.Len(t, plan.Ops, 2)
	assert.Len(t, plan.Ops[0].Hints, maxRecallHintsPerOp)
	assert.Equal(t, uint64(maxRecallHintsPerOp), plan.Ops[0].Requested)
	assert.Len(t, plan.Ops[1].Hints, 1)
	assert.Equal(t, uint64(1), plan.Ops[1].Requested)
}

// 一名玩家的行数超过一条流能同时未决的上限:派不出去,冻结之前就拒绝。
func TestBuildRecallPlan_TooManyOpsForOnePlayerIsRejected(t *testing.T) {
	_, err := buildRecallPlan(itemFilter(), proven(manyInstanceRows(maxRecallHintsPerOp*maxRecallOpsPerPlayer+1)...))
	require.ErrorIs(t, err, errRecallPlanTooLarge)
}

// 货币汇总超过 scene 的 INT64_MAX 上限不能拆:欠款是合并的。
func TestBuildRecallPlan_CurrencyOverflowIsRejected(t *testing.T) {
	rows := []*store.TransactionLogRow{
		{TxID: 1, ToPlayer: 1, CurrencyDelta: math.MaxInt64},
		{TxID: 2, ToPlayer: 1, CurrencyDelta: 1},
	}
	_, err := buildRecallPlan(goldFilter(), proven(rows...))
	require.ErrorIs(t, err, errRecallPlanTooLarge)
}

// 道具数量进 uint32:超了拆行,不能截断。
func TestBuildRecallPlan_ItemCountNeverTruncates(t *testing.T) {
	rows := []*store.TransactionLogRow{
		{TxID: 1, ToPlayer: 1, ItemConfigID: 10, ItemQuantity: math.MaxUint32},
		{TxID: 2, ToPlayer: 1, ItemConfigID: 10, ItemQuantity: 1},
	}
	plan, err := buildRecallPlan(itemFilter(), proven(rows...))
	require.NoError(t, err)
	require.Len(t, plan.Ops, 2)
	assert.Equal(t, uint64(math.MaxUint32), plan.Ops[0].Requested)
	assert.Equal(t, uint64(1), plan.Ops[1].Requested)
	assert.Equal(t, uint64(math.MaxUint32)+1, plan.AmountRequested)
}

// ── 证据:已提交账本(第一轮数据审计 §4 第 7 条的下游后果)───────────

// 同一条指令重投再应用:流水多出一条同 correlation_id、不同 tx_id 的,已提交账本说只应用了一次,只能回收一次。
// 原来只按 tx_id 去重,金额算两遍,扣不够的部分挂成永不过期的欠款。
func TestBuildRecallPlan_RedeliveredInstructionCountsOnce(t *testing.T) {
	rows := []*store.TransactionLogRow{
		{TxID: 1, ToPlayer: 7, CurrencyDelta: 100, CorrelationID: 900},
		{TxID: 2, ToPlayer: 7, CurrencyDelta: 100, CorrelationID: 900},
	}
	plan, err := buildRecallPlan(goldFilter(), recallInputs{Rows: rows, Ledgers: map[uint64]PlayerStoreLedger{7: committedCredits(900)}})
	require.NoError(t, err)
	require.Len(t, plan.Ops, 1)
	assert.Equal(t, uint64(100), plan.Ops[0].Requested, "账本只应用了一次")
	assert.Equal(t, uint64(100), plan.AmountRequested)
	assert.Equal(t, []uint64{1, 2}, plan.Ops[0].SourceTxIDs, "同一指令的流水一起消费,下一批不会再回收重投出来的那条")
	assert.Equal(t, uint32(2), plan.SourceRows)
	assert.Empty(t, plan.Reviews)
}

// 重投出来的那条落在时间窗外:并进同一条指令一起消费。否则下一批的时间窗盖住它时,会把这条指令再回收一次。
func TestBuildRecallPlan_RedeliveryOutsideWindowIsConsumedTogether(t *testing.T) {
	inWindow := &store.TransactionLogRow{TxID: 1, ToPlayer: 7, CurrencyDelta: 100, CorrelationID: 900}
	outside := &store.TransactionLogRow{TxID: 2, ToPlayer: 7, CurrencyDelta: 100, CorrelationID: 900}
	unrelated := &store.TransactionLogRow{TxID: 3, ToPlayer: 7, CurrencyDelta: 5, CorrelationID: 901}
	plan, err := buildRecallPlan(goldFilter(), recallInputs{
		Rows:     []*store.TransactionLogRow{inWindow},
		Siblings: []*store.TransactionLogRow{inWindow, outside, unrelated},
		Ledgers:  map[uint64]PlayerStoreLedger{7: committedCredits(900, 901)},
	})
	require.NoError(t, err)
	require.Len(t, plan.Ops, 1)
	assert.Equal(t, uint64(100), plan.Ops[0].Requested)
	assert.Equal(t, []uint64{1, 2}, plan.Ops[0].SourceTxIDs, "窗外别的指令不归这一批")
}

// 同一指令有一条流水已经被回收过:整条指令剔除,不管被消费的是哪一条。
func TestBuildRecallPlan_InstructionAlreadyRecalledIsLeftOut(t *testing.T) {
	recalled := &store.TransactionLogRow{TxID: 1, ToPlayer: 7, CurrencyDelta: 100, CorrelationID: 900}
	lateCopy := &store.TransactionLogRow{TxID: 2, ToPlayer: 7, CurrencyDelta: 100, CorrelationID: 900}
	plan, err := buildRecallPlan(goldFilter(), recallInputs{
		Rows:     []*store.TransactionLogRow{lateCopy},
		Siblings: []*store.TransactionLogRow{recalled, lateCopy},
		Ledgers:  map[uint64]PlayerStoreLedger{7: committedCredits(900)},
		Consumed: map[uint64]bool{1: true},
	})
	require.NoError(t, err)
	assert.Empty(t, plan.Ops, "晚到的重投流水不能让这条指令再被回收一次")
	assert.Empty(t, plan.Reviews)
}

// 已提交账本证明不了的收益列为待人工核对,不进自动扣款,也不消费。
func TestBuildRecallPlan_UnprovenGainsNeedReview(t *testing.T) {
	partial := receiptStream(assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT, 905, true, true)
	partial.PartialSeqs = []uint64{1}
	twice := receiptStream(assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT, 907, true, true)
	markSeq(twice, 2, true)
	twice.Receipts = append(twice.Receipts, &componentpb.AssetOpReceipt{Seq: 2, CorrelationId: 907})

	rows := []*store.TransactionLogRow{
		{TxID: 1, ToPlayer: 1, CurrencyDelta: 10},
		{TxID: 2, ToPlayer: 2, CurrencyDelta: 20, CorrelationID: 902},
		{TxID: 3, ToPlayer: 3, CurrencyDelta: 30, CorrelationID: 903},
		{TxID: 4, ToPlayer: 4, CurrencyDelta: 40, CorrelationID: 904},
		{TxID: 5, ToPlayer: 5, CurrencyDelta: 50, CorrelationID: 905},
		{TxID: 6, ToPlayer: 6, CurrencyDelta: 60, CorrelationID: 906},
		{TxID: 7, ToPlayer: 7, CurrencyDelta: 70, CorrelationID: 907},
		{TxID: 8, ToPlayer: 8, CurrencyDelta: 80, CorrelationID: 908},
	}
	plan, err := buildRecallPlan(goldFilter(), recallInputs{Rows: rows, Ledgers: map[uint64]PlayerStoreLedger{
		// 玩家 2 没有已提交存档。
		3: committedCredits(999),
		4: ledgerOf(receiptStream(assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT, 904, true, false)),
		5: ledgerOf(partial),
		6: ledgerOf(receiptStream(assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT, 906, false, false)),
		7: ledgerOf(twice),
		8: ledgerOf(receiptStream(assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT, 908, true, true)),
	}})
	require.NoError(t, err)
	assert.Empty(t, plan.Ops, "证明不了的一笔都不自动扣")
	assert.Zero(t, plan.SourceRows, "待核对的不消费:下一批证据齐了还能回收")
	assert.Equal(t, map[uint64]dspb.RecallReviewReason{
		1: dspb.RecallReviewReason_RECALL_REVIEW_NO_CORRELATION,
		2: dspb.RecallReviewReason_RECALL_REVIEW_NO_PLAYER_STORE,
		3: dspb.RecallReviewReason_RECALL_REVIEW_NOT_IN_LEDGER,
		4: dspb.RecallReviewReason_RECALL_REVIEW_LEDGER_REJECTED,
		5: dspb.RecallReviewReason_RECALL_REVIEW_LEDGER_PARTIAL,
		6: dspb.RecallReviewReason_RECALL_REVIEW_LEDGER_NOT_APPLIED,
		7: dspb.RecallReviewReason_RECALL_REVIEW_LEDGER_AMBIGUOUS,
		8: dspb.RecallReviewReason_RECALL_REVIEW_NOT_IN_LEDGER, // 扣款流上的收据证明不了一笔收益
	}, reviewReasons(plan))
}

// 账本证实只应用了一次,但几条流水还原不成同一次发放:不猜,待人工核对。同一实例被重投则认作一次。
func TestBuildRecallPlan_RowsThatAreNotOneApplicationNeedReview(t *testing.T) {
	gold := []*store.TransactionLogRow{
		{TxID: 1, ToPlayer: 7, CurrencyDelta: 100, CorrelationID: 900},
		{TxID: 2, ToPlayer: 7, CurrencyDelta: 60, CorrelationID: 900},
	}
	plan, err := buildRecallPlan(goldFilter(), recallInputs{Rows: gold, Ledgers: map[uint64]PlayerStoreLedger{7: committedCredits(900)}})
	require.NoError(t, err)
	assert.Empty(t, plan.Ops)
	assert.Equal(t, map[uint64]dspb.RecallReviewReason{
		1: dspb.RecallReviewReason_RECALL_REVIEW_ROWS_DISAGREE,
		2: dspb.RecallReviewReason_RECALL_REVIEW_ROWS_DISAGREE,
	}, reviewReasons(plan), "一次发放每个币种只有一条流水,数额对不上就不是同一次")

	items := []*store.TransactionLogRow{
		// 实例号不同:一次发了两件不可叠加物品,还是重投时新铸了实例,流水上分不出来。
		{TxID: 3, ToPlayer: 7, ItemConfigID: 10, ItemQuantity: 1, ItemUUID: 801, CorrelationID: 901},
		{TxID: 4, ToPlayer: 7, ItemConfigID: 10, ItemQuantity: 1, ItemUUID: 802, CorrelationID: 901},
		// 同一实例(同 item_uuid、同数量)被重投:就是那一件。
		{TxID: 5, ToPlayer: 7, ItemConfigID: 10, ItemQuantity: 1, ItemUUID: 803, CorrelationID: 902},
		{TxID: 6, ToPlayer: 7, ItemConfigID: 10, ItemQuantity: 1, ItemUUID: 803, CorrelationID: 902},
	}
	plan, err = buildRecallPlan(itemFilter(), recallInputs{Rows: items, Ledgers: map[uint64]PlayerStoreLedger{7: committedCredits(901, 902)}})
	require.NoError(t, err)
	require.Len(t, plan.Ops, 1)
	assert.Equal(t, uint64(1), plan.Ops[0].Requested)
	assert.Equal(t, []uint64{803}, plan.Ops[0].Hints)
	assert.Equal(t, []uint64{5, 6}, plan.Ops[0].SourceTxIDs)
	assert.Equal(t, map[uint64]dspb.RecallReviewReason{
		3: dspb.RecallReviewReason_RECALL_REVIEW_ROWS_DISAGREE,
		4: dspb.RecallReviewReason_RECALL_REVIEW_ROWS_DISAGREE,
	}, reviewReasons(plan))
}

// ── 摘要 ───────────────────────────────────────────────────────

// 摘要不随查询顺序变;过滤条件、金额、流水、线索变了都要变。
func TestRecallPlanDigest_StableAcrossOrderAndSensitiveToContent(t *testing.T) {
	rows := []*store.TransactionLogRow{
		{TxID: 1, ToPlayer: 1, ItemConfigID: 10, ItemQuantity: 2, ItemUUID: 900},
		{TxID: 2, ToPlayer: 2, ItemConfigID: 10, ItemQuantity: 3},
	}
	reversed := []*store.TransactionLogRow{rows[1], rows[0]}
	first, err := buildRecallPlan(itemFilter(), proven(rows...))
	require.NoError(t, err)
	second, err := buildRecallPlan(itemFilter(), proven(reversed...))
	require.NoError(t, err)
	assert.Equal(t, first.DigestHex(), second.DigestHex())

	different := func(mutate func(*store.TransactionLogRow)) string {
		changed := *rows[0]
		mutate(&changed)
		plan, err := buildRecallPlan(itemFilter(), proven(&changed, rows[1]))
		require.NoError(t, err)
		return plan.DigestHex()
	}
	assert.NotEqual(t, first.DigestHex(), different(func(r *store.TransactionLogRow) { r.ItemQuantity = 9 }), "数量")
	assert.NotEqual(t, first.DigestHex(), different(func(r *store.TransactionLogRow) { r.TxID = 7 }), "流水")
	assert.NotEqual(t, first.DigestHex(), different(func(r *store.TransactionLogRow) { r.ItemUUID = 901 }), "线索")

	wider := itemFilter()
	wider.TimeEnd = 3
	other, err := buildRecallPlan(wider, proven(rows...))
	require.NoError(t, err)
	assert.NotEqual(t, first.DigestHex(), other.DigestHex(), "过滤条件")
}

// 待核对的流水也进摘要:同样的流水,账本后来证实了,或者待核对的多了一条,计划都不再是人审的那一份。
func TestRecallPlanDigest_CoversReviewRows(t *testing.T) {
	rows := []*store.TransactionLogRow{{TxID: 1, ToPlayer: 7, CurrencyDelta: 100, CorrelationID: 900}}
	unproven, err := buildRecallPlan(goldFilter(), recallInputs{Rows: rows})
	require.NoError(t, err)
	require.Len(t, unproven.Reviews, 1)

	nowProven, err := buildRecallPlan(goldFilter(), recallInputs{Rows: rows, Ledgers: map[uint64]PlayerStoreLedger{7: committedCredits(900)}})
	require.NoError(t, err)
	assert.NotEqual(t, unproven.DigestHex(), nowProven.DigestHex(), "证据变了")

	more := []*store.TransactionLogRow{rows[0], {TxID: 2, ToPlayer: 8, CurrencyDelta: 5}}
	withMore, err := buildRecallPlan(goldFilter(), recallInputs{Rows: more})
	require.NoError(t, err)
	assert.NotEqual(t, unproven.DigestHex(), withMore.DigestHex(), "待核对的多了一条")
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
