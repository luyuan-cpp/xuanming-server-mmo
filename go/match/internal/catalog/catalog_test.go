package catalog

import (
	"testing"

	"match/internal/scope"

	battlepb "proto/battle"
	matchpb "proto/match"
	"shared/generated/table"
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

// 回合上限与引擎同一换算(turn_battle_constants.h:kRoundDurationMs=6000、kDefaultMaxRounds=300、
// RoundsFromMilliseconds 向上取整):不截断到 MaxBattleSeconds,缺省与不在表里取 300。
func TestRoundLimitMirrorsEngine(t *testing.T) {
	if EngineRoundDurationMs != 6000 || EngineDefaultRoundLimit != 300 {
		t.Fatal("与 cpp/libs/services/battle/constants/turn_battle_constants.h 的常量不一致")
	}
	rows := baseRows()
	rows.Dungeons = append(rows.Dungeons,
		DungeonRow{Id: 4, MaxTeamSize: 5, TimeLimitSeconds: 7, MonsterCount: 1},
		DungeonRow{Id: 5, MaxTeamSize: 5, TimeLimitSeconds: 1, MonsterCount: 1})
	c := mustNew(t, rows)
	for config, want := range map[uint32]uint32{
		1:  300,  // 1800 秒正好 300 回合
		2:  300,  // time_limit 0 取引擎缺省
		3:  1500, // 9000 秒:整场期限截到 3600 秒,回合上限不截
		4:  2,    // 7 秒向上取整
		5:  1,    // 最短 1 回合
		0:  300,  // 切磋的模板 0 不在表里
		42: 300,
	} {
		if got := c.RoundLimit(config); got != want {
			t.Errorf("RoundLimit(%d)=%d want %d", config, got, want)
		}
	}
	var nilCatalog *Catalog
	if nilCatalog.RoundLimit(1) != EngineDefaultRoundLimit {
		t.Fatal("nil 目录应取引擎缺省回合上限")
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
		"PVE_TEAM 人数为 0":  mutate(func(r *Rows) { r.PlayModes[1].TeamSizeMax = 0 }),
		"PVP_TEAM 每侧超过 5": mutate(func(r *Rows) { r.PlayModes[4].TeamSizeMax = 6 }),
		"1V1 写了人数":        mutate(func(r *Rows) { r.PlayModes[2].TeamSizeMax = 1 }),
		"未开放的模式":          mutate(func(r *Rows) { r.PlayModes[2].Mode = modeReserved }),
		"Dungeon id 重复": mutate(func(r *Rows) {
			r.Dungeons = append(r.Dungeons, DungeonRow{Id: 1, MaxTeamSize: 5, MonsterCount: 1})
		}),
		"大区号段重叠": mutate(func(r *Rows) {
			r.ZoneGroups = append(r.ZoneGroups, scope.Range{GroupID: 2, ZoneBegin: 100, ZoneEnd: 200})
		}),
		"allowed_team_sizes 超出上限": mutate(func(r *Rows) {
			r.PlayModes[4].AllowedTeamSizes = []uint32{1, 6}
		}),
		"allowed_team_sizes 含重复": mutate(func(r *Rows) {
			r.PlayModes[4].AllowedTeamSizes = []uint32{1, 1}
		}),
		"固定人数模式配置了 allowed_team_sizes": mutate(func(r *Rows) {
			r.PlayModes[2].AllowedTeamSizes = []uint32{1}
		}),
		"POLARITY_5V5 写了 team_size_max": mutate(func(r *Rows) {
			r.PlayModes = append(r.PlayModes, PlayModeRow{Id: 98, Mode: matchpb.MatchMode_MATCH_MODE_POLARITY_5V5, ConfigId: 1, Enabled: true, TeamSizeMax: 5})
		}),
	}
	for name, rows := range bad {
		if _, err := New(rows); err == nil {
			t.Errorf("%s: 应拒绝", name)
		}
	}
}

func TestAllowedTeamSizesWhitelistAndPolarity5v5Pool(t *testing.T) {
	c, err := New(Rows{
		Dungeons: []DungeonRow{
			{Id: 5, MaxTeamSize: 5, TimeLimitSeconds: 600, MonsterCount: 1},
			{Id: 110, MaxTeamSize: 5, TimeLimitSeconds: 96, RoundLimit: 16, MonsterCount: 0},
		},
		PlayModes: []PlayModeRow{
			{Id: 6, Mode: matchpb.MatchMode_MATCH_MODE_PVP_TEAM, ConfigId: 5, Enabled: true, TeamSizeMax: 5, AllowedTeamSizes: []uint32{1, 3}},
			{Id: 301, Mode: matchpb.MatchMode_MATCH_MODE_POLARITY_5V5, ConfigId: 110, Enabled: true, TeamSizeMax: 0},
		},
	})
	if err != nil {
		t.Fatalf("合法冷启动白名单与物法易势目录应构建成功: %v", err)
	}
	if !c.AllowedTeamSize(matchpb.MatchMode_MATCH_MODE_PVP_TEAM, 5, 1) ||
		!c.AllowedTeamSize(matchpb.MatchMode_MATCH_MODE_PVP_TEAM, 5, 3) {
		t.Fatalf("白名单内的 1V1 / 3V3 必须放行")
	}
	for _, blocked := range []uint32{0, 2, 4, 5, 6} {
		if c.AllowedTeamSize(matchpb.MatchMode_MATCH_MODE_PVP_TEAM, 5, blocked) {
			t.Fatalf("冷启动期未开放的 %dV%d 不应放行", blocked, blocked)
		}
	}
	if got := c.RoundLimit(110); got != 16 {
		t.Fatalf("显式 round_limit=16 应优先于 time_limit: got %d", got)
	}
	wantPools := []Pool{
		{Mode: matchpb.MatchMode_MATCH_MODE_PVP_TEAM, ConfigId: 5, TeamSize: 1},
		{Mode: matchpb.MatchMode_MATCH_MODE_PVP_TEAM, ConfigId: 5, TeamSize: 3},
		{Mode: matchpb.MatchMode_MATCH_MODE_POLARITY_5V5, ConfigId: 110, TeamSize: 10},
	}
	gotPools := c.Pools()
	if len(gotPools) != len(wantPools) {
		t.Fatalf("Pools() = %+v, want %+v", gotPools, wantPools)
	}
	for i := range wantPools {
		if gotPools[i] != wantPools[i] {
			t.Fatalf("Pools()[%d] = %+v, want %+v", i, gotPools[i], wantPools[i])
		}
	}
}

func TestLoadGeneratedTablesIncludesBossTrialsAndColdStartWhitelist(t *testing.T) {
	c, err := Load("../../../../generated/tables")
	if err != nil {
		t.Fatalf("加载真实导出表失败: %v", err)
	}
	if !c.AllowedTeamSize(modePvpTeam, 5, 1) || !c.AllowedTeamSize(modePvpTeam, 5, 3) {
		t.Fatalf("真实 PlayMode 表中 PVP_TEAM(5) 必须放行冷启动 1V1 与 3V3")
	}
	for _, blocked := range []uint32{2, 4, 5} {
		if c.AllowedTeamSize(modePvpTeam, 5, blocked) {
			t.Fatalf("真实 PlayMode 表中 PVP_TEAM(5) 冷启动不应开放 %dV%d", blocked, blocked)
		}
	}
	if !c.Open(matchpb.MatchMode_MATCH_MODE_POLARITY_5V5, 110) {
		t.Fatalf("真实 PlayMode 表应开放物法易势 5V5 (Dungeon 110)")
	}
	if got := c.RoundLimit(110); got != 16 {
		t.Fatalf("Dungeon 110 回合上限应为 16, got %d", got)
	}
	for id := uint32(101); id <= 109; id++ {
		if !c.Open(modeSolo, id) || !c.Open(modeTeam, id) {
			t.Fatalf("首领试炼/护卫救援 Dungeon %d 应同时开放单人与组队入口", id)
		}
	}
	if got := c.RoundLimit(109); got != 8 {
		t.Fatalf("护卫救援 Dungeon 109 回合上限应为 8, got %d", got)
	}

	// 核对真实导出表生成的全部 11 格试炼志上下文：卷一(101..104)、卷二(201..204)、卷三护卫救援(301)、物法易势(302)、人宠会武(303)。
	wantPveTrials := map[uint32]struct {
		entryId  uint32
		guardId  uint32
		seasonId uint32
	}{
		101: {entryId: 101, guardId: 102, seasonId: 0},
		102: {entryId: 103, guardId: 102, seasonId: 0},
		103: {entryId: 102, guardId: 103, seasonId: 0},
		104: {entryId: 104, guardId: 103, seasonId: 0},
		105: {entryId: 201, guardId: 105, seasonId: 0},
		106: {entryId: 203, guardId: 105, seasonId: 0},
		107: {entryId: 202, guardId: 107, seasonId: 0},
		108: {entryId: 204, guardId: 107, seasonId: 0},
		109: {entryId: 301, guardId: 102, seasonId: 1},
	}
	for dungeonId, want := range wantPveTrials {
		solo := c.TrialContextFor(modeSolo, dungeonId, nil, nil)
		team := c.TrialContextFor(modeTeam, dungeonId, nil, nil)
		if solo == nil || team == nil {
			t.Fatalf("Dungeon %d 在单人/组队 PVE 必须生成 TrialContext", dungeonId)
		}
		if solo.GetJournalEntryId() != want.entryId || solo.GetTrialRuleVersion() != 1 ||
			solo.GetSeasonId() != want.seasonId || solo.GetGuardMonsterTableId() != want.guardId {
			t.Fatalf("Dungeon %d TrialContext=%+v want entry=%d guard=%d season=%d", dungeonId, solo, want.entryId, want.guardId, want.seasonId)
		}
		if team.GetJournalEntryId() != want.entryId || team.GetGuardMonsterTableId() != want.guardId {
			t.Fatalf("Dungeon %d 组队 TrialContext=%+v", dungeonId, team)
		}
		// 篡改返回值不得污染目录内部缓存。
		solo.JournalEntryId = 9999
		if again := c.TrialContextFor(modeSolo, dungeonId, nil, nil); again.GetJournalEntryId() != want.entryId {
			t.Fatalf("TrialContextFor 必须返回深拷贝")
		}
		// 帮会活动、带排位上下文的 PVE 与切磋不得写入 PVE 试炼志上下文。
		if got := c.TrialContextFor(modeTeam, dungeonId, &battlepb.BattleActivityContext{GuildId: 1}, nil); got != nil {
			t.Fatalf("活动战不应生成 TrialContext: %+v", got)
		}
		if got := c.TrialContextFor(modeSolo, dungeonId, nil, &battlepb.RankedMatchContext{SeasonId: 1}); got != nil {
			t.Fatalf("带排位上下文的 PVE 不应生成 TrialContext: %+v", got)
		}
		if got := c.TrialContextFor(modeDuel, dungeonId, nil, nil); got != nil {
			t.Fatalf("切磋模式不应生成 TrialContext: %+v", got)
		}
	}
	// 卷三 · 物法易势 5V5 (Dungeon 110 → Entry 302, season=1)
	polarityCtx := c.TrialContextFor(matchpb.MatchMode_MATCH_MODE_POLARITY_5V5, 110, nil, nil)
	if polarityCtx == nil || polarityCtx.GetJournalEntryId() != 302 || polarityCtx.GetSeasonId() != 1 {
		t.Fatalf("物法易势 5V5 (Dungeon 110) 必须生成 Entry 302(season=1): got %+v", polarityCtx)
	}
	// 卷三 · 正式人宠会武 PVP_TEAM (Dungeon 5 → Entry 303, season=1)，跨季不匹配时拒挂
	rankedTrial := c.TrialContextFor(modePvpTeam, 5, nil, &battlepb.RankedMatchContext{SeasonId: 1})
	if rankedTrial == nil || rankedTrial.GetJournalEntryId() != 303 || rankedTrial.GetSeasonId() != 1 {
		t.Fatalf("正式人宠会武 S1 必须生成 Entry 303(season=1): got %+v", rankedTrial)
	}
	if mismatchSeason := c.TrialContextFor(modePvpTeam, 5, nil, &battlepb.RankedMatchContext{SeasonId: 2}); mismatchSeason != nil {
		t.Fatalf("赛季号不匹配的排位不应挂 S1 试炼志上下文: got %+v", mismatchSeason)
	}
	for _, nonTrial := range []uint32{1, 2, 3, 4} {
		if got := c.TrialContextFor(modeSolo, nonTrial, nil, nil); got != nil {
			t.Fatalf("普通副本 Dungeon %d 不应生成 TrialContext: %+v", nonTrial, got)
		}
	}

	// 验证 BuildFromTableSet 路径与 Load 一致（热更与 --validate-tables 走此路径）。
	ts, err := table.Prepare("../../../../generated/tables", false)
	if err != nil {
		t.Fatalf("Prepare 真实表失败: %v", err)
	}
	fromTS, err := BuildFromTableSet(ts)
	if err != nil {
		t.Fatalf("BuildFromTableSet 失败: %v", err)
	}
	if got := fromTS.TrialContextFor(modeSolo, 102, nil, nil); got == nil || got.GetJournalEntryId() != 103 || got.GetGuardMonsterTableId() != 102 {
		t.Fatalf("BuildFromTableSet Dungeon 102 映射错误: %+v", got)
	}
	if got := fromTS.TrialContextFor(modeSolo, 106, nil, nil); got == nil || got.GetJournalEntryId() != 203 || got.GetGuardMonsterTableId() != 105 {
		t.Fatalf("BuildFromTableSet Dungeon 106 映射错误: %+v", got)
	}
	if got := fromTS.TrialContextFor(matchpb.MatchMode_MATCH_MODE_POLARITY_5V5, 110, nil, &battlepb.RankedMatchContext{UseFreePreset: true}); got == nil || got.GetJournalEntryId() != 302 {
		t.Fatalf("BuildFromTableSet Dungeon 110 带免费预设娱乐上下文映射错误: %+v", got)
	}
	if got := fromTS.TrialContextFor(modeSolo, 109, nil, &battlepb.RankedMatchContext{UseFreePreset: true}); got == nil || got.GetJournalEntryId() != 301 {
		t.Fatalf("BuildFromTableSet Dungeon 109 护卫救援带免费预设娱乐上下文映射错误: %+v", got)
	}
	gotSizes := c.AllowedTeamSizes(modePvpTeam, 5)
	if len(gotSizes) != 2 || gotSizes[0] != 1 || gotSizes[1] != 3 {
		t.Fatalf("PVP_TEAM(5) AllowedTeamSizes 应为 [1 3], got %v", gotSizes)
	}
	if got := c.BossTrialKind(109); got != 5 {
		t.Fatalf("Dungeon 109 BossTrialKind 应为 5, got %d", got)
	}
	if got := c.BossTrialKind(110); got != 6 {
		t.Fatalf("Dungeon 110 BossTrialKind 应为 6, got %d", got)
	}
}

func TestTrialContextRejectsDuplicateEnabledVariant(t *testing.T) {
	rows := baseRows()
	rows.TrialEntries = []TrialJournalEntryRow{
		{Id: 101, SourceKind: 1, Difficulty: 1, VariantKey: "rejuvenation", RuleVersion: 1, Enabled: true},
		{Id: 199, SourceKind: 1, Difficulty: 1, VariantKey: "rejuvenation", RuleVersion: 1, Enabled: true},
	}
	if _, err := New(rows); err == nil {
		t.Fatal("重复启用的同难度同机制 TrialJournalEntry 必须拒绝")
	}
}
