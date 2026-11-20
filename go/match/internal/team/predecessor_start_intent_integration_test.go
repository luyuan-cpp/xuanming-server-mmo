package team

import (
	"context"
	"testing"

	"match/internal/authority"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	base "proto/common/base"
	dspb "proto/data_service"
	matchpb "proto/match"
	teampb "proto/team"
	"shared/generated/pb/table"
	"shared/rolefence/fencetest"
	"shared/roleidentity"
)

// 原生 SQL 回归:角色过户后,同一台设备上的新主人带着前主人留下的开战请求号来恢复或重发。
// 过户前护栏要求整队原件已终结、角色已离队(RoleFencePending),所以前主人的原件这时已关闭。
// 新主人得到确定结论 TeamMatchIntentForeign:GetMyTeam 不报故障,StartTeamMatch 按业务拒绝,前主人的原件原样不动;
// 前主人的旧会话照旧按原会话失效拒绝。
func TestTeamTiDBPredecessorStartIntentIsDefinitiveForNewOwner(t *testing.T) {
	f := newNativeTeamFixture(t)
	f.create(t, 1, 101)
	f.inviteAndAccept(t, 1, 2, 101)
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

	// 过户前的前提:前主人撤掉排队、解散队伍。
	closed, err := f.owner.WithdrawObservedTeamQueue(context.Background(), launch)
	require.NoError(t, err)
	require.True(t, closed)
	disband := &teampb.DisbandTeamRequest{ExpectedTeamId: 101, RequestId: uuid.NewString()}
	result, err := f.store.Mutate(fixtureTeamAction(t, f.actor(t, 1), 1, OpDisband, disband), BindCaller(1, 101), DisbandOp(1))
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)

	fencetest.Update(t, context.Background(), f.owner.DB, 1, "buyer", 8, 0)
	buyer, err := roleidentity.Capture(context.Background(), &dspb.AccountRoleOwnership{Account: "buyer", OwnerRevision: 8, Role: &base.AccountSimplePlayer{PlayerId: 1}})
	require.NoError(t, err)

	intent, err := f.owner.TeamMatchIntent(buyer, 1, request.RequestId)
	require.NoError(t, err)
	require.Equal(t, authority.TeamMatchIntentForeign, intent.GetState())
	require.Zero(t, intent.GetBattleId())

	h := newServiceHarness(t)
	h.addPlayers(1, 2)
	h.svcCtx.Authority, h.service.store = f.owner, f.store
	view := h.service.GetMyTeam(buyer, 1, &teampb.GetMyTeamRequest{MatchRequestId: request.RequestId})
	require.Zero(t, view.GetErrorMessage().GetId(), "前主人的原件是确定结论,不是故障")
	require.NotNil(t, view.GetTeam(), "回包照常带调用者自己的视图")
	require.Zero(t, view.GetTeam().GetTeamId())
	require.Equal(t, authority.TeamMatchIntentForeign, view.GetMatchIntent().GetState())
	require.Equal(t, request.RequestId, view.GetMatchIntent().GetRequestId())

	resent := h.service.StartTeamMatch(buyer, 1, request)
	require.Equal(t, uint32(table.CommonError_kInvalidParameter), resent.GetErrorMessage().GetId())
	require.Equal(t, authority.TeamMatchIntentForeign, resent.GetMatchIntent().GetState())
	require.Equal(t, request.RequestId, resent.GetMatchIntent().GetRequestId())

	var state uint32
	var account string
	var revision uint64
	require.NoError(t, f.owner.DB.QueryRow("SELECT state,owner_account,owner_revision FROM team_launch WHERE actor_id=1 AND request_high=? AND request_low=?", launch.RequestHigh, launch.RequestLow).Scan(&state, &account, &revision))
	require.Equal(t, uint32(5), state)
	require.Equal(t, "team-original-1", account, "新主人的查询与重发不改写前主人的原件")
	require.Equal(t, uint64(7), revision)
	var launches int
	require.NoError(t, f.owner.DB.QueryRow("SELECT COUNT(*) FROM team_launch WHERE actor_id=1").Scan(&launches))
	require.Equal(t, 1, launches, "重发前主人的请求号不能为新主人建出第二份原件")

	_, err = f.owner.TeamMatchIntent(f.actor(t, 1), 1, request.RequestId)
	require.ErrorIs(t, err, authority.ErrOriginalPlayer)
	require.NotErrorIs(t, err, authority.ErrForeignOriginal, "旧会话本身已失效,不是过户前的原件")
}
