package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"data_service/internal/constants"
	"data_service/internal/store"
	"data_service/internal/svc"

	assetpb "proto/common/asset"
	dspb "proto/data_service"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type fakeSnapshotStore struct {
	playerIDs    []uint64
	listErr      error
	snapshotByID map[uint64]*store.SnapshotRow
	latest       *store.SnapshotRow
	insertID     uint64
	insertErr    error
	insertCalls  int
	auditErr     error
	auditErrs    []error
	audits       []*store.AuditLogRow

	// 帮会闸(07)用:zone → (player → 该 zone 下最新快照 created_at)。
	playerTimesByZone map[uint32]map[uint64]uint64
	timesErr          error
	// auditCtxErrs 记下每次写审计时 ctx 的状态:RESULT 审计必须在脱钩 ctx 上写(07 §7.5.3-3)。
	auditCtxErrs []error
	// onAudit / onInsert 在写审计 / 写快照(回档前安全快照 = 第一笔写)的那一刻回调,用来断言"先于第一笔写"。
	onAudit  func(row *store.AuditLogRow)
	onInsert func()
}

func (f *fakeSnapshotStore) Close() error { return nil }
func (f *fakeSnapshotStore) InsertSnapshot(_ context.Context, row *store.SnapshotRow) (uint64, error) {
	if f.onInsert != nil {
		f.onInsert()
	}
	f.insertCalls++
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	id := f.insertID
	if id == 0 {
		id = 900
	}
	if f.snapshotByID == nil {
		f.snapshotByID = make(map[uint64]*store.SnapshotRow)
	}
	copyRow := *row
	copyRow.ID = id
	f.snapshotByID[id] = &copyRow
	return id, nil
}
func (f *fakeSnapshotStore) InsertSnapshotIfGuidAbsent(ctx context.Context, row *store.SnapshotRow) (uint64, bool, error) {
	// Kafka 落库入口,logic 层不调用;复用 InsertSnapshot 保持接口完整。
	id, err := f.InsertSnapshot(ctx, row)
	return id, err == nil, err
}
func (f *fakeSnapshotStore) InsertSnapshotsIfGuidAbsent(context.Context, []*store.SnapshotRow) (int64, error) {
	// Kafka 批量落库入口(G2a),回档用例不走这条路。返回错误而不是 (0, nil):将来误用时当场暴露,不静默。
	return 0, errors.New("InsertSnapshotsIfGuidAbsent is not used by rollback tests")
}
func (f *fakeSnapshotStore) GetSnapshotByID(_ context.Context, id uint64) (*store.SnapshotRow, error) {
	return f.snapshotByID[id], nil
}
func (f *fakeSnapshotStore) GetLatestSnapshotBefore(context.Context, uint64, uint64) (*store.SnapshotRow, error) {
	return f.latest, nil
}
func (f *fakeSnapshotStore) ListSnapshotsMeta(context.Context, uint64, uint64, uint32) ([]*store.SnapshotMeta, error) {
	return nil, nil
}
func (f *fakeSnapshotStore) GetSnapshotPlayerIDsByZone(context.Context, uint32, uint64) ([]uint64, error) {
	return append([]uint64(nil), f.playerIDs...), f.listErr
}
func (f *fakeSnapshotStore) GetSnapshotPlayerTimesByZone(_ context.Context, zoneID uint32, _ uint64) (map[uint64]uint64, error) {
	if f.timesErr != nil {
		return nil, f.timesErr
	}
	out := make(map[uint64]uint64, len(f.playerTimesByZone[zoneID]))
	for pid, at := range f.playerTimesByZone[zoneID] {
		out[pid] = at
	}
	return out, nil
}
func (f *fakeSnapshotStore) InsertAuditLog(ctx context.Context, row *store.AuditLogRow) error {
	f.auditCtxErrs = append(f.auditCtxErrs, ctx.Err())
	if f.onAudit != nil {
		f.onAudit(row)
	}
	callIndex := len(f.audits)
	f.audits = append(f.audits, row)
	if callIndex < len(f.auditErrs) {
		return f.auditErrs[callIndex]
	}
	return f.auditErr
}

type fakeTransactionLogStore struct {
	rows     []*store.TransactionLogRow
	total    uint32
	totalSet bool
	err      error
	queries  []*store.TransactionLogQuery
}

func (f *fakeTransactionLogStore) Close() error { return nil }
func (f *fakeTransactionLogStore) InsertBatchIgnore(_ context.Context, rows []*store.TransactionLogRow) (int64, error) {
	// Kafka 落库入口,logic 层不调用;返回全部插入以满足接口。
	return int64(len(rows)), nil
}
func (f *fakeTransactionLogStore) QueryLog(_ context.Context, query *store.TransactionLogQuery) ([]*store.TransactionLogRow, uint32, error) {
	queryCopy := *query
	f.queries = append(f.queries, &queryCopy)
	total := uint32(len(f.rows))
	if f.totalSet {
		total = f.total
	}
	return append([]*store.TransactionLogRow(nil), f.rows...), total, f.err
}

// fakeRecallStore 只模拟回收存储的接缝,不是真 SQL。真 SQL 的状态机在 store 的集成测试里。
type fakeRecallStore struct {
	svc.RecallStore
	batches   map[uint64]*store.RecallBatchRow
	ops       map[uint64][]*store.RecallOpRow
	consumed  map[uint64]bool
	createErr error
	cancelErr error
	listErr   error
	created   []*store.RecallOpPlan
	cancelled []uint64
}

func (f *fakeRecallStore) Close() error { return nil }

func (f *fakeRecallStore) ReadBatch(_ context.Context, requestID uint64) (*store.RecallBatchRow, error) {
	batch, ok := f.batches[requestID]
	if !ok {
		return nil, store.ErrRecallBatchNotFound
	}
	return batch, nil
}

func (f *fakeRecallStore) ListOps(_ context.Context, requestID uint64) ([]*store.RecallOpRow, error) {
	return f.ops[requestID], f.listErr
}

func (f *fakeRecallStore) ConsumedSources(_ context.Context, txIDs []uint64) (map[uint64]bool, error) {
	out := make(map[uint64]bool)
	for _, id := range txIDs {
		if f.consumed[id] {
			out[id] = true
		}
	}
	return out, nil
}

func (f *fakeRecallStore) CreateBatch(_ context.Context, batch *store.RecallBatchRow, ops []*store.RecallOpPlan, _ uint64) error {
	if f.createErr != nil {
		return f.createErr
	}
	if f.batches == nil {
		f.batches = make(map[uint64]*store.RecallBatchRow)
		f.ops = make(map[uint64][]*store.RecallOpRow)
	}
	stored := *batch
	stored.OpenOps = batch.OpCount
	stored.State = 1
	f.batches[batch.RequestID] = &stored
	f.created = ops
	for _, op := range ops {
		f.ops[batch.RequestID] = append(f.ops[batch.RequestID], &store.RecallOpRow{OpID: op.OpID, PlayerID: op.PlayerID, Status: 6, Requested: op.Requested,
			CurrencyType: op.CurrencyType, ItemConfigID: op.ItemConfigID})
	}
	return nil
}

func (f *fakeRecallStore) CancelBatch(_ context.Context, requestID uint64, _, _ string, _ uint64) error {
	f.cancelled = append(f.cancelled, requestID)
	return f.cancelErr
}

type fakeRecallOpIDs struct{ next uint64 }

func (f *fakeRecallOpIDs) Next(context.Context) (uint64, error) {
	f.next++
	return 7000 + f.next, nil
}

func recallGoldRows() []*store.TransactionLogRow {
	return []*store.TransactionLogRow{
		{TxID: 1, ToPlayer: 9001, CurrencyType: 0, CurrencyDelta: 100},
		{TxID: 2, ToPlayer: 9001, CurrencyType: 0, CurrencyDelta: 50},
		{TxID: 3, ToPlayer: 9002, CurrencyType: 0, CurrencyDelta: 70},
	}
}

func newRecallSvcCtx(t *testing.T, rows []*store.TransactionLogRow) (*svc.ServiceContext, *fakeRecallStore, *fakeSnapshotStore) {
	t.Helper()
	svcCtx, _ := newTestSvcCtx(t)
	recall := &fakeRecallStore{}
	snapshots := &fakeSnapshotStore{}
	svcCtx.TxLogStore = &fakeTransactionLogStore{rows: rows}
	svcCtx.SnapshotStore = snapshots
	svcCtx.Recall = recall
	svcCtx.RecallReady = true
	svcCtx.RecallOpIDs = &fakeRecallOpIDs{}
	return svcCtx, recall, snapshots
}

func goldRecallReq(dryRun bool) *BatchRecallReq {
	return &BatchRecallReq{Kind: dspb.RecallKind_RECALL_KIND_CURRENCY, CurrencyType: 0, TimeStart: 1, TimeEnd: 2, DryRun: dryRun,
		RequestID: 55, Operator: "tester", Reason: "工单 123"}
}

func previewDigest(t *testing.T, svcCtx *svc.ServiceContext) string {
	t.Helper()
	resp, err := BatchRecallItems(context.Background(), svcCtx, goldRecallReq(true))
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, resp.ErrorCode)
	require.NotEmpty(t, resp.PlanDigest)
	return resp.PlanDigest
}

// 币种 0 是银两。原来 currency_type == 0 被当成没填,银两根本回收不了。
func TestBatchRecallItems_GoldIsRecallable(t *testing.T) {
	svcCtx, _, _ := newRecallSvcCtx(t, recallGoldRows())
	resp, err := BatchRecallItems(context.Background(), svcCtx, goldRecallReq(true))
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, resp.ErrorCode)
	assert.Equal(t, uint32(3), resp.TotalMatched)
	require.Len(t, resp.Results, 2, "按玩家汇总")
	assert.Equal(t, uint64(150), resp.Results[0].Amount)
	assert.Equal(t, uint64(70), resp.Results[1].Amount)
	assert.Equal(t, uint64(220), resp.AmountRequested)
	assert.False(t, resp.Results[0].Success, "预览不是回收")

	query := svcCtx.TxLogStore.(*fakeTransactionLogStore).queries[0]
	assert.True(t, query.ExactCurrency, "CurrencyType 0 在 QueryLog 里表示不过滤,银两要显式说")
}

func TestBatchRecallItems_RejectsInvalidRequests(t *testing.T) {
	svcCtx, recall, _ := newRecallSvcCtx(t, recallGoldRows())
	cases := map[string]func(*BatchRecallReq){
		"没填种类":    func(r *BatchRecallReq) { r.Kind = dspb.RecallKind_RECALL_KIND_UNSPECIFIED },
		"道具没填配置号": func(r *BatchRecallReq) { r.Kind = dspb.RecallKind_RECALL_KIND_ITEM },
		"货币带配置号":  func(r *BatchRecallReq) { r.ItemConfigID = 10 },
		"不认识的币种":  func(r *BatchRecallReq) { r.CurrencyType = 999 },
		"没有时间窗":   func(r *BatchRecallReq) { r.TimeStart = 0 },
		"时间窗反了":   func(r *BatchRecallReq) { r.TimeStart, r.TimeEnd = 3, 2 },
		"玩家名单超上限": func(r *BatchRecallReq) { r.PlayerIDs = make([]uint64, maxRecallPlayers+1) },
		"执行没带请求号": func(r *BatchRecallReq) { r.DryRun, r.ExpectedPlanDigest, r.RequestID = false, "x", 0 },
		"执行没带摘要":  func(r *BatchRecallReq) { r.DryRun, r.ExpectedPlanDigest = false, "" },
		"执行没带操作人": func(r *BatchRecallReq) { r.DryRun, r.ExpectedPlanDigest, r.Operator = false, "x", "" },
		"执行没带理由":  func(r *BatchRecallReq) { r.DryRun, r.ExpectedPlanDigest, r.Reason = false, "x", "" },
		"执行理由超过列宽": func(r *BatchRecallReq) {
			r.DryRun, r.ExpectedPlanDigest, r.Reason = false, "x", strings.Repeat("字", 192)
		},
		"执行操作人超过列宽": func(r *BatchRecallReq) {
			r.DryRun, r.ExpectedPlanDigest, r.Operator = false, "x", strings.Repeat("字", 65)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := goldRecallReq(true)
			mutate(req)
			resp, err := BatchRecallItems(context.Background(), svcCtx, req)
			require.NoError(t, err)
			assert.Equal(t, constants.ErrCodeInvalidRequest, resp.ErrorCode)
			assert.Empty(t, recall.created)
		})
	}
}

func TestBatchRecallItems_NonDryRunRequiresAuditStore(t *testing.T) {
	svcCtx, recall, _ := newRecallSvcCtx(t, recallGoldRows())
	svcCtx.SnapshotStore = nil
	req := goldRecallReq(false)
	req.ExpectedPlanDigest = "anything"
	resp, err := BatchRecallItems(context.Background(), svcCtx, req)
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeSnapshotDBError, resp.ErrorCode)
	assert.Empty(t, recall.created)
}

// 通道没装配时不能写批次,也不能回成功;发起过一次回收这个事实要留痕,但审计表不替没发生的回收背书。
func TestBatchRecallItems_DisabledChannelRefusesAndAuditsZeroAffected(t *testing.T) {
	svcCtx, recall, snapshots := newRecallSvcCtx(t, recallGoldRows())
	digest := previewDigest(t, svcCtx)
	svcCtx.RecallReady = false

	req := goldRecallReq(false)
	req.ExpectedPlanDigest = digest
	resp, err := BatchRecallItems(context.Background(), svcCtx, req)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeRecallDisabled, resp.ErrorCode)
	assert.Empty(t, recall.created)
	assert.Zero(t, resp.TotalRecalled)
	require.Len(t, snapshots.audits, 1)
	assert.Zero(t, snapshots.audits[0].PlayersAffected)
	assert.Contains(t, snapshots.audits[0].Reason, "DISABLED")
}

// 受理回 Pending,不回 0:计划冻结不等于事故处置完。
func TestBatchRecallItems_AcceptedAnswersPendingNotSuccess(t *testing.T) {
	svcCtx, recall, _ := newRecallSvcCtx(t, recallGoldRows())
	req := goldRecallReq(false)
	req.ExpectedPlanDigest = previewDigest(t, svcCtx)

	resp, err := BatchRecallItems(context.Background(), svcCtx, req)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeRecallPending, resp.ErrorCode)
	assert.Zero(t, resp.TotalRecalled, "scene 还没扣,不能算回收")
	assert.Equal(t, uint32(2), resp.TotalPending)
	assert.Equal(t, req.ExpectedPlanDigest, resp.PlanDigest)
	require.Len(t, resp.Results, 2)
	for _, result := range resp.Results {
		assert.False(t, result.Success)
		assert.NotEmpty(t, result.ErrorDetail)
		assert.NotZero(t, result.OpID)
	}

	require.Len(t, recall.created, 2)
	assert.Equal(t, []uint64{1, 2}, recall.created[0].SourceTxIDs)
	bundle := &assetpb.AssetBundle{}
	require.NoError(t, proto.Unmarshal(recall.created[0].Payload, bundle))
	require.Len(t, bundle.GetCurrencies(), 1)
	assert.Equal(t, uint64(150), bundle.GetCurrencies()[0].GetAmount())
}

// 人审过的不是这份计划就不能执行:流水晚到、别的批次消费过都会走到这里。
func TestBatchRecallItems_PlanChangedSinceReviewWritesNothing(t *testing.T) {
	svcCtx, recall, snapshots := newRecallSvcCtx(t, recallGoldRows())
	req := goldRecallReq(false)
	req.ExpectedPlanDigest = previewDigest(t, svcCtx)

	txLog := svcCtx.TxLogStore.(*fakeTransactionLogStore)
	txLog.rows = append(txLog.rows, &store.TransactionLogRow{TxID: 4, ToPlayer: 9003, CurrencyDelta: 1})

	resp, err := BatchRecallItems(context.Background(), svcCtx, req)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeRecallPlanChanged, resp.ErrorCode)
	assert.Empty(t, recall.created)
	require.Len(t, snapshots.audits, 1)
	assert.Contains(t, snapshots.audits[0].Reason, "PLAN_CHANGED")
}

// 重放同一个 request_id 只回现状:不重查流水,不再写批次。
func TestBatchRecallItems_ReplayReturnsFrozenBatch(t *testing.T) {
	svcCtx, recall, _ := newRecallSvcCtx(t, recallGoldRows())
	req := goldRecallReq(false)
	req.ExpectedPlanDigest = previewDigest(t, svcCtx)
	_, err := BatchRecallItems(context.Background(), svcCtx, req)
	require.NoError(t, err)
	txLog := svcCtx.TxLogStore.(*fakeTransactionLogStore)
	queriesBefore := len(txLog.queries)
	recall.created = nil

	// 流水变了也不影响:计划已经冻结。
	txLog.rows = append(txLog.rows, &store.TransactionLogRow{TxID: 4, ToPlayer: 9003, CurrencyDelta: 1})
	resp, err := BatchRecallItems(context.Background(), svcCtx, req)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeRecallPending, resp.ErrorCode)
	assert.Equal(t, uint32(2), resp.TotalPending)
	assert.Empty(t, recall.created, "重放不再写")
	assert.Len(t, txLog.queries, queriesBefore, "重放不重查流水")
}

func TestBatchRecallItems_SameRequestIDDifferentFiltersConflicts(t *testing.T) {
	svcCtx, recall, _ := newRecallSvcCtx(t, recallGoldRows())
	req := goldRecallReq(false)
	req.ExpectedPlanDigest = previewDigest(t, svcCtx)
	_, err := BatchRecallItems(context.Background(), svcCtx, req)
	require.NoError(t, err)
	recall.created = nil

	other := goldRecallReq(false)
	other.ExpectedPlanDigest = req.ExpectedPlanDigest
	other.TimeEnd = 3
	resp, err := BatchRecallItems(context.Background(), svcCtx, other)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeRecallRequestConflict, resp.ErrorCode)
	assert.Empty(t, recall.created)
}

// 两批并发撞同一条流水:后提交的那批回滚,让它重新预览。
func TestBatchRecallItems_ConcurrentSourceConsumptionIsPlanChanged(t *testing.T) {
	svcCtx, recall, _ := newRecallSvcCtx(t, recallGoldRows())
	req := goldRecallReq(false)
	req.ExpectedPlanDigest = previewDigest(t, svcCtx)
	recall.createErr = store.ErrRecallSourceConsumed

	resp, err := BatchRecallItems(context.Background(), svcCtx, req)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeRecallPlanChanged, resp.ErrorCode)
}

func TestBatchRecallItems_CommitFailureIsNotReportedAsAccepted(t *testing.T) {
	svcCtx, recall, _ := newRecallSvcCtx(t, recallGoldRows())
	req := goldRecallReq(false)
	req.ExpectedPlanDigest = previewDigest(t, svcCtx)
	recall.createErr = errors.New("commit outcome unknown")

	resp, err := BatchRecallItems(context.Background(), svcCtx, req)
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeSnapshotDBError, resp.ErrorCode)
	assert.Zero(t, resp.TotalPending)
}

// 已消费的流水不再进计划:时间窗重叠的第二批不会重复回收。
func TestBatchRecallItems_ConsumedSourcesAreLeftOut(t *testing.T) {
	svcCtx, recall, _ := newRecallSvcCtx(t, recallGoldRows())
	recall.consumed = map[uint64]bool{1: true, 3: true}
	resp, err := BatchRecallItems(context.Background(), svcCtx, goldRecallReq(true))
	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
	assert.Equal(t, uint64(9001), resp.Results[0].PlayerID)
	assert.Equal(t, uint64(50), resp.Results[0].Amount)
}

// 全部已消费时计划为空:不写批次,也不回 0。0 说的是这批全额回收了,这里什么都没动。
func TestBatchRecallItems_EmptyPlanWritesNothingAndIsNotSuccess(t *testing.T) {
	svcCtx, recall, _ := newRecallSvcCtx(t, recallGoldRows())
	recall.consumed = map[uint64]bool{1: true, 2: true, 3: true}
	req := goldRecallReq(false)
	req.ExpectedPlanDigest = previewDigest(t, svcCtx)

	resp, err := BatchRecallItems(context.Background(), svcCtx, req)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeRecallNothingToRecall, resp.ErrorCode)
	assert.Empty(t, recall.created)
	assert.Empty(t, resp.Results)
	assert.Zero(t, resp.TotalRecalled)
}

// 带着新预览的摘要重放旧 request_id:回旧批次的现状会让人以为新计划受理了。
func TestBatchRecallItems_SameRequestIDDifferentPlanConflicts(t *testing.T) {
	svcCtx, recall, _ := newRecallSvcCtx(t, recallGoldRows())
	req := goldRecallReq(false)
	req.ExpectedPlanDigest = previewDigest(t, svcCtx)
	_, err := BatchRecallItems(context.Background(), svcCtx, req)
	require.NoError(t, err)
	recall.created = nil

	other := goldRecallReq(false)
	other.ExpectedPlanDigest = "digest of a newer preview"
	resp, err := BatchRecallItems(context.Background(), svcCtx, other)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeRecallRequestConflict, resp.ErrorCode)
	assert.Empty(t, recall.created)
}

// 已经提交了,只是读不回来:不能回失败让人以为没受理。
func TestBatchRecallItems_ReadBackFailureAfterCommitStaysPending(t *testing.T) {
	svcCtx, recall, _ := newRecallSvcCtx(t, recallGoldRows())
	req := goldRecallReq(false)
	req.ExpectedPlanDigest = previewDigest(t, svcCtx)
	recall.listErr = errors.New("read unavailable")

	resp, err := BatchRecallItems(context.Background(), svcCtx, req)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeRecallPending, resp.ErrorCode)
	assert.Equal(t, uint32(2), resp.TotalPending)
	assert.NotEmpty(t, recall.created)
}

func TestBatchRecallItems_DryRunTruncationFailsClosedWithCandidateTotal(t *testing.T) {
	svcCtx, recall, snapshots := newRecallSvcCtx(t, nil)
	txLog := &fakeTransactionLogStore{
		rows:     []*store.TransactionLogRow{{TxID: 1, ToPlayer: 9002, ItemConfigID: 10, ItemQuantity: 3}},
		total:    10001,
		totalSet: true,
	}
	svcCtx.TxLogStore = txLog
	resp, err := BatchRecallItems(context.Background(), svcCtx, &BatchRecallReq{Kind: dspb.RecallKind_RECALL_KIND_ITEM, ItemConfigID: 10, TimeStart: 1, TimeEnd: 2, DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeResultTruncated, resp.ErrorCode)
	assert.Equal(t, uint32(10001), resp.TotalMatched)
	assert.Empty(t, resp.PlanDigest, "截断的候选集不出计划")
	assert.Empty(t, recall.created)
	assert.Empty(t, snapshots.audits, "预览不写库")
}

func TestBatchRecallItems_ExecutionTruncationIsAudited(t *testing.T) {
	svcCtx, recall, snapshots := newRecallSvcCtx(t, nil)
	svcCtx.TxLogStore = &fakeTransactionLogStore{
		rows:     []*store.TransactionLogRow{{TxID: 1, ToPlayer: 9002, ItemConfigID: 10, ItemQuantity: 3}},
		total:    10001,
		totalSet: true,
	}
	resp, err := BatchRecallItems(context.Background(), svcCtx, &BatchRecallReq{Kind: dspb.RecallKind_RECALL_KIND_ITEM, ItemConfigID: 10, TimeStart: 1, TimeEnd: 2,
		RequestID: 55, ExpectedPlanDigest: "x", Operator: "tester", Reason: "工单"})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeResultTruncated, resp.ErrorCode)
	assert.Empty(t, recall.created)
	require.Len(t, snapshots.audits, 1)
	assert.Zero(t, snapshots.audits[0].PlayersAffected)
}

// 名单逐个查,每个都没截断,合起来超过一批的上限同样拒绝。
func TestBatchRecallItems_PlayerListTotalOverLimitIsTruncated(t *testing.T) {
	svcCtx, _, _ := newRecallSvcCtx(t, nil)
	rows := make([]*store.TransactionLogRow, 0, 6000)
	for i := range 6000 {
		rows = append(rows, &store.TransactionLogRow{TxID: uint64(i + 1), ToPlayer: 9001, ItemConfigID: 10, ItemQuantity: 1})
	}
	svcCtx.TxLogStore = &fakeTransactionLogStore{rows: rows}
	req := &BatchRecallReq{Kind: dspb.RecallKind_RECALL_KIND_ITEM, ItemConfigID: 10, TimeStart: 1, TimeEnd: 2, DryRun: true, PlayerIDs: []uint64{9001, 9002}}
	resp, err := BatchRecallItems(context.Background(), svcCtx, req)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeResultTruncated, resp.ErrorCode)
}

func TestBatchRecallItems_PlayerQueryFailureCannotBeSkipped(t *testing.T) {
	svcCtx, _, _ := newRecallSvcCtx(t, nil)
	svcCtx.TxLogStore = &fakeTransactionLogStore{err: errors.New("query unavailable")}
	req := &BatchRecallReq{Kind: dspb.RecallKind_RECALL_KIND_ITEM, ItemConfigID: 10, TimeStart: 1, TimeEnd: 2, DryRun: true, PlayerIDs: []uint64{1, 2}}
	resp, err := BatchRecallItems(context.Background(), svcCtx, req)
	require.Error(t, err)
	assert.ErrorContains(t, err, "player 1")
	assert.Equal(t, constants.ErrCodeSnapshotDBError, resp.ErrorCode)
}

func TestBatchRecallItems_AuditFailureIsReturnedToCaller(t *testing.T) {
	svcCtx, recall, snapshots := newRecallSvcCtx(t, recallGoldRows())
	snapshots.auditErr = errors.New("audit unavailable")
	req := goldRecallReq(false)
	req.ExpectedPlanDigest = "not the digest"

	resp, err := BatchRecallItems(context.Background(), svcCtx, req)
	require.Error(t, err)
	assert.ErrorContains(t, err, "write batch recall audit")
	assert.Equal(t, constants.ErrCodeSnapshotDBError, resp.ErrorCode)
	assert.Empty(t, recall.created)
}

// error_code == 0 只说明全部全额回收。有缺口或没结果都是 Incomplete,还有行没结局是 Pending。
func TestGetRecallBatch_ErrorCodeTellsTheTruth(t *testing.T) {
	batch := func(open, complete, shortfall, failed uint32) *store.RecallBatchRow {
		return &store.RecallBatchRow{RequestID: 55, OpenOps: open, OpsComplete: complete, OpsShortfall: shortfall, OpsFailed: failed, PlanDigest: []byte{1}}
	}
	cases := []struct {
		name         string
		batch        *store.RecallBatchRow
		want         uint32
		wantRecalled uint32
		wantFailed   uint32
		wantPending  uint32
	}{
		{name: "还有行没结局", batch: batch(1, 1, 0, 0), want: constants.ErrCodeRecallPending, wantRecalled: 1, wantPending: 1},
		{name: "全额", batch: batch(0, 2, 0, 0), want: constants.ErrCodeOK, wantRecalled: 2},
		{name: "有缺口", batch: batch(0, 1, 1, 0), want: constants.ErrCodeRecallIncomplete, wantRecalled: 1, wantFailed: 1},
		{name: "scene 拒绝或取消", batch: batch(0, 0, 0, 2), want: constants.ErrCodeRecallIncomplete, wantFailed: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, recall, _ := newRecallSvcCtx(t, nil)
			recall.batches = map[uint64]*store.RecallBatchRow{55: tc.batch}
			resp, err := GetRecallBatch(context.Background(), svcCtx, 55)
			require.NoError(t, err)
			assert.Equal(t, tc.want, resp.ErrorCode)
			assert.Equal(t, tc.wantRecalled, resp.TotalRecalled)
			assert.Equal(t, tc.wantFailed, resp.TotalFailed)
			assert.Equal(t, tc.wantPending, resp.TotalPending)
		})
	}
}

func TestRecallResultOf_OnlyFullyCoveredRowsSucceed(t *testing.T) {
	const (
		pending   = 1
		applied   = 2
		rejected  = 3
		partial   = 5
		cancelled = 7
	)
	cases := []struct {
		name    string
		row     *store.RecallOpRow
		success bool
	}{
		{name: "还没扣", row: &store.RecallOpRow{Status: pending, Requested: 5}},
		{name: "全部扣到", row: &store.RecallOpRow{Status: applied, Requested: 5, Taken: 5}, success: true},
		{name: "余额不足转欠款也算全额兜底", row: &store.RecallOpRow{Status: applied, Requested: 5, Taken: 2, Debt: 3}, success: true},
		{name: "道具没了", row: &store.RecallOpRow{Status: applied, Requested: 5, Taken: 2, Shortfall: 3}},
		{name: "一件没扣到", row: &store.RecallOpRow{Status: applied, Requested: 5, Shortfall: 5}},
		{name: "scene 拒绝", row: &store.RecallOpRow{Status: rejected, Requested: 5, Shortfall: 5}},
		{name: "取消", row: &store.RecallOpRow{Status: cancelled, Requested: 5, Shortfall: 5}},
		{name: "部分应用", row: &store.RecallOpRow{Status: partial, Requested: 5, Taken: 2, Shortfall: 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := recallResultOf(tc.row)
			assert.Equal(t, tc.success, result.Success)
			assert.Equal(t, tc.success, result.ErrorDetail == "", "失败要说原因")
		})
	}
}

func TestCancelRecallBatch_RequiresOperatorAndKnownBatch(t *testing.T) {
	svcCtx, recall, _ := newRecallSvcCtx(t, nil)
	resp, err := CancelRecallBatch(context.Background(), svcCtx, 55, "", "理由", "peer")
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeInvalidRequest, resp.ErrorCode)
	assert.Empty(t, recall.cancelled)

	recall.cancelErr = store.ErrRecallBatchNotFound
	resp, err = CancelRecallBatch(context.Background(), svcCtx, 55, "tester", "理由", "peer")
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeRecallNotFound, resp.ErrorCode)
}

func TestResolveRecallOp_FullAppliedIsNotAcceptable(t *testing.T) {
	svcCtx, _, _ := newRecallSvcCtx(t, nil)
	const applied = 2
	resolved, code, err := ResolveRecallOp(context.Background(), svcCtx, 1, applied, "tester", "理由")
	require.NoError(t, err)
	assert.False(t, resolved)
	assert.Equal(t, constants.ErrCodeInvalidRequest, code, "全额结果只能来自 scene 的明细")
}
