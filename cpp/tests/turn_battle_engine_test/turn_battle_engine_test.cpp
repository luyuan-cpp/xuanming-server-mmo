#include <gtest/gtest.h>

#include <cmath>
#include <memory>
#include <random>
#include <stdexcept>
#include <string>
#include <vector>

#include "constants/turn_battle_constants.h"
#include "memory_battle_data_provider.h"
#include "system/combat_damage_rules.h"
#include "system/turn_battle_engine.h"
#include "../../libs/services/scene/battle/system/battle_settlement_application_cache.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/skill_error_tip.pb.h"

// 回合制战斗引擎单元测试(设计文档 §5.1)。
//
// 表管理器依赖用 MemoryBattleDataProvider 注入隔离,不依赖 Excel 数据与配置加载;
// 速度数字 2026-09-14 起统一 ×12(速度单位放大,见 turn_battle_constants.h 的 kFleeSpeedFactor):
// 相对大小与逃跑成功率与改前等价,出手序 / 逃跑结论不变。
// 覆盖:确定性事件流 / 速度序 / 超时默认行动 / 冷却回合 / buff 到期·叠层·周期·驱散·免疫 /
// 沉默许可 / 胜负边界(全灭·打满·平局)/ 结算数值 / FLEE·DEFEND·ITEM 路径 /
// 二期:自动战斗(SetActorAuto·就绪·快照排除·确定性回归)/ 队伍人数上限(设计文档 §11)。
// 装备战斗类属性(equipment-attributes.md §4.5,文件末尾):必杀 / 抗性按伤害类型分流、连击、反震、反击、
// 所有技能上升、抗异常、零概率不耗随机数、新掷骰的随机数消费顺序、下发前清洗。

namespace turnbattle {
// 只注入当前客户端无法施加的非玩家来源周期伤害，不改公开业务接口。
class TurnBattleEngineDeathTestAccess {
public:
    static bool AddBuff(TurnBattleEngine& engine, uint64_t targetId, uint32_t buffId,
                        uint64_t sourceId) {
        auto* target = engine.FindActor(targetId);
        if (target == nullptr) return false;
        TurnResultS2C result;
        engine.AddBuffToActor(*target, buffId, sourceId, 0, result);
        return true;
    }

    // 同 AddBuff,但把过程中产出的事件带回来:抗异常要看的是 RESIST / BUFF_ADD / BUFF_REMOVE 事件。
    // 目标不存在时返回空结果。
    static TurnResultS2C AddBuffWithEvents(TurnBattleEngine& engine, uint64_t targetId,
                                           uint32_t buffId, uint64_t sourceId) {
        TurnResultS2C result;
        if (auto* target = engine.FindActor(targetId); target != nullptr) {
            engine.AddBuffToActor(*target, buffId, sourceId, 0, result);
        }
        return result;
    }

    // 引擎 RNG 的下一个输出。在副本上取,不推进引擎自己的 RNG。
    // 同种子的两台引擎这个值相同 <=> 它们至今消耗的随机数个数相同 —— 「零概率不耗随机数」靠它直接断言。
    static uint64_t PeekNextRandom(const TurnBattleEngine& engine) {
        auto rngCopy = engine.rng;
        return rngCopy();
    }
};
} // namespace turnbattle

namespace {

using turnbattle::TurnBattleEngine;

constexpr uint64_t kPlayerA = 5001;
constexpr uint64_t kPlayerB = 5002;
constexpr uint64_t kPlayerC = 5003;
const uint64_t kMonsterId = turnbattle::kMonsterActorIdBase;

// 常用表 id
constexpr uint32_t kSkillDamage = 101;    // 普通伤害技能(冷却组 9)
constexpr uint32_t kSkillPoison = 102;    // 纯 buff 技能:挂毒
constexpr uint32_t kSkillNuke = 103;      // 一击必杀
constexpr uint32_t kSkillDispel = 104;    // 纯驱散技能
constexpr uint32_t kBuffPoison = 201;     // 毒:2 回合,每回合 10 点
constexpr uint32_t kBuffSilence = 210;    // 沉默
constexpr uint32_t kBuffDispel = 220;     // 纯驱散 buff
constexpr uint32_t kBuffImmunePoison = 230;  // 免疫毒 tag
constexpr uint32_t kBuffRegen = 240;      // 周期回血
constexpr uint32_t kItemPotion = 301;     // 战斗药品
constexpr uint32_t kCooldownGroup = 9;
constexpr uint32_t kDungeonConfig = 7;

// 装配一套标准测试表数据
std::shared_ptr<MemoryBattleDataProvider> MakeProvider() {
    auto provider = std::make_shared<MemoryBattleDataProvider>();

    // 伤害技能:单体目标(targeting_mode 存位号,1 = 指向性),普通施放类型
    auto& damageSkill = provider->AddSkill(kSkillDamage);
    damageSkill.add_targeting_mode(1);
    damageSkill.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    damageSkill.set_cooldown_id(kCooldownGroup);
    provider->SetCooldownMs(kCooldownGroup, 12000);  // 2 回合冷却
    provider->SetSkillDamage(kSkillDamage, 50.0);

    // 挂毒技能:无伤害纯 buff
    auto& poisonSkill = provider->AddSkill(kSkillPoison);
    poisonSkill.add_targeting_mode(1);
    poisonSkill.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    poisonSkill.add_effect(kBuffPoison);

    // 一击必杀
    auto& nukeSkill = provider->AddSkill(kSkillNuke);
    nukeSkill.add_targeting_mode(1);
    nukeSkill.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    provider->SetSkillDamage(kSkillNuke, 1000.0);

    // 纯驱散技能
    auto& dispelSkill = provider->AddSkill(kSkillDispel);
    dispelSkill.add_targeting_mode(1);
    dispelSkill.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    dispelSkill.add_effect(kBuffDispel);

    // 战斗药水:回血 100(原先写死在 kDefaultItemHealHp,2026-09-17 起读表)
    auto& potion = provider->AddItem(kItemPotion);
    potion.set_battle_usable(1);
    potion.set_battle_heal_hp(100);

    // 毒 buff:12 秒 → 2 回合,6 秒周期 → 每回合 tick,每层 10 点
    auto& poisonBuff = provider->AddBuff(kBuffPoison);
    poisonBuff.set_buff_type(turnbattle::kBuffTypePoison);
    poisonBuff.set_duration(12.0);
    poisonBuff.set_interval(6.0);
    poisonBuff.add_interval_effect(10.0);
    poisonBuff.set_max_layer(3);
    (*poisonBuff.mutable_tag())["poison_tag"] = true;

    // 沉默 buff
    auto& silenceBuff = provider->AddBuff(kBuffSilence);
    silenceBuff.set_buff_type(turnbattle::kBuffTypeSilence);
    silenceBuff.set_duration(12.0);

    // 纯驱散 buff:驱掉毒 tag
    auto& dispelBuff = provider->AddBuff(kBuffDispel);
    dispelBuff.set_buff_type(turnbattle::kBuffTypeDispel);
    (*dispelBuff.mutable_dispel_tag())["poison_tag"] = true;

    // 免疫毒 tag 的 buff
    auto& immuneBuff = provider->AddBuff(kBuffImmunePoison);
    immuneBuff.set_infinite_duration(1);
    (*immuneBuff.mutable_immune_tag())["poison_tag"] = true;

    // 周期回血 buff:12 秒 → 2 回合,每回合回 25
    auto& regenBuff = provider->AddBuff(kBuffRegen);
    regenBuff.set_buff_type(turnbattle::kBuffTypeHealthRegenerationBasedOnLostHealth);
    regenBuff.set_duration(12.0);
    regenBuff.set_interval(6.0);
    provider->SetBuffRegen(kBuffRegen, 25.0);

    // 沉默许可行:格值按技能类型位号平铺,kSuccess=放行,其余为错误码
    auto& permission = provider->AddSkillPermission(turnbattle::kCombatStateSilence);
    permission.add_skill_type(kSuccess);                              // 被动
    permission.add_skill_type(kSkillCannotBeCastSilenceRestriction);  // 普通施放:沉默禁用
    permission.add_skill_type(kSkillCannotBeCastSilenceRestriction);  // 吟唱
    permission.add_skill_type(kSuccess);                              // 开关
    permission.add_skill_type(kSuccess);                              // 激活
    permission.add_skill_type(kSuccess);                              // 普攻

    return provider;
}

CreateBattleRequest MakeRequest(uint64_t battleId, uint32_t matchMode, uint64_t seed) {
    CreateBattleRequest request;
    request.set_battle_id(battleId);
    request.set_battle_config_id(kDungeonConfig);
    request.set_match_mode(matchMode);
    request.set_seed(seed);
    return request;
}

// 兼容属性加点线的用例:省略 battle_id 的两参重载(固定 9900)
CreateBattleRequest MakeRequest(uint32_t matchMode, uint64_t seed) {
    return MakeRequest(9900, matchMode, seed);
}

BattlePlayerSnapshot* AddPlayer(CreateBattleRequest& request, uint64_t playerId, uint32_t teamIndex,
                                uint64_t health, uint64_t maxHealth, uint64_t strength,
                                uint64_t armor, uint64_t critChance, uint64_t speed) {
    auto* snapshot = request.add_players();
    snapshot->set_player_id(playerId);
    snapshot->set_player_name("测试玩家");
    snapshot->set_level(10);
    snapshot->set_team_index(teamIndex);
    snapshot->set_max_health(maxHealth);
    auto* attributes = snapshot->mutable_base_attributes();
    attributes->set_health(health);
    attributes->set_strength(strength);
    attributes->set_armor(armor);
    attributes->set_critchance(critChance);
    attributes->set_speed(speed);
    snapshot->add_skill_table_ids(kSkillDamage);
    snapshot->add_skill_table_ids(kSkillPoison);
    snapshot->add_skill_table_ids(kSkillNuke);
    snapshot->add_skill_table_ids(kSkillDispel);
    return snapshot;
}

BattleAction MakeAction(eBattleActionType actionType, uint64_t targetId = 0,
                        uint32_t skillTableId = 0, uint32_t itemTableId = 0) {
    BattleAction action;
    action.set_action_type(actionType);
    action.set_target_id(targetId);
    action.set_skill_table_id(skillTableId);
    action.set_item_table_id(itemTableId);
    return action;
}

int CountEvents(const TurnResultS2C& result, eBattleEventType eventType) {
    int count = 0;
    for (const auto& event : result.events()) {
        if (event.event_type() == eventType) {
            ++count;
        }
    }
    return count;
}

const BattleEventItem* FindFirstEvent(const TurnResultS2C& result, eBattleEventType eventType) {
    for (const auto& event : result.events()) {
        if (event.event_type() == eventType) {
            return &event;
        }
    }
    return nullptr;
}

const BattleActorState* FindStateActor(const BattleStateS2C& state, uint64_t actorId) {
    for (const auto& actor : state.actors()) {
        if (actor.actor_id() == actorId) {
            return &actor;
        }
    }
    return nullptr;
}

}  // namespace

// ---------------------------------------------------------------------------
// 确定性:同种子同指令 → 逐字节相同事件流
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, SameSeedSameCommandsProduceIdenticalEventStream) {
    // 高暴击率逼引擎大量消耗 RNG,任何随机路径分叉都会导致字节流不一致
    const auto runBattle = [](uint64_t seed) {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9001, turnbattle::kMatchModePveSolo, seed);
        AddPlayer(request, kPlayerA, 0, 1000, 1000, 4, 100, 50, 120);
        EXPECT_TRUE(engine.Initialize(request));

        std::string stream;
        for (int round = 0; round < 10 && engine.Outcome() == BATTLE_OUTCOME_ONGOING; ++round) {
            engine.SubmitAction(kPlayerA,
                                MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillDamage));
            const auto result = engine.ResolveCurrentRound();
            for (const auto& event : result.events()) {
                stream += event.SerializeAsString();
                stream += '|';
            }
        }
        stream += engine.BuildSettlement(kPlayerA).SerializeAsString();
        return stream;
    };

    EXPECT_EQ(runBattle(42), runBattle(42));
    EXPECT_EQ(runBattle(20260815), runBattle(20260815));
}

TEST(TurnBattleEngineTest, AppearanceIdentitySurvivesBattleStateSnapshots) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9010, turnbattle::kMatchModePveSolo, 42);
    auto* player = AddPlayer(request, kPlayerA, 0, 1000, 1000, 4, 100, 50, 120);
    player->set_appearance_id("04_mountain_guardian_boy");
    player->set_class_id(3);
    player->set_gender(1);
    ASSERT_TRUE(engine.Initialize(request));
    // 入场、断线恢复与观战共用快照，不能从职业或局内 actor_id 猜外观。
    const auto state = engine.BuildStateSnapshot();
    const auto* actor = FindStateActor(state, kPlayerA);
    ASSERT_NE(actor, nullptr);
    EXPECT_EQ(actor->appearance_id(), "04_mountain_guardian_boy");
    EXPECT_EQ(actor->class_id(), 3);
    EXPECT_EQ(actor->gender(), 1);
}

// ---------------------------------------------------------------------------
// 速度序:速度降序结算,同速按 actor_id 升序
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, TurnOrderIsSpeedDescending) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9002, 3 /* PVP 1v1 */, 1);
    AddPlayer(request, kPlayerA, 0, 500, 500, 0, 100, 0, 120);
    AddPlayer(request, kPlayerB, 1, 500, 500, 0, 100, 0, 240);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));
    ASSERT_TRUE(engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_ATTACK, kPlayerA)));

    const auto result = engine.ResolveCurrentRound();
    ASSERT_GE(result.events_size(), 1);
    EXPECT_EQ(result.events(0).event_type(), BATTLE_EVENT_ATTACK);
    EXPECT_EQ(result.events(0).source_id(), kPlayerB);  // 速度 240 先手
}

TEST(TurnBattleEngineTest, TurnOrderTieBreaksByActorIdAscending) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9003, 3, 1);
    AddPlayer(request, kPlayerA, 0, 500, 500, 0, 100, 0, 120);
    AddPlayer(request, kPlayerB, 1, 500, 500, 0, 100, 0, 120);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));
    engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_ATTACK, kPlayerA));

    const auto result = engine.ResolveCurrentRound();
    ASSERT_GE(result.events_size(), 1);
    EXPECT_EQ(result.events(0).source_id(), kPlayerA);  // 同速,小 id 先手
}

// ---------------------------------------------------------------------------
// 超时默认行动:未提交者按默认普攻结算
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, TimeoutFillsDefaultBasicAttack) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9004, turnbattle::kMatchModePveSolo, 1);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 4, 100, 0, 120);
    ASSERT_TRUE(engine.Initialize(request));

    // 玩家不提交任何行动,直接结算(等价回合超时)
    const auto result = engine.ResolveCurrentRound();

    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_ATTACK), 2);  // 玩家默认普攻 + 怪物普攻
    ASSERT_GE(result.events_size(), 1);
    // 玩家速度 120 > 怪物默认速度 60,先手是玩家的默认普攻
    EXPECT_EQ(result.events(0).event_type(), BATTLE_EVENT_ATTACK);
    EXPECT_EQ(result.events(0).source_id(), kPlayerA);
    EXPECT_EQ(result.events(0).target_id(), kMonsterId);
}

// ---------------------------------------------------------------------------
// 冷却:12000ms → 2 回合,冷却中提交不落账
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, CooldownConvertsToRoundsAndBlocksResubmission) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9005, turnbattle::kMatchModePveSolo, 1);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 120);
    ASSERT_TRUE(engine.Initialize(request));

    // 第 1 回合:技能可用
    EXPECT_TRUE(engine.SubmitAction(kPlayerA,
                                    MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillDamage)));
    auto result = engine.ResolveCurrentRound();
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_SKILL), 1);

    // 第 2 回合:冷却中,提交不落账(就绪保持 false),改普攻可就绪
    EXPECT_FALSE(engine.SubmitAction(kPlayerA,
                                     MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillDamage)));
    EXPECT_TRUE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId)));
    result = engine.ResolveCurrentRound();
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_SKILL), 0);

    // 第 3 回合:冷却结束,技能恢复可用
    EXPECT_TRUE(engine.SubmitAction(kPlayerA,
                                    MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillDamage)));
    result = engine.ResolveCurrentRound();
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_SKILL), 1);
}

// ---------------------------------------------------------------------------
// buff:周期 tick 与回合到期
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, PoisonBuffTicksEachRoundAndExpiresAfterTwoRounds) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9006, turnbattle::kMatchModePveSolo, 1);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 120);
    ASSERT_TRUE(engine.Initialize(request));

    // 第 1 回合:挂毒 → BUFF_ADD;回合末第一跳(每层 10 点)
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillPoison));
    auto result = engine.ResolveCurrentRound();
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_ADD), 1);
    const auto* tickEvent = FindFirstEvent(result, BATTLE_EVENT_BUFF_TICK);
    ASSERT_NE(tickEvent, nullptr);
    EXPECT_EQ(tickEvent->buff_table_id(), kBuffPoison);
    EXPECT_EQ(tickEvent->value(), 10u);
    EXPECT_EQ(tickEvent->target_id(), kMonsterId);

    // 第 2 回合:第二跳后到期移除
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_DEFEND));
    result = engine.ResolveCurrentRound();
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_TICK), 1);
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_REMOVE), 1);

    // 第 3 回合:毒已不在
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_DEFEND));
    result = engine.ResolveCurrentRound();
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_TICK), 0);
    const auto* monster = FindStateActor(result.state(), kMonsterId);
    ASSERT_NE(monster, nullptr);
    EXPECT_EQ(monster->buffs_size(), 0);
}

TEST(TurnBattleEngineTest, BuffStacksUpToMaxLayer) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9007, turnbattle::kMatchModePveSolo, 1);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 120);
    ASSERT_TRUE(engine.Initialize(request));

    // 连续 4 回合重复挂毒:层数 1→2→3→3(max_layer=3 封顶)
    const uint32_t expectedLayers[] = {1, 2, 3, 3};
    for (const auto expectedLayer : expectedLayers) {
        engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillPoison));
        const auto result = engine.ResolveCurrentRound();
        const auto* addEvent = FindFirstEvent(result, BATTLE_EVENT_BUFF_ADD);
        ASSERT_NE(addEvent, nullptr);
        EXPECT_EQ(addEvent->value(), expectedLayer);  // value 携带当前叠层数

        const auto* monster = FindStateActor(result.state(), kMonsterId);
        ASSERT_NE(monster, nullptr);
        ASSERT_EQ(monster->buffs_size(), 1);
        EXPECT_EQ(monster->buffs(0).layer(), expectedLayer);
    }
}

TEST(TurnBattleEngineTest, DispelSkillRemovesPoisonWithoutLeavingEntry) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9008, turnbattle::kMatchModePveSolo, 1);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 120);
    ASSERT_TRUE(engine.Initialize(request));

    // 第 1 回合挂毒
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillPoison));
    engine.ResolveCurrentRound();

    // 第 2 回合驱散:毒被移除,纯驱散 buff 自身不落地,回合末无毒 tick
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillDispel));
    const auto result = engine.ResolveCurrentRound();
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_REMOVE), 1);
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_ADD), 0);
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_TICK), 0);
    const auto* monster = FindStateActor(result.state(), kMonsterId);
    ASSERT_NE(monster, nullptr);
    EXPECT_EQ(monster->buffs_size(), 0);
}

TEST(TurnBattleEngineTest, ImmuneTagBlocksPoison) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9009, 3 /* PVP */, 1);
    AddPlayer(request, kPlayerA, 0, 500, 500, 0, 100, 0, 120);
    auto* defender = AddPlayer(request, kPlayerB, 1, 500, 500, 0, 100, 0, 60);
    // B 携带免疫毒 tag 的参战 buff(无限持续,remain_rounds=0 哨兵)
    auto* immuneEntry = defender->add_buffs();
    immuneEntry->set_buff_id(900);
    immuneEntry->set_buff_table_id(kBuffImmunePoison);
    immuneEntry->set_layer(1);
    immuneEntry->set_remain_rounds(0);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kPlayerB, kSkillPoison));
    engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_DEFEND));
    const auto result = engine.ResolveCurrentRound();

    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_ADD), 0);  // 免疫挡下
    const auto* defenderState = FindStateActor(result.state(), kPlayerB);
    ASSERT_NE(defenderState, nullptr);
    ASSERT_EQ(defenderState->buffs_size(), 1);  // 只剩免疫 buff 本体
    EXPECT_EQ(defenderState->buffs(0).buff_table_id(), kBuffImmunePoison);
}

TEST(TurnBattleEngineTest, SnapshotRegenBuffHealsAtRoundEnd) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9010, turnbattle::kMatchModePveSolo, 1);
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 100, 200, 0, 100, 0, 120);
    // 参战携带周期回血 buff(remain_rounds 已是回合口径:2 回合)
    auto* regenEntry = snapshot->add_buffs();
    regenEntry->set_buff_id(901);
    regenEntry->set_buff_table_id(kBuffRegen);
    regenEntry->set_layer(1);
    regenEntry->set_remain_rounds(2);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_DEFEND));
    const auto result = engine.ResolveCurrentRound();

    const auto* tickEvent = FindFirstEvent(result, BATTLE_EVENT_BUFF_TICK);
    ASSERT_NE(tickEvent, nullptr);
    EXPECT_EQ(tickEvent->buff_table_id(), kBuffRegen);
    EXPECT_EQ(tickEvent->value(), 25u);
    const auto* playerState = FindStateActor(result.state(), kPlayerA);
    ASSERT_NE(playerState, nullptr);
    // 比例减伤下怪物普攻至少打出四成,玩家本回合也会挨一下(防御中再减半):终值 = 100 − 挨打 + 回血 25
    uint64_t damageTaken = 0;
    for (const auto& event : result.events()) {
        if (event.event_type() == BATTLE_EVENT_DAMAGE && event.target_id() == kPlayerA) {
            damageTaken += event.value();
        }
    }
    EXPECT_EQ(playerState->attributes().health(), 100u - damageTaken + 25u);
}

// ---------------------------------------------------------------------------
// 沉默:SkillPermission 按技能类型格值放行/拦截
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, SilenceBlocksGeneralSkillButAllowsBasicAttack) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9011, turnbattle::kMatchModePveSolo, 1);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 120);
    ASSERT_TRUE(engine.Initialize(request));
    // 沉默必须在局内挂上:决策 D49 规定战前快照里的控制类 buff(眩晕/冰冻/沉默)一律剔除
    // (见 SnapshotBuffsDropControlInstantUnknownAndSanitizeCaster),从快照带入的沉默不会生效。
    ASSERT_TRUE(turnbattle::TurnBattleEngineDeathTestAccess::AddBuff(
        engine, kPlayerA, kBuffSilence, kMonsterId));

    // 沉默中:普通施放类技能被许可表拦下,不落账
    EXPECT_FALSE(engine.SubmitAction(kPlayerA,
                                     MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillDamage)));
    // 普攻不走技能许可,可就绪
    EXPECT_TRUE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId)));
}

// ---------------------------------------------------------------------------
// 胜负边界
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, KillingAllMonstersWinsSideA) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9012, turnbattle::kMatchModePveSolo, 1);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 120);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillNuke));
    const auto result = engine.ResolveCurrentRound();

    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_DEATH), 1);
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);

    const auto settlement = engine.BuildSettlement(kPlayerA);
    EXPECT_EQ(settlement.outcome(), BATTLE_OUTCOME_SIDE_A_WIN);
    EXPECT_EQ(settlement.total_rounds(), 1u);
    EXPECT_FALSE(settlement.is_dead());
    EXPECT_EQ(settlement.health(), 1000u);  // 怪物先被秒,未能出手
}

TEST(TurnBattleEngineTest, MaxRoundsExhaustionDefeatsAttacker) {
    auto provider = MakeProvider();
    // DungeonTable.time_limit=12 秒 → 上限 2 回合
    provider->AddDungeon(kDungeonConfig).set_time_limit(12);

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9013, turnbattle::kMatchModePveSolo, 1);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 120);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_DEFEND));
    engine.ResolveCurrentRound();
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_ONGOING);

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_DEFEND));
    engine.ResolveCurrentRound();
    // 打满 2 回合:进攻方(A 方)判负
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_B_WIN);
    EXPECT_EQ(engine.BuildSettlement(kPlayerA).total_rounds(), 2u);
}

TEST(TurnBattleEngineTest, SimultaneousPoisonDeathIsDrawAndDefendHalvesTick) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9014, 3 /* PVP */, 1);
    AddPlayer(request, kPlayerA, 0, 15, 15, 0, 100, 0, 120);
    AddPlayer(request, kPlayerB, 1, 15, 15, 0, 100, 0, 60);
    ASSERT_TRUE(engine.Initialize(request));

    // 第 1 回合:互相挂毒;回合末各掉 10 → 双方剩 5
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kPlayerB, kSkillPoison));
    engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_SKILL, kPlayerA, kSkillPoison));
    auto result = engine.ResolveCurrentRound();
    const auto* firstTick = FindFirstEvent(result, BATTLE_EVENT_BUFF_TICK);
    ASSERT_NE(firstTick, nullptr);
    EXPECT_EQ(firstTick->value(), 10u);
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_ONGOING);

    // 第 2 回合:双方防御;毒 tick 减半(10→5)仍致死 → 同回合双死判平
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_DEFEND));
    engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_DEFEND));
    result = engine.ResolveCurrentRound();
    const auto* halvedTick = FindFirstEvent(result, BATTLE_EVENT_BUFF_TICK);
    ASSERT_NE(halvedTick, nullptr);
    EXPECT_EQ(halvedTick->value(), 5u);  // DEFEND 覆盖回合末周期伤害
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_DEATH), 2);
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_DRAW);
}

// ---------------------------------------------------------------------------
// 结算数值:伤害公式与终值
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, DamageFormulaMatchesRealtimeSemantics) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9015, turnbattle::kMatchModePveSolo, 1);
    // strength=4,无暴击;怪物默认 armor=24、resistance=0,怪物等级取参战玩家最高等级 10
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 4, 100, 0, 120);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillDamage));
    const auto result = engine.ResolveCurrentRound();

    // 原始 50 * (1 + 4*0.1) = 70;等级系数 360 + 120*10 = 1560;受伤比例 1560 / (24 + 1560) ≈ 0.9848(防御单位 ×12,与改前 130/132 逐位相同)
    // 70 * 0.9848 ≈ 68.94 → 向上取整 69
    const auto* damageEvent = FindFirstEvent(result, BATTLE_EVENT_DAMAGE);
    ASSERT_NE(damageEvent, nullptr);
    EXPECT_EQ(damageEvent->value(), 69u);
    EXPECT_FALSE(damageEvent->is_critical());
    EXPECT_EQ(damageEvent->target_health_after(),
              turnbattle::kMonsterDefaultHealth - 69);

    const auto* monster = FindStateActor(result.state(), kMonsterId);
    ASSERT_NE(monster, nullptr);
    EXPECT_EQ(monster->attributes().health(), turnbattle::kMonsterDefaultHealth - 69);
}

// ---------------------------------------------------------------------------
// FLEE / ITEM 路径
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, FleeIsDeterministicAndConsistentWithOutcome) {
    const auto runFlee = [](uint64_t seed) {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9016, turnbattle::kMatchModePveSolo, seed);
        AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 600);  // 高速,逃跑成功率高
        EXPECT_TRUE(engine.Initialize(request));

        engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_FLEE));
        const auto result = engine.ResolveCurrentRound();

        const auto* fleeEvent = FindFirstEvent(result, BATTLE_EVENT_FLEE);
        EXPECT_NE(fleeEvent, nullptr);
        if (fleeEvent == nullptr) {
            return std::string();
        }
        // 逃跑结果与单位状态、胜负一致:成功 → 单位离场,A 方无存活 → B 胜
        const auto* playerState = FindStateActor(result.state(), kPlayerA);
        EXPECT_NE(playerState, nullptr);
        if (playerState != nullptr) {
            EXPECT_EQ(playerState->fled(), fleeEvent->success());
        }
        if (fleeEvent->success()) {
            EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_B_WIN);
            EXPECT_TRUE(engine.BuildSettlement(kPlayerA).fled());
        } else {
            EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_ONGOING);
        }
        return fleeEvent->SerializeAsString();
    };

    // 同种子两次运行结果逐字节一致
    EXPECT_EQ(runFlee(7), runFlee(7));
    EXPECT_EQ(runFlee(1234), runFlee(1234));
}

TEST(TurnBattleEngineTest, FleeIsRejectedInPvp) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9017, 3 /* PVP 一期不可逃 */, 1);
    AddPlayer(request, kPlayerA, 0, 500, 500, 0, 100, 0, 120);
    AddPlayer(request, kPlayerB, 1, 500, 500, 0, 100, 0, 60);
    ASSERT_TRUE(engine.Initialize(request));

    // 逃跑提交不落账;超时结算按默认普攻
    EXPECT_FALSE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_FLEE)));
    engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_DEFEND));
    const auto result = engine.ResolveCurrentRound();

    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_FLEE), 0);
    const auto* attackEvent = FindFirstEvent(result, BATTLE_EVENT_ATTACK);
    ASSERT_NE(attackEvent, nullptr);
    EXPECT_EQ(attackEvent->source_id(), kPlayerA);  // 默认普攻兜底
}

TEST(TurnBattleEngineTest, ItemHealsAndConsumptionGoesIntoSettlement) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9018, turnbattle::kMatchModePveSolo, 1);
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 50, 200, 0, 100, 0, 120);
    auto* item = snapshot->add_items();
    item->set_item_table_id(kItemPotion);
    item->set_count(2);
    ASSERT_TRUE(engine.Initialize(request));

    // 第 1 次使用:50 → 150,回 100
    EXPECT_TRUE(engine.SubmitAction(kPlayerA,
                                    MakeAction(BATTLE_ACTION_ITEM, kPlayerA, 0, kItemPotion)));
    auto result = engine.ResolveCurrentRound();
    const auto* itemEvent = FindFirstEvent(result, BATTLE_EVENT_ITEM);
    ASSERT_NE(itemEvent, nullptr);
    EXPECT_EQ(itemEvent->item_table_id(), kItemPotion);
    EXPECT_EQ(itemEvent->value(), 100u);
    EXPECT_EQ(itemEvent->target_health_after(), 150u);

    // 第 2 次使用:回满封顶 200。比例减伤下第 1 回合怪物普攻至少打出四成,回合末血量低于 150,
    // 所以这次实际回复量 = 200 − 回合初血量(仍小于药量 100,验证的是封顶)
    const auto* afterFirstRound = FindStateActor(result.state(), kPlayerA);
    ASSERT_NE(afterFirstRound, nullptr);
    const uint64_t healthBeforeSecond = afterFirstRound->attributes().health();
    ASSERT_GT(healthBeforeSecond, 100u);
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ITEM, kPlayerA, 0, kItemPotion));
    result = engine.ResolveCurrentRound();
    const auto* secondItemEvent = FindFirstEvent(result, BATTLE_EVENT_ITEM);
    ASSERT_NE(secondItemEvent, nullptr);
    EXPECT_EQ(secondItemEvent->value(), 200u - healthBeforeSecond);
    EXPECT_EQ(secondItemEvent->target_health_after(), 200u);

    // 副本已用尽:第 3 次提交不落账
    EXPECT_FALSE(engine.SubmitAction(kPlayerA,
                                     MakeAction(BATTLE_ACTION_ITEM, kPlayerA, 0, kItemPotion)));

    // 结算账本:消耗 2 个,交由 scene 按实际持有校验扣除
    const auto settlement = engine.BuildSettlement(kPlayerA);
    ASSERT_EQ(settlement.items_consumed_size(), 1);
    EXPECT_EQ(settlement.items_consumed(0).item_table_id(), kItemPotion);
    EXPECT_EQ(settlement.items_consumed(0).count(), 2u);
}

// ---------------------------------------------------------------------------
// 状态快照:待行动名单与回合推进
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, StateSnapshotTracksPendingActorsAndRoundIndex) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9019, turnbattle::kMatchModePveSolo, 1);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 120);
    ASSERT_TRUE(engine.Initialize(request));

    auto state = engine.BuildStateSnapshot();
    EXPECT_EQ(state.round_index(), 1u);
    ASSERT_EQ(state.pending_actor_ids_size(), 1);
    EXPECT_EQ(state.pending_actor_ids(0), kPlayerA);

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_DEFEND));
    state = engine.BuildStateSnapshot();
    EXPECT_EQ(state.pending_actor_ids_size(), 0);

    engine.ResolveCurrentRound();
    state = engine.BuildStateSnapshot();
    EXPECT_EQ(state.round_index(), 2u);
    ASSERT_EQ(state.pending_actor_ids_size(), 1);  // 新回合重新待行动
}

// ---------------------------------------------------------------------------
// 二期:自动战斗(设计文档 §11 D12)
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, AutoActorCountsAsReadyAndActsWithDefaultAttack) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9020, turnbattle::kMatchModePveSolo, 1);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 4, 100, 0, 120);
    ASSERT_TRUE(engine.Initialize(request));

    // 未提交且未挂机:不就绪;开挂机后无需提交即就绪(提前结算的判据)
    EXPECT_FALSE(engine.AllPlayersReady());
    ASSERT_EQ(engine.SetActorAuto(kPlayerA, true), 0u);
    EXPECT_TRUE(engine.AllPlayersReady());

    // 结算走默认普攻代打:玩家速度 120 > 怪物默认 60,首事件是玩家对怪的普攻
    const auto result = engine.ResolveCurrentRound();
    ASSERT_GE(result.events_size(), 1);
    EXPECT_EQ(result.events(0).event_type(), BATTLE_EVENT_ATTACK);
    EXPECT_EQ(result.events(0).source_id(), kPlayerA);
    EXPECT_EQ(result.events(0).target_id(), kMonsterId);

    // 关闭挂机:回到待提交状态
    ASSERT_EQ(engine.SetActorAuto(kPlayerA, false), 0u);
    EXPECT_FALSE(engine.AllPlayersReady());
}

TEST(TurnBattleEngineTest, SetActorAutoRejectsMonsterDeadAndFledActors) {
    // 怪物 / 不存在的单位:参数错误
    {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9021, turnbattle::kMatchModePveSolo, 1);
        AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 120);
        ASSERT_TRUE(engine.Initialize(request));
        EXPECT_EQ(engine.SetActorAuto(kMonsterId, true), static_cast<uint32_t>(kInvalidParameter));
        EXPECT_EQ(engine.SetActorAuto(999999u, true), static_cast<uint32_t>(kInvalidParameter));
    }

    // 死亡玩家:1v2 秒掉 B 后战斗仍进行中,B 被拒,存活的 C 可开
    {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9022, 3 /* PVP */, 1);
        AddPlayer(request, kPlayerA, 0, 500, 500, 0, 100, 0, 120);
        AddPlayer(request, kPlayerB, 1, 250, 250, 0, 0, 0, 60);  // 无甲、250 血:PVP 下 nuke 1000 × kPvpDamageScale(0.3) = 300,仍一击可杀
        AddPlayer(request, kPlayerC, 1, 500, 500, 0, 100, 0, 60);
        ASSERT_TRUE(engine.Initialize(request));

        engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kPlayerB, kSkillNuke));
        engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_DEFEND));
        engine.SubmitAction(kPlayerC, MakeAction(BATTLE_ACTION_DEFEND));
        engine.ResolveCurrentRound();

        ASSERT_EQ(engine.Outcome(), BATTLE_OUTCOME_ONGOING);
        EXPECT_EQ(engine.SetActorAuto(kPlayerB, true),
                  static_cast<uint32_t>(kThisEntityIsInvalid));
        EXPECT_EQ(engine.SetActorAuto(kPlayerC, true), 0u);
    }

    // 已逃玩家:逃跑成功率随种子,扫种子找一局"A 首回合逃跑成功、B 仍在场"
    // (成功率封顶 0.95,32 枚种子内必现;Rand01 跨平台同种子同序列,结果确定可复现)
    {
        bool verified = false;
        for (uint64_t seed = 1; seed <= 32 && !verified; ++seed) {
            TurnBattleEngine engine(MakeProvider());
            auto request = MakeRequest(9023, turnbattle::kMatchModePveTeam, seed);
            AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 600);  // 高速,逃跑成功率高
            AddPlayer(request, kPlayerB, 0, 1000, 1000, 0, 100, 0, 120);
            ASSERT_TRUE(engine.Initialize(request));

            engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_FLEE));
            engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_DEFEND));
            const auto result = engine.ResolveCurrentRound();
            const auto* fleeEvent = FindFirstEvent(result, BATTLE_EVENT_FLEE);
            ASSERT_NE(fleeEvent, nullptr);
            if (!fleeEvent->success()) {
                continue;
            }

            ASSERT_EQ(engine.Outcome(), BATTLE_OUTCOME_ONGOING);  // B 还在,战斗未结束
            EXPECT_EQ(engine.SetActorAuto(kPlayerA, true),
                      static_cast<uint32_t>(kThisEntityIsInvalid));
            verified = true;
        }
        EXPECT_TRUE(verified);
    }
}

TEST(TurnBattleEngineTest, AutoModeMatchesManualDefaultAttackEventStream) {
    // 确定性回归:开 auto(引擎代打默认普攻)与手动提交等价指令
    // (ATTACK + target 0,即默认行动本体)必须产出逐字节相同的事件流与结算
    const auto runBattle = [](uint64_t seed, bool useAuto) {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9024, turnbattle::kMatchModePveSolo, seed);
        // 高暴击逼引擎大量消耗 RNG,任何随机路径分叉都会导致字节流不一致
        AddPlayer(request, kPlayerA, 0, 1000, 1000, 4, 100, 50, 120);
        EXPECT_TRUE(engine.Initialize(request));
        if (useAuto) {
            EXPECT_EQ(engine.SetActorAuto(kPlayerA, true), 0u);
        }

        std::string stream;
        for (int round = 0; round < 10 && engine.Outcome() == BATTLE_OUTCOME_ONGOING; ++round) {
            if (!useAuto) {
                engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, 0));
            }
            const auto result = engine.ResolveCurrentRound();
            for (const auto& event : result.events()) {
                stream += event.SerializeAsString();
                stream += '|';
            }
        }
        stream += engine.BuildSettlement(kPlayerA).SerializeAsString();
        return stream;
    };

    EXPECT_EQ(runBattle(42, true), runBattle(42, false));
    EXPECT_EQ(runBattle(20260831, true), runBattle(20260831, false));
}

TEST(TurnBattleEngineTest, SnapshotMarksAutoActorAndExcludesFromPending) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9025, 3 /* PVP */, 1);
    AddPlayer(request, kPlayerA, 0, 500, 500, 0, 100, 0, 120);
    AddPlayer(request, kPlayerB, 1, 500, 500, 0, 100, 0, 60);
    ASSERT_TRUE(engine.Initialize(request));

    ASSERT_EQ(engine.BuildStateSnapshot().pending_actor_ids_size(), 2);

    // A 开挂机:快照携带 is_auto,待行动名单只剩手动的 B
    ASSERT_EQ(engine.SetActorAuto(kPlayerA, true), 0u);
    auto state = engine.BuildStateSnapshot();
    const auto* autoActor = FindStateActor(state, kPlayerA);
    ASSERT_NE(autoActor, nullptr);
    EXPECT_TRUE(autoActor->is_auto());
    const auto* manualActor = FindStateActor(state, kPlayerB);
    ASSERT_NE(manualActor, nullptr);
    EXPECT_FALSE(manualActor->is_auto());
    ASSERT_EQ(state.pending_actor_ids_size(), 1);
    EXPECT_EQ(state.pending_actor_ids(0), kPlayerB);

    // 关闭挂机:重回待行动名单
    ASSERT_EQ(engine.SetActorAuto(kPlayerA, false), 0u);
    state = engine.BuildStateSnapshot();
    EXPECT_EQ(state.pending_actor_ids_size(), 2);
}

// ---------------------------------------------------------------------------
// 二期:队伍人数上限(设计文档 §11 D14)
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, InitializeEnforcesTeamSizeLimit) {
    // 单队 6 人:超上限拒绝建房
    {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9026, turnbattle::kMatchModePveTeam, 1);
        for (uint64_t offset = 0; offset < turnbattle::kMaxBattleTeamSize + 1; ++offset) {
            AddPlayer(request, kPlayerA + offset, 0, 1000, 1000, 0, 100, 0, 120);
        }
        EXPECT_FALSE(engine.Initialize(request));
    }

    // 单队 5 人:恰在上限,放行
    {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9027, turnbattle::kMatchModePveTeam, 1);
        for (uint64_t offset = 0; offset < turnbattle::kMaxBattleTeamSize; ++offset) {
            AddPlayer(request, kPlayerA + offset, 0, 1000, 1000, 0, 100, 0, 120);
        }
        EXPECT_TRUE(engine.Initialize(request));
    }
}

// 2026-09-02:怪物属性从 MonsterTable 读入(不再写死 300 常量)→ PVE 多回合;
// 玩家击杀怪物后结算累加 MonsterTable.exp_reward/gold_reward。
TEST(TurnBattleEngineTest, MonsterAttributesFromTableEnableMultiRoundAndRewards) {
    auto provider = MakeProvider();
    // 表配怪物:200 HP、护甲 60(防御单位 ×12,原 5)、速度 96(慢于玩家),奖励经验 50 金币 25。
    constexpr uint32_t kMonsterTableId = 7000;
    auto& monster = provider->AddMonster(kMonsterTableId);
    monster.set_health(200);
    monster.set_strength(10);
    monster.set_armor(60);
    monster.set_speed(96);
    monster.set_exp_reward(50);
    monster.set_gold_reward(25);
    provider->SetDungeonMonsters(kDungeonConfig, {kMonsterTableId});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9030, turnbattle::kMatchModePveSolo, 7);
    // 玩家:1000 HP、力量 20(普攻 30,怪物护甲 60 → 30 * 1560 / 1620 ≈ 29)、速度 240(先手)。
    AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/20, /*armor*/10, /*crit*/0, /*speed*/240);
    ASSERT_TRUE(engine.Initialize(request));

    // 怪物从表读到 200 HP(不是常量 300),证明读表生效
    const auto initState = engine.BuildStateSnapshot();
    const auto* monsterActor = FindStateActor(initState, kMonsterId);
    ASSERT_NE(monsterActor, nullptr);
    EXPECT_EQ(monsterActor->attributes().health(), 200u);
    EXPECT_EQ(monsterActor->max_health(), 200u);

    // 玩家逐回合普攻,战斗应持续多回合(200/29≈7 回合),不再一击秒杀
    int rounds = 0;
    while (engine.Outcome() == BATTLE_OUTCOME_ONGOING && rounds < 30) {
        engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId));
        engine.ResolveCurrentRound();
        ++rounds;
    }
    EXPECT_GE(rounds, 3);  // 多回合(不是 1 回合秒杀)
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);  // 玩家胜

    // 结算:击杀怪物累加经验/金币
    const auto settlement = engine.BuildSettlement(kPlayerA);
    EXPECT_EQ(settlement.exp_gain(), 50u);
    EXPECT_EQ(settlement.gold_gain(), 25u);

    // 败方(此局无败玩家)/逃跑玩家不发奖的语义:阵亡玩家 exp_gain 应为 0
    // (此处玩家胜,单独验证"未击杀/失败不发奖"由 outcome 分支保证,不再造局)
}

// 玩家阵亡(怪物过强)时不发奖励:outcome != SIDE_A_WIN 分支
TEST(TurnBattleEngineTest, PlayerDefeatYieldsNoReward) {
    auto provider = MakeProvider();
    constexpr uint32_t kBossTableId = 7001;
    auto& boss = provider->AddMonster(kBossTableId);
    boss.set_health(100000);     // 打不死
    boss.set_strength(1000);     // 秒玩家
    boss.set_speed(1200);         // 先手
    boss.set_exp_reward(9999);
    boss.set_gold_reward(9999);
    provider->SetDungeonMonsters(kDungeonConfig, {kBossTableId});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9031, turnbattle::kMatchModePveSolo, 7);
    AddPlayer(request, kPlayerA, 0, 50, 50, 5, 0, 0, 12);  // 脆弱、后手
    ASSERT_TRUE(engine.Initialize(request));
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId));
    engine.ResolveCurrentRound();
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_B_WIN);  // 玩家败
    const auto settlement = engine.BuildSettlement(kPlayerA);
    EXPECT_EQ(settlement.exp_gain(), 0u);   // 败方无奖励
    EXPECT_EQ(settlement.gold_gain(), 0u);
}

// ---------------------------------------------------------------------------
// 属性加点二级属性:物伤/法伤/防御加法接入伤害公式(怪物/老存档为 0 时与老公式逐字节一致)
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, DerivedPhysicalAttackIsAdditiveOnBasicAttack) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9201, turnbattle::kMatchModePveSolo, 1);
    // 基线同 DamageFormulaMatchesRealtimeSemantics(strength=4、怪物默认 armor=24、resistance=0、等级系数 1560),
    // 再叠属性加点二级属性 physical_attack=50:
    //   普攻 10 * (1 + 4*0.1) + 50 = 64;64 * 1560 / (24 + 1560) ≈ 63.03 → 向上取整 64
    // speed=600 > 怪物默认 60,玩家先手,首个伤害事件即玩家普攻。
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/4, /*armor*/0, /*crit*/0, /*speed*/600);
    snapshot->set_physical_attack(50);
    snapshot->set_magic_attack(999);  // 普攻只吃物伤,法伤不得混入
    ASSERT_TRUE(engine.Initialize(request));

    ASSERT_TRUE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId)));
    const auto result = engine.ResolveCurrentRound();

    const auto* damageEvent = FindFirstEvent(result, BATTLE_EVENT_DAMAGE);
    ASSERT_NE(damageEvent, nullptr);
    EXPECT_EQ(damageEvent->source_id(), kPlayerA);
    EXPECT_EQ(damageEvent->value(), 64u);
}

TEST(TurnBattleEngineTest, DerivedMagicAttackIsAdditiveOnSkill) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9202, turnbattle::kMatchModePveSolo, 1);
    // 同 DamageFormulaMatchesRealtimeSemantics 的技能基线(受伤比例 1560 / 1584),叠 magic_attack=20:
    //   50 * (1 + 4*0.1) + 20 = 90;90 * 1560 / 1584 ≈ 88.64 → 向上取整 89
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/4, /*armor*/100, /*crit*/0, /*speed*/120);
    snapshot->set_magic_attack(20);
    snapshot->set_physical_attack(999);  // 技能只吃法伤
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillDamage));
    const auto result = engine.ResolveCurrentRound();

    const auto* damageEvent = FindFirstEvent(result, BATTLE_EVENT_DAMAGE);
    ASSERT_NE(damageEvent, nullptr);
    EXPECT_EQ(damageEvent->value(), 89u);
}

TEST(TurnBattleEngineTest, DerivedDefenseReducesIncomingDamageProportionally) {
    // 玩家 speed=12 < 怪物默认 60:怪物先手。怪物普攻 10 * (1 + 5*0.1) = 15;
    // 玩家 armor=0、resistance=0、等级 10(等级系数 1560),受伤比例 = 1560 / (防御 + 1560),最低 0.4。
    auto makeRun = [](uint64_t defense) {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9203, turnbattle::kMatchModePveSolo, 1);
        auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/4, /*armor*/0, /*crit*/0, /*speed*/12);
        snapshot->set_defense(defense);
        EXPECT_TRUE(engine.Initialize(request));
        EXPECT_TRUE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId)));
        return engine.ResolveCurrentRound();
    };
    const auto healthAfter = [](const TurnResultS2C& result) -> uint64_t {
        const auto* player = FindStateActor(result.state(), kPlayerA);
        EXPECT_NE(player, nullptr);
        return player != nullptr ? player->attributes().health() : 0;
    };

    // 防御 0:全额 15
    EXPECT_EQ(healthAfter(makeRun(0)), 985u);

    // 防御 = 等级系数 1560:减半,7.5 → 向上取整 8
    const auto halved = makeRun(1560);
    EXPECT_EQ(healthAfter(halved), 992u);
    const auto* halvedPlayer = FindStateActor(halved.state(), kPlayerA);
    ASSERT_NE(halvedPlayer, nullptr);
    EXPECT_EQ(halvedPlayer->defense(), 1560u);

    // 防御再高也只减六成。减法口径下这里是 0 伤害 —— 2026-09-10 低级怪全体打不动人的根因
    EXPECT_EQ(healthAfter(makeRun(1000000)),
              1000u - static_cast<uint64_t>(std::ceil(15.0 * (1.0 - combatdamage::kMaxPassiveReduction))));
}

// PVP(非 PVE 模式)的直接伤害再乘 kPvpDamageScale:同级同配置首击秒杀的对策(2026-09-13)
TEST(TurnBattleEngineTest, PvpDirectDamageIsScaled) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9204, 3 /* PVP */, 1);
    // A 力量 21:普攻 10 * (1 + 2.1) = 31;B 护甲 0、防御 0、抗性 0、等级 10 → 受伤比例 1
    AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/21, /*armor*/0, /*crit*/0, /*speed*/120);
    AddPlayer(request, kPlayerB, 1, 1000, 1000, /*strength*/0, /*armor*/0, /*crit*/0, /*speed*/60);
    ASSERT_TRUE(engine.Initialize(request));

    // SubmitAction 返回全员就绪状态；A 已提交，仍需等待 B。
    ASSERT_FALSE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB)));
    ASSERT_TRUE(engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_ATTACK, kPlayerA)));
    const auto result = engine.ResolveCurrentRound();

    const auto* damageEvent = FindFirstEvent(result, BATTLE_EVENT_DAMAGE);
    ASSERT_NE(damageEvent, nullptr);
    EXPECT_EQ(damageEvent->source_id(), kPlayerA);  // A 速度快,先出手
    // 31 * 0.3 = 9.3 → 向上取整 10;PVE 口径(不乘系数)是 31(2026-09-14 系数 0.2 → 0.3)
    EXPECT_EQ(damageEvent->value(), 10u);
}

// 技能按 SkillTable.damage_type 选攻击:物理技能吃物伤、法伤不混入;attack_multiplier 放大攻击那部分
TEST(TurnBattleEngineTest, PhysicalSkillUsesPhysicalAttackWithMultiplier) {
    constexpr uint32_t kSkillPhysical = 105;
    auto provider = MakeProvider();
    auto& skill = provider->AddSkill(kSkillPhysical);
    skill.add_targeting_mode(1);
    skill.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    skill.set_damage_type(combatdamage::kPhysicalDamage);
    skill.set_attack_multiplier(2.0);
    provider->SetSkillDamage(kSkillPhysical, 50.0);

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9205, turnbattle::kMatchModePveSolo, 1);
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/4, /*armor*/100, /*crit*/0, /*speed*/120);
    snapshot->set_physical_attack(30);
    snapshot->set_magic_attack(999);  // 物理技能不得吃法伤
    snapshot->add_skill_table_ids(kSkillPhysical);
    ASSERT_TRUE(engine.Initialize(request));

    ASSERT_TRUE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillPhysical)));
    const auto result = engine.ResolveCurrentRound();

    const auto* damageEvent = FindFirstEvent(result, BATTLE_EVENT_DAMAGE);
    ASSERT_NE(damageEvent, nullptr);
    // 50 * (1 + 4*0.1) + 30 * 2 = 130;130 * 1560 / (24 + 1560) ≈ 128.03 → 向上取整 129
    EXPECT_EQ(damageEvent->value(), 129u);
}

TEST(TurnBattleEngineTest, MonsterRowWithoutStatsFallsBackToDefaults) {
    auto provider = MakeProvider();
    constexpr uint32_t kBareMonster = 7002;
    provider->AddMonster(kBareMonster);  // 只有 id,其余字段全 0
    provider->SetDungeonMonsters(kDungeonConfig, {kBareMonster});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9040, turnbattle::kMatchModePveSolo, 11);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/20, /*armor*/10, /*crit*/0, /*speed*/240);
    ASSERT_TRUE(engine.Initialize(request));

    const auto state = engine.BuildStateSnapshot();  // 快照要先落地:指向临时对象的指针在整句结束后悬空
    const auto* monster = FindStateActor(state, kMonsterId);
    ASSERT_NE(monster, nullptr);
    EXPECT_EQ(monster->attributes().health(), turnbattle::kMonsterDefaultHealth);
    EXPECT_EQ(monster->max_health(), turnbattle::kMonsterDefaultHealth);
    EXPECT_EQ(monster->attributes().strength(), turnbattle::kMonsterDefaultStrength);
    EXPECT_EQ(monster->attributes().armor(), turnbattle::kMonsterDefaultArmor);
    EXPECT_EQ(monster->attributes().speed(), turnbattle::kMonsterDefaultSpeed);
    EXPECT_EQ(monster->monster_table_id(), kBareMonster);

    // 回退常量必须给出一个「真能打」的对手:玩家普攻 10*(1+2.0) = 30,受伤比例 1560/(24+1560) → 29.55 → 向上取整 30,
    // 300 血要 10 刀。(原期望 11 是 2026-09-13 改比例减伤前"攻击减护甲"口径 30-2=28 留下的,改公式时漏改)
    // 只断言 Outcome==ONGOING 是恒真的(刚 Initialize 完必然如此),抓不到任何回归。
    int rounds = 0;
    while (engine.Outcome() == BATTLE_OUTCOME_ONGOING && rounds < 30) {
        engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId));
        engine.ResolveCurrentRound();
        ++rounds;
    }
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);
    EXPECT_EQ(rounds, 10) << "回退怪物 300 血/护甲 24,应打满 10 回合而不是开局即结束";
}

// 表配了血量但速度为 0 → 只有速度回退常量(0 速会破坏出手序),其余一律按表
TEST(TurnBattleEngineTest, MonsterZeroSpeedInTableFallsBackToDefaultSpeed) {
    auto provider = MakeProvider();
    constexpr uint32_t kSlowMonster = 7003;
    auto& row = provider->AddMonster(kSlowMonster);
    row.set_health(150);
    row.set_strength(8);
    row.set_armor(1);
    provider->SetDungeonMonsters(kDungeonConfig, {kSlowMonster});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9041, turnbattle::kMatchModePveSolo, 12);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 20, 10, 0, 240);
    ASSERT_TRUE(engine.Initialize(request));

    const auto state = engine.BuildStateSnapshot();  // 快照要先落地:指向临时对象的指针在整句结束后悬空
    const auto* monster = FindStateActor(state, kMonsterId);
    ASSERT_NE(monster, nullptr);
    EXPECT_EQ(monster->attributes().health(), 150u);
    EXPECT_EQ(monster->attributes().strength(), 8u);
    EXPECT_EQ(monster->attributes().armor(), 1u);
    EXPECT_EQ(monster->attributes().speed(), turnbattle::kMonsterDefaultSpeed);
}

// 多怪副本:奖励按击杀的每只怪逐个累加,一只不多一只不少
TEST(TurnBattleEngineTest, RewardsAccumulateAcrossAllMonstersInGroup) {
    auto provider = MakeProvider();
    constexpr uint32_t kMonsterX = 7004;
    constexpr uint32_t kMonsterY = 7005;
    auto& x = provider->AddMonster(kMonsterX);
    x.set_health(60);
    x.set_strength(1);
    x.set_speed(36);
    x.set_exp_reward(10);
    x.set_gold_reward(5);
    auto& y = provider->AddMonster(kMonsterY);
    y.set_health(60);
    y.set_strength(1);
    y.set_speed(24);
    y.set_exp_reward(15);
    y.set_gold_reward(7);
    provider->SetDungeonMonsters(kDungeonConfig, {kMonsterX, kMonsterY});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9042, turnbattle::kMatchModePveSolo, 13);
    // 玩家力量 20 → 普攻 10*(1+2.0)=30(怪物护甲 0、受伤比例 1),每只 2 刀;怪物力量 1 每下只打几点
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 20, 10, 0, 240);
    ASSERT_TRUE(engine.Initialize(request));

    int rounds = 0;
    while (engine.Outcome() == BATTLE_OUTCOME_ONGOING && rounds < 30) {
        const auto state = engine.BuildStateSnapshot();
        uint64_t target = 0;
        for (const auto& actor : state.actors()) {
            if (actor.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && !actor.is_dead()) {
                target = actor.actor_id();
                break;
            }
        }
        ASSERT_NE(target, 0u);
        engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, target));
        engine.ResolveCurrentRound();
        ++rounds;
    }
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);
    EXPECT_EQ(rounds, 4);  // 2 只 x 2 刀,确定性

    const auto settlement = engine.BuildSettlement(kPlayerA);
    EXPECT_EQ(settlement.exp_gain(), 25u);
    EXPECT_EQ(settlement.gold_gain(), 12u);
    ASSERT_EQ(settlement.defeated_monsters_size(), 2);
    EXPECT_EQ(settlement.defeated_monsters(0).monster_config_id(), kMonsterX);
    EXPECT_EQ(settlement.defeated_monsters(1).monster_config_id(), kMonsterY);
    EXPECT_EQ(settlement.defeated_monsters(0).count(), 1u);
    EXPECT_EQ(settlement.defeated_monsters(1).count(), 1u);
    EXPECT_EQ(engine.BuildSettlement(kPlayerA).defeated_monsters_size(), 2);
}

// 胜方里逃跑的玩家不发奖:结算条件是 SIDE_A_WIN && team_index==0 && !fled && !is_dead。
// 只测 outcome 分支不够 —— 队伍打赢但自己中途跑了,照发奖就是刷奖漏洞。
TEST(TurnBattleEngineTest, FledPlayerOnWinningTeamGetsNoReward) {
    auto provider = MakeProvider();
    constexpr uint32_t kRewardMonster = 7006;
    auto& monster = provider->AddMonster(kRewardMonster);
    monster.set_health(150);
    monster.set_strength(1);   // 每下只打几点,不会误杀
    monster.set_speed(12);      // 最慢:B 的逃跑成功率被夹到上限 0.95
    monster.set_exp_reward(40);
    monster.set_gold_reward(20);
    provider->SetDungeonMonsters(kDungeonConfig, {kRewardMonster});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9043, turnbattle::kMatchModePveTeam, 21);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/20, /*armor*/10, /*crit*/0, /*speed*/360);
    AddPlayer(request, kPlayerB, 0, 1000, 1000, /*strength*/0, /*armor*/10, /*crit*/0, /*speed*/1440);
    ASSERT_TRUE(engine.Initialize(request));

    // 第一阶段:A 防御拖时间,B 反复逃跑直到成功(0.95/次,种子固定 → 确定性)
    bool fled = false;
    for (int round = 0; round < 5 && !fled; ++round) {
        engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_DEFEND));
        engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_FLEE));
        engine.ResolveCurrentRound();
        const auto* actorB = FindStateActor(engine.BuildStateSnapshot(), kPlayerB);
        ASSERT_NE(actorB, nullptr);
        fled = actorB->fled();
    }
    ASSERT_TRUE(fled) << "B 应已逃离战斗";
    ASSERT_EQ(engine.Outcome(), BATTLE_OUTCOME_ONGOING) << "A 还在,战斗不该结束";

    // 第二阶段:A 独自打死怪物
    for (int round = 0; round < 20 && engine.Outcome() == BATTLE_OUTCOME_ONGOING; ++round) {
        engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId));
        engine.ResolveCurrentRound();
    }
    ASSERT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);

    const auto settlementA = engine.BuildSettlement(kPlayerA);
    EXPECT_FALSE(settlementA.fled());
    EXPECT_EQ(settlementA.exp_gain(), 40u);
    EXPECT_EQ(settlementA.gold_gain(), 20u);

    const auto settlementB = engine.BuildSettlement(kPlayerB);
    EXPECT_TRUE(settlementB.fled());
    EXPECT_EQ(settlementB.defeated_monsters_size(), 0);
    ASSERT_EQ(settlementA.defeated_monsters_size(), 1);
    EXPECT_EQ(settlementA.defeated_monsters(0).monster_config_id(), kRewardMonster);
    EXPECT_EQ(settlementB.exp_gain(), 0u) << "逃跑玩家不能吃队伍的胜利奖励";
    EXPECT_EQ(settlementB.gold_gain(), 0u);
}

// 胜方里阵亡的玩家同样不发奖(结算条件的 !is_dead 那一半)。
// 与 FledPlayerOnWinningTeamGetsNoReward 成对:一个覆盖 fled,一个覆盖 is_dead。
TEST(TurnBattleEngineTest, DeadPlayerOnWinningTeamGetsNoReward) {
    auto provider = MakeProvider();
    constexpr uint32_t kBruiserMonster = 7007;
    auto& monster = provider->AddMonster(kBruiserMonster);
    monster.set_health(400);   // 够 A 慢慢磨,给怪物足够回合打到 B
    monster.set_strength(20);  // 普攻 10*(1+2.0)=30,足以一击带走 1 血的 B
    monster.set_speed(12);
    monster.set_exp_reward(60);
    monster.set_gold_reward(30);
    provider->SetDungeonMonsters(kDungeonConfig, {kBruiserMonster});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9044, turnbattle::kMatchModePveTeam, 33);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/20, /*armor*/10, /*crit*/0, /*speed*/360);
    AddPlayer(request, kPlayerB, 0, 1, 1, /*strength*/0, /*armor*/0, /*crit*/0, /*speed*/24);
    ASSERT_TRUE(engine.Initialize(request));

    // 第一阶段:A 防御拖回合,等怪物随机选到 B 把他打死(种子固定 → 确定性)
    bool dead = false;
    for (int round = 0; round < 15 && !dead && engine.Outcome() == BATTLE_OUTCOME_ONGOING; ++round) {
        engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_DEFEND));
        engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_DEFEND));
        engine.ResolveCurrentRound();
        const auto* actorB = FindStateActor(engine.BuildStateSnapshot(), kPlayerB);
        ASSERT_NE(actorB, nullptr);
        dead = actorB->is_dead();
    }
    ASSERT_TRUE(dead) << "B 应已阵亡";
    ASSERT_EQ(engine.Outcome(), BATTLE_OUTCOME_ONGOING) << "A 还活着,战斗不该结束";

    // 第二阶段:A 独自打死怪物
    for (int round = 0; round < 30 && engine.Outcome() == BATTLE_OUTCOME_ONGOING; ++round) {
        engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId));
        engine.ResolveCurrentRound();
    }
    ASSERT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);

    const auto settlementA = engine.BuildSettlement(kPlayerA);
    EXPECT_FALSE(settlementA.is_dead());
    EXPECT_EQ(settlementA.exp_gain(), 60u);
    EXPECT_EQ(settlementA.gold_gain(), 30u);

    const auto settlementB = engine.BuildSettlement(kPlayerB);
    EXPECT_TRUE(settlementB.is_dead());
    EXPECT_EQ(settlementB.defeated_monsters_size(), 0);
    ASSERT_EQ(settlementA.defeated_monsters_size(), 1);
    EXPECT_EQ(settlementA.defeated_monsters(0).monster_config_id(), kBruiserMonster);
    EXPECT_EQ(settlementB.exp_gain(), 0u) << "阵亡玩家不吃队伍的胜利奖励";
    EXPECT_EQ(settlementB.gold_gain(), 0u);
}

// 组队 PVE 的奖励口径:每个达成条件的成员**各得全额**,不是按人头平分。
// 结算是逐人重算的,平分/漏发都只会在多人局暴露,单人局测不出来。
TEST(TurnBattleEngineTest, EveryQualifyingTeamMemberGetsFullReward) {
    auto provider = MakeProvider();
    constexpr uint32_t kSharedMonster = 7009;
    auto& monster = provider->AddMonster(kSharedMonster);
    monster.set_health(120);
    monster.set_strength(1);
    monster.set_speed(12);
    monster.set_exp_reward(80);
    monster.set_gold_reward(40);
    provider->SetDungeonMonsters(kDungeonConfig, {kSharedMonster});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9055, turnbattle::kMatchModePveTeam, 7);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/20, /*armor*/10, /*crit*/0, /*speed*/360);
    AddPlayer(request, kPlayerB, 0, 1000, 1000, /*strength*/20, /*armor*/10, /*crit*/0, /*speed*/300);
    ASSERT_TRUE(engine.Initialize(request));

    int rounds = 0;
    while (engine.Outcome() == BATTLE_OUTCOME_ONGOING && rounds < 30) {
        engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId));
        engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId));
        engine.ResolveCurrentRound();
        ++rounds;
    }
    ASSERT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);

    const auto settlementA = engine.BuildSettlement(kPlayerA);
    const auto settlementB = engine.BuildSettlement(kPlayerB);
    EXPECT_EQ(settlementA.exp_gain(), 80u);
    EXPECT_EQ(settlementA.gold_gain(), 40u);
    EXPECT_EQ(settlementB.exp_gain(), 80u) << "队友不该被平分掉奖励";
    EXPECT_EQ(settlementB.gold_gain(), 40u);
}

// 回合上限来源:DungeonTable.time_limit(秒)换算;缺行或 time_limit==0 一律退回 kDefaultMaxRounds。
// 打满不是平局 —— 进攻方(A 方)判负(设计文档 §5.1),这条把上限与判负口径一起钉住。
TEST(TurnBattleEngineTest, MaxRoundsFallsBackToDefaultAndAttackerLosesOnTimeout) {
    constexpr uint32_t kTankMonster = 7010;
    // 僵局局面:玩家力量 0 且每回合防御(打不动怪),怪物力量 1 伤害很低(比例减伤、防御减半后每回合约 6 点)
    const auto playStalemate = [](const std::shared_ptr<MemoryBattleDataProvider>& provider,
                                  uint64_t battleId) {
        TurnBattleEngine engine(provider);
        auto request = MakeRequest(battleId, turnbattle::kMatchModePveSolo, 7);
        AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/0, /*armor*/10, /*crit*/0, /*speed*/240);
        EXPECT_TRUE(engine.Initialize(request));
        int rounds = 0;
        while (engine.Outcome() == BATTLE_OUTCOME_ONGOING && rounds < 60) {
            engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_DEFEND));
            engine.ResolveCurrentRound();
            ++rounds;
        }
        return std::make_pair(rounds, engine.Outcome());
    };
    const auto makeTankProvider = [](uint32_t monsterId) {
        auto provider = MakeProvider();
        auto& monster = provider->AddMonster(monsterId);
        monster.set_health(100000);  // 打不死,只能靠回合上限收场
        monster.set_strength(1);
        monster.set_speed(12);
        provider->SetDungeonMonsters(kDungeonConfig, {monsterId});
        return provider;
    };

    {  // 副本缺行 → 默认 30 回合
        const auto [rounds, outcome] = playStalemate(makeTankProvider(kTankMonster), 9060);
        EXPECT_EQ(rounds, static_cast<int>(turnbattle::kDefaultMaxRounds));
        EXPECT_EQ(outcome, BATTLE_OUTCOME_SIDE_B_WIN);
    }
    {  // 有行但 time_limit==0 → 同样退回默认(`> 0` 守卫)
        auto provider = makeTankProvider(kTankMonster);
        provider->AddDungeon(kDungeonConfig).set_time_limit(0);
        const auto [rounds, outcome] = playStalemate(provider, 9061);
        EXPECT_EQ(rounds, static_cast<int>(turnbattle::kDefaultMaxRounds));
        EXPECT_EQ(outcome, BATTLE_OUTCOME_SIDE_B_WIN);
    }
    {  // time_limit=12s,回合 6s → 2 回合
        auto provider = makeTankProvider(kTankMonster);
        provider->AddDungeon(kDungeonConfig).set_time_limit(12);
        const auto [rounds, outcome] = playStalemate(provider, 9062);
        EXPECT_EQ(rounds, 2);
        EXPECT_EQ(outcome, BATTLE_OUTCOME_SIDE_B_WIN);
    }
}

int main(int argc, char** argv) {
    ::testing::InitGoogleTest(&argc, argv);
    return RUN_ALL_TESTS();
}

// ---------------------------------------------------------------------------
// 表现层数据(turn-battle-presentation.md D1-D5):同一次行动内事件共用 group_id、
// 行动序 LastActionOrder、阵位 formation_slot、技能耗蓝 MANA 事件
// ---------------------------------------------------------------------------

// 一次行动 = ATTACK + 其伤害事件,共用一个非 0 group_id;不同行动 group_id 不同;
// 单目标 hit_index 恒为 0;基础命中率 100 下普攻不产出 MISS;行动序与事件流一致
TEST(TurnBattleEngineTest, PresentationGroupIdSharedWithinActionDistinctAcrossActions) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9101, 3 /* PVP 1v1 */, 1);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/20, /*armor*/10, 0, /*speed*/120);
    AddPlayer(request, kPlayerB, 1, 1000, 1000, /*strength*/20, /*armor*/10, 0, /*speed*/240);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));
    ASSERT_TRUE(engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_ATTACK, kPlayerA)));
    const auto result = engine.ResolveCurrentRound();

    // 行动序:B(速度 240)先手,A 后手;battle 节点把它填进 TurnResultS2C.action_order
    ASSERT_EQ(engine.LastActionOrder().size(), 2u);
    EXPECT_EQ(engine.LastActionOrder()[0], kPlayerB);
    EXPECT_EQ(engine.LastActionOrder()[1], kPlayerA);

    // 事件流:ATTACK(B→A) DAMAGE(B→A) ATTACK(A→B) DAMAGE(A→B)
    ASSERT_GE(result.events_size(), 4);
    EXPECT_EQ(result.events(0).event_type(), BATTLE_EVENT_ATTACK);
    EXPECT_EQ(result.events(1).event_type(), BATTLE_EVENT_DAMAGE);
    EXPECT_EQ(result.events(2).event_type(), BATTLE_EVENT_ATTACK);
    EXPECT_EQ(result.events(3).event_type(), BATTLE_EVENT_DAMAGE);
    EXPECT_EQ(result.events(0).source_id(), kPlayerB);
    EXPECT_EQ(result.events(2).source_id(), kPlayerA);

    EXPECT_NE(result.events(0).group_id(), 0u);
    EXPECT_EQ(result.events(0).group_id(), result.events(1).group_id());
    EXPECT_EQ(result.events(2).group_id(), result.events(3).group_id());
    EXPECT_NE(result.events(0).group_id(), result.events(2).group_id());
    for (const auto& event : result.events()) {
        EXPECT_EQ(event.hit_index(), 0u);
    }
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_MISS), 0);
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_DAMAGE), 2);
}

// 阵位:同队按快照顺序 0.. 递增,怪物方独立计数
TEST(TurnBattleEngineTest, PresentationFormationSlotIncrementsPerTeamInSnapshotOrder) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9102, turnbattle::kMatchModePveTeam, 1);
    AddPlayer(request, kPlayerA, 0, 500, 500, 20, 10, 0, 120);
    AddPlayer(request, kPlayerB, 0, 500, 500, 20, 10, 0, 108);
    ASSERT_TRUE(engine.Initialize(request));

    const auto state = engine.BuildStateSnapshot();
    const auto* actorA = FindStateActor(state, kPlayerA);
    const auto* actorB = FindStateActor(state, kPlayerB);
    const auto* monster = FindStateActor(state, kMonsterId);
    ASSERT_NE(actorA, nullptr);
    ASSERT_NE(actorB, nullptr);
    ASSERT_NE(monster, nullptr);
    EXPECT_EQ(actorA->formation_slot(), 0u);
    EXPECT_EQ(actorB->formation_slot(), 1u);
    EXPECT_EQ(monster->formation_slot(), 0u);  // 怪物队从 0 起
}

// 耗蓝技能:施放后产出 MANA 事件(value=消耗量,target_mana_after=剩余),与 SKILL 同 group;
// 状态快照里的法力同步扣减;无耗蓝技能不产出 MANA
TEST(TurnBattleEngineTest, PresentationSkillManaCostEmitsManaEventInSkillGroup) {
    auto provider = MakeProvider();
    auto* cost = provider->AddSkill(kSkillDamage).add_cost_resource();
    cost->set_cost_resource_id(turnbattle::kSkillCostResourceMana);
    cost->set_cost_resource_cost(30);

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9103, turnbattle::kMatchModePveSolo, 1);
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, 20, 10, 0, 240);
    snapshot->set_max_mana(100);
    snapshot->mutable_base_attributes()->set_mana(100);
    ASSERT_TRUE(engine.Initialize(request));

    ASSERT_TRUE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillDamage)));
    const auto result = engine.ResolveCurrentRound();

    const auto* skillEvent = FindFirstEvent(result, BATTLE_EVENT_SKILL);
    const auto* manaEvent = FindFirstEvent(result, BATTLE_EVENT_MANA);
    ASSERT_NE(skillEvent, nullptr);
    ASSERT_NE(manaEvent, nullptr);
    EXPECT_EQ(manaEvent->source_id(), kPlayerA);
    EXPECT_EQ(manaEvent->target_id(), kPlayerA);
    EXPECT_EQ(manaEvent->skill_table_id(), kSkillDamage);
    EXPECT_EQ(manaEvent->value(), 30u);
    EXPECT_EQ(manaEvent->target_mana_after(), 70u);
    EXPECT_EQ(manaEvent->group_id(), skillEvent->group_id());
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_MANA), 1);

    const auto state = engine.BuildStateSnapshot();
    const auto* actorA = FindStateActor(state, kPlayerA);
    ASSERT_NE(actorA, nullptr);
    EXPECT_EQ(actorA->attributes().mana(), 70u);

    // 第二回合改用无耗蓝的挂毒技能:不再产出 MANA
    if (engine.Outcome() == BATTLE_OUTCOME_ONGOING) {
        ASSERT_TRUE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillPoison)));
        const auto second = engine.ResolveCurrentRound();
        EXPECT_EQ(CountEvents(second, BATTLE_EVENT_MANA), 0);
    }
}

// ---------------------------------------------------------------------------
// 宝宝(宠物)作为独立参战单位(player-pet.md §5)
// ---------------------------------------------------------------------------


namespace {

// 给某个玩家快照挂一只出战宝宝
BattlePetSnapshot* AddPet(BattlePlayerSnapshot* owner, uint64_t petId, uint64_t health,
                          uint64_t maxHealth, uint64_t strength, uint64_t speed,
                          uint64_t physicalAttack = 0) {
    auto* pet = owner->add_pets();
    pet->set_pet_id(petId);
    pet->set_owner_player_id(owner->player_id());
    pet->set_pet_name("小灵狐");
    pet->set_pet_table_id(1);
    pet->set_level(owner->level());
    pet->set_max_health(maxHealth);
    pet->set_physical_attack(physicalAttack);
    auto* attributes = pet->mutable_base_attributes();
    attributes->set_health(health);
    attributes->set_strength(strength);
    attributes->set_speed(speed);
    return pet;
}

constexpr uint64_t kPetA = 700001;  // 宝宝的真实 pet_id(不是它在战斗里的 actor_id)

// 按真实 pet_id 找宝宝单位:宝宝的 actor_id 是引擎局内号(kPetActorIdBase 段),不等于 pet_id
const BattleActorState* FindPetActor(const BattleStateS2C& state, uint64_t petId) {
    for (const auto& actor : state.actors()) {
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET && actor.pet_id() == petId) {
            return &actor;
        }
    }
    return nullptr;
}

}  // namespace

TEST(TurnBattleEngineTest, PetJoinsOwnerTeamAndActsWithoutClientAction) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9401, turnbattle::kMatchModePveSolo, 31337);
    auto* owner = AddPlayer(request, kPlayerA, 0, 1000, 1000, 5, 0, 0, 120);
    // 宝宝比主人快:出手序里应排在主人之前
    AddPet(owner, kPetA, 400, 400, 4, 360);
    ASSERT_TRUE(engine.Initialize(request));

    const auto state = engine.BuildStateSnapshot();
    const auto* petActor = FindPetActor(state, kPetA);
    ASSERT_NE(petActor, nullptr);
    const uint64_t petActorId = petActor->actor_id();
    EXPECT_EQ(petActorId, turnbattle::kPetActorIdBase);  // 第一只宝宝拿局内号段的第 0 号
    EXPECT_EQ(petActor->team_index(), 0u);
    EXPECT_EQ(petActor->owner_player_id(), kPlayerA);
    // 宝宝无客户端行动权:标 auto,不进就绪判定 —— 主人一提交行动就能结算
    EXPECT_TRUE(petActor->is_auto());

    ASSERT_TRUE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId)));
    const auto result = engine.ResolveCurrentRound();
    // 出手序看 engine.LastActionOrder():TurnResultS2C.action_order 是 battle **节点**
    // 从这里透传上去的(battle_room_manager.cpp),引擎自己返回的那份里恒为空
    const auto& order = engine.LastActionOrder();
    ASSERT_GE(order.size(), 2u);
    EXPECT_EQ(order[0], petActorId);  // 速度 360 > 120

    // 宝宝这一回合确实出了手(普攻由默认行动路径代打)
    bool petAttacked = false;
    for (const auto& event : result.events()) {
        if (event.event_type() == BATTLE_EVENT_ATTACK && event.source_id() == petActorId) {
            petAttacked = true;
        }
    }
    EXPECT_TRUE(petAttacked);
}

TEST(TurnBattleEngineTest, PetIsNotControllableByClient) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9402, turnbattle::kMatchModePveSolo, 4242);
    auto* owner = AddPlayer(request, kPlayerA, 0, 1000, 1000, 5, 0, 0, 120);
    AddPet(owner, kPetA, 400, 400, 4, 360);
    ASSERT_TRUE(engine.Initialize(request));

    const auto* petBefore = FindPetActor(engine.BuildStateSnapshot(), kPetA);
    ASSERT_NE(petBefore, nullptr);
    const uint64_t petActorId = petBefore->actor_id();

    // 对宝宝提交行动不落账(SubmitAction 只收 PLAYER),也不能给它开关自动战斗
    EXPECT_FALSE(engine.SubmitAction(petActorId, MakeAction(BATTLE_ACTION_DEFEND)));
    EXPECT_NE(engine.SetActorAuto(petActorId, false), 0u);

    const auto state = engine.BuildStateSnapshot();
    const auto* petActor = FindPetActor(state, kPetA);
    ASSERT_NE(petActor, nullptr);
    EXPECT_TRUE(petActor->is_auto());
}

TEST(TurnBattleEngineTest, PetFinalStateGoesIntoOwnerSettlement) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9403, turnbattle::kMatchModePveSolo, 55);
    auto* owner = AddPlayer(request, kPlayerA, 0, 1000, 1000, 5, 0, 0, 120);
    AddPet(owner, kPetA, 400, 400, 4, 360);
    ASSERT_TRUE(engine.Initialize(request));

    ASSERT_TRUE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId)));
    engine.ResolveCurrentRound();

    const auto settlement = engine.BuildSettlement(kPlayerA);
    ASSERT_EQ(settlement.pets_size(), 1);
    EXPECT_EQ(settlement.pets(0).pet_id(), kPetA);
    EXPECT_LE(settlement.pets(0).health(), 400u);
    EXPECT_FALSE(settlement.pets(0).is_dead());
}

// ---------------------------------------------------------------------------
// 局内 actor_id 命名空间(2026-09-11):号段改造后 player_id / pet_id 都从 1 起发
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, PetWithSameIdAsItsOwnerDoesNotCollide) {
    // 1 号玩家带 1 号宝宝是开服第一天就会发生的事。旧写法宝宝直接用 pet_id 当 actor_id,
    // 这里会撞号、InitPets 拒绝开局
    constexpr uint64_t kSameId = 5;
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9404, turnbattle::kMatchModePveSolo, 7);
    auto* owner = AddPlayer(request, kSameId, 0, 1000, 1000, 5, 0, 0, 120);
    AddPet(owner, kSameId, 400, 400, 4, 360);
    ASSERT_TRUE(engine.Initialize(request));

    const auto state = engine.BuildStateSnapshot();
    const auto* player = FindStateActor(state, kSameId);
    const auto* pet = FindPetActor(state, kSameId);
    ASSERT_NE(player, nullptr);
    ASSERT_NE(pet, nullptr);
    EXPECT_EQ(player->actor_type(), BATTLE_ACTOR_TYPE_PLAYER);
    EXPECT_NE(pet->actor_id(), player->actor_id());

    // 结算按真实 pet_id 归还,而不是局内号
    ASSERT_TRUE(engine.SubmitAction(kSameId, MakeAction(BATTLE_ACTION_DEFEND)));
    engine.ResolveCurrentRound();
    const auto settlement = engine.BuildSettlement(kSameId);
    ASSERT_EQ(settlement.pets_size(), 1);
    EXPECT_EQ(settlement.pets(0).pet_id(), kSameId);
}

TEST(TurnBattleEngineTest, PlayerWithLegacyMonsterBaseIdDoesNotShareActorWithMonster) {
    // 旧 kMonsterActorIdBase = 1000000:第 100 万个玩家进 PVE 会与 0 号怪同号,
    // 而怪物追加不查重 —— 两个单位共用一个 actor_id,按 id 找人时静默打错
    constexpr uint64_t kMillionthPlayer = 1000000;
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9405, turnbattle::kMatchModePveSolo, 7);
    AddPlayer(request, kMillionthPlayer, 0, 1000, 1000, 5, 0, 0, 120);
    ASSERT_TRUE(engine.Initialize(request));

    const auto state = engine.BuildStateSnapshot();
    int holders = 0;
    for (const auto& actor : state.actors()) {
        if (actor.actor_id() == kMillionthPlayer) {
            ++holders;
        }
    }
    EXPECT_EQ(holders, 1);  // 只有玩家自己
    const auto* monster = FindStateActor(state, kMonsterId);
    ASSERT_NE(monster, nullptr);
    EXPECT_EQ(monster->actor_type(), BATTLE_ACTOR_TYPE_MONSTER);
}

TEST(TurnBattleEngineTest, PlayerIdInEngineLocalNamespaceIsRejected) {
    // bit63 是引擎局内号的保留段,真实 player_id 不可能落进来;落进来直接拒绝开局
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9406, turnbattle::kMatchModePveSolo, 7);
    AddPlayer(request, turnbattle::kEngineLocalActorIdFlag | 42, 0, 1000, 1000, 5, 0, 0, 120);
    EXPECT_FALSE(engine.Initialize(request));
}

// 与阵位顺序相反的击杀必须原样传到顺序任务，结算读取不能再次积累。
TEST(TurnBattleEngineTest, SettlementKeepsActualKillOrderAndRepeatedReadIsStable) {
    auto provider = MakeProvider();
    constexpr uint32_t xId = 7701, yId = 7702;
    for (const auto id : {xId, yId}) {
        auto& row = provider->AddMonster(id);
        row.set_health(8);
        row.set_strength(1);
        row.set_speed(12);
        row.set_exp_reward(id == xId ? 10 : 15);
        row.set_gold_reward(id == xId ? 5 : 7);
    }
    provider->SetDungeonMonsters(kDungeonConfig, {xId, yId});
    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9701, turnbattle::kMatchModePveSolo, 7);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 240);
    ASSERT_TRUE(engine.Initialize(request));
    ASSERT_TRUE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId + 1)));
    engine.ResolveCurrentRound();
    ASSERT_EQ(engine.Outcome(), BATTLE_OUTCOME_ONGOING);
    ASSERT_TRUE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId)));
    engine.ResolveCurrentRound();
    ASSERT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);
    const auto settlement = engine.BuildSettlement(kPlayerA);
    ASSERT_EQ(settlement.defeated_monsters_size(), 2);
    EXPECT_EQ(settlement.defeated_monsters(0).monster_config_id(), yId);
    EXPECT_EQ(settlement.defeated_monsters(1).monster_config_id(), xId);
    EXPECT_EQ(settlement.defeated_monsters(0).count(), 1u);
    EXPECT_EQ(settlement.defeated_monsters(1).count(), 1u);
    EXPECT_EQ(settlement.gold_gain(), 12u);
    EXPECT_EQ(settlement.exp_gain(), 25u);
    EXPECT_EQ(engine.BuildSettlement(kPlayerA).SerializeAsString(), settlement.SerializeAsString());
}

TEST(TurnBattleEngineTest, SettlementKeepsPoisonAndBurnKillBeforeLaterSkillKill) {
    for (const auto buffType : {turnbattle::kBuffTypePoison, turnbattle::kBuffTypeBurn}) {
        SCOPED_TRACE(buffType);
        auto provider = MakeProvider();
        provider->AddBuff(kBuffPoison).set_buff_type(buffType);
        constexpr uint32_t xId = 7711, yId = 7712;
        for (const auto id : {xId, yId}) {
            auto& row = provider->AddMonster(id);
            row.set_health(8);
            row.set_strength(1);
            row.set_speed(12);
            row.set_gold_reward(3);
        }
        provider->SetDungeonMonsters(kDungeonConfig, {xId, yId});
        TurnBattleEngine engine(provider);
        auto request = MakeRequest(9702, turnbattle::kMatchModePveSolo, 7);
        AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 240);
        ASSERT_TRUE(engine.Initialize(request));
        ASSERT_TRUE(engine.SubmitAction(kPlayerA,
            MakeAction(BATTLE_ACTION_SKILL, kMonsterId + 1, kSkillPoison)));
        const auto first = engine.ResolveCurrentRound();
        EXPECT_EQ(CountEvents(first, BATTLE_EVENT_DEATH), 1);
        ASSERT_TRUE(engine.SubmitAction(kPlayerA,
            MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillNuke)));
        engine.ResolveCurrentRound();
        ASSERT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);
        const auto settlement = engine.BuildSettlement(kPlayerA);
        ASSERT_EQ(settlement.defeated_monsters_size(), 2);
        EXPECT_EQ(settlement.defeated_monsters(0).monster_config_id(), yId);
        EXPECT_EQ(settlement.defeated_monsters(1).monster_config_id(), xId);
        EXPECT_EQ(settlement.gold_gain(), 6u);
        EXPECT_EQ(engine.BuildSettlement(kPlayerA).SerializeAsString(), settlement.SerializeAsString());
    }
}

TEST(TurnBattleEngineTest, SettlementDoesNotCreditUnknownOrMonsterSourceDeath) {
    for (const uint64_t sourceId : {uint64_t{0}, kMonsterId}) {
        SCOPED_TRACE(sourceId);
        auto provider = MakeProvider();
        constexpr uint32_t xId = 7721, yId = 7722;
        for (const auto id : {xId, yId}) {
            auto& row = provider->AddMonster(id);
            row.set_health(8);
            row.set_strength(1);
            row.set_speed(12);
            row.set_gold_reward(3);
        }
        provider->SetDungeonMonsters(kDungeonConfig, {xId, yId});
        TurnBattleEngine engine(provider);
        auto request = MakeRequest(9703, turnbattle::kMatchModePveSolo, 7);
        AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 240);
        ASSERT_TRUE(engine.Initialize(request));
        ASSERT_TRUE(turnbattle::TurnBattleEngineDeathTestAccess::AddBuff(
            engine, kMonsterId + 1, kBuffPoison, sourceId));
        ASSERT_TRUE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_DEFEND)));
        const auto first = engine.ResolveCurrentRound();
        EXPECT_EQ(CountEvents(first, BATTLE_EVENT_DEATH), 1);
        ASSERT_TRUE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId)));
        engine.ResolveCurrentRound();
        ASSERT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);
        const auto settlement = engine.BuildSettlement(kPlayerA);
        ASSERT_EQ(settlement.defeated_monsters_size(), 1);
        EXPECT_EQ(settlement.defeated_monsters(0).monster_config_id(), xId);
        EXPECT_EQ(settlement.gold_gain(), 3u);
    }
}

// 以两个已取得相同 Redis 锁结果的回调模拟 DEL 尚未完成的重复投递窗口。
TEST(SettlementApplicationCacheTest, DuplicateCallbacksAndReloginDoNotRepeatSideEffects) {
    using Cache = battle_settlement::SettlementApplicationCache;
    Cache cache;
    const auto now = Cache::Clock::time_point{};
    int gold = 0, missionKills = 0;
    auto apply = [&] { gold += 12; missionKills += 2; return true; };
    EXPECT_EQ(cache.Apply(kPlayerA, 100, now, apply), Cache::Result::Applied);
    EXPECT_EQ(cache.Apply(kPlayerA, 100, now, apply), Cache::Result::Duplicate);
    // 缓存键独立于 entt::entity，下线重建并再收到旧 pending 仍是同一笔。
    EXPECT_EQ(cache.Apply(kPlayerA, 100, now + std::chrono::seconds(2), apply), Cache::Result::Duplicate);
    EXPECT_EQ(gold, 12);
    EXPECT_EQ(missionKills, 2);
    // 新局 id 不要求单调增大；同局其他玩家也不应被吞。
    EXPECT_EQ(cache.Apply(kPlayerA, 99, now, apply), Cache::Result::Applied);
    EXPECT_EQ(cache.Apply(kPlayerB, 100, now, apply), Cache::Result::Applied);
    EXPECT_EQ(gold, 36);
    EXPECT_EQ(missionKills, 6);
}

TEST(SettlementApplicationCacheTest, ReentryIsBlockedAndFailedApplicationCanRetry) {
    using Cache = battle_settlement::SettlementApplicationCache;
    Cache cache;
    const auto now = Cache::Clock::time_point{};
    int effects = 0;
    EXPECT_EQ(cache.Apply(kPlayerA, 100, now, [&] {
        EXPECT_EQ(cache.Apply(kPlayerA, 100, now, [&] { ++effects; return true; }), Cache::Result::InFlight);
        return false; // 模拟金币入口在产生任何副作用之前拒绝。
    }), Cache::Result::Failed);
    EXPECT_EQ(effects, 0);
    EXPECT_EQ(cache.Size(), 0u);
    EXPECT_EQ(cache.Apply(kPlayerA, 100, now, [&] { ++effects; return true; }), Cache::Result::Applied);
    EXPECT_EQ(effects, 1);
}

TEST(SettlementApplicationCacheTest, ExceptionBeforeEffectsReleasesReservation) {
    using Cache = battle_settlement::SettlementApplicationCache;
    Cache cache;
    const auto now = Cache::Clock::time_point{};
    EXPECT_THROW(cache.Apply(kPlayerA, 100, now, []() -> bool {
        throw std::runtime_error("测试前置检查异常");
    }), std::runtime_error);
    EXPECT_EQ(cache.Size(), 0u);
    EXPECT_EQ(cache.Apply(kPlayerA, 100, now, [] { return true; }), Cache::Result::Applied);
}

TEST(SettlementApplicationCacheTest, CapacityNeverEvictsInflightAndRetentionIsFinite) {
    using Cache = battle_settlement::SettlementApplicationCache;
    Cache cache(1, std::chrono::seconds(10));
    const auto now = Cache::Clock::time_point{};
    EXPECT_EQ(cache.Apply(kPlayerA, 100, now, [&] {
        EXPECT_EQ(cache.Apply(kPlayerB, 100, now, [] { return true; }), Cache::Result::Full);
        EXPECT_EQ(cache.Size(), 1u);
        return true;
    }), Cache::Result::Applied);
    EXPECT_EQ(cache.Apply(kPlayerA, 100, now + std::chrono::seconds(9), [] { return true; }), Cache::Result::Duplicate);
    EXPECT_EQ(cache.Apply(kPlayerA, 100, now + std::chrono::seconds(10), [] { return true; }), Cache::Result::Applied);
    // 满容量淘汰已完成旧项，不阻止合法新局，也不增长内存。
    EXPECT_EQ(cache.Apply(kPlayerA, 101, now + std::chrono::seconds(11), [] { return true; }), Cache::Result::Applied);
    EXPECT_EQ(cache.Size(), 1u);
}

// ---------------------------------------------------------------------------
// 2026-09-17 缺口收口(docs/design/turn-battle-gap-closure.md):
// 掉落(G1)、道具表驱动/目标/限次/校验回码(G2)、快照 buff 清洗(G5)、
// 技能类型过滤(G6)、本人剩余道具(G7)
// ---------------------------------------------------------------------------

namespace {

constexpr uint32_t kMonsterWithDrop = 61;   // 必掉 kItemPotion 的测试怪
constexpr uint32_t kDungeonWithDrop = 62;
constexpr uint32_t kItemManaPotion = 302;   // 纯回蓝药
constexpr uint32_t kItemNotBattleUsable = 303;
constexpr uint32_t kSkillPassiveOnly = 401; // 被动技能:不该能提交
constexpr uint32_t kBuffStunTable = 250;    // 控制类:不该被快照带进来
constexpr uint32_t kBuffInstantTable = 251; // 瞬时类:同上

// 在标准表基础上补一只"必掉药"的怪 + 一个只有它的副本
std::shared_ptr<MemoryBattleDataProvider> MakeDropProvider() {
    auto provider = MakeProvider();
    auto& monster = provider->AddMonster(kMonsterWithDrop);
    monster.set_health(1);          // 一刀秒,保证第一回合就打完
    monster.set_speed(1);
    monster.set_exp_reward(7);
    monster.set_gold_reward(3);
    auto* drop = monster.add_drop();
    drop->set_drop_item(kItemPotion);
    drop->set_drop_count(2);
    drop->set_drop_rate(10000);     // 万分比:必掉
    provider->SetDungeonMonsters(kDungeonWithDrop, {kMonsterWithDrop});
    auto& dungeon = provider->AddDungeon(kDungeonWithDrop);
    dungeon.set_time_limit(1800);
    return provider;
}

}  // namespace

TEST(TurnBattleEngineTest, VictoryRollsMonsterDropsIntoSettlement) {
    auto provider = MakeDropProvider();
    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9301, turnbattle::kMatchModePveSolo, 42);
    request.set_battle_config_id(kDungeonWithDrop);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 500, 0, 0, 500);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK));
    engine.ResolveCurrentRound();
    ASSERT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);

    const auto settlement = engine.BuildSettlement(kPlayerA);
    ASSERT_EQ(settlement.items_gained_size(), 1);
    EXPECT_EQ(settlement.items_gained(0).item_table_id(), kItemPotion);
    EXPECT_EQ(settlement.items_gained(0).count(), 2u);
    // 掉落只在终局摇一次:重复读取结算必须完全一致(节点侧 outbox 会重投,读多次)
    const auto again = engine.BuildSettlement(kPlayerA);
    ASSERT_EQ(again.items_gained_size(), 1);
    EXPECT_EQ(again.items_gained(0).count(), 2u);
}

TEST(TurnBattleEngineTest, DropRateZeroNeverDrops) {
    auto provider = MakeDropProvider();
    provider->AddMonster(kMonsterWithDrop).mutable_drop(0)->set_drop_rate(0);

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9302, turnbattle::kMatchModePveSolo, 42);
    request.set_battle_config_id(kDungeonWithDrop);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 500, 0, 0, 500);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK));
    engine.ResolveCurrentRound();
    ASSERT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);
    EXPECT_EQ(engine.BuildSettlement(kPlayerA).items_gained_size(), 0);
}

TEST(TurnBattleEngineTest, ItemEffectComesFromItemTableAndManaPotionRestoresMana) {
    auto provider = MakeProvider();
    auto& manaPotion = provider->AddItem(kItemManaPotion);
    manaPotion.set_battle_usable(1);
    manaPotion.set_battle_heal_mp(30);

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9303, turnbattle::kMatchModePveSolo, 7);
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 500);
    snapshot->mutable_base_attributes()->set_mana(10);
    snapshot->set_max_mana(100);
    auto* item = snapshot->add_items();
    item->set_item_table_id(kItemManaPotion);
    item->set_count(1);
    ASSERT_TRUE(engine.Initialize(request));

    EXPECT_TRUE(engine.SubmitAction(
        kPlayerA, MakeAction(BATTLE_ACTION_ITEM, kPlayerA, 0, kItemManaPotion)));
    const auto result = engine.ResolveCurrentRound();
    const auto* itemEvent = FindFirstEvent(result, BATTLE_EVENT_ITEM);
    ASSERT_NE(itemEvent, nullptr);
    EXPECT_EQ(itemEvent->value(), 30u);              // 纯回蓝药:value 取回蓝量
    EXPECT_EQ(itemEvent->target_mana_after(), 40u);  // 10 + 30
}

TEST(TurnBattleEngineTest, ValidateActionRejectsNonBattleItemAndUnknownItem) {
    auto provider = MakeProvider();
    provider->AddItem(kItemNotBattleUsable).set_battle_usable(0);  // 不是战斗消耗品

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9304, turnbattle::kMatchModePveSolo, 7);
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 500);
    auto* item = snapshot->add_items();
    item->set_item_table_id(kItemNotBattleUsable);
    item->set_count(5);
    ASSERT_TRUE(engine.Initialize(request));

    // 表里没有这个 id
    EXPECT_EQ(engine.ValidateAction(kPlayerA, MakeAction(BATTLE_ACTION_ITEM, 0, 0, 9999)),
              kInvalidTableId);
    // 表里有但不可战斗使用
    EXPECT_EQ(
        engine.ValidateAction(kPlayerA, MakeAction(BATTLE_ACTION_ITEM, 0, 0, kItemNotBattleUsable)),
        kInvalidParameter);
    // 校验不过的行动不落账:单人局因此不会就绪
    EXPECT_FALSE(
        engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ITEM, 0, 0, kItemNotBattleUsable)));
}

TEST(TurnBattleEngineTest, ItemCanTargetTeammateButNotEnemy) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9305, turnbattle::kMatchModePveTeam, 7);
    auto* healer = AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 500);
    AddPlayer(request, kPlayerB, 0, 50, 1000, 0, 100, 0, 10);
    auto* item = healer->add_items();
    item->set_item_table_id(kItemPotion);
    item->set_count(2);
    ASSERT_TRUE(engine.Initialize(request));

    // 给队友用药:合法
    EXPECT_EQ(
        engine.ValidateAction(kPlayerA, MakeAction(BATTLE_ACTION_ITEM, kPlayerB, 0, kItemPotion)),
        kSuccess);
    // 给敌方用药:拒绝
    const uint64_t monsterId = turnbattle::kMonsterActorIdBase;
    EXPECT_EQ(
        engine.ValidateAction(kPlayerA, MakeAction(BATTLE_ACTION_ITEM, monsterId, 0, kItemPotion)),
        kSkillInvalidTarget);
    // 不存在的目标:拒绝
    EXPECT_EQ(
        engine.ValidateAction(kPlayerA, MakeAction(BATTLE_ACTION_ITEM, 123456, 0, kItemPotion)),
        kSkillInvalidTargetId);

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ITEM, kPlayerB, 0, kItemPotion));
    engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_DEFEND));
    const auto result = engine.ResolveCurrentRound();
    const auto* itemEvent = FindFirstEvent(result, BATTLE_EVENT_ITEM);
    ASSERT_NE(itemEvent, nullptr);
    EXPECT_EQ(itemEvent->source_id(), kPlayerA);
    EXPECT_EQ(itemEvent->target_id(), kPlayerB);  // 药落在队友身上
    EXPECT_EQ(itemEvent->value(), 100u);
    // 消耗记在用药者账上,队友账本为空
    ASSERT_EQ(engine.BuildSettlement(kPlayerA).items_consumed_size(), 1);
    EXPECT_EQ(engine.BuildSettlement(kPlayerA).items_consumed(0).count(), 1u);
    EXPECT_EQ(engine.BuildSettlement(kPlayerB).items_consumed_size(), 0);
}

TEST(TurnBattleEngineTest, PvpItemUseIsCappedPerBattle) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9306, 3 /* PVP 1V1 */, 7);
    auto* attacker = AddPlayer(request, kPlayerA, 0, 1000, 5000, 0, 100, 0, 500);
    AddPlayer(request, kPlayerB, 1, 1000, 5000, 0, 100, 0, 10);
    auto* item = attacker->add_items();
    item->set_item_table_id(kItemPotion);
    item->set_count(turnbattle::kMaxItemUsesPerBattlePvp + 3);
    ASSERT_TRUE(engine.Initialize(request));

    for (uint32_t used = 0; used < turnbattle::kMaxItemUsesPerBattlePvp; ++used) {
        ASSERT_EQ(engine.ValidateAction(kPlayerA, MakeAction(BATTLE_ACTION_ITEM, 0, 0, kItemPotion)),
                  kSuccess)
            << "第 " << used << " 次用药应当放行";
        engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ITEM, 0, 0, kItemPotion));
        engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_DEFEND));
        engine.ResolveCurrentRound();
    }
    // 副本里还有药,但 PVP 限次拒绝(不限次可以靠海量药水把回合拖满白赢)
    EXPECT_FALSE(engine.SelfItems(kPlayerA).empty());
    EXPECT_EQ(engine.ValidateAction(kPlayerA, MakeAction(BATTLE_ACTION_ITEM, 0, 0, kItemPotion)),
              kInvalidParameter);
}

TEST(TurnBattleEngineTest, SelfItemsReflectsRemainingCopyAndDropsExhaustedEntries) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9307, turnbattle::kMatchModePveSolo, 7);
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 100, 1000, 0, 100, 0, 500);
    auto* item = snapshot->add_items();
    item->set_item_table_id(kItemPotion);
    item->set_count(1);
    ASSERT_TRUE(engine.Initialize(request));

    const auto before = engine.SelfItems(kPlayerA);
    ASSERT_EQ(before.size(), 1u);
    EXPECT_EQ(before[0].item_table_id(), kItemPotion);
    EXPECT_EQ(before[0].count(), 1u);

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ITEM, 0, 0, kItemPotion));
    engine.ResolveCurrentRound();

    // 用光的条目不再下发(客户端按"没有这一项"处理)
    EXPECT_TRUE(engine.SelfItems(kPlayerA).empty());
    // 不在本局的玩家:空表
    EXPECT_TRUE(engine.SelfItems(kPlayerC).empty());
}

TEST(TurnBattleEngineTest, SnapshotBuffsDropControlInstantUnknownAndSanitizeCaster) {
    auto provider = MakeProvider();
    auto& stunBuff = provider->AddBuff(kBuffStunTable);
    stunBuff.set_buff_type(turnbattle::kBuffTypeStun);
    stunBuff.set_duration(12.0);
    auto& instantBuff = provider->AddBuff(kBuffInstantTable);
    instantBuff.set_buff_type(turnbattle::kBuffTypePoison);
    instantBuff.set_duration(0.0);  // 非无限 + duration<=0 = 瞬时

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9308, turnbattle::kMatchModePveSolo, 7);
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 500);
    // 控制类:必须丢掉,否则场景里只剩半秒的眩晕进战斗会变成整整几回合
    auto* stun = snapshot->add_buffs();
    stun->set_buff_id(11);
    stun->set_buff_table_id(kBuffStunTable);
    stun->set_remain_rounds(2);
    // 瞬时类:实时侧挂上即到期,不该在战斗里永驻
    auto* instant = snapshot->add_buffs();
    instant->set_buff_id(12);
    instant->set_buff_table_id(kBuffInstantTable);
    instant->set_remain_rounds(0);
    // 表里没有的 buff:丢掉
    auto* unknown = snapshot->add_buffs();
    unknown->set_buff_id(13);
    unknown->set_buff_table_id(9999);
    // 合法 buff,但 caster 是本局不存在的 id(scene 的 entt 实体整数)
    auto* poison = snapshot->add_buffs();
    poison->set_buff_id(14);
    poison->set_buff_table_id(kBuffPoison);
    poison->set_remain_rounds(2);
    poison->set_caster_id(777777);
    ASSERT_TRUE(engine.Initialize(request));

    const auto state = engine.BuildStateSnapshot();
    const auto* actor = FindStateActor(state, kPlayerA);
    ASSERT_NE(actor, nullptr);
    ASSERT_EQ(actor->buffs_size(), 1);
    EXPECT_EQ(actor->buffs(0).buff_table_id(), kBuffPoison);
    EXPECT_EQ(actor->buffs(0).caster_id(), 0u);  // 认不出的施法者置 0
}

TEST(TurnBattleEngineTest, PassiveSkillIsNotCastableAndNotListedOnActor) {
    auto provider = MakeProvider();
    provider->AddSkill(kSkillPassiveOnly).add_skill_type(turnbattle::kSkillTypeBitPassive);

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9309, turnbattle::kMatchModePveSolo, 7);
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 500);
    snapshot->add_skill_table_ids(kSkillPassiveOnly);
    ASSERT_TRUE(engine.Initialize(request));

    const auto state = engine.BuildStateSnapshot();
    const auto* actor = FindStateActor(state, kPlayerA);
    ASSERT_NE(actor, nullptr);
    bool listed = false;
    for (const auto skillTableId : actor->skill_table_ids()) {
        listed = listed || skillTableId == kSkillPassiveOnly;
    }
    EXPECT_FALSE(listed);
    // 不在列表里 = 校验链按"未持有"拒绝
    EXPECT_EQ(
        engine.ValidateAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kPlayerA, kSkillPassiveOnly)),
        kInvalidParameter);
}

// ---------------------------------------------------------------------------
// 装备战斗类属性(docs/design/equipment-attributes.md §4.5,D3–D8):
// 必杀 / 抗性按伤害类型分流、连击、反震、反击、所有技能上升、抗异常、零概率不耗随机数、
// 新掷骰的随机数消费顺序、下发前清洗。
//
// 数值口径(全部选在离整数足够远的地方,向上取整不受浮点末位影响):
//   PVE 打默认怪(护甲 24、等级 10 → 受伤比例 1560 / 1584 ≈ 0.98485):
//     力量 4 普攻 = 14 × 0.98485 ≈ 13.79 → 14,必杀 ≈ 27.58 → 28;
//     再叠物伤 50 = 64 × 0.98485 ≈ 63.03 → 64,连击段 ≈ 31.52 → 32;
//     50 点技能 = 70 × 0.98485 ≈ 68.94 → 69,必杀 ≈ 137.88 → 138。
//   PVP 双方无甲无防(受伤比例 1,再乘 kPvpDamageScale 0.3),力量 0:
//     物伤 1004 普攻 = 1014 × 0.3 = 304.2 → 305,连击段 152.1 → 153;物伤 504 普攻 = 154.2 → 155;
//     物伤 1001 普攻 = 1011 × 0.3 = 303.3 → 304。
// ---------------------------------------------------------------------------

namespace {

using turnbattle::TurnBattleEngineDeathTestAccess;

constexpr uint32_t kMatchModePvp = 3;           // PVP 1v1:非 PVE,直接伤害再乘 kPvpDamageScale
constexpr uint32_t kSkillPhysicalStrike = 106;  // 物理伤害技能(damage_type = 物理),基础伤害 50
constexpr uint32_t kBuffAilmentFreeze = 261;    // 冰冻
constexpr uint32_t kBuffAilmentStun = 262;      // 眩晕
constexpr uint32_t kBuffBurn = 263;             // 灼烧:不在异常映射里
constexpr uint32_t kBuffStunWithDispel = 264;   // 眩晕 + 顺带驱散毒 tag

// 标准表之上补:一个物理技能、冰冻 / 眩晕 / 灼烧 buff、一个带驱散的眩晕 buff
std::shared_ptr<MemoryBattleDataProvider> MakeCombatProvider() {
    auto provider = MakeProvider();

    auto& physicalSkill = provider->AddSkill(kSkillPhysicalStrike);
    physicalSkill.add_targeting_mode(1);
    physicalSkill.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    physicalSkill.set_damage_type(combatdamage::kPhysicalDamage);
    provider->SetSkillDamage(kSkillPhysicalStrike, 50.0);

    auto& freezeBuff = provider->AddBuff(kBuffAilmentFreeze);
    freezeBuff.set_buff_type(turnbattle::kBuffTypeFreeze);
    freezeBuff.set_duration(12.0);

    auto& stunBuff = provider->AddBuff(kBuffAilmentStun);
    stunBuff.set_buff_type(turnbattle::kBuffTypeStun);
    stunBuff.set_duration(12.0);

    auto& burnBuff = provider->AddBuff(kBuffBurn);
    burnBuff.set_buff_type(turnbattle::kBuffTypeBurn);
    burnBuff.set_duration(12.0);

    auto& stunDispelBuff = provider->AddBuff(kBuffStunWithDispel);
    stunDispelBuff.set_buff_type(turnbattle::kBuffTypeStun);
    stunDispelBuff.set_duration(12.0);
    (*stunDispelBuff.mutable_dispel_tag())["poison_tag"] = true;

    return provider;
}

// 某个出手者打出的全部 DAMAGE 事件(按事件流顺序;指针指向 result 内部,result 活着才有效)
std::vector<const BattleEventItem*> DamageEventsFrom(const TurnResultS2C& result,
                                                     uint64_t sourceId) {
    std::vector<const BattleEventItem*> events;
    for (const auto& event : result.events()) {
        if (event.event_type() == BATTLE_EVENT_DAMAGE && event.source_id() == sourceId) {
            events.push_back(&event);
        }
    }
    return events;
}

int CountEventsOfKind(const TurnResultS2C& result, eBattleEventType eventType,
                      eBattleHitKind hitKind) {
    int count = 0;
    for (const auto& event : result.events()) {
        if (event.event_type() == eventType && event.hit_kind() == hitKind) {
            ++count;
        }
    }
    return count;
}

// PVP 打一回合:A 出指定行动,B 防御。B 更慢,防御落在 A 出手之后,不影响 A 这一下的伤害
TurnResultS2C ResolveDuelRound(TurnBattleEngine& engine, const BattleAction& attackerAction) {
    engine.SubmitAction(kPlayerA, attackerAction);
    engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_DEFEND));
    return engine.ResolveCurrentRound();
}

}  // namespace

// ---- 数据贯通:快照 → 引擎单位状态 ----

TEST(TurnBattleEngineTest, CombatAttributesAreCopiedFromSnapshotForPlayersOnly) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9500, turnbattle::kMatchModePveTeam, 1);
    auto* equipped = AddPlayer(request, kPlayerA, 0, 1000, 1000, 5, 0, 0, 120);
    equipped->mutable_combat()->set_physical_crit_rate(7);
    equipped->mutable_combat()->set_resist_confusion(9);
    AddPet(equipped, kPetA, 400, 400, 4, 360);
    AddPlayer(request, kPlayerB, 0, 1000, 1000, 5, 0, 0, 108);  // 快照没带 combat
    ASSERT_TRUE(engine.Initialize(request));

    const auto state = engine.BuildStateSnapshot();
    const auto* equippedActor = FindStateActor(state, kPlayerA);
    ASSERT_NE(equippedActor, nullptr);
    EXPECT_EQ(equippedActor->combat().physical_crit_rate(), 7u);
    EXPECT_EQ(equippedActor->combat().resist_confusion(), 9u);

    // 没带的玩家、宝宝、怪物:连子消息都不建(全 0),单位状态字节与装备系统落地前相同
    const auto* bareActor = FindStateActor(state, kPlayerB);
    const auto* petActor = FindPetActor(state, kPetA);
    const auto* monsterActor = FindStateActor(state, kMonsterId);
    ASSERT_NE(bareActor, nullptr);
    ASSERT_NE(petActor, nullptr);
    ASSERT_NE(monsterActor, nullptr);
    EXPECT_FALSE(bareActor->has_combat());
    EXPECT_FALSE(petActor->has_combat());
    EXPECT_FALSE(monsterActor->has_combat());
}

// ---- 必杀分类:物理必杀率只管物理伤害,法术必杀率只管法术伤害 ----

TEST(TurnBattleEngineTest, CombatPhysicalCritRateAppliesToPhysicalDamageOnly) {
    TurnBattleEngine engine(MakeCombatProvider());
    auto request = MakeRequest(9501, turnbattle::kMatchModePveSolo, 1);
    // 基础暴击率置 0,隔离出装备加成的那一份
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/4, /*armor*/100, /*crit*/0, /*speed*/120);
    snapshot->add_skill_table_ids(kSkillPhysicalStrike);
    snapshot->mutable_combat()->set_physical_crit_rate(100);
    ASSERT_TRUE(engine.Initialize(request));

    // 普攻(物理):必杀,13.79 × 2 → 28
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId));
    auto result = engine.ResolveCurrentRound();
    auto hits = DamageEventsFrom(result, kPlayerA);
    ASSERT_EQ(hits.size(), 1u);
    EXPECT_TRUE(hits[0]->is_critical());
    EXPECT_EQ(hits[0]->value(), 28u);

    // 法术技能:不因物理必杀率而必杀,68.94 → 69
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillDamage));
    result = engine.ResolveCurrentRound();
    hits = DamageEventsFrom(result, kPlayerA);
    ASSERT_EQ(hits.size(), 1u);
    EXPECT_FALSE(hits[0]->is_critical());
    EXPECT_EQ(hits[0]->value(), 69u);

    // 物理技能:分流看的是伤害类型而不是"普攻还是技能",同样必杀,68.94 × 2 → 138
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillPhysicalStrike));
    result = engine.ResolveCurrentRound();
    hits = DamageEventsFrom(result, kPlayerA);
    ASSERT_EQ(hits.size(), 1u);
    EXPECT_TRUE(hits[0]->is_critical());
    EXPECT_EQ(hits[0]->value(), 138u);
}

TEST(TurnBattleEngineTest, CombatMagicCritRateAppliesToMagicDamageOnly) {
    TurnBattleEngine engine(MakeCombatProvider());
    auto request = MakeRequest(9502, turnbattle::kMatchModePveSolo, 1);
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/4, /*armor*/100, /*crit*/0, /*speed*/120);
    snapshot->add_skill_table_ids(kSkillPhysicalStrike);
    snapshot->mutable_combat()->set_magic_crit_rate(100);
    ASSERT_TRUE(engine.Initialize(request));

    // 普攻(物理):不因法术必杀率而必杀,13.79 → 14
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId));
    auto result = engine.ResolveCurrentRound();
    auto hits = DamageEventsFrom(result, kPlayerA);
    ASSERT_EQ(hits.size(), 1u);
    EXPECT_FALSE(hits[0]->is_critical());
    EXPECT_EQ(hits[0]->value(), 14u);

    // 法术技能:必杀,68.94 × 2 → 138
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillDamage));
    result = engine.ResolveCurrentRound();
    hits = DamageEventsFrom(result, kPlayerA);
    ASSERT_EQ(hits.size(), 1u);
    EXPECT_TRUE(hits[0]->is_critical());
    EXPECT_EQ(hits[0]->value(), 138u);

    // 物理技能:不必杀,68.94 → 69
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillPhysicalStrike));
    result = engine.ResolveCurrentRound();
    hits = DamageEventsFrom(result, kPlayerA);
    ASSERT_EQ(hits.size(), 1u);
    EXPECT_FALSE(hits[0]->is_critical());
    EXPECT_EQ(hits[0]->value(), 69u);
}

// ---- 抗性分类:抗物理只减物理伤害,抗法术只减法术伤害;叠在基础抗性上,共用 60% 封顶 ----

TEST(TurnBattleEngineTest, CombatResistAppliesToMatchingDamageTypeOnly) {
    // A 打 B 一下,返回这一下的伤害。A 的物伤 1001、法伤 961:普攻与 50 点法术技能的原始伤害都是 1011
    const auto hitDamage = [](uint64_t baseResistance, uint64_t physicalResist,
                              uint64_t magicResist, bool useMagicSkill) -> uint64_t {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9503, kMatchModePvp, 1);
        auto* attacker = AddPlayer(request, kPlayerA, 0, 100000, 100000, 0, 0, 0, 120);
        attacker->set_physical_attack(1001);
        attacker->set_magic_attack(961);
        auto* defender = AddPlayer(request, kPlayerB, 1, 100000, 100000, 0, 0, 0, 60);
        defender->mutable_base_attributes()->set_resistance(baseResistance);
        defender->mutable_combat()->set_physical_resist(physicalResist);
        defender->mutable_combat()->set_magic_resist(magicResist);
        EXPECT_TRUE(engine.Initialize(request));

        const auto result = ResolveDuelRound(
            engine, useMagicSkill ? MakeAction(BATTLE_ACTION_SKILL, kPlayerB, kSkillDamage)
                                  : MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));
        const auto hits = DamageEventsFrom(result, kPlayerA);
        EXPECT_EQ(hits.size(), 1u);
        return hits.empty() ? 0 : hits[0]->value();
    };

    // 对照组:无抗性,1011 × 0.3 = 303.3 → 304
    EXPECT_EQ(hitDamage(0, 0, 0, false), 304u);
    EXPECT_EQ(hitDamage(0, 0, 0, true), 304u);

    // 抗物理 30:普攻 1011 × 0.7 × 0.3 = 212.31 → 213;法术技能不受影响
    EXPECT_EQ(hitDamage(0, 30, 0, false), 213u);
    EXPECT_EQ(hitDamage(0, 30, 0, true), 304u);

    // 抗法术 30:法术技能 → 213;普攻不受影响
    EXPECT_EQ(hitDamage(0, 0, 30, false), 304u);
    EXPECT_EQ(hitDamage(0, 0, 30, true), 213u);

    // 叠在基础抗性上:10 + 30 = 40 → 1011 × 0.6 × 0.3 = 181.98 → 182
    EXPECT_EQ(hitDamage(10, 30, 0, false), 182u);
    EXPECT_EQ(hitDamage(10, 0, 30, true), 182u);

    // 与护甲 / 防御共用 60% 常驻减伤封顶(D7):抗物理 90 也只减六成,1011 × 0.4 × 0.3 = 121.32 → 122
    EXPECT_EQ(hitDamage(0, 90, 0, false), 122u);
}

// ---- 连击 ----

TEST(TurnBattleEngineTest, CombatComboRateAddsOneHalfDamageHitInSameGroup) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9510, turnbattle::kMatchModePveSolo, 1);
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/4, /*armor*/100, /*crit*/0, /*speed*/120);
    snapshot->set_physical_attack(50);
    snapshot->mutable_combat()->set_combo_rate(100);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId));
    const auto result = engine.ResolveCurrentRound();

    // 恰好 2 段:首段 63.03 → 64,追加段按普攻公式独立结算再 × 50% = 31.52 → 32
    const auto hits = DamageEventsFrom(result, kPlayerA);
    ASSERT_EQ(hits.size(), 2u);
    EXPECT_EQ(hits[0]->hit_index(), 0u);
    EXPECT_EQ(hits[0]->hit_kind(), BATTLE_HIT_NORMAL);
    EXPECT_EQ(hits[0]->value(), 64u);
    EXPECT_EQ(hits[0]->target_health_after(), turnbattle::kMonsterDefaultHealth - 64);
    EXPECT_EQ(hits[1]->hit_index(), 1u);
    EXPECT_EQ(hits[1]->hit_kind(), BATTLE_HIT_COMBO);
    EXPECT_EQ(hits[1]->value(), 32u);
    EXPECT_EQ(hits[1]->value(), (hits[0]->value() + 1) / 2);  // ceil(首段 × 50%)
    EXPECT_EQ(hits[1]->target_id(), kMonsterId);
    EXPECT_EQ(hits[1]->target_health_after(), turnbattle::kMonsterDefaultHealth - 64 - 32);
    // 同一拍:与首段同 group_id;整次普攻只有一个 ATTACK 事件
    EXPECT_NE(hits[0]->group_id(), 0u);
    EXPECT_EQ(hits[1]->group_id(), hits[0]->group_id());
    int attackEvents = 0;
    for (const auto& event : result.events()) {
        if (event.event_type() == BATTLE_EVENT_ATTACK && event.source_id() == kPlayerA) {
            ++attackEvents;
        }
    }
    EXPECT_EQ(attackEvents, 1);

    // 怪物没有战斗类属性:它的普攻只有一段,且 hit_index 回到 0(连击的段序不外溢到后面的行动)
    const auto monsterHits = DamageEventsFrom(result, kMonsterId);
    ASSERT_EQ(monsterHits.size(), 1u);
    EXPECT_EQ(monsterHits[0]->hit_index(), 0u);
    EXPECT_EQ(monsterHits[0]->hit_kind(), BATTLE_HIT_NORMAL);
}

TEST(TurnBattleEngineTest, CombatComboDoesNotContinueAfterKillingTarget) {
    auto provider = MakeProvider();
    constexpr uint32_t kFrailMonster = 7801;
    auto& monster = provider->AddMonster(kFrailMonster);
    monster.set_health(10);  // 首段 14 点就打死
    monster.set_strength(1);
    monster.set_speed(12);
    provider->SetDungeonMonsters(kDungeonConfig, {kFrailMonster});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9511, turnbattle::kMatchModePveSolo, 1);
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/4, /*armor*/100, /*crit*/0, /*speed*/120);
    snapshot->mutable_combat()->set_combo_rate(100);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId));
    const auto result = engine.ResolveCurrentRound();

    const auto hits = DamageEventsFrom(result, kPlayerA);
    ASSERT_EQ(hits.size(), 1u);  // 目标已死,不追加
    EXPECT_EQ(hits[0]->value(), 10u);
    EXPECT_EQ(hits[0]->hit_kind(), BATTLE_HIT_NORMAL);
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_DEATH), 1);
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);
}

TEST(TurnBattleEngineTest, CombatComboAlsoAppliesWhenSkillDowngradesToBasicAttack) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9512, turnbattle::kMatchModePveSolo, 1);
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/4, /*armor*/100, /*crit*/0, /*speed*/120);
    snapshot->set_physical_attack(50);
    snapshot->mutable_combat()->set_combo_rate(100);
    ASSERT_TRUE(engine.Initialize(request));

    // 提交时技能合法;出手前被沉默 → 结算期校验链不过,降级成普攻。降级出来的普攻同样吃连击
    ASSERT_TRUE(engine.SubmitAction(kPlayerA,
                                    MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillDamage)));
    ASSERT_TRUE(TurnBattleEngineDeathTestAccess::AddBuff(engine, kPlayerA, kBuffSilence, kMonsterId));
    const auto result = engine.ResolveCurrentRound();

    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_SKILL), 0);
    const auto hits = DamageEventsFrom(result, kPlayerA);
    ASSERT_EQ(hits.size(), 2u);
    EXPECT_EQ(hits[0]->value(), 64u);
    EXPECT_EQ(hits[1]->hit_kind(), BATTLE_HIT_COMBO);
    EXPECT_EQ(hits[1]->value(), 32u);
}

// ---- 反震 ----

TEST(TurnBattleEngineTest, CombatReflectRateReturnsHalfOfDealtDamageToAttacker) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9520, kMatchModePvp, 1);
    auto* attacker = AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 0, 0, 120);
    attacker->set_physical_attack(1004);
    auto* defender = AddPlayer(request, kPlayerB, 1, 1000, 1000, 0, 0, 0, 60);
    defender->mutable_combat()->set_reflect_rate(100);
    ASSERT_TRUE(engine.Initialize(request));

    const auto result = ResolveDuelRound(engine, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));

    // 事件流:ATTACK(A→B) DAMAGE(A→B 305) DAMAGE(B→A 反震 153) DEFEND(B)
    ASSERT_EQ(result.events_size(), 4);
    const auto& attackEvent = result.events(0);
    const auto& damageEvent = result.events(1);
    const auto& reflectEvent = result.events(2);
    EXPECT_EQ(attackEvent.event_type(), BATTLE_EVENT_ATTACK);
    EXPECT_EQ(damageEvent.event_type(), BATTLE_EVENT_DAMAGE);
    EXPECT_EQ(damageEvent.source_id(), kPlayerA);
    EXPECT_EQ(damageEvent.target_id(), kPlayerB);
    EXPECT_EQ(damageEvent.hit_kind(), BATTLE_HIT_NORMAL);
    EXPECT_EQ(damageEvent.value(), 305u);

    EXPECT_EQ(reflectEvent.event_type(), BATTLE_EVENT_DAMAGE);
    EXPECT_EQ(reflectEvent.source_id(), kPlayerB);  // 受击者
    EXPECT_EQ(reflectEvent.target_id(), kPlayerA);  // 出手者
    EXPECT_EQ(reflectEvent.hit_kind(), BATTLE_HIT_REFLECT);
    EXPECT_EQ(reflectEvent.value(), 153u);          // ceil(305 × 50%)
    EXPECT_EQ(reflectEvent.value(), (damageEvent.value() + 1) / 2);
    EXPECT_FALSE(reflectEvent.is_critical());
    EXPECT_EQ(reflectEvent.target_health_after(), 1000u - 153u);  // 填的是出手者的气血
    EXPECT_EQ(reflectEvent.group_id(), damageEvent.group_id());
    EXPECT_EQ(result.events(3).event_type(), BATTLE_EVENT_DEFEND);

    const auto* attackerState = FindStateActor(result.state(), kPlayerA);
    const auto* defenderState = FindStateActor(result.state(), kPlayerB);
    ASSERT_NE(attackerState, nullptr);
    ASSERT_NE(defenderState, nullptr);
    EXPECT_EQ(attackerState->attributes().health(), 1000u - 153u);
    EXPECT_EQ(defenderState->attributes().health(), 1000u - 305u);
}

TEST(TurnBattleEngineTest, CombatReflectAlsoTriggersOnComboHit) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9521, kMatchModePvp, 1);
    auto* attacker = AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 0, 0, 120);
    attacker->set_physical_attack(1004);
    attacker->mutable_combat()->set_combo_rate(100);
    auto* defender = AddPlayer(request, kPlayerB, 1, 1000, 1000, 0, 0, 0, 60);
    defender->mutable_combat()->set_reflect_rate(100);
    ASSERT_TRUE(engine.Initialize(request));

    const auto result = ResolveDuelRound(engine, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));

    // 每段固定顺序 落伤害 → 反震 → 连击续段:
    // ATTACK, DAMAGE(首段 305), DAMAGE(反震 153), DAMAGE(连击 153), DAMAGE(反震 77), DEFEND
    ASSERT_EQ(result.events_size(), 6);
    EXPECT_EQ(result.events(1).hit_kind(), BATTLE_HIT_NORMAL);
    EXPECT_EQ(result.events(1).value(), 305u);
    EXPECT_EQ(result.events(2).hit_kind(), BATTLE_HIT_REFLECT);
    EXPECT_EQ(result.events(2).value(), 153u);
    EXPECT_EQ(result.events(2).hit_index(), 0u);
    EXPECT_EQ(result.events(3).hit_kind(), BATTLE_HIT_COMBO);
    EXPECT_EQ(result.events(3).source_id(), kPlayerA);
    EXPECT_EQ(result.events(3).value(), 153u);  // 152.1 → 153
    EXPECT_EQ(result.events(3).hit_index(), 1u);
    EXPECT_EQ(result.events(4).hit_kind(), BATTLE_HIT_REFLECT);
    EXPECT_EQ(result.events(4).source_id(), kPlayerB);
    EXPECT_EQ(result.events(4).target_id(), kPlayerA);
    EXPECT_EQ(result.events(4).value(), 77u);  // ceil(153 × 50%)
    EXPECT_EQ(result.events(4).hit_index(), 1u);
    EXPECT_EQ(result.events(4).target_health_after(), 1000u - 153u - 77u);
    for (int index = 0; index < 5; ++index) {
        EXPECT_EQ(result.events(index).group_id(), result.events(0).group_id());
    }
}

TEST(TurnBattleEngineTest, CombatReflectKillingAttackerEndsItsActionAndDecidesOutcome) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9522, kMatchModePvp, 1);
    // A 只有 100 血:打出 305,反震 153 夹到 100,自己被弹死。
    // A 还带着 100% 连击、B 还带着 100% 反击 —— 出手者一死,这两样都不该再发生
    auto* attacker = AddPlayer(request, kPlayerA, 0, 100, 100, 0, 0, 0, 120);
    attacker->set_physical_attack(1004);
    attacker->mutable_combat()->set_combo_rate(100);
    auto* defender = AddPlayer(request, kPlayerB, 1, 1000, 1000, 0, 0, 0, 60);
    defender->mutable_combat()->set_reflect_rate(100);
    defender->mutable_combat()->set_counter_rate(100);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));
    engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_ATTACK, kPlayerA));
    const auto result = engine.ResolveCurrentRound();

    // ATTACK(A→B) DAMAGE(A→B) DAMAGE(B→A 反震,致死) DEATH(A);B 自己的行动因为没有存活敌人而落空
    ASSERT_EQ(result.events_size(), 4);
    EXPECT_EQ(result.events(0).event_type(), BATTLE_EVENT_ATTACK);
    EXPECT_EQ(result.events(1).event_type(), BATTLE_EVENT_DAMAGE);
    EXPECT_EQ(result.events(1).value(), 305u);
    EXPECT_EQ(result.events(2).event_type(), BATTLE_EVENT_DAMAGE);
    EXPECT_EQ(result.events(2).hit_kind(), BATTLE_HIT_REFLECT);
    EXPECT_EQ(result.events(2).value(), 100u);  // 不超过出手者当前气血
    EXPECT_EQ(result.events(2).target_health_after(), 0u);
    EXPECT_EQ(result.events(3).event_type(), BATTLE_EVENT_DEATH);
    EXPECT_EQ(result.events(3).target_id(), kPlayerA);
    EXPECT_EQ(CountEventsOfKind(result, BATTLE_EVENT_DAMAGE, BATTLE_HIT_COMBO), 0);
    EXPECT_EQ(CountEventsOfKind(result, BATTLE_EVENT_ATTACK, BATTLE_HIT_COUNTER), 0);

    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_B_WIN);
    EXPECT_TRUE(engine.BuildSettlement(kPlayerA).is_dead());
    EXPECT_FALSE(engine.BuildSettlement(kPlayerB).is_dead());
}

TEST(TurnBattleEngineTest, CombatReflectDoesNotTriggerOnSkillOrLethalHit) {
    // 技能伤害不反震
    {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9523, kMatchModePvp, 1);
        auto* attacker = AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 0, 0, 120);
        attacker->set_magic_attack(961);
        auto* defender = AddPlayer(request, kPlayerB, 1, 1000, 1000, 0, 0, 0, 60);
        defender->mutable_combat()->set_reflect_rate(100);
        ASSERT_TRUE(engine.Initialize(request));

        const auto result =
            ResolveDuelRound(engine, MakeAction(BATTLE_ACTION_SKILL, kPlayerB, kSkillDamage));
        EXPECT_EQ(DamageEventsFrom(result, kPlayerA).size(), 1u);
        EXPECT_EQ(CountEventsOfKind(result, BATTLE_EVENT_DAMAGE, BATTLE_HIT_REFLECT), 0);
        const auto* attackerState = FindStateActor(result.state(), kPlayerA);
        ASSERT_NE(attackerState, nullptr);
        EXPECT_EQ(attackerState->attributes().health(), 1000u);
    }

    // 受击者被这一下打死:死人不反震
    {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9524, kMatchModePvp, 1);
        auto* attacker = AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 0, 0, 120);
        attacker->set_physical_attack(1004);
        auto* defender = AddPlayer(request, kPlayerB, 1, 10, 10, 0, 0, 0, 60);
        defender->mutable_combat()->set_reflect_rate(100);
        ASSERT_TRUE(engine.Initialize(request));

        const auto result = ResolveDuelRound(engine, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));
        EXPECT_EQ(CountEventsOfKind(result, BATTLE_EVENT_DAMAGE, BATTLE_HIT_REFLECT), 0);
        EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);
        EXPECT_EQ(engine.BuildSettlement(kPlayerA).health(), 1000u);
    }
}

TEST(TurnBattleEngineTest, CombatReflectKillCreditsDefenderAndWinsBattle) {
    auto provider = MakeProvider();
    constexpr uint32_t kGlassMonster = 7802;
    auto& monster = provider->AddMonster(kGlassMonster);
    monster.set_health(5);     // 自己普攻打出 15,被弹回 ceil(7.5) = 8,夹到 5 点气血
    monster.set_strength(5);
    monster.set_speed(96);     // 比玩家快,先出手
    monster.set_exp_reward(50);
    monster.set_gold_reward(25);
    provider->SetDungeonMonsters(kDungeonConfig, {kGlassMonster});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9525, turnbattle::kMatchModePveSolo, 1);
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 0, 0, /*speed*/12);
    snapshot->mutable_combat()->set_reflect_rate(100);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_DEFEND));
    const auto result = engine.ResolveCurrentRound();

    // ATTACK(怪→A) DAMAGE(怪→A 15) DAMAGE(A→怪 反震 5) DEATH(怪) DEFEND(A)
    ASSERT_EQ(result.events_size(), 5);
    EXPECT_EQ(result.events(1).value(), 15u);
    EXPECT_EQ(result.events(2).hit_kind(), BATTLE_HIT_REFLECT);
    EXPECT_EQ(result.events(2).source_id(), kPlayerA);
    EXPECT_EQ(result.events(2).target_id(), kMonsterId);
    EXPECT_EQ(result.events(2).value(), 5u);
    EXPECT_EQ(result.events(3).event_type(), BATTLE_EVENT_DEATH);
    EXPECT_EQ(result.events(3).target_id(), kMonsterId);

    // 击杀归属受击者(玩家):照常记击杀簿、发经验金币
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);
    const auto settlement = engine.BuildSettlement(kPlayerA);
    ASSERT_EQ(settlement.defeated_monsters_size(), 1);
    EXPECT_EQ(settlement.defeated_monsters(0).monster_config_id(), kGlassMonster);
    EXPECT_EQ(settlement.exp_gain(), 50u);
    EXPECT_EQ(settlement.gold_gain(), 25u);
}

// ---- 反击 ----

TEST(TurnBattleEngineTest, CombatCounterRateStrikesBackOnceInItsOwnGroup) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9530, kMatchModePvp, 1);
    auto* attacker = AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 0, 0, 120);
    attacker->set_physical_attack(1004);
    attacker->mutable_combat()->set_counter_rate(100);  // 出手者自己也 100% 反击:不得反击"反击"
    auto* defender = AddPlayer(request, kPlayerB, 1, 1000, 1000, 0, 0, 0, 60);
    defender->set_physical_attack(504);
    defender->mutable_combat()->set_counter_rate(100);
    ASSERT_TRUE(engine.Initialize(request));

    const auto result = ResolveDuelRound(engine, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));

    // ATTACK(A→B) DAMAGE(A→B) ATTACK(B→A 反击) DAMAGE(B→A 反击) DEFEND(B)
    ASSERT_EQ(result.events_size(), 5);
    const auto& attackEvent = result.events(0);
    const auto& damageEvent = result.events(1);
    const auto& counterAttack = result.events(2);
    const auto& counterDamage = result.events(3);
    EXPECT_EQ(attackEvent.event_type(), BATTLE_EVENT_ATTACK);
    EXPECT_EQ(attackEvent.source_id(), kPlayerA);
    EXPECT_EQ(attackEvent.target_id(), kPlayerB);
    EXPECT_EQ(attackEvent.hit_kind(), BATTLE_HIT_NORMAL);
    EXPECT_EQ(damageEvent.event_type(), BATTLE_EVENT_DAMAGE);
    EXPECT_EQ(damageEvent.source_id(), kPlayerA);
    EXPECT_EQ(damageEvent.hit_kind(), BATTLE_HIT_NORMAL);
    EXPECT_EQ(damageEvent.value(), 305u);

    EXPECT_EQ(counterAttack.event_type(), BATTLE_EVENT_ATTACK);
    EXPECT_EQ(counterAttack.source_id(), kPlayerB);
    EXPECT_EQ(counterAttack.target_id(), kPlayerA);
    EXPECT_EQ(counterAttack.hit_kind(), BATTLE_HIT_COUNTER);
    EXPECT_EQ(counterDamage.event_type(), BATTLE_EVENT_DAMAGE);
    EXPECT_EQ(counterDamage.source_id(), kPlayerB);
    EXPECT_EQ(counterDamage.target_id(), kPlayerA);
    EXPECT_EQ(counterDamage.hit_kind(), BATTLE_HIT_COUNTER);
    EXPECT_EQ(counterDamage.value(), 155u);  // 按普攻公式:514 × 0.3 = 154.2 → 155,不打折
    EXPECT_EQ(counterDamage.target_health_after(), 1000u - 155u);
    EXPECT_EQ(result.events(4).event_type(), BATTLE_EVENT_DEFEND);

    // 分拍:出手一组、反击另起一组(非 0、hit_index 归 0)、B 自己的行动又是新的一组
    EXPECT_NE(attackEvent.group_id(), 0u);
    EXPECT_EQ(damageEvent.group_id(), attackEvent.group_id());
    EXPECT_NE(counterAttack.group_id(), attackEvent.group_id());
    EXPECT_EQ(counterDamage.group_id(), counterAttack.group_id());
    EXPECT_EQ(counterAttack.hit_index(), 0u);
    EXPECT_EQ(counterDamage.hit_index(), 0u);
    EXPECT_NE(result.events(4).group_id(), counterAttack.group_id());
    EXPECT_NE(result.events(4).group_id(), attackEvent.group_id());

    // 反击不引发对方再反击
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_ATTACK), 2);
}

TEST(TurnBattleEngineTest, CombatCounterDoesNotChainWhenBothSidesAlwaysCounter) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9531, kMatchModePvp, 1);
    auto* attacker = AddPlayer(request, kPlayerA, 0, 100000, 100000, 0, 0, 0, 120);
    attacker->mutable_combat()->set_counter_rate(100);
    attacker->mutable_combat()->set_combo_rate(100);    // 反击那一下不吃连击
    attacker->mutable_combat()->set_reflect_rate(100);  // 反击那一下也不被反震
    auto* defender = AddPlayer(request, kPlayerB, 1, 100000, 100000, 0, 0, 0, 60);
    defender->mutable_combat()->set_counter_rate(100);
    ASSERT_TRUE(engine.Initialize(request));

    // 双方互相普攻:每次普攻恰好引发一次反击,反击本身不再引发任何连锁
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));
    engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_ATTACK, kPlayerA));
    const auto result = engine.ResolveCurrentRound();

    EXPECT_EQ(CountEventsOfKind(result, BATTLE_EVENT_ATTACK, BATTLE_HIT_NORMAL), 2);
    EXPECT_EQ(CountEventsOfKind(result, BATTLE_EVENT_ATTACK, BATTLE_HIT_COUNTER), 2);
    EXPECT_EQ(CountEventsOfKind(result, BATTLE_EVENT_DAMAGE, BATTLE_HIT_COUNTER), 2);
    // A 的出手:首段 + 连击段;B 没有反震,所以这两段不被弹
    // B 的出手:一段,被 A 反震一次;A 反击 B 的那一下只有一段(反击不连击)
    EXPECT_EQ(CountEventsOfKind(result, BATTLE_EVENT_DAMAGE, BATTLE_HIT_COMBO), 1);
    EXPECT_EQ(CountEventsOfKind(result, BATTLE_EVENT_DAMAGE, BATTLE_HIT_REFLECT), 1);
    EXPECT_EQ(CountEventsOfKind(result, BATTLE_EVENT_DAMAGE, BATTLE_HIT_NORMAL), 2);
    // B 反击 A 的那一下没有被 A 的反震弹回:唯一的反震事件来自 B 的正常出手,排在 B 的 ATTACK 之后
    int lastNormalAttackIndex = -1;
    int reflectIndex = -1;
    for (int index = 0; index < result.events_size(); ++index) {
        const auto& event = result.events(index);
        if (event.event_type() == BATTLE_EVENT_ATTACK && event.hit_kind() == BATTLE_HIT_NORMAL) {
            lastNormalAttackIndex = index;
        }
        if (event.event_type() == BATTLE_EVENT_DAMAGE && event.hit_kind() == BATTLE_HIT_REFLECT) {
            reflectIndex = index;
        }
    }
    ASSERT_GE(lastNormalAttackIndex, 0);
    EXPECT_EQ(result.events(lastNormalAttackIndex).source_id(), kPlayerB);
    EXPECT_GT(reflectIndex, lastNormalAttackIndex);
}

TEST(TurnBattleEngineTest, CombatCounterIsSuppressedWhileStunnedOrFrozen) {
    // 0 = 对照组(没被控,应当反击);其余两组:受击者开打前被对手挂上眩晕 / 冰冻
    for (const uint32_t controlBuff : {uint32_t{0}, kBuffAilmentStun, kBuffAilmentFreeze}) {
        SCOPED_TRACE(controlBuff);
        TurnBattleEngine engine(MakeCombatProvider());
        auto request = MakeRequest(9532, kMatchModePvp, 1);
        AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 0, 0, 120);
        auto* defender = AddPlayer(request, kPlayerB, 1, 1000, 1000, 0, 0, 0, 60);
        defender->mutable_combat()->set_counter_rate(100);
        ASSERT_TRUE(engine.Initialize(request));
        if (controlBuff != 0) {
            ASSERT_TRUE(TurnBattleEngineDeathTestAccess::AddBuff(engine, kPlayerB, controlBuff,
                                                                 kPlayerA));
        }

        // 被控单位的提交不落账,结算时它自己的行动也作废;这里只看它会不会还手
        const auto result = ResolveDuelRound(engine, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));
        EXPECT_EQ(DamageEventsFrom(result, kPlayerA).size(), 1u);
        EXPECT_EQ(CountEventsOfKind(result, BATTLE_EVENT_ATTACK, BATTLE_HIT_COUNTER),
                  controlBuff == 0 ? 1 : 0);
    }
}

TEST(TurnBattleEngineTest, CombatCounterDoesNotTriggerOnSkill) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9533, kMatchModePvp, 1);
    auto* attacker = AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 0, 0, 120);
    attacker->set_magic_attack(961);
    auto* defender = AddPlayer(request, kPlayerB, 1, 1000, 1000, 0, 0, 0, 60);
    defender->mutable_combat()->set_counter_rate(100);
    ASSERT_TRUE(engine.Initialize(request));

    const auto result =
        ResolveDuelRound(engine, MakeAction(BATTLE_ACTION_SKILL, kPlayerB, kSkillDamage));
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_SKILL), 1);
    EXPECT_EQ(DamageEventsFrom(result, kPlayerA).size(), 1u);
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_ATTACK), 0);  // 技能不触发反击
    EXPECT_EQ(DamageEventsFrom(result, kPlayerB).size(), 0u);
}

TEST(TurnBattleEngineTest, CombatCounterKillingAttackerGoesThroughDeathHandling) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9534, kMatchModePvp, 1);
    AddPlayer(request, kPlayerA, 0, 50, 50, 0, 0, 0, 120);  // 反击 155 点足以打死
    auto* defender = AddPlayer(request, kPlayerB, 1, 1000, 1000, 0, 0, 0, 60);
    defender->set_physical_attack(504);
    defender->mutable_combat()->set_counter_rate(100);
    ASSERT_TRUE(engine.Initialize(request));

    const auto result = ResolveDuelRound(engine, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));

    // ATTACK(A→B) DAMAGE(A→B) ATTACK(B→A 反击) DAMAGE(B→A 反击,夹到 50) DEATH(A) DEFEND(B)
    ASSERT_EQ(result.events_size(), 6);
    EXPECT_EQ(result.events(3).hit_kind(), BATTLE_HIT_COUNTER);
    EXPECT_EQ(result.events(3).value(), 50u);
    EXPECT_EQ(result.events(3).target_health_after(), 0u);
    EXPECT_EQ(result.events(4).event_type(), BATTLE_EVENT_DEATH);
    EXPECT_EQ(result.events(4).target_id(), kPlayerA);
    EXPECT_EQ(result.events(4).group_id(), result.events(3).group_id());
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_B_WIN);
}

// ---- 所有技能上升 ----

TEST(TurnBattleEngineTest, CombatSkillLevelBonusRaisesSkillDamageLevelParameter) {
    // 技能伤害 = 50 + 2 × 等级参数;玩家 10 级、力量 0、法伤 0,打默认怪
    const auto castOnce = [](uint64_t skillLevelBonus, double& levelParameter,
                             uint32_t& actorLevel) -> uint64_t {
        auto provider = MakeProvider();
        provider->SetSkillDamagePerLevel(kSkillDamage, 2.0);
        TurnBattleEngine engine(provider);
        auto request = MakeRequest(9540, turnbattle::kMatchModePveSolo, 1);
        auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/0, /*armor*/100, /*crit*/0, /*speed*/120);
        if (skillLevelBonus > 0) {
            snapshot->mutable_combat()->set_skill_level_bonus(skillLevelBonus);
        }
        EXPECT_TRUE(engine.Initialize(request));

        engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillDamage));
        const auto result = engine.ResolveCurrentRound();
        levelParameter = provider->LastSkillDamageLevel();
        const auto* actor = FindStateActor(result.state(), kPlayerA);
        EXPECT_NE(actor, nullptr);
        actorLevel = actor != nullptr ? actor->level() : 0;
        const auto hits = DamageEventsFrom(result, kPlayerA);
        EXPECT_EQ(hits.size(), 1u);
        return hits.empty() ? 0 : hits[0]->value();
    };

    double levelParameter = 0.0;
    uint32_t actorLevel = 0;
    // 无加成:等级参数 = 10 → 基础 70 × 0.98485 = 68.94 → 69
    EXPECT_EQ(castOnce(0, levelParameter, actorLevel), 69u);
    EXPECT_EQ(levelParameter, 10.0);
    EXPECT_EQ(actorLevel, 10u);

    // 所有技能上升 5:等级参数 = 15 → 基础 80 × 0.98485 = 78.79 → 79;角色等级本身不变
    EXPECT_EQ(castOnce(5, levelParameter, actorLevel), 79u);
    EXPECT_EQ(levelParameter, 15.0);
    EXPECT_EQ(actorLevel, 10u);
}

// ---- 抗异常 ----

TEST(TurnBattleEngineTest, CombatAilmentResistBlocksEnemyPoisonAndEmitsResistEvent) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9550, kMatchModePvp, 1);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 0, 0, 120);
    auto* defender = AddPlayer(request, kPlayerB, 1, 1000, 1000, 0, 0, 0, 60);
    defender->mutable_combat()->set_resist_poison(100);
    ASSERT_TRUE(engine.Initialize(request));

    const auto result =
        ResolveDuelRound(engine, MakeAction(BATTLE_ACTION_SKILL, kPlayerB, kSkillPoison));

    const auto* skillEvent = FindFirstEvent(result, BATTLE_EVENT_SKILL);
    const auto* resistEvent = FindFirstEvent(result, BATTLE_EVENT_RESIST);
    ASSERT_NE(skillEvent, nullptr);
    ASSERT_NE(resistEvent, nullptr);
    EXPECT_EQ(resistEvent->source_id(), kPlayerA);  // 施加者
    EXPECT_EQ(resistEvent->target_id(), kPlayerB);  // 抵抗者
    EXPECT_EQ(resistEvent->buff_table_id(), kBuffPoison);
    EXPECT_EQ(resistEvent->group_id(), skillEvent->group_id());
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_RESIST), 1);
    // 没挂上:无 BUFF_ADD、回合末无毒 tick、状态里没有 buff、气血没掉
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_ADD), 0);
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_TICK), 0);
    const auto* defenderState = FindStateActor(result.state(), kPlayerB);
    ASSERT_NE(defenderState, nullptr);
    EXPECT_EQ(defenderState->buffs_size(), 0);
    EXPECT_EQ(defenderState->attributes().health(), 1000u);
}

TEST(TurnBattleEngineTest, CombatAilmentResistAddsAllAilmentAndSubtractsIgnore) {
    // A 给 B 上毒;返回本回合事件。三项都取 0 / 100 的组合,有效抵抗率恒为 0 或 >= 100,不涉及随机
    const auto castPoison = [](uint64_t resistPoison, uint64_t resistAllAilment,
                               uint64_t ignoreAilmentResist) {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9551, kMatchModePvp, 1);
        auto* attacker = AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 0, 0, 120);
        attacker->mutable_combat()->set_ignore_ailment_resist(ignoreAilmentResist);
        auto* defender = AddPlayer(request, kPlayerB, 1, 1000, 1000, 0, 0, 0, 60);
        defender->mutable_combat()->set_resist_poison(resistPoison);
        defender->mutable_combat()->set_resist_all_ailment(resistAllAilment);
        EXPECT_TRUE(engine.Initialize(request));
        return ResolveDuelRound(engine, MakeAction(BATTLE_ACTION_SKILL, kPlayerB, kSkillPoison));
    };
    const auto expectResisted = [](const TurnResultS2C& result, bool resisted) {
        EXPECT_EQ(CountEvents(result, BATTLE_EVENT_RESIST), resisted ? 1 : 0);
        EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_ADD), resisted ? 0 : 1);
    };

    {
        SCOPED_TRACE("no resist at all");
        expectResisted(castPoison(0, 0, 0), false);
    }
    {
        SCOPED_TRACE("all-ailment resist alone");
        expectResisted(castPoison(0, 100, 0), true);
    }
    {
        SCOPED_TRACE("specific 60 + all 40 = 100");
        expectResisted(castPoison(60, 40, 0), true);
    }
    {
        SCOPED_TRACE("ignore 100 cancels resist 100");
        expectResisted(castPoison(100, 0, 100), false);
    }
    {
        SCOPED_TRACE("ignore 100 cancels specific 60 + all 40");
        expectResisted(castPoison(60, 40, 100), false);
    }
    {
        SCOPED_TRACE("ignore 100 only cancels half of 100 + 100");
        expectResisted(castPoison(100, 100, 100), true);
    }
    {
        SCOPED_TRACE("ignore larger than resist floors at zero");
        expectResisted(castPoison(40, 0, 100), false);
    }
}

TEST(TurnBattleEngineTest, CombatAilmentResistDoesNotApplyToOwnTeamBuffs) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9552, turnbattle::kMatchModePveTeam, 1);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 0, 0, 120);
    auto* teammate = AddPlayer(request, kPlayerB, 0, 1000, 1000, 0, 0, 0, 108);
    teammate->mutable_combat()->set_resist_poison(100);
    teammate->mutable_combat()->set_resist_all_ailment(100);
    ASSERT_TRUE(engine.Initialize(request));

    // 队友给他上毒、他自己给自己上毒:都不判抵抗(不同施法者各占一条,所以是两次 BUFF_ADD)
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kPlayerB, kSkillPoison));
    engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_SKILL, kPlayerB, kSkillPoison));
    const auto result = engine.ResolveCurrentRound();

    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_RESIST), 0);
    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_ADD), 2);
    const auto* teammateState = FindStateActor(result.state(), kPlayerB);
    ASSERT_NE(teammateState, nullptr);
    EXPECT_EQ(teammateState->buffs_size(), 2);
}

TEST(TurnBattleEngineTest, CombatAilmentResistIsPerAilmentType) {
    struct AilmentCase {
        const char* name;
        uint32_t buffTableId;
        void (*setResist)(CombatAttributes& combat, uint64_t percent);
    };
    // 映射(turn_battle_constants.h 的 kAilmentBuffMappings):中毒 → 抗中毒,冰冻 → 抗冰冻,
    // 眩晕 → 抗昏睡,沉默 → 抗遗忘
    const AilmentCase cases[] = {
        {"poison", kBuffPoison,
         [](CombatAttributes& combat, uint64_t percent) { combat.set_resist_poison(percent); }},
        {"freeze", kBuffAilmentFreeze,
         [](CombatAttributes& combat, uint64_t percent) { combat.set_resist_freeze(percent); }},
        {"stun", kBuffAilmentStun,
         [](CombatAttributes& combat, uint64_t percent) { combat.set_resist_sleep(percent); }},
        {"silence", kBuffSilence,
         [](CombatAttributes& combat, uint64_t percent) { combat.set_resist_forget(percent); }},
    };

    // B 带着 combat 开一局,A(敌方)直接给 B 挂 buffTableId,返回过程中的事件
    const auto applyFromEnemy = [](const CombatAttributes& defenderCombat, uint32_t buffTableId) {
        TurnBattleEngine engine(MakeCombatProvider());
        auto request = MakeRequest(9553, kMatchModePvp, 1);
        AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 0, 0, 120);
        auto* defender = AddPlayer(request, kPlayerB, 1, 1000, 1000, 0, 0, 0, 60);
        *defender->mutable_combat() = defenderCombat;
        EXPECT_TRUE(engine.Initialize(request));
        return TurnBattleEngineDeathTestAccess::AddBuffWithEvents(engine, kPlayerB, buffTableId,
                                                                  kPlayerA);
    };

    // 单项抗性只挡自己那一种异常
    for (const auto& resistCase : cases) {
        CombatAttributes combat;
        resistCase.setResist(combat, 100);
        for (const auto& buffCase : cases) {
            SCOPED_TRACE(std::string("resist=") + resistCase.name + " buff=" + buffCase.name);
            const bool expectResisted = resistCase.buffTableId == buffCase.buffTableId;
            const auto result = applyFromEnemy(combat, buffCase.buffTableId);
            EXPECT_EQ(CountEvents(result, BATTLE_EVENT_RESIST), expectResisted ? 1 : 0);
            EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_ADD), expectResisted ? 0 : 1);
        }
    }

    // 所有抗异常挡全部四种;灼烧不在异常清单里,照挂
    {
        CombatAttributes combat;
        combat.set_resist_all_ailment(100);
        for (const auto& buffCase : cases) {
            SCOPED_TRACE(std::string("resist=all buff=") + buffCase.name);
            EXPECT_EQ(CountEvents(applyFromEnemy(combat, buffCase.buffTableId), BATTLE_EVENT_RESIST), 1);
        }
        const auto burnResult = applyFromEnemy(combat, kBuffBurn);
        EXPECT_EQ(CountEvents(burnResult, BATTLE_EVENT_RESIST), 0);
        EXPECT_EQ(CountEvents(burnResult, BATTLE_EVENT_BUFF_ADD), 1);
    }

    // 抗混乱:引擎没有混乱 buff,暂无消费点 —— 四种异常一种都挡不住
    {
        CombatAttributes combat;
        combat.set_resist_confusion(100);
        for (const auto& buffCase : cases) {
            SCOPED_TRACE(std::string("resist=confusion buff=") + buffCase.name);
            EXPECT_EQ(CountEvents(applyFromEnemy(combat, buffCase.buffTableId), BATTLE_EVENT_RESIST), 0);
        }
    }
}

TEST(TurnBattleEngineTest, CombatResistedAilmentDoesNotDispelExistingBuffs) {
    // B 身上先有一层毒;A 再挂"眩晕 + 驱散毒 tag"。resistSleep = 100 时眩晕被抵抗,毒必须原样留着
    for (const uint64_t resistSleep : {uint64_t{0}, uint64_t{100}}) {
        SCOPED_TRACE(resistSleep);
        TurnBattleEngine engine(MakeCombatProvider());
        auto request = MakeRequest(9554, kMatchModePvp, 1);
        AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 0, 0, 120);
        auto* defender = AddPlayer(request, kPlayerB, 1, 1000, 1000, 0, 0, 0, 60);
        defender->mutable_combat()->set_resist_sleep(resistSleep);
        ASSERT_TRUE(engine.Initialize(request));
        ASSERT_TRUE(TurnBattleEngineDeathTestAccess::AddBuff(engine, kPlayerB, kBuffPoison, kPlayerA));

        const auto result = TurnBattleEngineDeathTestAccess::AddBuffWithEvents(
            engine, kPlayerB, kBuffStunWithDispel, kPlayerA);
        const auto state = engine.BuildStateSnapshot();
        const auto* defenderState = FindStateActor(state, kPlayerB);
        ASSERT_NE(defenderState, nullptr);
        ASSERT_EQ(defenderState->buffs_size(), 1);
        if (resistSleep == 100) {
            EXPECT_EQ(CountEvents(result, BATTLE_EVENT_RESIST), 1);
            EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_REMOVE), 0);
            EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_ADD), 0);
            EXPECT_EQ(defenderState->buffs(0).buff_table_id(), kBuffPoison);
        } else {
            // 对照组:没抵抗 → 毒被驱散、眩晕挂上
            EXPECT_EQ(CountEvents(result, BATTLE_EVENT_RESIST), 0);
            EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_REMOVE), 1);
            EXPECT_EQ(CountEvents(result, BATTLE_EVENT_BUFF_ADD), 1);
            EXPECT_EQ(defenderState->buffs(0).buff_table_id(), kBuffStunWithDispel);
        }
    }
}

TEST(TurnBattleEngineTest, CombatPartialAilmentResistRollsDeterministically) {
    // 抵抗率 50:走引擎 RNG。同种子必同结果;不同种子两种结果都要出现过(否则说明根本没掷骰)
    const auto resistedWithSeed = [](uint64_t seed) {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9555, kMatchModePvp, seed);
        AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 0, 0, 120);
        auto* defender = AddPlayer(request, kPlayerB, 1, 1000, 1000, 0, 0, 0, 60);
        defender->mutable_combat()->set_resist_poison(50);
        EXPECT_TRUE(engine.Initialize(request));
        const auto result =
            ResolveDuelRound(engine, MakeAction(BATTLE_ACTION_SKILL, kPlayerB, kSkillPoison));
        const int resisted = CountEvents(result, BATTLE_EVENT_RESIST);
        // 要么被抵抗、要么挂上,二者恰居其一
        EXPECT_EQ(resisted + CountEvents(result, BATTLE_EVENT_BUFF_ADD), 1);
        return resisted == 1;
    };

    int resistedCount = 0;
    constexpr int kSeedCount = 40;
    for (int seed = 1; seed <= kSeedCount; ++seed) {
        const bool resisted = resistedWithSeed(static_cast<uint64_t>(seed));
        EXPECT_EQ(resisted, resistedWithSeed(static_cast<uint64_t>(seed)));
        resistedCount += resisted ? 1 : 0;
    }
    // 40 个种子全落在同一侧的概率约 2^-39;真出现就说明抵抗率没有被当成 50% 在掷
    EXPECT_GT(resistedCount, 0);
    EXPECT_LT(resistedCount, kSeedCount);
}

// ---- 确定性:零概率不消耗随机数 ----

TEST(TurnBattleEngineTest, CombatZeroProbabilityRollsConsumeNoRandomNumbers) {
    enum class Variant {
        kNoCombat,       // 快照不带 combat(装备系统落地前的输入)
        kEmptyCombat,    // 带了子消息但全 0(scene 对无装备玩家的实际输出)
        kIrrelevant,     // 只带本场用不上的属性
        kComboAttacker,  // 敏感性对照:出手者真带 50% 连击,随机序列必然被多消耗
    };
    static constexpr int kRounds = 6;  // static:下面不带捕获的 lambda 里也要用

    // A 每回合普攻 B,B 每回合防御。双方基础暴击 50:每次普攻恰好消耗 1 个随机数(必杀掷骰),
    // 此外本场没有任何随机消费(目标有效不重选、无逃跑、未分胜负不掷掉落)。
    const auto runDuel = [](uint64_t seed, Variant variant, uint64_t& nextRandom) {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9560, kMatchModePvp, seed);
        auto* attacker = AddPlayer(request, kPlayerA, 0, 100000, 100000, 4, 0, /*crit*/50, 120);
        auto* defender = AddPlayer(request, kPlayerB, 1, 100000, 100000, 4, 0, /*crit*/50, 60);
        if (variant == Variant::kEmptyCombat) {
            attacker->mutable_combat();
            defender->mutable_combat();
        } else if (variant == Variant::kIrrelevant) {
            // 没人打 A:A 的反震 / 反击 / 两种抗性都用不上;A 只普攻:法术必杀、技能等级用不上;没人给 A 上 buff:抗异常用不上
            auto* attackerCombat = attacker->mutable_combat();
            attackerCombat->set_reflect_rate(50);
            attackerCombat->set_counter_rate(50);
            attackerCombat->set_physical_resist(30);
            attackerCombat->set_magic_resist(30);
            attackerCombat->set_magic_crit_rate(50);
            attackerCombat->set_skill_level_bonus(3);
            attackerCombat->set_resist_poison(50);
            attackerCombat->set_resist_all_ailment(50);
            // B 只防御不出手:连击、两种必杀、忽视抗异常都用不上;A 的普攻是物理:B 的抗法术用不上
            auto* defenderCombat = defender->mutable_combat();
            defenderCombat->set_combo_rate(50);
            defenderCombat->set_physical_crit_rate(50);
            defenderCombat->set_magic_crit_rate(50);
            defenderCombat->set_ignore_ailment_resist(50);
            defenderCombat->set_magic_resist(30);
            defenderCombat->set_resist_freeze(50);
            defenderCombat->set_resist_confusion(50);
        } else if (variant == Variant::kComboAttacker) {
            attacker->mutable_combat()->set_combo_rate(50);
        }
        EXPECT_TRUE(engine.Initialize(request));

        std::string stream;
        for (int round = 0; round < kRounds; ++round) {
            const auto result = ResolveDuelRound(engine, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));
            for (const auto& event : result.events()) {
                stream += event.SerializeAsString();
                stream += '|';
            }
        }
        EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_ONGOING);
        stream += engine.BuildSettlement(kPlayerA).SerializeAsString();
        stream += '|';
        stream += engine.BuildSettlement(kPlayerB).SerializeAsString();
        nextRandom = TurnBattleEngineDeathTestAccess::PeekNextRandom(engine);
        return stream;
    };

    for (const uint64_t seed : {uint64_t{42}, uint64_t{20261007}}) {
        SCOPED_TRACE(seed);
        uint64_t baselineNext = 0;
        const auto baseline = runDuel(seed, Variant::kNoCombat, baselineNext);

        // 全 0 路径的随机数消耗量与装备系统落地前相同:kRounds 次普攻 = kRounds 个随机数,一个不多。
        // 用独立的 mt19937_64 推出"第 kRounds + 1 个输出"来对账,不依赖引擎自己和自己比
        std::mt19937_64 reference(seed);
        reference.discard(kRounds);
        EXPECT_EQ(baselineNext, reference());

        uint64_t emptyNext = 0;
        EXPECT_EQ(runDuel(seed, Variant::kEmptyCombat, emptyNext), baseline);
        EXPECT_EQ(emptyNext, baselineNext);

        // 只带与本场无关的属性:事件流逐字节相同,随机序列一步不差
        uint64_t irrelevantNext = 0;
        EXPECT_EQ(runDuel(seed, Variant::kIrrelevant, irrelevantNext), baseline);
        EXPECT_EQ(irrelevantNext, baselineNext);

        // 对照:真带连击率的出手者每回合至少多掷一次,探针必须能看出来(否则上面的相等没有说服力)
        uint64_t comboNext = 0;
        runDuel(seed, Variant::kComboAttacker, comboNext);
        EXPECT_NE(comboNext, baselineNext);
    }
}

TEST(TurnBattleEngineTest, CombatAllZeroEventsCarryOnlyPreEquipmentFields) {
    // 既有的确定性用例都是"引擎自己和自己比",守不住"新字段悄悄上线"这一类变化
    // (例如普通出手被标了非 0 的 hit_kind、或 hit_kind 被改成带 presence 的 optional)。
    // 这里用手工拼出的事件当基线:只填装备系统落地前就有的字段,逐字节相等才算没动旧布局。
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9561, kMatchModePvp, 1);
    auto* attacker = AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 0, 0, 120);
    attacker->set_physical_attack(1004);
    AddPlayer(request, kPlayerB, 1, 1000, 1000, 0, 0, 0, 60);
    ASSERT_TRUE(engine.Initialize(request));

    // 暴击率 0、目标有效:本回合不消耗任何随机数,事件内容完全由公式决定
    const auto result = ResolveDuelRound(engine, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));
    ASSERT_EQ(result.events_size(), 3);

    BattleEventItem expectedAttack;
    expectedAttack.set_event_type(BATTLE_EVENT_ATTACK);
    expectedAttack.set_source_id(kPlayerA);
    expectedAttack.set_target_id(kPlayerB);
    expectedAttack.set_group_id(1);
    EXPECT_EQ(result.events(0).SerializeAsString(), expectedAttack.SerializeAsString());

    BattleEventItem expectedDamage;
    expectedDamage.set_event_type(BATTLE_EVENT_DAMAGE);
    expectedDamage.set_source_id(kPlayerA);
    expectedDamage.set_target_id(kPlayerB);
    expectedDamage.set_value(305);  // 1014 × 0.3 = 304.2 → 305
    expectedDamage.set_target_health_after(1000 - 305);
    expectedDamage.set_group_id(1);
    EXPECT_EQ(result.events(1).SerializeAsString(), expectedDamage.SerializeAsString());

    // 没有反击:B 自己的行动紧接着就是下一组
    BattleEventItem expectedDefend;
    expectedDefend.set_event_type(BATTLE_EVENT_DEFEND);
    expectedDefend.set_source_id(kPlayerB);
    expectedDefend.set_target_id(kPlayerB);
    expectedDefend.set_group_id(2);
    EXPECT_EQ(result.events(2).SerializeAsString(), expectedDefend.SerializeAsString());
}

TEST(TurnBattleEngineTest, CombatUnequippedPveRoundConsumesOnlyTargetSelectionRandoms) {
    // PVE:无装备玩家 + 宝宝 + 默认怪,三者的战斗类属性都是全 0(宝宝 / 怪物连子消息都没有)。
    // 每回合的随机消费只有两次默认行动的选目标:宝宝选怪(候选只有 1 个也掷一次 RandIndex)、
    // 怪在玩家与宝宝之间选。暴击率全 0 不掷;连击 / 反震 / 反击的判定一次都不该消耗随机数。
    constexpr uint64_t kSeed = 20261008;
    constexpr int kPveRounds = 5;  // 每回合怪物掉 15 + 14 点,5 回合打不死(300 血)
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9562, turnbattle::kMatchModePveSolo, kSeed);
    auto* owner = AddPlayer(request, kPlayerA, 0, 1000, 1000, 5, 0, 0, 120);
    AddPet(owner, kPetA, 400, 400, 4, 360);
    ASSERT_TRUE(engine.Initialize(request));

    for (int round = 0; round < kPveRounds; ++round) {
        ASSERT_TRUE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId)));
        const auto result = engine.ResolveCurrentRound();
        // 宝宝、玩家、怪物各出手一次,全是普通段:没有追加段、没有反震、没有还手
        EXPECT_EQ(CountEvents(result, BATTLE_EVENT_ATTACK), 3);
        EXPECT_EQ(CountEvents(result, BATTLE_EVENT_DAMAGE), 3);
        EXPECT_EQ(CountEventsOfKind(result, BATTLE_EVENT_ATTACK, BATTLE_HIT_NORMAL), 3);
        EXPECT_EQ(CountEventsOfKind(result, BATTLE_EVENT_DAMAGE, BATTLE_HIT_NORMAL), 3);
        EXPECT_EQ(CountEvents(result, BATTLE_EVENT_RESIST), 0);
    }
    ASSERT_EQ(engine.Outcome(), BATTLE_OUTCOME_ONGOING);

    // 用独立的 mt19937_64 对账:恰好消耗了 2 × 回合数 个随机数
    std::mt19937_64 reference(kSeed);
    reference.discard(2 * kPveRounds);
    EXPECT_EQ(TurnBattleEngineDeathTestAccess::PeekNextRandom(engine), reference());
}

// ---- 确定性:新掷骰之间的随机数消费顺序 ----

TEST(TurnBattleEngineTest, CombatFractionalRollsFollowDocumentedRandomOrder) {
    // 前面的用例里连击 / 反震 / 反击的概率不是 0 就是 100,而 RollPercent 在这两端都不取随机数:
    // 事件的先后被断言了,随机数的消费顺序没有 —— 调换任意两步掷骰,那些用例照样全绿,
    // 带装备玩家的同种子回放却会整体平移。这里把相关概率全设成 50,用独立的 mt19937_64
    // 按 turn_battle_constants.h 写明的顺序逐回合预测,再与引擎的事件流对账:
    //   首段必杀 → 首段反震 → 连击续段判定 →(连击段必杀 → 连击段反震)→ 反击判定 →(反击那一下的必杀)
    // A 每回合普攻 B,B 每回合防御(B 更慢,防御落在 A 出手之后):目标有效不重选、命中率 100% 不掷,
    // 除上面这几步之外本场没有别的随机消费。
    constexpr uint64_t kSeedCount = 16;
    constexpr int kOrderRounds = 8;  // 每回合双方各掉十来点血,气血 100000 下谁都死不了

    // 一回合的事件流压成一串记号,对不上时一眼能看出是哪一步:
    //   A = 出手方的 ATTACK;N0 / N1 = 首段(未必杀 / 必杀);C0 / C1 = 连击段;R = 反震;
    //   K = 反击的 ATTACK;X0 / X1 = 反击那一下;F = DEFEND;? = 本场不该出现的事件
    const auto describeRound = [](const TurnResultS2C& result) {
        std::string tokens;
        for (const auto& event : result.events()) {
            const char criticalMark = event.is_critical() ? '1' : '0';
            if (event.event_type() == BATTLE_EVENT_ATTACK) {
                tokens += (event.hit_kind() == BATTLE_HIT_COUNTER ? 'K' : 'A');
            } else if (event.event_type() == BATTLE_EVENT_DEFEND) {
                tokens += 'F';
            } else if (event.event_type() != BATTLE_EVENT_DAMAGE) {
                tokens += '?';
            } else if (event.hit_kind() == BATTLE_HIT_REFLECT) {
                tokens += 'R';
            } else if (event.hit_kind() == BATTLE_HIT_COMBO) {
                tokens += 'C';
                tokens += criticalMark;
            } else if (event.hit_kind() == BATTLE_HIT_COUNTER) {
                tokens += 'X';
                tokens += criticalMark;
            } else {
                tokens += 'N';
                tokens += criticalMark;
            }
            tokens += ' ';
        }
        return tokens;
    };

    int totalRounds = 0;
    int criticalFirstHits = 0;
    int reflectedFirstHits = 0;
    int comboRounds = 0;
    int counterRounds = 0;
    for (uint64_t seed = 1; seed <= kSeedCount; ++seed) {
        SCOPED_TRACE(seed);
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9563, kMatchModePvp, seed);
        // 基础暴击 0:必杀率只来自装备加成那一份
        auto* attacker = AddPlayer(request, kPlayerA, 0, 100000, 100000, 0, 0, 0, 120);
        attacker->mutable_combat()->set_physical_crit_rate(50);
        attacker->mutable_combat()->set_combo_rate(50);
        auto* defender = AddPlayer(request, kPlayerB, 1, 100000, 100000, 0, 0, 0, 60);
        defender->mutable_combat()->set_physical_crit_rate(50);  // 只有反击那一下用得上
        defender->mutable_combat()->set_reflect_rate(50);
        defender->mutable_combat()->set_counter_rate(50);
        ASSERT_TRUE(engine.Initialize(request));

        // 与引擎同种子的独立随机源。换算照抄引擎的两种写法:
        //   Rand01 = 高 53 位 / 2^53;必杀 = Rand01 < 暴击率(0.5);其余判定 = Rand01 × 100 < 百分点(50)
        std::mt19937_64 reference(seed);
        const auto next01 = [&reference]() {
            return static_cast<double>(reference() >> 11) * (1.0 / 9007199254740992.0);
        };
        const auto rollCritical = [&next01]() { return next01() < 0.5; };
        const auto rollHalf = [&next01]() { return next01() * 100.0 < 50.0; };

        for (int round = 0; round < kOrderRounds; ++round) {
            SCOPED_TRACE(round);
            // 每一步各占一条语句:求值顺序就是随机数的消费顺序,不能合进同一个表达式
            std::string expected = "A ";
            const bool firstCritical = rollCritical();
            expected += (firstCritical ? "N1 " : "N0 ");
            const bool firstReflected = rollHalf();
            if (firstReflected) {
                expected += "R ";
            }
            const bool comboed = rollHalf();
            if (comboed) {
                // 连击段是独立的一段:自己的必杀、自己的反震,都排在反击判定之前
                const bool comboCritical = rollCritical();
                expected += (comboCritical ? "C1 " : "C0 ");
                const bool comboReflected = rollHalf();
                if (comboReflected) {
                    expected += "R ";
                }
            }
            const bool countered = rollHalf();
            if (countered) {
                const bool counterCritical = rollCritical();
                expected += "K ";
                expected += (counterCritical ? "X1 " : "X0 ");
            }
            expected += "F ";

            const auto result = ResolveDuelRound(engine, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));
            EXPECT_EQ(describeRound(result), expected);

            ++totalRounds;
            criticalFirstHits += firstCritical ? 1 : 0;
            reflectedFirstHits += firstReflected ? 1 : 0;
            comboRounds += comboed ? 1 : 0;
            counterRounds += countered ? 1 : 0;
        }
        ASSERT_EQ(engine.Outcome(), BATTLE_OUTCOME_ONGOING);
        // 个数也要对上:预测用掉几个随机数,引擎就恰好用掉几个
        EXPECT_EQ(TurnBattleEngineDeathTestAccess::PeekNextRandom(engine), reference());
    }

    // 每种判定的"成立"与"不成立"都必须走到过,否则上面的对账没有覆盖到全部掷骰点
    // (独立脚本按同一顺序算出 128 回合里分别是 66 / 74 / 73 / 64 次)
    ASSERT_EQ(totalRounds, static_cast<int>(kSeedCount) * kOrderRounds);
    EXPECT_GT(criticalFirstHits, 0);
    EXPECT_LT(criticalFirstHits, totalRounds);
    EXPECT_GT(reflectedFirstHits, 0);
    EXPECT_LT(reflectedFirstHits, totalRounds);
    EXPECT_GT(comboRounds, 0);
    EXPECT_LT(comboRounds, totalRounds);
    EXPECT_GT(counterRounds, 0);
    EXPECT_LT(counterRounds, totalRounds);
}

// ---- 下发前清洗 ----

TEST(TurnBattleEngineTest, StripEngineOnlyStateClearsCombatWithoutTouchingEngine) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9570, kMatchModePvp, 1);
    auto* attacker = AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 0, 0, 120);
    attacker->mutable_combat()->set_combo_rate(100);
    auto* defender = AddPlayer(request, kPlayerB, 1, 1000, 1000, 0, 0, 0, 60);
    defender->mutable_combat()->set_reflect_rate(100);
    ASSERT_TRUE(engine.Initialize(request));

    // 开局全量(开战包 / 重连补拉 / 观战首帧同源):引擎出的是"全知"版本,清洗后任何单位都不带 combat
    auto opening = engine.BuildStateSnapshot();
    const auto* openingAttacker = FindStateActor(opening, kPlayerA);
    ASSERT_NE(openingAttacker, nullptr);
    ASSERT_TRUE(openingAttacker->has_combat());
    TurnBattleEngine::StripEngineOnlyState(opening);
    ASSERT_EQ(opening.actors_size(), 2);
    for (const auto& actor : opening.actors()) {
        EXPECT_FALSE(actor.has_combat());
        // 其余字段原样保留
        EXPECT_EQ(actor.attributes().health(), 1000u);
        EXPECT_EQ(actor.max_health(), 1000u);
    }
    EXPECT_EQ(opening.battle_id(), 9570u);
    EXPECT_EQ(opening.pending_actor_ids_size(), 2);

    // 回合结果里携带的 state 同样要清
    auto result = ResolveDuelRound(engine, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));
    TurnBattleEngine::StripEngineOnlyState(*result.mutable_state());
    ASSERT_EQ(result.state().actors_size(), 2);
    for (const auto& actor : result.state().actors()) {
        EXPECT_FALSE(actor.has_combat());
    }

    // 清的只是出站拷贝:引擎内部状态不受影响,下一回合连击 / 反震照常生效
    const auto afterStrip = engine.BuildStateSnapshot();
    const auto* stillEquipped = FindStateActor(afterStrip, kPlayerA);
    ASSERT_NE(stillEquipped, nullptr);
    EXPECT_EQ(stillEquipped->combat().combo_rate(), 100u);
    const auto second = ResolveDuelRound(engine, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));
    EXPECT_EQ(CountEventsOfKind(second, BATTLE_EVENT_DAMAGE, BATTLE_HIT_COMBO), 1);
    EXPECT_EQ(CountEventsOfKind(second, BATTLE_EVENT_DAMAGE, BATTLE_HIT_REFLECT), 2);
}
