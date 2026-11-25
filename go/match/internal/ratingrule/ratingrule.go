// Package ratingrule 是计分匹配的规则：哪些模式计分、组内分差的容差怎样随等待放宽、整队的队伍分怎样算
// （docs/design/global-match-hierarchical-pools.md 开头「计分组排」与 §8）。
//
// 撮合（logic 给 poolgroup 准备队伍分与锚点容差）和提交点（authority.claimTx 的组内极差复核）用这同一份规则，
// 两边对同一组票必须得出同一个结论；参数进全局策略指纹，各副本一致。纯函数，不读 SQL、不看时钟。
package ratingrule

import (
	"math"
	"strconv"
	"strings"

	"match/internal/config"

	pb "proto/match"
)

// DefaultCenti 是没有评分记录的玩家的评分：1500.00 分 ×100。
const DefaultCenti int64 = 150_000

// ratedModes 是按评分凑组、按结果更新评分的队列模式，按枚举值升序。
var ratedModes = []pb.MatchMode{
	pb.MatchMode_MATCH_MODE_5V5,
	pb.MatchMode_MATCH_MODE_1V1,
	pb.MatchMode_MATCH_MODE_PVP_TEAM, // 人数档真人 PVP（2V2～5V5），2026-11-05 起计分
}

// Rated 报告该队列模式是否按评分凑组、按结果更新评分：1V1、5V5 与人数档真人 PVP（PVP_TEAM）。
// PVE 组队不看评分；切磋点名成局、PVE 单人即时开战不入队，也不计分。
func Rated(mode pb.MatchMode) bool {
	for _, rated := range ratedModes {
		if mode == rated {
			return true
		}
	}
	return false
}

// FingerprintKey 是计分模式集合在全局策略指纹里的片段（枚举值升序、逗号分隔）：
// 提交点按它决定复不复核组内极差，各副本必须一致。
func FingerprintKey() string {
	parts := make([]string, len(ratedModes))
	for i, mode := range ratedModes {
		parts[i] = strconv.Itoa(int(mode))
	}
	return strings.Join(parts, ",")
}

// Curve 是组内评分极差的容差曲线，单位是评分点（不是 ×100）：
//
//	tol = min(Max, Base + floor(wait / StepSeconds) × StepDelta)，等满 MaxWaitSeconds 后不限。
//
// 字段 ≤ 0（漏配、单测直接构造）取默认值 100 / 5 / 100 / 1000，终态兜底 90 秒。
// Base 取 100 而不是 0，是为了让两个都是 1500 的新号（以及几局内评分小幅分化的号）第一轮就能互配。
// 终态兜底不支持关闭：曲线上限之外没有兜底，分差超过 Max 的两人会一直等到票过期。
type Curve struct {
	Base           int64
	StepSeconds    int64
	StepDelta      int64
	Max            int64
	MaxWaitSeconds int64
}

// CurveOf 取配置里的容差曲线（config.Config 的 RatingTolerance* 字段）。
func CurveOf(c config.Config) Curve {
	return Curve{
		Base:           c.RatingToleranceBase,
		StepSeconds:    c.RatingToleranceStepSeconds,
		StepDelta:      c.RatingToleranceStepDelta,
		Max:            c.RatingToleranceMax,
		MaxWaitSeconds: c.RatingToleranceMaxWaitSeconds,
	}
}

// Tolerance 是等待 waitSeconds 秒时曲线给出的容差（评分点），不含终态兜底；负等待按 0。
func (c Curve) Tolerance(waitSeconds int64) float64 {
	base := orDefault(c.Base, 100)
	step := orDefault(c.StepSeconds, 5)
	delta := orDefault(c.StepDelta, 100)
	limit := orDefault(c.Max, 1000)
	if waitSeconds < 0 {
		waitSeconds = 0
	}
	// 乘法放在浮点里做：等待极长时整数会溢出。
	return math.Min(float64(limit), float64(base)+float64(waitSeconds/step)*float64(delta))
}

// MaxWait 是终态兜底秒数：等到这么久后容差不限。
func (c Curve) MaxWait() int64 {
	return orDefault(c.MaxWaitSeconds, 90)
}

// Anchor 是等待 waitSeconds 秒的锚点实际使用的容差（评分点）：等满 MaxWait 不限（+Inf），否则走曲线。
func (c Curve) Anchor(waitSeconds int64) float64 {
	if waitSeconds >= c.MaxWait() {
		return math.Inf(1)
	}
	return c.Tolerance(waitSeconds)
}

// AnchorCenti 是 Anchor 的 ×100 形态，入参为毫秒（按整秒取整）。撮合（poolgroup.Policy.ToleranceCenti）
// 与提交点复核共用；曲线随等待单调不减，提交点用组内最长等待算，只会比撮合时更宽。
func (c Curve) AnchorCenti(waitMs uint64) float64 {
	seconds := waitMs / 1000
	if seconds > math.MaxInt64 {
		return math.Inf(1)
	}
	return c.Anchor(int64(seconds)) * 100
}

// UnitCenti 是一个撮合单位的队伍分（×100）：成员评分的平均（四舍五入到 0.01 分）加组队加成
// bonusPerMemberCenti ×（人数 − 1）。单人就是本人评分；成员为空返回 0。
// 队伍分只用于凑人、分边和提交点的极差复核，Elo 结算仍按成员原分。
func UnitCenti(members []int64, bonusPerMemberCenti int64) int64 {
	if len(members) == 0 {
		return 0
	}
	var sum int64
	for _, rating := range members {
		sum += rating
	}
	average := int64(math.Round(float64(sum) / float64(len(members))))
	return average + bonusPerMemberCenti*int64(len(members)-1)
}

func orDefault(value, fallback int64) int64 {
	if value <= 0 {
		return fallback
	}
	return value
}

// RankedCurve 是论剑排位(PVP_TEAM)硬上限分差曲线:默认 0-30s 为 100 分,30-90s 为 200 分,
// 90s 及以上封顶 300 分,永不退化为 +Inf(docs/design/ranked-play-season-ui-candidate.md §2.3)。
type RankedCurve struct {
	Tier1Seconds uint32
	Tier1Points  uint32
	Tier2Seconds uint32
	Tier2Points  uint32
	Tier3Points  uint32
}

// DefaultRankedCurve 返回论剑首发 V1 默认硬上限分差窗口(30s/100, 90s/200, 300)。
func DefaultRankedCurve() RankedCurve {
	return RankedCurve{
		Tier1Seconds: 30,
		Tier1Points:  100,
		Tier2Seconds: 90,
		Tier2Points:  200,
		Tier3Points:  300,
	}
}

func (c RankedCurve) normalized() RankedCurve {
	d := DefaultRankedCurve()
	if c.Tier1Seconds == 0 || c.Tier2Seconds <= c.Tier1Seconds || c.Tier1Points == 0 || c.Tier2Points < c.Tier1Points || c.Tier3Points < c.Tier2Points {
		return d
	}
	return c
}

// SpreadPoints 返回等待 waitSeconds 秒时允许的最大排位分差(评分点,硬上限封顶)。
func (c RankedCurve) SpreadPoints(waitSeconds int64) uint32 {
	n := c.normalized()
	if waitSeconds < 0 {
		waitSeconds = 0
	}
	if uint64(waitSeconds) < uint64(n.Tier1Seconds) {
		return n.Tier1Points
	}
	if uint64(waitSeconds) < uint64(n.Tier2Seconds) {
		return n.Tier2Points
	}
	return n.Tier3Points
}

// Anchor 返回等待 waitSeconds 秒时允许的最大排位分差(评分点,永不返回 +Inf)。
func (c RankedCurve) Anchor(waitSeconds int64) float64 {
	return float64(c.SpreadPoints(waitSeconds))
}

// AnchorCenti 返回等待 waitMs 毫秒时允许的最大排位分差(×100,永不返回 +Inf)。
func (c RankedCurve) AnchorCenti(waitMs uint64) float64 {
	seconds := waitMs / 1000
	if seconds > math.MaxInt64 {
		seconds = math.MaxInt64
	}
	return float64(c.SpreadPoints(int64(seconds))) * 100
}
