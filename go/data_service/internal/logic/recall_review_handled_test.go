package logic

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"data_service/internal/constants"
	"data_service/internal/store"
	"data_service/internal/svc"

	dbpb "proto/common/database"
	dspb "proto/data_service"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reviewRecallStore 在 fakeRecallStore 上加交人工处置的登记:登记的流水同时记进 consumed(与真 SQL 同一个主键,
// 计划按它剔除)和 manual;只在 consumed 里的算被回收批次消费。真 SQL(同一事务写来源与审计、撞主键整笔回滚)在 store 的集成测试里。
type reviewRecallStore struct {
	*fakeRecallStore
	manual     map[uint64]bool
	registered []store.RecallReviewHandled
	// onRegister 非空时替代默认的登记:模拟并发写者抢先、库故障。
	onRegister func(store.RecallReviewHandled) (bool, error)
}

func newReviewRecallStore() *reviewRecallStore {
	return &reviewRecallStore{fakeRecallStore: &fakeRecallStore{consumed: map[uint64]bool{}}, manual: map[uint64]bool{}}
}

func (f *reviewRecallStore) markManual(txIDs []uint64) {
	for _, id := range txIDs {
		f.manual[id], f.consumed[id] = true, true
	}
}

func (f *reviewRecallStore) SourceState(_ context.Context, txIDs []uint64) (store.RecallSourceState, error) {
	var state store.RecallSourceState
	for _, id := range txIDs {
		switch {
		case f.manual[id]:
			state.Manual = true
		case f.consumed[id]:
			state.Batch = true
		}
	}
	return state, nil
}

func (f *reviewRecallStore) RegisterReviewHandled(_ context.Context, h store.RecallReviewHandled, _ uint64) (bool, error) {
	if f.onRegister != nil {
		return f.onRegister(h)
	}
	f.registered = append(f.registered, h)
	f.markManual(h.TxIDs)
	return true, nil
}

// reviewRows 是玩家 9001 收到的一笔银两(指令号 601):重投出了两条流水,已提交账本里找不到这条指令。
func reviewRows() []*store.TransactionLogRow {
	return []*store.TransactionLogRow{
		{TxID: 11, ToPlayer: 9001, CurrencyDelta: 100, CorrelationID: 601, Timestamp: 1},
		{TxID: 12, ToPlayer: 9001, CurrencyDelta: 100, CorrelationID: 601, Timestamp: 5},
	}
}

// ledgerWithout 是一份有写权行、但账本里没有这条指令的已提交存档:计划判 NOT_IN_LEDGER。
func ledgerWithout(playerID uint64) *fakeRecallLedgers {
	return &fakeRecallLedgers{rows: map[uint64]*store.PlayerAssetRow{playerID: {Released: true,
		Player: &dbpb.PlayerDatabase{PlayerId: playerID, AssetOpLedger: committedCredits(999).Ledger}}}}
}

func reviewSvcCtx(t *testing.T, rows []*store.TransactionLogRow, ledgers *fakeRecallLedgers) (*svc.ServiceContext, *reviewRecallStore) {
	t.Helper()
	svcCtx, _, _ := newRecallSvcCtx(t, rows)
	recall := newReviewRecallStore()
	svcCtx.Recall = recall
	svcCtx.PlayerStoreAssets = ledgers
	return svcCtx, recall
}

func reviewReq() *RecallReviewHandledReq {
	return &RecallReviewHandledReq{Kind: dspb.RecallKind_RECALL_KIND_CURRENCY, CurrencyType: 0, PlayerID: 9001, CorrelationID: 601,
		TxIDs: []uint64{12, 11}, ReviewReason: dspb.RecallReviewReason_RECALL_REVIEW_NOT_IN_LEDGER,
		Operator: "tester", Reason: "工单 123 已挂欠款", PeerAddr: "127.0.0.1:1"}
}

func reviewClock() time.Time { return manualNow }

// 端到端:预览列出 needs_review → 登记 → 再预览不再列出;之后账本证实了这条指令,也不再进自动回收(人工处置过的不会被扣第二次)。
// 再登记一次幂等。
func TestRegisterRecallReviewHandled_ListedInstructionLeavesReviewAndAutoRecall(t *testing.T) {
	rows := reviewRows()
	svcCtx, recall := reviewSvcCtx(t, rows, ledgerWithout(9001))
	preview, err := BatchRecallItems(context.Background(), svcCtx, goldRecallReq(true))
	require.NoError(t, err)
	require.Len(t, preview.NeedsReview, 2)
	assert.Empty(t, preview.Results)
	listed := []uint64{preview.NeedsReview[0].TxID, preview.NeedsReview[1].TxID}
	assert.Equal(t, dspb.RecallReviewReason_RECALL_REVIEW_NOT_IN_LEDGER, preview.NeedsReview[0].Reason)

	req := reviewReq()
	req.TxIDs = listed
	got, err := registerRecallReviewHandled(context.Background(), svcCtx, req, reviewClock)
	require.NoError(t, err)
	assert.Equal(t, RecallReviewHandledResult{ErrorCode: constants.ErrCodeOK, Registered: true, SourceRows: 2,
		ReviewReason: dspb.RecallReviewReason_RECALL_REVIEW_NOT_IN_LEDGER}, got)
	require.Len(t, recall.registered, 1)
	assert.Equal(t, store.RecallReviewHandled{PlayerID: 9001, CorrelationID: 601, TxIDs: []uint64{11, 12}, Operator: "tester",
		Reason: "工单 123 已挂欠款", Detail: recall.registered[0].Detail}, recall.registered[0])
	assert.Contains(t, recall.registered[0].Detail, "review_reason=RECALL_REVIEW_NOT_IN_LEDGER")

	again, err := BatchRecallItems(context.Background(), svcCtx, goldRecallReq(true))
	require.NoError(t, err)
	assert.Empty(t, again.NeedsReview, "登记过的指令不再列入 needs_review")
	assert.Empty(t, again.Results)

	svcCtx.PlayerStoreAssets = provenLedgers(rows)
	proven, err := BatchRecallItems(context.Background(), svcCtx, goldRecallReq(true))
	require.NoError(t, err)
	assert.Empty(t, proven.Results, "证据转正之后也不进自动回收:人工扣过的不会再被扣一次")
	assert.Empty(t, proven.NeedsReview)

	repeat, err := registerRecallReviewHandled(context.Background(), svcCtx, req, reviewClock)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, repeat.ErrorCode)
	assert.False(t, repeat.Registered, "幂等:已登记过,什么都不改")
	assert.Len(t, recall.registered, 1)
}

// 只收此刻确实在 needs_review 里的指令。每一种对不上都回 32(没有这条指令回 34),什么都不写。
func TestRegisterRecallReviewHandled_RefusesWhatIsNotListed(t *testing.T) {
	cases := []struct {
		name    string
		ledgers func([]*store.TransactionLogRow) *fakeRecallLedgers
		setup   func(*reviewRecallStore)
		edit    func(*RecallReviewHandledReq)
		code    uint32
		reason  dspb.RecallReviewReason
	}{
		{name: "账本已证实、能自动扣:走 BatchRecallItems", ledgers: provenLedgers, code: constants.ErrCodeRecallPlanChanged},
		{name: "已被回收批次消费", setup: func(f *reviewRecallStore) { f.consumed[11] = true }, code: constants.ErrCodeRecallPlanChanged},
		{name: "少报了一条流水", edit: func(r *RecallReviewHandledReq) { r.TxIDs = []uint64{11} }, code: constants.ErrCodeRecallPlanChanged,
			reason: dspb.RecallReviewReason_RECALL_REVIEW_NOT_IN_LEDGER},
		{name: "多报了别的流水", edit: func(r *RecallReviewHandledReq) { r.TxIDs = []uint64{11, 12, 99} }, code: constants.ErrCodeRecallPlanChanged,
			reason: dspb.RecallReviewReason_RECALL_REVIEW_NOT_IN_LEDGER},
		{name: "原因码和重算的不一样", edit: func(r *RecallReviewHandledReq) {
			r.ReviewReason = dspb.RecallReviewReason_RECALL_REVIEW_LEDGER_AMBIGUOUS
		}, code: constants.ErrCodeRecallPlanChanged, reason: dspb.RecallReviewReason_RECALL_REVIEW_NOT_IN_LEDGER},
		{name: "这名玩家没有这条指令", edit: func(r *RecallReviewHandledReq) { r.CorrelationID = 602 }, code: constants.ErrCodeRecallNotFound},
		{name: "资产不对:这条指令没发道具", edit: func(r *RecallReviewHandledReq) {
			r.Kind, r.CurrencyType, r.ItemConfigID = dspb.RecallKind_RECALL_KIND_ITEM, 0, 10
		}, code: constants.ErrCodeRecallNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows := reviewRows()
			ledgers := ledgerWithout(9001)
			if c.ledgers != nil {
				ledgers = c.ledgers(rows)
			}
			svcCtx, recall := reviewSvcCtx(t, rows, ledgers)
			if c.setup != nil {
				c.setup(recall)
			}
			req := reviewReq()
			if c.edit != nil {
				c.edit(req)
			}
			got, err := registerRecallReviewHandled(context.Background(), svcCtx, req, reviewClock)
			require.NoError(t, err)
			assert.Equal(t, c.code, got.ErrorCode)
			assert.Equal(t, c.reason, got.ReviewReason)
			assert.False(t, got.Registered)
			assert.Empty(t, recall.registered, "不在 needs_review 里就不登记")
			assert.Empty(t, recall.manual)
		})
	}
}

// 入参校验在碰任何存储之前。
func TestRegisterRecallReviewHandled_RejectsInvalidRequests(t *testing.T) {
	cases := map[string]func(*RecallReviewHandledReq){
		"没有玩家":     func(r *RecallReviewHandledReq) { r.PlayerID = 0 },
		"没有指令号":    func(r *RecallReviewHandledReq) { r.CorrelationID = 0 },
		"没有流水":     func(r *RecallReviewHandledReq) { r.TxIDs = nil },
		"流水重复":     func(r *RecallReviewHandledReq) { r.TxIDs = []uint64{11, 11} },
		"流水号为 0":   func(r *RecallReviewHandledReq) { r.TxIDs = []uint64{0, 11} },
		"流水超过上限":   func(r *RecallReviewHandledReq) { r.TxIDs = make([]uint64, maxRecallSourceRows+1) },
		"没填原因码":    func(r *RecallReviewHandledReq) { r.ReviewReason = dspb.RecallReviewReason_RECALL_REVIEW_REASON_UNSPECIFIED },
		"没有指令号的原因": func(r *RecallReviewHandledReq) { r.ReviewReason = dspb.RecallReviewReason_RECALL_REVIEW_NO_CORRELATION },
		"不认识的原因码":  func(r *RecallReviewHandledReq) { r.ReviewReason = 99 },
		"道具没填配置号":  func(r *RecallReviewHandledReq) { r.Kind = dspb.RecallKind_RECALL_KIND_ITEM },
		"货币带配置号":   func(r *RecallReviewHandledReq) { r.ItemConfigID = 10 },
		"不认识的币种":   func(r *RecallReviewHandledReq) { r.CurrencyType = 999 },
		"没有操作人":    func(r *RecallReviewHandledReq) { r.Operator = "" },
		"没有理由":     func(r *RecallReviewHandledReq) { r.Reason = "" },
		"理由超过列宽":   func(r *RecallReviewHandledReq) { r.Reason = strings.Repeat("字", 192) },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			ledgers := ledgerWithout(9001)
			svcCtx, recall := reviewSvcCtx(t, reviewRows(), ledgers)
			req := reviewReq()
			edit(req)
			got, err := registerRecallReviewHandled(context.Background(), svcCtx, req, reviewClock)
			require.NoError(t, err)
			assert.Equal(t, constants.ErrCodeInvalidRequest, got.ErrorCode)
			assert.Empty(t, svcCtx.TxLogStore.(*fakeTransactionLogStore).queries)
			assert.Empty(t, ledgers.reads)
			assert.Empty(t, recall.registered)
		})
	}
}

// truncatedTxLog 回的行比 total 少:这条指令的流水超过一次能取的上限。
type truncatedTxLog struct{ *fakeTransactionLogStore }

func (f truncatedTxLog) QueryLog(ctx context.Context, q *store.TransactionLogQuery) ([]*store.TransactionLogRow, uint32, error) {
	rows, total, err := f.fakeTransactionLogStore.QueryLog(ctx, q)
	return rows, total + 1, err
}

// 读不全证据就不登记(fail-closed):查流水失败、流水被截断、读账本失败、登记写失败。
func TestRegisterRecallReviewHandled_FailsClosed(t *testing.T) {
	t.Run("查流水失败", func(t *testing.T) {
		svcCtx, recall := reviewSvcCtx(t, reviewRows(), ledgerWithout(9001))
		svcCtx.TxLogStore = &fakeTransactionLogStore{err: errors.New("query unavailable")}
		got, err := registerRecallReviewHandled(context.Background(), svcCtx, reviewReq(), reviewClock)
		require.Error(t, err)
		assert.Equal(t, constants.ErrCodeSnapshotDBError, got.ErrorCode)
		assert.Empty(t, recall.registered)
	})
	t.Run("流水被截断", func(t *testing.T) {
		svcCtx, recall := reviewSvcCtx(t, reviewRows(), ledgerWithout(9001))
		svcCtx.TxLogStore = truncatedTxLog{&fakeTransactionLogStore{rows: reviewRows()}}
		got, err := registerRecallReviewHandled(context.Background(), svcCtx, reviewReq(), reviewClock)
		require.NoError(t, err)
		assert.Equal(t, constants.ErrCodeResultTruncated, got.ErrorCode)
		assert.Empty(t, recall.registered)
	})
	t.Run("读账本失败", func(t *testing.T) {
		ledgers := ledgerWithout(9001)
		ledgers.err = errors.New("tikv down")
		svcCtx, recall := reviewSvcCtx(t, reviewRows(), ledgers)
		got, err := registerRecallReviewHandled(context.Background(), svcCtx, reviewReq(), reviewClock)
		require.Error(t, err)
		assert.Equal(t, constants.ErrCodeSnapshotDBError, got.ErrorCode)
		assert.Empty(t, recall.registered)
	})
	t.Run("登记写失败", func(t *testing.T) {
		svcCtx, recall := reviewSvcCtx(t, reviewRows(), ledgerWithout(9001))
		recall.onRegister = func(store.RecallReviewHandled) (bool, error) { return false, errors.New("commit outcome unknown") }
		got, err := registerRecallReviewHandled(context.Background(), svcCtx, reviewReq(), reviewClock)
		require.Error(t, err)
		assert.Equal(t, constants.ErrCodeSnapshotDBError, got.ErrorCode)
		assert.False(t, got.Registered)
	})
}

// 登记撞了主键(事务已回滚):重读分辨抢先的是谁。另一次相同的登记抢先 = 幂等成功;回收批次抢先 = 它已不在 needs_review 里。
func TestRegisterRecallReviewHandled_ConcurrentWriterIsReclassified(t *testing.T) {
	cases := []struct {
		name   string
		winner func(*reviewRecallStore, []uint64)
		code   uint32
	}{
		{"另一次相同的登记抢先", func(f *reviewRecallStore, ids []uint64) { f.markManual(ids) }, constants.ErrCodeOK},
		{"回收批次抢先", func(f *reviewRecallStore, ids []uint64) {
			for _, id := range ids {
				f.consumed[id] = true
			}
		}, constants.ErrCodeRecallPlanChanged},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svcCtx, recall := reviewSvcCtx(t, reviewRows(), ledgerWithout(9001))
			recall.onRegister = func(h store.RecallReviewHandled) (bool, error) {
				c.winner(recall, h.TxIDs)
				return false, store.ErrRecallSourceConsumed
			}
			got, err := registerRecallReviewHandled(context.Background(), svcCtx, reviewReq(), reviewClock)
			require.NoError(t, err)
			assert.Equal(t, c.code, got.ErrorCode)
			assert.False(t, got.Registered, "本次没有写入")
			assert.Zero(t, got.SourceRows)
		})
	}
}
