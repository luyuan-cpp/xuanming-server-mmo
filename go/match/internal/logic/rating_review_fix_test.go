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

	matchpb "proto/match"

	"github.com/stretchr/testify/require"
)

// 分差 1100 > 容差上限 1000 的两人:等 60s(曲线已饱和)仍不配,饥饿 gauge 记 60;
// 等满 RatingToleranceMaxWaitSeconds(默认 90)后容差 ∞,成组;运维把上限调小到 300
// 时分差 350 的两人同样只等到兜底秒数。
func TestStarvedAnchorsMatchAfterMaxWait(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	groups := stubGather(t)
	var mu sync.Mutex
	starved := map[string]float64{}
	prev := setStarvedAnchorWaitFn
	setStarvedAnchorWaitFn = func(mode, config, _ string, seconds float64) {
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
