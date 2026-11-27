#include "player_jianghu.h"

#include <limits>
#include <string>
#include <vector>

#include "muduo/base/Logging.h"

#include "battle/system/player_battle.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "player/system/client_request_ledger.h"
#include "player/system/player_feature_snapshot.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "player/system/role_admission_system.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/player_jianghu_comp.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"
#include "proto/match/match_service.pb.h"
#include "proto/scene/player_jianghu.pb.h"
#include "proto/scene/scene.pb.h"
#include "table/code/jianghurule_table.h"
#include "table/proto/tip/asset_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/jianghu_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
constexpr uint32_t kJianghuRuleRowId = 1;
constexpr uint32_t kDefaultGoldWritItemId = 3415;
constexpr uint32_t kDefaultClearItemId = 3416;
constexpr uint64_t kDefaultSilverWritCost = 500;
constexpr uint64_t kDefaultSilverClearCost = 1000;
constexpr uint32_t kDefaultDefeatMarkDurationSec = 1800;
constexpr uint32_t kDefaultClearBadgeDurationSec = 1800;
constexpr int kMaxEligibilityPlayersPerCall = 10;

struct ResolvedJianghuRule
{
	bool found{false};
	bool enabled{false};
	uint64_t silverWritCost{kDefaultSilverWritCost};
	uint32_t goldWritItemId{kDefaultGoldWritItemId};
	uint32_t clearItemId{kDefaultClearItemId};
	uint64_t silverClearCost{kDefaultSilverClearCost};
	uint32_t defeatMarkDurationSec{kDefaultDefeatMarkDurationSec};
	uint32_t clearBadgeDurationSec{kDefaultClearBadgeDurationSec};
};

ResolvedJianghuRule LoadRule()
{
	ResolvedJianghuRule out;
	const auto [row, err] = JianghuRuleTableManager::Instance().FindByIdSilent(kJianghuRuleRowId);
	if (row == nullptr)
	{
		return out;
	}
	out.found = true;
	out.enabled = row->enabled() != 0;
	out.silverWritCost = row->silver_writ_cost();
	out.goldWritItemId = row->gold_writ_item_id();
	out.clearItemId = row->clear_item_id();
	out.silverClearCost = row->silver_clear_cost();
	out.defeatMarkDurationSec = row->defeat_mark_duration_sec();
	out.clearBadgeDurationSec = row->clear_badge_duration_sec();
	return out;
}

uint64_t CurrentOwnerRevision(entt::entity player)
{
	if (const auto *admission = tlsEcs.actorRegistry.try_get<PlayerRoleAdmissionComp>(player);
		admission != nullptr && admission->ownerRevision > 0)
	{
		return admission->ownerRevision;
	}
	const uint64_t playerId = tlsEcs.GetPlayerId(player);
	if (playerId != 0)
	{
		if (const auto confirmed = RoleAdmissionSystem::ConfirmedIdentity(playerId);
			confirmed && confirmed->owner_revision() > 0)
		{
			return confirmed->owner_revision();
		}
	}
	return 0;
}

bool OwnerRevisionMismatched(const PlayerJianghuComp &comp, uint64_t currentOwnerRev)
{
	return currentOwnerRev != 0 && comp.owner_revision() != 0 && comp.owner_revision() != currentOwnerRev;
}

void ClearDefeatMarkFields(PlayerJianghuComp &comp)
{
	comp.set_defeat_mark_expires_at_ms(0);
	comp.set_defeat_mark_winner_id(0);
	comp.clear_defeat_mark_winner_name();
	comp.set_defeat_mark_style_id(0);
	comp.set_defeat_mark_is_guild_name(false);
	comp.set_defeat_mark_battle_id(0);
}

void ClearBadgeFields(PlayerJianghuComp &comp)
{
	comp.set_clear_badge_expires_at_ms(0);
	comp.set_clear_badge_style_id(0);
}

// 待退标记的三个字段总是一起写、一起清(预扣时写,退还 / 结算消耗 / 过户时清)。
void ClearPendingSilverFee(PlayerJianghuComp &comp)
{
	comp.set_pending_silver_fee_battle_id(0);
	comp.set_pending_silver_fee_amount(0);
	comp.set_pending_silver_fee_review_after_ms(0);
}

void NormalizeOwnerRevision(PlayerJianghuComp &comp, uint64_t currentOwnerRev)
{
	if (currentOwnerRev == 0)
	{
		return;
	}
	if (comp.owner_revision() != 0 && comp.owner_revision() != currentOwnerRev)
	{
		// 官方角色过户换手后清空旧号主战败印记、洒脱徽记与未结银两痕迹，防止新号主替旧号主背仇。
		ClearDefeatMarkFields(comp);
		ClearBadgeFields(comp);
		ClearPendingSilverFee(comp);
	}
	comp.set_owner_revision(currentOwnerRev);
}

uint32_t RemovableInventoryItemCount(entt::entity player, uint32_t itemConfigId)
{
	if (itemConfigId == 0)
	{
		return 0;
	}
	const auto *bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
	if (bags == nullptr)
	{
		return 0;
	}
	const auto count = BagService::RemovableCount(player, bags->bags[kInventory], itemConfigId, 0);
	return count > std::numeric_limits<uint32_t>::max()
			   ? std::numeric_limits<uint32_t>::max()
			   : static_cast<uint32_t>(count);
}

void FillProfileView(entt::entity player, const PlayerJianghuComp &comp, const ResolvedJianghuRule &rule,
					 uint64_t nowMs, ::JianghuProfileView &out)
{
	out.Clear();
	out.set_server_time_ms(nowMs);
	out.set_duel_refuse_strangers(comp.duel_refuse_strangers());

	const bool ownerMismatch = OwnerRevisionMismatched(comp, CurrentOwnerRevision(player));
	if (!ownerMismatch && comp.defeat_mark_expires_at_ms() > nowMs)
	{
		out.set_has_active_defeat_mark(true);
		out.set_defeat_mark_expires_at_ms(comp.defeat_mark_expires_at_ms());
		out.set_defeat_mark_winner_id(comp.defeat_mark_winner_id());
		out.set_defeat_mark_winner_name(comp.defeat_mark_winner_name());
		out.set_defeat_mark_style_id(comp.defeat_mark_style_id());
		out.set_defeat_mark_is_guild_name(comp.defeat_mark_is_guild_name());
		out.set_defeat_mark_battle_id(comp.defeat_mark_battle_id());
	}
	if (!ownerMismatch && comp.clear_badge_expires_at_ms() > nowMs)
	{
		out.set_has_active_clear_badge(true);
		out.set_clear_badge_expires_at_ms(comp.clear_badge_expires_at_ms());
		out.set_clear_badge_style_id(comp.clear_badge_style_id());
	}
	out.set_silver_balance(CurrencySystem::GetBalance(player, kCurrencyGold));
	out.set_gold_writ_item_count(RemovableInventoryItemCount(player, rule.goldWritItemId));
	out.set_clear_charm_item_count(RemovableInventoryItemCount(player, rule.clearItemId));
	out.set_silver_writ_cost(rule.silverWritCost);
	out.set_silver_clear_cost(rule.silverClearCost);
}

void FillClearReplayResponse(entt::entity player, ::JianghuClearMarkMode mode, const ResolvedJianghuRule &rule,
							 uint64_t nowMs, ::ClearJianghuDefeatMarkResponse &response)
{
	if (const auto *comp = tlsEcs.actorRegistry.try_get<PlayerJianghuComp>(player); comp != nullptr)
	{
		FillProfileView(player, *comp, rule, nowMs, *response.mutable_profile());
	}
	if (mode == ::JIANGHU_CLEAR_MARK_MODE_ITEM_CHARM)
	{
		if (PlayerBagSystem::BuildSnapshot(player, kInventory, *response.mutable_bag()) != kSuccess)
		{
			response.clear_bag();
		}
	}
}
} // namespace

uint32_t PlayerJianghuSystem::GetProfile(entt::entity player, uint64_t nowMs, ::JianghuProfileView &out)
{
	out.Clear();
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return kEntityIsNull;
	}
	const auto *comp = tlsEcs.actorRegistry.try_get<PlayerJianghuComp>(player);
	if (comp == nullptr)
	{
		LOG_ERROR << "[Jianghu] GetProfile rejected: PlayerJianghuComp missing (load invariant broken) player_id="
				  << tlsEcs.GetPlayerId(player);
		return kServiceUnavailable;
	}
	const auto rule = LoadRule();
	FillProfileView(player, *comp, rule, nowMs, out);
	return kSuccess;
}

uint32_t PlayerJianghuSystem::SetRefuseStrangers(entt::entity player, bool refuseStrangers, uint64_t nowMs,
												 ::JianghuProfileView &out)
{
	out.Clear();
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return kEntityIsNull;
	}
	if (!player_mutability::IsPlayerMutable(player) || PlayerBattleSystem::IsInBattle(player))
	{
		return kFeatureUnavailable;
	}
	auto *comp = tlsEcs.actorRegistry.try_get<PlayerJianghuComp>(player);
	if (comp == nullptr)
	{
		LOG_ERROR << "[Jianghu] SetRefuseStrangers rejected: PlayerJianghuComp missing player_id="
				  << tlsEcs.GetPlayerId(player);
		return kServiceUnavailable;
	}
	NormalizeOwnerRevision(*comp, CurrentOwnerRevision(player));
	if (comp->duel_refuse_strangers() != refuseStrangers)
	{
		comp->set_duel_refuse_strangers(refuseStrangers);
		PlayerLifecycleSystem::RequestKeyProgressSave(player, "jianghu_refuse_strangers");
	}
	const auto rule = LoadRule();
	FillProfileView(player, *comp, rule, nowMs, out);
	return kSuccess;
}

uint32_t PlayerJianghuSystem::ClearDefeatMark(entt::entity player, const ::ClearJianghuDefeatMarkRequest &request,
											  uint64_t nowMs, ::ClearJianghuDefeatMarkResponse &response)
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
	const uint64_t clientRequestId = request.client_request_id();
	if (clientRequestId == 0)
	{
		return kInvalidParameter;
	}
	const auto mode = request.mode();
	if (mode != ::JIANGHU_CLEAR_MARK_MODE_SILVER && mode != ::JIANGHU_CLEAR_MARK_MODE_ITEM_CHARM)
	{
		return kInvalidParameter;
	}

	const auto rule = LoadRule();
	const std::string fingerprint = ClientRequestFingerprint(request);
	ClientRequestResult first;
	switch (client_request_dedup::Check(player, clientRequestId, ClientRequestKind::kClearJianghuDefeatMark,
										fingerprint, first))
	{
	case ClientRequestVerdict::kReplay:
		response.set_replayed(true);
		FillClearReplayResponse(player, mode, rule, nowMs, response);
		return kSuccess;
	case ClientRequestVerdict::kMismatch:
		return kInvalidParameter;
	case ClientRequestVerdict::kUnavailable:
		return kServiceUnavailable;
	default:
		break;
	}

	if (!player_mutability::IsPlayerMutable(player) || PlayerBattleSystem::IsInBattle(player))
	{
		return kFeatureUnavailable;
	}
	if (!rule.found || !rule.enabled)
	{
		return kJianghuDuelDisabled;
	}
	auto *comp = tlsEcs.actorRegistry.try_get<PlayerJianghuComp>(player);
	auto *ledger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
	if (comp == nullptr || ledger == nullptr ||
		tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player) ||
		ledger->next_seq() == std::numeric_limits<uint64_t>::max())
	{
		return kServiceUnavailable;
	}

	const uint64_t ownerRev = CurrentOwnerRevision(player);
	if (OwnerRevisionMismatched(*comp, ownerRev) || comp->defeat_mark_expires_at_ms() <= nowMs)
	{
		return kJianghuNoActiveDefeatMark;
	}

	PlayerJianghuComp candidateComp = *comp;
	NormalizeOwnerRevision(candidateComp, ownerRev);
	ClearDefeatMarkFields(candidateComp);
	if (mode == ::JIANGHU_CLEAR_MARK_MODE_ITEM_CHARM && rule.clearBadgeDurationSec > 0)
	{
		candidateComp.set_clear_badge_expires_at_ms(
			nowMs + static_cast<uint64_t>(rule.clearBadgeDurationSec) * 1000ULL);
		candidateComp.set_clear_badge_style_id(1);
	}

	ClientRequestLedgerComp candidateLedger = *ledger;
	if (!client_request_ledger::Remember(candidateLedger, clientRequestId, ClientRequestKind::kClearJianghuDefeatMark,
										 fingerprint, ClientRequestResult{}) ||
		!client_request_ledger::Validate(candidateLedger).empty())
	{
		return kServiceUnavailable;
	}

	if (mode == ::JIANGHU_CLEAR_MARK_MODE_SILVER)
	{
		if (rule.silverClearCost == 0 ||
			rule.silverClearCost > static_cast<uint64_t>(std::numeric_limits<int64_t>::max()))
		{
			return kServiceUnavailable;
		}
		const auto cost = static_cast<int64_t>(rule.silverClearCost);
		if (!CurrencySystem::CanAfford(player, kCurrencyGold, cost))
		{
			return kJianghuInsufficientWritOrSilver;
		}
		const uint32_t deductErr =
			CurrencySystem::DeductCurrency(player, kCurrencyGold, cost, TX_JIANGHU_CLEAR_DEFEAT_MARK, clientRequestId);
		if (deductErr != kSuccess)
		{
			return deductErr == kAssetCurrencyInsufficient ? kJianghuInsufficientWritOrSilver : kFeatureUnavailable;
		}
		comp->Swap(&candidateComp);
		ledger->Swap(&candidateLedger);
		PlayerLifecycleSystem::RequestKeyProgressSave(player, "jianghu_clear_defeat_mark_silver");
		FillProfileView(player, *comp, rule, nowMs, *response.mutable_profile());
		LOG_INFO << "[Jianghu] Cleared defeat mark with silver: player_id=" << tlsEcs.GetPlayerId(player)
				 << " cost=" << rule.silverClearCost << " client_request_id=" << clientRequestId;
		return kSuccess;
	}

	// JIANGHU_CLEAR_MARK_MODE_ITEM_CHARM: 消耗 1 张 Item 3416「清风洗尘符」，附赠 30 分钟洒脱徽记。
	if (rule.clearItemId == 0)
	{
		return kServiceUnavailable;
	}
	if (RemovableInventoryItemCount(player, rule.clearItemId) < 1)
	{
		return kJianghuInsufficientWritOrSilver;
	}
	const std::string beforeCompBytes = comp->SerializeAsString();
	const std::string beforeLedgerBytes = ledger->SerializeAsString();
	ItemCountMap consumed;
	consumed[rule.clearItemId] = 1;
	std::vector<GuidVector> written;
	const uint32_t exchangeErr = BagService::ExchangeInventoryExact(
		player, consumed, {}, {}, clientRequestId,
		"{\"source\":\"jianghu_clear_defeat_mark\"}", written,
		[&](const std::vector<GuidVector> &) -> uint32_t {
			if (!player_mutability::IsPlayerMutable(player) || PlayerBattleSystem::IsInBattle(player))
			{
				return kFeatureUnavailable;
			}
			auto *curComp = tlsEcs.actorRegistry.try_get<PlayerJianghuComp>(player);
			auto *curLedger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
			if (curComp == nullptr || curLedger == nullptr ||
				tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player) ||
				curComp->SerializeAsString() != beforeCompBytes ||
				curLedger->SerializeAsString() != beforeLedgerBytes)
			{
				return kServiceUnavailable;
			}
			curComp->Swap(&candidateComp);
			curLedger->Swap(&candidateLedger);
			return kSuccess;
		});
	if (exchangeErr != kSuccess)
	{
		if (exchangeErr == kFeatureUnavailable || exchangeErr == kServiceUnavailable)
		{
			return exchangeErr;
		}
		return kJianghuInsufficientWritOrSilver;
	}

	PlayerLifecycleSystem::RequestKeyProgressSave(player, "jianghu_clear_defeat_mark_charm");
	FillClearReplayResponse(player, mode, rule, nowMs, response);
	LOG_INFO << "[Jianghu] Cleared defeat mark with charm: player_id=" << tlsEcs.GetPlayerId(player)
			 << " item_id=" << rule.clearItemId << " client_request_id=" << clientRequestId;
	return kSuccess;
}

void PlayerJianghuSystem::CheckDuelEligibility(const ::CheckJianghuDuelEligibilityRequest &request,
											   ::CheckJianghuDuelEligibilityResponse &response)
{
	response.Clear();
	const auto rule = LoadRule();
	if (!rule.found || !rule.enabled)
	{
		response.mutable_error_message()->set_id(kJianghuDuelDisabled);
		return;
	}
	if (request.player_ids_size() > kMaxEligibilityPlayersPerCall)
	{
		response.mutable_error_message()->set_id(kInvalidParameter);
		return;
	}
	for (const uint64_t playerId : request.player_ids())
	{
		auto *entry = response.add_players();
		entry->set_player_id(playerId);
		const auto entity = tlsEcs.GetPlayer(playerId);
		const bool online =
			tlsEcs.actorRegistry.valid(entity) && !tlsEcs.actorRegistry.any_of<UnregisterPlayer>(entity);
		entry->set_online_on_node(online);
		if (online)
		{
			if (const auto *comp = tlsEcs.actorRegistry.try_get<PlayerJianghuComp>(entity); comp != nullptr)
			{
				entry->set_duel_refuse_strangers(comp->duel_refuse_strangers());
			}
		}
	}

	if (request.payer_player_id() == 0)
	{
		return;
	}
	const auto payer = tlsEcs.GetPlayer(request.payer_player_id());
	if (!tlsEcs.actorRegistry.valid(payer) || tlsEcs.actorRegistry.any_of<UnregisterPlayer>(payer))
	{
		response.set_payer_has_sufficient_fee(false);
		return;
	}
	if (request.writ_tier() == static_cast<uint32_t>(match::JIANGHU_WRIT_TIER_SILVER))
	{
		const uint64_t silverCost = request.silver_cost() > 0 ? request.silver_cost() : rule.silverWritCost;
		const bool affordable =
			silverCost > 0 && silverCost <= static_cast<uint64_t>(std::numeric_limits<int64_t>::max()) &&
			CurrencySystem::CanAfford(payer, kCurrencyGold, static_cast<int64_t>(silverCost));
		response.set_payer_has_sufficient_fee(affordable);
		return;
	}
	if (request.writ_tier() == static_cast<uint32_t>(match::JIANGHU_WRIT_TIER_GOLD_ITEM))
	{
		const uint32_t itemConfigId = request.item_config_id() > 0 ? request.item_config_id() : rule.goldWritItemId;
		response.set_payer_has_sufficient_fee(itemConfigId != 0 && RemovableInventoryItemCount(payer, itemConfigId) >= 1);
		return;
	}
	response.set_payer_has_sufficient_fee(false);
}

uint32_t PlayerJianghuSystem::ValidateAndCollectPrepareFee(entt::entity player, uint64_t battleId,
														   const ::JianghuDuelPrepareFee &fee,
														   item_hold::HoldLines &heldLines,
														   uint64_t &outSilverToDeduct)
{
	outSilverToDeduct = 0;
	if (!tlsEcs.actorRegistry.valid(player) || battleId == 0)
	{
		return kInvalidParameter;
	}
	const auto rule = LoadRule();
	if (!rule.found || !rule.enabled)
	{
		return kJianghuDuelDisabled;
	}
	const auto *comp = tlsEcs.actorRegistry.try_get<PlayerJianghuComp>(player);
	if (comp == nullptr)
	{
		LOG_ERROR << "[Jianghu] PrepareBattle fee rejected: PlayerJianghuComp missing player_id="
				  << tlsEcs.GetPlayerId(player) << " battle_id=" << battleId;
		return kServiceUnavailable;
	}
	if (fee.writ_tier() == static_cast<uint32_t>(match::JIANGHU_WRIT_TIER_GOLD_ITEM))
	{
		const uint32_t itemConfigId = fee.item_config_id() != 0 ? fee.item_config_id() : rule.goldWritItemId;
		if (itemConfigId == 0 || itemConfigId != rule.goldWritItemId)
		{
			return kInvalidParameter;
		}
		if (RemovableInventoryItemCount(player, itemConfigId) < 1)
		{
			return kJianghuInsufficientWritOrSilver;
		}
		heldLines[itemConfigId] += 1;
		return kSuccess;
	}
	if (fee.writ_tier() == static_cast<uint32_t>(match::JIANGHU_WRIT_TIER_SILVER))
	{
		const uint64_t silverCost = fee.silver_cost() != 0 ? fee.silver_cost() : rule.silverWritCost;
		if (silverCost == 0 || silverCost != rule.silverWritCost ||
			silverCost > static_cast<uint64_t>(std::numeric_limits<int64_t>::max()))
		{
			return kInvalidParameter;
		}
		if (comp->pending_silver_fee_battle_id() == battleId)
		{
			return kSuccess;
		}
		if (!CurrencySystem::CanAfford(player, kCurrencyGold, static_cast<int64_t>(silverCost)))
		{
			return kJianghuInsufficientWritOrSilver;
		}
		outSilverToDeduct = silverCost;
		return kSuccess;
	}
	return kInvalidParameter;
}

uint32_t PlayerJianghuSystem::CommitPrepareSilverFee(entt::entity player, uint64_t battleId, uint64_t silverToDeduct)
{
	if (silverToDeduct == 0)
	{
		return kSuccess;
	}
	if (!tlsEcs.actorRegistry.valid(player) || battleId == 0 ||
		silverToDeduct > static_cast<uint64_t>(std::numeric_limits<int64_t>::max()))
	{
		return kInvalidParameter;
	}
	auto *comp = tlsEcs.actorRegistry.try_get<PlayerJianghuComp>(player);
	if (comp == nullptr)
	{
		return kServiceUnavailable;
	}
	if (comp->pending_silver_fee_battle_id() == battleId)
	{
		return kSuccess;
	}
	const uint32_t deductErr = CurrencySystem::DeductCurrency(
		player, kCurrencyGold, static_cast<int64_t>(silverToDeduct), TX_JIANGHU_DUEL_FEE, battleId);
	if (deductErr != kSuccess)
	{
		return deductErr == kAssetCurrencyInsufficient ? kJianghuInsufficientWritOrSilver : kFeatureUnavailable;
	}
	comp->set_pending_silver_fee_battle_id(battleId);
	comp->set_pending_silver_fee_amount(silverToDeduct);
	return kSuccess;
}

void PlayerJianghuSystem::OnBattlePrepareCancelled(entt::entity player, uint64_t battleId, bool skipSilverRefund)
{
	if (!tlsEcs.actorRegistry.valid(player) || battleId == 0)
	{
		return;
	}
	auto *comp = tlsEcs.actorRegistry.try_get<PlayerJianghuComp>(player);
	if (comp == nullptr || comp->pending_silver_fee_battle_id() != battleId)
	{
		return;
	}
	const uint64_t refundAmount = comp->pending_silver_fee_amount();
	comp->set_pending_silver_fee_battle_id(0);
	comp->set_pending_silver_fee_amount(0);
	if (skipSilverRefund || refundAmount == 0 ||
		refundAmount > static_cast<uint64_t>(std::numeric_limits<int64_t>::max()))
	{
		return;
	}
	const uint32_t creditErr = CurrencySystem::AddCurrency(
		player, kCurrencyGold, static_cast<int64_t>(refundAmount), TX_JIANGHU_DUEL_REFUND, battleId);
	if (creditErr != kSuccess)
	{
		LOG_ERROR << "[Jianghu] Failed to refund silver duel fee on CancelBattlePrepare: player_id="
				  << tlsEcs.GetPlayerId(player) << " battle_id=" << battleId << " amount=" << refundAmount
				  << " err=" << creditErr;
		return;
	}
	PlayerLifecycleSystem::RequestKeyProgressSave(player, "jianghu_duel_fee_refund");
	LOG_INFO << "[Jianghu] Refunded silver duel fee on CancelBattlePrepare: player_id=" << tlsEcs.GetPlayerId(player)
			 << " battle_id=" << battleId << " amount=" << refundAmount;
}

void PlayerJianghuSystem::OnBattleVoid(entt::entity player, uint64_t battleId)
{
	// 所有进入终局收件箱的作废局(battle VOID / 节点失联提前作废 / 到期作废)均由 scene 随收件箱存档原子退还普通银两并清零标记,
	// 保证离线重登也能 100% 退回且永不双退。
	OnBattlePrepareCancelled(player, battleId, /*skipSilverRefund=*/false);
}

uint32_t PlayerJianghuSystem::PrepareBattleSettlement(entt::entity player, const ::PlayerJianghuComp &current,
													  const ::BattleSettlementData &settlement, uint64_t nowMs,
													  ::PlayerJianghuComp &candidateOut, bool &changedOut,
													  uint32_t &goldWritToConsumeOut,
													  uint64_t *silverRefundOut)
{
	candidateOut = current;
	changedOut = false;
	goldWritToConsumeOut = 0;
	if (silverRefundOut != nullptr)
	{
		*silverRefundOut = 0;
	}

	if (settlement.match_mode() != match::MATCH_MODE_JIANGHU_DUEL)
	{
		return kSuccess;
	}
	const uint64_t playerId = tlsEcs.GetPlayerId(player);
	if (playerId == 0 || playerId != settlement.player_id())
	{
		return kInvalidParameter;
	}

	const std::string beforeBytes = current.SerializeAsString();
	const uint64_t ownerRev = CurrentOwnerRevision(player);
	NormalizeOwnerRevision(candidateOut, ownerRev);

	uint64_t pendingSilverForThisBattle = 0;
	if (candidateOut.pending_silver_fee_battle_id() == settlement.battle_id())
	{
		pendingSilverForThisBattle = candidateOut.pending_silver_fee_amount();
		candidateOut.set_pending_silver_fee_battle_id(0);
		candidateOut.set_pending_silver_fee_amount(0);
	}

	// 作废或 0 回合流局：不消费金帖、不挂战败印记，预留由调用方 ReleaseHold 释放，普通战书银两随本收件箱原子退还。
	if (settlement.outcome() == ::BATTLE_OUTCOME_VOID || settlement.total_rounds() == 0)
	{
		if (silverRefundOut != nullptr)
		{
			*silverRefundOut = pendingSilverForThisBattle;
		}
		changedOut = candidateOut.SerializeAsString() != beforeBytes;
		return kSuccess;
	}

	const auto rule = LoadRule();
	const uint32_t goldWritItemId = rule.found && rule.goldWritItemId != 0 ? rule.goldWritItemId : kDefaultGoldWritItemId;
	const uint32_t defeatDurationSec =
		rule.found && rule.defeatMarkDurationSec != 0 ? rule.defeatMarkDurationSec : kDefaultDefeatMarkDurationSec;

	if (settlement.has_jianghu_duel())
	{
		const auto &ctx = settlement.jianghu_duel();
		if (ctx.writ_tier() == static_cast<uint32_t>(match::JIANGHU_WRIT_TIER_GOLD_ITEM) &&
			ctx.challenger_captain_id() == playerId)
		{
			goldWritToConsumeOut = goldWritItemId;
		}

		const bool playerWon =
			(settlement.outcome() == ::BATTLE_OUTCOME_SIDE_A_WIN && settlement.player_team_index() == 0) ||
			(settlement.outcome() == ::BATTLE_OUTCOME_SIDE_B_WIN && settlement.player_team_index() == 1);
		const bool playerLost =
			(settlement.outcome() == ::BATTLE_OUTCOME_SIDE_A_WIN && settlement.player_team_index() == 1) ||
			(settlement.outcome() == ::BATTLE_OUTCOME_SIDE_B_WIN && settlement.player_team_index() == 0);

		if (playerLost && candidateOut.defeat_mark_battle_id() != settlement.battle_id())
		{
			const uint32_t winnerTeam = settlement.outcome() == ::BATTLE_OUTCOME_SIDE_A_WIN ? 0u : 1u;
			const bool isGuildDuel =
				ctx.side_a_guild_id() != 0 && ctx.side_b_guild_id() != 0 &&
				ctx.side_a_guild_id() != ctx.side_b_guild_id();
			uint64_t winnerId = 0;
			std::string winnerName;
			if (isGuildDuel)
			{
				winnerId = winnerTeam == 0 ? ctx.side_a_guild_id() : ctx.side_b_guild_id();
				winnerName = winnerTeam == 0 ? ctx.side_a_guild_name() : ctx.side_b_guild_name();
			}
			else
			{
				winnerId = winnerTeam == 0 ? ctx.challenger_captain_id() : ctx.target_captain_id();
				winnerName = winnerTeam == 0 ? ctx.challenger_captain_name() : ctx.target_captain_name();
			}
			candidateOut.set_defeat_mark_expires_at_ms(
				nowMs + static_cast<uint64_t>(defeatDurationSec) * 1000ULL);
			candidateOut.set_defeat_mark_winner_id(winnerId);
			candidateOut.set_defeat_mark_winner_name(winnerName);
			candidateOut.set_defeat_mark_style_id(
				ctx.writ_tier() == static_cast<uint32_t>(match::JIANGHU_WRIT_TIER_GOLD_ITEM) ? 2u : 1u);
			candidateOut.set_defeat_mark_is_guild_name(isGuildDuel);
			candidateOut.set_defeat_mark_battle_id(settlement.battle_id());
		}
		else if (playerWon && candidateOut.defeat_mark_expires_at_ms() > nowMs)
		{
			const uint64_t opponentCaptainId =
				settlement.player_team_index() == 0 ? ctx.target_captain_id() : ctx.challenger_captain_id();
			const uint64_t opponentGuildId =
				settlement.player_team_index() == 0 ? ctx.side_b_guild_id() : ctx.side_a_guild_id();
			const bool revengeMatched =
				(!candidateOut.defeat_mark_is_guild_name() && opponentCaptainId != 0 &&
				 opponentCaptainId == candidateOut.defeat_mark_winner_id()) ||
				(candidateOut.defeat_mark_is_guild_name() && opponentGuildId != 0 &&
				 opponentGuildId == candidateOut.defeat_mark_winner_id());
			if (revengeMatched)
			{
				ClearDefeatMarkFields(candidateOut);
			}
		}
	}

	changedOut = candidateOut.SerializeAsString() != beforeBytes;
	return kSuccess;
}
