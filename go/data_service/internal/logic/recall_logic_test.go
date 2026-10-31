package logic

import (
	"context"
	"errors"
	"testing"

	"data_service/internal/constants"
	"data_service/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestBatchRecallItems_NonDryRunRequiresAuditStore(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	svcCtx.TxLogStore = &fakeTransactionLogStore{}
	svcCtx.SnapshotStore = nil

	resp, err := BatchRecallItems(context.Background(), svcCtx, &BatchRecallReq{
		TimeStart:    1,
		TimeEnd:      2,
		ItemConfigID: 10,
		Operator:     "tester",
	})
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeSnapshotDBError, resp.ErrorCode)
}

func TestBatchRecallItems_NonDryRunFailsClosedAndAuditsZeroAffected(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	svcCtx.TxLogStore = &fakeTransactionLogStore{rows: []*store.TransactionLogRow{{
		ToPlayer:     9000,
		ItemConfigID: 10,
		ItemQuantity: 3,
	}}}
	snapshots := &fakeSnapshotStore{}
	svcCtx.SnapshotStore = snapshots

	resp, err := BatchRecallItems(context.Background(), svcCtx, &BatchRecallReq{
		TimeStart:    1,
		TimeEnd:      2,
		ItemConfigID: 10,
		Operator:     "tester",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeNotImplemented, resp.ErrorCode)
	assert.Equal(t, uint32(1), resp.TotalMatched)
	assert.Zero(t, resp.TotalRecalled)
	assert.Equal(t, uint32(1), resp.TotalFailed)
	require.Len(t, resp.Results, 1)
	assert.False(t, resp.Results[0].Success)
	require.Len(t, snapshots.audits, 1)
	assert.Zero(t, snapshots.audits[0].PlayersAffected)
	assert.Equal(t, uint32(1), snapshots.audits[0].PlayersFailed)
}

func TestBatchRecallItems_DryRunTruncationFailsClosedWithCandidateTotal(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	txLog := &fakeTransactionLogStore{
		rows: []*store.TransactionLogRow{{
			ToPlayer:     9002,
			ItemConfigID: 10,
			ItemQuantity: 3,
		}},
		total:    10001,
		totalSet: true,
	}
	snapshots := &fakeSnapshotStore{}
	svcCtx.TxLogStore = txLog
	svcCtx.SnapshotStore = snapshots

	resp, err := BatchRecallItems(context.Background(), svcCtx, &BatchRecallReq{
		TimeStart:    1,
		TimeEnd:      2,
		ItemConfigID: 10,
		Operator:     "tester",
		Reason:       "truncation test",
		DryRun:       true,
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeResultTruncated, resp.ErrorCode)
	assert.Equal(t, uint32(10001), resp.TotalMatched)
	assert.Equal(t, uint32(10001), resp.TotalFailed)
	assert.Zero(t, resp.TotalRecalled)
	assert.Empty(t, resp.Results)
	require.Len(t, txLog.queries, 1)
	assert.Equal(t, batchRecallQueryLimit, txLog.queries[0].Limit)
	require.Len(t, snapshots.audits, 1)
	assert.Zero(t, snapshots.audits[0].PlayersAffected)
	assert.Contains(t, snapshots.audits[0].Reason, "TRUNCATED")
	assert.Contains(t, snapshots.audits[0].Reason, "candidate_total=10001")
}

func TestBatchRecallItems_PlayerQueryFailureCannotBeSkipped(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	svcCtx.TxLogStore = &fakeTransactionLogStore{err: errors.New("query unavailable")}
	svcCtx.SnapshotStore = &fakeSnapshotStore{}

	resp, err := BatchRecallItems(context.Background(), svcCtx, &BatchRecallReq{
		PlayerIDs:    []uint64{1, 2},
		TimeStart:    1,
		TimeEnd:      2,
		ItemConfigID: 10,
		DryRun:       true,
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "player 1")
	assert.Equal(t, constants.ErrCodeSnapshotDBError, resp.ErrorCode)
}

func TestBatchRecallItems_AuditFailureIsReturnedToCaller(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	svcCtx.TxLogStore = &fakeTransactionLogStore{rows: []*store.TransactionLogRow{{
		ToPlayer:     9001,
		ItemConfigID: 10,
		ItemQuantity: 3,
	}}}
	svcCtx.SnapshotStore = &fakeSnapshotStore{auditErr: errors.New("audit unavailable")}

	resp, err := BatchRecallItems(context.Background(), svcCtx, &BatchRecallReq{
		TimeStart:    1,
		TimeEnd:      2,
		ItemConfigID: 10,
		Operator:     "tester",
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "write batch recall audit")
	assert.Equal(t, constants.ErrCodeSnapshotDBError, resp.ErrorCode)
	assert.Equal(t, uint32(1), resp.TotalMatched)
	assert.Equal(t, uint32(1), resp.TotalFailed)
	assert.Zero(t, resp.TotalRecalled)
}
