package logic

// 评分匹配(cross-zone-matchmaking.md §11)的 miniredis 测试:容差曲线 / 新号立即
// 互配 / 分差超容差等待放宽后再配 / 候选按分差就近 / 队首凑不到不阻塞后面的锚点 /
// PVE 不看评分 / list 与评分镜像 ZSET 在入队、取消、回队首、剔除、镜像补写后一致 /
// Elo 更新与幂等 / 5v5 蛇形分队(纯函数 + gather 真跑到 CreateBattle)。

import (
	"context"
	"strconv"
	"testing"
	"time"

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

// testRatings 是 setRating 写入的评分,经 ratingOverrideForTest 喂给 loadRating。
var testRatings map[uint64]float64

// setRating 设玩家评分(模拟历史对局结果)。评分不在 Redis,mr 只为保持调用形状。
func setRating(t *testing.T, mr *miniredis.Miniredis, playerId uint64, rating float64) {
	t.Helper()
	if ratingOverrideForTest == nil {
		testRatings = map[uint64]float64{}
		ratingOverrideForTest = func(id uint64) (float64, bool) {
			r, ok := testRatings[id]
			return r, ok
		}
		t.Cleanup(func() {
			ratingOverrideForTest = nil
			testRatings = nil
		})
	}
	testRatings[playerId] = rating
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
	useTestCatalog(t, svcCtx, map[uint32]uint32{1: 3})
	groups := stubGather(t)
	// 不填 team_size = 副本上限(3 人档);PVE_TEAM 的队列 key 带人数段。
	queueKey := matchSizedQueueKey(int32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), 1, 3)
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
	mr.Push(queue1v1, "11503") // 人工构造遗留重复项;生产入队现在会去重。
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

// 镜像缺失的 list 成员由 matcher 按票据评分补写并能成组。原先挂在旧格式队列迁移用例的后半段,
// 2026-10-20 迁移代码删除后单列,补写路径本身不变。
func TestMatcherBackfillsMissingRankMirror(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	for _, pid := range []uint64{11601, 11602} {
		setPlayerLocation(t, mr, pid, 1)
		require.Zero(t, joinQueue1v1(t, svcCtx, pid).ErrorCode)
	}
	assertQueueMirrorConsistent(t, mr, queue1v1)

	// 摘掉一名成员的镜像:matcher 校验时补写镜像并能成组。
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

	// 1v1:两人各一队,谁在 0 队由 battle_id 决定(splitmix64 终混最低位:id 1 → 1,id 2 → 0),
	// 不再恒归排在前面的锚点;切磋保持发起者在 0 队。
	require.Equal(t, []uint32{1, 0}, teamAssignment(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 1, []uint64{1, 2}))
	require.Equal(t, []uint32{0, 1}, teamAssignment(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 2, []uint64{1, 2}))
	require.Equal(t, []uint32{0, 1}, teamAssignment(svcCtx, matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE, 1, []uint64{1, 2}))
	flips := 0
	for battleId := uint64(1); battleId <= 1000; battleId++ {
		flips += int(sideFlip(battleId))
	}
	require.InDelta(t, 500, flips, 50, "两侧机会均等(id 1..1000 实为 495)")
	teams := teamAssignment(svcCtx, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, 1, []uint64{1, 2, 3})
	require.Equal(t, []uint32{0, 0, 0}, teams)
}

// 评分只在全球权威库:没有权威库(dev / test 档)时结果不入账、评分仍是默认值。
func TestApplyBattleResultWithoutAuthorityIsIgnored(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	outcome, err := ApplyBattleResult(svcCtx, resultEvent(9001, matchpb.MatchMode_MATCH_MODE_1V1, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN, []uint64{12001}, []uint64{12002}))
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeIgnored, outcome)
	rating, err := loadRating(svcCtx, 12001)
	require.NoError(t, err)
	require.Equal(t, float64(defaultRating), rating)
}
