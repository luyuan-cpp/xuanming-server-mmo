// 角色形象切换（商城 10 元角色形象卡）与 1 小时免费试用（bit 位图）单测。
// 覆盖纯规则、bit 位图未知高位保留、幂等重放/错参、形象卡扣除与预留保护、1 小时试用到期自动还原与目录同步退避。

#include <gtest/gtest.h>

#include <cstdint>
#include <string>
#include <vector>

#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/type_define/type_define.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_item_hold_comp.pb.h"
#include "proto/common/database/player_cache.pb.h"
#include "proto/data_service/data_service.pb.h"
#include "proto/scene/player_appearance.pb.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/appearance_rules.h"
#include "services/scene/player/system/player_appearance.h"
#include "table/code/appearancerule_table.h"
#include "table/code/characterappearance_table.h"
#include "table/code/item_table.h"
#include "table/proto/tip/appearance_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
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
}

class PlayerAppearanceSystemTest : public testing::Test
{
protected:
	void SetUp() override
	{
		ItemTableManager::Instance().Load();
		CharacterAppearanceTableManager::Instance().Load();
		AppearanceRuleTableManager::Instance().Load();
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
	SwitchAppearanceRequest invalidReq;
	invalidReq.set_client_request_id(1003);
	invalidReq.set_appearance_id("30_han_xiangzi");
	EXPECT_EQ(PlayerAppearanceSystem::SwitchAppearance(player, invalidReq, kNowSec + 3, resp),
			  static_cast<uint32_t>(kAppearanceInvalid));

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
	req.set_appearance_id("08_alchemy_prodigy_boy");
	StartAppearanceTrialResponse resp;
	ASSERT_EQ(PlayerAppearanceSystem::StartTrial(player, req, kNowSec, resp), kSuccess);
	EXPECT_FALSE(resp.replayed());
	EXPECT_EQ(resp.info().permanent_appearance_id(), "00_reference_topright_boy");
	EXPECT_EQ(resp.info().active_appearance_id(), "08_alchemy_prodigy_boy");
	EXPECT_EQ(resp.info().trial_appearance_id(), "08_alchemy_prodigy_boy");
	EXPECT_EQ(resp.info().trial_expires_at(), kNowSec + trialDurationSec);
	EXPECT_EQ(PlayerAppearanceSystem::EffectiveAppearanceId(player, kNowSec + 10), "08_alchemy_prodigy_boy");
	EXPECT_EQ(CardCountInBag(), 2u); // 免费试用不扣卡

	// 同一形象不可二次试用(新请求键)。
	StartAppearanceTrialRequest secondReq;
	secondReq.set_client_request_id(2002);
	secondReq.set_appearance_id("08_alchemy_prodigy_boy");
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
	req.set_appearance_id("04_mountain_guardian_boy");
	SwitchAppearanceResponse resp;
	EXPECT_EQ(PlayerAppearanceSystem::SwitchAppearance(player, req, kNowSec, resp),
			  static_cast<uint32_t>(kAppearanceNoCard));

	// 冻结时试用也拒绝。
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	StartAppearanceTrialRequest trialReq;
	trialReq.set_client_request_id(3002);
	trialReq.set_appearance_id("04_mountain_guardian_boy");
	StartAppearanceTrialResponse trialResp;
	EXPECT_EQ(PlayerAppearanceSystem::StartTrial(player, trialReq, kNowSec, trialResp),
			  static_cast<uint32_t>(kAppearanceFrozen));
}
} // namespace
