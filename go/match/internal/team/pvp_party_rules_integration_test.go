package team

// 人数档真人 PVP 的两条组队限制在开战提交事务里的真库回归（docs/design/global-match-hierarchical-pools.md
// 开头第 9、10 条）：拒绝时整笔回滚，名册和开战锁都不动；放行时整队直接进队列，成员票带本模式的评分快照。

import (
	"context"
	"testing"

	matchpb "proto/match"
	teampb "proto/team"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func pvpTeamRequest(teamID uint64, perSide uint32) *teampb.StartTeamMatchRequest {
	return &teampb.StartTeamMatchRequest{ExpectedTeamId: teamID, BattleConfigId: 1, TargetTeamSize: perSide, MatchMode: matchpb.MatchMode_MATCH_MODE_PVP_TEAM, RequestId: uuid.NewString()}
}

func TestTeamTiDBPvpFiveSideRejectsFourStack(t *testing.T) {
	f := newNativeTeamFixture(t)
	f.create(t, 1, 101)
	for _, member := range []uint64{2, 3, 4} {
		f.inviteAndAccept(t, 1, member, 101)
	}
	for _, player := range []uint64{1, 2, 3, 4} {
		f.profile(t, player)
	}

	snap, err := f.store.ReadFree(f.actor(t, 1), 1)
	require.NoError(t, err)
	fiveSide := pvpTeamRequest(101, 5)
	pinned, launch, err := f.store.sql.createLaunch(f.actor(t, 1), snap, 1, fiveSide, uuid.NewString(), MatchRoster(snap.Record), 100)
	require.NoError(t, err)
	require.Nil(t, launch)
	require.Equal(t, ErrPvpPartySize, pinned.Code, "每侧 5 人不收 4 人队")
	rejected, err := f.owner.TeamLaunch(f.actor(t, 1), 1, fiveSide.RequestId)
	require.NoError(t, err)
	require.Nil(t, rejected, "拒绝时不留开战原件")

	snap, err = f.store.ReadFree(f.actor(t, 1), 1)
	require.NoError(t, err)
	require.Empty(t, snap.Record.MatchLockToken, "拒绝时开战锁没动")
	fourSide := pvpTeamRequest(101, 4)
	pinned, launch, err = f.store.sql.createLaunch(f.actor(t, 1), snap, 1, fourSide, uuid.NewString(), MatchRoster(snap.Record), 100)
	require.NoError(t, err)
	require.Zero(t, pinned.Code)
	require.NotNil(t, launch)
	require.Equal(t, uint32(2), launch.State, "同一支 4 人队打 4V4 直接进队列")
}

func TestTeamTiDBPvpRatingGapRejectsBeforeLaunch(t *testing.T) {
	f := newNativeTeamFixture(t)
	f.owner.Config.PartyRatingGapMax = 500
	f.create(t, 1, 101)
	f.inviteAndAccept(t, 1, 2, 101)
	f.profile(t, 1)
	f.profile(t, 2)
	pvp := uint32(matchpb.MatchMode_MATCH_MODE_PVP_TEAM)
	_, err := f.owner.DB.Exec("INSERT INTO match_rating(player_id,mode,rating_centi,games,updated_ms,score_changed_ms) VALUES(?,?,?,?,?,?),(?,?,?,?,?,?)",
		1, pvp, 120_000, 3, 1, 1, 2, pvp, 180_000, 3, 1, 1)
	require.NoError(t, err)

	snap, err := f.store.ReadFree(f.actor(t, 1), 1)
	require.NoError(t, err)
	tooWide := pvpTeamRequest(101, 2)
	pinned, launch, err := f.store.sql.createLaunch(f.actor(t, 1), snap, 1, tooWide, uuid.NewString(), MatchRoster(snap.Record), 100)
	require.NoError(t, err)
	require.Nil(t, launch)
	require.Equal(t, ErrRatingGapTooLarge, pinned.Code, "队员评分差 600 超过上限 500")
	rejected, err := f.owner.TeamLaunch(f.actor(t, 1), 1, tooWide.RequestId)
	require.NoError(t, err)
	require.Nil(t, rejected, "拒绝时整笔回滚，不留开战原件")

	// 分差缩到 500，恰好等于上限：整队直接进队列，成员票带本模式的评分快照。
	_, err = f.owner.DB.Exec("UPDATE match_rating SET rating_centi=? WHERE player_id=? AND mode=?", 170_000, 2, pvp)
	require.NoError(t, err)
	snap, err = f.store.ReadFree(f.actor(t, 1), 1)
	require.NoError(t, err)
	pinned, launch, err = f.store.sql.createLaunch(f.actor(t, 1), snap, 1, pvpTeamRequest(101, 2), uuid.NewString(), MatchRoster(snap.Record), 100)
	require.NoError(t, err)
	require.Zero(t, pinned.Code)
	require.NotNil(t, launch)
	require.Equal(t, uint32(2), launch.State)
	for player, want := range map[uint64]int64{1: 120_000, 2: 170_000} {
		ticket, err := f.owner.Ticket(context.Background(), player)
		require.NoError(t, err)
		require.Equal(t, want, ticket.RatingCenti, "成员 %d 的票带本模式的评分快照", player)
	}
}
