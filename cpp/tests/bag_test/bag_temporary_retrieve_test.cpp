// 临时格取回(审计 cpp-economy#2,docs/design/bag-rule-policy-layering.md §6.6)。
//
// 三层各钉各的:
//   * 容器 Bag::MoveInstancesTo —— 两包候选上的全或无搬运:装备原样(guid / equip 段不变)、可叠加先并堆
//     (余量保留原 guid,整堆并入的原 guid 进回执)、不铸号、放不下两包都不动;
//   * 玩法入口 PlayerBagSystem::RetrieveTemporary —— 参数二选一、交接冻结中拒绝(战斗中放行)、
//     取回已不在临时格的那一件(重发)回 kBagTemporaryItemNotFound、整格取回、人物背包放不下回
//     kBagTemporaryRetrieveBagFull;
//   * handler —— 经生成的 CallMethod 分发,成功回人物背包与临时格两份全量,失败不带半份快照。
#include <gtest/gtest.h>
#include <google/protobuf/unknown_field_set.h>

#include <cstdint>
#include <map>
#include <memory>
#include <string>
#include <vector>

#include "../../nodes/scene/handler/rpc/player/player_bag_handler.h"
#include "engine/core/type_define/type_define.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/item_base_comp.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/system/player_feature_snapshot.h"
#include "table/code/item_table.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
// 物品表里的真实行(data/Item.xlsx):1、2 号是不可叠加的装备,9 号每堆最多 2 个,2001 号是可叠加的强化石。
constexpr uint32_t kRetrieveEquip = 1;
constexpr uint32_t kRetrieveEquip2 = 2;
constexpr uint32_t kRetrievePair = 9;
constexpr uint32_t kRetrieveStone = 2001;

constexpr Guid kEquipGuid = 7700001;
constexpr Guid kPairInTemporary = 7700002;
constexpr Guid kPairInInventory = 7700003;
constexpr Guid kStoneGuid = 7700004;
constexpr Guid kOtherGuid = 7700005;
constexpr Guid kSecondEquipGuid = 7700006;

ItemComp MakeInstance(Guid guid, uint32_t configId, uint32_t size)
{
	ItemComp item;
	item.set_item_id(guid);
	item.set_config_id(configId);
	item.set_size(size);
	return item;
}

// 一件「已初始化」的装备:带一条随机属性、强化到 +3、嵌了一颗宝石 —— 搬运必须一个字节都不改。
ItemComp MakeForgedEquip(Guid guid)
{
	ItemComp item = MakeInstance(guid, kRetrieveEquip, 1);
	auto *equip = item.mutable_equip();
	auto *affix = equip->add_affixes();
	affix->set_attr_id(1);
	affix->set_tier(1);
	affix->set_value(7);
	equip->set_enhance_level(3);
	auto *gem = equip->add_gems();
	gem->set_socket(0);
	gem->set_gem_config_id(2101);
	return item;
}

// 一个包此刻的全部状态:每个实例的序列化字节 + 槽位表。拒绝路径要求两包都与之前逐字节相同。
struct BagBytes
{
	std::map<Guid, std::string> items;
	std::map<uint32_t, Guid> slots;
	bool operator==(const BagBytes &) const = default;
};

BagBytes Capture(const Bag &bag)
{
	BagBytes state;
	bag.ForEachItem([&state](Guid guid, const ItemComp &item) { state.items.emplace(guid, item.SerializeAsString()); });
	for (const auto &[slot, guid] : bag.GridSlots())
	{
		state.slots.emplace(slot, guid);
	}
	return state;
}

// 比较「除了入包序号之外」是不是同一份实例:搬运会让目标包重新盖章,其余字段(含 equip 段)必须原样。
std::string BytesWithoutAcquireSeq(ItemComp item)
{
	item.clear_acquire_seq();
	return item.SerializeAsString();
}

uint64_t ItemGuidsAvailable()
{
	return tlsGuidSegmentRegistry.Get(GuidKind::kItem).Available();
}

class RetrieveTestBagService : public SceneBagClientPlayer
{
};

class BagMoveInstancesTest : public testing::Test
{
protected:
	void SetUp() override
	{
		ItemTableManager::Instance().Load();
		ASSERT_NE(nullptr, ItemTableManager::Instance().FindByIdSilent(kRetrieveEquip).first);
		ASSERT_EQ(1u, ItemTableManager::Instance().FindByIdSilent(kRetrieveEquip).first->max_stack_size());
		ASSERT_NE(nullptr, ItemTableManager::Instance().FindByIdSilent(kRetrieveEquip2).first);
		ASSERT_EQ(1u, ItemTableManager::Instance().FindByIdSilent(kRetrieveEquip2).first->max_stack_size());
		ASSERT_NE(nullptr, ItemTableManager::Instance().FindByIdSilent(kRetrievePair).first);
		ASSERT_EQ(2u, ItemTableManager::Instance().FindByIdSilent(kRetrievePair).first->max_stack_size());
		ASSERT_NE(nullptr, ItemTableManager::Instance().FindByIdSilent(kRetrieveStone).first);
		ASSERT_GT(ItemTableManager::Instance().FindByIdSilent(kRetrieveStone).first->max_stack_size(), 1u);
		temporary.SetProfile(BagProfile::Flat(5));
		inventory.SetProfile(BagProfile::Flat(5));
	}

	Bag temporary;
	Bag inventory;
};

// 装备整份搬过去:guid、随机属性、强化等级、宝石一个都不变,只有入包序号由人物背包重新盖章。不铸号、不出回执。
TEST_F(BagMoveInstancesTest, EquipmentMovesWithItsIdentityAndInstanceData)
{
	const ItemComp forged = MakeForgedEquip(kEquipGuid);
	temporary.RestoreItems({{forged, 3}});
	inventory.RestoreItems({{MakeInstance(kOtherGuid, kRetrieveStone, 4), 0}});
	const uint64_t guidsBefore = ItemGuidsAvailable();

	std::vector<DestroyedInstance> absorbed;
	ASSERT_EQ(kSuccess, temporary.MoveInstancesTo(inventory, {kEquipGuid}, &absorbed));

	EXPECT_TRUE(absorbed.empty());
	EXPECT_FALSE(temporary.ContainsGuid(kEquipGuid));
	EXPECT_EQ(0u, temporary.OccupiedGridCount());
	const ItemComp *moved = inventory.GetItemCompByGuid(kEquipGuid);
	ASSERT_NE(nullptr, moved);
	EXPECT_EQ(BytesWithoutAcquireSeq(forged), BytesWithoutAcquireSeq(*moved)) << "the instance is carried byte for byte";
	EXPECT_TRUE(moved->has_equip());
	EXPECT_NE(0u, moved->acquire_seq()) << "the inventory stamps its own acquire order";
	EXPECT_EQ(guidsBefore, ItemGuidsAvailable()) << "moving never mints an item guid";
	EXPECT_TRUE(temporary.IsLayerConsistent());
	EXPECT_TRUE(inventory.IsLayerConsistent());
}

// 可叠加:先灌进人物背包里同种物品的未满堆;灌不完的余量仍用临时格那一堆原来的 guid。
TEST_F(BagMoveInstancesTest, StackableFillsExistingStacksAndTheRemainderKeepsItsGuid)
{
	temporary.RestoreItems({{MakeInstance(kPairInTemporary, kRetrievePair, 2), 0}});
	inventory.RestoreItems({{MakeInstance(kPairInInventory, kRetrievePair, 1), 0}});

	std::vector<DestroyedInstance> absorbed;
	ASSERT_EQ(kSuccess, temporary.MoveInstancesTo(inventory, {kPairInTemporary}, &absorbed));

	EXPECT_TRUE(absorbed.empty()) << "part of the stack is left over, so its guid survives";
	ASSERT_NE(nullptr, inventory.GetItemCompByGuid(kPairInInventory));
	EXPECT_EQ(2u, inventory.GetItemCompByGuid(kPairInInventory)->size()) << "the open stack was topped up first";
	ASSERT_NE(nullptr, inventory.GetItemCompByGuid(kPairInTemporary));
	EXPECT_EQ(1u, inventory.GetItemCompByGuid(kPairInTemporary)->size());
	EXPECT_EQ(3u, inventory.GetTotalItemCount(kRetrievePair)) << "no unit is created or lost";
	EXPECT_EQ(0u, temporary.OccupiedGridCount());
	EXPECT_TRUE(inventory.IsLayerConsistent());
}

// 整堆都并进去了:原 guid 退役,进回执(数量 0:数量没有消失),人物背包不多占一格。
TEST_F(BagMoveInstancesTest, FullyAbsorbedStackRetiresItsGuidIntoTheReceipt)
{
	temporary.RestoreItems({{MakeInstance(kPairInTemporary, kRetrievePair, 1), 0}});
	inventory.RestoreItems({{MakeInstance(kPairInInventory, kRetrievePair, 1), 0}});

	std::vector<DestroyedInstance> absorbed;
	ASSERT_EQ(kSuccess, temporary.MoveInstancesTo(inventory, {kPairInTemporary}, &absorbed));

	ASSERT_EQ(1u, absorbed.size());
	EXPECT_EQ(kPairInTemporary, absorbed.front().guid);
	EXPECT_EQ(kRetrievePair, absorbed.front().configId);
	EXPECT_EQ(0u, absorbed.front().size) << "the quantity moved into another stack; nothing was destroyed";
	EXPECT_FALSE(inventory.ContainsGuid(kPairInTemporary));
	EXPECT_EQ(2u, inventory.GetTotalItemCount(kRetrievePair));
	EXPECT_EQ(1u, inventory.OccupiedGridCount());
	EXPECT_EQ(0u, temporary.OccupiedGridCount());
}

// 带着本版本不认识的字段(新版本加的物品属性)的堆不并:原样另占一格,未知字段一个都不丢。
TEST_F(BagMoveInstancesTest, StackCarryingNewerVersionFieldsMovesAsItsOwnInstance)
{
	ItemComp newer = MakeInstance(kStoneGuid, kRetrieveStone, 3);
	newer.mutable_unknown_fields()->AddVarint(412345, 1);
	temporary.RestoreItems({{newer, 0}});
	inventory.RestoreItems({{MakeInstance(kOtherGuid, kRetrieveStone, 4), 0}});

	ASSERT_EQ(kSuccess, temporary.MoveInstancesTo(inventory, {kStoneGuid}));

	const ItemComp *moved = inventory.GetItemCompByGuid(kStoneGuid);
	ASSERT_NE(nullptr, moved);
	EXPECT_EQ(3u, moved->size()) << "it is not merged into the ordinary stack";
	EXPECT_EQ(BytesWithoutAcquireSeq(newer), BytesWithoutAcquireSeq(*moved)) << "unknown fields survive the move";
	EXPECT_EQ(4u, inventory.GetItemCompByGuid(kOtherGuid)->size());
}

// 全或无:人物背包只够放一件、要搬两件 —— 一件都不搬,两个包逐字节不变,回执不动。
TEST_F(BagMoveInstancesTest, TargetWithoutRoomForTheWholeBatchLeavesBothBagsUntouched)
{
	inventory.SetCapacityForRestore(2);
	inventory.RestoreItems({{MakeInstance(kOtherGuid, kRetrieveStone, 4), 0}});
	temporary.RestoreItems({{MakeForgedEquip(kEquipGuid), 0}, {MakeInstance(kSecondEquipGuid, kRetrieveEquip2, 1), 1}});
	const BagBytes temporaryBefore = Capture(temporary);
	const BagBytes inventoryBefore = Capture(inventory);

	std::vector<DestroyedInstance> absorbed{{kOtherGuid, kRetrieveStone, 0}};
	EXPECT_EQ(kBagAddItemBagFull, temporary.MoveInstancesTo(inventory, {kEquipGuid, kSecondEquipGuid}, &absorbed));

	EXPECT_EQ(temporaryBefore, Capture(temporary));
	EXPECT_EQ(inventoryBefore, Capture(inventory));
	EXPECT_EQ(1u, absorbed.size()) << "a refused move leaves the receipt as it was";
}

// 编程错误 / 坏输入一律在任何修改之前拒绝:空列表、重复 guid、不在源包、目标已有同 guid、源包就是目标包。
TEST_F(BagMoveInstancesTest, BadGuidListsAreRefusedBeforeAnything)
{
	temporary.RestoreItems({{MakeForgedEquip(kEquipGuid), 0}});
	inventory.RestoreItems({{MakeInstance(kOtherGuid, kRetrieveStone, 4), 0}});
	const BagBytes temporaryBefore = Capture(temporary);
	const BagBytes inventoryBefore = Capture(inventory);

	EXPECT_EQ(kBagAddItemInvalidParam, temporary.MoveInstancesTo(inventory, {}));
	EXPECT_EQ(kBagAddItemInvalidParam, temporary.MoveInstancesTo(inventory, {kEquipGuid, kEquipGuid}));
	EXPECT_EQ(kBagDeleteItemFindGuid, temporary.MoveInstancesTo(inventory, {kEquipGuid, kStoneGuid}));
	EXPECT_EQ(kBagAddItemInvalidParam, temporary.MoveInstancesTo(temporary, {kEquipGuid}));
	inventory.RestoreItems({{MakeForgedEquip(kEquipGuid), 3}}); // 两包里同一个身份 = 数据已经坏了,不再搬出第三份
	const BagBytes inventoryWithClash = Capture(inventory);
	EXPECT_EQ(kBagDeleteItemAlreadyHasGuid, temporary.MoveInstancesTo(inventory, {kEquipGuid}));

	EXPECT_EQ(temporaryBefore, Capture(temporary));
	EXPECT_EQ(inventoryWithClash, Capture(inventory));
	EXPECT_NE(inventoryBefore, inventoryWithClash);
}

// ── 玩法入口与 handler ───────────────────────────────────────────────────────

class TemporaryRetrieveTest : public testing::Test
{
protected:
	void SetUp() override
	{
		ItemTableManager::Instance().Load();
		player = tlsEcs.actorRegistry.create();
		auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
		for (auto &bag : bags.bags)
		{
			bag.SetPlayerGuid(Guid{770000});
		}
	}
	void TearDown() override
	{
		if (tlsEcs.actorRegistry.valid(player))
		{
			tlsEcs.actorRegistry.destroy(player);
		}
	}
	Bag &Inventory() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory]; }
	Bag &Temporary() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kTemporary]; }

	// 临时格里两样东西:一件锻造过的装备、一堆强化石。
	void FillTemporary()
	{
		Temporary().RestoreItems({{MakeForgedEquip(kEquipGuid), 0}, {MakeInstance(kStoneGuid, kRetrieveStone, 6), 1}});
	}
	void ExpectBothBagsUnchanged(const BagBytes &inventoryBefore, const BagBytes &temporaryBefore)
	{
		EXPECT_EQ(inventoryBefore, Capture(Inventory()));
		EXPECT_EQ(temporaryBefore, Capture(Temporary()));
	}

	entt::entity player{entt::null};
	SceneBagClientPlayerHandler handler{std::make_unique<RetrieveTestBagService>()};
};

// 纯参数错误先于状态判定:两样都给或都不给;实体无效;没有背包组件(也不许顺手造一个)。
TEST_F(TemporaryRetrieveTest, ParametersAndMissingStateAreRefusedFirst)
{
	FillTemporary();
	const BagBytes inventoryBefore = Capture(Inventory());
	const BagBytes temporaryBefore = Capture(Temporary());

	EXPECT_EQ(kInvalidParameter, PlayerBagSystem::RetrieveTemporary(player, 0, false));
	EXPECT_EQ(kInvalidParameter, PlayerBagSystem::RetrieveTemporary(player, kEquipGuid, true));
	EXPECT_EQ(kThisEntityIsInvalid, PlayerBagSystem::RetrieveTemporary(entt::null, kEquipGuid, false));
	ExpectBothBagsUnchanged(inventoryBefore, temporaryBefore);

	const auto bagless = tlsEcs.actorRegistry.create();
	EXPECT_EQ(kServiceUnavailable, PlayerBagSystem::RetrieveTemporary(bagless, 0, true));
	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerBagsComp>(bagless)) << "a refused call must not create the bags";
	tlsEcs.actorRegistry.destroy(bagless);
}

// 跨节点交接冻结中拒,两个包不动。战斗中放行(2026-11-06 评审修正):两包都放不下而整笔延后的那一局,
// InBattleComp 要等结算应用之后才摘,战斗中也拒的话玩家在冻结到期之前取不回、腾不出位置,延后的结算就补不上
// (结算一侧的回归:PlayerBattleSettlementItemTest.InboxDeferredForFullBagsAppliesAfterARetrieveDuringTheBattle)。
TEST_F(TemporaryRetrieveTest, RefusedWhileFrozenButAllowedInBattle)
{
	FillTemporary();
	const BagBytes inventoryBefore = Capture(Inventory());
	const BagBytes temporaryBefore = Capture(Temporary());

	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	EXPECT_EQ(kBagTemporaryRetrieveBusy, PlayerBagSystem::RetrieveTemporary(player, kEquipGuid, false));
	EXPECT_EQ(kBagTemporaryRetrieveBusy, PlayerBagSystem::RetrieveTemporary(player, 0, true));
	// 冻结与战斗同时在:冻结照样拒(战斗放行不能把冻结闸带掉)。
	tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(4242);
	EXPECT_EQ(kBagTemporaryRetrieveBusy, PlayerBagSystem::RetrieveTemporary(player, kEquipGuid, false));
	ExpectBothBagsUnchanged(inventoryBefore, temporaryBefore);

	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
	ASSERT_EQ(kSuccess, PlayerBagSystem::RetrieveTemporary(player, kEquipGuid, false)) << "in battle the retrieve is allowed";
	EXPECT_TRUE(Inventory().ContainsGuid(kEquipGuid));
	EXPECT_FALSE(Temporary().ContainsGuid(kEquipGuid));
	EXPECT_TRUE(tlsEcs.actorRegistry.any_of<InBattleComp>(player)) << "retrieving never touches the battle freeze";
}

// 取回一件:只动这一件。原样重发(上一次的回包丢了)= 它已经不在临时格,回 kBagTemporaryItemNotFound、两包不动。
TEST_F(TemporaryRetrieveTest, RetrievingOneItemMovesOnlyItAndAReplayIsNotFound)
{
	FillTemporary();

	ASSERT_EQ(kSuccess, PlayerBagSystem::RetrieveTemporary(player, kEquipGuid, false));
	EXPECT_TRUE(Inventory().ContainsGuid(kEquipGuid));
	EXPECT_FALSE(Temporary().ContainsGuid(kEquipGuid));
	EXPECT_TRUE(Temporary().ContainsGuid(kStoneGuid)) << "the rest stays in the temporary bag";

	const BagBytes inventoryBefore = Capture(Inventory());
	const BagBytes temporaryBefore = Capture(Temporary());
	EXPECT_EQ(kBagTemporaryItemNotFound, PlayerBagSystem::RetrieveTemporary(player, kEquipGuid, false));
	EXPECT_EQ(kBagTemporaryItemNotFound, PlayerBagSystem::RetrieveTemporary(player, kOtherGuid, false));
	ExpectBothBagsUnchanged(inventoryBefore, temporaryBefore);
}

// 整格取回:全部一次搬完;临时格空了之后再来一次 = 成功、什么都不动(重发的「全部取回」也落在这里)。
TEST_F(TemporaryRetrieveTest, RetrievingAllEmptiesTheTemporaryBagAndAnEmptyRetryIsANoOp)
{
	FillTemporary();
	Inventory().RestoreItems({{MakeInstance(kOtherGuid, kRetrieveStone, 4), 0}});

	ASSERT_EQ(kSuccess, PlayerBagSystem::RetrieveTemporary(player, 0, true));
	EXPECT_EQ(0u, Temporary().OccupiedGridCount());
	EXPECT_TRUE(Inventory().ContainsGuid(kEquipGuid));
	EXPECT_EQ(10u, Inventory().GetTotalItemCount(kRetrieveStone)) << "the stones merged into the open stack";
	EXPECT_FALSE(Inventory().ContainsGuid(kStoneGuid)) << "the absorbed stack retired its guid";

	const BagBytes inventoryBefore = Capture(Inventory());
	const BagBytes temporaryBefore = Capture(Temporary());
	EXPECT_EQ(kSuccess, PlayerBagSystem::RetrieveTemporary(player, 0, true));
	ExpectBothBagsUnchanged(inventoryBefore, temporaryBefore);
}

// 人物背包放不下这一批:整批拒绝(换成本入口自己的码),两个包逐字节不变;腾出一格后一件一件取就能取。
TEST_F(TemporaryRetrieveTest, InventoryWithoutRoomRefusesTheWholeBatch)
{
	FillTemporary();
	Inventory().SetCapacityForRestore(1);
	const BagBytes inventoryBefore = Capture(Inventory());
	const BagBytes temporaryBefore = Capture(Temporary());

	EXPECT_EQ(kBagTemporaryRetrieveBagFull, PlayerBagSystem::RetrieveTemporary(player, 0, true));
	ExpectBothBagsUnchanged(inventoryBefore, temporaryBefore);

	EXPECT_EQ(kSuccess, PlayerBagSystem::RetrieveTemporary(player, kStoneGuid, false)) << "one at a time still fits";
	EXPECT_EQ(kBagTemporaryRetrieveBagFull, PlayerBagSystem::RetrieveTemporary(player, kEquipGuid, false));
	EXPECT_TRUE(Temporary().ContainsGuid(kEquipGuid)) << "a refused item stays where it was";
}

// handler:参数错误与业务拒绝只回 tip、不带半份快照;成功回人物背包与临时格两份全量(已经是搬完之后的样子)。
TEST_F(TemporaryRetrieveTest, HandlerReturnsBothBagsAndDoesNotLeakTips)
{
	const auto *method = SceneBagClientPlayer::descriptor()->FindMethodByName("RetrieveTemporaryItems");
	ASSERT_NE(nullptr, method) << "SceneBagClientPlayer.RetrieveTemporaryItems is missing: run proto-gen first";
	Temporary().RestoreItems({{MakeInstance(kStoneGuid, kRetrieveStone, 6), 0}});

	RetrieveTemporaryItemsRequest request;
	RetrieveTemporaryItemsResponse response;
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(kInvalidParameter, response.error_message().id());
	EXPECT_FALSE(response.has_bag());
	EXPECT_FALSE(response.has_temporary());

	request.set_item_id(kOtherGuid);
	response.Clear();
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(kBagTemporaryItemNotFound, response.error_message().id());
	EXPECT_FALSE(response.has_bag());
	EXPECT_FALSE(response.has_temporary());

	request.set_item_id(kStoneGuid);
	response.Clear();
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(0u, response.error_message().id());
	ASSERT_TRUE(response.has_bag());
	ASSERT_TRUE(response.has_temporary());
	EXPECT_EQ(uint32_t{kInventory}, response.bag().layout().bag_type());
	EXPECT_EQ(uint32_t{kTemporary}, response.temporary().layout().bag_type());
	ASSERT_EQ(1, response.bag().items_size());
	EXPECT_EQ(kStoneGuid, response.bag().items(0).item_id());
	EXPECT_EQ(6u, response.bag().items(0).count());
	EXPECT_EQ(0, response.temporary().items_size());
}

// ── 两包都满时的日志节拍(§6.6「日志节拍」)─────────────────────────────────
// metric=bag_grant_overflow_rejected:首次打一条,之后每连续 8 次一条,其余只打 DEBUG。8 的推导(封顶后约 8 分钟一条,
// 落在 [10m] 日志窗口之内)写在 BagService::kGrantOverflowWarnEveryRejections 上;改节拍时这条与那段推导一起改。
// 连续次数怎么增减由 PlayerBattleSettlementItemTest.InboxDeferredForFullBagsAppliesAfterARetrieveDuringTheBattle 钉住。
TEST(GrantOverflowLogCadenceTest, WarnsOnTheFirstRejectionAndThenEveryEighth)
{
	ASSERT_EQ(8u, BagService::kGrantOverflowWarnEveryRejections);
	EXPECT_FALSE(BagService::IsGrantOverflowWarnDue(0)) << "nothing has been rejected yet";
	EXPECT_TRUE(BagService::IsGrantOverflowWarnDue(1)) << "the first rejection is always logged";
	for (uint32_t rejections = 2; rejections < 8; ++rejections)
	{
		EXPECT_FALSE(BagService::IsGrantOverflowWarnDue(rejections)) << "rejections=" << rejections;
	}
	EXPECT_TRUE(BagService::IsGrantOverflowWarnDue(8));
	EXPECT_FALSE(BagService::IsGrantOverflowWarnDue(9));
	EXPECT_TRUE(BagService::IsGrantOverflowWarnDue(16));
}
} // namespace
