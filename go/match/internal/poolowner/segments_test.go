package poolowner

import (
	"reflect"
	"strings"
	"testing"

	"match/internal/catalog"

	matchpb "proto/match"
)

// 撮合段的枚举、归属键与读页区间（docs/design/global-match-hierarchical-pools.md §5.2.1 第 1～3 条）。

var (
	soloPool = catalog.Pool{Mode: matchpb.MatchMode_MATCH_MODE_PVE_SOLO, ConfigId: 1, TeamSize: 1}
	teamOne  = catalog.Pool{Mode: matchpb.MatchMode_MATCH_MODE_PVE_TEAM, ConfigId: 1, TeamSize: 1}
	teamFive = catalog.Pool{Mode: matchpb.MatchMode_MATCH_MODE_PVE_TEAM, ConfigId: 1, TeamSize: 5}
	duel     = catalog.Pool{Mode: matchpb.MatchMode_MATCH_MODE_1V1, ConfigId: 1, TeamSize: 2}
	pvpOne   = catalog.Pool{Mode: matchpb.MatchMode_MATCH_MODE_PVP_TEAM, ConfigId: 1, TeamSize: 1}
)

func fourSubPools(catalog.Pool) uint32 { return 4 }

// 每组只有 1 人的池不拆：配了子池、有几个机房都只出一个整池段。每侧 1 人的 PVP_TEAM 一局 2 人，照样拆。
func TestSegmentsKeepSinglePlayerPoolsWhole(t *testing.T) {
	got := Segments([]catalog.Pool{soloPool, teamOne}, []uint32{1, 2}, fourSubPools)
	want := []Segment{{Pool: soloPool, Role: RolePool}, {Pool: teamOne, Role: RolePool}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("每组 1 人的池应各一个整池段:\n got %+v\nwant %+v", got, want)
	}
	if Split(soloPool) || Split(teamOne) || !Split(pvpOne) || !Split(duel) {
		t.Fatal("Split 应只按一局人数判：1 人不拆，2 人及以上拆")
	}
}

// 多人池：每个机房一个机房段，配了 K 的模板再跟 K 个子池段，最后一个跨机房段；机房去重、去 0、按号排序。
func TestSegmentsSplitByRegionThenSubPool(t *testing.T) {
	got := Segments([]catalog.Pool{teamFive, duel}, []uint32{2, 0, 1, 2}, func(pool catalog.Pool) uint32 {
		if pool == teamFive {
			return 2
		}
		return 0 // 没配：不拆
	})
	want := []Segment{
		{Pool: teamFive, Role: RoleRegion, Region: 1, SubPools: 2},
		{Pool: teamFive, Role: RoleSub, Region: 1, SubPools: 2, Sub: 0},
		{Pool: teamFive, Role: RoleSub, Region: 1, SubPools: 2, Sub: 1},
		{Pool: teamFive, Role: RoleRegion, Region: 2, SubPools: 2},
		{Pool: teamFive, Role: RoleSub, Region: 2, SubPools: 2, Sub: 0},
		{Pool: teamFive, Role: RoleSub, Region: 2, SubPools: 2, Sub: 1},
		{Pool: teamFive, Role: RoleCross},
		{Pool: duel, Role: RoleRegion, Region: 1, SubPools: 1},
		{Pool: duel, Role: RoleRegion, Region: 2, SubPools: 1},
		{Pool: duel, Role: RoleCross},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("段枚举不符:\n got %+v\nwant %+v", got, want)
	}
	if !reflect.DeepEqual(got, Segments([]catalog.Pool{teamFive, duel}, []uint32{1, 2}, func(pool catalog.Pool) uint32 {
		if pool == teamFive {
			return 2
		}
		return 1
	})) {
		t.Fatal("机房列表的顺序与重复不应影响结果；子池数 0 与 1 同样是不拆")
	}
}

// 还没有任何 battle 节点注册（机房列表为空）时，多人池只剩跨机房段；nil 子池函数等于全部不拆。
func TestSegmentsWithoutRegionsLeaveOnlyCross(t *testing.T) {
	got := Segments([]catalog.Pool{duel}, nil, nil)
	if want := []Segment{{Pool: duel, Role: RoleCross}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("没有机房时应只有跨机房段: %+v", got)
	}
}

// 归属键两两不同、都以 pool: 开头（与后台工作键分开）；几个键的形状固定：各副本按同一字符串算归属，改形状等于全体换主。
// 机房段的键不含子池数：改 K 不会让机房段换主。
func TestSegmentKeysAreDistinctAndStable(t *testing.T) {
	seen := map[string]Segment{}
	for _, segment := range Segments([]catalog.Pool{soloPool, teamOne, teamFive, duel, pvpOne}, []uint32{1, 2, 3}, func(catalog.Pool) uint32 { return 3 }) {
		key := segment.Key()
		if !strings.HasPrefix(key, "pool:") {
			t.Fatalf("段 %+v 的键 %q 应以 pool: 开头", segment, key)
		}
		if previous, duplicate := seen[key]; duplicate {
			t.Fatalf("段 %+v 与 %+v 撞键 %q", previous, segment, key)
		}
		seen[key] = segment
	}
	for want, segment := range map[string]Segment{
		"pool:5:1:5":                {Pool: teamFive, Role: RolePool},
		"pool:5:1:5:region:2":       {Pool: teamFive, Role: RoleRegion, Region: 2, SubPools: 3},
		"pool:5:1:5:region:2:sub:1": {Pool: teamFive, Role: RoleSub, Region: 2, SubPools: 3, Sub: 1},
		"pool:5:1:5:cross":          {Pool: teamFive, Role: RoleCross},
		"pool:3:1:2:region:7":       {Pool: duel, Role: RoleRegion, Region: 7, SubPools: 1},
	} {
		if got := segment.Key(); got != want {
			t.Fatalf("段 %+v 的键应为 %q, got %q", segment, want, got)
		}
	}
	if (Segment{Pool: teamFive, Role: RoleRegion, Region: 2, SubPools: 1}).Key() != (Segment{Pool: teamFive, Role: RoleRegion, Region: 2, SubPools: 8}).Key() {
		t.Fatal("机房段的键不应随子池数变化")
	}
}

// 读页区间（§5.2.1 第 3 条）：机房段配了子池只读等满合并时长的，子池段只读不满的，跨机房段只读等满跨机房阈值的。
// 时钟还没走过阈值时，按「0 = 不限」会读反，所以机房段（配了子池）与跨机房段本轮跳过；子池段读全部（都还在子池阶段）。
func TestSegmentWindowFollowsRole(t *testing.T) {
	const now, merge, cross = uint64(1_000_000), uint64(5_000), uint64(30_000)
	for _, c := range []struct {
		name         string
		segment      Segment
		after, until uint64
	}{
		{"整池段不限", Segment{Pool: teamOne, Role: RolePool}, 0, 0},
		{"不拆子池的机房段不限", Segment{Pool: duel, Role: RoleRegion, Region: 1, SubPools: 1}, 0, 0},
		{"拆了子池的机房段", Segment{Pool: teamFive, Role: RoleRegion, Region: 1, SubPools: 4}, 0, now - merge},
		{"子池段", Segment{Pool: teamFive, Role: RoleSub, Region: 1, SubPools: 4, Sub: 2}, now - merge, 0},
		{"跨机房段", Segment{Pool: teamFive, Role: RoleCross}, 0, now - cross},
	} {
		after, until, ok := c.segment.Window(now, merge, cross)
		if !ok || after != c.after || until != c.until {
			t.Fatalf("%s: got (%d, %d, %v), want (%d, %d, true)", c.name, after, until, ok, c.after, c.until)
		}
	}
	if _, _, ok := (Segment{Pool: teamFive, Role: RoleCross}).Window(cross, merge, cross); ok {
		t.Fatal("时钟没走过跨机房阈值时跨机房段应跳过")
	}
	if _, _, ok := (Segment{Pool: teamFive, Role: RoleRegion, Region: 1, SubPools: 4}).Window(merge, merge, cross); ok {
		t.Fatal("时钟没走过合并时长时拆了子池的机房段应跳过")
	}
	if after, until, ok := (Segment{Pool: teamFive, Role: RoleSub, Region: 1, SubPools: 4}).Window(merge, merge, cross); !ok || after != 0 || until != 0 {
		t.Fatal("时钟没走过合并时长时子池段应读全部")
	}
}

// 指标的 role 标签固定这四个值、这个顺序。
func TestRoleLabels(t *testing.T) {
	var labels []string
	for _, role := range Roles {
		labels = append(labels, role.String())
	}
	if want := []string{"pool", "region", "sub", "cross"}; !reflect.DeepEqual(labels, want) {
		t.Fatalf("role 标签应为 %v, got %v", want, labels)
	}
}
