package logic

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"data_service/internal/constants"
	"data_service/internal/store"
	"data_service/internal/svc"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/assetop"
	"shared/assetop/assetoptest"

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
	if len(query.CorrelationIDs) > 0 {
		// 回收按指令号补查同指令流水:只回这些指令号的行,和真 SQL 的 correlation_id IN 一致。
		var matched []*store.TransactionLogRow
		for _, row := range f.rows {
			if slices.Contains(query.CorrelationIDs, row.CorrelationID) {
				matched = append(matched, row)
			}
		}
		return matched, uint32(len(matched)), f.err
	}
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

// fakeRecallLedgers 按玩家回已提交的资产账本(player_store 的写权行 + 钱包行);没登记的玩家回「没有写权行」。
type fakeRecallLedgers struct {
	rows  map[uint64]*store.PlayerAssetRow
	err   error
	reads []uint64
}

func (f *fakeRecallLedgers) ReadPlayerStoreAsset(_ context.Context, playerID uint64) (*store.PlayerAssetRow, error) {
	f.reads = append(f.reads, playerID)
	if f.err != nil {
		return nil, f.err
	}
	return f.rows[playerID], nil
}

// provenLedgers 造出已提交账本,证明每条带指令号的流水,它那条指令都恰好应用了一次。
func provenLedgers(rows []*store.TransactionLogRow) *fakeRecallLedgers {
	byPlayer := make(map[uint64][]uint64)
	for _, row := range rows {
		if row.ToPlayer == 0 || row.CorrelationID == 0 || slices.Contains(byPlayer[row.ToPlayer], row.CorrelationID) {
			continue
		}
		byPlayer[row.ToPlayer] = append(byPlayer[row.ToPlayer], row.CorrelationID)
	}
	f := &fakeRecallLedgers{rows: make(map[uint64]*store.PlayerAssetRow, len(byPlayer))}
	for player, ids := range byPlayer {
		f.rows[player] = &store.PlayerAssetRow{Released: true,
			Player: &dbpb.PlayerDatabase{PlayerId: player, AssetOpLedger: committedCredits(ids...).Ledger}}
	}
	return f
}

// recallGoldRows 是三笔经资产通道发放的银两,各有自己的指令号。
func recallGoldRows() []*store.TransactionLogRow {
	return []*store.TransactionLogRow{
		{TxID: 1, ToPlayer: 9001, CurrencyType: 0, CurrencyDelta: 100, CorrelationID: 501},
		{TxID: 2, ToPlayer: 9001, CurrencyType: 0, CurrencyDelta: 50, CorrelationID: 502},
		{TxID: 3, ToPlayer: 9002, CurrencyType: 0, CurrencyDelta: 70, CorrelationID: 503},
	}
}

// newRecallSvcCtx 装好回收用的替身。已提交账本缺省证实 rows 里每条带指令号的流水;要别的证据就换掉 PlayerStoreAssets。
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
	svcCtx.PlayerStoreAssets = provenLedgers(rows)
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

// 第一轮数据审计 §4 第 7 条的下游后果:同一条指令重投出两条流水,端到端只回收一次;同指令流水不限时间窗补查。
func TestBatchRecallItems_RedeliveredInstructionIsRecalledOnce(t *testing.T) {
	rows := []*store.TransactionLogRow{
		{TxID: 1, ToPlayer: 9001, CurrencyDelta: 100, CorrelationID: 501},
		{TxID: 2, ToPlayer: 9001, CurrencyDelta: 100, CorrelationID: 501},
	}
	svcCtx, recall, _ := newRecallSvcCtx(t, rows)
	preview, err := BatchRecallItems(context.Background(), svcCtx, goldRecallReq(true))
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, preview.ErrorCode)
	require.Len(t, preview.Results, 1)
	assert.Equal(t, uint64(100), preview.Results[0].Amount, "账本只应用了一次")
	assert.Equal(t, uint64(100), preview.AmountRequested)

	queries := svcCtx.TxLogStore.(*fakeTransactionLogStore).queries
	require.Len(t, queries, 2, "一次时间窗查询 + 一次同指令流水查询")
	assert.Equal(t, uint64(9001), queries[1].PlayerID)
	assert.Equal(t, []uint64{501}, queries[1].CorrelationIDs)
	assert.Zero(t, queries[1].TimeStart, "同指令流水不限时间窗")
	assert.Zero(t, queries[1].TimeEnd)
	assert.True(t, queries[1].ExactCurrency)

	req := goldRecallReq(false)
	req.ExpectedPlanDigest = preview.PlanDigest
	resp, err := BatchRecallItems(context.Background(), svcCtx, req)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeRecallPending, resp.ErrorCode)
	require.Len(t, recall.created, 1)
	assert.Equal(t, uint64(100), recall.created[0].Requested)
	assert.Equal(t, []uint64{1, 2}, recall.created[0].SourceTxIDs, "两条一起消费")
}

// 已提交账本证明不了的收益列给人工核对:不进计划,执行时也不消费。
func TestBatchRecallItems_UnprovenGainsAreListedForReview(t *testing.T) {
	rows := []*store.TransactionLogRow{
		{TxID: 1, ToPlayer: 9001, CurrencyDelta: 100, CorrelationID: 501, TxType: 2, Timestamp: 1},
		{TxID: 2, ToPlayer: 9001, CurrencyDelta: 40, TxType: 9, Timestamp: 1},
		{TxID: 3, ToPlayer: 9002, CurrencyDelta: 70, CorrelationID: 503, TxType: 8, Timestamp: 2},
	}
	svcCtx, recall, _ := newRecallSvcCtx(t, rows)
	svcCtx.PlayerStoreAssets = provenLedgers(rows[:1]) // 只有第一条有已提交证据

	preview, err := BatchRecallItems(context.Background(), svcCtx, goldRecallReq(true))
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, preview.ErrorCode)
	require.Len(t, preview.Results, 1)
	assert.Equal(t, uint64(9001), preview.Results[0].PlayerID)
	assert.Equal(t, uint64(100), preview.Results[0].Amount)
	assert.Equal(t, uint64(100), preview.AmountRequested, "待核对的不算进要回收的数")
	assert.Equal(t, []RecallReviewResult{
		{PlayerID: 9001, TxID: 2, TxType: 9, Amount: 40, Timestamp: 1, Reason: dspb.RecallReviewReason_RECALL_REVIEW_NO_CORRELATION},
		{PlayerID: 9002, TxID: 3, CorrelationID: 503, TxType: 8, Amount: 70, Timestamp: 2, Reason: dspb.RecallReviewReason_RECALL_REVIEW_NO_PLAYER_STORE},
	}, preview.NeedsReview)

	req := goldRecallReq(false)
	req.ExpectedPlanDigest = preview.PlanDigest
	_, err = BatchRecallItems(context.Background(), svcCtx, req)
	require.NoError(t, err)
	require.Len(t, recall.created, 1)
	assert.Equal(t, []uint64{1}, recall.created[0].SourceTxIDs, "待核对的流水不消费,证据齐了下一批还能回收")
}

// 读不到已提交账本就不出计划(fail-closed):少一份证据,算出来的就不是人审的那份。
func TestBatchRecallItems_LedgerReadFailureFailsClosed(t *testing.T) {
	svcCtx, recall, _ := newRecallSvcCtx(t, recallGoldRows())
	svcCtx.PlayerStoreAssets.(*fakeRecallLedgers).err = errors.New("tikv down")
	resp, err := BatchRecallItems(context.Background(), svcCtx, goldRecallReq(true))
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeSnapshotDBError, resp.ErrorCode)
	assert.Empty(t, resp.PlanDigest, "没有证据不出计划")
	assert.Empty(t, resp.Results)
	assert.Empty(t, recall.created)
}

// 要核已提交账本的玩家超过上限:每人一次账本读,整批拒绝,一份都不读。
func TestBatchRecallItems_TooManyPlayersToVerifyIsRejected(t *testing.T) {
	rows := make([]*store.TransactionLogRow, 0, maxRecallPlayers+1)
	for i := range maxRecallPlayers + 1 {
		rows = append(rows, &store.TransactionLogRow{TxID: uint64(i + 1), ToPlayer: uint64(10_000 + i), CurrencyDelta: 1, CorrelationID: uint64(20_000 + i)})
	}
	svcCtx, recall, _ := newRecallSvcCtx(t, rows)
	resp, err := BatchRecallItems(context.Background(), svcCtx, goldRecallReq(true))
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeResultTruncated, resp.ErrorCode)
	assert.Empty(t, svcCtx.PlayerStoreAssets.(*fakeRecallLedgers).reads)
	assert.Empty(t, recall.created)
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
	result, err := ResolveRecallOp(context.Background(), svcCtx, 1, applied, "tester", "理由")
	require.NoError(t, err)
	assert.False(t, result.Resolved)
	assert.Equal(t, constants.ErrCodeInvalidRequest, result.ErrorCode, "全额结果只能来自 scene 的明细")
}

// ── ResolveRecallOp 的证据 ─────────────────────────────────────

const (
	manualOpID     uint64 = 7001
	manualPlayerID uint64 = 9001

	manualStatusPending  = uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_PENDING)
	manualStatusApplied  = uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_APPLIED)
	manualStatusRejected = uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_REJECTED)
	manualStatusAborted  = uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_ABORTED)
	manualStatusPartial  = uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_PARTIAL)
	manualStatusPlanned  = uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_PLANNED)
)

// manualNow 是人工终结用例的固定时钟:判「安静」只看它,不看真实墙钟。
var manualNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func manualClock() time.Time { return manualNow }

// manualRecallStore 只模拟人工终结的两步:读行、带见证终结。真 SQL(行锁下比见证、批次计数、来源放回)在 store 的集成测试里。
type manualRecallStore struct {
	svc.RecallStore
	row      store.RecallOpState
	readErr  error
	resolved bool
	calls    []assetop.ManualResolution
	witness  []assetop.QuietWitness
}

func (f *manualRecallStore) ReadOpState(context.Context, uint64) (store.RecallOpState, error) {
	return f.row, f.readErr
}

func (f *manualRecallStore) ResolveManuallyIfQuiet(_ context.Context, r assetop.ManualResolution, witness assetop.QuietWitness, _ uint64) (bool, error) {
	f.calls = append(f.calls, r)
	f.witness = append(f.witness, witness)
	return f.resolved, nil
}

func manualRecallSvcCtx(recall *manualRecallStore, ledgers *fakeRecallLedgers) *svc.ServiceContext {
	return &svc.ServiceContext{Recall: recall, PlayerStoreAssets: ledgers}
}

// manualRecallRow 是读到的一行卡死的回收:PENDING,收回 100 银两,此刻没人持租约,两小时前最后一次投递。
func manualRecallRow() store.RecallOpState {
	op := assetop.Op{OpID: manualOpID, PlayerID: manualPlayerID, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_GM_RECALL,
		Seq: 3, StreamEpoch: 1_700_000_000_000, CorrelationID: manualOpID, TxType: store.RecallTxType,
		Bundle: &assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 0, Amount: 100}}}}
	op.LastSendMs = uint64(manualNow.Add(-2 * time.Hour).UnixMilli())
	return store.RecallOpState{Status: manualStatusPending, Op: op}
}

// committedRecall 造这名玩家已提交的账本:scene 把 res 和资产同一次提交,收据身份按 op 算。res 为 nil 时账本没见过这个 seq。
func committedRecall(t *testing.T, op assetop.Op, res *assetop.Result) *componentpb.PlayerAssetOpLedgerComp {
	t.Helper()
	ledger := &assetoptest.CommittedLedger{}
	if res != nil {
		require.NoError(t, ledger.Commit(op.Request(), *res))
	}
	got, err := ledger.ReadPersistedLedger(context.Background(), op.PlayerID)
	require.NoError(t, err)
	if got == nil {
		return &componentpb.PlayerAssetOpLedgerComp{}
	}
	return got
}

// manualLedgers 是 player_store 的写权行加账本,两者出自同一次读。
func manualLedgers(released bool, ledger *componentpb.PlayerAssetOpLedgerComp) *fakeRecallLedgers {
	return &fakeRecallLedgers{rows: map[uint64]*store.PlayerAssetRow{manualPlayerID: {Released: released,
		Player: &dbpb.PlayerDatabase{PlayerId: manualPlayerID, AssetOpLedger: ledger}}}}
}

// 证据 × 人工结局:回收自己的对表(recallManualAllows),每一格都走一遍完整路径。
// 放行的要带读行时的租约与发送时刻终结;不放行的回 39、不碰库,evidence 写明结论、来源和这条证据下能收什么。
func TestResolveRecallOp_EvidenceDecidesWhichFinalIsAccepted(t *testing.T) {
	recallLine := func(requested uint64) *assetpb.AssetSnapshot {
		return &assetpb.AssetSnapshot{RecallLines: []*assetpb.AssetRecallLine{{CurrencyType: 0, Requested: requested, Taken: 60, Debt: requested - 60}}}
	}
	applied := &assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true, Snapshot: recallLine(100)}
	partial := &assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true, Partial: true, Snapshot: recallLine(100)}
	rejected := &assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true, Reason: 27004}
	committed := func(released bool, res *assetop.Result) func(*testing.T, assetop.Op) *fakeRecallLedgers {
		return func(t *testing.T, op assetop.Op) *fakeRecallLedgers { return manualLedgers(released, committedRecall(t, op, res)) }
	}
	// 同一个 seq 上记的是另一笔请求(数额不同),收据摘要对不上。
	otherRequest := func(t *testing.T, op assetop.Op) *fakeRecallLedgers {
		other := op
		other.Bundle = &assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 0, Amount: 99}}}
		return manualLedgers(true, committedRecall(t, other, &assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED,
			Durable: true, Snapshot: recallLine(99)}))
	}
	// 窗口下沿 watermark=10:seq 3 已滑出窗口,结局不可知。
	slid := func(_ *testing.T, op assetop.Op) *fakeRecallLedgers {
		return manualLedgers(true, &componentpb.PlayerAssetOpLedgerComp{Streams: []*componentpb.AssetOpStreamLedger{{
			Stream: op.Stream, StreamEpoch: op.StreamEpoch, Watermark: 10, MaxSeq: 20,
			SeenBits: make([]uint64, assetop.WindowWords), AppliedBits: make([]uint64, assetop.WindowWords)}}})
	}
	noOwnerRow := func(*testing.T, assetop.Op) *fakeRecallLedgers { return &fakeRecallLedgers{} }
	row := func(edit func(*store.RecallOpState)) func() store.RecallOpState {
		return func() store.RecallOpState {
			r := manualRecallRow()
			if edit != nil {
				edit(&r)
			}
			return r
		}
	}

	cases := []struct {
		name     string
		row      func() store.RecallOpState
		ledgers  func(*testing.T, assetop.Op) *fakeRecallLedgers
		verdict  assetop.ManualVerdict
		accepted []uint32
	}{
		{"账本已应用,收据是这一条", row(nil), committed(true, applied), assetop.ManualVerdictApplied, nil},
		{"账本已部分应用", row(nil), committed(true, partial), assetop.ManualVerdictPartial, nil},
		{"行上带粘性的部分发放码", row(func(r *store.RecallOpState) { r.Op.LastReason = assetop.ReasonPartialApplied }), committed(true, applied),
			assetop.ManualVerdictPartial, nil},
		{"账本已拒绝,收据是这一条", row(nil), committed(false, rejected), assetop.ManualVerdictRejected, []uint32{manualStatusRejected, manualStatusAborted}},
		{"已释放、账本没见过、行安静", row(nil), committed(true, nil), assetop.ManualVerdictNeverApplied, []uint32{manualStatusAborted}},
		{"账本没见过,玩家还被 scene 持有", row(nil), committed(false, nil), assetop.ManualVerdictNone, []uint32{manualStatusPartial}},
		{"已释放、没见过,但此刻有人持租约", row(func(r *store.RecallOpState) { r.LeaseUntilMs = uint64(manualNow.Add(5 * time.Second).UnixMilli()) }),
			committed(true, nil), assetop.ManualVerdictNone, []uint32{manualStatusPartial}},
		{"已释放、没见过,但一分钟前刚投递过", row(func(r *store.RecallOpState) { r.Op.LastSendMs = uint64(manualNow.Add(-time.Minute).UnixMilli()) }),
			committed(true, nil), assetop.ManualVerdictNone, []uint32{manualStatusPartial}},
		// last_send_ms = 0 是「Store 没维护」:派发已写初值,只有改库或契约落地前派发的存量行才会是 0,按不安静处理。
		{"已释放、没见过,但 last_send_ms 为 0", row(func(r *store.RecallOpState) { r.Op.LastSendMs = 0 }),
			committed(true, nil), assetop.ManualVerdictNone, []uint32{manualStatusPartial}},
		{"滑出账本窗口", row(nil), slid, assetop.ManualVerdictNone, []uint32{manualStatusPartial}},
		{"同一个 seq 上是别的请求的收据", row(nil), otherRequest, assetop.ManualVerdictNone, []uint32{manualStatusPartial}},
		{"没有写权行", row(nil), noOwnerRow, assetop.ManualVerdictNone, []uint32{manualStatusPartial}},
		{"载荷解不开,还原不出请求", row(func(r *store.RecallOpState) { r.Op.Bundle, r.OpErr = nil, errors.New("decode payload") }),
			committed(true, nil), assetop.ManualVerdictNone, []uint32{manualStatusPartial}},
	}
	for _, c := range cases {
		for _, final := range []uint32{manualStatusRejected, manualStatusAborted, manualStatusPartial} {
			t.Run(c.name+"/"+store.RecallStatusLabel(final), func(t *testing.T) {
				r := c.row()
				recall := &manualRecallStore{row: r, resolved: true}
				got, err := resolveRecallOp(context.Background(), manualRecallSvcCtx(recall, c.ledgers(t, r.Op)), manualOpID, final, "tester", "工单 123", manualClock)
				require.NoError(t, err)
				assert.Contains(t, got.Evidence, "evidence="+c.verdict.String()+" ")
				assert.Contains(t, got.Evidence, "source: committed asset ledger")
				if !slices.Contains(c.accepted, final) {
					assert.Equal(t, constants.ErrCodeRecallEvidenceRefused, got.ErrorCode)
					assert.False(t, got.Resolved)
					assert.Equal(t, manualStatusPending, got.Status)
					assert.Empty(t, recall.calls, "证据不支持就不终结")
					labels := make([]string, 0, len(c.accepted))
					for _, status := range c.accepted {
						labels = append(labels, store.RecallStatusLabel(status))
					}
					wantAccepts := strings.Join(labels, "|")
					if wantAccepts == "" {
						wantAccepts = "none ("
					}
					assert.Contains(t, got.Evidence, "recall_accepts="+wantAccepts)
					return
				}
				assert.Equal(t, constants.ErrCodeOK, got.ErrorCode)
				assert.True(t, got.Resolved)
				assert.Equal(t, final, got.Status)
				require.Len(t, recall.calls, 1)
				wantFinal, _ := store.RecallManualFinal(final)
				assert.Equal(t, wantFinal, recall.calls[0].Final)
				assert.Equal(t, manualOpID, recall.calls[0].OpID)
				assert.Equal(t, assetop.QuietWitness{LeaseUntilMs: r.LeaseUntilMs, LastSendMs: r.Op.LastSendMs}, recall.witness[0],
					"终结带读行时的租约与发送时刻")
			})
		}
	}
}

// 回收的人工结局对表:全额 APPLIED 与放弃任何证据下都不收,其余见 recallManualAllows 的注释。
func TestRecallManualAllows(t *testing.T) {
	accepted := map[assetop.ManualVerdict][]assetop.Status{
		assetop.ManualVerdictRejected:     {assetop.StatusRejected, assetop.StatusAborted},
		assetop.ManualVerdictNeverApplied: {assetop.StatusAborted},
		assetop.ManualVerdictNone:         {assetop.StatusAppliedPartial},
		assetop.ManualVerdictApplied:      nil,
		assetop.ManualVerdictPartial:      nil,
	}
	finals := []assetop.Status{assetop.StatusApplied, assetop.StatusRejected, assetop.StatusAborted, assetop.StatusAppliedPartial, assetop.StatusAbandoned}
	for verdict, want := range accepted {
		for _, final := range finals {
			assert.Equal(t, slices.Contains(want, final), recallManualAllows(verdict, final), "%s × %s", verdict, final)
		}
	}
}

// 行已不是 PENDING:回现状,不读账本、不终结。还没派发的(PLANNED)同样不收,撤它用 CancelRecallBatch。
func TestResolveRecallOp_NotPendingReturnsTheCurrentStatus(t *testing.T) {
	for _, status := range []uint32{manualStatusApplied, manualStatusAborted, manualStatusPartial, manualStatusPlanned} {
		t.Run(store.RecallStatusLabel(status), func(t *testing.T) {
			r := manualRecallRow()
			r.Status = status
			recall := &manualRecallStore{row: r, resolved: true}
			ledgers := &fakeRecallLedgers{}
			got, err := resolveRecallOp(context.Background(), manualRecallSvcCtx(recall, ledgers), manualOpID, manualStatusAborted, "tester", "工单 123", manualClock)
			require.NoError(t, err)
			assert.Equal(t, constants.ErrCodeOK, got.ErrorCode)
			assert.False(t, got.Resolved)
			assert.Equal(t, status, got.Status)
			assert.Empty(t, got.Evidence)
			assert.Empty(t, ledgers.reads, "不是 PENDING 不读账本")
			assert.Empty(t, recall.calls)
		})
	}
}

// 读不到已提交账本就没有证据:回故障码,不终结(fail-closed)。连回收特有的保守关单 PARTIAL 也不收。
func TestResolveRecallOp_LedgerReadFailureFailsClosed(t *testing.T) {
	recall := &manualRecallStore{row: manualRecallRow(), resolved: true}
	ledgers := &fakeRecallLedgers{err: errors.New("tikv down")}
	got, err := resolveRecallOp(context.Background(), manualRecallSvcCtx(recall, ledgers), manualOpID, manualStatusPartial, "tester", "工单 123", manualClock)
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeSnapshotDBError, got.ErrorCode)
	assert.False(t, got.Resolved)
	assert.Empty(t, recall.calls)
	assert.Equal(t, []uint64{manualPlayerID}, ledgers.reads)
}

// 读证据之后这一行被领过、发过或已终结:带见证的结算落空,回 40,什么都没改,重新调用即重新读证据。
func TestResolveRecallOp_ChangedAfterEvidenceIsNotResolved(t *testing.T) {
	r := manualRecallRow()
	recall := &manualRecallStore{row: r, resolved: false}
	got, err := resolveRecallOp(context.Background(), manualRecallSvcCtx(recall, manualLedgers(true, committedRecall(t, r.Op, nil))),
		manualOpID, manualStatusAborted, "tester", "工单 123", manualClock)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeRecallOpChanged, got.ErrorCode)
	assert.False(t, got.Resolved)
	assert.Equal(t, manualStatusPending, got.Status)
	assert.Contains(t, got.Evidence, "evidence=never_applied")
	require.Len(t, recall.calls, 1)
}

// 没有这一行回 34;读行失败是故障,不往下走。
func TestResolveRecallOp_RowReadErrors(t *testing.T) {
	recall := &manualRecallStore{readErr: store.ErrRecallOpNotFound}
	ledgers := &fakeRecallLedgers{}
	got, err := resolveRecallOp(context.Background(), manualRecallSvcCtx(recall, ledgers), manualOpID, manualStatusAborted, "tester", "工单 123", manualClock)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeRecallNotFound, got.ErrorCode)

	recall.readErr = errors.New("connection reset")
	got, err = resolveRecallOp(context.Background(), manualRecallSvcCtx(recall, ledgers), manualOpID, manualStatusAborted, "tester", "工单 123", manualClock)
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeSnapshotDBError, got.ErrorCode)
	assert.Empty(t, recall.calls)
	assert.Empty(t, ledgers.reads)
}
