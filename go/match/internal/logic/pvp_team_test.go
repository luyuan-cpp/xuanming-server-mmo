package logic

import (
	"context"
	"fmt"
	"testing"

	"match/internal/constants"
	"match/internal/svc"
	matchpb "proto/match"

	"github.com/stretchr/testify/require"
)

func TestPvpTeamSoloQueuesEverySideSizeWithoutStartingAlone(t *testing.T) {
	for size := uint32(1); size <= 5; size++ {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s, mr := newPartySvcCtx(t)
			calls := stubPveGather(t)
			setPlayerLocation(t, mr, 71001, 1)
			response, err := NewJoinQueueLogic(context.Background(), s).JoinQueue(&matchpb.JoinQueueRequest{
				PlayerId: 71001, Mode: matchpb.MatchMode(7), TeamSize: size,
			})
			require.NoError(t, err)
			require.Zero(t, response.ErrorCode, "每方1到5人的真人PVP都应开放")
			key := matchSizedQueueKey(7, 0, size)
			require.Equal(t, []string{"71001"}, queueMembers(t, s, key))
			require.Equal(t, size*2, requiredPlayersForQueue(s, key, 7, 0))
			matchQueueOnce(s, key)
			expectNoPveGather(t, calls)
		})
	}
}

func enqueuePvpParty(t *testing.T, s *svc.ServiceContext, teamID uint64, size uint32, players ...uint64) {
	t.Helper()
	failed, err := NewTeamBattleStarter(s).EnqueuePartyMode(context.Background(), 7, 0, size, teamID, players, nil)
	require.NoError(t, err)
	require.Zero(t, failed)
}

func TestPvpTeamKeepsCompleteSidesForAllSizes(t *testing.T) {
	for size := uint32(1); size <= 5; size++ {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s, _, _ := newPartySvcCtx(t)
			var left, right []uint64
			for i := uint32(0); i < size; i++ {
				left = append(left, 72001+uint64(i))
				right = append(right, 72101+uint64(i))
			}
			enqueuePvpParty(t, s, 9201, size, left...)
			key := matchSizedQueueKey(7, 0, size)
			_, _, ready := popGroup(s, key, size*2)
			require.False(t, ready, "一支满队仍须等待真人对手")
			enqueuePvpParty(t, s, 9202, size, right...)
			teams := map[uint64]uint32{}
			members, tickets, ok := popGroup(s, key, size*2, teams)
			require.True(t, ok)
			require.Len(t, members, int(size*2))
			require.Len(t, tickets, int(size*2))
			for _, player := range left {
				require.Equal(t, uint32(0), teams[player])
			}
			for _, player := range right {
				require.Equal(t, uint32(1), teams[player])
			}
		})
	}
}

func TestPvpTeamCountAloneCannotSplitParties(t *testing.T) {
	s, _, _ := newPartySvcCtx(t)
	// 4+4+2虽为10人，却无法装成两支5人队；不能拆开两人队填缝。
	enqueuePvpParty(t, s, 9211, 5, 73001, 73002, 73003, 73004)
	enqueuePvpParty(t, s, 9212, 5, 73101, 73102, 73103, 73104)
	enqueuePvpParty(t, s, 9213, 5, 73201, 73202)
	key := matchSizedQueueKey(7, 0, 5)
	_, _, ok := popGroup(s, key, 10)
	require.False(t, ok)
	require.Len(t, queueMembers(t, s, key), 10)
}

func TestPvpTeamMixesSolosWithoutSplittingTheirParty(t *testing.T) {
	s, mr, _ := newPartySvcCtx(t)
	enqueuePvpParty(t, s, 9221, 3, 74001, 74002)
	enqueuePvpParty(t, s, 9222, 3, 74101, 74102, 74103)
	setPlayerLocation(t, mr, 74201, 1)
	response, err := NewJoinQueueLogic(context.Background(), s).JoinQueue(&matchpb.JoinQueueRequest{PlayerId: 74201, Mode: matchpb.MatchMode(7), TeamSize: 3})
	require.NoError(t, err)
	require.Zero(t, response.ErrorCode)
	teams := map[uint64]uint32{}
	members, _, ok := popGroup(s, matchSizedQueueKey(7, 0, 3), 6, teams)
	require.True(t, ok)
	require.Len(t, members, 6)
	require.Equal(t, teams[74001], teams[74002])
	require.Equal(t, teams[74001], teams[74201])
	require.NotEqual(t, teams[74001], teams[74101])
}

func TestPvpTeamCancelMemberWithdrawsWholeParty(t *testing.T) {
	s, _, port := newPartySvcCtx(t)
	enqueuePvpParty(t, s, 9231, 2, 75001, 75002)
	_, err := NewCancelQueueLogic(context.Background(), s).CancelQueue(&matchpb.CancelQueueRequest{PlayerId: 75002})
	require.NoError(t, err)
	requireNoTicket(t, s, 75001, 75002)
	require.Empty(t, queueMembers(t, s, matchSizedQueueKey(7, 0, 2)))
	require.Len(t, port.endedCalls(), 1)
}

func TestPvpTeamGatherUsesExplicitSidesAndRejectsInvalidSides(t *testing.T) {
	t.Run("完整分边原样送建房", func(t *testing.T) {
		s, mr := newGatherSvcCtx(t)
		members := []uint64{76001, 76002, 76003, 76004}
		fake := stubGatherRPCs(t, nil)
		for _, player := range members {
			setPlayerLocation(t, mr, player, 1)
		}
		teams := map[uint64]uint32{76001: 0, 76002: 1, 76003: 0, 76004: 1}
		require.True(t, runGatherWithOptions(s, matchpb.MatchMode(7), 0, members, false, false, nil, gatherOptions{teamByPlayer: teams}))
		require.Len(t, fake.created, 1)
		for _, snapshot := range fake.created[0].Players {
			require.Equal(t, teams[snapshot.PlayerId], snapshot.TeamIndex)
		}
	})
	t.Run("人数不等不冻结任何玩家", func(t *testing.T) {
		s, _ := newGatherSvcCtx(t)
		fake := stubGatherRPCs(t, nil)
		require.False(t, runGatherWithOptions(s, matchpb.MatchMode(7), 0, []uint64{76101, 76102}, false, false, nil, gatherOptions{teamByPlayer: map[uint64]uint32{76101: 0, 76102: 0}}))
		require.Empty(t, fake.prepares)
		require.Empty(t, fake.created)
	})
}

func TestPvpTeamRejectsOversizedSideBeforeWritingTicket(t *testing.T) {
	s, mr := newPartySvcCtx(t)
	setPlayerLocation(t, mr, 71002, 1)
	response, err := NewJoinQueueLogic(context.Background(), s).JoinQueue(&matchpb.JoinQueueRequest{
		PlayerId: 71002, Mode: matchpb.MatchMode(7), TeamSize: 6,
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrModeNotOpen, response.ErrorCode)
	ticket, err := loadTicket(s, 71002)
	require.NoError(t, err)
	require.Nil(t, ticket)
}
