package logic

// 活动开局 MatchInternal.StartActivityBattle 与 RunActivityGather(docs/design/guild-phase2/06-activities.md
// §6.18、§6.40)的 miniredis 测试。
//
// 确定性:StartActivityBattle 的 gather 经 runActivityGatherFn 换成记录器、spawnActivityGatherFn 换成同步执行,
// 不等 goroutine、不读墙钟做断言;票据 TTL 用 miniredis 的 TTL 精确比较;建票时序经 createActivityTicketFn 注入。
// RunActivityGather 真跑 gather,scene / battle 的 gRPC 用 stubGatherRPCs 顶替。测试之间不并行(包级测试缝)。

import (
	"context"
	"strconv"
	"testing"
	"time"

	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
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

// activityGatherCall 是记录器收到的一次活动 gather。
type activityGatherCall struct {
	configId uint32
	members  []uint64
	tickets  map[uint64]string
	battleID uint64
	actx     *battlepb.BattleActivityContext
}

// activityGatherRecorder 顶替 RunActivityGather;result 是它返回给调用方的开局结果。
type activityGatherRecorder struct {
	calls  []activityGatherCall
	result bool
}

// stubActivityGather 把活动 gather 换成记录器,并让 spawnActivityGatherFn 同步执行。
func stubActivityGather(t *testing.T) *activityGatherRecorder {
	t.Helper()
	rec := &activityGatherRecorder{result: true}
	prevRun, prevSpawn := runActivityGatherFn, spawnActivityGatherFn
	runActivityGatherFn = func(_ *svc.ServiceContext, configId uint32, members []uint64, tickets map[uint64]string,
		battleID uint64, actx *battlepb.BattleActivityContext,
	) bool {
		rec.calls = append(rec.calls, activityGatherCall{
			configId: configId, members: members, tickets: tickets, battleID: battleID, actx: actx,
		})
		return rec.result
	}
	spawnActivityGatherFn = func(_ string, fn func()) { fn() }
	t.Cleanup(func() { runActivityGatherFn, spawnActivityGatherFn = prevRun, prevSpawn })
	return rec
}

// stubActivityTicketCreate 替换建票入口;hook 拿到原实现,可在调用前后插入并发 / 故障时序。
func stubActivityTicketCreate(t *testing.T,
	hook func(prev func(context.Context, *svc.ServiceContext, uint64, *queueTicket, int) (bool, error),
		ctx context.Context, sc *svc.ServiceContext, pid uint64, ticket *queueTicket, ttl int) (bool, error),
) {
	t.Helper()
	prev := createActivityTicketFn
	createActivityTicketFn = func(ctx context.Context, sc *svc.ServiceContext, pid uint64, ticket *queueTicket, ttl int) (bool, error) {
		return hook(prev, ctx, sc, pid, ticket, ttl)
	}
	t.Cleanup(func() { createActivityTicketFn = prev })
}

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

// activityZoneFor 测试里成员所在 zone:下标偶数在 zone1、奇数在 zone2(newGatherSvcCtx 两个 zone 都有 scene)。
func activityZoneFor(index int) uint32 {
	return uint32(1 + index%2)
}

// readyActivityMembers 让成员在线、已进场(无战斗锁、无票据)。
func readyActivityMembers(t *testing.T, mr *miniredis.Miniredis, members []uint64) {
	t.Helper()
	for i, pid := range members {
		setPlayerSessionOnline(t, mr, pid)
		setPlayerLocation(t, mr, pid, activityZoneFor(i))
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

func requireNoTickets(t *testing.T, mr *miniredis.Miniredis, members ...uint64) {
	t.Helper()
	for _, pid := range members {
		require.False(t, mr.Exists(matchTicketKey(pid)), "不得留下票据 player=%d", pid)
	}
}

// 参数校验:任一不满足 → INVALID_ARGUMENT、offender=0、不写票、不起 gather;指标 kind label 收敛到固定集合。
// 带会话的 ctx → gRPC PermissionDenied。
func TestStartActivityBattleValidates(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	rec := stubActivityGather(t)
	all := []uint64{9601, 9602, 9603, 9604, 9605, 9606}
	readyActivityMembers(t, mr, all)

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
	requireNoTickets(t, mr, all...)
	require.Empty(t, rec.calls, "参数非法不得起 gather")

	// 带会话 = 客户端来源:gRPC PermissionDenied(拦截器被绕过时的第二道防线)。
	sessionCtx := ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{SessionId: 1, PlayerId: 9601})
	resp, err := startActivityBattle(sessionCtx, svcCtx, activityRequest(9601, 9602))
	require.Nil(t, resp)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	requireNoTickets(t, mr, all...)
	require.Empty(t, rec.calls)
}

// 第 2 人不在线(无会话 / 断线等重连)→ MEMBER_OFFLINE,offender=第 2 人,无票。
func TestStartActivityBattleOfflineMember(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setSession func(t *testing.T, mr *miniredis.Miniredis, pid uint64)
	}{
		{"无会话", func(*testing.T, *miniredis.Miniredis, uint64) {}},
		{"断线等重连", func(t *testing.T, mr *miniredis.Miniredis, pid uint64) {
			raw, err := proto.Marshal(&plpb.PlayerSession{
				GateId: "3", GateInstanceId: "gate-inst-3", State: plpb.PlayerSessionState_SESSION_STATE_DISCONNECTING,
			})
			require.NoError(t, err)
			require.NoError(t, mr.Set(playerSessionKey(pid), string(raw)))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, mr := newGatherSvcCtx(t)
			rec := stubActivityGather(t)
			members := []uint64{9611, 9612, 9613}
			readyActivityMembers(t, mr, members)
			mr.Del(playerSessionKey(9612))
			tc.setSession(t, mr, 9612)

			resp, err := startActivityBattle(context.Background(), svcCtx, activityRequest(members...))

			requireActivityRejected(t, resp, err, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_OFFLINE, 9612)
			requireNoTickets(t, mr, members...)
			require.Empty(t, rec.calls)
		})
	}
}

// 第 2 人持有 battle:lock → MEMBER_IN_BATTLE。
func TestStartActivityBattleLockedMember(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	rec := stubActivityGather(t)
	members := []uint64{9621, 9622, 9623}
	readyActivityMembers(t, mr, members)
	require.NoError(t, mr.Set(battleLockKey(9622), "1"))
	baseline := metrics.ActivityBattleValue(activityKindTrial, activityResultInBattle)

	resp, err := startActivityBattle(context.Background(), svcCtx, activityRequest(members...))

	requireActivityRejected(t, resp, err, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE, 9622)
	requireNoTickets(t, mr, members...)
	require.Empty(t, rec.calls)
	require.Equal(t, baseline+1, metrics.ActivityBattleValue(activityKindTrial, activityResultInBattle))
}

// 第 3 人没有场景位置 → MEMBER_NOT_READY。
func TestStartActivityBattleMemberNotInScene(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	rec := stubActivityGather(t)
	members := []uint64{9631, 9632, 9633}
	readyActivityMembers(t, mr, members)
	mr.Del(getPlayerLocationKey(9633))

	resp, err := startActivityBattle(context.Background(), svcCtx, activityRequest(members...))

	requireActivityRejected(t, resp, err, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY, 9633)
	requireNoTickets(t, mr, members...)
	require.Empty(t, rec.calls)
}

// 第 3 人持 queued 票且真在队列里 → MEMBER_NOT_READY;第 1、2 人没有留下票据,第 3 人的票原样保留。
func TestStartActivityBattleExistingTicket(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	rec := stubActivityGather(t)
	members := []uint64{9641, 9642, 9643}
	readyActivityMembers(t, mr, members)
	queued := joinQueue1v1(t, svcCtx, 9643)
	require.Zero(t, queued.ErrorCode)

	resp, err := startActivityBattle(context.Background(), svcCtx, activityRequest(members...))

	requireActivityRejected(t, resp, err, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY, 9643)
	requireNoTickets(t, mr, 9641, 9642)
	ticket, err := loadTicket(svcCtx, 9643)
	require.NoError(t, err)
	require.NotNil(t, ticket)
	require.Equal(t, queued.QueueTicket, ticket.Ticket, "别人的在途票不能被动")
	require.Equal(t, ticketStateQueued, ticket.State)
	require.Empty(t, rec.calls)
}

// 在途票按 JoinQueue 同一套规则自愈:queued 孤儿(不在队列)与已结束战斗的 ready 残留都被清掉,照常开局。
func TestStartActivityBattleHealsStaleTickets(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	rec := stubActivityGather(t)
	members := []uint64{9651, 9652}
	readyActivityMembers(t, mr, members)
	mustCreateTicket(t, svcCtx, 9651, &queueTicket{
		Ticket: "orphan-9651", Mode: int32(matchpb.MatchMode_MATCH_MODE_1V1), State: ticketStateQueued,
		QueueKey: matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0), EnqueuedAtMs: nowMs(),
	})
	mustCreateTicket(t, svcCtx, 9652, &queueTicket{
		Ticket: "ready-9652", Mode: int32(matchpb.MatchMode_MATCH_MODE_PVE_SOLO), State: ticketStateReady, EnqueuedAtMs: nowMs(),
	})

	resp, err := startActivityBattle(context.Background(), svcCtx, activityRequest(members...))

	require.NoError(t, err)
	require.Equal(t, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_NONE, resp.GetReject())
	require.NotZero(t, resp.GetBattleId())
	require.Len(t, rec.calls, 1)
	for _, pid := range members {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket)
		require.Equal(t, rec.calls[0].tickets[pid], ticket.Ticket, "残留票已被本次活动票替换 player=%d", pid)
		require.Equal(t, ticketStateMatched, ticket.State)
	}
}

// 预检之后第 3 人被别人抢先写票(建票返回 false)→ MEMBER_NOT_READY + 第 3 人;
// 第 1、2 人本次刚建的票被 CAS 删除,第 3 人别人的票不动,第 4 人不再建票。
func TestStartActivityBattleTicketRaceRollsBack(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	rec := stubActivityGather(t)
	members := []uint64{9661, 9662, 9663, 9664}
	readyActivityMembers(t, mr, members)
	stubActivityTicketCreate(t, func(prev func(context.Context, *svc.ServiceContext, uint64, *queueTicket, int) (bool, error),
		ctx context.Context, sc *svc.ServiceContext, pid uint64, ticket *queueTicket, ttl int,
	) (bool, error) {
		if pid == 9663 {
			mustCreateTicket(t, sc, pid, &queueTicket{
				Ticket: "foreign-9663", Mode: int32(matchpb.MatchMode_MATCH_MODE_PVE_SOLO), State: ticketStateMatched, EnqueuedAtMs: nowMs(),
			})
		}
		return prev(ctx, sc, pid, ticket, ttl)
	})

	resp, err := startActivityBattle(context.Background(), svcCtx, activityRequest(members...))

	requireActivityRejected(t, resp, err, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY, 9663)
	requireNoTickets(t, mr, 9661, 9662, 9664)
	require.Equal(t, "foreign-9663", mr.HGet(matchTicketKey(9663), ticketFieldTicket), "别人的票不在回滚集合")
	require.Empty(t, rec.calls)
}

// 第 2 人建票返回 err 但服务端已写入,且请求 ctx 同时到期 → INTERNAL、offender=0;
// 回滚集合含他,独立 ctx 仍删成功。
func TestStartActivityBattleTicketUnknownResultRollsBack(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	rec := stubActivityGather(t)
	members := []uint64{9671, 9672, 9673}
	readyActivityMembers(t, mr, members)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stubActivityTicketCreate(t, func(prev func(context.Context, *svc.ServiceContext, uint64, *queueTicket, int) (bool, error),
		c context.Context, sc *svc.ServiceContext, pid uint64, ticket *queueTicket, ttl int,
	) (bool, error) {
		if pid != 9672 {
			return prev(c, sc, pid, ticket, ttl)
		}
		created, err := prev(context.Background(), sc, pid, ticket, ttl)
		require.NoError(t, err)
		require.True(t, created)
		cancel()
		return false, context.DeadlineExceeded
	})

	resp, err := startActivityBattle(ctx, svcCtx, activityRequest(members...))

	requireActivityRejected(t, resp, err, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0)
	require.Error(t, ctx.Err(), "回滚时请求 ctx 已取消")
	requireNoTickets(t, mr, members...)
	require.Empty(t, rec.calls)
}

// 全员建票成功后发现调用方已放弃(ctx 结束)→ 不开局:回滚全部票据,回 INTERNAL。
func TestStartActivityBattleCallerGoneBeforeLaunch(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	rec := stubActivityGather(t)
	members := []uint64{9681, 9682}
	readyActivityMembers(t, mr, members)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stubActivityTicketCreate(t, func(prev func(context.Context, *svc.ServiceContext, uint64, *queueTicket, int) (bool, error),
		c context.Context, sc *svc.ServiceContext, pid uint64, ticket *queueTicket, ttl int,
	) (bool, error) {
		created, err := prev(c, sc, pid, ticket, ttl)
		if pid == 9682 {
			cancel() // 最后一张票写成之后,guild 的预算到期
		}
		return created, err
	})
	baseline := metrics.ActivityBattleValue(activityKindTrial, activityResultInternal)

	resp, err := startActivityBattle(ctx, svcCtx, activityRequest(members...))

	requireActivityRejected(t, resp, err, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0)
	requireNoTickets(t, mr, members...)
	require.Empty(t, rec.calls, "调用方已放弃就不得开局")
	require.Equal(t, baseline+1, metrics.ActivityBattleValue(activityKindTrial, activityResultInternal))
}

// 发号失败(失租 fence)→ INTERNAL,不得用 0 顶替,不写票、不起 gather。
func TestStartActivityBattleIDGenFailure(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	rec := stubActivityGather(t)
	members := []uint64{9691, 9692}
	readyActivityMembers(t, mr, members)
	svcCtx.BattleIDGen.Fence()

	resp, err := startActivityBattle(context.Background(), svcCtx, activityRequest(members...))

	requireActivityRejected(t, resp, err, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0)
	requireNoTickets(t, mr, members...)
	require.Empty(t, rec.calls)
}

// 成功:battle_id≠0;全员有 matched 票(PVE_TEAM、不入队、无 team_id、TTL=matchedTicketTTLFor(3));
// gather 收到同一 battle_id、原序名单、本次 ticket id 与相等的上下文副本;改请求不影响副本。
func TestStartActivityBattleHappy(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	rec := stubActivityGather(t)
	members := []uint64{9703, 9701, 9702}
	readyActivityMembers(t, mr, members)
	req := activityRequest(members...)
	want := proto.Clone(req.GetActivityContext()).(*battlepb.BattleActivityContext)
	startedBase := metrics.ActivityBattleValue(activityKindTrial, activityResultStarted)
	okBase := metrics.ActivityBattleValue(activityKindTrial, activityResultGatherOK)

	resp, err := startActivityBattle(context.Background(), svcCtx, req)

	require.NoError(t, err)
	require.Equal(t, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_NONE, resp.GetReject())
	require.Zero(t, resp.GetOffenderPlayerId())
	require.NotZero(t, resp.GetBattleId())

	require.Len(t, rec.calls, 1)
	call := rec.calls[0]
	require.Equal(t, testActivityConfigId, call.configId)
	require.Equal(t, members, call.members, "站位顺序 = 名单顺序,发起人在首位")
	require.Equal(t, resp.GetBattleId(), call.battleID, "gather 用的就是回给 guild 的 battle_id")
	require.True(t, proto.Equal(want, call.actx), "上下文原样透传")
	require.NotSame(t, req.GetActivityContext(), call.actx, "gather 拿到的是深拷贝")

	ttl := time.Duration(matchedTicketTTLFor(svcCtx, uint32(len(members)))) * time.Second
	for i, pid := range members {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket)
		require.Equal(t, call.tickets[pid], ticket.Ticket, "gather 拿到的 ticket id 就是落盘的那张")
		require.Equal(t, ticketStateMatched, ticket.State)
		require.Equal(t, int32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), ticket.Mode)
		require.Equal(t, testActivityConfigId, ticket.Config)
		require.Equal(t, activityZoneFor(i), ticket.ZoneId)
		require.Empty(t, ticket.QueueKey, "活动开局不入队")
		fields, err := mr.HKeys(matchTicketKey(pid))
		require.NoError(t, err)
		require.NotContains(t, fields, ticketFieldTeamId, "活动票不设 team_id")
		require.Equal(t, ttl, mr.TTL(matchTicketKey(pid)), "TTL = matchedTicketTTLFor(n)")
	}
	require.False(t, mr.Exists(matchQueueIndexKey), "活动开局不登记任何队列")

	// 调用方(gRPC 框架)之后改请求对象,不影响 gather 手里的副本。
	req.ActivityContext.GuildId = 0
	req.MemberPlayerIds[0] = 1
	require.Equal(t, testActivityGuildId, call.actx.GetGuildId())
	require.Equal(t, uint64(9703), call.members[0])

	require.Equal(t, startedBase+1, metrics.ActivityBattleValue(activityKindTrial, activityResultStarted))
	require.Equal(t, okBase+1, metrics.ActivityBattleValue(activityKindTrial, activityResultGatherOK))
}

// gather 之后失败:同步出口仍是 started(battle_id 已回给 guild),异步终态记 gather_failed。
func TestStartActivityBattleGatherFailureMetric(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	rec := stubActivityGather(t)
	rec.result = false
	members := []uint64{9711, 9712}
	readyActivityMembers(t, mr, members)
	failedBase := metrics.ActivityBattleValue(activityKindTrial, activityResultGatherFailed)

	resp, err := startActivityBattle(context.Background(), svcCtx, activityRequest(members...))

	require.NoError(t, err)
	require.NotZero(t, resp.GetBattleId())
	require.Len(t, rec.calls, 1)
	require.Equal(t, failedBase+1, metrics.ActivityBattleValue(activityKindTrial, activityResultGatherFailed))
}

// mustCreateActivityTickets 按 StartActivityBattle 同口径为成员建 matched 票(真跑 gather 的用例铺数据)。
func mustCreateActivityTickets(t *testing.T, svcCtx *svc.ServiceContext, members []uint64) map[uint64]string {
	t.Helper()
	tickets := make(map[uint64]string, len(members))
	ttl := matchedTicketTTLFor(svcCtx, uint32(len(members)))
	for _, pid := range members {
		tickets[pid] = "act-" + strconv.FormatUint(pid, 10)
		created, err := createTicketIfAbsent(svcCtx, pid, &queueTicket{
			Ticket: tickets[pid], Mode: int32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), Config: testActivityConfigId,
			State: ticketStateMatched, EnqueuedAtMs: nowMs(), ZoneId: 1,
		}, ttl)
		require.NoError(t, err)
		require.True(t, created)
	}
	return tickets
}

// RunActivityGather 真跑 gather:PrepareBattle / CreateBattle 用预设 battle_id,CreateBattleRequest 带相等的上下文,
// 模式 PVE_TEAM、名单原序;发号器已 fence 仍成功 = gather 没有调用 BattleIDGen。成功后票据 ready 且 battle_id 为预设值。
func TestRunActivityGatherPassesPresetIDAndContext(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, nil)
	svcCtx.BattleIDGen.Fence()
	members := []uint64{9722, 9721}
	for _, pid := range members {
		setPlayerLocation(t, mr, pid, 1)
	}
	tickets := mustCreateActivityTickets(t, svcCtx, members)
	actx := testActivityContext(9722)
	const presetID uint64 = 880_000_000_001

	require.True(t, RunActivityGather(svcCtx, testActivityConfigId, members, tickets, presetID, actx))

	require.Len(t, fake.prepares, len(members))
	for i, prep := range fake.prepares {
		require.Equal(t, members[i], prep.PlayerId, "PrepareBattle 按名单顺序")
		require.Equal(t, presetID, prep.BattleId)
	}
	require.Len(t, fake.created, 1)
	created := fake.created[0]
	require.Equal(t, presetID, created.BattleId)
	require.Equal(t, testActivityConfigId, created.BattleConfigId)
	require.Equal(t, uint32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), created.MatchMode)
	require.True(t, proto.Equal(actx, created.GetActivityContext()), "活动上下文原样进 CreateBattleRequest")
	for i, p := range created.Players {
		require.Equal(t, members[i], p.PlayerId)
		require.Zero(t, p.TeamIndex, "PVE 全员 team 0")
	}
	for _, pid := range members {
		require.Equal(t, ticketStateReady, mr.HGet(matchTicketKey(pid), ticketFieldState))
		require.Equal(t, strconv.FormatUint(presetID, 10), mr.HGet(matchTicketKey(pid), ticketFieldBattleId))
	}
}

// RunActivityGather 失败:全员按 ticket id 删票、不回队列;没有建局。
func TestRunActivityGatherFailureDeletesAllTickets(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, nil)
	members := []uint64{9731, 9732, 9733}
	setPlayerLocation(t, mr, 9731, 1)
	setPlayerLocation(t, mr, 9733, 1) // 9732 没有位置 → no_location
	tickets := mustCreateActivityTickets(t, svcCtx, members)

	require.False(t, RunActivityGather(svcCtx, testActivityConfigId, members, tickets, 880_000_000_002, testActivityContext(9731)))

	require.Equal(t, []uint64{9731}, fake.cancelled, "已冻结者解冻")
	require.Empty(t, fake.created)
	requireNoTickets(t, mr, members...)
	require.False(t, mr.Exists(matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), testActivityConfigId)), "不回队列")
}

// 回归:不带选项的 gather(runGather 的全部原有入口)仍自己发号、CreateBattleRequest 不带上下文;
// 发号器 fence 时照旧 fail-closed。
func TestRunGatherWithoutOptionsUnchanged(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, nil)
	members := []uint64{9741, 9742}
	for _, pid := range members {
		setPlayerLocation(t, mr, pid, 1)
	}
	tickets := mustCreateActivityTickets(t, svcCtx, members)

	require.True(t, RunTeamGather(svcCtx, testActivityConfigId, members, tickets))
	require.Len(t, fake.created, 1)
	require.NotZero(t, fake.created[0].BattleId, "无预设时 gather 自己发号")
	require.Nil(t, fake.created[0].GetActivityContext(), "普通对局不带活动上下文")

	// 发号器 fence:无预设 battle_id 的 gather 必须整体失败、不 PrepareBattle。
	others := []uint64{9743, 9744}
	for _, pid := range others {
		setPlayerLocation(t, mr, pid, 1)
	}
	otherTickets := mustCreateActivityTickets(t, svcCtx, others)
	svcCtx.BattleIDGen.Fence()
	preparesBefore := len(fake.prepares)

	require.False(t, RunTeamGather(svcCtx, testActivityConfigId, others, otherTickets))
	require.Len(t, fake.prepares, preparesBefore, "发号失败不得进入 PrepareBattle")
	requireNoTickets(t, mr, others...)
}
