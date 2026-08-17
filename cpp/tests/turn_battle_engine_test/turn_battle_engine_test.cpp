#include <gtest/gtest.h>

#include <memory>
#include <string>

#include "constants/turn_battle_constants.h"
#include "memory_battle_data_provider.h"
#include "system/turn_battle_engine.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/skill_error_tip.pb.h"

// 回合制战斗引擎单元测试(设计文档 §5.1)。
//
// 表管理器依赖用 MemoryBattleDataProvider 注入隔离,不依赖 Excel 数据与配置加载;
// 覆盖:确定性事件流 / 速度序 / 超时默认行动 / 冷却回合 / buff 到期·叠层·周期·驱散·免疫 /
// 沉默许可 / 胜负边界(全灭·打满·平局)/ 结算数值 / FLEE·DEFEND·ITEM 路径。

namespace {

using turnbattle::TurnBattleEngine;

constexpr uint64_t kPlayerA = 5001;
constexpr uint64_t kPlayerB = 5002;
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
        AddPlayer(request, kPlayerA, 0, 1000, 1000, 4, 100, 50, 10);
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

// ---------------------------------------------------------------------------
// 速度序:速度降序结算,同速按 actor_id 升序
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, TurnOrderIsSpeedDescending) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9002, 3 /* PVP 1v1 */, 1);
    AddPlayer(request, kPlayerA, 0, 500, 500, 0, 100, 0, 10);
    AddPlayer(request, kPlayerB, 1, 500, 500, 0, 100, 0, 20);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));
    ASSERT_TRUE(engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_ATTACK, kPlayerA)));

    const auto result = engine.ResolveCurrentRound();
    ASSERT_GE(result.events_size(), 1);
    EXPECT_EQ(result.events(0).event_type(), BATTLE_EVENT_ATTACK);
    EXPECT_EQ(result.events(0).source_id(), kPlayerB);  // 速度 20 先手
}

TEST(TurnBattleEngineTest, TurnOrderTieBreaksByActorIdAscending) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9003, 3, 1);
    AddPlayer(request, kPlayerA, 0, 500, 500, 0, 100, 0, 10);
    AddPlayer(request, kPlayerB, 1, 500, 500, 0, 100, 0, 10);
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
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 4, 100, 0, 10);
    ASSERT_TRUE(engine.Initialize(request));

    // 玩家不提交任何行动,直接结算(等价回合超时)
    const auto result = engine.ResolveCurrentRound();

    EXPECT_EQ(CountEvents(result, BATTLE_EVENT_ATTACK), 2);  // 玩家默认普攻 + 怪物普攻
    ASSERT_GE(result.events_size(), 1);
    // 玩家速度 10 > 怪物默认速度,先手是玩家的默认普攻
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
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 10);
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
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 10);
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
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 10);
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
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 10);
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
    AddPlayer(request, kPlayerA, 0, 500, 500, 0, 100, 0, 10);
    auto* defender = AddPlayer(request, kPlayerB, 1, 500, 500, 0, 100, 0, 5);
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
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 100, 200, 0, 100, 0, 10);
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
    EXPECT_EQ(playerState->attributes().health(), 125u);
}

// ---------------------------------------------------------------------------
// 沉默:SkillPermission 按技能类型格值放行/拦截
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, SilenceBlocksGeneralSkillButAllowsBasicAttack) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9011, turnbattle::kMatchModePveSolo, 1);
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 10);
    auto* silenceEntry = snapshot->add_buffs();
    silenceEntry->set_buff_id(902);
    silenceEntry->set_buff_table_id(kBuffSilence);
    silenceEntry->set_layer(1);
    silenceEntry->set_remain_rounds(2);
    ASSERT_TRUE(engine.Initialize(request));

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
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 10);
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
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 10);
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
    AddPlayer(request, kPlayerA, 0, 15, 15, 0, 100, 0, 10);
    AddPlayer(request, kPlayerB, 1, 15, 15, 0, 100, 0, 5);
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
    // strength=4,无暴击;怪物默认 armor=2、resistance=0
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 4, 100, 0, 10);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillDamage));
    const auto result = engine.ResolveCurrentRound();

    // 50 * (1 + 4*0.1) = 70;70 - 2 = 68;68 * (1 - 0) = 68
    const auto* damageEvent = FindFirstEvent(result, BATTLE_EVENT_DAMAGE);
    ASSERT_NE(damageEvent, nullptr);
    EXPECT_EQ(damageEvent->value(), 68u);
    EXPECT_FALSE(damageEvent->is_critical());
    EXPECT_EQ(damageEvent->target_health_after(),
              turnbattle::kMonsterDefaultHealth - 68);

    const auto* monster = FindStateActor(result.state(), kMonsterId);
    ASSERT_NE(monster, nullptr);
    EXPECT_EQ(monster->attributes().health(), turnbattle::kMonsterDefaultHealth - 68);
}

// ---------------------------------------------------------------------------
// FLEE / ITEM 路径
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, FleeIsDeterministicAndConsistentWithOutcome) {
    const auto runFlee = [](uint64_t seed) {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9016, turnbattle::kMatchModePveSolo, seed);
        AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 50);  // 高速,逃跑成功率高
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
    AddPlayer(request, kPlayerA, 0, 500, 500, 0, 100, 0, 10);
    AddPlayer(request, kPlayerB, 1, 500, 500, 0, 100, 0, 5);
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
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 50, 200, 0, 100, 0, 10);
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

    // 第 2 次使用:150 → 200(封顶),回 50
    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ITEM, kPlayerA, 0, kItemPotion));
    result = engine.ResolveCurrentRound();
    const auto* secondItemEvent = FindFirstEvent(result, BATTLE_EVENT_ITEM);
    ASSERT_NE(secondItemEvent, nullptr);
    EXPECT_EQ(secondItemEvent->value(), 50u);

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
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 10);
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

int main(int argc, char** argv) {
    ::testing::InitGoogleTest(&argc, argv);
    return RUN_ALL_TESTS();
}
