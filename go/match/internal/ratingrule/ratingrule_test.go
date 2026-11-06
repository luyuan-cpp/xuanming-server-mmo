package ratingrule

import (
	"math"
	"strconv"
	"testing"

	"match/internal/config"

	pb "proto/match"
)

func TestRatedModes(t *testing.T) {
	for mode, want := range map[pb.MatchMode]bool{
		pb.MatchMode_MATCH_MODE_1V1:           true,
		pb.MatchMode_MATCH_MODE_5V5:           true,
		pb.MatchMode_MATCH_MODE_PVP_TEAM:      true,
		pb.MatchMode_MATCH_MODE_PVE_SOLO:      false,
		pb.MatchMode_MATCH_MODE_PVE_TEAM:      false,
		pb.MatchMode_MATCH_MODE_PVP_CHALLENGE: false,
		pb.MatchMode_MATCH_MODE_3V3:           false,
		pb.MatchMode_MATCH_MODE_UNSPECIFIED:   false,
	} {
		if Rated(mode) != want {
			t.Errorf("Rated(%s) = %v, want %v", mode, !want, want)
		}
	}
}

func TestFingerprintKeyListsRatedModesInOrder(t *testing.T) {
	want := strconv.Itoa(int(pb.MatchMode_MATCH_MODE_5V5)) + "," + strconv.Itoa(int(pb.MatchMode_MATCH_MODE_1V1)) + "," + strconv.Itoa(int(pb.MatchMode_MATCH_MODE_PVP_TEAM))
	if got := FingerprintKey(); got != want {
		t.Fatalf("FingerprintKey() = %q, want %q", got, want)
	}
	for i := 1; i < len(ratedModes); i++ {
		if ratedModes[i-1] >= ratedModes[i] {
			t.Fatalf("计分模式要按枚举值升序排列，指纹才与书写顺序无关: %v", ratedModes)
		}
	}
}

// 默认曲线与 logic 原来的 ratingToleranceFor 口径一致（rating_match_test.go 用同一组数）。
func TestCurveDefaults(t *testing.T) {
	var c Curve
	for wait, want := range map[int64]float64{0: 100, 4: 100, 5: 200, 10: 300, 45: 1000, 3600: 1000, -3: 100} {
		if got := c.Tolerance(wait); got != want {
			t.Errorf("Tolerance(%d) = %v, want %v", wait, got, want)
		}
	}
	if c.MaxWait() != 90 {
		t.Fatalf("终态兜底缺省 90 秒: %d", c.MaxWait())
	}
	if c.Anchor(89) != 1000 || !math.IsInf(c.Anchor(90), 1) {
		t.Fatalf("等满 90 秒后容差不限: %v / %v", c.Anchor(89), c.Anchor(90))
	}
	if got := c.Tolerance(math.MaxInt64); got != 1000 {
		t.Fatalf("等待极长也不能溢出: %v", got)
	}
}

func TestCurveCustom(t *testing.T) {
	c := Curve{Base: 50, StepSeconds: 10, StepDelta: 25, Max: 120, MaxWaitSeconds: 20}
	for wait, want := range map[int64]float64{9: 50, 10: 75, 100: 120} {
		if got := c.Tolerance(wait); got != want {
			t.Errorf("Tolerance(%d) = %v, want %v", wait, got, want)
		}
	}
	if c.Anchor(19) != 75 || !math.IsInf(c.Anchor(20), 1) {
		t.Fatalf("自定义终态兜底: %v / %v", c.Anchor(19), c.Anchor(20))
	}
}

func TestAnchorCentiUsesWholeSeconds(t *testing.T) {
	var c Curve
	if c.AnchorCenti(4_999) != 10_000 || c.AnchorCenti(5_000) != 20_000 {
		t.Fatalf("按整秒取容差: %v / %v", c.AnchorCenti(4_999), c.AnchorCenti(5_000))
	}
	if !math.IsInf(c.AnchorCenti(90_000), 1) || !math.IsInf(c.AnchorCenti(math.MaxUint64), 1) {
		t.Fatal("等满兜底时长后不限")
	}
}

func TestCurveOfReadsConfig(t *testing.T) {
	c := CurveOf(config.Config{RatingToleranceBase: 1, RatingToleranceStepSeconds: 2, RatingToleranceStepDelta: 3, RatingToleranceMax: 4, RatingToleranceMaxWaitSeconds: 5})
	if c != (Curve{Base: 1, StepSeconds: 2, StepDelta: 3, Max: 4, MaxWaitSeconds: 5}) {
		t.Fatalf("CurveOf 字段对应错误: %+v", c)
	}
}

func TestUnitCenti(t *testing.T) {
	cases := []struct {
		name    string
		members []int64
		bonus   int64
		want    int64
	}{
		{"单人就是本人评分，不加组队加成", []int64{163_200}, 2_500, 163_200},
		{"双排取平均再加一份加成", []int64{150_000, 160_000}, 2_500, 157_500},
		{"5 人队加四份", []int64{150_000, 150_000, 150_000, 150_000, 150_000}, 2_500, 160_000},
		{"平均四舍五入到 0.01 分", []int64{1, 2}, 0, 2},
		{"三人平均向下取整的情形", []int64{100, 100, 101}, 0, 100},
		{"没有成员", nil, 2_500, 0},
	}
	for _, c := range cases {
		if got := UnitCenti(c.members, c.bonus); got != c.want {
			t.Errorf("%s: UnitCenti(%v, %d) = %d, want %d", c.name, c.members, c.bonus, got, c.want)
		}
	}
}
