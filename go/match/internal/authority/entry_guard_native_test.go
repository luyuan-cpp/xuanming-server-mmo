package authority

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	battlepb "proto/battle"
	pb "proto/match"
	teampb "proto/team"
	"shared/matchentry"
	"shared/rolefence/fencetest"
)

// 原角色P在独占TiDB库的护栏副本上；不沿用独立状态机的跨域fixture guard。
func nativeEntryStore(t *testing.T) *Store {
	t.Helper()
	s := nativeRoleStore(t)
	s.entryGuard = s.requireEntryOriginalsTx
	return s
}

func nativeEntryActor(t *testing.T, s *Store, player uint64) (context.Context, *pb.MatchAuthorityTicketRecord, *pb.JoinQueueRequest) {
	t.Helper()
	account := fmt.Sprintf("entry-original-%d", player)
	fencetest.Seed(t, context.Background(), s.DB, player, account, 7)
	actor := nativeRoleActor(t, context.Background(), player, account, 7)
	now, err := s.Now(actor)
	require.NoError(t, err)
	plan := fmt.Sprintf("entry-plan-%d", player)
	profile := &pb.MatchNetworkProfile{PlanId: plan, SessionBinding: "entry-bound", Receipts: []*pb.NetworkProbeReceipt{{Grant: &pb.NetworkProbeGrant{PlayerId: player, PlanId: plan, SessionBinding: "entry-bound", PhysicalRegion: 1, ExpiresAtMs: now + 120000}, RttMs: 40, SampleCount: 3, MeasuredAtMs: now, ExpiresAtMs: now + 120000}}}
	require.NoError(t, s.SavePlan(actor, player, profile, now+120000))
	request := &pb.JoinQueueRequest{RequestId: uuid.NewString(), NetworkPlanId: plan, Mode: pb.MatchMode(3), BattleConfigId: 1}
	ticket := &pb.MatchAuthorityTicketRecord{PlayerId: player, Ticket: uuid.NewString(), RequestId: request.RequestId, Mode: 3, ConfigId: 1, TeamSize: 2, ZoneId: 1, OwnerAccount: account, OwnerRevision: 7}
	return actor, ticket, request
}

func nativeEntryTeam(t *testing.T, group []*pb.MatchAuthorityTicketRecord) *teampb.TeamLaunchRecord {
	t.Helper()
	request := &teampb.StartTeamMatchRequest{RequestId: uuid.NewString(), ExpectedTeamId: 701, BattleConfigId: 1, TargetTeamSize: uint32(len(group))}
	key, err := DirectUUIDKey(1, group[0].PlayerId, request.RequestId)
	require.NoError(t, err)
	original := &teampb.TeamLaunchOriginal{Actor: &teampb.TeamOriginalRole{PlayerId: group[0].PlayerId, OwnerAccount: group[0].OwnerAccount, OwnerRevision: group[0].OwnerRevision}, TeamId: 701, TeamVersion: 3, LockToken: uuid.NewString(), Request: request, Zones: map[uint64]uint32{}, EnqueuedMs: 1}
	for index, ticket := range group {
		original.Members = append(original.Members, &teampb.TeamLaunchMember{Original: &teampb.TeamOriginalRole{PlayerId: ticket.PlayerId, OwnerAccount: ticket.OwnerAccount, OwnerRevision: ticket.OwnerRevision}, RosterOrder: uint32(index + 1), Ticket: ticket.Ticket})
		original.Zones[ticket.PlayerId] = 1
	}
	requestRaw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	require.NoError(t, err)
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(&teampb.TeamLaunchData{Original: original, Members: original.Members, RequestPayload: requestRaw})
	require.NoError(t, err)
	launch := &teampb.TeamLaunchRecord{ActorId: key.OwnerId, RequestHigh: key.RequestHigh, RequestLow: key.RequestLow, TeamId: 701, TeamVersion: original.TeamVersion, LockToken: original.LockToken, ConfigId: 1, TargetSize: uint32(len(group)), MemberCount: uint32(len(group)), State: 1, EnqueuedMs: 1, Payload: payload, OwnerAccount: original.Actor.OwnerAccount, OwnerRevision: original.Actor.OwnerRevision}
	_, err = DecodeTeamLaunch(launch)
	require.NoError(t, err)
	return launch
}

// 夹具只模拟首次事务提交的正规原件，不伪造Match票；实际角色P由调用处持有。
func insertNativeEntryTeam(ctx context.Context, tx *sql.Tx, launch *teampb.TeamLaunchRecord, group []*pb.MatchAuthorityTicketRecord) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO team_launch("+teamLaunchColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", launch.ActorId, launch.RequestHigh, launch.RequestLow, launch.TeamId, launch.TeamVersion, launch.LockToken, launch.ConfigId, launch.TargetSize, launch.MemberCount, launch.State, launch.EnqueuedMs, launch.NotBeforeMs, launch.BattleId, launch.Payload, launch.OwnerAccount, launch.OwnerRevision, launch.Mode)
	if err != nil {
		return err
	}
	for _, member := range group {
		if _, err = tx.ExecContext(ctx, "INSERT INTO team_launch_role(actor_id,request_high,request_low,player_id,state) VALUES(?,?,?,?,?)", launch.ActorId, launch.RequestHigh, launch.RequestLow, member.PlayerId, launch.State); err != nil {
			return err
		}
	}
	return nil
}

func TestEntryTiDBTeamLaunchWithoutTicketWinsAgainstLateSoloDirectAndWatch(t *testing.T) {
	s := nativeEntryStore(t)
	actor, first, request := nativeEntryActor(t, s, 11)
	_, second, _ := nativeEntryActor(t, s, 22)
	group := []*pb.MatchAuthorityTicketRecord{first, second}
	launch := nativeEntryTeam(t, group)
	ctx, cancel := context.WithTimeout(actor, 20*time.Second)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, s.lockOriginalPlayers(ctx, tx, group))
	require.NoError(t, insertNativeEntryTeam(ctx, tx, launch, group))
	before := nativeMatchSnapshot(t, s, ctx)
	// 标记只观察真实P guard已被调用；不替代其SQL角色锁。
	realGuard := s.roleGuard
	attempted := make(chan struct{})
	var once sync.Once
	s.roleGuard = func(ctx context.Context, tx *sql.Tx, originals []*pb.MatchAuthorityTicketRecord) error {
		once.Do(func() { close(attempted) })
		return realGuard(ctx, tx, originals)
	}
	result := make(chan error, 1)
	go func() { _, _, _, err := s.PrepareJoin(ctx, first, request, "entry-bound"); result <- err }()
	select {
	case <-attempted:
	case <-ctx.Done():
		t.Fatal("正常Join未进入真实P锁")
	}
	require.NoError(t, tx.Commit())
	require.ErrorIs(t, <-result, ErrMatchEntryOccupied)
	_, _, err = s.Join(ctx, first, request.NetworkPlanId, "entry-bound")
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	_, err = s.SpectatorIntent(ctx, 11, &pb.WatchBattleRequest{PlayerId: 11, RequestId: uuid.NewString()})
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	_, err = s.PrepareDirectAdmission(ctx, &pb.MatchDirectIntentRecord{Kind: 3, OwnerId: 11, RequestLow: 9}, []byte("挑战新原意图"), &pb.MatchFormationRecord{BattleId: 901, OperationId: uuid.NewString(), PhysicalRegion: 1, Mode: 3, ConfigId: 1}, group)
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	require.Equal(t, before, nativeMatchSnapshot(t, s, ctx))
	// 即使没有票、原enqueue早已过去，state4仍不能代替state5的结清证明。
	_, err = s.DB.ExecContext(ctx, "UPDATE team_launch SET state=4")
	require.NoError(t, err)
	_, err = s.DB.ExecContext(ctx, "UPDATE team_launch_role SET state=4")
	require.NoError(t, err)
	_, _, _, err = s.PrepareJoin(ctx, first, request, "entry-bound")
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
}

func TestEntryTiDBNewTeamRejectsActualPendingQueuedAndMatchedTickets(t *testing.T) {
	for _, state := range []uint32{0, 1, 2, 3, 9} {
		t.Run(fmt.Sprint(state), func(t *testing.T) {
			s := nativeEntryStore(t)
			actor, member, request := nativeEntryActor(t, s, 11)
			prepared, _, _, err := s.PrepareJoin(actor, member, request, "entry-bound")
			require.NoError(t, err)
			_, err = s.DB.Exec("UPDATE match_authority_ticket SET state=?,expires_ms=1 WHERE player_id=11", state)
			require.NoError(t, err)
			tx, err := s.DB.BeginTx(actor, nil)
			require.NoError(t, err)
			defer tx.Rollback()
			require.NoError(t, s.lockOriginalPlayers(actor, tx, []*pb.MatchAuthorityTicketRecord{prepared}))
			err = s.RequireNewTeamLaunchTx(actor, tx, []*teampb.TeamOriginalRole{{PlayerId: member.PlayerId, OwnerAccount: member.OwnerAccount, OwnerRevision: member.OwnerRevision}})
			require.ErrorIs(t, err, ErrMatchEntryOccupied)
		})
	}
}

func TestEntryTiDBOwnTeamExceptionRequiresActorUUIDAndWholeOriginalRoster(t *testing.T) {
	s := nativeEntryStore(t)
	actor, first, _ := nativeEntryActor(t, s, 11)
	_, second, _ := nativeEntryActor(t, s, 22)
	group := []*pb.MatchAuthorityTicketRecord{first, second}
	launch := nativeEntryTeam(t, group)
	for _, member := range group {
		member.TeamId = launch.TeamId
	}
	tx, err := s.DB.BeginTx(actor, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, s.lockOriginalPlayers(actor, tx, group))
	require.NoError(t, insertNativeEntryTeam(actor, tx, launch, group))
	require.NoError(t, tx.Commit())
	probe := func(members []*pb.MatchAuthorityTicketRecord, own *teampb.TeamLaunchRecord) error {
		tx, err := s.DB.BeginTx(actor, nil)
		require.NoError(t, err)
		defer tx.Rollback()
		require.NoError(t, s.lockOriginalPlayers(actor, tx, group))
		return s.requireMatchEntryTx(actor, tx, members, &matchEntryOriginal{teams: []*teampb.TeamLaunchRecord{own}})
	}
	require.NoError(t, probe(group, launch))
	require.Error(t, probe(group[:1], launch), "不能只带同party一人穿过原完整名单")
	otherUUID := nativeEntryTeam(t, group)
	require.ErrorIs(t, probe(group, otherUUID), ErrMatchEntryOccupied)
	otherActor := nativeEntryTeam(t, []*pb.MatchAuthorityTicketRecord{second, first})
	require.ErrorIs(t, probe(group, otherActor), ErrMatchEntryOccupied)
}

func TestEntryTiDBNullRoleOrParentNeverDisappearsThroughSQLThreeValuedLogic(t *testing.T) {
	for _, domain := range []string{"team-role", "team-parent", "occupancy"} {
		t.Run(domain, func(t *testing.T) {
			s := nativeEntryStore(t)
			actor, first, _ := nativeEntryActor(t, s, 11)
			_, second, _ := nativeEntryActor(t, s, 22)
			group := []*pb.MatchAuthorityTicketRecord{first, second}
			if strings.HasPrefix(domain, "team-") {
				launch := nativeEntryTeam(t, group)
				launch.State = 5
				tx, err := s.DB.BeginTx(actor, nil)
				require.NoError(t, err)
				require.NoError(t, s.lockOriginalPlayers(actor, tx, group))
				require.NoError(t, insertNativeEntryTeam(actor, tx, launch, group))
				require.NoError(t, tx.Commit())
				table := "team_launch_role"
				if domain == "team-parent" {
					table = "team_launch"
				}
				_, err = testInstall(t, s).Exec("ALTER TABLE " + table + " MODIFY COLUMN state INT UNSIGNED NULL")
				require.NoError(t, err)
				_, err = s.DB.Exec("UPDATE " + table + " SET state=NULL")
				require.NoError(t, err)
			} else {
				_, err := testInstall(t, s).Exec("ALTER TABLE match_role_occupancy MODIFY COLUMN expires_ms BIGINT UNSIGNED NULL")
				require.NoError(t, err)
				_, err = s.DB.Exec("INSERT INTO match_role_occupancy("+occupancyColumns+") VALUES(11,?,71,?,7,NULL)", occupancyKindTrial, first.OwnerAccount)
				require.NoError(t, err)
			}
			tx, err := s.DB.BeginTx(actor, nil)
			require.NoError(t, err)
			defer tx.Rollback()
			require.NoError(t, s.lockOriginalPlayers(actor, tx, group))
			require.Error(t, s.requireMatchEntryTx(actor, tx, group, nil))
		})
	}
}

func TestEntryTiDBRoleTerminalNeedsActualParentTerminalAndOccupancyOnlyEndsWithItsLease(t *testing.T) {
	for _, domain := range []string{"team", "occupancy"} {
		t.Run(domain, func(t *testing.T) {
			s := nativeEntryStore(t)
			actor, first, request := nativeEntryActor(t, s, 11)
			_, second, _ := nativeEntryActor(t, s, 22)
			ctx, cancel := context.WithTimeout(actor, 20*time.Second)
			defer cancel()
			switch domain {
			case "team":
				launch := nativeEntryTeam(t, []*pb.MatchAuthorityTicketRecord{first, second})
				tx, err := s.DB.BeginTx(ctx, nil)
				require.NoError(t, err)
				require.NoError(t, s.lockOriginalPlayers(ctx, tx, []*pb.MatchAuthorityTicketRecord{first, second}))
				require.NoError(t, insertNativeEntryTeam(ctx, tx, launch, []*pb.MatchAuthorityTicketRecord{first, second}))
				require.NoError(t, tx.Commit())
				_, err = s.DB.ExecContext(ctx, "UPDATE team_launch_role SET state=5")
				require.NoError(t, err)
				_, _, _, err = s.PrepareJoin(ctx, first, request, "entry-bound")
				require.ErrorIs(t, err, ErrMatchEntryOccupied)
				_, err = s.DB.ExecContext(ctx, "DELETE FROM team_launch")
				require.NoError(t, err)
				_, _, _, err = s.PrepareJoin(ctx, first, request, "entry-bound")
				require.ErrorIs(t, err, ErrMatchEntryOccupied)
			case "occupancy":
				_, err := s.AcquireTrialOccupancy(ctx, 71, []*pb.TrialOccupancyRole{{PlayerId: 11}, {PlayerId: 22}}, 60000)
				require.NoError(t, err)
				_, _, _, err = s.PrepareJoin(ctx, first, request, "entry-bound")
				require.ErrorIs(t, err, ErrMatchEntryOccupied)
				_, err = s.DB.ExecContext(ctx, "UPDATE match_role_occupancy SET expires_ms=1")
				require.NoError(t, err)
				_, _, _, err = s.PrepareJoin(ctx, first, request, "entry-bound")
				require.NoError(t, err, "租约过期视为空闲")
			}
		})
	}
}

func TestEntryTiDBOccupiedSQLMatchesAllFourteenProtoFieldsAndRejectsNull(t *testing.T) {
	s := testStore(t)
	columns := []string{"state", "ticket", "request_id", "formation_id", "mode", "config_id", "team_size", "enqueued_ms", "not_before_ms", "zone_id", "rating_centi", "team_id", "network_profile", "expires_ms", "selected_region"}
	selects := make([]string, len(columns))
	for index, column := range columns {
		selects[index] = "? AS " + column
	}
	query := "SELECT " + matchentry.TicketOccupiedSQL + " FROM (SELECT " + strings.Join(selects, ",") + ") exact_ticket"
	values := func(ticket *pb.MatchAuthorityTicketRecord) []any {
		profile := ticket.NetworkProfile
		if profile == nil {
			profile = []byte{}
		}
		return []any{ticket.State, ticket.Ticket, ticket.RequestId, ticket.FormationId, ticket.Mode, ticket.ConfigId, ticket.TeamSize, ticket.EnqueuedMs, ticket.NotBeforeMs, ticket.ZoneId, ticket.RatingCenti, ticket.TeamId, profile, ticket.ExpiresMs, ticket.SelectedRegion}
	}
	for index := range columns {
		ticket := &pb.MatchAuthorityTicketRecord{}
		if index != 0 {
			message := ticket.ProtoReflect()
			descriptor := message.Descriptor().Fields().ByName(protoreflect.Name(columns[index]))
			switch descriptor.Kind() {
			case protoreflect.StringKind:
				message.Set(descriptor, protoreflect.ValueOfString("original"))
			case protoreflect.BytesKind:
				message.Set(descriptor, protoreflect.ValueOfBytes([]byte{1}))
			case protoreflect.Uint32Kind:
				message.Set(descriptor, protoreflect.ValueOfUint32(1))
			case protoreflect.Uint64Kind:
				message.Set(descriptor, protoreflect.ValueOfUint64(1))
			case protoreflect.Int64Kind:
				message.Set(descriptor, protoreflect.ValueOfInt64(1))
			default:
				t.Fatalf("占用字段类型变化：%s", descriptor.Kind())
			}
		}
		var occupied bool
		require.NoError(t, s.DB.QueryRow(query, values(ticket)...).Scan(&occupied))
		require.Equal(t, matchentry.TicketOccupied(ticket), occupied, columns[index])
		nulls := values(&pb.MatchAuthorityTicketRecord{})
		nulls[index] = nil
		require.NoError(t, s.DB.QueryRow(query, nulls...).Scan(&occupied))
		require.True(t, occupied, "NULL:"+columns[index])
	}
}

// 活动开局的固定原件:大厅 71 的占用已在 match 上,请求由 guild 签名(签名在 logic 层核)。
func nativeEntryTrial(t *testing.T, s *Store, group []*pb.MatchAuthorityTicketRecord) (*pb.StartActivityBattleRequest, *pb.MatchFormationRecord) {
	t.Helper()
	roster := make([]*pb.TrialOccupancyRole, 0, len(group))
	members := make([]uint64, 0, len(group))
	for _, ticket := range group {
		roster = append(roster, &pb.TrialOccupancyRole{PlayerId: ticket.PlayerId, Account: ticket.OwnerAccount, OwnerRevision: ticket.OwnerRevision})
		members = append(members, ticket.PlayerId)
	}
	_, err := s.AcquireTrialOccupancy(context.Background(), 71, roster, 60000)
	require.NoError(t, err)
	request := &pb.StartActivityBattleRequest{RequestId: 71, BattleConfigId: 1, MemberPlayerIds: members, ActivityContext: &battlepb.BattleActivityContext{Kind: battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL, GuildId: 1, ActivityId: 1, PeriodKey: 20261021, GuildPeriodKey: 20261021, InitiatorPlayerId: group[0].PlayerId, LaunchRequestId: 71}}
	return request, nativeTrialFormation(t, request)
}

func nativeTrialFormation(t *testing.T, request *pb.StartActivityBattleRequest) *pb.MatchFormationRecord {
	t.Helper()
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request.ActivityContext)
	require.NoError(t, err)
	return &pb.MatchFormationRecord{BattleId: 901, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 1, TargetInstanceId: uuid.NewString(), TargetEndpoint: "127.0.0.1:1", Mode: uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM), ConfigId: 1, ActivityContext: raw}
}

func nativeTrialGroup(first, second *pb.MatchAuthorityTicketRecord) []*pb.MatchAuthorityTicketRecord {
	group := []*pb.MatchAuthorityTicketRecord{proto.Clone(first).(*pb.MatchAuthorityTicketRecord), proto.Clone(second).(*pb.MatchAuthorityTicketRecord)}
	for _, member := range group {
		member.Mode = uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM)
	}
	return group
}

func TestEntryTiDBTrialOnlyExactOwnerMayConsumeItsOwnOccupancy(t *testing.T) {
	s := nativeEntryStore(t)
	actor, first, solo := nativeEntryActor(t, s, 11)
	_, second, _ := nativeEntryActor(t, s, 22)
	group := nativeTrialGroup(first, second)
	request, formation := nativeEntryTrial(t, s, group)
	_, _, _, err := s.PrepareJoin(actor, first, solo, "entry-bound")
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	// 别的大厅不能用这份占用开局。
	other := proto.Clone(request).(*pb.StartActivityBattleRequest)
	other.RequestId, other.ActivityContext.LaunchRequestId = 72, 72
	otherRaw, err := proto.MarshalOptions{Deterministic: true}.Marshal(other)
	require.NoError(t, err)
	_, err = s.PrepareDirectAdmission(actor, &pb.MatchDirectIntentRecord{Kind: 2, OwnerId: 1, RequestLow: 72}, otherRaw, nativeTrialFormation(t, other), group)
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	// 名单少一个人也不行:这个大厅还占着名单之外的角色。
	partial := proto.Clone(request).(*pb.StartActivityBattleRequest)
	partial.MemberPlayerIds = partial.MemberPlayerIds[:1]
	partialRaw, err := proto.MarshalOptions{Deterministic: true}.Marshal(partial)
	require.NoError(t, err)
	key := &pb.MatchDirectIntentRecord{Kind: 2, OwnerId: 1, RequestLow: 71}
	_, err = s.PrepareDirectAdmission(actor, key, partialRaw, formation, group[:1])
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	_, err = s.DB.Exec("DELETE FROM match_direct_intent")
	require.NoError(t, err)
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	require.NoError(t, err)
	prepared, err := s.PrepareDirectAdmission(actor, key, raw, formation, group)
	require.NoError(t, err)
	actual, err := s.CompletePreparedDirect(actor, prepared)
	require.NoError(t, err)
	require.Equal(t, formation.BattleId, actual.BattleId)
}
