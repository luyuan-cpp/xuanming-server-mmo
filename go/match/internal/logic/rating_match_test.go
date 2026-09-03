package logic

// 评分匹配(cross-zone-matchmaking.md §11)的 miniredis 测试:容差曲线 / 新号立即
// 互配 / 分差超容差等待放宽后再配 / 候选按分差就近 / 队首凑不到不阻塞后面的锚点 /
// PVE 不看评分 / list 与评分镜像 ZSET 在入队、取消、回队首、剔除、迁移后一致 /
// Elo 更新与幂等 / 5v5 蛇形分队(纯函数 + gather 真跑到 CreateBattle)。

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"

	"match/internal/metrics"
	"match/internal/svc"

	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	matchpb "proto/match"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

// assertQueueMirrorConsistent 断言队列 list 与评分镜像 ZSET 一致:ZCARD==LLEN 且
// 成员集合相同(两者都不存在也算一致)。每个用例末尾都调用。
func assertQueueMirrorConsistent(t *testing.T, mr *miniredis.Miniredis, queueKey string) {
	t.Helper()
	rankKey, err := rankKeyForQueue(queueKey)
	require.NoError(t, err)
	var list, members []string
	if mr.Exists(queueKey) {
		list, err = mr.List(queueKey)
		require.NoError(t, err)
	}
	if mr.Exists(rankKey) {
		members, err = mr.ZMembers(rankKey)
		require.NoError(t, err)
	}
	require.Equal(t, len(list), len(members), "ZCARD 必须等于 LLEN queue=%s list=%v zset=%v", queueKey, list, members)
	require.ElementsMatch(t, list, members, "list 与 ZSET 成员集合必须相同 queue=%s", queueKey)
}

// setRating 直接写玩家评分 hash(模拟历史对局结果)。
func setRating(t *testing.T, mr *miniredis.Miniredis, playerId uint64, rating float64) {
	t.Helper()
	mr.HSet(matchRatingKey(playerId), ratingFieldRating, formatRating(rating))
}

// setEnqueuedAgo 把票据的入队时刻回拨(模拟已等待 d),容差随之放宽。
func setEnqueuedAgo(t *testing.T, mr *miniredis.Miniredis, playerId uint64, d time.Duration) {
	t.Helper()
	mr.HSet(matchTicketKey(playerId), ticketFieldEnqueuedAt, strconv.FormatUint(nowMs()-uint64(d.Milliseconds()), 10))
}

func zscore(t *testing.T, mr *miniredis.Miniredis, queueKey string, playerId uint64) float64 {
	t.Helper()
	rankKey, err := rankKeyForQueue(queueKey)
	require.NoError(t, err)
	score, err := mr.ZScore(rankKey, strconv.FormatUint(playerId, 10))
	require.NoError(t, err)
	return score
}

func expectGroup(t *testing.T, groups <-chan gatherCall, want []uint64) gatherCall {
	t.Helper()
	select {
	case call := <-groups:
		require.Equal(t, want, call.members)
		return call
	case <-time.After(3 * time.Second):
		t.Fatalf("没有弹出期望的组 %v", want)
		return gatherCall{}
	}
}

func expectNoGroup(t *testing.T, groups <-chan gatherCall) {
	t.Helper()
	select {
	case call := <-groups:
		t.Fatalf("不应弹组,却弹出了 %v", call.members)
	case <-time.After(200 * time.Millisecond):
	}
}

var queue1v1 = matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)

// ---- 容差曲线 ----

// 默认曲线:100 起、每 5s +100、上限 1000;配置覆盖生效;漏配按默认。
func TestRatingToleranceCurve(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	require.Equal(t, 100.0, ratingToleranceFor(svcCtx, 0))
	require.Equal(t, 100.0, ratingToleranceFor(svcCtx, 4))
	require.Equal(t, 200.0, ratingToleranceFor(svcCtx, 5))
	require.Equal(t, 300.0, ratingToleranceFor(svcCtx, 10))
	require.Equal(t, 1000.0, ratingToleranceFor(svcCtx, 45))
	require.Equal(t, 1000.0, ratingToleranceFor(svcCtx, 3600))
	require.Equal(t, 100.0, ratingToleranceFor(svcCtx, -3), "负等待按 0")

	svcCtx.Config.RatingToleranceBase = 50
	svcCtx.Config.RatingToleranceStepSeconds = 10
	svcCtx.Config.RatingToleranceStepDelta = 25
	svcCtx.Config.RatingToleranceMax = 120
	require.Equal(t, 50.0, ratingToleranceFor(svcCtx, 9))
	require.Equal(t, 75.0, ratingToleranceFor(svcCtx, 10))
	require.Equal(t, 120.0, ratingToleranceFor(svcCtx, 100))
}

// ---- 凑组 ----

// 两个零历史新号(都 1500)第一轮就互配;票据记评分、镜像 score=1500;弹空后一致。
func TestNewPlayersMatchImmediately(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	groups := stubGather(t)
	setPlayerLocation(t, mr, 11001, 1)
	setPlayerLocation(t, mr, 11002, 2)
	require.Zero(t, joinQueue1v1(t, svcCtx, 11001).ErrorCode)
	require.Zero(t, joinQueue1v1(t, svcCtx, 11002).ErrorCode)
	ticket, err := loadTicket(svcCtx, 11001)
	require.NoError(t, err)
	require.Equal(t, float64(defaultRating), ticket.Rating)
	require.Equal(t, formatRating(defaultRating), mr.HGet(matchTicketKey(11001), ticketFieldRating), "评分必须落进票据")
	require.Equal(t, float64(defaultRating), zscore(t, mr, queue1v1, 11002))
	assertQueueMirrorConsistent(t, mr, queue1v1)

	runMatcherRound(context.Background(), svcCtx)

	call := expectGroup(t, groups, []uint64{11001, 11002})
	require.Len(t, call.tickets, 2)
	require.False(t, mr.Exists(queue1v1))
	assertQueueMirrorConsistent(t, mr, queue1v1)
}

// 分差 300 > 起始容差 100:不配;锚点等到 10s(容差 300)后配上。
func TestRatingGapWaitsUntilToleranceWidens(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	groups := stubGather(t)
	setPlayerLocation(t, mr, 11101, 1)
	setPlayerLocation(t, mr, 11102, 1)
	setRating(t, mr, 11101, 1500)
	setRating(t, mr, 11102, 1800)
	require.Zero(t, joinQueue1v1(t, svcCtx, 11101).ErrorCode)
	require.Zero(t, joinQueue1v1(t, svcCtx, 11102).ErrorCode)
	require.Equal(t, 1800.0, zscore(t, mr, queue1v1, 11102), "镜像 score 必须是入队时读到的评分")

	runMatcherRound(context.Background(), svcCtx)
	expectNoGroup(t, groups)
	for _, pid := range []uint64{11101, 11102} {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.Equal(t, ticketStateQueued, ticket.State, "候选不足时队列不动、票据仍 queued")
	}
	list, err := mr.List(queue1v1)
	require.NoError(t, err)
	require.Equal(t, []string{"11101", "11102"}, list, "队列不动")
	assertQueueMirrorConsistent(t, mr, queue1v1)

	// 锚点已等 9s:容差 200 仍不够。
	setEnqueuedAgo(t, mr, 11101, 9*time.Second)
	runMatcherRound(context.Background(), svcCtx)
	expectNoGroup(t, groups)

	// 10s:容差 300,配上。
	setEnqueuedAgo(t, mr, 11101, 10*time.Second)
	runMatcherRound(context.Background(), svcCtx)
	expectGroup(t, groups, []uint64{11101, 11102})
	assertQueueMirrorConsistent(t, mr, queue1v1)
}

// 容差内多个候选按与锚点的分差就近取;没被选中的留在队列且镜像一致。
func TestClosestCandidateWins(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	groups := stubGather(t)
	for pid, rating := range map[uint64]float64{11201: 1500, 11202: 1700, 11203: 1520} {
		setPlayerLocation(t, mr, pid, 1)
		setRating(t, mr, pid, rating)
	}
	require.Zero(t, joinQueue1v1(t, svcCtx, 11201).ErrorCode)
	require.Zero(t, joinQueue1v1(t, svcCtx, 11202).ErrorCode)
	require.Zero(t, joinQueue1v1(t, svcCtx, 11203).ErrorCode)
	setEnqueuedAgo(t, mr, 11201, time.Minute) // 容差 1000,两人都在窗口内

	runMatcherRound(context.Background(), svcCtx)

	expectGroup(t, groups, []uint64{11201, 11203})
	list, err := mr.List(queue1v1)
	require.NoError(t, err)
	require.Equal(t, []string{"11202"}, list, "分差更大的 11202 留下")
	assertQueueMirrorConsistent(t, mr, queue1v1)
}

// 队首凑不到候选不阻塞后面的人:A(1500)与 B/C(1800/1820)超容差,B 作为
// 锚点与 C 成组,A 继续等。
func TestHeadAnchorDoesNotBlockLaterAnchors(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	groups := stubGather(t)
	for pid, rating := range map[uint64]float64{11301: 1500, 11302: 1800, 11303: 1820} {
		setPlayerLocation(t, mr, pid, 1)
		setRating(t, mr, pid, rating)
	}
	require.Zero(t, joinQueue1v1(t, svcCtx, 11301).ErrorCode)
	require.Zero(t, joinQueue1v1(t, svcCtx, 11302).ErrorCode)
	require.Zero(t, joinQueue1v1(t, svcCtx, 11303).ErrorCode)

	runMatcherRound(context.Background(), svcCtx)

	expectGroup(t, groups, []uint64{11302, 11303})
	list, err := mr.List(queue1v1)
	require.NoError(t, err)
	require.Equal(t, []string{"11301"}, list)
	ticket, err := loadTicket(svcCtx, 11301)
	require.NoError(t, err)
	require.Equal(t, ticketStateQueued, ticket.State)
	assertQueueMirrorConsistent(t, mr, queue1v1)
}

// PVE 组队不看评分:分差 2000 的三人按等待序立即成组;镜像照样维护。
func TestPveTeamIgnoresRating(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	svcCtx.Config.PveTeamSizeByConfigId = map[string]uint32{"1": 3}
	groups := stubGather(t)
	queueKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), 1)
	for i, rating := range []float64{3000, 1000, 2000} {
		pid := uint64(11401 + i)
		setPlayerLocation(t, mr, pid, 1)
		setRating(t, mr, pid, rating)
		joinQueue(t, svcCtx, pid, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, 1)
	}
	require.Equal(t, 3000.0, zscore(t, mr, queueKey, 11401))
	assertQueueMirrorConsistent(t, mr, queueKey)

	runMatcherRound(context.Background(), svcCtx)

	expectGroup(t, groups, []uint64{11401, 11402, 11403})
	assertQueueMirrorConsistent(t, mr, queueKey)
}

// ---- list 与镜像一致性:入队 / 取消 / 回队首 / 剔除 / 重复项 / 孤儿镜像 ----

func TestQueueMirrorStaysConsistentAcrossOperations(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	stubGather(t)
	ids := []uint64{11501, 11502, 11503}
	tickets := make(map[uint64]string, len(ids))
	for _, pid := range ids {
		setPlayerLocation(t, mr, pid, 1)
		setRating(t, mr, pid, 2000) // 全员超出默认容差的同分,matcher 不会把他们配走
		tickets[pid] = joinQueue1v1(t, svcCtx, pid).QueueTicket
	}
	assertQueueMirrorConsistent(t, mr, queue1v1)

	// 取消:list+ZSET 一起摘。
	_, err := NewCancelQueueLogic(context.Background(), svcCtx).CancelQueue(&matchpb.CancelQueueRequest{
		PlayerId: 11502, QueueTicket: tickets[11502],
	})
	require.NoError(t, err)
	list, err := mr.List(queue1v1)
	require.NoError(t, err)
	require.Equal(t, []string{"11501", "11503"}, list)
	assertQueueMirrorConsistent(t, mr, queue1v1)

	// 弹出后回队首:ZADD 回原评分。
	members, got, ok := popGroup(svcCtx, queue1v1, 2)
	require.True(t, ok)
	require.Equal(t, []uint64{11501, 11503}, members)
	require.False(t, mr.Exists(queue1v1))
	assertQueueMirrorConsistent(t, mr, queue1v1)
	for _, pid := range members {
		written, err := setTicketMatched(svcCtx, pid, got[pid], 30)
		require.NoError(t, err)
		require.True(t, written)
	}
	requeueFront(svcCtx, queue1v1, members, got)
	list, err = mr.List(queue1v1)
	require.NoError(t, err)
	require.Equal(t, []string{"11501", "11503"}, list)
	require.Equal(t, 2000.0, zscore(t, mr, queue1v1, 11501), "回队首写回原评分")
	assertQueueMirrorConsistent(t, mr, queue1v1)

	// 无效成员剔除:票据被删的成员在 matcher 校验时从 list+ZSET 一起摘掉。
	deleteTicket(svcCtx, 11501)
	runMatcherRound(context.Background(), svcCtx)
	list, err = mr.List(queue1v1)
	require.NoError(t, err)
	require.Equal(t, []string{"11503"}, list)
	assertQueueMirrorConsistent(t, mr, queue1v1)

	// 重复项:同一玩家两份,弹组把两份一起摘掉。
	require.NoError(t, enqueueAtomic(svcCtx, queue1v1, 11503, 2000))
	setPlayerLocation(t, mr, 11504, 1)
	setRating(t, mr, 11504, 2000)
	require.Zero(t, joinQueue1v1(t, svcCtx, 11504).ErrorCode)
	members, _, ok = popGroup(svcCtx, queue1v1, 2)
	require.True(t, ok)
	require.Equal(t, []uint64{11503, 11504}, members)
	require.False(t, mr.Exists(queue1v1), "重复的一份也被摘掉")
	assertQueueMirrorConsistent(t, mr, queue1v1)

	// 孤儿镜像(list 空、ZSET 有人)在剔除空队列时一并清掉。
	rankKey, err := rankKeyForQueue(queue1v1)
	require.NoError(t, err)
	_, err = mr.ZAdd(rankKey, 1500, "11599")
	require.NoError(t, err)
	_, err = mr.SetAdd(matchQueueIndexKey, queue1v1)
	require.NoError(t, err)
	runMatcherRound(context.Background(), svcCtx)
	require.False(t, mr.Exists(rankKey), "孤儿镜像必须随空队列剔除一起清掉")
	if mr.Exists(matchQueueIndexKey) {
		inIndex, err := mr.IsMember(matchQueueIndexKey, queue1v1)
		require.NoError(t, err)
		require.False(t, inIndex)
	}
	assertQueueMirrorConsistent(t, mr, queue1v1)
}

// 旧格式队列迁移时也写评分镜像(评分读不到用 1500);镜像缺失的 list 成员
// (滚动升级窗口内旧实例只写 list)由 matcher 按票据评分补写。
func TestMigrateLegacyQueuesWritesRankMirror(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	legacyKey := legacyMatchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	writeLegacyTicket(t, mr, 11601, "tk-a", matchpb.MatchMode_MATCH_MODE_1V1, 0)
	writeLegacyTicket(t, mr, 11602, "tk-b", matchpb.MatchMode_MATCH_MODE_1V1, 0)
	setRating(t, mr, 11601, 1700)
	_, err := mr.RPush(legacyKey, "11601", "11602")
	require.NoError(t, err)

	migrateLegacyQueues(svcCtx)

	require.Equal(t, 1700.0, zscore(t, mr, queue1v1, 11601), "有评分记录的按记录写")
	require.Equal(t, float64(defaultRating), zscore(t, mr, queue1v1, 11602), "无记录按默认 1500")
	assertQueueMirrorConsistent(t, mr, queue1v1)

	// 只写 list 不写 ZSET 的成员(旧实例入队):matcher 校验时补写镜像并能成组。
	rankKey, err := rankKeyForQueue(queue1v1)
	require.NoError(t, err)
	_, err = mr.ZRem(rankKey, "11602")
	require.NoError(t, err)
	groups := stubGather(t)
	runMatcherRound(context.Background(), svcCtx)
	expectGroup(t, groups, []uint64{11601, 11602})
	assertQueueMirrorConsistent(t, mr, queue1v1)
}

// ---- Elo 结果回流 ----

func resultEvent(battleId uint64, mode matchpb.MatchMode, outcome battlepb.EBattleOutcome, teamA, teamB []uint64) *kafkapb.BattleResultEvent {
	return &kafkapb.BattleResultEvent{
		BattleId:  battleId,
		MatchMode: uint32(mode),
		Outcome:   outcome,
		Teams: []*kafkapb.BattleResultTeam{
			{TeamIndex: 0, PlayerIds: teamA},
			{TeamIndex: 1, PlayerIds: teamB},
		},
	}
}

func ratingOf(t *testing.T, svcCtx *svc.ServiceContext, playerId uint64) float64 {
	t.Helper()
	r, err := loadRating(svcCtx, playerId)
	require.NoError(t, err)
	return r
}

// 1V1:胜者 +16 / 败者 -16(同分 K=32);同一 battle_id 再投递一次不重复入账;
// 平局按 0.5;PVE 忽略;计数器按 outcome 分别 +1。
func TestApplyBattleResultEloAndIdempotent(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	mode := matchpb.MatchMode_MATCH_MODE_1V1
	modeName := mode.String()
	applied := metrics.RatingUpdateValue(modeName, ratingOutcomeApplied)
	dup := metrics.RatingUpdateValue(modeName, ratingOutcomeDuplicate)

	outcome, err := ApplyBattleResult(svcCtx, resultEvent(9001, mode, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN, []uint64{12001}, []uint64{12002}))
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeApplied, outcome)
	require.InDelta(t, 1516, ratingOf(t, svcCtx, 12001), 0.01)
	require.InDelta(t, 1484, ratingOf(t, svcCtx, 12002), 0.01)
	require.Equal(t, "1", mr.HGet(matchRatingKey(12001), ratingFieldGames))
	require.NotEmpty(t, mr.HGet(matchRatingKey(12001), ratingFieldUpdatedAt))
	require.True(t, mr.Exists(matchRatingAppliedKey(9001)))
	ttl := mr.TTL(matchRatingAppliedKey(9001))
	require.Greater(t, ttl, 6*24*time.Hour)
	require.LessOrEqual(t, ttl, 7*24*time.Hour)
	require.Equal(t, applied+1, metrics.RatingUpdateValue(modeName, ratingOutcomeApplied))

	// 重复投递:评分与局数都不变。
	outcome, err = ApplyBattleResult(svcCtx, resultEvent(9001, mode, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN, []uint64{12001}, []uint64{12002}))
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeDuplicate, outcome)
	require.InDelta(t, 1516, ratingOf(t, svcCtx, 12001), 0.01)
	require.Equal(t, "1", mr.HGet(matchRatingKey(12001), ratingFieldGames))
	require.Equal(t, dup+1, metrics.RatingUpdateValue(modeName, ratingOutcomeDuplicate))

	// 平局:高分方期望 > 0.5,平局即失分。
	expectedA := 1 / (1 + math.Pow(10, (1484-1516)/400.0))
	deltaA := 32 * (0.5 - expectedA)
	outcome, err = ApplyBattleResult(svcCtx, resultEvent(9002, mode, battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW, []uint64{12001}, []uint64{12002}))
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeApplied, outcome)
	require.Less(t, deltaA, 0.0)
	require.InDelta(t, 1516+deltaA, ratingOf(t, svcCtx, 12001), 0.02)
	require.InDelta(t, 1484-deltaA, ratingOf(t, svcCtx, 12002), 0.02)
	require.Equal(t, "2", mr.HGet(matchRatingKey(12001), ratingFieldGames))

	// B 胜(team 1):胜者 = team 1。
	before := ratingOf(t, svcCtx, 12002)
	outcome, err = ApplyBattleResult(svcCtx, resultEvent(9003, mode, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN, []uint64{12001}, []uint64{12002}))
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeApplied, outcome)
	require.Greater(t, ratingOf(t, svcCtx, 12002), before)

	// PVE / 未结束 / 队伍形态异常:忽略,不写标记也不改分。
	ignored := metrics.RatingUpdateValue(matchpb.MatchMode_MATCH_MODE_PVE_TEAM.String(), ratingOutcomeIgnored)
	outcome, err = ApplyBattleResult(svcCtx, resultEvent(9004, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN, []uint64{12003}, []uint64{}))
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeIgnored, outcome)
	require.False(t, mr.Exists(matchRatingAppliedKey(9004)))
	require.False(t, mr.Exists(matchRatingKey(12003)))
	require.Equal(t, ignored+1, metrics.RatingUpdateValue(matchpb.MatchMode_MATCH_MODE_PVE_TEAM.String(), ratingOutcomeIgnored))
	outcome, err = ApplyBattleResult(svcCtx, resultEvent(9005, mode, battlepb.EBattleOutcome_BATTLE_OUTCOME_ONGOING, []uint64{12001}, []uint64{12002}))
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeIgnored, outcome)
	outcome, err = ApplyBattleResult(svcCtx, resultEvent(9006, mode, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN, []uint64{12001}, nil))
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeIgnored, outcome, "空队伍不计分")
	require.False(t, mr.Exists(matchRatingAppliedKey(9006)))
	require.Equal(t, "3", mr.HGet(matchRatingKey(12001), ratingFieldGames), "忽略的局不计入局数")
}

// 5V5:队伍用平均分算期望,同队每人同一增量。A 队 1600/1400(均 1500)对
// B 队 1500/1500,B 胜 → A 队每人 -16、B 队每人 +16。
func TestApplyBattleResult5v5UsesTeamAverage(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	setRating(t, mr, 12101, 1600)
	setRating(t, mr, 12102, 1400)
	outcome, err := ApplyBattleResult(svcCtx, resultEvent(9101, matchpb.MatchMode_MATCH_MODE_5V5,
		battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN, []uint64{12101, 12102}, []uint64{12103, 12104}))
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeApplied, outcome)
	require.InDelta(t, 1584, ratingOf(t, svcCtx, 12101), 0.01)
	require.InDelta(t, 1384, ratingOf(t, svcCtx, 12102), 0.01)
	require.InDelta(t, 1516, ratingOf(t, svcCtx, 12103), 0.01)
	require.InDelta(t, 1516, ratingOf(t, svcCtx, 12104), 0.01)
}

// ---- 5v5 蛇形分队 ----

// 按评分降序蛇形 0/1/1/0/0/1/1/0/0/1:第 1、4、5、8、9 名一队,其余一队;同分按弹出序稳定。
func TestAssignBalancedTeamsSnake(t *testing.T) {
	members := []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	ratings := map[uint64]float64{}
	for i, pid := range members {
		ratings[pid] = float64(2000 - 100*i) // 成员 1 最高
	}
	teams := assignBalancedTeams(members, ratings)
	require.Equal(t, []uint32{0, 1, 1, 0, 0, 1, 1, 0, 0, 1}, teams)
	sum := [2]float64{}
	for i, pid := range members {
		sum[teams[i]] += ratings[pid]
	}
	// 等差评分 10 人(5 对)蛇形分配两队总分只差一个步长(7800 / 7700),
	// 远优于"前 5 后 5"的 8500 / 7000。
	require.InDelta(t, sum[0], sum[1], 100, "蛇形分配两队总分接近")

	// 乱序输入:分队跟评分走,不跟下标走。
	shuffled := []uint64{10, 1, 9, 2, 8, 3, 7, 4, 6, 5}
	teams = assignBalancedTeams(shuffled, ratings)
	byPlayer := map[uint64]uint32{}
	for i, pid := range shuffled {
		byPlayer[pid] = teams[i]
	}
	require.Equal(t, map[uint64]uint32{1: 0, 2: 1, 3: 1, 4: 0, 5: 0, 6: 1, 7: 1, 8: 0, 9: 0, 10: 1}, byPlayer)

	// 全员同分:按弹出序蛇形,前后各 5 人且第 1 位在 0 队。
	same := map[uint64]float64{}
	for _, pid := range members {
		same[pid] = defaultRating
	}
	teams = assignBalancedTeams(members, same)
	require.Equal(t, []uint32{0, 1, 1, 0, 0, 1, 1, 0, 0, 1}, teams)
}

// gather 真跑到 CreateBattle:5v5 快照的 TeamIndex 按评分蛇形分队,每队 5 人。
func TestGather5v5SnakeTeamsReachCreateBattle(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, nil)
	var members []uint64
	tickets := map[uint64]string{}
	ratings := map[uint64]float64{}
	for i := 0; i < required5v5Players; i++ {
		pid := uint64(12201 + i)
		setPlayerLocation(t, mr, pid, 1)
		ratings[pid] = float64(1000 + 137*i%900) // 打散的评分
		setRating(t, mr, pid, ratings[pid])
		members = append(members, pid)
		tickets[pid] = "t"
	}
	want := assignBalancedTeams(members, ratings)

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_5V5, 7, members, true, tickets)

	require.Len(t, fake.created, 1, "5v5 必须建局")
	require.Empty(t, fake.cancelled)
	players := fake.created[0].Players
	require.Len(t, players, required5v5Players)
	count := [2]int{}
	for i, p := range players {
		require.Equal(t, members[i], p.PlayerId)
		require.Equal(t, want[i], p.TeamIndex, "player=%d", p.PlayerId)
		count[p.TeamIndex]++
	}
	require.Equal(t, [2]int{5, 5}, count)

	// 1v1 语义不变:前后两人各一队。
	teams := teamAssignment(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 1, []uint64{1, 2})
	require.Equal(t, []uint32{0, 1}, teams)
	teams = teamAssignment(svcCtx, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, 1, []uint64{1, 2, 3})
	require.Equal(t, []uint32{0, 0, 0}, teams)
}
