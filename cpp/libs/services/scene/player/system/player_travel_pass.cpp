#include "player_travel_pass.h"

#include <algorithm>
#include <limits>
#include <map>
#include <set>
#include <string>
#include <vector>

#include "muduo/base/Logging.h"

#include "battle/system/player_battle.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h"
#include "player/system/client_request_ledger.h"
#include "player/system/player_appearance.h"
#include "player/system/player_equipment_material_reward.h"
#include "player/system/player_feature_snapshot.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "player/system/player_spirit_shop.h"
#include "player/system/travel_pass_rules.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_spirit_shop_comp.pb.h"
#include "proto/common/component/player_travel_pass_comp.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"
#include "proto/match/match_service.pb.h"
#include "proto/scene/player_activity.pb.h"
#include "proto/scene/player_appearance.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "proto/scene/player_spirit_shop.pb.h"
#include "table/code/characterappearance_table.h"
#include "table/code/item_table.h"
#include "table/code/spiritshopgoods_table.h"
#include "table/code/travelpassseason_table.h"
#include "table/code/travelpasstier_table.h"
#include "table/proto/tip/asset_error_tip.pb.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/travel_pass_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
	struct ResolvedSeason
	{
		uint32_t id{0};
		uint32_t enabled{0};
		std::string name;
		std::string description;
		std::string iconKey;
		std::string bannerAssetKey;
		uint64_t startsAtMs{0};
		uint64_t endsAtMs{0};
		uint32_t maxPoints{0};
		uint32_t pveWinPoints{0};
		uint32_t hardOrPvpWinPoints{0};
		uint32_t pvpQualifiedOtherPoints{0};
		uint32_t premiumCurrencyType{1};
		uint64_t premiumPrice{0};
		uint32_t configRevision{1};
		uint32_t sortOrder{0};
		bool isArchived{false};
	};

	struct ResolvedTier
	{
		uint32_t tierId{0};
		uint32_t passId{0};
		uint32_t tierIndex{0};
		uint32_t requiredPoints{0};
		uint32_t freeItemId{0};
		uint32_t freeItemCount{0};
		std::string freeItemName;
		std::string freeItemIconKey;
		uint64_t freeGold{0};
		std::string freeCosmeticKey;
		std::string freeCosmeticName;
		uint32_t premiumItemId{0};
		uint32_t premiumItemCount{0};
		std::string premiumItemName;
		std::string premiumItemIconKey;
		uint64_t premiumGold{0};
		std::string premiumCosmeticKey;
		std::string premiumCosmeticName;
	};

	struct ClaimTrackTarget
	{
		std::size_t tierPos{0};
		bool premiumTrack{false};
	};

	std::string PlayerLogId(entt::entity player)
	{
		const auto *guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
		return std::string("player_id=") + std::to_string(guid != nullptr ? guid->value : kInvalidGuid);
	}

	bool ResolveSeasonRow(const TravelPassSeasonTable &row, ResolvedSeason &out, const char *&reason)
	{
		if (row.id() == 0)
		{
			reason = "season id is 0";
			return false;
		}
		if (row.max_points() == 0)
		{
			reason = "max_points is 0";
			return false;
		}
		if (row.ends_at_ms() != 0 && row.ends_at_ms() <= row.starts_at_ms())
		{
			reason = "ends_at_ms must be greater than starts_at_ms";
			return false;
		}
		if (!IsKnownCurrencyType(row.premium_currency_type()))
		{
			reason = "unknown premium_currency_type";
			return false;
		}
		if (row.premium_price() == 0 || row.premium_price() > travel_pass_rules::kMaxUnitPrice)
		{
			reason = "premium_price not in 1..1000000000";
			return false;
		}
		if (row.config_revision() == 0)
		{
			reason = "config_revision is 0";
			return false;
		}
		out = ResolvedSeason{};
		out.id = row.id();
		out.enabled = row.enabled();
		out.name = row.name();
		out.description = row.description();
		out.iconKey = row.icon_key();
		out.bannerAssetKey = row.banner_asset_key();
		out.startsAtMs = row.starts_at_ms();
		out.endsAtMs = row.ends_at_ms();
		out.maxPoints = row.max_points();
		out.pveWinPoints = row.pve_win_points();
		out.hardOrPvpWinPoints = row.hard_or_pvp_win_points();
		out.pvpQualifiedOtherPoints = row.pvp_qualified_other_points();
		out.premiumCurrencyType = row.premium_currency_type();
		out.premiumPrice = row.premium_price();
		out.configRevision = row.config_revision();
		out.sortOrder = row.sort_order() != 0 ? row.sort_order() : row.id();
		out.isArchived = row.is_archived() != 0;
		return true;
	}

	bool ResolveTierRow(const TravelPassTierTable &row, uint32_t seasonMaxPoints, ResolvedTier &out, const char *&reason)
	{
		if (row.id() == 0 || row.pass_id() == 0)
		{
			reason = "tier id or pass_id is 0";
			return false;
		}
		if (row.tier_index() == 0 || row.tier_index() > travel_pass_rules::kMaxTiersPerSeason)
		{
			reason = "tier_index not in 1..64";
			return false;
		}
		if (row.required_points() == 0 || row.required_points() > seasonMaxPoints)
		{
			reason = "required_points out of 1..season.max_points";
			return false;
		}

		out = ResolvedTier{};
		out.tierId = row.id();
		out.passId = row.pass_id();
		out.tierIndex = row.tier_index();
		out.requiredPoints = row.required_points();

		if (row.free_item_id() != 0)
		{
			const auto [item, itemErr] = ItemTableManager::Instance().FindByIdSilent(row.free_item_id());
			if (item == nullptr || row.free_item_count() == 0 || item->max_stack_size() == 0)
			{
				reason = "free_item_id missing or free_item_count is 0";
				return false;
			}
			out.freeItemId = row.free_item_id();
			out.freeItemCount = row.free_item_count();
			out.freeItemName = item->name();
			out.freeItemIconKey = item->icon_key();
		}
		out.freeGold = row.free_gold();
		if (!row.free_cosmetic_key().empty())
		{
			if (!travel_pass_rules::IsValidCosmeticKey(row.free_cosmetic_key()) || row.free_cosmetic_name().empty())
			{
				reason = "free_cosmetic_key invalid or free_cosmetic_name empty";
				return false;
			}
			out.freeCosmeticKey = row.free_cosmetic_key();
			out.freeCosmeticName = row.free_cosmetic_name();
		}

		if (row.premium_item_id() != 0)
		{
			const auto [item, itemErr] = ItemTableManager::Instance().FindByIdSilent(row.premium_item_id());
			if (item == nullptr || row.premium_item_count() == 0 || item->max_stack_size() == 0)
			{
				reason = "premium_item_id missing or premium_item_count is 0";
				return false;
			}
			out.premiumItemId = row.premium_item_id();
			out.premiumItemCount = row.premium_item_count();
			out.premiumItemName = item->name();
			out.premiumItemIconKey = item->icon_key();
		}
		out.premiumGold = row.premium_gold();
		if (!row.premium_cosmetic_key().empty())
		{
			if (!travel_pass_rules::IsValidCosmeticKey(row.premium_cosmetic_key()) || row.premium_cosmetic_name().empty())
			{
				reason = "premium_cosmetic_key invalid or premium_cosmetic_name empty";
				return false;
			}
			out.premiumCosmeticKey = row.premium_cosmetic_key();
			out.premiumCosmeticName = row.premium_cosmetic_name();
		}

		const bool hasFree = out.freeItemId != 0 || out.freeGold != 0 || !out.freeCosmeticKey.empty();
		const bool hasPremium = out.premiumItemId != 0 || out.premiumGold != 0 || !out.premiumCosmeticKey.empty();
		if (!hasFree && !hasPremium)
		{
			reason = "tier has neither free nor premium reward";
			return false;
		}
		return true;
	}

	bool CollectSortedTiers(uint32_t passId, uint32_t seasonMaxPoints, std::vector<ResolvedTier> &outTiers, const char *&reason)
	{
		outTiers.clear();
		for (const auto &row : TravelPassTierTableManager::Instance().FindAll().data())
		{
			if (row.pass_id() != passId)
			{
				continue;
			}
			ResolvedTier tier;
			if (!ResolveTierRow(row, seasonMaxPoints, tier, reason))
			{
				return false;
			}
			outTiers.push_back(std::move(tier));
		}
		if (outTiers.empty())
		{
			reason = "season has no tier rows";
			return false;
		}
		std::sort(outTiers.begin(), outTiers.end(),
				  [](const ResolvedTier &lhs, const ResolvedTier &rhs) { return lhs.tierIndex < rhs.tierIndex; });
		for (std::size_t i = 0; i < outTiers.size(); ++i)
		{
			if (i > 0)
			{
				if (outTiers[i].tierIndex == outTiers[i - 1].tierIndex)
				{
					reason = "duplicate tier_index within season";
					return false;
				}
				if (outTiers[i].requiredPoints <= outTiers[i - 1].requiredPoints)
				{
					reason = "required_points must strictly increase with tier_index";
					return false;
				}
			}
		}
		return true;
	}

	bool HasUnclaimedReward(const std::vector<ResolvedTier> &tiers, const TravelPassSeasonProgress &progress)
	{
		for (const auto &tier : tiers)
		{
			if (progress.progress_points() < tier.requiredPoints)
			{
				continue;
			}
			const bool hasFree = tier.freeItemId != 0 || tier.freeGold != 0 || !tier.freeCosmeticKey.empty();
			if (hasFree && !travel_pass_rules::IsTierClaimed(progress.free_claimed_mask(), tier.tierIndex))
			{
				return true;
			}
			const bool hasPremium = tier.premiumItemId != 0 || tier.premiumGold != 0 || !tier.premiumCosmeticKey.empty();
			if (progress.premium_unlocked() && hasPremium &&
				!travel_pass_rules::IsTierClaimed(progress.premium_claimed_mask(), tier.tierIndex))
			{
				return true;
			}
		}
		return false;
	}

	bool FindSeasonById(uint32_t passId, ResolvedSeason &outSeason, std::vector<ResolvedTier> &outTiers)
	{
		const auto [row, err] = TravelPassSeasonTableManager::Instance().FindByIdSilent(passId);
		if (row == nullptr)
		{
			return false;
		}
		const char *reason = "";
		if (!ResolveSeasonRow(*row, outSeason, reason))
		{
			return false;
		}
		return CollectSortedTiers(outSeason.id, outSeason.maxPoints, outTiers, reason);
	}

	struct BookshelfItem
	{
		ResolvedSeason season;
		std::vector<ResolvedTier> tiers;
		bool windowOpen{false};
		bool progressEligible{false};
	};

	uint32_t CountClaimableTracks(const std::vector<ResolvedTier> &tiers, const TravelPassSeasonProgress *progress,
								  uint32_t maxPoints)
	{
		if (progress == nullptr)
		{
			return 0u;
		}
		const uint32_t points = std::min(progress->progress_points(), maxPoints);
		uint32_t count = 0u;
		for (const auto &tier : tiers)
		{
			if (points < tier.requiredPoints)
			{
				continue;
			}
			const bool hasFree = tier.freeItemId != 0 || tier.freeGold != 0 || !tier.freeCosmeticKey.empty();
			if (hasFree && !travel_pass_rules::IsTierClaimed(progress->free_claimed_mask(), tier.tierIndex))
			{
				++count;
			}
			const bool hasPremium =
				tier.premiumItemId != 0 || tier.premiumGold != 0 || !tier.premiumCosmeticKey.empty();
			if (progress->premium_unlocked() && hasPremium &&
				!travel_pass_rules::IsTierClaimed(progress->premium_claimed_mask(), tier.tierIndex))
			{
				++count;
			}
		}
		return count;
	}

	std::vector<BookshelfItem> CollectBookshelf(const PlayerTravelPassComp &comp, uint64_t nowMs)
	{
		std::set<uint32_t> participatedIds;
		for (const auto &prog : comp.seasons())
		{
			participatedIds.insert(prog.pass_id());
		}

		std::vector<BookshelfItem> shelf;
		for (const auto &row : TravelPassSeasonTableManager::Instance().FindAll().data())
		{
			ResolvedSeason season;
			const char *reason = "";
			if (!ResolveSeasonRow(row, season, reason))
			{
				continue;
			}
			const bool windowOpen =
				season.enabled != 0 && travel_pass_rules::IsWindowOpen(season.startsAtMs, season.endsAtMs, nowMs);
			const auto *prog = travel_pass_rules::FindSeason(comp, season.id);
			const bool participated = participatedIds.find(season.id) != participatedIds.end();
			if (!windowOpen && !participated)
			{
				continue;
			}
			std::vector<ResolvedTier> tiers;
			if (!CollectSortedTiers(season.id, season.maxPoints, tiers, reason))
			{
				continue;
			}
			const uint32_t curPoints = prog != nullptr ? std::min(prog->progress_points(), season.maxPoints) : 0u;
			const bool progressEligible = windowOpen || (prog != nullptr && prog->premium_unlocked() && curPoints < season.maxPoints);
			shelf.push_back(BookshelfItem{std::move(season), std::move(tiers), windowOpen, progressEligible});
		}
		std::sort(shelf.begin(), shelf.end(), [](const BookshelfItem &a, const BookshelfItem &b)
				  {
					  if (a.season.sortOrder != b.season.sortOrder)
					  {
						  return a.season.sortOrder < b.season.sortOrder;
					  }
					  return a.season.id < b.season.id;
				  });
		return shelf;
	}

	// 解析唯一在研期次 ID（防多期并发加点 M-1）：
	// 1. 玩家显式选定且未满点的可用期次优先；
	// 2. 否则按书架升序顺延至第一本未满点的可用期次；
	// 3. 若全部已满点，保持已选期次或回退至书架末期/有未领奖励的历史期次。
	uint32_t ResolveActivePassId(const PlayerTravelPassComp &comp, const std::vector<BookshelfItem> &shelf)
	{
		if (shelf.empty())
		{
			return 0u;
		}
		const auto isIncomplete = [&](const BookshelfItem &item) {
			const auto *prog = travel_pass_rules::FindSeason(comp, item.season.id);
			const uint32_t pts = prog != nullptr ? std::min(prog->progress_points(), item.season.maxPoints) : 0u;
			return item.progressEligible && pts < item.season.maxPoints;
		};

		if (comp.active_pass_id() != 0)
		{
			for (const auto &item : shelf)
			{
				if (item.season.id == comp.active_pass_id() && isIncomplete(item))
				{
					return item.season.id;
				}
			}
		}
		for (const auto &item : shelf)
		{
			if (isIncomplete(item))
			{
				return item.season.id;
			}
		}
		if (comp.active_pass_id() != 0)
		{
			for (const auto &item : shelf)
			{
				if (item.season.id == comp.active_pass_id())
				{
					return item.season.id;
				}
			}
		}
		for (const auto &item : shelf)
		{
			if (item.windowOpen)
			{
				return item.season.id;
			}
		}
		for (auto it = shelf.rbegin(); it != shelf.rend(); ++it)
		{
			const auto *prog = travel_pass_rules::FindSeason(comp, it->season.id);
			if (prog != nullptr && HasUnclaimedReward(it->tiers, *prog))
			{
				return it->season.id;
			}
		}
		return shelf.front().season.id;
	}

	// 自动选择目标期次：requestedPassId != 0 精确查找；否则取唯一在研期次（或有未领奖励的历史期次）。
	bool SelectTargetSeason(const PlayerTravelPassComp &comp, uint64_t nowMs, uint32_t requestedPassId,
							ResolvedSeason &outSeason, std::vector<ResolvedTier> &outTiers)
	{
		if (requestedPassId != 0)
		{
			return FindSeasonById(requestedPassId, outSeason, outTiers);
		}
		auto shelf = CollectBookshelf(comp, nowMs);
		const uint32_t targetId = ResolveActivePassId(comp, shelf);
		for (auto &item : shelf)
		{
			if (item.season.id == targetId)
			{
				outSeason = std::move(item.season);
				outTiers = std::move(item.tiers);
				return true;
			}
		}
		return false;
	}

	uint32_t MapBagError(uint32_t bagErr)
	{
		if (bagErr == kAssetBlocked)
		{
			return kTravelPassItemBlocked;
		}
		if (bagErr == kBagAddItemBagFull || bagErr == kBagItemNotStacked)
		{
			return kTravelPassBagFull;
		}
		if (bagErr == kTravelPassFrozen || bagErr == kTravelPassInBattle || bagErr == kTravelPassUnavailable ||
			bagErr == kTravelPassItemBlocked || bagErr == kTravelPassBagFull)
		{
			return bagErr;
		}
		return kTravelPassUnavailable;
	}

	void FillSnapshot(entt::entity player, uint64_t nowMs, const PlayerTravelPassComp &comp,
					  const ResolvedSeason &season, const std::vector<ResolvedTier> &tiers,
					  ::TravelPassSnapshot &out)
	{
		out.Clear();
		out.set_available(true);
		out.set_server_now_ms(nowMs);
		out.set_pass_revision(comp.pass_revision());
		out.set_pass_id(season.id);
		out.set_config_revision(season.configRevision);
		out.set_name(season.name);
		out.set_description(season.description);
		out.set_icon_key(season.iconKey);
		out.set_banner_asset_key(season.bannerAssetKey);
		out.set_starts_at_ms(season.startsAtMs);
		out.set_ends_at_ms(season.endsAtMs);
		out.set_max_points(season.maxPoints);
		out.set_pve_win_points(season.pveWinPoints);
		out.set_hard_or_pvp_win_points(season.hardOrPvpWinPoints);
		out.set_pvp_qualified_other_points(season.pvpQualifiedOtherPoints);
		out.set_premium_currency_type(season.premiumCurrencyType);
		out.set_premium_price(season.premiumPrice);
		out.set_diamond_balance(CurrencySystem::GetBalance(player, kCurrencyDiamond));
		out.set_bind_diamond_balance(CurrencySystem::GetBalance(player, kCurrencyBindDiamond));

		const auto *progress = travel_pass_rules::FindSeason(comp, season.id);
		const uint32_t points = progress != nullptr ? std::min(progress->progress_points(), season.maxPoints) : 0u;
		const bool premiumUnlocked = progress != nullptr && progress->premium_unlocked();
		const uint64_t freeMask = progress != nullptr ? progress->free_claimed_mask() : 0ULL;
		const uint64_t premiumMask = progress != nullptr ? progress->premium_claimed_mask() : 0ULL;

		const bool windowOpen =
			season.enabled != 0 && travel_pass_rules::IsWindowOpen(season.startsAtMs, season.endsAtMs, nowMs);
		// 解锁典藏后永不过期：哪怕期次已过 ends_at_ms，未满点仍可持续推点！
		out.set_open_for_progress(windowOpen || (premiumUnlocked && points < season.maxPoints));
		out.set_open_for_unlock(windowOpen && !premiumUnlocked);
		out.set_progress_points(points);
		out.set_premium_unlocked(premiumUnlocked);
		out.set_premium_unlocked_at_ms(progress != nullptr ? progress->premium_unlocked_at_ms() : 0ULL);

		uint32_t claimableFree = 0;
		uint32_t claimablePremium = 0;
		uint32_t retroactivePremium = 0;
		for (const auto &tier : tiers)
		{
			auto *view = out.add_tiers();
			view->set_tier_id(tier.tierId);
			view->set_tier_index(tier.tierIndex);
			view->set_required_points(tier.requiredPoints);
			const bool reached = points >= tier.requiredPoints;
			view->set_reached(reached);

			const bool hasFree = tier.freeItemId != 0 || tier.freeGold != 0 || !tier.freeCosmeticKey.empty();
			const bool freeClaimed = travel_pass_rules::IsTierClaimed(freeMask, tier.tierIndex);
			const bool freeCanClaim = hasFree && reached && !freeClaimed;
			view->set_free_item_id(tier.freeItemId);
			view->set_free_item_count(tier.freeItemCount);
			view->set_free_item_name(tier.freeItemName);
			view->set_free_item_icon_key(tier.freeItemIconKey);
			view->set_free_gold(tier.freeGold);
			view->set_free_cosmetic_key(tier.freeCosmeticKey);
			view->set_free_cosmetic_name(tier.freeCosmeticName);
			view->set_free_claimed(freeClaimed);
			view->set_free_can_claim(freeCanClaim);
			if (freeCanClaim)
			{
				++claimableFree;
			}

			const bool hasPremium =
				tier.premiumItemId != 0 || tier.premiumGold != 0 || !tier.premiumCosmeticKey.empty();
			const bool premiumClaimed = travel_pass_rules::IsTierClaimed(premiumMask, tier.tierIndex);
			const bool premiumCanClaim = hasPremium && reached && premiumUnlocked && !premiumClaimed;
			view->set_premium_item_id(tier.premiumItemId);
			view->set_premium_item_count(tier.premiumItemCount);
			view->set_premium_item_name(tier.premiumItemName);
			view->set_premium_item_icon_key(tier.premiumItemIconKey);
			view->set_premium_gold(tier.premiumGold);
			view->set_premium_cosmetic_key(tier.premiumCosmeticKey);
			view->set_premium_cosmetic_name(tier.premiumCosmeticName);
			view->set_premium_claimed(premiumClaimed);
			view->set_premium_can_claim(premiumCanClaim);
			if (premiumCanClaim)
			{
				++claimablePremium;
			}
			else if (hasPremium && reached && !premiumUnlocked && !premiumClaimed)
			{
				++retroactivePremium;
			}
		}
		out.set_claimable_free_count(claimableFree);
		out.set_claimable_premium_count(claimablePremium);
		out.set_retroactive_premium_count(retroactivePremium);

		std::set<std::string> cosmetics;
		for (const auto &key : comp.bonus_cosmetic_keys())
		{
			cosmetics.insert(key);
		}
		for (const auto &s : comp.seasons())
		{
			for (const auto &key : s.unlocked_cosmetic_keys())
			{
				cosmetics.insert(key);
			}
		}
		for (const auto &key : cosmetics)
		{
			out.add_unlocked_cosmetic_keys(key);
		}
		out.set_equipped_frame_key(comp.equipped_frame_key());
		out.set_equipped_title_key(comp.equipped_title_key());

		const auto shelf = CollectBookshelf(comp, nowMs);
		const uint32_t activePassId = ResolveActivePassId(comp, shelf);
		out.set_active_pass_id(activePassId);
		for (const auto &item : shelf)
		{
			const auto *prog = travel_pass_rules::FindSeason(comp, item.season.id);
			const uint32_t pts = prog != nullptr ? std::min(prog->progress_points(), item.season.maxPoints) : 0u;
			const bool prem = prog != nullptr && prog->premium_unlocked();
			auto *cat = out.add_bookshelf();
			cat->set_pass_id(item.season.id);
			cat->set_name(item.season.name);
			cat->set_icon_key(item.season.iconKey);
			cat->set_banner_asset_key(item.season.bannerAssetKey);
			cat->set_sort_order(item.season.sortOrder);
			cat->set_is_archived(item.season.isArchived);
			cat->set_is_active(item.season.id == activePassId);
			cat->set_progress_points(pts);
			cat->set_max_points(item.season.maxPoints);
			cat->set_premium_unlocked(prem);
			cat->set_claimable_count(CountClaimableTracks(item.tiers, prog, item.season.maxPoints));
			cat->set_open_for_progress(item.progressEligible);
			cat->set_open_for_unlock(item.windowOpen && !prem);
		}
	}

	uint32_t ComputeBattlePoints(const ResolvedSeason &season, const BattleSettlementData &settlement)
	{
		if (!PlayerTravelPassSystem::IsEligibleBattle(settlement))
		{
			return 0u;
		}
		const bool playerWon =
			(settlement.outcome() == BATTLE_OUTCOME_SIDE_A_WIN && settlement.player_team_index() == 0) ||
			(settlement.outcome() == BATTLE_OUTCOME_SIDE_B_WIN && settlement.player_team_index() == 1);
		switch (settlement.match_mode())
		{
		case match::MATCH_MODE_PVE_SOLO:
		case match::MATCH_MODE_PVE_TEAM:
			if (!playerWon)
			{
				return 0u;
			}
			return settlement.battle_difficulty() == 2u ? season.hardOrPvpWinPoints : season.pveWinPoints;
		case match::MATCH_MODE_1V1:
		case match::MATCH_MODE_5V5:
		case match::MATCH_MODE_PVP_TEAM:
		case match::MATCH_MODE_POLARITY_5V5:
		case match::MATCH_MODE_GUILD_WAR_PVP:
		case match::MATCH_MODE_GUILD_WAR_SIEGE:
			return playerWon ? season.hardOrPvpWinPoints : season.pvpQualifiedOtherPoints;
		default:
			return 0u;
		}
	}

	std::string ClaimExtraJson(uint32_t passId, uint32_t claimedTracks, uint64_t goldGain)
	{
		return std::string("{\"source\":\"travel_pass\",\"pass_id\":") + std::to_string(passId) +
			   ",\"claimed_tracks\":" + std::to_string(claimedTracks) +
			   ",\"gold\":" + std::to_string(goldGain) + "}";
	}
} // namespace

bool PlayerTravelPassSystem::ValidateTables()
{
	bool allValid = true;
	std::set<uint32_t> seasonIds;
	for (const auto &row : TravelPassSeasonTableManager::Instance().FindAll().data())
	{
		ResolvedSeason season;
		const char *reason = "";
		if (!ResolveSeasonRow(row, season, reason))
		{
			LOG_ERROR << "[TravelPass] Invalid TravelPassSeason row: id=" << row.id() << " reason=" << reason;
			allValid = false;
			continue;
		}
		seasonIds.insert(season.id);
		std::vector<ResolvedTier> tiers;
		if (!CollectSortedTiers(season.id, season.maxPoints, tiers, reason))
		{
			LOG_ERROR << "[TravelPass] Invalid TravelPassTier rows for pass_id=" << season.id << " reason=" << reason;
			allValid = false;
			continue;
		}
	}

	for (const auto &tierRow : TravelPassTierTableManager::Instance().FindAll().data())
	{
		if (seasonIds.find(tierRow.pass_id()) == seasonIds.end())
		{
			LOG_ERROR << "[TravelPass] TravelPassTier references unknown pass_id: tier_id=" << tierRow.id()
					  << " pass_id=" << tierRow.pass_id();
			allValid = false;
		}
	}
	return allValid;
}

bool PlayerTravelPassSystem::IsEligibleBattle(const BattleSettlementData &settlement)
{
	if (settlement.battle_id() == 0 || settlement.player_id() == 0 || settlement.fled() ||
		settlement.total_rounds() == 0 || settlement.player_team_index() > 1 ||
		(settlement.boss_trial_kind() >= 9u && settlement.boss_trial_kind() <= 11u) ||
		(settlement.outcome() != BATTLE_OUTCOME_SIDE_A_WIN &&
		 settlement.outcome() != BATTLE_OUTCOME_SIDE_B_WIN &&
		 settlement.outcome() != BATTLE_OUTCOME_DRAW))
	{
		return false;
	}
	const bool playerWon =
		(settlement.outcome() == BATTLE_OUTCOME_SIDE_A_WIN && settlement.player_team_index() == 0) ||
		(settlement.outcome() == BATTLE_OUTCOME_SIDE_B_WIN && settlement.player_team_index() == 1);
	switch (settlement.match_mode())
	{
	case match::MATCH_MODE_PVE_SOLO:
	case match::MATCH_MODE_PVE_TEAM:
		return playerWon;
	case match::MATCH_MODE_1V1:
	case match::MATCH_MODE_5V5:
	case match::MATCH_MODE_PVP_TEAM:
	case match::MATCH_MODE_POLARITY_5V5:
	case match::MATCH_MODE_GUILD_WAR_PVP:
	case match::MATCH_MODE_GUILD_WAR_SIEGE:
		if (!settlement.participated())
		{
			return false;
		}
		if (playerWon)
		{
			return true;
		}
		// PVP 平/负局统一走防挂机门槛，杜绝秒死/纯托管刷点。
		return settlement.total_rounds() >= PlayerEquipmentMaterialRewardSystem::kPvpMinRoundsForLoss &&
			   settlement.manual_action_rounds() >= PlayerEquipmentMaterialRewardSystem::kPvpMinManualRoundsForLoss &&
			   settlement.effective_combat_contribution();
	default:
		return false;
	}
}

uint32_t PlayerTravelPassSystem::PrepareBattleCredit(const PlayerTravelPassComp &current,
													 const BattleSettlementData &settlement,
													 uint64_t nowMs,
													 PlayerTravelPassComp &candidate,
													 bool &pointsIncreased)
{
	candidate = current;
	pointsIncreased = false;
	if (!IsEligibleBattle(settlement))
	{
		return kSuccess;
	}
	if (!travel_pass_rules::ValidateComp(current).empty())
	{
		return kTravelPassUnavailable;
	}

	// 永不绝版书架制（防多期并发加点 M-1）：单场战斗仅注入唯一解析出的在研期次；当前期次满点时自动顺延至下一未满期次。
	const auto shelf = CollectBookshelf(current, nowMs);
	const uint32_t targetPassId = ResolveActivePassId(current, shelf);
	if (targetPassId == 0u)
	{
		return kSuccess;
	}
	for (const auto &item : shelf)
	{
		if (item.season.id != targetPassId || !item.progressEligible)
		{
			continue;
		}
		const uint32_t gain = ComputeBattlePoints(item.season, settlement);
		if (gain == 0u)
		{
			break;
		}
		auto *prog = travel_pass_rules::FindOrAddSeason(candidate, item.season.id);
		if (prog == nullptr)
		{
			break;
		}
		const uint32_t before = std::min(prog->progress_points(), item.season.maxPoints);
		if (before >= item.season.maxPoints)
		{
			break;
		}
		const uint32_t added = std::min(gain, item.season.maxPoints - before);
		prog->set_progress_points(before + added);
		prog->set_last_updated_at_ms(nowMs);
		candidate.set_active_pass_id(item.season.id);
		pointsIncreased = true;
		break;
	}

	if (pointsIncreased)
	{
		if (candidate.pass_revision() < std::numeric_limits<uint64_t>::max())
		{
			candidate.set_pass_revision(candidate.pass_revision() + 1u);
		}
	}
	return kSuccess;
}

uint32_t PlayerTravelPassSystem::GetState(entt::entity player, uint64_t nowMs, uint32_t requestedPassId,
										  ::TravelPassSnapshot &out)
{
	out.Clear();
	out.set_server_now_ms(nowMs);
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return kThisEntityIsInvalid;
	}
	const auto *comp = tlsEcs.actorRegistry.try_get<PlayerTravelPassComp>(player);
	if (comp == nullptr)
	{
		LOG_ERROR << "[TravelPass] GetState rejected: PlayerTravelPassComp missing " << PlayerLogId(player);
		return kTravelPassUnavailable;
	}
	if (const std::string err = travel_pass_rules::ValidateComp(*comp); !err.empty())
	{
		LOG_ERROR << "[TravelPass] GetState rejected: PlayerTravelPassComp invalid (" << err << ") "
				  << PlayerLogId(player);
		return kTravelPassUnavailable;
	}

	ResolvedSeason season;
	std::vector<ResolvedTier> tiers;
	if (!SelectTargetSeason(*comp, nowMs, requestedPassId, season, tiers))
	{
		out.set_available(false);
		out.set_unavailable_reason("游历手记暂未开放");
		out.set_pass_revision(comp->pass_revision());
		out.set_diamond_balance(CurrencySystem::GetBalance(player, kCurrencyDiamond));
		out.set_bind_diamond_balance(CurrencySystem::GetBalance(player, kCurrencyBindDiamond));
		out.set_equipped_frame_key(comp->equipped_frame_key());
		out.set_equipped_title_key(comp->equipped_title_key());
		return kSuccess;
	}

	FillSnapshot(player, nowMs, *comp, season, tiers, out);
	return kSuccess;
}

uint32_t PlayerTravelPassSystem::UnlockPremium(entt::entity player,
											   const ::UnlockTravelPassPremiumRequest &request,
											   uint64_t nowMs,
											   ::UnlockTravelPassPremiumResponse &response)
{
	response.Clear();
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return kThisEntityIsInvalid;
	}
	const uint64_t clientRequestId = request.client_request_id();
	if (clientRequestId == 0 || request.pass_id() == 0)
	{
		return kInvalidParameter;
	}

	// 1. 幂等查重优先于一切依赖状态的校验。
	const std::string fingerprint = ClientRequestFingerprint(request);
	ClientRequestResult replayResult;
	const auto verdict = client_request_dedup::Check(player, clientRequestId, ClientRequestKind::kUnlockTravelPassPremium,
													 fingerprint, replayResult);
	if (verdict == ClientRequestVerdict::kMismatch)
	{
		return kInvalidParameter;
	}
	if (verdict == ClientRequestVerdict::kUnavailable)
	{
		return kServiceUnavailable;
	}
	if (verdict == ClientRequestVerdict::kReplay)
	{
		response.set_replayed(true);
		(void)GetState(player, nowMs, request.pass_id(), *response.mutable_snapshot());
		return kSuccess;
	}

	// 2. 可写与战斗闸。
	if (!player_mutability::IsPlayerMutable(player))
	{
		return kTravelPassFrozen;
	}
	if (PlayerBattleSystem::IsInBattle(player))
	{
		return kTravelPassInBattle;
	}

	// 3. 期次有效性、开放时间窗与报价/修订防呆。
	ResolvedSeason season;
	std::vector<ResolvedTier> tiers;
	if (!FindSeasonById(request.pass_id(), season, tiers) || season.enabled == 0 ||
		!travel_pass_rules::IsWindowOpen(season.startsAtMs, season.endsAtMs, nowMs))
	{
		return kTravelPassNotOpen;
	}
	if (request.expected_currency_type() != season.premiumCurrencyType ||
		request.expected_price() != season.premiumPrice ||
		request.expected_config_revision() != season.configRevision)
	{
		return kTravelPassPriceChanged;
	}

	auto *comp = tlsEcs.actorRegistry.try_get<PlayerTravelPassComp>(player);
	auto *ledger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
	if (comp == nullptr || !travel_pass_rules::ValidateComp(*comp).empty())
	{
		return kTravelPassUnavailable;
	}
	if (ledger == nullptr || tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player) ||
		ledger->next_seq() == std::numeric_limits<uint64_t>::max())
	{
		return kServiceUnavailable;
	}
	if (const auto *existing = travel_pass_rules::FindSeason(*comp, season.id);
		existing != nullptr && existing->premium_unlocked())
	{
		return kTravelPassAlreadyUnlocked;
	}

	const auto currencyType = static_cast<CurrencyType>(season.premiumCurrencyType);
	const int64_t cost = static_cast<int64_t>(season.premiumPrice);
	if (!CurrencySystem::CanAfford(player, currencyType, cost))
	{
		return kTravelPassCurrencyNotEnough;
	}

	// 4. 预构造候选组件与幂等账本，再同步扣款并原子交换。
	PlayerTravelPassComp candidateComp = *comp;
	auto *targetSeason = travel_pass_rules::FindOrAddSeason(candidateComp, season.id);
	if (targetSeason == nullptr || candidateComp.pass_revision() == std::numeric_limits<uint64_t>::max())
	{
		return kTravelPassUnavailable;
	}
	targetSeason->set_premium_unlocked(true);
	targetSeason->set_premium_unlocked_at_ms(nowMs);
	targetSeason->set_last_updated_at_ms(nowMs);
	candidateComp.set_pass_revision(candidateComp.pass_revision() + 1u);
	if (!travel_pass_rules::ValidateComp(candidateComp).empty())
	{
		return kTravelPassUnavailable;
	}

	ClientRequestLedgerComp candidateLedger = *ledger;
	if (!client_request_ledger::Remember(candidateLedger, clientRequestId, ClientRequestKind::kUnlockTravelPassPremium,
										 fingerprint, ClientRequestResult{}) ||
		!client_request_ledger::Validate(candidateLedger).empty())
	{
		return kServiceUnavailable;
	}

	if (const uint32_t deductErr =
			CurrencySystem::DeductCurrency(player, currencyType, cost, TX_TRAVEL_PASS_UNLOCK, clientRequestId);
		deductErr != kSuccess)
	{
		LOG_ERROR << "[TravelPass] UnlockPremium currency deduction failed: " << PlayerLogId(player)
				  << " pass_id=" << season.id << " cost=" << cost << " err=" << deductErr;
		return deductErr == kAssetCurrencyInsufficient ? uint32_t{kTravelPassCurrencyNotEnough}
													   : uint32_t{kTravelPassUnavailable};
	}

	comp->Swap(&candidateComp);
	ledger->Swap(&candidateLedger);
	PlayerLifecycleSystem::RequestKeyProgressSave(player, "travel_pass_unlock");

	LOG_INFO << "[TravelPass] metric=travel_pass_premium_unlocked " << PlayerLogId(player)
			 << " pass_id=" << season.id << " cost=" << cost << " currency_type=" << season.premiumCurrencyType
			 << " client_request_id=" << clientRequestId;

	FillSnapshot(player, nowMs, *comp, season, tiers, *response.mutable_snapshot());
	return kSuccess;
}

uint32_t PlayerTravelPassSystem::ClaimReward(entt::entity player,
											 const ::ClaimTravelPassRewardRequest &request,
											 uint64_t nowMs,
											 ::ClaimTravelPassRewardResponse &response)
{
	response.Clear();
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return kThisEntityIsInvalid;
	}
	const uint64_t clientRequestId = request.client_request_id();
	if (clientRequestId == 0 || request.pass_id() == 0)
	{
		return kInvalidParameter;
	}

	// 1. 幂等查重。
	const std::string fingerprint = ClientRequestFingerprint(request);
	ClientRequestResult replayResult;
	const auto verdict = client_request_dedup::Check(player, clientRequestId, ClientRequestKind::kClaimTravelPassReward,
													 fingerprint, replayResult);
	if (verdict == ClientRequestVerdict::kMismatch)
	{
		return kInvalidParameter;
	}
	if (verdict == ClientRequestVerdict::kUnavailable)
	{
		return kServiceUnavailable;
	}
	if (verdict == ClientRequestVerdict::kReplay)
	{
		response.set_replayed(true);
		response.set_claimed_count(replayResult.travelPassClaimedCount);
		(void)GetState(player, nowMs, request.pass_id(), *response.mutable_snapshot());
		(void)PlayerBagSystem::BuildSnapshot(player, kInventory, *response.mutable_bag());
		return kSuccess;
	}

	// 2. 可写与战斗闸。
	if (!player_mutability::IsPlayerMutable(player))
	{
		return kTravelPassFrozen;
	}
	if (PlayerBattleSystem::IsInBattle(player))
	{
		return kTravelPassInBattle;
	}

	// 3. 解析期次与档位（已达成档位在档期结束后仍可永久补领）。
	ResolvedSeason season;
	std::vector<ResolvedTier> tiers;
	if (!FindSeasonById(request.pass_id(), season, tiers))
	{
		return kTravelPassNotOpen;
	}

	auto *comp = tlsEcs.actorRegistry.try_get<PlayerTravelPassComp>(player);
	auto *ledger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
	if (comp == nullptr || !travel_pass_rules::ValidateComp(*comp).empty())
	{
		return kTravelPassUnavailable;
	}
	if (ledger == nullptr || tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player) ||
		ledger->next_seq() == std::numeric_limits<uint64_t>::max())
	{
		return kServiceUnavailable;
	}

	const auto *existingProgress = travel_pass_rules::FindSeason(*comp, season.id);
	const uint32_t currentPoints =
		existingProgress != nullptr ? std::min(existingProgress->progress_points(), season.maxPoints) : 0u;
	const bool premiumUnlocked = existingProgress != nullptr && existingProgress->premium_unlocked();
	const uint64_t freeMask = existingProgress != nullptr ? existingProgress->free_claimed_mask() : 0ULL;
	const uint64_t premiumMask = existingProgress != nullptr ? existingProgress->premium_claimed_mask() : 0ULL;

	std::vector<ClaimTrackTarget> targets;
	if (request.tier_index() == 0)
	{
		// 一键领取全部已达成且未领取的免费轨 + 已解锁典藏轨。
		for (std::size_t i = 0; i < tiers.size(); ++i)
		{
			const auto &tier = tiers[i];
			if (currentPoints < tier.requiredPoints)
			{
				continue;
			}
			const bool hasFree = tier.freeItemId != 0 || tier.freeGold != 0 || !tier.freeCosmeticKey.empty();
			if (hasFree && !travel_pass_rules::IsTierClaimed(freeMask, tier.tierIndex))
			{
				targets.push_back(ClaimTrackTarget{i, false});
			}
			const bool hasPremium =
				tier.premiumItemId != 0 || tier.premiumGold != 0 || !tier.premiumCosmeticKey.empty();
			if (premiumUnlocked && hasPremium && !travel_pass_rules::IsTierClaimed(premiumMask, tier.tierIndex))
			{
				targets.push_back(ClaimTrackTarget{i, true});
			}
		}
		if (targets.empty())
		{
			return kTravelPassNothingToClaim;
		}
	}
	else
	{
		std::size_t foundPos = tiers.size();
		for (std::size_t i = 0; i < tiers.size(); ++i)
		{
			if (tiers[i].tierIndex == request.tier_index())
			{
				foundPos = i;
				break;
			}
		}
		if (foundPos == tiers.size())
		{
			return kInvalidParameter;
		}
		const auto &tier = tiers[foundPos];
		if (currentPoints < tier.requiredPoints)
		{
			return kTravelPassNotReached;
		}
		if (request.premium_track())
		{
			if (!premiumUnlocked)
			{
				return kTravelPassPremiumRequired;
			}
			const bool hasPremium =
				tier.premiumItemId != 0 || tier.premiumGold != 0 || !tier.premiumCosmeticKey.empty();
			if (!hasPremium || travel_pass_rules::IsTierClaimed(premiumMask, tier.tierIndex))
			{
				return kTravelPassNothingToClaim;
			}
			targets.push_back(ClaimTrackTarget{foundPos, true});
		}
		else
		{
			const bool hasFree = tier.freeItemId != 0 || tier.freeGold != 0 || !tier.freeCosmeticKey.empty();
			if (!hasFree || travel_pass_rules::IsTierClaimed(freeMask, tier.tierIndex))
			{
				return kTravelPassNothingToClaim;
			}
			targets.push_back(ClaimTrackTarget{foundPos, false});
		}
	}

	// 4. 聚合全部待发绑定物品、银两与零战力纪念外观键。
	std::map<uint32_t, uint32_t> mergedItems;
	uint64_t totalGold = 0;
	std::vector<std::string> cosmeticsToUnlock;
	for (const auto &target : targets)
	{
		const auto &tier = tiers[target.tierPos];
		const uint32_t itemId = target.premiumTrack ? tier.premiumItemId : tier.freeItemId;
		const uint32_t itemCount = target.premiumTrack ? tier.premiumItemCount : tier.freeItemCount;
		const uint64_t gold = target.premiumTrack ? tier.premiumGold : tier.freeGold;
		const std::string &cosmeticKey = target.premiumTrack ? tier.premiumCosmeticKey : tier.freeCosmeticKey;

		if (itemId != 0 && itemCount > 0)
		{
			if (GainBlockService::IsItemGainBlocked(player, itemId))
			{
				return kTravelPassItemBlocked;
			}
			uint32_t &acc = mergedItems[itemId];
			if (itemCount > std::numeric_limits<uint32_t>::max() - acc)
			{
				return kTravelPassUnavailable;
			}
			acc += itemCount;
		}
		if (gold > 0)
		{
			if (gold > static_cast<uint64_t>(std::numeric_limits<int64_t>::max()) - totalGold)
			{
				return kTravelPassUnavailable;
			}
			totalGold += gold;
		}
		if (!cosmeticKey.empty())
		{
			cosmeticsToUnlock.push_back(cosmeticKey);
		}
	}

	if (totalGold > 0 && CurrencySystem::CreditOverflows(player, kCurrencyGold, totalGold))
	{
		return kTravelPassUnavailable;
	}

	// 5. 预构造候选进度与幂等账本。
	PlayerTravelPassComp candidateComp = *comp;
	auto *targetSeason = travel_pass_rules::FindOrAddSeason(candidateComp, season.id);
	if (targetSeason == nullptr || candidateComp.pass_revision() == std::numeric_limits<uint64_t>::max())
	{
		return kTravelPassUnavailable;
	}
	for (const auto &target : targets)
	{
		const uint64_t bit = travel_pass_rules::TierBit(tiers[target.tierPos].tierIndex);
		if (target.premiumTrack)
		{
			targetSeason->set_premium_claimed_mask(targetSeason->premium_claimed_mask() | bit);
		}
		else
		{
			targetSeason->set_free_claimed_mask(targetSeason->free_claimed_mask() | bit);
		}
	}
	for (const auto &key : cosmeticsToUnlock)
	{
		if (!travel_pass_rules::AddCosmeticKey(*targetSeason, key))
		{
			return kTravelPassUnavailable;
		}
	}
	targetSeason->set_last_updated_at_ms(nowMs);
	candidateComp.set_pass_revision(candidateComp.pass_revision() + 1u);
	if (!travel_pass_rules::ValidateComp(candidateComp).empty())
	{
		return kTravelPassUnavailable;
	}

	const uint32_t claimedCount = static_cast<uint32_t>(targets.size());
	ClientRequestResult recordedResult;
	recordedResult.travelPassClaimedCount = claimedCount;
	ClientRequestLedgerComp candidateLedger = *ledger;
	if (!client_request_ledger::Remember(candidateLedger, clientRequestId, ClientRequestKind::kClaimTravelPassReward,
										 fingerprint, recordedResult) ||
		!client_request_ledger::Validate(candidateLedger).empty())
	{
		return kServiceUnavailable;
	}

	// 6. 原子入包（所有物品强制 bound = true）+ 入账银两 + 交换候选组件与幂等账本。
	const std::string beforeCompBytes = comp->SerializeAsString();
	const std::string beforeLedgerBytes = ledger->SerializeAsString();

	const auto commitStateAndGold = [&]() -> uint32_t {
		if (!player_mutability::IsPlayerMutable(player))
		{
			return kTravelPassFrozen;
		}
		if (PlayerBattleSystem::IsInBattle(player))
		{
			return kTravelPassInBattle;
		}
		auto *curComp = tlsEcs.actorRegistry.try_get<PlayerTravelPassComp>(player);
		auto *curLedger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
		if (curComp == nullptr || curLedger == nullptr ||
			tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player) ||
			curComp->SerializeAsString() != beforeCompBytes ||
			curLedger->SerializeAsString() != beforeLedgerBytes)
		{
			return kTravelPassUnavailable;
		}
		if (totalGold > 0)
		{
			const uint32_t goldErr = CurrencySystem::AddCurrency(
				player, kCurrencyGold, static_cast<int64_t>(totalGold), TX_TRAVEL_PASS_REWARD, clientRequestId);
			if (goldErr != kSuccess)
			{
				return goldErr == kAssetBlocked ? uint32_t{kTravelPassItemBlocked} : uint32_t{kTravelPassUnavailable};
			}
		}
		curComp->Swap(&candidateComp);
		curLedger->Swap(&candidateLedger);
		return kSuccess;
	};

	if (!mergedItems.empty())
	{
		std::vector<InitItemParam> gained;
		gained.reserve(mergedItems.size());
		for (const auto &[configId, count] : mergedItems)
		{
			InitItemParam param;
			param.itemPBComp.set_config_id(configId);
			param.itemPBComp.set_size(count);
			param.itemPBComp.set_item_id(kInvalidGuid);
			param.itemPBComp.set_bound(true); // 强制仅自用绑定，绝不污染玩家市场。
			gained.push_back(std::move(param));
		}
		std::vector<GuidVector> written;
		const uint32_t exchangeErr = BagService::ExchangeInventoryExact(
			player, {}, gained, {}, clientRequestId, ClaimExtraJson(season.id, claimedCount, totalGold), written,
			[&](const std::vector<GuidVector> &) -> uint32_t { return commitStateAndGold(); });
		if (exchangeErr != kSuccess)
		{
			return MapBagError(exchangeErr);
		}
	}
	else
	{
		const uint32_t err = commitStateAndGold();
		if (err != kSuccess)
		{
			return err;
		}
	}

	PlayerLifecycleSystem::RequestKeyProgressSave(player, "travel_pass_claim");

	LOG_INFO << "[TravelPass] metric=travel_pass_reward_claimed " << PlayerLogId(player)
			 << " pass_id=" << season.id << " claimed_tracks=" << claimedCount
			 << " item_lines=" << mergedItems.size() << " gold=" << totalGold
			 << " client_request_id=" << clientRequestId;

	response.set_claimed_count(claimedCount);
	FillSnapshot(player, nowMs, *comp, season, tiers, *response.mutable_snapshot());
	if (const uint32_t bagErr = PlayerBagSystem::BuildSnapshot(player, kInventory, *response.mutable_bag());
		bagErr != kSuccess)
	{
		response.clear_bag();
		LOG_ERROR << "[TravelPass] Claim succeeded but bag snapshot failed: " << PlayerLogId(player)
				  << " err=" << bagErr;
	}
	return kSuccess;
}

uint32_t PlayerTravelPassSystem::EquipCosmetic(entt::entity player,
											   uint64_t nowMs,
											   const ::EquipTravelPassCosmeticRequest &request,
											   ::EquipTravelPassCosmeticResponse &response)
{
	response.Clear();
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return kThisEntityIsInvalid;
	}
	if (request.cosmetic_slot() != travel_pass_rules::kCosmeticSlotFrame &&
		request.cosmetic_slot() != travel_pass_rules::kCosmeticSlotTitle)
	{
		return kInvalidParameter;
	}
	if (!player_mutability::IsPlayerMutable(player))
	{
		return kTravelPassFrozen;
	}
	auto *comp = tlsEcs.actorRegistry.try_get<PlayerTravelPassComp>(player);
	if (comp == nullptr || !travel_pass_rules::ValidateComp(*comp).empty())
	{
		return kTravelPassUnavailable;
	}

	const std::string &key = request.cosmetic_key();
	if (!key.empty())
	{
		const bool validForSlot =
			request.cosmetic_slot() == travel_pass_rules::kCosmeticSlotFrame
				? travel_pass_rules::IsFrameKey(key)
				: travel_pass_rules::IsTitleKey(key);
		if (!validForSlot)
		{
			return kInvalidParameter;
		}
		if (!travel_pass_rules::HasCosmeticKey(*comp, key))
		{
			return kTravelPassCosmeticNotOwned;
		}
	}

	if (request.cosmetic_slot() == travel_pass_rules::kCosmeticSlotFrame)
	{
		comp->set_equipped_frame_key(key);
	}
	else
	{
		comp->set_equipped_title_key(key);
	}
	if (comp->pass_revision() < std::numeric_limits<uint64_t>::max())
	{
		comp->set_pass_revision(comp->pass_revision() + 1u);
	}
	PlayerLifecycleSystem::RequestKeyProgressSave(player, "travel_pass_cosmetic");

	LOG_INFO << "[TravelPass] metric=travel_pass_cosmetic_equipped " << PlayerLogId(player)
			 << " slot=" << request.cosmetic_slot() << " key=" << (key.empty() ? "none" : key);

	(void)GetState(player, nowMs, 0u, *response.mutable_snapshot());
	return kSuccess;
}

uint32_t PlayerTravelPassSystem::SelectActivePass(entt::entity player,
												  uint64_t nowMs,
												  const ::SelectActiveTravelPassRequest &request,
												  ::SelectActiveTravelPassResponse &response)
{
	response.Clear();
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return kThisEntityIsInvalid;
	}
	if (request.pass_id() == 0u)
	{
		return kInvalidParameter;
	}
	if (!player_mutability::IsPlayerMutable(player))
	{
		return kTravelPassFrozen;
	}
	auto *comp = tlsEcs.actorRegistry.try_get<PlayerTravelPassComp>(player);
	if (comp == nullptr || !travel_pass_rules::ValidateComp(*comp).empty())
	{
		return kTravelPassUnavailable;
	}

	ResolvedSeason season;
	std::vector<ResolvedTier> tiers;
	if (!FindSeasonById(request.pass_id(), season, tiers))
	{
		return kTravelPassNotOpen;
	}
	const bool windowOpen =
		season.enabled != 0 && travel_pass_rules::IsWindowOpen(season.startsAtMs, season.endsAtMs, nowMs);
	const auto *existing = travel_pass_rules::FindSeason(*comp, season.id);
	const bool unlockedCarryover = existing != nullptr && existing->premium_unlocked();
	if (!windowOpen && !unlockedCarryover)
	{
		return kTravelPassNotOpen;
	}

	if (comp->active_pass_id() != season.id)
	{
		comp->set_active_pass_id(season.id);
		if (comp->pass_revision() < std::numeric_limits<uint64_t>::max())
		{
			comp->set_pass_revision(comp->pass_revision() + 1u);
		}
		PlayerLifecycleSystem::RequestKeyProgressSave(player, "travel_pass_select_active");
		LOG_INFO << "[TravelPass] metric=travel_pass_active_selected " << PlayerLogId(player)
				 << " active_pass_id=" << season.id;
	}
	FillSnapshot(player, nowMs, *comp, season, tiers, *response.mutable_snapshot());
	return kSuccess;
}

bool PlayerTravelPassSystem::GrantBonusCosmetic(entt::entity player, std::string_view cosmeticKey)
{
	if (cosmeticKey.empty())
	{
		return true;
	}
	if (!travel_pass_rules::IsValidCosmeticKey(cosmeticKey))
	{
		return false;
	}
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return false;
	}
	auto *comp = tlsEcs.actorRegistry.try_get<PlayerTravelPassComp>(player);
	if (comp == nullptr)
	{
		return true; // 仅极简旧单测夹具无此组件；生产玩家加载必然携带 PlayerTravelPassComp
	}
	bool added = false;
	if (!travel_pass_rules::AddBonusCosmeticKey(*comp, cosmeticKey, &added))
	{
		return false;
	}
	if (added && comp->pass_revision() < std::numeric_limits<uint64_t>::max())
	{
		comp->set_pass_revision(comp->pass_revision() + 1u);
	}
	return true;
}

bool PlayerTravelPassSystem::HasPremiumUnlocked(entt::entity player, uint32_t passId)
{
	if (!tlsEcs.actorRegistry.valid(player) || passId == 0)
	{
		return false;
	}
	const auto *comp = tlsEcs.actorRegistry.try_get<PlayerTravelPassComp>(player);
	if (comp == nullptr)
	{
		return false;
	}
	const auto *existing = travel_pass_rules::FindSeason(*comp, passId);
	return existing != nullptr && existing->premium_unlocked();
}

uint32_t PlayerTravelPassSystem::GrantPremiumFromGift(entt::entity player, uint32_t passId, uint64_t nowMs,
													  bool *alreadyOwned)
{
	if (alreadyOwned != nullptr)
	{
		*alreadyOwned = false;
	}
	if (!tlsEcs.actorRegistry.valid(player) || passId == 0)
	{
		return kInvalidParameter;
	}
	auto *comp = tlsEcs.actorRegistry.try_get<PlayerTravelPassComp>(player);
	if (comp == nullptr || !travel_pass_rules::ValidateComp(*comp).empty())
	{
		return kTravelPassUnavailable;
	}
	ResolvedSeason season;
	std::vector<ResolvedTier> tiers;
	if (!FindSeasonById(passId, season, tiers) || season.enabled == 0)
	{
		return kTravelPassNotOpen;
	}
	auto *prog = travel_pass_rules::FindOrAddSeason(*comp, passId);
	if (prog == nullptr)
	{
		return kTravelPassUnavailable;
	}
	if (prog->premium_unlocked())
	{
		if (alreadyOwned != nullptr)
		{
			*alreadyOwned = true;
		}
		return kSuccess;
	}
	prog->set_premium_unlocked(true);
	prog->set_premium_unlocked_at_ms(nowMs);
	prog->set_last_updated_at_ms(nowMs);
	if (comp->pass_revision() < std::numeric_limits<uint64_t>::max())
	{
		comp->set_pass_revision(comp->pass_revision() + 1u);
	}
	LOG_INFO << "[TravelPass] metric=travel_pass_gift_credited " << PlayerLogId(player) << " pass_id=" << passId;
	return kSuccess;
}

bool PlayerTravelPassSystem::RevokePremiumFromChargeback(entt::entity player, uint32_t passId, uint64_t nowMs)
{
	if (!tlsEcs.actorRegistry.valid(player) || passId == 0)
	{
		return false;
	}
	auto *comp = tlsEcs.actorRegistry.try_get<PlayerTravelPassComp>(player);
	if (comp == nullptr)
	{
		return false;
	}
	auto *prog = travel_pass_rules::FindMutableSeason(*comp, passId);
	if (prog == nullptr || !prog->premium_unlocked())
	{
		return true;
	}
	prog->set_premium_unlocked(false);
	prog->set_premium_unlocked_at_ms(0u);
	prog->set_last_updated_at_ms(nowMs);
	if (comp->pass_revision() < std::numeric_limits<uint64_t>::max())
	{
		comp->set_pass_revision(comp->pass_revision() + 1u);
	}
	LOG_WARN << "[TravelPass] metric=travel_pass_gift_revoked " << PlayerLogId(player) << " pass_id=" << passId;
	return true;
}

void PlayerTravelPassSystem::AppendActivityCatalogEntries(entt::entity player, uint64_t nowMs,
														  ::GetActivityListResponse &out)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}

	// 1. 主打拉收入活动卡：「青云游历手记」（仅在玩家有持久组件且配置有效时追加）。
	if (tlsEcs.actorRegistry.try_get<PlayerTravelPassComp>(player) != nullptr)
	{
		TravelPassSnapshot snap;
		if (GetState(player, nowMs, 0u, snap) == kSuccess && snap.available())
		{
			auto *entry = out.add_activities();
			entry->set_activity_id(travel_pass_rules::kActivityIdTravelPass);
			entry->set_name(snap.name());
			uint32_t claimableTotal = 0u;
			for (const auto &book : snap.bookshelf())
			{
				claimableTotal += book.claimable_count();
			}
			if (claimableTotal == 0u)
			{
				claimableTotal = snap.claimable_free_count() + snap.claimable_premium_count();
			}
			if (claimableTotal > 0)
			{
				entry->set_description("有 " + std::to_string(claimableTotal) + " 档游历好礼待领取 · 免费轨可领，典藏轨可先玩满再补票");
			}
			else if (snap.premium_unlocked())
			{
				entry->set_description("典藏手记已解锁（永不过期）· 正常打试炼/会武自动涨游历印，无每日打卡");
			}
			else
			{
				entry->set_description("无每日打卡、正常战斗自动涨点；免费领定星保底符，满点后可自愿 180 灵石领典藏外观");
			}
			entry->set_icon_key(snap.icon_key());
			entry->set_status(PLAYER_ACTIVITY_OPEN);
			entry->set_starts_at_ms(snap.starts_at_ms());
			entry->set_ends_at_ms(snap.ends_at_ms());
			entry->set_can_participate(true);
			entry->set_definition_key("activity.travel_pass." + std::to_string(snap.pass_id()));
			entry->set_configuration_revision(snap.config_revision());
			entry->set_category(PLAYER_ACTIVITY_CATEGORY_BENEFIT);
			entry->add_reward_types(PLAYER_ACTIVITY_REWARD_ITEM);
			entry->set_entry_kind(PLAYER_ACTIVITY_ENTRY_TRAVEL_PASS);
			entry->set_completion_count_known(true);
			entry->set_completed_count(snap.progress_points());
			entry->set_completion_limit(snap.max_points());
			entry->set_completion_period("整期累计 · 无日清");
			// 仅当有免费或已解锁典藏可立即领取时亮红点；绝不为未购典藏刷焦虑红点。
			entry->set_has_claimable_reward(claimableTotal > 0);

			// 预览 2 件代表奖励：免费定星保底符 (3405) + 典藏角色形象卡 (3402)。
			const uint32_t previewIds[2] = {3405u, 3402u};
			for (const uint32_t itemId : previewIds)
			{
				const auto [itemRow, itemErr] = ItemTableManager::Instance().FindByIdSilent(itemId);
				if (itemRow == nullptr)
				{
					continue;
				}
				auto *r = entry->add_rewards();
				r->set_item_config_id(itemId);
				r->set_count(itemId == 3402u ? 2u : 1u);
				r->set_name(itemRow->name());
				r->set_icon_key(itemRow->icon_key());
				r->set_binding_description("绑定仅自用");
			}
		}
	}

	// 2. 免费体验种草卡：「留灯风物 · 免费试穿」（复用 1 小时免费试用 + 永久衣柜）。
	if (tlsEcs.actorRegistry.try_get<PlayerProfileComp>(player) != nullptr &&
		!CharacterAppearanceTableManager::Instance().FindAll().data().empty())
	{
		PlayerAppearanceInfo appearanceInfo;
		PlayerAppearanceSystem::FillInfo(player, nowMs / 1000ULL, appearanceInfo);
		uint32_t totalCount = 0;
		uint32_t ownedCount = 0;
		uint32_t trialAvailableCount = 0;
		for (const auto &app : appearanceInfo.appearances())
		{
			++totalCount;
			if (app.owned())
			{
				++ownedCount;
			}
			else if (!app.trial_used())
			{
				++trialAvailableCount;
			}
		}
		if (totalCount > 0)
		{
			auto *entry = out.add_activities();
			entry->set_activity_id(travel_pass_rules::kActivityIdAppearanceShowcase);
			entry->set_name("留灯风物 · 免费试穿");
			if (trialAvailableCount > 0)
			{
				entry->set_description("还有 " + std::to_string(trialAvailableCount) + " 位角色可免费试穿 1 小时，阅读人物志并随时自由切换（0 数值加成）");
			}
			else
			{
				entry->set_description("永久收藏衣柜与人物志合篇 · 已拥有形象随时免费切换，0 战力加成");
			}
			entry->set_icon_key("UI/Ugui/TravelPassV1/showcase_appearance_icon");
			entry->set_status(PLAYER_ACTIVITY_OPEN);
			entry->set_can_participate(true);
			entry->set_definition_key("activity.showcase.appearance.v1");
			entry->set_configuration_revision(1);
			entry->set_category(PLAYER_ACTIVITY_CATEGORY_BENEFIT);
			entry->add_reward_types(PLAYER_ACTIVITY_REWARD_ITEM);
			entry->set_entry_kind(PLAYER_ACTIVITY_ENTRY_APPEARANCE_SHOWCASE);
			entry->set_completion_count_known(true);
			entry->set_completed_count(ownedCount);
			entry->set_completion_limit(totalCount);
			entry->set_completion_period("常驻体验 · 零强迫");
			entry->set_has_claimable_reward(false);
		}
	}

	// 3. 低打扰官方直购导览卡：「灵石百宝阁 · 轻便利」（仅在货架存在可售商品时显示，零红点）。
	if (tlsEcs.actorRegistry.try_get<PlayerSpiritShopComp>(player) != nullptr &&
		!SpiritShopGoodsTableManager::Instance().FindAll().data().empty())
	{
		SpiritShopInfo shopInfo;
		if (PlayerSpiritShopSystem::GetInfo(player, nowMs, shopInfo) == kSuccess && shopInfo.goods_size() > 0)
		{
			uint32_t starterRemaining = 0;
			uint32_t starterLimit = 0;
			for (const auto &g : shopInfo.limit_groups())
			{
				if (g.group_id() == 1u)
				{
					starterLimit = g.max_weight();
					starterRemaining = g.max_weight() > g.used_weight() ? g.max_weight() - g.used_weight() : 0u;
					break;
				}
			}
			auto *entry = out.add_activities();
			entry->set_activity_id(travel_pass_rules::kActivityIdSpiritShopShowcase);
			entry->set_name("灵石百宝阁 · 轻便利");
			entry->set_description("明码标价 6–30 元绑定提速包、定星保底符、角色形象卡与轻享卡，0 战力上限加成");
			entry->set_icon_key("UI/Ugui/TravelPassV1/showcase_spirit_shop_icon");
			entry->set_status(PLAYER_ACTIVITY_OPEN);
			entry->set_can_participate(true);
			entry->set_definition_key("activity.showcase.spirit_shop.v1");
			entry->set_configuration_revision(1);
			entry->set_category(PLAYER_ACTIVITY_CATEGORY_BENEFIT);
			entry->add_reward_types(PLAYER_ACTIVITY_REWARD_ITEM);
			entry->set_entry_kind(PLAYER_ACTIVITY_ENTRY_SPIRIT_SHOP_SHOWCASE);
			entry->set_completion_count_known(starterLimit > 0);
			entry->set_completed_count(starterLimit > starterRemaining ? starterLimit - starterRemaining : 0u);
			entry->set_completion_limit(starterLimit);
			entry->set_completion_period("终身限额 · 按需自愿");
			entry->set_has_claimable_reward(false);
		}
	}
}
