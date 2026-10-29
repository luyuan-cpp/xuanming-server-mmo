package poolgroup

import (
	"math"
	"reflect"
	"testing"

	"match/internal/scope"
)

const (
	testNow     = uint64(1_000_000)
	regionEast  = uint32(1)
	regionWest  = uint32(2)
	crossWaitMs = uint64(30_000)
)

var testScope = scope.Schedule{GroupAfterMs: 10_000, GlobalAfterMs: 20_000}

func bothRegions() map[uint32]bool { return map[uint32]bool{regionEast: true, regionWest: true} }

// solo 构造一个单排单位：zone/大区、等待毫秒、首选机房、到各机房的 RTT。
func solo(id uint64, zone, group uint32, waitedMs uint64, preferred uint32, rtt map[uint32]uint32) Unit {
	enq := testNow - waitedMs
	return Unit{
		ID:         id,
		Players:    []uint64{id},
		Members:    []scope.Member{{Node: scope.Node{Zone: zone, Group: group}, EnqueuedMs: enq}},
		EnqueuedMs: enq,
		Preferred:  preferred,
		RTT:        rtt,
	}
}

func east() map[uint32]uint32 { return map[uint32]uint32{regionEast: 30, regionWest: 120} }
func west() map[uint32]uint32 { return map[uint32]uint32{regionEast: 120, regionWest: 30} }

func pvePolicy(target int) Policy {
	return Policy{Target: target, Scope: testScope, CrossRegionAfterMs: crossWaitMs}
}

func ids(units []Unit, g Group) []uint64 {
	out := make([]uint64, 0, len(g.Units))
	for _, i := range g.Units {
		out = append(out, units[i].ID)
	}
	return out
}

func TestSameZoneFormsImmediately(t *testing.T) {
	units := []Unit{solo(1, 5, 1, 0, regionEast, east()), solo(2, 5, 1, 0, regionEast, east())}
	groups := Form(units, bothRegions(), testNow, pvePolicy(2))
	if len(groups) != 1 || groups[0].Level != scope.LevelZone || groups[0].Region != regionEast || groups[0].Cross {
		t.Fatalf("同 zone 应立即本机房成组: %+v", groups)
	}
}

func TestOtherZoneWaitsForGroupLevel(t *testing.T) {
	// 同大区不同 zone，都没等满大区阈值：不成组。
	units := []Unit{solo(1, 5, 1, 5_000, regionEast, east()), solo(2, 7, 1, 5_000, regionEast, east())}
	if groups := Form(units, bothRegions(), testNow, pvePolicy(2)); len(groups) != 0 {
		t.Fatalf("未等满大区阈值不应跨 zone: %+v", groups)
	}
	// 都等满：按大区层成组。
	units = []Unit{solo(1, 5, 1, 10_000, regionEast, east()), solo(2, 7, 1, 10_000, regionEast, east())}
	groups := Form(units, bothRegions(), testNow, pvePolicy(2))
	if len(groups) != 1 || groups[0].Level != scope.LevelGroup {
		t.Fatalf("等满大区阈值应按大区层成组: %+v", groups)
	}
}

func TestOtherGroupWaitsForGlobalLevel(t *testing.T) {
	units := []Unit{solo(1, 5, 1, 15_000, regionEast, east()), solo(2, 205, 2, 15_000, regionEast, east())}
	if groups := Form(units, bothRegions(), testNow, pvePolicy(2)); len(groups) != 0 {
		t.Fatalf("未等满全服阈值不应跨大区: %+v", groups)
	}
	units = []Unit{solo(1, 5, 1, 20_000, regionEast, east()), solo(2, 205, 2, 20_000, regionEast, east())}
	groups := Form(units, bothRegions(), testNow, pvePolicy(2))
	if len(groups) != 1 || groups[0].Level != scope.LevelGlobal {
		t.Fatalf("等满全服阈值应按全服层成组: %+v", groups)
	}
}

func TestNarrowerLevelPreferred(t *testing.T) {
	// 锚点已允许全服，但同 zone 有人：先在 zone 层成组，不拉远处的人。
	units := []Unit{
		solo(1, 5, 1, 25_000, regionEast, east()),
		solo(2, 205, 2, 24_000, regionEast, east()), // 更早可用但更远
		solo(3, 5, 1, 1_000, regionEast, east()),    // 同 zone，刚入队
	}
	groups := Form(units, bothRegions(), testNow, pvePolicy(2))
	if len(groups) != 1 || !reflect.DeepEqual(ids(units, groups[0]), []uint64{1, 3}) || groups[0].Level != scope.LevelZone {
		t.Fatalf("应先在同 zone 成组: %+v", groups)
	}
}

func TestFreshPlayerNotDraggedWide(t *testing.T) {
	// 3 人档：锚点与另一 zone 的人都等满大区阈值，同 zone 新人刚入队。新人不能被拉进跨 zone 组。
	units := []Unit{
		solo(1, 5, 1, 12_000, regionEast, east()),
		solo(2, 5, 1, 0, regionEast, east()),
		solo(3, 7, 1, 12_000, regionEast, east()),
	}
	if groups := Form(units, bothRegions(), testNow, pvePolicy(3)); len(groups) != 0 {
		t.Fatalf("刚入队的人不应被拉进大区层组: %+v", groups)
	}
}

func TestUngroupedZoneSkipsGroupLevel(t *testing.T) {
	// 两个未登记大区的 zone：等满大区阈值也不成组，等满全服阈值才成组。
	units := []Unit{solo(1, 5, 0, 15_000, regionEast, east()), solo(2, 7, 0, 15_000, regionEast, east())}
	if groups := Form(units, bothRegions(), testNow, pvePolicy(2)); len(groups) != 0 {
		t.Fatalf("未登记大区的 zone 不应在大区层合并: %+v", groups)
	}
	units = []Unit{solo(1, 5, 0, 20_000, regionEast, east()), solo(2, 7, 0, 20_000, regionEast, east())}
	if groups := Form(units, bothRegions(), testNow, pvePolicy(2)); len(groups) != 1 {
		t.Fatalf("等满全服阈值应成组: %+v", groups)
	}
}

func TestCrossRegionNeedsAllMembersWaited(t *testing.T) {
	// 首选机房不同：本机房阶段凑不上；只有双方都等满跨机房阈值才跨机房。
	units := []Unit{solo(1, 5, 1, 31_000, regionEast, east()), solo(2, 5, 1, 10_000, regionWest, west())}
	if groups := Form(units, bothRegions(), testNow, pvePolicy(2)); len(groups) != 0 {
		t.Fatalf("对方未等满跨机房阈值不应跨机房: %+v", groups)
	}
	units = []Unit{solo(1, 5, 1, 31_000, regionEast, east()), solo(2, 5, 1, 30_000, regionWest, west())}
	groups := Form(units, bothRegions(), testNow, pvePolicy(2))
	// 东西两机房最大 RTT 与 RTT 之和都相等（120 / 150），按机房号取东。
	if len(groups) != 1 || !groups[0].Cross || groups[0].MaxRTT != 120 || groups[0].Region != regionEast || groups[0].Level != scope.LevelZone {
		t.Fatalf("双方等满应跨机房成组: %+v", groups)
	}
}

func TestCrossRegionPicksLowestMaxRTT(t *testing.T) {
	mid := map[uint32]uint32{regionEast: 60, regionWest: 60}
	units := []Unit{solo(1, 5, 1, 31_000, regionEast, east()), solo(2, 5, 1, 31_000, regionWest, mid)}
	// 东：max(30,60)=60；西：max(120,60)=120。应选东。
	groups := Form(units, bothRegions(), testNow, pvePolicy(2))
	if len(groups) != 1 || groups[0].Region != regionEast || groups[0].MaxRTT != 60 {
		t.Fatalf("应选组内最大 RTT 最小的机房: %+v", groups)
	}
}

func TestManyGroupsPerCall(t *testing.T) {
	var units []Unit
	for i := uint64(1); i <= 10; i++ {
		units = append(units, solo(i, 5, 1, 0, regionEast, east()))
	}
	groups := Form(units, bothRegions(), testNow, pvePolicy(2))
	if len(groups) != 5 {
		t.Fatalf("一次应切出 5 组, got %d", len(groups))
	}
	seen := map[uint64]bool{}
	for _, g := range groups {
		for _, i := range g.Units {
			if seen[units[i].ID] {
				t.Fatalf("玩家 %d 进了两组", units[i].ID)
			}
			seen[units[i].ID] = true
		}
	}
}

func TestPartyIsNotSplit(t *testing.T) {
	enq := testNow
	party := Unit{
		ID:      100,
		Players: []uint64{100, 101, 102},
		Members: []scope.Member{
			{Node: scope.Node{Zone: 5, Group: 1}, EnqueuedMs: enq},
			{Node: scope.Node{Zone: 5, Group: 1}, EnqueuedMs: enq},
			{Node: scope.Node{Zone: 5, Group: 1}, EnqueuedMs: enq},
		},
		EnqueuedMs: enq, Preferred: regionEast, RTT: east(),
	}
	units := []Unit{solo(1, 5, 1, 0, regionEast, east()), party, solo(2, 5, 1, 0, regionEast, east())}
	groups := Form(units, bothRegions(), testNow, pvePolicy(5))
	if len(groups) != 1 || len(groups[0].Units) != 3 {
		t.Fatalf("应为 2 个单排 + 1 支 3 人队: %+v", groups)
	}
	// 4 人档放不下 3 人队 + 2 个单排，只能 1 单排 + 队伍。
	groups = Form(units, bothRegions(), testNow, pvePolicy(4))
	if len(groups) != 1 || !reflect.DeepEqual(ids(units, groups[0]), []uint64{1, 100}) {
		t.Fatalf("4 人档应装 1 单排 + 3 人队: %+v", groups)
	}
}

func TestFullUnitPlacedWithoutWaiting(t *testing.T) {
	// 满员单位（单人档）首选机房无节点：立即放到可用机房，不等跨机房阈值。
	units := []Unit{solo(1, 5, 1, 0, regionEast, east())}
	groups := Form(units, map[uint32]bool{regionWest: true}, testNow, pvePolicy(1))
	if len(groups) != 1 || groups[0].Region != regionWest || !groups[0].Cross {
		t.Fatalf("满员单位应立即放到可用机房: %+v", groups)
	}
	groups = Form(units, bothRegions(), testNow, pvePolicy(1))
	if len(groups) != 1 || groups[0].Region != regionEast || groups[0].Cross {
		t.Fatalf("首选机房可用时应放首选: %+v", groups)
	}
}

func TestRatedPicksNearestWithinTolerance(t *testing.T) {
	policy := Policy{Target: 2, Scope: testScope, CrossRegionAfterMs: crossWaitMs, Rated: true,
		ToleranceCenti: func(uint64) float64 { return 10_000 }} // ±100 分
	a := solo(1, 5, 1, 0, regionEast, east())
	a.RatingCenti = 150_000
	far := solo(2, 5, 1, 0, regionEast, east())
	far.RatingCenti = 158_000
	near := solo(3, 5, 1, 0, regionEast, east())
	near.RatingCenti = 151_000
	units := []Unit{a, far, near}
	groups := Form(units, bothRegions(), testNow, policy)
	if len(groups) != 1 || !reflect.DeepEqual(ids(units, groups[0]), []uint64{1, 3}) {
		t.Fatalf("应按分差就近配对: %+v", groups)
	}
}

func TestRatedRangeBoundedByTolerance(t *testing.T) {
	// 5V5 简化为 3 人：锚点两侧各一人都在 ±tol 内，但三人极差超过 tol，不成组。
	policy := Policy{Target: 3, Scope: testScope, CrossRegionAfterMs: crossWaitMs, Rated: true,
		ToleranceCenti: func(uint64) float64 { return 10_000 }}
	a := solo(1, 5, 1, 0, regionEast, east())
	a.RatingCenti = 150_000
	low := solo(2, 5, 1, 0, regionEast, east())
	low.RatingCenti = 141_000
	high := solo(3, 5, 1, 0, regionEast, east())
	high.RatingCenti = 159_000
	if groups := Form([]Unit{a, low, high}, bothRegions(), testNow, policy); len(groups) != 0 {
		t.Fatalf("组内极差超过容差不应成组: %+v", groups)
	}
	inf := policy
	inf.ToleranceCenti = func(uint64) float64 { return math.Inf(1) }
	if groups := Form([]Unit{a, low, high}, bothRegions(), testNow, inf); len(groups) != 1 {
		t.Fatalf("容差不限时应成组: %+v", groups)
	}
}

func TestRatedRejectsParties(t *testing.T) {
	policy := Policy{Target: 2, Scope: testScope, Rated: true}
	party := solo(1, 5, 1, 0, regionEast, east())
	party.Players = []uint64{1, 2}
	party.Members = append(party.Members, party.Members[0])
	units := []Unit{party, solo(3, 5, 1, 0, regionEast, east())}
	for _, g := range Form(units, bothRegions(), testNow, policy) {
		for _, i := range g.Units {
			if i == 0 {
				t.Fatal("计分模式不接受预组队单位")
			}
		}
	}
}

func TestInvalidUnitsIgnored(t *testing.T) {
	noRTT := solo(1, 5, 1, 0, regionEast, nil)
	noZone := solo(2, 0, 1, 0, regionEast, east())
	ok1 := solo(3, 5, 1, 0, regionEast, east())
	ok2 := solo(4, 5, 1, 0, regionEast, east())
	units := []Unit{noRTT, noZone, ok1, ok2}
	groups := Form(units, bothRegions(), testNow, pvePolicy(2))
	if len(groups) != 1 || !reflect.DeepEqual(ids(units, groups[0]), []uint64{3, 4}) {
		t.Fatalf("无效单位应被跳过: %+v", groups)
	}
	if Form(units, nil, testNow, pvePolicy(2)) != nil {
		t.Fatal("没有可用机房应不成组")
	}
	if Form(units, bothRegions(), testNow, pvePolicy(0)) != nil || Form(units, bothRegions(), testNow, pvePolicy(MaxTarget+1)) != nil {
		t.Fatal("人数越界应不成组")
	}
}

func TestAnchorBudgetBoundsWork(t *testing.T) {
	var units []Unit
	for i := uint64(1); i <= 4; i++ {
		units = append(units, solo(i, 5, 1, 0, regionEast, east()))
	}
	policy := pvePolicy(2)
	policy.MaxAnchorsPerPass = 1
	if groups := Form(units, bothRegions(), testNow, policy); len(groups) != 1 {
		t.Fatalf("每阶段只试 1 个锚点时应只成 1 组, got %d", len(groups))
	}
}

func TestDeterministic(t *testing.T) {
	units := []Unit{
		solo(3, 5, 1, 12_000, regionEast, east()),
		solo(1, 7, 1, 12_000, regionEast, east()),
		solo(2, 5, 1, 0, regionEast, east()),
		solo(4, 7, 1, 0, regionEast, east()),
	}
	first := Form(units, bothRegions(), testNow, pvePolicy(2))
	for i := 0; i < 20; i++ {
		if again := Form(units, bothRegions(), testNow, pvePolicy(2)); !reflect.DeepEqual(first, again) {
			t.Fatalf("同输入结果不一致: %+v vs %+v", first, again)
		}
	}
	// 排队序 (入队时刻, ID) = ID1, ID3, ID2, ID4。同 zone 优先：1 与 4（zone 7）先成组，3 与 2（zone 5）后成组。
	if len(first) != 2 || first[0].Level != scope.LevelZone || first[1].Level != scope.LevelZone ||
		!reflect.DeepEqual(ids(units, first[0]), []uint64{1, 4}) || !reflect.DeepEqual(ids(units, first[1]), []uint64{3, 2}) {
		t.Fatalf("应按 zone 各成一组: %+v", first)
	}
}

func TestLocalRegionBeforeNarrowScope(t *testing.T) {
	// 区域内优先（D-16）先于范围树：同机房的跨 zone 组优先于同 zone 的跨机房组。
	units := []Unit{
		solo(1, 5, 1, 31_000, regionEast, east()),
		solo(2, 7, 1, 31_000, regionEast, east()),
		solo(3, 5, 1, 31_000, regionWest, west()),
	}
	groups := Form(units, bothRegions(), testNow, pvePolicy(2))
	if len(groups) != 1 || !reflect.DeepEqual(ids(units, groups[0]), []uint64{1, 2}) || groups[0].Cross || groups[0].Level != scope.LevelGroup {
		t.Fatalf("应先同机房成组: %+v", groups)
	}
}

func TestCrossRegionEscalatesScope(t *testing.T) {
	// 跨机房阶段同样按 zone → 大区 → 全服 依次放宽。
	units := []Unit{solo(1, 5, 1, 31_000, regionEast, east()), solo(2, 7, 1, 31_000, regionWest, west())}
	groups := Form(units, bothRegions(), testNow, pvePolicy(2))
	if len(groups) != 1 || !groups[0].Cross || groups[0].Level != scope.LevelGroup {
		t.Fatalf("同大区跨机房应为大区层: %+v", groups)
	}
	units = []Unit{solo(1, 5, 1, 31_000, regionEast, east()), solo(2, 205, 2, 31_000, regionWest, west())}
	groups = Form(units, bothRegions(), testNow, pvePolicy(2))
	if len(groups) != 1 || !groups[0].Cross || groups[0].Level != scope.LevelGlobal {
		t.Fatalf("跨大区跨机房应为全服层: %+v", groups)
	}
}

func TestRatedCrossRegion(t *testing.T) {
	policy := Policy{Target: 2, Scope: testScope, CrossRegionAfterMs: crossWaitMs, Rated: true,
		ToleranceCenti: func(uint64) float64 { return 10_000 }}
	mid := map[uint32]uint32{regionEast: 90, regionWest: 40}
	a := solo(1, 5, 1, 31_000, regionEast, east())
	a.RatingCenti = 150_000
	b := solo(2, 5, 1, 31_000, regionWest, mid)
	b.RatingCenti = 152_000
	units := []Unit{a, b}
	groups := Form(units, bothRegions(), testNow, policy)
	// 东：max(30,90)=90；西：max(120,40)=120。应选东。
	if len(groups) != 1 || !groups[0].Cross || groups[0].Region != regionEast || groups[0].MaxRTT != 90 {
		t.Fatalf("计分模式跨机房应选最大 RTT 最小的机房: %+v", groups)
	}
}

func TestFullPartyPlacedWithoutWaiting(t *testing.T) {
	// 跨大区的满员整队：首选不一致（0），刚入队；不分层、不等待，放到 RTT 最小的可用机房。
	party := Unit{
		ID:      100,
		Players: []uint64{100, 101, 102},
		Members: []scope.Member{
			{Node: scope.Node{Zone: 5, Group: 1}, EnqueuedMs: testNow},
			{Node: scope.Node{Zone: 205, Group: 2}, EnqueuedMs: testNow},
			{Node: scope.Node{Zone: 5, Group: 1}, EnqueuedMs: testNow},
		},
		EnqueuedMs: testNow, Preferred: 0, RTT: map[uint32]uint32{regionEast: 80, regionWest: 60},
	}
	groups := Form([]Unit{party}, bothRegions(), testNow, pvePolicy(3))
	if len(groups) != 1 || !reflect.DeepEqual(groups[0].Units, []int{0}) || groups[0].Level != scope.LevelGlobal ||
		!groups[0].Cross || groups[0].Region != regionWest {
		t.Fatalf("满员整队应立即放置: %+v", groups)
	}
	if groups := Form([]Unit{party}, map[uint32]bool{3: true}, testNow, pvePolicy(3)); len(groups) != 0 {
		t.Fatalf("没有可达的可用机房不应成组: %+v", groups)
	}
}

func TestMixedZonePartyStartsAtGroupLevel(t *testing.T) {
	// 同大区跨 zone 的 2 人队自身已在大区层，刚入队即可与同大区等满阈值的单排凑 3 人档。
	party := Unit{
		ID:      100,
		Players: []uint64{100, 101},
		Members: []scope.Member{
			{Node: scope.Node{Zone: 5, Group: 1}, EnqueuedMs: testNow},
			{Node: scope.Node{Zone: 7, Group: 1}, EnqueuedMs: testNow},
		},
		EnqueuedMs: testNow, Preferred: regionEast, RTT: east(),
	}
	units := []Unit{solo(1, 9, 1, 10_000, regionEast, east()), party}
	groups := Form(units, bothRegions(), testNow, pvePolicy(3))
	if len(groups) != 1 || groups[0].Level != scope.LevelGroup || !reflect.DeepEqual(ids(units, groups[0]), []uint64{1, 100}) {
		t.Fatalf("跨 zone 预组队应在大区层成组: %+v", groups)
	}
}

func TestSharedPlayerExcluded(t *testing.T) {
	// 玩家 1 同时出现在两个单位里（读页时状态不一致）：两个单位本轮都不参加。
	dup := solo(3, 5, 1, 0, regionEast, east())
	dup.Players = []uint64{1}
	units := []Unit{solo(1, 5, 1, 0, regionEast, east()), solo(2, 5, 1, 0, regionEast, east()), dup, solo(4, 5, 1, 0, regionEast, east())}
	groups := Form(units, bothRegions(), testNow, pvePolicy(2))
	if len(groups) != 1 || !reflect.DeepEqual(ids(units, groups[0]), []uint64{2, 4}) {
		t.Fatalf("共用玩家的单位应被排除: %+v", groups)
	}
}

func TestInvalidUnitBranches(t *testing.T) {
	ok := func() Unit { return solo(9, 5, 1, 0, regionEast, east()) }
	oversized := solo(1, 5, 1, 0, regionEast, east())
	oversized.Players = []uint64{1, 11, 12}
	oversized.Members = []scope.Member{oversized.Members[0], oversized.Members[0], oversized.Members[0]}
	mismatch := solo(2, 5, 1, 0, regionEast, east())
	mismatch.Members = append(mismatch.Members, mismatch.Members[0])
	zeroPlayer := solo(3, 5, 1, 0, regionEast, east())
	zeroPlayer.Players = []uint64{0}
	dupInside := solo(4, 5, 1, 0, regionEast, east())
	dupInside.Players = []uint64{4, 4}
	dupInside.Members = append(dupInside.Members, dupInside.Members[0])
	noPreferred := solo(5, 5, 1, 0, 0, east())
	for name, bad := range map[string]Unit{"人数超过一组": oversized, "成员数与玩家数不一致": mismatch, "玩家号为0": zeroPlayer, "单位内玩家重复": dupInside, "非满员且无首选机房": noPreferred} {
		if groups := Form([]Unit{bad, ok()}, bothRegions(), testNow, pvePolicy(2)); len(groups) != 0 {
			t.Errorf("%s: 不应成组: %+v", name, groups)
		}
	}
	rated := Policy{Target: 2, Scope: testScope, Rated: true, ToleranceCenti: func(uint64) float64 { return math.NaN() }}
	if groups := Form([]Unit{solo(1, 5, 1, 0, regionEast, east()), solo(2, 5, 1, 0, regionEast, east())}, bothRegions(), testNow, rated); len(groups) != 0 {
		t.Errorf("容差为 NaN 不应成组: %+v", groups)
	}
}

func TestCandidateLimitCountsAfterRegionFilter(t *testing.T) {
	// 候选上限 1：排在前面但到不了东区的 B 不能占掉东区的名额。
	const south, north = uint32(3), uint32(4)
	a := solo(1, 5, 1, 40_000, regionEast, map[uint32]uint32{regionEast: 30, regionWest: 100})
	b := solo(2, 5, 1, 35_000, south, map[uint32]uint32{south: 30})
	c := solo(3, 5, 1, 31_000, north, map[uint32]uint32{regionEast: 50})
	units := []Unit{a, b, c}
	policy := pvePolicy(2)
	policy.MaxCandidates = 1
	groups := Form(units, bothRegions(), testNow, policy)
	if len(groups) != 1 || !reflect.DeepEqual(ids(units, groups[0]), []uint64{1, 3}) || groups[0].Region != regionEast {
		t.Fatalf("应先按机房过滤再计候选上限: %+v", groups)
	}
}

func TestRejectedAnchorsDoNotUseBudget(t *testing.T) {
	// 首选机房没有节点的锚点不占预算：后面首选东区的两人仍能成组。
	var units []Unit
	for i := uint64(1); i <= 3; i++ {
		units = append(units, solo(i, 5, 1, 5_000, regionWest, west()))
	}
	units = append(units, solo(10, 5, 1, 0, regionEast, east()), solo(11, 5, 1, 0, regionEast, east()))
	policy := pvePolicy(2)
	policy.MaxAnchorsPerPass = 1
	groups := Form(units, map[uint32]bool{regionEast: true}, testNow, policy)
	if len(groups) != 1 || !reflect.DeepEqual(ids(units, groups[0]), []uint64{10, 11}) {
		t.Fatalf("被拒锚点不应耗尽预算: %+v", groups)
	}
}

func TestUnplaceableAnchorDoesNotUseCrossBudget(t *testing.T) {
	// 跨机房阶段：最早的票只能连到没有节点的机房，不能占掉预算让后面的跨机房组饿死。
	const south = uint32(3)
	stuck := solo(1, 5, 1, 40_000, south, map[uint32]uint32{south: 30})
	units := []Unit{stuck, solo(2, 5, 1, 35_000, regionEast, east()), solo(3, 5, 1, 35_000, regionWest, west())}
	policy := pvePolicy(2)
	policy.MaxAnchorsPerPass = 1
	groups := Form(units, bothRegions(), testNow, policy)
	if len(groups) != 1 || !reflect.DeepEqual(ids(units, groups[0]), []uint64{2, 3}) || !groups[0].Cross {
		t.Fatalf("无可分配机房的锚点不应占预算: %+v", groups)
	}
	// 满员单位版本：Target=1。
	full := []Unit{solo(1, 5, 1, 40_000, south, map[uint32]uint32{south: 30}), solo(2, 5, 1, 0, regionEast, east())}
	policy = pvePolicy(1)
	policy.MaxAnchorsPerPass = 1
	if groups := Form(full, bothRegions(), testNow, policy); len(groups) != 1 || !reflect.DeepEqual(ids(full, groups[0]), []uint64{2}) {
		t.Fatalf("无可分配机房的满员单位不应占预算: %+v", groups)
	}
}
