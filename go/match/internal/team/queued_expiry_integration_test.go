package team

import (
	"context"
	"testing"

	"match/internal/authority"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	matchpb "proto/match"
	teampb "proto/team"
)

// 原生 SQL 回归：成员查询先结束到期票，恢复关闭全队原件；原请求丢回包后仍能查确证终态，原队无需踢人重组即可再排。
func TestTeamTiDBExpiredMemberTicketRecoversIntentAndAllowsRequeue(t *testing.T) {
	f := newNativeTeamFixture(t)
	f.create(t, 1, 101)
	f.inviteAndAccept(t, 1, 2, 101)
	start := func() *teampb.TeamLaunchRecord {
		t.Helper()
		for _, player := range []uint64{1, 2} {
			f.profile(t, player)
		}
		snap, err := f.store.ReadFree(f.actor(t, 1), 1)
		require.NoError(t, err)
		request := &teampb.StartTeamMatchRequest{BattleConfigId: 1, ExpectedTeamId: 101, TargetTeamSize: 2, MatchMode: matchpb.MatchMode_MATCH_MODE_PVP_TEAM, RequestId: uuid.NewString()}
		pinned, launch, err := f.store.sql.createLaunch(f.actor(t, 1), snap, 1, request, uuid.NewString(), MatchRoster(snap.Record), 100)
		require.NoError(t, err)
		require.NotNil(t, pinned.Commit)
		require.Equal(t, uint32(2), launch.State)
		return launch
	}
	first := start()
	original, err := authority.DecodeTeamLaunch(first)
	require.NoError(t, err)
	requestID := original.Original.Request.RequestId
	data, err := f.owner.TeamMatchIntent(f.actor(t, 1), 1, requestID)
	require.NoError(t, err)
	require.Equal(t, uint32(2), data.State)
	require.Zero(t, data.BattleId)

	_, err = f.owner.DB.Exec("UPDATE match_authority_ticket SET expires_ms=1 WHERE player_id=2")
	require.NoError(t, err)
	ticket, err := f.owner.Ticket(f.actor(t, 2), 2)
	require.NoError(t, err)
	require.Equal(t, uint32(4), ticket.State)
	closed, err := f.owner.MaintainQueuedTeam(context.Background(), first)
	require.NoError(t, err)
	require.True(t, closed)
	closed, err = f.owner.MaintainQueuedTeam(context.Background(), first)
	require.NoError(t, err)
	require.False(t, closed, "重复维护不能产生第二次关闭")

	h := newServiceHarness(t)
	h.addPlayers(1, 2)
	h.svcCtx.Authority, h.service.store = f.owner, f.store
	response := h.service.GetMyTeam(f.actor(t, 1), 1, &teampb.GetMyTeamRequest{MatchRequestId: requestID})
	require.Zero(t, response.GetErrorMessage().GetId())
	require.Equal(t, uint32(5), response.GetMatchIntent().GetState())
	require.Zero(t, response.GetMatchIntent().GetBattleId())
	require.Equal(t, requestID, response.GetMatchIntent().GetRequestId())
	second := start()
	require.NotEqual(t, first.Payload, second.Payload)
	tickets, err := f.owner.TeamLaunchTickets(context.Background(), second)
	require.NoError(t, err)
	require.Len(t, tickets, 2)
}
