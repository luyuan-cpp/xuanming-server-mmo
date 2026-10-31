package catalog

import (
	"testing"

	"match/internal/scope"

	matchpb "proto/match"
)

const (
	modeSolo     = matchpb.MatchMode_MATCH_MODE_PVE_SOLO
	modeTeam     = matchpb.MatchMode_MATCH_MODE_PVE_TEAM
	mode1v1      = matchpb.MatchMode_MATCH_MODE_1V1
	mode5v5      = matchpb.MatchMode_MATCH_MODE_5V5
	modePvpTeam  = matchpb.MatchMode_MATCH_MODE_PVP_TEAM
	modeDuel     = matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE
	modeReserved = matchpb.MatchMode_MATCH_MODE_3V3
)

func baseRows() Rows {
	return Rows{
		Dungeons: []DungeonRow{
			{Id: 1, MaxTeamSize: 5, TimeLimitSeconds: 1800, MonsterCount: 2},
			{Id: 2, MaxTeamSize: 3, TimeLimitSeconds: 0, MonsterCount: 2},
			{Id: 3, MaxTeamSize: 10, TimeLimitSeconds: 9000, MonsterCount: 3},
		},
		PlayModes: []PlayModeRow{
			{Id: 1, Mode: modeSolo, ConfigId: 1, Enabled: true, TeamSizeMax: 1},
			{Id: 2, Mode: modeTeam, ConfigId: 1, Enabled: true, TeamSizeMax: 3},
			{Id: 3, Mode: mode1v1, ConfigId: 1, Enabled: true},
			{Id: 4, Mode: mode5v5, ConfigId: 1, Enabled: true},
			{Id: 5, Mode: modePvpTeam, ConfigId: 1, Enabled: true, TeamSizeMax: 2},
			{Id: 6, Mode: modeDuel, ConfigId: 0, Enabled: true},
			{Id: 7, Mode: modeTeam, ConfigId: 2, Enabled: false, TeamSizeMax: 3},
		},
		ZoneGroups: []scope.Range{{GroupID: 1, ZoneBegin: 1, ZoneEnd: 100}},
	}
}

func mustNew(t *testing.T, rows Rows) *Catalog {
	t.Helper()
	c, err := New(rows)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestOpenAndSizes(t *testing.T) {
	c := mustNew(t, baseRows())
	cases := []struct {
		mode   matchpb.MatchMode
		config uint32
		open   bool
		max    uint32
	}{
		{modeSolo, 1, true, 1},
		{modeTeam, 1, true, 3},
		{mode1v1, 1, true, 0},
		{mode5v5, 1, true, 0},
		{modePvpTeam, 1, true, 2},
		{modeDuel, 0, true, 0},
		{modeTeam, 2, false, 0}, // 关闭的行不开放
		{modeSolo, 2, false, 0}, // 没有这一行
		{mode1v1, 9, false, 0},  // 客户端任填的模板
	}
	for _, tc := range cases {
		if got := c.Open(tc.mode, tc.config); got != tc.open {
			t.Errorf("Open(%v,%d)=%v want %v", tc.mode, tc.config, got, tc.open)
		}
		if got := c.MaxTeamSize(tc.mode, tc.config); got != tc.max {
			t.Errorf("MaxTeamSize(%v,%d)=%d want %d", tc.mode, tc.config, got, tc.max)
		}
	}
}

func TestPoolsEnumerateOpenEntries(t *testing.T) {
	got := mustNew(t, baseRows()).Pools()
	want := []Pool{
		{Mode: mode5v5, ConfigId: 1, TeamSize: 10},
		{Mode: mode1v1, ConfigId: 1, TeamSize: 2},
		{Mode: modeSolo, ConfigId: 1, TeamSize: 1},
		{Mode: modeTeam, ConfigId: 1, TeamSize: 1},
		{Mode: modeTeam, ConfigId: 1, TeamSize: 2},
		{Mode: modeTeam, ConfigId: 1, TeamSize: 3},
		{Mode: modePvpTeam, ConfigId: 1, TeamSize: 1},
		{Mode: modePvpTeam, ConfigId: 1, TeamSize: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("pools=%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pool %d = %+v want %+v", i, got[i], want[i])
		}
	}
	if (Pool{Mode: modePvpTeam, TeamSize: 3}).Players() != 6 || (Pool{Mode: modeTeam, TeamSize: 3}).Players() != 3 {
		t.Fatal("Players 口径错误")
	}
}

func TestBattleSeconds(t *testing.T) {
	c := mustNew(t, baseRows())
	if c.BattleSeconds(1) != 1800 || c.BattleSeconds(2) != DefaultBattleSeconds || c.BattleSeconds(3) != MaxBattleSeconds || c.BattleSeconds(42) != DefaultBattleSeconds {
		t.Fatal("战斗时长取值错误")
	}
	var nilCatalog *Catalog
	if nilCatalog.BattleSeconds(1) != DefaultBattleSeconds {
		t.Fatal("nil 目录应取默认时长")
	}
}

func TestZoneNode(t *testing.T) {
	c := mustNew(t, baseRows())
	if c.ZoneNode(5) != (scope.Node{Zone: 5, Group: 1}) || c.ZoneNode(500) != (scope.Node{Zone: 500}) {
		t.Fatal("大区映射错误")
	}
	var nilCatalog *Catalog
	if nilCatalog.ZoneNode(5) != (scope.Node{Zone: 5}) || !nilCatalog.Open(mode1v1, 9) || nilCatalog.MaxTeamSize(modeTeam, 1) != 0 || nilCatalog.Pools() != nil {
		t.Fatal("nil 目录行为错误")
	}
}

func TestRejectsBadRows(t *testing.T) {
	mutate := func(edit func(*Rows)) Rows {
		rows := baseRows()
		edit(&rows)
		return rows
	}
	bad := map[string]Rows{
		"(模式,模板)重复": mutate(func(r *Rows) {
			r.PlayModes = append(r.PlayModes, PlayModeRow{Id: 99, Mode: mode1v1, ConfigId: 1})
		}),
		"模板不在 Dungeon 表": mutate(func(r *Rows) {
			r.PlayModes = append(r.PlayModes, PlayModeRow{Id: 99, Mode: mode1v1, ConfigId: 77})
		}),
		"PVE 引用无怪物模板": mutate(func(r *Rows) {
			r.Dungeons = append(r.Dungeons, DungeonRow{Id: 8, MaxTeamSize: 5})
			r.PlayModes = append(r.PlayModes, PlayModeRow{Id: 99, Mode: modeSolo, ConfigId: 8, TeamSizeMax: 1})
		}),
		"PVE 引用模板 0": mutate(func(r *Rows) {
			r.PlayModes = append(r.PlayModes, PlayModeRow{Id: 99, Mode: modeSolo, ConfigId: 0, TeamSizeMax: 1})
		}),
		"PVE_SOLO 人数不是 1": mutate(func(r *Rows) { r.PlayModes[0].TeamSizeMax = 2 }),
		// 关闭的行同样要合法：模板 2 的 Dungeon.max_team_size 是 3。
		"PVE_TEAM 超过模板上限": mutate(func(r *Rows) { r.PlayModes[6].TeamSizeMax = 4 }),
		"PVE_TEAM 超过引擎上限": mutate(func(r *Rows) {
			r.PlayModes = append(r.PlayModes, PlayModeRow{Id: 99, Mode: modeTeam, ConfigId: 3, TeamSizeMax: 6})
		}),
		"PVE_TEAM 人数为 0":   mutate(func(r *Rows) { r.PlayModes[1].TeamSizeMax = 0 }),
		"PVP_TEAM 每侧超过 5": mutate(func(r *Rows) { r.PlayModes[4].TeamSizeMax = 6 }),
		"1V1 写了人数":        mutate(func(r *Rows) { r.PlayModes[2].TeamSizeMax = 1 }),
		"未开放的模式":          mutate(func(r *Rows) { r.PlayModes[2].Mode = modeReserved }),
		"Dungeon id 重复": mutate(func(r *Rows) {
			r.Dungeons = append(r.Dungeons, DungeonRow{Id: 1, MaxTeamSize: 5, MonsterCount: 1})
		}),
		"大区号段重叠": mutate(func(r *Rows) {
			r.ZoneGroups = append(r.ZoneGroups, scope.Range{GroupID: 2, ZoneBegin: 100, ZoneEnd: 200})
		}),
	}
	for name, rows := range bad {
		if _, err := New(rows); err == nil {
			t.Errorf("%s: 应拒绝", name)
		}
	}
}

func TestOwnerKeyGroupsSizesOfOneTemplate(t *testing.T) {
	a := Pool{Mode: modeTeam, ConfigId: 1, TeamSize: 2}
	b := Pool{Mode: modeTeam, ConfigId: 1, TeamSize: 5}
	c := Pool{Mode: modeTeam, ConfigId: 2, TeamSize: 2}
	if a.OwnerKey() != b.OwnerKey() || a.OwnerKey() == c.OwnerKey() {
		t.Fatal("同模板各人数档应同归属键，不同模板应不同")
	}
}
