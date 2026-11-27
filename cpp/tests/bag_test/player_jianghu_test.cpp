// 江湖恩怨与天墉生死状一期(docs/planning/battle-jianghu-grudge-duel-bounty-v1.md)单测:
//   * 零核心材料产出红线(MATCH_MODE_JIANGHU_DUEL 恒不发放装备材料额度)
//   * 百宝阁 305/306 灵石直购绑定 Item 3415「金帖生死状」与 Item 3416「清风洗尘符」
//   * 闭关免战切换与 match -> scene 资格/资费预检(CheckDuelEligibility)
//   * 金帖预留(PlaceHold)→ 备战取消/作废释放 vs 正常结算从预留精准扣除
//   * 普通 500 银两战书预扣、取消 / 作废退还只退一次、余额不足拒绝;别的局未结清时拒绝新备战、入账失败保留标记、
//     获取封禁不挡退费(2026-11-26,数据覆写审计 r2 第 4 轮;终局复核的用例在 battle_freeze_hold_test.cpp)
//   * 30 分钟战败社交印记挂起/覆盖/复仇清除/过户 owner_revision 自动清零
//   * 1000 银两普通清除与清风洗尘符(Item 3416)+洒脱徽记幂等清除(重放防双扣、参数错位拒绝)

#include <gtest/gtest.h>

#include <cstdint>
#include <functional>
#include <limits>
#include <string>
#include <vector>

#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/type_define/type_define.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h" // 退费不算新增获得(CountsAsGain)
#include "modules/id_segment/guid_segment_registry.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_item_hold_comp.pb.h"
#include "proto/common/component/player_jianghu_comp.pb.h"
#include "proto/common/component/player_spirit_shop_comp.pb.h"
#include "proto/match/match_service.pb.h"
#include "proto/scene/player_jianghu.pb.h"
#include "proto/scene/player_spirit_shop.pb.h"
#include "proto/scene/scene.pb.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/player_equipment_material_reward.h"
#include "services/scene/player/system/player_jianghu.h"
#include "services/scene/player/system/player_spirit_shop.h"
#include "services/scene/player/system/role_admission_system.h"
#include "table/code/item_table.h"
#include "table/code/jianghurule_table.h"
#include "table/code/spiritshopgoods_table.h"
#include "table/code/spiritshoplimitgroup_table.h"
#include "table/proto/tip/asset_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/jianghu_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
constexpr uint64_t kPlayerId = 910001;
constexpr uint64_t kOpponentCaptainId = 910002;
constexpr uint64_t kOtherCaptainId = 910003;
constexpr uint64_t kNowMs = 1795608000000ULL;
constexpr uint32_t kGoldWritItemId = 3415;
constexpr uint32_t kClearCharmItemId = 3416;

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

class PlayerJianghuTest : public ::testing::Test
{
protected:
	void SetUp() override
	{
		ItemTableManager::Instance().Load();
		JianghuRuleTableManager::Instance().Load();
		SpiritShopLimitGroupTableManager::Instance().Load();
		SpiritShopGoodsTableManager::Instance().Load();
		ASSERT_TRUE(ArmItemSegment(7100000, 7200000));

		player = tlsEcs.actorRegistry.create();
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, kPlayerId);
		tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 12;
		tlsEcs.actorRegistry.emplace<PlayerRoleAdmissionComp>(player, PlayerRoleAdmissionComp{1, 10});
		tlsEcs.actorRegistry.emplace<LevelComp>(player).set_level(50);
		auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
		for (auto &bag : bags.bags)
		{
			bag.SetPlayerGuid(kPlayerId);
		}
		tlsEcs.actorRegistry.emplace<PlayerItemHoldComp>(player);
		tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(player);
		tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
		tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
		tlsEcs.actorRegistry.emplace<PlayerSpiritShopComp>(player);
		tlsEcs.actorRegistry.emplace<PlayerJianghuComp>(player);
		ASSERT_TRUE(tlsEcs.playerList.emplace(kPlayerId, player).second);
	}

	void TearDown() override
	{
		tlsEcs.playerList.erase(kPlayerId);
		if (tlsEcs.actorRegistry.valid(player))
		{
			tlsEcs.actorRegistry.destroy(player);
		}
	}

	void GrantBoundItem(uint32_t itemId, uint32_t count)
	{
		InitItemParam param;
		param.itemPBComp.set_config_id(itemId);
		param.itemPBComp.set_size(count);
		param.itemPBComp.set_item_id(kInvalidGuid);
		param.itemPBComp.set_bound(true);
		auto &inv = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
		ASSERT_EQ(kSuccess, BagService::AddItem(player, inv, param));
	}

	entt::entity player{entt::null};
};

TEST_F(PlayerJianghuTest, ZeroMaterialRewardForJianghuDuel)
{
	BattleSettlementData settlement;
	settlement.set_battle_id(77001);
	settlement.set_player_id(kPlayerId);
	settlement.set_match_mode(match::MATCH_MODE_JIANGHU_DUEL);
	settlement.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN);
	settlement.set_player_team_index(0);
	settlement.set_total_rounds(6);
	settlement.set_manual_action_rounds(6);
	settlement.set_effective_combat_contribution(true);

	EXPECT_FALSE(PlayerEquipmentMaterialRewardSystem::IsEligibleBattle(settlement));
}

TEST_F(PlayerJianghuTest, SpiritShopSellsBoundGoldWritAndClearCharm)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyDiamond, 100));

	auto buyGoods = [&](uint32_t goodsId, uint64_t reqId, uint64_t price, uint32_t itemId) {
		BuySpiritShopGoodsRequest req;
		req.set_client_request_id(reqId);
		req.set_goods_id(goodsId);
		req.set_count(1);
		req.set_expected_currency_type(static_cast<uint32_t>(kCurrencyDiamond));
		req.set_expected_unit_price(price);
		auto *expItem = req.add_expected_items();
		expItem->set_item_config_id(itemId);
		expItem->set_item_count(1);
		expItem->set_bound(true);

		BuySpiritShopGoodsResponse resp;
		ASSERT_EQ(kSuccess, PlayerSpiritShopSystem::Buy(player, req, kNowMs, resp));
	};

	buyGoods(305, 88001, 20, kGoldWritItemId);
	buyGoods(306, 88002, 10, kClearCharmItemId);
	EXPECT_EQ(70u, CurrencySystem::GetBalance(player, kCurrencyDiamond));

	JianghuProfileView view;
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::GetProfile(player, kNowMs, view));
	EXPECT_EQ(1u, view.gold_writ_item_count());
	EXPECT_EQ(1u, view.clear_charm_item_count());
}

TEST_F(PlayerJianghuTest, RefuseStrangersAndEligibilityCheck)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 600));
	JianghuProfileView view;
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::SetRefuseStrangers(player, true, kNowMs, view));
	EXPECT_TRUE(view.duel_refuse_strangers());

	CheckJianghuDuelEligibilityRequest req;
	req.add_player_ids(kPlayerId);
	req.add_player_ids(999999);
	req.set_payer_player_id(kPlayerId);
	req.set_writ_tier(static_cast<uint32_t>(match::JIANGHU_WRIT_TIER_SILVER));
	req.set_silver_cost(500);

	CheckJianghuDuelEligibilityResponse resp;
	PlayerJianghuSystem::CheckDuelEligibility(req, resp);
	ASSERT_EQ(0u, resp.error_message().id());
	ASSERT_EQ(2, resp.players_size());
	EXPECT_TRUE(resp.players(0).online_on_node());
	EXPECT_TRUE(resp.players(0).duel_refuse_strangers());
	EXPECT_FALSE(resp.players(1).online_on_node());
	EXPECT_TRUE(resp.payer_has_sufficient_fee());

	// 金帖余额不足时返回 false。
	req.set_writ_tier(static_cast<uint32_t>(match::JIANGHU_WRIT_TIER_GOLD_ITEM));
	req.set_item_config_id(kGoldWritItemId);
	PlayerJianghuSystem::CheckDuelEligibility(req, resp);
	EXPECT_FALSE(resp.payer_has_sufficient_fee());
}

TEST_F(PlayerJianghuTest, GoldWritHoldCancelAndSettlementConsume)
{
	GrantBoundItem(kGoldWritItemId, 1);

	JianghuDuelPrepareFee fee;
	fee.set_duel_invite_id(1001);
	fee.set_writ_tier(static_cast<uint32_t>(match::JIANGHU_WRIT_TIER_GOLD_ITEM));
	fee.set_item_config_id(kGoldWritItemId);

	item_hold::HoldLines heldLines;
	uint64_t silverToDeduct = 0;
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::ValidateAndCollectPrepareFee(player, 55001, fee, heldLines, silverToDeduct));
	EXPECT_EQ(0u, silverToDeduct);
	ASSERT_EQ(1u, heldLines[kGoldWritItemId]);

	ASSERT_EQ(kSuccess, BagService::PlaceHold(player, 55001, item_hold::ItemHoldKind::kBattle, heldLines, kNowMs, kNowMs + 60000));
	auto &inv = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
	EXPECT_EQ(0u, BagService::RemovableCount(player, inv, kGoldWritItemId, 0));

	// 取消备战释放预留，金帖回到可动用状态。
	EXPECT_TRUE(BagService::ReleaseHold(player, 55001));
	EXPECT_EQ(1u, BagService::RemovableCount(player, inv, kGoldWritItemId, 0));

	// 重新预留后，0 回合流局不消耗金帖；真实打完消耗 1 张金帖。
	ASSERT_EQ(kSuccess, BagService::PlaceHold(player, 55002, item_hold::ItemHoldKind::kBattle, heldLines, kNowMs, kNowMs + 60000));
	auto &comp = tlsEcs.actorRegistry.get<PlayerJianghuComp>(player);

	BattleSettlementData zeroRound;
	zeroRound.set_battle_id(55002);
	zeroRound.set_player_id(kPlayerId);
	zeroRound.set_match_mode(match::MATCH_MODE_JIANGHU_DUEL);
	zeroRound.set_outcome(BATTLE_OUTCOME_DRAW);
	zeroRound.set_total_rounds(0);
	zeroRound.mutable_jianghu_duel()->set_challenger_captain_id(kPlayerId);
	zeroRound.mutable_jianghu_duel()->set_writ_tier(static_cast<uint32_t>(match::JIANGHU_WRIT_TIER_GOLD_ITEM));

	PlayerJianghuComp cand;
	bool changed = false;
	uint32_t goldWritToConsume = 0;
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::PrepareBattleSettlement(player, comp, zeroRound, kNowMs, cand, changed, goldWritToConsume));
	EXPECT_EQ(0u, goldWritToConsume);

	BattleSettlementData realFinish = zeroRound;
	realFinish.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN);
	realFinish.set_player_team_index(0);
	realFinish.set_total_rounds(4);
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::PrepareBattleSettlement(player, comp, realFinish, kNowMs, cand, changed, goldWritToConsume));
	EXPECT_EQ(kGoldWritItemId, goldWritToConsume);
}

TEST_F(PlayerJianghuTest, SilverWritDebitAndCancelRefund)
{
	JianghuDuelPrepareFee fee;
	fee.set_duel_invite_id(1002);
	fee.set_writ_tier(static_cast<uint32_t>(match::JIANGHU_WRIT_TIER_SILVER));
	fee.set_silver_cost(500);

	item_hold::HoldLines heldLines;
	uint64_t silverToDeduct = 0;
	EXPECT_EQ(kJianghuInsufficientWritOrSilver,
			  PlayerJianghuSystem::ValidateAndCollectPrepareFee(player, 56001, fee, heldLines, silverToDeduct));

	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 1200));
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::ValidateAndCollectPrepareFee(player, 56001, fee, heldLines, silverToDeduct));
	EXPECT_EQ(500u, silverToDeduct);
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::CommitPrepareSilverFee(player, 56001, silverToDeduct, kNowMs + 60000));
	EXPECT_EQ(700u, CurrencySystem::GetBalance(player, kCurrencyGold));
	const auto &comp = tlsEcs.actorRegistry.get<PlayerJianghuComp>(player);
	EXPECT_EQ(56001u, comp.pending_silver_fee_battle_id());
	EXPECT_EQ(500u, comp.pending_silver_fee_amount());
	EXPECT_EQ(kNowMs + 60000, comp.pending_silver_fee_review_after_ms()) << "复核时刻与同一局的道具预留相同";

	// 同一局重入备战不重扣。
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::ValidateAndCollectPrepareFee(player, 56001, fee, heldLines, silverToDeduct));
	EXPECT_EQ(0u, silverToDeduct);
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::CommitPrepareSilverFee(player, 56001, 500, kNowMs + 60000));
	EXPECT_EQ(700u, CurrencySystem::GetBalance(player, kCurrencyGold));

	// 取消备战：精准退还 500 银两并清掉三个标记字段，重调幂等不双退。
	EXPECT_TRUE(PlayerJianghuSystem::RefundPendingSilverFee(player, 56001, "prepare_cancelled"));
	EXPECT_EQ(1200u, CurrencySystem::GetBalance(player, kCurrencyGold));
	EXPECT_EQ(0u, comp.pending_silver_fee_battle_id());
	EXPECT_EQ(0u, comp.pending_silver_fee_amount());
	EXPECT_EQ(0u, comp.pending_silver_fee_review_after_ms());
	EXPECT_FALSE(PlayerJianghuSystem::RefundPendingSilverFee(player, 56001, "prepare_cancelled"));
	EXPECT_EQ(1200u, CurrencySystem::GetBalance(player, kCurrencyGold));
}

// 别的局还有未结清的预扣(取消没送到、终局还没落定)时,新的普通战书备战一律拒绝、标记不动:
// 覆盖标记就把旧局那 500 永久弄丢(2026-11-26,数据覆写审计 r2 第 4 轮)。金帖档不写标记,不受影响。
TEST_F(PlayerJianghuTest, SilverWritPendingForAnotherBattleRejectsTheNewPreparationWithoutTouchingTheMarker)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 1500));
	JianghuDuelPrepareFee silverFee;
	silverFee.set_duel_invite_id(1101);
	silverFee.set_writ_tier(static_cast<uint32_t>(match::JIANGHU_WRIT_TIER_SILVER));
	silverFee.set_silver_cost(500);
	item_hold::HoldLines heldLines;
	uint64_t silverToDeduct = 0;
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::ValidateAndCollectPrepareFee(player, 56101, silverFee, heldLines, silverToDeduct));
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::CommitPrepareSilverFee(player, 56101, silverToDeduct, kNowMs + 60000));
	ASSERT_EQ(1000u, CurrencySystem::GetBalance(player, kCurrencyGold));

	EXPECT_EQ(kFeatureUnavailable,
			  PlayerJianghuSystem::ValidateAndCollectPrepareFee(player, 56102, silverFee, heldLines, silverToDeduct));
	EXPECT_EQ(0u, silverToDeduct);
	EXPECT_EQ(kFeatureUnavailable, PlayerJianghuSystem::CommitPrepareSilverFee(player, 56102, 500, kNowMs + 90000))
		<< "提交点复核同一条规则";
	const auto &comp = tlsEcs.actorRegistry.get<PlayerJianghuComp>(player);
	EXPECT_EQ(56101u, comp.pending_silver_fee_battle_id()) << "旧局的标记没被覆盖";
	EXPECT_EQ(500u, comp.pending_silver_fee_amount());
	EXPECT_EQ(kNowMs + 60000, comp.pending_silver_fee_review_after_ms());
	EXPECT_EQ(1000u, CurrencySystem::GetBalance(player, kCurrencyGold)) << "一分没多扣";

	// 金帖档只走道具预留,不碰待退标记。
	GrantBoundItem(kGoldWritItemId, 1);
	JianghuDuelPrepareFee goldFee;
	goldFee.set_duel_invite_id(1102);
	goldFee.set_writ_tier(static_cast<uint32_t>(match::JIANGHU_WRIT_TIER_GOLD_ITEM));
	goldFee.set_item_config_id(kGoldWritItemId);
	item_hold::HoldLines goldLines;
	EXPECT_EQ(kSuccess, PlayerJianghuSystem::ValidateAndCollectPrepareFee(player, 56103, goldFee, goldLines, silverToDeduct));
	EXPECT_EQ(1u, goldLines[kGoldWritItemId]);

	// 旧局退还之后照常能开。
	ASSERT_TRUE(PlayerJianghuSystem::RefundPendingSilverFee(player, 56101, "ABORT"));
	EXPECT_EQ(1500u, CurrencySystem::GetBalance(player, kCurrencyGold));
	EXPECT_EQ(kSuccess, PlayerJianghuSystem::ValidateAndCollectPrepareFee(player, 56102, silverFee, heldLines, silverToDeduct));
	EXPECT_EQ(500u, silverToDeduct);
}

// 先入账成功、再清标记:入账失败(这里用余额溢出)保留标记,之后照常退一次;玩家不可改时什么都不动。
TEST_F(PlayerJianghuTest, SilverRefundKeepsTheMarkerWhenTheCreditFailsOrThePlayerIsNotMutable)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 1000));
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::CommitPrepareSilverFee(player, 56201, 500, kNowMs + 60000));
	const auto &comp = tlsEcs.actorRegistry.get<PlayerJianghuComp>(player);

	auto &epoch = tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player);
	epoch.epoch = 0; // 没有归属围栏:本节点此刻不改这名玩家的存档
	EXPECT_FALSE(PlayerJianghuSystem::RefundPendingSilverFee(player, 56201, "prepare_cancelled"));
	EXPECT_EQ(56201u, comp.pending_silver_fee_battle_id());
	EXPECT_EQ(500u, CurrencySystem::GetBalance(player, kCurrencyGold));
	epoch.epoch = 12;

	constexpr uint64_t kNearlyFull = std::numeric_limits<uint64_t>::max() - 100;
	tlsEcs.actorRegistry.get<CurrencyComp>(player).set_values(static_cast<int>(kCurrencyGold), kNearlyFull);
	EXPECT_FALSE(PlayerJianghuSystem::RefundPendingSilverFee(player, 56201, "void_settlement"));
	EXPECT_EQ(56201u, comp.pending_silver_fee_battle_id()) << "入账失败:标记留着,复核下一拍重试";
	EXPECT_EQ(500u, comp.pending_silver_fee_amount());
	EXPECT_EQ(kNearlyFull, CurrencySystem::GetBalance(player, kCurrencyGold));

	tlsEcs.actorRegistry.get<CurrencyComp>(player).set_values(static_cast<int>(kCurrencyGold), 500);
	EXPECT_TRUE(PlayerJianghuSystem::RefundPendingSilverFee(player, 56201, "ABORT"));
	EXPECT_EQ(1000u, CurrencySystem::GetBalance(player, kCurrencyGold));
	EXPECT_EQ(0u, comp.pending_silver_fee_battle_id());
	EXPECT_FALSE(PlayerJianghuSystem::RefundPendingSilverFee(player, 56201, "ABORT"));
	EXPECT_EQ(1000u, CurrencySystem::GetBalance(player, kCurrencyGold)) << "只退一次";
}

// 退还的是玩家自己预扣的钱:获取封禁(这里是本人 GM 封禁流通银两)挡新增获得,不挡退费。
TEST_F(PlayerJianghuTest, GainBlockDoesNotStopTheSilverRefund)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 1000));
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::CommitPrepareSilverFee(player, 56301, 500, kNowMs + 60000));
	EXPECT_FALSE(GainBlockService::CountsAsGain(TX_JIANGHU_DUEL_REFUND));
	EXPECT_TRUE(GainBlockService::CountsAsGain(TX_JIANGHU_DUEL_FEE));

	ASSERT_EQ(kSuccess, CurrencySystem::BlockCurrency(player, kCurrencyGold));
	EXPECT_EQ(static_cast<uint32_t>(kAssetBlocked), CurrencySystem::AddCurrency(player, kCurrencyGold, 1))
		<< "新增获得仍被挡";
	EXPECT_TRUE(PlayerJianghuSystem::RefundPendingSilverFee(player, 56301, "prepare_cancelled"));
	EXPECT_EQ(1000u, CurrencySystem::GetBalance(player, kCurrencyGold));
	EXPECT_EQ(0u, tlsEcs.actorRegistry.get<PlayerJianghuComp>(player).pending_silver_fee_battle_id());
}

TEST_F(PlayerJianghuTest, DefeatMarkOverwriteRevengeClearAndOwnerRevisionFence)
{
	auto &comp = tlsEcs.actorRegistry.get<PlayerJianghuComp>(player);
	BattleSettlementData loss1;
	loss1.set_battle_id(60001);
	loss1.set_player_id(kPlayerId);
	loss1.set_match_mode(match::MATCH_MODE_JIANGHU_DUEL);
	loss1.set_outcome(BATTLE_OUTCOME_SIDE_B_WIN);
	loss1.set_player_team_index(0);
	loss1.set_total_rounds(5);
	auto *ctx1 = loss1.mutable_jianghu_duel();
	ctx1->set_writ_tier(static_cast<uint32_t>(match::JIANGHU_WRIT_TIER_GOLD_ITEM));
	ctx1->set_challenger_captain_id(kPlayerId);
	ctx1->set_target_captain_id(kOpponentCaptainId);
	ctx1->set_target_captain_name("QingFeng");

	PlayerJianghuComp cand;
	bool changed = false;
	uint32_t goldWrit = 0;
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::PrepareBattleSettlement(player, comp, loss1, kNowMs, cand, changed, goldWrit));
	ASSERT_TRUE(changed);
	comp.Swap(&cand);
	EXPECT_EQ(kNowMs + 1800000ULL, comp.defeat_mark_expires_at_ms());
	EXPECT_EQ(kOpponentCaptainId, comp.defeat_mark_winner_id());
	EXPECT_EQ("QingFeng", comp.defeat_mark_winner_name());
	EXPECT_EQ(2u, comp.defeat_mark_style_id());

	// 赢了无关对手不清除印记。
	BattleSettlementData winOther = loss1;
	winOther.set_battle_id(60002);
	winOther.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN);
	winOther.mutable_jianghu_duel()->set_target_captain_id(kOtherCaptainId);
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::PrepareBattleSettlement(player, comp, winOther, kNowMs + 5000, cand, changed, goldWrit));
	EXPECT_FALSE(changed);

	// 复仇击败原胜方队长，立刻清除战败印记。
	BattleSettlementData revenge = loss1;
	revenge.set_battle_id(60003);
	revenge.set_outcome(BATTLE_OUTCOME_SIDE_A_WIN);
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::PrepareBattleSettlement(player, comp, revenge, kNowMs + 10000, cand, changed, goldWrit));
	ASSERT_TRUE(changed);
	comp.Swap(&cand);
	EXPECT_EQ(0u, comp.defeat_mark_expires_at_ms());

	// 重新挂印记后模拟角色官方过户(ownerRevision 10 -> 11)，查询即隐藏旧印记。
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::PrepareBattleSettlement(player, comp, loss1, kNowMs + 20000, cand, changed, goldWrit));
	comp.Swap(&cand);
	tlsEcs.actorRegistry.get<PlayerRoleAdmissionComp>(player).ownerRevision = 11;
	JianghuProfileView view;
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::GetProfile(player, kNowMs + 21000, view));
	EXPECT_FALSE(view.has_active_defeat_mark());
}

TEST_F(PlayerJianghuTest, ClearDefeatMarkSilverAndCharmIdempotent)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 2500));
	GrantBoundItem(kClearCharmItemId, 2);

	auto setMark = [&](uint64_t battleId) {
		auto &comp = tlsEcs.actorRegistry.get<PlayerJianghuComp>(player);
		comp.set_owner_revision(10);
		comp.set_defeat_mark_expires_at_ms(kNowMs + 1800000ULL);
		comp.set_defeat_mark_winner_id(kOpponentCaptainId);
		comp.set_defeat_mark_winner_name("QingFeng");
		comp.set_defeat_mark_style_id(1);
		comp.set_defeat_mark_battle_id(battleId);
	};

	// 无印记时拒绝。
	{
		ClearJianghuDefeatMarkRequest req;
		req.set_client_request_id(90001);
		req.set_mode(JIANGHU_CLEAR_MARK_MODE_SILVER);
		ClearJianghuDefeatMarkResponse resp;
		EXPECT_EQ(kJianghuNoActiveDefeatMark, PlayerJianghuSystem::ClearDefeatMark(player, req, kNowMs, resp));
	}

	// 1000 银两清除 + 同键重放不双扣 + 换参数拒绝。
	setMark(70001);
	{
		ClearJianghuDefeatMarkRequest req;
		req.set_client_request_id(90002);
		req.set_mode(JIANGHU_CLEAR_MARK_MODE_SILVER);
		ClearJianghuDefeatMarkResponse resp;
		ASSERT_EQ(kSuccess, PlayerJianghuSystem::ClearDefeatMark(player, req, kNowMs, resp));
		EXPECT_FALSE(resp.replayed());
		EXPECT_FALSE(resp.profile().has_active_defeat_mark());
		EXPECT_FALSE(resp.profile().has_active_clear_badge());
		EXPECT_EQ(1500u, CurrencySystem::GetBalance(player, kCurrencyGold));

		ClearJianghuDefeatMarkResponse replay;
		ASSERT_EQ(kSuccess, PlayerJianghuSystem::ClearDefeatMark(player, req, kNowMs + 1000, replay));
		EXPECT_TRUE(replay.replayed());
		EXPECT_EQ(1500u, CurrencySystem::GetBalance(player, kCurrencyGold));

		req.set_mode(JIANGHU_CLEAR_MARK_MODE_ITEM_CHARM);
		ClearJianghuDefeatMarkResponse mismatch;
		EXPECT_EQ(kInvalidParameter, PlayerJianghuSystem::ClearDefeatMark(player, req, kNowMs + 2000, mismatch));
	}

	// 清风洗尘符清除 + 附赠 30 分钟洒脱徽记 + 重放不双扣。
	setMark(70002);
	{
		ClearJianghuDefeatMarkRequest req;
		req.set_client_request_id(90003);
		req.set_mode(JIANGHU_CLEAR_MARK_MODE_ITEM_CHARM);
		ClearJianghuDefeatMarkResponse resp;
		ASSERT_EQ(kSuccess, PlayerJianghuSystem::ClearDefeatMark(player, req, kNowMs, resp));
		EXPECT_FALSE(resp.replayed());
		EXPECT_FALSE(resp.profile().has_active_defeat_mark());
		EXPECT_TRUE(resp.profile().has_active_clear_badge());
		EXPECT_EQ(1u, resp.profile().clear_badge_style_id());
		EXPECT_EQ(1u, resp.profile().clear_charm_item_count());

		ClearJianghuDefeatMarkResponse replay;
		ASSERT_EQ(kSuccess, PlayerJianghuSystem::ClearDefeatMark(player, req, kNowMs + 1000, replay));
		EXPECT_TRUE(replay.replayed());
		EXPECT_EQ(1u, replay.profile().clear_charm_item_count());
	}
}

TEST_F(PlayerJianghuTest, VoidAndZeroRoundRefundPendingSilverFeeOnce)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 1000));
	// 1. 备战预扣 500 银两后遇到 BATTLE_OUTCOME_VOID (含节点崩溃/超时作废收件箱)，作废结算退还一次，重复调用不双退。
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::CommitPrepareSilverFee(player, 81001, 500, kNowMs + 60000));
	EXPECT_EQ(500u, CurrencySystem::GetBalance(player, kCurrencyGold));
	EXPECT_TRUE(PlayerJianghuSystem::RefundPendingSilverFee(player, 81001, "void_settlement"));
	EXPECT_EQ(1000u, CurrencySystem::GetBalance(player, kCurrencyGold));
	EXPECT_FALSE(PlayerJianghuSystem::RefundPendingSilverFee(player, 81001, "void_settlement"));
	EXPECT_EQ(1000u, CurrencySystem::GetBalance(player, kCurrencyGold));

	// 2. 0 回合流局：PrepareBattleSettlement 返回 500 待退银两并清零 pending_silver_fee。
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::CommitPrepareSilverFee(player, 81002, 500, kNowMs + 60000));
	EXPECT_EQ(500u, CurrencySystem::GetBalance(player, kCurrencyGold));
	const auto &comp = tlsEcs.actorRegistry.get<PlayerJianghuComp>(player);
	BattleSettlementData zeroRound;
	zeroRound.set_battle_id(81002);
	zeroRound.set_player_id(kPlayerId);
	zeroRound.set_match_mode(match::MATCH_MODE_JIANGHU_DUEL);
	zeroRound.set_outcome(BATTLE_OUTCOME_DRAW);
	zeroRound.set_total_rounds(0);
	PlayerJianghuComp cand;
	bool changed = false;
	uint32_t goldWrit = 0;
	uint64_t silverRefund = 0;
	ASSERT_EQ(kSuccess, PlayerJianghuSystem::PrepareBattleSettlement(
		player, comp, zeroRound, kNowMs, cand, changed, goldWrit, &silverRefund));
	EXPECT_TRUE(changed);
	EXPECT_EQ(0u, goldWrit);
	EXPECT_EQ(500u, silverRefund);
	EXPECT_EQ(0u, cand.pending_silver_fee_battle_id());
	EXPECT_EQ(0u, cand.pending_silver_fee_amount());
	EXPECT_EQ(0u, cand.pending_silver_fee_review_after_ms());
}

} // namespace
