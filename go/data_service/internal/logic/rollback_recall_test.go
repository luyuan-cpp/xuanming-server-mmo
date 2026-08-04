package logic

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"data_service/internal/constants"
	"data_service/internal/store"
	"data_service/internal/svc"

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
}

func (f *fakeSnapshotStore) Close() error { return nil }
func (f *fakeSnapshotStore) InsertSnapshot(_ context.Context, row *store.SnapshotRow) (uint64, error) {
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
func (f *fakeSnapshotStore) InsertAuditLog(_ context.Context, row *store.AuditLogRow) error {
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
func (f *fakeTransactionLogStore) QueryLog(_ context.Context, query *store.TransactionLogQuery) ([]*store.TransactionLogRow, uint32, error) {
	queryCopy := *query
	f.queries = append(f.queries, &queryCopy)
	total := uint32(len(f.rows))
	if f.totalSet {
		total = f.total
	}
	return append([]*store.TransactionLogRow(nil), f.rows...), total, f.err
}

type fakeRollbackFence struct {
	playerErr      error
	zoneErr        error
	serverErr      error
	playerAcquired int
	zoneAcquired   int
	serverAcquired int
	playerReleased int
	zoneReleased   int
	serverReleased int
}

func (f *fakeRollbackFence) AcquirePlayer(context.Context, uint64) (func(), error) {
	f.playerAcquired++
	if f.playerErr != nil {
		return nil, f.playerErr
	}
	return func() { f.playerReleased++ }, nil
}

func (f *fakeRollbackFence) AcquireZone(context.Context, uint32) (func(), error) {
	f.zoneAcquired++
	if f.zoneErr != nil {
		return nil, f.zoneErr
	}
	return func() { f.zoneReleased++ }, nil
}

func (f *fakeRollbackFence) AcquireServer(context.Context, []uint32) (func(), error) {
	f.serverAcquired++
	if f.serverErr != nil {
		return nil, f.serverErr
	}
	return func() { f.serverReleased++ }, nil
}

func mustSnapshotData(t *testing.T, fields map[string][]byte) []byte {
	t.Helper()
	blob, err := json.Marshal(&snapshotData{Fields: fields})
	require.NoError(t, err)
	return blob
}

func TestRollbackZone_NoSnapshotsStillReportsCurrentPlayers(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 801, 9)
	setupPlayer(t, svcCtx, 802, 9)
	snapshots := &fakeSnapshotStore{}
	svcCtx.SnapshotStore = snapshots
	svcCtx.RollbackFence = &fakeRollbackFence{}

	resp, err := RollbackZone(ctx, svcCtx, &RollbackZoneReq{
		ZoneID:     9,
		TargetTime: 123456,
		Reason:     "test",
		Operator:   "tester",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, resp.ErrorCode)
	assert.ElementsMatch(t, []uint64{801, 802}, resp.OrphanPlayerIDs)
	assert.Zero(t, resp.PlayersAffected)
	assert.Zero(t, resp.OrphansCleaned)
	require.Len(t, snapshots.audits, 2)
	assert.True(t, strings.HasPrefix(snapshots.audits[0].Reason, "STARTED:"))
	assert.Zero(t, snapshots.audits[1].OrphansCleaned)
	for _, playerID := range []uint64{801, 802} {
		zoneID, lookupErr := svcCtx.Router.GetPlayerHomeZone(ctx, playerID)
		require.NoError(t, lookupErr)
		assert.Equal(t, uint32(9), zoneID, "candidate reporting must not delete the player mapping")
	}
}

func TestRollbackPlayer_WithoutCrossServiceFenceDoesNotMutate(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 810, 9)
	setResp, err := SetPlayerField(ctx, svcCtx, 810, "gold", []byte("current"), 0)
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, setResp.ErrorCode)
	svcCtx.SnapshotStore = &fakeSnapshotStore{snapshotByID: map[uint64]*store.SnapshotRow{
		1: {ID: 1, PlayerID: 810, Data: mustSnapshotData(t, map[string][]byte{"gold": []byte("old")})},
	}}

	resp, err := RollbackPlayer(ctx, svcCtx, &RollbackPlayerReq{PlayerID: 810, SnapshotID: 1})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeNotImplemented, resp.ErrorCode)
	value, getErr := GetPlayerField(ctx, svcCtx, 810, "gold")
	require.NoError(t, getErr)
	assert.Equal(t, []byte("current"), value)
	assert.Empty(t, svcCtx.SnapshotStore.(*fakeSnapshotStore).audits)
}

func TestRollbackPlayer_OnlineFenceFailureDoesNotMutate(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 811, 9)
	setResp, err := SetPlayerField(ctx, svcCtx, 811, "gold", []byte("current"), 0)
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, setResp.ErrorCode)
	snapshots := &fakeSnapshotStore{snapshotByID: map[uint64]*store.SnapshotRow{
		1: {ID: 1, PlayerID: 811, Data: mustSnapshotData(t, map[string][]byte{"gold": []byte("old")})},
	}}
	svcCtx.SnapshotStore = snapshots
	svcCtx.RollbackFence = &fakeRollbackFence{playerErr: svc.ErrRollbackTargetOnline}

	resp, err := RollbackPlayer(ctx, svcCtx, &RollbackPlayerReq{PlayerID: 811, SnapshotID: 1})
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodePlayerOnline, resp.ErrorCode)
	value, getErr := GetPlayerField(ctx, svcCtx, 811, "gold")
	require.NoError(t, getErr)
	assert.Equal(t, []byte("current"), value)
	assert.Empty(t, snapshots.audits)
}

func TestRollbackPlayer_SafetySnapshotFailureStopsBeforeOverwrite(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 812, 9)
	setResp, err := SetPlayerField(ctx, svcCtx, 812, "gold", []byte("current"), 0)
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, setResp.ErrorCode)
	snapshots := &fakeSnapshotStore{
		snapshotByID: map[uint64]*store.SnapshotRow{
			1: {ID: 1, PlayerID: 812, Data: mustSnapshotData(t, map[string][]byte{"gold": []byte("old")})},
		},
		insertErr: errors.New("snapshot unavailable"),
	}
	fence := &fakeRollbackFence{}
	svcCtx.SnapshotStore = snapshots
	svcCtx.RollbackFence = fence

	resp, err := RollbackPlayer(ctx, svcCtx, &RollbackPlayerReq{PlayerID: 812, SnapshotID: 1})
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeSnapshotDBError, resp.ErrorCode)
	value, getErr := GetPlayerField(ctx, svcCtx, 812, "gold")
	require.NoError(t, getErr)
	assert.Equal(t, []byte("current"), value)
	assert.Equal(t, 1, snapshots.insertCalls)
	require.Len(t, snapshots.audits, 2)
	assert.Equal(t, 1, fence.playerAcquired)
	assert.Equal(t, 1, fence.playerReleased)
}

func TestRollbackPlayer_IntentAuditFailureStopsBeforeSafetySnapshot(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 813, 9)
	setResp, err := SetPlayerField(ctx, svcCtx, 813, "gold", []byte("current"), 0)
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, setResp.ErrorCode)
	snapshots := &fakeSnapshotStore{
		snapshotByID: map[uint64]*store.SnapshotRow{
			1: {ID: 1, PlayerID: 813, Data: mustSnapshotData(t, map[string][]byte{"gold": []byte("old")})},
		},
		auditErr: errors.New("audit unavailable"),
	}
	svcCtx.SnapshotStore = snapshots
	svcCtx.RollbackFence = &fakeRollbackFence{}

	resp, err := RollbackPlayer(ctx, svcCtx, &RollbackPlayerReq{PlayerID: 813, SnapshotID: 1})
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeSnapshotDBError, resp.ErrorCode)
	assert.Zero(t, snapshots.insertCalls)
	value, getErr := GetPlayerField(ctx, svcCtx, 813, "gold")
	require.NoError(t, getErr)
	assert.Equal(t, []byte("current"), value)
}

func TestRollbackPlayer_ResultAuditFailureIsReturned(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	ctx := context.Background()
	setupPlayer(t, svcCtx, 814, 9)
	setResp, err := SetPlayerField(ctx, svcCtx, 814, "gold", []byte("current"), 0)
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, setResp.ErrorCode)
	snapshots := &fakeSnapshotStore{
		snapshotByID: map[uint64]*store.SnapshotRow{
			1: {ID: 1, PlayerID: 814, Data: mustSnapshotData(t, map[string][]byte{"gold": []byte("old")})},
		},
		auditErrs: []error{nil, errors.New("result audit unavailable")},
	}
	svcCtx.SnapshotStore = snapshots
	svcCtx.RollbackFence = &fakeRollbackFence{}

	resp, err := RollbackPlayer(ctx, svcCtx, &RollbackPlayerReq{PlayerID: 814, SnapshotID: 1})
	require.Error(t, err)
	assert.ErrorContains(t, err, "result audit")
	assert.Equal(t, constants.ErrCodeSnapshotDBError, resp.ErrorCode)
	value, getErr := GetPlayerField(ctx, svcCtx, 814, "gold")
	require.NoError(t, getErr)
	assert.Equal(t, []byte("old"), value, "the caller must see the audit failure even after the fenced write completed")
}

func TestRollbackPlayer_MissingSnapshotStoreReturnsErrorInsteadOfPanicking(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	svcCtx.SnapshotStore = nil

	resp, err := RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{PlayerID: 815, SnapshotID: 1})
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeSnapshotDBError, resp.ErrorCode)
}

func TestRollbackAll_MissingSnapshotStoreReturnsErrorInsteadOfPanicking(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	svcCtx.SnapshotStore = nil

	resp, err := RollbackAll(context.Background(), svcCtx, &RollbackAllReq{TargetTime: 1})
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeSnapshotDBError, resp.ErrorCode)
}

func TestCreatePlayerSnapshot_MissingStoreReturnsErrorInsteadOfPanicking(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	svcCtx.SnapshotStore = nil

	resp, err := CreatePlayerSnapshot(context.Background(), svcCtx, &CreateSnapshotReq{PlayerID: 816})
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeSnapshotDBError, resp.ErrorCode)
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
