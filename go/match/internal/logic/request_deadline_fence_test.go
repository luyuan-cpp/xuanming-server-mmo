package logic

// 请求截止之后不再提交(docs/design/client-request-ordering.md §6.1 / O11)的回归测试。
//
// gate 对同一会话的路由类请求单飞:路由服在 ForwardTimeoutMs 回超时,gate 就放行下一条。match 的 handler 若在
// 截止之后仍把写发出去,这条写就会晚于下一条请求落地,例如「加入 → 取消」执行成「取消 → 加入」。这里用一个
// 已取消的 ctx 模拟「请求已截止」,断言客户端请求路径上的提交点什么都不写。

import (
	"context"
	"testing"

	"match/internal/constants"
	"match/internal/pkg/ctxkeys"

	battlepb "proto/battle"
	base "proto/common/base"
	matchpb "proto/match"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sessionRequestCtx 模拟 gate 经路由服带来的会话身份。setPlayerSessionOnline 写入的当前会话是
// SessionId = playerId % 1000、GateInstanceId = "gate-inst-3"。
func sessionRequestCtx(playerId uint64, sessionId uint32, gateInstanceId string) context.Context {
	return ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{
		PlayerId: playerId, SessionId: sessionId, GateInstanceId: gateInstanceId,
	})
}

// 跨会话栅栏(client-request-ordering.md §6.3 / O13):旧会话上还在途的 JoinQueue 不得在玩家重连之后建票。
func TestJoinQueueFromSupersededSessionIsRejected(t *testing.T) {
	cases := []struct {
		name           string
		sessionId      uint32
		gateInstanceId string
		online         bool
		wantQueued     bool
	}{
		{name: "当前会话放行", sessionId: 9811 % 1000, gateInstanceId: "gate-inst-3", online: true, wantQueued: true},
		{name: "旧 session_id(同 gate 重连换了号)", sessionId: 9811%1000 + 1, gateInstanceId: "gate-inst-3", online: true},
		{name: "旧 gate 实例(换 gate 重连)", sessionId: 9811 % 1000, gateInstanceId: "gate-inst-old", online: true},
		{name: "玩家会话不存在(已离线)", sessionId: 9811 % 1000, gateInstanceId: "gate-inst-3", online: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svcCtx, mr := newTestSvcCtx(t)
			const playerId = uint64(9811)
			setPlayerLocation(t, mr, playerId, 1)
			if c.online {
				setPlayerSessionOnline(t, mr, playerId)
			}

			resp, err := NewJoinQueueLogic(sessionRequestCtx(playerId, c.sessionId, c.gateInstanceId), svcCtx).
				JoinQueue(&matchpb.JoinQueueRequest{Mode: matchpb.MatchMode_MATCH_MODE_1V1})
			require.NoError(t, err)
			if c.wantQueued {
				require.Zero(t, resp.ErrorCode, "当前会话必须照常入队")
				require.True(t, mr.Exists(matchTicketKey(playerId)))
				return
			}
			require.Equal(t, constants.ErrNotInScene, resp.ErrorCode)
			require.False(t, mr.Exists(matchTicketKey(playerId)), "旧会话 / 离线的请求不得建票")
		})
	}
}

// canceledRequestCtx 是一个已经截止的请求 ctx(go-zero 超时拦截器到期后 handler 拿到的就是这种 ctx)。
func canceledRequestCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestJoinQueueAfterRequestDeadlineWritesNoTicketAndDoesNotEnqueue(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	const playerId = uint64(9801)
	setPlayerLocation(t, mr, playerId, 1)

	resp, err := NewJoinQueueLogic(canceledRequestCtx(), svcCtx).JoinQueue(&matchpb.JoinQueueRequest{
		PlayerId: playerId, Mode: matchpb.MatchMode_MATCH_MODE_1V1,
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrInternal, resp.ErrorCode, "截止之后必须回错误,不能回成功")
	require.Empty(t, resp.QueueTicket)
	require.False(t, mr.Exists(matchTicketKey(playerId)), "截止之后不得建票")
	require.False(t, mr.Exists(queueKeyForJoin(matchpb.MatchMode_MATCH_MODE_1V1, 0, 2)), "截止之后不得入队")
}

func TestJoinQueuePveSoloAfterRequestDeadlineDoesNotStartGather(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	groups := stubGather(t)
	const playerId = uint64(9802)
	setPlayerLocation(t, mr, playerId, 1)

	resp, err := NewJoinQueueLogic(canceledRequestCtx(), svcCtx).JoinQueue(&matchpb.JoinQueueRequest{
		PlayerId: playerId, Mode: matchpb.MatchMode_MATCH_MODE_PVE_SOLO,
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrInternal, resp.ErrorCode)
	require.False(t, mr.Exists(matchTicketKey(playerId)), "即时开战路径截止之后同样不得建票")
	select {
	case g := <-groups:
		t.Fatalf("截止之后不得启动 gather,实际收到 %v", g.members)
	default:
	}
}

func TestCancelQueueAfterRequestDeadlineLeavesTicketAndQueueUntouched(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	const playerId = uint64(9803)
	queueKey := queueKeyForJoin(matchpb.MatchMode_MATCH_MODE_1V1, 0, 2)
	mustCreateTicket(t, svcCtx, playerId, &queueTicket{
		Ticket: "t-9803", Mode: int32(matchpb.MatchMode_MATCH_MODE_1V1), State: ticketStateQueued,
		EnqueuedAtMs: nowMs(), ZoneId: 1, QueueKey: queueKey,
	})
	require.NoError(t, enqueueAtomic(svcCtx, queueKey, playerId, defaultRating))

	_, err := NewCancelQueueLogic(canceledRequestCtx(), svcCtx).CancelQueue(&matchpb.CancelQueueRequest{
		PlayerId: playerId, QueueTicket: "t-9803",
	})
	require.Error(t, err, "截止之后删票失败要如实回错误(客户端按结果未知处理,先查 GetQueueStatus)")
	require.True(t, mr.Exists(matchTicketKey(playerId)), "截止之后不得删票")
	members, err := mr.List(queueKey)
	require.NoError(t, err)
	require.Contains(t, members, "9803", "截止之后不得出队")
}

func TestWatchBattleAfterRequestDeadlineDoesNotBindObserver(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(9804)
	setPlayerLocation(t, mr, watcher, 2)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, 880980, nowMs(), "甲", "乙")

	resp, err := NewWatchBattleLogic(canceledRequestCtx(), svcCtx).WatchBattle(&matchpb.WatchBattleRequest{
		PlayerId: watcher, BattleId: 880980,
	})
	require.NoError(t, err)
	require.NotZero(t, resp.GetErrorMessage().GetId(), "截止之后必须回错误")
	require.Empty(t, fake.adds, "截止之后不得调用 battle 挂观众")
	require.Empty(t, watchingMark(t, mr, watcher), "截止之后不得留下观战标记")
}

func TestWatchBattleRewatchAfterRequestDeadlineDoesNotEvictCurrentWatch(t *testing.T) {
	// 已超时的旧 WatchBattle(B) 不得拆掉 gate 放行之后的新请求刚建立的观战(A):换场清退是副作用,截止之后不再发出。
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(9805)
	setPlayerLocation(t, mr, watcher, 2)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, 880981, nowMs(), "甲", "乙")
	registerBattle(t, svcCtx, 880982, nowMs(), "丙", "丁")
	require.NoError(t, mr.Set(spectateWatchingKey(watcher), "880981"))

	resp, err := NewWatchBattleLogic(canceledRequestCtx(), svcCtx).WatchBattle(&matchpb.WatchBattleRequest{
		PlayerId: watcher, BattleId: 880982,
	})
	require.NoError(t, err)
	require.NotZero(t, resp.GetErrorMessage().GetId())
	require.Empty(t, fake.removes, "截止之后不得对当前观战的场发 RemoveObserver")
	require.Equal(t, "880981", watchingMark(t, mr, watcher), "截止之后不得删当前观战标记")
}

func TestWatchBattleAddObserverOutcomeUnknownRemovesObserver(t *testing.T) {
	// AddObserver 截止 / 取消 / 不可用:观众可能已在 battle 挂上。回失败之前补一次 RemoveObserver,与回滚标记一致。
	cases := []struct {
		name string
		err  error
	}{
		{name: "截止", err: status.Error(codes.DeadlineExceeded, "context deadline exceeded")},
		{name: "取消", err: status.Error(codes.Canceled, "context canceled")},
		{name: "不可用", err: status.Error(codes.Unavailable, "transport is closing")},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, mr := newGatherSvcCtx(t)
			fake := stubObserverRPCs(t)
			watcher := uint64(9810 + i)
			battleId := uint64(880990 + i)
			setPlayerLocation(t, mr, watcher, 2)
			setPlayerSessionOnline(t, mr, watcher)
			registerBattle(t, svcCtx, battleId, nowMs(), "甲", "乙")
			fake.onAdd = func(*battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
				return nil, tc.err
			}

			resp := watchBattle(t, svcCtx, watcher, battleId)
			require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
			require.Len(t, fake.removes, 1, "结果未知必须补发 RemoveObserver")
			require.Equal(t, battleId, fake.removes[0].GetBattleId())
			// 补偿 Remove 必须带原 Add 的 op / 截止 / 实例:battle 据此在截止前封墓碑,挡住仍在途中的同 op Add。
			requireRemoveOf(t, fake.adds[0], fake.removes[0], "watch_outcome_unknown")
			require.Empty(t, watchingMark(t, mr, watcher))
		})
	}
}
