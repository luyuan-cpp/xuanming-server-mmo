package rolefence

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	base "proto/common/base"
	dspb "proto/data_service"
	rfpb "proto/role_fence"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/serverbase"
	"shared/serviceauth"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// fakeFenceDB 只认本包发的两条语句,用内存行模拟「版本大的赢」和事务回滚。真实 SQL 的合并语义由 rolefence_tidb_test.go 钉住。
type fakeFenceDB struct {
	mu        sync.Mutex
	rows      map[uint64]Fence
	saved     map[uint64]Fence
	commits   int
	rollbacks int
	locked    []uint64
	applies   int
	lockErr   error
	applyErr  error
}

func newFakeFenceDB(t *testing.T) (*fakeFenceDB, *sql.DB) {
	t.Helper()
	f := &fakeFenceDB{rows: map[uint64]Fence{}}
	db := sql.OpenDB(f)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return f, db
}

func (f *fakeFenceDB) Connect(context.Context) (driver.Conn, error) { return &fakeFenceConn{f}, nil }
func (f *fakeFenceDB) Driver() driver.Driver                        { return nil }

type fakeFenceConn struct{ db *fakeFenceDB }

func (c *fakeFenceConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (c *fakeFenceConn) Close() error { return nil }
func (c *fakeFenceConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *fakeFenceConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	f := c.db
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = make(map[uint64]Fence, len(f.rows))
	for player, fence := range f.rows {
		f.saved[player] = fence
	}
	return &fakeFenceTx{f}, nil
}

// 保留原始类型:RemovalToken 的最高位过不了默认转换器,真驱动也有自己的 CheckNamedValue。
func (c *fakeFenceConn) CheckNamedValue(*driver.NamedValue) error { return nil }

type fakeFenceTx struct{ db *fakeFenceDB }

func (t *fakeFenceTx) Commit() error {
	t.db.mu.Lock()
	defer t.db.mu.Unlock()
	t.db.commits++
	t.db.saved = nil
	return nil
}
func (t *fakeFenceTx) Rollback() error {
	t.db.mu.Lock()
	defer t.db.mu.Unlock()
	t.db.rollbacks++
	t.db.rows = t.db.saved
	t.db.saved = nil
	return nil
}

func (c *fakeFenceConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	f := c.db
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	if query != applySQL {
		return nil, fmt.Errorf("unexpected statement: %s", query)
	}
	f.applies++
	incoming := Fence{
		PlayerID:      args[0].Value.(uint64),
		Account:       string(args[1].Value.([]byte)),
		OwnerRevision: args[2].Value.(uint64),
		FreezeToken:   args[3].Value.(uint64),
		FreezeFinal:   args[4].Value.(bool),
		Version:       args[5].Value.(uint64),
		HomeZone:      args[6].Value.(uint32),
	}
	if old, ok := f.rows[incoming.PlayerID]; ok && incoming.Version <= old.Version {
		return driver.RowsAffected(0), nil
	}
	f.rows[incoming.PlayerID] = incoming
	return driver.RowsAffected(1), nil
}

func (c *fakeFenceConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	f := c.db
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lockErr != nil {
		return nil, f.lockErr
	}
	if query != lockSQL {
		return nil, fmt.Errorf("unexpected statement: %s", query)
	}
	player := args[0].Value.(uint64)
	f.locked = append(f.locked, player)
	fence, ok := f.rows[player]
	return &fakeFenceRows{fence: fence, left: ok}, nil
}

type fakeFenceRows struct {
	fence Fence
	left  bool
}

func (r *fakeFenceRows) Columns() []string {
	return []string{"account", "owner_revision", "freeze_token", "freeze_final", "version", "home_zone"}
}
func (r *fakeFenceRows) Close() error { return nil }
func (r *fakeFenceRows) Next(dest []driver.Value) error {
	if !r.left {
		return io.EOF
	}
	r.left = false
	dest[0], dest[1], dest[2], dest[3], dest[4], dest[5] = []byte(r.fence.Account), r.fence.OwnerRevision, r.fence.FreezeToken, r.fence.FreezeFinal, r.fence.Version, int64(r.fence.HomeZone)
	return nil
}

var errDenied = errors.New("服务原来的拒绝错误")

func TestRemovalTokenStaysOutOfListingRange(t *testing.T) {
	const listing = uint64(1)<<63 - 1 // 发号器发的最大值
	if IsRemovalToken(listing) {
		t.Fatal("63 位以内的 listing id 不能被当成删角令牌")
	}
	token := RemovalToken(listing)
	if !IsRemovalToken(token) || token == listing {
		t.Fatalf("token %d", token)
	}
	if RemovalToken(11) != uint64(1)<<63|11 {
		t.Fatal("令牌格式是规格 §3.2 写死的")
	}
}

func TestSecretEnvNames(t *testing.T) {
	if got := SecretEnv("player_inbox"); got != "MMORPG_ROLE_FENCE_SECRET_PLAYER_INBOX" {
		t.Fatal(got)
	}
	if len(Callers) != len(Participants)+1 || Callers[len(Callers)-1] != "chat" {
		t.Fatalf("Callers 应该是参与者加 chat: %v", Callers)
	}
}

func TestCheck(t *testing.T) {
	owner := Fence{PlayerID: 11, Account: "seller", OwnerRevision: 5, Version: 9, HomeZone: 1}
	frozen := owner
	frozen.FreezeToken, frozen.FreezeFinal = 101, true
	preparing := owner
	preparing.FreezeToken = 101
	tombstone := Fence{PlayerID: 11, OwnerRevision: 6, Version: 10, HomeZone: 1}
	session := Expect{PlayerID: 11, Account: "seller", OwnerRevision: 5, Reject: errDenied}
	peer := Expect{PlayerID: 11, Reject: errDenied}
	boom := errors.New("sql failed")
	for _, tc := range []struct {
		name    string
		fence   Fence
		lockErr error
		expect  Expect
		want    error // nil 放行;errStale 该自愈;其余原样
	}{
		{name: "原身份相符", fence: owner, expect: session},
		{name: "没有原身份只判冻结和删除", fence: owner, expect: peer},
		{name: "缺行要自愈", fence: Fence{PlayerID: 11}, lockErr: ErrMissing, expect: session, want: errStale},
		{name: "SQL 失败原样返回", lockErr: boom, expect: session, want: boom},
		{name: "冻结要自愈,可能只是解冻推送丢了", fence: frozen, expect: session, want: errStale},
		{name: "对端冻结同样要自愈", fence: frozen, expect: peer, want: errStale},
		{name: "自己持有的令牌不算冻结", fence: frozen, expect: Expect{PlayerID: 11, Account: "seller", OwnerRevision: 5, AllowToken: 101, Reject: errDenied}},
		{name: "别的令牌仍算冻结", fence: frozen, expect: Expect{PlayerID: 11, Account: "seller", OwnerRevision: 5, AllowToken: 102, Reject: errDenied}, want: errStale},
		{name: "投票期间的准备令牌同样拦业务写入", fence: preparing, expect: session, want: errStale},
		{name: "投票期间对端同样拦", fence: preparing, expect: peer, want: errStale},
		{name: "这笔业务自己的准备令牌放行", fence: preparing, expect: Expect{PlayerID: 11, Account: "seller", OwnerRevision: 5, AllowToken: 101, Reject: errDenied}},
		{name: "旧主的会话直接拒绝,不自愈", fence: Fence{PlayerID: 11, Account: "buyer", OwnerRevision: 6, Version: 10, HomeZone: 1}, expect: session, want: errDenied},
		{name: "新主的会话副本落后,自愈", fence: owner, expect: Expect{PlayerID: 11, Account: "buyer", OwnerRevision: 6, Reject: errDenied}, want: errStale},
		{name: "修订号相等账号不等,拒绝", fence: owner, expect: Expect{PlayerID: 11, Account: "other", OwnerRevision: 5, Reject: errDenied}, want: errDenied},
		{name: "墓碑挡住旧会话", fence: tombstone, expect: session, want: errDenied},
		{name: "墓碑挡住对端", fence: tombstone, expect: peer, want: errDenied},
		{name: "没填 Reject 也不能放行", fence: tombstone, expect: Expect{PlayerID: 11}, want: ErrRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(tc.fence, tc.lockErr, tc.expect)
			var stale *StaleError
			if errors.As(err, &stale) {
				err = errStale
				if len(stale.Players) != 1 || stale.Players[0] != 11 {
					t.Fatalf("players %v", stale.Players)
				}
			}
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

var errStale = errors.New("stale")

func lockAllOn(t *testing.T, db *sql.DB, expects ...Expect) (map[uint64]Fence, error) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	return LockAll(context.Background(), tx, expects...)
}

func TestLockAllLocksAscendingAndCollectsEveryStalePlayer(t *testing.T) {
	f, db := newFakeFenceDB(t)
	f.rows[2] = Fence{PlayerID: 2, Account: "b", OwnerRevision: 1, Version: 1, HomeZone: 1}
	f.rows[3] = Fence{PlayerID: 3, Account: "c", OwnerRevision: 1, FreezeToken: 7, Version: 2, HomeZone: 1}
	_, err := lockAllOn(t, db,
		Expect{PlayerID: 3, Reject: errDenied},
		Expect{PlayerID: 1, Reject: errDenied},
		Expect{PlayerID: 2, Account: "b", OwnerRevision: 1, Reject: errDenied},
		Expect{PlayerID: 3, Account: "c", OwnerRevision: 1, Reject: errDenied},
	)
	var stale *StaleError
	if !errors.As(err, &stale) {
		t.Fatalf("err %v", err)
	}
	if fmt.Sprint(stale.Players) != "[1 3]" {
		t.Fatalf("一次收齐要自愈的角色,否则第二名角色会被当成真实拒绝: %v", stale.Players)
	}
	if !errors.Is(stale.Reject, errDenied) {
		t.Fatalf("reject %v", stale.Reject)
	}
	if fmt.Sprint(f.locked) != "[1 2 3]" {
		t.Fatalf("升序、每名角色只锁一次: %v", f.locked)
	}
}

func TestLockAllReturnsFencesAndStopsAtDefiniteRejection(t *testing.T) {
	f, db := newFakeFenceDB(t)
	f.rows[1] = Fence{PlayerID: 1, Account: "a", OwnerRevision: 1, Version: 1, HomeZone: 3}
	f.rows[2] = Fence{PlayerID: 2, Account: "b", OwnerRevision: 2, Version: 2, HomeZone: 1}
	fences, err := lockAllOn(t, db, Expect{PlayerID: 1, Reject: errDenied})
	if err != nil || fences[1].HomeZone != 3 {
		t.Fatalf("fences %v err %v", fences, err)
	}
	_, err = lockAllOn(t, db, Expect{PlayerID: 2, Account: "b", OwnerRevision: 1, Reject: errDenied}, Expect{PlayerID: 3, Reject: errDenied})
	if !errors.Is(err, errDenied) {
		t.Fatalf("修订号更大的是确定拒绝: %v", err)
	}
	if _, err = lockAllOn(t, db, Expect{Reject: errDenied}); !errors.Is(err, errDenied) {
		t.Fatalf("player 0: %v", err)
	}
}

func TestLockRejectsCorruptReplica(t *testing.T) {
	f, db := newFakeFenceDB(t)
	f.rows[1] = Fence{PlayerID: 1, Account: "", OwnerRevision: 1, FreezeToken: 9, Version: 1, HomeZone: 1}
	if _, err := lockAllOn(t, db, Expect{PlayerID: 1, Reject: errDenied}); err == nil || errors.Is(err, errDenied) {
		t.Fatalf("墓碑带令牌是损坏,不是业务拒绝: %v", err)
	}
}

func TestApplyOnlyWritesLargerVersion(t *testing.T) {
	f, db := newFakeFenceDB(t)
	ctx := context.Background()
	state := &rfpb.RoleFenceState{PlayerId: 1, Account: "a", OwnerRevision: 1, FreezeToken: 7, Version: 5, HomeZone: 1}
	if applied, err := Apply(ctx, db, state); err != nil || !applied {
		t.Fatalf("applied %v err %v", applied, err)
	}
	older := proto.Clone(state).(*rfpb.RoleFenceState)
	older.Version, older.FreezeToken = 4, 0
	if applied, err := Apply(ctx, db, older); err != nil || applied || f.rows[1].FreezeToken != 7 {
		t.Fatalf("延迟到达的旧状态不生效: applied %v err %v row %+v", applied, err, f.rows[1])
	}
	for _, bad := range []*rfpb.RoleFenceState{
		nil,
		{Account: "a", OwnerRevision: 1, Version: 1, HomeZone: 1},
		{PlayerId: 1, Account: "a", Version: 1, HomeZone: 1},
		{PlayerId: 1, Account: "a", OwnerRevision: 1, HomeZone: 1},
		{PlayerId: 1, Account: "a", OwnerRevision: 1, Version: 1},
		{PlayerId: 1, OwnerRevision: 1, FreezeToken: 7, Version: 1, HomeZone: 1},
		{PlayerId: 1, Account: "\xff", OwnerRevision: 1, Version: 1, HomeZone: 1},
		{PlayerId: 1, Account: "a", OwnerRevision: 1, FreezeFinal: true, Version: 1, HomeZone: 1},
	} {
		if _, err := Apply(ctx, db, bad); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("%v: %v", bad, err)
		}
	}
	tombstone := &rfpb.RoleFenceState{PlayerId: 1, OwnerRevision: 2, Version: 6, HomeZone: 1}
	if applied, err := Apply(ctx, db, tombstone); err != nil || !applied || f.rows[1].Account != "" {
		t.Fatalf("墓碑是合法状态: applied %v err %v", applied, err)
	}
}

// 赋值按列出的顺序生效,version 不排最后的话,后面的 IF 读到新值,其余列就不更新了。
func TestApplySQLAssignsVersionLast(t *testing.T) {
	last := strings.LastIndex(applySQL, "IF(VALUES(version)>version")
	if !strings.HasPrefix(applySQL[strings.LastIndex(applySQL[:last], ",")+1:], "version=IF(") {
		t.Fatalf("version must be assigned last: %s", applySQL)
	}
	if strings.Count(applySQL, "IF(VALUES(version)>version") != 7 {
		t.Fatalf("every column must be guarded: %s", applySQL)
	}
}

// 准备令牌转最终冻结时令牌不变,只有 freeze_final 和版本变:这一列也要跟着版本走。
func TestApplyCarriesFreezeFinal(t *testing.T) {
	f, db := newFakeFenceDB(t)
	ctx := context.Background()
	preparing := &rfpb.RoleFenceState{PlayerId: 1, Account: "a", OwnerRevision: 1, FreezeToken: 7, Version: 2, HomeZone: 1}
	if _, err := Apply(ctx, db, preparing); err != nil {
		t.Fatal(err)
	}
	if f.rows[1].FreezeFinal {
		t.Fatalf("row %+v", f.rows[1])
	}
	final := proto.Clone(preparing).(*rfpb.RoleFenceState)
	final.FreezeFinal, final.Version = true, 3
	if applied, err := Apply(ctx, db, final); err != nil || !applied {
		t.Fatalf("applied %v err %v", applied, err)
	}
	fences, err := lockAllOn(t, db, Expect{PlayerID: 1, AllowToken: 7, Reject: errDenied})
	if err != nil || !fences[1].FreezeFinal || fences[1].FreezeToken != 7 {
		t.Fatalf("fences %+v err %v", fences, err)
	}
	if applied, err := Apply(ctx, db, preparing); err != nil || applied || !f.rows[1].FreezeFinal {
		t.Fatalf("迟到的准备状态不能把最终冻结改回去: applied %v err %v row %+v", applied, err, f.rows[1])
	}
}

type fakeStateReader struct {
	states map[uint64]*rfpb.RoleFenceState
	err    error
	calls  []uint64
	onRead func()
}

func (r *fakeStateReader) GetRoleFenceState(_ context.Context, player uint64) (*rfpb.RoleFenceState, error) {
	r.calls = append(r.calls, player)
	if r.onRead != nil {
		r.onRead()
	}
	if r.err != nil {
		return nil, r.err
	}
	state, ok := r.states[player]
	if !ok {
		return nil, ErrRoleNotFound
	}
	return state, nil
}

func TestRunTxHealsBetweenAttemptsAndRetriesOnce(t *testing.T) {
	f, db := newFakeFenceDB(t)
	var events []string
	reader := &fakeStateReader{
		states: map[uint64]*rfpb.RoleFenceState{1: {PlayerId: 1, Account: "a", OwnerRevision: 1, Version: 1, HomeZone: 1}},
		onRead: func() { events = append(events, "rpc") },
	}
	attempts := 0
	err := NewGuard(db, reader).RunTx(context.Background(), func() error {
		attempts++
		events = append(events, "tx")
		_, err := lockAllOn(t, db, Expect{PlayerID: 1, Account: "a", OwnerRevision: 1, Reject: errDenied})
		return err
	})
	if err != nil || attempts != 2 {
		t.Fatalf("err %v attempts %d", err, attempts)
	}
	if fmt.Sprint(events) != "[tx rpc tx]" {
		t.Fatalf("RPC 只能在重试间隙: %v", events)
	}
	if f.rows[1].Version != 1 {
		t.Fatalf("没写副本: %+v", f.rows[1])
	}
}

func TestRunTxSecondStaleIsRealRejection(t *testing.T) {
	_, db := newFakeFenceDB(t)
	reader := &fakeStateReader{states: map[uint64]*rfpb.RoleFenceState{1: {PlayerId: 1, Account: "a", OwnerRevision: 1, FreezeToken: 7, Version: 2, HomeZone: 1}}}
	attempts := 0
	err := NewGuard(db, reader).RunTx(context.Background(), func() error {
		attempts++
		_, err := lockAllOn(t, db, Expect{PlayerID: 1, Account: "a", OwnerRevision: 1, Reject: errDenied})
		return err
	})
	if !errors.Is(err, errDenied) || attempts != 2 {
		t.Fatalf("真冻结:自愈之后还是冻结,回服务原来的错误。err %v attempts %d", err, attempts)
	}
	if _, stale := err.(*StaleError); stale {
		t.Fatal("StaleError 不能漏给调用方")
	}
}

func TestRunTxPassesOtherErrorsThroughWithoutRPC(t *testing.T) {
	_, db := newFakeFenceDB(t)
	reader := &fakeStateReader{}
	for _, want := range []error{nil, errDenied} {
		attempts := 0
		err := NewGuard(db, reader).RunTx(context.Background(), func() error {
			attempts++
			return want
		})
		if !errors.Is(err, want) || attempts != 1 {
			t.Fatalf("err %v attempts %d", err, attempts)
		}
	}
	if len(reader.calls) != 0 {
		t.Fatalf("calls %v", reader.calls)
	}
}

func TestRunTxHealFailureStopsWithoutRetry(t *testing.T) {
	_, db := newFakeFenceDB(t)
	attempts := 0
	action := func() error {
		attempts++
		return &StaleError{Players: []uint64{1, 2}, Reject: errDenied}
	}
	err := NewGuard(db, &fakeStateReader{err: ErrUnavailable}).RunTx(context.Background(), action)
	if !errors.Is(err, ErrUnavailable) || attempts != 1 {
		t.Fatalf("读不到权威状态是可重试失败,不是拒绝: err %v attempts %d", err, attempts)
	}
	if err = (*Guard)(nil).Heal(context.Background(), 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil guard: %v", err)
	}
}

// 规格 §3.6:自愈读不到目录时事务已回滚,置上观察位:调用方没映射、照旧回故障的,拦截器改回可重试的状态,不打 Hold。
// 目录里没有这名角色是确定的结论,由调用方判,不置。
func TestRunTxHealUnavailableNotesNoWriteRetry(t *testing.T) {
	_, db := newFakeFenceDB(t)
	stale := func() error { return &StaleError{Players: []uint64{1}, Reject: errDenied} }

	ctx, _ := authoritysql.ObserveWriterFrozen(context.Background())
	if err := NewGuard(db, &fakeStateReader{err: errors.Join(ErrUnavailable, errors.New("data_service down"))}).RunTx(ctx, stale); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err %v", err)
	}
	if !authoritysql.NoWriteRetrySeen(ctx) {
		t.Fatal("读不到目录要置观察位")
	}

	ctx, _ = authoritysql.ObserveWriterFrozen(context.Background())
	if err := NewGuard(db, &fakeStateReader{}).RunTx(ctx, stale); !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("err %v", err)
	}
	if authoritysql.NoWriteRetrySeen(ctx) {
		t.Fatal("目录里没有这名角色不是读不到")
	}
}

// 目录没有的角色不写副本,其余照常写好,原样交给调用方:好友的对端不存在按原逻辑跳过。
func TestHealNotFoundWritesNothingForThatRole(t *testing.T) {
	f, db := newFakeFenceDB(t)
	reader := &fakeStateReader{states: map[uint64]*rfpb.RoleFenceState{2: {PlayerId: 2, Account: "b", OwnerRevision: 1, Version: 1, HomeZone: 1}}}
	err := NewGuard(db, reader).Heal(context.Background(), 1, 2)
	if !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("err %v", err)
	}
	var missing *RoleNotFoundError
	if !errors.As(err, &missing) || fmt.Sprint(missing.Players) != "[1]" {
		t.Fatalf("missing %+v", missing)
	}
	if _, ok := f.rows[1]; ok {
		t.Fatal("目录没有的角色不能写副本")
	}
	if _, ok := f.rows[2]; !ok {
		t.Fatal("其余角色要写好")
	}
}

func TestHealRejectsStateOfAnotherRole(t *testing.T) {
	f, db := newFakeFenceDB(t)
	reader := &fakeStateReader{states: map[uint64]*rfpb.RoleFenceState{1: {PlayerId: 2, Account: "b", OwnerRevision: 1, Version: 1, HomeZone: 1}}}
	if err := NewGuard(db, reader).Heal(context.Background(), 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err %v", err)
	}
	if len(f.rows) != 0 {
		t.Fatalf("rows %v", f.rows)
	}
}

var testCoordinatorSecret = strings.Repeat("c", 32)

func signedPrepare(t *testing.T, state *rfpb.RoleFenceState) *rfpb.PrepareRoleFreezeRequest {
	t.Helper()
	in := &rfpb.PrepareRoleFreezeRequest{State: state}
	if err := serviceauth.Sign(rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName, in, CoordinatorCaller, testCoordinatorSecret, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	return in
}

func signedApply(t *testing.T, state *rfpb.RoleFenceState) *rfpb.ApplyRoleFenceRequest {
	t.Helper()
	in := &rfpb.ApplyRoleFenceRequest{State: state}
	if err := serviceauth.Sign(rfpb.RoleFenceParticipant_ApplyRoleFence_FullMethodName, in, CoordinatorCaller, testCoordinatorSecret, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	return in
}

func frozenState(version uint64) *rfpb.RoleFenceState {
	return &rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 1, FreezeToken: 101, Version: version, HomeZone: 1}
}

func participantWith(t *testing.T, db *sql.DB, pending PendingFunc) *ParticipantServer {
	t.Helper()
	s, err := NewParticipantServer(db, pending, testCoordinatorSecret)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPrepareCleanCommitsFrozenReplica(t *testing.T) {
	f, db := newFakeFenceDB(t)
	var seen *rfpb.RoleFenceState
	s := participantWith(t, db, func(_ context.Context, tx *sql.Tx, state *rfpb.RoleFenceState) (string, error) {
		seen = state
		// 读挂起全集时已经持有副本行锁:这个事务里读到的是冻结之后的副本。
		if fence, err := Lock(context.Background(), tx, state.PlayerId); err != nil || fence.FreezeToken != state.FreezeToken {
			t.Errorf("fence %+v err %v", fence, err)
		}
		return "", nil
	})
	out, err := s.PrepareRoleFreeze(context.Background(), signedPrepare(t, frozenState(2)))
	if err != nil || !out.Clean || out.Stale || out.Blocker != "" {
		t.Fatalf("out %v err %v", out, err)
	}
	if !proto.Equal(seen, frozenState(2)) {
		t.Fatalf("pending saw %v", seen)
	}
	if f.commits != 1 || f.rows[11].FreezeToken != 101 || f.rows[11].Version != 2 {
		t.Fatalf("commits %d row %+v", f.commits, f.rows[11])
	}
	// 重放是同一个版本:Apply 不生效,再判一遍,照样通过。
	if out, err = s.PrepareRoleFreeze(context.Background(), signedPrepare(t, frozenState(2))); err != nil || !out.Clean {
		t.Fatalf("replay out %v err %v", out, err)
	}
}

func TestPrepareBlockedRollsBackSoThisLibraryDoesNotFreeze(t *testing.T) {
	f, db := newFakeFenceDB(t)
	f.rows[11] = Fence{PlayerID: 11, Account: "seller", OwnerRevision: 1, Version: 1, HomeZone: 1}
	s := participantWith(t, db, func(context.Context, *sql.Tx, *rfpb.RoleFenceState) (string, error) {
		return "guild_member", nil
	})
	out, err := s.PrepareRoleFreeze(context.Background(), signedPrepare(t, frozenState(2)))
	if err != nil || out.Clean || out.Blocker != "guild_member" {
		t.Fatalf("out %v err %v", out, err)
	}
	if f.rows[11].FreezeToken != 0 || f.rows[11].Version != 1 || f.commits != 0 {
		t.Fatalf("有挂起时本库不冻结: %+v commits %d", f.rows[11], f.commits)
	}
}

// 撤销版本加 1:解冻已经到了的副本上,在途的旧 Prepare 作废,不能把角色重新冻住。
func TestPrepareStaleWhenReplicaIsNewerSkipsPending(t *testing.T) {
	f, db := newFakeFenceDB(t)
	f.rows[11] = Fence{PlayerID: 11, Account: "seller", OwnerRevision: 1, Version: 3, HomeZone: 1}
	s := participantWith(t, db, func(context.Context, *sql.Tx, *rfpb.RoleFenceState) (string, error) {
		t.Error("过时的 Prepare 不用读挂起")
		return "", nil
	})
	out, err := s.PrepareRoleFreeze(context.Background(), signedPrepare(t, frozenState(2)))
	if err != nil || !out.Stale || out.Clean {
		t.Fatalf("out %v err %v", out, err)
	}
	if f.rows[11].Version != 3 || f.rows[11].FreezeToken != 0 {
		t.Fatalf("迟到的 Prepare 不能冻结: %+v", f.rows[11])
	}
}

// 查询失败不算通过:带内回 retry,协调者当不可达。回 gRPC 错误的话排空证明会被打成 Hold。
func TestPrepareFailuresAreNotClean(t *testing.T) {
	f, db := newFakeFenceDB(t)
	s := participantWith(t, db, func(context.Context, *sql.Tx, *rfpb.RoleFenceState) (string, error) {
		return "", errors.New("pending query failed")
	})
	out, err := s.PrepareRoleFreeze(context.Background(), signedPrepare(t, frozenState(2)))
	if err != nil || !out.Retry || out.Clean {
		t.Fatalf("out %v err %v", out, err)
	}
	if _, ok := f.rows[11]; ok {
		t.Fatal("失败要回滚")
	}
	f.applyErr = errors.New("write failed")
	if out, err = s.PrepareRoleFreeze(context.Background(), signedPrepare(t, frozenState(2))); err != nil || !out.Retry || out.Clean {
		t.Fatalf("out %v err %v", out, err)
	}
}

// 试探不写副本,事务回滚,只回挂起全集的结论。
func TestPrepareDryRunNeverTouchesReplica(t *testing.T) {
	blocker := ""
	f, db := newFakeFenceDB(t)
	f.rows[11] = Fence{PlayerID: 11, Account: "seller", OwnerRevision: 1, Version: 1, HomeZone: 1}
	s := participantWith(t, db, func(_ context.Context, _ *sql.Tx, state *rfpb.RoleFenceState) (string, error) {
		if state.FreezeToken != 101 {
			t.Errorf("挂起判定要拿到这笔的令牌: %v", state)
		}
		return blocker, nil
	})
	dryRun := func() *rfpb.PrepareRoleFreezeResponse {
		t.Helper()
		in := &rfpb.PrepareRoleFreezeRequest{State: frozenState(2), DryRun: true}
		if err := serviceauth.Sign(rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName, in, CoordinatorCaller, testCoordinatorSecret, uint64(time.Now().UnixMilli())); err != nil {
			t.Fatal(err)
		}
		out, err := s.PrepareRoleFreeze(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := dryRun(); !out.Clean || out.Blocker != "" {
		t.Fatalf("out %v", out)
	}
	blocker = "guild_member"
	if out := dryRun(); out.Clean || out.Blocker != "guild_member" {
		t.Fatalf("out %v", out)
	}
	if f.applies != 0 || f.commits != 0 || f.rollbacks != 2 {
		t.Fatalf("applies %d commits %d rollbacks %d", f.applies, f.commits, f.rollbacks)
	}
	if f.rows[11].FreezeToken != 0 || f.rows[11].Version != 1 {
		t.Fatalf("试探不能动副本: %+v", f.rows[11])
	}
}

func TestPrepareDryRunFailureIsRetry(t *testing.T) {
	_, db := newFakeFenceDB(t)
	s := participantWith(t, db, func(context.Context, *sql.Tx, *rfpb.RoleFenceState) (string, error) {
		return "", errors.New("pending query failed")
	})
	in := &rfpb.PrepareRoleFreezeRequest{State: frozenState(2), DryRun: true}
	if err := serviceauth.Sign(rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName, in, CoordinatorCaller, testCoordinatorSecret, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	out, err := s.PrepareRoleFreeze(context.Background(), in)
	if err != nil || !out.Retry || out.Clean {
		t.Fatalf("out %v err %v", out, err)
	}
}

// 规格 §3.4:参与者的内部失败幂等、有版本保护,没有结局未知,不能让排空证明 Hold。
func TestParticipantInternalFailuresDoNotHoldDrain(t *testing.T) {
	f, db := newFakeFenceDB(t)
	s := participantWith(t, db, func(context.Context, *sql.Tx, *rfpb.RoleFenceState) (string, error) {
		return "", errors.New("pending query failed")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gate := drainproof.New()
	admission := serverbase.RootAdmission(gate)
	prepare := func(ctx context.Context, req any) (any, error) {
		return s.PrepareRoleFreeze(ctx, req.(*rfpb.PrepareRoleFreezeRequest))
	}
	apply := func(ctx context.Context, req any) (any, error) {
		return s.ApplyRoleFence(ctx, req.(*rfpb.ApplyRoleFenceRequest))
	}
	if _, err := admission(ctx, signedPrepare(t, frozenState(2)), &grpc.UnaryServerInfo{FullMethod: rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName}, prepare); err != nil {
		t.Fatal(err)
	}
	f.applyErr = errors.New("write failed")
	if _, err := admission(ctx, signedPrepare(t, frozenState(2)), &grpc.UnaryServerInfo{FullMethod: rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName}, prepare); err != nil {
		t.Fatal(err)
	}
	if _, err := admission(ctx, signedApply(t, frozenState(3)), &grpc.UnaryServerInfo{FullMethod: rfpb.RoleFenceParticipant_ApplyRoleFence_FullMethodName}, apply); err != nil {
		t.Fatal(err)
	}
	gate.CloseAdmission()
	if err := gate.Wait(ctx); err != nil {
		t.Fatalf("排空证明不该 Hold: %v", err)
	}
}

func TestPrepareRejectsBadInput(t *testing.T) {
	f, db := newFakeFenceDB(t)
	s := participantWith(t, db, NoPending)
	thawed := frozenState(2)
	thawed.FreezeToken = 0
	if _, err := s.PrepareRoleFreeze(context.Background(), signedPrepare(t, thawed)); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Prepare 必须带令牌: %v", err)
	}
	if _, err := s.PrepareRoleFreeze(context.Background(), &rfpb.PrepareRoleFreezeRequest{State: frozenState(2)}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("没签名: %v", err)
	}
	final := frozenState(2)
	final.FreezeFinal = true
	if _, err := s.PrepareRoleFreeze(context.Background(), signedPrepare(t, final)); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("最终冻结不用再问参与者: %v", err)
	}
	changed := signedPrepare(t, frozenState(2))
	changed.State.FreezeToken = 102
	if _, err := s.PrepareRoleFreeze(context.Background(), changed); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("改了正文: %v", err)
	}
	other := &rfpb.PrepareRoleFreezeRequest{State: frozenState(2)}
	if err := serviceauth.Sign(rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName, other, "guild", testCoordinatorSecret, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareRoleFreeze(context.Background(), other); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("不是协调者: %v", err)
	}
	if f.applies != 0 {
		t.Fatalf("拒绝的请求不能写副本: %d", f.applies)
	}
}

func TestApplyRoleFenceMergesByVersion(t *testing.T) {
	f, db := newFakeFenceDB(t)
	s := participantWith(t, db, NoPending)
	if _, err := s.ApplyRoleFence(context.Background(), signedApply(t, frozenState(3))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyRoleFence(context.Background(), signedApply(t, &rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 1, Version: 2, HomeZone: 1})); err != nil {
		t.Fatal(err)
	}
	if f.rows[11].FreezeToken != 101 {
		t.Fatalf("乱序到达的旧解冻不生效: %+v", f.rows[11])
	}
	if _, err := s.ApplyRoleFence(context.Background(), &rfpb.ApplyRoleFenceRequest{State: frozenState(9)}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("没签名: %v", err)
	}
	if _, err := s.ApplyRoleFence(context.Background(), signedApply(t, &rfpb.RoleFenceState{PlayerId: 11})); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("非法状态: %v", err)
	}
	f.applyErr = errors.New("write failed")
	out, err := s.ApplyRoleFence(context.Background(), signedApply(t, frozenState(9)))
	if err != nil || !out.Retry {
		t.Fatalf("写失败带内回 retry: out %v err %v", out, err)
	}
}

func TestNewParticipantServerRequiresEverything(t *testing.T) {
	_, db := newFakeFenceDB(t)
	if _, err := NewParticipantServer(nil, NoPending, testCoordinatorSecret); err == nil {
		t.Fatal("nil db")
	}
	if _, err := NewParticipantServer(db, nil, testCoordinatorSecret); err == nil {
		t.Fatal("没注入挂起判定不能算没有挂起")
	}
	if _, err := NewParticipantServer(db, NoPending, "short"); err == nil {
		t.Fatal("short secret")
	}
}

type fakeDirectoryRPC struct {
	dspb.DataServiceClient
	in       *dspb.GetRoleFenceStateRequest
	out      *dspb.GetRoleFenceStateResponse
	admitOut *dspb.VerifyRoleAdmissionResponse
	err      error
}

func (f *fakeDirectoryRPC) GetRoleFenceState(_ context.Context, in *dspb.GetRoleFenceStateRequest, _ ...grpc.CallOption) (*dspb.GetRoleFenceStateResponse, error) {
	f.in = in
	return f.out, f.err
}

func (f *fakeDirectoryRPC) VerifyRoleAdmission(context.Context, *dspb.VerifyRoleAdmissionRequest, ...grpc.CallOption) (*dspb.VerifyRoleAdmissionResponse, error) {
	return f.admitOut, f.err
}

func TestDirectoryReaderSignsAndClassifies(t *testing.T) {
	secret := strings.Repeat("g", 32)
	state := &rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 1, FreezeToken: 101, Version: 2, HomeZone: 1}
	rpc := &fakeDirectoryRPC{out: &dspb.GetRoleFenceStateResponse{Found: true, State: state}}
	reader, err := NewDirectoryReader(rpc, "guild", secret)
	if err != nil {
		t.Fatal(err)
	}
	out, err := reader.GetRoleFenceState(context.Background(), 11)
	if err != nil || !proto.Equal(out, state) {
		t.Fatalf("out %v err %v", out, err)
	}
	auth, err := serviceauth.New(map[string]string{"guild": secret})
	if err != nil {
		t.Fatal(err)
	}
	if caller, err := auth.Verify(context.Background(), accountdirectory.MethodPrefix+"GetRoleFenceState", rpc.in); err != nil || caller != "guild" {
		t.Fatalf("caller %q err %v", caller, err)
	}
	rpc.out = &dspb.GetRoleFenceStateResponse{}
	if _, err = reader.GetRoleFenceState(context.Background(), 11); !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("not found: %v", err)
	}
	rpc.out = &dspb.GetRoleFenceStateResponse{Retry: true}
	if _, err = reader.GetRoleFenceState(context.Background(), 11); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("读不到不是没有这名角色: %v", err)
	}
	rpc.out = nil
	for _, code := range []codes.Code{codes.Unavailable, codes.PermissionDenied, codes.DeadlineExceeded, codes.NotFound} {
		rpc.err = status.Error(code, "x")
		if _, err = reader.GetRoleFenceState(context.Background(), 11); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrRoleNotFound) {
			t.Fatalf("%v: %v", code, err)
		}
	}
	rpc.err = nil
	rpc.out = &dspb.GetRoleFenceStateResponse{Found: true, State: &rfpb.RoleFenceState{PlayerId: 12, Account: "seller", OwnerRevision: 1, Version: 2, HomeZone: 1}}
	if _, err = reader.GetRoleFenceState(context.Background(), 11); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("别人的状态: %v", err)
	}
	rpc.out = &dspb.GetRoleFenceStateResponse{Found: true}
	if _, err = reader.GetRoleFenceState(context.Background(), 11); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("found 却没有状态: %v", err)
	}
	if _, err = NewDirectoryReader(rpc, "guild", "short"); err == nil {
		t.Fatal("short secret")
	}
	if _, err = NewDirectoryReader(nil, "guild", secret); err == nil {
		t.Fatal("nil rpc")
	}
}

func TestAdmissionVerifierClassifies(t *testing.T) {
	rpc := &fakeDirectoryRPC{admitOut: &dspb.VerifyRoleAdmissionResponse{Valid: true}}
	verifier, err := NewAdmissionVerifier(rpc, "match", strings.Repeat("g", 32))
	if err != nil {
		t.Fatal(err)
	}
	if valid, err := verifier.VerifyRoleAdmission(context.Background(), &base.RoleAdmissionIdentity{PlayerId: 11}); err != nil || !valid {
		t.Fatalf("valid %v err %v", valid, err)
	}
	rpc.admitOut = &dspb.VerifyRoleAdmissionResponse{}
	if valid, err := verifier.VerifyRoleAdmission(context.Background(), &base.RoleAdmissionIdentity{PlayerId: 11}); err != nil || valid {
		t.Fatalf("登记失效是答复,不是错误: valid %v err %v", valid, err)
	}
	rpc.admitOut = &dspb.VerifyRoleAdmissionResponse{Retry: true}
	if _, err := verifier.VerifyRoleAdmission(context.Background(), &base.RoleAdmissionIdentity{PlayerId: 11}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("读不到不是登记失效: %v", err)
	}
}
