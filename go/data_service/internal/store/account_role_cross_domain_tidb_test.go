package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	base "proto/common/base"
	dspb "proto/data_service"
	rfpb "proto/role_fence"
	"shared/accountdirectory"
	"shared/rolefence"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// fakeFences 是假的协调者:用例直接定 Prepare 的结论,记下问过和推过的状态。
// 参与者各自的挂起判定由 F2、F3 在各服务的用例里覆盖,这里只测目录一侧的两阶段。
type fakeFences struct {
	mu       sync.Mutex
	outcome  rolefence.PrepareOutcome
	byPlayer map[uint64]rolefence.PrepareOutcome
	// dryRun 是试探的结论,nil 表示放行:大多数用例要测的是真正的 Prepare。
	dryRun   *rolefence.PrepareOutcome
	probed   []*rfpb.RoleFenceState
	prepared []*rfpb.RoleFenceState
	pushed   []*rfpb.RoleFenceState
}

func (f *fakeFences) DryRun(_ context.Context, state *rfpb.RoleFenceState) rolefence.PrepareOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probed = append(f.probed, proto.Clone(state).(*rfpb.RoleFenceState))
	if f.dryRun != nil {
		return *f.dryRun
	}
	return rolefence.PrepareClean
}

func (f *fakeFences) setDryRun(outcome rolefence.PrepareOutcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dryRun = &outcome
}

func (f *fakeFences) probedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.probed)
}

func (f *fakeFences) Prepare(_ context.Context, state *rfpb.RoleFenceState) rolefence.PrepareOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared = append(f.prepared, proto.Clone(state).(*rfpb.RoleFenceState))
	if outcome, ok := f.byPlayer[state.PlayerId]; ok {
		return outcome
	}
	return f.outcome
}

func (f *fakeFences) Push(_ context.Context, state *rfpb.RoleFenceState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushed = append(f.pushed, proto.Clone(state).(*rfpb.RoleFenceState))
}

func (f *fakeFences) set(outcome rolefence.PrepareOutcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcome = outcome
}

func (f *fakeFences) preparedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prepared)
}

func (f *fakeFences) pushedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pushed)
}

func (f *fakeFences) lastPrepared() *rfpb.RoleFenceState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prepared[len(f.prepared)-1]
}

func (f *fakeFences) lastPushed() *rfpb.RoleFenceState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pushed[len(f.pushed)-1]
}

func fencesOf(d *AccountDirectoryStore) *fakeFences { return d.fences.(*fakeFences) }

func fenceVersionForTest(t *testing.T, ctx context.Context, db *sql.DB, player uint64) uint64 {
	t.Helper()
	var version uint64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT fence_version FROM account_role WHERE player_id=?", player).Scan(&version))
	return version
}

func tradeStateForTest(t *testing.T, ctx context.Context, db *sql.DB, listing uint64) dspb.AccountRoleTradeState {
	t.Helper()
	var state uint32
	require.NoError(t, db.QueryRowContext(ctx, "SELECT state FROM account_role_trade WHERE listing_id=?", listing).Scan(&state))
	return dspb.AccountRoleTradeState(state)
}

func tableRowsForTest(t *testing.T, ctx context.Context, db *sql.DB, table string) int {
	t.Helper()
	var rows int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&rows))
	return rows
}

func fenceRole(t *testing.T) (context.Context, *AccountDirectoryStore, *sql.DB, *fakeFences, *dspb.BeginAccountRoleTradeRequest) {
	t.Helper()
	d, db := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	return ctx, d, db, fencesOf(d), &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
}

func TestTiDBRoleCancelBeforeUnknownBeginPersistsPermanentTombstone(t *testing.T) {
	d, _ := accountDirectoryTiDB(t)
	s := NewAccountRoleTradeStore(d, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	for listing := uint64(501); listing < 509; listing++ {
		begin := &dspb.BeginAccountRoleTradeRequest{ListingId: listing, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
		stop := &dspb.CancelAccountRoleTradeRequest{ListingId: listing, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
		var wg sync.WaitGroup
		var beginErr, stopErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, beginErr = s.Begin(ctx, begin) }()
		go func() { defer wg.Done(); <-start; _, stopErr = s.Cancel(ctx, stop) }()
		close(start)
		wg.Wait()
		require.NoError(t, stopErr)
		require.True(t, beginErr == nil || errors.Is(beginErr, accountdirectory.ErrConflict))
		replayed, err := s.Begin(ctx, begin)
		require.NoError(t, err)
		require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED, replayed.State)
		owned, err := d.Lookup(ctx, 11)
		require.NoError(t, err)
		require.Zero(t, owned.TradeListingId)
	}
}

// 全通过:参与者投的是带准备令牌的新版本,之后才换成最终令牌、进 FREEZING。重放只回收据,不再问参与者。
func TestTiDBRoleBeginAllCleanFreezesParticipantsThenEntersFreezing(t *testing.T) {
	ctx, d, db, fences, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, nil)
	require.EqualValues(t, 1, fenceVersionForTest(t, ctx, db, 11))
	out, err := s.Begin(ctx, begin)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING, out.State)
	require.Equal(t, 1, fences.probedCount())
	require.Equal(t, 1, fences.preparedCount())
	require.True(t, proto.Equal(&rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 1, FreezeToken: 101, Version: 2, HomeZone: 1}, fences.lastPrepared()))
	require.EqualValues(t, 3, fenceVersionForTest(t, ctx, db, 11), "写准备令牌和转最终令牌各加 1")
	require.Equal(t, 1, fences.pushedCount())
	final := &rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 1, FreezeToken: 101, FreezeFinal: true, Version: 3, HomeZone: 1}
	require.True(t, proto.Equal(final, fences.lastPushed()), "副本停在准备令牌上,推一次最终状态。丢了由退款判定前的自愈兜住")
	state, err := d.RoleFenceState(ctx, 11)
	require.NoError(t, err)
	require.True(t, proto.Equal(final, state))
	replayed, err := s.Begin(ctx, begin)
	require.NoError(t, err)
	require.True(t, proto.Equal(out, replayed))
	require.Equal(t, 1, fences.probedCount(), "已有收据不再试探")
	require.Equal(t, 1, fences.preparedCount())
	require.EqualValues(t, 3, fenceVersionForTest(t, ctx, db, 11))
}

// 规格 §3.5:试探有挂起时目录一个字都不写,trade 的对账循环重放也不会一遍遍打断玩家。
func TestTiDBRoleBeginDryRunRejectionWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome rolefence.PrepareOutcome
		want    error
	}{
		{"有挂起", rolefence.PrepareBlocked, accountdirectory.ErrConflict},
		{"问不齐", rolefence.PrepareRetry, accountdirectory.ErrRetryLater},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, d, db, fences, begin := fenceRole(t)
			s := NewAccountRoleTradeStore(d, nil)
			fences.setDryRun(tc.outcome)
			for range 3 {
				_, err := s.Begin(ctx, begin)
				require.ErrorIs(t, err, tc.want)
			}
			require.Equal(t, 3, fences.probedCount())
			require.True(t, proto.Equal(&rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 1, FreezeToken: 101, Version: 1, HomeZone: 1}, fences.probed[0]), "试探带这笔的令牌,挂起判定靠它放行本 listing")
			require.Zero(t, fences.preparedCount())
			require.Zero(t, fences.pushedCount())
			require.Zero(t, tableRowsForTest(t, ctx, db, "account_role_trade"))
			require.EqualValues(t, 1, fenceVersionForTest(t, ctx, db, 11))
			state, err := d.RoleFenceState(ctx, 11)
			require.NoError(t, err)
			require.Zero(t, state.FreezeToken)

			fences.setDryRun(rolefence.PrepareClean)
			out, err := s.Begin(ctx, begin)
			require.NoError(t, err)
			require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING, out.State)
		})
	}
}

// 不合格的 Begin 连试探都不用问,更不写。
func TestTiDBRoleBeginRejectedBeforeDryRun(t *testing.T) {
	ctx, d, db, fences, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, nil)
	wrong := proto.Clone(begin).(*dspb.BeginAccountRoleTradeRequest)
	wrong.ExpectedOwnerRevision = 2
	_, err := s.Begin(ctx, wrong)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	wrong = proto.Clone(begin).(*dspb.BeginAccountRoleTradeRequest)
	wrong.SellerAccount = "other"
	_, err = s.Begin(ctx, wrong)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	missing := proto.Clone(begin).(*dspb.BeginAccountRoleTradeRequest)
	missing.PlayerId = 999
	_, err = s.Begin(ctx, missing)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
	require.Zero(t, fences.probedCount())
	require.Zero(t, tableRowsForTest(t, ctx, db, "account_role_trade"))
}

// 规格 §3.5:准备期间 Register 和 Lookup 不受影响;转 FREEZING 的事务持角色行锁,把投票期间进场的实例也快照进 holder,此后 Register 才被挡。
func TestTiDBRolePreparingDoesNotBlockEntryAndFreezingSnapshotsIt(t *testing.T) {
	ctx, d, db, fences, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, nil)
	a := NewAccountRoleAdmissionStore(d)
	fences.set(rolefence.PrepareRetry)
	_, err := s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	require.Zero(t, tableRowsForTest(t, ctx, db, "account_role_trade_holder"))

	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, owned.TradeListingId, "入场判定只看最终令牌")
	require.Zero(t, owned.PrepareToken, "准备令牌不出目录")
	state, err := d.RoleFenceState(ctx, 11)
	require.NoError(t, err)
	require.EqualValues(t, 101, state.FreezeToken, "参与者要拦业务写入,看得到令牌")
	require.False(t, state.FreezeFinal)
	_, err = a.Register(ctx, admissionTestRequest(11, 1, 1))
	require.NoError(t, err, "投票期间照常进场")

	fences.set(rolefence.PrepareClean)
	out, err := s.Begin(ctx, begin)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING, out.State)
	require.Len(t, out.Holders, 1, "投票期间进场的实例要在本轮全集里")
	require.EqualValues(t, 1, out.Holders[0].Seal.ThroughGeneration)
	_, err = a.Register(ctx, admissionTestRequest(11, 2, 1))
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "最终令牌之后才挡")
	owned, err = d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.EqualValues(t, 101, owned.TradeListingId)
	state, err = d.RoleFenceState(ctx, 11)
	require.NoError(t, err)
	require.True(t, state.FreezeFinal)
}

// 准备令牌视同有令牌:建角重放、身份补齐、删角、别的 Begin、别的 listing 的取消都拒绝。
func TestTiDBRolePreparingRejectsOwnershipOperations(t *testing.T) {
	ctx, d, db, fences, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, nil)
	fences.set(rolefence.PrepareRetry)
	_, err := s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	_, err = createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	_, err = d.Repair(ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11, ClassId: 3}, 1)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.ErrorIs(t, d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}), accountdirectory.ErrConflict)
	other := proto.Clone(begin).(*dspb.BeginAccountRoleTradeRequest)
	other.ListingId = 102
	_, err = s.Begin(ctx, other)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	_, err = s.Cancel(ctx, &dspb.CancelAccountRoleTradeRequest{ListingId: 102, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.Equal(t, 1, tableRowsForTest(t, ctx, db, "account_role_trade"), "拒绝不留墓碑")
	require.Zero(t, tableRowsForTest(t, ctx, db, "account_role_removal"))
}

// 规格 §3.5:重放超过时限还问不齐就撤销。撤销之后在途的 Prepare 作废,下一次重放从试探重来。
func TestTiDBRolePreparingExpiredIsAbortedByReplay(t *testing.T) {
	ctx, d, db, fences, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, nil)
	digest, err := roleTradeDigest(begin)
	require.NoError(t, err)
	fences.set(rolefence.PrepareRetry)
	_, err = s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	inFlight := fences.lastPrepared()
	_, err = s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_PREPARING, tradeStateForTest(t, ctx, db, 101), "没到时限不撤销")
	require.Zero(t, fences.pushedCount())

	_, err = db.ExecContext(ctx, "UPDATE account_role_trade SET created_ms=created_ms-?", rolePrepareDeadline.Milliseconds()+1000)
	require.NoError(t, err)
	_, err = s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrRetryLater, "原因是问不齐,不是有挂起:调用方按短退避重试。撤销已提交,结局已知")
	require.Zero(t, tableRowsForTest(t, ctx, db, "account_role_trade"), "和被拒的 Begin 一样不留痕")
	require.EqualValues(t, 3, fenceVersionForTest(t, ctx, db, 11))
	require.True(t, proto.Equal(&rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 1, Version: 3, HomeZone: 1}, fences.lastPushed()))

	_, err = s.finishPrepare(ctx, begin, digest, inFlight)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "撤销之后在途的结论不算数")
	state, err := d.RoleFenceState(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, state.FreezeToken)

	// 超时但这一轮问齐了:接着走完,不撤销。
	_, err = s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	_, err = db.ExecContext(ctx, "UPDATE account_role_trade SET created_ms=created_ms-?", rolePrepareDeadline.Milliseconds()+1000)
	require.NoError(t, err)
	fences.set(rolefence.PrepareClean)
	out, err := s.Begin(ctx, begin)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING, out.State)
}

// 规格 §3.5:没人重放的 PREPARING 由后台撤销。没到时限的、已经离开 PREPARING 的不碰,每轮有上限。
func TestTiDBRoleAbortStalePreparingInBackground(t *testing.T) {
	ctx, d, db, fences, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, testRoleFreezeVerifier())
	digest, err := roleTradeDigest(begin)
	require.NoError(t, err)
	_, err = createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 12})
	require.NoError(t, err)
	beginAndConfirmRole(t, s, ctx, 102, 12, "seller", 1)
	fences.set(rolefence.PrepareRetry)
	_, err = s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	inFlight := fences.lastPrepared()
	version := fenceVersionForTest(t, ctx, db, 11)
	pushed := fences.pushedCount()

	result, err := d.AbortStalePreparing(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, PrepareAbortResult{}, result, "没到时限不碰")
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_PREPARING, tradeStateForTest(t, ctx, db, 101))

	_, err = db.ExecContext(ctx, "UPDATE account_role_trade SET created_ms=created_ms-?", rolePrepareDeadline.Milliseconds()+1000)
	require.NoError(t, err)
	result, err = d.AbortStalePreparing(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, PrepareAbortResult{Aborted: 1}, result, "FROZEN 的不是 PREPARING,不碰")
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN, tradeStateForTest(t, ctx, db, 102))
	require.Equal(t, 1, tableRowsForTest(t, ctx, db, "account_role_trade"))
	require.Equal(t, version+1, fenceVersionForTest(t, ctx, db, 11), "版本加 1 让在途 Prepare 作废")
	require.Equal(t, pushed+1, fences.pushedCount())
	state, err := d.RoleFenceState(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, state.FreezeToken)
	_, err = s.finishPrepare(ctx, begin, digest, inFlight)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "撤销之后在途的结论不算数")

	result, err = d.AbortStalePreparing(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, PrepareAbortResult{}, result, "幂等")

	// 上限:两行超时,一轮只撤一行。
	_, err = createInitialRoleForTest(d, ctx, "other-seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 13})
	require.NoError(t, err)
	_, err = s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 103, PlayerId: 13, SellerAccount: "other-seller", ExpectedOwnerRevision: 1})
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	_, err = db.ExecContext(ctx, "UPDATE account_role_trade SET created_ms=created_ms-? WHERE state=?", rolePrepareDeadline.Milliseconds()+1000, uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_PREPARING))
	require.NoError(t, err)
	result, err = d.AbortStalePreparing(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, PrepareAbortResult{Aborted: 1}, result)
	result, err = d.AbortStalePreparing(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, PrepareAbortResult{Aborted: 1}, result)
}

// brokenStoreSteps 让存档库读写一律失败,记下碰过几次。目录一侧走真的。
type brokenStoreSteps struct {
	directorySteps
	touched *atomic.Int32
}

var errBrokenPlayerStore = errors.New("player store down")

func (b brokenStoreSteps) readStoreOwner(context.Context, uint32, uint64) (*lockedPlayerStoreOwner, error) {
	b.touched.Add(1)
	return nil, errBrokenPlayerStore
}

func (b brokenStoreSteps) raiseStoreEpoch(context.Context, uint32, uint64, lockedPlayerStoreOwner, uint64) (bool, error) {
	b.touched.Add(1)
	return false, errBrokenPlayerStore
}

// 规格 §3.5:Cancel 不依赖存档库。PREPARING 没有 holder,存档库故障时照样能完成,一次都不碰它。
func TestTiDBRoleCancelFromPreparingDoesNotTouchPlayerStore(t *testing.T) {
	ctx, d, _, fences, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, nil)
	fences.set(rolefence.PrepareRetry)
	_, err := s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	var touched atomic.Int32
	d.steps = brokenStoreSteps{directorySteps: d.steps, touched: &touched}
	out, err := s.Cancel(ctx, &dspb.CancelAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED, out.State)
	require.Zero(t, touched.Load())
}

// 进了 FREEZING 没有未排空的持有者也一样;有未排空的才要判死,这时存档库的故障照实回错误。
func TestTiDBRoleCancelOnlyTouchesPlayerStoreForPendingHolders(t *testing.T) {
	ctx, d, _, _, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, nil)
	_, err := s.Begin(ctx, begin)
	require.NoError(t, err)
	var touched atomic.Int32
	d.steps = brokenStoreSteps{directorySteps: d.steps, touched: &touched}
	stop := &dspb.CancelAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	out, err := s.Cancel(ctx, stop)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED, out.State)
	require.Zero(t, touched.Load())

	_, err = createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 12})
	require.NoError(t, err)
	_, err = NewAccountRoleAdmissionStore(d).Register(ctx, admissionTestRequest(12, 1, 1))
	require.NoError(t, err)
	// 判死在 Begin 收尾时就要读存档库,所以先摘掉故障,进 FREEZING 之后再装回去。
	broken := d.steps
	d.steps = broken.(brokenStoreSteps).directorySteps
	holdPlayerStoreForTest(t, ctx, d, 12)
	_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 102, PlayerId: 12, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	d.steps = broken
	_, err = s.Cancel(ctx, &dspb.CancelAccountRoleTradeRequest{ListingId: 102, PlayerId: 12, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.ErrorIs(t, err, errBrokenPlayerStore)
	require.NotZero(t, touched.Load())
}

// 有挂起:和今天被拒的 Begin 一样不留痕。挂起清了之后同一 listing 可以重来。
func TestTiDBRoleBeginBlockedLeavesNoTrace(t *testing.T) {
	ctx, d, db, fences, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, nil)
	fences.set(rolefence.PrepareBlocked)
	_, err := s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, owned.TradeListingId)
	require.Zero(t, tableRowsForTest(t, ctx, db, "account_role_trade"))
	require.EqualValues(t, 3, fenceVersionForTest(t, ctx, db, 11), "写令牌和清令牌各加 1")
	require.Equal(t, 1, fences.pushedCount())
	require.True(t, proto.Equal(&rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 1, Version: 3, HomeZone: 1}, fences.lastPushed()))
	fences.set(rolefence.PrepareClean)
	out, err := s.Begin(ctx, begin)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING, out.State)
	require.EqualValues(t, 5, fenceVersionForTest(t, ctx, db, 11))
}

// 不可达或 stale:准备令牌留着,入场不受影响。用同一 listing 重放接着做,版本不动。
func TestTiDBRoleBeginUnreachableKeepsPreparingAndReplayResumes(t *testing.T) {
	ctx, d, db, fences, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, testRoleFreezeVerifier())
	a := NewAccountRoleAdmissionStore(d)
	_, err := a.Register(ctx, admissionTestRequest(11, 1, 1))
	require.NoError(t, err)
	fences.set(rolefence.PrepareRetry)
	_, err = s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrRetryLater, "第 1 步已提交,结局已知,不能打成 Hold")
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_PREPARING, tradeStateForTest(t, ctx, db, 101))
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, owned.TradeListingId)
	require.Zero(t, fences.pushedCount())
	_, err = s.Confirm(ctx, &dspb.ConfirmAccountRoleTradeFreezeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.ErrorIs(t, err, accountdirectory.ErrRetryLater, "还没冻结完,不能确认")
	changed := proto.Clone(begin).(*dspb.BeginAccountRoleTradeRequest)
	changed.ExpectedOwnerRevision = 2
	_, err = s.Begin(ctx, changed)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "换载荷不能接着做")
	_, err = s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	require.Equal(t, 2, fences.preparedCount())
	require.True(t, proto.Equal(fences.prepared[0], fences.prepared[1]), "重放问的是同一个状态")
	require.EqualValues(t, 2, fenceVersionForTest(t, ctx, db, 11))
	fences.set(rolefence.PrepareClean)
	out, err := s.Begin(ctx, begin)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING, out.State)
	require.Len(t, out.Holders, 1, "第 1 步之前提交的登记要纳入本轮全集")
	require.EqualValues(t, 3, fenceVersionForTest(t, ctx, db, 11))
}

func TestTiDBRoleCancelFromPreparingThawsAndKeepsTombstone(t *testing.T) {
	ctx, d, db, fences, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, nil)
	fences.set(rolefence.PrepareRetry)
	_, err := s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	out, err := s.Cancel(ctx, &dspb.CancelAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED, out.State)
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, owned.TradeListingId)
	require.EqualValues(t, 3, fenceVersionForTest(t, ctx, db, 11))
	require.True(t, proto.Equal(&rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 1, Version: 3, HomeZone: 1}, fences.lastPushed()))
	fences.set(rolefence.PrepareClean)
	replayed, err := s.Begin(ctx, begin)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED, replayed.State, "取消之后原 Begin 只重放墓碑")
	require.EqualValues(t, 3, fenceVersionForTest(t, ctx, db, 11))
}

// 每个改归属字段的事务加 1,别的事务不动。终态事务提交之后推送,同 tx_id 重放补推。
func TestTiDBRoleFenceVersionBumpsOnEveryOwnershipChangeAndTransferPushes(t *testing.T) {
	ctx, d, db, fences, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, testRoleFreezeVerifier())
	require.EqualValues(t, 1, fenceVersionForTest(t, ctx, db, 11), "建角从 1 开始")
	beginAndConfirmRole(t, s, ctx, begin.ListingId, 11, "seller", 1)
	require.EqualValues(t, 3, fenceVersionForTest(t, ctx, db, 11), "写准备令牌和转最终令牌各加 1,Confirm 不改令牌")
	require.Equal(t, 1, fences.pushedCount(), "转最终令牌推一次")
	transfer := &dspb.TransferAccountRoleRequest{TxId: 1001, ListingId: 101, PlayerId: 11, SellerAccount: "seller", BuyerAccount: "buyer", ExpectedOwnerRevision: 1}
	_, err := s.Transfer(ctx, transfer)
	require.NoError(t, err)
	require.EqualValues(t, 4, fenceVersionForTest(t, ctx, db, 11))
	sold := &rfpb.RoleFenceState{PlayerId: 11, Account: "buyer", OwnerRevision: 2, Version: 4, HomeZone: 1}
	require.Equal(t, 2, fences.pushedCount())
	require.True(t, proto.Equal(sold, fences.lastPushed()))
	_, err = s.Transfer(ctx, transfer)
	require.NoError(t, err)
	require.Equal(t, 3, fences.pushedCount(), "重放补推")
	require.True(t, proto.Equal(sold, fences.lastPushed()))
	require.EqualValues(t, 4, fenceVersionForTest(t, ctx, db, 11))
	_, err = d.Repair(ctx, "buyer", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11, ClassId: 3}, 2)
	require.NoError(t, err)
	require.EqualValues(t, 4, fenceVersionForTest(t, ctx, db, 11), "Repair 不改归属字段")
	require.NoError(t, d.Remove(ctx, "buyer", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 2}}))
	require.EqualValues(t, 6, fenceVersionForTest(t, ctx, db, 11), "写令牌和墓碑各加 1")
	tombstone := &rfpb.RoleFenceState{PlayerId: 11, OwnerRevision: 3, Version: 6, HomeZone: 1}
	require.True(t, proto.Equal(tombstone, fences.lastPushed()))
	state, err := d.RoleFenceState(ctx, 11)
	require.NoError(t, err)
	require.True(t, proto.Equal(tombstone, state), "已删除的角色回墓碑状态,参与者要能自愈成它")
	_, err = d.RoleFenceState(ctx, 999)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
	_, err = d.RoleFenceState(ctx, 0)
	require.ErrorIs(t, err, accountdirectory.ErrInvalid)
}

// 缺协调者时过户和删角拒绝,不能静默跳过参与者。取消只会放开限制,照常。
func TestTiDBRoleMissingCoordinatorRejectsOwnershipChanges(t *testing.T) {
	ctx, d, db, _, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, testRoleFreezeVerifier())
	beginAndConfirmRole(t, s, ctx, begin.ListingId, 11, "seller", 1)
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 12})
	require.NoError(t, err)
	d.ConfigureFenceCoordinator(nil)
	_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 102, PlayerId: 12, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	require.Equal(t, 1, tableRowsForTest(t, ctx, db, "account_role_trade"))
	_, err = s.Transfer(ctx, &dspb.TransferAccountRoleRequest{TxId: 1001, ListingId: 101, PlayerId: 11, SellerAccount: "seller", BuyerAccount: "buyer", ExpectedOwnerRevision: 1})
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	require.ErrorIs(t, d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 12, OwnerRevision: 1}}), accountdirectory.ErrUnavailable)
	require.Zero(t, tableRowsForTest(t, ctx, db, "account_role_removal"))
	owned, err := d.Lookup(ctx, 12)
	require.NoError(t, err)
	require.Zero(t, owned.TradeListingId)
	_, err = d.ResumeRemovals(ctx, time.Minute, 10)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	out, err := s.Cancel(ctx, &dspb.CancelAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED, out.State)
}

// 旧一轮 Prepare 的结论不算数:版本变了,finish 和 abort 都不能动新一轮。取消赢了就只重放。
func TestTiDBRolePrepareConclusionOfOlderRoundDoesNotCount(t *testing.T) {
	ctx, d, db, fences, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, nil)
	digest, err := roleTradeDigest(begin)
	require.NoError(t, err)
	fences.set(rolefence.PrepareRetry)
	_, err = s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	older := fences.lastPrepared()
	fences.set(rolefence.PrepareBlocked)
	_, err = s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	_, err = s.finishPrepare(ctx, begin, digest, older)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "别的重放已经撤销,收据没了")
	fences.set(rolefence.PrepareRetry)
	_, err = s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	version := fenceVersionForTest(t, ctx, db, 11)
	require.Greater(t, version, older.Version)
	_, err = s.finishPrepare(ctx, begin, digest, older)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	_, err = s.abortPrepare(ctx, begin, digest, older, accountdirectory.ErrConflict)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_PREPARING, tradeStateForTest(t, ctx, db, 101))
	require.Equal(t, version, fenceVersionForTest(t, ctx, db, 11))
	_, err = s.Cancel(ctx, &dspb.CancelAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	replayed, err := s.finishPrepare(ctx, begin, digest, fences.lastPrepared())
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED, replayed.State)
}

// 进 FREEZING 时才发现 holder 全集不合规:这笔冻结做不了,令牌不能留着,否则重放永远停在 PREPARING。
func TestTiDBRoleBeginHolderConflictAtFinishLeavesNoTrace(t *testing.T) {
	ctx, d, db, fences, _ := fenceRole(t)
	s := NewAccountRoleTradeStore(d, nil)
	_, err := NewAccountRoleAdmissionStore(d).Register(ctx, admissionTestRequest(11, 1, 1))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "UPDATE account_role SET owner_revision=2 WHERE player_id=11")
	require.NoError(t, err)
	_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 2})
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "开放登记属于旧修订")
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, owned.TradeListingId)
	require.Zero(t, tableRowsForTest(t, ctx, db, "account_role_trade"))
	require.Zero(t, tableRowsForTest(t, ctx, db, "account_role_trade_holder"))
	require.EqualValues(t, 3, fences.lastPushed().Version)
}

func TestTiDBRoleListingCannotTakeRemovalTokenRange(t *testing.T) {
	ctx, d, _, _, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, nil)
	begin.ListingId = rolefence.RemovalToken(11)
	_, err := s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrInvalid)
	_, err = s.Cancel(ctx, &dspb.CancelAccountRoleTradeRequest{ListingId: begin.ListingId, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.ErrorIs(t, err, accountdirectory.ErrInvalid)
}

func TestTiDBRoleRemovalBlockedClearsTokenAndSmallTable(t *testing.T) {
	ctx, d, db, fences, _ := fenceRole(t)
	removal := []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}
	fences.set(rolefence.PrepareBlocked)
	require.ErrorIs(t, d.Remove(ctx, "seller", removal), accountdirectory.ErrConflict)
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, owned.TradeListingId)
	require.EqualValues(t, 1, owned.OwnerRevision)
	require.Zero(t, tableRowsForTest(t, ctx, db, "account_role_removal"))
	require.EqualValues(t, 3, fenceVersionForTest(t, ctx, db, 11))
	require.True(t, proto.Equal(&rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 1, Version: 3, HomeZone: 1}, fences.lastPushed()))
	fences.set(rolefence.PrepareClean)
	require.NoError(t, d.Remove(ctx, "seller", removal))
	_, err = d.Lookup(ctx, 11)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
	require.NoError(t, d.Remove(ctx, "seller", removal), "相同删除幂等")
}

// 规格 §3.5:删角的令牌同样是准备令牌,投票期间不挡入场。墓碑事务持角色行锁再核一次登记,进了场就放弃,令牌不能留着。
func TestTiDBRoleRemovalEntryDuringPrepareAbandons(t *testing.T) {
	ctx, d, db, fences, _ := fenceRole(t)
	removal := []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}
	a := NewAccountRoleAdmissionStore(d)
	fences.set(rolefence.PrepareRetry)
	require.ErrorIs(t, d.Remove(ctx, "seller", removal), accountdirectory.ErrUnavailable)
	_, err := a.Register(ctx, admissionTestRequest(11, 1, 1))
	require.NoError(t, err, "投票期间照常进场")

	fences.set(rolefence.PrepareClean)
	require.ErrorIs(t, d.Remove(ctx, "seller", removal), accountdirectory.ErrConflict)
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err, "角色还在")
	require.EqualValues(t, 1, owned.OwnerRevision)
	state, err := d.RoleFenceState(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, state.FreezeToken)
	require.Zero(t, tableRowsForTest(t, ctx, db, "account_role_removal"))
	require.Zero(t, fences.lastPushed().FreezeToken, "放弃要解冻")

	_, err = a.CloseAdmissions(ctx, &dspb.CloseRoleAdmissionsRequest{PlayerId: 11, SceneInstanceId: admissionTestInstance, OwnerRevision: 1, ThroughGeneration: 1})
	require.NoError(t, err)
	require.NoError(t, d.Remove(ctx, "seller", removal))
	_, err = d.Lookup(ctx, 11)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
}

// 不可达:准备令牌留着,重放和后台都能接着做。没到时间的后台不碰。
func TestTiDBRoleRemovalUnreachableKeepsTokenAndBackgroundFinishes(t *testing.T) {
	ctx, d, db, fences, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, nil)
	removal := []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}
	token := rolefence.RemovalToken(11)
	fences.set(rolefence.PrepareRetry)
	require.ErrorIs(t, d.Remove(ctx, "seller", removal), accountdirectory.ErrRetryLater, "第 1 步已提交,结局已知,不能打成 Hold")
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, owned.TradeListingId, "准备令牌不出目录")
	require.Equal(t, "seller", owned.Account)
	require.Equal(t, 1, tableRowsForTest(t, ctx, db, "account_role_removal"))
	require.True(t, proto.Equal(&rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 1, FreezeToken: token, Version: 2, HomeZone: 1}, fences.lastPrepared()))
	_, err = d.Repair(ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11, ClassId: 3}, 1)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	_, err = s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.ErrorIs(t, d.Remove(ctx, "seller", removal), accountdirectory.ErrUnavailable)
	require.EqualValues(t, 2, fenceVersionForTest(t, ctx, db, 11), "重放只再问一遍")

	result, err := d.ResumeRemovals(ctx, 5*time.Minute, 10)
	require.NoError(t, err)
	require.Equal(t, RemovalResumeResult{}, result, "没到 5 分钟不碰")
	_, err = db.ExecContext(ctx, "UPDATE account_role_removal SET started_ms=started_ms-600000")
	require.NoError(t, err)
	result, err = d.ResumeRemovals(ctx, 5*time.Minute, 10)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	require.Equal(t, RemovalResumeResult{Failed: 1}, result, "还不可达就留给下一轮")
	fences.set(rolefence.PrepareClean)
	result, err = d.ResumeRemovals(ctx, 5*time.Minute, 10)
	require.NoError(t, err)
	require.Equal(t, RemovalResumeResult{Finished: 1}, result)
	_, err = d.Lookup(ctx, 11)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
	require.Zero(t, tableRowsForTest(t, ctx, db, "account_role_removal"))
	require.True(t, proto.Equal(&rfpb.RoleFenceState{PlayerId: 11, OwnerRevision: 2, Version: 3, HomeZone: 1}, fences.lastPushed()))
}

func TestTiDBRoleRemovalBackgroundAbandonsWhenBlocked(t *testing.T) {
	ctx, d, db, fences, _ := fenceRole(t)
	fences.set(rolefence.PrepareRetry)
	require.ErrorIs(t, d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}), accountdirectory.ErrUnavailable)
	_, err := db.ExecContext(ctx, "UPDATE account_role_removal SET started_ms=started_ms-600000")
	require.NoError(t, err)
	fences.set(rolefence.PrepareBlocked)
	result, err := d.ResumeRemovals(ctx, 5*time.Minute, 10)
	require.NoError(t, err)
	require.Equal(t, RemovalResumeResult{Abandoned: 1}, result)
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, owned.TradeListingId)
	require.Zero(t, tableRowsForTest(t, ctx, db, "account_role_removal"))
}

// 多名角色逐个走:前面的删了就是删了,后面的失败不回滚,原请求重放接着做。
func TestTiDBRoleRemovalWalksRolesOneByOne(t *testing.T) {
	ctx, d, _, fences, _ := fenceRole(t)
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 12})
	require.NoError(t, err)
	removal := []*dspb.AccountRoleVersion{{PlayerId: 12, OwnerRevision: 1}, {PlayerId: 11, OwnerRevision: 1}}
	fences.mu.Lock()
	fences.byPlayer = map[uint64]rolefence.PrepareOutcome{12: rolefence.PrepareBlocked}
	fences.mu.Unlock()
	// 规格 §3.6:11 已经写了墓碑,这一笔不是「什么都没写」,不能回确定性的拒绝。调用方重放,12 的结论到那时再说。
	// 11 的墓碑已提交,12 的拒绝已回滚,结局都已知,不打 Hold。
	err = d.Remove(ctx, "seller", removal)
	require.ErrorIs(t, err, accountdirectory.ErrRetryLater)
	require.NotErrorIs(t, err, accountdirectory.ErrConflict)
	_, err = d.Lookup(ctx, 11)
	require.ErrorIs(t, err, accountdirectory.ErrMissing, "按 player_id 升序,11 已经删了")
	owned, err := d.Lookup(ctx, 12)
	require.NoError(t, err)
	require.Zero(t, owned.TradeListingId)
	fences.mu.Lock()
	fences.byPlayer = nil
	fences.mu.Unlock()
	require.NoError(t, d.Remove(ctx, "seller", removal))
	_, err = d.Lookup(ctx, 12)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
	require.ErrorIs(t, d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}, {PlayerId: 11, OwnerRevision: 1}}), accountdirectory.ErrInvalid)
}

// 第一名角色就被拒:什么都没删,确定性的拒绝照旧。
func TestTiDBRoleRemovalFirstRoleRejectedStaysDefinite(t *testing.T) {
	ctx, d, _, fences, _ := fenceRole(t)
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 12})
	require.NoError(t, err)
	fences.mu.Lock()
	fences.byPlayer = map[uint64]rolefence.PrepareOutcome{11: rolefence.PrepareBlocked}
	fences.mu.Unlock()
	err = d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}, {PlayerId: 12, OwnerRevision: 1}})
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	for _, player := range []uint64{11, 12} {
		_, err = d.Lookup(ctx, player)
		require.NoError(t, err, "两名角色都还在")
	}
}

// pendingHolderRejectedSteps 让 Begin 提交 FREEZING 之后的判死回一个哨兵。
type pendingHolderRejectedSteps struct{ directorySteps }

func (pendingHolderRejectedSteps) hasPendingHolders(context.Context, uint64, uint64) (bool, error) {
	return false, accountdirectory.ErrConflict
}

// 规格 §3.6:FREEZING 已经提交,之后判死失败不能回确定性的拒绝:trade 会把它当成这笔没写。调用方重放拿收据。结局已知,不打 Hold。
func TestTiDBRoleBeginFailureAfterFreezingCommitIsNotADefiniteRejection(t *testing.T) {
	ctx, d, _, _, begin := fenceRole(t)
	s := NewAccountRoleTradeStore(d, nil)
	healthy := d.steps
	d.steps = pendingHolderRejectedSteps{directorySteps: healthy}
	_, err := s.Begin(ctx, begin)
	require.ErrorIs(t, err, accountdirectory.ErrRetryLater)
	require.NotErrorIs(t, err, accountdirectory.ErrConflict)
	d.steps = healthy
	out, err := s.Begin(ctx, begin)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING, out.State)
}
