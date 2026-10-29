package team

import (
	"context"
	"fmt"
	"testing"

	matchpb "proto/match"
	teampb "proto/team"

	"github.com/stretchr/testify/require"
)

type pvpBattlePort struct {
	*fakeBattlePort
	modes []uint32
}

func (p *pvpBattlePort) EnqueuePartyMode(ctx context.Context, mode, config, target uint32,
	teamID uint64, players []uint64, zones map[uint64]uint32) (uint64, error) {
	p.modes = append(p.modes, mode)
	return p.EnqueueParty(ctx, config, target, teamID, players, zones)
}

func (p *pvpBattlePort) PartyQueueMode(context.Context, uint64, uint64) (uint32, error) {
	return 7, nil
}

func TestPvpTeamStartQueuesCurrentRosterForEverySize(t *testing.T) {
	for size := 1; size <= 5; size++ {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			players := make([]uint64, size)
			for i := range players {
				players[i] = 81001 + uint64(i)
			}
			b := newBattleHarness(t, players...)
			port := &pvpBattlePort{fakeBattlePort: b.port}
			b.service.starter = port
			response := b.ok(b.server.StartTeamMatch(asPlayer(players[0]), &teampb.StartTeamMatchRequest{
				ExpectedTeamId: b.teamId, MatchMode: matchpb.MatchMode(7),
			}))
			requireCode(t, 0, response)
			require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_QUEUED, response.Team.MatchState)
			require.Equal(t, uint32(size), response.Team.MatchTargetSize)
			require.Equal(t, matchpb.MatchMode(7), response.Team.MatchMode)
			require.True(t, response.Team.PvpMatchSupported)
			require.Equal(t, []uint32{7}, port.modes)
			require.Len(t, port.enqueues, 1)
			require.Equal(t, players, port.enqueues[0].roster)
			require.Empty(t, port.ticketCalls, "PVP满队也不能当成PVE即时开局")
			require.Empty(t, port.gathers)
			if size > 1 {
				memberView := b.myTeam(players[1])
				require.Equal(t, matchpb.MatchMode(7), memberView.MatchMode)
				require.Equal(t, uint32(size), memberView.MatchTargetSize)
			}
		})
	}
}

func TestPvpTeamStartRejectsMemberAndInvalidTarget(t *testing.T) {
	for _, test := range []struct {
		name   string
		caller uint64
		target uint32
		want   uint32
	}{
		{"队员不能发起", 82002, 3, ErrNotLeader},
		{"目标不能小于队伍", 82001, 1, ErrSizeExceeded},
		{"每侧最多五人", 82001, 6, ErrSizeExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := newBattleHarness(t, 82001, 82002)
			port := &pvpBattlePort{fakeBattlePort: b.port}
			b.service.starter = port
			response := b.ok(b.server.StartTeamMatch(asPlayer(test.caller), &teampb.StartTeamMatchRequest{
				ExpectedTeamId: b.teamId, MatchMode: matchpb.MatchMode(7), TargetTeamSize: test.target,
			}))
			requireCode(t, test.want, response)
			require.Empty(t, port.enqueues)
			require.Empty(t, port.ticketCalls)
		})
	}
}

func TestPvpTeamStartAllowsExplicitRealPlayerBackfill(t *testing.T) {
	b := newBattleHarness(t, 83001, 83002)
	// 旧开关只控制PVE补位，不关闭独立声明能力的真人PVP。
	b.service.svcCtx.Config.Team.FillQueueDisabled = true
	port := &pvpBattlePort{fakeBattlePort: b.port}
	b.service.starter = port
	response := b.ok(b.server.StartTeamMatch(asPlayer(83001), &teampb.StartTeamMatchRequest{
		ExpectedTeamId: b.teamId, MatchMode: matchpb.MatchMode(7), TargetTeamSize: 5,
	}))
	requireCode(t, 0, response)
	require.Equal(t, uint32(5), response.Team.MatchTargetSize)
	require.Equal(t, []uint64{83001, 83002}, port.enqueues[0].roster)
	require.Empty(t, port.gathers)
}

func TestPveTeamExplicitModeStillStartsCurrentRoster(t *testing.T) {
	for size := 1; size <= 5; size++ {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			players := make([]uint64, size)
			for i := range players {
				players[i] = 84001 + uint64(i)
			}
			b := newBattleHarness(t, players...)
			response := b.ok(b.server.StartTeamMatch(asPlayer(players[0]), &teampb.StartTeamMatchRequest{
				ExpectedTeamId: b.teamId, BattleConfigId: testBattleConfig, MatchMode: matchpb.MatchMode_MATCH_MODE_PVE_TEAM,
			}))
			requireCode(t, 0, response)
			require.Empty(t, b.port.enqueues)
			require.Len(t, b.port.ticketCalls, 1)
			require.Equal(t, players, b.port.ticketCalls[0].roster)
		})
	}
}
