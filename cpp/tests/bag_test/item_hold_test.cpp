// 物品数量预留(docs/design/battle-item-reservation.md):纯规则(item_hold.h)与 BagService 的唯一闸口。
//
// 盯住的契约:
//   * 某个配置号被占用的数量 = 全部预留之和(坏数据、本版本不认识的种类也算);
//   * 只约束这名玩家的人物背包:别的包、别的实体、候选副本都按「没有预留」;
//   * 四个出包入口不拿走被预留占住的数量(夹紧或整批拒),唯一能动用某份预留的是
//     ConsumeInventoryAndGrant(consumeFromHoldId = 那份预留的号);
//   * PlaceHold 按「可出包数量」核数量,缺组件即拒绝,任何失败一字不改。
//
// 铺底一律走 Bag::InsertItemForRestore(预设 guid,不铸号),与 bag_remove_exact_test.cpp 同一个理由:不碰 item 号段,
// 不受别的用例改号段的先后影响。不读流水(单测没有可读侧)。

#include <gtest/gtest.h>

#include <cstdint>
#include <string>
#include <vector>

#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/type_define/type_define.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/bag/item_hold.h"
#include "proto/common/component/player_item_hold_comp.pb.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "table/code/item_table.h"
#include "table/proto/tip/asset_error_tip.pb.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
using item_hold::AddVerdict;
using item_hold::HoldLines;
using item_hold::ItemHoldKind;

// generated/tables/item.json 的既有行:10、11 可叠加且 battle_usable,3 不可叠加。
constexpr uint32_t kPotion = 10;
constexpr uint32_t kElixir = 11;
constexpr uint32_t kTalisman = 3;

constexpr Guid kPotionStackA = (uint64_t{1} << 60) + 9501;
constexpr Guid kPotionStackB = (uint64_t{1} << 60) + 9502;
constexpr Guid kElixirStack = (uint64_t{1} << 60) + 9503;
constexpr Guid kTalismanA = (uint64_t{1} << 60) + 9504;
constexpr Guid kTalismanB = (uint64_t{1} << 60) + 9505;

constexpr uint64_t kBattleA = 880101;
constexpr uint64_t kBattleB = 880102;

PlayerItemHoldComp MakeHolds(std::initializer_list<std::pair<uint64_t, HoldLines>> holds)
{
	PlayerItemHoldComp comp;
	for (const auto& [holdId, lines] : holds)
	{
		EXPECT_EQ(AddVerdict::kAdded, item_hold::Add(comp, holdId, ItemHoldKind::kBattle, lines, 1, 2)) << "hold " << holdId;
	}
	return comp;
}

// ── 纯规则 ───────────────────────────────────────────────────────────────────────────

TEST(ItemHoldRules, HeldCountSumsEveryHoldAndCanExcludeOne)
{
	const auto holds = MakeHolds({{kBattleA, {{kPotion, 3}, {kElixir, 1}}}, {kBattleB, {{kPotion, 2}}}});
	EXPECT_EQ(5u, item_hold::HeldCount(holds, kPotion));
	EXPECT_EQ(1u, item_hold::HeldCount(holds, kElixir));
	EXPECT_EQ(0u, item_hold::HeldCount(holds, kTalisman));
	EXPECT_EQ(2u, item_hold::HeldCount(holds, kPotion, /*exceptHoldId=*/kBattleA)) << "本局自己的预留可以排除";
	EXPECT_EQ(0u, item_hold::HeldCount(holds, kElixir, /*exceptHoldId=*/kBattleA));

	const auto* found = item_hold::Find(holds, kBattleB);
	ASSERT_NE(nullptr, found);
	EXPECT_EQ(static_cast<uint32_t>(ItemHoldKind::kBattle), found->kind());
	EXPECT_EQ(nullptr, item_hold::Find(holds, 1));
	EXPECT_TRUE(item_hold::Validate(holds).empty()) << item_hold::Validate(holds);
}

TEST(ItemHoldRules, AddWritesLinesInAscendingConfigOrder)
{
	PlayerItemHoldComp holds;
	ASSERT_EQ(AddVerdict::kAdded, item_hold::Add(holds, kBattleA, ItemHoldKind::kBattle, {{kElixir, 4}, {kPotion, 7}}, 11, 22));
	ASSERT_EQ(1, holds.holds_size());
	const auto& hold = holds.holds(0);
	EXPECT_EQ(kBattleA, hold.hold_id());
	EXPECT_EQ(11u, hold.created_at_ms());
	EXPECT_EQ(22u, hold.review_after_ms());
	ASSERT_EQ(2, hold.lines_size());
	EXPECT_EQ(kPotion, hold.lines(0).config_id()) << "lines 按 config_id 升序";
	EXPECT_EQ(7u, hold.lines(0).count());
	EXPECT_EQ(kElixir, hold.lines(1).config_id());
}

TEST(ItemHoldRules, AddRefusesInvalidDuplicateAndFullWithoutChangingAnything)
{
	PlayerItemHoldComp holds;
	EXPECT_EQ(AddVerdict::kInvalid, item_hold::Add(holds, 0, ItemHoldKind::kBattle, {{kPotion, 1}}, 1, 2)) << "号为 0";
	EXPECT_EQ(AddVerdict::kInvalid, item_hold::Add(holds, kBattleA, ItemHoldKind::kUnspecified, {{kPotion, 1}}, 1, 2));
	EXPECT_EQ(AddVerdict::kInvalid, item_hold::Add(holds, kBattleA, ItemHoldKind::kBattle, {}, 1, 2)) << "空清单";
	EXPECT_EQ(AddVerdict::kInvalid, item_hold::Add(holds, kBattleA, ItemHoldKind::kBattle, {{kPotion, 0}}, 1, 2));
	EXPECT_EQ(AddVerdict::kInvalid, item_hold::Add(holds, kBattleA, ItemHoldKind::kBattle, {{0, 1}}, 1, 2));
	EXPECT_EQ(0, holds.holds_size());

	ASSERT_EQ(AddVerdict::kAdded, item_hold::Add(holds, kBattleA, ItemHoldKind::kBattle, {{kPotion, 1}}, 1, 2));
	const auto snapshot = holds.SerializeAsString();
	EXPECT_EQ(AddVerdict::kDuplicate, item_hold::Add(holds, kBattleA, ItemHoldKind::kBattle, {{kElixir, 1}}, 1, 2));
	EXPECT_EQ(snapshot, holds.SerializeAsString()) << "重复的号一字不改";

	for (uint64_t id = 2; holds.holds_size() < static_cast<int>(item_hold::kMaxHolds); ++id)
	{
		ASSERT_EQ(AddVerdict::kAdded, item_hold::Add(holds, kBattleA + id, ItemHoldKind::kBattle, {{kPotion, 1}}, 1, 2));
	}
	EXPECT_EQ(AddVerdict::kFull, item_hold::Add(holds, 1, ItemHoldKind::kBattle, {{kPotion, 1}}, 1, 2));
	EXPECT_EQ(static_cast<int>(item_hold::kMaxHolds), holds.holds_size());
}

TEST(ItemHoldRules, RemoveDropsEveryRecordWithTheIdAndKeepsTheRestInOrder)
{
	auto holds = MakeHolds({{kBattleA, {{kPotion, 1}}}, {kBattleB, {{kPotion, 2}}}, {kBattleA + 7, {{kElixir, 1}}}});
	// 坏数据里同号重复:Remove 一并摘掉。
	*holds.add_holds() = holds.holds(0);
	EXPECT_EQ(2u, item_hold::Remove(holds, kBattleA));
	ASSERT_EQ(2, holds.holds_size());
	EXPECT_EQ(kBattleB, holds.holds(0).hold_id());
	EXPECT_EQ(kBattleA + 7, holds.holds(1).hold_id());
	EXPECT_EQ(0u, item_hold::Remove(holds, kBattleA)) << "幂等";
}

// 坏数据原样携带、照样计入占用:多占只会让玩家暂时拿不走,少占会让晚到的结算扣不出来。
TEST(ItemHoldRules, ValidateFlagsCorruptRecordsWhileHeldCountStillCountsThem)
{
	PlayerItemHoldComp duplicate = MakeHolds({{kBattleA, {{kPotion, 1}}}});
	*duplicate.add_holds() = duplicate.holds(0);
	EXPECT_EQ("duplicate_hold_id", item_hold::Validate(duplicate));
	EXPECT_EQ(2u, item_hold::HeldCount(duplicate, kPotion));

	PlayerItemHoldComp unsorted;
	auto* hold = unsorted.add_holds();
	hold->set_hold_id(kBattleA);
	hold->set_kind(static_cast<uint32_t>(ItemHoldKind::kBattle));
	auto* first = hold->add_lines();
	first->set_config_id(kElixir);
	first->set_count(2);
	auto* second = hold->add_lines();
	second->set_config_id(kPotion);
	second->set_count(3);
	EXPECT_EQ("lines_not_ascending", item_hold::Validate(unsorted));
	EXPECT_EQ(3u, item_hold::HeldCount(unsorted, kPotion));

	PlayerItemHoldComp zeroCount = unsorted;
	zeroCount.mutable_holds(0)->mutable_lines(0)->set_count(0);
	EXPECT_EQ("zero_config_or_count", item_hold::Validate(zeroCount));

	PlayerItemHoldComp unspecified = MakeHolds({{kBattleA, {{kPotion, 1}}}});
	unspecified.mutable_holds(0)->set_kind(0);
	EXPECT_EQ("unspecified_kind", item_hold::Validate(unspecified));
	EXPECT_EQ(1u, item_hold::HeldCount(unspecified, kPotion));
}

// 本版本不认识的种类(更新版本写下的预留)不算坏数据,照样计入占用,等认识它的版本释放。
TEST(ItemHoldRules, UnknownKindIsValidAndCounted)
{
	PlayerItemHoldComp holds = MakeHolds({{kBattleA, {{kPotion, 4}}}});
	holds.mutable_holds(0)->set_kind(static_cast<uint32_t>(ItemHoldKind::kCount) + 5);
	EXPECT_TRUE(item_hold::Validate(holds).empty()) << item_hold::Validate(holds);
	EXPECT_EQ(4u, item_hold::HeldCount(holds, kPotion));
	EXPECT_STREQ("unknown", item_hold::ItemHoldKindName(holds.holds(0).kind()));
	EXPECT_STREQ("battle", item_hold::ItemHoldKindName(static_cast<uint32_t>(ItemHoldKind::kBattle)));
}

// ── BagService 闸口 ───────────────────────────────────────────────────────────────────

class BagServiceItemHoldTest : public testing::Test
{
protected:
	void SetUp() override
	{
		ItemTableManager::Instance().Load();
		for (const auto configId : {kPotion, kElixir})
		{
			const auto* row = ItemTableManager::Instance().FindByIdSilent(configId).first;
			ASSERT_NE(nullptr, row) << "fixture Item row " << configId;
			ASSERT_GE(row->max_stack_size(), 10u) << "Item row " << configId;
		}
		const auto* talisman = ItemTableManager::Instance().FindByIdSilent(kTalisman).first;
		ASSERT_NE(nullptr, talisman);
		ASSERT_EQ(1u, talisman->max_stack_size());

		static uint64_t nextPlayer = 9878800;
		playerId = ++nextPlayer;
		player = tlsEcs.actorRegistry.create();
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, Guid{playerId});
		// ConsumeInventoryAndGrant 要可改的实体(归属围栏)与按 player_id 现取实体。
		tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 1;
		tlsEcs.playerList.erase(playerId);
		tlsEcs.playerList.emplace(playerId, player);
		auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
		for (auto& bag : bags.bags)
		{
			bag.SetPlayerGuid(Guid{playerId});
		}
		Inventory().SetCapacityForRestore(20);
		Inventory().InsertItemForRestore(kPotionStackA, kPotion, /*stackSize=*/4, /*pos=*/0);
		Inventory().InsertItemForRestore(kPotionStackB, kPotion, /*stackSize=*/2, /*pos=*/1);
		Inventory().InsertItemForRestore(kElixirStack, kElixir, /*stackSize=*/5, /*pos=*/2);
		Inventory().InsertItemForRestore(kTalismanA, kTalisman, /*stackSize=*/1, /*pos=*/3);
		Inventory().InsertItemForRestore(kTalismanB, kTalisman, /*stackSize=*/1, /*pos=*/4);
		tlsEcs.actorRegistry.emplace<PlayerItemHoldComp>(player);
	}

	void TearDown() override
	{
		tlsEcs.playerList.erase(playerId);
		if (tlsEcs.actorRegistry.valid(player))
		{
			tlsEcs.actorRegistry.destroy(player);
		}
	}

	Bag& Inventory() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory]; }
	Bag& Warehouse() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kWarehouse]; }
	PlayerItemHoldComp& Holds() { return tlsEcs.actorRegistry.get<PlayerItemHoldComp>(player); }

	uint32_t Hold(uint64_t holdId, HoldLines lines)
	{
		return BagService::PlaceHold(player, holdId, ItemHoldKind::kBattle, lines, /*createdAtMs=*/100, /*reviewAfterMs=*/200);
	}

	uint64_t playerId{};
	entt::entity player{entt::null};
};

TEST_F(BagServiceItemHoldTest, RemovableCountSubtractsHoldsOnlyOnThePlayersInventory)
{
	ASSERT_EQ(kSuccess, Hold(kBattleA, {{kPotion, 5}}));
	EXPECT_EQ(5u, BagService::HeldCount(player, Inventory(), kPotion));
	EXPECT_EQ(1u, BagService::RemovableCount(player, Inventory(), kPotion));
	EXPECT_EQ(6u, BagService::RemovableCount(player, Inventory(), kPotion, /*exceptHoldId=*/kBattleA));
	EXPECT_EQ(5u, BagService::RemovableCount(player, Inventory(), kElixir)) << "没被预留的配置号照常";

	// 别的包、副本、别的实体都按「没有预留」。
	Warehouse().SetCapacityForRestore(20);
	Warehouse().InsertItemForRestore((uint64_t{1} << 60) + 9599, kPotion, 3, 0);
	EXPECT_EQ(0u, BagService::HeldCount(player, Warehouse(), kPotion));
	EXPECT_EQ(3u, BagService::RemovableCount(player, Warehouse(), kPotion));
	Bag detached; // 不挂在任何实体上的包(候选副本同理):地址不是他的人物背包
	detached.InsertItemForRestore(kPotionStackA, kPotion, 4, 0);
	EXPECT_EQ(0u, BagService::HeldCount(player, detached, kPotion));
	EXPECT_EQ(0u, BagService::HeldCount(entt::null, Inventory(), kPotion));

	// 读缺失按「没有预留」(有记录的例外,bag_service.h)。
	tlsEcs.actorRegistry.remove<PlayerItemHoldComp>(player);
	EXPECT_EQ(6u, BagService::RemovableCount(player, Inventory(), kPotion));
}

TEST_F(BagServiceItemHoldTest, CheckRemovableReportsHeldBelowSkewAndInsufficient)
{
	ASSERT_EQ(kSuccess, Hold(kBattleA, {{kPotion, 5}}));
	EXPECT_EQ(BagService::RemovalVerdict::kEnough, BagService::CheckRemovable(player, Inventory(), {{kPotion, 1}}));
	EXPECT_EQ(BagService::RemovalVerdict::kHeld, BagService::CheckRemovable(player, Inventory(), {{kPotion, 2}}));
	EXPECT_EQ(BagService::RemovalVerdict::kInsufficient, BagService::CheckRemovable(player, Inventory(), {{kPotion, 7}}))
		<< "连算上预留都不够,是确定不足";
	EXPECT_EQ(BagService::RemovalVerdict::kHeld,
			  BagService::CheckRemovable(player, Inventory(), {{kElixir, 5}, {kPotion, 2}}));
	EXPECT_EQ(BagService::RemovalVerdict::kEnough, BagService::CheckRemovable(player, Inventory(), {{kPotion, 0}}));
}

TEST_F(BagServiceItemHoldTest, RemoveItemsExactRefusesToReachIntoHeldQuantities)
{
	ASSERT_EQ(kSuccess, Hold(kBattleA, {{kPotion, 5}}));
	EXPECT_EQ(kBagInsufficientItems, BagService::RemoveItemsExact(player, Inventory(), {{kPotion, 2}}, TX_EQUIP_ENHANCE));
	EXPECT_EQ(6u, Inventory().GetTotalItemCount(kPotion)) << "一件不扣";
	EXPECT_EQ(kSuccess, BagService::RemoveItemsExact(player, Inventory(), {{kPotion, 1}}, TX_EQUIP_ENHANCE));
	EXPECT_EQ(5u, Inventory().GetTotalItemCount(kPotion));
	EXPECT_EQ(0u, BagService::RemovableCount(player, Inventory(), kPotion));
}

TEST_F(BagServiceItemHoldTest, RemoveItemsClampedStopsAtTheHold)
{
	ASSERT_EQ(kSuccess, Hold(kBattleA, {{kPotion, 4}}));
	EXPECT_EQ(kSuccess, BagService::RemoveItemsClamped(player, Inventory(), {{kPotion, 6}, {kElixir, 1}}));
	EXPECT_EQ(4u, Inventory().GetTotalItemCount(kPotion)) << "只扣到预留为止";
	EXPECT_EQ(4u, Inventory().GetTotalItemCount(kElixir));
}

TEST_F(BagServiceItemHoldTest, RemoveItemsByGuidRetriesWhenInstancesAreHeld)
{
	ASSERT_EQ(kSuccess, Hold(kBattleA, {{kTalisman, 1}}));
	bool mutated = true;
	EXPECT_EQ(kAssetInBattle, BagService::RemoveItemsByGuid(player, Inventory(), {kTalismanA, kTalismanB}, TX_TRADE, 1,
															nullptr, &mutated))
		<< "拿走两件会低于预留:RETRY 类,一件不扣";
	EXPECT_FALSE(mutated);
	EXPECT_EQ(2u, Inventory().GetTotalItemCount(kTalisman));
	EXPECT_EQ(kSuccess, BagService::RemoveItemsByGuid(player, Inventory(), {kTalismanB}, TX_TRADE, 1, nullptr, &mutated));
	EXPECT_TRUE(mutated);
	EXPECT_EQ(1u, Inventory().GetTotalItemCount(kTalisman));
	EXPECT_EQ(BagService::RemovalVerdict::kHeld, BagService::CheckRemovableInstances(player, Inventory(), {kTalismanA}));
}

TEST_F(BagServiceItemHoldTest, RemoveItemRefusesAHeldInstance)
{
	ASSERT_EQ(kSuccess, Hold(kBattleA, {{kPotion, 6}}));
	EXPECT_EQ(kBagInsufficientItems, BagService::RemoveItem(player, Inventory(), kPotionStackB));
	EXPECT_NE(nullptr, Inventory().GetItemCompByGuid(kPotionStackB));
	EXPECT_EQ(kSuccess, BagService::RemoveItem(player, Inventory(), kElixirStack)) << "没被预留的照常";
}

// 唯一能动用预留的路:结算按自己那份预留扣(consumeFromHoldId),别的预留照样不许碰。
TEST_F(BagServiceItemHoldTest, ConsumeInventoryAndGrantMayUseItsOwnHoldButNotAnothers)
{
	ASSERT_EQ(kSuccess, Hold(kBattleA, {{kPotion, 4}}));
	ASSERT_EQ(kSuccess, Hold(kBattleB, {{kPotion, 2}}));
	ASSERT_EQ(0u, BagService::RemovableCount(player, Inventory(), kPotion));

	EXPECT_EQ(kSuccess, BagService::ConsumeInventoryAndGrant(player, {{kPotion, 6}}, {}, kBattleA, "{}", {},
															 /*consumeFromHoldId=*/kBattleA));
	EXPECT_EQ(2u, Inventory().GetTotalItemCount(kPotion)) << "动用了自己那 4 瓶,B 的 2 瓶不碰";
	ASSERT_EQ(2, Holds().holds_size()) << "本函数不释放预留,释放是调用方的事";
	ASSERT_TRUE(BagService::ReleaseHold(player, kBattleA));
	EXPECT_EQ(0u, BagService::RemovableCount(player, Inventory(), kPotion)) << "剩下的 2 瓶仍归 B";

	// 不指名任何预留:一瓶都扣不出(全被 B 占着)。
	EXPECT_EQ(kSuccess, BagService::ConsumeInventoryAndGrant(player, {{kPotion, 1}}, {}, 1, "{}"));
	EXPECT_EQ(2u, Inventory().GetTotalItemCount(kPotion));
}

TEST_F(BagServiceItemHoldTest, PlaceHoldChecksRemovableQuantityAndRefusesWithoutChanges)
{
	ASSERT_EQ(kSuccess, Hold(kBattleA, {{kPotion, 4}}));
	EXPECT_EQ(kBagInsufficientItems, Hold(kBattleB, {{kPotion, 3}})) << "只剩 2 瓶可占";
	EXPECT_EQ(kInvalidParameter, Hold(kBattleA, {{kElixir, 1}})) << "同号已有预留";
	EXPECT_EQ(kInvalidParameter, Hold(0, {{kElixir, 1}}));
	EXPECT_EQ(1, Holds().holds_size());
	EXPECT_EQ(kSuccess, Hold(kBattleB, {{kPotion, 2}, {kElixir, 5}}));
	EXPECT_EQ(2, Holds().holds_size());

	EXPECT_TRUE(BagService::ReleaseHold(player, kBattleA));
	EXPECT_FALSE(BagService::ReleaseHold(player, kBattleA)) << "幂等";
	EXPECT_FALSE(BagService::ReleaseHold(player, 0));

	// 写入口缺组件即拒绝(持久数据只在加载时创建,不补建)。
	tlsEcs.actorRegistry.remove<PlayerItemHoldComp>(player);
	EXPECT_EQ(kServiceUnavailable, Hold(kBattleA, {{kPotion, 1}}));
	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerItemHoldComp>(player));
	EXPECT_FALSE(BagService::ReleaseHold(player, kBattleB));
}

} // namespace
