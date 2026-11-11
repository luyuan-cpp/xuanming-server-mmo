// Package battlescript 是战斗剧本(docs/design/battle-script.md)在 match 侧的校验、官方目录与解析。
//
// match 是「每队这一局用哪份剧本」的唯一决策点:官方剧本(本包内置的 protobuf JSON)、玩家选用、
// 玩家上传并经人工审核的剧本都在这里校验、解析,按值冻结进 CreateBattleRequest.team_scripts。
// battle 开局按自己的配表再校验一遍(cpp/libs/services/battle/system/battle_script.{h,cpp}),
// 两边规则逐条对应:改一处必须同改另一处,对拍用例见 testdata/vectors.json。
package battlescript

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"
	"proto/battle"
	pbtable "shared/generated/pb/table"
)

// 上限:与 C++ battle_script.h 同名常量逐条一致
const (
	SchemaVersion        = 1
	MaxRoles             = 8
	MaxRulesPerRole      = 16
	MaxConditionsPerRule = 2
	MaxFiltersPerRule    = 3
	MaxRoleMembers       = 10   // 一队最多 5 人 + 5 宝宝,或 10 只怪
	MaxRound             = 1000 // 回合类参数上限
	MaxUnitCount         = 20   // 「存活敌方 >= N」的 N 上限
	MaxClassID           = 1000 // 职业号只做区间校验
	PermyriadFull        = 10000
	MaxNameBytes         = 64
	MaxDescriptionBytes  = 512
	MaxNoteBytes         = 256
)

// buff 类型、技能类型位号、耗蓝资源、职业技能目标阵营:镜像 cpp turn_battle_constants.h / class_skill_rules.h
const (
	buffTypeIncreaseAttack                     = 10
	buffTypeIncreaseDefense                    = 11
	buffTypeMovementSpeedBoost                 = 13
	buffTypeStun                               = 30
	buffTypeSilence                            = 31
	buffTypeInvincibility                      = 32
	buffTypeImmunity                           = 34
	buffTypeHealthRegeneration                 = 40
	buffTypeManaRegeneration                   = 41
	buffTypeHealthRegenerationBasedOnLostHealth = 42
	buffTypePoison                             = 50
	buffTypeBurn                               = 51
	buffTypeFreeze                             = 52

	skillTypeBitPassive = 0
	skillTypeBitChannel = 2
	skillTypeBitToggle  = 3

	skillCostResourceMana = 1

	targetTeamEnemy = 1
	targetTeamAlly  = 2
	targetTeamSelf  = 3

	matchModePveSolo = 4
	matchModePveTeam = 5
)

// HAS_BUFF_TYPE 只认回合引擎里挂得住、会生效的持续状态类型;驱散(35)是瞬时的,不收
var statusBuffTypes = map[uint32]bool{
	buffTypeIncreaseAttack: true, buffTypeIncreaseDefense: true, buffTypeMovementSpeedBoost: true,
	buffTypeStun: true, buffTypeSilence: true, buffTypeInvincibility: true, buffTypeImmunity: true,
	buffTypeHealthRegeneration: true, buffTypeManaRegeneration: true,
	buffTypeHealthRegenerationBasedOnLostHealth: true,
	buffTypePoison: true, buffTypeBurn: true, buffTypeFreeze: true,
}

// IsStatusBuffType 报告 HAS_BUFF_TYPE 能不能用这个类型
func IsStatusBuffType(buffType uint32) bool { return statusBuffTypes[buffType] }

// Tables 是校验要查的配表。生产实现读全局表管理器(TableLookup),单测喂内存实现
type Tables interface {
	Skill(id uint32) (*pbtable.SkillTable, bool)
	HasBuff(id uint32) bool
	HasMonster(id uint32) bool
	HasDungeon(id uint32) bool
}

// castable 与引擎 TurnBattleEngine::IsTurnBattleCastableSkill 同一判据:被动 / 开关 / 持续施法放不了
func castable(row *pbtable.SkillTable) bool {
	if row.GetPassiveStat() != "" {
		return false
	}
	for _, bit := range row.GetSkillType() {
		if bit == skillTypeBitPassive || bit == skillTypeBitToggle || bit == skillTypeBitChannel {
			return false
		}
	}
	return true
}

// legacyManaCost 与引擎 TurnBattleEngine::LegacySkillManaCost 同一算法:cost_resource 里法力资源的 cost 之和
func legacyManaCost(row *pbtable.SkillTable) uint64 {
	var cost uint64
	for _, entry := range row.GetCostResource() {
		if entry.GetCostResourceId() == skillCostResourceMana {
			cost += uint64(entry.GetCostResourceCost())
		}
	}
	return cost
}

func inRange(value, low, high uint32) bool { return value >= low && value <= high }

func checkCondition(c *battle.BattleScriptCondition, where string) error {
	switch c.GetKind() {
	case battle.EBattleScriptCondition_BATTLE_SCRIPT_COND_SELF_HP_BELOW:
		if !inRange(c.GetP1(), 1, PermyriadFull) || c.GetP2() != 0 {
			return fmt.Errorf("%s: SELF_HP_BELOW needs p1 in 1..10000 and p2 = 0", where)
		}
	case battle.EBattleScriptCondition_BATTLE_SCRIPT_COND_ROUND_AT_LEAST:
		if !inRange(c.GetP1(), 1, MaxRound) || c.GetP2() != 0 {
			return fmt.Errorf("%s: ROUND_AT_LEAST needs p1 in 1..%d and p2 = 0", where, MaxRound)
		}
	case battle.EBattleScriptCondition_BATTLE_SCRIPT_COND_ROUND_EVERY:
		if !inRange(c.GetP1(), 1, MaxRound) || !inRange(c.GetP2(), 1, MaxRound) {
			return fmt.Errorf("%s: ROUND_EVERY needs period p1 and first round p2 in 1..%d", where, MaxRound)
		}
	case battle.EBattleScriptCondition_BATTLE_SCRIPT_COND_ENEMY_COUNT_AT_LEAST:
		if !inRange(c.GetP1(), 1, MaxUnitCount) || c.GetP2() != 0 {
			return fmt.Errorf("%s: ENEMY_COUNT_AT_LEAST needs p1 in 1..%d and p2 = 0", where, MaxUnitCount)
		}
	default:
		return fmt.Errorf("%s: unknown condition kind %d", where, c.GetKind())
	}
	return nil
}

func checkFilter(f *battle.BattleScriptTargetFilter, roleIDs map[uint32]bool, tables Tables, where string) error {
	p1 := f.GetP1()
	switch f.GetKind() {
	case battle.EBattleScriptTargetFilter_BATTLE_SCRIPT_FILTER_HP_BELOW:
		if !inRange(p1, 1, PermyriadFull) {
			return fmt.Errorf("%s: HP_BELOW needs p1 in 1..10000", where)
		}
	case battle.EBattleScriptTargetFilter_BATTLE_SCRIPT_FILTER_HAS_BUFF_TYPE:
		if !IsStatusBuffType(p1) {
			return fmt.Errorf("%s: HAS_BUFF_TYPE %d is not a status type", where, p1)
		}
	case battle.EBattleScriptTargetFilter_BATTLE_SCRIPT_FILTER_HAS_BUFF:
		if p1 == 0 || !tables.HasBuff(p1) {
			return fmt.Errorf("%s: unknown buff %d", where, p1)
		}
	case battle.EBattleScriptTargetFilter_BATTLE_SCRIPT_FILTER_IS_MONSTER:
		if p1 == 0 || !tables.HasMonster(p1) {
			return fmt.Errorf("%s: unknown monster %d", where, p1)
		}
	case battle.EBattleScriptTargetFilter_BATTLE_SCRIPT_FILTER_IS_CLASS:
		if !inRange(p1, 1, MaxClassID) {
			return fmt.Errorf("%s: IS_CLASS needs a class id", where)
		}
	case battle.EBattleScriptTargetFilter_BATTLE_SCRIPT_FILTER_IS_ACTOR_TYPE:
		if p1 != uint32(battle.EBattleActorType_BATTLE_ACTOR_TYPE_PLAYER) &&
			p1 != uint32(battle.EBattleActorType_BATTLE_ACTOR_TYPE_MONSTER) &&
			p1 != uint32(battle.EBattleActorType_BATTLE_ACTOR_TYPE_PET) {
			return fmt.Errorf("%s: IS_ACTOR_TYPE needs 1 (player), 2 (monster) or 3 (pet)", where)
		}
	case battle.EBattleScriptTargetFilter_BATTLE_SCRIPT_FILTER_CLAIMED_BY_ROLE:
		if p1 != 0 && !roleIDs[p1] {
			return fmt.Errorf("%s: CLAIMED_BY_ROLE refers to unknown role %d", where, p1)
		}
	default:
		return fmt.Errorf("%s: unknown filter kind %d", where, f.GetKind())
	}
	return nil
}

func checkTarget(t *battle.BattleScriptTarget, roleIDs map[uint32]bool, tables Tables, where string) error {
	side := t.GetSide()
	switch side {
	case battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_SELF, battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_ENEMY,
		battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_ALLY, battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_ENGINE_PICK:
	default:
		return fmt.Errorf("%s: target side must be SELF, ENEMY, ALLY or ENGINE_PICK", where)
	}
	if _, ok := battle.EBattleScriptTargetOrder_name[int32(t.GetOrder())]; !ok {
		return fmt.Errorf("%s: unknown target order %d", where, t.GetOrder())
	}
	if len(t.GetFilters()) > MaxFiltersPerRule {
		return fmt.Errorf("%s: at most %d target filters", where, MaxFiltersPerRule)
	}
	pickable := side == battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_ENEMY ||
		side == battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_ALLY
	if !pickable && (len(t.GetFilters()) != 0 || t.GetOrder() != battle.EBattleScriptTargetOrder_BATTLE_SCRIPT_ORDER_FORMATION) {
		return fmt.Errorf("%s: SELF / ENGINE_PICK targets take no filters and no order", where)
	}
	for _, f := range t.GetFilters() {
		if err := checkFilter(f, roleIDs, tables, where); err != nil {
			return err
		}
	}
	return nil
}

// checkAction 拦「写错了就永远不会生效」和「生效了会打错边」两类配置;出手前引擎还会过一遍完整校验链
func checkAction(role *battle.BattleScriptRole, rule *battle.BattleScriptRule, tables Tables, where string) error {
	side := rule.GetTarget().GetSide()
	switch rule.GetAction() {
	case battle.EBattleActionType_BATTLE_ACTION_ATTACK:
		if rule.GetSkillId() != 0 {
			return fmt.Errorf("%s: ATTACK carries no skill_id", where)
		}
		if side != battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_ENEMY &&
			side != battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_ENGINE_PICK {
			return fmt.Errorf("%s: ATTACK targets ENEMY or ENGINE_PICK", where)
		}
	case battle.EBattleActionType_BATTLE_ACTION_DEFEND:
		if rule.GetSkillId() != 0 {
			return fmt.Errorf("%s: DEFEND carries no skill_id", where)
		}
		if side != battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_SELF {
			return fmt.Errorf("%s: DEFEND targets SELF", where)
		}
	case battle.EBattleActionType_BATTLE_ACTION_SKILL:
		return checkSkillAction(role, rule, tables, where)
	default:
		return fmt.Errorf("%s: action must be ATTACK, SKILL or DEFEND", where)
	}
	return nil
}

func checkSkillAction(role *battle.BattleScriptRole, rule *battle.BattleScriptRule, tables Tables, where string) error {
	side := rule.GetTarget().GetSide()
	if rule.GetSkillId() == 0 {
		return fmt.Errorf("%s: SKILL needs skill_id", where)
	}
	row, ok := tables.Skill(rule.GetSkillId())
	if !ok {
		return fmt.Errorf("%s: unknown skill %d", where, rule.GetSkillId())
	}
	if !castable(row) {
		return fmt.Errorf("%s: skill %d cannot be cast in turn battles (passive, toggle or channel)", where, rule.GetSkillId())
	}
	if row.GetClassId() == 0 {
		// 老技能没有敌我语义(引擎不分阵营,伤害照打):只许打敌方
		if side != battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_ENEMY {
			return fmt.Errorf("%s: legacy skill %d can only target ENEMY", where, rule.GetSkillId())
		}
		return nil
	}
	if role.GetActorType() != battle.EBattleActorType_BATTLE_ACTOR_TYPE_NONE &&
		role.GetActorType() != battle.EBattleActorType_BATTLE_ACTOR_TYPE_PLAYER {
		return fmt.Errorf("%s: class skill %d needs a role that accepts players", where, rule.GetSkillId())
	}
	if role.GetClassId() != 0 && role.GetClassId() != row.GetClassId() {
		return fmt.Errorf("%s: class skill %d belongs to class %d but the role only accepts class %d",
			where, rule.GetSkillId(), row.GetClassId(), role.GetClassId())
	}
	if side == battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_ENGINE_PICK {
		return nil
	}
	team := row.GetTargetTeam()
	matches := (team == targetTeamEnemy && side == battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_ENEMY) ||
		(team == targetTeamAlly && side == battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_ALLY) ||
		(team == targetTeamSelf && side == battle.EBattleScriptTargetSide_BATTLE_SCRIPT_TARGET_SELF)
	if !matches {
		return fmt.Errorf("%s: class skill %d target side does not match its target_team %d", where, rule.GetSkillId(), team)
	}
	return nil
}

func checkRoleHeader(role *battle.BattleScriptRole, tables Tables, where string) error {
	if role.GetName() != "" && len(role.GetName()) > MaxNameBytes {
		return fmt.Errorf("%s: name too long", where)
	}
	if _, ok := battle.EBattleActorType_name[int32(role.GetActorType())]; !ok {
		return fmt.Errorf("%s: unknown actor_type", where)
	}
	if role.GetClassId() > MaxClassID {
		return fmt.Errorf("%s: class_id out of range", where)
	}
	if role.GetClassId() != 0 && role.GetActorType() != battle.EBattleActorType_BATTLE_ACTOR_TYPE_NONE &&
		role.GetActorType() != battle.EBattleActorType_BATTLE_ACTOR_TYPE_PLAYER {
		return fmt.Errorf("%s: class_id only applies to players", where)
	}
	if role.GetMonsterId() != 0 {
		if role.GetActorType() != battle.EBattleActorType_BATTLE_ACTOR_TYPE_NONE &&
			role.GetActorType() != battle.EBattleActorType_BATTLE_ACTOR_TYPE_MONSTER {
			return fmt.Errorf("%s: monster_id only applies to monsters", where)
		}
		if role.GetClassId() != 0 {
			return fmt.Errorf("%s: a role cannot require both a class and a monster", where)
		}
		if !tables.HasMonster(role.GetMonsterId()) {
			return fmt.Errorf("%s: unknown monster %d", where, role.GetMonsterId())
		}
	}
	if role.GetMaxMembers() > MaxRoleMembers {
		return fmt.Errorf("%s: max_members above %d", where, MaxRoleMembers)
	}
	if n := len(role.GetRules()); n == 0 || n > MaxRulesPerRole {
		return fmt.Errorf("%s: needs 1..%d rules", where, MaxRulesPerRole)
	}
	return nil
}

// Normalize 校验剧本中与「绑到哪一方」无关的部分(结构、上限、引用、条件 / 筛选 / 行动 × 目标组合),
// 通过后返回角色位按 order、规则按 priority 排好的副本;入参不改
func Normalize(script *battle.BattleScript, tables Tables) (*battle.BattleScript, error) {
	where := fmt.Sprintf("script %d", script.GetScriptId())
	if script.GetSchemaVersion() != SchemaVersion {
		return nil, fmt.Errorf("%s: unsupported schema_version %d", where, script.GetSchemaVersion())
	}
	if len(script.GetName()) > MaxNameBytes || len(script.GetDescription()) > MaxDescriptionBytes {
		return nil, fmt.Errorf("%s: name or description too long", where)
	}
	if n := len(script.GetRoles()); n == 0 || n > MaxRoles {
		return nil, fmt.Errorf("%s: needs 1..%d roles", where, MaxRoles)
	}

	// 先收齐角色位:规则里的 CLAIMED_BY_ROLE 可以引用排在后面的角色位
	roleIDs := make(map[uint32]bool, len(script.GetRoles()))
	roleOrders := make(map[uint32]bool, len(script.GetRoles()))
	for _, role := range script.GetRoles() {
		roleWhere := fmt.Sprintf("%s role %d", where, role.GetRoleId())
		if role.GetRoleId() == 0 || roleIDs[role.GetRoleId()] {
			return nil, fmt.Errorf("%s: role_id must be non-zero and unique", roleWhere)
		}
		roleIDs[role.GetRoleId()] = true
		if roleOrders[role.GetOrder()] {
			return nil, fmt.Errorf("%s: duplicate role order %d", roleWhere, role.GetOrder())
		}
		roleOrders[role.GetOrder()] = true
		if err := checkRoleHeader(role, tables, roleWhere); err != nil {
			return nil, err
		}
	}

	for _, role := range script.GetRoles() {
		roleWhere := fmt.Sprintf("%s role %d", where, role.GetRoleId())
		priorities := make(map[uint32]bool, len(role.GetRules()))
		for _, rule := range role.GetRules() {
			ruleWhere := fmt.Sprintf("%s rule %d", roleWhere, rule.GetPriority())
			if priorities[rule.GetPriority()] {
				return nil, fmt.Errorf("%s: duplicate rule priority", ruleWhere)
			}
			priorities[rule.GetPriority()] = true
			if len(rule.GetNote()) > MaxNoteBytes {
				return nil, fmt.Errorf("%s: note too long", ruleWhere)
			}
			if len(rule.GetConditions()) > MaxConditionsPerRule {
				return nil, fmt.Errorf("%s: at most %d conditions", ruleWhere, MaxConditionsPerRule)
			}
			for _, c := range rule.GetConditions() {
				if err := checkCondition(c, ruleWhere); err != nil {
					return nil, err
				}
			}
			if err := checkTarget(rule.GetTarget(), roleIDs, tables, ruleWhere); err != nil {
				return nil, err
			}
			if err := checkAction(role, rule, tables, ruleWhere); err != nil {
				return nil, err
			}
		}
	}

	// 规范化:集合顺序改由显式字段决定(AGENTS §11.6)。order / priority 已校验唯一,排序结果唯一
	normalized := proto.Clone(script).(*battle.BattleScript)
	sort.Slice(normalized.Roles, func(i, j int) bool { return normalized.Roles[i].GetOrder() < normalized.Roles[j].GetOrder() })
	for _, role := range normalized.Roles {
		sort.Slice(role.Rules, func(i, j int) bool { return role.Rules[i].GetPriority() < role.Rules[j].GetPriority() })
	}
	return normalized, nil
}

// CheckSide 报告剧本能不能用在这一方:怪物方只许认领怪物、只放零耗蓝的非职业技能;玩家方不许认领怪物。
// 剧本须已经过 Normalize
func CheckSide(script *battle.BattleScript, side battle.EBattleScriptSide, tables Tables) error {
	where := fmt.Sprintf("script %d", script.GetScriptId())
	if side != battle.EBattleScriptSide_BATTLE_SCRIPT_SIDE_PLAYER && side != battle.EBattleScriptSide_BATTLE_SCRIPT_SIDE_MONSTER {
		return fmt.Errorf("%s: side must be PLAYER or MONSTER", where)
	}
	for _, role := range script.GetRoles() {
		roleWhere := fmt.Sprintf("%s role %d", where, role.GetRoleId())
		if side == battle.EBattleScriptSide_BATTLE_SCRIPT_SIDE_PLAYER {
			if role.GetActorType() == battle.EBattleActorType_BATTLE_ACTOR_TYPE_MONSTER || role.GetMonsterId() != 0 {
				return fmt.Errorf("%s: player side roles cannot accept monsters", roleWhere)
			}
			continue
		}
		if role.GetActorType() != battle.EBattleActorType_BATTLE_ACTOR_TYPE_NONE &&
			role.GetActorType() != battle.EBattleActorType_BATTLE_ACTOR_TYPE_MONSTER {
			return fmt.Errorf("%s: monster side roles can only accept monsters", roleWhere)
		}
		if role.GetClassId() != 0 {
			return fmt.Errorf("%s: monster side roles cannot require a class", roleWhere)
		}
		for _, rule := range role.GetRules() {
			if rule.GetAction() != battle.EBattleActionType_BATTLE_ACTION_SKILL {
				continue
			}
			ruleWhere := fmt.Sprintf("%s rule %d", roleWhere, rule.GetPriority())
			row, ok := tables.Skill(rule.GetSkillId())
			// 怪物没有职业、也没有法力(Monster 表无法力列):只能放零耗蓝的老技能
			if !ok || row.GetClassId() != 0 {
				return fmt.Errorf("%s: monsters can only cast legacy skills", ruleWhere)
			}
			if legacyManaCost(row) != 0 {
				return fmt.Errorf("%s: monsters have no mana but skill %d costs mana", ruleWhere, rule.GetSkillId())
			}
		}
	}
	return nil
}

// IsPveMatchMode 报告这个 MatchMode 有没有怪物方
func IsPveMatchMode(matchMode uint32) bool {
	return matchMode == matchModePveSolo || matchMode == matchModePveTeam
}
