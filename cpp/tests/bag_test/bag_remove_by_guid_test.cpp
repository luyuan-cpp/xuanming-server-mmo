// 按 guid 全或无扣物的用例(聚宝斋 P2 托管原语,docs/design/jubaozhai-market.md §6.2,
// 错误码口径见 docs/design/guild-phase2/04-asset-channel.md §4.10)。
//
// 这条路与 RemoveItemsClamped **语义相反**,所以要单独一组用例钉住:
//   * Clamped:按 config 夹紧、恒成功(战斗结算消耗,契约就是"不足按 0");
//   * ByGuid :按 guid 全或无,少一件、多报一件、类型不对都整批拒,且**销毁实例**。
// 两者最容易被后来者"顺手合并",合并的代价是玩家挂出去卖的那件装备会被悄悄夹掉,
// 或者留下 size=0 的空壳与交易库快照同 guid 双存在(过户回来 = 复制)。
//
// 本文件的公共夹具(配置表、item 号段)由 bag_test.cpp 的 main() 统一装好。

#include <gtest/gtest.h>

#include <vector>

#include "engine/core/type_define/type_define.h"

#include "modules/bag/bag_service.h"
#include "modules/bag/bag_system.h"
#include "player/comp/player_frozen_comp.h"
#include "table/code/item_table.h"
#include "table/proto/tip/asset_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include <thread_context/ecs_context.h>

namespace
{

constexpr uint32_t kNonStackA = 1; // max_stack_size == 1
constexpr uint32_t kNonStackB = 2; // max_stack_size == 1
constexpr uint32_t kStackable = 10;

InitItemParam MakeOne(uint32_t configId, uint32_t size = 1)
{
	InitItemParam param;
	param.itemPBComp.set_config_id(configId);
	param.itemPBComp.set_size(size);
	return param;
}

Guid AddAndGetGuid(Bag &bag, uint32_t configId)
{
	std::vector<Guid> written;
	EXPECT_EQ(kSuccess, bag.AddItem(MakeOne(configId), &written));
	EXPECT_EQ(1u, written.size());
	return written.empty() ? kInvalidGuid : written.front();
}

// 前置条件:测试用的两个 config 必须真的是不可叠加的,否则下面每条用例验的都不是本意。
void RequireNonStackableFixtureData()
{
	ASSERT_EQ(1u, ItemTableManager::Instance().FindById(kNonStackA).first->max_stack_size());
	ASSERT_EQ(1u, ItemTableManager::Instance().FindById(kNonStackB).first->max_stack_size());
	ASSERT_GT(ItemTableManager::Instance().FindById(kStackable).first->max_stack_size(), 1u);
}

} // namespace

TEST(BagRemoveByGuidTest, RemovesEveryRequestedInstanceAndFreesItsSlot)
{
	RequireNonStackableFixtureData();

	Bag bag;
	const Guid a = AddAndGetGuid(bag, kNonStackA);
	const Guid b = AddAndGetGuid(bag, kNonStackB);
	ASSERT_EQ(2u, bag.OccupiedGridCount());

	std::vector<DestroyedInstance> removed;
	EXPECT_EQ(kSuccess, BagService::RemoveItemsByGuid(entt::null, bag, {a, b},
													 TX_AUCTION_SELL, 4242, &removed));

	// **实例真的没了**,不是 size 变 0 的僵尸堆 —— 这是与 Drain 那条路的分水岭:
	// 留着空壳,玩家 blob 与交易库快照会同 guid 双存在,过户回来就是复制。
	EXPECT_EQ(nullptr, bag.GetItemCompByGuid(a));
	EXPECT_EQ(nullptr, bag.GetItemCompByGuid(b));
	EXPECT_EQ(0u, bag.OccupiedGridCount());
	EXPECT_EQ(0u, bag.GridSlotCount()) << "槽位必须跟着释放,否则背包会莫名其妙满";
	EXPECT_TRUE(bag.IsLayerConsistent());

	// 回执是调用方落流水与回填交易库快照的唯一依据,必须按入参顺序给齐。
	ASSERT_EQ(2u, removed.size());
	EXPECT_EQ(a, removed[0].guid);
	EXPECT_EQ(kNonStackA, removed[0].configId);
	EXPECT_EQ(1u, removed[0].size);
	EXPECT_EQ(b, removed[1].guid);
	EXPECT_EQ(kNonStackB, removed[1].configId);
}

TEST(BagRemoveByGuidTest, MissingGuidRejectsWholeBatchWithoutRemovingAnything)
{
	RequireNonStackableFixtureData();

	Bag bag;
	const Guid a = AddAndGetGuid(bag, kNonStackA);
	const Guid absent = a + 1000000; // 一定不在这个包里

	std::vector<DestroyedInstance> removed;
	EXPECT_EQ(kAssetInvalidBundle,
			  BagService::RemoveItemsByGuid(entt::null, bag, {a, absent}, TX_AUCTION_SELL, 1, &removed));

	// 全或无:第一件在包里,但整批被拒,它必须原封不动。
	EXPECT_NE(nullptr, bag.GetItemCompByGuid(a));
	EXPECT_EQ(1u, bag.OccupiedGridCount());
	EXPECT_TRUE(removed.empty()) << "整批被拒时不得留下半截回执";
	EXPECT_TRUE(bag.IsLayerConsistent());
}

TEST(BagRemoveByGuidTest, DuplicateGuidRejectsWholeBatch)
{
	RequireNonStackableFixtureData();

	Bag bag;
	const Guid a = AddAndGetGuid(bag, kNonStackA);

	// 同一 guid 报两次:放过它的话,第二次销毁会失败,而第一件已经没了 ——
	// 全或无当场破掉,而调用方(交易库)会按"托管了两件"记账。
	EXPECT_EQ(kAssetInvalidBundle,
			  BagService::RemoveItemsByGuid(entt::null, bag, {a, a}));
	EXPECT_NE(nullptr, bag.GetItemCompByGuid(a));
	EXPECT_EQ(1u, bag.OccupiedGridCount());
}

TEST(BagRemoveByGuidTest, StackableItemRejected)
{
	RequireNonStackableFixtureData();

	Bag bag;
	std::vector<Guid> written;
	ASSERT_EQ(kSuccess, bag.AddItem(MakeOne(kStackable, 3), &written));
	ASSERT_FALSE(written.empty());

	// 可叠加物品的 guid 不是稳定身份:AddStackableItem 会把预设 guid 并进既有堆后重铸,
	// "按 guid 还回去"在交付那一侧根本不成立。所以这里必须终局拒绝,而不是"尽力而为"。
	EXPECT_EQ(kAssetInvalidBundle,
			  BagService::RemoveItemsByGuid(entt::null, bag, {written.front()}));
	EXPECT_NE(nullptr, bag.GetItemCompByGuid(written.front()));
	EXPECT_EQ(3u, bag.GetTotalItemCount(kStackable));
}

TEST(BagRemoveByGuidTest, EmptyRequestRejected)
{
	Bag bag;
	EXPECT_EQ(kAssetInvalidBundle, BagService::RemoveItemsByGuid(entt::null, bag, {}));
}

TEST(BagRemoveByGuidTest, ReserveIsSideEffectFree)
{
	RequireNonStackableFixtureData();

	Bag bag;
	const Guid a = AddAndGetGuid(bag, kNonStackA);
	const Guid b = AddAndGetGuid(bag, kNonStackB);

	// reserve 段单独调用:它是"规划 -> 预留 -> 提交"里的预留,**一格都不许动**。
	// 这条用例存在的理由:一旦有人为了省事把销毁挪进 reserve,失败路径就会留下半批,
	// 而失败路径恰恰是没人手动测的那一条。
	std::vector<DestroyedInstance> plan;
	EXPECT_EQ(kSuccess, bag.ReserveForBatchRemove({a, b}, &plan));
	EXPECT_EQ(2u, plan.size());
	EXPECT_EQ(2u, bag.OccupiedGridCount());
	EXPECT_NE(nullptr, bag.GetItemCompByGuid(a));
	EXPECT_NE(nullptr, bag.GetItemCompByGuid(b));
	EXPECT_TRUE(bag.IsLayerConsistent());
}

TEST(BagRemoveByGuidTest, FrozenPlayerKeepsEverythingAndReportsRetryClassCode)
{
	RequireNonStackableFixtureData();

	const auto player = tlsEcs.actorRegistry.create();
	Bag bag;
	const Guid a = AddAndGetGuid(bag, kNonStackA);

	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	// kAssetFrozen 是 RETRY 类:交接在途是会自己结束的条件,调用方应当重投,
	// 而不是把这次寄售终局拒掉。
	EXPECT_EQ(kAssetFrozen, BagService::RemoveItemsByGuid(player, bag, {a}));
	EXPECT_NE(nullptr, bag.GetItemCompByGuid(a));
	EXPECT_EQ(1u, bag.OccupiedGridCount());

	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
	EXPECT_EQ(kSuccess, BagService::RemoveItemsByGuid(player, bag, {a}));
	EXPECT_EQ(nullptr, bag.GetItemCompByGuid(a));

	tlsEcs.actorRegistry.destroy(player);
}
