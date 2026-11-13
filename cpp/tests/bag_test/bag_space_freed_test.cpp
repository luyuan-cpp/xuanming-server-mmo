// 背包「腾位通知」(bag_service.h,docs/design/bag-rule-policy-layering.md §6.6「腾位唤醒」,2026-11-12)。
//
// 盯住的契约:
//   * BagService 的每个出包入口在**成功**移走了人物背包或临时格里的东西之后通知一次,玩法入口不自己调收件箱;
//   * 只认这名玩家的人物背包与临时格:仓库、别的包、没有玩家的包都不通知;
//   * 失败、一件没动的调用不通知;没装订阅方时什么都不做;
//   * 不算净值:兑换(扣消耗 + 发获得)只要扣到了就通知。
// 订阅方换成只记次数的记录器(space_freed_recorder_for_test.h);收件箱那一侧「被唤醒之后下一拍重试」由
// player_battle_settlement_test.cpp 的 InboxRetriesABagFullDeferOnTheNextTick* 两条走正式状态机验证,穿戴的那一处在
// player_equip_test.cpp。
//
// 铺底一律走 Bag::InsertItemForRestore(预设 guid,不铸号),与 item_hold_test.cpp 同一个理由:不碰 item 号段。
// 不读流水(单测没有可读侧)。

#include <gtest/gtest.h>

#include <cstdint>
#include <vector>

#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/type_define/type_define.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "proto/common/rollback/transaction_log.pb.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "space_freed_recorder_for_test.h"
#include "table/code/item_table.h"
#include "table/proto/tip/asset_error_tip.pb.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
// generated/tables/item.json 的既有行:10 可叠加(上限 999),11 可叠加(上限 9),3 不可叠加。
constexpr uint32_t kPotion = 10;
constexpr uint32_t kElixir = 11;
constexpr uint32_t kTalisman = 3;

constexpr Guid kPotionStackA = (uint64_t{1} << 60) + 9601;
constexpr Guid kPotionStackB = (uint64_t{1} << 60) + 9602;
constexpr Guid kTalismanA = (uint64_t{1} << 60) + 9603;
constexpr Guid kTalismanB = (uint64_t{1} << 60) + 9604;
constexpr Guid kParkedTalisman = (uint64_t{1} << 60) + 9605;
constexpr Guid kWarehousePotion = (uint64_t{1} << 60) + 9606;
constexpr Guid kUnknownGuid = (uint64_t{1} << 60) + 9699;

class BagSpaceFreedTest : public testing::Test
{
protected:
	void SetUp() override
	{
		ItemTableManager::Instance().Load();
		ASSERT_NE(nullptr, ItemTableManager::Instance().FindByIdSilent(kPotion).first);
		ASSERT_GE(ItemTableManager::Instance().FindByIdSilent(kPotion).first->max_stack_size(), 10u);
		ASSERT_NE(nullptr, ItemTableManager::Instance().FindByIdSilent(kElixir).first);
		ASSERT_NE(nullptr, ItemTableManager::Instance().FindByIdSilent(kTalisman).first);
		ASSERT_EQ(1u, ItemTableManager::Instance().FindByIdSilent(kTalisman).first->max_stack_size());

		static uint64_t nextPlayer = 9879900;
		playerId = ++nextPlayer;
		player = tlsEcs.actorRegistry.create();
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, Guid{playerId});
		// ConsumeInventoryAndGrant 要可改的实体(归属围栏)与按 player_id 现取实体。
		tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 1;
		tlsEcs.playerList.erase(playerId);
		tlsEcs.playerList.emplace(playerId, player);
		auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
		for (auto &bag : bags.bags)
		{
			bag.SetPlayerGuid(Guid{playerId});
		}
		// 人物背包:两堆没满的药(能合成一堆)、两件不可叠加的护符;临时格:一件护符;仓库:一堆药。
		Inventory().SetCapacityForRestore(20);
		Inventory().InsertItemForRestore(kPotionStackA, kPotion, /*stackSize=*/4, /*pos=*/0);
		Inventory().InsertItemForRestore(kPotionStackB, kPotion, /*stackSize=*/2, /*pos=*/1);
		Inventory().InsertItemForRestore(kTalismanA, kTalisman, /*stackSize=*/1, /*pos=*/2);
		Inventory().InsertItemForRestore(kTalismanB, kTalisman, /*stackSize=*/1, /*pos=*/3);
		Temporary().InsertItemForRestore(kParkedTalisman, kTalisman, /*stackSize=*/1, /*pos=*/0);
		Warehouse().InsertItemForRestore(kWarehousePotion, kPotion, /*stackSize=*/5, /*pos=*/0);
	}

	void TearDown() override
	{
		tlsEcs.playerList.erase(playerId);
		if (tlsEcs.actorRegistry.valid(player))
		{
			tlsEcs.actorRegistry.destroy(player);
		}
	}

	Bag &Inventory() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory]; }
	Bag &Temporary() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kTemporary]; }
	Bag &Warehouse() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kWarehouse]; }

	static const std::vector<entt::entity> &Notified() { return ScopedSpaceFreedRecorder::Notified(); }
	std::vector<entt::entity> Once() const { return {player}; }

	uint64_t playerId{};
	entt::entity player{entt::null};
};

// 按数量扣(强化扣材料、镶嵌扣宝石、资产通道寄出 / 捐献、GM 回收都走它):成功一次,通知一次。
TEST_F(BagSpaceFreedTest, RemovingByQuantityFromTheInventoryNotifiesOnce)
{
	ScopedSpaceFreedRecorder recorder;
	ASSERT_EQ(kSuccess, BagService::RemoveItemsExact(player, Inventory(), {{kPotion, 2}}, TX_ITEM_DESTROY));
	EXPECT_EQ(Once(), Notified());
}

// 按 guid 整批销毁(聚宝斋上架托管、交易扣出、GM 回收):整批成功只通知一次,不按件数。
TEST_F(BagSpaceFreedTest, RemovingABatchByGuidNotifiesOnceForTheWholeBatch)
{
	ScopedSpaceFreedRecorder recorder;
	ASSERT_EQ(kSuccess, BagService::RemoveItemsByGuid(player, Inventory(), {kTalismanA, kTalismanB}, TX_AUCTION_SELL, 1));
	EXPECT_EQ(Once(), Notified());
}

// 单件销毁通知;按持有夹紧的扣除只在真扣到了东西时通知(夹到 0 的一件没动)。
TEST_F(BagSpaceFreedTest, SingleRemovalAndClampedRemovalNotifyOnlyWhenSomethingLeft)
{
	ScopedSpaceFreedRecorder recorder;
	ASSERT_EQ(kSuccess, BagService::RemoveItem(player, Inventory(), kTalismanA));
	EXPECT_EQ(Once(), Notified());

	ASSERT_EQ(kSuccess, BagService::RemoveItemsClamped(player, Inventory(), {{kElixir, 3}})) << "包里没有这种:夹到 0,照常成功";
	EXPECT_EQ(Once(), Notified()) << "一件没扣,不通知";

	ASSERT_EQ(kSuccess, BagService::RemoveItemsClamped(player, Inventory(), {{kPotion, 1}}));
	EXPECT_EQ((std::vector<entt::entity>{player, player}), Notified());
}

// 兑换(战斗结算扣消耗、以后的开箱一类):扣到了消耗就通知,不管同一笔获得占了多少位;一件没扣(夹到 0)不通知。
TEST_F(BagSpaceFreedTest, ConsumeAndGrantNotifiesWhenItConsumedSomething)
{
	ScopedSpaceFreedRecorder recorder;
	ASSERT_EQ(kSuccess, BagService::ConsumeInventoryAndGrant(player, {{kElixir, 1}}, {}, /*correlationId=*/1, "{}"));
	EXPECT_TRUE(Notified().empty()) << "包里没有可扣的,一件没动";

	ASSERT_EQ(kSuccess, BagService::ConsumeInventoryAndGrant(player, {{kPotion, 1}}, {}, /*correlationId=*/2, "{}"));
	EXPECT_EQ(Once(), Notified());
	EXPECT_EQ(5u, Inventory().GetTotalItemCount(kPotion));
}

// 整理:合堆退役了实例(格子空出来)才通知;已经最优、什么都没合时不通知。
TEST_F(BagSpaceFreedTest, SortingNotifiesOnlyWhenStacksWereMerged)
{
	ScopedSpaceFreedRecorder recorder;
	ASSERT_EQ(4u, Inventory().OccupiedGridCount());
	bool changed = false;
	ASSERT_EQ(kSuccess, BagService::SortByPlayerRequest(player, Inventory(), &changed));
	ASSERT_TRUE(changed);
	EXPECT_EQ(3u, Inventory().OccupiedGridCount()) << "两堆药合成了一堆,空出一格";
	EXPECT_EQ(6u, Inventory().GetTotalItemCount(kPotion));
	EXPECT_EQ(Once(), Notified());

	ASSERT_EQ(kSuccess, BagService::SortByPlayerRequest(player, Inventory(), &changed));
	EXPECT_EQ(Once(), Notified()) << "第二次没有可合的堆,一格都没腾";
}

// 临时格取回:临时格搬走了实例,按临时格通知一次(人物背包同时多了一件,不算净值)。
TEST_F(BagSpaceFreedTest, RetrievingFromTheTemporaryBagNotifiesOnce)
{
	ScopedSpaceFreedRecorder recorder;
	ASSERT_EQ(kSuccess, BagService::RetrieveFromTemporary(player, {kParkedTalisman}));
	EXPECT_EQ(0u, Temporary().OccupiedGridCount());
	EXPECT_EQ(Once(), Notified());
}

// 仓库不通知(入账不落在那里);失败、一件没动的调用不通知;冻结期一律不通知。
TEST_F(BagSpaceFreedTest, OtherBagsAndFailedCallsDoNotNotify)
{
	ScopedSpaceFreedRecorder recorder;
	ASSERT_EQ(kSuccess, BagService::RemoveItemsExact(player, Warehouse(), {{kPotion, 1}}, TX_ITEM_DESTROY));
	EXPECT_TRUE(Notified().empty()) << "仓库腾位帮不了入账";

	EXPECT_EQ(kBagInsufficientItems, BagService::RemoveItemsExact(player, Inventory(), {{kPotion, 100}}, TX_ITEM_DESTROY));
	EXPECT_EQ(kAssetInvalidBundle, BagService::RemoveItemsByGuid(player, Inventory(), {kUnknownGuid}, TX_AUCTION_SELL, 1));
	EXPECT_NE(kSuccess, BagService::RemoveItem(player, Inventory(), kUnknownGuid));
	EXPECT_NE(kSuccess, BagService::RetrieveFromTemporary(player, {kUnknownGuid}));
	EXPECT_TRUE(Notified().empty()) << "失败的调用一件没动";

	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	EXPECT_NE(kSuccess, BagService::RemoveItemsExact(player, Inventory(), {{kPotion, 1}}, TX_ITEM_DESTROY));
	EXPECT_NE(kSuccess, BagService::RetrieveFromTemporary(player, {kParkedTalisman}));
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
	EXPECT_TRUE(Notified().empty()) << "冻结期一件不扣,不通知";
	EXPECT_EQ(6u, Inventory().GetTotalItemCount(kPotion));
}

// 通知只认这名玩家自己的人物背包与临时格:别处来的包(候选副本、别人的包)与无效实体都不通知。
TEST_F(BagSpaceFreedTest, NotifyOnlyAcceptsThePlayersOwnInventoryAndTemporaryBag)
{
	ScopedSpaceFreedRecorder recorder;
	Bag elsewhere;
	BagService::NotifySpaceFreed(player, elsewhere);
	BagService::NotifySpaceFreed(player, Warehouse());
	BagService::NotifySpaceFreed(entt::null, Inventory());
	EXPECT_TRUE(Notified().empty());

	BagService::NotifySpaceFreed(player, Inventory());
	BagService::NotifySpaceFreed(player, Temporary());
	EXPECT_EQ((std::vector<entt::entity>{player, player}), Notified());
}

// 没装订阅方(单测缺省、scene 装配之前)时出包照常成功,什么都不发。
TEST_F(BagSpaceFreedTest, NothingHappensWithoutAListener)
{
	{
		ScopedSpaceFreedRecorder recorder; // 出作用域即卸掉
	}
	ASSERT_EQ(kSuccess, BagService::RemoveItemsExact(player, Inventory(), {{kPotion, 1}}, TX_ITEM_DESTROY));
	EXPECT_TRUE(ScopedSpaceFreedRecorder::Notified().empty());
	EXPECT_EQ(5u, Inventory().GetTotalItemCount(kPotion));
}

} // namespace
