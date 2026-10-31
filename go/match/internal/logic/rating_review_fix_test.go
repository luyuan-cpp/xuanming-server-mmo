package logic

// 容差兜底(cross-zone-matchmaking.md §11.11):分差超过容差上限的两人等满兜底秒数后容差 ∞,不会永久饥饿。
// 撮合时怎么用这个容差见 global_match_test.go 与 poolgroup 包。

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAnchorToleranceFallsBackAfterMaxWait(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)

	// 默认曲线饱和于 1000,等满 90s 后容差 ∞(分差 1100 的两人从这一刻起能配上)。
	require.Equal(t, 1000.0, anchorToleranceFor(svcCtx, true, 60))
	require.Equal(t, 1000.0, anchorToleranceFor(svcCtx, true, 89))
	require.True(t, math.IsInf(anchorToleranceFor(svcCtx, true, 90), 1))

	// 非计分模式恒 ∞:PVE 组队不看评分,沿用等待序。
	require.True(t, math.IsInf(anchorToleranceFor(svcCtx, false, 0), 1))

	// 上限调小 + 兜底调短:分差 350 > 300 的两人 20s 后成组。
	svcCtx.Config.RatingToleranceMax = 300
	svcCtx.Config.RatingToleranceMaxWaitSeconds = 20
	require.Equal(t, 300.0, anchorToleranceFor(svcCtx, true, 19))
	require.True(t, math.IsInf(anchorToleranceFor(svcCtx, true, 20), 1))

	// 漏配按默认 90。
	svcCtx.Config.RatingToleranceMaxWaitSeconds = 0
	require.Equal(t, int64(90), ratingToleranceMaxWaitFor(svcCtx))
}
