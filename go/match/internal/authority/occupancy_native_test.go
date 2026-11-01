package authority

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	pb "proto/match"
	rfpb "proto/role_fence"
	teampb "proto/team"
	"shared/rolefence"
	"shared/rolefence/fencetest"
	"shared/serviceauth"
)

// 规格 §4 的仲裁:票先赢则 Acquire 拒绝,占用先赢则入场拒绝。
func TestOccupancyTiDBTicketWinsThenAcquireRejects(t *testing.T) {
	s := nativeEntryStore(t)
	actor, first, request := nativeEntryActor(t, s, 11)
	_, _, _ = nativeEntryActor(t, s, 22)
	_, _, _, err := s.PrepareJoin(actor, first, request, "entry-bound")
	require.NoError(t, err)
	offender, err := s.AcquireTrialOccupancy(actor, 71, []*pb.TrialOccupancyRole{{PlayerId: 11}, {PlayerId: 22}}, 60000)
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	require.EqualValues(t, 11, offender)
	var rows int
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_role_occupancy").Scan(&rows))
	require.Zero(t, rows, "全成或全不成")
	// 票终局之后才空闲。
	_, err = s.DB.Exec("UPDATE match_authority_ticket SET state=4 WHERE player_id=11")
	require.NoError(t, err)
	_, err = s.AcquireTrialOccupancy(actor, 71, []*pb.TrialOccupancyRole{{PlayerId: 11}, {PlayerId: 22}}, 60000)
	require.NoError(t, err)
}

func TestOccupancyTiDBTeamLaunchWinsThenAcquireRejects(t *testing.T) {
	s := nativeEntryStore(t)
	actor, first, _ := nativeEntryActor(t, s, 11)
	_, second, _ := nativeEntryActor(t, s, 22)
	group := []*pb.MatchAuthorityTicketRecord{first, second}
	launch := nativeEntryTeam(t, group)
	tx, err := s.DB.BeginTx(actor, nil)
	require.NoError(t, err)
	require.NoError(t, s.lockOriginalPlayers(actor, tx, group))
	require.NoError(t, insertNativeEntryTeam(actor, tx, launch, group))
	require.NoError(t, tx.Commit())
	_, err = s.AcquireTrialOccupancy(actor, 71, []*pb.TrialOccupancyRole{{PlayerId: 22}}, 60000)
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	// 父行和投影都要终结:role5 而父 4、缺父行都不算。
	_, err = s.DB.Exec("UPDATE team_launch_role SET state=5")
	require.NoError(t, err)
	_, err = s.AcquireTrialOccupancy(actor, 71, []*pb.TrialOccupancyRole{{PlayerId: 22}}, 60000)
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	_, err = s.DB.Exec("UPDATE team_launch SET state=5")
	require.NoError(t, err)
	_, err = s.AcquireTrialOccupancy(actor, 71, []*pb.TrialOccupancyRole{{PlayerId: 22}}, 60000)
	require.NoError(t, err)
}

func TestOccupancyTiDBOccupancyWinsThenEntryRejects(t *testing.T) {
	s := nativeEntryStore(t)
	actor, first, request := nativeEntryActor(t, s, 11)
	_, second, _ := nativeEntryActor(t, s, 22)
	_, err := s.AcquireTrialOccupancy(actor, 71, []*pb.TrialOccupancyRole{{PlayerId: 11, Account: first.OwnerAccount, OwnerRevision: 7}, {PlayerId: 22}}, 60000)
	require.NoError(t, err)
	before := nativeMatchSnapshot(t, s, actor)
	_, _, _, err = s.PrepareJoin(actor, first, request, "entry-bound")
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	_, _, err = s.Join(actor, first, request.NetworkPlanId, "entry-bound")
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	_, err = s.SpectatorIntent(actor, 11, &pb.WatchBattleRequest{PlayerId: 11, RequestId: request.RequestId})
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	tx, err := s.DB.BeginTx(actor, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, s.lockOriginalPlayers(actor, tx, []*pb.MatchAuthorityTicketRecord{first, second}))
	err = s.RequireNewTeamLaunchTx(actor, tx, []*teampb.TeamOriginalRole{{PlayerId: 22, OwnerAccount: second.OwnerAccount, OwnerRevision: 7}})
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	require.Equal(t, before, nativeMatchSnapshot(t, s, actor))
}

// 别的大厅占着时 Acquire 拒绝并指出是谁;到期之后可以拿走。名单之外的占用不受影响。
func TestOccupancyTiDBOtherOwnerRejectedUntilLeaseEnds(t *testing.T) {
	s := nativeEntryStore(t)
	actor, _, _ := nativeEntryActor(t, s, 11)
	_, _, _ = nativeEntryActor(t, s, 22)
	_, err := s.AcquireTrialOccupancy(actor, 71, []*pb.TrialOccupancyRole{{PlayerId: 22}}, 60000)
	require.NoError(t, err)
	offender, err := s.AcquireTrialOccupancy(actor, 72, []*pb.TrialOccupancyRole{{PlayerId: 11}, {PlayerId: 22}}, 60000)
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	require.EqualValues(t, 22, offender)
	var rows int
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_role_occupancy WHERE owner_id=72").Scan(&rows))
	require.Zero(t, rows, "全成或全不成")
	_, err = s.DB.Exec("UPDATE match_role_occupancy SET expires_ms=1")
	require.NoError(t, err)
	_, err = s.AcquireTrialOccupancy(actor, 72, []*pb.TrialOccupancyRole{{PlayerId: 11}, {PlayerId: 22}}, 60000)
	require.NoError(t, err)
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_role_occupancy WHERE owner_id=72").Scan(&rows))
	require.Equal(t, 2, rows)
}

// 同 owner 重放幂等:身份不变,只延长不缩短。释放只删 owner 相同的行,也幂等。
func TestOccupancyTiDBSameOwnerReplayIsIdempotentAndReleaseOnlyTouchesOwner(t *testing.T) {
	s := nativeEntryStore(t)
	actor, first, _ := nativeEntryActor(t, s, 11)
	_, _, _ = nativeEntryActor(t, s, 22)
	roster := []*pb.TrialOccupancyRole{{PlayerId: 11, Account: first.OwnerAccount, OwnerRevision: 7}, {PlayerId: 22}}
	_, err := s.AcquireTrialOccupancy(actor, 71, roster, 600000)
	require.NoError(t, err)
	var long uint64
	require.NoError(t, s.DB.QueryRow("SELECT expires_ms FROM match_role_occupancy WHERE player_id=11").Scan(&long))
	_, err = s.AcquireTrialOccupancy(actor, 71, roster, 1000)
	require.NoError(t, err)
	var kept uint64
	var account string
	var revision uint64
	require.NoError(t, s.DB.QueryRow("SELECT expires_ms FROM match_role_occupancy WHERE player_id=11").Scan(&kept))
	require.Equal(t, long, kept, "重放不缩短租约")
	require.NoError(t, s.DB.QueryRow("SELECT account,owner_revision FROM match_role_occupancy WHERE player_id=22").Scan(&account, &revision))
	require.Equal(t, "entry-original-22", account, "对端身份从本库副本捕获")
	require.EqualValues(t, 7, revision)
	// 换了身份的重放不算同一份。
	_, err = s.AcquireTrialOccupancy(actor, 71, []*pb.TrialOccupancyRole{{PlayerId: 22, Account: "someone-else", OwnerRevision: 7}}, 1000)
	require.ErrorIs(t, err, ErrOriginalPlayer)

	require.NoError(t, s.ReleaseTrialOccupancy(actor, 72, []uint64{11, 22}))
	var rows int
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_role_occupancy").Scan(&rows))
	require.Equal(t, 2, rows, "别的 owner 放不开")
	for range 2 {
		require.NoError(t, s.ReleaseTrialOccupancy(actor, 71, []uint64{11, 22}))
	}
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM match_role_occupancy").Scan(&rows))
	require.Zero(t, rows)
}

func TestOccupancyRejectsBadArguments(t *testing.T) {
	s := &Store{}
	roster := []*pb.TrialOccupancyRole{{PlayerId: 11}}
	for _, bad := range []func() (uint64, error){
		func() (uint64, error) { return s.AcquireTrialOccupancy(context.Background(), 0, roster, 1000) },
		func() (uint64, error) { return s.AcquireTrialOccupancy(context.Background(), 71, roster, 0) },
		func() (uint64, error) {
			return s.AcquireTrialOccupancy(context.Background(), 71, roster, maxOccupancyTTLMs+1)
		},
		func() (uint64, error) { return s.AcquireTrialOccupancy(context.Background(), 71, nil, 1000) },
		func() (uint64, error) {
			return s.AcquireTrialOccupancy(context.Background(), 71, []*pb.TrialOccupancyRole{{PlayerId: 11}, {PlayerId: 11}}, 1000)
		},
		func() (uint64, error) {
			return s.AcquireTrialOccupancy(context.Background(), 71, []*pb.TrialOccupancyRole{{}}, 1000)
		},
	} {
		_, err := bad()
		require.ErrorIs(t, err, ErrOccupancyInvalid)
	}
	require.ErrorIs(t, s.ReleaseTrialOccupancy(context.Background(), 0, []uint64{11}), ErrOccupancyInvalid)
}

// 规格 §4:StartActivityBattle 是提交点。租约丢了就拒绝,不写票,不写意图。
func TestOccupancyTiDBStartRejectsAfterLeaseLost(t *testing.T) {
	for _, mode := range []string{"expired", "released", "taken", "identity"} {
		t.Run(mode, func(t *testing.T) {
			s := nativeEntryStore(t)
			actor, first, _ := nativeEntryActor(t, s, 11)
			_, second, _ := nativeEntryActor(t, s, 22)
			group := nativeTrialGroup(first, second)
			request, formation := nativeEntryTrial(t, s, group)
			want := ErrMatchEntryOccupied
			switch mode {
			case "expired":
				_, err := s.DB.Exec("UPDATE match_role_occupancy SET expires_ms=1")
				require.NoError(t, err)
			case "released":
				require.NoError(t, s.ReleaseTrialOccupancy(actor, 71, []uint64{22}))
			case "taken":
				_, err := s.DB.Exec("UPDATE match_role_occupancy SET owner_id=72 WHERE player_id=22")
				require.NoError(t, err)
			case "identity":
				// 占用行记的身份和开局名单对不上。
				_, err := s.DB.Exec("UPDATE match_role_occupancy SET owner_revision=6 WHERE player_id=22")
				require.NoError(t, err)
				want = ErrOriginalPlayer
			}
			raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
			require.NoError(t, err)
			before := nativeMatchSnapshot(t, s, actor)
			_, err = s.PrepareDirectAdmission(actor, &pb.MatchDirectIntentRecord{Kind: 2, OwnerId: 1, RequestLow: 71}, raw, formation, group)
			require.ErrorIs(t, err, want)
			require.Equal(t, before, nativeMatchSnapshot(t, s, actor), "拒绝不留痕:没有票、没有意图")
		})
	}
}

// 开局之后占用留到 guild 释放:票终局到 guild 结算之间不放新入场。提交点之后租约到期也不挡成局。
func TestOccupancyTiDBStartExtendsLeaseAndCompletesWithoutIt(t *testing.T) {
	s := nativeEntryStore(t)
	actor, first, solo := nativeEntryActor(t, s, 11)
	_, second, _ := nativeEntryActor(t, s, 22)
	group := nativeTrialGroup(first, second)
	request, formation := nativeEntryTrial(t, s, group)
	var before uint64
	require.NoError(t, s.DB.QueryRow("SELECT expires_ms FROM match_role_occupancy WHERE player_id=11").Scan(&before))
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	require.NoError(t, err)
	prepared, err := s.PrepareDirectAdmission(actor, &pb.MatchDirectIntentRecord{Kind: 2, OwnerId: 1, RequestLow: 71}, raw, formation, group)
	require.NoError(t, err)
	var after uint64
	require.NoError(t, s.DB.QueryRow("SELECT expires_ms FROM match_role_occupancy WHERE player_id=11").Scan(&after))
	require.Greater(t, after, before, "占用要盖住这一局的期限")

	_, err = s.DB.Exec("UPDATE match_role_occupancy SET expires_ms=1")
	require.NoError(t, err)
	actual, err := s.CompletePreparedDirect(actor, prepared)
	require.NoError(t, err, "票已写,到期不影响接着成局")
	require.Equal(t, formation.BattleId, actual.BattleId)
	// 票已写,到期的占用也不能被别的大厅拿走。
	_, err = s.AcquireTrialOccupancy(actor, 72, []*pb.TrialOccupancyRole{{PlayerId: 11}}, 60000)
	require.ErrorIs(t, err, ErrMatchEntryOccupied)

	// 票终局、guild 还没结算:占用接着挡新入场。
	_, err = s.DB.Exec("UPDATE match_role_occupancy SET expires_ms=? WHERE owner_id=71", after)
	require.NoError(t, err)
	_, err = s.DB.Exec("UPDATE match_authority_ticket SET state=4")
	require.NoError(t, err)
	_, _, _, err = s.PrepareJoin(actor, first, solo, "entry-bound")
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	require.NoError(t, s.ReleaseTrialOccupancy(actor, 71, []uint64{11, 22}))
	_, _, _, err = s.PrepareJoin(actor, first, solo, "entry-bound")
	require.NoError(t, err)
}

// 规格 §3.3:冻结之后写被拒;新主自愈后通过,旧主被拒;副本缺行时重试一次。全部经真实目录替身。
func healStore(t *testing.T) (*Store, *fencetest.Directory) {
	t.Helper()
	s := testStore(t)
	directory := fencetest.NewDirectory()
	require.NoError(t, s.ConfigureRoleFence(context.Background(), directory, fencetest.Admissions{Valid: true}))
	return s, directory
}

func TestOccupancyTiDBMissingReplicaHealsAndRetriesOnce(t *testing.T) {
	s, directory := healStore(t)
	directory.Set(11, "seller", 7, 0)
	_, err := s.AcquireTrialOccupancy(context.Background(), 71, []*pb.TrialOccupancyRole{{PlayerId: 11}}, 60000)
	require.NoError(t, err, "副本缺行不是拒绝")
	require.Equal(t, []uint64{11}, directory.Reads, "只读一次目录")
	fence, err := rolefence.Read(context.Background(), s.DB, 11)
	require.NoError(t, err)
	require.Equal(t, "seller", fence.Account)
	// 目录里也没有:和旧的 account_role 缺行一样。
	_, err = s.AcquireTrialOccupancy(context.Background(), 72, []*pb.TrialOccupancyRole{{PlayerId: 12}}, 60000)
	require.ErrorIs(t, err, ErrOriginalPlayer)
	// 目录不可达:没有结论,不是拒绝。
	directory.Err = rolefence.ErrUnavailable
	_, err = s.AcquireTrialOccupancy(context.Background(), 72, []*pb.TrialOccupancyRole{{PlayerId: 13}}, 60000)
	require.ErrorIs(t, err, rolefence.ErrUnavailable)
	require.False(t, errors.Is(err, ErrOriginalPlayer))
}

func TestOccupancyTiDBFrozenRoleRejectsMatchWritesEvenAfterHeal(t *testing.T) {
	s, directory := healStore(t)
	ctx := nativeRoleActor(t, context.Background(), 11, "seller", 7)
	state := directory.Set(11, "seller", 7, 901)
	_, err := rolefence.Apply(ctx, s.DB, state)
	require.NoError(t, err)
	_, err = s.AcquireTrialOccupancy(ctx, 71, []*pb.TrialOccupancyRole{{PlayerId: 11}}, 60000)
	require.ErrorIs(t, err, ErrOriginalPlayer)
	_, err = s.Ticket(ctx, 11)
	require.ErrorIs(t, err, ErrOriginalPlayer)
	require.ErrorIs(t, s.SavePlan(ctx, 11, &pb.MatchNetworkProfile{}, 1), ErrOriginalPlayer)
	require.Len(t, directory.Reads, 3, "真冻结:每个入口自愈一次,第二次判定才拒绝")
	// 解冻推送丢了:下一次访问自愈,照常放行。
	directory.Set(11, "seller", 7, 0)
	_, err = s.AcquireTrialOccupancy(ctx, 71, []*pb.TrialOccupancyRole{{PlayerId: 11}}, 60000)
	require.NoError(t, err)
}

func TestOccupancyTiDBHealLetsNewOwnerInAndKeepsOldOwnerOut(t *testing.T) {
	s, directory := healStore(t)
	oldOwner := nativeRoleActor(t, context.Background(), 11, "seller", 7)
	newOwner := nativeRoleActor(t, context.Background(), 11, "buyer", 8)
	// 过户之前全部参与者都已冻结;过户的推送丢了,副本停在冻结态。
	_, err := rolefence.Apply(context.Background(), s.DB, directory.Set(11, "seller", 7, 901))
	require.NoError(t, err)
	directory.Set(11, "buyer", 8, 0)

	before := append([]uint64(nil), directory.Reads...)
	_, err = s.Ticket(newOwner, 11)
	require.NoError(t, err, "新主修订号比副本大,自愈后通过")
	require.Len(t, directory.Reads, len(before)+1)

	_, err = s.Ticket(oldOwner, 11)
	require.ErrorIs(t, err, ErrOriginalPlayer, "旧主修订号比副本小,直接拒绝")
	require.Len(t, directory.Reads, len(before)+1, "旧主不触发自愈")
}

// 挂起全集:从目录原来的跨库守卫搬来的用例,改成直接测 RoleFencePending。
func pendingStore(t *testing.T) (context.Context, *Store) {
	t.Helper()
	s := nativeEntryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx, s
}

func pendingBlocker(t *testing.T, ctx context.Context, s *Store, player uint64) string {
	t.Helper()
	tx, err := s.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	blocker, err := RoleFencePending(ctx, tx, &rfpb.RoleFenceState{PlayerId: player, Account: "seller", OwnerRevision: 1, FreezeToken: 88, Version: 2, HomeZone: 1})
	require.NoError(t, err)
	return blocker
}

// 夹具按正式 PB 列全集写入,不另建一份状态或表定义。
func insertPendingFixture(t *testing.T, s *Store, table string, row proto.Message) {
	t.Helper()
	m := row.ProtoReflect()
	fields := m.Descriptor().Fields()
	var columns, marks string
	var values []any
	for n := 0; n < fields.Len(); n++ {
		f := fields.Get(n)
		if n > 0 {
			columns, marks = columns+",", marks+","
		}
		columns, marks = columns+"`"+string(f.Name())+"`", marks+"?"
		v := m.Get(f)
		switch f.Kind() {
		case protoreflect.Uint32Kind, protoreflect.Uint64Kind:
			values = append(values, v.Uint())
		case protoreflect.Int32Kind, protoreflect.Int64Kind:
			values = append(values, v.Int())
		case protoreflect.BoolKind:
			values = append(values, v.Bool())
		case protoreflect.EnumKind:
			values = append(values, int32(v.Enum()))
		case protoreflect.StringKind:
			values = append(values, v.String())
		case protoreflect.BytesKind:
			values = append(values, append([]byte{}, v.Bytes()...))
		default:
			t.Fatalf("挂起夹具遇到未支持字段: %s", f.FullName())
		}
	}
	_, err := s.DB.Exec("INSERT INTO `"+table+"` ("+columns+") VALUES ("+marks+")", values...)
	require.NoError(t, err)
}

func TestPendingTiDBMatchBlocksFreezeAndRemoval(t *testing.T) {
	for _, test := range []struct {
		name, blocker, table string
		row                  proto.Message
		predicate            string
	}{
		{"match-queued", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: "original", State: 1}, "player_id=11"},
		{"match-formed", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: "original", FormationId: 51, State: 2}, "player_id=11"},
		{"match-created", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: "original", FormationId: 51, State: 3}, "player_id=11"},
		{"match-unknown", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, State: 99}, "player_id=11"},
		{"match-state-zero-with-ticket", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: "original"}, "player_id=11"},
		{"match-state-zero-with-request", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, RequestId: "original"}, "player_id=11"},
		{"match-state-zero-with-formation", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, FormationId: 51}, "player_id=11"},
		{"match-state-zero-with-mode", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, Mode: 1}, "player_id=11"},
		{"match-state-zero-with-config", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, ConfigId: 1}, "player_id=11"},
		{"match-state-zero-with-team-size", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, TeamSize: 1}, "player_id=11"},
		{"match-state-zero-with-enqueue", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, EnqueuedMs: 1}, "player_id=11"},
		{"match-state-zero-with-not-before", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, NotBeforeMs: 1}, "player_id=11"},
		{"match-state-zero-with-home", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, ZoneId: 1}, "player_id=11"},
		{"match-state-zero-with-rating", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, RatingCenti: -1}, "player_id=11"},
		{"match-state-zero-with-team", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, TeamId: 1}, "player_id=11"},
		{"match-state-zero-with-network", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, NetworkProfile: []byte{1}}, "player_id=11"},
		{"match-state-zero-with-expiry", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, ExpiresMs: 1}, "player_id=11"},
		{"match-state-zero-with-selected-region", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, SelectedRegion: 1}, "player_id=11"},
		{"match-state-zero-with-preferred-region", "match_authority_ticket", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, PreferredRegion: 1}, "player_id=11"},
		{"match-challenge-challenger-pending", "match_challenge_invitation", "match_challenge_invitation", &pb.MatchChallengeInvitationRecord{ChallengeId: 51, ChallengerPlayerId: 11, TargetPlayerId: 12, State: 1, ExpiresMs: ^uint64(0)}, "challenger_player_id=11"},
		{"match-challenge-target-pending", "match_challenge_invitation", "match_challenge_invitation", &pb.MatchChallengeInvitationRecord{ChallengeId: 51, ChallengerPlayerId: 12, TargetPlayerId: 11, State: 1, ExpiresMs: ^uint64(0)}, "target_player_id=11"},
		{"match-challenge-state-zero", "match_challenge_invitation", "match_challenge_invitation", &pb.MatchChallengeInvitationRecord{ChallengeId: 51, ChallengerPlayerId: 11, TargetPlayerId: 12, State: 0}, "challenger_player_id=11"},
		{"match-challenge-unknown", "match_challenge_invitation", "match_challenge_invitation", &pb.MatchChallengeInvitationRecord{ChallengeId: 51, ChallengerPlayerId: 11, TargetPlayerId: 12, State: 99}, "challenger_player_id=11"},
		{"team-member", "team_membership", "team_membership", &teampb.TeamMembershipRecord{PlayerId: 11, TeamId: 51, Epoch: 2}, "player_id=11"},
		{"team-invite", "team_invite_index", "team_invite_index", &teampb.TeamInviteIndexRecord{InviteeId: 11, TeamId: 51, ExpiresMs: ^uint64(0)}, "invitee_id=11"},
		{"team-application", "team_application_index", "team_application_index", &teampb.TeamApplicationIndexRecord{ApplicantId: 11, TeamId: 51, ExpiresMs: ^uint64(0)}, "applicant_id=11"},
		{"team-launch-missing-original", "team_launch_role", "team_launch_role", &teampb.TeamLaunchRoleRecord{PlayerId: 11, ActorId: 12, RequestHigh: 51, State: 5}, "player_id=11"},
		{"spectator-pending", "match_spectator", "match_spectator", &pb.MatchSpectatorRecord{PlayerId: 11, State: 1}, "player_id=11"},
		{"spectator-active", "match_spectator", "match_spectator", &pb.MatchSpectatorRecord{PlayerId: 11, State: 2}, "player_id=11"},
		{"spectator-cancelling", "match_spectator", "match_spectator", &pb.MatchSpectatorRecord{PlayerId: 11, State: 3}, "player_id=11"},
		{"spectator-unknown-result", "match_spectator", "match_spectator", &pb.MatchSpectatorRecord{PlayerId: 11, State: 4}, "player_id=11"},
		{"spectator-invalid-state", "match_spectator", "match_spectator", &pb.MatchSpectatorRecord{PlayerId: 11, State: 99}, "player_id=11"},
		{"occupancy-held", "match_role_occupancy", "match_role_occupancy", &pb.MatchRoleOccupancyRecord{PlayerId: 11, OwnerKind: 1, OwnerId: 71, ExpiresMs: ^uint64(0)}, "player_id=11"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, s := pendingStore(t)
			insertPendingFixture(t, s, test.table, test.row)
			require.Equal(t, test.blocker, pendingBlocker(t, ctx, s, 11))
			_, err := s.DB.Exec("DELETE FROM `" + test.table + "` WHERE " + test.predicate)
			require.NoError(t, err)
			require.Empty(t, pendingBlocker(t, ctx, s, 11))
		})
	}
}

func TestPendingTiDBMatchKnownTerminalsDoNotBlock(t *testing.T) {
	ctx, s := pendingStore(t)
	insertPendingFixture(t, s, "team_launch", &teampb.TeamLaunchRecord{ActorId: 11, RequestHigh: 51, State: 5})
	insertPendingFixture(t, s, "team_launch_role", &teampb.TeamLaunchRoleRecord{PlayerId: 11, ActorId: 11, RequestHigh: 51, State: 5})
	insertPendingFixture(t, s, "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: "finished", FormationId: 51, State: 4})
	for index, state := range []uint32{1, 2, 3} {
		expires := ^uint64(0)
		if state == 1 {
			expires = 1
		}
		insertPendingFixture(t, s, "match_challenge_invitation", &pb.MatchChallengeInvitationRecord{ChallengeId: uint64(51 + index), ChallengerPlayerId: 11, TargetPlayerId: 12, State: state, ExpiresMs: expires})
	}
	insertPendingFixture(t, s, "match_spectator", &pb.MatchSpectatorRecord{PlayerId: 11, State: 5})
	insertPendingFixture(t, s, "match_role_occupancy", &pb.MatchRoleOccupancyRecord{PlayerId: 11, OwnerKind: 1, OwnerId: 71, ExpiresMs: 1})
	require.Empty(t, pendingBlocker(t, ctx, s, 11), "过期的占用视为空闲")
}

func TestPendingTiDBExactEmptyMatchPlaceholderDoesNotTrapFreeze(t *testing.T) {
	for _, account := range []string{"", "seller"} {
		t.Run("placeholder-owner-"+account, func(t *testing.T) {
			ctx, s := pendingStore(t)
			insertPendingFixture(t, s, "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, OwnerAccount: account, OwnerRevision: 1})
			require.Empty(t, pendingBlocker(t, ctx, s, 11))
		})
	}
}

func TestPendingTiDBTeamLaunchTerminalRequiresOriginalAndProjectionTogether(t *testing.T) {
	for _, test := range []struct {
		name string
		rows map[string]proto.Message
	}{
		{"team-original-active", map[string]proto.Message{
			"team_launch":      &teampb.TeamLaunchRecord{ActorId: 11, RequestHigh: 51, State: 1},
			"team_launch_role": &teampb.TeamLaunchRoleRecord{PlayerId: 11, ActorId: 11, RequestHigh: 51, State: 5},
		}},
		{"team-projection-active", map[string]proto.Message{
			"team_launch":      &teampb.TeamLaunchRecord{ActorId: 11, RequestHigh: 51, State: 5},
			"team_launch_role": &teampb.TeamLaunchRoleRecord{PlayerId: 11, ActorId: 11, RequestHigh: 51, State: 1},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, s := pendingStore(t)
			for table, row := range test.rows {
				insertPendingFixture(t, s, table, row)
			}
			require.Equal(t, "team_launch_role", pendingBlocker(t, ctx, s, 11))
		})
	}
}

func TestPendingTiDBMatchNullStateRemainsBlocking(t *testing.T) {
	for _, test := range []struct {
		table, column, predicate, blocker string
		row                               proto.Message
	}{
		{"match_authority_ticket", "state", "player_id=11", "match_authority_ticket", &pb.MatchAuthorityTicketRecord{PlayerId: 11, State: 4}},
		{"match_challenge_invitation", "state", "challenger_player_id=11", "match_challenge_invitation", &pb.MatchChallengeInvitationRecord{ChallengeId: 51, ChallengerPlayerId: 11, TargetPlayerId: 12, State: 3}},
	} {
		t.Run(test.table, func(t *testing.T) {
			ctx, s := pendingStore(t)
			insertPendingFixture(t, s, test.table, test.row)
			_, err := testInstall(t, s).ExecContext(ctx, "ALTER TABLE `"+test.table+"` MODIFY `"+test.column+"` INT UNSIGNED NULL")
			require.NoError(t, err)
			_, err = s.DB.ExecContext(ctx, "UPDATE `"+test.table+"` SET `"+test.column+"`=NULL WHERE "+test.predicate)
			require.NoError(t, err)
			require.Equal(t, test.blocker, pendingBlocker(t, ctx, s, 11))
		})
	}
}

// 规格 §3.5 的串行点:冻结和新匹配参与只能有一方先赢。票先赢则挂起判定读到票并回滚,冻结先赢则新参与在副本行上被拒。
func TestPendingTiDBMatchParticipationAndFreezeShareReplicaLock(t *testing.T) {
	const secret = "fixture-only-coordinator-secret-0000"
	s := nativeEntryStore(t)
	participant, err := rolefence.NewParticipantServer(s.DB, RoleFencePending, secret)
	require.NoError(t, err)
	for player := uint64(51); player <= 56; player++ {
		actor, ticket, request := nativeEntryActor(t, s, player)
		in := &rfpb.PrepareRoleFreezeRequest{State: &rfpb.RoleFenceState{PlayerId: player, Account: ticket.OwnerAccount, OwnerRevision: 7, FreezeToken: 1000 + player, Version: 2, HomeZone: 1}}
		require.NoError(t, serviceauth.Sign(rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName, in, rolefence.CoordinatorCaller, secret, uint64(time.Now().UnixMilli())))
		start := make(chan struct{})
		prepared := make(chan *rfpb.PrepareRoleFreezeResponse, 1)
		joined := make(chan error, 1)
		go func() {
			<-start
			out, err := participant.PrepareRoleFreeze(context.Background(), in)
			if err != nil {
				out = nil
			}
			prepared <- out
		}()
		go func() {
			<-start
			_, _, _, err := s.PrepareJoin(actor, ticket, request, "entry-bound")
			joined <- err
		}()
		close(start)
		out, joinErr := <-prepared, <-joined
		require.NotNil(t, out)
		require.True(t, out.Clean != (joinErr == nil), "冻结与新匹配参与只能一方提交: clean=%v join=%v", out.Clean, joinErr)
		fence, err := rolefence.Read(context.Background(), s.DB, player)
		require.NoError(t, err)
		require.Equal(t, out.Clean, fence.FreezeToken != 0)
	}
}
