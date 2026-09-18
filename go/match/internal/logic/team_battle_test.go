package logic

// 整队开战票据域(docs/design/team-system.md §E.1 第 3、5、7、8 步;§I.3 #17、#18、#29 的票据部分)
// 的 miniredis 测试。队伍记录、开战锁、整轮重来与 EndMatch 的用例在 internal/team/team_battle_test.go
// (那边用假端口)。本文件不 import team:team 依赖 proto/team 生成物。
//
// 确定性:票据 TTL 用 miniredis 的 TTL 精确比较(不 FastForward、不读墙钟做断言);
// 并发 / 故障时序经 createTeamTicketFn、runTeamGatherFn 桩注入。测试之间不并行(包级测试缝)。

import (
	"context"
	"strconv"
	"testing"
	"time"

	"match/internal/svc"

	matchpb "proto/match"

	"github.com/stretchr/testify/require"
)

// teamBattlePort 与 team.BattleStarter 同形的本地副本,钉住 TeamBattleStarter 的方法集。
// 真正的编译期保证在 match_service.go 装配处(team.NewService(..., logic.NewTeamBattleStarter(svcCtx)))。
type teamBattlePort interface {
	TeamSizeFor(battleConfigId uint32) uint32
	MatchLockTTLSeconds(memberCount int) int
	TicketBlocked(ctx context.Context, playerId uint64) (bool, error)
	CreateMatchedTickets(ctx context.Context, battleConfigId uint32, teamId uint64,
		roster []uint64, zones map[uint64]uint32) (map[uint64]string, uint64)
	RunGather(battleConfigId uint32, roster []uint64, tickets map[uint64]string) bool
}

var _ teamBattlePort = (*TeamBattleStarter)(nil)

const testTeamId uint64 = 88_000_001

// #18:人数口径、锁时长;kMaxBattleTeamSize 与 team.Capacity 同值。
func TestTeamBattleSizeAndLockTTL(t *testing.T) {
	// team.Capacity == 5(§D.1)。logic 测试不能 import team(regen 前编不过),两边测试各自钉住
	// 同一个字面量:改任一侧都会让测试失败,提醒同步。
	require.Equal(t, 5, kMaxBattleTeamSize)

	svcCtx, _ := newTestSvcCtx(t)
	svcCtx.Config.PveTeamSizeByConfigId = map[string]uint32{"1": 3, "2": 10}
	starter := NewTeamBattleStarter(svcCtx)
	require.Equal(t, uint32(3), starter.TeamSizeFor(1))
	require.Equal(t, uint32(kMaxBattleTeamSize), starter.TeamSizeFor(2), "上限按 kMaxBattleTeamSize 收口,与 PVE_TEAM 排队同口径")
	require.Zero(t, starter.TeamSizeFor(99), "未配置 = 未开放组队")

	// 5 人:matched 48s + 补偿 25s + 余量 10s(§E.1 第 6 步)。
	require.Equal(t, 83, starter.MatchLockTTLSeconds(5))
	require.Equal(t, matchedWorstCaseSeconds(5)+compensationWorstCaseSeconds(5)+10, starter.MatchLockTTLSeconds(5))
}

// #17:成员已有票据的预检与 JoinQueue 同一套自愈规则。
func TestTeamTicketBlockedHealsLikeJoinQueue(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	starter := NewTeamBattleStarter(svcCtx)
	ctx := context.Background()
	pveTeam := int32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM)
	queueKey := matchQueueKey(pveTeam, 1)

	blocked, err := starter.TicketBlocked(ctx, 7001)
	require.NoError(t, err)
	require.False(t, blocked, "没有票据不拦")

	// queued 且确实在队列:仍在途 → 拦,票据不动。
	mustCreateTicket(t, svcCtx, 7002, &queueTicket{
		Ticket: "q-7002", Mode: pveTeam, Config: 1, State: ticketStateQueued, EnqueuedAtMs: nowMs(), QueueKey: queueKey,
	})
	require.NoError(t, enqueueAtomic(svcCtx, queueKey, 7002, defaultRating))
	blocked, err = starter.TicketBlocked(ctx, 7002)
	require.NoError(t, err)
	require.True(t, blocked, "自愈后仍在队列 → TeamMemberNotReady")
	require.True(t, mr.Exists(matchTicketKey(7002)))

	// queued 但不在队列(崩溃残留)→ 自愈删票,不拦。
	mustCreateTicket(t, svcCtx, 7003, &queueTicket{
		Ticket: "q-7003", Mode: pveTeam, Config: 1, State: ticketStateQueued, EnqueuedAtMs: nowMs(), QueueKey: queueKey,
	})
	blocked, err = starter.TicketBlocked(ctx, 7003)
	require.NoError(t, err)
	require.False(t, blocked)
	require.False(t, mr.Exists(matchTicketKey(7003)), "孤儿票据被自愈清掉")

	// ready 残留(上一场战斗已结束)→ 自愈删票,不拦。
	mustCreateTicket(t, svcCtx, 7004, &queueTicket{
		Ticket: "r-7004", Mode: pveTeam, Config: 1, State: ticketStateReady, EnqueuedAtMs: nowMs(),
	})
	blocked, err = starter.TicketBlocked(ctx, 7004)
	require.NoError(t, err)
	require.False(t, blocked)
	require.False(t, mr.Exists(matchTicketKey(7004)))

	// matched:gather 在途 → 拦,且不动票据(matched 态只靠 TTL 自愈)。
	mustCreateTicket(t, svcCtx, 7005, &queueTicket{
		Ticket: "m-7005", Mode: pveTeam, Config: 1, State: ticketStateMatched, EnqueuedAtMs: nowMs(),
	})
	blocked, err = starter.TicketBlocked(ctx, 7005)
	require.NoError(t, err)
	require.True(t, blocked)
	require.True(t, mr.Exists(matchTicketKey(7005)))

	// 请求 ctx 已取消:fail-closed。
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	blocked, err = starter.TicketBlocked(canceled, 7001)
	require.Error(t, err)
	require.True(t, blocked)
}

// #18:票据带 team_id,state=matched,TTL 恰为 matchedTicketTTLFor(n);非组队票据字段集不变。
func TestCreateMatchedTicketsWritesTeamTickets(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	starter := NewTeamBattleStarter(svcCtx)
	roster := []uint64{7103, 7101, 7102}
	zones := map[uint64]uint32{7101: 1, 7102: 2, 7103: 1}

	tickets, failed := starter.CreateMatchedTickets(context.Background(), 1, testTeamId, roster, zones)

	require.Zero(t, failed)
	require.Len(t, tickets, len(roster))
	ttl := time.Duration(matchedTicketTTLFor(svcCtx, uint32(len(roster)))) * time.Second
	for _, pid := range roster {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket)
		require.Equal(t, tickets[pid], ticket.Ticket, "返回的 ticket id 就是落盘的那张")
		require.Equal(t, ticketStateMatched, ticket.State)
		require.Equal(t, int32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), ticket.Mode)
		require.Equal(t, uint32(1), ticket.Config)
		require.Equal(t, zones[pid], ticket.ZoneId)
		require.Equal(t, testTeamId, ticket.TeamId)
		require.Equal(t, strconv.FormatUint(testTeamId, 10), mr.HGet(matchTicketKey(pid), ticketFieldTeamId), "team_id 必须落盘")
		require.Empty(t, ticket.QueueKey, "整队开战不入队")
		require.Equal(t, ttl, mr.TTL(matchTicketKey(pid)), "TTL = matchedTicketTTLFor(n)")
	}

	// 非组队票据不写 team_id,读回为 0。
	mustCreateTicket(t, svcCtx, 7199, &queueTicket{
		Ticket: "solo-7199", Mode: int32(matchpb.MatchMode_MATCH_MODE_1V1), State: ticketStateQueued, EnqueuedAtMs: nowMs(),
	})
	fields, err := mr.HKeys(matchTicketKey(7199))
	require.NoError(t, err)
	require.NotContains(t, fields, ticketFieldTeamId)
	solo, err := loadTicket(svcCtx, 7199)
	require.NoError(t, err)
	require.Zero(t, solo.TeamId)
}

// #17:第 4 人已有别人的票据 → 前 3 张本次写下的票被 CAS 删除,第 4 人的票不动,之后不再建票。
func TestCreateMatchedTicketsRollsBackOnExistingTicket(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	starter := NewTeamBattleStarter(svcCtx)
	roster := []uint64{7201, 7202, 7203, 7204, 7205}
	mustCreateTicket(t, svcCtx, 7204, &queueTicket{
		Ticket: "foreign-7204", Mode: int32(matchpb.MatchMode_MATCH_MODE_PVE_SOLO), State: ticketStateMatched, EnqueuedAtMs: nowMs(),
	})

	tickets, failed := starter.CreateMatchedTickets(context.Background(), 1, testTeamId, roster, nil)

	require.Nil(t, tickets)
	require.Equal(t, uint64(7204), failed)
	for _, pid := range []uint64{7201, 7202, 7203} {
		require.False(t, mr.Exists(matchTicketKey(pid)), "本次写下的票必须被 CAS 删除 player=%d", pid)
	}
	require.Equal(t, "foreign-7204", mr.HGet(matchTicketKey(7204), ticketFieldTicket), "别人的票不在回滚集合")
	require.False(t, mr.Exists(matchTicketKey(7205)), "失败之后不再建票")
}

// #29:第 3 人建票返回 err 但票已写入,且请求 ctx 已取消 → 回滚集合含他,独立 ctx 仍删成功。
func TestCreateMatchedTicketsRollsBackUnknownResultWithDetachedContext(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	starter := NewTeamBattleStarter(svcCtx)
	roster := []uint64{7301, 7302, 7303, 7304}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prev := createTeamTicketFn
	t.Cleanup(func() { createTeamTicketFn = prev })
	createTeamTicketFn = func(c context.Context, sc *svc.ServiceContext, pid uint64, ticket *queueTicket, ttl int) (bool, error) {
		if pid != 7303 {
			return prev(c, sc, pid, ticket, ttl)
		}
		// 模拟 Eval 超时:服务端已写入,客户端拿到错误;请求 ctx 同时到期。
		created, err := prev(context.Background(), sc, pid, ticket, ttl)
		require.NoError(t, err)
		require.True(t, created)
		cancel()
		return false, context.DeadlineExceeded
	}

	tickets, failed := starter.CreateMatchedTickets(ctx, 1, testTeamId, roster, nil)

	require.Nil(t, tickets)
	require.Equal(t, uint64(7303), failed)
	require.Error(t, ctx.Err(), "回滚时请求 ctx 已取消")
	for _, pid := range roster {
		require.False(t, mr.Exists(matchTicketKey(pid)), "结果未知的票也要按 ticket id 删掉 player=%d", pid)
	}
}

// #18:端口的 RunGather 原样把 roster 顺序与 tickets 交给 gather,并透传结果。
func TestTeamBattleRunGatherPassesRosterInOrder(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	starter := NewTeamBattleStarter(svcCtx)
	var gotConfig uint32
	var gotRoster []uint64
	var gotTickets map[uint64]string
	result := true
	prev := runTeamGatherFn
	t.Cleanup(func() { runTeamGatherFn = prev })
	runTeamGatherFn = func(_ *svc.ServiceContext, configId uint32, roster []uint64, tickets map[uint64]string) bool {
		gotConfig, gotRoster, gotTickets = configId, roster, tickets
		return result
	}
	tickets := map[uint64]string{2: "t2", 1: "t1", 3: "t3"}

	require.True(t, starter.RunGather(7, []uint64{2, 1, 3}, tickets))
	require.Equal(t, uint32(7), gotConfig)
	require.Equal(t, []uint64{2, 1, 3}, gotRoster, "队长在前再按 join_seq 的顺序由 team 排好,这里不重排")
	require.Equal(t, tickets, gotTickets)

	result = false
	require.False(t, starter.RunGather(7, []uint64{2, 1, 3}, tickets))
}

// RunTeamGather 真跑 gather:roster 原序进 PrepareBattle / CreateBattle,模式 PVE_TEAM、全员 team 0,成功后票据 ready。
func TestRunTeamGatherSuccessKeepsRosterOrder(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, nil)
	roster := []uint64{9403, 9401, 9402}
	for _, pid := range roster {
		setPlayerLocation(t, mr, pid, 1)
	}
	tickets, failed := NewTeamBattleStarter(svcCtx).CreateMatchedTickets(context.Background(), 1, testTeamId, roster, nil)
	require.Zero(t, failed)

	require.True(t, RunTeamGather(svcCtx, 1, roster, tickets))

	require.Len(t, fake.prepares, len(roster))
	for i, req := range fake.prepares {
		require.Equal(t, roster[i], req.PlayerId, "PrepareBattle 按 roster 顺序")
	}
	require.Len(t, fake.created, 1)
	require.Equal(t, uint32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), fake.created[0].MatchMode)
	require.Len(t, fake.created[0].Players, len(roster))
	for i, p := range fake.created[0].Players {
		require.Equal(t, roster[i], p.PlayerId, "快照顺序 = roster 顺序(站位,J-20)")
		require.Zero(t, p.TeamIndex, "PVE 全员 team 0")
	}
	for _, pid := range roster {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket)
		require.Equal(t, ticketStateReady, ticket.State)
	}
}

// RunTeamGather 失败:整队出局,全员按 ticket id 删票、不回队列(§E.2)。
func TestRunTeamGatherFailureDeletesAllTickets(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, nil)
	roster := []uint64{9501, 9502, 9503}
	setPlayerLocation(t, mr, 9501, 1)
	setPlayerLocation(t, mr, 9503, 1) // 9502 没有位置 → no_location
	tickets, failed := NewTeamBattleStarter(svcCtx).CreateMatchedTickets(context.Background(), 1, testTeamId, roster, nil)
	require.Zero(t, failed)

	require.False(t, RunTeamGather(svcCtx, 1, roster, tickets))

	require.Equal(t, []uint64{9501}, fake.cancelled, "已冻结者解冻")
	require.Empty(t, fake.created)
	for _, pid := range roster {
		require.False(t, mr.Exists(matchTicketKey(pid)), "整队出局:全员删票 player=%d", pid)
	}
	require.False(t, mr.Exists(matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), 1)), "不回队列")
}
