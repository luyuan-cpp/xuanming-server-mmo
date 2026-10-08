package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"data_service/internal/config"
	"data_service/internal/constants"
	"data_service/internal/guildcheck"
	"data_service/internal/routing"
	"data_service/internal/store"
	"data_service/internal/svc"

	guildpb "proto/guild"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/logx/logtest"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	enableGuildGate(svcCtx, cleanGuildChecker())

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
	enableGuildGate(svcCtx, cleanGuildChecker())

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
	enableGuildGate(svcCtx, cleanGuildChecker())

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

// ── 帮会资产闸(docs/design/guild-phase2/07-rollback-fail-closed.md §7.10.2 D2、D7–D14、D17–D18)────────
//
// 全部经真实接缝注入:svc.ServiceContext.GuildDivergence(fake checker,或真实 guildcheck 包一层 fake RPC 客户端)、
// svc.ServiceContext.GuildCheckSleep(记录等待、不真睡)。玩家数据写在 miniredis,"零写入"用两条证据判:
// Redis 里的值没变 + 回档前安全快照一次都没打(insertCalls == 0;安全快照是每个玩家的第一笔写)。

// fakeGuildChecker 按调用序号作答;记下每次调用的 since 与 ctx。
type fakeGuildChecker struct {
	answer func(n int, ctx context.Context, since map[uint64]uint64) (guildcheck.GuildCheckResult, error)
	since  []map[uint64]uint64
	ctxs   []context.Context
	// ctxErrs 是**调用那一刻**的 ctx.Err():调用方返回后它自己的 defer cancel() 会把 ctx 取消掉,事后再看没有意义。
	ctxErrs []error
}

func (f *fakeGuildChecker) ListDivergences(ctx context.Context, since map[uint64]uint64) (guildcheck.GuildCheckResult, error) {
	cp := make(map[uint64]uint64, len(since))
	for k, v := range since {
		cp[k] = v
	}
	f.since = append(f.since, cp)
	f.ctxs = append(f.ctxs, ctx)
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	return f.answer(len(f.since)-1, ctx, since)
}

func cleanGuildChecker() *fakeGuildChecker {
	return &fakeGuildChecker{answer: func(int, context.Context, map[uint64]uint64) (guildcheck.GuildCheckResult, error) {
		return guildcheck.GuildCheckResult{}, nil
	}}
}

// staticGuildChecker 第一次(检查阶段)答 first,之后(写后复查)都答 recheck。
func staticGuildChecker(first guildcheck.GuildCheckResult, recheck guildcheck.GuildCheckResult) *fakeGuildChecker {
	return &fakeGuildChecker{answer: func(n int, _ context.Context, _ map[uint64]uint64) (guildcheck.GuildCheckResult, error) {
		if n == 0 {
			return first, nil
		}
		return recheck, nil
	}}
}

// sleepRecorder 替代真实等待:记下每次等多久,hook 可以在第 n 次等待时做点什么(取消入口 ctx、翻转 fake 状态)。
type sleepRecorder struct {
	durations []time.Duration
	ctxErrs   []error
	hook      func(n int, ctx context.Context) error
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	n := len(s.durations)
	s.durations = append(s.durations, d)
	if s.hook != nil {
		if err := s.hook(n, ctx); err != nil {
			return err
		}
	}
	s.ctxErrs = append(s.ctxErrs, ctx.Err())
	return ctx.Err()
}

// enableGuildGate 装上帮会闸:合法的余量 / 预算(文档默认值)、给定的 checker、不真睡的等待。
func enableGuildGate(svcCtx *svc.ServiceContext, checker guildcheck.GuildDivergenceChecker) *sleepRecorder {
	svcCtx.Config.GuildClockSkewMarginMs = 300000
	svcCtx.Config.GuildCheckBudgetSeconds = 120
	svcCtx.GuildDivergence = checker
	rec := &sleepRecorder{}
	svcCtx.GuildCheckSleep = rec.sleep
	return rec
}

const gateSnapshotCreatedAt uint64 = 1_700_000_000

// newGateTestPlayer:玩家当前 gold = "current",快照 1(created_at = gateSnapshotCreatedAt)里 gold = "old",
// 栅栏可用。帮会闸由各用例自己装。
func newGateTestPlayer(t *testing.T, playerID uint64) (*svc.ServiceContext, *fakeSnapshotStore, *fakeRollbackFence) {
	t.Helper()
	svcCtx, _ := newTestSvcCtx(t)
	setupPlayer(t, svcCtx, playerID, 9)
	setResp, err := SetPlayerField(context.Background(), svcCtx, playerID, "gold", []byte("current"), 0)
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, setResp.ErrorCode)
	snapshots := &fakeSnapshotStore{snapshotByID: map[uint64]*store.SnapshotRow{
		1: {ID: 1, PlayerID: playerID, CreatedAt: gateSnapshotCreatedAt, Data: mustSnapshotData(t, map[string][]byte{"gold": []byte("old")})},
	}}
	fence := &fakeRollbackFence{}
	svcCtx.SnapshotStore = snapshots
	svcCtx.RollbackFence = fence
	return svcCtx, snapshots, fence
}

func goldOf(t *testing.T, svcCtx *svc.ServiceContext, playerID uint64) string {
	t.Helper()
	v, err := GetPlayerField(context.Background(), svcCtx, playerID, "gold")
	require.NoError(t, err)
	return string(v)
}

func divergenceRows(playerID uint64, opIDs ...uint64) []guildcheck.GuildDivergence {
	rows := make([]guildcheck.GuildDivergence, 0, len(opIDs))
	for _, op := range opIDs {
		rows = append(rows, guildcheck.GuildDivergence{
			OpID:       op,
			PlayerID:   playerID,
			GuildID:    77,
			Kind:       uint32(guildpb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE),
			Status:     uint32(guildpb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED),
			FundsDelta: 100,
			UpdatedMs:  gateSnapshotCreatedAt*1000 + op,
		})
	}
	return rows
}

func codeTag(code uint32) string { return fmt.Sprintf("code=%d", code) }

// D2:GuildDivergence == nil → CheckFailed、零写入;nil 不是"跳过检查"(与 LoginAdminClient 的先例方向相反)。
// 配置非法(余量 0)同样拒绝,且 checker 一次都不被调用。两种都不进沉降等待。
func TestRollbackGuildGate_D2_NilCheckerRejectsWithoutWriting(t *testing.T) {
	const pid = 820
	svcCtx, snapshots, _ := newGateTestPlayer(t, pid)
	rec := &sleepRecorder{}
	svcCtx.GuildCheckSleep = rec.sleep
	svcCtx.Config.GuildClockSkewMarginMs = 300000
	svcCtx.Config.GuildCheckBudgetSeconds = 120

	resp, err := RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{PlayerID: pid, SnapshotID: 1, Operator: "ops", Reason: "d2"})
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeRollbackGuildCheckFailed, resp.ErrorCode)
	assert.Equal(t, "current", goldOf(t, svcCtx, pid))
	assert.Zero(t, snapshots.insertCalls, "零写入:连回档前安全快照都不许打")
	assert.Empty(t, rec.durations, "预检失败不该先白等 30s")
	require.Len(t, snapshots.audits, 2)
	assert.True(t, strings.HasPrefix(snapshots.audits[0].Reason, "STARTED:"))
	assert.Contains(t, snapshots.audits[1].Reason, codeTag(constants.ErrCodeRollbackGuildCheckFailed))

	// 放行开关对"问不到"无效。
	resp, _ = RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{
		PlayerID: pid, SnapshotID: 1, Operator: "ops", Reason: "d2", AcceptGuildDivergence: true,
	})
	assert.Equal(t, constants.ErrCodeRollbackGuildCheckFailed, resp.ErrorCode)
	assert.Equal(t, "current", goldOf(t, svcCtx, pid))

	checker := cleanGuildChecker()
	enableGuildGate(svcCtx, checker)
	svcCtx.Config.GuildClockSkewMarginMs = 0
	resp, _ = RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{PlayerID: pid, SnapshotID: 1})
	assert.Equal(t, constants.ErrCodeRollbackGuildCheckFailed, resp.ErrorCode)
	assert.Empty(t, checker.since, "非法余量不能拿去做检查")
	assert.Equal(t, "current", goldOf(t, svcCtx, pid))
	assert.Zero(t, snapshots.insertCalls)
}

// D7:有分歧、未放行 → Divergence + 真实计数 + ≤20 条按 op_id 升序的样本;零写入;审计 STARTED + 带码与条数的 RESULT。
// 检查发生在一次 30s 沉降之后;since = 快照时刻 ×1000 − 余量。
func TestRollbackGuildGate_D7_DivergenceRejectsWithoutWriting(t *testing.T) {
	const pid = 821
	svcCtx, snapshots, _ := newGateTestPlayer(t, pid)
	ops := make([]uint64, 0, 25)
	for op := uint64(1); op <= 25; op++ {
		ops = append(ops, op)
	}
	checker := staticGuildChecker(guildcheck.GuildCheckResult{Divergences: divergenceRows(pid, ops...)}, guildcheck.GuildCheckResult{})
	rec := enableGuildGate(svcCtx, checker)

	resp, err := RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{PlayerID: pid, SnapshotID: 1, Operator: "ops", Reason: "d7"})
	require.NoError(t, err, "规则拒绝不返回 err,否则计数与样本带不出去")
	assert.Equal(t, constants.ErrCodeRollbackGuildDivergence, resp.ErrorCode)
	assert.Equal(t, uint32(25), resp.GuildDivergenceCount)
	require.Len(t, resp.GuildDivergences, maxDivergenceSample)
	for i, d := range resp.GuildDivergences {
		assert.Equal(t, uint64(i+1), d.OpID)
	}
	assert.Zero(t, resp.GuildUnprovablePlayerCount)

	assert.Equal(t, "current", goldOf(t, svcCtx, pid))
	assert.Zero(t, snapshots.insertCalls)
	require.Len(t, snapshots.audits, 2)
	assert.True(t, strings.HasPrefix(snapshots.audits[0].Reason, "STARTED:"))
	assert.Contains(t, snapshots.audits[1].Reason, codeTag(constants.ErrCodeRollbackGuildDivergence))
	assert.Contains(t, snapshots.audits[1].Reason, "guild_divergences=25")

	assert.Equal(t, []time.Duration{guildSettleDelay}, rec.durations, "只沉降一次,不做写后复查")
	require.Len(t, checker.since, 1)
	assert.Equal(t, map[uint64]uint64{pid: gateSnapshotCreatedAt*1000 - 300000}, checker.since[0])
}

// R6:ROLLBACK_PARTIAL 同样过闸。
func TestRollbackGuildGate_PartialScopeIsGatedToo(t *testing.T) {
	const pid = 822
	svcCtx, snapshots, _ := newGateTestPlayer(t, pid)
	enableGuildGate(svcCtx, staticGuildChecker(guildcheck.GuildCheckResult{Divergences: divergenceRows(pid, 7)}, guildcheck.GuildCheckResult{}))

	resp, err := RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{PlayerID: pid, SnapshotID: 1, Scope: 1, Fields: []string{"gold"}})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeRollbackGuildDivergence, resp.ErrorCode)
	assert.Equal(t, "current", goldOf(t, svcCtx, pid))
	assert.Zero(t, snapshots.insertCalls)
}

// D8(logic 半边):放行缺 reason / operator → InvalidRequest,在栅栏与任何审计之前。
// x-admin-token 那半边在 server 层(dataserviceserver_test.go TestRollbackRPCs_RequireAdminToken)。
func TestRollbackGuildGate_D8_AcceptRequiresReasonAndOperator(t *testing.T) {
	const pid = 823
	svcCtx, snapshots, fence := newGateTestPlayer(t, pid)
	enableGuildGate(svcCtx, cleanGuildChecker())

	for _, req := range []*RollbackPlayerReq{
		{PlayerID: pid, SnapshotID: 1, AcceptGuildDivergence: true, Operator: "ops"},
		{PlayerID: pid, SnapshotID: 1, AcceptGuildDivergence: true, Reason: "why"},
		{PlayerID: pid, SnapshotID: 1, AcceptGuildDivergence: true, Reason: "  ", Operator: "ops"},
	} {
		resp, err := RollbackPlayer(context.Background(), svcCtx, req)
		require.NoError(t, err)
		assert.Equal(t, constants.ErrCodeInvalidRequest, resp.ErrorCode)
	}
	zoneResp, err := RollbackZone(context.Background(), svcCtx, &RollbackZoneReq{ZoneID: 9, TargetTime: 1, AcceptGuildDivergence: true})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeInvalidRequest, zoneResp.ErrorCode)
	allResp, err := RollbackAll(context.Background(), svcCtx, &RollbackAllReq{TargetTime: 1, AcceptGuildDivergence: true})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeInvalidRequest, allResp.ErrorCode)

	assert.Zero(t, fence.playerAcquired+fence.zoneAcquired+fence.serverAcquired, "参数不全的放行不该挡任何人登录")
	assert.Empty(t, snapshots.audits)
	assert.Equal(t, "current", goldOf(t, svcCtx, pid))
}

// D9:合法放行 → ACCEPTED 审计与逐行 ERROR 日志都在第一笔写(安全快照)之前;写发生;审计顺序 STARTED → ACCEPTED → RESULT。
func TestRollbackGuildGate_D9_AcceptedAuditAndLogsPrecedeFirstWrite(t *testing.T) {
	const pid = 824
	logs := logtest.NewCollector(t)
	svcCtx, snapshots, _ := newGateTestPlayer(t, pid)
	rows := divergenceRows(pid, 5, 6)
	rec := enableGuildGate(svcCtx, staticGuildChecker(guildcheck.GuildCheckResult{Divergences: rows}, guildcheck.GuildCheckResult{Divergences: rows}))

	var goldAtAccepted string
	insertsAtAccepted := -1
	snapshots.onAudit = func(row *store.AuditLogRow) {
		if strings.HasPrefix(row.Reason, "GUILD_DIVERGENCE_ACCEPTED") {
			goldAtAccepted = goldOf(t, svcCtx, pid)
			insertsAtAccepted = snapshots.insertCalls
		}
	}
	var logsAtFirstWrite string
	snapshots.onInsert = func() {
		if logsAtFirstWrite == "" {
			logsAtFirstWrite = logs.String()
		}
	}

	resp, err := RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{
		PlayerID: pid, SnapshotID: 1, Operator: "ops-alice", Reason: "incident-42", AcceptGuildDivergence: true, Caller: "peer=test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, resp.ErrorCode)
	assert.Equal(t, uint32(2), resp.GuildDivergenceCount, "放行成功也回计数与样本")
	assert.Equal(t, "old", goldOf(t, svcCtx, pid), "放行后写发生")

	assert.Equal(t, "current", goldAtAccepted, "ACCEPTED 审计先于写 Redis")
	assert.Zero(t, insertsAtAccepted, "ACCEPTED 审计先于回档前安全快照")
	assert.Contains(t, logsAtFirstWrite, "[Rollback][GuildDivergence] accepted")
	assert.Contains(t, logsAtFirstWrite, "op_id=5")
	assert.Contains(t, logsAtFirstWrite, "op_id=6")
	assert.Contains(t, logsAtFirstWrite, "by_kind=donate:2")

	require.Len(t, snapshots.audits, 3)
	assert.True(t, strings.HasPrefix(snapshots.audits[0].Reason, "STARTED:"))
	assert.Equal(t, "GUILD_DIVERGENCE_ACCEPTED count=2 unprovable_players=0: incident-42", snapshots.audits[1].Reason)
	assert.Equal(t, "ops-alice", snapshots.audits[1].Operator)
	assert.Contains(t, snapshots.audits[2].Reason, codeTag(constants.ErrCodeOK))
	assert.Equal(t, []time.Duration{guildSettleDelay, guildRecheckDelay}, rec.durations, "写了就要复查")
}

// D10:ACCEPTED 审计写不进 → 拒绝、零写入(R3 放行必留痕),也不留"已放行"的逐行日志。
func TestRollbackGuildGate_D10_AcceptedAuditFailureBlocksWrites(t *testing.T) {
	const pid = 825
	logs := logtest.NewCollector(t)
	svcCtx, snapshots, _ := newGateTestPlayer(t, pid)
	snapshots.auditErrs = []error{nil, errors.New("audit db down")}
	enableGuildGate(svcCtx, staticGuildChecker(guildcheck.GuildCheckResult{Divergences: divergenceRows(pid, 5)}, guildcheck.GuildCheckResult{}))

	resp, err := RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{
		PlayerID: pid, SnapshotID: 1, Operator: "ops", Reason: "r", AcceptGuildDivergence: true,
	})
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeSnapshotDBError, resp.ErrorCode)
	assert.Equal(t, "current", goldOf(t, svcCtx, pid))
	assert.Zero(t, snapshots.insertCalls)
	assert.NotContains(t, logs.String(), "[Rollback][GuildDivergence] accepted", "审计失败时不能留下一批会被拿去补偿的放行日志")
}

// fakeGuildInternal 是 guildpb.GuildInternalClient 的假实现,给"真实 guildcheck + 假 RPC"的用例(D11)。
type fakeGuildInternal struct {
	calls  int
	answer func(n int, req *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error)
}

func (f *fakeGuildInternal) ListAppliedAssetOpsSince(_ context.Context, in *guildpb.ListAppliedAssetOpsSinceRequest, _ ...grpc.CallOption) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
	n := f.calls
	f.calls++
	return f.answer(n, in)
}

// D11:RollbackZone 整份清单查完才写(R4)—— 150 个玩家分两块,第 2 块失败 → 第 1 块的玩家也一个都没写。
func TestRollbackGuildGate_D11_ZoneSecondChunkFailureWritesNobody(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	const first = uint64(1001)
	setupPlayer(t, svcCtx, first, 9)
	_, err := SetPlayerField(context.Background(), svcCtx, first, "gold", []byte("current"), 0)
	require.NoError(t, err)
	times := make(map[uint64]uint64, 150)
	for pid := first; pid < first+150; pid++ {
		times[pid] = gateSnapshotCreatedAt
	}
	snapshots := &fakeSnapshotStore{
		playerTimesByZone: map[uint32]map[uint64]uint64{9: times},
		latest:            &store.SnapshotRow{ID: 3, PlayerID: first, CreatedAt: gateSnapshotCreatedAt, Data: mustSnapshotData(t, map[string][]byte{"gold": []byte("old")})},
	}
	svcCtx.SnapshotStore = snapshots
	svcCtx.RollbackFence = &fakeRollbackFence{}
	rpc := &fakeGuildInternal{answer: func(n int, _ *guildpb.ListAppliedAssetOpsSinceRequest) (*guildpb.ListAppliedAssetOpsSinceResponse, error) {
		if n == 1 {
			return nil, status.Error(codes.Unavailable, "guild restarting")
		}
		return &guildpb.ListAppliedAssetOpsSinceResponse{}, nil
	}}
	enableGuildGate(svcCtx, guildcheck.New(rpc))

	resp, err := RollbackZone(context.Background(), svcCtx, &RollbackZoneReq{ZoneID: 9, TargetTime: gateSnapshotCreatedAt, Operator: "ops", Reason: "d11", AcceptGuildDivergence: true})
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeRollbackGuildCheckFailed, resp.ErrorCode, "问不到:放行也不管用")
	assert.Equal(t, 2, rpc.calls)
	assert.Zero(t, snapshots.insertCalls)
	assert.Zero(t, resp.PlayersAffected)
	assert.Equal(t, "current", goldOf(t, svcCtx, first))
	require.Len(t, snapshots.audits, 2)
	assert.Contains(t, snapshots.audits[1].Reason, codeTag(constants.ErrCodeRollbackGuildCheckFailed))
}

// D12:RollbackAll 先 plan 全部 zone、合并过一次闸,全部通过才写第一个 zone(R4)。zone 2 的玩家有分歧 → zone 1 也零写入;
// 每个写过 STARTED 的 zone 都补一条带拒绝码的 RESULT,不留悬空。
func TestRollbackGuildGate_D12_ServerRejectionWritesNoZone(t *testing.T) {
	mr := miniredis.RunT(t)
	c := config.Config{
		MappingRedis:     redis.RedisConf{Host: mr.Addr(), Type: "node"},
		Regions:          []config.RegionConfig{{Id: 1, Zones: []uint32{2, 1}, Redis: config.RedisClusterConfig{Addrs: []string{mr.Addr()}}}},
		PlayerLockTTLSec: 3,
	}
	r := routing.NewRouter(c)
	t.Cleanup(r.Close)
	snapshots := &fakeSnapshotStore{playerTimesByZone: map[uint32]map[uint64]uint64{
		1: {501: gateSnapshotCreatedAt},
		2: {502: gateSnapshotCreatedAt + 60},
	}}
	fence := &fakeRollbackFence{}
	svcCtx := &svc.ServiceContext{Config: c, Router: r, SnapshotStore: snapshots, RollbackFence: fence}
	checker := staticGuildChecker(guildcheck.GuildCheckResult{Divergences: divergenceRows(502, 9)}, guildcheck.GuildCheckResult{})
	rec := enableGuildGate(svcCtx, checker)

	resp, err := RollbackAll(context.Background(), svcCtx, &RollbackAllReq{TargetTime: gateSnapshotCreatedAt + 100, Operator: "ops", Reason: "d12"})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeRollbackGuildDivergence, resp.ErrorCode)
	assert.Equal(t, uint32(1), resp.GuildDivergenceCount)
	assert.Zero(t, resp.ZonesProcessed)
	assert.Zero(t, snapshots.insertCalls, "zone 1 也不许写")
	assert.Equal(t, 1, fence.serverAcquired)
	assert.Equal(t, []time.Duration{guildSettleDelay}, rec.durations, "全服只沉降一次")

	require.Len(t, checker.since, 1, "全部 zone 合并检查一次")
	assert.Len(t, checker.since[0], 2)

	require.Len(t, snapshots.audits, 6)
	wantPrefix := []string{"STARTED:", "STARTED: server rollback", "STARTED: server rollback", "RESULT", "RESULT", "RESULT"}
	for i, row := range snapshots.audits {
		assert.True(t, strings.HasPrefix(row.Reason, wantPrefix[i]), "audit %d = %q", i, row.Reason)
	}
	assert.Equal(t, uint32(1), snapshots.audits[1].ZoneID, "zone 按升序处理")
	assert.Equal(t, uint32(2), snapshots.audits[2].ZoneID)
	for _, row := range snapshots.audits[3:] {
		assert.Contains(t, row.Reason, codeTag(constants.ErrCodeRollbackGuildDivergence))
	}
}

// D13 R5:执行期选中的快照早于检查时用的那份 → 该玩家失败、不写(连安全快照都不打)。
func TestRollbackGuildGate_D13_OlderExecutionSnapshotIsNotWritten(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	const pid = uint64(601)
	setupPlayer(t, svcCtx, pid, 9)
	_, err := SetPlayerField(context.Background(), svcCtx, pid, "gold", []byte("current"), 0)
	require.NoError(t, err)
	snapshots := &fakeSnapshotStore{
		playerTimesByZone: map[uint32]map[uint64]uint64{9: {pid: gateSnapshotCreatedAt}},
		latest:            &store.SnapshotRow{ID: 5, PlayerID: pid, CreatedAt: gateSnapshotCreatedAt - 1, Data: mustSnapshotData(t, map[string][]byte{"gold": []byte("old")})},
	}
	svcCtx.SnapshotStore = snapshots
	svcCtx.RollbackFence = &fakeRollbackFence{}
	rec := enableGuildGate(svcCtx, cleanGuildChecker())

	resp, err := RollbackZone(context.Background(), svcCtx, &RollbackZoneReq{ZoneID: 9, TargetTime: gateSnapshotCreatedAt, Operator: "ops", Reason: "d13"})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, resp.ErrorCode, "单个玩家失败不让整个 zone 失败(原样)")
	assert.Equal(t, uint32(1), resp.PlayersFailed)
	assert.Equal(t, []uint64{pid}, resp.FailedPlayerIDs)
	assert.Zero(t, snapshots.insertCalls)
	assert.Equal(t, "current", goldOf(t, svcCtx, pid))
	assert.Equal(t, []time.Duration{guildSettleDelay}, rec.durations, "没人写就不复查")
}

// D14:写后复查多出一行 → DivergedAfterWrite + 新行计数与样本(数据已写、不撤销);复查本身失败 → 同码、计数 0。
func TestRollbackGuildGate_D14_PostWriteRecheck(t *testing.T) {
	t.Run("新分歧", func(t *testing.T) {
		const pid = 826
		svcCtx, snapshots, _ := newGateTestPlayer(t, pid)
		known := divergenceRows(pid, 5)
		rec := enableGuildGate(svcCtx, staticGuildChecker(
			guildcheck.GuildCheckResult{Divergences: known},
			guildcheck.GuildCheckResult{Divergences: append(append([]guildcheck.GuildDivergence(nil), known...), divergenceRows(pid, 8)...)},
		))

		resp, err := RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{
			PlayerID: pid, SnapshotID: 1, Operator: "ops", Reason: "r", AcceptGuildDivergence: true,
		})
		require.NoError(t, err, "写后分歧要带出计数与样本,不能走 err")
		assert.Equal(t, constants.ErrCodeRollbackGuildDivergedAfterWrite, resp.ErrorCode)
		assert.Equal(t, uint32(1), resp.GuildDivergenceCount, "只报检查阶段没有的新行")
		require.Len(t, resp.GuildDivergences, 1)
		assert.Equal(t, uint64(8), resp.GuildDivergences[0].OpID)
		assert.NotZero(t, resp.PreRollbackSnapshotID, "运维要靠它撤销这次回档")
		assert.Equal(t, "old", goldOf(t, svcCtx, pid), "数据已写,不自动撤销")
		assert.Equal(t, []time.Duration{guildSettleDelay, guildRecheckDelay}, rec.durations)
		last := snapshots.audits[len(snapshots.audits)-1]
		assert.Contains(t, last.Reason, codeTag(constants.ErrCodeRollbackGuildDivergedAfterWrite))
		assert.Equal(t, uint32(1), last.PlayersAffected)
	})

	t.Run("复查失败", func(t *testing.T) {
		const pid = 827
		svcCtx, _, _ := newGateTestPlayer(t, pid)
		enableGuildGate(svcCtx, &fakeGuildChecker{answer: func(n int, _ context.Context, _ map[uint64]uint64) (guildcheck.GuildCheckResult, error) {
			if n == 0 {
				return guildcheck.GuildCheckResult{}, nil
			}
			return guildcheck.GuildCheckResult{}, errors.New("guild unreachable")
		}})

		resp, err := RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{PlayerID: pid, SnapshotID: 1})
		require.NoError(t, err)
		assert.Equal(t, constants.ErrCodeRollbackGuildDivergedAfterWrite, resp.ErrorCode, "证明不了没有新分歧,同样紧急")
		assert.Zero(t, resp.GuildDivergenceCount)
		assert.Equal(t, "old", goldOf(t, svcCtx, pid))
	})
}

// 写后复查只查走到写 Redis 那一步的玩家:zone 里 601 因 R5 没被写、602 被写。601 在检查之后新终结的行不是分歧
// (他的数据没有回退),按完整计划复查会把它报成 post-write,运维照单补偿就多扣一次。
func TestRollbackGuildGate_PostWriteRecheckCoversOnlyWrittenPlayers(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	const notWritten, written = uint64(601), uint64(602)
	for _, pid := range []uint64{notWritten, written} {
		setupPlayer(t, svcCtx, pid, 9)
		_, err := SetPlayerField(context.Background(), svcCtx, pid, "gold", []byte("current"), 0)
		require.NoError(t, err)
	}
	snapshots := &fakeSnapshotStore{
		// 执行期两人拿到的都是 created_at = gateSnapshotCreatedAt 的那份;601 的计划值更晚 → R5 判他失败、不写。
		playerTimesByZone: map[uint32]map[uint64]uint64{9: {notWritten: gateSnapshotCreatedAt + 10, written: gateSnapshotCreatedAt}},
		latest:            &store.SnapshotRow{ID: 5, PlayerID: written, CreatedAt: gateSnapshotCreatedAt, Data: mustSnapshotData(t, map[string][]byte{"gold": []byte("old")})},
	}
	svcCtx.SnapshotStore = snapshots
	svcCtx.RollbackFence = &fakeRollbackFence{}
	checker := &fakeGuildChecker{answer: func(n int, _ context.Context, since map[uint64]uint64) (guildcheck.GuildCheckResult, error) {
		if n == 0 {
			return guildcheck.GuildCheckResult{}, nil
		}
		// 与真实 checker 一样只回被查玩家的行:复查若仍带着没写过的 601,他这条新行就会被报成 post-write。
		var res guildcheck.GuildCheckResult
		if _, asked := since[notWritten]; asked {
			res.Divergences = divergenceRows(notWritten, 8)
		}
		return res, nil
	}}
	rec := enableGuildGate(svcCtx, checker)

	resp, err := RollbackZone(context.Background(), svcCtx, &RollbackZoneReq{ZoneID: 9, TargetTime: gateSnapshotCreatedAt + 100, Operator: "ops", Reason: "narrow"})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, resp.ErrorCode, "没写过的玩家的新行不是写后分歧")
	assert.Zero(t, resp.GuildDivergenceCount)
	assert.Equal(t, uint32(1), resp.PlayersAffected)
	assert.Equal(t, []uint64{notWritten}, resp.FailedPlayerIDs)
	assert.Equal(t, "current", goldOf(t, svcCtx, notWritten))
	assert.Equal(t, "old", goldOf(t, svcCtx, written))
	assert.Equal(t, []time.Duration{guildSettleDelay, guildRecheckDelay}, rec.durations)
	require.Len(t, checker.since, 2)
	assert.Len(t, checker.since[0], 2, "检查阶段查整份清单")
	require.Len(t, checker.since[1], 1, "复查只查走到写那一步的玩家")
	assert.Contains(t, checker.since[1], written)
	assert.Equal(t, checker.since[0][written], checker.since[1][written], "复查沿用检查阶段的 since")
}

// 单人回档:写 Redis 失败之后写后复查又报警 → 响应码是更紧急的 DivergedAfterWrite(带新行),
// 但 RESULT 审计必须按真实结果记"恢复 0 人、失败 1 人",不能因为响应码是 DivergedAfterWrite 就算成已恢复。
func TestRollbackGuildGate_PostWriteFlagAfterFailedWriteCountsAsFailed(t *testing.T) {
	const pid = uint64(828)
	svcCtx, mr := newTestSvcCtx(t)
	setupPlayer(t, svcCtx, pid, 9)
	setResp, err := SetPlayerField(context.Background(), svcCtx, pid, "gold", []byte("current"), 0)
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, setResp.ErrorCode)
	snapshots := &fakeSnapshotStore{snapshotByID: map[uint64]*store.SnapshotRow{
		1: {ID: 1, PlayerID: pid, CreatedAt: gateSnapshotCreatedAt, Data: mustSnapshotData(t, map[string][]byte{"gold": []byte("old")})},
	}}
	// 回档前安全快照落库之后让 Redis 整体报错:随后的 SavePlayerData 失败,但"走到写那一步"已经成立。
	// 用非 LOADING / READONLY 的文案,免得 go-redis 当成可重试错误退避重试。
	snapshots.onInsert = func() { mr.SetError("ERR simulated redis failure") }
	svcCtx.SnapshotStore = snapshots
	svcCtx.RollbackFence = &fakeRollbackFence{}
	enableGuildGate(svcCtx, staticGuildChecker(guildcheck.GuildCheckResult{}, guildcheck.GuildCheckResult{Divergences: divergenceRows(pid, 8)}))

	resp, err := RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{PlayerID: pid, SnapshotID: 1, Operator: "ops", Reason: "r"})
	require.NoError(t, err, "写后分歧要带出计数与样本,不能走 err")
	assert.Equal(t, constants.ErrCodeRollbackGuildDivergedAfterWrite, resp.ErrorCode)
	assert.Equal(t, uint32(1), resp.GuildDivergenceCount)
	last := snapshots.audits[len(snapshots.audits)-1]
	assert.Contains(t, last.Reason, codeTag(constants.ErrCodeRollbackGuildDivergedAfterWrite))
	assert.Zero(t, last.PlayersAffected, "写失败的玩家不能记成已恢复")
	assert.Equal(t, uint32(1), last.PlayersFailed)

	mr.SetError("")
	assert.Equal(t, "current", goldOf(t, svcCtx, pid), "写失败,数据没有被覆盖")
}

// D17:不可证明(快照早于帮会流水保留期)× 放行两态。未放行 → Divergence + 不可证明计数、零写入、unprovable rejected 日志;
// 合法放行 → unprovable accepted 日志先于第一笔写、写发生、ACCEPTED 审计记下 unprovable_players。
func TestRollbackGuildGate_D17_UnprovablePlayers(t *testing.T) {
	unprovable := func(pid uint64) guildcheck.GuildCheckResult {
		return guildcheck.GuildCheckResult{UnprovablePlayerIDs: []uint64{pid}, RetentionCutoffMs: gateSnapshotCreatedAt*1000 + 1}
	}

	t.Run("未放行", func(t *testing.T) {
		const pid = 828
		logs := logtest.NewCollector(t)
		svcCtx, snapshots, _ := newGateTestPlayer(t, pid)
		enableGuildGate(svcCtx, staticGuildChecker(unprovable(pid), guildcheck.GuildCheckResult{}))

		resp, err := RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{PlayerID: pid, SnapshotID: 1, Operator: "ops", Reason: "r"})
		require.NoError(t, err)
		assert.Equal(t, constants.ErrCodeRollbackGuildDivergence, resp.ErrorCode, "保留期不可证明是可放行的拒绝,不是 CheckFailed")
		assert.Equal(t, uint32(1), resp.GuildUnprovablePlayerCount)
		assert.Zero(t, resp.GuildDivergenceCount)
		assert.Zero(t, snapshots.insertCalls)
		assert.Equal(t, "current", goldOf(t, svcCtx, pid))
		assert.Contains(t, logs.String(), "[Rollback][GuildDivergence] unprovable rejected")
		assert.Contains(t, logs.String(), fmt.Sprintf("player_ids=%d", pid))
	})

	t.Run("合法放行", func(t *testing.T) {
		const pid = 829
		logs := logtest.NewCollector(t)
		svcCtx, snapshots, _ := newGateTestPlayer(t, pid)
		enableGuildGate(svcCtx, staticGuildChecker(unprovable(pid), unprovable(pid)))
		var logsAtFirstWrite string
		snapshots.onInsert = func() {
			if logsAtFirstWrite == "" {
				logsAtFirstWrite = logs.String()
			}
		}

		resp, err := RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{
			PlayerID: pid, SnapshotID: 1, Operator: "ops", Reason: "checked by hand", AcceptGuildDivergence: true,
		})
		require.NoError(t, err)
		assert.Equal(t, constants.ErrCodeOK, resp.ErrorCode, "复查里的不可证明本身不算新分歧")
		assert.Equal(t, "old", goldOf(t, svcCtx, pid))
		assert.Contains(t, logsAtFirstWrite, "[Rollback][GuildDivergence] unprovable accepted")
		require.GreaterOrEqual(t, len(snapshots.audits), 2)
		assert.Equal(t, "GUILD_DIVERGENCE_ACCEPTED count=0 unprovable_players=1: checked by hand", snapshots.audits[1].Reason)
	})
}

// D18 时序与 ctx。
func TestRollbackGuildGate_D18_TimingAndContexts(t *testing.T) {
	// (a) 沉降:checker 在沉降等待之前答空、之后答一行 → 首次检查必须发生在沉降之后 → 拒绝、零写入。
	t.Run("a 检查在沉降之后", func(t *testing.T) {
		const pid = 830
		svcCtx, snapshots, _ := newGateTestPlayer(t, pid)
		rec := &sleepRecorder{}
		checker := &fakeGuildChecker{answer: func(int, context.Context, map[uint64]uint64) (guildcheck.GuildCheckResult, error) {
			if len(rec.durations) == 0 {
				return guildcheck.GuildCheckResult{}, nil
			}
			return guildcheck.GuildCheckResult{Divergences: divergenceRows(pid, 1)}, nil
		}}
		enableGuildGate(svcCtx, checker)
		svcCtx.GuildCheckSleep = rec.sleep

		resp, err := RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{PlayerID: pid, SnapshotID: 1})
		require.NoError(t, err)
		assert.Equal(t, constants.ErrCodeRollbackGuildDivergence, resp.ErrorCode)
		require.NotEmpty(t, rec.durations)
		assert.Equal(t, guildSettleDelay, rec.durations[0])
		assert.Zero(t, snapshots.insertCalls)
		assert.Equal(t, "current", goldOf(t, svcCtx, pid))
	})

	// (b) 脱钩 ctx:入口 ctx 在最后一笔写完之后被取消 → 复查仍被调用且拿到未取消、带截止时间的 ctx;
	// RESULT 审计写入成功;结论是 clean(OK),不是 post_write_recheck_failed。
	t.Run("b 写后阶段脱钩", func(t *testing.T) {
		const pid = 831
		svcCtx, snapshots, _ := newGateTestPlayer(t, pid)
		checker := cleanGuildChecker()
		rec := enableGuildGate(svcCtx, checker)
		entry, cancel := context.WithCancel(context.Background())
		defer cancel()
		rec.hook = func(n int, _ context.Context) error {
			if n == 1 { // 第二次等待 = 写后复查前的等待:数据已写完,调用方此时断开
				cancel()
			}
			return nil
		}

		resp, err := RollbackPlayer(entry, svcCtx, &RollbackPlayerReq{PlayerID: pid, SnapshotID: 1})
		require.NoError(t, err)
		assert.Equal(t, constants.ErrCodeOK, resp.ErrorCode)
		assert.Equal(t, "old", goldOf(t, svcCtx, pid))
		require.Error(t, entry.Err(), "用例前提:入口 ctx 已取消")

		require.Len(t, rec.ctxErrs, 2)
		assert.NoError(t, rec.ctxErrs[1], "复查等待用的是脱钩 ctx")
		require.Len(t, checker.ctxs, 2, "入口 ctx 取消后复查仍然发生")
		assert.NoError(t, checker.ctxErrs[1], "复查拿到的 ctx 在调用时未被取消")
		deadline, ok := checker.ctxs[1].Deadline()
		require.True(t, ok, "脱钩 ctx 必须自带截止时间")
		assert.LessOrEqual(t, time.Until(deadline), guildRecheckDelay+guildRecheckBudget)

		last := len(snapshots.auditCtxErrs) - 1
		assert.NoError(t, snapshots.auditCtxErrs[last], "RESULT 审计在脱钩 ctx 上写")
		assert.Contains(t, snapshots.audits[last].Reason, codeTag(constants.ErrCodeOK))
	})

	// (c) 复查超出预算(脱钩 ctx 到点)→ DivergedAfterWrite + "post-write recheck failed" 日志。
	t.Run("c 复查超预算", func(t *testing.T) {
		const pid = 832
		logs := logtest.NewCollector(t)
		svcCtx, _, _ := newGateTestPlayer(t, pid)
		recheckHadDeadline := false
		enableGuildGate(svcCtx, &fakeGuildChecker{answer: func(n int, ctx context.Context, _ map[uint64]uint64) (guildcheck.GuildCheckResult, error) {
			if n == 0 {
				return guildcheck.GuildCheckResult{}, nil
			}
			_, recheckHadDeadline = ctx.Deadline()
			return guildcheck.GuildCheckResult{}, fmt.Errorf("guild check budget exhausted: %w", context.DeadlineExceeded)
		}})

		resp, err := RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{PlayerID: pid, SnapshotID: 1})
		require.NoError(t, err)
		assert.Equal(t, constants.ErrCodeRollbackGuildDivergedAfterWrite, resp.ErrorCode)
		assert.True(t, recheckHadDeadline, "复查必须在有截止时间的 ctx 上跑")
		assert.Contains(t, logs.String(), "[Rollback][GuildDivergence] post-write recheck failed")
	})
}

// 调用方在沉降中取消 / 检查超上限 → CheckFailed、零写入(放行无效)。
func TestRollbackGuildGate_SettleCancelledOrTooManyRowsRejects(t *testing.T) {
	const pid = 833
	svcCtx, snapshots, _ := newGateTestPlayer(t, pid)
	rec := enableGuildGate(svcCtx, cleanGuildChecker())
	rec.hook = func(int, context.Context) error { return context.Canceled }

	resp, err := RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{PlayerID: pid, SnapshotID: 1})
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeRollbackGuildCheckFailed, resp.ErrorCode)
	assert.Zero(t, snapshots.insertCalls)

	enableGuildGate(svcCtx, &fakeGuildChecker{answer: func(int, context.Context, map[uint64]uint64) (guildcheck.GuildCheckResult, error) {
		return guildcheck.GuildCheckResult{}, fmt.Errorf("too many: %w", guildcheck.ErrTooManyDivergences)
	}})
	resp, err = RollbackPlayer(context.Background(), svcCtx, &RollbackPlayerReq{PlayerID: pid, SnapshotID: 1, Operator: "ops", Reason: "r", AcceptGuildDivergence: true})
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeRollbackGuildCheckFailed, resp.ErrorCode, "超上限:放行也不管用")
	assert.Zero(t, snapshots.insertCalls)
	assert.Equal(t, "current", goldOf(t, svcCtx, pid))
}

func TestGuildSinceMs(t *testing.T) {
	got, err := guildSinceMs(gateSnapshotCreatedAt, 300000)
	require.NoError(t, err)
	assert.Equal(t, gateSnapshotCreatedAt*1000-300000, got)
	got, err = guildSinceMs(100, 300000) // 小时间戳不下溢,钳到 1(guild 把 0 当漏填)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), got)
	_, err = guildSinceMs(^uint64(0)/10, 300000)
	require.Error(t, err)
}
