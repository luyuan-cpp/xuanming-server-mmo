package team

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"match/internal/authority"
	"match/internal/config"

	battlepb "proto/battle"
	base "proto/common/base"
	dspb "proto/data_service"
	guildpb "proto/guild"
	matchpb "proto/match"
	teampb "proto/team"
	"schemamigrate"
	"shared/placement"
	"shared/roleidentity"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type nativeTeamFixture struct {
	owner  *authority.Store
	store  *Store
	schema string
}

// 随机TiDB库，真实目录P/原账号修订/route与merge片。没有生产guard替身、Redis或外部残留。
func newNativeTeamFixture(t *testing.T) *nativeTeamFixture {
	t.Helper()
	address := os.Getenv("MATCH_IT_TIDB")
	if address == "" {
		if os.Getenv("MATCH_IT_REQUIRE_TIDB") == "1" {
			t.Fatal("缺MATCH_IT_TIDB，Team原生事务验收不得跳过")
		}
		t.Skip("显式设置已核任务所有的MATCH_IT_TIDB数值loopback地址启用隔离TiDB Team事务回归")
	}
	host, portText, err := net.SplitHostPort(address)
	require.NoError(t, err)
	ip := net.ParseIP(host)
	require.NotNil(t, ip)
	require.True(t, ip.IsLoopback())
	port, err := strconv.ParseUint(portText, 10, 16)
	require.NoError(t, err)
	require.NotZero(t, port)
	connection := mysql.NewConfig()
	connection.Net = "tcp"
	connection.Addr = address
	connection.User = "root"
	connection.Timeout, connection.ReadTimeout, connection.WriteTimeout = 3*time.Second, 5*time.Second, 5*time.Second
	connection.Params = map[string]string{"tidb_txn_mode": "'pessimistic'", "transaction_isolation": "'READ-COMMITTED'", "tidb_read_staleness": "0", "tidb_low_resolution_tso": "OFF", "tidb_replica_read": "'leader'", "tidb_read_consistency": "'strict'", "tidb_snapshot": "''"}
	admin, err := sql.Open("mysql", connection.FormatDSN())
	require.NoError(t, err)
	var engine string
	require.NoError(t, admin.QueryRow("SELECT VERSION()").Scan(&engine))
	require.Contains(t, strings.ToLower(engine), "tidb")
	schema := "teamit_" + uuid.NewString()[:8]
	_, err = admin.Exec("CREATE DATABASE `" + schema + "`")
	require.NoError(t, err)
	connection.DBName = schema
	db, err := sql.Open("mysql", connection.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() {
		db.Close()
		_, err := admin.Exec("DROP DATABASE `" + schema + "`")
		if err != nil {
			t.Errorf("清理Team隔离库: %v", err)
		}
		admin.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = schemamigrate.Up(ctx, db, schemamigrate.Options{Database: schema, Tables: []proto.Message{&matchpb.MatchAuthorityTicketRecord{}, &matchpb.MatchNetworkProfileRecord{}, &matchpb.MatchTicketRegionRecord{}, &matchpb.MatchJoinIntentRecord{}, &matchpb.MatchDirectIntentRecord{}, &matchpb.MatchFormationRecord{}, &matchpb.MatchSpectatorRecord{}, &matchpb.MatchSpectatorIntentRecord{}, &matchpb.MatchSpectatorHeadRecord{}, &teampb.TeamAuthorityRecord{}, &teampb.TeamMembershipRecord{}, &teampb.TeamInviteIndexRecord{}, &teampb.TeamApplicationIndexRecord{}, &teampb.TeamActionReceiptRecord{}, &teampb.TeamLaunchRecord{}, &teampb.TeamLaunchRegionRecord{}, &teampb.TeamLaunchRoleRecord{}, &guildpb.GuildTrialLaunchRecord{}, &guildpb.GuildTrialLaunchRoleRecord{}, &guildpb.GuildTrialLobbyRecord{}, &guildpb.GuildTrialLobbySlotRecord{}}})
	require.NoError(t, err)
	for _, statement := range []string{
		"CREATE TABLE account_role(player_id BIGINT UNSIGNED PRIMARY KEY,account VARBINARY(191) NOT NULL,owner_revision BIGINT UNSIGNED NOT NULL,trade_listing_id BIGINT UNSIGNED NOT NULL DEFAULT 0)",
		"CREATE TABLE route_meta(id INT PRIMARY KEY,writable INT NOT NULL)",
		"INSERT INTO route_meta VALUES(1,1)",
		"CREATE TABLE player_route(route_shard INT UNSIGNED NOT NULL,player_id BIGINT UNSIGNED NOT NULL,home_zone INT UNSIGNED NULL,rev BIGINT UNSIGNED NOT NULL,frozen_run_id VARBINARY(96) NULL,PRIMARY KEY(route_shard,player_id))",
		"CREATE TABLE zone_merge_fence(zone_id INT UNSIGNED NOT NULL,shard INT UNSIGNED NOT NULL,run_id VARBINARY(96) NULL,expires_at_ms BIGINT UNSIGNED NULL,PRIMARY KEY(zone_id,shard))",
	} {
		_, err = db.ExecContext(ctx, statement)
		require.NoError(t, err)
	}
	for player := uint64(1); player <= 16; player++ {
		_, err = db.ExecContext(ctx, "INSERT INTO account_role VALUES(?,?,7,0)", player, fmt.Sprintf("team-original-%d", player))
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, "INSERT INTO player_route VALUES(?,?,1,1,NULL)", placement.RouteRowShard(player), player)
		require.NoError(t, err)
	}
	owner := &authority.Store{DB: db, Secret: []byte("fixture-only-network-secret-0000000000"), Config: config.GlobalMatchConf{RequireTiDB: true, HardLatencyMs: 180, CrossRegionWaitSeconds: 30}, TeamConfig: config.TeamConf{AllowCrossZone: true}}
	require.NoError(t, owner.ConfigureRoleAuthority(ctx, schema))
	require.NoError(t, owner.ConfigureMatchEntryAuthority(ctx, schema))
	require.NoError(t, owner.CheckTeamRouteContract(ctx))
	loader := func(_ context.Context, players []uint64) Sessions {
		out := Sessions{}
		for _, player := range players {
			out[player] = SessionOnline
		}
		return out
	}
	return &nativeTeamFixture{owner: owner, store: NewSQLStore(owner, loader, RuleConfig{AllowCrossZone: true}), schema: schema}
}

func (f *nativeTeamFixture) actor(t *testing.T, player uint64) context.Context {
	t.Helper()
	ctx, err := roleidentity.Capture(context.Background(), &dspb.AccountRoleOwnership{Account: fmt.Sprintf("team-original-%d", player), OwnerRevision: 7, Role: &base.AccountSimplePlayer{PlayerId: player}})
	require.NoError(t, err)
	return ctx
}

func fixtureTeamAction(t *testing.T, ctx context.Context, player uint64, kind OpKind, request proto.Message) context.Context {
	t.Helper()
	actor, err := teamActor(ctx, player)
	require.NoError(t, err)
	id, err := uuid.Parse(request.(interface{ GetRequestId() string }).GetRequestId())
	require.NoError(t, err)
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	require.NoError(t, err)
	return context.WithValue(ctx, sqlActionKey{}, &sqlAction{actor: actor, high: binary.BigEndian.Uint64(id[:8]), low: binary.BigEndian.Uint64(id[8:]), method: uint32(kind), payload: payload})
}

func (f *nativeTeamFixture) create(t *testing.T, player, teamID uint64) {
	t.Helper()
	request := &teampb.CreateTeamRequest{RequestId: uuid.NewString()}
	ctx := fixtureTeamAction(t, f.actor(t, player), player, OpCreate, request)
	result, err := f.store.Mutate(ctx, BindCreate(player, teamID), CreateOp(player, teamID, 1))
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
}

func (f *nativeTeamFixture) inviteAndAccept(t *testing.T, leader, member, teamID uint64) {
	t.Helper()
	invite := &teampb.InviteToTeamRequest{TargetPlayerId: member, ExpectedTeamId: teamID, RequestId: uuid.NewString()}
	ctx := fixtureTeamAction(t, f.actor(t, leader), leader, OpInvite, invite)
	result, err := f.store.Mutate(ctx, BindCaller(leader, teamID), InviteOp(leader, member, 1))
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	accept := &teampb.RespondInviteRequest{TeamId: teamID, Accept: true, RequestId: uuid.NewString()}
	ctx = fixtureTeamAction(t, f.actor(t, member), member, OpRespondInvite, accept)
	result, err = f.store.Mutate(ctx, BindTarget(member, teamID), RespondInviteOp(member, true))
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
}

func TestTeamTiDBOriginalUUIDReplayAfterLeaveAndRestart(t *testing.T) {
	f := newNativeTeamFixture(t)
	request := &teampb.CreateTeamRequest{RequestId: uuid.NewString()}
	ctx := fixtureTeamAction(t, f.actor(t, 1), 1, OpCreate, request)
	first, err := f.store.Mutate(ctx, BindCreate(1, 101), CreateOp(1, 101, 1))
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, first.Outcome)
	originalEpoch := first.Commit.Indexes[1].Epoch
	// 模拟提交ACK丢失后另一个副本重新发号，但原UUID只能返回原结果。
	replayCtx := fixtureTeamAction(t, f.actor(t, 1), 1, OpCreate, request)
	_, err = f.store.Mutate(replayCtx, BindCreate(1, 202), CreateOp(1, 202, 1))
	require.NoError(t, err)
	replay := replayCtx.Value(sqlActionKey{}).(*sqlAction)
	require.True(t, replay.replayed)
	require.Equal(t, uint64(101), replay.committedResponse.Team.TeamId)
	leave := &teampb.LeaveTeamRequest{ExpectedTeamId: 101, RequestId: uuid.NewString()}
	leaveCtx := fixtureTeamAction(t, f.actor(t, 1), 1, OpLeave, leave)
	left, err := f.store.Mutate(leaveCtx, BindCaller(1, 101), LeaveOp(1))
	require.NoError(t, err)
	require.Greater(t, left.Commit.Indexes[1].Epoch, originalEpoch)
	_, err = f.store.Mutate(fixtureTeamAction(t, f.actor(t, 1), 1, OpCreate, request), BindCreate(1, 303), CreateOp(1, 303, 1))
	require.NoError(t, err)
	restarted := NewSQLStore(f.owner, nil, RuleConfig{AllowCrossZone: true})
	snap, healed, err := restarted.ReadFree(f.actor(t, 1), 1)
	require.NoError(t, err)
	require.False(t, healed)
	require.Zero(t, snap.PlayerTeamId)
	require.Equal(t, left.Commit.Indexes[1].Epoch, snap.PlayerEpoch)
	var count int
	require.NoError(t, f.owner.DB.QueryRow("SELECT COUNT(*) FROM team_authority WHERE team_id IN(202,303)").Scan(&count))
	require.Zero(t, count)
}

func TestTeamTiDBOldInvitationCannotAuthorizeNewOwner(t *testing.T) {
	f := newNativeTeamFixture(t)
	f.create(t, 1, 101)
	invite := &teampb.InviteToTeamRequest{TargetPlayerId: 2, ExpectedTeamId: 101, RequestId: uuid.NewString()}
	ctx := fixtureTeamAction(t, f.actor(t, 1), 1, OpInvite, invite)
	_, err := f.store.Mutate(ctx, BindCaller(1, 101), InviteOp(1, 2, 1))
	require.NoError(t, err)
	_, err = f.owner.DB.Exec("UPDATE account_role SET account='buyer',owner_revision=8 WHERE player_id=2")
	require.NoError(t, err)
	buyer, err := roleidentity.Capture(context.Background(), &dspb.AccountRoleOwnership{Account: "buyer", OwnerRevision: 8, Role: &base.AccountSimplePlayer{PlayerId: 2}})
	require.NoError(t, err)
	for _, actor := range []context.Context{f.actor(t, 2), buyer} {
		request := &teampb.RespondInviteRequest{TeamId: 101, Accept: true, RequestId: uuid.NewString()}
		_, err = f.store.Mutate(fixtureTeamAction(t, actor, 2, OpRespondInvite, request), BindTarget(2, 101), RespondInviteOp(2, true))
		require.ErrorIs(t, err, authority.ErrOriginalPlayer)
	}
	_, _, err = f.store.ReadFree(f.actor(t, 2), 2)
	require.ErrorIs(t, err, authority.ErrOriginalPlayer)
	snap, _, err := f.store.ReadFree(buyer, 2)
	require.NoError(t, err)
	require.Zero(t, snap.PlayerTeamId)
}

func TestTeamTiDBConcurrentAcceptHasOneMembership(t *testing.T) {
	f := newNativeTeamFixture(t)
	f.create(t, 1, 101)
	f.create(t, 3, 303)
	for _, leader := range []uint64{1, 3} {
		teamID := leader * 101
		request := &teampb.InviteToTeamRequest{TargetPlayerId: 2, ExpectedTeamId: teamID, RequestId: uuid.NewString()}
		result, err := f.store.Mutate(fixtureTeamAction(t, f.actor(t, leader), leader, OpInvite, request), BindCaller(leader, teamID), InviteOp(leader, 2, 1))
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, result.Outcome)
	}
	start := make(chan struct{})
	var workers sync.WaitGroup
	results := make(chan *MutateResult, 2)
	failures := make(chan error, 2)
	for _, teamID := range []uint64{101, 303} {
		teamID := teamID
		request := &teampb.RespondInviteRequest{TeamId: teamID, Accept: true, RequestId: uuid.NewString()}
		ctx := fixtureTeamAction(t, f.actor(t, 2), 2, OpRespondInvite, request)
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			result, err := f.store.Mutate(ctx, BindTarget(2, teamID), RespondInviteOp(2, true))
			results <- result
			failures <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	committed := 0
	for result := range results {
		if result.Outcome == OutcomeCommitted {
			committed++
		}
	}
	require.Equal(t, 1, committed)
	memberships := 0
	for _, teamID := range []uint64{101, 303} {
		snap, err := f.store.ReadMembers(context.Background(), teamID, nil)
		require.NoError(t, err)
		if FindMember(snap.Record, 2) != nil {
			memberships++
		}
	}
	require.Equal(t, 1, memberships)
}

func (f *nativeTeamFixture) profile(t *testing.T, player uint64) {
	t.Helper()
	stamp, err := f.owner.Now(context.Background())
	require.NoError(t, err)
	profile := &matchpb.MatchNetworkProfile{PlanId: uuid.NewString(), SessionBinding: "native-team", Receipts: []*matchpb.NetworkProbeReceipt{{Grant: &matchpb.NetworkProbeGrant{PlayerId: player, PhysicalRegion: 1, ExpiresAtMs: stamp + 120000}, RttMs: 20, SampleCount: 3, MeasuredAtMs: stamp, ExpiresAtMs: stamp + 120000}, {Grant: &matchpb.NetworkProbeGrant{PlayerId: player, PhysicalRegion: 2, ExpiresAtMs: stamp + 120000}, RttMs: 80, SampleCount: 3, MeasuredAtMs: stamp, ExpiresAtMs: stamp + 120000}}}
	require.NoError(t, f.owner.SavePlan(f.actor(t, player), player, profile, stamp+120000))
}

func (f *nativeTeamFixture) queued(t *testing.T, leader, teamID uint64) *teampb.TeamLaunchRecord {
	t.Helper()
	snap, _, err := f.store.ReadFree(f.actor(t, leader), leader)
	require.NoError(t, err)
	roster := MatchRoster(snap.Record)
	zones := map[uint64]uint32{}
	for _, player := range roster {
		f.profile(t, player)
		var home uint32
		require.NoError(t, f.owner.DB.QueryRow("SELECT home_zone FROM player_route WHERE route_shard=? AND player_id=?", placement.RouteRowShard(player), player).Scan(&home))
		zones[player] = home
	}
	request := &teampb.StartTeamMatchRequest{BattleConfigId: 1, ExpectedTeamId: teamID, TargetTeamSize: 5, RequestId: uuid.NewString()}
	pinned, launch, err := f.store.sql.createLaunch(f.actor(t, leader), snap, leader, request, uuid.NewString(), roster, zones, 100)
	require.NoError(t, err)
	require.NotNil(t, pinned.Commit)
	require.Equal(t, uint32(2), launch.State)
	return launch
}

func TestTeamTiDBFirstFullLaunchAndPendingSoloUseTheSamePCommitFence(t *testing.T) {
	for _, first := range []string{"solo", "team"} {
		t.Run(first, func(t *testing.T) {
			f := newNativeTeamFixture(t)
			f.create(t, 1, 101)
			f.inviteAndAccept(t, 1, 2, 101)
			f.profile(t, 1)
			f.profile(t, 2)
			actor := f.actor(t, 2)
			profile, _, err := f.owner.Profile(actor, 2)
			require.NoError(t, err)
			join := &matchpb.JoinQueueRequest{RequestId: uuid.NewString(), Mode: matchpb.MatchMode(3), BattleConfigId: 1, NetworkPlanId: profile.PlanId}
			offered := &matchpb.MatchAuthorityTicketRecord{PlayerId: 2, Ticket: uuid.NewString(), RequestId: join.RequestId, Mode: 3, ConfigId: 1, TeamSize: 2, ZoneId: 1}
			if first == "solo" {
				_, _, pending, err := f.owner.PrepareJoin(actor, offered, join, "native-team")
				require.NoError(t, err)
				require.True(t, pending)
			}
			snap, _, err := f.store.ReadFree(f.actor(t, 1), 1)
			require.NoError(t, err)
			start := &teampb.StartTeamMatchRequest{ExpectedTeamId: 101, BattleConfigId: 1, TargetTeamSize: 2, RequestId: uuid.NewString()}
			_, launch, err := f.store.sql.createLaunch(f.actor(t, 1), snap, 1, start, uuid.NewString(), []uint64{1, 2}, map[uint64]uint32{1: 1, 2: 1}, 100)
			if first == "solo" {
				require.ErrorIs(t, err, authority.ErrMatchEntryOccupied)
				require.Nil(t, launch)
				var count int
				require.NoError(t, f.owner.DB.QueryRow("SELECT COUNT(*) FROM team_launch").Scan(&count))
				require.Zero(t, count, "失败的首次整队提交不能留下原launch")
				after, _, readErr := f.store.ReadFree(f.actor(t, 1), 1)
				require.NoError(t, readErr)
				require.Equal(t, snap.Version, after.Version)
				require.True(t, proto.Equal(snap.Record, after.Record))
				return
			}
			require.NoError(t, err)
			require.Equal(t, uint32(1), launch.State)
			var count int
			require.NoError(t, f.owner.DB.QueryRow("SELECT COUNT(*) FROM match_authority_ticket").Scan(&count))
			require.Zero(t, count, "满队首次原launch与后续Match落票间的真实窗口")
			_, _, _, err = f.owner.PrepareJoin(actor, offered, join, "native-team")
			require.ErrorIs(t, err, authority.ErrMatchEntryOccupied)
			_, err = f.owner.SpectatorIntent(actor, 2, &matchpb.WatchBattleRequest{RequestId: uuid.NewString()})
			require.ErrorIs(t, err, authority.ErrMatchEntryOccupied)
		})
	}
}

func TestTeamTiDBWholeUnitsCrossWaitUnknownAndRecovery(t *testing.T) {
	f := newNativeTeamFixture(t)
	f.create(t, 1, 101)
	f.inviteAndAccept(t, 1, 2, 101)
	f.inviteAndAccept(t, 1, 3, 101)
	f.create(t, 4, 404)
	f.inviteAndAccept(t, 4, 5, 404)
	a, b := f.queued(t, 1, 101), f.queued(t, 4, 404)
	left, err := f.owner.TeamLaunchTickets(context.Background(), a)
	require.NoError(t, err)
	right, err := f.owner.TeamLaunchTickets(context.Background(), b)
	require.NoError(t, err)
	group := append(left, right...)
	stamp, err := f.owner.Now(context.Background())
	require.NoError(t, err)
	peers, err := f.owner.TeamLaunchPeers(context.Background(), left[0], a, 1, 2, true, stamp)
	require.NoError(t, err)
	require.Len(t, peers, 1)
	formation := &matchpb.MatchFormationRecord{BattleId: 900, OperationId: uuid.NewString(), PhysicalRegion: 2, TargetNodeId: 1, TargetInstanceId: uuid.NewString(), TargetEndpoint: "127.0.0.1:1", Mode: uint32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), ConfigId: 1}
	require.Error(t, f.owner.ClaimTeamUnits(context.Background(), formation, group, []*teampb.TeamLaunchRecord{a, b}), "全员未等满，队伍失败不重排也不能绕过跨区wait")
	formation.PhysicalRegion = 1
	require.NoError(t, f.owner.ClaimTeamUnits(context.Background(), formation, group, []*teampb.TeamLaunchRecord{a, b}))
	for _, ticket := range group {
		var state uint32
		var battle uint64
		require.NoError(t, f.owner.DB.QueryRow("SELECT state,formation_id FROM match_authority_ticket WHERE player_id=?", ticket.PlayerId).Scan(&state, &battle))
		require.Equal(t, uint32(2), state)
		require.Equal(t, uint64(900), battle)
	}
	stop, pinned, err := f.store.sql.endMatch(context.Background(), 101, a.LockToken)
	require.NoError(t, err)
	require.Equal(t, EndMatchDeadline, stop)
	require.Nil(t, pinned)
	snap, _, err := f.store.ReadFree(f.actor(t, 1), 1)
	require.NoError(t, err)
	require.True(t, MatchLockActive(snap.Record, stamp+365*24*3600000), "SQL持久launch锁不按TTL复活名册")
	work, err := f.owner.AcquireWork(context.Background(), 900)
	require.NoError(t, err)
	require.NoError(t, f.owner.Finish(context.Background(), work, true))
	_, pinned, err = f.store.sql.endMatch(context.Background(), 101, a.LockToken)
	require.NoError(t, err)
	require.NotNil(t, pinned.Commit)
	require.Empty(t, pinned.Commit.Decision.Record.MatchLockToken)
}

func TestTeamTiDBFinalHomeFiltersWholeUnitsBeforeLimit(t *testing.T) {
	f := newNativeTeamFixture(t)
	f.owner.TeamConfig.AllowCrossZone = false // 隔离夹具分别验证两种已准入规则，不是生产热更新入口。
	f.create(t, 1, 101)
	f.inviteAndAccept(t, 1, 2, 101)
	f.inviteAndAccept(t, 1, 3, 101)
	for leader := uint64(4); leader <= 14; leader += 2 {
		f.create(t, leader, 1000+leader)
		f.inviteAndAccept(t, leader, leader+1, 1000+leader)
	}
	for player := uint64(4); player <= 13; player++ {
		_, err := f.owner.DB.Exec("UPDATE player_route SET home_zone=2,rev=rev+1 WHERE route_shard=? AND player_id=?", placement.RouteRowShard(player), player)
		require.NoError(t, err)
	}
	anchor := f.queued(t, 1, 101)
	var foreign []*teampb.TeamLaunchRecord
	for leader := uint64(4); leader <= 12; leader += 2 {
		foreign = append(foreign, f.queued(t, leader, 1000+leader))
	}
	same := f.queued(t, 14, 1014)
	left, err := f.owner.TeamLaunchTickets(context.Background(), anchor)
	require.NoError(t, err)
	stamp, err := f.owner.Now(context.Background())
	require.NoError(t, err)
	peers, err := f.owner.TeamLaunchPeers(context.Background(), left[0], anchor, 1, 2, true, stamp)
	require.NoError(t, err)
	require.Len(t, peers, 1, "五个更早的其它home整单不能遮住合法晚整单")
	require.Equal(t, same.ActorId, peers[0].ActorId)
	right, err := f.owner.TeamLaunchTickets(context.Background(), foreign[0])
	require.NoError(t, err)
	formation := &matchpb.MatchFormationRecord{BattleId: 902, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 1, TargetInstanceId: uuid.NewString(), TargetEndpoint: "127.0.0.1:1", Mode: uint32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), ConfigId: 1}
	require.ErrorIs(t, f.owner.ClaimTeamUnits(context.Background(), formation, append(append([]*matchpb.MatchAuthorityTicketRecord(nil), left...), right...), []*teampb.TeamLaunchRecord{anchor, foreign[0]}), authority.ErrTeamRoute, "提交也须拦完整组跨home")
	var count int
	require.NoError(t, f.owner.DB.QueryRow("SELECT COUNT(*) FROM match_formation WHERE battle_id=902").Scan(&count))
	require.Zero(t, count)
	right, err = f.owner.TeamLaunchTickets(context.Background(), same)
	require.NoError(t, err)
	// 已排队后route换home，不能用旧投影/旧launch或新当前home补权。
	_, err = f.owner.DB.Exec("UPDATE player_route SET home_zone=2,rev=rev+1 WHERE route_shard=? AND player_id=14", placement.RouteRowShard(14))
	require.NoError(t, err)
	require.ErrorIs(t, f.owner.ClaimTeamUnits(context.Background(), formation, append(append([]*matchpb.MatchAuthorityTicketRecord(nil), left...), right...), []*teampb.TeamLaunchRecord{anchor, same}), authority.ErrTeamRoute)
}

func TestTeamTiDBFinalHomeFiltersSoloPeersBeforeLimit(t *testing.T) {
	f := newNativeTeamFixture(t)
	f.owner.TeamConfig.AllowCrossZone = false
	ctx := context.Background()
	var anchor *matchpb.MatchAuthorityTicketRecord
	for player := uint64(1); player <= 10; player++ {
		home := uint32(1)
		if player >= 2 && player <= 6 {
			home = 2
			_, err := f.owner.DB.Exec("UPDATE player_route SET home_zone=2,rev=rev+1 WHERE route_shard=? AND player_id=?", placement.RouteRowShard(player), player)
			require.NoError(t, err)
		}
		f.profile(t, player)
		var payload []byte
		require.NoError(t, f.owner.DB.QueryRow("SELECT payload FROM match_network_profile WHERE player_id=?", player).Scan(&payload))
		profile, err := authority.DecodeProfile(payload)
		require.NoError(t, err)
		ticket, created, err := f.owner.Join(f.actor(t, player), &matchpb.MatchAuthorityTicketRecord{PlayerId: player, Ticket: uuid.NewString(), RequestId: uuid.NewString(), Mode: uint32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), ConfigId: 1, TeamSize: 5, ZoneId: home}, profile.PlanId, profile.SessionBinding)
		require.NoError(t, err)
		require.True(t, created)
		if player == 1 {
			anchor = ticket
		}
	}
	stamp, err := f.owner.Now(ctx)
	require.NoError(t, err)
	peers, err := f.owner.CandidateTeamPeers(ctx, anchor, 1, true, stamp)
	require.NoError(t, err)
	require.Len(t, peers, 4, "其它home的五个早单排不能占满LIMIT后才被丢弃")
	for _, peer := range peers {
		require.Equal(t, uint32(1), peer.ZoneId)
		require.GreaterOrEqual(t, peer.PlayerId, uint64(7))
	}
	formation := &matchpb.MatchFormationRecord{BattleId: 903, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 1, TargetInstanceId: uuid.NewString(), TargetEndpoint: "127.0.0.1:1", Mode: uint32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), ConfigId: 1}
	require.NoError(t, f.owner.ClaimTeamUnits(ctx, formation, append([]*matchpb.MatchAuthorityTicketRecord{anchor}, peers...), nil))
}

func TestTeamTiDBFailedFormationRequeuesWholeOriginalUnit(t *testing.T) {
	f := newNativeTeamFixture(t)
	f.create(t, 1, 101)
	f.inviteAndAccept(t, 1, 2, 101)
	f.inviteAndAccept(t, 1, 3, 101)
	f.create(t, 4, 404)
	f.inviteAndAccept(t, 4, 5, 404)
	a, b := f.queued(t, 1, 101), f.queued(t, 4, 404)
	left, err := f.owner.TeamLaunchTickets(context.Background(), a)
	require.NoError(t, err)
	right, err := f.owner.TeamLaunchTickets(context.Background(), b)
	require.NoError(t, err)
	group := append(left, right...)
	formation := &matchpb.MatchFormationRecord{BattleId: 901, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 1, TargetInstanceId: uuid.NewString(), TargetEndpoint: "127.0.0.1:1", Mode: uint32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), ConfigId: 1}
	require.NoError(t, f.owner.ClaimTeamUnits(context.Background(), formation, group, []*teampb.TeamLaunchRecord{a, b}))
	work, err := f.owner.AcquireWork(context.Background(), 901)
	require.NoError(t, err)
	require.NoError(t, f.owner.Finish(context.Background(), work, false))
	stamp, err := f.owner.Now(context.Background())
	require.NoError(t, err)
	for _, launch := range []*teampb.TeamLaunchRecord{a, b} {
		_, pinned, err := f.store.sql.endMatch(context.Background(), launch.TeamId, launch.LockToken)
		require.NoError(t, err)
		require.NotNil(t, pinned.Commit)
		data, err := authority.DecodeTeamLaunch(launch)
		require.NoError(t, err)
		current, err := f.owner.TeamLaunch(f.actor(t, launch.ActorId), launch.ActorId, data.Original.Request.RequestId)
		require.NoError(t, err)
		require.Equal(t, uint32(2), current.State)
		require.Zero(t, current.BattleId)
		require.GreaterOrEqual(t, current.NotBeforeMs, stamp+1000)
		require.Equal(t, launch.Payload, current.Payload)
		tickets, err := f.owner.TeamLaunchTickets(context.Background(), current)
		require.NoError(t, err)
		for _, ticket := range tickets {
			for _, before := range group {
				if ticket.PlayerId != before.PlayerId {
					continue
				}
				require.Equal(t, before.Ticket, ticket.Ticket)
				require.Equal(t, before.EnqueuedMs, ticket.EnqueuedMs)
				require.Equal(t, before.NetworkProfile, ticket.NetworkProfile)
				require.Equal(t, before.OwnerAccount, ticket.OwnerAccount)
				require.Equal(t, uint32(1), ticket.State)
				require.Zero(t, ticket.FormationId)
			}
		}
		var singleRows int
		require.NoError(t, f.owner.DB.QueryRow("SELECT COUNT(*) FROM match_ticket_region r JOIN match_authority_ticket t ON t.player_id=r.player_id WHERE t.team_id=?", launch.TeamId).Scan(&singleRows))
		require.Zero(t, singleRows, "整队恢复不得造逐人补位候选")
	}
}

func TestTeamTiDBMergeFenceAndFrozenRouteRejectBeforeMembership(t *testing.T) {
	f := newNativeTeamFixture(t)
	_, err := f.owner.DB.Exec("INSERT INTO zone_merge_fence(zone_id,shard,run_id,expires_at_ms) VALUES(1,?,'fixture-merge',18446744073709551615)", placement.RouteRowShard(1))
	require.NoError(t, err)
	request := &teampb.CreateTeamRequest{RequestId: uuid.NewString()}
	ctx := fixtureTeamAction(t, f.actor(t, 1), 1, OpCreate, request)
	_, err = f.store.Mutate(ctx, BindCreate(1, 101), CreateOp(1, 101, 1))
	require.ErrorIs(t, err, authority.ErrTeamRoute)
	var count int
	require.NoError(t, f.owner.DB.QueryRow("SELECT COUNT(*) FROM team_membership").Scan(&count))
	require.Zero(t, count)
	_, err = f.owner.DB.Exec("UPDATE zone_merge_fence SET run_id=NULL,expires_at_ms=NULL WHERE zone_id=1")
	require.NoError(t, err)
	_, err = f.owner.DB.Exec("UPDATE player_route SET frozen_run_id='fixture-frozen' WHERE player_id=1")
	require.NoError(t, err)
	_, err = f.store.Mutate(ctx, BindCreate(1, 101), CreateOp(1, 101, 1))
	require.ErrorIs(t, err, authority.ErrTeamRoute)
}

func TestTeamTiDBMemberCancelClosesWholeUnitAndOldReplayCannotCancelNew(t *testing.T) {
	f := newNativeTeamFixture(t)
	f.create(t, 1, 101)
	f.inviteAndAccept(t, 1, 2, 101)
	old := f.queued(t, 1, 101)
	tickets, err := f.owner.TeamLaunchTickets(context.Background(), old)
	require.NoError(t, err)
	var memberTicket *matchpb.MatchAuthorityTicketRecord
	for _, ticket := range tickets {
		if ticket.PlayerId == 2 {
			memberTicket = ticket
		}
	}
	members, err := f.owner.CancelTeamQueueForActor(f.actor(t, 2), 2, memberTicket)
	require.NoError(t, err)
	require.ElementsMatch(t, []uint64{1, 2}, members)
	for _, ticket := range tickets {
		var state uint32
		require.NoError(t, f.owner.DB.QueryRow("SELECT state FROM match_authority_ticket WHERE player_id=?", ticket.PlayerId).Scan(&state))
		require.Equal(t, uint32(4), state)
	}
	current := f.queued(t, 1, 101)
	require.NotEqual(t, old.RequestLow, current.RequestLow)
	members, err = f.owner.CancelTeamLaunchForActor(f.actor(t, 1), 1, old)
	require.NoError(t, err)
	require.Empty(t, members)
	currentTickets, err := f.owner.TeamLaunchTickets(context.Background(), current)
	require.NoError(t, err)
	require.Len(t, currentTickets, 2)
	for _, ticket := range currentTickets {
		require.Equal(t, uint32(1), ticket.State)
	}
}

func TestTeamTiDBPreparedLaunchCapturesOnlyOriginalSpectatorAndResumesWholeQueue(t *testing.T) {
	f := newNativeTeamFixture(t)
	f.create(t, 1, 101)
	f.inviteAndAccept(t, 1, 2, 101)
	var group []*matchpb.MatchAuthorityTicketRecord
	for _, player := range []uint64{4, 5} {
		f.profile(t, player)
		profile, _, err := f.owner.Profile(context.Background(), player)
		require.NoError(t, err)
		ticket, _, err := f.owner.Join(f.actor(t, player), &matchpb.MatchAuthorityTicketRecord{PlayerId: player, Ticket: uuid.NewString(), RequestId: uuid.NewString(), Mode: 3, ConfigId: 1, TeamSize: 2}, profile.PlanId, profile.SessionBinding)
		require.NoError(t, err)
		group = append(group, ticket)
	}
	room := &matchpb.MatchFormationRecord{BattleId: 930, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 1, TargetInstanceId: uuid.NewString(), TargetEndpoint: "127.0.0.1:1", Mode: 3, ConfigId: 1}
	require.NoError(t, f.owner.Claim(context.Background(), room, group))
	work, err := f.owner.AcquireWork(context.Background(), room.BattleId)
	require.NoError(t, err)
	require.NoError(t, f.owner.Finish(context.Background(), work, true))
	room, err = f.owner.Formation(context.Background(), room.BattleId)
	require.NoError(t, err)
	watch := &matchpb.WatchBattleRequest{RequestId: uuid.NewString()}
	_, err = f.owner.SpectatorIntent(f.actor(t, 1), 1, watch)
	require.NoError(t, err)
	add := &battlepb.AddObserverRequest{Routing: &battlepb.BattleRouting{SessionId: 1, GateNodeId: 1, GateInstanceId: uuid.NewString(), ZoneId: 1}}
	original, err := f.owner.BeginSpectator(f.actor(t, 1), 1, watch, room, add)
	require.NoError(t, err)
	ok, err := f.owner.AdvanceSpectator(f.actor(t, 1), original, 2)
	require.NoError(t, err)
	require.True(t, ok)
	snap, _, err := f.store.ReadFree(f.actor(t, 1), 1)
	require.NoError(t, err)
	f.profile(t, 1)
	f.profile(t, 2)
	request := &teampb.StartTeamMatchRequest{ExpectedTeamId: 101, BattleConfigId: 1, TargetTeamSize: 5, RequestId: uuid.NewString()}
	_, launch, err := f.store.sql.createLaunch(f.actor(t, 1), snap, 1, request, uuid.NewString(), []uint64{1, 2}, map[uint64]uint32{1: 1, 2: 1}, 100)
	require.NoError(t, err)
	require.Equal(t, uint32(1), launch.State)
	data, err := authority.DecodeTeamLaunch(launch)
	require.NoError(t, err)
	require.Len(t, data.SpectatorExits, 1)
	require.Equal(t, watch.RequestId, data.SpectatorExits[0].OperationId)
	require.Equal(t, uint32(3), data.SpectatorExits[0].State)
	require.ErrorIs(t, f.owner.EnqueuePreparedTeamLaunch(context.Background(), launch), authority.ErrSpectatorBusy)
	// 模拟原Remove已获正证明；新Watch的首次head也不能穿过prepared到queued的间隙。
	ok, err = f.owner.AdvanceSpectator(context.Background(), data.SpectatorExits[0], 5)
	require.NoError(t, err)
	require.True(t, ok)
	later := &matchpb.WatchBattleRequest{RequestId: uuid.NewString()}
	_, err = f.owner.SpectatorIntent(f.actor(t, 1), 1, later)
	require.ErrorIs(t, err, authority.ErrMatchEntryOccupied)
	_, err = f.owner.BeginSpectator(f.actor(t, 1), 1, later, room, add)
	require.ErrorIs(t, err, authority.ErrSpectatorBusy)
	require.NoError(t, f.owner.EnqueuePreparedTeamLaunch(context.Background(), launch))
	queued, err := f.owner.TeamLaunch(f.actor(t, 1), 1, request.RequestId)
	require.NoError(t, err)
	require.Equal(t, uint32(2), queued.State)
	tickets, err := f.owner.TeamLaunchTickets(context.Background(), queued)
	require.NoError(t, err)
	require.Len(t, tickets, 2)
	_, err = f.owner.CancelTeamLaunchForActor(f.actor(t, 1), 1, queued)
	require.NoError(t, err)
	current, err := f.owner.BeginSpectator(f.actor(t, 1), 1, later, room, add)
	require.NoError(t, err)
	closed, err := f.owner.SpectatorExitClosed(context.Background(), data.SpectatorExits[0])
	require.NoError(t, err)
	require.True(t, closed)
	actual, err := f.owner.SpectatorForActor(f.actor(t, 1), 1, later.RequestId)
	require.NoError(t, err)
	require.True(t, proto.Equal(current, actual), "旧Start只查原history终态，不清退新watch")
}
