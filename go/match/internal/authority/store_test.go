package authority

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"shared/authoritysql/sqltest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"match/internal/config"
	pb "proto/match"
	"schemamigrate"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	addr := os.Getenv("MATCH_IT_TIDB")
	if addr == "" {
		if os.Getenv("MATCH_IT_REQUIRE_TIDB") == "1" {
			t.Fatal("缺MATCH_IT_TIDB，显式TiDB验收不得跳过")
		}
		t.Skip("设置MATCH_IT_TIDB为已核任务所有的数值loopback端口启用隔离TiDB事务测试")
	}
	requireNativeTiDBAddress(t, addr)
	c := mysql.NewConfig()
	c.Net = "tcp"
	c.Addr = addr
	c.User = "root"
	c.Timeout, c.ReadTimeout, c.WriteTimeout = 3*time.Second, 3*time.Second, 3*time.Second
	c.Params = map[string]string{"tidb_txn_mode": "'pessimistic'", "transaction_isolation": "'READ-COMMITTED'", "tidb_read_staleness": "0", "tidb_low_resolution_tso": "OFF", "tidb_read_consistency": "'strict'", "tidb_replica_read": "'leader'", "tidb_snapshot": "''"}
	name := "matchit_" + uuid.NewString()[:8]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	opts, e := sqltest.OptionsFromEnv(c.FormatDSN(), name+"_writer", name)
	require.NoError(t, e)
	fixture, e := sqltest.New(ctx, t, opts)
	require.NoError(t, e)
	install, e := fixture.OpenSchema(ctx, name)
	require.NoError(t, e)
	var version, comment string
	require.NoError(t, install.QueryRowContext(ctx, "SELECT VERSION(),@@version_comment").Scan(&version, &comment))
	require.Contains(t, strings.ToLower(version+comment), "tidb", "隔离端口必须是真实TiDB，不能回退MySQL")
	_, e = schemamigrate.Up(ctx, install, schemamigrate.Options{Database: name, Tables: Tables()})
	require.NoError(t, e)
	require.NoError(t, fixture.GrantProtoTables(ctx, name, Tables()))
	db, e := fixture.Open(ctx, name, 16)
	require.NoError(t, e)
	// 本夹具只验证独立匹配SQL状态机，明确注入P与跨域准入替身；原生夹具覆盖实际原件围栏。
	return &Store{DB: db, Secret: []byte("fixture-only-probe-key-00000000000"), Config: config.GlobalMatchConf{Backend: "tidb", HardLatencyMs: 180, CrossRegionWaitSeconds: 30}, roleGuard: func(context.Context, *sql.Tx, []*pb.MatchAuthorityTicketRecord) error { return nil }, entryGuard: func(context.Context, *sql.Tx, []*pb.MatchAuthorityTicketRecord, *matchEntryOriginal) error {
		return nil
	}}
}

func requireNativeTiDBAddress(t *testing.T, address string) {
	t.Helper()
	host, portText, err := net.SplitHostPort(address)
	require.NoError(t, err)
	ip := net.ParseIP(host)
	require.NotNil(t, ip, "测试端点必须显式为数值loopback，不能跟随DNS")
	require.True(t, ip.IsLoopback())
	port, err := strconv.ParseUint(portText, 10, 16)
	require.NoError(t, err)
	require.NotZero(t, port)
}

func testJoin(t *testing.T, s *Store, pid uint64) *pb.MatchAuthorityTicketRecord {
	t.Helper()
	ctx := context.Background()
	n, e := s.Now(ctx)
	require.NoError(t, e)
	plan := fmt.Sprintf("plan-%d", pid)
	p := &pb.MatchNetworkProfile{PlanId: plan, SessionBinding: "bound", Receipts: []*pb.NetworkProbeReceipt{{Grant: &pb.NetworkProbeGrant{PlayerId: pid, PlanId: plan, SessionBinding: "bound", PhysicalRegion: 1, ExpiresAtMs: n + 120000}, RttMs: 50, SampleCount: 3, MeasuredAtMs: n, ExpiresAtMs: n + 120000}}}
	require.NoError(t, s.SavePlan(ctx, pid, p, n+120000))
	ticket, created, e := s.Join(ctx, &pb.MatchAuthorityTicketRecord{PlayerId: pid, Ticket: uuid.NewString(), RequestId: uuid.NewString(), Mode: 3, ConfigId: 1, TeamSize: 2, RatingCenti: 150000}, plan, "bound")
	require.NoError(t, e)
	require.True(t, created)
	return ticket
}

func TestAuthorityTiDBReplayAndClaimAtomicity(t *testing.T) {
	s := testStore(t)
	a, b := testJoin(t, s, 1), testJoin(t, s, 2)
	ctx := context.Background()
	replay, created, e := s.Join(ctx, a, "plan-1", "bound")
	require.NoError(t, e)
	require.False(t, created)
	require.Equal(t, a.EnqueuedMs, replay.EnqueuedMs)
	require.Equal(t, a.Ticket, replay.Ticket)
	start := make(chan struct{})
	var wg sync.WaitGroup
	out := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			out <- s.Claim(ctx, &pb.MatchFormationRecord{BattleId: uint64(100 + i), OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 3, TargetInstanceId: "instance", TargetEndpoint: "127.0.0.1:1", Mode: 3, ConfigId: 1}, []*pb.MatchAuthorityTicketRecord{a, b})
		}(i)
	}
	close(start)
	wg.Wait()
	close(out)
	wins := 0
	for e := range out {
		if e == nil {
			wins++
		}
	}
	require.Equal(t, 1, wins)
	afterA, e := s.Ticket(ctx, 1)
	require.NoError(t, e)
	afterB, e := s.Ticket(ctx, 2)
	require.NoError(t, e)
	require.Equal(t, uint32(2), afterA.State)
	require.Equal(t, afterA.FormationId, afterB.FormationId)
	require.NoError(t, s.Cancel(ctx, 1, a.Ticket))
	again, e := s.Ticket(ctx, 1)
	require.NoError(t, e)
	require.Equal(t, uint32(2), again.State)
}

func TestAuthorityTiDBCancelledMemberDoesNotPartiallyClaim(t *testing.T) {
	s := testStore(t)
	a, b := testJoin(t, s, 1), testJoin(t, s, 2)
	ctx := context.Background()
	require.NoError(t, s.Cancel(ctx, 2, b.Ticket))
	e := s.Claim(ctx, &pb.MatchFormationRecord{BattleId: 200, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 3, TargetInstanceId: "i", TargetEndpoint: "e", Mode: 3, ConfigId: 1}, []*pb.MatchAuthorityTicketRecord{a, b})
	require.Error(t, e)
	current, e := s.Ticket(ctx, 1)
	require.NoError(t, e)
	require.Equal(t, uint32(1), current.State)
	_, e = s.Formation(ctx, 200)
	require.ErrorIs(t, e, sql.ErrNoRows)
}

func TestProbeSignaturesRejectTamperingAndDomains(t *testing.T) {
	secret := []byte("fixture-only-secret-at-least-32bytes")
	g := &pb.NetworkProbeGrant{PlayerId: ^uint64(0), PlanId: "p", SessionBinding: "s", PhysicalRegion: 2, ExpiresAtMs: 9000}
	signed, e := Signed(secret, GrantDomain, g)
	require.NoError(t, e)
	require.NoError(t, Verify(secret, GrantDomain, signed, &pb.NetworkProbeGrant{}))
	require.Error(t, Verify(secret, ReceiptDomain, signed, &pb.NetworkProbeGrant{}))
	signed.Payload[0] ^= 1
	require.Error(t, Verify(secret, GrantDomain, signed, &pb.NetworkProbeGrant{}))
}

func TestAuthorityTiDBImmutableIntentAndFailedQueueKeepsWait(t *testing.T) {
	t.Run("开房前失败保留原票和等待时间", func(t *testing.T) {
		s := testStore(t)
		a, b := testJoin(t, s, 1), testJoin(t, s, 2)
		ctx := context.Background()
		f := &pb.MatchFormationRecord{BattleId: 300, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 3, TargetInstanceId: "original-instance", TargetEndpoint: "original-endpoint", Mode: 3, ConfigId: 1, RequeueOnFailure: true, ActivityContext: []byte("activity-owner")}
		require.NoError(t, s.Claim(ctx, f, []*pb.MatchAuthorityTicketRecord{a, b}))
		work, e := s.AcquireWork(ctx, 300)
		require.NoError(t, e)
		require.NoError(t, s.Unknown(ctx, work))
		require.NoError(t, s.Finish(ctx, work, false))
		for _, original := range []*pb.MatchAuthorityTicketRecord{a, b} {
			after, err := s.Ticket(ctx, original.PlayerId)
			require.NoError(t, err)
			require.Equal(t, uint32(1), after.State)
			require.Equal(t, original.Ticket, after.Ticket)
			require.Equal(t, original.EnqueuedMs, after.EnqueuedMs)
			require.Zero(t, after.FormationId)
		}
		// 迟到成功不能把已安全中止的旧成局重新变为ready。
		require.ErrorIs(t, s.Finish(ctx, work, true), ErrWorkLease)
		after, err := s.Ticket(ctx, 1)
		require.NoError(t, err)
		require.Equal(t, uint32(1), after.State)
	})

	s := testStore(t)
	a, b := testJoin(t, s, 1), testJoin(t, s, 2)
	ctx := context.Background()
	f := &pb.MatchFormationRecord{BattleId: 300, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 3, TargetInstanceId: "original-instance", TargetEndpoint: "original-endpoint", Mode: 3, ConfigId: 1, RequeueOnFailure: true, ActivityContext: []byte("activity-owner")}
	require.NoError(t, s.Claim(ctx, f, []*pb.MatchAuthorityTicketRecord{a, b}))
	work, e := s.AcquireWork(ctx, 300)
	require.NoError(t, e)
	first, e := s.SaveIntent(ctx, work, []byte("canonical-create"))
	require.NoError(t, e)
	require.Equal(t, []byte("canonical-create"), first)
	replayed, e := s.SaveIntent(ctx, work, first)
	require.NoError(t, e)
	require.Equal(t, first, replayed)
	require.NoError(t, s.Unknown(ctx, work))
	recovered, e := s.Formation(ctx, 300)
	require.NoError(t, e)
	require.Equal(t, uint32(3), recovered.State)
	require.Equal(t, "original-instance", recovered.TargetInstanceId)
	require.Equal(t, []byte("activity-owner"), recovered.ActivityContext)
	beforeTickets := make([]*pb.MatchAuthorityTicketRecord, 0, 2)
	for _, original := range []*pb.MatchAuthorityTicketRecord{a, b} {
		before, err := s.Ticket(ctx, original.PlayerId)
		require.NoError(t, err)
		beforeTickets = append(beforeTickets, before)
	}
	_, e = s.SaveIntent(ctx, work, []byte("different-late-create"))
	require.Error(t, e, "迟到worker不得替换已经持久化的seed/快照/目标")
	require.Error(t, s.Finish(ctx, work, false), "已有Create原件不能仅凭运行时失败释放原票，必须取得唯一SQL终局")
	afterFormation, e := s.Formation(ctx, 300)
	require.NoError(t, e)
	require.True(t, proto.Equal(recovered, afterFormation), "原件冲突和无SQL终局失败均不得改变成局")
	for _, before := range beforeTickets {
		after, err := s.Ticket(ctx, before.PlayerId)
		require.NoError(t, err)
		require.True(t, proto.Equal(before, after), "不得释放或替换任一成员原票")
	}
}

func TestAuthorityTiDBWholePartyReservationRollsBackOnBusyMember(t *testing.T) {
	s := testStore(t)
	busy := testJoin(t, s, 2)
	ctx := context.Background()
	testJoin(t, s, 1)
	require.NoError(t, s.Cancel(ctx, 1, (func() string { v, _ := s.Ticket(ctx, 1); return v.Ticket })()))
	f := &pb.MatchFormationRecord{BattleId: 400, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 3, TargetInstanceId: "instance", TargetEndpoint: "endpoint", Mode: 5, ConfigId: 1}
	group := []*pb.MatchAuthorityTicketRecord{{PlayerId: 1, Ticket: "party-1", RequestId: "party", Mode: 5, ConfigId: 1, TeamSize: 2}, {PlayerId: 2, Ticket: "party-2", RequestId: "party", Mode: 5, ConfigId: 1, TeamSize: 2}}
	require.Error(t, s.ReserveGroup(ctx, f, group))
	after, e := s.Ticket(ctx, 1)
	require.NoError(t, e)
	require.Equal(t, uint32(4), after.State, "先锁到的空闲成员也不能留下半队占位")
	other, e := s.Ticket(ctx, 2)
	require.NoError(t, e)
	require.Equal(t, busy.Ticket, other.Ticket)
	require.Equal(t, uint32(1), other.State)
}

func TestAuthorityTiDBCancelUnknownJoinPreventsLateAndOldReplay(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	request := uuid.NewString()
	require.NoError(t, s.CancelIntent(ctx, 11, "", request))
	confirmed, err := s.RequestCancelled(ctx, 11, request)
	require.NoError(t, err)
	require.True(t, confirmed)
	_, _, err = s.Join(ctx, &pb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: uuid.NewString(), RequestId: request, Mode: 3, ConfigId: 1, TeamSize: 2}, "late-plan", "bound")
	require.ErrorIs(t, err, ErrJoinCancelled, "撤销先于迟到 Join；即使原探测已失效也先认墓碑")
	current, err := s.Ticket(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, current.State)

	newer := testJoin(t, s, 11)
	require.NoError(t, s.CancelIntent(ctx, 11, "", request), "重投旧取消不能摘掉新票")
	current, err = s.Ticket(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, newer.Ticket, current.Ticket)
	require.Equal(t, uint32(1), current.State)
	require.NoError(t, s.Cancel(ctx, 11, newer.Ticket))
	_, _, err = s.Join(ctx, newer, "plan-11", "bound")
	require.ErrorIs(t, err, ErrJoinCancelled, "换票后旧请求也不能复活")
}

func TestAuthorityTiDBJoinCancelCompeteOnSamePlayerLock(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seed := testJoin(t, s, 12)
	require.NoError(t, s.Cancel(ctx, 12, seed.Ticket))
	target := proto.Clone(seed).(*pb.MatchAuthorityTicketRecord)
	target.Ticket, target.RequestId = uuid.NewString(), uuid.NewString()
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; _, _, e := s.Join(ctx, target, "plan-12", "bound"); results <- e }()
	go func() { <-start; results <- s.CancelIntent(ctx, 12, "", target.RequestId) }()
	close(start)
	for range 2 {
		e := <-results
		if e != nil {
			require.ErrorIs(t, e, ErrJoinCancelled)
		}
	}
	confirmed, err := s.RequestCancelled(ctx, 12, target.RequestId)
	require.NoError(t, err)
	require.True(t, confirmed)
	current, err := s.Ticket(ctx, 12)
	require.NoError(t, err)
	require.NotEqual(t, uint32(1), current.State)
	_, _, err = s.Join(ctx, target, "plan-12", "bound")
	require.ErrorIs(t, err, ErrJoinCancelled)
}

func TestAuthorityTiDBDirectIntentCommitsWholeGroupAndReplaysOriginalFormation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, b := testJoin(t, s, 21), testJoin(t, s, 22)
	require.NoError(t, s.Cancel(ctx, 21, a.Ticket))
	require.NoError(t, s.Cancel(ctx, 22, b.Ticket))
	key, e := DirectUUIDKey(1, 21, uuid.NewString())
	require.NoError(t, e)
	group := []*pb.MatchAuthorityTicketRecord{{PlayerId: 21, Ticket: uuid.NewString(), RequestId: uuid.NewString(), Mode: 5, ConfigId: 1, TeamSize: 2}, {PlayerId: 22, Ticket: uuid.NewString(), RequestId: uuid.NewString(), Mode: 5, ConfigId: 1, TeamSize: 2}}
	f := &pb.MatchFormationRecord{BattleId: 510, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 3, TargetInstanceId: "instance", TargetEndpoint: "endpoint", Mode: 5, ConfigId: 1}
	first, e := s.ReserveDirect(ctx, key, []byte("original-roster-and-client-request"), f, group)
	require.NoError(t, e)
	for _, pid := range []uint64{21, 22} {
		current, e := s.Ticket(ctx, pid)
		require.NoError(t, e)
		require.Equal(t, uint32(2), current.State)
		require.Equal(t, uint64(510), current.FormationId)
	}
	replayed, e := s.ReserveDirect(ctx, key, []byte("original-roster-and-client-request"), &pb.MatchFormationRecord{BattleId: 511}, group)
	require.NoError(t, e)
	require.Equal(t, first.BattleId, replayed.BattleId)
	_, e = s.ReserveDirect(ctx, key, []byte("different-client-request"), f, group)
	require.Error(t, e)
	work, e := s.AcquireWork(ctx, 510)
	require.NoError(t, e)
	require.NoError(t, s.Finish(ctx, work, false))
	replayed, e = s.ReserveDirect(ctx, key, []byte("original-roster-and-client-request"), &pb.MatchFormationRecord{BattleId: 512}, group)
	require.NoError(t, e)
	require.Equal(t, uint32(5), replayed.State)
	_, e = s.Formation(ctx, 511)
	require.ErrorIs(t, e, sql.ErrNoRows)
	t.Run("cancelled-original-rejects-late-prepare-without-partial-tickets", func(t *testing.T) {
		s := testStore(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		a, b := testJoin(t, s, 101), testJoin(t, s, 102)
		require.NoError(t, s.Cancel(ctx, a.PlayerId, a.Ticket))
		require.NoError(t, s.Cancel(ctx, b.PlayerId, b.Ticket))
		beforeA, err := s.Ticket(ctx, a.PlayerId)
		require.NoError(t, err)
		beforeB, err := s.Ticket(ctx, b.PlayerId)
		require.NoError(t, err)
		key := &pb.MatchDirectIntentRecord{Kind: 2, OwnerId: 44, RequestLow: 7001}
		body := []byte("permanent-cancel-original")
		receipt, err := s.CancelActivity(ctx, key, body)
		require.NoError(t, err)
		require.True(t, receipt.RequestCancelled)
		a.Mode, b.Mode = 5, 5
		formation := &pb.MatchFormationRecord{BattleId: 1701, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 3, TargetInstanceId: "instance", TargetEndpoint: "endpoint", Mode: 5, ConfigId: 1}
		_, err = s.PrepareDirectAdmission(ctx, key, body, formation, []*pb.MatchAuthorityTicketRecord{a, b})
		require.ErrorIs(t, err, ErrDirectCancelled)
		_, err = s.PrepareDirectAdmission(ctx, key, []byte("changed-original"), formation, []*pb.MatchAuthorityTicketRecord{a, b})
		require.ErrorIs(t, err, ErrDirectConflict)
		actualA, err := s.Ticket(ctx, a.PlayerId)
		require.NoError(t, err)
		actualB, err := s.Ticket(ctx, b.PlayerId)
		require.NoError(t, err)
		require.True(t, proto.Equal(beforeA, actualA))
		require.True(t, proto.Equal(beforeB, actualB))
		current, err := s.DirectAdmission(ctx, key)
		require.NoError(t, err)
		require.NotNil(t, current)
		require.True(t, current.Cancelled)
		require.False(t, current.ExitPrepared)
		require.Zero(t, current.BattleId)
		require.Equal(t, body, current.Payload)
		var formations int
		require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_formation").Scan(&formations))
		require.Zero(t, formations)
		replay, err := s.CancelActivity(ctx, key, body)
		require.NoError(t, err)
		require.Equal(t, receipt, replay)
	})
	t.Run("concurrent-cancel-and-prepare-have-one-original-decision", func(t *testing.T) {
		s := testStore(t)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for n := uint64(1); n <= 8; n++ {
			a, b := testJoin(t, s, 200+n*2), testJoin(t, s, 201+n*2)
			require.NoError(t, s.Cancel(ctx, a.PlayerId, a.Ticket))
			require.NoError(t, s.Cancel(ctx, b.PlayerId, b.Ticket))
			beforeA, err := s.Ticket(ctx, a.PlayerId)
			require.NoError(t, err)
			beforeB, err := s.Ticket(ctx, b.PlayerId)
			require.NoError(t, err)
			a.Mode, b.Mode = 5, 5
			key := &pb.MatchDirectIntentRecord{Kind: 2, OwnerId: 44, RequestLow: 8000 + n}
			body := []byte("concurrent-original")
			formation := &pb.MatchFormationRecord{BattleId: 1800 + n, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 3, TargetInstanceId: "instance", TargetEndpoint: "endpoint", Mode: 5, ConfigId: 1}
			start := make(chan struct{})
			var wg sync.WaitGroup
			var reserveErr, cancelErr error
			var reserved *pb.MatchFormationRecord
			var receipt *pb.CancelActivityBattleResponse
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				reserved, reserveErr = s.ReserveDirect(ctx, key, body, formation, []*pb.MatchAuthorityTicketRecord{a, b})
			}()
			go func() {
				defer wg.Done()
				<-start
				receipt, cancelErr = s.CancelActivity(ctx, key, body)
			}()
			close(start)
			wg.Wait()
			require.NoError(t, cancelErr)
			require.NotNil(t, receipt)
			current, err := s.DirectAdmission(ctx, key)
			require.NoError(t, err)
			require.NotNil(t, current)
			require.Equal(t, body, current.Payload)
			actualA, err := s.Ticket(ctx, a.PlayerId)
			require.NoError(t, err)
			actualB, err := s.Ticket(ctx, b.PlayerId)
			require.NoError(t, err)
			var formations int
			require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_formation WHERE battle_id=?", formation.BattleId).Scan(&formations))
			if receipt.RequestCancelled {
				require.ErrorIs(t, reserveErr, ErrDirectCancelled)
				require.Nil(t, reserved)
				require.True(t, current.Cancelled)
				require.Zero(t, current.BattleId)
				require.Zero(t, formations)
				for _, ticket := range []*pb.MatchAuthorityTicketRecord{actualA, actualB} {
					require.Equal(t, uint32(4), ticket.State)
					require.Zero(t, ticket.FormationId)
				}
				if !current.ExitPrepared {
					require.True(t, proto.Equal(beforeA, actualA))
					require.True(t, proto.Equal(beforeB, actualB))
				}
			} else {
				require.NoError(t, reserveErr)
				require.NotNil(t, reserved)
				require.Equal(t, formation.BattleId, reserved.BattleId)
				require.Equal(t, formation.BattleId, receipt.BattleId)
				require.EqualValues(t, 1, receipt.FormationState)
				require.False(t, current.Cancelled)
				require.Equal(t, formation.BattleId, current.BattleId)
				require.Equal(t, 1, formations)
				for _, ticket := range []*pb.MatchAuthorityTicketRecord{actualA, actualB} {
					require.Equal(t, uint32(2), ticket.State)
					require.Equal(t, formation.BattleId, ticket.FormationId)
				}
			}
			replay, err := s.CancelActivity(ctx, key, body)
			require.NoError(t, err)
			require.Equal(t, receipt, replay)
			_, err = s.CancelActivity(ctx, key, []byte("changed-original"))
			require.ErrorIs(t, err, ErrDirectConflict)
		}
	})
}

func TestAuthorityTiDBDeclineTombstonePreventsLateAcceptAndPartialReservation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, b := testJoin(t, s, 31), testJoin(t, s, 32)
	require.NoError(t, s.Cancel(ctx, 31, a.Ticket))
	require.NoError(t, s.Cancel(ctx, 32, b.Ticket))
	key := &pb.MatchDirectIntentRecord{Kind: 3, OwnerId: 32, RequestLow: 900}
	won, e := s.DeclineDirect(ctx, key, []byte("declined"))
	require.NoError(t, e)
	require.True(t, won)
	f := &pb.MatchFormationRecord{BattleId: 600, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 3, TargetInstanceId: "instance", TargetEndpoint: "endpoint", Mode: 4, ConfigId: 1}
	group := []*pb.MatchAuthorityTicketRecord{{PlayerId: 31, Ticket: uuid.NewString(), RequestId: uuid.NewString(), Mode: 4, ConfigId: 1, TeamSize: 2}, {PlayerId: 32, Ticket: uuid.NewString(), RequestId: uuid.NewString(), Mode: 4, ConfigId: 1, TeamSize: 2}}
	_, e = s.ReserveDirect(ctx, key, []byte("accepted"), f, group)
	require.Error(t, e)
	for _, pid := range []uint64{31, 32} {
		current, e := s.Ticket(ctx, pid)
		require.NoError(t, e)
		require.Equal(t, uint32(4), current.State)
	}
	current, payload, e := s.FindDirect(ctx, key)
	require.NoError(t, e)
	require.Nil(t, current)
	require.Equal(t, []byte("declined"), payload)
}

func TestAuthorityInvalidRequestIDFailsBeforeDatabaseAccess(t *testing.T) {
	s := &Store{}
	for _, request := range []string{"not-a-uuid", uuid.Nil.String(), "fF000000-0000-0000-0000-000000000001"} {
		_, _, e := s.Join(context.Background(), &pb.MatchAuthorityTicketRecord{RequestId: request}, "", "")
		require.Error(t, e)
		require.Error(t, s.CancelIntent(context.Background(), 1, "", request))
		_, e = s.RequestCancelled(context.Background(), 1, request)
		require.Error(t, e)
	}
}

func TestAuthorityTiDBOnlyOneReplicaWorksAndExpiredWorkerCannotCommit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, b := testJoin(t, s, 71), testJoin(t, s, 72)
	f := &pb.MatchFormationRecord{BattleId: 701, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 3, TargetInstanceId: "instance", TargetEndpoint: "endpoint", Mode: 3, ConfigId: 1}
	require.NoError(t, s.Claim(ctx, f, []*pb.MatchAuthorityTicketRecord{a, b}))
	work, e := s.AcquireWork(ctx, 701)
	require.NoError(t, e)
	other := &Store{DB: s.DB, Config: s.Config}
	_, e = other.AcquireWork(ctx, 701)
	require.ErrorIs(t, e, ErrWorkLease)
	require.NoError(t, s.Prepared(ctx, work, 71, "original-scene"))
	// 故障注入模拟原租约已经过期；下一个副本只能中止或查询同一成局，不能重新备战。
	_, e = s.DB.ExecContext(ctx, "UPDATE match_formation SET retry_at_ms=1 WHERE battle_id=701")
	require.NoError(t, e)
	stale := proto.Clone(work).(*pb.MatchFormationRecord)
	stale.RetryAtMs = 1
	recovered, e := other.AcquireWork(ctx, 701)
	require.NoError(t, e)
	require.Equal(t, uint32(3), recovered.State)
	require.ErrorIs(t, s.CheckWork(ctx, stale), ErrWorkLease)
	require.ErrorIs(t, s.Prepared(ctx, stale, 72, "late-scene"), ErrWorkLease)
	_, e = s.SaveIntent(ctx, stale, []byte("late-create"))
	require.ErrorIs(t, e, ErrWorkLease)
	require.ErrorIs(t, s.Unknown(ctx, stale), ErrWorkLease)
	require.ErrorIs(t, s.Finish(ctx, stale, true), ErrWorkLease)
	require.NoError(t, other.Finish(ctx, recovered, false))
	ticket, e := s.Ticket(ctx, 71)
	require.NoError(t, e)
	require.Equal(t, uint32(4), ticket.State)
}

func TestAuthorityTiDBAcceptAndDeclineShareOneDecision(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, b := testJoin(t, s, 81), testJoin(t, s, 82)
	require.NoError(t, s.Cancel(ctx, 81, a.Ticket))
	require.NoError(t, s.Cancel(ctx, 82, b.Ticket))
	key := &pb.MatchDirectIntentRecord{Kind: 3, OwnerId: 82, RequestLow: 801}
	f := &pb.MatchFormationRecord{BattleId: 801, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 3, TargetInstanceId: "instance", TargetEndpoint: "endpoint", Mode: 3, ConfigId: 1}
	start := make(chan struct{})
	accepted, declined := make(chan bool, 1), make(chan bool, 1)
	go func() {
		<-start
		_, e := s.ReserveDirect(ctx, key, []byte("accept"), f, []*pb.MatchAuthorityTicketRecord{a, b})
		accepted <- e == nil
	}()
	go func() { <-start; ok, e := s.DeclineDirect(ctx, key, []byte("decline")); declined <- e == nil && ok }()
	close(start)
	yes, no := <-accepted, <-declined
	require.NotEqual(t, yes, no)
	one, e := s.Ticket(ctx, 81)
	require.NoError(t, e)
	two, e := s.Ticket(ctx, 82)
	require.NoError(t, e)
	require.Equal(t, one.State, two.State)
	if yes {
		require.Equal(t, uint32(2), one.State)
	} else {
		require.Equal(t, uint32(4), one.State)
	}
}
