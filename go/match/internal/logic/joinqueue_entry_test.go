package logic

// JoinQueue 在任何 SQL 之前的入口判定:身份、模式白名单、人数档、战斗锁、位置、会话复核。
// 这里的 ServiceContext 没有 Authority,放行的请求走到 SQL 就 panic,所以每条用例都必须在入口被拒。
// 入队提交见 authority 包(需要 TiDB)。

import (
	"context"
	"testing"

	"match/internal/constants"
	"match/internal/pkg/ctxkeys"
	"match/internal/playercontract"

	base "proto/common/base"
	matchpb "proto/match"

	"github.com/stretchr/testify/require"
)

func TestJoinQueueRejectsAtEntry(t *testing.T) {
	const player = uint64(24001)
	pveTeam, pvpTeam := matchpb.MatchMode_MATCH_MODE_PVE_TEAM, matchpb.MatchMode_MATCH_MODE_PVP_TEAM
	cases := []struct {
		name string
		ctx  context.Context
		req  *matchpb.JoinQueueRequest
		code uint32
	}{
		{name: "缺身份", req: &matchpb.JoinQueueRequest{Mode: matchpb.MatchMode_MATCH_MODE_1V1}, code: constants.ErrInternal},
		{name: "PVE 组队人数超过副本上限", req: &matchpb.JoinQueueRequest{PlayerId: player, Mode: pveTeam, BattleConfigId: 1, TeamSize: 4}, code: constants.ErrModeNotOpen},
		{name: "副本未开放组队", req: &matchpb.JoinQueueRequest{PlayerId: player, Mode: pveTeam, BattleConfigId: 99, TeamSize: 2}, code: constants.ErrTeamSizeNotConfigured},
		{name: "PVP 每方超过 5 人", req: &matchpb.JoinQueueRequest{PlayerId: player, Mode: pvpTeam, TeamSize: 6}, code: constants.ErrModeNotOpen},
		{name: "PVP 模板未开放", req: &matchpb.JoinQueueRequest{PlayerId: player, Mode: pvpTeam, BattleConfigId: 99}, code: constants.ErrModeNotOpen},
		{name: "3V3 未开放", req: &matchpb.JoinQueueRequest{PlayerId: player, Mode: matchpb.MatchMode_MATCH_MODE_3V3}, code: constants.ErrModeNotOpen},
		{name: "切磋不走排队入口", req: &matchpb.JoinQueueRequest{PlayerId: player, Mode: matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE}, code: constants.ErrModeNotOpen},
		{name: "1V1 模板未开放", req: &matchpb.JoinQueueRequest{PlayerId: player, Mode: matchpb.MatchMode_MATCH_MODE_1V1, BattleConfigId: 99}, code: constants.ErrModeNotOpen},
		{name: "不在场景", req: &matchpb.JoinQueueRequest{PlayerId: player + 1, Mode: matchpb.MatchMode_MATCH_MODE_1V1}, code: constants.ErrNotInScene},
		{name: "战斗中", req: &matchpb.JoinQueueRequest{PlayerId: player + 2, Mode: matchpb.MatchMode_MATCH_MODE_1V1}, code: constants.ErrInBattle},
		{name: "旧会话的请求", ctx: ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{PlayerId: player, SessionId: 999, GateInstanceId: "old-gate"}),
			req: &matchpb.JoinQueueRequest{Mode: matchpb.MatchMode_MATCH_MODE_1V1}, code: constants.ErrNotInScene},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svcCtx, mr := newTestSvcCtx(t)
			useTestCatalog(t, svcCtx, map[uint32]uint32{1: 3})
			setPlayerLocation(t, mr, player, 1)
			setPlayerSessionOnline(t, mr, player)
			setPlayerLocation(t, mr, player+2, 1)
			require.NoError(t, mr.Set(playercontract.BattleLockKey(player+2), "777"))
			ctx := c.ctx
			if ctx == nil {
				ctx = context.Background()
			}

			resp, err := NewJoinQueueLogic(ctx, svcCtx).JoinQueue(c.req)

			require.NoError(t, err)
			require.Equal(t, c.code, resp.GetErrorCode())
			require.Equal(t, c.code, resp.GetErrorMessage().GetId())
		})
	}
}

// Redis 故障时 fail-closed:读不到战斗锁就拒绝排队,不能放进双战斗。
func TestJoinQueueBattleLockReadFailureFailsClosed(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	useTestCatalog(t, svcCtx, nil)
	mr.SetError("ERR injected failure")

	resp, err := NewJoinQueueLogic(context.Background(), svcCtx).JoinQueue(&matchpb.JoinQueueRequest{PlayerId: 24010, Mode: matchpb.MatchMode_MATCH_MODE_1V1})

	require.NoError(t, err)
	require.Equal(t, constants.ErrInternal, resp.GetErrorCode())
}
