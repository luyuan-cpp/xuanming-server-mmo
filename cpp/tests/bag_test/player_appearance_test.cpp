// 角色形象切换（商城 10 元角色形象卡）与 1 小时免费试用（bit 位图）单测。
// 覆盖纯规则、bit 位图未知高位保留、幂等重放/错参、形象卡扣除与预留保护、1 小时试用到期自动还原与目录同步退避。

#include <gtest/gtest.h>

#include <cstdint>
#include <string>
#include <vector>

#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/type_define/type_define.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_item_hold_comp.pb.h"
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/common/component/player_travel_pass_comp.pb.h"
#include "proto/common/database/player_cache.pb.h"
#include "proto/data_service/data_service.pb.h"
#include "proto/player_inbox/player_inbox.pb.h"
#include "proto/scene/player_appearance.pb.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/appearance_rules.h"
#include "services/scene/player/system/player_appearance.h"
#include "services/scene/player/system/player_travel_pass.h"
#include "table/code/appearancerule_table.h"
#include "table/code/appearanceseries_table.h"
#include "table/code/characterappearance_table.h"
#include "table/code/cosmeticshowcase_table.h"
#include "table/code/item_table.h"
#include "table/code/pet_table.h"
#include "table/code/petcosmetic_table.h"
#include "table/code/skill_table.h"
#include "table/code/skillvfxcosmetic_table.h"
#include "table/code/spiritshopgoods_table.h"
#include "table/code/travelpassseason_table.h"
#include "table/code/travelpasstier_table.h"
#include "table/proto/tip/appearance_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/pet_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
constexpr Guid kPlayerGuid = 960300001;
constexpr uint64_t kNowSec = 1'800'000'000ull;
constexpr uint64_t kNowMs = kNowSec * 1000ull;

TEST(PlayerAppearanceRulesTest, RuleBoundsBitStorageAndUnknownHighBitPreservation)
{
	player_appearance::Rule good{3402, 1, 3600, 3402, 1000};
	EXPECT_EQ(player_appearance::RuleProblem(good), nullptr);

	auto bad = good;
	bad.cardItemId = 0;
	EXPECT_NE(player_appearance::RuleProblem(bad), nullptr);
	bad = good;
	bad.trialDurationSeconds = 59;
	EXPECT_NE(player_appearance::RuleProblem(bad), nullptr);
	bad = good;
	bad.cardItemCount = 0;
	EXPECT_NE(player_appearance::RuleProblem(bad), nullptr);

	// 未知高位 bit(例如第 17 位 = byte 2 bit 1)在置位已知低位 bit 后必须原样保留。
	std::string bits(3, '\0');
	bits[2] = static_cast<char>(1u << 1);
	EXPECT_FALSE(player_appearance::IsTrialUsed(bits, 1));
	EXPECT_TRUE(player_appearance::MarkTrialUsed(bits, 1));
	EXPECT_TRUE(player_appearance::MarkTrialUsed(bits, 11));
	EXPECT_TRUE(player_appearance::IsTrialUsed(bits, 1));
	EXPECT_TRUE(player_appearance::IsTrialUsed(bits, 11));
	EXPECT_FALSE(player_appearance::IsTrialUsed(bits, 2));
	EXPECT_EQ(static_cast<unsigned char>(bits[0]), 0x01u); // id=1 -> bit 0
	EXPECT_EQ(static_cast<unsigned char>(bits[1]), 0x04u); // id=11 -> bit 10 (byte 1 bit 2)
	EXPECT_EQ(static_cast<unsigned char>(bits[2]), 0x02u); // 未知高位保留不变

	EXPECT_EQ(player_appearance::EffectiveAppearance("00_reference_topright_boy", "01_ice_sword_girl", kNowSec + 10, kNowSec),
			  "01_ice_sword_girl");
	EXPECT_EQ(player_appearance::EffectiveAppearance("00_reference_topright_boy", "01_ice_sword_girl", kNowSec, kNowSec),
			  "00_reference_topright_boy");

	// 永久拥有位图与切换计划四分支判定(含 card_unlockable=0 玩法专属开关)。
	std::string ownedBits(3, '\0');
	ownedBits[2] = static_cast<char>(1u << 3);
	EXPECT_TRUE(player_appearance::IsOwned(ownedBits, 2, "00_reference_topright_boy", "00_reference_topright_boy"));
	EXPECT_FALSE(player_appearance::IsOwned(ownedBits, 3, "00_reference_topright_boy", "01_ice_sword_girl"));
	EXPECT_TRUE(player_appearance::MarkOwned(ownedBits, 3));
	EXPECT_TRUE(player_appearance::IsOwned(ownedBits, 3, "00_reference_topright_boy", "01_ice_sword_girl"));
	EXPECT_EQ(static_cast<unsigned char>(ownedBits[2]), 0x08u);

	EXPECT_EQ(player_appearance::PlanSwitch(/*alreadyOwned=*/true, /*expectFreeSwitch=*/true, /*cardUnlockable=*/false),
			  player_appearance::SwitchPlan::kFreeSwitch);
	EXPECT_EQ(player_appearance::PlanSwitch(/*alreadyOwned=*/true, /*expectFreeSwitch=*/false, /*cardUnlockable=*/false),
			  player_appearance::SwitchPlan::kFreeSwitch);
	EXPECT_EQ(player_appearance::PlanSwitch(/*alreadyOwned=*/false, /*expectFreeSwitch=*/true, /*cardUnlockable=*/true),
			  player_appearance::SwitchPlan::kRefuseNotOwned);
	EXPECT_EQ(player_appearance::PlanSwitch(/*alreadyOwned=*/false, /*expectFreeSwitch=*/false, /*cardUnlockable=*/false),
			  player_appearance::SwitchPlan::kRefuseNotCardUnlockable);
	EXPECT_EQ(player_appearance::PlanSwitch(/*alreadyOwned=*/false, /*expectFreeSwitch=*/false, /*cardUnlockable=*/true),
			  player_appearance::SwitchPlan::kCardUnlock);
	EXPECT_FALSE(player_appearance::IsSeriesCompleted(0, 0));
	EXPECT_FALSE(player_appearance::IsSeriesCompleted(2, 3));
	EXPECT_TRUE(player_appearance::IsSeriesCompleted(3, 3));

	// 人物志字段合法性: 全空放行, 四项核心字段齐全(无论有无小名)放行, 半残行或仅填空白一律拒绝。
	EXPECT_TRUE(player_appearance::BiographyFieldsValid("", "", "", "", ""));
	EXPECT_TRUE(player_appearance::BiographyFieldsValid("   ", "\t", "", "\n", ""));
	EXPECT_TRUE(player_appearance::BiographyFieldsValid("陆照禾", "阿禾", "小传", "第四只碗", "正文"));
	EXPECT_TRUE(player_appearance::BiographyFieldsValid("霜见雪", "", "小传", "霜阶扫雪", "正文"));
	EXPECT_FALSE(player_appearance::BiographyFieldsValid("霜见雪", "", "", "霜阶扫雪", "正文"));
	EXPECT_FALSE(player_appearance::BiographyFieldsValid("", "小名", "", "", ""));
	EXPECT_FALSE(player_appearance::BiographyFieldsValid("星璇", "", "小传", "十八星印", "   "));
}

class PlayerAppearanceSystemTest : public testing::Test
{
protected:
	void SetUp() override
	{
		ItemTableManager::Instance().Load();
		PetTableManager::Instance().Load();
		SkillTableManager::Instance().Load();
		AppearanceSeriesTableManager::Instance().Load();
		CharacterAppearanceTableManager::Instance().Load();
		AppearanceRuleTableManager::Instance().Load();
		PetCosmeticTableManager::Instance().Load();
		SkillVfxCosmeticTableManager::Instance().Load();
		CosmeticShowcaseTableManager::Instance().Load();
		SpiritShopGoodsTableManager::Instance().Load();
		TravelPassSeasonTableManager::Instance().Load();
		TravelPassTierTableManager::Instance().Load();
		ASSERT_TRUE(PlayerAppearanceSystem::ValidateTables());

		const auto &ruleRow = *AppearanceRuleTableManager::Instance().FindAll().data().begin();
		cardItemId = ruleRow.card_item_id();
		cardCount = ruleRow.card_item_count();
		trialDurationSec = ruleRow.trial_duration_seconds();

		PlayerAppearanceSystem::ResetTestState();
		PlayerAppearanceSystem::TestHooks hooks;
		hooks.directorySync = [this](const data_service::UpdateRoleAppearanceRequest &req,
									 PlayerAppearanceSystem::DirectorySyncDone done)
		{
			if (!acceptDirectorySend)
			{
				return false;
			}
			dirSyncRequests.push_back(req);
			pendingDirSyncDone = std::move(done);
			return true;
		};
		hooks.broadcast = [this](entt::entity /*p*/, const ActorAppearanceChangedS2C &msg)
		{
			broadcasts.push_back(msg);
		};
		hooks.notify = [this](entt::entity /*p*/, const AppearanceStateNotify &n)
		{
			stateNotifies.push_back(n);
		};
		hooks.save = [this](entt::entity /*p*/, const char *reason)
		{
			saveReasons.emplace_back(reason);
		};
		PlayerAppearanceSystem::SetTestHooks(std::move(hooks));

		player = CreatePlayer(kPlayerGuid, "00_reference_topright_boy", 2);
		PlayerAppearanceSystem::OnPlayerLoaded(player, kNowSec);
	}

	void TearDown() override
	{
		PlayerAppearanceSystem::ResetTestState();
		tlsEcs.playerList.erase(kPlayerGuid);
		if (tlsEcs.actorRegistry.valid(player))
		{
			tlsEcs.actorRegistry.destroy(player);
		}
	}

	entt::entity CreatePlayer(Guid guid, const std::string &appearanceId, uint32_t cards)
	{
		const auto entity = tlsEcs.actorRegistry.create();
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(entity, guid);
		auto &epoch = tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(entity);
		epoch.epoch = 1;
		auto &profile = tlsEcs.actorRegistry.emplace<PlayerProfileComp>(entity);
		profile.set_appearance_id(appearanceId);
		auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(entity);
		bags.bags[kInventory].SetCapacityForRestore(40);
		if (cards > 0)
		{
			bags.bags[kInventory].InsertItemForRestore((uint64_t{1} << 60) + 8801, cardItemId, cards, 0);
		}
		tlsEcs.actorRegistry.emplace<PlayerItemHoldComp>(entity);
		tlsEcs.actorRegistry.emplace<PlayerUint64Comp>(entity);
		auto &currency = tlsEcs.actorRegistry.emplace<CurrencyComp>(entity);
		while (currency.values_size() < static_cast<int>(kCurrencyMax))
		{
			currency.add_values(0);
		}
		tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(entity);
		tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(entity);
		auto &lvl = tlsEcs.actorRegistry.emplace<LevelComp>(entity);
		lvl.set_level(20);
		tlsEcs.actorRegistry.emplace<PlayerTravelPassComp>(entity);
		tlsEcs.playerList.emplace(guid, entity);
		return entity;
	}

	PlayerAllData SnapshotCurrent() const
	{
		PlayerAllData all;
		*all.mutable_player_database_data()->mutable_profile_component() = tlsEcs.actorRegistry.get<PlayerProfileComp>(player);
		return all;
	}

	uint64_t CardCountInBag() const
	{
		return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory].GetTotalItemCount(cardItemId);
	}

	uint32_t cardItemId{0};
	uint32_t cardCount{0};
	uint32_t trialDurationSec{0};
	entt::entity player{entt::null};
	bool acceptDirectorySend{true};
	std::vector<data_service::UpdateRoleAppearanceRequest> dirSyncRequests;
	PlayerAppearanceSystem::DirectorySyncDone pendingDirSyncDone;
	std::vector<ActorAppearanceChangedS2C> broadcasts;
	std::vector<AppearanceStateNotify> stateNotifies;
	std::vector<std::string> saveReasons;
};

TEST_F(PlayerAppearanceSystemTest, SwitchConsumesCardIsIdempotentAndSyncsDirectoryOnSaveCommit)
{
	SwitchAppearanceRequest req;
	req.set_client_request_id(1001);
	req.set_appearance_id("01_ice_sword_girl");
	SwitchAppearanceResponse resp;
	ASSERT_EQ(PlayerAppearanceSystem::SwitchAppearance(player, req, kNowSec, resp), kSuccess);
	EXPECT_FALSE(resp.replayed());
	EXPECT_EQ(resp.info().permanent_appearance_id(), "01_ice_sword_girl");
	EXPECT_EQ(resp.info().active_appearance_id(), "01_ice_sword_girl");
	EXPECT_EQ(resp.info().owned_card_count(), 1u);
	EXPECT_EQ(CardCountInBag(), 1u);
	ASSERT_EQ(broadcasts.size(), 1u);
	EXPECT_EQ(broadcasts[0].appearance_id(), "01_ice_sword_girl");
	EXPECT_FALSE(broadcasts[0].is_trial());

	// 同键同参重放:不再扣卡,直接回 replayed = true。
	SwitchAppearanceResponse replayResp;
	ASSERT_EQ(PlayerAppearanceSystem::SwitchAppearance(player, req, kNowSec + 1, replayResp), kSuccess);
	EXPECT_TRUE(replayResp.replayed());
	EXPECT_EQ(CardCountInBag(), 1u);

	// 同键换参:拒绝并零副作用。
	SwitchAppearanceRequest mismatchReq;
	mismatchReq.set_client_request_id(1001);
	mismatchReq.set_appearance_id("02_fire_talisman_boy");
	SwitchAppearanceResponse mismatchResp;
	EXPECT_EQ(PlayerAppearanceSystem::SwitchAppearance(player, mismatchReq, kNowSec + 2, mismatchResp), kInvalidParameter);
	EXPECT_EQ(CardCountInBag(), 1u);

	// 切回已是当前永久形象被拒;无效形象被拒。
	SwitchAppearanceRequest sameReq;
	sameReq.set_client_request_id(1002);
	sameReq.set_appearance_id("01_ice_sword_girl");
	EXPECT_EQ(PlayerAppearanceSystem::SwitchAppearance(player, sameReq, kNowSec + 3, resp),
			  static_cast<uint32_t>(kAppearanceAlreadyCurrent));
	for (const char *removedId : {"30_han_xiangzi", "08_alchemy_prodigy_boy", "04_mountain_guardian_boy", "17_ghost_script_calligrapher_boy"})
	{
		SwitchAppearanceRequest invalidReq;
		invalidReq.set_client_request_id(1003);
		invalidReq.set_appearance_id(removedId);
		EXPECT_EQ(PlayerAppearanceSystem::SwitchAppearance(player, invalidReq, kNowSec + 3, resp),
				  static_cast<uint32_t>(kAppearanceInvalid));
	}

	// 存盘提交后触发选角目录外观同步;瞬时失败由 Tick 退避重试。
	acceptDirectorySend = false;
	PlayerAppearanceSystem::OnSaveCommitted(kPlayerGuid, SnapshotCurrent(), kNowMs + 50);
	EXPECT_TRUE(dirSyncRequests.empty());
	acceptDirectorySend = true;
	PlayerAppearanceSystem::Tick(kNowSec + 3, kNowMs + 5'000);
	ASSERT_EQ(dirSyncRequests.size(), 1u);
	EXPECT_EQ(dirSyncRequests[0].player_id(), kPlayerGuid);
	EXPECT_EQ(dirSyncRequests[0].appearance_id(), "01_ice_sword_girl");
	data_service::UpdateRoleAppearanceResponse dirReply;
	dirReply.set_updated(true);
	pendingDirSyncDone(true, &dirReply);
}

TEST_F(PlayerAppearanceSystemTest, TrialBitStorageOverwriteExpiryAndCancel)
{
	StartAppearanceTrialRequest req;
	req.set_client_request_id(2001);
	req.set_appearance_id("02_fire_talisman_boy");
	StartAppearanceTrialResponse resp;
	ASSERT_EQ(PlayerAppearanceSystem::StartTrial(player, req, kNowSec, resp), kSuccess);
	EXPECT_FALSE(resp.replayed());
	EXPECT_EQ(resp.info().permanent_appearance_id(), "00_reference_topright_boy");
	EXPECT_EQ(resp.info().active_appearance_id(), "02_fire_talisman_boy");
	EXPECT_EQ(resp.info().trial_appearance_id(), "02_fire_talisman_boy");
	EXPECT_EQ(resp.info().trial_expires_at(), kNowSec + trialDurationSec);
	EXPECT_EQ(PlayerAppearanceSystem::EffectiveAppearanceId(player, kNowSec + 10), "02_fire_talisman_boy");
	EXPECT_EQ(CardCountInBag(), 2u); // 免费试用不扣卡

	// 同一形象不可二次试用(新请求键)。
	StartAppearanceTrialRequest secondReq;
	secondReq.set_client_request_id(2002);
	secondReq.set_appearance_id("02_fire_talisman_boy");
	EXPECT_EQ(PlayerAppearanceSystem::StartTrial(player, secondReq, kNowSec + 20, resp),
			  static_cast<uint32_t>(kAppearanceTrialAlreadyUsed));

	// 1 小时到期后 Tick 自动还原永久形象并推 AppearanceStateNotify(trial_expired=true)。
	PlayerAppearanceSystem::Tick(kNowSec + trialDurationSec, (kNowSec + trialDurationSec) * 1000ull);
	EXPECT_EQ(PlayerAppearanceSystem::EffectiveAppearanceId(player, kNowSec + trialDurationSec),
			  "00_reference_topright_boy");
	ASSERT_EQ(stateNotifies.size(), 1u);
	EXPECT_TRUE(stateNotifies[0].trial_expired());
	EXPECT_TRUE(stateNotifies[0].info().trial_appearance_id().empty());

	// 试用另一个未试用形象后可手动提前取消。
	StartAppearanceTrialRequest anotherReq;
	anotherReq.set_client_request_id(2003);
	anotherReq.set_appearance_id("09_bamboo_archer_girl");
	ASSERT_EQ(PlayerAppearanceSystem::StartTrial(player, anotherReq, kNowSec + trialDurationSec + 5, resp), kSuccess);
	PlayerAppearanceInfo cancelInfo;
	ASSERT_EQ(PlayerAppearanceSystem::CancelTrial(player, kNowSec + trialDurationSec + 10, cancelInfo), kSuccess);
	EXPECT_TRUE(cancelInfo.trial_appearance_id().empty());
	EXPECT_EQ(cancelInfo.active_appearance_id(), "00_reference_topright_boy");
	// 再次取消应报无生效试用。
	EXPECT_EQ(PlayerAppearanceSystem::CancelTrial(player, kNowSec + trialDurationSec + 11, cancelInfo),
			  static_cast<uint32_t>(kAppearanceNoActiveTrial));
}

TEST_F(PlayerAppearanceSystemTest, RejectsWhenNoCardOrFrozen)
{
	// 扣光形象卡后再切换报 kAppearanceNoCard。
	auto &bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(player);
	bags.bags[kInventory].RemoveItems({{cardItemId, 2}});
	SwitchAppearanceRequest req;
	req.set_client_request_id(3001);
	req.set_appearance_id("05_celestial_musician_girl");
	SwitchAppearanceResponse resp;
	EXPECT_EQ(PlayerAppearanceSystem::SwitchAppearance(player, req, kNowSec, resp),
			  static_cast<uint32_t>(kAppearanceNoCard));

	// 冻结时试用也拒绝。
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	StartAppearanceTrialRequest trialReq;
	trialReq.set_client_request_id(3002);
	trialReq.set_appearance_id("05_celestial_musician_girl");
	StartAppearanceTrialResponse trialResp;
	EXPECT_EQ(PlayerAppearanceSystem::StartTrial(player, trialReq, kNowSec, trialResp),
			  static_cast<uint32_t>(kAppearanceFrozen));
}

TEST_F(PlayerAppearanceSystemTest, PermanentWardrobeFreeSwitchFailClosedAndUnknownHighBits)
{
	// 预置未知高位 bit,确认只读 FillInfo 隐式把当前永久形象并入响应但不回写持久化字段。
	auto &profile = tlsEcs.actorRegistry.get<PlayerProfileComp>(player);
	std::string seedBits(3, '\0');
	seedBits[2] = static_cast<char>(1u << 1);
	profile.set_appearance_owned_bits(seedBits);

	PlayerAppearanceInfo initialInfo;
	PlayerAppearanceSystem::FillInfo(player, kNowSec, initialInfo);
	EXPECT_EQ(profile.appearance_owned_bits(), seedBits); // 只读不脏写
	EXPECT_TRUE(player_appearance::IsBitMarked(initialInfo.owned_bits(), 2));
	EXPECT_EQ(static_cast<unsigned char>(initialInfo.owned_bits()[2]), 0x02u);
	ASSERT_EQ(initialInfo.appearances_size(), 9);
	for (const auto &entry : initialInfo.appearances())
	{
		// 20_star_formation_master_girl(id=11) 为五行试炼志 M901 专属形象(card_unlockable=0)，其余 8 项支持形象卡解锁。
		EXPECT_EQ(entry.card_unlockable(), entry.appearance_id() != "20_star_formation_master_girl");
		EXPECT_EQ(entry.owned(), entry.appearance_id() == "00_reference_topright_boy");
	}

	// expect_free_switch=true 指向未拥有形象:fail-closed 拒绝,0 扣卡。
	SwitchAppearanceRequest failClosedReq;
	failClosedReq.set_client_request_id(4001);
	failClosedReq.set_appearance_id("02_fire_talisman_boy");
	failClosedReq.set_expect_free_switch(true);
	SwitchAppearanceResponse resp;
	EXPECT_EQ(PlayerAppearanceSystem::SwitchAppearance(player, failClosedReq, kNowSec, resp),
			  static_cast<uint32_t>(kAppearanceNotOwned));
	EXPECT_EQ(CardCountInBag(), 2u);

	// 扣 1 张卡买断解锁 02_fire_talisman_boy:旧永久 00 与新 02 同时入衣柜,且未知高位保留。
	SwitchAppearanceRequest unlockReq;
	unlockReq.set_client_request_id(4002);
	unlockReq.set_appearance_id("02_fire_talisman_boy");
	unlockReq.set_expect_free_switch(false);
	ASSERT_EQ(PlayerAppearanceSystem::SwitchAppearance(player, unlockReq, kNowSec + 1, resp), kSuccess);
	EXPECT_EQ(CardCountInBag(), 1u);
	EXPECT_TRUE(player_appearance::IsBitMarked(profile.appearance_owned_bits(), 2));
	EXPECT_TRUE(player_appearance::IsBitMarked(profile.appearance_owned_bits(), 4));
	EXPECT_EQ(static_cast<unsigned char>(profile.appearance_owned_bits()[2]), 0x02u);

	// 同键改 expect_free_switch 必须判定参数不匹配。
	SwitchAppearanceRequest flippedReq = unlockReq;
	flippedReq.set_expect_free_switch(true);
	EXPECT_EQ(PlayerAppearanceSystem::SwitchAppearance(player, flippedReq, kNowSec + 2, resp), kInvalidParameter);

	// 扣光背包剩余形象卡,验证已拥有形象双向免费切换(expect_free_switch=true/false 均 0 扣卡)。
	auto &bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(player);
	bags.bags[kInventory].RemoveItems({{cardItemId, 1}});
	ASSERT_EQ(CardCountInBag(), 0u);

	// 对已拥有的非当前形象发起试用应拒并返回 kAppearanceAlreadyOwned,不污染试用位图。
	StartAppearanceTrialRequest ownedTrialReq;
	ownedTrialReq.set_client_request_id(4003);
	ownedTrialReq.set_appearance_id("00_reference_topright_boy");
	StartAppearanceTrialResponse ownedTrialResp;
	EXPECT_EQ(PlayerAppearanceSystem::StartTrial(player, ownedTrialReq, kNowSec + 3, ownedTrialResp),
			  static_cast<uint32_t>(kAppearanceAlreadyOwned));
	EXPECT_TRUE(profile.appearance_trial_used_bits().empty());

	// 0 卡免费切回 00_reference_topright_boy。
	SwitchAppearanceRequest freeBackReq;
	freeBackReq.set_client_request_id(4004);
	freeBackReq.set_appearance_id("00_reference_topright_boy");
	freeBackReq.set_expect_free_switch(true);
	ASSERT_EQ(PlayerAppearanceSystem::SwitchAppearance(player, freeBackReq, kNowSec + 4, resp), kSuccess);
	EXPECT_EQ(resp.info().permanent_appearance_id(), "00_reference_topright_boy");
	EXPECT_EQ(CardCountInBag(), 0u);

	// 0 卡且 expect_free_switch=false 再次切到已拥有的 02_fire_talisman_boy,同样走免费分支。
	SwitchAppearanceRequest freeForwardReq;
	freeForwardReq.set_client_request_id(4005);
	freeForwardReq.set_appearance_id("02_fire_talisman_boy");
	freeForwardReq.set_expect_free_switch(false);
	ASSERT_EQ(PlayerAppearanceSystem::SwitchAppearance(player, freeForwardReq, kNowSec + 5, resp), kSuccess);
	EXPECT_EQ(resp.info().permanent_appearance_id(), "02_fire_talisman_boy");
	EXPECT_EQ(CardCountInBag(), 0u);
}

TEST_F(PlayerAppearanceSystemTest, GrantAppearanceIsIdempotentAndPreservesActiveAndTrial)
{
	// 正在试用 03_lotus_healer_girl 时,内部授形 05_celestial_musician_girl 不中断试用也不改变当前永久形象。
	StartAppearanceTrialRequest trialReq;
	trialReq.set_client_request_id(5001);
	trialReq.set_appearance_id("03_lotus_healer_girl");
	StartAppearanceTrialResponse trialResp;
	ASSERT_EQ(PlayerAppearanceSystem::StartTrial(player, trialReq, kNowSec, trialResp), kSuccess);
	const size_t savesBefore = saveReasons.size();
	const size_t notifiesBefore = stateNotifies.size();

	bool alreadyOwned = true;
	ASSERT_EQ(PlayerAppearanceSystem::GrantAppearance(player, "05_celestial_musician_girl", kNowSec + 1, &alreadyOwned),
			  kSuccess);
	EXPECT_FALSE(alreadyOwned);
	EXPECT_EQ(saveReasons.size(), savesBefore + 1);
	EXPECT_EQ(stateNotifies.size(), notifiesBefore + 1);
	EXPECT_FALSE(stateNotifies.back().trial_expired());
	EXPECT_EQ(stateNotifies.back().info().permanent_appearance_id(), "00_reference_topright_boy");
	EXPECT_EQ(stateNotifies.back().info().active_appearance_id(), "03_lotus_healer_girl");

	// 重复授形幂等无副作用。
	ASSERT_EQ(PlayerAppearanceSystem::GrantAppearance(player, "05_celestial_musician_girl", kNowSec + 2, &alreadyOwned),
			  kSuccess);
	EXPECT_TRUE(alreadyOwned);
	EXPECT_EQ(saveReasons.size(), savesBefore + 1);
	EXPECT_EQ(stateNotifies.size(), notifiesBefore + 1);

	// 扣光卡后可凭授形结果免费切到 05_celestial_musician_girl。
	auto &bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(player);
	bags.bags[kInventory].RemoveItems({{cardItemId, 2}});
	SwitchAppearanceRequest freeSwitchReq;
	freeSwitchReq.set_client_request_id(5002);
	freeSwitchReq.set_appearance_id("05_celestial_musician_girl");
	freeSwitchReq.set_expect_free_switch(true);
	SwitchAppearanceResponse switchResp;
	ASSERT_EQ(PlayerAppearanceSystem::SwitchAppearance(player, freeSwitchReq, kNowSec + 3, switchResp), kSuccess);
	EXPECT_EQ(switchResp.info().permanent_appearance_id(), "05_celestial_musician_girl");
	EXPECT_TRUE(switchResp.info().trial_appearance_id().empty());
}

TEST_F(PlayerAppearanceSystemTest, SeriesProgressTracksOwnershipIgnoresTrialsAndCompletesSeries)
{
	// 初始仅拥有 00_reference_topright_boy(留灯院旧友 1/3), 其余两册 0/3 与 0/2。
	PlayerAppearanceInfo info;
	PlayerAppearanceSystem::FillInfo(player, kNowSec, info);
	ASSERT_EQ(info.series_progress_size(), 3);
	EXPECT_EQ(info.series_progress(0).series_id(), 1u);
	EXPECT_EQ(info.series_progress(0).series_key(), "lantern_courtyard");
	EXPECT_EQ(info.series_progress(0).owned_count(), 1u);
	EXPECT_EQ(info.series_progress(0).total_count(), 3u);
	EXPECT_FALSE(info.series_progress(0).completed());
	EXPECT_EQ(info.series_progress(0).member_appearance_ids_size(), 3);
	EXPECT_EQ(info.series_progress(1).series_id(), 2u);
	EXPECT_EQ(info.series_progress(1).owned_count(), 0u);
	EXPECT_EQ(info.series_progress(1).total_count(), 3u);
	EXPECT_EQ(info.series_progress(2).series_id(), 3u);
	EXPECT_EQ(info.series_progress(2).owned_count(), 0u);
	EXPECT_EQ(info.series_progress(2).total_count(), 2u);

	// 试用同系列成员 09_bamboo_archer_girl 绝不计入系列拥有进度。
	StartAppearanceTrialRequest trialReq;
	trialReq.set_client_request_id(6001);
	trialReq.set_appearance_id("09_bamboo_archer_girl");
	StartAppearanceTrialResponse trialResp;
	ASSERT_EQ(PlayerAppearanceSystem::StartTrial(player, trialReq, kNowSec + 1, trialResp), kSuccess);
	EXPECT_EQ(trialResp.info().series_progress(0).owned_count(), 1u);
	EXPECT_FALSE(trialResp.info().series_progress(0).completed());

	// 消耗形象卡解锁 09_bamboo_archer_girl -> 2/3, 仍未集齐。
	SwitchAppearanceRequest unlockReq;
	unlockReq.set_client_request_id(6002);
	unlockReq.set_appearance_id("09_bamboo_archer_girl");
	SwitchAppearanceResponse switchResp;
	ASSERT_EQ(PlayerAppearanceSystem::SwitchAppearance(player, unlockReq, kNowSec + 2, switchResp), kSuccess);
	EXPECT_EQ(switchResp.info().series_progress(0).owned_count(), 2u);
	EXPECT_FALSE(switchResp.info().series_progress(0).completed());

	// 内部授形 10_crimson_spear_girl -> 留灯院旧友 3/3 集齐点亮印章与合篇。
	ASSERT_EQ(PlayerAppearanceSystem::GrantAppearance(player, "10_crimson_spear_girl", kNowSec + 3), kSuccess);
	ASSERT_FALSE(stateNotifies.empty());
	const auto &completedSeries = stateNotifies.back().info().series_progress(0);
	EXPECT_EQ(completedSeries.owned_count(), 3u);
	EXPECT_EQ(completedSeries.total_count(), 3u);
	EXPECT_TRUE(completedSeries.completed());
	EXPECT_EQ(completedSeries.completion_badge_name(), "留灯同袍");
	EXPECT_EQ(completedSeries.epilogue_title(), "灯下三碗面");
	EXPECT_EQ(stateNotifies.back().info().series_progress(1).owned_count(), 0u);
	EXPECT_EQ(stateNotifies.back().info().series_progress(2).owned_count(), 0u);
}

TEST_F(PlayerAppearanceSystemTest, AllNineActiveAppearancesHaveCompleteBiographiesWithoutRemovedName)
{
	EXPECT_TRUE(PlayerAppearanceSystem::ValidateTables());
	const auto &rows = CharacterAppearanceTableManager::Instance().FindAll().data();
	ASSERT_EQ(rows.size(), 9);
	uint32_t withBiography = 0;
	for (const auto &row : rows)
	{
		if (row.enabled() == 0)
		{
			continue;
		}
		EXPECT_TRUE(player_appearance::BiographyFieldsValid(
			row.archive_name(),
			row.nickname(),
			row.biography_summary(),
			row.story_title(),
			row.story_body()))
			<< "id=" << row.id();
		EXPECT_FALSE(row.archive_name().empty()) << "id=" << row.id();
		EXPECT_FALSE(row.biography_summary().empty()) << "id=" << row.id();
		EXPECT_FALSE(row.story_title().empty()) << "id=" << row.id();
		EXPECT_FALSE(row.story_body().empty()) << "id=" << row.id();
		EXPECT_EQ(row.biography_summary().find("闻砚生"), std::string::npos) << "id=" << row.id();
		EXPECT_EQ(row.story_body().find("闻砚生"), std::string::npos) << "id=" << row.id();
		++withBiography;
	}
	EXPECT_EQ(withBiography, 9u);
}

TEST(PlayerAppearanceRulesTest, PetAndSkillCosmeticBitmapsTintAndSpeciesRules)
{
	std::string petBits(2, '\0');
	petBits[1] = static_cast<char>(1u << 4); // 未知高位保留
	EXPECT_FALSE(player_appearance::IsPetCosmeticOwned(petBits, 1));
	EXPECT_TRUE(player_appearance::MarkPetCosmeticOwned(petBits, 1));
	EXPECT_TRUE(player_appearance::IsPetCosmeticOwned(petBits, 1));
	EXPECT_EQ(static_cast<unsigned char>(petBits[1]), 0x10u);

	std::string vfxBits(2, '\0');
	vfxBits[1] = static_cast<char>(1u << 2);
	EXPECT_FALSE(player_appearance::IsSkillVfxOwned(vfxBits, 2));
	EXPECT_TRUE(player_appearance::MarkSkillVfxOwned(vfxBits, 2));
	EXPECT_TRUE(player_appearance::IsSkillVfxOwned(vfxBits, 2));
	EXPECT_EQ(static_cast<unsigned char>(vfxBits[1]), 0x04u);

	EXPECT_TRUE(player_appearance::IsPetSpeciesCompatible(0, 6));
	EXPECT_TRUE(player_appearance::IsPetSpeciesCompatible(6, 6));
	EXPECT_FALSE(player_appearance::IsPetSpeciesCompatible(6, 7));

	EXPECT_TRUE(player_appearance::IsValidTintHex(""));
	EXPECT_TRUE(player_appearance::IsValidTintHex("#FFE28A"));
	EXPECT_TRUE(player_appearance::IsValidTintHex("#00ffaa"));
	EXPECT_FALSE(player_appearance::IsValidTintHex("FFE28A"));
	EXPECT_FALSE(player_appearance::IsValidTintHex("#FFF"));
	EXPECT_FALSE(player_appearance::IsValidTintHex("#GGGGGG"));
}

TEST_F(PlayerAppearanceSystemTest, PetCosmeticGrantEquipClearAndPerInstanceIsolation)
{
	auto &petComp = tlsEcs.actorRegistry.emplace<PlayerPetComp>(player);
	auto *pet1 = petComp.add_pets();
	pet1->set_pet_id(70001);
	pet1->set_pet_table_id(6); // 绛铃
	auto *pet2 = petComp.add_pets();
	pet2->set_pet_id(70002);
	pet2->set_pet_table_id(5); // 烛翎

	// 未拥有且 expect_free_equip=true 时拒绝穿戴，不改灵宠实例；expect_free_equip=false 但无幻化符时返 kAppearanceNoCard。
	EquipPetCosmeticRequest req;
	req.set_client_request_id(8001);
	req.set_pet_id(70001);
	req.set_cosmetic_id(1);
	req.set_expect_free_equip(true);
	EquipPetCosmeticResponse resp;
	EXPECT_EQ(PlayerAppearanceSystem::EquipPetCosmetic(player, req, kNowSec + 1, resp),
			  static_cast<uint32_t>(kAppearanceNotOwned));
	req.set_expect_free_equip(false);
	EXPECT_EQ(PlayerAppearanceSystem::EquipPetCosmetic(player, req, kNowSec + 1, resp),
			  static_cast<uint32_t>(kAppearanceNoCard));

	// 不存在的灵宠实例拒绝。
	req.set_pet_id(99999);
	EXPECT_EQ(PlayerAppearanceSystem::EquipPetCosmetic(player, req, kNowSec + 2, resp),
			  static_cast<uint32_t>(kPetNotFound));

	// 内部授权授予灵宠幻化 id=1(苍嶂麟)：不自动穿戴，且重复授予幂等。
	bool alreadyOwned = true;
	ASSERT_EQ(PlayerAppearanceSystem::GrantPetCosmetic(player, 1, kNowSec + 3, &alreadyOwned), kSuccess);
	EXPECT_FALSE(alreadyOwned);
	ASSERT_EQ(PlayerAppearanceSystem::GrantPetCosmetic(player, 1, kNowSec + 4, &alreadyOwned), kSuccess);
	EXPECT_TRUE(alreadyOwned);
	EXPECT_EQ(petComp.pets(0).cosmetic_id(), 0u);

	// 仅对灵宠 70001 穿戴幻化 id=1，灵宠 70002 仍为原形。
	req.set_client_request_id(8002);
	req.set_pet_id(70001);
	req.set_cosmetic_id(1);
	ASSERT_EQ(PlayerAppearanceSystem::EquipPetCosmetic(player, req, kNowSec + 5, resp), kSuccess);
	EXPECT_FALSE(resp.replayed());
	EXPECT_EQ(resp.pet_id(), 70001u);
	EXPECT_EQ(resp.equipped_cosmetic_id(), 1u);
	EXPECT_EQ(petComp.pets(0).cosmetic_id(), 1u);
	EXPECT_EQ(petComp.pets(1).cosmetic_id(), 0u);

	uint32_t resolvedId = 0;
	std::string resolvedArt;
	PlayerAppearanceSystem::ResolvePetCosmeticVisual(player, petComp.pets(0), resolvedId, resolvedArt);
	EXPECT_EQ(resolvedId, 1u);
	EXPECT_EQ(resolvedArt, "07-cangzhanglin");

	// 重复穿戴同一幻化幂等收敛。
	req.set_client_request_id(8003);
	ASSERT_EQ(PlayerAppearanceSystem::EquipPetCosmetic(player, req, kNowSec + 6, resp), kSuccess);
	EXPECT_TRUE(resp.replayed());
	EXPECT_EQ(resp.equipped_cosmetic_id(), 1u);

	// 清除幻化(cosmetic_id=0)恢复原形，重复清除幂等收敛。
	req.set_client_request_id(8004);
	req.set_cosmetic_id(0);
	ASSERT_EQ(PlayerAppearanceSystem::EquipPetCosmetic(player, req, kNowSec + 7, resp), kSuccess);
	EXPECT_FALSE(resp.replayed());
	EXPECT_EQ(resp.equipped_cosmetic_id(), 0u);
	EXPECT_EQ(petComp.pets(0).cosmetic_id(), 0u);

	req.set_client_request_id(8005);
	ASSERT_EQ(PlayerAppearanceSystem::EquipPetCosmetic(player, req, kNowSec + 8, resp), kSuccess);
	EXPECT_TRUE(resp.replayed());

	// 角色冻结期间拒绝修改。
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	req.set_client_request_id(8006);
	req.set_cosmetic_id(1);
	EXPECT_EQ(PlayerAppearanceSystem::EquipPetCosmetic(player, req, kNowSec + 9, resp),
			  static_cast<uint32_t>(kAppearanceFrozen));
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
}

TEST_F(PlayerAppearanceSystemTest, SkillVfxGrantEquipClearAndBattleSnapshotPropagation)
{
	auto &cls = tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(player);
	cls.set_class_(2); // 玄霄
	auto &skills = tlsEcs.actorRegistry.emplace<PlayerSkillListComp>(player);

	// 未拥有且 expect_free_equip=true 时拒绝启用；未习得技能前优先拒回 kAppearanceInvalid，避免误扣券。
	EquipSkillVfxRequest req;
	req.set_client_request_id(9001);
	req.set_skill_table_id(1201);
	req.set_cosmetic_id(1);
	req.set_expect_free_equip(true);
	EquipSkillVfxResponse resp;
	EXPECT_EQ(PlayerAppearanceSystem::EquipSkillVfx(player, req, kNowSec + 1, resp),
			  static_cast<uint32_t>(kAppearanceInvalid));

	// 习得 1201 一重后，未拥有且 expect_free_equip=true 拒回 kAppearanceNotOwned，expect_free_equip=false 无券拒回 kAppearanceNoCard。
	auto *learned = skills.add_skill_list();
	learned->set_skill_table_id(1201);
	learned->set_rank(1);
	EXPECT_EQ(PlayerAppearanceSystem::EquipSkillVfx(player, req, kNowSec + 1, resp),
			  static_cast<uint32_t>(kAppearanceNotOwned));
	req.set_expect_free_equip(false);
	EXPECT_EQ(PlayerAppearanceSystem::EquipSkillVfx(player, req, kNowSec + 1, resp),
			  static_cast<uint32_t>(kAppearanceNoCard));

	// 授予技能特效 id=1(适用于 1201 玄符落曜)，错配技能号 1202 仍拒绝；正确技能号 1201 成功启用并透传战斗快照。
	bool alreadyOwned = true;
	ASSERT_EQ(PlayerAppearanceSystem::GrantSkillVfx(player, 1, kNowSec + 2, &alreadyOwned), kSuccess);
	EXPECT_FALSE(alreadyOwned);

	req.set_client_request_id(9002);
	req.set_skill_table_id(1202);
	req.set_cosmetic_id(1);
	EXPECT_EQ(PlayerAppearanceSystem::EquipSkillVfx(player, req, kNowSec + 4, resp),
			  static_cast<uint32_t>(kAppearanceInvalid));

	req.set_client_request_id(9003);
	req.set_skill_table_id(1201);
	req.set_cosmetic_id(1);
	ASSERT_EQ(PlayerAppearanceSystem::EquipSkillVfx(player, req, kNowSec + 5, resp), kSuccess);
	EXPECT_FALSE(resp.replayed());
	EXPECT_EQ(resp.skill_table_id(), 1201u);
	EXPECT_EQ(resp.equipped_cosmetic_id(), 1u);
	ASSERT_EQ(resp.info().equipped_skill_vfx().count(1201u), 1u);
	EXPECT_EQ(resp.info().equipped_skill_vfx().at(1201u), 1u);

	BattlePlayerSnapshot battleSnap;
	PlayerAppearanceSystem::FillBattleSkillVfx(player, battleSnap);
	ASSERT_EQ(battleSnap.equipped_skill_vfx().count(1201u), 1u);
	EXPECT_EQ(battleSnap.equipped_skill_vfx().at(1201u), 1u);

	// 重复启用幂等收敛。
	req.set_client_request_id(9004);
	ASSERT_EQ(PlayerAppearanceSystem::EquipSkillVfx(player, req, kNowSec + 6, resp), kSuccess);
	EXPECT_TRUE(resp.replayed());

	// 恢复默认(cosmetic_id=0)清除映射并使战斗快照为空。
	req.set_client_request_id(9005);
	req.set_cosmetic_id(0);
	ASSERT_EQ(PlayerAppearanceSystem::EquipSkillVfx(player, req, kNowSec + 7, resp), kSuccess);
	EXPECT_FALSE(resp.replayed());
	EXPECT_EQ(resp.equipped_cosmetic_id(), 0u);
	EXPECT_EQ(resp.info().equipped_skill_vfx().count(1201u), 0u);

	PlayerAppearanceSystem::FillBattleSkillVfx(player, battleSnap);
	EXPECT_TRUE(battleSnap.equipped_skill_vfx().empty());
}

TEST_F(PlayerAppearanceSystemTest, PetAndSkillCosmeticVoucherUnlockIdempotencyAndExclusiveGuard)
{
	auto &petComp = tlsEcs.actorRegistry.emplace<PlayerPetComp>(player);
	auto *pet1 = petComp.add_pets();
	pet1->set_pet_id(71001);
	pet1->set_pet_table_id(6);
	auto &cls = tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(player);
	cls.set_class_(2);
	auto &skills = tlsEcs.actorRegistry.emplace<PlayerSkillListComp>(player);
	auto *learned = skills.add_skill_list();
	learned->set_skill_table_id(1201);
	learned->set_rank(1);

	auto &bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(player);
	bags.bags[kInventory].InsertItemForRestore((uint64_t{1} << 60) + 9101, 3411, 2, 0);
	bags.bags[kInventory].InsertItemForRestore((uint64_t{1} << 60) + 9102, 3412, 2, 0);

	PlayerAppearanceInfo info;
	PlayerAppearanceSystem::FillInfo(player, kNowSec, info);
	ASSERT_GE(info.pet_cosmetics_size(), 3);
	EXPECT_EQ(info.pet_cosmetics(0).card_item_id(), 3411u);
	EXPECT_EQ(info.pet_cosmetics(0).card_item_count(), 1u);
	EXPECT_EQ(info.pet_cosmetics(0).owned_card_count(), 2u);
	EXPECT_EQ(info.pet_cosmetics(0).spirit_shop_goods_id(), 303u);
	EXPECT_EQ(info.pet_cosmetics(2).card_item_id(), 0u);
	ASSERT_GE(info.skill_vfx_cosmetics_size(), 3);
	EXPECT_EQ(info.skill_vfx_cosmetics(0).card_item_id(), 3412u);
	EXPECT_EQ(info.skill_vfx_cosmetics(0).owned_card_count(), 2u);
	EXPECT_EQ(info.skill_vfx_cosmetics(0).spirit_shop_goods_id(), 304u);
	EXPECT_EQ(info.skill_vfx_cosmetics(2).card_item_id(), 0u);

	// 专属灵宠幻化(id=3 card_item_id=0)不允许耗券解锁，绝不扣券。
	EquipPetCosmeticRequest petReq;
	petReq.set_client_request_id(9501);
	petReq.set_pet_id(71001);
	petReq.set_cosmetic_id(3);
	petReq.set_expect_free_equip(false);
	EquipPetCosmeticResponse petResp;
	EXPECT_EQ(PlayerAppearanceSystem::EquipPetCosmetic(player, petReq, kNowSec + 1, petResp),
			  static_cast<uint32_t>(kAppearanceNotCardUnlockable));
	EXPECT_EQ(bags.bags[kInventory].GetTotalItemCount(3411), 2u);

	// 耗 1 张 3411 解锁并穿戴灵宠幻化 id=1，同键重放不二次扣券，换参拒回 kInvalidParameter。
	petReq.set_client_request_id(9502);
	petReq.set_cosmetic_id(1);
	ASSERT_EQ(PlayerAppearanceSystem::EquipPetCosmetic(player, petReq, kNowSec + 2, petResp), kSuccess);
	EXPECT_FALSE(petResp.replayed());
	EXPECT_EQ(petResp.equipped_cosmetic_id(), 1u);
	EXPECT_EQ(bags.bags[kInventory].GetTotalItemCount(3411), 1u);
	EXPECT_EQ(petResp.info().pet_cosmetics(0).owned_card_count(), 1u);
	EXPECT_TRUE(petResp.info().pet_cosmetics(0).owned());

	ASSERT_EQ(PlayerAppearanceSystem::EquipPetCosmetic(player, petReq, kNowSec + 3, petResp), kSuccess);
	EXPECT_TRUE(petResp.replayed());
	EXPECT_EQ(bags.bags[kInventory].GetTotalItemCount(3411), 1u);

	EquipPetCosmeticRequest badPetReq = petReq;
	badPetReq.set_cosmetic_id(2);
	EXPECT_EQ(PlayerAppearanceSystem::EquipPetCosmetic(player, badPetReq, kNowSec + 4, petResp), kInvalidParameter);

	// 专属技能特效(id=3 对应 1203)及耗券解锁技能特效 id=1(3412) + 幂等重放。
	auto *learned3 = skills.add_skill_list();
	learned3->set_skill_table_id(1203);
	learned3->set_rank(1);
	EquipSkillVfxRequest vfxReq;
	vfxReq.set_client_request_id(9601);
	vfxReq.set_skill_table_id(1203);
	vfxReq.set_cosmetic_id(3);
	vfxReq.set_expect_free_equip(false);
	EquipSkillVfxResponse vfxResp;
	EXPECT_EQ(PlayerAppearanceSystem::EquipSkillVfx(player, vfxReq, kNowSec + 5, vfxResp),
			  static_cast<uint32_t>(kAppearanceNotCardUnlockable));
	EXPECT_EQ(bags.bags[kInventory].GetTotalItemCount(3412), 2u);

	vfxReq.set_client_request_id(9602);
	vfxReq.set_skill_table_id(1201);
	vfxReq.set_cosmetic_id(1);
	ASSERT_EQ(PlayerAppearanceSystem::EquipSkillVfx(player, vfxReq, kNowSec + 6, vfxResp), kSuccess);
	EXPECT_FALSE(vfxResp.replayed());
	EXPECT_EQ(vfxResp.equipped_cosmetic_id(), 1u);
	EXPECT_EQ(bags.bags[kInventory].GetTotalItemCount(3412), 1u);
	EXPECT_TRUE(vfxResp.info().skill_vfx_cosmetics(0).owned());

	ASSERT_EQ(PlayerAppearanceSystem::EquipSkillVfx(player, vfxReq, kNowSec + 7, vfxResp), kSuccess);
	EXPECT_TRUE(vfxResp.replayed());
	EXPECT_EQ(bags.bags[kInventory].GetTotalItemCount(3412), 1u);

	EquipSkillVfxRequest badVfxReq = vfxReq;
	badVfxReq.set_cosmetic_id(0);
	EXPECT_EQ(PlayerAppearanceSystem::EquipSkillVfx(player, badVfxReq, kNowSec + 8, vfxResp), kInvalidParameter);
}

TEST_F(PlayerAppearanceSystemTest, SteamSeriesBundleQuotesEnforcesRealDiamondAndGuardsConcurrentChange)
{
	// 初始拥有 00_reference_topright_boy（系列 1 留灯院旧友 3 人中已拥 1 人，缺 2 人：09_bamboo_archer_girl, 10_crimson_spear_girl）。
	PlayerAppearanceInfo info;
	PlayerAppearanceSystem::FillInfo(player, kNowSec, info);
	ASSERT_GE(info.series_progress_size(), 1);
	const auto &s1 = info.series_progress(0);
	EXPECT_EQ(s1.series_id(), 1u);
	EXPECT_EQ(s1.missing_unlockable_count(), 2u);
	EXPECT_EQ(s1.bundle_discount_bps(), 8500u);
	EXPECT_EQ(s1.bundle_original_diamond(), 200u);
	EXPECT_EQ(s1.bundle_discount_diamond(), 170u);
	ASSERT_EQ(s1.missing_appearance_ids_size(), 2);

	// 1. 绑定灵石不可抵扣系列补齐包。
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyBindDiamond, 500, TX_GM_GRANT, 1));
	BuyAppearanceSeriesBundleRequest buyReq;
	buyReq.set_client_request_id(9701);
	buyReq.set_series_id(1);
	buyReq.add_expected_missing_appearance_ids("09_bamboo_archer_girl");
	buyReq.add_expected_missing_appearance_ids("10_crimson_spear_girl");
	buyReq.set_expected_price_diamond(170);
	BuyAppearanceSeriesBundleResponse buyResp;
	EXPECT_EQ(PlayerAppearanceSystem::BuySeriesBundle(player, buyReq, kNowSec, buyResp),
			  static_cast<uint32_t>(kAppearanceSeriesBundleCurrencyNotEnough));

	// 2. 充真实灵石后，若并发已获其中 1 人，旧缺失清单报价被 M-3 防线拒绝（绝不多扣钱）。
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyDiamond, 300, TX_GM_GRANT, 2));
	ASSERT_EQ(kSuccess, PlayerAppearanceSystem::GrantAppearance(player, "09_bamboo_archer_girl", kNowSec));
	EXPECT_EQ(PlayerAppearanceSystem::BuySeriesBundle(player, buyReq, kNowSec, buyResp),
			  static_cast<uint32_t>(kAppearanceSeriesBundlePriceChanged));
	EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyDiamond), 300u);

	// 3. 仅差最后 1 人时同样享受 8.5 折 = 85 灵石，且保留未知高位 bit + 幂等重放不双扣。
	auto &profile = tlsEcs.actorRegistry.get<PlayerProfileComp>(player);
	std::string ownedWithHighBit = profile.appearance_owned_bits();
	if (ownedWithHighBit.size() < 4)
	{
		ownedWithHighBit.resize(4, '\0');
	}
	ownedWithHighBit[3] = static_cast<char>(0x40u);
	profile.set_appearance_owned_bits(ownedWithHighBit);

	BuyAppearanceSeriesBundleRequest buyLast;
	buyLast.set_client_request_id(9702);
	buyLast.set_series_id(1);
	buyLast.add_expected_missing_appearance_ids("10_crimson_spear_girl");
	buyLast.set_expected_price_diamond(85);
	ASSERT_EQ(PlayerAppearanceSystem::BuySeriesBundle(player, buyLast, kNowSec, buyResp), kSuccess);
	EXPECT_FALSE(buyResp.replayed());
	EXPECT_EQ(buyResp.unlocked_count(), 1u);
	EXPECT_EQ(buyResp.spent_diamond(), 85u);
	EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyDiamond), 215u);
	EXPECT_TRUE(buyResp.info().series_progress(0).completed());
	EXPECT_EQ(static_cast<unsigned char>(profile.appearance_owned_bits()[3]), 0x40u);

	BuyAppearanceSeriesBundleResponse replayResp;
	ASSERT_EQ(PlayerAppearanceSystem::BuySeriesBundle(player, buyLast, kNowSec + 1, replayResp), kSuccess);
	EXPECT_TRUE(replayResp.replayed());
	EXPECT_EQ(replayResp.unlocked_count(), 1u);
	EXPECT_EQ(replayResp.spent_diamond(), 85u);
	EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyDiamond), 215u);

	BuyAppearanceSeriesBundleRequest alreadyDone = buyLast;
	alreadyDone.set_client_request_id(9703);
	EXPECT_EQ(PlayerAppearanceSystem::BuySeriesBundle(player, alreadyDone, kNowSec + 2, replayResp),
			  static_cast<uint32_t>(kAppearanceSeriesAlreadyCompleted));
}

TEST_F(PlayerAppearanceSystemTest, LightCardGrantsWeeklyExtraWardrobeTrialsWithoutGrantingOwnership)
{
	// 1. 首次免费试穿 01_ice_sword_girl 消耗终身免费位。
	StartAppearanceTrialRequest req;
	req.set_client_request_id(9801);
	req.set_appearance_id("01_ice_sword_girl");
	StartAppearanceTrialResponse resp;
	ASSERT_EQ(PlayerAppearanceSystem::StartTrial(player, req, kNowSec, resp), kSuccess);

	// 无轻享卡时再次试穿同一形象被拒。
	req.set_client_request_id(9802);
	EXPECT_EQ(PlayerAppearanceSystem::StartTrial(player, req, kNowSec + 10, resp),
			  static_cast<uint32_t>(kAppearanceTrialAlreadyUsed));

	// 2. 激活仙途轻享卡后，每周享有 3 次随心试穿，绝不授予永久权，第 4 次被拒。
	tlsEcs.actorRegistry.get<PlayerUint64Comp>(player).set_afk_pass_expires_at(kNowSec + 30 * 86400ull);
	for (uint32_t i = 1; i <= 3; ++i)
	{
		req.set_client_request_id(9810 + i);
		ASSERT_EQ(PlayerAppearanceSystem::StartTrial(player, req, kNowSec + 100 + i, resp), kSuccess);
		EXPECT_EQ(resp.info().light_card_trial_quota(), 3u);
		EXPECT_EQ(resp.info().light_card_trial_used(), i);
		EXPECT_FALSE(player_appearance::IsOwned(
			tlsEcs.actorRegistry.get<PlayerProfileComp>(player).appearance_owned_bits(), 3,
			"00_reference_topright_boy", "01_ice_sword_girl"));
	}
	req.set_client_request_id(9814);
	EXPECT_EQ(PlayerAppearanceSystem::StartTrial(player, req, kNowSec + 200, resp),
			  static_cast<uint32_t>(kAppearanceTrialAlreadyUsed));

	// 3. 跨周（+7 天）重置，且时钟回拨不重置。
	const uint64_t nextWeekSec = kNowSec + 7 * 86400ull;
	req.set_client_request_id(9815);
	ASSERT_EQ(PlayerAppearanceSystem::StartTrial(player, req, nextWeekSec, resp), kSuccess);
	EXPECT_EQ(resp.info().light_card_trial_used(), 1u);
}

TEST_F(PlayerAppearanceSystemTest, ShowcaseCosmeticsVoucherUnlockBattleSnapshotAndM6AuthoritativeFilter)
{
	// 1. 发放 2 张 3417 排面装饰券
	auto &inv = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
	inv.InsertItemForRestore((uint64_t{1} << 60) + 9901, 3417, 2, 1);

	// 专属限定 #13（支持者包）不可用 3417 解锁
	EquipShowcaseCosmeticRequest exclReq;
	exclReq.set_client_request_id(12001);
	exclReq.set_slot_kind(player_appearance::kShowcaseSlotFormation);
	exclReq.set_cosmetic_id(13);
	EquipShowcaseCosmeticResponse exclResp;
	EXPECT_EQ(PlayerAppearanceSystem::EquipShowcaseCosmetic(player, exclReq, kNowSec, exclResp),
			  static_cast<uint32_t>(kAppearanceNotCardUnlockable));

	// 耗 1 张 3417 解锁并装备 #1 金芒八卦阵纹，耗 1 张解锁 #6 青锋破阵旗
	EquipShowcaseCosmeticRequest formReq;
	formReq.set_client_request_id(12002);
	formReq.set_slot_kind(player_appearance::kShowcaseSlotFormation);
	formReq.set_cosmetic_id(1);
	EquipShowcaseCosmeticResponse formResp;
	ASSERT_EQ(PlayerAppearanceSystem::EquipShowcaseCosmetic(player, formReq, kNowSec, formResp), kSuccess);
	EXPECT_FALSE(formResp.replayed());
	EXPECT_EQ(formResp.equipped_cosmetic_id(), 1u);
	EXPECT_EQ(inv.GetTotalItemCount(3417), 1u);

	// 同键幂等重放不二次扣券
	EquipShowcaseCosmeticResponse formReplay;
	ASSERT_EQ(PlayerAppearanceSystem::EquipShowcaseCosmetic(player, formReq, kNowSec, formReplay), kSuccess);
	EXPECT_TRUE(formReplay.replayed());
	EXPECT_EQ(inv.GetTotalItemCount(3417), 1u);

	EquipShowcaseCosmeticRequest bannerReq;
	bannerReq.set_client_request_id(12003);
	bannerReq.set_slot_kind(player_appearance::kShowcaseSlotEntranceVictory);
	bannerReq.set_cosmetic_id(6);
	EquipShowcaseCosmeticResponse bannerResp;
	ASSERT_EQ(PlayerAppearanceSystem::EquipShowcaseCosmetic(player, bannerReq, kNowSec, bannerResp), kSuccess);
	EXPECT_EQ(inv.GetTotalItemCount(3417), 0u);

	// 2. 战斗快照权威透传 formation_skin_id=1, entrance_victory_skin_id=6
	BattlePlayerSnapshot snap;
	PlayerAppearanceSystem::FillBattleSkillVfx(player, snap);
	EXPECT_EQ(snap.formation_skin_id(), 1u);
	EXPECT_EQ(snap.entrance_victory_skin_id(), 6u);

	// 3. M-6 防伪造：若存档被篡改写入未拥有的 #2 阵纹，快照必须静默回落 0
	auto &profile = tlsEcs.actorRegistry.get<PlayerProfileComp>(player);
	(*profile.mutable_equipped_showcase_cosmetics())[player_appearance::kShowcaseSlotFormation] = 2u;
	BattlePlayerSnapshot forgedSnap;
	PlayerAppearanceSystem::FillBattleSkillVfx(player, forgedSnap);
	EXPECT_EQ(forgedSnap.formation_skin_id(), 0u);
	EXPECT_EQ(forgedSnap.entrance_victory_skin_id(), 6u);
}

TEST_F(PlayerAppearanceSystemTest, CityCelebrationSafeCityLimit5AndWeeklyBindGoldCapM5)
{
	auto &inv = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
	inv.InsertItemForRestore((uint64_t{1} << 60) + 9911, 3418, 10, 1); // 天墉灵灯 150 绑银
	inv.InsertItemForRestore((uint64_t{1} << 60) + 9912, 3420, 10, 1); // 仙茶同乐席 400 绑银

	// 非安全主城（scene_config_id=2 野外）禁止施放
	PlayerAppearanceSystem::TestHooks hooks;
	hooks.overrideSceneId = 5001;
	hooks.overrideSceneConfigId = 2;
	std::vector<CityCelebrationNotify> celebNotifies;
	hooks.celebrationBroadcast = [&](uint64_t, const CityCelebrationNotify &n) { celebNotifies.push_back(n); };
	hooks.save = [this](entt::entity, const char *r) { saveReasons.emplace_back(r); };
	PlayerAppearanceSystem::SetTestHooks(hooks);

	CastCityCelebrationRequest badCityReq;
	badCityReq.set_client_request_id(13001);
	badCityReq.set_item_config_id(3418);
	CastCityCelebrationResponse badCityResp;
	EXPECT_EQ(PlayerAppearanceSystem::CastCityCelebration(player, badCityReq, kNowSec, badCityResp),
			  static_cast<uint32_t>(kCityCelebrationNotSafeCity));

	// 切回顾元城主城（scene_config_id=1），连续施放 5 场成功，第 6 场受并发上限 5 拦截且不扣道具
	hooks.overrideSceneConfigId = 1;
	PlayerAppearanceSystem::SetTestHooks(hooks);
	uint64_t firstCelebId = 0;
	for (uint32_t i = 0; i < 5u; ++i)
	{
		CastCityCelebrationRequest cReq;
		cReq.set_client_request_id(13010 + i);
		cReq.set_item_config_id(3420);
		CastCityCelebrationResponse cResp;
		ASSERT_EQ(PlayerAppearanceSystem::CastCityCelebration(player, cReq, kNowSec, cResp), kSuccess);
		if (i == 0) firstCelebId = cResp.celebration().celebration_id();
	}
	EXPECT_EQ(celebNotifies.size(), 5u);
	EXPECT_EQ(inv.GetTotalItemCount(3420), 5u);

	CastCityCelebrationRequest sixthReq;
	sixthReq.set_client_request_id(13020);
	sixthReq.set_item_config_id(3420);
	CastCityCelebrationResponse sixthResp;
	EXPECT_EQ(PlayerAppearanceSystem::CastCityCelebration(player, sixthReq, kNowSec, sixthResp),
			  static_cast<uint32_t>(kCityCelebrationChannelFull));
	EXPECT_EQ(inv.GetTotalItemCount(3420), 5u);

	// 低等级 (<10) 拒领；满 10 级正常领取 400 绑定银两（CURRENCY_TYPE_BIND_GOLD=3），重复领拒回
	tlsEcs.actorRegistry.get<LevelComp>(player).set_level(9);
	ClaimCityCelebrationGiftRequest claimReq;
	claimReq.set_client_request_id(13030);
	claimReq.set_celebration_id(firstCelebId);
	ClaimCityCelebrationGiftResponse claimResp;
	EXPECT_EQ(PlayerAppearanceSystem::ClaimCityCelebrationGift(player, claimReq, kNowSec, claimResp),
			  static_cast<uint32_t>(kCityCelebrationLevelTooLow));

	tlsEcs.actorRegistry.get<LevelComp>(player).set_level(20);
	ASSERT_EQ(PlayerAppearanceSystem::ClaimCityCelebrationGift(player, claimReq, kNowSec, claimResp), kSuccess);
	EXPECT_EQ(claimResp.granted_bind_gold(), 400u);
	EXPECT_EQ(claimResp.weekly_claimed_total(), 400u);
	EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyBindGold), 400u);
	EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 0u); // 绝不发流通银两

	// 同一演出换键二次领拒回
	claimReq.set_client_request_id(13031);
	EXPECT_EQ(PlayerAppearanceSystem::ClaimCityCelebrationGift(player, claimReq, kNowSec, claimResp),
			  static_cast<uint32_t>(kCityCelebrationAlreadyClaimed));

	// 模拟本周已领 2900 绑银：领下一场 400 绑银应截断至 100（总 3000），再领第 3 场拒回上限
	tlsEcs.actorRegistry.get<PlayerProfileComp>(player).set_celebration_bind_gold_claimed(2900u);
	claimReq.set_client_request_id(13032);
	claimReq.set_celebration_id(firstCelebId + 1);
	ASSERT_EQ(PlayerAppearanceSystem::ClaimCityCelebrationGift(player, claimReq, kNowSec, claimResp), kSuccess);
	EXPECT_EQ(claimResp.granted_bind_gold(), 100u);
	EXPECT_EQ(claimResp.weekly_claimed_total(), 3000u);

	claimReq.set_client_request_id(13033);
	claimReq.set_celebration_id(firstCelebId + 2);
	EXPECT_EQ(PlayerAppearanceSystem::ClaimCityCelebrationGift(player, claimReq, kNowSec, claimResp),
			  static_cast<uint32_t>(kCityCelebrationWeeklyCapReached));
}

TEST_F(PlayerAppearanceSystemTest, FriendCosmeticGiftRealDiamondOnlyInboxCreditRaceRefundAndChargebackRevokeM4)
{
	// 给发送方 500 绑定灵石 + 0 真实灵石：验证绝不混扣绑定灵石
	ASSERT_EQ(CurrencySystem::AddCurrency(player, kCurrencyBindDiamond, 500, TX_GM_GRANT, 1), kSuccess);

	std::vector<player_inbox::CosmeticGiftCreditPayload> dispatchedCredits;
	std::vector<player_inbox::CosmeticGiftRefundPayload> dispatchedRefunds;
	PlayerAppearanceSystem::GiftSocialVerdict social{true, false, true, false};

	PlayerAppearanceSystem::TestHooks hooks;
	hooks.save = [this](entt::entity, const char *r) { saveReasons.emplace_back(r); };
	hooks.notify = [this](entt::entity, const AppearanceStateNotify &n) { stateNotifies.push_back(n); };
	hooks.giftSocialCheck = [&](Guid, Guid, uint32_t, uint32_t, std::string_view) { return social; };
	hooks.giftCreditDispatch = [&](Guid, const player_inbox::CosmeticGiftCreditPayload &p)
	{
		dispatchedCredits.push_back(p);
		return true;
	};
	hooks.giftRefundDispatch = [&](Guid, const player_inbox::CosmeticGiftRefundPayload &p)
	{
		dispatchedRefunds.push_back(p);
		return true;
	};
	PlayerAppearanceSystem::SetTestHooks(hooks);

	SendCosmeticGiftRequest req;
	req.set_client_request_id(14001);
	req.set_target_player_id(960300099);
	req.set_target_kind(COSMETIC_GIFT_TARGET_SHOWCASE);
	req.set_target_id_u32(1);
	req.set_expected_price_diamond(80);
	SendCosmeticGiftResponse resp;

	// 仅持绑定灵石 -> 严拒
	EXPECT_EQ(PlayerAppearanceSystem::SendCosmeticGift(player, req, kNowSec, resp),
			  static_cast<uint32_t>(kCosmeticGiftCurrencyNotEnough));
	EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyBindDiamond), 500u);

	// 补真实灵石，但非 7 天好友 -> 严拒
	ASSERT_EQ(CurrencySystem::AddCurrency(player, kCurrencyDiamond, 500, TX_GM_GRANT, 2), kSuccess);
	social.friendsFor7Days = false;
	EXPECT_EQ(PlayerAppearanceSystem::SendCosmeticGift(player, req, kNowSec, resp),
			  static_cast<uint32_t>(kCosmeticGiftNotFriend7Days));

	// 专属不可买断项 #13 -> 严拒
	social.friendsFor7Days = true;
	SendCosmeticGiftRequest exclReq = req;
	exclReq.set_target_id_u32(13);
	EXPECT_EQ(PlayerAppearanceSystem::SendCosmeticGift(player, exclReq, kNowSec, resp),
			  static_cast<uint32_t>(kCosmeticGiftInvalidTarget));

	// 正常赠送 #1 排面阵纹 -> 扣 80 真实灵石、发收件箱载荷、同键重放幂等
	ASSERT_EQ(PlayerAppearanceSystem::SendCosmeticGift(player, req, kNowSec, resp), kSuccess);
	EXPECT_FALSE(resp.replayed());
	EXPECT_EQ(resp.spent_diamond(), 80u);
	EXPECT_EQ(resp.sent_this_week(), 1u);
	EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyDiamond), 420u);
	ASSERT_EQ(dispatchedCredits.size(), 1u);

	SendCosmeticGiftResponse replayResp;
	ASSERT_EQ(PlayerAppearanceSystem::SendCosmeticGift(player, req, kNowSec, replayResp), kSuccess);
	EXPECT_TRUE(replayResp.replayed());
	EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyDiamond), 420u);

	// 受赠方（复用本测试实体）通过 PlayerInbox 到账授予 #1，再重复到账模拟竞态已拥有 -> 触发原路退款载荷
	std::string creditBytes;
	ASSERT_TRUE(dispatchedCredits[0].SerializeToString(&creditBytes));
	auto r1 = PlayerAppearanceSystem::ApplyCosmeticGiftCredit(player, creditBytes);
	EXPECT_EQ(r1.kind, PlayerInboxExecKind::kApplied);
	EXPECT_TRUE(player_appearance::IsShowcaseCosmeticOwned(
		tlsEcs.actorRegistry.get<PlayerProfileComp>(player).showcase_cosmetic_owned_bits(), 1));
	EXPECT_TRUE(dispatchedRefunds.empty());

	auto r2 = PlayerAppearanceSystem::ApplyCosmeticGiftCredit(player, creditBytes);
	EXPECT_EQ(r2.kind, PlayerInboxExecKind::kApplied);
	ASSERT_EQ(dispatchedRefunds.size(), 1u);
	EXPECT_EQ(dispatchedRefunds[0].refund_diamond(), 80u);

	// 发送方收到退款载荷 -> 原路退回 80 真实灵石（420 -> 500）
	std::string refundBytes;
	ASSERT_TRUE(dispatchedRefunds[0].SerializeToString(&refundBytes));
	auto r3 = PlayerAppearanceSystem::ApplyCosmeticGiftRefund(player, refundBytes);
	EXPECT_EQ(r3.kind, PlayerInboxExecKind::kApplied);
	EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyDiamond), 500u);

	// M-4 黑卡撤单精准回收：穿戴 #1 后执行 REVOKE -> 位图清零且自动卸下
	(*tlsEcs.actorRegistry.get<PlayerProfileComp>(player).mutable_equipped_showcase_cosmetics())[1] = 1u;
	player_inbox::CosmeticGiftRevokePayload revoke;
	revoke.set_gift_id(dispatchedCredits[0].gift_id());
	revoke.set_sender_player_id(kPlayerGuid);
	revoke.set_target_kind(COSMETIC_GIFT_TARGET_SHOWCASE);
	revoke.set_target_id_u32(1);
	revoke.set_reason("chargeback");
	std::string revokeBytes;
	ASSERT_TRUE(revoke.SerializeToString(&revokeBytes));
	auto r4 = PlayerAppearanceSystem::ApplyCosmeticGiftRevoke(player, revokeBytes);
	EXPECT_EQ(r4.kind, PlayerInboxExecKind::kApplied);
	EXPECT_FALSE(player_appearance::IsShowcaseCosmeticOwned(
		tlsEcs.actorRegistry.get<PlayerProfileComp>(player).showcase_cosmetic_owned_bits(), 1));
	EXPECT_TRUE(tlsEcs.actorRegistry.get<PlayerProfileComp>(player).equipped_showcase_cosmetics().empty());

	// 手记典藏赠礼 + 撤单回退验证
	player_inbox::CosmeticGiftCreditPayload passGift;
	passGift.set_gift_id(99002);
	passGift.set_sender_player_id(kPlayerGuid);
	passGift.set_target_kind(COSMETIC_GIFT_TARGET_TRAVEL_PASS);
	passGift.set_target_id_u32(2);
	passGift.set_paid_diamond(180);
	std::string passBytes;
	ASSERT_TRUE(passGift.SerializeToString(&passBytes));
	EXPECT_EQ(PlayerAppearanceSystem::ApplyCosmeticGiftCredit(player, passBytes).kind, PlayerInboxExecKind::kApplied);
	EXPECT_TRUE(PlayerTravelPassSystem::HasPremiumUnlocked(player, 2));

	player_inbox::CosmeticGiftRevokePayload passRevoke;
	passRevoke.set_gift_id(99002);
	passRevoke.set_sender_player_id(kPlayerGuid);
	passRevoke.set_target_kind(COSMETIC_GIFT_TARGET_TRAVEL_PASS);
	passRevoke.set_target_id_u32(2);
	std::string passRevokeBytes;
	ASSERT_TRUE(passRevoke.SerializeToString(&passRevokeBytes));
	EXPECT_EQ(PlayerAppearanceSystem::ApplyCosmeticGiftRevoke(player, passRevokeBytes).kind, PlayerInboxExecKind::kApplied);
	EXPECT_FALSE(PlayerTravelPassSystem::HasPremiumUnlocked(player, 2));
}
} // namespace
