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
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/common/component/player_season_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/common/database/mysql_database_table.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "proto/scene/player_skill.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "services/scene/player/comp/last_persisted_snapshot_comp.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/equip_loadout_rules.h"
#include "services/scene/player/system/player_attribute.h"
#include "services/scene/player/system/player_equip_loadout.h"
#include "services/scene/player/system/player_pet.h"
#include "services/scene/player/system/player_season.h"
#include "services/scene/player/system/player_skill.h"
#include "services/scene/player/system/skill_loadout_rules.h"
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
#include "table/code/pet_table.h"
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
	GuidSegmentClient::Options options;
	options.mixIds = false;
	options.kindName = "item";
	options.initialStep = 100000;
	if (!seg.Enable(options, [](const std::string &, uint32_t) { return true; },
					[](GuidSegmentClient::TimerKind, double, std::function<void()>) {}, [] { return 0.0; }))
		return false;
	seg.Warm();
	seg.OnResponse(0, lo, hi);
	return seg.IsReady();
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
		PetTableManager::Instance().Load();
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
		tlsEcs.actorRegistry.emplace<PlayerUint64Comp>(player_);

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
		auto saved = std::make_shared<PlayerAllData>();
		saved->set_last_save_seq(1);
		auto *db = saved->mutable_player_database_data();
		db->set_player_id(kPlayerId);
		db->mutable_equip_loadout_component()->set_unlocked_scheme_count(unlockedCount);
		auto &persisted = tlsEcs.actorRegistry.get_or_emplace<PlayerLastPersistedSnapshotComp>(player_);
		persisted.RememberCommit(saved, kOwnerEpoch, 1);
		persisted.Replace(*saved);
	}

	uint64_t CurrentVersion()
	{
		GetEquipLoadoutsResponse resp;
		EXPECT_EQ(kSuccess, PlayerEquipLoadoutSystem::Get(player_, resp));
		return resp.snapshot().state_version();
	}

	uint64_t CurrentVersionAt(uint64_t nowUnixSec)
	{
		GetEquipLoadoutsResponse resp;
		EXPECT_EQ(kSuccess, PlayerEquipLoadoutSystem::GetAt(player_, nowUnixSec, resp));
		return resp.snapshot().state_version();
	}

	entt::entity player_{entt::null};
};

TEST(EquipLoadoutRulesTest, UnifiedSevenSlotDefaultsNameValidationAndSwapPlan)
{
	// 1. 方案 A 统一席位基础常量（免费 2 套起，总上限 7 套，与战法·加点对齐）
	EXPECT_EQ(2u, equiploadout::kFreeSchemes);
	EXPECT_EQ(7u, equiploadout::kMaxSchemes);
	EXPECT_EQ("方案一", equiploadout::DefaultSchemeName(1));
	EXPECT_EQ("方案六", equiploadout::DefaultSchemeName(6));
	EXPECT_EQ("方案七", equiploadout::DefaultSchemeName(7));
	EXPECT_EQ(2u, equiploadout::EffectiveUnlockedCount(0));
	EXPECT_EQ(7u, equiploadout::EffectiveUnlockedCount(99));
	// 到期锁定态允许保留 1..7 的激活方案号，不强切回方案一
	EXPECT_EQ(7u, equiploadout::EffectiveActiveSchemeId(7, equiploadout::kMaxSchemes));
	EXPECT_EQ(1u, equiploadout::EffectiveActiveSchemeId(0, equiploadout::kMaxSchemes));
	EXPECT_EQ(1u, equiploadout::EffectiveActiveSchemeId(8, equiploadout::kMaxSchemes));

	// 2. 名称合法性校验
	EXPECT_TRUE(equiploadout::ValidateSchemeName("PVP高速控场"));
	EXPECT_TRUE(equiploadout::ValidateSchemeName("五行极速控场PVP主力套")); // 12 字符上限
	EXPECT_FALSE(equiploadout::ValidateSchemeName("五行极速控场PVP主力套超")); // 13 字符拒
	EXPECT_FALSE(equiploadout::ValidateSchemeName("   \t  "));
	EXPECT_FALSE(equiploadout::ValidateSchemeName("<b>加粗</b>"));
	EXPECT_FALSE(equiploadout::ValidateSchemeName("官方最强套"));

	// 3. 8v8 原位互换净背包需求为 0
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

	// 方案 1 与方案 2 均永久免费：零付费直接保存两套方案！
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

TEST_F(PlayerEquipLoadoutTest, UnifiedSevenSeatsWithMaxLevelLightCardRentAndExpiryLockOnly)
{
	constexpr uint64_t kNow = 1'780'000'000ULL;
	ASSERT_EQ(kSuccess, Equipment().PutInstance(MakeEquipItem(7001, 1101), 3));
	ASSERT_EQ(kSuccess, Inventory().PutInstance(MakeEquipItem(7002, 1101)));

	// 1. 80 级无月卡无租用：默认永久免费 2 套（上限 7 套），方案 3 不可保存
	{
		GetEquipLoadoutsResponse resp;
		ASSERT_EQ(kSuccess, PlayerEquipLoadoutSystem::GetAt(player_, kNow, resp));
		EXPECT_EQ(2u, resp.snapshot().unlocked_scheme_count());
		EXPECT_EQ(7u, resp.snapshot().max_scheme_count());
		EXPECT_EQ(7, resp.snapshot().schemes_size());
		EXPECT_EQ(2u, resp.snapshot().free_schemes());
		EXPECT_EQ(0u, resp.snapshot().light_card_bonus_schemes());
		EXPECT_EQ(0u, resp.snapshot().rented_extra_slots());
		EXPECT_EQ(30u, resp.snapshot().rent_unit_price_diamond());
		EXPECT_EQ(30u, resp.snapshot().rent_period_days());
		EXPECT_EQ(180u, resp.snapshot().rent_max_remaining_days());
		EXPECT_TRUE(resp.snapshot().schemes(0).unlocked());
		EXPECT_TRUE(resp.snapshot().schemes(1).unlocked());
		EXPECT_FALSE(resp.snapshot().schemes(2).unlocked());

		SaveEquipLoadoutRequest req;
		req.set_scheme_id(3);
		req.set_capture_current_equipped(true);
		req.set_state_version(resp.snapshot().state_version());
		req.set_request_id("save_locked3_req_0001");
		SaveEquipLoadoutResponse saveResp;
		PlayerEquipLoadoutSystem::SaveAt(player_, req, kNow, saveResp);
		EXPECT_EQ(static_cast<uint32_t>(kEquipLoadoutSchemeInvalid), saveResp.error_message().id());
	}

	// 2. 升至 85 级满级：自动永久免费第 3 套
	tlsEcs.actorRegistry.get<LevelComp>(player_).set_level(85);
	{
		GetEquipLoadoutsResponse resp;
		ASSERT_EQ(kSuccess, PlayerEquipLoadoutSystem::GetAt(player_, kNow, resp));
		EXPECT_EQ(3u, resp.snapshot().free_schemes());
		EXPECT_EQ(3u, resp.snapshot().unlocked_scheme_count());
		EXPECT_TRUE(resp.snapshot().schemes(2).unlocked());
		EXPECT_FALSE(resp.snapshot().schemes(3).unlocked());
	}

	// 3. 轻享卡生效：再 +2 套（共 5 套）
	tlsEcs.actorRegistry.get<PlayerUint64Comp>(player_).set_afk_pass_expires_at(kNow + 10ULL * 86400ULL);
	{
		GetEquipLoadoutsResponse resp;
		ASSERT_EQ(kSuccess, PlayerEquipLoadoutSystem::GetAt(player_, kNow, resp));
		EXPECT_EQ(2u, resp.snapshot().light_card_bonus_schemes());
		EXPECT_EQ(5u, resp.snapshot().unlocked_scheme_count());
	}

	// 4. 经统一战法·加点租用接口租 +2 套（60 灵石 / 30 天）：装备侧同步立开至 7 套，零二次收费！
	GrantCurrency(kCurrencyDiamond, 60);
	{
		RentSkillLoadoutSlotsRequest rentReq;
		rentReq.set_client_request_id(555001);
		rentReq.set_target_rented_slots(2);
		rentReq.set_add_periods(1);
		rentReq.set_expected_cost_diamond(60);
		RentSkillLoadoutSlotsResponse rentResp;
		PlayerSkillSystem::RentLoadoutSlotsAt(player_, rentReq, kNow, rentResp);
		ASSERT_FALSE(rentResp.has_error_message()) << rentResp.error_message().id();
		EXPECT_EQ(2u, rentResp.rented_extra_slots());
		EXPECT_EQ(60u, rentResp.spent_diamond());
	}
	{
		GetEquipLoadoutsResponse resp;
		ASSERT_EQ(kSuccess, PlayerEquipLoadoutSystem::GetAt(player_, kNow, resp));
		EXPECT_EQ(7u, resp.snapshot().unlocked_scheme_count());
		EXPECT_EQ(2u, resp.snapshot().rented_extra_slots());
		EXPECT_EQ(kNow + skillloadout::kRentPeriodSeconds, resp.snapshot().rent_expires_at_sec());
		EXPECT_TRUE(resp.snapshot().schemes(6).unlocked());
	}

	// 5. 在第 7 套保存自定义名称与装备，并切换激活第 7 套
	{
		SaveEquipLoadoutRequest req;
		req.set_scheme_id(7);
		req.set_name("帮战抗控套");
		req.set_role_tag(4);
		auto *s3 = req.add_slots();
		s3->set_slot(3);
		s3->set_item_guid(7002);
		req.set_state_version(CurrentVersionAt(kNow));
		req.set_request_id("save_scheme7_req_0001");
		SaveEquipLoadoutResponse saveResp;
		PlayerEquipLoadoutSystem::SaveAt(player_, req, kNow, saveResp);
		ASSERT_FALSE(saveResp.has_error_message()) << saveResp.error_message().id();

		SwitchEquipLoadoutRequest swReq;
		swReq.set_scheme_id(7);
		swReq.set_apply_linked_schemes(false);
		swReq.set_state_version(saveResp.snapshot().state_version());
		swReq.set_request_id("switch_scheme7_req_01");
		SwitchEquipLoadoutResponse swResp;
		PlayerEquipLoadoutSystem::SwitchAt(player_, swReq, kNow, swResp);
		ASSERT_FALSE(swResp.has_error_message()) << swResp.error_message().id();
		EXPECT_EQ(7u, swResp.snapshot().active_scheme_id());
		EXPECT_EQ(7002u, Equipment().Layout().At(3));
	}

	// 6. 到期后（轻享卡与月租均过 31 天）：只锁不删、保留已存 7 套与当前激活态、禁止再切入锁定套，允许切回 1..3
	const uint64_t afterExpiry = kNow + 31ULL * 86400ULL;
	{
		GetEquipLoadoutsResponse resp;
		ASSERT_EQ(kSuccess, PlayerEquipLoadoutSystem::GetAt(player_, afterExpiry, resp));
		EXPECT_EQ(3u, resp.snapshot().unlocked_scheme_count());
		EXPECT_EQ(0u, resp.snapshot().light_card_bonus_schemes());
		EXPECT_EQ(0u, resp.snapshot().rented_extra_slots());
		EXPECT_EQ(0u, resp.snapshot().rent_expires_at_sec());
		// 保留当前激活态 7（不半夜强切）且完整保留方案 7 名称与装备槽位供只读预览
		EXPECT_EQ(7u, resp.snapshot().active_scheme_id());
		EXPECT_FALSE(resp.snapshot().schemes(6).unlocked());
		EXPECT_EQ("帮战抗控套", resp.snapshot().schemes(6).name());
		EXPECT_EQ(7002u, resp.snapshot().schemes(6).slots(0).item_guid());

		// 切回免费方案 1 成功
		SwitchEquipLoadoutRequest sw1;
		sw1.set_scheme_id(1);
		sw1.set_state_version(resp.snapshot().state_version());
		sw1.set_request_id("switch_back1_expired1");
		SwitchEquipLoadoutResponse sw1Resp;
		PlayerEquipLoadoutSystem::SwitchAt(player_, sw1, afterExpiry, sw1Resp);
		ASSERT_FALSE(sw1Resp.has_error_message()) << sw1Resp.error_message().id();
		EXPECT_EQ(1u, sw1Resp.snapshot().active_scheme_id());

		// 切走后再试图切回锁定方案 7 被拒
		SwitchEquipLoadoutRequest sw7;
		sw7.set_scheme_id(7);
		sw7.set_state_version(sw1Resp.snapshot().state_version());
		sw7.set_request_id("switch_lock7_expired1");
		SwitchEquipLoadoutResponse sw7Resp;
		PlayerEquipLoadoutSystem::SwitchAt(player_, sw7, afterExpiry, sw7Resp);
		EXPECT_EQ(static_cast<uint32_t>(kEquipLoadoutSchemeInvalid), sw7Resp.error_message().id());
	}

	// 7. 续期 4 套（120 灵石）后，方案 7 原样复活并可再次切换
	GrantCurrency(kCurrencyDiamond, 120);
	{
		RentSkillLoadoutSlotsRequest renewReq;
		renewReq.set_client_request_id(555002);
		renewReq.set_target_rented_slots(4);
		renewReq.set_add_periods(1);
		renewReq.set_expected_cost_diamond(120);
		RentSkillLoadoutSlotsResponse renewResp;
		PlayerSkillSystem::RentLoadoutSlotsAt(player_, renewReq, afterExpiry, renewResp);
		ASSERT_FALSE(renewResp.has_error_message()) << renewResp.error_message().id();

		SwitchEquipLoadoutRequest sw7;
		sw7.set_scheme_id(7);
		sw7.set_state_version(CurrentVersionAt(afterExpiry));
		sw7.set_request_id("switch_revived7_req01");
		SwitchEquipLoadoutResponse sw7Resp;
		PlayerEquipLoadoutSystem::SwitchAt(player_, sw7, afterExpiry, sw7Resp);
		ASSERT_FALSE(sw7Resp.has_error_message()) << sw7Resp.error_message().id();
		EXPECT_EQ(7u, sw7Resp.snapshot().active_scheme_id());
		EXPECT_EQ(7002u, Equipment().Layout().At(3));
	}
}

TEST_F(PlayerEquipLoadoutTest, LegacySeparateUnlockRejectedWithoutCharging)
{
	GrantCurrency(kCurrencyDiamond, 500);
	GrantCurrency(kCurrencyBindDiamond, 200);
	GrantTalisman(2);

	UnlockEquipLoadoutRequest req;
	req.set_client_request_id(777001);
	req.set_expected_unlocked_count(2);
	req.set_use_bind_diamond_first(true);

	UnlockEquipLoadoutResponse resp;
	PlayerEquipLoadoutSystem::Unlock(player_, req, resp);
	EXPECT_EQ(UnlockEquipLoadoutResponse::REJECTED, resp.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kFeatureUnavailable), resp.error_message().id());
	EXPECT_EQ(2u, resp.current_unlocked_count());
	EXPECT_EQ(500u, CurrencySystem::GetBalance(player_, kCurrencyDiamond));
	EXPECT_EQ(200u, CurrencySystem::GetBalance(player_, kCurrencyBindDiamond));
	EXPECT_EQ(2u, Inventory().GetTotalItemCount(equiploadout::kUnlockItemConfigId));
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

	// 准备两把武器与 2 套装备方案（方案 1 与方案 2 均永久免费）
	ASSERT_EQ(kSuccess, Equipment().PutInstance(MakeEquipItem(6001, 1101), 3));
	ASSERT_EQ(kSuccess, Inventory().PutInstance(MakeEquipItem(6002, 1101)));

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

TEST_F(PlayerEquipLoadoutTest, SwitchAppliesLinkedPetAndWuxingFormationWithGracefulFallbackOnMissingPet)
{
	// 挂载灵宠组件（2 只 1 级可带灵宠）与赛季组件（承接 5 套免费五行阵法）
	auto &petComp = tlsEcs.actorRegistry.emplace<PlayerPetComp>(player_);
	auto *pet1 = petComp.add_pets();
	pet1->set_pet_id(770001);
	pet1->set_pet_table_id(PetSystem::kStarterPetTableId);
	pet1->set_name("群攻灵狐");
	pet1->set_level(1);
	auto *pet2 = petComp.add_pets();
	pet2->set_pet_id(770002);
	pet2->set_pet_table_id(PetSystem::kStarterPetTableId);
	pet2->set_name("高速控宠");
	pet2->set_level(1);
	petComp.set_active_pet_id(770001);

	auto &seasonComp = tlsEcs.actorRegistry.emplace<PlayerSeasonComp>(player_);
	seasonComp.set_equipped_formation_id(1);

	ASSERT_EQ(kSuccess, Equipment().PutInstance(MakeEquipItem(6101, 1101), 3));
	ASSERT_EQ(kSuccess, Inventory().PutInstance(MakeEquipItem(6102, 1101)));

	// 1) 快照只读返回可选灵宠与 5 套免费阵法
	{
		GetEquipLoadoutsResponse getResp;
		ASSERT_EQ(kSuccess, PlayerEquipLoadoutSystem::Get(player_, getResp));
		ASSERT_EQ(2, getResp.snapshot().pets_size());
		EXPECT_EQ(770001u, getResp.snapshot().pets(0).pet_id());
		EXPECT_TRUE(getResp.snapshot().pets(0).active());
		EXPECT_EQ("群攻灵狐", getResp.snapshot().pets(0).name());
		EXPECT_EQ(770002u, getResp.snapshot().pets(1).pet_id());
		EXPECT_FALSE(getResp.snapshot().pets(1).active());
		ASSERT_EQ(5, getResp.snapshot().formations_size());
		EXPECT_EQ(1u, getResp.snapshot().formations(0).scheme_id());
		EXPECT_EQ("天覆阵", getResp.snapshot().formations(0).name());
		EXPECT_EQ(5u, getResp.snapshot().formations(4).scheme_id());
		EXPECT_EQ("虎翼阵", getResp.snapshot().formations(4).name());
	}

	// 2) 非法灵宠 ID 与非法阵法 ID（> 5）均须被保存阶段拒绝
	{
		SaveEquipLoadoutRequest badPet;
		badPet.set_scheme_id(1);
		badPet.set_capture_current_equipped(true);
		badPet.set_linked_pet_id(999999);
		badPet.set_state_version(CurrentVersion());
		badPet.set_request_id("bad_linked_pet_req_01");
		SaveEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Save(player_, badPet, resp);
		ASSERT_TRUE(resp.has_error_message());
		EXPECT_EQ(kInvalidParameter, resp.error_message().id());
	}
	{
		SaveEquipLoadoutRequest badFormation;
		badFormation.set_scheme_id(1);
		badFormation.set_capture_current_equipped(true);
		badFormation.set_linked_formation_id(6);
		badFormation.set_state_version(CurrentVersion());
		badFormation.set_request_id("bad_linked_form_req01");
		SaveEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Save(player_, badFormation, resp);
		ASSERT_TRUE(resp.has_error_message());
		EXPECT_EQ(kInvalidParameter, resp.error_message().id());
	}

	// 3) 方案 1 绑定 群攻灵狐 + 天覆阵(1)，方案 2 绑定 高速控宠 + 虎翼阵(5)
	{
		SaveEquipLoadoutRequest req;
		req.set_scheme_id(1);
		req.set_capture_current_equipped(true);
		req.set_linked_pet_id(770001);
		req.set_linked_formation_id(1);
		req.set_state_version(CurrentVersion());
		req.set_request_id("pet_form_save1_req_01");
		SaveEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Save(player_, req, resp);
		ASSERT_FALSE(resp.has_error_message()) << resp.error_message().id();
		EXPECT_EQ(770001u, resp.snapshot().schemes(0).linked_pet_id());
		EXPECT_EQ(1u, resp.snapshot().schemes(0).linked_formation_id());
	}
	{
		SaveEquipLoadoutRequest req;
		req.set_scheme_id(2);
		auto *s3 = req.add_slots();
		s3->set_slot(3);
		s3->set_item_guid(6102);
		req.set_linked_pet_id(770002);
		req.set_linked_formation_id(5);
		req.set_state_version(CurrentVersion());
		req.set_request_id("pet_form_save2_req_01");
		SaveEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Save(player_, req, resp);
		ASSERT_FALSE(resp.has_error_message()) << resp.error_message().id();
		EXPECT_EQ(770002u, resp.snapshot().schemes(1).linked_pet_id());
		EXPECT_EQ(5u, resp.snapshot().schemes(1).linked_formation_id());
	}

	// 4) 一键切至方案 2：装备、首发灵宠与五行阵法全部同步生效，零额外提示
	{
		SwitchEquipLoadoutRequest req;
		req.set_scheme_id(2);
		req.set_apply_linked_schemes(true);
		req.set_state_version(CurrentVersion());
		req.set_request_id("pet_form_sw2_req_0001");
		SwitchEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Switch(player_, req, resp);
		ASSERT_FALSE(resp.has_error_message()) << resp.error_message().id();
		EXPECT_TRUE(resp.linked_notices().empty());
		EXPECT_EQ(6102u, Equipment().Layout().At(3));
		EXPECT_EQ(770002u, petComp.active_pet_id());
		EXPECT_EQ(5u, PlayerSeasonSystem::EquippedFormationId(player_));
	}

	// 5) 若方案 1 绑定的灵宠 770001 事后被交易/放生移出灵宠栏，切回方案 1 仍完成换装与切阵(1)，并在 linked_notices 明文提示
	petComp.mutable_pets()->DeleteSubrange(0, 1);
	{
		SwitchEquipLoadoutRequest req;
		req.set_scheme_id(1);
		req.set_apply_linked_schemes(true);
		req.set_state_version(CurrentVersion());
		req.set_request_id("pet_form_sw1_req_0001");
		SwitchEquipLoadoutResponse resp;
		PlayerEquipLoadoutSystem::Switch(player_, req, resp);
		ASSERT_FALSE(resp.has_error_message()) << resp.error_message().id();
		EXPECT_EQ(6101u, Equipment().Layout().At(3));
		EXPECT_EQ(1u, PlayerSeasonSystem::EquippedFormationId(player_));
		EXPECT_EQ(770002u, petComp.active_pet_id()); // 原出战灵宠保持不动
		ASSERT_EQ(1, resp.linked_notices_size());
		EXPECT_NE(std::string::npos, resp.linked_notices(0).find("不在灵宠栏"));
	}
}

TEST_F(PlayerEquipLoadoutTest, RpcHandlerEndToEndSmoke)
{
	GetEquipLoadoutsRequest getReq;
	GetEquipLoadoutsResponse getResp;
	SceneBagClientPlayerHandler::GetEquipLoadouts(player_, &getReq, &getResp);
	ASSERT_FALSE(getResp.has_error_message());
	EXPECT_EQ(2u, getResp.snapshot().unlocked_scheme_count());
	EXPECT_EQ(7u, getResp.snapshot().max_scheme_count());
	EXPECT_EQ(7, getResp.snapshot().schemes_size());
	EXPECT_EQ(2u, getResp.snapshot().free_schemes());
	EXPECT_EQ(0u, getResp.snapshot().light_card_bonus_schemes());
	EXPECT_EQ(0u, getResp.snapshot().rented_extra_slots());
	EXPECT_EQ(30u, getResp.snapshot().rent_unit_price_diamond());
	EXPECT_EQ(30u, getResp.snapshot().rent_period_days());
	EXPECT_EQ(180u, getResp.snapshot().rent_max_remaining_days());
	EXPECT_EQ(0u, getResp.snapshot().next_unlock_cost_diamond());
	EXPECT_FALSE(getResp.snapshot().next_unlock_allow_bind_diamond());
}

} // namespace
