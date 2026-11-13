package poolgroup

import (
	"reflect"
	"testing"
)

// 热池拆分（docs/design/global-match-hierarchical-pools.md §5.2.1）：撮合段按 Policy.Passes 选跑哪些机房阶段。

// 机房段、子池段只跑本机房阶段：首选机房不同、只能跨机房成组的两个老单位不出组；同首选机房的照常成组。
func TestPassesLocalSkipsCrossStage(t *testing.T) {
	split := []Unit{solo(1, 5, 1, 40_000, regionEast, east()), solo(2, 5, 1, 40_000, regionWest, west())}
	both := Form(split, bothRegions(), testNow, pvePolicy(2))
	if len(both) != 1 || !both[0].Cross {
		t.Fatalf("两个阶段都跑时应在跨机房阶段成组: %+v", both)
	}
	local := pvePolicy(2)
	local.Passes = PassesLocal
	if groups := Form(split, bothRegions(), testNow, local); len(groups) != 0 {
		t.Fatalf("只跑本机房阶段不应跨机房成组: %+v", groups)
	}
	same := []Unit{solo(1, 5, 1, 0, regionEast, east()), solo(2, 5, 1, 0, regionEast, east())}
	groups := Form(same, bothRegions(), testNow, local)
	if len(groups) != 1 || groups[0].Cross || groups[0].Region != regionEast {
		t.Fatalf("同首选机房应在本机房阶段成组: %+v", groups)
	}
}

// 跨机房段先跑本机房阶段占位：能在首选机房凑成的老单位（1、2）不产出、也不进跨机房阶段，留给机房段提交；
// 只产出只能跨机房成的组（3 的首选机房 west 没有同伴，4 的首选机房不可分配）。
func TestPassesCrossEmitsOnlyCrossGroups(t *testing.T) {
	const regionDown = uint32(3) // 没有可分配节点的机房
	units := []Unit{
		solo(1, 5, 1, 40_000, regionEast, east()),
		solo(2, 5, 1, 40_000, regionEast, east()),
		solo(3, 5, 1, 40_000, regionWest, west()),
		solo(4, 5, 1, 40_000, regionDown, map[uint32]uint32{regionDown: 10, regionWest: 40}),
	}
	both := Form(units, bothRegions(), testNow, pvePolicy(2))
	if len(both) != 2 {
		t.Fatalf("两个阶段都跑时应成两组（本机房 1+2、跨机房 3+4）: %+v", both)
	}
	cross := pvePolicy(2)
	cross.Passes = PassesCross
	groups := Form(units, bothRegions(), testNow, cross)
	if len(groups) != 1 || !groups[0].Cross || groups[0].Region != regionWest || !reflect.DeepEqual(ids(units, groups[0]), []uint64{3, 4}) {
		t.Fatalf("跨机房段只应产出 3+4 的跨机房组: %+v", groups)
	}
}

// 零值 Passes 与拆分前相同：两个阶段的组都产出。
func TestPassesZeroValueRunsBothStages(t *testing.T) {
	if PassesBoth != 0 {
		t.Fatal("PassesBoth 必须是零值：入队即时成局与整池段不设 Passes")
	}
	units := []Unit{solo(1, 5, 1, 40_000, regionEast, east()), solo(2, 5, 1, 40_000, regionEast, east()),
		solo(3, 5, 1, 40_000, regionWest, west()), solo(4, 5, 1, 40_000, regionEast, east())}
	explicit := pvePolicy(2)
	explicit.Passes = PassesBoth
	if !reflect.DeepEqual(Form(units, bothRegions(), testNow, pvePolicy(2)), Form(units, bothRegions(), testNow, explicit)) {
		t.Fatal("显式 PassesBoth 应与零值完全相同")
	}
}
