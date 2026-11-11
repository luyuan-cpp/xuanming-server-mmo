#include "system/battle_script.h"

#include <algorithm>
#include <iterator>
#include <limits>
#include <set>
#include <utility>

#include "constants/turn_battle_constants.h"
#include "system/class_skill_rules.h"
#include "system/turn_battle_engine.h"

namespace turnbattle {

namespace {

uint64_t SaturatingMul(uint64_t lhs, uint64_t rhs) {
    if (lhs != 0 && rhs > std::numeric_limits<uint64_t>::max() / lhs) {
        return std::numeric_limits<uint64_t>::max();
    }
    return lhs * rhs;
}

// 剧本能当「状态」判断的 buff 类型:回合引擎里挂得住、会生效的持续类(turn_battle_constants.h)
constexpr uint32_t kScriptStatusBuffTypes[] = {
    kBuffTypeIncreaseAttack, kBuffTypeIncreaseDefense, kBuffTypeMovementSpeedBoost,
    kBuffTypeStun,           kBuffTypeSilence,         kBuffTypeInvincibility,
    kBuffTypeImmunity,       kBuffTypeHealthRegeneration, kBuffTypeManaRegeneration,
    kBuffTypeHealthRegenerationBasedOnLostHealth, kBuffTypePoison, kBuffTypeBurn,
    kBuffTypeFreeze,
};

std::optional<BattleScript> Reject(std::string& error, std::string message) {
    error = std::move(message);
    return std::nullopt;
}

bool Fail(std::string& error, std::string message) {
    error = std::move(message);
    return false;
}

bool InRange(uint32_t value, uint32_t low, uint32_t high) {
    return value >= low && value <= high;
}

std::string Num(uint64_t value) {
    return std::to_string(value);
}

bool CheckCondition(const BattleScriptCondition& condition, const std::string& where, std::string& error) {
    switch (condition.kind()) {
    case BATTLE_SCRIPT_COND_SELF_HP_BELOW:
        if (!InRange(condition.p1(), 1, kPermyriadFull) || condition.p2() != 0) {
            return Fail(error, where + ": SELF_HP_BELOW needs p1 in 1..10000 and p2 = 0");
        }
        return true;
    case BATTLE_SCRIPT_COND_ROUND_AT_LEAST:
        if (!InRange(condition.p1(), 1, kMaxScriptRound) || condition.p2() != 0) {
            return Fail(error, where + ": ROUND_AT_LEAST needs p1 in 1.." + Num(kMaxScriptRound) + " and p2 = 0");
        }
        return true;
    case BATTLE_SCRIPT_COND_ROUND_EVERY:
        if (!InRange(condition.p1(), 1, kMaxScriptRound) || !InRange(condition.p2(), 1, kMaxScriptRound)) {
            return Fail(error, where + ": ROUND_EVERY needs period p1 and first round p2 in 1.." + Num(kMaxScriptRound));
        }
        return true;
    case BATTLE_SCRIPT_COND_ENEMY_COUNT_AT_LEAST:
        if (!InRange(condition.p1(), 1, kMaxScriptUnitCount) || condition.p2() != 0) {
            return Fail(error, where + ": ENEMY_COUNT_AT_LEAST needs p1 in 1.." + Num(kMaxScriptUnitCount) +
                                   " and p2 = 0");
        }
        return true;
    default:
        return Fail(error, where + ": unknown condition kind " + Num(static_cast<uint32_t>(condition.kind())));
    }
}

bool CheckFilter(const BattleScriptTargetFilter& filter, const std::set<uint32_t>& roleIds,
                 const BattleDataProvider& data, const std::string& where, std::string& error) {
    const uint32_t p1 = filter.p1();
    switch (filter.kind()) {
    case BATTLE_SCRIPT_FILTER_HP_BELOW:
        return InRange(p1, 1, kPermyriadFull) ? true : Fail(error, where + ": HP_BELOW needs p1 in 1..10000");
    case BATTLE_SCRIPT_FILTER_HAS_BUFF_TYPE:
        return IsScriptStatusBuffType(p1) ? true
                                          : Fail(error, where + ": HAS_BUFF_TYPE " + Num(p1) + " is not a status type");
    case BATTLE_SCRIPT_FILTER_HAS_BUFF:
        return p1 != 0 && data.FindBuff(p1) != nullptr ? true : Fail(error, where + ": unknown buff " + Num(p1));
    case BATTLE_SCRIPT_FILTER_IS_MONSTER:
        return p1 != 0 && data.FindMonster(p1) != nullptr ? true : Fail(error, where + ": unknown monster " + Num(p1));
    case BATTLE_SCRIPT_FILTER_IS_CLASS:
        return InRange(p1, 1, kMaxScriptClassId) ? true : Fail(error, where + ": IS_CLASS needs a class id");
    case BATTLE_SCRIPT_FILTER_IS_ACTOR_TYPE:
        return p1 == static_cast<uint32_t>(BATTLE_ACTOR_TYPE_PLAYER) ||
                       p1 == static_cast<uint32_t>(BATTLE_ACTOR_TYPE_MONSTER) ||
                       p1 == static_cast<uint32_t>(BATTLE_ACTOR_TYPE_PET)
                   ? true
                   : Fail(error, where + ": IS_ACTOR_TYPE needs 1 (player), 2 (monster) or 3 (pet)");
    case BATTLE_SCRIPT_FILTER_CLAIMED_BY_ROLE:
        return p1 == 0 || roleIds.count(p1) != 0 ? true
                                                 : Fail(error, where + ": CLAIMED_BY_ROLE refers to unknown role " + Num(p1));
    default:
        return Fail(error, where + ": unknown filter kind " + Num(static_cast<uint32_t>(filter.kind())));
    }
}

bool CheckTarget(const BattleScriptTarget& target, const std::set<uint32_t>& roleIds, const BattleDataProvider& data,
                 const std::string& where, std::string& error) {
    const auto side = target.side();
    if (side != BATTLE_SCRIPT_TARGET_SELF && side != BATTLE_SCRIPT_TARGET_ENEMY &&
        side != BATTLE_SCRIPT_TARGET_ALLY && side != BATTLE_SCRIPT_TARGET_ENGINE_PICK) {
        return Fail(error, where + ": target side must be SELF, ENEMY, ALLY or ENGINE_PICK");
    }
    if (!eBattleScriptTargetOrder_IsValid(target.order())) {
        return Fail(error, where + ": unknown target order " + Num(static_cast<uint32_t>(target.order())));
    }
    if (target.filters_size() > kMaxScriptFiltersPerRule) {
        return Fail(error, where + ": at most " + Num(kMaxScriptFiltersPerRule) + " target filters");
    }
    // 自己 / 交给引擎选:没有候选可筛、可排,带了只会让人误以为生效
    if (side != BATTLE_SCRIPT_TARGET_ENEMY && side != BATTLE_SCRIPT_TARGET_ALLY &&
        (target.filters_size() != 0 || target.order() != BATTLE_SCRIPT_ORDER_FORMATION)) {
        return Fail(error, where + ": SELF / ENGINE_PICK targets take no filters and no order");
    }
    for (const auto& filter : target.filters()) {
        if (!CheckFilter(filter, roleIds, data, where, error)) {
            return false;
        }
    }
    return true;
}

// 行动 × 目标组合。剧本选出的行动出手前还要过 CheckActionPrerequisites,这里拦的是「写错了就永远不会生效」
// 和「生效了会打错边」两类配置错误
bool CheckAction(const BattleScriptRole& role, const BattleScriptRule& rule, const BattleDataProvider& data,
                 const std::string& where, std::string& error) {
    const auto side = rule.target().side();
    switch (rule.action()) {
    case BATTLE_ACTION_ATTACK:
        if (rule.skill_id() != 0) {
            return Fail(error, where + ": ATTACK carries no skill_id");
        }
        if (side != BATTLE_SCRIPT_TARGET_ENEMY && side != BATTLE_SCRIPT_TARGET_ENGINE_PICK) {
            return Fail(error, where + ": ATTACK targets ENEMY or ENGINE_PICK");
        }
        return true;
    case BATTLE_ACTION_DEFEND:
        if (rule.skill_id() != 0) {
            return Fail(error, where + ": DEFEND carries no skill_id");
        }
        if (side != BATTLE_SCRIPT_TARGET_SELF) {
            return Fail(error, where + ": DEFEND targets SELF");
        }
        return true;
    case BATTLE_ACTION_SKILL: {
        if (rule.skill_id() == 0) {
            return Fail(error, where + ": SKILL needs skill_id");
        }
        const auto* row = data.FindSkill(rule.skill_id());
        if (row == nullptr) {
            return Fail(error, where + ": unknown skill " + Num(rule.skill_id()));
        }
        if (!TurnBattleEngine::IsTurnBattleCastableSkill(*row)) {
            return Fail(error, where + ": skill " + Num(rule.skill_id()) +
                                   " cannot be cast in turn battles (passive, toggle or channel)");
        }
        if (row->class_id() == 0) {
            // 老技能没有敌我语义(ValidateSkillTarget 不分阵营,伤害照打):只许打敌方,免得剧本把伤害打到自己人身上
            return side == BATTLE_SCRIPT_TARGET_ENEMY
                       ? true
                       : Fail(error, where + ": legacy skill " + Num(rule.skill_id()) + " can only target ENEMY");
        }
        // 职业技能只有本职业的玩家放得出来:角色位必须可能认领到这样的玩家
        if (role.actor_type() != BATTLE_ACTOR_TYPE_NONE && role.actor_type() != BATTLE_ACTOR_TYPE_PLAYER) {
            return Fail(error, where + ": class skill " + Num(rule.skill_id()) + " needs a role that accepts players");
        }
        if (role.class_id() != 0 && role.class_id() != row->class_id()) {
            return Fail(error, where + ": class skill " + Num(rule.skill_id()) + " belongs to class " +
                                   Num(row->class_id()) + " but the role only accepts class " + Num(role.class_id()));
        }
        if (side == BATTLE_SCRIPT_TARGET_ENGINE_PICK) {
            return true;
        }
        const auto team = static_cast<classskill::TargetTeam>(row->target_team());
        const bool sideMatches = (team == classskill::TargetTeam::kEnemy && side == BATTLE_SCRIPT_TARGET_ENEMY) ||
                                 (team == classskill::TargetTeam::kAlly && side == BATTLE_SCRIPT_TARGET_ALLY) ||
                                 (team == classskill::TargetTeam::kSelf && side == BATTLE_SCRIPT_TARGET_SELF);
        return sideMatches ? true
                           : Fail(error, where + ": class skill " + Num(rule.skill_id()) +
                                             " target side does not match its target_team " + Num(row->target_team()));
    }
    default:
        return Fail(error, where + ": action must be ATTACK, SKILL or DEFEND");
    }
}

}  // namespace

bool RatioBelowPermyriad(uint64_t value, uint64_t max, uint32_t permyriad) {
    if (max == 0) {
        return false;
    }
    return SaturatingMul(value, kPermyriadFull) < SaturatingMul(max, permyriad);
}

bool RatioLess(uint64_t lhsValue, uint64_t lhsMax, uint64_t rhsValue, uint64_t rhsMax) {
    if (lhsMax == 0) {
        lhsValue = 1;
        lhsMax = 1;
    }
    if (rhsMax == 0) {
        rhsValue = 1;
        rhsMax = 1;
    }
    return SaturatingMul(lhsValue, rhsMax) < SaturatingMul(rhsValue, lhsMax);
}

bool IsScriptStatusBuffType(uint32_t buffType) {
    return std::find(std::begin(kScriptStatusBuffTypes), std::end(kScriptStatusBuffTypes), buffType) !=
           std::end(kScriptStatusBuffTypes);
}

std::optional<BattleScript> NormalizeBattleScript(const BattleScript& script, const BattleDataProvider& data,
                                                  std::string& error) {
    const std::string scriptWhere = "script " + Num(script.script_id());
    if (script.schema_version() != kBattleScriptSchemaVersion) {
        return Reject(error, scriptWhere + ": unsupported schema_version " + Num(script.schema_version()));
    }
    if (script.name().size() > kMaxScriptNameBytes || script.description().size() > kMaxScriptDescriptionBytes) {
        return Reject(error, scriptWhere + ": name or description too long");
    }
    if (script.roles_size() == 0 || script.roles_size() > kMaxScriptRoles) {
        return Reject(error, scriptWhere + ": needs 1.." + Num(kMaxScriptRoles) + " roles");
    }

    // 先收齐角色位:规则里的 CLAIMED_BY_ROLE 可以引用排在后面的角色位
    std::set<uint32_t> roleIds;
    std::set<uint32_t> roleOrders;
    for (const auto& role : script.roles()) {
        const std::string roleWhere = scriptWhere + " role " + Num(role.role_id());
        if (role.role_id() == 0 || !roleIds.insert(role.role_id()).second) {
            return Reject(error, roleWhere + ": role_id must be non-zero and unique");
        }
        if (!roleOrders.insert(role.order()).second) {
            return Reject(error, roleWhere + ": duplicate role order " + Num(role.order()));
        }
        if (role.name().size() > kMaxScriptNameBytes) {
            return Reject(error, roleWhere + ": name too long");
        }
        if (!eBattleActorType_IsValid(role.actor_type())) {
            return Reject(error, roleWhere + ": unknown actor_type");
        }
        if (role.class_id() > kMaxScriptClassId) {
            return Reject(error, roleWhere + ": class_id out of range");
        }
        if (role.class_id() != 0 && role.actor_type() != BATTLE_ACTOR_TYPE_NONE &&
            role.actor_type() != BATTLE_ACTOR_TYPE_PLAYER) {
            return Reject(error, roleWhere + ": class_id only applies to players");
        }
        if (role.monster_id() != 0) {
            if (role.actor_type() != BATTLE_ACTOR_TYPE_NONE && role.actor_type() != BATTLE_ACTOR_TYPE_MONSTER) {
                return Reject(error, roleWhere + ": monster_id only applies to monsters");
            }
            if (role.class_id() != 0) {
                return Reject(error, roleWhere + ": a role cannot require both a class and a monster");
            }
            if (data.FindMonster(role.monster_id()) == nullptr) {
                return Reject(error, roleWhere + ": unknown monster " + Num(role.monster_id()));
            }
        }
        if (role.max_members() > kMaxScriptRoleMembers) {
            return Reject(error, roleWhere + ": max_members above " + Num(kMaxScriptRoleMembers));
        }
        if (role.rules_size() == 0 || role.rules_size() > kMaxScriptRulesPerRole) {
            return Reject(error, roleWhere + ": needs 1.." + Num(kMaxScriptRulesPerRole) + " rules");
        }
    }

    for (const auto& role : script.roles()) {
        const std::string roleWhere = scriptWhere + " role " + Num(role.role_id());
        std::set<uint32_t> priorities;
        for (const auto& rule : role.rules()) {
            const std::string ruleWhere = roleWhere + " rule " + Num(rule.priority());
            if (!priorities.insert(rule.priority()).second) {
                return Reject(error, ruleWhere + ": duplicate rule priority");
            }
            if (rule.note().size() > kMaxScriptNoteBytes) {
                return Reject(error, ruleWhere + ": note too long");
            }
            if (rule.conditions_size() > kMaxScriptConditionsPerRule) {
                return Reject(error, ruleWhere + ": at most " + Num(kMaxScriptConditionsPerRule) + " conditions");
            }
            for (const auto& condition : rule.conditions()) {
                if (!CheckCondition(condition, ruleWhere, error)) {
                    return std::nullopt;
                }
            }
            if (!CheckTarget(rule.target(), roleIds, data, ruleWhere, error) ||
                !CheckAction(role, rule, data, ruleWhere, error)) {
                return std::nullopt;
            }
        }
    }

    // 规范化:集合顺序改由显式字段决定(AGENTS §11.6)。order / priority 已校验唯一,排序结果唯一
    BattleScript normalized = script;
    auto* roles = normalized.mutable_roles();
    std::sort(roles->begin(), roles->end(),
              [](const BattleScriptRole& lhs, const BattleScriptRole& rhs) { return lhs.order() < rhs.order(); });
    for (auto& role : *roles) {
        auto* rules = role.mutable_rules();
        std::sort(rules->begin(), rules->end(), [](const BattleScriptRule& lhs, const BattleScriptRule& rhs) {
            return lhs.priority() < rhs.priority();
        });
    }
    return normalized;
}

bool CheckBattleScriptSide(const BattleScript& script, eBattleScriptSide side, const BattleDataProvider& data,
                           std::string& error) {
    const std::string scriptWhere = "script " + Num(script.script_id());
    if (side != BATTLE_SCRIPT_SIDE_PLAYER && side != BATTLE_SCRIPT_SIDE_MONSTER) {
        return Fail(error, scriptWhere + ": side must be PLAYER or MONSTER");
    }
    for (const auto& role : script.roles()) {
        const std::string roleWhere = scriptWhere + " role " + Num(role.role_id());
        if (side == BATTLE_SCRIPT_SIDE_PLAYER) {
            if (role.actor_type() == BATTLE_ACTOR_TYPE_MONSTER || role.monster_id() != 0) {
                return Fail(error, roleWhere + ": player side roles cannot accept monsters");
            }
            continue;
        }
        if (role.actor_type() != BATTLE_ACTOR_TYPE_NONE && role.actor_type() != BATTLE_ACTOR_TYPE_MONSTER) {
            return Fail(error, roleWhere + ": monster side roles can only accept monsters");
        }
        if (role.class_id() != 0) {
            return Fail(error, roleWhere + ": monster side roles cannot require a class");
        }
        for (const auto& rule : role.rules()) {
            if (rule.action() != BATTLE_ACTION_SKILL) {
                continue;
            }
            const auto* row = data.FindSkill(rule.skill_id());
            const std::string ruleWhere = roleWhere + " rule " + Num(rule.priority());
            // 怪物没有职业、也没有法力(Monster 表无法力列,开局法力为 0):只能放零耗蓝的老技能
            if (row == nullptr || row->class_id() != 0) {
                return Fail(error, ruleWhere + ": monsters can only cast legacy skills");
            }
            if (TurnBattleEngine::LegacySkillManaCost(*row) != 0) {
                return Fail(error, ruleWhere + ": monsters have no mana but skill " + Num(rule.skill_id()) +
                                       " costs mana");
            }
        }
    }
    return true;
}

}  // namespace turnbattle
