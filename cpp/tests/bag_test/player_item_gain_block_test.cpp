// 单玩家物品获取封禁在背包入口上的行为(docs/design/player-inbox-service.md §2.2)。
//
// 各物品获得入口共用 GainBlockService::IsItemGainBlocked(全服 + 本人)。这里钉住 BagService 的四个入口:
// 单件 AddItem、按数量 AddItems、按完整实例 AddItems、ConsumeInventoryAndGrant(战斗结算的整笔换取,含临时格兜底)。
// 资产通道(含按预设实例直接入包)的用例在 currency_test 的 AssetOpSystemTest;收件箱执行与重投在
// currency_test 的 GmSanctionInboxExecutorTest / PlayerInboxSystemTest;存档往返在 PlayerFeaturePersistenceTest。
//
// 判据:被封禁 → 整批 kAssetBlocked,在容器做任何规划之前,一件不发、不铸号、不扣材料、不调外层回调,
// 也不因「主包放不下」改投临时格;只影响这名玩家、这个配置;不扣已有库存;存档还原与同一所有者内部搬运不受影响。
// 不连 Redis、不发 RPC,物品号段用 bag_test main 装好的基线。

#include <gtest/gtest.h>

#include <cstdint>
#include <vector>

#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/type_define/type_define.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_item_gain_block_comp.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"
#include "services/scene/player/comp/player_ownership_comp.h" // PlayerOwnerEpochComp:ConsumeInventoryAndGrant 要求可改
#include "table/proto/tip/asset_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
	// 与 bag_test.cpp / player_battle_settlement_test.cpp 同一份 item.json 的配置号。
	constexpr uint32_t kNonStackItem = 1;  // 不可叠加:每件一个新实例
	constexpr uint32_t kOtherNonStack = 2; // 不可叠加
	constexpr uint32_t kStackItem = 10;    // 可叠加
	constexpr uint32_t kMaterialItem = 11; // 可叠加,充当被消耗的材料

	// 玩家号与其它用例文件错开。
	constexpr Guid kBannedPlayerId = 950400001;
	constexpr Guid kOtherPlayerId = 950400002;

	InitItemParam Piece(uint32_t configId, uint32_t count)
	{
		InitItemParam param;
		param.itemPBComp.set_config_id(configId);
		param.itemPBComp.set_size(count);
		return param;
	}

	uint64_t ItemIdsIssued()
	{
		return tlsGuidSegmentRegistry.Get(GuidKind::kItem).GetStats().issued;
	}

	// 全服封禁来自真实全服入口;即使 ASSERT 提前退出也恢复原状。
	class ScopedGlobalItemBan
	{
	public:
		explicit ScopedGlobalItemBan(uint32_t configId)
			: configId_(configId), wasBlocked_(GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kItem, configId))
		{
			GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, configId_);
		}
		~ScopedGlobalItemBan()
		{
			if (!wasBlocked_)
			{
				GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, configId_);
			}
		}
		ScopedGlobalItemBan(const ScopedGlobalItemBan &) = delete;
		ScopedGlobalItemBan &operator=(const ScopedGlobalItemBan &) = delete;

	private:
		uint32_t configId_;
		bool wasBlocked_;
	};

	class PlayerItemGainBlockTest : public testing::Test
	{
	protected:
		void SetUp() override
		{
			banned_ = CreateLoadedPlayer(kBannedPlayerId);
			other_ = CreateLoadedPlayer(kOtherPlayerId);
		}

		void TearDown() override
		{
			DestroyPlayer(kBannedPlayerId, banned_);
			DestroyPlayer(kOtherPlayerId, other_);
		}

		static void DestroyPlayer(Guid playerId, entt::entity player)
		{
			tlsEcs.playerList.erase(playerId);
			if (tlsEcs.actorRegistry.valid(player))
			{
				tlsEcs.actorRegistry.destroy(player);
			}
		}

		// 手搭一个「加载已完成、可被修改」的玩家:持久组件(背包、货币两份、物品获取封禁表)在测试创建点显式挂上
		// (生产里只由 PlayerDatabaseMessageFieldsUnmarshal 创建);有归属围栏(ConsumeInventoryAndGrant 要求可改)。
		static entt::entity CreateLoadedPlayer(Guid playerId)
		{
			const auto player = tlsEcs.actorRegistry.create();
			tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, playerId);
			tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 7;
			tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
			tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
			tlsEcs.actorRegistry.emplace<PlayerItemGainBlockComp>(player);
			auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
			for (auto &bag : bags.bags)
			{
				bag.SetPlayerGuid(playerId);
			}
			bags.bags[kInventory].ExpandCapacity(20);
			tlsEcs.playerList[playerId] = player;
			return player;
		}

		static Bag &Inventory(entt::entity player)
		{
			return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
		}
		static Bag &Warehouse(entt::entity player)
		{
			return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kWarehouse];
		}
		static Bag &Temporary(entt::entity player)
		{
			return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kTemporary];
		}

		void Ban(uint32_t configId) const
		{
			ASSERT_EQ(static_cast<uint32_t>(kSuccess), GainBlockService::BlockPlayerItemGain(banned_, configId));
		}

		entt::entity banned_{entt::null};
		entt::entity other_{entt::null};
	};
} // namespace

// 单件、按数量批量、按完整实例批量三个入包入口都拦;只拦这名玩家的这个配置,另一名玩家照常获得。
TEST_F(PlayerItemGainBlockTest, BannedConfigIsRejectedByEveryBagEntryForThisPlayerOnly)
{
	ASSERT_NO_FATAL_FAILURE(Ban(kStackItem));
	auto &inventory = Inventory(banned_);
	const auto issuedBefore = ItemIdsIssued();

	EXPECT_EQ(static_cast<uint32_t>(kAssetBlocked), BagService::AddItem(banned_, inventory, Piece(kStackItem, 1)));
	EXPECT_EQ(static_cast<uint32_t>(kAssetBlocked), BagService::AddItems(banned_, inventory, ItemCountMap{{kStackItem, 1}}));
	EXPECT_EQ(static_cast<uint32_t>(kAssetBlocked),
			  BagService::AddItems(banned_, inventory, std::vector<InitItemParam>{Piece(kStackItem, 1)}));
	EXPECT_EQ(0u, inventory.GetTotalItemCount(kStackItem));
	EXPECT_EQ(0u, inventory.OccupiedGridCount());
	EXPECT_EQ(issuedBefore, ItemIdsIssued()) << "封禁在容器规划之前判,一个号都不铸";

	EXPECT_EQ(static_cast<uint32_t>(kSuccess), BagService::AddItem(banned_, inventory, Piece(kMaterialItem, 1)))
		<< "同一名玩家的其它配置照常";
	EXPECT_EQ(static_cast<uint32_t>(kSuccess), BagService::AddItem(other_, Inventory(other_), Piece(kStackItem, 2)))
		<< "另一名玩家照常获得被封的配置";
	EXPECT_EQ(2u, Inventory(other_).GetTotalItemCount(kStackItem));
	EXPECT_FALSE(GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kItem, kStackItem));
}

// 混合批次里有一项被封禁:整批拒绝,未封禁的那几项也一件不发,不留半笔。
TEST_F(PlayerItemGainBlockTest, MixedBatchWithOneBannedConfigGrantsNothing)
{
	ASSERT_NO_FATAL_FAILURE(Ban(kNonStackItem));
	auto &inventory = Inventory(banned_);
	const auto issuedBefore = ItemIdsIssued();

	EXPECT_EQ(static_cast<uint32_t>(kAssetBlocked),
			  BagService::AddItems(banned_, inventory, ItemCountMap{{kMaterialItem, 3}, {kNonStackItem, 1}, {kOtherNonStack, 1}}));
	EXPECT_EQ(static_cast<uint32_t>(kAssetBlocked),
			  BagService::AddItems(banned_, inventory,
								   std::vector<InitItemParam>{Piece(kMaterialItem, 3), Piece(kNonStackItem, 1)}));
	EXPECT_EQ(0u, inventory.GetTotalItemCount(kMaterialItem));
	EXPECT_EQ(0u, inventory.GetTotalItemCount(kNonStackItem));
	EXPECT_EQ(0u, inventory.GetTotalItemCount(kOtherNonStack));
	EXPECT_EQ(0u, inventory.OccupiedGridCount());
	EXPECT_EQ(issuedBefore, ItemIdsIssued());
}

// 战斗结算的整笔换取:被封禁的掉落在扣材料、建候选、调外层回调(发币)之前就整笔拒绝;
// 主包放不下时的临时格兜底只用于容量不足,封禁不会走到它。解封后同一笔照常落到临时格,证明拦住它的就是封禁。
TEST_F(PlayerItemGainBlockTest, ConsumeInventoryAndGrantRejectsBeforeConsumingAndNeverFallsBackToTemporary)
{
	auto &inventory = Inventory(banned_);
	auto &temporary = Temporary(banned_);
	inventory.SetCapacityForRestore(1);
	temporary.SetCapacityForRestore(1);
	inventory.InsertItemForRestore(950400101, kMaterialItem, 3, 0); // 存档还原:不是新增获得
	ASSERT_EQ(3u, inventory.GetTotalItemCount(kMaterialItem));
	ASSERT_NO_FATAL_FAILURE(Ban(kNonStackItem));

	int credits = 0;
	const auto creditGold = [&credits, player = banned_] {
		++credits;
		return CurrencySystem::AddCurrency(player, kCurrencyGold, 5);
	};
	EXPECT_EQ(static_cast<uint32_t>(kAssetBlocked),
			  BagService::ConsumeInventoryAndGrant(banned_, {{kMaterialItem, 1}}, {{kNonStackItem, 1}}, 7101,
												   "item-ban-test", creditGold));
	EXPECT_EQ(0, credits) << "外层回调(发币)不调";
	EXPECT_EQ(0u, CurrencySystem::GetBalance(banned_, kCurrencyGold));
	EXPECT_EQ(3u, inventory.GetTotalItemCount(kMaterialItem)) << "材料一件不扣";
	EXPECT_EQ(0u, inventory.GetTotalItemCount(kNonStackItem));
	EXPECT_EQ(0u, temporary.OccupiedGridCount()) << "不改投临时格";

	ASSERT_EQ(static_cast<uint32_t>(kSuccess), GainBlockService::UnblockPlayerItemGain(banned_, kNonStackItem));
	ASSERT_EQ(static_cast<uint32_t>(kSuccess),
			  BagService::ConsumeInventoryAndGrant(banned_, {{kMaterialItem, 1}}, {{kNonStackItem, 1}}, 7102,
												   "item-ban-test", creditGold));
	EXPECT_EQ(1, credits);
	EXPECT_EQ(5u, CurrencySystem::GetBalance(banned_, kCurrencyGold));
	EXPECT_EQ(2u, inventory.GetTotalItemCount(kMaterialItem));
	EXPECT_EQ(1u, temporary.GetTotalItemCount(kNonStackItem)) << "解封之后主包放不下才落临时格";
}

// 整笔换取的新增里混着一项被封禁的配置:整笔拒绝,另一项掉落也不发。
TEST_F(PlayerItemGainBlockTest, ConsumeInventoryAndGrantMixedGainsGrantNothing)
{
	auto &inventory = Inventory(banned_);
	inventory.InsertItemForRestore(950400102, kMaterialItem, 2, 0);
	ASSERT_NO_FATAL_FAILURE(Ban(kStackItem));

	EXPECT_EQ(static_cast<uint32_t>(kAssetBlocked),
			  BagService::ConsumeInventoryAndGrant(banned_, {{kMaterialItem, 1}}, {{kOtherNonStack, 1}, {kStackItem, 4}}, 7103,
												   "item-ban-test"));
	EXPECT_EQ(2u, inventory.GetTotalItemCount(kMaterialItem));
	EXPECT_EQ(0u, inventory.GetTotalItemCount(kOtherNonStack));
	EXPECT_EQ(0u, inventory.GetTotalItemCount(kStackItem));
	EXPECT_EQ(0u, Temporary(banned_).OccupiedGridCount());
}

// 封禁只挡今后的新增获得:封禁之前已持有的不扣、照常可用(消耗、整理);存档还原与同一所有者内部搬运
// (背包 → 仓库,与装备穿脱同一对 TakeInstance / PutInstance 原语)不经过获得判定。
TEST_F(PlayerItemGainBlockTest, ExistingStockStaysAndRestoreOrInternalMovesAreNotGains)
{
	auto &inventory = Inventory(banned_);
	auto &warehouse = Warehouse(banned_);
	constexpr Guid kHeldInstance = 950400103;
	inventory.InsertItemForRestore(950400104, kStackItem, 5, 0);
	inventory.InsertItemForRestore(kHeldInstance, kNonStackItem, 1, 1);
	ASSERT_NO_FATAL_FAILURE(Ban(kStackItem));
	ASSERT_NO_FATAL_FAILURE(Ban(kNonStackItem));

	EXPECT_EQ(5u, inventory.GetTotalItemCount(kStackItem)) << "封禁不扣已有库存";
	EXPECT_EQ(1u, inventory.GetTotalItemCount(kNonStackItem));

	inventory.InsertItemForRestore(950400105, kStackItem, 2, 2); // 存档还原不是新增获得
	EXPECT_EQ(7u, inventory.GetTotalItemCount(kStackItem));

	ItemComp carried;
	ASSERT_EQ(static_cast<uint32_t>(kSuccess), inventory.TakeInstance(kHeldInstance, carried));
	ASSERT_EQ(static_cast<uint32_t>(kSuccess), warehouse.PutInstance(carried)) << "同一所有者内部搬运不受封禁影响";
	EXPECT_EQ(1u, warehouse.GetTotalItemCount(kNonStackItem));
	EXPECT_EQ(0u, inventory.GetTotalItemCount(kNonStackItem));

	EXPECT_EQ(static_cast<uint32_t>(kSuccess),
			  BagService::MergeAndCompact(banned_, inventory, CompactPolicy::kMergeAndReorder)) << "整理不是新增获得";
	EXPECT_EQ(7u, inventory.GetTotalItemCount(kStackItem));
	EXPECT_EQ(static_cast<uint32_t>(kSuccess),
			  BagService::RemoveItemsExact(banned_, inventory, ItemCountMap{{kStackItem, 3}}, TX_ITEM_DESTROY))
		<< "已有库存照常可消耗";
	EXPECT_EQ(4u, inventory.GetTotalItemCount(kStackItem));

	EXPECT_EQ(static_cast<uint32_t>(kAssetBlocked), BagService::AddItem(banned_, inventory, Piece(kStackItem, 1)))
		<< "新增获得仍被拦";
}

// 解封之后照常获得;重复封禁 / 解封都是幂等的。
TEST_F(PlayerItemGainBlockTest, UnbanRestoresGainsAndRepeatsAreIdempotent)
{
	ASSERT_NO_FATAL_FAILURE(Ban(kStackItem));
	ASSERT_NO_FATAL_FAILURE(Ban(kStackItem));
	EXPECT_EQ(1, tlsEcs.actorRegistry.get<PlayerItemGainBlockComp>(banned_).item_config_ids_size());
	EXPECT_EQ(static_cast<uint32_t>(kAssetBlocked), BagService::AddItem(banned_, Inventory(banned_), Piece(kStackItem, 1)));

	ASSERT_EQ(static_cast<uint32_t>(kSuccess), GainBlockService::UnblockPlayerItemGain(banned_, kStackItem));
	ASSERT_EQ(static_cast<uint32_t>(kSuccess), GainBlockService::UnblockPlayerItemGain(banned_, kStackItem));
	EXPECT_EQ(static_cast<uint32_t>(kSuccess), BagService::AddItem(banned_, Inventory(banned_), Piece(kStackItem, 1)));
	EXPECT_EQ(1u, Inventory(banned_).GetTotalItemCount(kStackItem));
}

// 全服封禁与单人封禁互相独立:全服封了谁都拿不到;解除全服封禁不会解开单人封禁,反之亦然。
TEST_F(PlayerItemGainBlockTest, GlobalAndPlayerBansAreIndependent)
{
	ASSERT_NO_FATAL_FAILURE(Ban(kStackItem));
	{
		const ScopedGlobalItemBan global(kOtherNonStack);
		EXPECT_EQ(static_cast<uint32_t>(kAssetBlocked), BagService::AddItem(banned_, Inventory(banned_), Piece(kOtherNonStack, 1)));
		EXPECT_EQ(static_cast<uint32_t>(kAssetBlocked), BagService::AddItem(other_, Inventory(other_), Piece(kOtherNonStack, 1)))
			<< "全服封禁对所有玩家生效";
		EXPECT_FALSE(GainBlockService::IsPlayerItemGainBlocked(banned_, kOtherNonStack)) << "全服封禁不写进玩家存档";
	}
	EXPECT_EQ(static_cast<uint32_t>(kSuccess), BagService::AddItem(banned_, Inventory(banned_), Piece(kOtherNonStack, 1)));
	EXPECT_EQ(static_cast<uint32_t>(kAssetBlocked), BagService::AddItem(banned_, Inventory(banned_), Piece(kStackItem, 1)))
		<< "解除全服封禁不影响单人封禁";
	EXPECT_EQ(static_cast<uint32_t>(kSuccess), BagService::AddItem(other_, Inventory(other_), Piece(kStackItem, 1)));
}

// 判定的口径(见 gain_block_service.h):无效句柄没有玩家,只判全服;有效玩家缺封禁表(加载不变量被破坏)
// 按「没有单人封禁」回答、不补建组件 —— 这种实体的存盘会被整份拒绝(PlayerFeaturePersistenceTest),
// 发出去的物品成不了盘上事实。
TEST_F(PlayerItemGainBlockTest, ReadPathNeverCreatesTheBanComponent)
{
	ASSERT_NO_FATAL_FAILURE(Ban(kStackItem));
	EXPECT_FALSE(GainBlockService::IsItemGainBlocked(entt::null, kStackItem));

	tlsEcs.actorRegistry.remove<PlayerItemGainBlockComp>(banned_);
	EXPECT_FALSE(GainBlockService::IsItemGainBlocked(banned_, kStackItem));
	EXPECT_EQ(static_cast<uint32_t>(kSuccess), BagService::AddItem(banned_, Inventory(banned_), Piece(kStackItem, 1)));
	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerItemGainBlockComp>(banned_)) << "读路径不改变实体的组件集合";
	EXPECT_EQ(static_cast<uint32_t>(kInvalidParameter), GainBlockService::BlockPlayerItemGain(banned_, kStackItem))
		<< "写路径只取不建";
	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerItemGainBlockComp>(banned_));
}
