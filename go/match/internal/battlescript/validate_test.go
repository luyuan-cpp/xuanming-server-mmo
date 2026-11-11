package battlescript

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"proto/battle"
	pbtable "shared/generated/pb/table"
)

// 与 C++ cpp/tests/turn_battle_engine_test/battle_script_test.cpp 的校验用例一一对应(同一批非法写法两边都要拒)。

const (
	skillSeal    = 9501 // 老技能,单体
	skillSlam    = 9504 // 老技能,零耗蓝
	skillCostly  = 9505 // 老技能,耗蓝 10
	skillPassive = 9507 // 被动
	skillMend    = 9601 // 职业 3 友方治疗
	monsterBoss  = 102
	dungeonID    = 31
	buffStun     = 9930
	classSeal    = 11
	classHeal    = 3
)

type fakeTables struct {
	skills   map[uint32]*pbtable.SkillTable
	buffs    map[uint32]bool
	monsters map[uint32]bool
	dungeons map[uint32]bool
}

func (f fakeTables) Skill(id uint32) (*pbtable.SkillTable, bool) {
	row, ok := f.skills[id]
	return row, ok
}
func (f fakeTables) HasBuff(id uint32) bool    { return f.buffs[id] }
func (f fakeTables) HasMonster(id uint32) bool { return f.monsters[id] }
func (f fakeTables) HasDungeon(id uint32) bool { return f.dungeons[id] }

func newFakeTables() fakeTables {
	legacy := func(id uint32) *pbtable.SkillTable {
		return &pbtable.SkillTable{Id: id, TargetingMode: []uint32{1}, SkillType: []uint32{1}}
	}
	costly := legacy(skillCostly)
	costly.CostResource = []*pbtable.SkillcostResource{{CostResourceId: skillCostResourceMana, CostResourceCost: 10}}
	return fakeTables{
		skills: map[uint32]*pbtable.SkillTable{
			skillSeal:    legacy(skillSeal),
			skillSlam:    legacy(skillSlam),
			skillCostly:  costly,
			skillPassive: {Id: skillPassive, SkillType: []uint32{skillTypeBitPassive}},
			skillMend:    {Id: skillMend, ClassId: classHeal, SkillType: []uint32{1}, TargetTeam: targetTeamAlly},
		},
		buffs:    map[uint32]bool{buffStun: true},
		monsters: map[uint32]bool{monsterBoss: true},
		dungeons: map[uint32]bool{dungeonID: true},
	}
}

func filter(kind battle.EBattleScriptTargetFilter, p1 uint32, negate bool) *battle.BattleScriptTargetFilter {
	return &battle.BattleScriptTargetFilter{Kind: kind, P1: p1, Negate: negate}
}

func rule(priority uint32, action battle.EBattleActionType, skill uint32, side battle.EBattleScriptTargetSide,
	filters ...*battle.BattleScriptTargetFilter) *battle.BattleScriptRule {
	return &battle.BattleScriptRule{
		Priority: priority, Action: action, SkillId: skill,
		Target: &battle.BattleScriptTarget{Side: side, Filters: filters},
	}
}

func role(id, order uint32, actor battle.EBattleActorType, class uint32, rules ...*battle.BattleScriptRule) *battle.BattleScriptRole {
	return &battle.BattleScriptRole{RoleId: id, Order: order, ActorType: actor, ClassId: class, Rules: rules}
}

const (
	attack = battle.EBattleActionType_BATTLE_ACTION_ATTACK
	skill  = battle.EBattleActionType_BATTLE_ACTION_SKILL
	defend = battle.EBattleActionType_BATTLE_ACTION_DEFEND
	enemy  = battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_ENEMY
	ally   = battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_ALLY
	self   = battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_SELF
	pick   = battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_ENGINE_PICK
	player = battle.EBattleActorType_BATTLE_ACTOR_TYPE_PLAYER
	pet    = battle.EBattleActorType_BATTLE_ACTOR_TYPE_PET
	mob    = battle.EBattleActorType_BATTLE_ACTOR_TYPE_MONSTER
	anyone = battle.EBattleActorType_BATTLE_ACTOR_TYPE_NONE
)

func baselineScript() *battle.BattleScript {
	return &battle.BattleScript{
		SchemaVersion: SchemaVersion, ScriptId: 1, Name: "测试剧本",
		Roles: []*battle.BattleScriptRole{
			role(1, 10, player, classSeal,
				rule(10, skill, skillSeal, enemy,
					filter(battle.EBattleScriptTargetFilter_BATTLE_SCRIPT_FILTER_HAS_BUFF_TYPE, buffTypeStun, true))),
		},
	}
}

func TestNormalizeSortsRolesAndRules(t *testing.T) {
	script := &battle.BattleScript{SchemaVersion: SchemaVersion, ScriptId: 1, Roles: []*battle.BattleScriptRole{
		role(7, 50, player, 0, rule(30, attack, 0, pick), rule(10, defend, 0, self)),
		role(3, 5, player, classSeal, rule(1, skill, skillSeal, enemy)),
	}}
	normalized, err := Normalize(script, newFakeTables())
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Roles[0].GetRoleId() != 3 || normalized.Roles[1].GetRoleId() != 7 {
		t.Fatalf("roles not sorted by order: %v", normalized)
	}
	if normalized.Roles[1].Rules[0].GetPriority() != 10 || normalized.Roles[1].Rules[1].GetPriority() != 30 {
		t.Fatalf("rules not sorted by priority: %v", normalized)
	}
	if script.Roles[0].GetRoleId() != 7 {
		t.Fatal("Normalize must not modify its input")
	}
}

func TestNormalizeRejectsMalformedScripts(t *testing.T) {
	tables := newFakeTables()
	if _, err := Normalize(baselineScript(), tables); err != nil {
		t.Fatalf("baseline must pass: %v", err)
	}
	first := func(s *battle.BattleScript) *battle.BattleScriptRule { return s.Roles[0].Rules[0] }
	cases := map[string]func(s *battle.BattleScript){
		"schema version":    func(s *battle.BattleScript) { s.SchemaVersion = 2 },
		"no roles":          func(s *battle.BattleScript) { s.Roles = nil },
		"zero role id":      func(s *battle.BattleScript) { s.Roles[0].RoleId = 0 },
		"duplicate role id": func(s *battle.BattleScript) { s.Roles = append(s.Roles, role(1, 20, pet, 0, rule(1, defend, 0, self))) },
		"duplicate order":   func(s *battle.BattleScript) { s.Roles = append(s.Roles, role(2, 10, pet, 0, rule(1, defend, 0, self))) },
		"role without rules": func(s *battle.BattleScript) { s.Roles[0].Rules = nil },
		"too many rules": func(s *battle.BattleScript) {
			for p := uint32(100); p < 116; p++ {
				s.Roles[0].Rules = append(s.Roles[0].Rules, rule(p, defend, 0, self))
			}
		},
		"duplicate priority":       func(s *battle.BattleScript) { s.Roles[0].Rules = append(s.Roles[0].Rules, rule(10, defend, 0, self)) },
		"unknown monster in role":  func(s *battle.BattleScript) { s.Roles[0].ActorType = anyone; s.Roles[0].ClassId = 0; s.Roles[0].MonsterId = 4242 },
		"class on monster role":    func(s *battle.BattleScript) { s.Roles[0].ActorType = mob },
		"condition none": func(s *battle.BattleScript) {
			first(s).Conditions = []*battle.BattleScriptCondition{{Kind: battle.EBattleScriptCondition_BATTLE_SCRIPT_COND_NONE, P1: 1}}
		},
		"hp condition out of range": func(s *battle.BattleScript) {
			first(s).Conditions = []*battle.BattleScriptCondition{{Kind: battle.EBattleScriptCondition_BATTLE_SCRIPT_COND_SELF_HP_BELOW, P1: 10001}}
		},
		"round every without period": func(s *battle.BattleScript) {
			first(s).Conditions = []*battle.BattleScriptCondition{{Kind: battle.EBattleScriptCondition_BATTLE_SCRIPT_COND_ROUND_EVERY, P1: 0, P2: 1}}
		},
		"unused p2 set": func(s *battle.BattleScript) {
			first(s).Conditions = []*battle.BattleScriptCondition{{Kind: battle.EBattleScriptCondition_BATTLE_SCRIPT_COND_ROUND_AT_LEAST, P1: 2, P2: 5}}
		},
		"three conditions": func(s *battle.BattleScript) {
			c := &battle.BattleScriptCondition{Kind: battle.EBattleScriptCondition_BATTLE_SCRIPT_COND_ROUND_AT_LEAST, P1: 1}
			first(s).Conditions = []*battle.BattleScriptCondition{c, c, c}
		},
		"filter none": func(s *battle.BattleScript) {
			first(s).Target.Filters = append(first(s).Target.Filters, filter(battle.EBattleScriptTargetFilter_BATTLE_SCRIPT_FILTER_NONE, 1, false))
		},
		"four filters": func(s *battle.BattleScript) {
			f := filter(battle.EBattleScriptTargetFilter_BATTLE_SCRIPT_FILTER_HP_BELOW, 5000, false)
			first(s).Target.Filters = append(first(s).Target.Filters, f, f, f)
		},
		"dispel is not a status": func(s *battle.BattleScript) {
			first(s).Target.Filters = append(first(s).Target.Filters, filter(battle.EBattleScriptTargetFilter_BATTLE_SCRIPT_FILTER_HAS_BUFF_TYPE, 35, false))
		},
		"unknown buff": func(s *battle.BattleScript) {
			first(s).Target.Filters = append(first(s).Target.Filters, filter(battle.EBattleScriptTargetFilter_BATTLE_SCRIPT_FILTER_HAS_BUFF, 4242, false))
		},
		"unknown monster filter": func(s *battle.BattleScript) {
			first(s).Target.Filters = append(first(s).Target.Filters, filter(battle.EBattleScriptTargetFilter_BATTLE_SCRIPT_FILTER_IS_MONSTER, 4242, false))
		},
		"bad actor type filter": func(s *battle.BattleScript) {
			first(s).Target.Filters = append(first(s).Target.Filters, filter(battle.EBattleScriptTargetFilter_BATTLE_SCRIPT_FILTER_IS_ACTOR_TYPE, 9, false))
		},
		"claim of unknown role": func(s *battle.BattleScript) {
			first(s).Target.Filters = append(first(s).Target.Filters, filter(battle.EBattleScriptTargetFilter_BATTLE_SCRIPT_FILTER_CLAIMED_BY_ROLE, 99, false))
		},
		"filters on engine pick": func(s *battle.BattleScript) { first(s).Target.Side = pick },
		"order on self": func(s *battle.BattleScript) {
			r := rule(20, defend, 0, self)
			r.Target.Order = battle.EBattleScriptTargetOrder_BATTLE_SCRIPT_ORDER_HP_LOWEST
			s.Roles[0].Rules = append(s.Roles[0].Rules, r)
		},
		"attack with skill id":  func(s *battle.BattleScript) { s.Roles[0].Rules = append(s.Roles[0].Rules, rule(20, attack, skillSeal, enemy)) },
		"attack on ally":        func(s *battle.BattleScript) { s.Roles[0].Rules = append(s.Roles[0].Rules, rule(20, attack, 0, ally)) },
		"defend on enemy":       func(s *battle.BattleScript) { s.Roles[0].Rules = append(s.Roles[0].Rules, rule(20, defend, 0, enemy)) },
		"skill without id":      func(s *battle.BattleScript) { s.Roles[0].Rules = append(s.Roles[0].Rules, rule(20, skill, 0, enemy)) },
		"unknown skill":         func(s *battle.BattleScript) { s.Roles[0].Rules = append(s.Roles[0].Rules, rule(20, skill, 4242, enemy)) },
		"passive skill":         func(s *battle.BattleScript) { s.Roles[0].Rules = append(s.Roles[0].Rules, rule(20, skill, skillPassive, enemy)) },
		"legacy skill on ally":  func(s *battle.BattleScript) { s.Roles[0].Rules = append(s.Roles[0].Rules, rule(20, skill, skillSlam, ally)) },
		"class skill of other class": func(s *battle.BattleScript) {
			s.Roles[0].Rules = append(s.Roles[0].Rules, rule(20, skill, skillMend, ally))
		},
		"class skill on wrong side": func(s *battle.BattleScript) {
			s.Roles = append(s.Roles, role(5, 50, player, classHeal, rule(1, skill, skillMend, enemy)))
		},
		"class skill in pet role": func(s *battle.BattleScript) {
			s.Roles = append(s.Roles, role(5, 50, pet, 0, rule(1, skill, skillMend, pick)))
		},
		"flee is not scriptable": func(s *battle.BattleScript) {
			s.Roles[0].Rules = append(s.Roles[0].Rules, rule(20, battle.EBattleActionType_BATTLE_ACTION_FLEE, 0, self))
		},
		"note too long": func(s *battle.BattleScript) { first(s).Note = strings.Repeat("x", MaxNoteBytes+1) },
	}
	for name, mutate := range cases {
		script := proto.Clone(baselineScript()).(*battle.BattleScript)
		mutate(script)
		if _, err := Normalize(script, tables); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestCheckSideKeepsMonstersAndPlayersApart(t *testing.T) {
	tables := newFakeTables()
	monsterScript := &battle.BattleScript{SchemaVersion: SchemaVersion, ScriptId: 2, Roles: []*battle.BattleScriptRole{
		{RoleId: 1, Order: 10, ActorType: mob, MonsterId: monsterBoss, Rules: []*battle.BattleScriptRule{rule(10, skill, skillSlam, enemy)}},
	}}
	if _, err := Normalize(monsterScript, tables); err != nil {
		t.Fatal(err)
	}
	if err := CheckSide(monsterScript, battle.EBattleScriptSide_BATTLE_SCRIPT_SIDE_MONSTER, tables); err != nil {
		t.Fatalf("monster script on monster side: %v", err)
	}
	if CheckSide(monsterScript, battle.EBattleScriptSide_BATTLE_SCRIPT_SIDE_PLAYER, tables) == nil {
		t.Fatal("monster roles must not be accepted on the player side")
	}
	if CheckSide(monsterScript, battle.EBattleScriptSide_BATTLE_SCRIPT_SIDE_UNSPECIFIED, tables) == nil {
		t.Fatal("unspecified side must be rejected")
	}

	costly := &battle.BattleScript{SchemaVersion: SchemaVersion, ScriptId: 3, Roles: []*battle.BattleScriptRole{
		role(1, 10, mob, 0, rule(10, skill, skillCostly, enemy)),
	}}
	if CheckSide(costly, battle.EBattleScriptSide_BATTLE_SCRIPT_SIDE_MONSTER, tables) == nil {
		t.Fatal("monsters have no mana: a costed skill must be rejected")
	}

	anyRole := &battle.BattleScript{SchemaVersion: SchemaVersion, ScriptId: 4, Roles: []*battle.BattleScriptRole{
		role(1, 10, anyone, 0, rule(10, skill, skillMend, ally)),
	}}
	if _, err := Normalize(anyRole, tables); err != nil {
		t.Fatal(err)
	}
	if err := CheckSide(anyRole, battle.EBattleScriptSide_BATTLE_SCRIPT_SIDE_PLAYER, tables); err != nil {
		t.Fatalf("class skill in an any-unit role is fine on the player side: %v", err)
	}
	if CheckSide(anyRole, battle.EBattleScriptSide_BATTLE_SCRIPT_SIDE_MONSTER, tables) == nil {
		t.Fatal("monsters cannot cast class skills")
	}
}
