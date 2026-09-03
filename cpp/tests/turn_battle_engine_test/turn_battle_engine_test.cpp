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
// 沉默许可 / 胜负边界(全灭·打满·平局)/ 结算数值 / FLEE·DEFEND·ITEM 路径 /
// 二期:自动战斗(SetActorAuto·就绪·快照排除·确定性回归)/ 队伍人数上限(设计文档 §11)。

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

// ---------------------------------------------------------------------------
// 二期:自动战斗(设计文档 §11 D12)
// ---------------------------------------------------------------------------

TEST(TurnBattleEngineTest, AutoActorCountsAsReadyAndActsWithDefaultAttack) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9020, turnbattle::kMatchModePveSolo, 1);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 4, 100, 0, 10);
    ASSERT_TRUE(engine.Initialize(request));

    // 未提交且未挂机:不就绪;开挂机后无需提交即就绪(提前结算的判据)
    EXPECT_FALSE(engine.AllPlayersReady());
    ASSERT_EQ(engine.SetActorAuto(kPlayerA, true), 0u);
    EXPECT_TRUE(engine.AllPlayersReady());

    // 结算走默认普攻代打:玩家速度 10 > 怪物默认 5,首事件是玩家对怪的普攻
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
        AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 10);
        ASSERT_TRUE(engine.Initialize(request));
        EXPECT_EQ(engine.SetActorAuto(kMonsterId, true), static_cast<uint32_t>(kInvalidParameter));
        EXPECT_EQ(engine.SetActorAuto(999999u, true), static_cast<uint32_t>(kInvalidParameter));
    }

    // 死亡玩家:1v2 秒掉 B 后战斗仍进行中,B 被拒,存活的 C 可开
    {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9022, 3 /* PVP */, 1);
        AddPlayer(request, kPlayerA, 0, 500, 500, 0, 100, 0, 10);
        AddPlayer(request, kPlayerB, 1, 500, 500, 0, 0, 0, 5);  // 无甲,一击可杀
        AddPlayer(request, kPlayerC, 1, 500, 500, 0, 100, 0, 5);
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
            AddPlayer(request, kPlayerA, 0, 1000, 1000, 0, 100, 0, 50);  // 高速,逃跑成功率高
            AddPlayer(request, kPlayerB, 0, 1000, 1000, 0, 100, 0, 10);
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
        AddPlayer(request, kPlayerA, 0, 1000, 1000, 4, 100, 50, 10);
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
    AddPlayer(request, kPlayerA, 0, 500, 500, 0, 100, 0, 10);
    AddPlayer(request, kPlayerB, 1, 500, 500, 0, 100, 0, 5);
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
            AddPlayer(request, kPlayerA + offset, 0, 1000, 1000, 0, 100, 0, 10);
        }
        EXPECT_FALSE(engine.Initialize(request));
    }

    // 单队 5 人:恰在上限,放行
    {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9027, turnbattle::kMatchModePveTeam, 1);
        for (uint64_t offset = 0; offset < turnbattle::kMaxBattleTeamSize; ++offset) {
            AddPlayer(request, kPlayerA + offset, 0, 1000, 1000, 0, 100, 0, 10);
        }
        EXPECT_TRUE(engine.Initialize(request));
    }
}

// 2026-09-02:怪物属性从 MonsterTable 读入(不再写死 300 常量)→ PVE 多回合;
// 玩家击杀怪物后结算累加 MonsterTable.exp_reward/gold_reward。
TEST(TurnBattleEngineTest, MonsterAttributesFromTableEnableMultiRoundAndRewards) {
    auto provider = MakeProvider();
    // 表配怪物:200 HP、护甲 5、速度 8(慢于玩家),奖励经验 50 金币 25。
    constexpr uint32_t kMonsterTableId = 7000;
    auto& monster = provider->AddMonster(kMonsterTableId);
    monster.set_health(200);
    monster.set_strength(10);
    monster.set_armor(5);
    monster.set_speed(8);
    monster.set_exp_reward(50);
    monster.set_gold_reward(25);
    provider->SetDungeonMonsters(kDungeonConfig, {kMonsterTableId});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9030, turnbattle::kMatchModePveSolo, 7);
    // 玩家:1000 HP、力量 20(普攻 10*(1+2.0)-5=25)、速度 20(先手)。
    AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/20, /*armor*/10, /*crit*/0, /*speed*/20);
    ASSERT_TRUE(engine.Initialize(request));

    // 怪物从表读到 200 HP(不是常量 300),证明读表生效
    const auto initState = engine.BuildStateSnapshot();
    const auto* monsterActor = FindStateActor(initState, kMonsterId);
    ASSERT_NE(monsterActor, nullptr);
    EXPECT_EQ(monsterActor->attributes().health(), 200u);
    EXPECT_EQ(monsterActor->max_health(), 200u);

    // 玩家逐回合普攻,战斗应持续多回合(200/25≈8 回合),不再一击秒杀
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
    boss.set_speed(100);         // 先手
    boss.set_exp_reward(9999);
    boss.set_gold_reward(9999);
    provider->SetDungeonMonsters(kDungeonConfig, {kBossTableId});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9031, turnbattle::kMatchModePveSolo, 7);
    AddPlayer(request, kPlayerA, 0, 50, 50, 5, 0, 0, 1);  // 脆弱、后手
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
    // 基线同 DamageFormulaMatchesRealtimeSemantics(strength=4、怪物默认 armor=2、resistance=0),
    // 再叠属性加点二级属性 physical_attack=50:
    //   普攻 10 * (1 + 4*0.1) + 50 = 64;64 - 2(armor) - 0(defense) = 62
    // speed=50 > 怪物默认 5,玩家先手,首个伤害事件即玩家普攻。
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/4, /*armor*/0, /*crit*/0, /*speed*/50);
    snapshot->set_physical_attack(50);
    snapshot->set_magic_attack(999);  // 普攻只吃物伤,法伤不得混入
    ASSERT_TRUE(engine.Initialize(request));

    ASSERT_TRUE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId)));
    const auto result = engine.ResolveCurrentRound();

    const auto* damageEvent = FindFirstEvent(result, BATTLE_EVENT_DAMAGE);
    ASSERT_NE(damageEvent, nullptr);
    EXPECT_EQ(damageEvent->source_id(), kPlayerA);
    EXPECT_EQ(damageEvent->value(), 62u);
}

TEST(TurnBattleEngineTest, DerivedMagicAttackIsAdditiveOnSkill) {
    TurnBattleEngine engine(MakeProvider());
    auto request = MakeRequest(9202, turnbattle::kMatchModePveSolo, 1);
    // 同 DamageFormulaMatchesRealtimeSemantics 的技能基线(50 * 1.4 - 2 = 68),叠 magic_attack=20:
    //   50 * (1 + 4*0.1) + 20 = 90;90 - 2 = 88
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/4, /*armor*/100, /*crit*/0, /*speed*/10);
    snapshot->set_magic_attack(20);
    snapshot->set_physical_attack(999);  // 技能只吃法伤
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_SKILL, kMonsterId, kSkillDamage));
    const auto result = engine.ResolveCurrentRound();

    const auto* damageEvent = FindFirstEvent(result, BATTLE_EVENT_DAMAGE);
    ASSERT_NE(damageEvent, nullptr);
    EXPECT_EQ(damageEvent->value(), 88u);
}

TEST(TurnBattleEngineTest, DerivedDefenseReducesIncomingDamageAdditively) {
    // 玩家 speed=1 < 怪物默认 5:怪物先手。怪物普攻 10 * (1 + 5*0.1) = 15,
    // 玩家 armor=0、resistance=0,故受伤 = 15 - defense。defense=5 → 10。
    auto makeRun = [](uint64_t defense) {
        TurnBattleEngine engine(MakeProvider());
        auto request = MakeRequest(9203, turnbattle::kMatchModePveSolo, 1);
        auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/4, /*armor*/0, /*crit*/0, /*speed*/1);
        snapshot->set_defense(defense);
        EXPECT_TRUE(engine.Initialize(request));
        EXPECT_TRUE(engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId)));
        return engine.ResolveCurrentRound();
    };

    const auto without = makeRun(0);
    const auto with = makeRun(5);

    const auto* plain = FindStateActor(without.state(), kPlayerA);
    const auto* guarded = FindStateActor(with.state(), kPlayerA);
    ASSERT_NE(plain, nullptr);
    ASSERT_NE(guarded, nullptr);
    // defense=0 时公式与老口径逐字节一致(15 点),defense=5 时正好少挨 5 点
    EXPECT_EQ(plain->attributes().health(), 985u);
    EXPECT_EQ(guarded->attributes().health(), 990u);
    EXPECT_EQ(guarded->defense(), 5u);
}

TEST(TurnBattleEngineTest, MonsterRowWithoutStatsFallsBackToDefaults) {
    auto provider = MakeProvider();
    constexpr uint32_t kBareMonster = 7002;
    provider->AddMonster(kBareMonster);  // 只有 id,其余字段全 0
    provider->SetDungeonMonsters(kDungeonConfig, {kBareMonster});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9040, turnbattle::kMatchModePveSolo, 11);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/20, /*armor*/10, /*crit*/0, /*speed*/20);
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

    // 回退常量必须给出一个「真能打」的对手:玩家普攻 10*(1+2.0)-2=28,300 血要 11 刀。
    // 只断言 Outcome==ONGOING 是恒真的(刚 Initialize 完必然如此),抓不到任何回归。
    int rounds = 0;
    while (engine.Outcome() == BATTLE_OUTCOME_ONGOING && rounds < 30) {
        engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kMonsterId));
        engine.ResolveCurrentRound();
        ++rounds;
    }
    EXPECT_EQ(engine.Outcome(), BATTLE_OUTCOME_SIDE_A_WIN);
    EXPECT_EQ(rounds, 11) << "回退怪物 300 血/护甲 2,应打满 11 回合而不是开局即结束";
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
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 20, 10, 0, 20);
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
    x.set_speed(3);
    x.set_exp_reward(10);
    x.set_gold_reward(5);
    auto& y = provider->AddMonster(kMonsterY);
    y.set_health(60);
    y.set_strength(1);
    y.set_speed(2);
    y.set_exp_reward(15);
    y.set_gold_reward(7);
    provider->SetDungeonMonsters(kDungeonConfig, {kMonsterX, kMonsterY});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9042, turnbattle::kMatchModePveSolo, 13);
    // 玩家力量 20 → 普攻 10*(1+2.0)=30(怪物护甲 0),每只 2 刀;怪物力量 1 打不穿护甲 10
    AddPlayer(request, kPlayerA, 0, 1000, 1000, 20, 10, 0, 20);
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
}

// 胜方里逃跑的玩家不发奖:结算条件是 SIDE_A_WIN && team_index==0 && !fled && !is_dead。
// 只测 outcome 分支不够 —— 队伍打赢但自己中途跑了,照发奖就是刷奖漏洞。
TEST(TurnBattleEngineTest, FledPlayerOnWinningTeamGetsNoReward) {
    auto provider = MakeProvider();
    constexpr uint32_t kRewardMonster = 7006;
    auto& monster = provider->AddMonster(kRewardMonster);
    monster.set_health(150);
    monster.set_strength(1);   // 打不穿玩家护甲,不会误杀
    monster.set_speed(1);      // 最慢:B 的逃跑成功率被夹到上限 0.95
    monster.set_exp_reward(40);
    monster.set_gold_reward(20);
    provider->SetDungeonMonsters(kDungeonConfig, {kRewardMonster});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9043, turnbattle::kMatchModePveTeam, 21);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/20, /*armor*/10, /*crit*/0, /*speed*/30);
    AddPlayer(request, kPlayerB, 0, 1000, 1000, /*strength*/0, /*armor*/10, /*crit*/0, /*speed*/120);
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
    monster.set_speed(1);
    monster.set_exp_reward(60);
    monster.set_gold_reward(30);
    provider->SetDungeonMonsters(kDungeonConfig, {kBruiserMonster});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9044, turnbattle::kMatchModePveTeam, 33);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/20, /*armor*/10, /*crit*/0, /*speed*/30);
    AddPlayer(request, kPlayerB, 0, 1, 1, /*strength*/0, /*armor*/0, /*crit*/0, /*speed*/2);
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
    monster.set_speed(1);
    monster.set_exp_reward(80);
    monster.set_gold_reward(40);
    provider->SetDungeonMonsters(kDungeonConfig, {kSharedMonster});

    TurnBattleEngine engine(provider);
    auto request = MakeRequest(9055, turnbattle::kMatchModePveTeam, 7);
    AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/20, /*armor*/10, /*crit*/0, /*speed*/30);
    AddPlayer(request, kPlayerB, 0, 1000, 1000, /*strength*/20, /*armor*/10, /*crit*/0, /*speed*/25);
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
    // 僵局局面:玩家力量 0 且每回合防御(打不动怪),怪物力量 1 打不穿玩家护甲(每回合 1 点)
    const auto playStalemate = [](const std::shared_ptr<MemoryBattleDataProvider>& provider,
                                  uint64_t battleId) {
        TurnBattleEngine engine(provider);
        auto request = MakeRequest(battleId, turnbattle::kMatchModePveSolo, 7);
        AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/0, /*armor*/10, /*crit*/0, /*speed*/20);
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
        monster.set_speed(1);
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
    AddPlayer(request, kPlayerA, 0, 1000, 1000, /*strength*/20, /*armor*/10, 0, /*speed*/10);
    AddPlayer(request, kPlayerB, 1, 1000, 1000, /*strength*/20, /*armor*/10, 0, /*speed*/20);
    ASSERT_TRUE(engine.Initialize(request));

    engine.SubmitAction(kPlayerA, MakeAction(BATTLE_ACTION_ATTACK, kPlayerB));
    ASSERT_TRUE(engine.SubmitAction(kPlayerB, MakeAction(BATTLE_ACTION_ATTACK, kPlayerA)));
    const auto result = engine.ResolveCurrentRound();

    // 行动序:B(速度 20)先手,A 后手;battle 节点把它填进 TurnResultS2C.action_order
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
    AddPlayer(request, kPlayerA, 0, 500, 500, 20, 10, 0, 10);
    AddPlayer(request, kPlayerB, 0, 500, 500, 20, 10, 0, 9);
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
    auto* snapshot = AddPlayer(request, kPlayerA, 0, 1000, 1000, 20, 10, 0, 20);
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
