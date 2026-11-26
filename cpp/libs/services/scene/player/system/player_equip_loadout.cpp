#include "player_equip_loadout.h"

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <limits>
#include <map>
#include <set>
#include <string>
#include <unordered_map>
#include <utility>
#include <vector>

#include <muduo/base/Logging.h>

#include "battle/system/player_battle.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/client_request_ledger.h"
#include "player/system/equip_attribute_rules.h"
#include "player/system/equip_loadout_rules.h"
#include "player/system/player_attribute.h"
#include "player/system/player_feature_snapshot.h"
#include "player/system/player_level_rules.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "player/system/player_skill.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/player_attribute_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_equip_loadout_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "table/code/item_table.h"
#include "table/proto/tip/attribute_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/equip_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{

struct EquipLoadoutPersistRequestComp
{
	std::chrono::steady_clock::time_point requested{};
};

PlayerEquipLoadoutSystem::PersistFn gPersistFn = &PlayerLifecycleSystem::SavePlayerForAssetCommit;

PlayerEquipLoadoutSystem::NameFilterFn &NameFilterSlot()
{
	static PlayerEquipLoadoutSystem::NameFilterFn slot;
	return slot;
}

const ItemTable *ItemRow(uint32_t configId)
{
	return configId == 0 ? nullptr : ItemTableManager::Instance().FindByIdSilent(configId).first;
}

bool IsCarriable(const ItemComp &item, const ItemTable &row)
{
	return row.max_stack_size() == 1 && item.size() == 1;
}

uint32_t PlayerLevel(entt::entity player)
{
	return playerlevel::CappedLevel(tlsEcs.actorRegistry.get<LevelComp>(player).level());
}

uint32_t PlayerClassId(entt::entity player)
{
	return tlsEcs.actorRegistry.get<PlayerUint32Comp>(player).class_();
}

uint32_t CheckRequiredInputs(entt::entity player)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return kEntityIsNull;
	}
	if (!tlsEcs.actorRegistry.all_of<PlayerEquipLoadoutComp, PlayerBagsComp, LevelComp, PlayerUint32Comp>(player))
	{
		LOG_ERROR << "[PlayerEquipLoadout] Required persisted component missing: player_id="
				  << tlsEcs.GetPlayerId(player);
		return kServiceUnavailable;
	}
	return kSuccess;
}

uint32_t CheckWritable(entt::entity player)
{
	if (const auto err = CheckRequiredInputs(player); err != kSuccess)
	{
		return err;
	}
	if (const auto mutability = player_mutability::ClassifyPlayerMutability(player);
		mutability != player_mutability::PlayerMutability::kMutable)
	{
		LOG_WARN << "[PlayerEquipLoadout] Rejected: player not mutable reason="
				 << player_mutability::PlayerMutabilityName(mutability)
				 << " player_id=" << tlsEcs.GetPlayerId(player);
		return kEquipFrozen;
	}
	if (PlayerBattleSystem::IsInBattle(player))
	{
		return kEquipInBattle;
	}
	return kSuccess;
}

const EquipLoadoutScheme *FindScheme(const PlayerEquipLoadoutComp &comp, uint32_t schemeId)
{
	for (const auto &scheme : comp.schemes())
	{
		if (scheme.scheme_id() == schemeId)
		{
			return &scheme;
		}
	}
	return nullptr;
}

EquipLoadoutScheme *FindOrCreateScheme(PlayerEquipLoadoutComp &comp, uint32_t schemeId)
{
	for (auto &scheme : *comp.mutable_schemes())
	{
		if (scheme.scheme_id() == schemeId)
		{
			return &scheme;
		}
	}
	auto *created = comp.add_schemes();
	created->set_scheme_id(schemeId);
	created->set_name(equiploadout::DefaultSchemeName(schemeId));
	created->set_role_tag(0);
	return created;
}

uint32_t HighestQualityTierOf(const ItemComp &item)
{
	if (!item.has_equip())
	{
		return 0;
	}
	uint32_t best = 0;
	for (const auto &affix : item.equip().fixed_affixes())
	{
		best = std::max(best, affix.quality_tier());
	}
	return best;
}

void FillSlotViewItemMeta(const ItemComp &item, EquipLoadoutSlotView &view)
{
	view.set_config_id(item.config_id());
	if (const auto *row = ItemRow(item.config_id()); row != nullptr)
	{
		view.set_item_name(row->name());
		view.set_icon_key(row->icon_key());
	}
	if (item.has_equip())
	{
		view.set_enhance_level(item.equip().enhance_level());
		view.set_quality_tier(HighestQualityTierOf(item));
	}
}

bool AttributeSchemeExists(entt::entity player, uint32_t schemeId)
{
	const auto *attrComp = tlsEcs.actorRegistry.try_get<PlayerAttributeComp>(player);
	if (attrComp == nullptr)
	{
		return false;
	}
	for (const auto &s : attrComp->schemes())
	{
		if (s.scheme_id() == schemeId)
		{
			return true;
		}
	}
	return false;
}

bool PutBack(Bag &bag, const ItemComp &item, uint32_t preferredSlot, const char *bagName, entt::entity player)
{
	if (preferredSlot != kInvalidU32Id && bag.PutInstance(item, preferredSlot) == kSuccess)
	{
		return true;
	}
	if (bag.PutInstance(item) == kSuccess)
	{
		LOG_ERROR << "[PlayerEquipLoadout] Rollback placed item in alternate slot: player_id="
				  << tlsEcs.GetPlayerId(player) << " bag=" << bagName << " guid=" << item.item_id()
				  << " preferred_slot=" << preferredSlot;
		return true;
	}
	LOG_ERROR << "[PlayerEquipLoadout][INSTANCE_LOST] Rollback failed; manual compensation required: player_id="
			  << tlsEcs.GetPlayerId(player) << " bag=" << bagName << " guid=" << item.item_id()
			  << " config=" << item.config_id();
	return false;
}

uint32_t ValidateEquipmentCandidate(entt::entity player, const Bag &equipment, const ItemComp &item,
									uint32_t targetSlot)
{
	const ItemTable *row = ItemRow(item.config_id());
	if (row == nullptr || row->equip_kind() == 0 || row->max_stack_size() != 1)
	{
		return kEquipNotEquipment;
	}
	if (!equiprules::IsKnownEquipmentProfile(row->equip_profile()))
	{
		return kFeatureUnavailable;
	}
	if (!IsCarriable(item, *row))
	{
		return kEquipItemNotFound;
	}
	if (PlayerLevel(player) < row->equip_level())
	{
		return kEquipLevelNotEnough;
	}
	if (row->equip_class() != 0 && row->equip_class() != PlayerClassId(player))
	{
		return kEquipClassMismatch;
	}
	const auto slots = equipment.SlotsAcceptingKind(row->equip_kind());
	if (std::find(slots.begin(), slots.end(), targetSlot) == slots.end())
	{
		return kEquipLoadoutSlotUnavailable;
	}
	return kSuccess;
}

bool IsUnlockDurable(entt::entity player, uint32_t unlockedCount)
{
	const auto *persisted = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
	const auto *identity = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
	const auto *owner = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player);
	if (unlockedCount < 2u || identity == nullptr || !player_mutability::HasFencedOwnership(player) ||
		owner == nullptr || persisted == nullptr || !persisted->IsCommittedForOwner(owner->epoch))
	{
		return false;
	}
	const auto &saved = persisted->snapshot->player_database_data();
	return saved.player_id() == identity->value &&
		   saved.equip_loadout_component().unlocked_scheme_count() >= unlockedCount;
}

void RequestUnlockPersist(entt::entity player, bool force)
{
	if (!player_mutability::IsPlayerMutable(player))
	{
		return;
	}
	auto &state = tlsEcs.actorRegistry.get_or_emplace<EquipLoadoutPersistRequestComp>(player);
	const auto now = std::chrono::steady_clock::now();
	if (!force && state.requested != std::chrono::steady_clock::time_point{} &&
		now - state.requested < std::chrono::seconds(1))
	{
		return;
	}
	state.requested = now;
	if (gPersistFn != nullptr)
	{
		gPersistFn(player);
	}
}

void FillUnlockAppliedReply(entt::entity player, const ClientRequestResult &result, bool replayed,
							UnlockEquipLoadoutResponse &response)
{
	response.set_outcome(UnlockEquipLoadoutResponse::APPLIED);
	response.set_replayed(replayed);
	response.set_unlocked_scheme_count(result.equipLoadoutUnlockedCount);
	response.set_used_item_config_id(result.equipLoadoutUsedItemId);
	response.set_spent_bind_diamond(result.equipLoadoutSpentBindDiamond);
	response.set_spent_diamond(result.equipLoadoutSpentDiamond);
	response.set_durable(IsUnlockDurable(player, result.equipLoadoutUnlockedCount));
	if (const auto snapErr = PlayerEquipLoadoutSystem::BuildSnapshot(player, *response.mutable_snapshot());
		snapErr != kSuccess)
	{
		response.clear_snapshot();
		response.mutable_error_message()->set_id(snapErr);
	}
	if (const auto bagErr = PlayerBagSystem::BuildSnapshot(player, kInventory, *response.mutable_bag());
		bagErr != kSuccess)
	{
		response.clear_bag();
		if (!response.has_error_message())
		{
			response.mutable_error_message()->set_id(bagErr);
		}
	}
	if (!response.durable())
	{
		RequestUnlockPersist(player, !replayed);
	}
}

std::string SkillSchemeDisplayName(uint32_t schemeId)
{
	switch (schemeId)
	{
	case 1u: return "战法方案一";
	case 2u: return "战法方案二";
	case 3u: return "战法方案三";
	case 4u: return "战法方案四";
	case 5u: return "战法方案五";
	case 6u: return "战法方案六";
	default: return "战法方案" + std::to_string(schemeId);
	}
}

} // namespace

void PlayerEquipLoadoutSystem::SetPersistForTest(PersistFn fn)
{
	gPersistFn = fn != nullptr ? fn : &PlayerLifecycleSystem::SavePlayerForAssetCommit;
}

void PlayerEquipLoadoutSystem::SetNameFilterForTest(NameFilterFn fn)
{
	NameFilterSlot() = std::move(fn);
}

uint32_t PlayerEquipLoadoutSystem::BuildSnapshot(entt::entity player, EquipLoadoutSnapshot &snapshot)
{
	snapshot.Clear();
	if (const auto err = CheckRequiredInputs(player); err != kSuccess)
	{
		return err;
	}
	const auto &comp = tlsEcs.actorRegistry.get<PlayerEquipLoadoutComp>(player);
	const auto &bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(player);
	const Bag &inventory = bags.bags[kInventory];
	const Bag &equipment = bags.bags[kEquipment];

	const uint32_t unlocked = equiploadout::EffectiveUnlockedCount(comp.unlocked_scheme_count());
	const uint32_t active = equiploadout::EffectiveActiveSchemeId(comp.active_scheme_id(), unlocked);
	const uint64_t version = equiploadout::EffectiveStateVersion(comp.state_version());

	snapshot.set_unlocked_scheme_count(unlocked);
	snapshot.set_max_scheme_count(equiploadout::kMaxSchemes);
	snapshot.set_active_scheme_id(active);
	snapshot.set_state_version(version);
	snapshot.set_unlock_item_config_id(equiploadout::kUnlockItemConfigId);
	snapshot.set_unlock_item_owned(BagService::RemovableCount(player, inventory, equiploadout::kUnlockItemConfigId));

	if (unlocked < equiploadout::kMaxSchemes)
	{
		const auto nextSpec = equiploadout::UnlockSpecForTarget(unlocked + 1u);
		snapshot.set_next_unlock_cost_diamond(nextSpec.costDiamond);
		snapshot.set_next_unlock_allow_bind_diamond(nextSpec.allowBindDiamond);
	}
	else
	{
		snapshot.set_next_unlock_cost_diamond(0);
		snapshot.set_next_unlock_allow_bind_diamond(false);
	}

	for (uint32_t schemeId = 1u; schemeId <= equiploadout::kMaxSchemes; ++schemeId)
	{
		auto *info = snapshot.add_schemes();
		info->set_scheme_id(schemeId);
		info->set_unlocked(schemeId <= unlocked);

		const auto *stored = FindScheme(comp, schemeId);
		info->set_name(stored != nullptr && !stored->name().empty() ? stored->name()
																	: equiploadout::DefaultSchemeName(schemeId));
		info->set_role_tag(stored != nullptr ? stored->role_tag() : 0u);
		info->set_linked_skill_scheme_id(stored != nullptr ? stored->linked_skill_scheme_id() : 0u);
		info->set_linked_attribute_scheme_id(stored != nullptr ? stored->linked_attribute_scheme_id() : 0u);

		std::map<uint32_t, uint64_t> boundBySlot;
		if (stored != nullptr)
		{
			for (const auto &binding : stored->slots())
			{
				if (equiploadout::IsFormalSlot(binding.slot()) && binding.item_guid() != 0)
				{
					boundBySlot[binding.slot()] = binding.item_guid();
				}
			}
		}

		for (const uint32_t slot : equiploadout::kFormalSlots)
		{
			auto *slotView = info->add_slots();
			slotView->set_slot(slot);
			const auto it = boundBySlot.find(slot);
			if (it == boundBySlot.end() || it->second == 0)
			{
				slotView->set_status(EQUIP_LOADOUT_SLOT_EMPTY);
				continue;
			}
			const uint64_t guid = it->second;
			slotView->set_item_guid(guid);
			if (const ItemComp *worn = equipment.GetItemCompByGuid(guid); worn != nullptr)
			{
				slotView->set_status(EQUIP_LOADOUT_SLOT_EQUIPPED);
				FillSlotViewItemMeta(*worn, *slotView);
			}
			else if (const ItemComp *inBag = inventory.GetItemCompByGuid(guid); inBag != nullptr)
			{
				slotView->set_status(EQUIP_LOADOUT_SLOT_IN_BAG);
				FillSlotViewItemMeta(*inBag, *slotView);
			}
			else
			{
				slotView->set_status(EQUIP_LOADOUT_SLOT_MISSING_OR_STORED);
			}
		}
	}

	const uint32_t skillSchemes = PlayerSkillSystem::UnlockedLoadoutSchemeCount(player);
	for (uint32_t sid = 1u; sid <= skillSchemes; ++sid)
	{
		auto *opt = snapshot.add_skill_schemes();
		opt->set_scheme_id(sid);
		opt->set_name(SkillSchemeDisplayName(sid));
	}
	if (const auto *attrComp = tlsEcs.actorRegistry.try_get<PlayerAttributeComp>(player); attrComp != nullptr)
	{
		for (const auto &s : attrComp->schemes())
		{
			auto *opt = snapshot.add_attribute_schemes();
			opt->set_scheme_id(s.scheme_id());
			opt->set_name(s.name().empty() ? ("加点方案" + std::to_string(s.scheme_id())) : s.name());
		}
	}

	return kSuccess;
}

uint32_t PlayerEquipLoadoutSystem::Get(entt::entity player, GetEquipLoadoutsResponse &response)
{
	response.Clear();
	const auto err = BuildSnapshot(player, *response.mutable_snapshot());
	if (err != kSuccess)
	{
		response.clear_snapshot();
		response.mutable_error_message()->set_id(err);
	}
	return err;
}

void PlayerEquipLoadoutSystem::Save(entt::entity player, const SaveEquipLoadoutRequest &request,
									SaveEquipLoadoutResponse &response)
{
	response.Clear();
	response.set_request_id(request.request_id());

	const auto finish = [&](uint32_t errorTip) {
		if (errorTip != kSuccess)
		{
			response.mutable_error_message()->set_id(errorTip);
		}
		if (const auto snapErr = BuildSnapshot(player, *response.mutable_snapshot()); snapErr != kSuccess)
		{
			response.clear_snapshot();
			if (errorTip == kSuccess)
			{
				response.mutable_error_message()->set_id(snapErr);
			}
		}
	};

	if (const auto err = CheckRequiredInputs(player); err != kSuccess)
	{
		response.mutable_error_message()->set_id(err);
		return;
	}
	if (!equiploadout::ValidRequestId(request.request_id()))
	{
		finish(kInvalidParameter);
		return;
	}

	auto &comp = tlsEcs.actorRegistry.get<PlayerEquipLoadoutComp>(player);
	if (comp.last_request_id() == request.request_id())
	{
		const bool same = comp.last_base_version() == request.state_version() &&
						  comp.last_scheme_id() == request.scheme_id();
		if (!same)
		{
			finish(kInvalidParameter);
			return;
		}
		response.set_replayed(true);
		finish(kSuccess);
		return;
	}

	if (const auto err = CheckWritable(player); err != kSuccess)
	{
		finish(err);
		return;
	}

	const uint32_t unlocked = equiploadout::EffectiveUnlockedCount(comp.unlocked_scheme_count());
	if (!equiploadout::ValidSchemeId(request.scheme_id(), unlocked))
	{
		finish(kEquipLoadoutSchemeInvalid);
		return;
	}
	const uint64_t currentVersion = equiploadout::EffectiveStateVersion(comp.state_version());
	if (request.state_version() != currentVersion || comp.state_version() == std::numeric_limits<uint64_t>::max())
	{
		finish(kEquipLoadoutVersionChanged);
		return;
	}
	if (!equiploadout::ValidRoleTag(request.role_tag()))
	{
		finish(kInvalidParameter);
		return;
	}

	std::string finalName = request.name().empty() ? equiploadout::DefaultSchemeName(request.scheme_id())
												   : request.name();
	if (!request.name().empty() && !equiploadout::ValidateSchemeName(finalName, NameFilterSlot()))
	{
		finish(kEquipLoadoutNameInvalid);
		return;
	}

	if (request.linked_skill_scheme_id() != 0 &&
		request.linked_skill_scheme_id() > PlayerSkillSystem::UnlockedLoadoutSchemeCount(player))
	{
		finish(kInvalidParameter);
		return;
	}
	if (request.linked_attribute_scheme_id() != 0 &&
		!AttributeSchemeExists(player, request.linked_attribute_scheme_id()))
	{
		finish(kInvalidParameter);
		return;
	}

	const auto &bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(player);
	const Bag &inventory = bags.bags[kInventory];
	const Bag &equipment = bags.bags[kEquipment];

	std::map<uint32_t, uint64_t> validatedSlots;
	if (request.capture_current_equipped())
	{
		for (const uint32_t slot : equiploadout::kFormalSlots)
		{
			const Guid wornGuid = equipment.Layout().At(slot);
			if (wornGuid != kInvalidGuid)
			{
				validatedSlots[slot] = wornGuid;
			}
		}
	}
	else
	{
		std::set<uint64_t> seenGuids;
		for (const auto &binding : request.slots())
		{
			if (!equiploadout::IsFormalSlot(binding.slot()))
			{
				finish(kEquipLoadoutSlotUnavailable);
				return;
			}
			if (validatedSlots.contains(binding.slot()))
			{
				finish(kInvalidParameter);
				return;
			}
			if (binding.item_guid() == 0)
			{
				validatedSlots[binding.slot()] = 0;
				continue;
			}
			if (!seenGuids.insert(binding.item_guid()).second)
			{
				finish(kEquipLoadoutDuplicateItem);
				return;
			}
			const ItemComp *item = equipment.GetItemCompByGuid(binding.item_guid());
			if (item == nullptr)
			{
				item = inventory.GetItemCompByGuid(binding.item_guid());
			}
			if (item == nullptr)
			{
				finish(kEquipItemNotFound);
				return;
			}
			const ItemTable *row = ItemRow(item->config_id());
			if (row == nullptr || row->equip_kind() == 0 || row->max_stack_size() != 1 ||
				!equiprules::IsKnownEquipmentProfile(row->equip_profile()) || !IsCarriable(*item, *row))
			{
				finish(kEquipLoadoutSlotUnavailable);
				return;
			}
			const auto acceptedSlots = equipment.SlotsAcceptingKind(row->equip_kind());
			if (std::find(acceptedSlots.begin(), acceptedSlots.end(), binding.slot()) == acceptedSlots.end())
			{
				finish(kEquipLoadoutSlotUnavailable);
				return;
			}
			validatedSlots[binding.slot()] = binding.item_guid();
		}
	}

	auto *scheme = FindOrCreateScheme(comp, request.scheme_id());
	scheme->set_name(finalName);
	scheme->set_role_tag(request.role_tag());
	scheme->set_linked_skill_scheme_id(request.linked_skill_scheme_id());
	scheme->set_linked_attribute_scheme_id(request.linked_attribute_scheme_id());
	scheme->clear_slots();
	for (const auto &[slot, guid] : validatedSlots)
	{
		if (guid == 0)
		{
			continue;
		}
		auto *outBinding = scheme->add_slots();
		outBinding->set_slot(slot);
		outBinding->set_item_guid(guid);
	}

	if (comp.unlocked_scheme_count() < equiploadout::kFreeSchemes)
	{
		comp.set_unlocked_scheme_count(equiploadout::kFreeSchemes);
	}
	if (comp.active_scheme_id() == 0)
	{
		comp.set_active_scheme_id(1u);
	}
	comp.set_state_version(currentVersion + 1u);
	comp.set_last_request_id(request.request_id());
	comp.set_last_base_version(request.state_version());
	comp.set_last_scheme_id(request.scheme_id());

	PlayerLifecycleSystem::RequestKeyProgressSave(player, "equip_loadout_saved");
	LOG_INFO << "[PlayerEquipLoadout] Scheme saved: player_id=" << tlsEcs.GetPlayerId(player)
			 << " scheme_id=" << request.scheme_id() << " slots=" << scheme->slots_size()
			 << " capture_current=" << request.capture_current_equipped()
			 << " version=" << comp.state_version();
	finish(kSuccess);
}

void PlayerEquipLoadoutSystem::Switch(entt::entity player, const SwitchEquipLoadoutRequest &request,
									  SwitchEquipLoadoutResponse &response)
{
	response.Clear();
	response.set_request_id(request.request_id());

	const auto finish = [&](uint32_t errorTip, bool includeBags) {
		if (errorTip != kSuccess)
		{
			response.mutable_error_message()->set_id(errorTip);
		}
		if (includeBags)
		{
			auto bagErr = PlayerBagSystem::BuildSnapshot(player, kInventory, *response.mutable_bag());
			if (bagErr == kSuccess)
			{
				bagErr = PlayerBagSystem::BuildSnapshot(player, kEquipment, *response.mutable_equipment());
			}
			if (bagErr != kSuccess)
			{
				response.clear_bag();
				response.clear_equipment();
				if (errorTip == kSuccess)
				{
					response.mutable_error_message()->set_id(bagErr);
				}
			}
		}
		if (const auto snapErr = BuildSnapshot(player, *response.mutable_snapshot()); snapErr != kSuccess)
		{
			response.clear_snapshot();
			if (errorTip == kSuccess && !response.has_error_message())
			{
				response.mutable_error_message()->set_id(snapErr);
			}
		}
	};

	if (const auto err = CheckRequiredInputs(player); err != kSuccess)
	{
		response.mutable_error_message()->set_id(err);
		return;
	}
	if (!equiploadout::ValidRequestId(request.request_id()))
	{
		finish(kInvalidParameter, false);
		return;
	}

	auto &comp = tlsEcs.actorRegistry.get<PlayerEquipLoadoutComp>(player);
	if (comp.last_request_id() == request.request_id())
	{
		const bool same = comp.last_base_version() == request.state_version() &&
						  comp.last_scheme_id() == request.scheme_id();
		if (!same)
		{
			finish(kInvalidParameter, false);
			return;
		}
		response.set_replayed(true);
		finish(kSuccess, true);
		return;
	}

	if (const auto err = CheckWritable(player); err != kSuccess)
	{
		finish(err, false);
		return;
	}

	const uint32_t unlocked = equiploadout::EffectiveUnlockedCount(comp.unlocked_scheme_count());
	if (!equiploadout::ValidSchemeId(request.scheme_id(), unlocked))
	{
		finish(kEquipLoadoutSchemeInvalid, false);
		return;
	}
	const uint64_t currentVersion = equiploadout::EffectiveStateVersion(comp.state_version());
	if (request.state_version() != currentVersion || comp.state_version() == std::numeric_limits<uint64_t>::max())
	{
		finish(kEquipLoadoutVersionChanged, false);
		return;
	}

	auto &bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(player);
	Bag &inventory = bags.bags[kInventory];
	Bag &equipment = bags.bags[kEquipment];

	// ---- 阶段 1：纯只读校验与计划构造（任一失败零副作用） ----
	std::map<uint32_t, uint64_t> currentBySlot;
	for (const uint32_t slot : equiploadout::kFormalSlots)
	{
		const Guid wornGuid = equipment.Layout().At(slot);
		if (wornGuid != kInvalidGuid)
		{
			currentBySlot[slot] = wornGuid;
		}
	}

	std::map<uint32_t, uint64_t> desiredBySlot;
	std::set<uint64_t> seenDesiredGuids;
	const EquipLoadoutScheme *storedScheme = FindScheme(comp, request.scheme_id());
	if (storedScheme != nullptr)
	{
		for (const auto &binding : storedScheme->slots())
		{
			if (!equiploadout::IsFormalSlot(binding.slot()))
			{
				finish(kEquipLoadoutSlotUnavailable, false);
				return;
			}
			if (binding.item_guid() != 0)
			{
				if (!seenDesiredGuids.insert(binding.item_guid()).second)
				{
					finish(kEquipLoadoutDuplicateItem, false);
					return;
				}
				desiredBySlot[binding.slot()] = binding.item_guid();
			}
		}
	}

	const auto plan = equiploadout::PlanAtomicSwap(
		currentBySlot, desiredBySlot,
		[&](uint64_t guid) { return inventory.GetItemCompByGuid(guid) != nullptr; });
	if (plan.error == equiploadout::SwapPlanError::kDuplicateItem)
	{
		finish(kEquipLoadoutDuplicateItem, false);
		return;
	}
	if (plan.error != equiploadout::SwapPlanError::kNone)
	{
		finish(kEquipLoadoutSlotUnavailable, false);
		return;
	}

	std::vector<Guid> equipFromBagGuids;
	equipFromBagGuids.reserve(plan.toEquipFromBag.size());
	for (const auto &move : plan.toEquipFromBag)
	{
		const ItemComp *item = inventory.GetItemCompByGuid(move.itemGuid);
		if (item == nullptr)
		{
			finish(kEquipItemNotFound, false);
			return;
		}
		if (const auto err = ValidateEquipmentCandidate(player, equipment, *item, move.targetSlot); err != kSuccess)
		{
			finish(err, false);
			return;
		}
		equipFromBagGuids.push_back(move.itemGuid);
	}
	for (const auto &move : plan.toMoveWithinEquip)
	{
		const ItemComp *item = equipment.GetItemCompByGuid(move.itemGuid);
		if (item == nullptr)
		{
			finish(kEquipInternalError, false);
			return;
		}
		if (const auto err = ValidateEquipmentCandidate(player, equipment, *item, move.targetSlot); err != kSuccess)
		{
			finish(err, false);
			return;
		}
	}
	if (!equipFromBagGuids.empty() &&
		BagService::CheckRemovableInstances(player, inventory, equipFromBagGuids) ==
			BagService::RemovalVerdict::kHeld)
	{
		LOG_INFO << "[PlayerEquipLoadout] Switch refused, target equipment held by unsettled battle: player_id="
				 << tlsEcs.GetPlayerId(player) << " scheme_id=" << request.scheme_id();
		finish(kEquipLoadoutSlotUnavailable, false);
		return;
	}
	for (const auto &move : plan.toUnequip)
	{
		const ItemComp *item = equipment.GetItemCompByGuid(move.itemGuid);
		const ItemTable *row = item != nullptr ? ItemRow(item->config_id()) : nullptr;
		if (item == nullptr || row == nullptr || !IsCarriable(*item, *row))
		{
			LOG_ERROR << "[PlayerEquipLoadout] Worn item not carriable; switch rejected: player_id="
					  << tlsEcs.GetPlayerId(player) << " guid=" << move.itemGuid;
			finish(kEquipInternalError, false);
			return;
		}
	}
	if (inventory.Layout().FreeCells() < plan.netExtraBagSlotsNeeded)
	{
		finish(kEquipLoadoutSlotUnavailable, false);
		return;
	}

	// ---- 阶段 2：原子搬运提交（先全部取出，再全部落位；异常失败时逆序回滚） ----
	struct TakenFromBag
	{
		uint32_t targetSlot{0};
		uint32_t srcBagPos{kInvalidU32Id};
		ItemComp item;
	};
	struct TakenFromEquip
	{
		uint32_t srcEquipSlot{0};
		uint32_t targetSlot{kInvalidU32Id}; // kInvalidU32Id 表示卸回背包
		uint32_t preferredBagPos{kInvalidU32Id};
		ItemComp item;
	};

	std::vector<TakenFromBag> takenFromBag;
	takenFromBag.reserve(plan.toEquipFromBag.size());
	std::unordered_map<uint64_t, uint32_t> incomingBagPosByGuid;

	std::vector<TakenFromEquip> takenUnequip;
	takenUnequip.reserve(plan.toUnequip.size());
	std::vector<TakenFromEquip> takenMoveWithin;
	takenMoveWithin.reserve(plan.toMoveWithinEquip.size());

	std::vector<uint64_t> placedInEquipGuids;
	std::vector<uint64_t> placedInBagGuids;

	const auto rollbackAll = [&]() {
		bool equipChanged = false;
		// 1. 取出已放进装备栏的件
		for (const uint64_t guid : placedInEquipGuids)
		{
			ItemComp undo;
			if (equipment.TakeInstance(guid, undo) != kSuccess)
			{
				equipChanged = true;
			}
		}
		// 2. 取出已放回背包的旧件
		for (const uint64_t guid : placedInBagGuids)
		{
			ItemComp undo;
			inventory.TakeInstance(guid, undo);
		}
		// 3. 把原本穿在身上的件放回装备栏原槽
		for (const auto &t : takenUnequip)
		{
			if (!PutBack(equipment, t.item, t.srcEquipSlot, "equipment", player))
			{
				equipChanged = true;
			}
		}
		for (const auto &t : takenMoveWithin)
		{
			if (!PutBack(equipment, t.item, t.srcEquipSlot, "equipment", player))
			{
				equipChanged = true;
			}
		}
		// 4. 把原本在背包的件放回背包原格
		for (const auto &t : takenFromBag)
		{
			PutBack(inventory, t.item, t.srcBagPos, "inventory", player);
		}
		if (equipChanged)
		{
			PlayerAttributeSystem::Recalculate(player, PlayerAttributeSystem::RecalcReason::kEquipmentChanged);
			PlayerAttributeSystem::PushPanel(player);
		}
	};

	for (const auto &move : plan.toEquipFromBag)
	{
		TakenFromBag taken;
		taken.targetSlot = move.targetSlot;
		taken.srcBagPos = inventory.GetItemPosByGuid(move.itemGuid);
		if (const auto err = inventory.TakeInstance(move.itemGuid, taken.item); err != kSuccess)
		{
			LOG_ERROR << "[PlayerEquipLoadout] TakeInstance from inventory failed: player_id="
					  << tlsEcs.GetPlayerId(player) << " guid=" << move.itemGuid << " err=" << err;
			rollbackAll();
			finish(kEquipInternalError, false);
			return;
		}
		incomingBagPosByGuid[move.itemGuid] = taken.srcBagPos;
		takenFromBag.push_back(std::move(taken));
	}

	for (const auto &move : plan.toUnequip)
	{
		TakenFromEquip taken;
		taken.srcEquipSlot = move.fromSlot;
		if (move.pairedIncomingBagGuid != 0)
		{
			if (const auto posIt = incomingBagPosByGuid.find(move.pairedIncomingBagGuid);
				posIt != incomingBagPosByGuid.end())
			{
				taken.preferredBagPos = posIt->second;
			}
		}
		if (const auto err = equipment.TakeInstance(move.itemGuid, taken.item); err != kSuccess)
		{
			LOG_ERROR << "[PlayerEquipLoadout] TakeInstance from equipment (unequip) failed: player_id="
					  << tlsEcs.GetPlayerId(player) << " guid=" << move.itemGuid << " err=" << err;
			rollbackAll();
			finish(kEquipInternalError, false);
			return;
		}
		takenUnequip.push_back(std::move(taken));
	}

	for (const auto &move : plan.toMoveWithinEquip)
	{
		TakenFromEquip taken;
		taken.srcEquipSlot = move.fromSlot;
		taken.targetSlot = move.targetSlot;
		if (const auto err = equipment.TakeInstance(move.itemGuid, taken.item); err != kSuccess)
		{
			LOG_ERROR << "[PlayerEquipLoadout] TakeInstance from equipment (re-slot) failed: player_id="
					  << tlsEcs.GetPlayerId(player) << " guid=" << move.itemGuid << " err=" << err;
			rollbackAll();
			finish(kEquipInternalError, false);
			return;
		}
		takenMoveWithin.push_back(std::move(taken));
	}

	for (const auto &t : takenFromBag)
	{
		if (const auto err = equipment.PutInstance(t.item, t.targetSlot); err != kSuccess)
		{
			LOG_ERROR << "[PlayerEquipLoadout] PutInstance into equipment failed: player_id="
					  << tlsEcs.GetPlayerId(player) << " guid=" << t.item.item_id()
					  << " slot=" << t.targetSlot << " err=" << err;
			rollbackAll();
			finish(kEquipInternalError, false);
			return;
		}
		placedInEquipGuids.push_back(t.item.item_id());
	}

	for (const auto &t : takenMoveWithin)
	{
		if (const auto err = equipment.PutInstance(t.item, t.targetSlot); err != kSuccess)
		{
			LOG_ERROR << "[PlayerEquipLoadout] PutInstance (re-slot) into equipment failed: player_id="
					  << tlsEcs.GetPlayerId(player) << " guid=" << t.item.item_id()
					  << " slot=" << t.targetSlot << " err=" << err;
			rollbackAll();
			finish(kEquipInternalError, false);
			return;
		}
		placedInEquipGuids.push_back(t.item.item_id());
	}

	for (const auto &t : takenUnequip)
	{
		uint32_t err = kEquipInternalError;
		if (t.preferredBagPos != kInvalidU32Id)
		{
			err = inventory.PutInstance(t.item, t.preferredBagPos);
		}
		if (err != kSuccess)
		{
			err = inventory.PutInstance(t.item);
		}
		if (err != kSuccess)
		{
			LOG_ERROR << "[PlayerEquipLoadout] PutInstance into inventory failed: player_id="
					  << tlsEcs.GetPlayerId(player) << " guid=" << t.item.item_id() << " err=" << err;
			rollbackAll();
			finish(kEquipInternalError, false);
			return;
		}
		placedInBagGuids.push_back(t.item.item_id());
	}

	if (plan.toEquipFromBag.size() > plan.toUnequip.size())
	{
		BagService::NotifySpaceFreed(player, inventory);
	}

	// ---- 状态更新与跨系统流派联动 ----
	if (comp.unlocked_scheme_count() < equiploadout::kFreeSchemes)
	{
		comp.set_unlocked_scheme_count(equiploadout::kFreeSchemes);
	}
	comp.set_active_scheme_id(request.scheme_id());
	comp.set_state_version(currentVersion + 1u);
	comp.set_last_request_id(request.request_id());
	comp.set_last_base_version(request.state_version());
	comp.set_last_scheme_id(request.scheme_id());

	if (request.apply_linked_schemes() && storedScheme != nullptr)
	{
		if (storedScheme->linked_skill_scheme_id() != 0)
		{
			std::string skillNotice;
			PlayerSkillSystem::TrySwitchLinkedLoadout(player, storedScheme->linked_skill_scheme_id(), skillNotice);
			if (!skillNotice.empty())
			{
				response.add_linked_notices(skillNotice);
			}
		}
		if (storedScheme->linked_attribute_scheme_id() != 0)
		{
			const uint32_t attrErr =
				PlayerAttributeSystem::SwitchScheme(player, storedScheme->linked_attribute_scheme_id(), false);
			if (attrErr == kAttributeSchemeSwitchCooldown)
			{
				response.add_linked_notices("属性加点方案切换冷却中，本次仅完成装备切换");
			}
			else if (attrErr == kAttributeSchemeNotFound)
			{
				response.add_linked_notices("绑定的属性加点方案不存在，本次仅完成装备切换");
			}
			else if (attrErr != kSuccess && attrErr != kAttributeSchemeAlreadyActive)
			{
				response.add_linked_notices("属性加点方案暂不可切换，本次仅完成装备切换");
			}
		}
	}

	// 全套换装 + 联动方案完成后统一只做 1 次重算与面板推送。
	PlayerAttributeSystem::Recalculate(player, PlayerAttributeSystem::RecalcReason::kEquipmentChanged);
	PlayerAttributeSystem::PushPanel(player);
	PlayerLifecycleSystem::RequestKeyProgressSave(player, "equip_loadout_switched");

	for (const uint32_t slot : plan.missingSlots)
	{
		response.add_missing_slots(slot);
	}

	LOG_INFO << "[PlayerEquipLoadout] Scheme switched: player_id=" << tlsEcs.GetPlayerId(player)
			 << " scheme_id=" << request.scheme_id()
			 << " equipped_from_bag=" << plan.toEquipFromBag.size()
			 << " moved_within_equip=" << plan.toMoveWithinEquip.size()
			 << " unequipped=" << plan.toUnequip.size()
			 << " missing=" << plan.missingSlots.size()
			 << " linked_notices=" << response.linked_notices_size()
			 << " version=" << comp.state_version();
	finish(kSuccess, true);
}

void PlayerEquipLoadoutSystem::Unlock(entt::entity player, const UnlockEquipLoadoutRequest &request,
									  UnlockEquipLoadoutResponse &response)
{
	response.Clear();
	response.set_client_request_id(request.client_request_id());

	const auto reject = [&](uint32_t errorTip, bool knownOutcome = true) {
		response.set_outcome(knownOutcome ? UnlockEquipLoadoutResponse::REJECTED
										  : UnlockEquipLoadoutResponse::UNKNOWN);
		response.mutable_error_message()->set_id(errorTip);
		if (BuildSnapshot(player, *response.mutable_snapshot()) != kSuccess)
		{
			response.clear_snapshot();
		}
	};

	if (const auto err = CheckRequiredInputs(player); err != kSuccess)
	{
		reject(err, false);
		return;
	}
	auto *ledger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
	const auto *identity = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
	if (identity == nullptr || identity->value == 0 || ledger == nullptr ||
		tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player) ||
		!client_request_ledger::Validate(*ledger).empty())
	{
		reject(kServiceUnavailable, false);
		return;
	}
	if (request.client_request_id() == 0)
	{
		reject(kInvalidParameter, true);
		return;
	}

	const std::string fingerprint = ClientRequestFingerprint(request);
	ClientRequestResult firstResult;
	const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
													 ClientRequestKind::kEquipLoadoutUnlock, fingerprint, firstResult);
	if (verdict == ClientRequestVerdict::kReplay)
	{
		FillUnlockAppliedReply(player, firstResult, true, response);
		return;
	}
	if (verdict == ClientRequestVerdict::kMismatch)
	{
		reject(kInvalidParameter, true);
		return;
	}
	if (verdict != ClientRequestVerdict::kNew)
	{
		reject(kServiceUnavailable, false);
		return;
	}

	if (const auto err = CheckWritable(player); err != kSuccess)
	{
		reject(err, true);
		return;
	}

	auto &comp = tlsEcs.actorRegistry.get<PlayerEquipLoadoutComp>(player);
	const uint32_t currentUnlocked = equiploadout::EffectiveUnlockedCount(comp.unlocked_scheme_count());
	if (request.expected_unlocked_count() != currentUnlocked)
	{
		// 回执未命中且当前已解锁数大于预期：原解锁可能早已成功但回执被淘汰，标 UNKNOWN 避免客户端误判失败。
		const bool known = currentUnlocked < request.expected_unlocked_count();
		reject(kEquipLoadoutUnlockCountChanged, known);
		return;
	}
	if (currentUnlocked >= equiploadout::kMaxSchemes)
	{
		reject(kEquipLoadoutMaxUnlocked, true);
		return;
	}
	if (ledger->next_seq() == std::numeric_limits<uint64_t>::max() ||
		comp.state_version() == std::numeric_limits<uint64_t>::max())
	{
		reject(kServiceUnavailable, false);
		return;
	}

	const uint32_t targetSchemeId = currentUnlocked + 1u;
	const auto spec = equiploadout::UnlockSpecForTarget(targetSchemeId);
	if (!spec.valid)
	{
		reject(kEquipLoadoutMaxUnlocked, true);
		return;
	}

	auto &bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(player);
	Bag &inventory = bags.bags[kInventory];
	const uint32_t talismanAvailable =
		BagService::RemovableCount(player, inventory, equiploadout::kUnlockItemConfigId);
	const uint64_t bindBalance = CurrencySystem::GetBalance(player, kCurrencyBindDiamond);
	const uint64_t diamondBalance = CurrencySystem::GetBalance(player, kCurrencyDiamond);

	const auto payment = equiploadout::PlanUnlockPayment(spec, talismanAvailable, bindBalance, diamondBalance,
														 request.use_bind_diamond_first());
	if (!payment.canAfford)
	{
		reject(kEquipLoadoutDiamondNotEnough, true);
		return;
	}

	ClientRequestResult receipt;
	receipt.equipLoadoutUnlockedCount = targetSchemeId;
	receipt.equipLoadoutUsedItemId = payment.useTalisman ? equiploadout::kUnlockItemConfigId : 0u;
	receipt.equipLoadoutSpentBindDiamond = payment.spendBindDiamond;
	receipt.equipLoadoutSpentDiamond = payment.spendDiamond;

	ClientRequestLedgerComp candidateLedger = *ledger;
	if (!client_request_ledger::Remember(candidateLedger, request.client_request_id(),
										 ClientRequestKind::kEquipLoadoutUnlock, fingerprint, receipt) ||
		!client_request_ledger::Validate(candidateLedger).empty())
	{
		reject(kServiceUnavailable, false);
		return;
	}

	const uint64_t nextVersion = equiploadout::EffectiveStateVersion(comp.state_version()) + 1u;
	if (payment.useTalisman)
	{
		std::vector<GuidVector> written;
		const std::string extraJson = std::string("{\"source\":\"equip_loadout_unlock\",\"target_scheme\":") +
									  std::to_string(targetSchemeId) + "}";
		const uint32_t exchangeErr = BagService::ExchangeInventoryExact(
			player, {{equiploadout::kUnlockItemConfigId, 1u}}, {}, {}, request.client_request_id(), extraJson,
			written, [&](const std::vector<GuidVector> &) -> uint32_t {
				if (!player_mutability::IsPlayerMutable(player))
				{
					return kEquipFrozen;
				}
				comp.set_unlocked_scheme_count(targetSchemeId);
				if (comp.active_scheme_id() == 0)
				{
					comp.set_active_scheme_id(1u);
				}
				FindOrCreateScheme(comp, targetSchemeId);
				comp.set_state_version(nextVersion);
				ledger->Swap(&candidateLedger);
				return kSuccess;
			});
		if (exchangeErr != kSuccess)
		{
			reject(exchangeErr == kEquipFrozen ? kEquipFrozen : kEquipLoadoutDiamondNotEnough, true);
			return;
		}
	}
	else
	{
		if (payment.spendBindDiamond > 0 &&
			!CurrencySystem::CanAfford(player, kCurrencyBindDiamond, static_cast<int64_t>(payment.spendBindDiamond)))
		{
			reject(kEquipLoadoutDiamondNotEnough, true);
			return;
		}
		if (payment.spendDiamond > 0 &&
			!CurrencySystem::CanAfford(player, kCurrencyDiamond, static_cast<int64_t>(payment.spendDiamond)))
		{
			reject(kEquipLoadoutDiamondNotEnough, true);
			return;
		}

		if (payment.spendBindDiamond > 0)
		{
			if (const auto err = CurrencySystem::DeductCurrency(
					player, kCurrencyBindDiamond, static_cast<int64_t>(payment.spendBindDiamond),
					TX_EQUIP_LOADOUT_UNLOCK, targetSchemeId);
				err != kSuccess)
			{
				reject(kEquipLoadoutDiamondNotEnough, true);
				return;
			}
		}
		if (payment.spendDiamond > 0)
		{
			if (const auto err = CurrencySystem::DeductCurrency(
					player, kCurrencyDiamond, static_cast<int64_t>(payment.spendDiamond),
					TX_EQUIP_LOADOUT_UNLOCK, targetSchemeId);
				err != kSuccess)
			{
				if (payment.spendBindDiamond > 0)
				{
					CurrencySystem::AddCurrency(player, kCurrencyBindDiamond,
												static_cast<int64_t>(payment.spendBindDiamond),
												TX_EQUIP_LOADOUT_UNLOCK, targetSchemeId);
				}
				reject(kEquipLoadoutDiamondNotEnough, true);
				return;
			}
		}

		comp.set_unlocked_scheme_count(targetSchemeId);
		if (comp.active_scheme_id() == 0)
		{
			comp.set_active_scheme_id(1u);
		}
		FindOrCreateScheme(comp, targetSchemeId);
		comp.set_state_version(nextVersion);
		ledger->Swap(&candidateLedger);
	}

	LOG_INFO << "[PlayerEquipLoadout] Scheme slot unlocked: player_id=" << tlsEcs.GetPlayerId(player)
			 << " unlocked=" << targetSchemeId << " used_item=" << receipt.equipLoadoutUsedItemId
			 << " spent_bind_diamond=" << receipt.equipLoadoutSpentBindDiamond
			 << " spent_diamond=" << receipt.equipLoadoutSpentDiamond
			 << " client_request_id=" << request.client_request_id();
	FillUnlockAppliedReply(player, receipt, false, response);
}
