// 角色形象切换（商城 10 元角色形象卡）与 1 小时免费试用（bit 位图）单测。
// 覆盖纯规则、bit 位图未知高位保留、幂等重放/错参、形象卡扣除与预留保护、1 小时试用到期自动还原与目录同步退避。

#include <gtest/gtest.h>

#include <cstdint>
#include <string>
#include <vector>

#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/type_define/type_define.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_item_hold_comp.pb.h"
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/common/database/player_cache.pb.h"
#include "proto/data_service/data_service.pb.h"
#include "proto/scene/player_appearance.pb.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/appearance_rules.h"
#include "services/scene/player/system/player_appearance.h"
#include "table/code/appearancerule_table.h"
#include "table/code/appearanceseries_table.h"
#include "table/code/characterappearance_table.h"
#include "table/code/item_table.h"
#include "table/code/pet_table.h"
#include "table/code/petcosmetic_table.h"
#include "table/code/skill_table.h"
#include "table/code/skillvfxcosmetic_table.h"
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
		tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(entity);
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

	// 未拥有时拒绝穿戴，不改灵宠实例。
	EquipPetCosmeticRequest req;
	req.set_client_request_id(8001);
	req.set_pet_id(70001);
	req.set_cosmetic_id(1);
	EquipPetCosmeticResponse resp;
	EXPECT_EQ(PlayerAppearanceSystem::EquipPetCosmetic(player, req, kNowSec + 1, resp),
			  static_cast<uint32_t>(kAppearanceNotOwned));

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

	// 未拥有时拒绝启用。
	EquipSkillVfxRequest req;
	req.set_client_request_id(9001);
	req.set_skill_table_id(1201);
	req.set_cosmetic_id(1);
	EquipSkillVfxResponse resp;
	EXPECT_EQ(PlayerAppearanceSystem::EquipSkillVfx(player, req, kNowSec + 1, resp),
			  static_cast<uint32_t>(kAppearanceNotOwned));

	// 授予技能特效 id=1(适用于 1201 玄符落曜)，尚未习得技能前仍拒绝启用。
	bool alreadyOwned = true;
	ASSERT_EQ(PlayerAppearanceSystem::GrantSkillVfx(player, 1, kNowSec + 2, &alreadyOwned), kSuccess);
	EXPECT_FALSE(alreadyOwned);
	EXPECT_EQ(PlayerAppearanceSystem::EquipSkillVfx(player, req, kNowSec + 3, resp),
			  static_cast<uint32_t>(kAppearanceInvalid));

	// 习得 1201 一重后，错配技能号 1202 仍拒绝；正确技能号 1201 成功启用并透传战斗快照。
	auto *learned = skills.add_skill_list();
	learned->set_skill_table_id(1201);
	learned->set_rank(1);

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
} // namespace
