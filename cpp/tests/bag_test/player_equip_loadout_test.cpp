#include <gtest/gtest.h>

#include <array>
#include <cstdint>
#include <map>
#include <string>
#include <vector>

#include "../../nodes/scene/handler/rpc/player/player_bag_handler.h"
#include "engine/core/time/system/time.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_attribute_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_equip_loadout_comp.pb.h"
#include "proto/common/component/player_item_gain_block_comp.pb.h"
#include "proto/common/component/player_item_hold_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/common/database/mysql_database_table.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "services/scene/player/comp/last_persisted_snapshot_comp.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/equip_loadout_rules.h"
#include "services/scene/player/system/player_attribute.h"
#include "services/scene/player/system/player_equip_loadout.h"
#include "table/code/attributeallocratio_table.h"
#include "table/code/attributedimension_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/attributerule_table.h"
#include "table/code/class_table.h"
#include "table/code/equipaffixpool_table.h"
#include "table/code/equipaffixrule_table.h"
#include "table/code/equipattribute_table.h"
#include "table/code/equipattributecap_table.h"
#include "table/code/equipenhance_table.h"
#include "table/code/equipgem_table.h"
#include "table/code/equipgemsocket_table.h"
#include "table/code/equipslot_table.h"
#include "table/code/item_table.h"
#include "table/code/skill_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/equip_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{

constexpr uint64_t kPlayerId = 980001;
constexpr uint64_t kOwnerEpoch = 17;

// 正式 8 槽对应模板 ID（均为 1 级通用装备）
constexpr std::array<uint32_t, 8> kFormalConfigIds = {
	1101u, // slot 3 武器 (kind 11)
	1201u, // slot 4 帽子 (kind 12)
	1301u, // slot 5 衣服 (kind 13)
	1401u, // slot 6 鞋子 (kind 14)
	3201u, // slot 7 首饰一 (kind 15)
	3201u, // slot 8 首饰二 (kind 15)
	3301u, // slot 9 法宝一 (kind 16)
	3301u, // slot 10 法宝二 (kind 16)
};

int gPersistCalls = 0;
bool RecordPersistForTest(entt::entity)
{
	++gPersistCalls;
	return true;
}

ItemComp MakeEquipItem(uint64_t guid, uint32_t configId, uint32_t enhanceLevel = 0)
{
	ItemComp item;
	item.set_item_id(guid);
	item.set_config_id(configId);
	item.set_size(1);
	item.mutable_equip()->set_enhance_level(enhanceLevel);
	return item;
}

bool ArmItemSegment(uint64_t lo, uint64_t hi)
{
	auto &seg = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
	seg.Reset();
	seg.Enable();
	return seg.OnSegmentAllocated(lo, hi);
}

class PlayerEquipLoadoutTest : public ::testing::Test
{
protected:
	static void LoadTables()
	{
		ItemTableManager::Instance().Load();
		EquipSlotTableManager::Instance().Load();
		EquipAttributeTableManager::Instance().Load();
		EquipAttributeCapTableManager::Instance().Load();
		EquipAffixPoolTableManager::Instance().Load();
		EquipAffixRuleTableManager::Instance().Load();
		EquipEnhanceTableManager::Instance().Load();
		EquipGemTableManager::Instance().Load();
		EquipGemSocketTableManager::Instance().Load();
		ClassTableManager::Instance().Load();
		SkillTableManager::Instance().Load();
		AttributeDimensionTableManager::Instance().Load();
		AttributePoolTableManager::Instance().Load();
		AttributeRuleTableManager::Instance().Load();
		AttributeAllocRatioTableManager::Instance().Load();
	}

	void SetUp() override
	{
		tlsEcs.actorRegistry.clear();
		GainBlockService::ClearAllGlobalBlocks();
		ASSERT_TRUE(ArmItemSegment(uint64_t{1} << 40, (uint64_t{1} << 40) + 100000));
		LoadTables();
		ASSERT_NE(nullptr, ItemTableManager::Instance().FindByIdSilent(equiploadout::kUnlockItemConfigId).first)
			<< "Item.xlsx must contain unlock talisman " << equiploadout::kUnlockItemConfigId;

		gPersistCalls = 0;
		PlayerEquipLoadoutSystem::SetPersistForTest(&RecordPersistForTest);
		PlayerEquipLoadoutSystem::SetNameFilterForTest({});

		player_ = tlsEcs.actorRegistry.create();
		tlsEcs.playerList[Guid{kPlayerId}] = player_;
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player_, Guid{kPlayerId});
		tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player_).epoch = kOwnerEpoch;

		auto &currency = tlsEcs.actorRegistry.emplace<CurrencyComp>(player_);
		while (currency.values_size() < static_cast<int>(kCurrencyMax))
		{
			currency.add_values(0);
		}
		tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player_);
		tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(player_);
		tlsEcs.actorRegistry.emplace<PlayerItemGainBlockComp>(player_);
		tlsEcs.actorRegistry.emplace<PlayerItemHoldComp>(player_);
		tlsEcs.actorRegistry.emplace<PlayerEquipLoadoutComp>(player_);
		tlsEcs.actorRegistry.emplace<PlayerSkillListComp>(player_);

		tlsEcs.actorRegistry.emplace<LevelComp>(player_).set_level(80);
		auto &u32 = tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(player_);
		const auto &classes = ClassTableManager::Instance().FindAll().data();
		if (classes.size() > 0)
		{
			u32.set_class_(classes.Get(0).id());
		}

		auto &base = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player_);
		base.set_health(1000);
		base.set_mana(500);
		tlsEcs.actorRegistry.emplace<PlayerAttributeComp>(player_);

		auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player_);
		for (auto &bag : bags.bags)
		{
			bag.SetPlayerGuid(Guid{kPlayerId});
		}
		bags.bags[kInventory].SetCapacityForRestore(20);

		PlayerAttributeSystem::InitializeOnLoad(player_);
	}

	void TearDown() override
	{
		PlayerEquipLoadoutSystem::SetPersistForTest(nullptr);
		PlayerEquipLoadoutSystem::SetNameFilterForTest({});
		GainBlockService::ClearAllGlobalBlocks();
		tlsEcs.playerList.erase(Guid{kPlayerId});
		if (player_ != entt::null && tlsEcs.actorRegistry.valid(player_))
		{
			tlsEcs.actorRegistry.destroy(player_);
		}
		player_ = entt::null;
		EXPECT_TRUE(ArmItemSegment(1, uint64_t{1} << 54));
	}

	Bag &Inventory() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player_).bags[kInventory]; }
	Bag &Equipment() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player_).bags[kEquipment]; }

	void GrantCurrency(CurrencyType type, int64_t amount)
	{
		ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, type, amount, TX_GM_GRANT, 1));
	}

	void GrantTalisman(uint32_t count)
	{
		ASSERT_EQ(kSuccess, BagService::AddItems(player_, Inventory(), {{equiploadout::kUnlockItemConfigId, count}},
												 TX_GM_GRANT));
	}

	void CommitPersistedUnlockedCount(uint32_t unlockedCount)
	{
		PlayerAllData saved;
		auto *db = saved.mutable_player_database_data();
		db->set_player_id(kPlayerId);
		db->mutable_equip_loadout_component()->set_unlocked_scheme_count(unlockedCount);
		auto &persisted = tlsEcs.actorRegistry.get_or_emplace<PlayerLastPersistedSnapshotComp>(player_);
		persisted.Replace(saved, kOwnerEpoch);
	}

	uint64_t CurrentVersion()
	{
		GetEquipLoadoutsResponse resp;
		EXPECT_EQ(kSuccess, PlayerEquipLoadoutSystem::Get(player_, resp));
		return resp.snapshot().state_version();
	}

	entt::entity player_{entt::null};
};

TEST(EquipLoadoutRulesTest, TierPricingNameValidationAndSwapPlan)
{
	// 1. 价格阶梯验证
	EXPECT_FALSE(equiploadout::UnlockSpecForTarget(1).valid);
	const auto s2 = equiploadout::UnlockSpecForTarget(2);
	EXPECT_TRUE(s2.valid);
	EXPECT_EQ(60u, s2.costDiamond);
	EXPECT_TRUE(s2.allowBindDiamond);
	EXPECT_EQ(120u, equiploadout::UnlockSpecForTarget(3).costDiamond);
	EXPECT_FALSE(equiploadout::UnlockSpecForTarget(3).allowBindDiamond);
	EXPECT_EQ(180u, equiploadout::UnlockSpecForTarget(4).costDiamond);
	EXPECT_EQ(240u, equiploadout::UnlockSpecForTarget(5).costDiamond);
	EXPECT_FALSE(equiploadout::UnlockSpecForTarget(6).valid);

	// 2. 扣费计划（3413 优先 > 方案 2 绑灵补非绑 > 非绑灵石）
	const auto byTalisman = equiploadout::PlanUnlockPayment(s2, 1, 0, 0, true);
	EXPECT_TRUE(byTalisman.canAfford);
	EXPECT_TRUE(byTalisman.useTalisman);
	EXPECT_EQ(0u, byTalisman.spendBindDiamond);
	EXPECT_EQ(0u, byTalisman.spendDiamond);

	const auto splitBind = equiploadout::PlanUnlockPayment(s2, 0, 45, 20, true);
	EXPECT_TRUE(splitBind.canAfford);
	EXPECT_FALSE(splitBind.useTalisman);
	EXPECT_EQ(45u, splitBind.spendBindDiamond);
	EXPECT_EQ(15u, splitBind.spendDiamond);

	const auto s3NoBind = equiploadout::PlanUnlockPayment(equiploadout::UnlockSpecForTarget(3), 0, 500, 100, true);
	EXPECT_FALSE(s3NoBind.canAfford);

	// 3. 名称合法性校验
	EXPECT_TRUE(equiploadout::ValidateSchemeName("PVP高速控场"));
	EXPECT_TRUE(equiploadout::ValidateSchemeName("五行极速控场PVP主力套")); // 12 字符上限
	EXPECT_FALSE(equiploadout::ValidateSchemeName("五行极速控场PVP主力套超")); // 13 字符拒
	EXPECT_FALSE(equiploadout::ValidateSchemeName("   \t  "));
	EXPECT_FALSE(equiploadout::ValidateSchemeName("<b>加粗</b>"));
	EXPECT_FALSE(equiploadout::ValidateSchemeName("官方最强套"));

	// 4. 8v8 原位互换净背包需求为 0
	std::map<uint32_t, uint64_t> cur;
	std::map<uint32_t, uint64_t> desired;
	for (uint32_t i = 0; i < 8; ++i)
	{
		cur[3u + i] = 100u + i;
		desired[3u + i] = 200u + i;
	}
	const auto swapPlan = equiploadout::PlanAtomicSwap(cur, desired, [](uint64_t g) { return g >= 200u && g < 208u; });
	EXPECT_EQ(equiploadout::SwapPlanError::kNone, swapPlan.error);
	EXPECT_EQ(8u, swapPlan.toEquipFromBag.size());
	EXPECT_EQ(8u, swapPlan.toUnequip.size());
	EXPECT_EQ(0u, swapPlan.netExtraBagSlotsNeeded);
	EXPECT_TRUE(swapPlan.missingSlots.empty());
}

TEST_F(PlayerEquipLoadoutTest, FullEightVsEightSwapSucceedsWithZeroFreeBagSlots)
{
	// 背包容量恰好 8 格，塞满 8 件 B 套装备 -> 剩余空格 0！
	Inventory().SetCapacityForRestore(8);
	std::array<uint64_t, 8> setA{};
	std::array<uint64_t, 8> setB{};
	std::map<uint32_t, uint32_t> setBOriginalBagPos;

	for (uint32_t i = 0; i < 8; ++i)
	{
		setA[i] = 1001u + i;
		setB[i] = 2001u + i;
		ASSERT_EQ(kSuccess, Equipment().PutInstance(MakeEquipItem(setA[i], kFormalConfigIds[i], 3), 3u + i));
		ASSERT_EQ(kSuccess, Inventory().PutInstance(MakeEquipItem(setB[i], kFormalConfigIds[i], 6)));
		setBOriginalBagPos[3u + i] = Inventory().GetItemPosByGuid(setB[i]);
	}
	ASSERT_EQ(0u, Inventory().Layout().FreeCells());

	// 保存方案 1（捕获当前穿着 A 套）
	SaveEquipLoadoutRequest save1;
	save1.set_scheme_id(1);
	save1.set_name("主线输出套");
	save1.set_role_tag(1);
	save1.set_capture_current_equipped(true);
	save1.set_state_version(CurrentVersion());
	save1.set_request_id("save_scheme1_req_0001");
	SaveEquipLoadoutResponse save1Resp;
	PlayerEquipLoadoutSystem::Save(player_, save1, save1Resp);
	ASSERT_FALSE(save1Resp.has_error_message()) << save1Resp.error_message().id();

	// 解锁方案 2 并保存 B 套
	GrantCurrency(kCurrencyDiamond, 60);
	UnlockEquipLoadoutRequest unlockReq;
	unlockReq.set_client_request_id(9001);
	unlockReq.set_expected_unlocked_count(1);
	UnlockEquipLoadoutResponse unlockResp;
	PlayerEquipLoadoutSystem::Unlock(player_, unlockReq, unlockResp);
	ASSERT_EQ(UnlockEquipLoadoutResponse::APPLIED, unlockResp.outcome());

	SaveEquipLoadoutRequest save2;
	save2.set_scheme_id(2);
	save2.set_name("PVP控场套");
	save2.set_role_tag(2);
	save2.set_capture_current_equipped(false);
	for (uint32_t i = 0; i < 8; ++i)
	{
		auto *b = save2.add_slots();
		b->set_slot(3u + i);
		b->set_item_guid(setB[i]);
	}
	save2.set_state_version(CurrentVersion());
	save2.set_request_id("save_scheme2_req_0001");
	SaveEquipLoadoutResponse save2Resp;
	PlayerEquipLoadoutSystem::Save(player_, save2, save2Resp);
	ASSERT_FALSE(save2Resp.has_error_message()) << save2Resp.error_message().id();

	// 0 空格一键切换至方案 2（8v8 原位原子互换）
	SwitchEquipLoadoutRequest sw2;
	sw2.set_scheme_id(2);
	sw2.set_apply_linked_schemes(false);
	sw2.set_state_version(CurrentVersion());
	sw2.set_request_id("switch_scheme2_req_01");
	SwitchEquipLoadoutResponse sw2Resp;
	PlayerEquipLoadoutSystem::Switch(player_, sw2, sw2Resp);
	ASSERT_FALSE(sw2Resp.has_error_message()) << sw2Resp.error_message().id();
	EXPECT_TRUE(sw2Resp.missing_slots().empty());
	EXPECT_EQ(2u, sw2Resp.snapshot().active_scheme_id());
	EXPECT_EQ(0u, Inventory().Layout().FreeCells());
	for (uint32_t i = 0; i < 8; ++i)
	{
		EXPECT_EQ(setB[i], Equipment().Layout().At(3u + i));
		ASSERT_NE(nullptr, Inventory().GetItemCompByGuid(setA[i]));
		EXPECT_EQ(setBOriginalBagPos[3u + i], Inventory().GetItemPosByGuid(setA[i]))
			<< "旧件应原位落回同槽新件原来的背包格";
	}

	// 再 0 空格一键切回方案 1
	SwitchEquipLoadoutRequest sw1;
	sw1.set_scheme_id(1);
	sw1.set_state_version(CurrentVersion());
	sw1.set_request_id("switch_scheme1_req_01");
	SwitchEquipLoadoutResponse sw1Resp;
	PlayerEquipLoadoutSystem::Switch(player_, sw1, sw1Resp);
	ASSERT_FALSE(sw1Resp.has_error_message()) << sw1Resp.error_message().id();
	for (uint32_t i = 0; i < 8; ++i)
	{
		EXPECT_EQ(setA[i], Equipment().Layout().At(3u + i));
		EXPECT_NE(nullptr, Inventory().GetItemCompByGuid(setB[i]));
	}
}

TEST_F(PlayerEquipLoadoutTest, PureUnequipRejectedAtomicallyWhenBagFull)
{
	Inventory().SetCapacityForRestore(1);
	ASSERT_EQ(kSuccess, Inventory().PutInstance(MakeEquipItem(3001, 1101))); // 占满唯一 1 格 -> 0 空格
	ASSERT_EQ(kSuccess, Equipment().PutInstance(MakeEquipItem(3101, 1101), 3));
	ASSERT_EQ(kSuccess, Equipment().PutInstance(MakeEquipItem(3102, 1201), 4));

	// 保存方案 1 只穿武器 3001（目标槽 4 为空 -> 需纯卸下帽子 3102，但 0 空格）
	// 实际上装备 3001 换 3101 净需 0 格，再纯卸下 3102 净需 1 格 -> 整笔拒绝！
	SaveEquipLoadoutRequest saveReq;
	saveReq.set_scheme_id(1);
	saveReq.set_state_version(CurrentVersion());
	saveReq.set_request_id("save_unequip_req_0001");
	auto *slot3 = saveReq.add_slots();
	slot3->set_slot(3);
	slot3->set_item_guid(3001);
	SaveEquipLoadoutResponse saveResp;
	PlayerEquipLoadoutSystem::Save(player_, saveReq, saveResp);
	ASSERT_FALSE(saveResp.has_error_message());

	SwitchEquipLoadoutRequest swReq;
	swReq.set_scheme_id(1);
	swReq.set_state_version(CurrentVersion());
	swReq.set_request_id("switch_unequip_fail01");
	SwitchEquipLoadoutResponse swResp;
	PlayerEquipLoadoutSystem::Switch(player_, swReq, swResp);
	ASSERT_TRUE(swResp.has_error_message());
	EXPECT_EQ(static_cast<uint32_t>(kEquipLoadoutSlotUnavailable), swResp.error_message().id());
	// 原子性验证：一件都没动！
	EXPECT_EQ(3101u, Equipment().Layout().At(3));
	EXPECT_EQ(3102u, Equipment().Layout().At(4));
	EXPECT_NE(nullptr, Inventory().GetItemCompByGuid(3001));
}

TEST_F(PlayerEquipLoadoutTest, DuplicateGuidRejectedOnSaveAndSwitch)
{
	ASSERT_EQ(kSuccess, Inventory().PutInstance(MakeEquipItem(4001, 3201)));
	SaveEquipLoadoutRequest saveReq;
	saveReq.set_scheme_id(1);
	saveReq.set_state_version(CurrentVersion());
	saveReq.set_request_id("save_dup_guid_req0001");
	auto *s7 = saveReq.add_slots();
	s7->set_slot(7);
	s7->set_item_guid(4001);
	auto *s8 = saveReq.add_slots();
	s8->set_slot(8);
	s8->set_item_guid(4001);
	SaveEquipLoadoutResponse saveResp;
	PlayerEquipLoadoutSystem::Save(player_, saveReq, saveResp);
	ASSERT_TRUE(saveResp.has_error_message());
	EXPECT_EQ(static_cast<uint32_t>(kEquipLoadoutDuplicateItem), saveResp.error_message().id());

	// 即使持久化组件内被异常写入重复 guid，Switch 也会在阶段 1 拦截。
	auto &comp = tlsEcs.actorRegistry.get<PlayerEquipLoadoutComp>(player_);
	auto *corrupt = comp.add_schemes();
	corrupt->set_scheme_id(1);
	auto *c7 = corrupt->add_slots();
	c7->set_slot(7);
	c7->set_item_guid(4001);
	auto *c8 = corrupt->add_slots();
	c8->set_slot(8);
	c8->set_item_guid(4001);

	SwitchEquipLoadoutRequest swReq;
	swReq.set_scheme_id(1);
	swReq.set_state_version(CurrentVersion());
	swReq.set_request_id("switch_dup_guid_00001");
	SwitchEquipLoadoutResponse swResp;
	PlayerEquipLoadoutSystem::Switch(player_, swReq, swResp);
	ASSERT_TRUE(swResp.has_error_message());
	EXPECT_EQ(static_cast<uint32_t>(kEquipLoadoutDuplicateItem), swResp.error_message().id());
}

TEST_F(PlayerEquipLoadoutTest, MissingItemSkippedAndReturnedInMissingSlots)
{
	ASSERT_EQ(kSuccess, Equipment().PutInstance(MakeEquipItem(5001, 1101), 3));
	ASSERT_EQ(kSuccess, Equipment().PutInstance(MakeEquipItem(5002, 1201), 4));
	ASSERT_EQ(kSuccess, Inventory().PutInstance(MakeEquipItem(5101, 1101)));
	ASSERT_EQ(kSuccess, Inventory().PutInstance(MakeEquipItem(5102, 1201)));

	SaveEquipLoadoutRequest saveReq;
	saveReq.set_scheme_id(1);
	saveReq.set_state_version(CurrentVersion());
	saveReq.set_request_id("save_missing_test_001");
	auto *b3 = saveReq.add_slots();
	b3->set_slot(3);
	b3->set_item_guid(5101);
	auto *b4 = saveReq.add_slots();
	b4->set_slot(4);
	b4->set_item_guid(5102);
	SaveEquipLoadoutResponse saveResp;
	PlayerEquipLoadoutSystem::Save(player_, saveReq, saveResp);
	ASSERT_FALSE(saveResp.has_error_message());

	// 模拟槽 4 装备被存仓库/卖掉（不在背包和装备栏）
	ItemComp removed;
	ASSERT_EQ(kSuccess, Inventory().TakeInstance(5102, removed));

	SwitchEquipLoadoutRequest swReq;
	swReq.set_scheme_id(1);
	swReq.set_state_version(CurrentVersion());
	swReq.set_request_id("switch_missing_test01");
	SwitchEquipLoadoutResponse swResp;
	PlayerEquipLoadoutSystem::Switch(player_, swReq, swResp);
	ASSERT_FALSE(swResp.has_error_message()) << swResp.error_message().id();
	ASSERT_EQ(1, swResp.missing_slots_size());
	EXPECT_EQ(4u, swResp.missing_slots(0));
	EXPECT_EQ(5101u, Equipment().Layout().At(3)); // 槽 3 正常换装
	EXPECT_EQ(5002u, Equipment().Layout().At(4)); // 缺失槽 4 保持原件不动
}

TEST_F(PlayerEquipLoadoutTest, TieredUnlockTalismanBindDiamondAndMaxCap)
{
	// 方案 2：60 灵石，允许优先绑灵（40）+ 非绑补足（20）
	GrantCurrency(kCurrencyBindDiamond, 40);
	GrantCurrency(kCurrencyDiamond, 20);
	{
		UnlockEquipLoadoutRequest req;
		req.set_client_request_id(10002);
		req.set_expected_unlocked_count(1);
		req.set_use_bind_diamond_first(true);
		UnlockEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Unlock(player_, req, resp);
		ASSERT_EQ(UnlockEquipLoadoutResponse::APPLIED, resp.outcome());
		EXPECT_EQ(2u, resp.result_unlocked_count());
		EXPECT_EQ(2u, resp.current_unlocked_count());
		EXPECT_EQ(0u, resp.used_item_config_id());
		EXPECT_EQ(40u, resp.spent_bind_diamond());
		EXPECT_EQ(20u, resp.spent_diamond());
		EXPECT_EQ(0u, CurrencySystem::GetBalance(player_, kCurrencyBindDiamond));
		EXPECT_EQ(0u, CurrencySystem::GetBalance(player_, kCurrencyDiamond));
	}

	// 方案 3：优先扣 3413 扩容符（零灵石亦可成功）
	GrantTalisman(1);
	{
		UnlockEquipLoadoutRequest req;
		req.set_client_request_id(10003);
		req.set_expected_unlocked_count(2);
		UnlockEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Unlock(player_, req, resp);
		ASSERT_EQ(UnlockEquipLoadoutResponse::APPLIED, resp.outcome());
		EXPECT_EQ(3u, resp.result_unlocked_count());
		EXPECT_EQ(equiploadout::kUnlockItemConfigId, resp.used_item_config_id());
		EXPECT_EQ(0u, resp.spent_diamond());
		EXPECT_EQ(0u, Inventory().GetTotalItemCount(equiploadout::kUnlockItemConfigId));
	}

	// 方案 4：180 非绑灵石（绑灵不生效）
	GrantCurrency(kCurrencyBindDiamond, 500);
	GrantCurrency(kCurrencyDiamond, 100);
	{
		UnlockEquipLoadoutRequest req;
		req.set_client_request_id(10004);
		req.set_expected_unlocked_count(3);
		req.set_use_bind_diamond_first(true);
		UnlockEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Unlock(player_, req, resp);
		EXPECT_EQ(UnlockEquipLoadoutResponse::REJECTED, resp.outcome());
		EXPECT_EQ(static_cast<uint32_t>(kEquipLoadoutDiamondNotEnough), resp.error_message().id());
	}
	GrantCurrency(kCurrencyDiamond, 80); // 凑足 180 非绑
	{
		UnlockEquipLoadoutRequest req;
		req.set_client_request_id(10005);
		req.set_expected_unlocked_count(3);
		UnlockEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Unlock(player_, req, resp);
		ASSERT_EQ(UnlockEquipLoadoutResponse::APPLIED, resp.outcome());
		EXPECT_EQ(4u, resp.result_unlocked_count());
		EXPECT_EQ(180u, resp.spent_diamond());
		EXPECT_EQ(500u, CurrencySystem::GetBalance(player_, kCurrencyBindDiamond));
	}

	// 方案 5：240 非绑灵石
	GrantCurrency(kCurrencyDiamond, 240);
	{
		UnlockEquipLoadoutRequest req;
		req.set_client_request_id(10006);
		req.set_expected_unlocked_count(4);
		UnlockEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Unlock(player_, req, resp);
		ASSERT_EQ(UnlockEquipLoadoutResponse::APPLIED, resp.outcome());
		EXPECT_EQ(5u, resp.result_unlocked_count());
		EXPECT_EQ(240u, resp.spent_diamond());
	}

	// 已达上限 5 套：拒绝
	GrantCurrency(kCurrencyDiamond, 500);
	{
		UnlockEquipLoadoutRequest req;
		req.set_client_request_id(10007);
		req.set_expected_unlocked_count(5);
		UnlockEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Unlock(player_, req, resp);
		EXPECT_EQ(UnlockEquipLoadoutResponse::REJECTED, resp.outcome());
		EXPECT_EQ(static_cast<uint32_t>(kEquipLoadoutMaxUnlocked), resp.error_message().id());
	}

	// 预期套数过期：拒绝并标 UNKNOWN
	{
		UnlockEquipLoadoutRequest req;
		req.set_client_request_id(10008);
		req.set_expected_unlocked_count(2);
		UnlockEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Unlock(player_, req, resp);
		EXPECT_EQ(UnlockEquipLoadoutResponse::UNKNOWN, resp.outcome());
		EXPECT_EQ(static_cast<uint32_t>(kEquipLoadoutUnlockCountChanged), resp.error_message().id());
	}
}

TEST_F(PlayerEquipLoadoutTest, UnlockIdempotencyPreventsDoubleChargeAndRequiresDurableCommit)
{
	GrantCurrency(kCurrencyDiamond, 120);

	UnlockEquipLoadoutRequest req;
	req.set_client_request_id(777001);
	req.set_expected_unlocked_count(1);
	req.set_use_bind_diamond_first(false);

	UnlockEquipLoadoutResponse first;
	PlayerEquipLoadoutSystem::Unlock(player_, req, first);
	ASSERT_EQ(UnlockEquipLoadoutResponse::APPLIED, first.outcome());
	EXPECT_FALSE(first.replayed());
	EXPECT_FALSE(first.durable());
	EXPECT_EQ(1, gPersistCalls);
	EXPECT_EQ(60u, CurrencySystem::GetBalance(player_, kCurrencyDiamond));

	// 同键同参重放：不二次扣钱，replayed = true
	UnlockEquipLoadoutResponse replay;
	PlayerEquipLoadoutSystem::Unlock(player_, req, replay);
	ASSERT_EQ(UnlockEquipLoadoutResponse::APPLIED, replay.outcome());
	EXPECT_TRUE(replay.replayed());
	EXPECT_FALSE(replay.durable());
	EXPECT_EQ(60u, replay.spent_diamond());
	EXPECT_EQ(60u, CurrencySystem::GetBalance(player_, kCurrencyDiamond));

	// 同键异参：拒 kInvalidParameter
	UnlockEquipLoadoutRequest mismatch = req;
	mismatch.set_use_bind_diamond_first(true);
	UnlockEquipLoadoutResponse mismatchResp;
	PlayerEquipLoadoutSystem::Unlock(player_, mismatch, mismatchResp);
	EXPECT_EQ(UnlockEquipLoadoutResponse::REJECTED, mismatchResp.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kInvalidParameter), mismatchResp.error_message().id());

	// 真实存盘确认后，重放回 durable = true
	CommitPersistedUnlockedCount(2);
	UnlockEquipLoadoutResponse durableReplay;
	PlayerEquipLoadoutSystem::Unlock(player_, req, durableReplay);
	ASSERT_EQ(UnlockEquipLoadoutResponse::APPLIED, durableReplay.outcome());
	EXPECT_TRUE(durableReplay.replayed());
	EXPECT_TRUE(durableReplay.durable());
}

TEST_F(PlayerEquipLoadoutTest, BattleAndFrozenStatesRejectWriteOperations)
{
	GrantCurrency(kCurrencyDiamond, 100);

	// 战斗中拦截
	tlsEcs.actorRegistry.emplace<InBattleComp>(player_);
	{
		SaveEquipLoadoutRequest saveReq;
		saveReq.set_scheme_id(1);
		saveReq.set_capture_current_equipped(true);
		saveReq.set_state_version(CurrentVersion());
		saveReq.set_request_id("battle_save_req_00001");
		SaveEquipLoadoutResponse saveResp;
		PlayerEquipLoadoutSystem::Save(player_, saveReq, saveResp);
		EXPECT_EQ(static_cast<uint32_t>(kEquipInBattle), saveResp.error_message().id());

		SwitchEquipLoadoutRequest swReq;
		swReq.set_scheme_id(1);
		swReq.set_state_version(CurrentVersion());
		swReq.set_request_id("battle_switch_req0001");
		SwitchEquipLoadoutResponse swResp;
		PlayerEquipLoadoutSystem::Switch(player_, swReq, swResp);
		EXPECT_EQ(static_cast<uint32_t>(kEquipInBattle), swResp.error_message().id());

		UnlockEquipLoadoutRequest ulReq;
		ulReq.set_client_request_id(88001);
		ulReq.set_expected_unlocked_count(1);
		UnlockEquipLoadoutResponse ulResp;
		PlayerEquipLoadoutSystem::Unlock(player_, ulReq, ulResp);
		EXPECT_EQ(static_cast<uint32_t>(kEquipInBattle), ulResp.error_message().id());
	}
	tlsEcs.actorRegistry.remove<InBattleComp>(player_);

	// 冻结态拦截
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player_);
	{
		SaveEquipLoadoutRequest saveReq;
		saveReq.set_scheme_id(1);
		saveReq.set_capture_current_equipped(true);
		saveReq.set_state_version(CurrentVersion());
		saveReq.set_request_id("frozen_save_req_00001");
		SaveEquipLoadoutResponse saveResp;
		PlayerEquipLoadoutSystem::Save(player_, saveReq, saveResp);
		EXPECT_EQ(static_cast<uint32_t>(kEquipFrozen), saveResp.error_message().id());

		SwitchEquipLoadoutRequest swReq;
		swReq.set_scheme_id(1);
		swReq.set_state_version(CurrentVersion());
		swReq.set_request_id("frozen_switch_req0001");
		SwitchEquipLoadoutResponse swResp;
		PlayerEquipLoadoutSystem::Switch(player_, swReq, swResp);
		EXPECT_EQ(static_cast<uint32_t>(kEquipFrozen), swResp.error_message().id());

		UnlockEquipLoadoutRequest ulReq;
		ulReq.set_client_request_id(88002);
		ulReq.set_expected_unlocked_count(1);
		UnlockEquipLoadoutResponse ulResp;
		PlayerEquipLoadoutSystem::Unlock(player_, ulReq, ulResp);
		EXPECT_EQ(static_cast<uint32_t>(kEquipFrozen), ulResp.error_message().id());
	}
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player_);
}

TEST_F(PlayerEquipLoadoutTest, SchemeNameValidationAndSensitiveWordFilter)
{
	PlayerEquipLoadoutSystem::SetNameFilterForTest(
		[](std::string_view name) { return name.find("禁词") == std::string_view::npos; });

	const auto trySaveName = [&](const std::string &name, const std::string &reqId) {
		SaveEquipLoadoutRequest req;
		req.set_scheme_id(1);
		req.set_name(name);
		req.set_capture_current_equipped(true);
		req.set_state_version(CurrentVersion());
		req.set_request_id(reqId);
		SaveEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Save(player_, req, resp);
		return resp;
	};

	EXPECT_EQ(static_cast<uint32_t>(kEquipLoadoutNameInvalid),
			  trySaveName("一二三四五六七八九十甲乙丙", "name_too_long_req0001").error_message().id());
	EXPECT_EQ(static_cast<uint32_t>(kEquipLoadoutNameInvalid),
			  trySaveName("   ", "name_spaces_req_00001").error_message().id());
	EXPECT_EQ(static_cast<uint32_t>(kEquipLoadoutNameInvalid),
			  trySaveName("包含禁词测试", "name_filter_req_00001").error_message().id());

	const auto ok = trySaveName("五行极速控场套", "name_valid_req_000001");
	ASSERT_FALSE(ok.has_error_message()) << ok.error_message().id();
	EXPECT_EQ("五行极速控场套", ok.snapshot().schemes(0).name());
}

TEST_F(PlayerEquipLoadoutTest, SwitchAppliesLinkedSchemesAndToleratesAttributeCooldown)
{
	// 准备第 2 套属性加点方案
	GrantCurrency(kCurrencyGold, 10000000);
	uint32_t attrScheme2 = 0;
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::CreateScheme(player_, "控速加点", attrScheme2));
	ASSERT_EQ(2u, attrScheme2);

	// 准备两把武器与 2 套装备方案
	ASSERT_EQ(kSuccess, Equipment().PutInstance(MakeEquipItem(6001, 1101), 3));
	ASSERT_EQ(kSuccess, Inventory().PutInstance(MakeEquipItem(6002, 1101)));

	GrantCurrency(kCurrencyDiamond, 60);
	UnlockEquipLoadoutRequest ulReq;
	ulReq.set_client_request_id(66001);
	ulReq.set_expected_unlocked_count(1);
	UnlockEquipLoadoutResponse ulResp;
	PlayerEquipLoadoutSystem::Unlock(player_, ulReq, ulResp);
	ASSERT_EQ(UnlockEquipLoadoutResponse::APPLIED, ulResp.outcome());

	// 方案 1 绑定战法 1 + 加点 1
	{
		SaveEquipLoadoutRequest req;
		req.set_scheme_id(1);
		req.set_capture_current_equipped(true);
		req.set_linked_skill_scheme_id(1);
		req.set_linked_attribute_scheme_id(1);
		req.set_state_version(CurrentVersion());
		req.set_request_id("linked_save1_req_0001");
		SaveEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Save(player_, req, resp);
		ASSERT_FALSE(resp.has_error_message());
	}
	// 方案 2 绑定战法 2 + 加点 2
	{
		SaveEquipLoadoutRequest req;
		req.set_scheme_id(2);
		auto *s3 = req.add_slots();
		s3->set_slot(3);
		s3->set_item_guid(6002);
		req.set_linked_skill_scheme_id(2);
		req.set_linked_attribute_scheme_id(2);
		req.set_state_version(CurrentVersion());
		req.set_request_id("linked_save2_req_0001");
		SaveEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Save(player_, req, resp);
		ASSERT_FALSE(resp.has_error_message());
	}

	// 切到方案 2（联动生效）
	{
		SwitchEquipLoadoutRequest req;
		req.set_scheme_id(2);
		req.set_apply_linked_schemes(true);
		req.set_state_version(CurrentVersion());
		req.set_request_id("linked_switch2_req001");
		SwitchEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Switch(player_, req, resp);
		ASSERT_FALSE(resp.has_error_message()) << resp.error_message().id();
		EXPECT_TRUE(resp.linked_notices().empty());
		EXPECT_EQ(6002u, Equipment().Layout().At(3));
		EXPECT_EQ(2u, tlsEcs.actorRegistry.get<PlayerSkillListComp>(player_).active_loadout_scheme_id());
		EXPECT_EQ(2u, tlsEcs.actorRegistry.get<PlayerAttributeComp>(player_).active_scheme_id());
	}

	// 立刻切回方案 1：若加点处于冷却中，装备与战法依然切换成功，并在 linked_notices 明文提示。
	auto &attrComp = tlsEcs.actorRegistry.get<PlayerAttributeComp>(player_);
	attrComp.set_last_switch_time(TimeSystem::NowSecondsUTC());
	const auto &rules = AttributeRuleTableManager::Instance().FindAll().data();
	const bool hasCooldown = rules.size() > 0 && rules.Get(0).switch_cooldown_seconds() > 0;
	{
		SwitchEquipLoadoutRequest req;
		req.set_scheme_id(1);
		req.set_apply_linked_schemes(true);
		req.set_state_version(CurrentVersion());
		req.set_request_id("linked_switch1_req001");
		SwitchEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Switch(player_, req, resp);
		ASSERT_FALSE(resp.has_error_message()) << resp.error_message().id();
		EXPECT_EQ(6001u, Equipment().Layout().At(3));
		EXPECT_EQ(1u, tlsEcs.actorRegistry.get<PlayerSkillListComp>(player_).active_loadout_scheme_id());
		if (hasCooldown)
		{
			EXPECT_EQ(2u, tlsEcs.actorRegistry.get<PlayerAttributeComp>(player_).active_scheme_id());
			ASSERT_EQ(1, resp.linked_notices_size());
		}
	}
}

TEST_F(PlayerEquipLoadoutTest, RpcHandlerEndToEndSmoke)
{
	GetEquipLoadoutsRequest getReq;
	GetEquipLoadoutsResponse getResp;
	SceneBagClientPlayerHandler::GetEquipLoadouts(player_, &getReq, &getResp);
	ASSERT_FALSE(getResp.has_error_message());
	EXPECT_EQ(1u, getResp.snapshot().unlocked_scheme_count());
	EXPECT_EQ(5u, getResp.snapshot().max_scheme_count());
	EXPECT_EQ(equiploadout::kUnlockItemConfigId, getResp.snapshot().unlock_item_config_id());
	EXPECT_EQ(60u, getResp.snapshot().next_unlock_cost_diamond());
	EXPECT_TRUE(getResp.snapshot().next_unlock_allow_bind_diamond());
}

} // namespace
