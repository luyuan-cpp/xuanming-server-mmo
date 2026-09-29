package logic

// CreateBattle 被 battle 明确拒绝(tip≠0)时 gather 补偿路径的回归测试。
//
// turn-based §22 D70 起 battle 侧 CreateBattle 改为 fail-closed:在任何副作用之前为全部
// 参战者预签票据,任一失败回 kServiceUnavailable、不建房。match 侧不为此改逻辑,沿用
// CreateBattle 失败的通用补偿:DestroyBattle(battle 侧对不存在的房间幂等成功)→ 逐人
// CancelBattlePrepare → 按入口语义回队首 / 删票;不推进 ready、不登记观战索引。
// 三个用例分别钉住 fail() 的三种票据分支:队列凑单(回队首)、整队(全员删票)、切磋(无票据)。
// scene / battle RPC 全部经 stubGatherRPCs 顶替,fakeGatherRPCs.createTip 模拟拒绝。

import (
	"context"
	"testing"

	"match/internal/svc"

	matchpb "proto/match"

	"shared/generated/pb/table"

	"github.com/stretchr/testify/require"
)

// requireRejectedBattleLeftNoTrace 断言被拒的那一场:只打过一次 CreateBattle 且未建成、
// DestroyBattle 幂等兜底打过一次、观战索引里没有它(观战与补签都不该找到一场不存在的战斗)。
func requireRejectedBattleLeftNoTrace(t *testing.T, svcCtx *svc.ServiceContext, fake *fakeGatherRPCs) {
	t.Helper()
	require.Empty(t, fake.created, "battle 拒绝建房,不能记成开局成功")
	require.Len(t, fake.createRejected, 1, "CreateBattle 只打一次,被拒后不重试")
	battleId := fake.createRejected[0].GetBattleId()
	require.NotZero(t, battleId)
	require.Equal(t, []uint64{battleId}, fake.destroyed,
		"被拒仍走通用兜底 DestroyBattle(battle 侧幂等成功),之后才进入解冻分支")
	record, err := loadSpectateBattle(svcCtx, battleId)
	require.NoError(t, err)
	require.Nil(t, record, "未建成的战斗不得登记观战索引")
	require.Zero(t, activeIndexSize(t, svcCtx))
}

// 队列凑单(requeueOnFail=true):battle 以 kServiceUnavailable 拒绝建房 →
// 两人都解冻;没有肇事者(failedPlayer=0),两人都不删票,按原序回队首、票据恢复 queued。
func TestGatherCreateBattleServiceUnavailableRequeuesAllMembers(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, map[uint64]string{9601: "fp", 9602: "fp"})
	fake.createTip = uint32(table.CommonError_kServiceUnavailable)
	members, tickets, queueKey := popMatchedPair(t, svcCtx, mr, 9601, 9602)

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	requireRejectedBattleLeftNoTrace(t, svcCtx, fake)
	require.Len(t, fake.prepares, 2, "两人都冻结过")
	require.ElementsMatch(t, members, fake.cancelled, "已冻结的两人都要解冻")

	for _, pid := range members {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket, "battle 拒绝不是玩家的错,不得删票 player=%d", pid)
		require.Equal(t, tickets[pid], ticket.Ticket, "回队首的是原票据,不是新票 player=%d", pid)
		require.Equal(t, ticketStateQueued, ticket.State, "不得推进 ready player=%d", pid)
		ttl, err := svcCtx.MatchRedis.Ttl(matchTicketKey(pid))
		require.NoError(t, err)
		require.Greater(t, ttl, int(svcCtx.Config.MatchedTicketTTLSeconds), "回队首恢复长 TTL player=%d", pid)
	}
	list, err := mr.List(queueKey)
	require.NoError(t, err)
	require.Equal(t, []string{"9601", "9602"}, list, "两人按弹出序回队首")
}

// 整队开战(requeueOnFail=false,带票据):battle 拒绝建房 → 全员解冻、全员按 ticket id 删票,
// 不回队列(team-system.md §E.2 "不可拆分")。
func TestRunTeamGatherCreateBattleServiceUnavailableDeletesAllTickets(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, nil)
	fake.createTip = uint32(table.CommonError_kServiceUnavailable)
	roster := []uint64{9611, 9612, 9613}
	for _, pid := range roster {
		setPlayerLocation(t, mr, pid, 1)
	}
	tickets, failed := NewTeamBattleStarter(svcCtx).CreateMatchedTickets(context.Background(), 1, testTeamId, roster, nil)
	require.Zero(t, failed)

	require.False(t, RunTeamGather(svcCtx, 1, roster, tickets), "battle 拒绝建房 = 开局失败")

	requireRejectedBattleLeftNoTrace(t, svcCtx, fake)
	require.ElementsMatch(t, roster, fake.cancelled, "全员都冻结过,全员解冻")
	for _, pid := range roster {
		require.False(t, mr.Exists(matchTicketKey(pid)), "整队出局:全员删票 player=%d", pid)
	}
	require.False(t, mr.Exists(matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), 1)), "不回队列")
}

// 切磋(没有票据):battle 拒绝建房 → 返回失败、两人解冻,不碰任何票据与队列。
func TestRunChallengeGatherCreateBattleServiceUnavailableUnfreezesBoth(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, nil)
	fake.createTip = uint32(table.CommonError_kServiceUnavailable)
	const challenger, responder = uint64(9621), uint64(9622)
	setPlayerLocation(t, mr, challenger, 1)
	setPlayerLocation(t, mr, responder, 2)

	require.False(t, RunChallengeGather(svcCtx, 0, challenger, responder), "battle 拒绝建房 = 切磋开局失败")

	requireRejectedBattleLeftNoTrace(t, svcCtx, fake)
	require.ElementsMatch(t, []uint64{challenger, responder}, fake.cancelled)
	require.False(t, mr.Exists(matchTicketKey(challenger)), "切磋从未入队,补偿不得凭空写票")
	require.False(t, mr.Exists(matchTicketKey(responder)))
}
