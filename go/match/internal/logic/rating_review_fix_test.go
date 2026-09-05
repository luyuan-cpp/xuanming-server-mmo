package logic

// 评分回流 / 容差复审修复(cross-zone-matchmaking.md §11.5 / §11.11)的 miniredis 测试:
// 写到一半失败后重试续写、无人算两次 / 同一玩家两局并发入账都落账(增量写)/
// 回合打满按平局 / 分差超过容差上限的两人等满兜底秒数后成组 + 饥饿 gauge。

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"match/internal/metrics"

	battlepb "proto/battle"
	matchpb "proto/match"

	"github.com/stretchr/testify/require"
)

// withRatingAfterLoadHook 装 ApplyBattleResult 的读评分后注入点,用例结束还原。
func withRatingAfterLoadHook(t *testing.T, fn func(battleId uint64)) {
	t.Helper()
	prev := ratingAfterLoadHook
	ratingAfterLoadHook = fn
	t.Cleanup(func() { ratingAfterLoadHook = prev })
}

// withZeroRatingWriteBackoff 把单人写分重试的退避调零(用例里的失败是持久的 WRONGTYPE)。
func withZeroRatingWriteBackoff(t *testing.T) {
	t.Helper()
	prev := ratingWriteBackoff
	ratingWriteBackoff = 0
	t.Cleanup(func() { ratingWriteBackoff = prev })
}

// 写到一半失败(B 队第 2 人的评分 key 是 string,Lua HGET 报 WRONGTYPE):返回 error、
// outcome=partial,标记留 applying|Δ;已写的人不回滚。修好后再投递同一局 → 续写只补
// 那个人,其余人不再算第二次,标记改 done,总分零和;第三次投递 duplicate。
func TestApplyBattleResultResumesAfterPartialWrite(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	withZeroRatingWriteBackoff(t)
	mode := matchpb.MatchMode_MATCH_MODE_5V5
	modeName := mode.String()
	teamA := []uint64{13001, 13002, 13003}
	teamB := []uint64{13004, 13005, 13006}
	partialBefore := metrics.RatingUpdateValue(modeName, ratingOutcomePartial)
	require.NoError(t, mr.Set(matchRatingKey(13005), "not-a-hash"))

	event := resultEvent(9301, mode, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN, teamA, teamB)
	outcome, err := ApplyBattleResult(svcCtx, event)
	require.Error(t, err)
	require.Equal(t, ratingOutcomePartial, outcome)
	require.Equal(t, partialBefore+1, metrics.RatingUpdateValue(modeName, ratingOutcomePartial))
	for _, pid := range teamA {
		require.InDelta(t, 1516, ratingOf(t, svcCtx, pid), 0.01)
		require.Equal(t, "1", mr.HGet(matchRatingKey(pid), ratingFieldGames))
	}
	require.InDelta(t, 1484, ratingOf(t, svcCtx, 13004), 0.01)
	require.InDelta(t, 1484, ratingOf(t, svcCtx, 13006), 0.01, "失败的人之后的成员也先写完,重试要补的人最少")
	marker, err := mr.Get(matchRatingAppliedKey(9301))
	require.NoError(t, err)
	state, delta, ok := decodeRatingAppliedMarker(marker)
	require.True(t, ok)
	require.Equal(t, ratingAppliedStateApplying, state)
	require.InDelta(t, 16, delta, 0.01, "标记里记的是赛前评分算出的 Δ_A")

	// 修好那个 key,消费者重试:只补 13005,其余人不变。
	mr.Del(matchRatingKey(13005))
	outcome, err = ApplyBattleResult(svcCtx, event)
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeApplied, outcome)
	sum := 0.0
	for _, pid := range append(append([]uint64(nil), teamA...), teamB...) {
		sum += ratingOf(t, svcCtx, pid)
		require.Equal(t, "1", mr.HGet(matchRatingKey(pid), ratingFieldGames), "无人被算两次 player=%d", pid)
	}
	for _, pid := range teamA {
		require.InDelta(t, 1516, ratingOf(t, svcCtx, pid), 0.01)
	}
	for _, pid := range teamB {
		require.InDelta(t, 1484, ratingOf(t, svcCtx, pid), 0.01)
	}
	require.InDelta(t, 6*1500, sum, 0.01, "Elo 零和")
	marker, err = mr.Get(matchRatingAppliedKey(9301))
	require.NoError(t, err)
	require.Equal(t, ratingAppliedStateDone, marker)
	ttl := mr.TTL(matchRatingAppliedKey(9301))
	require.Greater(t, ttl, 6*24*time.Hour)

	outcome, err = ApplyBattleResult(svcCtx, event)
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeDuplicate, outcome)
	require.Equal(t, "1", mr.HGet(matchRatingKey(13001), ratingFieldGames))

	// 滚动升级窗口:旧实例写的标记是实例 uuid,按旧语义"存在即已入账"。
	require.NoError(t, mr.Set(matchRatingAppliedKey(9302), "old-instance-uuid"))
	outcome, err = ApplyBattleResult(svcCtx, resultEvent(9302, mode, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN, teamA, teamB))
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeDuplicate, outcome)
	require.Equal(t, "1", mr.HGet(matchRatingKey(13001), ratingFieldGames))
}

// 同一玩家的两局(不同 battle_id,设计上会落在不同分区被两个实例并发消费)交错入账:
// 局 X 读完评分后暂停,局 Y 完整入账,再放行 X —— 两局的 ±16 都落账、局数 2,
// 不再是"读旧值 + 覆盖写"丢一局。
func TestApplyBattleResultConcurrentBattlesSamePlayerBothLand(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	const p, oppX, oppY = uint64(14001), uint64(14002), uint64(14003)
	const battleX, battleY = uint64(9401), uint64(9402)
	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	withRatingAfterLoadHook(t, func(battleId uint64) {
		if battleId != battleX {
			return
		}
		once.Do(func() { close(reached) })
		<-release
	})

	var wg sync.WaitGroup
	var outcomeX string
	var errX error
	wg.Add(1)
	go func() {
		defer wg.Done()
		outcomeX, errX = ApplyBattleResult(svcCtx, resultEvent(battleX, matchpb.MatchMode_MATCH_MODE_1V1,
			battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN, []uint64{p}, []uint64{oppX}))
	}()
	select {
	case <-reached:
	case <-time.After(3 * time.Second):
		t.Fatal("局 X 没有走到读评分后的注入点")
	}

	// 另一实例在 X 暂停期间把局 Y 完整入账。
	outcome, err := ApplyBattleResult(svcCtx, resultEvent(battleY, matchpb.MatchMode_MATCH_MODE_1V1,
		battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN, []uint64{p}, []uint64{oppY}))
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeApplied, outcome)
	require.InDelta(t, 1516, ratingOf(t, svcCtx, p), 0.01)

	close(release)
	wg.Wait()
	require.NoError(t, errX)
	require.Equal(t, ratingOutcomeApplied, outcomeX)
	require.InDelta(t, 1532, ratingOf(t, svcCtx, p), 0.01, "两局增量都落账")
	require.Equal(t, "2", mr.HGet(matchRatingKey(p), ratingFieldGames))
	require.InDelta(t, 1484, ratingOf(t, svcCtx, oppX), 0.01)
	require.InDelta(t, 1484, ratingOf(t, svcCtx, oppY), 0.01)
	recent := mr.HGet(matchRatingKey(p), ratingFieldRecentBattles)
	require.Contains(t, recent, "9401")
	require.Contains(t, recent, "9402")
}

// 回合打满:引擎对回合上限一律判 SIDE_B_WIN(PVE 规则),rated 模式 total_rounds ≥
// RatingDrawRoundCap 按平局 0.5 结算;未打满照胜负;按 config 覆盖的上限优先;0 关闭。
func TestApplyBattleResultRoundCapSettlesAsDraw(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	svcCtx.Config.RatingDrawRoundCap = 30
	svcCtx.Config.RatingDrawRoundCapByConfigId = map[string]uint32{"7": 300}
	mode := matchpb.MatchMode_MATCH_MODE_1V1
	modeName := mode.String()
	const high, low = uint64(15001), uint64(15002)
	drawBefore := metrics.RatingRoundCapDrawValue(modeName)

	// 1600 vs 1400,B(低分方)"胜"但回合打满 → 平局:高分方期望 0.76,失 8.31 分。
	setRating(t, mr, high, 1600)
	setRating(t, mr, low, 1400)
	expectedHigh := eloExpected(1600, 1400)
	drawDelta := eloK * (0.5 - expectedHigh)
	event := resultEvent(9501, mode, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN, []uint64{high}, []uint64{low})
	event.TotalRounds = 30
	outcome, err := ApplyBattleResult(svcCtx, event)
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeApplied, outcome)
	require.Less(t, drawDelta, 0.0)
	require.Greater(t, drawDelta, -eloK*expectedHigh, "平局失分少于真输")
	require.InDelta(t, 1600+drawDelta, ratingOf(t, svcCtx, high), 0.02)
	require.InDelta(t, 1400-drawDelta, ratingOf(t, svcCtx, low), 0.02)
	require.Equal(t, drawBefore+1, metrics.RatingRoundCapDrawValue(modeName))

	// 29 回合真输:按胜负全额。
	hBefore, lBefore := ratingOf(t, svcCtx, high), ratingOf(t, svcCtx, low)
	lossDelta := eloK * (0 - eloExpected(hBefore, lBefore))
	event = resultEvent(9502, mode, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN, []uint64{high}, []uint64{low})
	event.TotalRounds = 29
	outcome, err = ApplyBattleResult(svcCtx, event)
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeApplied, outcome)
	require.InDelta(t, hBefore+lossDelta, ratingOf(t, svcCtx, high), 0.02)
	require.Equal(t, drawBefore+1, metrics.RatingRoundCapDrawValue(modeName))

	// config 7 的上限是 300:30 回合不算打满。
	hBefore, lBefore = ratingOf(t, svcCtx, high), ratingOf(t, svcCtx, low)
	lossDelta = eloK * (0 - eloExpected(hBefore, lBefore))
	event = resultEvent(9503, mode, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN, []uint64{high}, []uint64{low})
	event.BattleConfigId = 7
	event.TotalRounds = 30
	outcome, err = ApplyBattleResult(svcCtx, event)
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeApplied, outcome)
	require.InDelta(t, hBefore+lossDelta, ratingOf(t, svcCtx, high), 0.02)
	require.Equal(t, drawBefore+1, metrics.RatingRoundCapDrawValue(modeName))

	// 关闭判定:30 回合照胜负。
	svcCtx.Config.RatingDrawRoundCap = 0
	svcCtx.Config.RatingDrawRoundCapByConfigId = nil
	hBefore, lBefore = ratingOf(t, svcCtx, high), ratingOf(t, svcCtx, low)
	lossDelta = eloK * (0 - eloExpected(hBefore, lBefore))
	event = resultEvent(9504, mode, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN, []uint64{high}, []uint64{low})
	event.TotalRounds = 30
	outcome, err = ApplyBattleResult(svcCtx, event)
	require.NoError(t, err)
	require.Equal(t, ratingOutcomeApplied, outcome)
	require.InDelta(t, hBefore+lossDelta, ratingOf(t, svcCtx, high), 0.02)
	require.Equal(t, drawBefore+1, metrics.RatingRoundCapDrawValue(modeName))
	require.False(t, math.IsNaN(ratingOf(t, svcCtx, high)))
}

// 分差 1100 > 容差上限 1000 的两人:等 60s(曲线已饱和)仍不配,饥饿 gauge 记 60;
// 等满 RatingToleranceMaxWaitSeconds(默认 90)后容差 ∞,成组;运维把上限调小到 300
// 时分差 350 的两人同样只等到兜底秒数。
func TestStarvedAnchorsMatchAfterMaxWait(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	groups := stubGather(t)
	var mu sync.Mutex
	starved := map[string]float64{}
	prev := setStarvedAnchorWaitFn
	setStarvedAnchorWaitFn = func(mode, config string, seconds float64) {
		mu.Lock()
		defer mu.Unlock()
		starved[mode+":"+config] = seconds
	}
	t.Cleanup(func() { setStarvedAnchorWaitFn = prev })
	gauge := func() float64 {
		mu.Lock()
		defer mu.Unlock()
		return starved[matchpb.MatchMode_MATCH_MODE_1V1.String()+":0"]
	}

	setPlayerLocation(t, mr, 16001, 1)
	setPlayerLocation(t, mr, 16002, 2)
	setRating(t, mr, 16001, 1500)
	setRating(t, mr, 16002, 2600)
	require.Zero(t, joinQueue1v1(t, svcCtx, 16001).ErrorCode)
	require.Zero(t, joinQueue1v1(t, svcCtx, 16002).ErrorCode)

	// 60s:两人容差都到 1000,分差 1100 仍不配;饥饿 gauge = 最久等待 60。
	setEnqueuedAgo(t, mr, 16001, 60*time.Second)
	setEnqueuedAgo(t, mr, 16002, 60*time.Second)
	runMatcherRound(context.Background(), svcCtx)
	expectNoGroup(t, groups)
	require.Equal(t, 60.0, gauge())
	list, err := mr.List(queue1v1)
	require.NoError(t, err)
	require.Equal(t, []string{"16001", "16002"}, list, "队列不动")

	// 89s:仍未到兜底。
	setEnqueuedAgo(t, mr, 16001, 89*time.Second)
	runMatcherRound(context.Background(), svcCtx)
	expectNoGroup(t, groups)
	require.Equal(t, 89.0, gauge())

	// 90s:锚点容差 ∞,成组;队列空,下一轮 gauge 归零。
	setEnqueuedAgo(t, mr, 16001, 90*time.Second)
	runMatcherRound(context.Background(), svcCtx)
	expectGroup(t, groups, []uint64{16001, 16002})
	assertQueueMirrorConsistent(t, mr, queue1v1)
	runMatcherRound(context.Background(), svcCtx)
	require.Equal(t, 0.0, gauge())

	// 上限调小 + 兜底调短:分差 350 > 300 的两人 20s 后成组。
	svcCtx.Config.RatingToleranceMax = 300
	svcCtx.Config.RatingToleranceMaxWaitSeconds = 20
	setPlayerLocation(t, mr, 16003, 1)
	setPlayerLocation(t, mr, 16004, 1)
	setRating(t, mr, 16003, 1500)
	setRating(t, mr, 16004, 1850)
	require.Zero(t, joinQueue1v1(t, svcCtx, 16003).ErrorCode)
	require.Zero(t, joinQueue1v1(t, svcCtx, 16004).ErrorCode)
	setEnqueuedAgo(t, mr, 16003, 19*time.Second)
	runMatcherRound(context.Background(), svcCtx)
	expectNoGroup(t, groups)
	setEnqueuedAgo(t, mr, 16003, 20*time.Second)
	runMatcherRound(context.Background(), svcCtx)
	expectGroup(t, groups, []uint64{16003, 16004})
	assertQueueMirrorConsistent(t, mr, queue1v1)

	// 曲线饱和秒数(默认 45s;上限 300 时 (300-100)/100×5 = 10s)。
	require.Equal(t, int64(10), ratingToleranceSaturationFor(svcCtx))
	svcCtx.Config.RatingToleranceMax = 0
	svcCtx.Config.RatingToleranceMaxWaitSeconds = 0 // 漏配按默认 90
	require.Equal(t, int64(45), ratingToleranceSaturationFor(svcCtx))
	require.True(t, math.IsInf(anchorToleranceFor(svcCtx, true, 90), 1))
	require.Equal(t, 1000.0, anchorToleranceFor(svcCtx, true, 89))
	require.True(t, math.IsInf(anchorToleranceFor(svcCtx, false, 0), 1))
}
