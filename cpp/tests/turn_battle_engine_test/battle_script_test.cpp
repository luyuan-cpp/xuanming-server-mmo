#include <gtest/gtest.h>

#include <cstdint>
#include <functional>
#include <limits>
#include <memory>
#include <string>
#include <utility>
#include <vector>

#include "constants/turn_battle_constants.h"
#include "memory_battle_data_provider.h"
#include "system/battle_script.h"
#include "system/class_skill_rules.h"
#include "system/turn_battle_engine.h"

// 战斗剧本(docs/design/battle-script.md)的开局复核与引擎用例。
// 剧本由 match 冻结进 CreateBattleRequest.team_scripts,这里直接往请求里放剧本,覆盖:
//   * 纯规则:比例判定、规范化(排序)、各类非法剧本被拒、怪物方 / 玩家方的边界;
//   * 引擎:封 / 冰 / 毒按角色位分工与组合(同回合跟随队友选的目标)、手动行动保留并计入认领、
//     超时玩家按剧本出手、同角色位不重复封同一个、怪物剧本派生技能集并集火、职业治疗、冷却回落下一条、
//     条件按回合生效、规则全落空回到现行默认、非法剧本被忽略且事件流与没有剧本时逐字节相同、同种子确定性。

namespace {

using turnbattle::TurnBattleEngine;

constexpr uint32_t kPveTeamMode = 5;  // MATCH_MODE_PVE_TEAM
constexpr uint32_t kDungeon = 31;
constexpr uint32_t kMinionTable = 101;
constexpr uint32_t kBossTable = 102;
constexpr uint64_t kMinion = turnbattle::kMonsterActorIdBase + 0;  // 怪物组 [101, 102] 按序生成
constexpr uint64_t kBoss = turnbattle::kMonsterActorIdBase + 1;

constexpr uint64_t kSealer = 8001;
constexpr uint64_t kIcer = 8002;
constexpr uint64_t kPoisoner = 8003;
constexpr uint64_t kStriker = 8004;
constexpr uint64_t kSealer2 = 8005;
constexpr uint64_t kHealer = 8006;

constexpr uint32_t kSealClass = 11;
constexpr uint32_t kIceClass = 12;
constexpr uint32_t kPoisonClass = 13;
constexpr uint32_t kStrikeClass = 14;
constexpr uint32_t kHealClass = 3;

constexpr uint32_t kSeal = 9501;        // 老技能:单体,挂眩晕(封)
constexpr uint32_t kFreeze = 9502;      // 老技能:单体,挂冰冻
constexpr uint32_t kPoison = 9503;      // 老技能:单体,挂中毒
constexpr uint32_t kSlam = 9504;        // 老技能:单体伤害,零耗蓝(怪物可放)
constexpr uint32_t kCostly = 9505;      // 老技能:耗蓝 10(怪物放不了)
constexpr uint32_t kGuarded = 9506;     // 老技能:冷却 3 回合
constexpr uint32_t kPassive = 9507;     // 被动技能(回合制放不了)
constexpr uint32_t kMend = 9601;        // 职业 3 的友方单体治疗
constexpr uint32_t kBuffStun = 9930;
constexpr uint32_t kBuffFreeze = 9952;
constexpr uint32_t kBuffPoison = 9950;
constexpr uint32_t kGuardedCooldown = 77;

// ---------------------------------------------------------------------------
// 数据与剧本拼装
// ---------------------------------------------------------------------------

SkillTable& AddLegacySkill(MemoryBattleDataProvider& provider, uint32_t skillId) {
    auto& row = provider.AddSkill(skillId);
    row.add_targeting_mode(1);  // 位号 1 = 单体指定目标
    row.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    return row;
}

BuffTable& AddStatusBuff(MemoryBattleDataProvider& provider, uint32_t buffId, uint32_t buffType) {
    auto& row = provider.AddBuff(buffId);
    row.set_buff_type(buffType);
    row.set_duration(18.0);  // 3 回合:第 2 回合还挂着
    return row;
}

std::shared_ptr<MemoryBattleDataProvider> MakeProvider() {
    auto provider = std::make_shared<MemoryBattleDataProvider>();
    provider->AddBattleReward(0);

    AddLegacySkill(*provider, kSeal).add_effect(kBuffStun);
    AddLegacySkill(*provider, kFreeze).add_effect(kBuffFreeze);
    AddLegacySkill(*provider, kPoison).add_effect(kBuffPoison);
    AddLegacySkill(*provider, kSlam);
    provider->SetSkillDamage(kSlam, 30.0);
    auto& costly = AddLegacySkill(*provider, kCostly);
    auto* cost = costly.add_cost_resource();
    cost->set_cost_resource_id(turnbattle::kSkillCostResourceMana);
    cost->set_cost_resource_cost(10);
    AddLegacySkill(*provider, kGuarded).set_cooldown_id(kGuardedCooldown);
    provider->SetCooldownMs(kGuardedCooldown, 18000);  // 3 回合
    auto& passive = provider->AddSkill(kPassive);
    passive.add_skill_type(turnbattle::kSkillTypeBitPassive);

    auto& mend = provider->AddSkill(kMend);
    mend.set_class_id(kHealClass);
    mend.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    mend.set_target_team(static_cast<uint32_t>(classskill::TargetTeam::kAlly));
    mend.set_max_targets(1);
    mend.set_unlock_level(1);
    mend.set_max_rank(10);
    mend.set_heal_multiplier(1.0);
    mend.set_heal_per_level(8.0);

    AddStatusBuff(*provider, kBuffStun, turnbattle::kBuffTypeStun);
    AddStatusBuff(*provider, kBuffFreeze, turnbattle::kBuffTypeFreeze);
    AddStatusBuff(*provider, kBuffPoison, turnbattle::kBuffTypePoison);

    auto& minion = provider->AddMonster(kMinionTable);
    minion.set_health(1000);
    minion.set_strength(20);
    minion.set_speed(100);  // 比玩家慢:玩家先出手
    auto& boss = provider->AddMonster(kBossTable);
    boss.set_health(3000);
    boss.set_strength(20);
    boss.set_speed(100);
    provider->AddDungeon(kDungeon);
    provider->SetDungeonMonsters(kDungeon, {kMinionTable, kBossTable});
    return provider;
}

CreateBattleRequest MakeRequest(uint64_t battleId = 77001) {
    CreateBattleRequest request;
    request.set_battle_id(battleId);
    request.set_battle_config_id(kDungeon);
    request.set_match_mode(kPveTeamMode);
    request.set_seed(20261111);
    return request;
}

BattlePlayerSnapshot* AddPlayer(CreateBattleRequest& request, uint64_t playerId, uint32_t classId, uint64_t speed,
                                std::vector<uint32_t> skills = {}) {
    auto* snapshot = request.add_players();
    snapshot->set_player_id(playerId);
    snapshot->set_player_name("剧本");
    snapshot->set_level(30);
    snapshot->set_class_id(classId);
    snapshot->set_team_index(0);
    snapshot->set_max_health(1000);
    snapshot->set_max_mana(100);
    auto* attributes = snapshot->mutable_base_attributes();
    attributes->set_health(1000);
    attributes->set_mana(100);
    attributes->set_speed(speed);
    attributes->set_strength(10);
    snapshot->set_physical_attack(50);
    for (const auto skillId : skills) {
        snapshot->add_skill_table_ids(skillId);
    }
    return snapshot;
}

BattleScriptTargetFilter Filter(eBattleScriptTargetFilter kind, uint32_t p1, bool negate = false) {
    BattleScriptTargetFilter filter;
    filter.set_kind(kind);
    filter.set_p1(p1);
    filter.set_negate(negate);
    return filter;
}

BattleScriptCondition Condition(eBattleScriptCondition kind, uint32_t p1, uint32_t p2 = 0, bool negate = false) {
    BattleScriptCondition condition;
    condition.set_kind(kind);
    condition.set_p1(p1);
    condition.set_p2(p2);
    condition.set_negate(negate);
    return condition;
}

BattleScriptRole* AddRole(BattleScript& script, uint32_t roleId, uint32_t order, eBattleActorType actorType,
                          uint32_t classId = 0, uint32_t monsterId = 0) {
    auto* role = script.add_roles();
    role->set_role_id(roleId);
    role->set_order(order);
    role->set_actor_type(actorType);
    role->set_class_id(classId);
    role->set_monster_id(monsterId);
    return role;
}

BattleScriptRule* AddRule(BattleScriptRole* role, uint32_t priority, eBattleActionType action, uint32_t skillId,
                          eBattleScriptTargetSide side, std::vector<BattleScriptTargetFilter> filters = {},
                          eBattleScriptTargetOrder order = BATTLE_SCRIPT_ORDER_FORMATION,
                          std::vector<BattleScriptCondition> conditions = {}) {
    auto* rule = role->add_rules();
    rule->set_priority(priority);
    rule->set_action(action);
    rule->set_skill_id(skillId);
    rule->mutable_target()->set_side(side);
    rule->mutable_target()->set_order(order);
    for (auto& filter : filters) {
        *rule->mutable_target()->add_filters() = std::move(filter);
    }
    for (auto& condition : conditions) {
        *rule->add_conditions() = std::move(condition);
    }
    return rule;
}

BattleScript NewScript() {
    BattleScript script;
    script.set_schema_version(turnbattle::kBattleScriptSchemaVersion);
    script.set_script_id(1);
    script.set_name("测试剧本");
    return script;
}

void AttachScript(CreateBattleRequest& request, uint32_t teamIndex, const BattleScript& script) {
    auto* teamScript = request.add_team_scripts();
    teamScript->set_team_index(teamIndex);
    *teamScript->mutable_script() = script;
    teamScript->set_origin(BATTLE_SCRIPT_ORIGIN_OFFICIAL);
}

// 封 / 冰 / 毒 / 输出四个角色位:封系先封 Boss;冰系冰「封系没选的」;毒系跟着冰系的目标下毒(组合);
// 输出集火冰系的目标
BattleScript ComboScript() {
    BattleScript script = NewScript();
    auto* sealRole = AddRole(script, 1, 10, BATTLE_ACTOR_TYPE_PLAYER, kSealClass);
    AddRule(sealRole, 10, BATTLE_ACTION_SKILL, kSeal, BATTLE_SCRIPT_TARGET_ENEMY,
            {Filter(BATTLE_SCRIPT_FILTER_IS_MONSTER, kBossTable),
             Filter(BATTLE_SCRIPT_FILTER_HAS_BUFF_TYPE, turnbattle::kBuffTypeStun, true)});
    AddRule(sealRole, 20, BATTLE_ACTION_ATTACK, 0, BATTLE_SCRIPT_TARGET_ENEMY, {}, BATTLE_SCRIPT_ORDER_HP_LOWEST);

    auto* iceRole = AddRole(script, 2, 20, BATTLE_ACTOR_TYPE_PLAYER, kIceClass);
    AddRule(iceRole, 10, BATTLE_ACTION_SKILL, kFreeze, BATTLE_SCRIPT_TARGET_ENEMY,
            {Filter(BATTLE_SCRIPT_FILTER_CLAIMED_BY_ROLE, 1, true),
             Filter(BATTLE_SCRIPT_FILTER_HAS_BUFF_TYPE, turnbattle::kBuffTypeFreeze, true)});
    AddRule(iceRole, 20, BATTLE_ACTION_ATTACK, 0, BATTLE_SCRIPT_TARGET_ENEMY);

    auto* poisonRole = AddRole(script, 3, 30, BATTLE_ACTOR_TYPE_PLAYER, kPoisonClass);
    AddRule(poisonRole, 10, BATTLE_ACTION_SKILL, kPoison, BATTLE_SCRIPT_TARGET_ENEMY,
            {Filter(BATTLE_SCRIPT_FILTER_CLAIMED_BY_ROLE, 2),
             Filter(BATTLE_SCRIPT_FILTER_HAS_BUFF_TYPE, turnbattle::kBuffTypePoison, true)});
    AddRule(poisonRole, 20, BATTLE_ACTION_ATTACK, 0, BATTLE_SCRIPT_TARGET_ENGINE_PICK);

    auto* strikeRole = AddRole(script, 4, 40, BATTLE_ACTOR_TYPE_PLAYER);
    AddRule(strikeRole, 10, BATTLE_ACTION_ATTACK, 0, BATTLE_SCRIPT_TARGET_ENEMY,
            {Filter(BATTLE_SCRIPT_FILTER_CLAIMED_BY_ROLE, 2)});
    AddRule(strikeRole, 20, BATTLE_ACTION_ATTACK, 0, BATTLE_SCRIPT_TARGET_ENEMY, {}, BATTLE_SCRIPT_ORDER_HP_LOWEST);
    return script;
}

CreateBattleRequest ComboRequest() {
    auto request = MakeRequest();
    AddPlayer(request, kSealer, kSealClass, 530, {kSeal});
    AddPlayer(request, kIcer, kIceClass, 520, {kFreeze});
    AddPlayer(request, kPoisoner, kPoisonClass, 510, {kPoison});
    AddPlayer(request, kStriker, kStrikeClass, 500);
    AttachScript(request, 0, ComboScript());
    return request;
}

void SetAllAuto(TurnBattleEngine& engine, const CreateBattleRequest& request) {
    for (const auto& snapshot : request.players()) {
        ASSERT_EQ(engine.SetActorAuto(snapshot.player_id(), true), 0u);
    }
}

// 某单位本回合唯一一次出手(ATTACK / SKILL 事件);找不到返回 nullptr
const BattleEventItem* FindAction(const TurnResultS2C& result, uint64_t sourceId) {
    for (const auto& event : result.events()) {
        if (event.source_id() == sourceId &&
            (event.event_type() == BATTLE_EVENT_ATTACK || event.event_type() == BATTLE_EVENT_SKILL ||
             event.event_type() == BATTLE_EVENT_DEFEND)) {
            return &event;
        }
    }
    return nullptr;
}

bool HasBuffAdd(const TurnResultS2C& result, uint64_t targetId, uint32_t buffId) {
    for (const auto& event : result.events()) {
        if (event.event_type() == BATTLE_EVENT_BUFF_ADD && event.target_id() == targetId &&
            event.buff_table_id() == buffId) {
            return true;
        }
    }
    return false;
}

const BattleActorState* FindStateActor(const BattleStateS2C& state, uint64_t actorId) {
    for (const auto& actor : state.actors()) {
        if (actor.actor_id() == actorId) {
            return &actor;
        }
    }
    return nullptr;
}

// ---------------------------------------------------------------------------
// 纯规则
// ---------------------------------------------------------------------------

TEST(BattleScriptRulesTest, RatioBelowPermyriadIsIntegerExact) {
    EXPECT_TRUE(turnbattle::RatioBelowPermyriad(29, 100, 3000));
    EXPECT_FALSE(turnbattle::RatioBelowPermyriad(30, 100, 3000));  // 恰好 30% 不算「低于」
    EXPECT_FALSE(turnbattle::RatioBelowPermyriad(0, 0, 10000));    // 没有上限按满算
    EXPECT_TRUE(turnbattle::RatioBelowPermyriad(0, 1, 1));
    // 乘法饱和、不回绕:满值不算低于 100%
    constexpr auto kMax = std::numeric_limits<uint64_t>::max();
    EXPECT_FALSE(turnbattle::RatioBelowPermyriad(kMax, kMax, 10000));
}

TEST(BattleScriptRulesTest, RatioLessComparesByCrossMultiplication) {
    EXPECT_TRUE(turnbattle::RatioLess(1, 3, 1, 2));
    EXPECT_FALSE(turnbattle::RatioLess(1, 2, 1, 3));
    EXPECT_FALSE(turnbattle::RatioLess(2, 4, 1, 2));  // 相等不算更小
    EXPECT_TRUE(turnbattle::RatioLess(99, 100, 0, 0));  // 上限 0 视为满(比例 1)
    EXPECT_FALSE(turnbattle::RatioLess(5, 0, 99, 100));
}

TEST(BattleScriptRulesTest, StatusBuffTypesExcludeInstantDispel) {
    EXPECT_TRUE(turnbattle::IsScriptStatusBuffType(turnbattle::kBuffTypeStun));
    EXPECT_TRUE(turnbattle::IsScriptStatusBuffType(turnbattle::kBuffTypeFreeze));
    EXPECT_TRUE(turnbattle::IsScriptStatusBuffType(turnbattle::kBuffTypePoison));
    EXPECT_FALSE(turnbattle::IsScriptStatusBuffType(turnbattle::kBuffTypeDispel));
    EXPECT_FALSE(turnbattle::IsScriptStatusBuffType(0));
    EXPECT_FALSE(turnbattle::IsScriptStatusBuffType(999));
}

TEST(BattleScriptValidationTest, NormalizeSortsRolesByOrderAndRulesByPriority) {
    auto provider = MakeProvider();
    BattleScript script = NewScript();
    auto* late = AddRole(script, 7, 50, BATTLE_ACTOR_TYPE_PLAYER);
    AddRule(late, 30, BATTLE_ACTION_ATTACK, 0, BATTLE_SCRIPT_TARGET_ENGINE_PICK);
    AddRule(late, 10, BATTLE_ACTION_DEFEND, 0, BATTLE_SCRIPT_TARGET_SELF);
    auto* early = AddRole(script, 3, 5, BATTLE_ACTOR_TYPE_PLAYER, kSealClass);
    AddRule(early, 1, BATTLE_ACTION_SKILL, kSeal, BATTLE_SCRIPT_TARGET_ENEMY);

    std::string error;
    const auto normalized = turnbattle::NormalizeBattleScript(script, *provider, error);
    ASSERT_TRUE(normalized.has_value()) << error;
    ASSERT_EQ(normalized->roles_size(), 2);
    EXPECT_EQ(normalized->roles(0).role_id(), 3u);
    EXPECT_EQ(normalized->roles(1).role_id(), 7u);
    EXPECT_EQ(normalized->roles(1).rules(0).priority(), 10u);
    EXPECT_EQ(normalized->roles(1).rules(1).priority(), 30u);
}

TEST(BattleScriptValidationTest, RejectsMalformedScripts) {
    auto provider = MakeProvider();
    // 合法基线:每个用例只改一处
    const auto baseline = [] {
        BattleScript script = NewScript();
        auto* role = AddRole(script, 1, 10, BATTLE_ACTOR_TYPE_PLAYER, kSealClass);
        AddRule(role, 10, BATTLE_ACTION_SKILL, kSeal, BATTLE_SCRIPT_TARGET_ENEMY,
                {Filter(BATTLE_SCRIPT_FILTER_HAS_BUFF_TYPE, turnbattle::kBuffTypeStun, true)});
        return script;
    };
    {
        std::string error;
        ASSERT_TRUE(turnbattle::NormalizeBattleScript(baseline(), *provider, error).has_value()) << error;
    }

    const std::vector<std::pair<std::string, std::function<void(BattleScript&)>>> cases = {
        {"schema version", [](BattleScript& s) { s.set_schema_version(2); }},
        {"no roles", [](BattleScript& s) { s.clear_roles(); }},
        {"zero role id", [](BattleScript& s) { s.mutable_roles(0)->set_role_id(0); }},
        {"duplicate role id",
         [](BattleScript& s) { AddRule(AddRole(s, 1, 20, BATTLE_ACTOR_TYPE_PET), 1, BATTLE_ACTION_DEFEND, 0,
                                       BATTLE_SCRIPT_TARGET_SELF); }},
        {"duplicate role order",
         [](BattleScript& s) { AddRule(AddRole(s, 2, 10, BATTLE_ACTOR_TYPE_PET), 1, BATTLE_ACTION_DEFEND, 0,
                                       BATTLE_SCRIPT_TARGET_SELF); }},
        {"role without rules", [](BattleScript& s) { s.mutable_roles(0)->clear_rules(); }},
        {"too many rules",
         [](BattleScript& s) {
             for (uint32_t priority = 100; priority < 116; ++priority) {
                 AddRule(s.mutable_roles(0), priority, BATTLE_ACTION_DEFEND, 0, BATTLE_SCRIPT_TARGET_SELF);
             }
         }},
        {"duplicate priority",
         [](BattleScript& s) { AddRule(s.mutable_roles(0), 10, BATTLE_ACTION_DEFEND, 0, BATTLE_SCRIPT_TARGET_SELF); }},
        {"unknown monster in role", [](BattleScript& s) { s.mutable_roles(0)->set_actor_type(BATTLE_ACTOR_TYPE_NONE);
                                                           s.mutable_roles(0)->set_class_id(0);
                                                           s.mutable_roles(0)->set_monster_id(4242); }},
        {"class on monster role", [](BattleScript& s) { s.mutable_roles(0)->set_actor_type(BATTLE_ACTOR_TYPE_MONSTER); }},
        {"condition none",
         [](BattleScript& s) { *s.mutable_roles(0)->mutable_rules(0)->add_conditions() = Condition(BATTLE_SCRIPT_COND_NONE, 1); }},
        {"hp condition out of range",
         [](BattleScript& s) {
             *s.mutable_roles(0)->mutable_rules(0)->add_conditions() = Condition(BATTLE_SCRIPT_COND_SELF_HP_BELOW, 10001);
         }},
        {"round every without period",
         [](BattleScript& s) {
             *s.mutable_roles(0)->mutable_rules(0)->add_conditions() = Condition(BATTLE_SCRIPT_COND_ROUND_EVERY, 0, 1);
         }},
        {"unused p2 set",
         [](BattleScript& s) {
             *s.mutable_roles(0)->mutable_rules(0)->add_conditions() =
                 Condition(BATTLE_SCRIPT_COND_ROUND_AT_LEAST, 2, 5);
         }},
        {"three conditions",
         [](BattleScript& s) {
             for (int i = 0; i < 3; ++i) {
                 *s.mutable_roles(0)->mutable_rules(0)->add_conditions() =
                     Condition(BATTLE_SCRIPT_COND_ROUND_AT_LEAST, 1);
             }
         }},
        {"filter none",
         [](BattleScript& s) { *s.mutable_roles(0)->mutable_rules(0)->mutable_target()->add_filters() =
                                   Filter(BATTLE_SCRIPT_FILTER_NONE, 1); }},
        {"four filters",
         [](BattleScript& s) {
             for (int i = 0; i < 3; ++i) {
                 *s.mutable_roles(0)->mutable_rules(0)->mutable_target()->add_filters() =
                     Filter(BATTLE_SCRIPT_FILTER_HP_BELOW, 5000);
             }
         }},
        {"dispel is not a status",
         [](BattleScript& s) { *s.mutable_roles(0)->mutable_rules(0)->mutable_target()->add_filters() =
                                   Filter(BATTLE_SCRIPT_FILTER_HAS_BUFF_TYPE, turnbattle::kBuffTypeDispel); }},
        {"unknown buff",
         [](BattleScript& s) { *s.mutable_roles(0)->mutable_rules(0)->mutable_target()->add_filters() =
                                   Filter(BATTLE_SCRIPT_FILTER_HAS_BUFF, 4242); }},
        {"unknown monster filter",
         [](BattleScript& s) { *s.mutable_roles(0)->mutable_rules(0)->mutable_target()->add_filters() =
                                   Filter(BATTLE_SCRIPT_FILTER_IS_MONSTER, 4242); }},
        {"bad actor type filter",
         [](BattleScript& s) { *s.mutable_roles(0)->mutable_rules(0)->mutable_target()->add_filters() =
                                   Filter(BATTLE_SCRIPT_FILTER_IS_ACTOR_TYPE, 9); }},
        {"claim of unknown role",
         [](BattleScript& s) { *s.mutable_roles(0)->mutable_rules(0)->mutable_target()->add_filters() =
                                   Filter(BATTLE_SCRIPT_FILTER_CLAIMED_BY_ROLE, 99); }},
        {"filters on engine pick",
         [](BattleScript& s) { s.mutable_roles(0)->mutable_rules(0)->mutable_target()->set_side(
                                   BATTLE_SCRIPT_TARGET_ENGINE_PICK); }},
        {"order on self",
         [](BattleScript& s) {
             auto* rule = AddRule(s.mutable_roles(0), 20, BATTLE_ACTION_DEFEND, 0, BATTLE_SCRIPT_TARGET_SELF);
             rule->mutable_target()->set_order(BATTLE_SCRIPT_ORDER_HP_LOWEST);
         }},
        {"attack with skill id",
         [](BattleScript& s) { AddRule(s.mutable_roles(0), 20, BATTLE_ACTION_ATTACK, kSeal, BATTLE_SCRIPT_TARGET_ENEMY); }},
        {"attack on ally",
         [](BattleScript& s) { AddRule(s.mutable_roles(0), 20, BATTLE_ACTION_ATTACK, 0, BATTLE_SCRIPT_TARGET_ALLY); }},
        {"defend on enemy",
         [](BattleScript& s) { AddRule(s.mutable_roles(0), 20, BATTLE_ACTION_DEFEND, 0, BATTLE_SCRIPT_TARGET_ENEMY); }},
        {"skill without id",
         [](BattleScript& s) { AddRule(s.mutable_roles(0), 20, BATTLE_ACTION_SKILL, 0, BATTLE_SCRIPT_TARGET_ENEMY); }},
        {"unknown skill",
         [](BattleScript& s) { AddRule(s.mutable_roles(0), 20, BATTLE_ACTION_SKILL, 4242, BATTLE_SCRIPT_TARGET_ENEMY); }},
        {"passive skill",
         [](BattleScript& s) { AddRule(s.mutable_roles(0), 20, BATTLE_ACTION_SKILL, kPassive, BATTLE_SCRIPT_TARGET_ENEMY); }},
        {"legacy skill on ally",
         [](BattleScript& s) { AddRule(s.mutable_roles(0), 20, BATTLE_ACTION_SKILL, kSlam, BATTLE_SCRIPT_TARGET_ALLY); }},
        {"class skill of another class",
         [](BattleScript& s) { AddRule(s.mutable_roles(0), 20, BATTLE_ACTION_SKILL, kMend, BATTLE_SCRIPT_TARGET_ALLY); }},
        {"class skill on wrong side",
         [](BattleScript& s) {
             AddRule(AddRole(s, 5, 50, BATTLE_ACTOR_TYPE_PLAYER, kHealClass), 1, BATTLE_ACTION_SKILL, kMend,
                     BATTLE_SCRIPT_TARGET_ENEMY);
         }},
        {"class skill in pet role",
         [](BattleScript& s) {
             AddRule(AddRole(s, 5, 50, BATTLE_ACTOR_TYPE_PET), 1, BATTLE_ACTION_SKILL, kMend,
                     BATTLE_SCRIPT_TARGET_ENGINE_PICK);
         }},
        {"flee is not scriptable",
         [](BattleScript& s) { AddRule(s.mutable_roles(0), 20, BATTLE_ACTION_FLEE, 0, BATTLE_SCRIPT_TARGET_SELF); }},
        {"note too long",
         [](BattleScript& s) {
             s.mutable_roles(0)->mutable_rules(0)->set_note(std::string(turnbattle::kMaxScriptNoteBytes + 1, 'x'));
         }},
    };
    for (const auto& [name, mutate] : cases) {
        BattleScript script = baseline();
        mutate(script);
        std::string error;
        EXPECT_FALSE(turnbattle::NormalizeBattleScript(script, *provider, error).has_value()) << name;
        EXPECT_FALSE(error.empty()) << name;
    }
}

TEST(BattleScriptValidationTest, SideRulesKeepMonstersAndPlayersApart) {
    auto provider = MakeProvider();
    std::string error;

    BattleScript monsterScript = NewScript();
    AddRule(AddRole(monsterScript, 1, 10, BATTLE_ACTOR_TYPE_MONSTER, 0, kBossTable), 10, BATTLE_ACTION_SKILL, kSlam,
            BATTLE_SCRIPT_TARGET_ENEMY);
    ASSERT_TRUE(turnbattle::NormalizeBattleScript(monsterScript, *provider, error).has_value()) << error;
    EXPECT_TRUE(turnbattle::CheckBattleScriptSide(monsterScript, BATTLE_SCRIPT_SIDE_MONSTER, *provider, error)) << error;
    EXPECT_FALSE(turnbattle::CheckBattleScriptSide(monsterScript, BATTLE_SCRIPT_SIDE_PLAYER, *provider, error));
    EXPECT_FALSE(turnbattle::CheckBattleScriptSide(monsterScript, BATTLE_SCRIPT_SIDE_UNSPECIFIED, *provider, error));

    // 怪物没有法力:耗蓝的技能放不了
    BattleScript costlyMonster = NewScript();
    AddRule(AddRole(costlyMonster, 1, 10, BATTLE_ACTOR_TYPE_MONSTER), 10, BATTLE_ACTION_SKILL, kCostly,
            BATTLE_SCRIPT_TARGET_ENEMY);
    ASSERT_TRUE(turnbattle::NormalizeBattleScript(costlyMonster, *provider, error).has_value()) << error;
    EXPECT_FALSE(turnbattle::CheckBattleScriptSide(costlyMonster, BATTLE_SCRIPT_SIDE_MONSTER, *provider, error));

    // 「不限单位类型」的角色位放职业技能:玩家方可以,怪物方不行(怪物没有职业)
    BattleScript anyRole = NewScript();
    AddRule(AddRole(anyRole, 1, 10, BATTLE_ACTOR_TYPE_NONE), 10, BATTLE_ACTION_SKILL, kMend,
            BATTLE_SCRIPT_TARGET_ALLY);
    ASSERT_TRUE(turnbattle::NormalizeBattleScript(anyRole, *provider, error).has_value()) << error;
    EXPECT_TRUE(turnbattle::CheckBattleScriptSide(anyRole, BATTLE_SCRIPT_SIDE_PLAYER, *provider, error)) << error;
    EXPECT_FALSE(turnbattle::CheckBattleScriptSide(anyRole, BATTLE_SCRIPT_SIDE_MONSTER, *provider, error));
}

// ---------------------------------------------------------------------------
// 引擎
// ---------------------------------------------------------------------------

// 封 / 冰 / 毒 / 集火:同一回合里后决策的角色位跟着先决策的目标走;第 2 回合不再封已封的、不再冰已冰的
TEST(BattleScriptEngineTest, RolesSealFreezePoisonAndFocusTogether) {
    auto provider = MakeProvider();
    const auto request = ComboRequest();
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    EXPECT_TRUE(engine.ScriptRejections().empty());
    SetAllAuto(engine, request);

    const auto round1 = engine.ResolveCurrentRound();
    const auto* seal = FindAction(round1, kSealer);
    ASSERT_NE(seal, nullptr);
    EXPECT_EQ(seal->event_type(), BATTLE_EVENT_SKILL);
    EXPECT_EQ(seal->skill_table_id(), kSeal);
    EXPECT_EQ(seal->target_id(), kBoss);  // 先封 Boss
    const auto* freeze = FindAction(round1, kIcer);
    ASSERT_NE(freeze, nullptr);
    EXPECT_EQ(freeze->skill_table_id(), kFreeze);
    EXPECT_EQ(freeze->target_id(), kMinion);  // 封系已认领 Boss,冰系去冰另一只
    const auto* poison = FindAction(round1, kPoisoner);
    ASSERT_NE(poison, nullptr);
    EXPECT_EQ(poison->skill_table_id(), kPoison);
    EXPECT_EQ(poison->target_id(), kMinion);  // 组合:跟着冰系的目标下毒
    const auto* strike = FindAction(round1, kStriker);
    ASSERT_NE(strike, nullptr);
    EXPECT_EQ(strike->event_type(), BATTLE_EVENT_ATTACK);
    EXPECT_EQ(strike->target_id(), kMinion);  // 集火冰系的目标
    EXPECT_TRUE(HasBuffAdd(round1, kBoss, kBuffStun));
    EXPECT_TRUE(HasBuffAdd(round1, kMinion, kBuffFreeze));
    EXPECT_TRUE(HasBuffAdd(round1, kMinion, kBuffPoison));
    // 被封 / 被冰的怪物这一回合动不了
    EXPECT_EQ(FindAction(round1, kBoss), nullptr);
    EXPECT_EQ(FindAction(round1, kMinion), nullptr);

    const auto round2 = engine.ResolveCurrentRound();
    const auto* sealAgain = FindAction(round2, kSealer);
    ASSERT_NE(sealAgain, nullptr);
    EXPECT_EQ(sealAgain->event_type(), BATTLE_EVENT_ATTACK);  // Boss 还封着:落到下一条
    const auto* freezeAgain = FindAction(round2, kIcer);
    ASSERT_NE(freezeAgain, nullptr);
    EXPECT_FALSE(freezeAgain->event_type() == BATTLE_EVENT_SKILL && freezeAgain->target_id() == kMinion)
        << "already frozen minion must not be frozen again";
}

BattleAction ManualSeal(uint64_t targetId) {
    BattleAction action;
    action.set_action_type(BATTLE_ACTION_SKILL);
    action.set_skill_table_id(kSeal);
    action.set_target_id(targetId);
    return action;
}

// 手动提交的行动原样保留:剧本自己会去封 Boss,手动选了小怪就封小怪
TEST(BattleScriptEngineTest, ManualActionIsNeverOverridden) {
    auto provider = MakeProvider();
    auto request = MakeRequest();
    AddPlayer(request, kSealer, kSealClass, 530, {kSeal});
    AttachScript(request, 0, ComboScript());
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    ASSERT_TRUE(engine.SubmitAction(kSealer, ManualSeal(kMinion)));

    const auto round = engine.ResolveCurrentRound();
    const auto* seal = FindAction(round, kSealer);
    ASSERT_NE(seal, nullptr);
    EXPECT_EQ(seal->skill_table_id(), kSeal);
    EXPECT_EQ(seal->target_id(), kMinion);
}

// 手动行动算作该角色位的认领:跟随封系的输出,打队友手选的 Boss(只按自己的下一条会去打血少的小怪)
TEST(BattleScriptEngineTest, ManualActionCountsAsRoleClaim) {
    auto provider = MakeProvider();
    auto followSealer = ComboScript();
    // 输出角色位(第 4 个加入的角色位)的第一条改成跟封系(role 1)
    followSealer.mutable_roles(3)->mutable_rules(0)->mutable_target()->mutable_filters(0)->set_p1(1);
    auto request = MakeRequest();
    AddPlayer(request, kSealer, kSealClass, 530, {kSeal});
    AddPlayer(request, kStriker, kStrikeClass, 500);
    AttachScript(request, 0, followSealer);
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    ASSERT_EQ(engine.SetActorAuto(kStriker, true), 0u);
    ASSERT_TRUE(engine.SubmitAction(kSealer, ManualSeal(kBoss)));

    const auto round = engine.ResolveCurrentRound();
    const auto* follow = FindAction(round, kStriker);
    ASSERT_NE(follow, nullptr);
    EXPECT_EQ(follow->event_type(), BATTLE_EVENT_ATTACK);
    EXPECT_EQ(follow->target_id(), kBoss);
}

// 超时没提交的(非挂机)玩家也按剧本出手,而不是默认普攻
TEST(BattleScriptEngineTest, TimedOutPlayerFollowsScript) {
    auto provider = MakeProvider();
    auto request = MakeRequest();
    AddPlayer(request, kSealer, kSealClass, 530, {kSeal});
    AttachScript(request, 0, ComboScript());
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));

    const auto round = engine.ResolveCurrentRound();  // 没有提交 = 回合超时
    const auto* seal = FindAction(round, kSealer);
    ASSERT_NE(seal, nullptr);
    EXPECT_EQ(seal->event_type(), BATTLE_EVENT_SKILL);
    EXPECT_EQ(seal->target_id(), kBoss);
}

// 同一角色位的两个人:「本角色位还没选过」让他们分开封,不重复封同一只
TEST(BattleScriptEngineTest, SameRoleMembersSpreadTargets) {
    auto provider = MakeProvider();
    BattleScript script = NewScript();
    auto* sealRole = AddRole(script, 1, 10, BATTLE_ACTOR_TYPE_PLAYER, kSealClass);
    AddRule(sealRole, 10, BATTLE_ACTION_SKILL, kSeal, BATTLE_SCRIPT_TARGET_ENEMY,
            {Filter(BATTLE_SCRIPT_FILTER_CLAIMED_BY_ROLE, 0, true),
             Filter(BATTLE_SCRIPT_FILTER_HAS_BUFF_TYPE, turnbattle::kBuffTypeStun, true)});
    auto request = MakeRequest();
    AddPlayer(request, kSealer, kSealClass, 530, {kSeal});
    AddPlayer(request, kSealer2, kSealClass, 520, {kSeal});
    AttachScript(request, 0, script);
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    SetAllAuto(engine, request);

    const auto round = engine.ResolveCurrentRound();
    const auto* first = FindAction(round, kSealer);
    const auto* second = FindAction(round, kSealer2);
    ASSERT_NE(first, nullptr);
    ASSERT_NE(second, nullptr);
    EXPECT_EQ(first->target_id(), kMinion);  // 阵位在前的先选,按阵位取第一只
    EXPECT_EQ(second->target_id(), kBoss);
}

// 怪物剧本:Boss 的技能集由剧本派生(开局状态里可见),先打治疗职业;小怪跟着 Boss 的目标打
TEST(BattleScriptEngineTest, MonsterScriptDerivesSkillsAndFocusesHealer) {
    auto provider = MakeProvider();
    BattleScript script = NewScript();
    auto* bossRole = AddRole(script, 1, 10, BATTLE_ACTOR_TYPE_MONSTER, 0, kBossTable);
    AddRule(bossRole, 10, BATTLE_ACTION_SKILL, kSlam, BATTLE_SCRIPT_TARGET_ENEMY,
            {Filter(BATTLE_SCRIPT_FILTER_IS_CLASS, kHealClass)}, BATTLE_SCRIPT_ORDER_HP_RATIO_LOWEST);
    AddRule(bossRole, 20, BATTLE_ACTION_ATTACK, 0, BATTLE_SCRIPT_TARGET_ENEMY, {}, BATTLE_SCRIPT_ORDER_HP_LOWEST);
    auto* minionRole = AddRole(script, 2, 20, BATTLE_ACTOR_TYPE_MONSTER);
    AddRule(minionRole, 10, BATTLE_ACTION_ATTACK, 0, BATTLE_SCRIPT_TARGET_ENEMY,
            {Filter(BATTLE_SCRIPT_FILTER_CLAIMED_BY_ROLE, 1)});

    auto request = MakeRequest();
    AddPlayer(request, kStriker, kStrikeClass, 50);  // 比怪物慢:怪物先出手
    AddPlayer(request, kHealer, kHealClass, 40);
    AttachScript(request, 1, script);
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    ASSERT_TRUE(engine.ScriptRejections().empty());

    const auto start = engine.BuildStateSnapshot();
    const auto* boss = FindStateActor(start, kBoss);
    ASSERT_NE(boss, nullptr);
    ASSERT_EQ(boss->skill_table_ids_size(), 1);
    EXPECT_EQ(boss->skill_table_ids(0), kSlam);
    const auto* minion = FindStateActor(start, kMinion);
    ASSERT_NE(minion, nullptr);
    EXPECT_EQ(minion->skill_table_ids_size(), 0);

    const auto round = engine.ResolveCurrentRound();
    const auto* slam = FindAction(round, kBoss);
    ASSERT_NE(slam, nullptr);
    EXPECT_EQ(slam->event_type(), BATTLE_EVENT_SKILL);
    EXPECT_EQ(slam->skill_table_id(), kSlam);
    EXPECT_EQ(slam->target_id(), kHealer);
    const auto* minionHit = FindAction(round, kMinion);
    ASSERT_NE(minionHit, nullptr);
    EXPECT_EQ(minionHit->event_type(), BATTLE_EVENT_ATTACK);
    EXPECT_EQ(minionHit->target_id(), kHealer);
}

// 职业治疗:只给气血低于一半的队友,按气血比例最低的先治
TEST(BattleScriptEngineTest, HealerHealsLowestRatioAllyBelowThreshold) {
    auto provider = MakeProvider();
    BattleScript script = NewScript();
    auto* healRole = AddRole(script, 1, 10, BATTLE_ACTOR_TYPE_PLAYER, kHealClass);
    AddRule(healRole, 10, BATTLE_ACTION_SKILL, kMend, BATTLE_SCRIPT_TARGET_ALLY,
            {Filter(BATTLE_SCRIPT_FILTER_HP_BELOW, 5000)}, BATTLE_SCRIPT_ORDER_HP_RATIO_LOWEST);
    AddRule(healRole, 20, BATTLE_ACTION_DEFEND, 0, BATTLE_SCRIPT_TARGET_SELF);

    auto request = MakeRequest();
    auto* healer = AddPlayer(request, kHealer, kHealClass, 530, {kMend});
    (*healer->mutable_skill_ranks())[kMend] = 1;
    auto* hurt = AddPlayer(request, kStriker, kStrikeClass, 520);
    hurt->mutable_base_attributes()->set_health(300);  // 30%
    auto* scratched = AddPlayer(request, kSealer, kSealClass, 510);
    scratched->mutable_base_attributes()->set_health(450);  // 45%
    AttachScript(request, 0, script);
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    ASSERT_EQ(engine.SetActorAuto(kHealer, true), 0u);
    BattleAction defend;
    defend.set_action_type(BATTLE_ACTION_DEFEND);
    ASSERT_FALSE(engine.SubmitAction(kStriker, defend));
    ASSERT_TRUE(engine.SubmitAction(kSealer, defend));

    const auto round = engine.ResolveCurrentRound();
    const auto* heal = FindAction(round, kHealer);
    ASSERT_NE(heal, nullptr);
    EXPECT_EQ(heal->event_type(), BATTLE_EVENT_SKILL);
    EXPECT_EQ(heal->skill_table_id(), kMend);
    EXPECT_EQ(heal->target_id(), kStriker);
}

// 冷却中的技能过不了校验链:落到下一条
TEST(BattleScriptEngineTest, CooldownFallsThroughToNextRule) {
    auto provider = MakeProvider();
    BattleScript script = NewScript();
    auto* role = AddRole(script, 1, 10, BATTLE_ACTOR_TYPE_PLAYER);
    AddRule(role, 10, BATTLE_ACTION_SKILL, kGuarded, BATTLE_SCRIPT_TARGET_ENEMY);
    AddRule(role, 20, BATTLE_ACTION_DEFEND, 0, BATTLE_SCRIPT_TARGET_SELF);
    auto request = MakeRequest();
    AddPlayer(request, kStriker, kStrikeClass, 530, {kGuarded});
    AttachScript(request, 0, script);
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    SetAllAuto(engine, request);

    const auto round1 = engine.ResolveCurrentRound();
    const auto* cast = FindAction(round1, kStriker);
    ASSERT_NE(cast, nullptr);
    EXPECT_EQ(cast->event_type(), BATTLE_EVENT_SKILL);
    const auto round2 = engine.ResolveCurrentRound();
    const auto* fallback = FindAction(round2, kStriker);
    ASSERT_NE(fallback, nullptr);
    EXPECT_EQ(fallback->event_type(), BATTLE_EVENT_DEFEND);
}

// ROUND_EVERY(2, 1):第 1、3 回合防御,第 2 回合落到普攻
TEST(BattleScriptEngineTest, RoundConditionsGateRules) {
    auto provider = MakeProvider();
    BattleScript script = NewScript();
    auto* role = AddRole(script, 1, 10, BATTLE_ACTOR_TYPE_PLAYER);
    AddRule(role, 10, BATTLE_ACTION_DEFEND, 0, BATTLE_SCRIPT_TARGET_SELF, {}, BATTLE_SCRIPT_ORDER_FORMATION,
            {Condition(BATTLE_SCRIPT_COND_ROUND_EVERY, 2, 1)});
    AddRule(role, 20, BATTLE_ACTION_ATTACK, 0, BATTLE_SCRIPT_TARGET_ENEMY);
    auto request = MakeRequest();
    AddPlayer(request, kStriker, kStrikeClass, 530);
    AttachScript(request, 0, script);
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    SetAllAuto(engine, request);

    const std::vector<eBattleEventType> expected = {BATTLE_EVENT_DEFEND, BATTLE_EVENT_ATTACK, BATTLE_EVENT_DEFEND};
    for (size_t round = 0; round < expected.size(); ++round) {
        const auto result = engine.ResolveCurrentRound();
        const auto* action = FindAction(result, kStriker);
        ASSERT_NE(action, nullptr) << "round " << round + 1;
        EXPECT_EQ(action->event_type(), expected[round]) << "round " << round + 1;
    }
}

// 规则全落空:回到现行默认(这里是普攻)
TEST(BattleScriptEngineTest, NoMatchingRuleFallsBackToDefault) {
    auto provider = MakeProvider();
    BattleScript script = NewScript();
    AddRule(AddRole(script, 1, 10, BATTLE_ACTOR_TYPE_PLAYER), 10, BATTLE_ACTION_DEFEND, 0, BATTLE_SCRIPT_TARGET_SELF,
            {}, BATTLE_SCRIPT_ORDER_FORMATION, {Condition(BATTLE_SCRIPT_COND_ROUND_AT_LEAST, 99)});
    auto request = MakeRequest();
    AddPlayer(request, kStriker, kStrikeClass, 530);
    AttachScript(request, 0, script);
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    SetAllAuto(engine, request);

    const auto round = engine.ResolveCurrentRound();
    const auto* action = FindAction(round, kStriker);
    ASSERT_NE(action, nullptr);
    EXPECT_EQ(action->event_type(), BATTLE_EVENT_ATTACK);
}

// 不合法的剧本整份忽略并给出原因;忽略之后与根本没带剧本的对局逐字节相同
TEST(BattleScriptEngineTest, InvalidScriptsAreIgnoredWithoutChangingTheBattle) {
    auto provider = MakeProvider();
    auto plain = MakeRequest();
    AddPlayer(plain, kSealer, kSealClass, 530, {kSeal});
    AddPlayer(plain, kStriker, kStrikeClass, 500);

    auto withBad = plain;
    BattleScript unknownSkill = NewScript();
    AddRule(AddRole(unknownSkill, 1, 10, BATTLE_ACTOR_TYPE_PLAYER), 10, BATTLE_ACTION_SKILL, 4242,
            BATTLE_SCRIPT_TARGET_ENEMY);
    AttachScript(withBad, 0, unknownSkill);
    AttachScript(withBad, 2, ComboScript());  // 队伍号越界
    // 玩家方剧本挂到怪物方:怪物方不许认领玩家
    AttachScript(withBad, 1, ComboScript());
    AttachScript(withBad, 1, ComboScript());  // 同一队第二份

    TurnBattleEngine plainEngine(provider);
    TurnBattleEngine badEngine(provider);
    ASSERT_TRUE(plainEngine.Initialize(plain));
    ASSERT_TRUE(badEngine.Initialize(withBad));
    EXPECT_EQ(badEngine.ScriptRejections().size(), 4u);
    SetAllAuto(plainEngine, plain);
    SetAllAuto(badEngine, withBad);
    for (int round = 0; round < 3; ++round) {
        auto lhs = plainEngine.ResolveCurrentRound();
        auto rhs = badEngine.ResolveCurrentRound();
        EXPECT_EQ(lhs.SerializeAsString(), rhs.SerializeAsString()) << "round " << round + 1;
    }
}

// 同请求同种子:带剧本的对局逐字节可重放
TEST(BattleScriptEngineTest, ScriptedBattleIsDeterministic) {
    auto provider = MakeProvider();
    const auto request = ComboRequest();
    TurnBattleEngine first(provider);
    TurnBattleEngine second(provider);
    ASSERT_TRUE(first.Initialize(request));
    ASSERT_TRUE(second.Initialize(request));
    SetAllAuto(first, request);
    SetAllAuto(second, request);
    for (int round = 0; round < 5; ++round) {
        EXPECT_EQ(first.ResolveCurrentRound().SerializeAsString(), second.ResolveCurrentRound().SerializeAsString())
            << "round " << round + 1;
    }
}

}  // namespace
