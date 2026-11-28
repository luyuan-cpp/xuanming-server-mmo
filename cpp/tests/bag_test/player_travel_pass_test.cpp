// 游历手记（不肝不逼氪双轨活动，docs/design/activity-travel-pass-and-showcase-v1.md）单测：
//   * TravelPassRulesTest —— 纯规则（位图、时间窗、外观键前缀、组件合法性校验）。
//   * PlayerTravelPassTest —— 真实配表校验、正常/困难/PVP/防挂机积点、先玩后买、禁止绑定灵石混扣、
//     报价/修订防呆、单档与一键全领原子强制绑定入包、满包零副作用回滚、幂等重放、纪念头像框/称谓穿戴与活动目录种草卡。

#include <gtest/gtest.h>

#include <cstdint>
#include <functional>
#include <string>

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
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_spirit_shop_comp.pb.h"
#include "proto/common/component/player_travel_pass_comp.pb.h"
#include "proto/match/match_service.pb.h"
#include "proto/scene/player_activity.pb.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/player_travel_pass.h"
#include "services/scene/player/system/travel_pass_rules.h"
#include "table/code/characterappearance_table.h"
#include "table/code/item_table.h"
#include "table/code/spiritshopgoods_table.h"
#include "table/code/spiritshoplimitgroup_table.h"
#include "table/code/travelpassseason_table.h"
#include "table/code/travelpasstier_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/travel_pass_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{

constexpr uint64_t kPlayerGuid = 838381;
constexpr uint64_t kSegmentLo = 2600000;
constexpr uint64_t kSegmentHi = 2700000;
constexpr uint64_t kNowMs = 1795521600000ULL;

bool ArmItemSegment(uint64_t lo, uint64_t hi)
{
	auto &client = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
	client.Reset();
	GuidSegmentClient::Options options;
	options.mixIds = false;
	options.kindName = "item";
	options.initialStep = 100000;
	if (!client.Enable(options, [](const std::string &, uint32_t) { return true; },
					   [](GuidSegmentClient::TimerKind, double, std::function<void()>) {},
					   [] { return 0.0; }))
	{
		return false;
	}
	client.Warm();
	client.OnResponse(0, lo, hi);
	return client.IsReady();
}

BattleSettlementData MakeBattleSettlement(uint32_t matchMode, eBattleOutcome outcome, uint32_t difficulty,
										  uint32_t totalRounds, uint32_t manualRounds, bool contribution,
										  bool fled = false)
{
	BattleSettlementData s;
	s.set_battle_id(90001);
	s.set_player_id(kPlayerGuid);
	s.set_match_mode(matchMode);
	s.set_player_team_index(0);
	s.set_outcome(outcome);
	s.set_battle_difficulty(difficulty);
	s.set_total_rounds(totalRounds);
	s.set_manual_action_rounds(manualRounds);
	s.set_effective_combat_contribution(contribution);
	s.set_participated(true);
	s.set_fled(fled);
	return s;
}

TEST(TravelPassRulesTest, ValidationAndBitmasks)
{
	EXPECT_EQ(1ULL, travel_pass_rules::TierBit(1));
	EXPECT_EQ(uint64_t{1} << 9, travel_pass_rules::TierBit(10));
	EXPECT_EQ(0ULL, travel_pass_rules::TierBit(0));
	EXPECT_EQ(0ULL, travel_pass_rules::TierBit(65));

	EXPECT_TRUE(travel_pass_rules::IsFrameKey("frame_travel_qingyun_s1"));
	EXPECT_FALSE(travel_pass_rules::IsFrameKey("title_travel_qingyun_s1"));
	EXPECT_TRUE(travel_pass_rules::IsTitleKey("title_travel_qingyun_s1"));
	EXPECT_FALSE(travel_pass_rules::IsTitleKey("frame_travel_qingyun_s1"));

	EXPECT_TRUE(travel_pass_rules::IsWindowOpen(0, 0, kNowMs));
	EXPECT_FALSE(travel_pass_rules::IsWindowOpen(kNowMs + 1, 0, kNowMs));
	EXPECT_FALSE(travel_pass_rules::IsWindowOpen(0, kNowMs, kNowMs));
	EXPECT_TRUE(travel_pass_rules::IsWindowOpen(kNowMs - 10, kNowMs + 10, kNowMs));

	PlayerTravelPassComp badDup;
	badDup.add_seasons()->set_pass_id(1);
	badDup.add_seasons()->set_pass_id(1);
	EXPECT_FALSE(travel_pass_rules::ValidateComp(badDup).empty());

	PlayerTravelPassComp badEquip;
	badEquip.set_equipped_frame_key("frame_travel_qingyun_s1");
	EXPECT_FALSE(travel_pass_rules::ValidateComp(badEquip).empty());
}

class PlayerTravelPassTest : public ::testing::Test
{
protected:
	void SetUp() override
	{
		tlsEcs.actorRegistry.clear();
		GainBlockService::ClearAllGlobalBlocks();
		ASSERT_TRUE(ArmItemSegment(kSegmentLo, kSegmentHi));

		ItemTableManager::Instance().Load();
		TravelPassSeasonTableManager::Instance().Load();
		TravelPassTierTableManager::Instance().Load();
		CharacterAppearanceTableManager::Instance().Load();
		SpiritShopLimitGroupTableManager::Instance().Load();
		SpiritShopGoodsTableManager::Instance().Load();

		ASSERT_TRUE(PlayerTravelPassSystem::ValidateTables());

		player_ = tlsEcs.actorRegistry.create();
		tlsEcs.playerList[Guid{kPlayerGuid}] = player_;
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player_, Guid{kPlayerGuid});
		tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player_).epoch = 1;
		auto &currency = tlsEcs.actorRegistry.emplace<CurrencyComp>(player_);
		while (currency.values_size() < static_cast<int>(kCurrencyMax))
		{
			currency.add_values(0);
		}
		tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player_);
		tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(player_);
		tlsEcs.actorRegistry.emplace<PlayerTravelPassComp>(player_);
		tlsEcs.actorRegistry.emplace<PlayerSpiritShopComp>(player_);
		auto &profile = tlsEcs.actorRegistry.emplace<PlayerProfileComp>(player_);
		profile.set_appearance_id("00_reference_topright_boy");

		auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player_);
		for (auto &bag : bags.bags)
		{
			bag.SetPlayerGuid(Guid{kPlayerGuid});
		}
		bags.bags[kInventory].SetCapacityForRestore(30);
	}

	void TearDown() override
	{
		GainBlockService::ClearAllGlobalBlocks();
		tlsEcs.playerList.erase(Guid{kPlayerGuid});
		if (player_ != entt::null && tlsEcs.actorRegistry.valid(player_))
		{
			tlsEcs.actorRegistry.destroy(player_);
		}
		player_ = entt::null;
		EXPECT_TRUE(ArmItemSegment(1, uint64_t{1} << 54));
	}

	void GrantCurrency(CurrencyType type, int64_t amount)
	{
		ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, type, amount, TX_GM_GRANT, 1));
	}

	entt::entity player_{entt::null};
};

TEST_F(PlayerTravelPassTest, BattleCreditNormalHardPvpAndAntiAfk)
{
	auto &comp = tlsEcs.actorRegistry.get<PlayerTravelPassComp>(player_);
	PlayerTravelPassComp candidate;
	bool increased = false;

	// 普通 PVE 胜局 +20
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::PrepareBattleCredit(
							comp, MakeBattleSettlement(match::MATCH_MODE_PVE_SOLO, BATTLE_OUTCOME_SIDE_A_WIN, 1, 5, 5, true),
							kNowMs, candidate, increased));
	ASSERT_TRUE(increased);
	comp.Swap(&candidate);
	EXPECT_EQ(20u, travel_pass_rules::FindSeason(comp, 1)->progress_points());

	// 困难首领胜局 +30 -> 50
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::PrepareBattleCredit(
							comp, MakeBattleSettlement(match::MATCH_MODE_PVE_SOLO, BATTLE_OUTCOME_SIDE_A_WIN, 2, 8, 8, true),
							kNowMs, candidate, increased));
	ASSERT_TRUE(increased);
	comp.Swap(&candidate);
	EXPECT_EQ(50u, travel_pass_rules::FindSeason(comp, 1)->progress_points());

	// PVP 胜局 +30 -> 80
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::PrepareBattleCredit(
							comp, MakeBattleSettlement(match::MATCH_MODE_1V1, BATTLE_OUTCOME_SIDE_A_WIN, 0, 4, 4, true),
							kNowMs, candidate, increased));
	ASSERT_TRUE(increased);
	comp.Swap(&candidate);
	EXPECT_EQ(80u, travel_pass_rules::FindSeason(comp, 1)->progress_points());

	// PVP 合格负局（>=3 回合、>=2 手操、有效贡献）+15 -> 95
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::PrepareBattleCredit(
							comp, MakeBattleSettlement(match::MATCH_MODE_1V1, BATTLE_OUTCOME_SIDE_B_WIN, 0, 3, 2, true),
							kNowMs, candidate, increased));
	ASSERT_TRUE(increased);
	comp.Swap(&candidate);
	EXPECT_EQ(95u, travel_pass_rules::FindSeason(comp, 1)->progress_points());

	// PVP 挂机负局（0 手操）-> +0
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::PrepareBattleCredit(
							comp, MakeBattleSettlement(match::MATCH_MODE_1V1, BATTLE_OUTCOME_SIDE_B_WIN, 0, 5, 0, true),
							kNowMs, candidate, increased));
	EXPECT_FALSE(increased);

	// 逃跑局 -> +0
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::PrepareBattleCredit(
							comp, MakeBattleSettlement(match::MATCH_MODE_PVE_SOLO, BATTLE_OUTCOME_SIDE_A_WIN, 1, 5, 5, true, true),
							kNowMs, candidate, increased));
	EXPECT_FALSE(increased);
}

TEST_F(PlayerTravelPassTest, PlayFirstBuyLaterAtomicBoundClaimsAndCosmetics)
{
	// 1. 先玩后买：正常战斗累积到 180 点（达成第 1..6 档），不花 1 分钱。
	auto &comp = tlsEcs.actorRegistry.get<PlayerTravelPassComp>(player_);
	auto *prog = travel_pass_rules::FindOrAddSeason(comp, 1);
	ASSERT_NE(nullptr, prog);
	prog->set_progress_points(180);

	TravelPassSnapshot snap;
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::GetState(player_, kNowMs, 0, snap));
	EXPECT_TRUE(snap.available());
	EXPECT_EQ(1u, snap.pass_id());
	EXPECT_EQ(180u, snap.progress_points());
	EXPECT_EQ(300u, snap.max_points());
	EXPECT_FALSE(snap.premium_unlocked());
	EXPECT_EQ(6u, snap.claimable_free_count());
	EXPECT_EQ(0u, snap.claimable_premium_count());
	EXPECT_EQ(6u, snap.retroactive_premium_count());
	EXPECT_EQ(10, snap.tiers_size());

	// 活动中心卡片：游历手记有免费可领故亮红点；衣柜与百宝阁种草卡绝不亮红点。
	GetActivityListResponse listResp;
	PlayerTravelPassSystem::AppendActivityCatalogEntries(player_, kNowMs, listResp);
	ASSERT_EQ(3, listResp.activities_size());
	EXPECT_EQ(PLAYER_ACTIVITY_ENTRY_TRAVEL_PASS, listResp.activities(0).entry_kind());
	EXPECT_TRUE(listResp.activities(0).has_claimable_reward());
	EXPECT_EQ(PLAYER_ACTIVITY_ENTRY_APPEARANCE_SHOWCASE, listResp.activities(1).entry_kind());
	EXPECT_FALSE(listResp.activities(1).has_claimable_reward());
	EXPECT_EQ(PLAYER_ACTIVITY_ENTRY_SPIRIT_SHOP_SHOWCASE, listResp.activities(2).entry_kind());
	EXPECT_FALSE(listResp.activities(2).has_claimable_reward());

	// 2. 免费单领第 5 档（绑定定星保底符 3405 x1）并验证严格绑定 + 幂等重放。
	ClaimTravelPassRewardRequest freeReq;
	freeReq.set_client_request_id(5001);
	freeReq.set_pass_id(1);
	freeReq.set_tier_index(5);
	freeReq.set_premium_track(false);
	ClaimTravelPassRewardResponse freeResp;
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::ClaimReward(player_, freeReq, kNowMs, freeResp));
	EXPECT_FALSE(freeResp.replayed());
	EXPECT_EQ(1u, freeResp.claimed_count());
	const auto &inv = tlsEcs.actorRegistry.get<PlayerBagsComp>(player_).bags[kInventory];
	EXPECT_EQ(1u, inv.GetTotalItemCount(3405));
	EXPECT_EQ(0u, BagService::RemovableTradableCount(player_, inv, 3405)); // 强制 bound=true

	ClaimTravelPassRewardResponse freeReplay;
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::ClaimReward(player_, freeReq, kNowMs, freeReplay));
	EXPECT_TRUE(freeReplay.replayed());
	EXPECT_EQ(1u, freeReplay.claimed_count());
	EXPECT_EQ(1u, inv.GetTotalItemCount(3405));

	// 3. 未解锁典藏领典藏档被拒。
	ClaimTravelPassRewardRequest lockedPremReq;
	lockedPremReq.set_client_request_id(5002);
	lockedPremReq.set_pass_id(1);
	lockedPremReq.set_tier_index(1);
	lockedPremReq.set_premium_track(true);
	ClaimTravelPassRewardResponse lockedPremResp;
	EXPECT_EQ(kTravelPassPremiumRequired,
			  PlayerTravelPassSystem::ClaimReward(player_, lockedPremReq, kNowMs, lockedPremResp));

	// 4. 禁止绑定灵石抵扣 ¥18 典藏解锁。
	GrantCurrency(kCurrencyBindDiamond, 500);
	UnlockTravelPassPremiumRequest unlockReq;
	unlockReq.set_client_request_id(5003);
	unlockReq.set_pass_id(1);
	unlockReq.set_expected_currency_type(1);
	unlockReq.set_expected_price(180);
	unlockReq.set_expected_config_revision(1);
	UnlockTravelPassPremiumResponse unlockResp;
	EXPECT_EQ(kTravelPassCurrencyNotEnough,
			  PlayerTravelPassSystem::UnlockPremium(player_, unlockReq, kNowMs, unlockResp));

	// 5. 报价防呆拦截。
	GrantCurrency(kCurrencyDiamond, 200);
	UnlockTravelPassPremiumRequest stalePriceReq = unlockReq;
	stalePriceReq.set_client_request_id(5004);
	stalePriceReq.set_expected_price(120);
	EXPECT_EQ(kTravelPassPriceChanged,
			  PlayerTravelPassSystem::UnlockPremium(player_, stalePriceReq, kNowMs, unlockResp));

	// 6. 正式 180 真实灵石解锁 + 幂等重放不双扣。
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::UnlockPremium(player_, unlockReq, kNowMs, unlockResp));
	EXPECT_FALSE(unlockResp.replayed());
	EXPECT_TRUE(unlockResp.snapshot().premium_unlocked());
	EXPECT_EQ(20u, CurrencySystem::GetBalance(player_, kCurrencyDiamond));
	EXPECT_EQ(6u, unlockResp.snapshot().claimable_premium_count());

	UnlockTravelPassPremiumResponse unlockReplay;
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::UnlockPremium(player_, unlockReq, kNowMs, unlockReplay));
	EXPECT_TRUE(unlockReplay.replayed());
	EXPECT_EQ(20u, CurrencySystem::GetBalance(player_, kCurrencyDiamond));

	// 7. 一键领取剩余全部可领档位（5 档免费 + 6 档典藏 = 11 轨），并检查头像框解锁与穿戴。
	ClaimTravelPassRewardRequest claimAllReq;
	claimAllReq.set_client_request_id(5005);
	claimAllReq.set_pass_id(1);
	claimAllReq.set_tier_index(0);
	ClaimTravelPassRewardResponse claimAllResp;
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::ClaimReward(player_, claimAllReq, kNowMs, claimAllResp));
	EXPECT_EQ(11u, claimAllResp.claimed_count());
	EXPECT_EQ(0u, claimAllResp.snapshot().claimable_free_count());
	EXPECT_EQ(0u, claimAllResp.snapshot().claimable_premium_count());
	// 银两：free(5000+8000) + prem(10000) = 23000
	EXPECT_EQ(23000u, CurrencySystem::GetBalance(player_, kCurrencyGold));
	// 绑定物品：3405 (已有1 + prem tier1 1 + prem tier6 1 = 3), 3402 (prem tier5 = 1), 3401 (prem tier3 = 1)
	EXPECT_EQ(3u, inv.GetTotalItemCount(3405));
	EXPECT_EQ(1u, inv.GetTotalItemCount(3402));
	EXPECT_EQ(1u, inv.GetTotalItemCount(3401));
	EXPECT_EQ(0u, BagService::RemovableTradableCount(player_, inv, 3402));

	// 穿戴已解锁的青云游历头像框成功，未解锁称谓被拒。
	EquipTravelPassCosmeticRequest equipFrame;
	equipFrame.set_cosmetic_slot(travel_pass_rules::kCosmeticSlotFrame);
	equipFrame.set_cosmetic_key("frame_travel_qingyun_s1");
	EquipTravelPassCosmeticResponse equipResp;
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::EquipCosmetic(player_, kNowMs, equipFrame, equipResp));
	EXPECT_EQ("frame_travel_qingyun_s1", equipResp.snapshot().equipped_frame_key());

	EquipTravelPassCosmeticRequest equipUnownedTitle;
	equipUnownedTitle.set_cosmetic_slot(travel_pass_rules::kCosmeticSlotTitle);
	equipUnownedTitle.set_cosmetic_key("title_travel_qingyun_s1");
	EXPECT_EQ(kTravelPassCosmeticNotOwned,
			  PlayerTravelPassSystem::EquipCosmetic(player_, kNowMs, equipUnownedTitle, equipResp));
}

TEST_F(PlayerTravelPassTest, FullBagRejectsClaimAtomicallyWithZeroMutation)
{
	auto &comp = tlsEcs.actorRegistry.get<PlayerTravelPassComp>(player_);
	auto *prog = travel_pass_rules::FindOrAddSeason(comp, 1);
	ASSERT_NE(nullptr, prog);
	prog->set_progress_points(60); // 档 1(5000 银两) + 档 2(重铸砂 3403 x2)

	// 将背包容量收成 0，确保发物品失败。
	auto &inv = tlsEcs.actorRegistry.get<PlayerBagsComp>(player_).bags[kInventory];
	inv.SetCapacityForRestore(0);

	ClaimTravelPassRewardRequest claimAllReq;
	claimAllReq.set_client_request_id(5010);
	claimAllReq.set_pass_id(1);
	claimAllReq.set_tier_index(0);
	ClaimTravelPassRewardResponse resp;
	EXPECT_EQ(kTravelPassBagFull, PlayerTravelPassSystem::ClaimReward(player_, claimAllReq, kNowMs, resp));
	// 零副作用：银两未增，掩码未置位！
	EXPECT_EQ(0u, CurrencySystem::GetBalance(player_, kCurrencyGold));
	EXPECT_EQ(0ULL, prog->free_claimed_mask());
}

TEST_F(PlayerTravelPassTest, BookshelfSelectsSingleActivePassAutoAdvancesAndAcceptsBonusCosmetic)
{
	auto &comp = tlsEcs.actorRegistry.get<PlayerTravelPassComp>(player_);

	// 1. 书架预置 6 期全部可见，默认在研为第 1 期。
	TravelPassSnapshot initialSnap;
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::GetState(player_, kNowMs, 0, initialSnap));
	EXPECT_EQ(6, initialSnap.bookshelf_size());
	EXPECT_EQ(1u, initialSnap.active_pass_id());
	EXPECT_EQ(1u, initialSnap.pass_id());

	// 2. 零消耗切换至第 3 期，单场战斗只注入第 3 期（防 M-1 多期并发加点）。
	SelectActiveTravelPassRequest selectReq;
	selectReq.set_pass_id(3);
	SelectActiveTravelPassResponse selectResp;
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::SelectActivePass(player_, kNowMs, selectReq, selectResp));
	EXPECT_EQ(3u, selectResp.snapshot().active_pass_id());
	EXPECT_EQ(3u, selectResp.snapshot().pass_id());

	PlayerTravelPassComp candidate;
	bool increased = false;
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::PrepareBattleCredit(
							comp, MakeBattleSettlement(match::MATCH_MODE_PVE_SOLO, BATTLE_OUTCOME_SIDE_A_WIN, 1, 5, 5, true),
							kNowMs, candidate, increased));
	ASSERT_TRUE(increased);
	comp.Swap(&candidate);
	ASSERT_NE(nullptr, travel_pass_rules::FindSeason(comp, 3));
	EXPECT_EQ(20u, travel_pass_rules::FindSeason(comp, 3)->progress_points());
	EXPECT_EQ(nullptr, travel_pass_rules::FindSeason(comp, 1));
	EXPECT_EQ(nullptr, travel_pass_rules::FindSeason(comp, 2));

	// 3. 第 3 期满点后，下一场战斗自动顺延至第一本未满期次（第 1 期）。
	travel_pass_rules::FindMutableSeason(comp, 3)->set_progress_points(300);
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::PrepareBattleCredit(
							comp, MakeBattleSettlement(match::MATCH_MODE_PVE_SOLO, BATTLE_OUTCOME_SIDE_A_WIN, 1, 5, 5, true),
							kNowMs, candidate, increased));
	ASSERT_TRUE(increased);
	comp.Swap(&candidate);
	EXPECT_EQ(1u, comp.active_pass_id());
	ASSERT_NE(nullptr, travel_pass_rules::FindSeason(comp, 1));
	EXPECT_EQ(20u, travel_pass_rules::FindSeason(comp, 1)->progress_points());

	// 4. 轻享卡一次性附赠的纪念头像框进入 bonus_cosmetic_keys，幂等去重且可直接佩戴。
	ASSERT_TRUE(PlayerTravelPassSystem::GrantBonusCosmetic(player_, "frame_lightcard_season_s1"));
	ASSERT_TRUE(PlayerTravelPassSystem::GrantBonusCosmetic(player_, "frame_lightcard_season_s1"));
	EXPECT_EQ(1, comp.bonus_cosmetic_keys_size());

	EquipTravelPassCosmeticRequest equipReq;
	equipReq.set_cosmetic_slot(travel_pass_rules::kCosmeticSlotFrame);
	equipReq.set_cosmetic_key("frame_lightcard_season_s1");
	EquipTravelPassCosmeticResponse equipResp;
	ASSERT_EQ(kSuccess, PlayerTravelPassSystem::EquipCosmetic(player_, kNowMs, equipReq, equipResp));
	EXPECT_EQ("frame_lightcard_season_s1", equipResp.snapshot().equipped_frame_key());
}

} // namespace
