#include <gtest/gtest.h>

#include <cstdint>
#include <vector>

#include "modules/condition/condition_type.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/bag/item_system.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "proto/battle/battle_data.pb.h"
#include "table/code/item_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
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

// ---------------------------------------------------------------------------
// 2026-09-17 G1/G3:结算道具落地(docs/design/turn-battle-gap-closure.md)
// 覆盖:消耗按实际持有夹紧、掉落真入包、道具失败不得连累金币入账。
// 物品 id 用真实 Item 表里的可叠加物(10 / 11);bag_test 的 main 已 Load 过 ItemTable
// 并装好 item 号段基线。
// ---------------------------------------------------------------------------

namespace {

constexpr uint32_t kSettlementStackItem = 10;   // 可叠加
constexpr uint32_t kSettlementDropItem = 11;    // 可叠加

class PlayerBattleSettlementItemTest : public PlayerBattleSettlementTest {
protected:
    void SetUp() override {
        PlayerBattleSettlementTest::SetUp();
        ItemTableManager::Instance().Load();
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        for (auto& bag : bags.bags) bag.SetPlayerGuid(Guid{playerId});
        bags.bags[kInventory].ExpandCapacity(20);
    }
    Bag& Inventory() {
        return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    }
    void GiveItem(uint32_t configId, uint32_t count) {
        InitItemParam param;
        param.itemPBComp.set_config_id(configId);
        param.itemPBComp.set_size(count);
        ASSERT_EQ(kSuccess, Inventory().AddItem(param));
    }
    void SetConsumed(uint32_t configId, uint64_t count) {
        auto* entry = settlement.add_items_consumed();
        entry->set_item_table_id(configId);
        entry->set_count(count);
    }
    void SetGained(uint32_t configId, uint64_t count) {
        auto* entry = settlement.add_items_gained();
        entry->set_item_table_id(configId);
        entry->set_count(count);
    }
};

TEST_F(PlayerBattleSettlementItemTest, ConsumedIsDeductedAndGainedIsAddedToInventory) {
    GiveItem(kSettlementStackItem, 5);
    SetConsumed(kSettlementStackItem, 2);
    SetGained(kSettlementDropItem, 3);

    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));

    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementStackItem), 3u);
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementDropItem), 3u);
    // 金币照常入账:道具与金币在同一次应用里
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
}

TEST_F(PlayerBattleSettlementItemTest, ConsumedIsClampedWhenPlayerNoLongerHoldsEnough) {
    // 战斗中途玩家的药被别处扣走了(今天已被局中闸挡住,这里验的是防守姿态):
    // 契约是"按实际持有校验扣除,不足按 0",绝不能整笔结算失败。
    GiveItem(kSettlementStackItem, 1);
    SetConsumed(kSettlementStackItem, 4);

    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));

    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementStackItem), 0u);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
}

TEST_F(PlayerBattleSettlementItemTest, ConsumedForItemPlayerDoesNotHaveDoesNotFailSettlement) {
    SetConsumed(kSettlementStackItem, 2);
    SetGained(kSettlementDropItem, 1);

    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));

    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementStackItem), 0u);
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementDropItem), 1u);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
}

TEST_F(PlayerBattleSettlementItemTest, ConsumedNonBattleItemIsRejectedNotDestroyed) {
    // 反向校验:快照只会把 battle_usable 的物品放进副本,账本里出现别的 id 只可能是
    // 陈旧的 battle 节点或伪造结算。放行等于让战斗服点名销毁玩家的任意物品。
    constexpr uint32_t kNonBattleItem = 1;  // Item 表里 battle_usable = 0
    GiveItem(kNonBattleItem, 3);
    SetConsumed(kNonBattleItem, 3);

    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));

    EXPECT_EQ(Inventory().GetTotalItemCount(kNonBattleItem), 3u);  // 一件都没被扣
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
}

TEST_F(PlayerBattleSettlementItemTest, RepeatedApplyDoesNotDoubleConsumeOrDoubleDrop) {
    GiveItem(kSettlementStackItem, 5);
    SetConsumed(kSettlementStackItem, 2);
    SetGained(kSettlementDropItem, 3);

    ASSERT_TRUE(PlayerBattleSettlementTestAccess::Apply(player, settlement));
    // 同 (player, battle_id) 的重投由应用缓存挡掉:道具不能再扣一次、也不能再发一次
    EXPECT_FALSE(PlayerBattleSettlementTestAccess::Apply(player, settlement));

    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementStackItem), 3u);
    EXPECT_EQ(Inventory().GetTotalItemCount(kSettlementDropItem), 3u);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 12u);
}

}  // namespace
