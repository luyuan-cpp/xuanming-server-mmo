package logic

import (
	"context"
	"testing"

	"match/internal/pkg/ctxkeys"
	"match/internal/playercontract"
	"match/internal/svc"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	battlepb "proto/battle"
	base "proto/common/base"
	matchpb "proto/match"
	plpb "proto/player_locator"
	"shared/serviceauth"
)

func TestValidateStartGuildWarBattle(t *testing.T) {
	validCtx := &battlepb.BattleActivityContext{
		Kind: battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_WAR,
		GuildWar: &battlepb.GuildWarBattleContext{
			WarBattleKind:   1,
			AttackerGuildId: 100,
			DefenderGuildId: 200,
		},
	}

	t.Run("live pvp valid", func(t *testing.T) {
		req := &matchpb.StartGuildWarBattleRequest{
			BattleConfigId:    1,
			MatchMode:         uint32(matchpb.MatchMode_MATCH_MODE_GUILD_WAR_PVP),
			AttackerPlayerIds: []uint64{101, 102},
			DefenderPlayerIds: []uint64{201},
			ActivityContext:   validCtx,
			RequestId:         1,
		}
		require.Empty(t, validateStartGuildWarBattle(req))
	})

	t.Run("siege valid with ai defender", func(t *testing.T) {
		req := &matchpb.StartGuildWarBattleRequest{
			BattleConfigId:    1,
			MatchMode:         uint32(matchpb.MatchMode_MATCH_MODE_GUILD_WAR_SIEGE),
			AttackerPlayerIds: []uint64{101},
			AiDefenderSnapshots: []*battlepb.BattlePlayerSnapshot{
				{PlayerId: 9001, TeamIndex: 1, AiControlled: true},
			},
			ActivityContext: validCtx,
			RequestId:       2,
		}
		require.Empty(t, validateStartGuildWarBattle(req))
	})

	t.Run("overlapping attacker and defender rejected", func(t *testing.T) {
		req := &matchpb.StartGuildWarBattleRequest{
			BattleConfigId:    1,
			MatchMode:         uint32(matchpb.MatchMode_MATCH_MODE_GUILD_WAR_PVP),
			AttackerPlayerIds: []uint64{101},
			DefenderPlayerIds: []uint64{101},
			ActivityContext:   validCtx,
			RequestId:         3,
		}
		require.NotEmpty(t, validateStartGuildWarBattle(req))
	})

	t.Run("siege without ai defender rejected", func(t *testing.T) {
		req := &matchpb.StartGuildWarBattleRequest{
			BattleConfigId:    1,
			MatchMode:         uint32(matchpb.MatchMode_MATCH_MODE_GUILD_WAR_SIEGE),
			AttackerPlayerIds: []uint64{101},
			ActivityContext:   validCtx,
			RequestId:         4,
		}
		require.NotEmpty(t, validateStartGuildWarBattle(req))
	})
}

func TestStartGuildWarBattleRejectsClientSession(t *testing.T) {
	ctx := ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{SessionId: 1, PlayerId: 42})
	_, err := NewStartGuildWarBattleLogic(ctx, &svc.ServiceContext{}).StartGuildWarBattle(&matchpb.StartGuildWarBattleRequest{})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestGuildWarPrecheckReturnsConflictBattleIDForSpectate(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	rawSess, err := proto.Marshal(&plpb.PlayerSession{
		PlayerId: 201,
		State:    plpb.PlayerSessionState_SESSION_STATE_ONLINE,
	})
	require.NoError(t, err)
	require.NoError(t, mr.Set(playercontract.SessionKey(201), string(rawSess)))
	require.NoError(t, mr.Set(playercontract.BattleLockKey(201), "778899"))

	auth, err := serviceauth.New(map[string]string{"guild": "0123456789abcdef0123456789abcdef"})
	require.NoError(t, err)
	svcCtx := &svc.ServiceContext{
		PlayerRedis:   rdb,
		GuildCallAuth: auth,
	}
	l := NewStartGuildWarBattleLogic(context.Background(), svcCtx)
	rej, bid := l.precheckGuildWarMember(201)
	require.Equal(t, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE, rej)
	require.Equal(t, uint64(778899), bid)
}

func TestGatherTeamAssignmentGuildWarModes(t *testing.T) {
	fPvp := &matchpb.MatchFormationRecord{
		Mode: uint32(matchpb.MatchMode_MATCH_MODE_GUILD_WAR_PVP),
		Members: []*matchpb.MatchFormationMember{
			{PlayerId: 101, TeamIndex: 0},
			{PlayerId: 102, TeamIndex: 0},
			{PlayerId: 201, TeamIndex: 1},
		},
	}
	teams, err := gatherTeamAssignment(nil, fPvp, []uint64{101, 102, 201})
	require.NoError(t, err)
	require.Equal(t, []uint32{0, 0, 1}, teams)

	fSiege := &matchpb.MatchFormationRecord{
		Mode: uint32(matchpb.MatchMode_MATCH_MODE_GUILD_WAR_SIEGE),
		Members: []*matchpb.MatchFormationMember{
			{PlayerId: 101, TeamIndex: 0},
		},
	}
	teams, err = gatherTeamAssignment(nil, fSiege, []uint64{101})
	require.NoError(t, err)
	require.Equal(t, []uint32{0}, teams)
}
