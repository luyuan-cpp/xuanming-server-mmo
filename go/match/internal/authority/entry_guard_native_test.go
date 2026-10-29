package authority

import (
	"context"
	"crypto/sha256"
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
	guildpb "proto/guild"
	pb "proto/match"
	teampb "proto/team"
	"schemamigrate"
	"shared/matchentry"
)

// 原角色P和四张正规Guild表都在独占TiDB库；不沿用独立状态机的跨域fixture guard。
func nativeEntryStore(t *testing.T) *Store {
	t.Helper()
	s := nativeRoleStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var schema string
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema))
	_, err := schemamigrate.Up(ctx, s.DB, schemamigrate.Options{Database: schema, Tables: []proto.Message{&guildpb.GuildTrialLaunchRecord{}, &guildpb.GuildTrialLaunchRoleRecord{}, &guildpb.GuildTrialLobbyRecord{}, &guildpb.GuildTrialLobbySlotRecord{}}})
	require.NoError(t, err)
	require.NoError(t, s.ConfigureMatchEntryAuthority(ctx, schema))
	return s
}

func nativeEntryActor(t *testing.T, s *Store, player uint64) (context.Context, *pb.MatchAuthorityTicketRecord, *pb.JoinQueueRequest) {
	t.Helper()
	account := fmt.Sprintf("entry-original-%d", player)
	_, err := s.DB.Exec("INSERT INTO account_role VALUES(?,?,7,0)", player, []byte(account))
	require.NoError(t, err)
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
	for _, domain := range []string{"team-role", "team-parent", "trial-role", "trial-parent"} {
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
				_, err = s.DB.Exec("ALTER TABLE " + table + " MODIFY COLUMN state INT UNSIGNED NULL")
				require.NoError(t, err)
				_, err = s.DB.Exec("UPDATE " + table + " SET state=NULL")
				require.NoError(t, err)
			} else {
				_, err := s.DB.Exec("INSERT INTO guild_trial_launch(request_id,battle_id,state,original_proto,request_digest,created_ms,updated_ms,cancel_receipt) VALUES(71,0,3,?,?,1,1,?)", []byte{}, []byte{}, []byte{})
				require.NoError(t, err)
				_, err = s.DB.Exec("INSERT INTO guild_trial_launch_role(player_id,request_id,account,owner_revision,state) VALUES(11,71,?,7,2)", first.OwnerAccount)
				require.NoError(t, err)
				table := "guild_trial_launch_role"
				if domain == "trial-parent" {
					table = "guild_trial_launch"
				}
				_, err = s.DB.Exec("ALTER TABLE " + table + " MODIFY COLUMN state INT UNSIGNED NULL")
				require.NoError(t, err)
				_, err = s.DB.Exec("UPDATE " + table + " SET state=NULL")
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

func TestEntryTiDBRoleTerminalNeedsActualParentTerminalAndPrivateLobbyCannotExpireAway(t *testing.T) {
	for _, domain := range []string{"team", "trial", "private"} {
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
			case "trial":
				_, err := s.DB.ExecContext(ctx, "INSERT INTO guild_trial_launch(request_id,battle_id,state,original_proto,request_digest,created_ms,updated_ms,cancel_receipt) VALUES(71,0,1,?, ?,1,1,?)", []byte{}, []byte{}, []byte{})
				require.NoError(t, err)
				_, err = s.DB.ExecContext(ctx, "INSERT INTO guild_trial_launch_role(player_id,request_id,account,owner_revision,state) VALUES(11,71,?,7,2)", first.OwnerAccount)
				require.NoError(t, err)
				_, _, _, err = s.PrepareJoin(ctx, first, request, "entry-bound")
				require.ErrorIs(t, err, ErrMatchEntryOccupied)
				_, err = s.DB.ExecContext(ctx, "UPDATE guild_trial_launch SET state=3")
				require.NoError(t, err)
				_, _, _, err = s.PrepareJoin(ctx, first, request, "entry-bound")
				require.NoError(t, err)
			case "private":
				_, err := s.DB.ExecContext(ctx, "INSERT INTO guild_trial_lobby_slot(player_id,lobby_id,cooldown_until_ms) VALUES(11,71,0)")
				require.NoError(t, err)
				_, _, _, err = s.PrepareJoin(ctx, first, request, "entry-bound")
				require.ErrorIs(t, err, ErrMatchEntryOccupied)
				_, err = s.DB.ExecContext(ctx, "INSERT INTO guild_trial_lobby(lobby_id,guild_id,activity_id,initiator_player_id,state,view_proto,original_proto,consent_proto,expires_ms,launch_deadline_ms,battle_id,created_ms,updated_ms) VALUES(71,1,1,11,2,?,?,?,1,1,0,1,1)", []byte{}, []byte{}, []byte{})
				require.NoError(t, err)
				_, _, _, err = s.PrepareJoin(ctx, first, request, "entry-bound")
				require.ErrorIs(t, err, ErrMatchEntryOccupied)
				_, err = s.DB.ExecContext(ctx, "UPDATE guild_trial_lobby SET state=4")
				require.NoError(t, err)
				_, _, _, err = s.PrepareJoin(ctx, first, request, "entry-bound")
				require.NoError(t, err)
			}
		})
	}
}

func TestEntryTiDBOccupiedSQLMatchesAllFifteenProtoFieldsAndRejectsNull(t *testing.T) {
	s := testStore(t)
	columns := []string{"state", "ticket", "request_id", "formation_id", "mode", "config_id", "team_size", "enqueued_ms", "not_before_ms", "zone_id", "rating_centi", "team_id", "network_profile", "expires_ms", "selected_region", "preferred_region"}
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
		return []any{ticket.State, ticket.Ticket, ticket.RequestId, ticket.FormationId, ticket.Mode, ticket.ConfigId, ticket.TeamSize, ticket.EnqueuedMs, ticket.NotBeforeMs, ticket.ZoneId, ticket.RatingCenti, ticket.TeamId, profile, ticket.ExpiresMs, ticket.SelectedRegion, ticket.PreferredRegion}
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

// 此函数同时给活动原件回归固定SQL私有大厅，不依赖Redis或当前owner补原意图。
func insertNativeEntryTrial(t *testing.T, s *Store, tx *sql.Tx, group []*pb.MatchAuthorityTicketRecord) (*pb.StartActivityBattleRequest, *pb.MatchFormationRecord) {
	t.Helper()
	original := &guildpb.GuildTrialLaunchOriginal{RequestId: 71, GuildId: 1, ActivityId: 1, PeriodKey: 20261021, GuildPeriodKey: 20261021, InitiatorPlayerId: group[0].PlayerId, DungeonId: 1, CreatedMs: 1}
	lobbyOriginal := &guildpb.GuildTrialLobbyOriginal{LobbyId: 71, GuildId: 1, ActivityId: 1, InitiatorPlayerId: group[0].PlayerId, CreatedMs: 1, ExpiresMs: 1}
	consents := &guildpb.GuildTrialLobbyConsents{}
	view := &guildpb.GuildTrialLobbyView{LobbyId: 71, InitiatorPlayerId: group[0].PlayerId, State: guildpb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHING, ExpireAtMs: 1}
	for _, ticket := range group {
		role := &guildpb.GuildTrialRoleIdentity{PlayerId: ticket.PlayerId, Account: ticket.OwnerAccount, OwnerRevision: ticket.OwnerRevision}
		original.Roster = append(original.Roster, role)
		lobbyOriginal.Roster = append(lobbyOriginal.Roster, role)
		consents.Consents = append(consents.Consents, &guildpb.GuildTrialLobbyConsent{LobbyId: 71, Identity: role})
		view.MemberPlayerIds = append(view.MemberPlayerIds, role.PlayerId)
		view.AcceptedPlayerIds = append(view.AcceptedPlayerIds, role.PlayerId)
	}
	marshal := func(message proto.Message) []byte {
		raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
		require.NoError(t, err)
		return raw
	}
	raw := marshal(original)
	sum := sha256.Sum256(raw)
	_, err := tx.Exec("INSERT INTO guild_trial_launch(request_id,battle_id,state,original_proto,request_digest,created_ms,updated_ms,cancel_receipt) VALUES(71,0,1,?,?,1,1,?)", raw, sum[:], []byte{})
	require.NoError(t, err)
	_, err = tx.Exec("INSERT INTO guild_trial_lobby(lobby_id,guild_id,activity_id,initiator_player_id,state,view_proto,original_proto,consent_proto,expires_ms,launch_deadline_ms,battle_id,created_ms,updated_ms) VALUES(71,1,1,?,2,?,?,?,1,1,0,1,1)", group[0].PlayerId, marshal(view), marshal(lobbyOriginal), marshal(consents))
	require.NoError(t, err)
	for _, member := range group {
		_, err = tx.Exec("INSERT INTO guild_trial_launch_role(player_id,request_id,account,owner_revision,state) VALUES(?,71,?, ?,1)", member.PlayerId, member.OwnerAccount, member.OwnerRevision)
		require.NoError(t, err)
		_, err = tx.Exec("INSERT INTO guild_trial_lobby_slot(player_id,lobby_id,cooldown_until_ms) VALUES(?,71,0)", member.PlayerId)
		require.NoError(t, err)
	}
	request := &pb.StartActivityBattleRequest{RequestId: 71, BattleConfigId: 1, MemberPlayerIds: view.MemberPlayerIds, ActivityContext: &battlepb.BattleActivityContext{Kind: battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL, GuildId: 1, ActivityId: 1, PeriodKey: 20261021, GuildPeriodKey: 20261021, InitiatorPlayerId: group[0].PlayerId, LaunchRequestId: 71}}
	formation := &pb.MatchFormationRecord{BattleId: 901, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 1, TargetInstanceId: uuid.NewString(), TargetEndpoint: "127.0.0.1:1", Mode: uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM), ConfigId: 1, ActivityContext: marshal(request.ActivityContext)}
	return request, formation
}

func TestEntryTiDBTrialOnlyExactOriginalMayConsumeItsOwnPrivateLaunch(t *testing.T) {
	s := nativeEntryStore(t)
	actor, first, solo := nativeEntryActor(t, s, 11)
	_, second, _ := nativeEntryActor(t, s, 22)
	group := []*pb.MatchAuthorityTicketRecord{proto.Clone(first).(*pb.MatchAuthorityTicketRecord), proto.Clone(second).(*pb.MatchAuthorityTicketRecord)}
	for _, member := range group {
		member.Mode = uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM)
	}
	tx, err := s.DB.BeginTx(actor, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, s.lockOriginalPlayers(actor, tx, group))
	request, formation := insertNativeEntryTrial(t, s, tx, group)
	require.NoError(t, tx.Commit())
	_, _, _, err = s.PrepareJoin(actor, first, solo, "entry-bound")
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	bad := proto.Clone(request).(*pb.StartActivityBattleRequest)
	bad.ActivityContext.GuildPeriodKey++
	badRaw, err := proto.MarshalOptions{Deterministic: true}.Marshal(bad)
	require.NoError(t, err)
	wrongFormation := proto.Clone(formation).(*pb.MatchFormationRecord)
	wrongFormation.ActivityContext, err = proto.Marshal(bad.ActivityContext)
	require.NoError(t, err)
	key := &pb.MatchDirectIntentRecord{Kind: 2, OwnerId: 1, RequestLow: 71}
	_, err = s.PrepareDirectAdmission(actor, key, badRaw, wrongFormation, group)
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	require.NoError(t, err)
	prepared, err := s.PrepareDirectAdmission(actor, key, raw, formation, group)
	require.NoError(t, err)
	actual, err := s.CompletePreparedDirect(actor, prepared)
	require.NoError(t, err)
	require.Equal(t, formation.BattleId, actual.BattleId)
}
