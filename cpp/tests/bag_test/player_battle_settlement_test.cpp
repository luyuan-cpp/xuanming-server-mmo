#include <gtest/gtest.h>

#include <cstdint>
#include <vector>

#include "modules/condition/condition_type.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/event/battle_event.pb.h"
#include "proto/common/event/mission_event.pb.h"
#include "services/scene/battle/system/player_battle.h"
#include "thread_context/ecs_context.h"

// 访问生产共用入口；不复制金币和任务副作用实现，不连接 Redis 或登录账号。
class PlayerBattleSettlementTestAccess {
public:
    static bool Apply(entt::entity player, const BattleSettlementData& settlement) {
        return PlayerBattleSystem::ApplySettlementToEntity(player, settlement);
    }
    static void ApplyPending(entt::entity player, const BattleSettlementData& settlement) {
        BattleSettlementEvent event;
        *event.mutable_settlement() = settlement;
        PlayerBattleSystem::ApplyPendingSettlement(player, event);
    }
};

namespace {
class PlayerBattleSettlementTest : public testing::Test {
protected:
    void SetUp() override {
        // 缓存跨实体生命周期，所以每个测试/重复运行分配不同真实身份。
        static uint64_t nextPlayer = 9876500;
        playerId = ++nextPlayer;
        CreatePlayerEntity();
        settlement.set_player_id(playerId);
        settlement.set_battle_id(701);
        settlement.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN);
        settlement.set_health(80);
        settlement.set_mana(20);
        settlement.set_gold_gain(12);
        for (const auto configId : {uint32_t{7002}, uint32_t{7001}}) {
            auto* defeat = settlement.add_defeated_monsters();
            defeat->set_monster_config_id(configId);
            defeat->set_count(1);
        }
        tlsEcs.dispatcher.sink<ConditionEvent>().connect<&PlayerBattleSettlementTest::OnCondition>(*this);
    }
    void TearDown() override {
        tlsEcs.dispatcher.sink<ConditionEvent>().disconnect<&PlayerBattleSettlementTest::OnCondition>(*this);
        if (tlsEcs.actorRegistry.valid(player)) tlsEcs.actorRegistry.destroy(player);
    }
    void CreatePlayerEntity() {
        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<Guid>(player, Guid{playerId});
        auto& attributes = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player);
        attributes.set_health(100);
        attributes.set_mana(50);
        tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
    }
    void OnCondition(const ConditionEvent& event) {
        if (event.entity() != entt::to_integral(player)) return;
        EXPECT_EQ(event.condition_type(), static_cast<uint32_t>(eConditionType::kConditionKillMonster));
        EXPECT_EQ(event.amount(), 1);
        if (event.condition_ids_size() == 1) kills.push_back(event.condition_ids(0));
        if (tryReentry) {
            tryReentry = false;
            didReenter = true;
            reentryApplied = PlayerBattleSettlementTestAccess::Apply(player, settlement);
        }
    }
    entt::entity player{entt::null};
    uint64_t playerId{};
    BattleSettlementData settlement;
    std::vector<uint32_t> kills;
    bool tryReentry{}, didReenter{}, reentryApplied{};
};

TEST_F(PlayerBattleSettlementTest, DuplicateSuccessfulCallbacksAndReentryPayAndProgressOnce) {
    tryReentry = true;
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_TRUE(didReenter);
    EXPECT_FALSE(reentryApplied);
    // 模拟两个 GET 均返回同一 battle_id，DEL 尚未执行时第二个回调抵达共用入口。
    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_EQ(kills, (std::vector<uint32_t>{7002, 7001}));
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health(), 80u);
    // 下一局不依赖 id 单调递增，正常收益仍然可应用。
    settlement.set_battle_id(700);
    EXPECT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 24u);
    EXPECT_EQ(kills.size(), 4u);
}

TEST_F(PlayerBattleSettlementTest, CurrencyFailureLeavesPendingSideEffectsUntouchedAndCanRetry) {
    tlsEcs.actorRegistry.remove<CurrencyComp>(player);
    tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(settlement.battle_id());
    PlayerBattleSettlementTestAccess::ApplyPending(player, settlement);
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health(), 100u);
    EXPECT_EQ(tlsEcs.actorRegistry.get<BaseAttributesComp>(player).mana(), 50u);
    EXPECT_TRUE(kills.empty());
    EXPECT_TRUE(tlsEcs.actorRegistry.all_of<InBattleComp>(player));
    tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_EQ(kills.size(), 2u);
}

TEST_F(PlayerBattleSettlementTest, ReloginOldPendingDoesNotPayAgainOrRemoveNewBattle) {
    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    const auto currency = tlsEcs.actorRegistry.get<CurrencyComp>(player);
    const auto attributes = tlsEcs.actorRegistry.get<BaseAttributesComp>(player);
    tlsEcs.actorRegistry.destroy(player);
    CreatePlayerEntity();
    tlsEcs.actorRegistry.get<CurrencyComp>(player) = currency;
    tlsEcs.actorRegistry.get<BaseAttributesComp>(player) = attributes;
    tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(702);
    PlayerBattleSettlementTestAccess::ApplyPending(player, settlement);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
    EXPECT_EQ(kills.size(), 2u);
    const auto* current = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
    ASSERT_NE(current, nullptr);
    EXPECT_EQ(current->battle_id(), 702u);
}

TEST_F(PlayerBattleSettlementTest, MissingAttributesCanRetryWithoutPrematureGoldCredit) {
    tlsEcs.actorRegistry.remove<BaseAttributesComp>(player);
    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_TRUE(kills.empty());
    tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player).set_health(100);
    EXPECT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
}

TEST_F(PlayerBattleSettlementTest, WrongPlayerCannotApplyOrPoisonValidRetry) {
    settlement.set_player_id(playerId + 1);
    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u);
    EXPECT_TRUE(kills.empty());
    settlement.set_player_id(playerId);
    EXPECT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
}
} // namespace
