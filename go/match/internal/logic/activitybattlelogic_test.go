package logic

// 活动开局 MatchInternal.StartActivityBattle(docs/design/guild-phase2/06-activities.md §6.18、§6.40)在任何 SQL 之前的判定:
// 参数校验、客户端来源拒绝、request_id 约束、成员预检的前三步(会话 → 战斗锁 → 位置)。
// 这里的 ServiceContext 没有 Authority,放行的请求走到 SQL 就 panic。意图、全员占用与成局见 authority 包(需要 TiDB)。

import (
	"context"
	"testing"

	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/playercontract"
	"match/internal/svc"

	battlepb "proto/battle"
	base "proto/common/base"
	matchpb "proto/match"
	plpb "proto/player_locator"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	testActivityConfigId uint32 = 1
	testActivityGuildId  uint64 = 77_000_001
	testActivityDayKey   uint32 = 20260929
	activityKindTrial           = "guild_trial"
)

func testActivityContext(initiator uint64) *battlepb.BattleActivityContext {
	return &battlepb.BattleActivityContext{
		Kind:              battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL,
		GuildId:           testActivityGuildId,
		ActivityId:        3,
		PeriodKey:         testActivityDayKey,
		InitiatorPlayerId: initiator,
		GuildPeriodKey:    testActivityDayKey,
	}
}

// activityRequest 合法请求:发起人 = 名单首位(名单为空时发起人为 0)。
func activityRequest(members ...uint64) *matchpb.StartActivityBattleRequest {
	var initiator uint64
	if len(members) > 0 {
		initiator = members[0]
	}
	return &matchpb.StartActivityBattleRequest{
		BattleConfigId:  testActivityConfigId,
		MemberPlayerIds: members,
		ActivityContext: testActivityContext(initiator),
	}
}

func startActivityBattle(ctx context.Context, svcCtx *svc.ServiceContext, req *matchpb.StartActivityBattleRequest) (*matchpb.StartActivityBattleResponse, error) {
	return NewStartActivityBattleLogic(ctx, svcCtx).StartActivityBattle(req)
}

func requireActivityRejected(t *testing.T, resp *matchpb.StartActivityBattleResponse, err error,
	reject matchpb.ActivityBattleReject, offender uint64,
) {
	t.Helper()
	require.NoError(t, err, "业务拒绝走 reject,不回 gRPC 错误")
	require.NotNil(t, resp)
	require.Equal(t, reject, resp.GetReject())
	require.Equal(t, offender, resp.GetOffenderPlayerId())
	require.Zero(t, resp.GetBattleId(), "拒绝时不得回 battle_id")
}

// 参数校验:任一不满足 → INVALID_ARGUMENT、offender=0;指标 kind label 收敛到固定集合。带会话的 ctx → gRPC PermissionDenied。
func TestStartActivityBattleValidates(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	all := []uint64{9601, 9602, 9603, 9604, 9605, 9606}

	cases := []struct {
		name  string
		kind  string
		build func() *matchpb.StartActivityBattleRequest
	}{
		{"0 人", activityKindTrial, func() *matchpb.StartActivityBattleRequest { return activityRequest() }},
		{"6 人超上限", activityKindTrial, func() *matchpb.StartActivityBattleRequest { return activityRequest(all...) }},
		{"名单重复", activityKindTrial, func() *matchpb.StartActivityBattleRequest { return activityRequest(9601, 9602, 9601) }},
		{"名单含 0", activityKindTrial, func() *matchpb.StartActivityBattleRequest { return activityRequest(9601, 0) }},
		{"battle_config_id 为 0", activityKindTrial, func() *matchpb.StartActivityBattleRequest {
			req := activityRequest(9601, 9602)
			req.BattleConfigId = 0
			return req
		}},
		{"guild_id 为 0", activityKindTrial, func() *matchpb.StartActivityBattleRequest {
			req := activityRequest(9601, 9602)
			req.ActivityContext.GuildId = 0
			return req
		}},
		{"activity_id 为 0", activityKindTrial, func() *matchpb.StartActivityBattleRequest {
			req := activityRequest(9601, 9602)
			req.ActivityContext.ActivityId = 0
			return req
		}},
		{"period_key 为 0", activityKindTrial, func() *matchpb.StartActivityBattleRequest {
			req := activityRequest(9601, 9602)
			req.ActivityContext.PeriodKey = 0
			return req
		}},
		{"guild_period_key 为 0", activityKindTrial, func() *matchpb.StartActivityBattleRequest {
			req := activityRequest(9601, 9602)
			req.ActivityContext.GuildPeriodKey = 0
			return req
		}},
		{"发起人不在首位", activityKindTrial, func() *matchpb.StartActivityBattleRequest {
			req := activityRequest(9601, 9602)
			req.ActivityContext.InitiatorPlayerId = 9602
			return req
		}},
		{"缺少上下文", "none", func() *matchpb.StartActivityBattleRequest {
			req := activityRequest(9601, 9602)
			req.ActivityContext = nil
			return req
		}},
		{"kind=NONE", "none", func() *matchpb.StartActivityBattleRequest {
			req := activityRequest(9601, 9602)
			req.ActivityContext.Kind = battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_NONE
			return req
		}},
		{"kind 未知", "unknown", func() *matchpb.StartActivityBattleRequest {
			req := activityRequest(9601, 9602)
			req.ActivityContext.Kind = battlepb.EBattleActivityKind(99)
			return req
		}},
		{"缺 request_id", activityKindTrial, func() *matchpb.StartActivityBattleRequest { return activityRequest(9601, 9602) }},
		{"launch_request_id 与 request_id 不符", activityKindTrial, func() *matchpb.StartActivityBattleRequest {
			req := activityRequest(9601, 9602)
			req.RequestId = 51
			req.ActivityContext.LaunchRequestId = 52
			return req
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseline := metrics.ActivityBattleValue(tc.kind, activityResultInvalid)
			resp, err := startActivityBattle(context.Background(), svcCtx, tc.build())
			requireActivityRejected(t, resp, err, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT, 0)
			require.Equal(t, baseline+1, metrics.ActivityBattleValue(tc.kind, activityResultInvalid),
				"kind label 必须收敛为 %q", tc.kind)
		})
	}

	// 带会话 = 客户端来源:gRPC PermissionDenied(拦截器被绕过时的第二道防线)。
	sessionCtx := ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{SessionId: 1, PlayerId: 9601})
	resp, err := startActivityBattle(sessionCtx, svcCtx, activityRequest(9601, 9602))
	require.Nil(t, resp)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

// 成员预检的固定顺序:会话 → 战斗锁 → 位置;票据那一步读 SQL,在 authority 包覆盖。Redis 故障 fail-closed。
func TestActivityPrecheckMember(t *testing.T) {
	const player = uint64(9611)
	setDisconnecting := func(t *testing.T, mr *miniredis.Miniredis) {
		raw, err := proto.Marshal(&plpb.PlayerSession{
			GateId: "3", GateInstanceId: "gate-inst-3", State: plpb.PlayerSessionState_SESSION_STATE_DISCONNECTING,
		})
		require.NoError(t, err)
		require.NoError(t, mr.Set(playercontract.SessionKey(player), string(raw)))
	}
	cases := []struct {
		name   string
		setup  func(t *testing.T, mr *miniredis.Miniredis)
		reject matchpb.ActivityBattleReject
	}{
		{"无会话", func(_ *testing.T, mr *miniredis.Miniredis) { mr.Del(playercontract.SessionKey(player)) },
			matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_OFFLINE},
		{"断线等重连", setDisconnecting, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_OFFLINE},
		{"持有战斗锁", func(t *testing.T, mr *miniredis.Miniredis) {
			require.NoError(t, mr.Set(playercontract.BattleLockKey(player), "1"))
		}, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE},
		{"没有场景位置", func(_ *testing.T, mr *miniredis.Miniredis) { mr.Del(playercontract.LocationKey(player)) },
			matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY},
		{"会话读不出来", func(t *testing.T, mr *miniredis.Miniredis) {
			require.NoError(t, mr.Set(playercontract.SessionKey(player), "\x0a\x05ab"))
		}, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svcCtx, mr := newTestSvcCtx(t)
			setPlayerSessionOnline(t, mr, player)
			setPlayerLocation(t, mr, player, 1)
			c.setup(t, mr)

			got := NewStartActivityBattleLogic(context.Background(), svcCtx).precheckMember(player)

			require.Equal(t, c.reject, got)
		})
	}
}

func TestActivityKindLabelStaysBounded(t *testing.T) {
	require.Equal(t, activityKindTrial, activityKindLabel(battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL))
	require.Equal(t, "none", activityKindLabel(battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_NONE))
	require.Equal(t, "unknown", activityKindLabel(battlepb.EBattleActivityKind(99)))
}
