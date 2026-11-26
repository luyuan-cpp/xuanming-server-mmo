#include "player_spirit_shop.h"

#include <algorithm>
#include <limits>
#include <string>
#include <vector>

#include "muduo/base/Logging.h"

#include "engine/core/type_define/player_guid_comp.h"
#include "battle/system/player_battle.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h"
#include "player/system/client_request_ledger.h"
#include "player/system/player_feature_snapshot.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "player/system/spirit_shop_rules.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/player_spirit_shop_comp.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "proto/scene/player_spirit_shop.pb.h"
#include "table/code/item_table.h"
#include "table/code/spiritshopgoods_table.h"
#include "table/code/spiritshoplimitgroup_table.h"
#include "table/proto/tip/asset_error_tip.pb.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/spirit_shop_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
	std::string PlayerLogId(entt::entity player)
	{
		const auto *guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
		return std::string("player_id=") + std::to_string(guid != nullptr ? guid->value : kInvalidGuid);
	}

	bool ResolveLimitGroup(uint32_t groupId, spirit_shop_rules::ResolvedLimitGroup &out, const char *&reason)
	{
		const auto [row, err] = SpiritShopLimitGroupTableManager::Instance().FindByIdSilent(groupId);
		if (row == nullptr)
		{
			reason = "limit group row not found";
			return false;
		}
		if (row->id() == 0)
		{
			reason = "limit group id is 0";
			return false;
		}
		if (!spirit_shop_rules::IsValidResetKind(row->reset_kind()))
		{
			reason = "limit group reset_kind is invalid";
			return false;
		}
		if (row->max_weight() == 0)
		{
			reason = "limit group max_weight is 0";
			return false;
		}
		out.groupId = row->id();
		out.name = row->name();
		out.resetKind = row->reset_kind();
		out.maxWeight = row->max_weight();
		return true;
	}

	bool ResolveGoodsRow(const SpiritShopGoodsTable &row, spirit_shop_rules::ResolvedGoods &out, const char *&reason)
	{
		if (row.id() == 0)
		{
			reason = "goods id is 0";
			return false;
		}
		if (row.enabled() == 0)
		{
			reason = "goods disabled";
			return false;
		}
		if (!IsKnownCurrencyType(row.currency_type()))
		{
			reason = "unknown currency_type";
			return false;
		}
		if (row.price() == 0 || row.price() > spirit_shop_rules::kMaxUnitPrice)
		{
			reason = "price not in 1..1000000000";
			return false;
		}
		if (row.max_buy_per_request() == 0 || row.max_buy_per_request() > spirit_shop_rules::kMaxBuyCountPerRequest)
		{
			reason = "max_buy_per_request not in 1..99";
			return false;
		}
		if (!spirit_shop_rules::IsValidResetKind(row.limit_reset_kind()))
		{
			reason = "limit_reset_kind is invalid";
			return false;
		}
		if (row.item_bound_1() > 1 || row.item_bound_2() > 1)
		{
			reason = "item_bound must be 0 or 1";
			return false;
		}
		const auto [item1, item1Err] = ItemTableManager::Instance().FindByIdSilent(row.item_id_1());
		if (item1 == nullptr || row.item_count_1() == 0)
		{
			reason = "item_id_1 missing or item_count_1 is 0";
			return false;
		}
		if (item1->max_stack_size() == 0 ||
			static_cast<uint64_t>(row.item_count_1()) * row.max_buy_per_request() > std::numeric_limits<uint32_t>::max())
		{
			reason = "item_1 stack size invalid or count overflows";
			return false;
		}

		out = spirit_shop_rules::ResolvedGoods{};
		out.goodsId = row.id();
		out.tabId = row.tab_id();
		out.sortOrder = row.sort_order();
		out.name = row.name().empty() ? item1->name() : row.name();
		out.description = row.description().empty() ? item1->description() : row.description();
		out.iconKey = row.icon_key().empty() ? item1->icon_key() : row.icon_key();
		out.badgeText = row.badge_text();
		out.currencyType = row.currency_type();
		out.unitPrice = row.price();
		out.maxBuyPerRequest = row.max_buy_per_request();
		out.limitResetKind = row.limit_reset_kind();
		out.limitCount = row.limit_count();
		out.items.push_back(spirit_shop_rules::ResolvedItem{row.item_id_1(), row.item_count_1(), row.item_bound_1() != 0});

		if (row.item_id_2() != 0)
		{
			const auto [item2, item2Err] = ItemTableManager::Instance().FindByIdSilent(row.item_id_2());
			if (item2 == nullptr || row.item_count_2() == 0)
			{
				reason = "item_id_2 missing or item_count_2 is 0";
				return false;
			}
			if (row.item_id_2() == row.item_id_1() && (row.item_bound_2() != 0) == (row.item_bound_1() != 0))
			{
				reason = "item_2 duplicates (item_id_1, item_bound_1)";
				return false;
			}
			if (item2->max_stack_size() == 0 ||
				static_cast<uint64_t>(row.item_count_2()) * row.max_buy_per_request() > std::numeric_limits<uint32_t>::max())
			{
				reason = "item_2 stack size invalid or count overflows";
				return false;
			}
			out.items.push_back(spirit_shop_rules::ResolvedItem{row.item_id_2(), row.item_count_2(), row.item_bound_2() != 0});
		}

		if (row.limit_group_id() != 0)
		{
			spirit_shop_rules::ResolvedLimitGroup group;
			if (!ResolveLimitGroup(row.limit_group_id(), group, reason))
			{
				return false;
			}
			if (row.limit_group_weight() == 0 || row.limit_group_weight() > spirit_shop_rules::kMaxLimitGroupWeight)
			{
				reason = "limit_group_weight not in 1..1000000";
				return false;
			}
			out.limitGroupId = row.limit_group_id();
			out.limitGroupWeight = row.limit_group_weight();
		}
		return true;
	}

	std::string PurchaseExtraJson(const spirit_shop_rules::ResolvedGoods &goods, uint32_t count)
	{
		return std::string("{\"source\":\"spirit_shop\",\"goods_id\":") + std::to_string(goods.goodsId) +
			   ",\"count\":" + std::to_string(count) +
			   ",\"unit_price\":" + std::to_string(goods.unitPrice) +
			   ",\"currency_type\":" + std::to_string(goods.currencyType) + "}";
	}

	void FillSuccessReply(entt::entity player, uint64_t nowMs, ::BuySpiritShopGoodsResponse &response)
	{
		PlayerSpiritShopSystem::FillInfo(player, nowMs, *response.mutable_info());
		if (const auto snapshotErr = PlayerBagSystem::BuildSnapshot(player, kInventory, *response.mutable_bag());
			snapshotErr != kSuccess)
		{
			response.clear_bag();
			LOG_ERROR << "[SpiritShop] Purchase succeeded but bag snapshot failed; replying without bag: "
					  << PlayerLogId(player) << " err=" << snapshotErr;
		}
	}

	uint32_t MapBagError(uint32_t bagErr)
	{
		if (bagErr == kAssetBlocked)
		{
			return kSpiritShopItemBlocked;
		}
		if (bagErr == kBagAddItemBagFull || bagErr == kBagItemNotStacked)
		{
			return kSpiritShopBagFull;
		}
		if (bagErr == kSpiritShopCurrencyNotEnough || bagErr == kSpiritShopFrozen ||
			bagErr == kSpiritShopInBattle || bagErr == kSpiritShopUnavailable)
		{
			return bagErr;
		}
		return kSpiritShopUnavailable;
	}
} // namespace

bool PlayerSpiritShopSystem::ValidateTables()
{
	bool allValid = true;
	uint32_t validGroups = 0;
	for (const auto &groupRow : SpiritShopLimitGroupTableManager::Instance().FindAll().data())
	{
		spirit_shop_rules::ResolvedLimitGroup group;
		const char *reason = "";
		if (!ResolveLimitGroup(groupRow.id(), group, reason))
		{
			LOG_ERROR << "[SpiritShop] Invalid SpiritShopLimitGroup row: id=" << groupRow.id() << " reason=" << reason;
			allValid = false;
			continue;
		}
		++validGroups;
	}

	uint32_t sellableGoods = 0;
	for (const auto &goodsRow : SpiritShopGoodsTableManager::Instance().FindAll().data())
	{
		if (goodsRow.enabled() == 0)
		{
			continue;
		}
		spirit_shop_rules::ResolvedGoods goods;
		const char *reason = "";
		if (!ResolveGoodsRow(goodsRow, goods, reason))
		{
			LOG_ERROR << "[SpiritShop] Invalid enabled SpiritShopGoods row: id=" << goodsRow.id() << " reason=" << reason;
			allValid = false;
			continue;
		}
		++sellableGoods;
	}
	LOG_INFO << "[SpiritShop] Table check finished: valid_groups=" << validGroups
			 << " sellable_goods=" << sellableGoods << " ok=" << (allValid ? "true" : "false");
	return allValid;
}

uint32_t PlayerSpiritShopSystem::GetInfo(entt::entity player, uint64_t nowMs, ::SpiritShopInfo &out)
{
	out.Clear();
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return kEntityIsNull;
	}
	const auto *comp = tlsEcs.actorRegistry.try_get<PlayerSpiritShopComp>(player);
	if (comp == nullptr || !spirit_shop_rules::ValidateComp(*comp).empty())
	{
		return kSpiritShopUnavailable;
	}
	FillInfo(player, nowMs, out);
	return kSuccess;
}

void PlayerSpiritShopSystem::FillInfo(entt::entity player, uint64_t nowMs, ::SpiritShopInfo &out)
{
	out.Clear();
	out.set_server_time_ms(nowMs);
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}

	uint64_t weekStartMs = 0;
	uint64_t weekNextMs = 0;
	const bool weekOk = spirit_shop_rules::ResolveWeek(nowMs, weekStartMs, weekNextMs);

	const auto *comp = tlsEcs.actorRegistry.try_get<PlayerSpiritShopComp>(player);
	out.set_shop_revision(comp != nullptr ? comp->shop_revision() : 0);
	out.set_diamond_balance(CurrencySystem::GetBalance(player, kCurrencyDiamond));
	out.set_bind_diamond_balance(CurrencySystem::GetBalance(player, kCurrencyBindDiamond));

	std::vector<spirit_shop_rules::ResolvedLimitGroup> groups;
	for (const auto &row : SpiritShopLimitGroupTableManager::Instance().FindAll().data())
	{
		spirit_shop_rules::ResolvedLimitGroup group;
		const char *reason = "";
		if (ResolveLimitGroup(row.id(), group, reason))
		{
			groups.push_back(std::move(group));
		}
	}
	std::sort(groups.begin(), groups.end(), [](const auto &a, const auto &b) { return a.groupId < b.groupId; });
	for (const auto &group : groups)
	{
		auto *view = out.add_limit_groups();
		view->set_group_id(group.groupId);
		view->set_name(group.name);
		view->set_reset_kind(group.resetKind);
		view->set_max_weight(group.maxWeight);
		const uint32_t used = (comp != nullptr && (group.resetKind == spirit_shop_rules::kResetLifetime || weekOk))
								  ? spirit_shop_rules::UsedGroupWeight(*comp, group.groupId, group.resetKind, weekStartMs)
								  : 0;
		view->set_used_weight(used);
		view->set_next_reset_ms(group.resetKind == spirit_shop_rules::kResetWeekly && weekOk ? weekNextMs : 0);
	}

	std::vector<spirit_shop_rules::ResolvedGoods> goodsList;
	for (const auto &row : SpiritShopGoodsTableManager::Instance().FindAll().data())
	{
		if (row.enabled() == 0)
		{
			continue;
		}
		spirit_shop_rules::ResolvedGoods resolved;
		const char *reason = "";
		if (ResolveGoodsRow(row, resolved, reason))
		{
			goodsList.push_back(std::move(resolved));
		}
	}
	std::sort(goodsList.begin(), goodsList.end(), [](const auto &a, const auto &b) {
		if (a.tabId != b.tabId)
		{
			return a.tabId < b.tabId;
		}
		if (a.sortOrder != b.sortOrder)
		{
			return a.sortOrder < b.sortOrder;
		}
		return a.goodsId < b.goodsId;
	});

	for (const auto &goods : goodsList)
	{
		auto *view = out.add_goods();
		view->set_goods_id(goods.goodsId);
		view->set_tab_id(goods.tabId);
		view->set_sort_order(goods.sortOrder);
		view->set_name(goods.name);
		view->set_description(goods.description);
		view->set_icon_key(goods.iconKey);
		view->set_badge_text(goods.badgeText);
		view->set_currency_type(goods.currencyType);
		view->set_unit_price(goods.unitPrice);
		view->set_max_buy_per_request(goods.maxBuyPerRequest);
		for (const auto &item : goods.items)
		{
			auto *entry = view->add_items();
			entry->set_item_config_id(item.itemConfigId);
			entry->set_item_count(item.itemCount);
			entry->set_bound(item.bound);
		}
		view->set_limit_reset_kind(goods.limitResetKind);
		view->set_limit_count(goods.limitCount);
		const uint32_t bought =
			(comp != nullptr && (goods.limitResetKind == spirit_shop_rules::kResetLifetime || weekOk))
				? spirit_shop_rules::BoughtCount(*comp, goods.goodsId, goods.limitResetKind, weekStartMs)
				: 0;
		view->set_bought_count(bought);
		view->set_limit_group_id(goods.limitGroupId);
		view->set_limit_group_weight(goods.limitGroupWeight);
		view->set_next_reset_ms(
			goods.limitCount > 0 && goods.limitResetKind == spirit_shop_rules::kResetWeekly && weekOk ? weekNextMs : 0);
	}
}

uint32_t PlayerSpiritShopSystem::Buy(entt::entity player, const ::BuySpiritShopGoodsRequest &request, uint64_t nowMs,
									 ::BuySpiritShopGoodsResponse &response)
{
	response.Clear();
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return kEntityIsNull;
	}
	if (request.GetReflection()->GetUnknownFields(request).field_count() != 0)
	{
		return kInvalidParameter;
	}
	for (const auto &expItem : request.expected_items())
	{
		if (expItem.GetReflection()->GetUnknownFields(expItem).field_count() != 0)
		{
			return kInvalidParameter;
		}
	}

	// 1. 幂等查重排在一切状态预检之前:新接口强制非 0 client_request_id。
	const uint64_t clientRequestId = request.client_request_id();
	if (clientRequestId == 0)
	{
		return kInvalidParameter;
	}
	std::string fingerprint = ClientRequestFingerprint(request);
	ClientRequestResult first;
	switch (client_request_dedup::Check(player, clientRequestId, ClientRequestKind::kBuySpiritShopGoods,
										fingerprint, first))
	{
	case ClientRequestVerdict::kReplay:
		response.set_replayed(true);
		FillSuccessReply(player, nowMs, response);
		return kSuccess;
	case ClientRequestVerdict::kMismatch:
		return kInvalidParameter;
	case ClientRequestVerdict::kUnavailable:
		return kServiceUnavailable;
	default:
		break;
	}

	// 2. 可写闸与战斗闸。
	if (!player_mutability::IsPlayerMutable(player))
	{
		return kSpiritShopFrozen;
	}
	if (PlayerBattleSystem::IsInBattle(player))
	{
		return kSpiritShopInBattle;
	}

	// 3. 查表、上架态、份数与报价防呆。
	if (request.goods_id() == 0)
	{
		return kSpiritShopGoodsOffShelf;
	}
	const auto [goodsRow, goodsRowErr] = SpiritShopGoodsTableManager::Instance().FindByIdSilent(request.goods_id());
	if (goodsRow == nullptr || goodsRow->enabled() == 0)
	{
		return kSpiritShopGoodsOffShelf;
	}
	spirit_shop_rules::ResolvedGoods goods;
	const char *reason = "";
	if (!ResolveGoodsRow(*goodsRow, goods, reason))
	{
		LOG_ERROR << "[SpiritShop] Purchase rejected: goods row invalid " << PlayerLogId(player)
				  << " goods_id=" << request.goods_id() << " reason=" << reason;
		return kSpiritShopUnavailable;
	}
	if (request.count() == 0 || request.count() > goods.maxBuyPerRequest)
	{
		return kSpiritShopInvalidCount;
	}
	if (request.expected_currency_type() != goods.currencyType ||
		request.expected_unit_price() != goods.unitPrice ||
		!spirit_shop_rules::ExpectedItemsMatch(request, goods.items))
	{
		return kSpiritShopPriceChanged;
	}

	// 4. 时钟周起点、持久限购组件与单品/共享组限购预检。
	uint64_t weekStartMs = 0;
	uint64_t weekNextMs = 0;
	if (!spirit_shop_rules::ResolveWeek(nowMs, weekStartMs, weekNextMs))
	{
		LOG_ERROR << "[SpiritShop] Purchase rejected: invalid wall clock now_ms=" << nowMs << " " << PlayerLogId(player);
		return kSpiritShopUnavailable;
	}
	auto *shopComp = tlsEcs.actorRegistry.try_get<PlayerSpiritShopComp>(player);
	if (shopComp == nullptr)
	{
		LOG_ERROR << "[SpiritShop] Purchase rejected: PlayerSpiritShopComp missing (load invariant broken) "
				  << PlayerLogId(player);
		return kSpiritShopUnavailable;
	}
	if (const std::string compErr = spirit_shop_rules::ValidateComp(*shopComp); !compErr.empty())
	{
		LOG_ERROR << "[SpiritShop] Purchase rejected: PlayerSpiritShopComp invalid (" << compErr << ") "
				  << PlayerLogId(player);
		return kSpiritShopUnavailable;
	}
	if (goods.limitCount > 0)
	{
		const uint32_t bought =
			spirit_shop_rules::BoughtCount(*shopComp, goods.goodsId, goods.limitResetKind, weekStartMs);
		if (spirit_shop_rules::WouldExceedLimit(bought, request.count(), goods.limitCount))
		{
			return kSpiritShopLimitReached;
		}
	}

	uint32_t addGroupWeight = 0;
	if (goods.limitGroupId > 0)
	{
		spirit_shop_rules::ResolvedLimitGroup group;
		if (!ResolveLimitGroup(goods.limitGroupId, group, reason))
		{
			LOG_ERROR << "[SpiritShop] Purchase rejected: limit group invalid " << PlayerLogId(player)
					  << " group_id=" << goods.limitGroupId << " reason=" << reason;
			return kSpiritShopUnavailable;
		}
		if (!spirit_shop_rules::TotalGroupWeight(goods.limitGroupWeight, request.count(), addGroupWeight))
		{
			return kSpiritShopInvalidCount;
		}
		const uint32_t usedWeight =
			spirit_shop_rules::UsedGroupWeight(*shopComp, group.groupId, group.resetKind, weekStartMs);
		if (spirit_shop_rules::WouldExceedLimit(usedWeight, addGroupWeight, group.maxWeight))
		{
			return kSpiritShopGroupLimitReached;
		}
	}

	// 5. 总价、物品封禁、余额预检与绑定发物清单构造。
	int64_t totalCost = 0;
	if (!spirit_shop_rules::TotalCost(goods.unitPrice, request.count(), totalCost))
	{
		return kSpiritShopInvalidCount;
	}
	for (const auto &item : goods.items)
	{
		if (GainBlockService::IsItemGainBlocked(player, item.itemConfigId))
		{
			return kSpiritShopItemBlocked;
		}
	}
	const auto currencyType = static_cast<CurrencyType>(goods.currencyType);
	if (!CurrencySystem::CanAfford(player, currencyType, totalCost))
	{
		return kSpiritShopCurrencyNotEnough;
	}

	auto *ledger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
	if (ledger == nullptr || tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player) ||
		ledger->next_seq() == std::numeric_limits<uint64_t>::max())
	{
		return kServiceUnavailable;
	}

	// 预构造候选限购与幂等账本,确保进入 ExchangeInventoryExact::beforeCommit 后不会因数据校验失败。
	PlayerSpiritShopComp candidateShop = *shopComp;
	if (!spirit_shop_rules::RecordPurchase(candidateShop, goods.goodsId, request.count(), goods.limitCount,
										   goods.limitGroupId, addGroupWeight, weekStartMs) ||
		!spirit_shop_rules::ValidateComp(candidateShop).empty())
	{
		return kSpiritShopUnavailable;
	}
	ClientRequestLedgerComp candidateLedger = *ledger;
	if (!client_request_ledger::Remember(candidateLedger, clientRequestId, ClientRequestKind::kBuySpiritShopGoods,
										 fingerprint, ClientRequestResult{}) ||
		!client_request_ledger::Validate(candidateLedger).empty())
	{
		return kServiceUnavailable;
	}

	std::vector<InitItemParam> gained;
	gained.reserve(goods.items.size());
	for (const auto &item : goods.items)
	{
		const uint64_t totalItemCount = static_cast<uint64_t>(item.itemCount) * request.count();
		if (totalItemCount == 0 || totalItemCount > std::numeric_limits<uint32_t>::max())
		{
			return kSpiritShopInvalidCount;
		}
		InitItemParam param;
		param.itemPBComp.set_config_id(item.itemConfigId);
		param.itemPBComp.set_size(static_cast<uint32_t>(totalItemCount));
		param.itemPBComp.set_item_id(kInvalidGuid);
		param.itemPBComp.set_bound(item.bound);
		gained.push_back(std::move(param));
	}

	const std::string beforeShopBytes = shopComp->SerializeAsString();
	const std::string beforeLedgerBytes = ledger->SerializeAsString();
	std::vector<GuidVector> written;

	// 6. 原子交换:ExchangeInventoryExact 先在候选背包完成完整分堆空间规划、禁发复核与发号;
	//    beforeCommit 内先扣灵石再 Swap 限购与幂等账本,任一失败则整批放弃(零扣钱、零发物、零占名额)。
	const uint32_t exchangeErr = BagService::ExchangeInventoryExact(
		player, {}, gained, {}, clientRequestId, PurchaseExtraJson(goods, request.count()), written,
		[&](const std::vector<GuidVector> &) -> uint32_t {
			if (!player_mutability::IsPlayerMutable(player))
			{
				return kSpiritShopFrozen;
			}
			if (PlayerBattleSystem::IsInBattle(player))
			{
				return kSpiritShopInBattle;
			}
			auto *curShop = tlsEcs.actorRegistry.try_get<PlayerSpiritShopComp>(player);
			auto *curLedger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
			if (curShop == nullptr || curLedger == nullptr ||
				tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player) ||
				curShop->SerializeAsString() != beforeShopBytes ||
				curLedger->SerializeAsString() != beforeLedgerBytes)
			{
				return kSpiritShopUnavailable;
			}
			if (!CurrencySystem::CanAfford(player, currencyType, totalCost))
			{
				return kSpiritShopCurrencyNotEnough;
			}
			if (const auto deductErr =
					CurrencySystem::DeductCurrency(player, currencyType, totalCost, TX_SPIRIT_SHOP_BUY, clientRequestId);
				deductErr != kSuccess)
			{
				LOG_ERROR << "[SpiritShop] Purchase rejected: currency deduction failed in beforeCommit (nothing granted) "
						  << PlayerLogId(player) << " goods_id=" << goods.goodsId << " cost=" << totalCost
						  << " currency_type=" << goods.currencyType << " err=" << deductErr;
				return kSpiritShopUnavailable;
			}
			curShop->Swap(&candidateShop);
			curLedger->Swap(&candidateLedger);
			return kSuccess;
		});

	if (exchangeErr != kSuccess)
	{
		return MapBagError(exchangeErr);
	}

	// 7. 关键进度直提存盘(1 秒节流,WALLET 与 BAG 同事务提交)。
	PlayerLifecycleSystem::RequestKeyProgressSave(player, "spirit_shop_buy");

	LOG_INFO << "[SpiritShop] metric=spirit_shop_bought " << PlayerLogId(player)
			 << " goods_id=" << goods.goodsId << " count=" << request.count()
			 << " cost=" << totalCost << " currency_type=" << goods.currencyType
			 << " group_id=" << goods.limitGroupId << " group_weight_added=" << addGroupWeight
			 << " client_request_id=" << clientRequestId;

	FillSuccessReply(player, nowMs, response);
	return kSuccess;
}
