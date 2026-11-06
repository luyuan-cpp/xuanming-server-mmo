package poolgroup

import (
	"math"
	"reflect"
	"testing"
)

// 分边模式的选人与分边（pvp_units.go），规则见 docs/design/global-match-hierarchical-pools.md 开头「计分组排」。

func sizedUnits(sizes ...int) []SideUnit {
	units := make([]SideUnit, len(sizes))
	for i, size := range sizes {
		units[i] = SideUnit{Size: size}
	}
	return units
}

func ratedSolos(ratings ...int64) []SideUnit {
	units := make([]SideUnit, len(ratings))
	for i, rating := range ratings {
		units[i] = SideUnit{Size: 1, RatingCenti: rating}
	}
	return units
}

// sidePeople 按分边结果统计两侧人数。
func sidePeople(units []SideUnit, indices []int, sides []uint32) [2]int {
	var people [2]int
	for i, index := range indices {
		people[sides[i]] += units[index].Size
	}
	return people
}

func TestPickSidesFullTeamsFaceEachOther(t *testing.T) {
	for size := 1; size <= 5; size++ {
		indices, sides := pickSides(sizedUnits(size, size), SideRule{PerSide: size})
		if !reflect.DeepEqual(indices, []int{0, 1}) || !reflect.DeepEqual(sides, []uint32{0, 1}) {
			t.Fatalf("%dv%d 两支满侧队伍必须分在两边: indices=%v sides=%v", size, size, indices, sides)
		}
	}
}

func TestPickSidesFillsPartiesWithSolos(t *testing.T) {
	// 5V5：我方 3 人队 + 2 人队，对方 4 人队 + 单人。
	units := sizedUnits(3, 2, 4, 1)
	indices, sides := pickSides(units, SideRule{PerSide: 5, GapStepMs: 10_000})
	if !reflect.DeepEqual(indices, []int{0, 1, 2, 3}) || !reflect.DeepEqual(sides, []uint32{0, 0, 1, 1}) {
		t.Fatalf("应为 3+2 打 4+1: indices=%v sides=%v", indices, sides)
	}
}

func TestPickSidesSkipsUnplaceableUnits(t *testing.T) {
	indices, sides := pickSides(sizedUnits(3, 4, 3), SideRule{PerSide: 3})
	if !reflect.DeepEqual(indices, []int{0, 2}) || !reflect.DeepEqual(sides, []uint32{0, 1}) {
		t.Fatalf("放不下的单位不能挡住后面能成局的队伍: %v %v", indices, sides)
	}
}

func TestPickSidesWaitsInsteadOfSplittingOrInventing(t *testing.T) {
	for _, input := range []struct {
		sizes   []int
		perSide int
	}{
		{[]int{5}, 5}, {[]int{3, 3, 2}, 4}, {[]int{2, 2}, 3},
		{nil, 1}, {[]int{0, 1}, 1}, {[]int{1, 1}, 0}, {[]int{1, 1}, 6},
	} {
		if indices, sides := pickSides(sizedUnits(input.sizes...), SideRule{PerSide: input.perSide}); indices != nil || sides != nil {
			t.Fatalf("凑不成两个完整的一侧时必须继续等: %v perSide=%d got=%v/%v", input.sizes, input.perSide, indices, sides)
		}
	}
}

func TestPickSidesPrefersEarliestCompletion(t *testing.T) {
	// 2V2：单人锚点后面依次是双排、单人、单人。双排 + 第一个单人就能成局，不等第三个单人。
	units := sizedUnits(1, 2, 1, 1)
	indices, sides := pickSides(units, SideRule{PerSide: 2})
	if !reflect.DeepEqual(indices, []int{0, 1, 2}) || !reflect.DeepEqual(sides, []uint32{0, 1, 0}) {
		t.Fatalf("应先来先成局: indices=%v sides=%v", indices, sides)
	}
}

func TestPickSidesPremadeGapWaitsForWeakerSide(t *testing.T) {
	// 5V5：5 人车队等了一分钟，5 个单人刚入队。车队差 4，要单人那一侧每人都等满 3 个步长。
	build := func(soloWaitMs uint64) []SideUnit {
		units := []SideUnit{{Size: 1, WaitMs: soloWaitMs}, {Size: 5, WaitMs: 60_000}}
		for i := 0; i < 4; i++ {
			units = append(units, SideUnit{Size: 1, WaitMs: soloWaitMs})
		}
		return units
	}
	rule := SideRule{PerSide: 5, GapStepMs: 10_000}
	if indices, _ := pickSides(build(0), rule); indices != nil {
		t.Fatalf("刚入队的单人不应被拉去打 5 人车队，车队等得再久也不行: %v", indices)
	}
	if indices, _ := pickSides(build(29_999), rule); indices != nil {
		t.Fatalf("单人只等了不到 3 个步长，还不能接受车队差 4: %v", indices)
	}
	indices, sides := pickSides(build(30_000), rule)
	if len(indices) != 6 || sidePeople(build(30_000), indices, sides) != [2]int{5, 5} {
		t.Fatalf("单人都等满 3 个步长后应成局: indices=%v sides=%v", indices, sides)
	}
	if indices, _ := pickSides(build(0), SideRule{PerSide: 5}); len(indices) != 6 {
		t.Fatalf("步长为 0 时不限车队差: %v", indices)
	}
}

func TestPickSidesSmallGapNeedsNoWait(t *testing.T) {
	// 3V3：双排 + 单人打 3 个单人，车队差 1，立即成局。
	units := []SideUnit{{Size: 2}, {Size: 1}, {Size: 1}, {Size: 1}, {Size: 1}}
	if indices, _ := pickSides(units, SideRule{PerSide: 3, GapStepMs: 10_000}); len(indices) != 5 {
		t.Fatalf("车队差不超过 1 应立即成局: %v", indices)
	}
	// 3 人队打 3 个单人，车队差 2：单人等满 1 个步长才行。
	fresh := []SideUnit{{Size: 3}, {Size: 1}, {Size: 1}, {Size: 1}}
	if indices, _ := pickSides(fresh, SideRule{PerSide: 3, GapStepMs: 10_000}); indices != nil {
		t.Fatalf("车队差 2 要单人等够: %v", indices)
	}
	waited := []SideUnit{{Size: 3}, {Size: 1, WaitMs: 10_000}, {Size: 1, WaitMs: 10_000}, {Size: 1, WaitMs: 10_000}}
	if indices, _ := pickSides(waited, SideRule{PerSide: 3, GapStepMs: 10_000}); len(indices) != 4 {
		t.Fatalf("单人等满 1 个步长后应能打 3 人队: %v", indices)
	}
}

func TestPickSidesSplitsDuosAcrossSides(t *testing.T) {
	// 5V5：两支双排加 6 个单人。两支双排各在一侧（车队差 0），而不是同侧打 5 个单人（车队差 1）。
	units := sizedUnits(2, 2, 1, 1, 1, 1, 1, 1)
	indices, sides := pickSides(units, SideRule{PerSide: 5})
	if len(indices) != 8 || sides[0] == sides[1] || sidePeople(units, indices, sides) != [2]int{5, 5} {
		t.Fatalf("两支双排应分在两侧: indices=%v sides=%v", indices, sides)
	}
	if !reflect.DeepEqual(sides, []uint32{0, 1, 1, 1, 1, 0, 0, 0}) {
		t.Fatalf("同样好的分法取掩码最小的，结果确定: %v", sides)
	}
}

func TestPickSidesRatedBalancesTotals(t *testing.T) {
	// 2V2：锚点 1500，候选 1600 / 1400 / 1500，容差 ±200。1500+1500 打 1600+1400，两侧总分相等。
	units := ratedSolos(150_000, 160_000, 140_000, 150_000)
	indices, sides := pickSides(units, SideRule{PerSide: 2, Rated: true, ToleranceCenti: 20_000})
	if !reflect.DeepEqual(indices, []int{0, 1, 2, 3}) || !reflect.DeepEqual(sides, []uint32{0, 1, 1, 0}) {
		t.Fatalf("应按总分平衡分边: indices=%v sides=%v", indices, sides)
	}
}

func TestPickSidesRatedWindowBoundsSpread(t *testing.T) {
	// 容差 ±100：1400 与 1600 不能同局；只剩 1500 / 1600 / 1500 一侧的窗口时人不够，继续等。
	rule := SideRule{PerSide: 2, Rated: true, ToleranceCenti: 10_000}
	units := ratedSolos(150_000, 160_000, 140_000, 150_000)
	if indices, _ := pickSides(units, rule); indices != nil {
		t.Fatalf("组内极差超过容差不应成局: %v", indices)
	}
	// 再来一个 1550：窗口 [1500, 1600] 里够 4 人，1400 不进组。
	units = append(units, SideUnit{Size: 1, RatingCenti: 155_000})
	indices, sides := pickSides(units, rule)
	if !reflect.DeepEqual(indices, []int{0, 1, 3, 4}) || !reflect.DeepEqual(sides, []uint32{0, 1, 1, 0}) {
		t.Fatalf("应在 [1500, 1600] 窗口里成局并平衡分边: indices=%v sides=%v", indices, sides)
	}
	if indices, _ := pickSides(units, SideRule{PerSide: 2, Rated: true, ToleranceCenti: math.Inf(1)}); len(indices) != 4 {
		t.Fatalf("容差不限时应成局: %v", indices)
	}
}

func TestPickSidesRatedKeepsPartyTogether(t *testing.T) {
	// 2V2 计分：双排（队伍分已含组队加成）打两个单人。
	units := []SideUnit{{Size: 2, RatingCenti: 152_500}, {Size: 1, RatingCenti: 150_000}, {Size: 1, RatingCenti: 150_000}}
	indices, sides := pickSides(units, SideRule{PerSide: 2, Rated: true, ToleranceCenti: 10_000})
	if !reflect.DeepEqual(indices, []int{0, 1, 2}) || !reflect.DeepEqual(sides, []uint32{0, 1, 1}) {
		t.Fatalf("双排整体在一侧: indices=%v sides=%v", indices, sides)
	}
}

func TestPickSidesRejectsInvalidTolerance(t *testing.T) {
	units := ratedSolos(150_000, 150_000)
	for _, tolerance := range []float64{math.NaN(), -1} {
		if indices, _ := pickSides(units, SideRule{PerSide: 1, Rated: true, ToleranceCenti: tolerance}); indices != nil {
			t.Fatalf("容差 %v 不应成局: %v", tolerance, indices)
		}
	}
}

func TestCompositionsForCountsPartitions(t *testing.T) {
	// 5V5 单人锚点：其余 9 人按每份不超过 5 拆分，共 23 种；5 人队锚点：其余 5 人共 7 种。
	for _, c := range []struct{ perSide, anchor, want int }{{5, 1, 23}, {5, 5, 7}, {2, 1, 2}, {1, 1, 1}} {
		got := compositionsFor(c.perSide, c.anchor)
		if len(got) != c.want {
			t.Fatalf("perSide=%d anchor=%d: 应有 %d 种组成，实际 %d", c.perSide, c.anchor, c.want, len(got))
		}
		seen := map[[maxPerSide + 1]int]bool{}
		for _, need := range got {
			total := 0
			for size, count := range need {
				total += size * count
				if count > 0 && size > c.perSide {
					t.Fatalf("组成里不能有超过一侧人数的份: %v", need)
				}
			}
			if total != 2*c.perSide-c.anchor || seen[need] {
				t.Fatalf("组成之和应为 %d 且不重复: %v", 2*c.perSide-c.anchor, need)
			}
			seen[need] = true
		}
	}
}

// 以下经 Form 走完整的撮合流程（范围树、机房、锚点顺序）。

func TestFormRatedSidesAcceptsParties(t *testing.T) {
	policy := Policy{Target: 4, Scope: testScope, CrossRegionAfterMs: crossWaitMs, Sides: true, Rated: true,
		ToleranceCenti: func(uint64) float64 { return 10_000 }}
	duo := party(100, 5, 1, 1_000, 100, 101)
	duo.RatingCenti = 150_000
	a, b := solo(1, 5, 1, 0, regionEast, east()), solo(2, 5, 1, 0, regionEast, east())
	a.RatingCenti, b.RatingCenti = 150_000, 150_000
	units := []Unit{duo, a, b}
	groups := Form(units, bothRegions(), testNow, policy)
	if len(groups) != 1 || !reflect.DeepEqual(groups[0].Units, []int{0, 1, 2}) || !reflect.DeepEqual(groups[0].Sides, []uint32{0, 1, 1}) {
		t.Fatalf("计分分边模式应收整队，双排打两个单人: %+v", groups)
	}
}

func TestFormRatedSidesFiltersByTolerance(t *testing.T) {
	policy := Policy{Target: 2, Scope: testScope, CrossRegionAfterMs: crossWaitMs, Sides: true, Rated: true,
		ToleranceCenti: func(uint64) float64 { return 10_000 }}
	a, far := solo(1, 5, 1, 0, regionEast, east()), solo(2, 5, 1, 0, regionEast, east())
	a.RatingCenti, far.RatingCenti = 150_000, 170_000
	if groups := Form([]Unit{a, far}, bothRegions(), testNow, policy); len(groups) != 0 {
		t.Fatalf("分差超过容差的两人不应成局: %+v", groups)
	}
}

func TestFormSidesPremadeGapUsesUnitWait(t *testing.T) {
	// 3V3：3 人队打 3 个单人（车队差 2），步长 10 秒。单人刚入队时不成局，等满 10 秒后成局。
	policy := sidesPolicy(3)
	policy.PremadeGapStepMs = 10_000
	trio := party(100, 5, 1, 0, 100, 101, 102)
	fresh := []Unit{trio, solo(1, 5, 1, 0, regionEast, east()), solo(2, 5, 1, 0, regionEast, east()), solo(3, 5, 1, 0, regionEast, east())}
	if groups := Form(fresh, bothRegions(), testNow, policy); len(groups) != 0 {
		t.Fatalf("单人没等够不应打 3 人队: %+v", groups)
	}
	waited := []Unit{trio, solo(1, 5, 1, 10_000, regionEast, east()), solo(2, 5, 1, 10_000, regionEast, east()), solo(3, 5, 1, 10_000, regionEast, east())}
	groups := Form(waited, bothRegions(), testNow, policy)
	if len(groups) != 1 || len(groups[0].Units) != 4 {
		t.Fatalf("单人等满一个步长后应成局: %+v", groups)
	}
	var people [2]int
	for i, u := range groups[0].Units {
		people[groups[0].Sides[i]] += len(waited[u].Players)
	}
	if people != [2]int{3, 3} {
		t.Fatalf("两侧各 3 人: %v", people)
	}
}
