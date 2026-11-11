package battlescript

import (
	"strings"
	"testing"

	"proto/battle"
	pbtable "shared/generated/pb/table"
)

const (
	sidePlayer  = battle.EBattleScriptSide_BATTLE_SCRIPT_SIDE_PLAYER
	sideMonster = battle.EBattleScriptSide_BATTLE_SCRIPT_SIDE_MONSTER
)

func catalogConfig(bindings ...*battle.BattleScriptBinding) *battle.BattleScriptConfig {
	playerScript := baselineScript()
	monsterScript := &battle.BattleScript{SchemaVersion: SchemaVersion, ScriptId: 2, Roles: []*battle.BattleScriptRole{
		role(1, 10, mob, 0, rule(10, skill, skillSlam, enemy)),
	}}
	generic := &battle.BattleScript{SchemaVersion: SchemaVersion, ScriptId: 3, Roles: []*battle.BattleScriptRole{
		role(1, 10, player, 0, rule(10, attack, 0, pick)),
	}}
	return &battle.BattleScriptConfig{
		Scripts:  []*battle.BattleScript{playerScript, monsterScript, generic},
		Bindings: bindings,
	}
}

func binding(mode, config uint32, side battle.EBattleScriptSide, script uint32) *battle.BattleScriptBinding {
	return &battle.BattleScriptBinding{MatchMode: mode, BattleConfigId: config, Side: side, ScriptId: script}
}

func TestCatalogLooksUpFromSpecificToGeneric(t *testing.T) {
	catalog, err := BuildCatalog(catalogConfig(
		binding(5, dungeonID, sidePlayer, 1),
		binding(5, 0, sidePlayer, 3),
		binding(0, 0, sideMonster, 2),
	), newFakeTables())
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.Bound(5, dungeonID, sidePlayer); got.GetScriptId() != 1 {
		t.Fatalf("mode+dungeon binding expected, got %v", got.GetScriptId())
	}
	if got := catalog.Bound(5, 99, sidePlayer); got.GetScriptId() != 3 {
		t.Fatalf("mode binding expected, got %v", got.GetScriptId())
	}
	if got := catalog.Bound(3, dungeonID, sidePlayer); got != nil {
		t.Fatalf("no binding for PVP 1v1, got %v", got.GetScriptId())
	}
	if got := catalog.Bound(4, 1, sideMonster); got.GetScriptId() != 2 {
		t.Fatalf("global monster binding expected, got %v", got.GetScriptId())
	}
	ids := []uint32{}
	for _, script := range catalog.OfficialScripts() {
		ids = append(ids, script.GetScriptId())
	}
	if len(ids) != 3 || ids[0] != 1 || ids[1] != 2 || ids[2] != 3 {
		t.Fatalf("official scripts must list by id: %v", ids)
	}
}

func TestCatalogRejectsBadConfig(t *testing.T) {
	cases := map[string]*battle.BattleScriptConfig{
		"unknown script":          catalogConfig(binding(5, 0, sidePlayer, 42)),
		"duplicate binding":       catalogConfig(binding(5, 0, sidePlayer, 1), binding(5, 0, sidePlayer, 3)),
		"unknown dungeon":         catalogConfig(binding(5, 4242, sidePlayer, 1)),
		"unknown match mode":      catalogConfig(binding(99, 0, sidePlayer, 1)),
		"monster side in PVP":     catalogConfig(binding(3, 0, sideMonster, 2)),
		"player script on monsters": catalogConfig(binding(5, 0, sideMonster, 1)),
		"monster script on players": catalogConfig(binding(5, 0, sidePlayer, 2)),
		"unspecified side":        catalogConfig(binding(5, 0, battle.EBattleScriptSide_BATTLE_SCRIPT_SIDE_UNSPECIFIED, 1)),
	}
	duplicateID := catalogConfig()
	duplicateID.Scripts[2].ScriptId = 1
	cases["duplicate script id"] = duplicateID
	zeroID := catalogConfig()
	zeroID.Scripts[0].ScriptId = 0
	cases["zero script id"] = zeroID
	for name, config := range cases {
		if _, err := BuildCatalog(config, newFakeTables()); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestParseCatalogRejectsUnknownJSONFields(t *testing.T) {
	_, err := ParseCatalog([]byte(`{"scripts":[],"bindingz":[]}`), newFakeTables())
	if err == nil || !strings.Contains(err.Error(), "protobuf JSON") {
		t.Fatalf("unknown field must be rejected by protojson, got %v", err)
	}
}

// 内置官方配置能被解析、并在「形状同真表」的配表下通过整份校验。
// 真表契约(id 都还在、职业 / 目标阵营没变)由 official_real_tables_test.go 对导出表再验一遍
func TestEmbeddedOfficialConfigParses(t *testing.T) {
	classSkill := func(id, class, team uint32) *pbtable.SkillTable {
		return &pbtable.SkillTable{Id: id, ClassId: class, SkillType: []uint32{1}, TargetTeam: team}
	}
	tables := fakeTables{
		skills: map[uint32]*pbtable.SkillTable{
			1101: classSkill(1101, 1, targetTeamEnemy), 1102: classSkill(1102, 1, targetTeamEnemy),
			1104: classSkill(1104, 1, targetTeamEnemy),
			1201: classSkill(1201, 2, targetTeamEnemy), 1202: classSkill(1202, 2, targetTeamEnemy),
			1302: classSkill(1302, 3, targetTeamAlly), 1305: classSkill(1305, 3, targetTeamAlly),
			1401: classSkill(1401, 4, targetTeamEnemy), 1404: classSkill(1404, 4, targetTeamEnemy),
		},
		buffs:    map[uint32]bool{11104: true, 11404: true},
		monsters: map[uint32]bool{16: true},
		dungeons: map[uint32]bool{3: true},
	}
	catalog, err := LoadOfficial(tables)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.OfficialScripts()) == 0 {
		t.Fatal("official config must ship at least one script")
	}
	if catalog.Bound(4, 3, sideMonster) == nil {
		t.Fatal("dungeon 3 monster side must be bound")
	}
}
