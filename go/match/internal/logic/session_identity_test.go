package logic

// 会话身份不回落到请求体(docs/design/client-request-ordering.md §7.2 第 9 条)。
//
// 带了会话头的请求只认会话头:会话还没绑角色(Login 之后、EnterGame 的 BindSession 之前,gate 把会话头 player_id
// 归一成 0)时,请求体里填别人的 player_id 不得生效。这里的 ServiceContext 是空的(没有 Authority、Redis、Catalog):
// 被拒的请求在入口就返回;修复被回退、回落到请求体时会走进读写匹配状态的路径、对空依赖解引用而 panic,
// callRejected 把它转成带说明的断言失败,不让整个测试进程崩掉。

import (
	"context"
	"testing"

	"match/internal/constants"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	base "proto/common/base"
	matchpb "proto/match"

	"github.com/stretchr/testify/require"
)

const (
	victimPlayerID = uint64(31001) // 请求体里冒填的别人
	ownPlayerID    = uint64(31002) // 会话自己绑定的角色
)

// 各方法「缺少玩家身份」分支回的 tip 参数(tipErr 的第二个实参)。
var missingIdentityParameters = []string{"缺少玩家身份"}

// gate 发出的未绑角色会话头:有会话号、实例号与会话代次,player_id 为 0。
func unboundSessionCtx() context.Context {
	return ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{
		SessionId: 7, GateInstanceId: "gate-a", SessionIncarnation: 3,
	})
}

func boundSessionCtx(playerID uint64) context.Context {
	return ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{
		SessionId: 7, GateInstanceId: "gate-a", SessionIncarnation: 3, PlayerId: playerID, OwnerRevision: 5,
	})
}

func callRejected(t *testing.T, call func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("未绑角色的会话回落到了请求体里的 player_id,走进了读写匹配状态的路径: %v", r)
		}
	}()
	call()
}

func TestAuthoritativePlayerIDNeverFallsBackToRequestBodyWhenSessionPresent(t *testing.T) {
	require.Zero(t, authoritativePlayerID(unboundSessionCtx(), victimPlayerID), "未绑角色的会话不得采用请求体")
	require.Equal(t, ownPlayerID, authoritativePlayerID(boundSessionCtx(ownPlayerID), victimPlayerID), "会话与请求体不一致时以会话为准")
	require.Equal(t, ownPlayerID, authoritativePlayerID(boundSessionCtx(ownPlayerID), 0))
	// 不带会话头 = 内部调用,请求体照旧生效(MatchService 今天没有这类调用方,originalRoleInterceptor 会先拒)。
	require.Equal(t, victimPlayerID, authoritativePlayerID(context.Background(), victimPlayerID))
}

func TestJoinQueueFromUnboundSessionIsRejected(t *testing.T) {
	var resp *matchpb.JoinQueueResponse
	var err error
	callRejected(t, func() {
		resp, err = NewJoinQueueLogic(unboundSessionCtx(), &svc.ServiceContext{}).JoinQueue(
			&matchpb.JoinQueueRequest{PlayerId: victimPlayerID, Mode: matchpb.MatchMode_MATCH_MODE_1V1})
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrInternal, resp.GetErrorCode())
	require.Equal(t, constants.ErrInternal, resp.GetErrorMessage().GetId())
	require.Equal(t, missingIdentityParameters, resp.GetErrorMessage().GetParameters())
}

// 撤销走既有的「无身份」分支:回空成功,不碰任何人的票据(碰了就会对空 Authority 解引用)。
func TestCancelQueueFromUnboundSessionTouchesNoTicket(t *testing.T) {
	var resp *base.Empty
	var err error
	callRejected(t, func() {
		resp, err = NewCancelQueueLogic(unboundSessionCtx(), &svc.ServiceContext{}).CancelQueue(
			&matchpb.CancelQueueRequest{PlayerId: victimPlayerID, QueueTicket: "ticket-of-victim"})
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestGetQueueStatusFromUnboundSessionReportsNotQueued(t *testing.T) {
	var resp *matchpb.GetQueueStatusResponse
	var err error
	callRejected(t, func() {
		resp, err = NewGetQueueStatusLogic(unboundSessionCtx(), &svc.ServiceContext{}).GetQueueStatus(
			&matchpb.GetQueueStatusRequest{PlayerId: victimPlayerID})
	})
	require.NoError(t, err)
	require.Equal(t, matchpb.QueueState_QUEUE_STATE_NOT_QUEUED, resp.GetState())
	require.Empty(t, resp.GetQueueTicket(), "不得回显别人的票据号")
}

func TestChallengePlayerFromUnboundSessionIsRejected(t *testing.T) {
	var resp *matchpb.ChallengePlayerResponse
	var err error
	callRejected(t, func() {
		resp, err = NewChallengeLogic(unboundSessionCtx(), &svc.ServiceContext{}).ChallengePlayer(
			&matchpb.ChallengePlayerRequest{PlayerId: victimPlayerID, TargetPlayerId: ownPlayerID})
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrInternal, resp.GetErrorMessage().GetId())
	require.Equal(t, missingIdentityParameters, resp.GetErrorMessage().GetParameters())
}

func TestRespondChallengeFromUnboundSessionIsRejected(t *testing.T) {
	var resp *matchpb.RespondChallengeResponse
	var err error
	callRejected(t, func() {
		resp, err = NewChallengeLogic(unboundSessionCtx(), &svc.ServiceContext{}).RespondChallenge(
			&matchpb.RespondChallengeRequest{PlayerId: victimPlayerID, ChallengeId: 1, Accept: true})
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrInternal, resp.GetErrorMessage().GetId())
	require.Equal(t, missingIdentityParameters, resp.GetErrorMessage().GetParameters())
}

func TestWatchBattleFromUnboundSessionIsRejected(t *testing.T) {
	var resp *matchpb.WatchBattleResponse
	var err error
	callRejected(t, func() {
		resp, err = NewWatchBattleLogic(unboundSessionCtx(), &svc.ServiceContext{}).WatchBattle(
			&matchpb.WatchBattleRequest{PlayerId: victimPlayerID, BattleId: 1})
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrInternal, resp.GetErrorMessage().GetId())
	require.Equal(t, missingIdentityParameters, resp.GetErrorMessage().GetParameters())
}
