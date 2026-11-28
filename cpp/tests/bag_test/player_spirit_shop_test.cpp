// 官方灵石百宝阁(docs/design/spirit-shop-and-starter-packs-v1.md)单测:
//   * SpiritShopRulesTest —— 纯规则(周边界与抗回拨、防溢出、限购记账、组件校验)。
//   * PlayerSpiritShopTest —— 真实配表加载、双余额展示、首套 300 份共享组自由拼单与封顶、
//     严格绑定分堆防洗白、禁止绑定灵石混扣、报价防呆、满包双物原子拒单、封禁/战斗/冻结/缺组件拦截、幂等重放。

#include <gtest/gtest.h>

#include <cstdint>
#include <functional>
#include <limits>
#include <string>
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
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_spirit_shop_comp.pb.h"
#include "proto/scene/player_spirit_shop.pb.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/player_spirit_shop.h"
#include "services/scene/player/system/spirit_shop_rules.h"
#include "table/code/item_table.h"
#include "table/code/spiritshopgoods_table.h"
#include "table/code/spiritshoplimitgroup_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/spirit_shop_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{

constexpr uint64_t kPlayerGuid = 828281;
constexpr uint64_t kSegmentLo = 2400000;
constexpr uint64_t kSegmentHi = 2500000;
// 2026-11-25 12:00:00 UTC (已进入某 UTC+8 周一 05:00 之后的正常周内)
constexpr uint64_t kNowMs = 1795521600000ULL;
// 2027 春节礼盒定时窗:2027-02-05 00:00:00 +08:00 ~ 2027-02-20 23:59:59 +08:00
constexpr uint64_t kSpringStartMs = 1801756800000ULL;
constexpr uint64_t kSpringEndMs = 1803139199000ULL;

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

// ════════════════════════════════════════════════════════════════════════════
// 纯规则单测
// ════════════════════════════════════════════════════════════════════════════

TEST(SpiritShopRulesTest, WeeklyResetAndClockRollbackProtection)
{
	uint64_t week1 = 0, next1 = 0;
	ASSERT_TRUE(spirit_shop_rules::ResolveWeek(kNowMs, week1, next1));
	EXPECT_EQ(next1 - week1, spirit_shop_rules::kWeekMs);

	PlayerSpiritShopComp comp;
	ASSERT_TRUE(spirit_shop_rules::RecordPurchase(comp, 901, 2, 5, 2, 100, week1));
	EXPECT_EQ(2u, spirit_shop_rules::BoughtCount(comp, 901, spirit_shop_rules::kResetWeekly, week1));
	EXPECT_EQ(100u, spirit_shop_rules::UsedGroupWeight(comp, 2, spirit_shop_rules::kResetWeekly, week1));

	// 时钟回拨到上一周:绝不清零本周已购数与已用权重。
	const uint64_t prevWeek = week1 - spirit_shop_rules::kWeekMs;
	EXPECT_EQ(2u, spirit_shop_rules::BoughtCount(comp, 901, spirit_shop_rules::kResetWeekly, prevWeek));
	EXPECT_EQ(100u, spirit_shop_rules::UsedGroupWeight(comp, 2, spirit_shop_rules::kResetWeekly, prevWeek));

	// 推进到下一周:周计数归零,终身计数永久保留。
	EXPECT_EQ(0u, spirit_shop_rules::BoughtCount(comp, 901, spirit_shop_rules::kResetWeekly, next1));
	EXPECT_EQ(0u, spirit_shop_rules::UsedGroupWeight(comp, 2, spirit_shop_rules::kResetWeekly, next1));
	EXPECT_EQ(2u, spirit_shop_rules::BoughtCount(comp, 901, spirit_shop_rules::kResetLifetime, next1));
	EXPECT_EQ(100u, spirit_shop_rules::UsedGroupWeight(comp, 2, spirit_shop_rules::kResetLifetime, next1));
}

TEST(SpiritShopRulesTest, OverflowAndValidationGuards)
{
	int64_t cost = 0;
	EXPECT_FALSE(spirit_shop_rules::TotalCost(0, 1, cost));
	EXPECT_FALSE(spirit_shop_rules::TotalCost(100, 0, cost));
	EXPECT_FALSE(spirit_shop_rules::TotalCost(100, spirit_shop_rules::kMaxBuyCountPerRequest + 1, cost));
	EXPECT_FALSE(spirit_shop_rules::TotalCost(spirit_shop_rules::kMaxUnitPrice + 1, 1, cost));
	ASSERT_TRUE(spirit_shop_rules::TotalCost(180, 10, cost));
	EXPECT_EQ(1800, cost);

	uint32_t weight = 0;
	EXPECT_FALSE(spirit_shop_rules::TotalGroupWeight(0, 1, weight));
	EXPECT_FALSE(spirit_shop_rules::TotalGroupWeight(spirit_shop_rules::kMaxLimitGroupWeight + 1, 1, weight));
	ASSERT_TRUE(spirit_shop_rules::TotalGroupWeight(60, 5, weight));
	EXPECT_EQ(300u, weight);

	EXPECT_TRUE(spirit_shop_rules::WouldExceedLimit(240, 180, 300));
	EXPECT_FALSE(spirit_shop_rules::WouldExceedLimit(240, 60, 300));
	EXPECT_TRUE(spirit_shop_rules::WouldExceedLimit(1, std::numeric_limits<uint32_t>::max(), 300));

	PlayerSpiritShopComp bad;
	bad.add_goods_records()->set_goods_id(101);
	bad.add_goods_records()->set_goods_id(101);
	EXPECT_FALSE(spirit_shop_rules::ValidateComp(bad).empty());
}

TEST(SpiritShopRulesTest, V2SaleWindowLevelAndChoiceRules)
{
	// 定时上下架:左闭右开 [start, end),0 表示该侧不限
	EXPECT_TRUE(spirit_shop_rules::IsInSaleWindow(0, 0, kNowMs));
	EXPECT_FALSE(spirit_shop_rules::IsInSaleWindow(kSpringStartMs, kSpringEndMs, kSpringStartMs - 1));
	EXPECT_TRUE(spirit_shop_rules::IsInSaleWindow(kSpringStartMs, kSpringEndMs, kSpringStartMs));
	EXPECT_TRUE(spirit_shop_rules::IsInSaleWindow(kSpringStartMs, kSpringEndMs, kSpringEndMs - 1));
	EXPECT_FALSE(spirit_shop_rules::IsInSaleWindow(kSpringStartMs, kSpringEndMs, kSpringEndMs));
	EXPECT_TRUE(spirit_shop_rules::IsInSaleWindow(kSpringStartMs, 0, kSpringEndMs + 1000));
	EXPECT_FALSE(spirit_shop_rules::IsInSaleWindow(0, kSpringEndMs, kSpringEndMs));

	// 角色有效等级门槛:闭区间 [min, max],0 表示该侧不限
	EXPECT_TRUE(spirit_shop_rules::IsLevelEligible(1, 0, 0));
	EXPECT_FALSE(spirit_shop_rules::IsLevelEligible(9, 10, 0));
	EXPECT_TRUE(spirit_shop_rules::IsLevelEligible(10, 10, 0));
	EXPECT_TRUE(spirit_shop_rules::IsLevelEligible(30, 10, 30));
	EXPECT_FALSE(spirit_shop_rules::IsLevelEligible(31, 10, 30));

	// ALL vs CHOOSE_ONE 解析
	spirit_shop_rules::ResolvedGoods bundle;
	bundle.grantMode = spirit_shop_rules::kGrantModeAll;
	bundle.items = {{3001, 60, true}, {3002, 60, true}, {3405, 2, true}, {3402, 1, true}};
	std::vector<spirit_shop_rules::ResolvedItem> selected;
	ASSERT_TRUE(spirit_shop_rules::ResolveSelectedItems(bundle, 0, selected));
	EXPECT_EQ(4u, selected.size());
	EXPECT_FALSE(spirit_shop_rules::ResolveSelectedItems(bundle, 1, selected));

	bundle.grantMode = spirit_shop_rules::kGrantModeChooseOne;
	ASSERT_TRUE(spirit_shop_rules::ResolveSelectedItems(bundle, 2, selected));
	ASSERT_EQ(1u, selected.size());
	EXPECT_EQ(3405u, selected[0].itemConfigId);
	EXPECT_EQ(2u, selected[0].itemCount);
	EXPECT_FALSE(spirit_shop_rules::ResolveSelectedItems(bundle, 4, selected));

	spirit_shop_rules::ResolvedGoods oneItemChoice;
	oneItemChoice.grantMode = spirit_shop_rules::kGrantModeChooseOne;
	oneItemChoice.items = {{3408, 1, true}};
	EXPECT_FALSE(spirit_shop_rules::ResolveSelectedItems(oneItemChoice, 0, selected));
}

// ════════════════════════════════════════════════════════════════════════════
// 编排与实表单测
// ════════════════════════════════════════════════════════════════════════════

class PlayerSpiritShopTest : public testing::Test
{
protected:
	void SetUp() override
	{
		ItemTableManager::Instance().Load();
		SpiritShopLimitGroupTableManager::Instance().Load();
		SpiritShopGoodsTableManager::Instance().Load();
		ASSERT_TRUE(ArmItemSegment(kSegmentLo, kSegmentHi));
		ASSERT_TRUE(PlayerSpiritShopSystem::ValidateTables());
		player = NewPlayer(kPlayerGuid, /*diamond=*/1000, /*bindDiamond=*/500);
	}

	void TearDown() override
	{
		GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, 3405);
		for (const auto guid : playerGuids)
		{
			tlsEcs.playerList.erase(Guid{guid});
		}
		for (const auto entity : players)
		{
			if (tlsEcs.actorRegistry.valid(entity))
			{
				tlsEcs.actorRegistry.destroy(entity);
			}
		}
		players.clear();
		playerGuids.clear();
		EXPECT_TRUE(ArmItemSegment(1, uint64_t{1} << 54));
	}

	entt::entity NewPlayer(uint64_t guid, uint64_t diamond, uint64_t bindDiamond)
	{
		const auto entity = tlsEcs.actorRegistry.create();
		players.push_back(entity);
		playerGuids.push_back(guid);
		tlsEcs.playerList[Guid{guid}] = entity;
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(entity, Guid{guid});
		tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(entity).epoch = 1;
		auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(entity);
		for (auto &bag : bags.bags)
		{
			bag.SetPlayerGuid(Guid{guid});
		}
		auto &currency = tlsEcs.actorRegistry.emplace<CurrencyComp>(entity);
		while (currency.values_size() < static_cast<int>(kCurrencyMax))
		{
			currency.add_values(0);
		}
		currency.set_values(static_cast<int>(kCurrencyDiamond), diamond);
		currency.set_values(static_cast<int>(kCurrencyBindDiamond), bindDiamond);
		tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(entity);
		tlsEcs.actorRegistry.emplace<PlayerSpiritShopComp>(entity);
		tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(entity);
		auto &lvl = tlsEcs.actorRegistry.emplace<LevelComp>(entity);
		lvl.set_level(1);
		return entity;
	}

	void SetPlayerLevel(entt::entity who, uint32_t level)
	{
		auto &lvl = tlsEcs.actorRegistry.get_or_emplace<LevelComp>(who);
		lvl.set_level(level);
	}

	Bag &Inventory(entt::entity who) { return tlsEcs.actorRegistry.get<PlayerBagsComp>(who).bags[kInventory]; }

	::BuySpiritShopGoodsRequest BuildBuyRequestFromShelfAt(entt::entity who, uint32_t goodsId, uint32_t count,
														   uint64_t reqId, uint64_t nowMs, uint32_t chosenIndex = 0)
	{
		::SpiritShopInfo info;
		PlayerSpiritShopSystem::FillInfo(who, nowMs, info);
		::BuySpiritShopGoodsRequest req;
		req.set_client_request_id(reqId);
		req.set_goods_id(goodsId);
		req.set_count(count);
		req.set_chosen_item_index(chosenIndex);
		for (const auto &g : info.goods())
		{
			if (g.goods_id() == goodsId)
			{
				req.set_expected_currency_type(g.currency_type());
				req.set_expected_unit_price(g.unit_price());
				if (g.grant_mode() == spirit_shop_rules::kGrantModeChooseOne)
				{
					if (chosenIndex < static_cast<uint32_t>(g.items_size()))
					{
						*req.add_expected_items() = g.items(static_cast<int>(chosenIndex));
					}
				}
				else
				{
					for (const auto &item : g.items())
					{
						*req.add_expected_items() = item;
					}
				}
				break;
			}
		}
		return req;
	}

	::BuySpiritShopGoodsRequest BuildBuyRequestFromShelf(uint32_t goodsId, uint32_t count, uint64_t reqId)
	{
		return BuildBuyRequestFromShelfAt(player, goodsId, count, reqId, kNowMs, 0);
	}

	entt::entity player{entt::null};
	std::vector<entt::entity> players;
	std::vector<uint64_t> playerGuids;
};

TEST_F(PlayerSpiritShopTest, FillInfoListsFinalizedShelfAndDualBalances)
{
	::SpiritShopInfo info;
	PlayerSpiritShopSystem::FillInfo(player, kNowMs, info);
	EXPECT_EQ(1000u, info.diamond_balance());
	EXPECT_EQ(500u, info.bind_diamond_balance());
	ASSERT_EQ(1, info.limit_groups_size());
	EXPECT_EQ(1u, info.limit_groups(0).group_id());
	EXPECT_EQ(300u, info.limit_groups(0).max_weight());
	EXPECT_EQ(0u, info.limit_groups(0).used_weight());
	ASSERT_GE(info.goods_size(), 9);
	// 常态时间窗(2026-11)下,2027 春节定时礼盒 401/402 必须自动潜伏不下发
	for (const auto &g : info.goods())
	{
		EXPECT_NE(401u, g.goods_id());
		EXPECT_NE(402u, g.goods_id());
	}

	// 核对定案单价:101=60, 102=180, 103=300, 201=60, 202=180, 301=100, 302=100, 303=120, 304=180
	auto priceOf = [&](uint32_t id) -> uint64_t {
		for (const auto &g : info.goods())
		{
			if (g.goods_id() == id) return g.unit_price();
		}
		return 0;
	};
	EXPECT_EQ(60u, priceOf(101));
	EXPECT_EQ(180u, priceOf(102));
	EXPECT_EQ(300u, priceOf(103));
	EXPECT_EQ(60u, priceOf(201));
	EXPECT_EQ(180u, priceOf(202));
	EXPECT_EQ(100u, priceOf(301));
	EXPECT_EQ(100u, priceOf(302));
	EXPECT_EQ(120u, priceOf(303));
	EXPECT_EQ(180u, priceOf(304));
}

TEST_F(PlayerSpiritShopTest, SharedStarterPackComboCapAt300AndStrictBoundStacks)
{
	// 预先放 10 份可交易打造材料(3001, bound=false),验证购买绑定提速包绝不与之合堆洗白。
	InitItemParam tradable;
	tradable.itemPBComp.set_config_id(3001);
	tradable.itemPBComp.set_size(10);
	tradable.itemPBComp.set_item_id(kInvalidGuid);
	tradable.itemPBComp.set_bound(false);
	ASSERT_EQ(kSuccess, BagService::AddItem(player, Inventory(player), tradable));

	::BuySpiritShopGoodsResponse resp;
	// 买 1 份 101(60) + 1 份 102(180) + 1 份 101(60) = 300 份满额
	ASSERT_EQ(kSuccess, PlayerSpiritShopSystem::Buy(player, BuildBuyRequestFromShelf(101, 1, 1001), kNowMs, resp));
	EXPECT_FALSE(resp.replayed());
	EXPECT_EQ(60u, resp.info().limit_groups(0).used_weight());
	EXPECT_EQ(940u, CurrencySystem::GetBalance(player, kCurrencyDiamond));

	ASSERT_EQ(kSuccess, PlayerSpiritShopSystem::Buy(player, BuildBuyRequestFromShelf(102, 1, 1002), kNowMs, resp));
	EXPECT_EQ(240u, resp.info().limit_groups(0).used_weight());
	EXPECT_EQ(760u, CurrencySystem::GetBalance(player, kCurrencyDiamond));

	// 此时剩余 60 份,再买 102(180) 必须被共享组拦下,零扣费
	EXPECT_EQ(kSpiritShopGroupLimitReached,
			  PlayerSpiritShopSystem::Buy(player, BuildBuyRequestFromShelf(102, 1, 1003), kNowMs, resp));
	EXPECT_EQ(760u, CurrencySystem::GetBalance(player, kCurrencyDiamond));

	// 补买 1 份 101(60) 恰好达 300/300
	ASSERT_EQ(kSuccess, PlayerSpiritShopSystem::Buy(player, BuildBuyRequestFromShelf(101, 1, 1004), kNowMs, resp));
	EXPECT_EQ(300u, resp.info().limit_groups(0).used_weight());
	EXPECT_EQ(700u, CurrencySystem::GetBalance(player, kCurrencyDiamond));

	// 300 满额后再买任意提速包均拒绝
	EXPECT_EQ(kSpiritShopGroupLimitReached,
			  PlayerSpiritShopSystem::Buy(player, BuildBuyRequestFromShelf(101, 1, 1005), kNowMs, resp));
	EXPECT_EQ(kSpiritShopGroupLimitReached,
			  PlayerSpiritShopSystem::Buy(player, BuildBuyRequestFromShelf(103, 1, 1006), kNowMs, resp));

	// 核对背包:3001 总计 10(可交易)+150(绑定)=160,可交易量严格保持 10;3002 绑定 150、可交易 0
	EXPECT_EQ(160u, Inventory(player).GetTotalItemCount(3001));
	EXPECT_EQ(10u, BagService::RemovableTradableCount(player, Inventory(player), 3001));
	EXPECT_EQ(150u, Inventory(player).GetTotalItemCount(3002));
	EXPECT_EQ(0u, BagService::RemovableTradableCount(player, Inventory(player), 3002));
}

TEST_F(PlayerSpiritShopTest, BindDiamondCannotPayForDiamondGoods)
{
	const auto poor = NewPlayer(kPlayerGuid + 1, /*diamond=*/0, /*bindDiamond=*/9999);
	auto req = BuildBuyRequestFromShelf(201, 1, 2001);
	::BuySpiritShopGoodsResponse resp;
	EXPECT_EQ(kSpiritShopCurrencyNotEnough, PlayerSpiritShopSystem::Buy(poor, req, kNowMs, resp));
	EXPECT_EQ(0u, CurrencySystem::GetBalance(poor, kCurrencyDiamond));
	EXPECT_EQ(9999u, CurrencySystem::GetBalance(poor, kCurrencyBindDiamond));
	EXPECT_EQ(0u, Inventory(poor).GetTotalItemCount(3405));
}

TEST_F(PlayerSpiritShopTest, PriceAndItemGuardsRejectStaleQuotes)
{
	::BuySpiritShopGoodsResponse resp;
	auto wrongPrice = BuildBuyRequestFromShelf(202, 1, 3001);
	wrongPrice.set_expected_unit_price(240);
	EXPECT_EQ(kSpiritShopPriceChanged, PlayerSpiritShopSystem::Buy(player, wrongPrice, kNowMs, resp));

	auto wrongBound = BuildBuyRequestFromShelf(202, 1, 3002);
	wrongBound.mutable_expected_items(0)->set_bound(false);
	EXPECT_EQ(kSpiritShopPriceChanged, PlayerSpiritShopSystem::Buy(player, wrongBound, kNowMs, resp));

	EXPECT_EQ(1000u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
	EXPECT_EQ(0u, Inventory(player).GetTotalItemCount(3408));
}

TEST_F(PlayerSpiritShopTest, IdempotentReplaySurvivesZeroBalanceAndFullQuota)
{
	const auto exact = NewPlayer(kPlayerGuid + 2, /*diamond=*/300, /*bindDiamond=*/0);
	auto req = BuildBuyRequestFromShelf(103, 1, 4001);
	::BuySpiritShopGoodsResponse firstResp;
	ASSERT_EQ(kSuccess, PlayerSpiritShopSystem::Buy(exact, req, kNowMs, firstResp));
	EXPECT_FALSE(firstResp.replayed());
	EXPECT_EQ(0u, CurrencySystem::GetBalance(exact, kCurrencyDiamond));
	EXPECT_EQ(300u, firstResp.info().limit_groups(0).used_weight());

	// 余额归零且 300/300 已满后重发同一幂等键:返回首次已成交且不重复发物。
	::BuySpiritShopGoodsResponse replayResp;
	ASSERT_EQ(kSuccess, PlayerSpiritShopSystem::Buy(exact, req, kNowMs, replayResp));
	EXPECT_TRUE(replayResp.replayed());
	EXPECT_EQ(150u, Inventory(exact).GetTotalItemCount(3001));
	EXPECT_EQ(150u, Inventory(exact).GetTotalItemCount(3002));

	// 同键换商品拒绝;键为 0 拒绝。
	auto mismatch = BuildBuyRequestFromShelf(201, 1, 4001);
	EXPECT_EQ(kInvalidParameter, PlayerSpiritShopSystem::Buy(exact, mismatch, kNowMs, replayResp));
	auto zeroKey = BuildBuyRequestFromShelf(201, 1, 0);
	EXPECT_EQ(kInvalidParameter, PlayerSpiritShopSystem::Buy(exact, zeroKey, kNowMs, replayResp));
}

TEST_F(PlayerSpiritShopTest, BagFullAbortsTwoItemBundleWithZeroSideEffects)
{
	// 将背包容量收紧至仅剩 1 格空位:101 需要 2 格(3001 + 3002),必须整批拒单零扣费。
	auto &inv = Inventory(player);
	inv.SetCapacityForRestore(inv.OccupiedGridCount() + 1);

	::BuySpiritShopGoodsResponse resp;
	EXPECT_EQ(kSpiritShopBagFull,
			  PlayerSpiritShopSystem::Buy(player, BuildBuyRequestFromShelf(101, 1, 5001), kNowMs, resp));
	EXPECT_EQ(1000u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
	EXPECT_EQ(0u, inv.GetTotalItemCount(3001));
	EXPECT_EQ(0u, inv.GetTotalItemCount(3002));
	EXPECT_EQ(0u, tlsEcs.actorRegistry.get<PlayerSpiritShopComp>(player).shop_revision());
}

TEST_F(PlayerSpiritShopTest, GainBlockBattleFrozenAndMissingCompAreFailClosed)
{
	::BuySpiritShopGoodsResponse resp;

	// 物品封禁
	GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, 3405);
	EXPECT_EQ(kSpiritShopItemBlocked,
			  PlayerSpiritShopSystem::Buy(player, BuildBuyRequestFromShelf(201, 1, 6001), kNowMs, resp));
	GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, 3405);

	// 战斗中
	tlsEcs.actorRegistry.emplace<InBattleComp>(player);
	EXPECT_EQ(kSpiritShopInBattle,
			  PlayerSpiritShopSystem::Buy(player, BuildBuyRequestFromShelf(201, 1, 6002), kNowMs, resp));
	tlsEcs.actorRegistry.remove<InBattleComp>(player);

	// 冻结
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	EXPECT_EQ(kSpiritShopFrozen,
			  PlayerSpiritShopSystem::Buy(player, BuildBuyRequestFromShelf(201, 1, 6003), kNowMs, resp));
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);

	// 缺少持久组件 fail-closed(查询与购买均拒绝)
	tlsEcs.actorRegistry.remove<PlayerSpiritShopComp>(player);
	::SpiritShopInfo info;
	EXPECT_EQ(kSpiritShopUnavailable, PlayerSpiritShopSystem::GetInfo(player, kNowMs, info));
	EXPECT_EQ(kSpiritShopUnavailable,
			  PlayerSpiritShopSystem::Buy(player, BuildBuyRequestFromShelf(201, 1, 6004), kNowMs, resp));
	EXPECT_EQ(1000u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
}

TEST_F(PlayerSpiritShopTest, V2FestivalWindowAutoOpensAndClosesAndLevelGates)
{
	SetPlayerLevel(player, 20);
	::SpiritShopInfo info;
	::BuySpiritShopGoodsResponse resp;

	auto containsGoods = [](const ::SpiritShopInfo &snapshot, uint32_t id) {
		for (const auto &g : snapshot.goods())
		{
			if (g.goods_id() == id) return true;
		}
		return false;
	};

	// 1. 开售前 1ms:货架自动隐藏 401/402,直接发包回 kSpiritShopNotInSaleWindow(37011)
	PlayerSpiritShopSystem::FillInfo(player, kSpringStartMs - 1, info);
	const int everydayCount = info.goods_size();
	ASSERT_GE(everydayCount, 9);
	EXPECT_FALSE(containsGoods(info, 401));
	EXPECT_FALSE(containsGoods(info, 402));

	// 预构造一份在售期报价单用于测窗口外与等级不足
	auto req401 = BuildBuyRequestFromShelfAt(player, 401, 1, 7001, kSpringStartMs, 0);
	ASSERT_EQ(4, req401.expected_items_size());
	ASSERT_EQ(180u, req401.expected_unit_price());
	EXPECT_EQ(kSpiritShopNotInSaleWindow,
			  PlayerSpiritShopSystem::Buy(player, req401, kSpringStartMs - 1, resp));

	// 2. 开售后但等级 < 10:货架自动过滤 401/402,直接发包回 kSpiritShopLevelNotMet(37012)
	SetPlayerLevel(player, 9);
	PlayerSpiritShopSystem::FillInfo(player, kSpringStartMs, info);
	EXPECT_EQ(everydayCount, info.goods_size());
	EXPECT_FALSE(containsGoods(info, 401));
	EXPECT_FALSE(containsGoods(info, 402));
	EXPECT_EQ(kSpiritShopLevelNotMet,
			  PlayerSpiritShopSystem::Buy(player, req401, kSpringStartMs, resp));

	// 缺 LevelComp 严格 fail-closed(隐藏 + 回 kSpiritShopUnavailable,绝不按 1 级放行)
	tlsEcs.actorRegistry.remove<LevelComp>(player);
	PlayerSpiritShopSystem::FillInfo(player, kSpringStartMs, info);
	EXPECT_EQ(everydayCount, info.goods_size());
	EXPECT_FALSE(containsGoods(info, 401));
	EXPECT_EQ(kSpiritShopUnavailable,
			  PlayerSpiritShopSystem::Buy(player, req401, kSpringStartMs, resp));

	// 3. 升至 10 级且进入活动期:401/402 自动出现在分页 4(比常态多 2 项),并携带原价划线与时间窗
	SetPlayerLevel(player, 10);
	PlayerSpiritShopSystem::FillInfo(player, kSpringStartMs, info);
	ASSERT_EQ(everydayCount + 2, info.goods_size());
	bool saw401 = false;
	bool saw402 = false;
	for (const auto &g : info.goods())
	{
		if (g.goods_id() == 401)
		{
			saw401 = true;
			EXPECT_EQ(4u, g.tab_id());
			EXPECT_EQ(180u, g.unit_price());
			EXPECT_EQ(300u, g.original_price());
			EXPECT_EQ(spirit_shop_rules::kGrantModeAll, g.grant_mode());
			EXPECT_EQ(10u, g.min_level());
			EXPECT_EQ(kSpringStartMs, g.start_time_ms());
			EXPECT_EQ(kSpringEndMs, g.end_time_ms());
			EXPECT_EQ(4, g.items_size());
		}
		else if (g.goods_id() == 402)
		{
			saw402 = true;
			EXPECT_EQ(4u, g.tab_id());
			EXPECT_EQ(120u, g.unit_price());
			EXPECT_EQ(180u, g.original_price());
			EXPECT_EQ(spirit_shop_rules::kGrantModeChooseOne, g.grant_mode());
			EXPECT_EQ(4, g.items_size());
		}
	}
	EXPECT_TRUE(saw401);
	EXPECT_TRUE(saw402);

	// 4. 4 件大礼包背包仅剩 3 格:整批原子拒回 kSpiritShopBagFull,零扣费、零发物
	auto &inv = Inventory(player);
	const uint32_t origCap = inv.Capacity();
	inv.SetCapacityForRestore(inv.OccupiedGridCount() + 3);
	EXPECT_EQ(kSpiritShopBagFull, PlayerSpiritShopSystem::Buy(player, req401, kSpringStartMs, resp));
	EXPECT_EQ(1000u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
	EXPECT_EQ(0u, inv.GetTotalItemCount(3001));
	EXPECT_EQ(0u, inv.GetTotalItemCount(3002));
	EXPECT_EQ(0u, inv.GetTotalItemCount(3405));
	EXPECT_EQ(0u, inv.GetTotalItemCount(3402));
	inv.SetCapacityForRestore(origCap);

	// 5. ALL 礼包传非 0 chosen_item_index 拒回 kSpiritShopInvalidChoice(37013)
	auto badChoice = req401;
	badChoice.set_client_request_id(7002);
	badChoice.set_chosen_item_index(1);
	EXPECT_EQ(kSpiritShopInvalidChoice, PlayerSpiritShopSystem::Buy(player, badChoice, kSpringStartMs, resp));

	// 6. 正常购入 401:按折后价 180(绝非划线原价 300)扣费,4 件绑定物全部入包;再买触发终身限购 1
	ASSERT_EQ(kSuccess, PlayerSpiritShopSystem::Buy(player, req401, kSpringStartMs, resp));
	EXPECT_FALSE(resp.replayed());
	EXPECT_EQ(820u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
	EXPECT_EQ(60u, inv.GetTotalItemCount(3001));
	EXPECT_EQ(60u, inv.GetTotalItemCount(3002));
	EXPECT_EQ(2u, inv.GetTotalItemCount(3405));
	EXPECT_EQ(1u, inv.GetTotalItemCount(3402));
	EXPECT_EQ(0u, BagService::RemovableTradableCount(player, inv, 3001));
	EXPECT_EQ(0u, BagService::RemovableTradableCount(player, inv, 3402));

	auto second401 = req401;
	second401.set_client_request_id(7003);
	EXPECT_EQ(kSpiritShopLimitReached, PlayerSpiritShopSystem::Buy(player, second401, kSpringStartMs, resp));

	// 7. 活动到期(nowMs == kSpringEndMs):货架自动下架回常态数量,新单拒回 37011;
	//    但旧已成交 client_request_id(7001) 幂等重放仍返回成功且不重复发物!
	PlayerSpiritShopSystem::FillInfo(player, kSpringEndMs, info);
	EXPECT_EQ(everydayCount, info.goods_size());
	EXPECT_FALSE(containsGoods(info, 401));
	EXPECT_FALSE(containsGoods(info, 402));
	auto afterClose = req401;
	afterClose.set_client_request_id(7004);
	EXPECT_EQ(kSpiritShopNotInSaleWindow, PlayerSpiritShopSystem::Buy(player, afterClose, kSpringEndMs, resp));

	::BuySpiritShopGoodsResponse replayResp;
	ASSERT_EQ(kSuccess, PlayerSpiritShopSystem::Buy(player, req401, kSpringEndMs, replayResp));
	EXPECT_TRUE(replayResp.replayed());
	EXPECT_EQ(60u, inv.GetTotalItemCount(3001));
	EXPECT_EQ(820u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
}

TEST_F(PlayerSpiritShopTest, V2ChooseOneBundleValidatesChoiceAndQuoteAndIdempotency)
{
	SetPlayerLevel(player, 20);
	::BuySpiritShopGoodsResponse resp;

	// 1. 越界下标(4)拒回 kSpiritShopInvalidChoice(37013)
	auto outOfRange = BuildBuyRequestFromShelfAt(player, 402, 1, 8001, kSpringStartMs, 0);
	outOfRange.set_chosen_item_index(4);
	EXPECT_EQ(kSpiritShopInvalidChoice, PlayerSpiritShopSystem::Buy(player, outOfRange, kSpringStartMs, resp));

	// 2. 下标 1(3412 技能特效卷)但报价单填了下标 0(3408 天工秘墨):报价防呆拒回 kSpiritShopPriceChanged(37002)
	auto mismatchQuote = BuildBuyRequestFromShelfAt(player, 402, 1, 8002, kSpringStartMs, 0);
	mismatchQuote.set_chosen_item_index(1);
	EXPECT_EQ(kSpiritShopPriceChanged, PlayerSpiritShopSystem::Buy(player, mismatchQuote, kSpringStartMs, resp));

	// 3. 封禁未选中的候选项 3405(下标 3):选择下标 1(3412)应正常买成,选择下标 3 则拒回 kSpiritShopItemBlocked
	GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, 3405);
	auto buyChoice1 = BuildBuyRequestFromShelfAt(player, 402, 1, 8003, kSpringStartMs, 1);
	ASSERT_EQ(1, buyChoice1.expected_items_size());
	ASSERT_EQ(3412u, buyChoice1.expected_items(0).item_config_id());
	ASSERT_EQ(kSuccess, PlayerSpiritShopSystem::Buy(player, buyChoice1, kSpringStartMs, resp));
	EXPECT_EQ(880u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
	EXPECT_EQ(1u, Inventory(player).GetTotalItemCount(3412));
	EXPECT_EQ(0u, Inventory(player).GetTotalItemCount(3408));
	EXPECT_EQ(0u, Inventory(player).GetTotalItemCount(3411));
	EXPECT_EQ(0u, Inventory(player).GetTotalItemCount(3405));

	auto buyChoice3 = BuildBuyRequestFromShelfAt(player, 402, 1, 8004, kSpringStartMs, 3);
	EXPECT_EQ(kSpiritShopItemBlocked, PlayerSpiritShopSystem::Buy(player, buyChoice3, kSpringStartMs, resp));
	GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, 3405);

	// 4. 解除封禁后买下标 3(3405 x3)成功,达终身限购 2/2,第 3 次买拒回 kSpiritShopLimitReached
	ASSERT_EQ(kSuccess, PlayerSpiritShopSystem::Buy(player, buyChoice3, kSpringStartMs, resp));
	EXPECT_EQ(760u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
	EXPECT_EQ(3u, Inventory(player).GetTotalItemCount(3405));

	auto thirdBuy = BuildBuyRequestFromShelfAt(player, 402, 1, 8005, kSpringStartMs, 0);
	EXPECT_EQ(kSpiritShopLimitReached, PlayerSpiritShopSystem::Buy(player, thirdBuy, kSpringStartMs, resp));

	// 5. 同 client_request_id(8003) 换成下标 2(3411):指纹不匹配拒回 kInvalidParameter;
	//    原样重放即使已过活动期仍返回 kSuccess + replayed=true 且绝不重复发 3412
	auto swapChoiceReplay = BuildBuyRequestFromShelfAt(player, 402, 1, 8003, kSpringStartMs, 2);
	EXPECT_EQ(kInvalidParameter, PlayerSpiritShopSystem::Buy(player, swapChoiceReplay, kSpringStartMs, resp));

	::BuySpiritShopGoodsResponse replayResp;
	ASSERT_EQ(kSuccess, PlayerSpiritShopSystem::Buy(player, buyChoice1, kSpringEndMs + 1000, replayResp));
	EXPECT_TRUE(replayResp.replayed());
	EXPECT_EQ(1u, Inventory(player).GetTotalItemCount(3412));
	EXPECT_EQ(760u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
}

TEST_F(PlayerSpiritShopTest, OfficialUnsealAndInheritTalismansAreShelvedAsBoundGoods)
{
	::BuySpiritShopGoodsResponse resp;
	auto buyUnseal = BuildBuyRequestFromShelf(203, 1, 9101);
	EXPECT_EQ(60u, buyUnseal.expected_unit_price());
	ASSERT_EQ(kSuccess, PlayerSpiritShopSystem::Buy(player, buyUnseal, kNowMs, resp));
	EXPECT_EQ(1u, Inventory(player).GetTotalItemCount(3409));
	EXPECT_EQ(0u, BagService::RemovableTradableCount(player, Inventory(player), 3409));

	auto buyInherit = BuildBuyRequestFromShelf(204, 1, 9102);
	EXPECT_EQ(120u, buyInherit.expected_unit_price());
	ASSERT_EQ(kSuccess, PlayerSpiritShopSystem::Buy(player, buyInherit, kNowMs, resp));
	EXPECT_EQ(1u, Inventory(player).GetTotalItemCount(3410));
	EXPECT_EQ(0u, BagService::RemovableTradableCount(player, Inventory(player), 3410));
}

} // namespace
