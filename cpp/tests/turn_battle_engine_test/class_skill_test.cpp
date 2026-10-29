#include <gtest/gtest.h>

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <limits>
#include <memory>
#include <string>
#include <vector>

#include "constants/turn_battle_constants.h"
#include "memory_battle_data_provider.h"
#include "system/class_skill_rules.h"
#include "system/combat_damage_rules.h"
#include "system/turn_battle_engine.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/skill_error_tip.pb.h"

// 四职业技能(docs/design/class-skills.md)的纯规则与回合引擎用例。
// 引擎用例用内存数据源喂一组"形状同真表"的职业技能行(id 9101..),不依赖 Excel;
// 真表 32 行的契约在 table_battle_data_provider_test.cpp 的 ClassSkillCatalog* 用例里。

namespace {

using turnbattle::TurnBattleEngine;
using classskill::TargetTeam;

// ---------------------------------------------------------------------------
// 纯规则
// ---------------------------------------------------------------------------

TEST(ClassSkillRulesTest, RankLimitOpensOneRankEveryTwoLevelsAfterUnlock) {
    EXPECT_EQ(classskill::RankLimit(1, 1, 10), 1u);
    EXPECT_EQ(classskill::RankLimit(2, 1, 10), 1u);
    EXPECT_EQ(classskill::RankLimit(3, 1, 10), 2u);
    EXPECT_EQ(classskill::RankLimit(19, 1, 10), 10u);
    EXPECT_EQ(classskill::RankLimit(85, 1, 10), 10u);
    EXPECT_EQ(classskill::RankLimit(9, 10, 10), 0u);   // 没到解锁等级
    EXPECT_EQ(classskill::RankLimit(10, 10, 10), 1u);
    EXPECT_EQ(classskill::RankLimit(13, 10, 10), 2u);
    EXPECT_EQ(classskill::RankLimit(50, 1, 0), 0u);    // 表里没填最高重
    EXPECT_EQ(classskill::EffectiveRank(7, 10, 1, 10), 5u);  // 存档等级被当前上限夹住
}

TEST(ClassSkillRulesTest, UpgradeCostFollowsContentBaseline) {
    EXPECT_EQ(classskill::UpgradeCost(1, 1), 110u);      // 100 × 1² + 10 × 1
    EXPECT_EQ(classskill::UpgradeCost(2, 10), 500u);     // 100 × 4 + 100
    EXPECT_EQ(classskill::UpgradeCost(10, 40), 10400u);  // 100 × 100 + 400
}

TEST(ClassSkillRulesTest, PlanUpgradeStopsAtLevelLimitGoldOrMaxRank) {
    constexpr uint64_t kRich = std::numeric_limits<uint64_t>::max();

    // 10 次修习被等级上限截在第 3 重:付的是 1、2、3 重的价
    auto plan = classskill::PlanUpgrade(0, 10, 3, 1, kRich, 10);
    EXPECT_EQ(plan.steps, 3u);
    EXPECT_EQ(plan.finalRank, 3u);
    EXPECT_EQ(plan.totalCost, 110u + 410u + 910u);
    EXPECT_EQ(plan.stop, classskill::UpgradeStop::kLevelLimit);

    // 余额刚好够两重:第三重付不起
    plan = classskill::PlanUpgrade(0, 10, 10, 1, 110 + 410, 10);
    EXPECT_EQ(plan.steps, 2u);
    EXPECT_EQ(plan.totalCost, 520u);
    EXPECT_EQ(plan.stop, classskill::UpgradeStop::kGold);

    // 按次数修完不算"停"
    plan = classskill::PlanUpgrade(4, 10, 10, 1, kRich, 1);
    EXPECT_EQ(plan.steps, 1u);
    EXPECT_EQ(plan.finalRank, 5u);
    EXPECT_EQ(plan.stop, classskill::UpgradeStop::kNone);

    // 满重一重都修不了,一分钱不扣
    plan = classskill::PlanUpgrade(10, 10, 10, 1, kRich, 10);
    EXPECT_EQ(plan.steps, 0u);
    EXPECT_EQ(plan.totalCost, 0u);
    EXPECT_EQ(plan.stop, classskill::UpgradeStop::kMaxRank);

    EXPECT_TRUE(classskill::IsValidUpgradeCount(1));
    EXPECT_TRUE(classskill::IsValidUpgradeCount(10));
    EXPECT_FALSE(classskill::IsValidUpgradeCount(0));
    EXPECT_FALSE(classskill::IsValidUpgradeCount(2));
    EXPECT_FALSE(classskill::IsValidUpgradeCount(11));
}

// 存储重数高于本版本 max_rank(新版本调大上限后写入 / 本版本下调了上限,加载期原样携带,混版本设计 §12.1 D5):
// 修习一重都不走、一分钱不扣,停在满重;scene 的 UpgradeSkill 不另加守卫,靠的就是这一条。
// 生效等级仍被本版本上限夹住。
TEST(ClassSkillRulesTest, PlanUpgradeFromAboveMaxRankPlansNothing) {
    constexpr uint64_t kRich = std::numeric_limits<uint64_t>::max();

    for (const uint32_t count : {1u, 10u}) {
        const auto plan = classskill::PlanUpgrade(15, 10, 10, 1, kRich, count);
        EXPECT_EQ(plan.steps, 0u) << "count " << count;
        EXPECT_EQ(plan.finalRank, 15u) << "count " << count;
        EXPECT_EQ(plan.totalCost, 0u) << "count " << count;
        EXPECT_EQ(plan.stop, classskill::UpgradeStop::kMaxRank) << "count " << count;
    }
    // 存储值在 uint32 顶端也先停在满重,不会算 rank + 1 回绕
    const auto top = classskill::PlanUpgrade(std::numeric_limits<uint32_t>::max(), 10, 10, 1, kRich, 10);
    EXPECT_EQ(top.steps, 0u);
    EXPECT_EQ(top.totalCost, 0u);
    EXPECT_EQ(top.stop, classskill::UpgradeStop::kMaxRank);

    EXPECT_EQ(classskill::EffectiveRank(15, 30, 1, 10), 10u);  // 超上限的存储重数按上限生效
    EXPECT_EQ(classskill::EffectiveRank(15, 5, 1, 10), 3u);    // 同时受角色等级的每重上限夹住
    EXPECT_EQ(classskill::EffectiveRank(0, 30, 1, 10), 0u);    // rank 0 即未学
}

TEST(ClassSkillRulesTest, ManaCostRoundsUpWithMinimumOneAndNeverOverflows) {
    EXPECT_EQ(classskill::ManaCost(100, 0.03), 3u);
    EXPECT_EQ(classskill::ManaCost(250, 0.06), 15u);
    EXPECT_EQ(classskill::ManaCost(10, 0.03), 1u);   // ceil(0.3)
    EXPECT_EQ(classskill::ManaCost(0, 0.03), 1u);    // 比例 > 0 至少 1
    EXPECT_EQ(classskill::ManaCost(100, 0.0), 0u);
    EXPECT_EQ(classskill::CeilRatio(std::numeric_limits<uint64_t>::max(), 2.0),
              std::numeric_limits<uint64_t>::max());
    EXPECT_EQ(classskill::CeilRatio(100, std::numeric_limits<double>::quiet_NaN()), 0u);
    // 0.07 在二进制里略大于本值:浮点直接乘 100 再向上取整会得 8,整数算法得 7
    EXPECT_EQ(classskill::CeilRatio(100, 0.07), 7u);
    EXPECT_EQ(classskill::FloorRatio(220, 0.8), 176u);
    EXPECT_EQ(classskill::FloorRatio(3000, 1.25), 3750u);
}

TEST(ClassSkillRulesTest, RankFactorsAndPassiveStatNames) {
    EXPECT_DOUBLE_EQ(classskill::DamageRankFactor(1), 1.0);
    EXPECT_DOUBLE_EQ(classskill::DamageRankFactor(10), 1.18);
    EXPECT_DOUBLE_EQ(classskill::SupportRankFactor(6), 1.10);
    EXPECT_DOUBLE_EQ(classskill::PassiveRankFactor(10), 1.45);
    EXPECT_DOUBLE_EQ(classskill::PassiveRankFactor(0), 1.0);  // 未学按 1 重算,调用方先保证已学

    EXPECT_EQ(classskill::ParsePassiveStat("maxHp"), classskill::PassiveStat::kMaxHealth);
    EXPECT_EQ(classskill::ParsePassiveStat("outgoingHeal"), classskill::PassiveStat::kOutgoingHeal);
    EXPECT_EQ(classskill::ParsePassiveStat("crit"), classskill::PassiveStat::kCrit);
    EXPECT_EQ(classskill::ParsePassiveStat(""), classskill::PassiveStat::kNone);
    EXPECT_EQ(classskill::ParsePassiveStat("bogus"), classskill::PassiveStat::kNone);

    EXPECT_EQ(turnbattle::ClassSkillStatOfBuffType(turnbattle::kBuffTypeIncreaseDefense),
              turnbattle::ClassBuffStat::kDefense);
    EXPECT_EQ(turnbattle::ClassSkillStatOfBuffType(turnbattle::kBuffTypePoison), turnbattle::ClassBuffStat::kNone);
}

// ---------------------------------------------------------------------------
// 引擎用例的数据
// ---------------------------------------------------------------------------

constexpr uint64_t kCaster = 7001;
constexpr uint64_t kAlly = 7002;
constexpr uint64_t kFoe = 7003;
constexpr uint64_t kFoe2 = 7004;
constexpr uint32_t kClassId = 1;
constexpr uint32_t kPvpMode = 3;
constexpr uint32_t kDungeonConfig = 7;

constexpr uint32_t kStrike = 9101;        // 敌方单体物理,耗蓝 10%,冷却 2
constexpr uint32_t kSweep = 9102;         // 敌方最多 3 个,法术,耗蓝 6%
constexpr uint32_t kGuard = 9103;         // 自身防御 +25%,冷却 3
constexpr uint32_t kMend = 9104;          // 友方单体治疗,残血(< 30%)再加 30%
constexpr uint32_t kMendAll = 9105;       // 友方最多 3 个治疗
constexpr uint32_t kIronPassive = 9106;   // 被动:防御
constexpr uint32_t kHealPassive = 9107;   // 被动:治疗加成 10%
constexpr uint32_t kFocus = 9108;         // 自身回蓝 20%,不耗蓝
constexpr uint32_t kCleanse = 9109;       // 友方单体驱散 1 个减益
constexpr uint32_t kSwift = 9110;         // 先手优先级 1 的敌方单体
constexpr uint32_t kSlow = 9111;          // 敌方单体,速度 -20%
constexpr uint32_t kBadTeam = 9112;       // target_team 没填的坏行
constexpr uint32_t kOtherClass = 9201;    // 职业 2 的技能
constexpr uint32_t kLegacyNoCd = 9001;    // 老技能:cooldown_id 0
constexpr uint32_t kLegacyNuke = 9002;    // 老技能:一击必杀
constexpr uint32_t kLegacySilence = 9003; // 老技能:挂沉默
constexpr uint32_t kBuffGuard = 9903;     // 防御增益(buff_type 11)
constexpr uint32_t kBuffSlow = 9913;      // 速度减益(buff_type 13,Debuff 标签)
constexpr uint32_t kBuffPoison = 9950;
constexpr uint32_t kBuffSilence = 9951;

SkillTable& AddClassSkill(MemoryBattleDataProvider& provider, uint32_t skillId, TargetTeam team,
                          uint32_t maxTargets) {
    auto& row = provider.AddSkill(skillId);
    row.set_class_id(kClassId);
    row.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    row.set_target_team(static_cast<uint32_t>(team));
    row.set_max_targets(maxTargets);
    row.set_unlock_level(1);
    row.set_max_rank(10);
    return row;
}

SkillTable& AddLegacySkill(MemoryBattleDataProvider& provider, uint32_t skillId) {
    auto& row = provider.AddSkill(skillId);
    row.add_targeting_mode(1);
    row.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    return row;
}

std::shared_ptr<MemoryBattleDataProvider> MakeClassProvider() {
    auto provider = std::make_shared<MemoryBattleDataProvider>();
    provider->AddBattleReward(0); // 职业技能测试显式使用无奖通用档，隔离奖励数值。

    auto& strike = AddClassSkill(*provider, kStrike, TargetTeam::kEnemy, 1);
    strike.set_damage_type(combatdamage::kPhysicalDamage);
    strike.set_attack_multiplier(1.0);
    strike.set_mana_percent(0.10);
    strike.set_cooldown_rounds(2);
    provider->SetSkillDamage(kStrike, 10.0);

    auto& sweep = AddClassSkill(*provider, kSweep, TargetTeam::kEnemy, 3);
    sweep.set_damage_type(combatdamage::kMagicDamage);
    sweep.set_attack_multiplier(0.85);
    sweep.set_mana_percent(0.06);
    provider->SetSkillDamage(kSweep, 10.0);

    auto& guard = AddClassSkill(*provider, kGuard, TargetTeam::kSelf, 1);
    guard.set_cooldown_rounds(3);
    guard.set_buff_percent(0.25);
    guard.add_effect(kBuffGuard);

    auto& mend = AddClassSkill(*provider, kMend, TargetTeam::kAlly, 1);
    mend.set_heal_multiplier(1.0);
    mend.set_heal_per_level(8.0);
    mend.set_execute_heal_threshold(0.3);
    mend.set_execute_heal_bonus(0.3);

    auto& mendAll = AddClassSkill(*provider, kMendAll, TargetTeam::kAlly, 3);
    mendAll.set_heal_multiplier(0.6);
    mendAll.set_heal_per_level(4.0);

    auto& iron = AddClassSkill(*provider, kIronPassive, TargetTeam::kSelf, 1);
    iron.clear_skill_type();
    iron.set_passive_stat("defense");
    iron.set_passive_percent(0.08);

    auto& healPassive = AddClassSkill(*provider, kHealPassive, TargetTeam::kSelf, 1);
    healPassive.clear_skill_type();
    healPassive.set_passive_stat("outgoingHeal");
    healPassive.set_passive_percent(0.10);

    auto& focus = AddClassSkill(*provider, kFocus, TargetTeam::kSelf, 1);
    focus.set_restore_mana_percent(0.2);

    auto& cleanse = AddClassSkill(*provider, kCleanse, TargetTeam::kAlly, 1);
    cleanse.set_dispel_count(1);

    auto& swift = AddClassSkill(*provider, kSwift, TargetTeam::kEnemy, 1);
    swift.set_damage_type(combatdamage::kPhysicalDamage);
    swift.set_initiative(1);
    swift.set_mana_percent(0.10);
    provider->SetSkillDamage(kSwift, 10.0);

    auto& slow = AddClassSkill(*provider, kSlow, TargetTeam::kEnemy, 1);
    slow.set_damage_type(combatdamage::kPhysicalDamage);
    slow.set_buff_percent(-0.2);
    slow.add_effect(kBuffSlow);
    provider->SetSkillDamage(kSlow, 10.0);

    AddClassSkill(*provider, kBadTeam, TargetTeam::kNone, 1);

    auto& otherClass = AddClassSkill(*provider, kOtherClass, TargetTeam::kEnemy, 1);
    otherClass.set_class_id(2);

    AddLegacySkill(*provider, kLegacyNoCd);  // cooldown_id 缺省 0
    provider->SetSkillDamage(kLegacyNoCd, 5.0);
    AddLegacySkill(*provider, kLegacyNuke);
    provider->SetSkillDamage(kLegacyNuke, 1000000.0);
    AddLegacySkill(*provider, kLegacySilence).add_effect(kBuffSilence);

    auto& guardBuff = provider->AddBuff(kBuffGuard);
    guardBuff.set_buff_type(turnbattle::kBuffTypeIncreaseDefense);
    guardBuff.set_no_caster(1);
    guardBuff.set_max_layer(1);
    guardBuff.set_duration(12.0);  // 2 回合
    (*guardBuff.mutable_tag())["ClassSkillBuff"] = true;

    auto& slowBuff = provider->AddBuff(kBuffSlow);
    slowBuff.set_buff_type(turnbattle::kBuffTypeMovementSpeedBoost);
    slowBuff.set_no_caster(1);
    slowBuff.set_max_layer(1);
    slowBuff.set_duration(12.0);
    (*slowBuff.mutable_tag())["ClassSkillBuff"] = true;
    (*slowBuff.mutable_tag())["Debuff"] = true;

    auto& poisonBuff = provider->AddBuff(kBuffPoison);
    poisonBuff.set_buff_type(turnbattle::kBuffTypePoison);
    poisonBuff.set_duration(30.0);
    poisonBuff.set_interval(6.0);
    poisonBuff.add_interval_effect(1.0);

    auto& silenceBuff = provider->AddBuff(kBuffSilence);
    silenceBuff.set_buff_type(turnbattle::kBuffTypeSilence);
    silenceBuff.set_duration(12.0);

    // 沉默许可:普通施放(位号 1)在沉默下禁用
    auto& permission = provider->AddSkillPermission(turnbattle::kCombatStateSilence);
    permission.add_skill_type(kSuccess);
    permission.add_skill_type(kSkillCannotBeCastSilenceRestriction);
    permission.add_skill_type(kSkillCannotBeCastSilenceRestriction);
    permission.add_skill_type(kSuccess);
    permission.add_skill_type(kSuccess);
    permission.add_skill_type(kSuccess);
    return provider;
}

CreateBattleRequest MakeRequest(uint64_t battleId, uint32_t matchMode) {
    CreateBattleRequest request;
    request.set_battle_id(battleId);
    request.set_battle_config_id(kDungeonConfig);
    request.set_match_mode(matchMode);
    request.set_seed(20261019);
    return request;
}

// 职业 1、30 级(各技能上限 10 重)、满血满蓝、无暴击、无防御的玩家
BattlePlayerSnapshot* AddClassPlayer(CreateBattleRequest& request, uint64_t playerId, uint32_t teamIndex,
                                     uint64_t speed) {
    auto* snapshot = request.add_players();
    snapshot->set_player_id(playerId);
    snapshot->set_player_name("职业技能");
    snapshot->set_level(30);
    snapshot->set_class_id(kClassId);
    snapshot->set_team_index(teamIndex);
    snapshot->set_max_health(1000);
    snapshot->set_max_mana(100);
    auto* attributes = snapshot->mutable_base_attributes();
    attributes->set_health(1000);
    attributes->set_mana(100);
    attributes->set_speed(speed);
    snapshot->set_physical_attack(100);
    snapshot->set_magic_attack(100);
    return snapshot;
}

void Learn(BattlePlayerSnapshot* snapshot, uint32_t skillId, uint32_t rank, bool castable = true) {
    (*snapshot->mutable_skill_ranks())[skillId] = rank;
    if (castable) {
        snapshot->add_skill_table_ids(skillId);
    }
}

BattleAction Skill(uint32_t skillId, uint64_t targetId) {
    BattleAction action;
    action.set_action_type(BATTLE_ACTION_SKILL);
    action.set_skill_table_id(skillId);
    action.set_target_id(targetId);
    return action;
}

BattleAction Plain(eBattleActionType type, uint64_t targetId = 0) {
    BattleAction action;
    action.set_action_type(type);
    action.set_target_id(targetId);
    return action;
}

const BattleActorState* FindActorIn(const BattleStateS2C& state, uint64_t actorId) {
    for (const auto& actor : state.actors()) {
        if (actor.actor_id() == actorId) {
            return &actor;
        }
    }
    return nullptr;
}

const BattleBuffEntry* FindBuffOn(const BattleActorState& actor, uint32_t buffTableId) {
    for (const auto& buff : actor.buffs()) {
        if (buff.buff_table_id() == buffTableId) {
            return &buff;
        }
    }
    return nullptr;
}

std::vector<const BattleEventItem*> EventsOf(const TurnResultS2C& result, eBattleEventType type,
                                             uint64_t sourceId) {
    std::vector<const BattleEventItem*> found;
    for (const auto& event : result.events()) {
        if (event.event_type() == type && event.source_id() == sourceId) {
            found.push_back(&event);
        }
    }
    return found;
}

int CountFromSource(const TurnResultS2C& result, uint64_t sourceId) {
    int count = 0;
    for (const auto& event : result.events()) {
        if (event.source_id() == sourceId) {
            ++count;
        }
    }
    return count;
}

// ---------------------------------------------------------------------------
// 快照复核
// ---------------------------------------------------------------------------

TEST(ClassSkillEngineTest, InitKeepsOnlyRankedSkillsOfOwnClassClampedToLevel) {
    TurnBattleEngine engine(MakeClassProvider());
    auto request = MakeRequest(9701, kPvpMode);
    auto* caster = AddClassPlayer(request, kCaster, 0, 120);
    caster->set_level(10);                          // 上限 = min(10, 1 + (10 - 1) / 2) = 5
    Learn(caster, kStrike, 7);                      // 夹到 5
    (*caster->mutable_skill_ranks())[kSweep] = 0;   // 0 级:丢掉,技能也不可用
    caster->add_skill_table_ids(kSweep);
    Learn(caster, kIronPassive, 2, false);          // 被动:留等级,不进可施放列表
    caster->add_skill_table_ids(kIronPassive);      // 硬塞进来也要被剔掉
    Learn(caster, kOtherClass, 3);                  // 别的职业:等级与技能都丢掉
    AddClassPlayer(request, kFoe, 1, 100);
    ASSERT_TRUE(engine.Initialize(request));

    const auto state = engine.BuildStateSnapshot();
    const auto* actor = FindActorIn(state, kCaster);
    ASSERT_NE(actor, nullptr);
    ASSERT_EQ(actor->skill_table_ids_size(), 1);
    EXPECT_EQ(actor->skill_table_ids(0), kStrike);
    ASSERT_EQ(actor->skill_ranks().size(), 2u);
    EXPECT_EQ(actor->skill_ranks().at(kStrike), 5u);
    EXPECT_EQ(actor->skill_ranks().at(kIronPassive), 2u);
    EXPECT_EQ(actor->skill_ranks().count(kOtherClass), 0u);

    EXPECT_EQ(engine.ValidateAction(kCaster, Skill(kIronPassive, 0)), kInvalidParameter);
    EXPECT_EQ(engine.ValidateAction(kCaster, Skill(kSweep, kFoe)), kInvalidParameter);
    EXPECT_EQ(engine.ValidateAction(kCaster, Skill(kOtherClass, kFoe)), kInvalidParameter);
}

TEST(ClassSkillEngineTest, PetsNeverOwnClassSkills) {
    TurnBattleEngine engine(MakeClassProvider());
    auto request = MakeRequest(9702, kPvpMode);
    auto* owner = AddClassPlayer(request, kCaster, 0, 120);
    auto* pet = owner->add_pets();
    pet->set_pet_id(88);
    pet->set_owner_player_id(kCaster);
    pet->mutable_base_attributes()->set_health(100);
    pet->set_max_health(100);
    pet->add_skill_table_ids(kStrike);
    AddClassPlayer(request, kFoe, 1, 100);
    ASSERT_TRUE(engine.Initialize(request));

    const auto state = engine.BuildStateSnapshot();
    for (const auto& actor : state.actors()) {
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET) {
            EXPECT_EQ(actor.skill_table_ids_size(), 0);
        }
    }
}

TEST(ClassSkillEngineTest, UnknownTargetTeamIsRejectedAtSubmit) {
    TurnBattleEngine engine(MakeClassProvider());
    auto request = MakeRequest(9703, kPvpMode);
    Learn(AddClassPlayer(request, kCaster, 0, 120), kBadTeam, 1);
    AddClassPlayer(request, kFoe, 1, 100);
    ASSERT_TRUE(engine.Initialize(request));
    EXPECT_EQ(engine.ValidateAction(kCaster, Skill(kBadTeam, kFoe)), kInvalidTableData);
}

// ---------------------------------------------------------------------------
// 耗蓝 / 伤害 / 目标
// ---------------------------------------------------------------------------

TEST(ClassSkillEngineTest, ManaCostIsPercentOfMaxManaAndChargedOnceForMultiTarget) {
    TurnBattleEngine engine(MakeClassProvider());
    auto request = MakeRequest(9704, kPvpMode);
    auto* caster = AddClassPlayer(request, kCaster, 0, 300);
    caster->set_max_mana(250);
    caster->mutable_base_attributes()->set_mana(250);
    Learn(caster, kSweep, 1);
    AddClassPlayer(request, kFoe, 1, 100);
    AddClassPlayer(request, kFoe2, 1, 100);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kCaster, Skill(kSweep, kFoe2));
    engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_DEFEND));
    engine.SubmitAction(kFoe2, Plain(BATTLE_ACTION_DEFEND));
    const auto result = engine.ResolveCurrentRound();

    const auto manaEvents = EventsOf(result, BATTLE_EVENT_MANA, kCaster);
    ASSERT_EQ(manaEvents.size(), 1u);  // 两个目标也只扣一次
    EXPECT_EQ(manaEvents[0]->value(), 15u);  // ceil(250 × 0.06)
    ASSERT_TRUE(manaEvents[0]->has_mana_delta());
    EXPECT_EQ(manaEvents[0]->mana_delta(), -15);
    EXPECT_EQ(manaEvents[0]->target_mana_after(), 235u);

    const auto damage = EventsOf(result, BATTLE_EVENT_DAMAGE, kCaster);
    ASSERT_EQ(damage.size(), 2u);
    EXPECT_EQ(damage[0]->target_id(), kFoe2);  // 选中的合法主目标排第一
    EXPECT_EQ(damage[1]->target_id(), kFoe);
    EXPECT_EQ(damage[0]->hit_index(), 0u);
    EXPECT_EQ(damage[1]->hit_index(), 1u);
}

TEST(ClassSkillEngineTest, DamageScalesWithRank) {
    const auto dealtAtRank = [](uint32_t rank) {
        TurnBattleEngine engine(MakeClassProvider());
        auto request = MakeRequest(9705, kPvpMode);
        Learn(AddClassPlayer(request, kCaster, 0, 300), kStrike, rank);
        AddClassPlayer(request, kFoe, 1, 100);
        EXPECT_TRUE(engine.Initialize(request));
        engine.SubmitAction(kCaster, Skill(kStrike, kFoe));
        engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_ATTACK, kCaster));
        const auto result = engine.ResolveCurrentRound();
        const auto damage = EventsOf(result, BATTLE_EVENT_DAMAGE, kCaster);
        EXPECT_EQ(damage.size(), 1u);
        return damage.empty() ? 0ull : static_cast<unsigned long long>(damage[0]->value());
    };
    // 与引擎同一顺序算:公式伤害 × PVP 系数 → × 等级系数 → 向上取整(无暴击、无防御、目标 30 级)
    const auto expected = [](uint32_t rank) {
        const double beforeRank =
            std::max(combatdamage::DamageBeforeCritical(10.0, 0, 100, 1.0, 0, 0, 0, 30) * turnbattle::kPvpDamageScale,
                     0.0);
        return static_cast<unsigned long long>(std::ceil(beforeRank * classskill::DamageRankFactor(rank)));
    };
    EXPECT_EQ(dealtAtRank(1), expected(1));
    EXPECT_EQ(dealtAtRank(6), expected(6));
    EXPECT_GT(dealtAtRank(6), dealtAtRank(1));
}

TEST(ClassSkillEngineTest, AllySkillRejectsEnemyTargetAndHealsLowestHealthRatioFirst) {
    TurnBattleEngine engine(MakeClassProvider());
    auto request = MakeRequest(9706, kPvpMode);
    Learn(AddClassPlayer(request, kCaster, 0, 300), kMend, 1);
    AddClassPlayer(request, kAlly, 0, 100)->mutable_base_attributes()->set_health(400);
    AddClassPlayer(request, kFoe, 1, 100);
    ASSERT_TRUE(engine.Initialize(request));

    EXPECT_EQ(engine.ValidateAction(kCaster, Skill(kMend, kFoe)), kSkillInvalidTarget);
    ASSERT_EQ(engine.ValidateAction(kCaster, Skill(kMend, 0)), kSuccess);  // 0 = 引擎按规则选

    engine.SubmitAction(kCaster, Skill(kMend, 0));
    engine.SubmitAction(kAlly, Plain(BATTLE_ACTION_DEFEND));
    engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_DEFEND));
    const auto result = engine.ResolveCurrentRound();

    const auto heals = EventsOf(result, BATTLE_EVENT_HEAL, kCaster);
    ASSERT_EQ(heals.size(), 1u);
    EXPECT_EQ(heals[0]->target_id(), kAlly);  // 40% 的队友排在满血的自己前面
    EXPECT_EQ(heals[0]->value(), 340u);       // 法伤 100 × 1.0 + 30 级 × 8
    EXPECT_EQ(heals[0]->target_health_after(), 740u);
    EXPECT_TRUE(EventsOf(result, BATTLE_EVENT_DAMAGE, kCaster).empty());
}

TEST(ClassSkillEngineTest, GroupHealSkipsDeadAlliesAndNeverRevives) {
    TurnBattleEngine engine(MakeClassProvider());
    auto request = MakeRequest(9707, kPvpMode);
    auto* caster = AddClassPlayer(request, kCaster, 0, 100);
    caster->mutable_base_attributes()->set_health(500);
    Learn(caster, kMendAll, 1);
    AddClassPlayer(request, kAlly, 0, 100);
    auto* foe = AddClassPlayer(request, kFoe, 1, 300);
    foe->add_skill_table_ids(kLegacyNuke);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kFoe, Skill(kLegacyNuke, kAlly));  // 先手把队友打死
    engine.SubmitAction(kCaster, Skill(kMendAll, 0));
    engine.SubmitAction(kAlly, Plain(BATTLE_ACTION_DEFEND));
    const auto result = engine.ResolveCurrentRound();

    const auto heals = EventsOf(result, BATTLE_EVENT_HEAL, kCaster);
    ASSERT_EQ(heals.size(), 1u);
    EXPECT_EQ(heals[0]->target_id(), kCaster);
    const auto* ally = FindActorIn(result.state(), kAlly);
    ASSERT_NE(ally, nullptr);
    EXPECT_TRUE(ally->is_dead());
    EXPECT_EQ(ally->attributes().health(), 0u);
}

TEST(ClassSkillEngineTest, OutgoingHealPassiveAndLowHealthBonusStack) {
    TurnBattleEngine engine(MakeClassProvider());
    auto request = MakeRequest(9708, kPvpMode);
    auto* caster = AddClassPlayer(request, kCaster, 0, 300);
    Learn(caster, kMend, 1);
    Learn(caster, kHealPassive, 1, false);
    AddClassPlayer(request, kAlly, 0, 100)->mutable_base_attributes()->set_health(100);  // 10% < 30%
    AddClassPlayer(request, kFoe, 1, 100);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kCaster, Skill(kMend, kAlly));
    engine.SubmitAction(kAlly, Plain(BATTLE_ACTION_DEFEND));
    engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_DEFEND));
    const auto result = engine.ResolveCurrentRound();

    double amount = (100.0 * 1.0 + 30.0 * 8.0) * classskill::DamageRankFactor(1) *
                    (1.0 + 0.10 * classskill::PassiveRankFactor(1));
    amount *= 1.0 + 0.3;
    const auto heals = EventsOf(result, BATTLE_EVENT_HEAL, kCaster);
    ASSERT_EQ(heals.size(), 1u);
    EXPECT_EQ(heals[0]->value(), static_cast<uint64_t>(amount));
}

// ---------------------------------------------------------------------------
// 增减益 / 冷却 / 先手 / 回蓝 / 驱散
// ---------------------------------------------------------------------------

TEST(ClassSkillEngineTest, SelfDefenseBuffLastsTwoFullRoundsWithRankStrength) {
    TurnBattleEngine engine(MakeClassProvider());
    auto request = MakeRequest(9709, kPvpMode);
    Learn(AddClassPlayer(request, kCaster, 0, 300), kGuard, 6);
    AddClassPlayer(request, kFoe, 1, 100);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kCaster, Skill(kGuard, 0));
    engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_DEFEND));
    auto result = engine.ResolveCurrentRound();
    const auto* caster = FindActorIn(result.state(), kCaster);
    ASSERT_NE(caster, nullptr);
    const auto* guard = FindBuffOn(*caster, kBuffGuard);
    ASSERT_NE(guard, nullptr);
    EXPECT_DOUBLE_EQ(guard->stat_percent(), 0.25 * classskill::SupportRankFactor(6));
    EXPECT_EQ(guard->remain_rounds(), 2u);  // 施加回合末不扣

    for (const uint32_t expectedRemain : {1u}) {
        engine.SubmitAction(kCaster, Plain(BATTLE_ACTION_DEFEND));
        engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_DEFEND));
        result = engine.ResolveCurrentRound();
        caster = FindActorIn(result.state(), kCaster);
        ASSERT_NE(caster, nullptr);
        guard = FindBuffOn(*caster, kBuffGuard);
        ASSERT_NE(guard, nullptr);
        EXPECT_EQ(guard->remain_rounds(), expectedRemain);
    }

    engine.SubmitAction(kCaster, Plain(BATTLE_ACTION_DEFEND));
    engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_DEFEND));
    result = engine.ResolveCurrentRound();
    caster = FindActorIn(result.state(), kCaster);
    ASSERT_NE(caster, nullptr);
    EXPECT_EQ(FindBuffOn(*caster, kBuffGuard), nullptr);  // 第 3 回合末到期
    bool removed = false;
    for (const auto& event : result.events()) {
        removed |= event.event_type() == BATTLE_EVENT_BUFF_REMOVE && event.buff_table_id() == kBuffGuard;
    }
    EXPECT_TRUE(removed);
}

TEST(ClassSkillEngineTest, DefenseBuffReducesDamageTaken) {
    TurnBattleEngine engine(MakeClassProvider());
    auto request = MakeRequest(9710, kPvpMode);
    auto* caster = AddClassPlayer(request, kCaster, 0, 100);
    caster->set_defense(3000);              // 防御够厚,+25% 才能在向上取整后看得出差别
    Learn(caster, kGuard, 1);
    AddClassPlayer(request, kFoe, 1, 300)->set_physical_attack(1000);  // 每回合都先出手
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_ATTACK, kCaster));
    engine.SubmitAction(kCaster, Skill(kGuard, 0));
    const auto first = engine.ResolveCurrentRound();
    const auto firstHit = EventsOf(first, BATTLE_EVENT_DAMAGE, kFoe);
    ASSERT_EQ(firstHit.size(), 1u);

    engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_ATTACK, kCaster));
    engine.SubmitAction(kCaster, Plain(BATTLE_ACTION_ATTACK, kFoe));
    const auto second = engine.ResolveCurrentRound();
    const auto secondHit = EventsOf(second, BATTLE_EVENT_DAMAGE, kFoe);
    ASSERT_EQ(secondHit.size(), 1u);
    EXPECT_LT(secondHit[0]->value(), firstHit[0]->value());
}

TEST(ClassSkillEngineTest, CooldownBlocksFollowingRoundsWithoutLockingLegacySkills) {
    TurnBattleEngine engine(MakeClassProvider());
    auto request = MakeRequest(9711, kPvpMode);
    auto* caster = AddClassPlayer(request, kCaster, 0, 300);
    Learn(caster, kStrike, 1);
    caster->add_skill_table_ids(kLegacyNoCd);
    auto* foe = AddClassPlayer(request, kFoe, 1, 100);
    foe->set_max_health(1000000);
    foe->mutable_base_attributes()->set_health(1000000);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kCaster, Skill(kStrike, kFoe));
    engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_DEFEND));
    auto result = engine.ResolveCurrentRound();
    const auto* actor = FindActorIn(result.state(), kCaster);
    ASSERT_NE(actor, nullptr);
    EXPECT_EQ(actor->skill_cooldown_rounds().at(kStrike), 2u);  // 还要等 2 个回合
    EXPECT_EQ(engine.ValidateAction(kCaster, Skill(kStrike, kFoe)), kSkillCooldownNotReady);
    // cooldown_id 同为 0 的老技能不被职业技能的冷却锁住
    EXPECT_EQ(engine.ValidateAction(kCaster, Skill(kLegacyNoCd, kFoe)), kSuccess);

    engine.SubmitAction(kCaster, Plain(BATTLE_ACTION_DEFEND));
    engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_DEFEND));
    engine.ResolveCurrentRound();
    EXPECT_EQ(engine.ValidateAction(kCaster, Skill(kStrike, kFoe)), kSkillCooldownNotReady);

    engine.SubmitAction(kCaster, Plain(BATTLE_ACTION_DEFEND));
    engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_DEFEND));
    engine.ResolveCurrentRound();
    EXPECT_EQ(engine.ValidateAction(kCaster, Skill(kStrike, kFoe)), kSuccess);
}

TEST(ClassSkillEngineTest, InitiativeActsBeforeFasterActorOnlyWhenCastable) {
    const auto firstActor = [](uint64_t casterMana) {
        TurnBattleEngine engine(MakeClassProvider());
        auto request = MakeRequest(9712, kPvpMode);
        auto* caster = AddClassPlayer(request, kCaster, 0, 100);
        caster->mutable_base_attributes()->set_mana(casterMana);
        Learn(caster, kSwift, 1);
        AddClassPlayer(request, kFoe, 1, 300);
        EXPECT_TRUE(engine.Initialize(request));
        engine.SubmitAction(kCaster, Skill(kSwift, kFoe));  // 蓝不够时提交被拒,超时按普攻
        engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_DEFEND));
        engine.ResolveCurrentRound();
        return engine.LastActionOrder().empty() ? 0ull : engine.LastActionOrder().front();
    };
    EXPECT_EQ(firstActor(100), kCaster);  // 先手优先级排在速度之前
    EXPECT_EQ(firstActor(0), kFoe);       // 放不出来就不能借优先级插队
}

TEST(ClassSkillEngineTest, SpeedDebuffTakesEffectFromNextRound) {
    TurnBattleEngine engine(MakeClassProvider());
    auto request = MakeRequest(9713, kPvpMode);
    Learn(AddClassPlayer(request, kCaster, 0, 200), kSlow, 1);
    auto* foe = AddClassPlayer(request, kFoe, 1, 220);
    foe->set_max_health(1000000);
    foe->mutable_base_attributes()->set_health(1000000);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kCaster, Skill(kSlow, kFoe));
    engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_DEFEND));
    const auto first = engine.ResolveCurrentRound();
    ASSERT_FALSE(engine.LastActionOrder().empty());
    EXPECT_EQ(engine.LastActionOrder().front(), kFoe);  // 本回合的出手序开局时已排定
    const auto* slowed = FindActorIn(first.state(), kFoe);
    ASSERT_NE(slowed, nullptr);
    const auto* slow = FindBuffOn(*slowed, kBuffSlow);
    ASSERT_NE(slow, nullptr);
    EXPECT_DOUBLE_EQ(slow->stat_percent(), -0.2);

    engine.SubmitAction(kCaster, Plain(BATTLE_ACTION_DEFEND));
    engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_DEFEND));
    engine.ResolveCurrentRound();
    ASSERT_FALSE(engine.LastActionOrder().empty());
    EXPECT_EQ(engine.LastActionOrder().front(), kCaster);  // 220 × 0.8 = 176 < 200
}

TEST(ClassSkillEngineTest, RestoreManaIsCappedAndCarriesPositiveDelta) {
    TurnBattleEngine engine(MakeClassProvider());
    auto request = MakeRequest(9714, kPvpMode);
    auto* caster = AddClassPlayer(request, kCaster, 0, 300);
    caster->mutable_base_attributes()->set_mana(95);
    Learn(caster, kFocus, 1);
    AddClassPlayer(request, kFoe, 1, 100);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kCaster, Skill(kFocus, 0));
    engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_DEFEND));
    auto result = engine.ResolveCurrentRound();
    auto mana = EventsOf(result, BATTLE_EVENT_MANA, kCaster);
    ASSERT_EQ(mana.size(), 1u);  // 不耗蓝,只有回蓝这一条
    EXPECT_EQ(mana[0]->target_id(), kCaster);
    EXPECT_EQ(mana[0]->value(), 5u);  // 回 20 但只缺 5
    ASSERT_TRUE(mana[0]->has_mana_delta());
    EXPECT_EQ(mana[0]->mana_delta(), 5);
    EXPECT_EQ(mana[0]->target_mana_after(), 100u);

    engine.SubmitAction(kCaster, Skill(kFocus, 0));
    engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_DEFEND));
    result = engine.ResolveCurrentRound();
    mana = EventsOf(result, BATTLE_EVENT_MANA, kCaster);
    ASSERT_EQ(mana.size(), 1u);
    EXPECT_EQ(mana[0]->value(), 0u);
    ASSERT_TRUE(mana[0]->has_mana_delta());
    EXPECT_EQ(mana[0]->mana_delta(), 0);
}

TEST(ClassSkillEngineTest, CleanseRemovesFirstDebuffAndKeepsBuffs) {
    TurnBattleEngine engine(MakeClassProvider());
    auto request = MakeRequest(9715, kPvpMode);
    Learn(AddClassPlayer(request, kCaster, 0, 300), kCleanse, 1);
    auto* ally = AddClassPlayer(request, kAlly, 0, 100);
    // 顺序:增益 → 毒 → 减速。快照带进来的 buff 局内幅度会被清掉,增益行没有 Debuff 标签,不算减益
    uint64_t buffId = 1;
    for (const uint32_t buffTableId : {kBuffGuard, kBuffPoison, kBuffSlow}) {
        auto* buff = ally->add_buffs();
        buff->set_buff_id(buffId++);
        buff->set_buff_table_id(buffTableId);
        buff->set_layer(1);
        buff->set_remain_rounds(2);
    }
    AddClassPlayer(request, kFoe, 1, 100);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kCaster, Skill(kCleanse, kAlly));
    engine.SubmitAction(kAlly, Plain(BATTLE_ACTION_DEFEND));
    engine.SubmitAction(kFoe, Plain(BATTLE_ACTION_DEFEND));
    const auto result = engine.ResolveCurrentRound();

    int removedPoison = 0;
    for (const auto& event : result.events()) {
        if (event.event_type() == BATTLE_EVENT_BUFF_REMOVE && event.target_id() == kAlly) {
            removedPoison += event.buff_table_id() == kBuffPoison ? 1 : 0;
        }
    }
    EXPECT_EQ(removedPoison, 1);
    const auto* after = FindActorIn(result.state(), kAlly);
    ASSERT_NE(after, nullptr);
    EXPECT_EQ(FindBuffOn(*after, kBuffPoison), nullptr);
    EXPECT_NE(FindBuffOn(*after, kBuffGuard), nullptr);
    EXPECT_NE(FindBuffOn(*after, kBuffSlow), nullptr);  // 只驱散 1 个
}

TEST(ClassSkillEngineTest, SilencedAllySkillFizzlesWhileEnemySkillFallsBackToAttack) {
    const auto runSilenced = [](uint32_t casterSkill, uint64_t casterTarget) {
        TurnBattleEngine engine(MakeClassProvider());
        auto request = MakeRequest(9716, kPvpMode);
        auto* caster = AddClassPlayer(request, kCaster, 0, 100);
        Learn(caster, kMend, 1);
        Learn(caster, kStrike, 1);
        AddClassPlayer(request, kAlly, 0, 90);
        auto* foe = AddClassPlayer(request, kFoe, 1, 300);
        foe->add_skill_table_ids(kLegacySilence);
        EXPECT_TRUE(engine.Initialize(request));
        engine.SubmitAction(kCaster, Skill(casterSkill, casterTarget));  // 提交时还没被沉默
        engine.SubmitAction(kAlly, Plain(BATTLE_ACTION_DEFEND));
        engine.SubmitAction(kFoe, Skill(kLegacySilence, kCaster));       // 先手把施法者沉默
        return engine.ResolveCurrentRound();
    };

    const auto allyResult = runSilenced(kMend, kAlly);
    // 友方技能放不出来不降级成普攻(那等于去打一个随机敌人),这一手作废
    EXPECT_EQ(CountFromSource(allyResult, kCaster), 0);

    const auto enemyResult = runSilenced(kStrike, kFoe);
    EXPECT_EQ(EventsOf(enemyResult, BATTLE_EVENT_ATTACK, kCaster).size(), 1u);  // 敌方技能照老规则降级普攻
    EXPECT_TRUE(EventsOf(enemyResult, BATTLE_EVENT_SKILL, kCaster).empty());
}

TEST(ClassSkillEngineTest, SameSeedClassSkillBattleIsDeterministic) {
    const auto run = []() {
        TurnBattleEngine engine(MakeClassProvider());
        auto request = MakeRequest(9717, kPvpMode);
        auto* caster = AddClassPlayer(request, kCaster, 0, 200);
        caster->mutable_base_attributes()->set_critchance(50);  // 逼出暴击掷骰
        Learn(caster, kSweep, 3);
        Learn(caster, kGuard, 2);
        AddClassPlayer(request, kFoe, 1, 150)->mutable_base_attributes()->set_critchance(50);
        AddClassPlayer(request, kFoe2, 1, 120);
        EXPECT_TRUE(engine.Initialize(request));
        std::string stream;
        for (int round = 0; round < 6 && engine.Outcome() == BATTLE_OUTCOME_ONGOING; ++round) {
            engine.SubmitAction(kCaster, round % 2 == 0 ? Skill(kSweep, 0) : Skill(kGuard, 0));
            const auto result = engine.ResolveCurrentRound();
            for (const auto& event : result.events()) {
                stream += event.SerializeAsString();
                stream += '|';
            }
        }
        return stream;
    };
    EXPECT_EQ(run(), run());
}

// ---------------------------------------------------------------------------
// 玩家挂机选技:只验证正式行动与事件,不读取候选评分或私有选择过程。
// ---------------------------------------------------------------------------

class AutoClassSkillEngineTest : public ::testing::Test {
protected:
    void SetUp() override {
        provider = MakeClassProvider();
        // 自动选技夹具的单体技明确强于普攻;不改原职业技能用例的数值。
        provider->AddSkill(kStrike).set_attack_multiplier(1.6);
    }

    static CreateBattleRequest Duel(uint64_t battleId) {
        auto request = MakeRequest(battleId, kPvpMode);
        AddClassPlayer(request, kCaster, 0, 300);
        AddClassPlayer(request, kFoe, 1, 100);
        return request;
    }

    static void CarryBuff(BattlePlayerSnapshot* player, uint32_t buffTableId) {
        auto* buff = player->add_buffs();
        buff->set_buff_id(1);
        buff->set_buff_table_id(buffTableId);
        buff->set_layer(1);
        buff->set_remain_rounds(3);
    }

    static TurnResultS2C Round(TurnBattleEngine& engine) {
        const auto state = engine.BuildStateSnapshot();
        for (const auto& actor : state.actors()) {
            if (actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER && actor.actor_id() != kCaster &&
                !actor.is_dead() && !actor.fled()) {
                const auto action = Plain(BATTLE_ACTION_DEFEND);
                EXPECT_EQ(engine.ValidateAction(actor.actor_id(), action), kSuccess);
                engine.SubmitAction(actor.actor_id(), action);
            }
        }
        return engine.ResolveCurrentRound();
    }

    static void ExpectSkill(const TurnResultS2C& result, uint32_t skillId, uint64_t targetId = 0) {
        const auto skills = EventsOf(result, BATTLE_EVENT_SKILL, kCaster);
        ASSERT_EQ(skills.size(), 1u);
        EXPECT_EQ(skills[0]->skill_table_id(), skillId);
        if (targetId != 0) {
            EXPECT_EQ(skills[0]->target_id(), targetId);
        }
        EXPECT_TRUE(EventsOf(result, BATTLE_EVENT_ATTACK, kCaster).empty());
    }

    static void ExpectBasicAttack(const TurnResultS2C& result) {
        EXPECT_TRUE(EventsOf(result, BATTLE_EVENT_SKILL, kCaster).empty());
        EXPECT_EQ(EventsOf(result, BATTLE_EVENT_ATTACK, kCaster).size(), 1u);
    }

    static constexpr uint64_t kExtraAlly = 7005;
    std::shared_ptr<MemoryBattleDataProvider> provider;
};

TEST_F(AutoClassSkillEngineTest, ChoosesSingleTargetOrGroupDamageFromActualEnemyCount) {
    for (const bool multipleEnemies : {false, true}) {
        SCOPED_TRACE(multipleEnemies);
        auto request = Duel(9801);
        auto* caster = request.mutable_players(0);
        Learn(caster, kSweep, 1);
        Learn(caster, kStrike, 1);
        if (multipleEnemies) {
            AddClassPlayer(request, kFoe2, 1, 100);
        }
        TurnBattleEngine engine(provider);
        ASSERT_TRUE(engine.Initialize(request));
        ASSERT_EQ(engine.SetActorAuto(kCaster, true), kSuccess);
        const auto result = Round(engine);
        ExpectSkill(result, multipleEnemies ? kSweep : kStrike);
        EXPECT_EQ(EventsOf(result, BATTLE_EVENT_DAMAGE, kCaster).size(), multipleEnemies ? 2u : 1u);
    }
}

TEST_F(AutoClassSkillEngineTest, NonAutoTimeoutKeepsBasicAttackAndPendingManualActionWins) {
    for (const bool automatic : {false, true}) {
        SCOPED_TRACE(automatic);
        auto request = Duel(9802);
        Learn(request.mutable_players(0), kStrike, 1);
        TurnBattleEngine engine(provider);
        ASSERT_TRUE(engine.Initialize(request));
        if (automatic) {
            // 已提交的玩家指令不能被随后打开的挂机覆盖。
            const auto manual = Plain(BATTLE_ACTION_DEFEND);
            ASSERT_EQ(engine.ValidateAction(kCaster, manual), kSuccess);
            engine.SubmitAction(kCaster, manual);
            ASSERT_EQ(engine.SetActorAuto(kCaster, true), kSuccess);
        }
        const auto result = Round(engine);
        if (automatic) {
            EXPECT_TRUE(EventsOf(result, BATTLE_EVENT_SKILL, kCaster).empty());
            EXPECT_TRUE(EventsOf(result, BATTLE_EVENT_ATTACK, kCaster).empty());
            EXPECT_EQ(EventsOf(result, BATTLE_EVENT_DEFEND, kCaster).size(), 1u);
        } else {
            ExpectBasicAttack(result);
        }
    }
}

TEST_F(AutoClassSkillEngineTest, FiltersInvalidPassiveUnlearnedAndForeignSkillsWithoutChangingPetOrLegacyFallback) {
    auto request = Duel(9803);
    auto* caster = request.mutable_players(0);
    Learn(caster, kBadTeam, 1);
    Learn(caster, kIronPassive, 1);
    Learn(caster, kSweep, 0);
    Learn(caster, kOtherClass, 1);
    Learn(caster, 99999, 1);
    caster->add_skill_table_ids(kLegacyNuke);
    auto* pet = caster->add_pets();
    pet->set_pet_id(88);
    pet->set_owner_player_id(kCaster);
    pet->set_max_health(1000);
    pet->mutable_base_attributes()->set_health(1000);
    pet->mutable_base_attributes()->set_speed(200);
    pet->set_physical_attack(100);
    pet->add_skill_table_ids(kStrike);
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    ASSERT_EQ(engine.SetActorAuto(kCaster, true), kSuccess);
    const auto result = Round(engine);
    ExpectBasicAttack(result);
    int pets = 0;
    for (const auto& actor : result.state().actors()) {
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET) {
            ++pets;
            EXPECT_TRUE(EventsOf(result, BATTLE_EVENT_SKILL, actor.actor_id()).empty());
            EXPECT_EQ(EventsOf(result, BATTLE_EVENT_ATTACK, actor.actor_id()).size(), 1u);
        }
    }
    EXPECT_EQ(pets, 1);
}

TEST_F(AutoClassSkillEngineTest, InsufficientManaOrSilenceFallsBackWithoutSpendingMana) {
    for (const bool silenced : {false, true}) {
        SCOPED_TRACE(silenced);
        auto request = Duel(9804);
        auto* caster = request.mutable_players(0);
        Learn(caster, kStrike, 1);
        if (silenced) {
            CarryBuff(caster, kBuffSilence);
        } else {
            caster->mutable_base_attributes()->set_mana(0);
        }
        TurnBattleEngine engine(provider);
        ASSERT_TRUE(engine.Initialize(request));
        ASSERT_EQ(engine.SetActorAuto(kCaster, true), kSuccess);
        const auto result = Round(engine);
        ExpectBasicAttack(result);
        EXPECT_TRUE(EventsOf(result, BATTLE_EVENT_MANA, kCaster).empty());
    }
}

TEST_F(AutoClassSkillEngineTest, CoolingDownSkillIsNotSelectedAgain) {
    auto request = Duel(9805);
    Learn(request.mutable_players(0), kStrike, 1);
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    ASSERT_EQ(engine.SetActorAuto(kCaster, true), kSuccess);
    ExpectSkill(Round(engine), kStrike);
    ASSERT_EQ(engine.ValidateAction(kCaster, Skill(kStrike, kFoe)), kSkillCooldownNotReady);
    const auto second = Round(engine);
    ExpectBasicAttack(second);
    EXPECT_TRUE(EventsOf(second, BATTLE_EVENT_MANA, kCaster).empty());
}

TEST_F(AutoClassSkillEngineTest, DamageNotBetterThanBasicAttackDoesNotSpendMana) {
    provider->AddSkill(kStrike).set_attack_multiplier(1.0);
    auto request = Duel(9806);
    Learn(request.mutable_players(0), kStrike, 1);
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    ASSERT_EQ(engine.SetActorAuto(kCaster, true), kSuccess);
    const auto result = Round(engine);
    ExpectBasicAttack(result);
    EXPECT_TRUE(EventsOf(result, BATTLE_EVENT_MANA, kCaster).empty());
}

TEST_F(AutoClassSkillEngineTest, CriticalAllyHealingTakesPriorityOverCleanseAndDamage) {
    auto request = Duel(9807);
    auto* caster = request.mutable_players(0);
    for (const uint32_t skillId : {kStrike, kMend, kMendAll, kCleanse}) {
        Learn(caster, skillId, 1);
    }
    AddClassPlayer(request, kAlly, 0, 100)->mutable_base_attributes()->set_health(100);
    auto* poisoned = AddClassPlayer(request, kExtraAlly, 0, 100);
    poisoned->mutable_base_attributes()->set_health(600);
    CarryBuff(poisoned, kBuffPoison);
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    ASSERT_EQ(engine.SetActorAuto(kCaster, true), kSuccess);
    const auto result = Round(engine);
    ExpectSkill(result, kMend, kAlly);
    const auto heals = EventsOf(result, BATTLE_EVENT_HEAL, kCaster);
    ASSERT_EQ(heals.size(), 1u);
    EXPECT_EQ(heals[0]->target_id(), kAlly);
    EXPECT_GT(heals[0]->value(), 0u);
}

TEST_F(AutoClassSkillEngineTest, GroupHealingWinsWhenItRepairsMoreActualMissingHealth) {
    auto request = Duel(9808);
    auto* caster = request.mutable_players(0);
    caster->mutable_base_attributes()->set_health(600);
    Learn(caster, kMend, 1);
    Learn(caster, kMendAll, 1);
    AddClassPlayer(request, kAlly, 0, 100)->mutable_base_attributes()->set_health(600);
    AddClassPlayer(request, kExtraAlly, 0, 100)->mutable_base_attributes()->set_health(600);
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    ASSERT_EQ(engine.SetActorAuto(kCaster, true), kSuccess);
    const auto result = Round(engine);
    ExpectSkill(result, kMendAll);
    const auto heals = EventsOf(result, BATTLE_EVENT_HEAL, kCaster);
    ASSERT_EQ(heals.size(), 3u);
    uint64_t restored = 0;
    for (const auto* heal : heals) {
        EXPECT_GT(heal->value(), 0u);
        restored += heal->value();
    }
    EXPECT_GT(restored, 340u);  // 单体夹具最多修复 340;群疗确实增加本回合有效恢复。
}

TEST_F(AutoClassSkillEngineTest, FullHealthPartyUsesDamageInsteadOfHealing) {
    auto request = Duel(9809);
    auto* caster = request.mutable_players(0);
    for (const uint32_t skillId : {kMend, kMendAll, kStrike}) {
        Learn(caster, skillId, 1);
    }
    AddClassPlayer(request, kAlly, 0, 100);
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    ASSERT_EQ(engine.SetActorAuto(kCaster, true), kSuccess);
    const auto result = Round(engine);
    ExpectSkill(result, kStrike, kFoe);
    EXPECT_TRUE(EventsOf(result, BATTLE_EVENT_HEAL, kCaster).empty());
}

TEST_F(AutoClassSkillEngineTest, CleanseExplicitlyTargetsDebuffedAllyRatherThanLowerHealthAlly) {
    auto request = Duel(9810);
    auto* caster = request.mutable_players(0);
    Learn(caster, kMend, 1);
    Learn(caster, kCleanse, 1);
    CarryBuff(AddClassPlayer(request, kAlly, 0, 100), kBuffPoison);
    AddClassPlayer(request, kExtraAlly, 0, 100)->mutable_base_attributes()->set_health(600);
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    ASSERT_EQ(engine.SetActorAuto(kCaster, true), kSuccess);
    const auto result = Round(engine);
    ExpectSkill(result, kCleanse, kAlly);
    const auto* ally = FindActorIn(result.state(), kAlly);
    ASSERT_NE(ally, nullptr);
    EXPECT_EQ(FindBuffOn(*ally, kBuffPoison), nullptr);
    bool removedFromAlly = false;
    for (const auto& event : result.events()) {
        removedFromAlly |= event.event_type() == BATTLE_EVENT_BUFF_REMOVE && event.target_id() == kAlly &&
                           event.buff_table_id() == kBuffPoison;
    }
    EXPECT_TRUE(removedFromAlly);
}

TEST_F(AutoClassSkillEngineTest, ManaRecoveryRequiresLowManaAndPositiveGainAfterItsOwnCost) {
    struct Scenario {
        uint64_t mana;
        double cost;
        double recovery;
        bool shouldRecover;
    };
    for (const Scenario scenario : {Scenario{10, 0.05, 0.20, true}, Scenario{30, 0.05, 0.20, true},
                                    Scenario{31, 0.05, 0.20, false}, Scenario{100, 0.05, 0.20, false},
                                    Scenario{10, 0.10, 0.10, false}}) {
        SCOPED_TRACE(scenario.mana);
        provider->AddSkill(kFocus).set_mana_percent(scenario.cost);
        provider->AddSkill(kFocus).set_restore_mana_percent(scenario.recovery);
        auto request = Duel(9811);
        auto* caster = request.mutable_players(0);
        caster->mutable_base_attributes()->set_mana(scenario.mana);
        Learn(caster, kFocus, 1);
        TurnBattleEngine engine(provider);
        ASSERT_TRUE(engine.Initialize(request));
        ASSERT_EQ(engine.SetActorAuto(kCaster, true), kSuccess);
        const auto result = Round(engine);
        if (scenario.shouldRecover) {
            ExpectSkill(result, kFocus, kCaster);
            const auto* after = FindActorIn(result.state(), kCaster);
            ASSERT_NE(after, nullptr);
            EXPECT_GT(after->attributes().mana(), scenario.mana);
        } else {
            ExpectBasicAttack(result);
            EXPECT_TRUE(EventsOf(result, BATTLE_EVENT_MANA, kCaster).empty());
        }
    }
}

TEST_F(AutoClassSkillEngineTest, DefenseSupportRequiresSixtyPercentHealthAndDoesNotRepeatActiveBuff) {
    provider->AddSkill(kGuard).set_cooldown_rounds(0);
    for (const uint64_t health : {600ull, 601ull}) {
        SCOPED_TRACE(health);
        auto request = Duel(9812);
        auto* caster = request.mutable_players(0);
        caster->mutable_base_attributes()->set_health(health);
        caster->set_defense(3000);
        Learn(caster, kGuard, 1);
        TurnBattleEngine engine(provider);
        ASSERT_TRUE(engine.Initialize(request));
        ASSERT_EQ(engine.SetActorAuto(kCaster, true), kSuccess);
        const auto first = Round(engine);
        if (health == 600) {
            ExpectSkill(first, kGuard, kCaster);
            ExpectBasicAttack(Round(engine));  // 没有冷却仍不浪费行动刷新同强度增益。
        } else {
            ExpectBasicAttack(first);
        }
    }
}

TEST_F(AutoClassSkillEngineTest, StrongerExistingDefenseBuffPreventsWeakerDifferentBuffRow) {
    constexpr uint32_t kStrongGuard = 9130;
    constexpr uint32_t kStrongGuardBuff = 9930;
    auto& strong = AddClassSkill(*provider, kStrongGuard, TargetTeam::kSelf, 1);
    strong.set_buff_percent(0.50);
    strong.add_effect(kStrongGuardBuff);
    auto& buff = provider->AddBuff(kStrongGuardBuff);
    buff.CopyFrom(*provider->FindBuff(kBuffGuard));
    buff.set_id(kStrongGuardBuff);
    provider->AddSkill(kGuard).set_cooldown_rounds(0);
    auto request = Duel(9813);
    auto* caster = request.mutable_players(0);
    caster->mutable_base_attributes()->set_health(500);
    caster->set_defense(3000);
    Learn(caster, kGuard, 1);
    Learn(caster, kStrongGuard, 1);
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(request));
    const auto strongGuard = Skill(kStrongGuard, 0);
    ASSERT_EQ(engine.ValidateAction(kCaster, strongGuard), kSuccess);
    engine.SubmitAction(kCaster, strongGuard);
    ExpectSkill(Round(engine), kStrongGuard);
    ASSERT_EQ(engine.SetActorAuto(kCaster, true), kSuccess);
    ExpectBasicAttack(Round(engine));
}

TEST_F(AutoClassSkillEngineTest, SpeedDebuffRequiresAnOrderImprovementAndIsNotRepeated) {
    provider->SetSkillDamage(kSlow, 0.0);  // 此例只考察减速收益,不靠附带伤害入选。
    for (const uint64_t foeSpeed : {220ull, 100ull}) {
        SCOPED_TRACE(foeSpeed);
        auto request = Duel(9814);
        auto* caster = request.mutable_players(0);
        caster->mutable_base_attributes()->set_speed(200);
        Learn(caster, kSlow, 1);
        request.mutable_players(1)->mutable_base_attributes()->set_speed(foeSpeed);
        TurnBattleEngine engine(provider);
        ASSERT_TRUE(engine.Initialize(request));
        ASSERT_EQ(engine.SetActorAuto(kCaster, true), kSuccess);
        const auto first = Round(engine);
        if (foeSpeed == 220) {
            ExpectSkill(first, kSlow, kFoe);
            ExpectBasicAttack(Round(engine));
        } else {
            ExpectBasicAttack(first);
        }
    }
}

TEST_F(AutoClassSkillEngineTest, SpeedBuffTargetsAllyWhoCanOvertakeAndAvoidsUselessOrRepeatedBuffs) {
    constexpr uint32_t kHaste = 9131;
    constexpr uint32_t kHasteBuff = 9931;
    auto& haste = AddClassSkill(*provider, kHaste, TargetTeam::kAlly, 1);
    haste.set_buff_percent(0.30);
    haste.add_effect(kHasteBuff);
    auto& buff = provider->AddBuff(kHasteBuff);
    buff.CopyFrom(*provider->FindBuff(kBuffSlow));
    buff.set_id(kHasteBuff);
    buff.mutable_tag()->erase("Debuff");
    for (const uint64_t allySpeed : {200ull, 400ull}) {
        SCOPED_TRACE(allySpeed);
        auto request = Duel(9815);
        Learn(request.mutable_players(0), kHaste, 1);
        request.mutable_players(1)->mutable_base_attributes()->set_speed(220);
        AddClassPlayer(request, kAlly, 0, allySpeed);
        TurnBattleEngine engine(provider);
        ASSERT_TRUE(engine.Initialize(request));
        ASSERT_EQ(engine.SetActorAuto(kCaster, true), kSuccess);
        const auto first = Round(engine);
        if (allySpeed == 200) {
            ExpectSkill(first, kHaste, kAlly);
            ExpectBasicAttack(Round(engine));
        } else {
            ExpectBasicAttack(first);
        }
    }
}

TEST_F(AutoClassSkillEngineTest, TieBreaksAreStableAcrossSkillOrderAndSelectionDoesNotConsumeCombatRandomness) {
    constexpr uint32_t kFast = 9132;
    constexpr uint32_t kSameFast = 9133;
    // 四招伤害相同,逐级以耗蓝、冷却、技能号决胜;两个目标同样按稳定身份决胜。
    for (const uint32_t skillId : {kSwift, kFast, kSameFast}) {
        auto& row = provider->AddSkill(skillId);
        row.CopyFrom(*provider->FindSkill(kStrike));
        row.set_id(skillId);
        row.set_mana_percent(0.05);
        row.set_cooldown_rounds(skillId == kSwift ? 1 : 0);
        row.set_initiative(0);
        provider->SetSkillDamage(skillId, 10.0);
    }
    const auto run = [&](bool automatic, bool reverseSkills) {
        auto request = Duel(9816);
        auto* caster = request.mutable_players(0);
        caster->set_max_mana(1000);
        caster->mutable_base_attributes()->set_mana(1000);
        caster->mutable_base_attributes()->set_critchance(50);
        std::vector<uint32_t> skills{kStrike, kSwift, kFast, kSameFast};
        if (reverseSkills) {
            std::reverse(skills.begin(), skills.end());
        }
        for (const uint32_t skillId : skills) {
            Learn(caster, skillId, 1);
        }
        auto* foe = request.mutable_players(1);
        foe->set_max_health(1000000);
        foe->mutable_base_attributes()->set_health(1000000);
        auto* foe2 = AddClassPlayer(request, kFoe2, 1, 100);
        foe2->set_max_health(1000000);
        foe2->mutable_base_attributes()->set_health(1000000);
        TurnBattleEngine engine(provider);
        EXPECT_TRUE(engine.Initialize(request));
        if (automatic) {
            EXPECT_EQ(engine.SetActorAuto(kCaster, true), kSuccess);
        }
        std::string events;
        for (int round = 0; round < 10; ++round) {
            if (!automatic) {
                const auto manual = Skill(kFast, kFoe);
                EXPECT_EQ(engine.ValidateAction(kCaster, manual), kSuccess);
                engine.SubmitAction(kCaster, manual);
            }
            const auto result = Round(engine);
            ExpectSkill(result, kFast, kFoe);
            for (const auto& event : result.events()) {
                events += event.SerializeAsString();
                events += '|';
            }
        }
        return events;
    };
    const auto manual = run(false, false);
    EXPECT_EQ(run(true, false), manual);
    EXPECT_EQ(run(true, true), manual);
}

}  // namespace
