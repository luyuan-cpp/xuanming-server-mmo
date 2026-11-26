#include "player_craft.h"

#include <algorithm>
#include <cstdint>
#include <limits>
#include <map>
#include <random>
#include <set>
#include <string>
#include <vector>

#include <muduo/base/Logging.h>

#include "battle/system/player_battle.h"
#include "core/type_define/player_guid_comp.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h"
#include "player/system/client_request_ledger.h"
#include "player/system/equip_craft_quality_rules.h"
#include "player/system/player_attribute.h"
#include "player/system/player_equipment_v1.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_pet.h"
#include "player/system/player_skill.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_craft_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"
#include "proto/scene/player_skill.pb.h"
#include "table/code/item_table.h"
#include "table/code/skill_table.h"
#include "table/code/skillscrollcraft_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/equip_error_tip.pb.h"
#include "table/proto/tip/skill_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {
constexpr uint64_t kMaxPayableGold = static_cast<uint64_t>(std::numeric_limits<int64_t>::max());

bool gCraftUnlockGateEnabled = false;
PlayerCraftSystem::CraftRollFn gCraftRollFn = nullptr;

uint32_t RollBelow(uint32_t bound) {
    if (bound <= 1) return 0;
    if (gCraftRollFn != nullptr) return gCraftRollFn(bound) % bound;
    thread_local std::mt19937_64 rng{std::random_device{}()};
    return static_cast<uint32_t>(std::uniform_int_distribution<uint32_t>(0, bound - 1)(rng));
}

const ItemTable* ItemRow(uint32_t id) {
    return ItemTableManager::Instance().FindByIdSilent(id).first;
}

const SkillTable* SkillRow(uint32_t id) {
    return SkillTableManager::Instance().FindByIdSilent(id).first;
}

const SkillScrollCraftTable* ScrollRow(uint32_t id) {
    return SkillScrollCraftTableManager::Instance().FindByIdSilent(id).first;
}

bool ValidStackableMaterial(uint32_t id, uint32_t count) {
    const auto* row = ItemRow(id);
    return id != 0 && count != 0 && row != nullptr && row->max_stack_size() != 0 &&
        row->equip_kind() == 0 && row->battle_usable() == 0;
}

std::string DisplayNameOf(entt::entity player, uint64_t playerId) {
    if (const auto* profile = tlsEcs.actorRegistry.try_get<PlayerProfileComp>(player);
        profile != nullptr && !profile->name().empty()) {
        return profile->name();
    }
    return "宗师#" + std::to_string(playerId);
}

std::vector<uint32_t> CollectScrollSkillIds(uint32_t scrollId) {
    std::vector<uint32_t> ids;
    for (const auto& skill : SkillTableManager::Instance().FindAll().data()) {
        if (skill.scroll_id() == scrollId && skill.season_id() == 0 && skill.battle_model() == 1) {
            ids.push_back(skill.id());
        }
    }
    std::sort(ids.begin(), ids.end());
    return ids;
}

uint32_t CheckPlayerReady(entt::entity player) {
    if (!tlsEcs.actorRegistry.valid(player)) return kEntityIsNull;
    const auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    const auto* craft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(player);
    const auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
    const auto* ledger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
    if (guid == nullptr || guid->value == 0 || craft == nullptr || bags == nullptr || ledger == nullptr ||
        tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player) ||
        !client_request_ledger::Validate(*ledger).empty()) {
        return kServiceUnavailable;
    }
    return kSuccess;
}

uint32_t CheckWritable(entt::entity player) {
    if (!PlayerLifecycleSystem::IsPlayerMutable(player) || PlayerBattleSystem::IsInBattle(player)) {
        return kSkillUpgradeBusy;
    }
    return kSuccess;
}

bool CanCreditProxyMaster(entt::entity master, uint64_t netFee) {
    if (netFee == 0) return true;
    if (tlsEcs.actorRegistry.try_get<CurrencyComp>(master) == nullptr ||
        tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(master) == nullptr) return false;
    if (GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kCurrency,
                                            static_cast<uint32_t>(kCurrencyGold)) ||
        CurrencySystem::IsCurrencyBlocked(master, kCurrencyGold)) return false;
    return !CurrencySystem::CreditOverflows(master, kCurrencyGold, netFee);
}

void FillStateSnapshot(entt::entity player, SkillCraftingStateSnapshot& out) {
    out.Clear();
    const auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    const auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];

    out.set_scroll_craft_revision(craft.scroll_craft_revision());
    PlayerCraftSystem::FillMasterySnapshots(player, *out.mutable_forge_mastery(), *out.mutable_scroll_mastery());

    std::vector<const SkillScrollCraftTable*> scrolls;
    for (const auto& row : SkillScrollCraftTableManager::Instance().FindAll().data()) {
        if (row.enabled() != 0) scrolls.push_back(&row);
    }
    std::sort(scrolls.begin(), scrolls.end(), [](const auto* a, const auto* b) { return a->id() < b->id(); });
    for (const auto* row : scrolls) {
        auto* entry = out.add_scrolls();
        entry->set_scroll_id(row->id());
        entry->set_name(row->name());
        entry->set_description(row->description());
        entry->set_rarity_tier(row->rarity_tier());
        entry->set_fragment_item_id(row->fragment_item_id());
        entry->set_random_fragment_count(row->random_fragment_count());
        entry->set_random_gold_cost(row->random_gold_cost());
        entry->set_targeted_fragment_count(row->targeted_fragment_count());
        entry->set_targeted_gold_cost(row->targeted_gold_cost());
        entry->set_dismantle_fragment_count(row->dismantle_fragment_count());
        entry->set_rare_scroll_item_id(row->rare_scroll_item_id());
        entry->set_rare_scroll_count(row->rare_scroll_count());
        entry->set_rare_ink_item_id(row->rare_ink_item_id());
        entry->set_rare_ink_count(row->rare_ink_count());
        entry->set_rare_gold_cost(row->rare_gold_cost());
        for (const auto skillId : CollectScrollSkillIds(row->id())) {
            entry->add_skill_table_ids(skillId);
        }
    }

    std::vector<CraftedSkillBookEntry> books(craft.skill_books().begin(), craft.skill_books().end());
    std::sort(books.begin(), books.end(), [](const auto& a, const auto& b) { return a.book_id() < b.book_id(); });
    for (const auto& b : books) *out.add_skill_books() = b;

    std::vector<uint32_t> unlocked(craft.book_unlocked_skill_ids().begin(), craft.book_unlocked_skill_ids().end());
    std::sort(unlocked.begin(), unlocked.end());
    unlocked.erase(std::unique(unlocked.begin(), unlocked.end()), unlocked.end());
    for (const auto id : unlocked) out.add_book_unlocked_skill_ids(id);

    out.set_gold_balance(CurrencySystem::GetBalance(player, kCurrencyGold));
    out.set_fragment_owned(inventory.GetTotalItemCount(equipcraft::kScrollFragmentConfigId));
    out.set_rare_scroll_owned(inventory.GetTotalItemCount(equipcraft::kRareScrollPageConfigId));
    out.set_rare_ink_owned(inventory.GetTotalItemCount(equipcraft::kRareScrollInkConfigId));
    out.set_proxy_stall_open(craft.proxy_stall_open());
    out.set_proxy_forge_fee_gold(craft.proxy_forge_fee_gold());
    out.set_proxy_scroll_fee_gold(craft.proxy_scroll_fee_gold());
    out.set_craft_unlock_gate_enabled(PlayerCraftSystem::IsCraftUnlockGateEnabled());
}

int FindBookIndex(const PlayerCraftComp& craft, uint64_t bookId) {
    for (int i = 0; i < craft.skill_books_size(); ++i) {
        if (craft.skill_books(i).book_id() == bookId) return i;
    }
    return -1;
}

void RemoveBookAt(PlayerCraftComp& craft, int index) {
    auto* books = craft.mutable_skill_books();
    if (index < 0 || index >= books->size()) return;
    if (index != books->size() - 1) {
        books->SwapElements(index, books->size() - 1);
    }
    books->RemoveLast();
}
} // namespace

bool PlayerCraftSystem::ValidateTables() {
    if (PlayerEquipmentV1System::ValidateQualityTable() != kSuccess) {
        LOG_ERROR << "[PlayerCraft] EquipAffixQuality table validation failed";
        return false;
    }
    const auto& scrolls = SkillScrollCraftTableManager::Instance().FindAll().data();
    if (scrolls.empty()) {
        LOG_ERROR << "[PlayerCraft] SkillScrollCraft table is empty";
        return false;
    }
    std::map<uint32_t, uint32_t> scrollRarity;
    for (const auto& scroll : scrolls) {
        if (scroll.id() == 0 || scroll.rarity_tier() > 1) {
            LOG_ERROR << "[PlayerCraft] invalid SkillScrollCraft row id=" << scroll.id()
                      << " rarity_tier=" << scroll.rarity_tier();
            return false;
        }
        if (scroll.enabled() == 0) continue;
        scrollRarity[scroll.id()] = scroll.rarity_tier();
        if (scroll.rarity_tier() == 0) {
            if (!ValidStackableMaterial(scroll.fragment_item_id(), scroll.random_fragment_count()) ||
                scroll.targeted_fragment_count() < scroll.random_fragment_count() ||
                scroll.dismantle_fragment_count() == 0 ||
                scroll.dismantle_fragment_count() > scroll.random_fragment_count()) {
                LOG_ERROR << "[PlayerCraft] invalid regular scroll recipe id=" << scroll.id();
                return false;
            }
        } else {
            if (!ValidStackableMaterial(scroll.rare_scroll_item_id(), scroll.rare_scroll_count()) ||
                !ValidStackableMaterial(scroll.rare_ink_item_id(), scroll.rare_ink_count())) {
                LOG_ERROR << "[PlayerCraft] invalid rare scroll recipe id=" << scroll.id();
                return false;
            }
        }
    }

    uint32_t totalScrollSkills = 0;
    uint32_t rareScrollSkills = 0;
    std::map<uint32_t, uint32_t> perScrollCount;
    for (const auto& skill : SkillTableManager::Instance().FindAll().data()) {
        if (skill.scroll_id() == 0) continue;
        const auto it = scrollRarity.find(skill.scroll_id());
        if (it == scrollRarity.end() || skill.season_id() != 0 || skill.battle_model() != 1 ||
            skill.rarity_tier() != it->second) {
            LOG_ERROR << "[PlayerCraft] skill scroll binding invalid skill_id=" << skill.id()
                      << " scroll_id=" << skill.scroll_id() << " rarity_tier=" << skill.rarity_tier();
            return false;
        }
        ++totalScrollSkills;
        ++perScrollCount[skill.scroll_id()];
        if (skill.rarity_tier() == 1) ++rareScrollSkills;
    }
    for (const auto& [scrollId, rarity] : scrollRarity) {
        (void)rarity;
        if (perScrollCount[scrollId] == 0) {
            LOG_ERROR << "[PlayerCraft] enabled scroll has no skills scroll_id=" << scrollId;
            return false;
        }
    }
    // 硬红线：永久技能中稀世绝学占比严锁 <= 10%。
    if (totalScrollSkills == 0 ||
        uint64_t{rareScrollSkills} * 10000u > uint64_t{totalScrollSkills} * equipcraft::kMaxRareSkillRatioBps) {
        LOG_ERROR << "[PlayerCraft] rare skill ratio exceeds 10% hard cap: rare=" << rareScrollSkills
                  << " total=" << totalScrollSkills;
        return false;
    }
    return true;
}

bool PlayerCraftSystem::IsCraftUnlockGateEnabled() {
    return gCraftUnlockGateEnabled;
}

void PlayerCraftSystem::SetCraftUnlockGateForTest(bool enabled) {
    gCraftUnlockGateEnabled = enabled;
}

void PlayerCraftSystem::SetCraftRollForTest(CraftRollFn fn) {
    gCraftRollFn = fn;
}

bool PlayerCraftSystem::IsSkillBookUnlocked(entt::entity player, uint32_t skillTableId) {
    if (skillTableId == 0 || !tlsEcs.actorRegistry.valid(player)) return false;
    const auto* craft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(player);
    if (craft == nullptr) return false;
    for (const auto id : craft->book_unlocked_skill_ids()) {
        if (id == skillTableId) return true;
    }
    return false;
}

void PlayerCraftSystem::FillMasterySnapshots(entt::entity player, SkillCraftMasterySnapshot& forgeOut,
                                             SkillCraftMasterySnapshot& scrollOut) {
    forgeOut.Clear();
    scrollOut.Clear();
    uint32_t forgeExp = 0;
    uint32_t scrollExp = 0;
    if (tlsEcs.actorRegistry.valid(player)) {
        if (const auto* craft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(player); craft != nullptr) {
            forgeExp = craft->forge_mastery_exp();
            scrollExp = craft->scroll_mastery_exp();
        }
    }
    const uint32_t forgeTier = equipcraft::MasteryTierForExp(forgeExp);
    forgeOut.set_tier(forgeTier);
    forgeOut.set_tier_name(equipcraft::MasteryTierName(forgeTier));
    forgeOut.set_exp(forgeExp);
    forgeOut.set_next_tier_exp(equipcraft::MasteryNextExp(forgeExp));
    forgeOut.set_bonus_rate_bps(equipcraft::ForgeTopQualityRateBps(forgeTier));

    const uint32_t scrollTier = equipcraft::MasteryTierForExp(scrollExp);
    scrollOut.set_tier(scrollTier);
    scrollOut.set_tier_name(equipcraft::MasteryTierName(scrollTier));
    scrollOut.set_exp(scrollExp);
    scrollOut.set_next_tier_exp(equipcraft::MasteryNextExp(scrollExp));
    scrollOut.set_bonus_rate_bps(equipcraft::ScrollBonusFragmentRateBps(scrollTier));
}

uint32_t PlayerCraftSystem::GetState(entt::entity player, GetSkillCraftingStateResponse& response) {
    response.Clear();
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return err;
    }
    FillStateSnapshot(player, *response.mutable_state());
    return kSuccess;
}

void PlayerCraftSystem::CraftSkillBook(entt::entity player, const CraftSkillBookRequest& request,
                                       CraftSkillBookResponse& response) {
    response.Clear();
    response.set_client_request_id(request.client_request_id());
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (request.client_request_id() == 0) {
        fail(kInvalidParameter);
        return;
    }

    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
                                                     ClientRequestKind::kSkillScrollCraft, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) {
        response.set_replayed(true);
        response.set_bonus_fragments_granted(first.scrollBonusFragments);
        const auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
        if (const int idx = FindBookIndex(craft, first.craftedSkillBookId); idx >= 0) {
            *response.mutable_crafted_book() = craft.skill_books(idx);
        } else {
            response.mutable_crafted_book()->set_book_id(first.craftedSkillBookId);
            response.mutable_crafted_book()->set_skill_table_id(first.craftedSkillTableId);
        }
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }
    if (verdict == ClientRequestVerdict::kMismatch) {
        fail(kInvalidParameter);
        return;
    }
    if (verdict != ClientRequestVerdict::kNew) {
        fail(kServiceUnavailable);
        return;
    }

    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    if (craft.scroll_craft_revision() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) {
        fail(kServiceUnavailable);
        return;
    }
    if (request.expected_revision() != craft.scroll_craft_revision()) {
        fail(kSkillCraftRevisionChanged);
        return;
    }

    const auto* scroll = ScrollRow(request.scroll_id());
    if (scroll == nullptr || scroll->enabled() == 0) {
        fail(kSkillScrollInvalid);
        return;
    }
    const auto pool = CollectScrollSkillIds(scroll->id());
    if (pool.empty()) {
        fail(kSkillScrollInvalid);
        return;
    }

    const auto mode = request.mode();
    uint32_t chosenSkillId = 0;
    ItemCountMap consumed;
    uint64_t baseGoldCost = 0;
    if (mode == CraftSkillBookRequest::RANDOM_REGULAR) {
        if (scroll->rarity_tier() != 0 || request.target_skill_table_id() != 0) {
            fail(kSkillScrollInvalid);
            return;
        }
        chosenSkillId = pool[RollBelow(static_cast<uint32_t>(pool.size()))];
        consumed[scroll->fragment_item_id()] += scroll->random_fragment_count();
        baseGoldCost = scroll->random_gold_cost();
    } else if (mode == CraftSkillBookRequest::TARGETED_REGULAR) {
        if (scroll->rarity_tier() != 0 ||
            !std::binary_search(pool.begin(), pool.end(), request.target_skill_table_id())) {
            fail(kSkillScrollInvalid);
            return;
        }
        chosenSkillId = request.target_skill_table_id();
        consumed[scroll->fragment_item_id()] += scroll->targeted_fragment_count();
        baseGoldCost = scroll->targeted_gold_cost();
    } else if (mode == CraftSkillBookRequest::TARGETED_RARE) {
        if (scroll->rarity_tier() != 1 ||
            !std::binary_search(pool.begin(), pool.end(), request.target_skill_table_id())) {
            fail(kSkillScrollInvalid);
            return;
        }
        chosenSkillId = request.target_skill_table_id();
        consumed[scroll->rare_scroll_item_id()] += scroll->rare_scroll_count();
        consumed[scroll->rare_ink_item_id()] += scroll->rare_ink_count();
        baseGoldCost = scroll->rare_gold_cost();
    } else {
        fail(kInvalidParameter);
        return;
    }

    // 解析可选主城宗师原子代制摊位（零物品移交，只读摊主制卷宗师档位并付净银两手工费）。
    uint64_t proxyMasterId = request.proxy_master_player_id();
    uint64_t proxyFeeGold = 0;
    std::string proxyMasterName;
    uint32_t effectiveScrollTier = equipcraft::MasteryTierForExp(craft.scroll_mastery_exp());
    if (proxyMasterId != 0) {
        const uint64_t buyerId = tlsEcs.GetPlayerId(player);
        if (proxyMasterId == buyerId) {
            fail(kEquipProxyMasterUnavailable);
            return;
        }
        const entt::entity master = tlsEcs.GetPlayer(proxyMasterId);
        const auto* masterCraft = tlsEcs.actorRegistry.valid(master)
            ? tlsEcs.actorRegistry.try_get<PlayerCraftComp>(master) : nullptr;
        if (masterCraft == nullptr || !PlayerLifecycleSystem::IsPlayerMutable(master) ||
            !masterCraft->proxy_stall_open() ||
            equipcraft::MasteryTierForExp(masterCraft->scroll_mastery_exp()) < equipcraft::kMasteryMaster ||
            !equipcraft::ValidProxyFee(masterCraft->proxy_scroll_fee_gold())) {
            fail(kEquipProxyMasterUnavailable);
            return;
        }
        const uint64_t netFee = equipcraft::ProxyNetGold(masterCraft->proxy_scroll_fee_gold());
        if (!CanCreditProxyMaster(master, netFee)) {
            fail(kEquipProxyMasterUnavailable);
            return;
        }
        proxyFeeGold = masterCraft->proxy_scroll_fee_gold();
        proxyMasterName = DisplayNameOf(master, proxyMasterId);
        effectiveScrollTier = equipcraft::kMasteryMaster;
    }

    // 防洗白硬隔离：代工费(proxyFeeGold)永远只扣流通银两(kCurrencyGold)；
    // 仅稀世绝学定向制书(TARGETED_RARE，产物必定永久绑定)的系统手续费允许优先抵扣绑定银两。
    const bool allowBindForBaseFee = (mode == CraftSkillBookRequest::TARGETED_RARE);
    const uint64_t strictGold = proxyFeeGold + (allowBindForBaseFee ? 0u : baseGoldCost);
    const uint64_t preferBindGold = allowBindForBaseFee ? baseGoldCost : 0u;
    if ((strictGold > 0 || preferBindGold > 0) &&
        !CurrencySystem::CanAffordSplitGold(player, strictGold, preferBindGold)) {
        fail(kSkillGoldNotEnough);
        return;
    }
    const auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    const auto matVerdict = BagService::CheckRemovable(player, inventory, consumed);
    if (matVerdict == BagService::RemovalVerdict::kInsufficient) {
        fail(kSkillCraftMaterialNotEnough);
        return;
    }
    if (matVerdict != BagService::RemovalVerdict::kEnough) {
        fail(kFeatureUnavailable);
        return;
    }

    // 巧匠/宗师拓印常规书时按 5%/10% 概率返还 2 张绑定悟道残页（不提高稀世产出）。
    uint32_t bonusFragments = 0;
    std::vector<InitItemParam> gained;
    if (scroll->rarity_tier() == 0) {
        const uint32_t bonusRate = equipcraft::ScrollBonusFragmentRateBps(effectiveScrollTier);
        if (bonusRate > 0 && RollBelow(10000u) < bonusRate) {
            if (BagService::CheckInventoryExchangeSpace(player, consumed,
                                                        {{scroll->fragment_item_id(), equipcraft::kScrollBonusFragmentsCount}}) == kSuccess) {
                bonusFragments = equipcraft::kScrollBonusFragmentsCount;
                InitItemParam bonusParam;
                bonusParam.itemPBComp.set_item_id(kInvalidGuid);
                bonusParam.itemPBComp.set_config_id(scroll->fragment_item_id());
                bonusParam.itemPBComp.set_size(bonusFragments);
                bonusParam.itemPBComp.set_bound(true);
                gained.push_back(std::move(bonusParam));
            }
        }
    }

    const auto ledgerBefore = ledger.SerializeAsString();
    const uint64_t revBefore = craft.scroll_craft_revision();
    CraftedSkillBookEntry newBook;
    bool consumedBound = false;
    std::vector<GuidVector> written;
    const auto exErr = BagService::ExchangeInventoryExact(
        player, consumed, gained, {}, request.client_request_id(), "{\"operation\":\"skill_scroll_craft\"}", written,
        [&](const std::vector<GuidVector>&) -> uint32_t {
            if (ledger.SerializeAsString() != ledgerBefore || craft.scroll_craft_revision() != revBefore ||
                CheckWritable(player) != kSuccess) {
                return kFeatureUnavailable;
            }
            if ((strictGold > 0 || preferBindGold > 0) &&
                !CurrencySystem::CanAffordSplitGold(player, strictGold, preferBindGold)) {
                return kSkillGoldNotEnough;
            }
            entt::entity proxyMaster = entt::null;
            uint64_t proxyNetGold = 0;
            if (proxyMasterId != 0) {
                proxyMaster = tlsEcs.GetPlayer(proxyMasterId);
                const auto* masterCraft = tlsEcs.actorRegistry.valid(proxyMaster)
                    ? tlsEcs.actorRegistry.try_get<PlayerCraftComp>(proxyMaster) : nullptr;
                if (masterCraft == nullptr || !PlayerLifecycleSystem::IsPlayerMutable(proxyMaster) ||
                    !masterCraft->proxy_stall_open() ||
                    equipcraft::MasteryTierForExp(masterCraft->scroll_mastery_exp()) < equipcraft::kMasteryMaster ||
                    masterCraft->proxy_scroll_fee_gold() != proxyFeeGold) {
                    return kEquipProxyMasterUnavailable;
                }
                proxyNetGold = equipcraft::ProxyNetGold(proxyFeeGold);
                if (!CanCreditProxyMaster(proxyMaster, proxyNetGold)) {
                    return kEquipProxyMasterUnavailable;
                }
            }

            const uint64_t bookId = craft.next_book_seq() == 0 ? 1u : craft.next_book_seq();
            newBook.Clear();
            newBook.set_book_id(bookId);
            newBook.set_skill_table_id(chosenSkillId);
            newBook.set_scroll_id(scroll->id());
            newBook.set_rarity_tier(scroll->rarity_tier());
            // 防洗白硬隔离：稀世绝学消耗天工秘墨（或任何绑定材料）产物永久仅自用绑定；常规书按实扣材料绑定性决定。
            newBook.set_bound(mode == CraftSkillBookRequest::TARGETED_RARE ? true : consumedBound);
            if (effectiveScrollTier == equipcraft::kMasteryMaster) {
                const uint64_t signerId = proxyMasterId != 0 ? proxyMasterId : tlsEcs.GetPlayerId(player);
                newBook.set_crafter_player_id(signerId);
                newBook.set_crafter_name(proxyMasterId != 0 ? proxyMasterName : DisplayNameOf(player, signerId));
            }

            ClientRequestResult applied;
            applied.craftedSkillBookId = bookId;
            applied.craftedSkillTableId = chosenSkillId;
            applied.scrollCraftRevision = revBefore + 1;
            applied.scrollBonusFragments = bonusFragments;

            ClientRequestLedgerComp nextLedger = ledger;
            if (!client_request_ledger::Remember(nextLedger, request.client_request_id(),
                                                 ClientRequestKind::kSkillScrollCraft, fingerprint, applied) ||
                !client_request_ledger::Validate(nextLedger).empty()) {
                return kServiceUnavailable;
            }
            if (strictGold > 0 || preferBindGold > 0) {
                const auto deductErr = CurrencySystem::DeductSplitGold(
                    player, strictGold, preferBindGold, TX_SKILL_SCROLL_CRAFT, request.client_request_id());
                if (deductErr != kSuccess) return kSkillGoldNotEnough;
            }
            if (proxyMaster != entt::null && proxyNetGold > 0) {
                (void)CurrencySystem::AddCurrency(proxyMaster, kCurrencyGold, static_cast<int64_t>(proxyNetGold),
                                                  TX_CRAFT_PROXY_FEE, request.client_request_id());
            }
            *craft.add_skill_books() = newBook;
            craft.set_next_book_seq(bookId + 1);
            craft.set_scroll_craft_revision(revBefore + 1);
            if (proxyMasterId == 0 && craft.scroll_mastery_exp() < std::numeric_limits<uint32_t>::max()) {
                craft.set_scroll_mastery_exp(craft.scroll_mastery_exp() + 1);
            }
            ledger.Swap(&nextLedger);
            return kSuccess;
        }, /*tradableOnly=*/false, &consumedBound);

    if (exErr != kSuccess) {
        fail(exErr);
        return;
    }

    PlayerLifecycleSystem::RequestKeyProgressSave(player, "skill_scroll_craft");
    *response.mutable_crafted_book() = newBook;
    response.set_bonus_fragments_granted(bonusFragments);
    FillStateSnapshot(player, *response.mutable_state());
}

void PlayerCraftSystem::LearnSkillBook(entt::entity player, const LearnSkillBookRequest& request,
                                       LearnSkillBookResponse& response) {
    response.Clear();
    response.set_client_request_id(request.client_request_id());
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (request.client_request_id() == 0 || request.book_id() == 0) {
        fail(kInvalidParameter);
        return;
    }

    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
                                                     ClientRequestKind::kLearnSkillBook, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) {
        response.set_replayed(true);
        response.set_learned_skill_table_id(first.craftedSkillTableId);
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }
    if (verdict == ClientRequestVerdict::kMismatch) {
        fail(kInvalidParameter);
        return;
    }
    if (verdict != ClientRequestVerdict::kNew) {
        fail(kServiceUnavailable);
        return;
    }

    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    if (craft.scroll_craft_revision() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) {
        fail(kServiceUnavailable);
        return;
    }
    if (request.expected_revision() != craft.scroll_craft_revision()) {
        fail(kSkillCraftRevisionChanged);
        return;
    }

    const int bookIdx = FindBookIndex(craft, request.book_id());
    if (bookIdx < 0) {
        fail(kSkillBookNotFound);
        return;
    }
    const uint32_t skillId = craft.skill_books(bookIdx).skill_table_id();
    const auto* row = SkillRow(skillId);
    if (row == nullptr || row->season_id() != 0 || row->battle_model() != 1) {
        fail(kInvalidTableId);
        return;
    }

    const uint32_t requiredMask = request.pet_id() == 0 ? 1u : 2u;
    if ((row->caster_mask() & requiredMask) == 0) {
        fail(kSkillNotInClass);
        return;
    }
    if (IsSkillBookUnlocked(player, skillId)) {
        fail(kSkillAlreadyBookUnlocked);
        return;
    }

    if (request.pet_id() == 0) {
        if (tlsEcs.actorRegistry.try_get<PlayerSkillListComp>(player) == nullptr) {
            fail(kServiceUnavailable);
            return;
        }
    } else {
        // 先校验宝宝存在性与可写状态，确保后面不发生半写。
        if (const auto petErr = PetSystem::CheckCanUnlockSkillFromBook(player, request.pet_id());
            petErr != kSuccess) {
            fail(petErr);
            return;
        }
    }

    ClientRequestResult applied;
    applied.craftedSkillBookId = request.book_id();
    applied.craftedSkillTableId = skillId;
    applied.scrollCraftRevision = craft.scroll_craft_revision() + 1;

    ClientRequestLedgerComp nextLedger = ledger;
    if (!client_request_ledger::Remember(nextLedger, request.client_request_id(),
                                         ClientRequestKind::kLearnSkillBook, fingerprint, applied) ||
        !client_request_ledger::Validate(nextLedger).empty()) {
        fail(kServiceUnavailable);
        return;
    }

    RemoveBookAt(craft, bookIdx);
    craft.add_book_unlocked_skill_ids(skillId);
    craft.set_scroll_craft_revision(applied.scrollCraftRevision);

    // 永久解锁且绝不顶掉任何已有技能。
    if (request.pet_id() == 0) {
        auto& skillList = tlsEcs.actorRegistry.get<PlayerSkillListComp>(player);
        int existingIdx = -1;
        for (int i = 0; i < skillList.skill_list_size(); ++i) {
            if (skillList.skill_list(i).skill_table_id() == skillId) {
                existingIdx = i;
                break;
            }
        }
        if (existingIdx < 0) {
            auto* added = skillList.add_skill_list();
            added->set_skill_table_id(skillId);
            added->set_rank(1);
        } else if (skillList.skill_list(existingIdx).rank() == 0) {
            skillList.mutable_skill_list(existingIdx)->set_rank(1);
        }
        if (PlayerSkillSystem::IsPassiveSkill(*row)) {
            PlayerAttributeSystem::Recalculate(player, PlayerAttributeSystem::RecalcReason::kSkillChanged);
            PlayerAttributeSystem::PushPanel(player);
        }
    } else {
        (void)PetSystem::UnlockSkillFromBook(player, request.pet_id(), skillId);
    }

    ledger.Swap(&nextLedger);
    PlayerLifecycleSystem::RequestKeyProgressSave(player, "skill_book_learned");
    response.set_learned_skill_table_id(skillId);
    FillStateSnapshot(player, *response.mutable_state());
}

void PlayerCraftSystem::DismantleSkillBook(entt::entity player, const DismantleSkillBookRequest& request,
                                           DismantleSkillBookResponse& response) {
    response.Clear();
    response.set_client_request_id(request.client_request_id());
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (request.client_request_id() == 0 || request.book_id() == 0) {
        fail(kInvalidParameter);
        return;
    }

    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
                                                     ClientRequestKind::kDismantleSkillBook, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) {
        response.set_replayed(true);
        response.set_refunded_fragments(first.scrollBonusFragments);
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }
    if (verdict == ClientRequestVerdict::kMismatch) {
        fail(kInvalidParameter);
        return;
    }
    if (verdict != ClientRequestVerdict::kNew) {
        fail(kServiceUnavailable);
        return;
    }

    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    if (craft.scroll_craft_revision() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) {
        fail(kServiceUnavailable);
        return;
    }
    if (request.expected_revision() != craft.scroll_craft_revision()) {
        fail(kSkillCraftRevisionChanged);
        return;
    }

    const int bookIdx = FindBookIndex(craft, request.book_id());
    if (bookIdx < 0) {
        fail(kSkillBookNotFound);
        return;
    }
    const CraftedSkillBookEntry book = craft.skill_books(bookIdx);
    if (book.rarity_tier() != 0) {
        fail(kSkillBookNotDismantlable);
        return;
    }
    const auto* scroll = ScrollRow(book.scroll_id());
    if (scroll == nullptr || scroll->dismantle_fragment_count() == 0 ||
        !ValidStackableMaterial(scroll->fragment_item_id(), scroll->dismantle_fragment_count())) {
        fail(kSkillBookNotDismantlable);
        return;
    }

    InitItemParam refundParam;
    refundParam.itemPBComp.set_item_id(kInvalidGuid);
    refundParam.itemPBComp.set_config_id(scroll->fragment_item_id());
    refundParam.itemPBComp.set_size(scroll->dismantle_fragment_count());
    refundParam.itemPBComp.set_bound(book.bound());

    const auto ledgerBefore = ledger.SerializeAsString();
    const uint64_t revBefore = craft.scroll_craft_revision();
    std::vector<GuidVector> written;
    const auto exErr = BagService::ExchangeInventoryExact(
        player, {}, {refundParam}, {}, request.client_request_id(), "{\"operation\":\"skill_book_dismantle\"}", written,
        [&](const std::vector<GuidVector>&) -> uint32_t {
            if (ledger.SerializeAsString() != ledgerBefore || craft.scroll_craft_revision() != revBefore ||
                CheckWritable(player) != kSuccess) {
                return kFeatureUnavailable;
            }
            const int currentIdx = FindBookIndex(craft, request.book_id());
            if (currentIdx < 0) return kSkillBookNotFound;

            ClientRequestResult applied;
            applied.craftedSkillBookId = book.book_id();
            applied.craftedSkillTableId = book.skill_table_id();
            applied.scrollCraftRevision = revBefore + 1;
            applied.scrollBonusFragments = scroll->dismantle_fragment_count();

            ClientRequestLedgerComp nextLedger = ledger;
            if (!client_request_ledger::Remember(nextLedger, request.client_request_id(),
                                                 ClientRequestKind::kDismantleSkillBook, fingerprint, applied) ||
                !client_request_ledger::Validate(nextLedger).empty()) {
                return kServiceUnavailable;
            }
            RemoveBookAt(craft, currentIdx);
            craft.set_scroll_craft_revision(revBefore + 1);
            ledger.Swap(&nextLedger);
            return kSuccess;
        });

    if (exErr != kSuccess) {
        fail(exErr);
        return;
    }

    PlayerLifecycleSystem::RequestKeyProgressSave(player, "skill_book_dismantled");
    response.set_refunded_fragments(scroll->dismantle_fragment_count());
    FillStateSnapshot(player, *response.mutable_state());
}

void PlayerCraftSystem::SealSkillBook(entt::entity player, const SealSkillBookRequest& request,
                                      SealSkillBookResponse& response) {
    response.Clear();
    response.set_client_request_id(request.client_request_id());
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (request.client_request_id() == 0 || request.book_id() == 0) {
        fail(kInvalidParameter);
        return;
    }

    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
                                                     ClientRequestKind::kSealSkillBook, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) {
        response.set_replayed(true);
        response.set_item_id(first.sealedBookItemId);
        response.set_skill_table_id(first.craftedSkillTableId);
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }
    if (verdict == ClientRequestVerdict::kMismatch) {
        fail(kInvalidParameter);
        return;
    }
    if (verdict != ClientRequestVerdict::kNew) {
        fail(kServiceUnavailable);
        return;
    }

    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    if (craft.scroll_craft_revision() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) {
        fail(kServiceUnavailable);
        return;
    }
    if (request.expected_revision() != craft.scroll_craft_revision()) {
        fail(kSkillCraftRevisionChanged);
        return;
    }

    const int bookIdx = FindBookIndex(craft, request.book_id());
    if (bookIdx < 0) {
        fail(kSkillBookNotFound);
        return;
    }
    const CraftedSkillBookEntry book = craft.skill_books(bookIdx);
    // 防洗白硬隔离：仅未绑定常规技能书允许封卷入包流通；稀世绝学或绑定书一律拒绝。
    if (book.bound() || book.rarity_tier() != 0 || book.skill_table_id() == 0 ||
        !SkillTableManager::Instance().Exists(book.skill_table_id())) {
        fail(kInvalidParameter);
        return;
    }
    const auto* sealedItemRow = ItemTableManager::Instance().FindByIdSilent(equipcraft::kSealedSkillBookConfigId).first;
    if (sealedItemRow == nullptr || sealedItemRow->max_stack_size() != 1 || sealedItemRow->equip_kind() != 0) {
        fail(kInvalidTableData);
        return;
    }

    InitItemParam sealedParam;
    sealedParam.itemPBComp.set_item_id(kInvalidGuid);
    sealedParam.itemPBComp.set_config_id(equipcraft::kSealedSkillBookConfigId);
    sealedParam.itemPBComp.set_size(1);
    sealedParam.itemPBComp.set_bound(false);
    auto* sealedData = sealedParam.itemPBComp.mutable_sealed_skill_book();
    sealedData->set_skill_table_id(book.skill_table_id());
    sealedData->set_scroll_id(book.scroll_id());
    sealedData->set_rarity_tier(0);
    if (!book.crafter_name().empty()) sealedData->set_crafter_name(book.crafter_name());
    if (book.crafter_player_id() != 0) sealedData->set_crafter_player_id(book.crafter_player_id());

    const auto ledgerBefore = ledger.SerializeAsString();
    const uint64_t revBefore = craft.scroll_craft_revision();
    Guid createdItemId = 0;
    std::vector<GuidVector> written;
    const auto exErr = BagService::ExchangeInventoryExact(
        player, {}, {sealedParam}, {}, request.client_request_id(), "{\"operation\":\"skill_book_seal\"}", written,
        [&](const std::vector<GuidVector>& createdGuids) -> uint32_t {
            if (ledger.SerializeAsString() != ledgerBefore || craft.scroll_craft_revision() != revBefore ||
                CheckWritable(player) != kSuccess) {
                return kFeatureUnavailable;
            }
            if (createdGuids.size() != 1 || createdGuids[0].size() != 1 || createdGuids[0][0] == 0) {
                return kServiceUnavailable;
            }
            const int currentIdx = FindBookIndex(craft, request.book_id());
            if (currentIdx < 0) return kSkillBookNotFound;

            createdItemId = createdGuids[0][0];
            ClientRequestResult applied;
            applied.craftedSkillBookId = book.book_id();
            applied.craftedSkillTableId = book.skill_table_id();
            applied.scrollCraftRevision = revBefore + 1;
            applied.sealedBookItemId = createdItemId;

            ClientRequestLedgerComp nextLedger = ledger;
            if (!client_request_ledger::Remember(nextLedger, request.client_request_id(),
                                                 ClientRequestKind::kSealSkillBook, fingerprint, applied) ||
                !client_request_ledger::Validate(nextLedger).empty()) {
                return kServiceUnavailable;
            }
            RemoveBookAt(craft, currentIdx);
            craft.set_scroll_craft_revision(revBefore + 1);
            ledger.Swap(&nextLedger);
            return kSuccess;
        });

    if (exErr != kSuccess) {
        fail(exErr);
        return;
    }

    PlayerLifecycleSystem::RequestKeyProgressSave(player, "skill_book_sealed");
    response.set_item_id(createdItemId);
    response.set_skill_table_id(book.skill_table_id());
    FillStateSnapshot(player, *response.mutable_state());
}

void PlayerCraftSystem::UnsealSkillBook(entt::entity player, const UnsealSkillBookRequest& request,
                                        UnsealSkillBookResponse& response) {
    response.Clear();
    response.set_client_request_id(request.client_request_id());
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (request.client_request_id() == 0 || request.item_id() == 0) {
        fail(kInvalidParameter);
        return;
    }

    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
                                                     ClientRequestKind::kUnsealSkillBook, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) {
        response.set_replayed(true);
        const auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
        if (const int idx = FindBookIndex(craft, first.craftedSkillBookId); idx >= 0) {
            *response.mutable_unsealed_book() = craft.skill_books(idx);
        } else {
            response.mutable_unsealed_book()->set_book_id(first.craftedSkillBookId);
            response.mutable_unsealed_book()->set_skill_table_id(first.craftedSkillTableId);
        }
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }
    if (verdict == ClientRequestVerdict::kMismatch) {
        fail(kInvalidParameter);
        return;
    }
    if (verdict != ClientRequestVerdict::kNew) {
        fail(kServiceUnavailable);
        return;
    }

    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    if (craft.scroll_craft_revision() == std::numeric_limits<uint64_t>::max() ||
        craft.next_book_seq() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) {
        fail(kServiceUnavailable);
        return;
    }
    if (request.expected_revision() != craft.scroll_craft_revision()) {
        fail(kSkillCraftRevisionChanged);
        return;
    }

    const auto* item = inventory.GetItemCompByGuid(request.item_id());
    if (item == nullptr) {
        fail(kSkillBookNotFound);
        return;
    }
    if (item->config_id() != equipcraft::kSealedSkillBookConfigId || item->size() != 1 ||
        !item->has_sealed_skill_book()) {
        fail(kInvalidParameter);
        return;
    }
    const auto& sealed = item->sealed_skill_book();
    if (sealed.skill_table_id() == 0 || sealed.rarity_tier() != 0 ||
        !SkillTableManager::Instance().Exists(sealed.skill_table_id())) {
        fail(kInvalidParameter);
        return;
    }
    if (BagService::CheckRemovableInstances(player, inventory, {request.item_id()}) != BagService::RemovalVerdict::kEnough) {
        fail(kFeatureUnavailable);
        return;
    }

    const uint64_t bookId = craft.next_book_seq() == 0 ? 1u : craft.next_book_seq();
    const uint64_t revBefore = craft.scroll_craft_revision();
    CraftedSkillBookEntry newBook;
    newBook.set_book_id(bookId);
    newBook.set_skill_table_id(sealed.skill_table_id());
    newBook.set_scroll_id(sealed.scroll_id());
    newBook.set_rarity_tier(0);
    // 防洗白：若封卷道具本身带绑定标记，开卷回书架后严格继承绑定。
    newBook.set_bound(item->bound());
    if (!sealed.crafter_name().empty()) newBook.set_crafter_name(sealed.crafter_name());
    if (sealed.crafter_player_id() != 0) newBook.set_crafter_player_id(sealed.crafter_player_id());

    ClientRequestResult applied;
    applied.craftedSkillBookId = bookId;
    applied.craftedSkillTableId = sealed.skill_table_id();
    applied.scrollCraftRevision = revBefore + 1;
    applied.sealedBookItemId = request.item_id();

    ClientRequestLedgerComp nextLedger = ledger;
    if (!client_request_ledger::Remember(nextLedger, request.client_request_id(),
                                         ClientRequestKind::kUnsealSkillBook, fingerprint, applied) ||
        !client_request_ledger::Validate(nextLedger).empty()) {
        fail(kServiceUnavailable);
        return;
    }

    const auto removeErr = BagService::RemoveItem(player, inventory, request.item_id());
    if (removeErr != kSuccess) {
        fail(removeErr);
        return;
    }

    *craft.add_skill_books() = newBook;
    craft.set_next_book_seq(bookId + 1);
    craft.set_scroll_craft_revision(revBefore + 1);
    ledger.Swap(&nextLedger);

    PlayerLifecycleSystem::RequestKeyProgressSave(player, "skill_book_unsealed");
    *response.mutable_unsealed_book() = newBook;
    FillStateSnapshot(player, *response.mutable_state());
}

void PlayerCraftSystem::SetProxyStall(entt::entity player, const SetCraftProxyStallRequest& request,
                                      SetCraftProxyStallResponse& response) {
    response.Clear();
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    if (!request.open()) {
        craft.set_proxy_stall_open(false);
        PlayerLifecycleSystem::RequestKeyProgressSave(player, "craft_proxy_stall_closed");
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }

    const bool forgeMaster = equipcraft::MasteryTierForExp(craft.forge_mastery_exp()) >= equipcraft::kMasteryMaster;
    const bool scrollMaster = equipcraft::MasteryTierForExp(craft.scroll_mastery_exp()) >= equipcraft::kMasteryMaster;
    if (!forgeMaster && !scrollMaster) {
        fail(kSkillProxyStallRequiresMaster);
        return;
    }
    if (forgeMaster && !equipcraft::ValidProxyFee(request.proxy_forge_fee_gold())) {
        fail(kSkillProxyFeeInvalid);
        return;
    }
    if (!forgeMaster && request.proxy_forge_fee_gold() != 0 && !equipcraft::ValidProxyFee(request.proxy_forge_fee_gold())) {
        fail(kSkillProxyFeeInvalid);
        return;
    }
    if (scrollMaster && !equipcraft::ValidProxyFee(request.proxy_scroll_fee_gold())) {
        fail(kSkillProxyFeeInvalid);
        return;
    }
    if (!scrollMaster && request.proxy_scroll_fee_gold() != 0 && !equipcraft::ValidProxyFee(request.proxy_scroll_fee_gold())) {
        fail(kSkillProxyFeeInvalid);
        return;
    }

    craft.set_proxy_stall_open(true);
    craft.set_proxy_forge_fee_gold(forgeMaster ? request.proxy_forge_fee_gold() : 0u);
    craft.set_proxy_scroll_fee_gold(scrollMaster ? request.proxy_scroll_fee_gold() : 0u);
    PlayerLifecycleSystem::RequestKeyProgressSave(player, "craft_proxy_stall_opened");
    FillStateSnapshot(player, *response.mutable_state());
}

void PlayerCraftSystem::ListProxyStalls(entt::entity player, ListCraftProxyStallsResponse& response) {
    response.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) {
        response.mutable_error_message()->set_id(kEntityIsNull);
        return;
    }
    std::vector<uint64_t> masterIds;
    for (const auto& [pid, entity] : tlsEcs.playerList) {
        if (pid == 0 || !tlsEcs.actorRegistry.valid(entity) || !PlayerLifecycleSystem::IsPlayerMutable(entity)) {
            continue;
        }
        const auto* craft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(entity);
        if (craft == nullptr || !craft->proxy_stall_open()) continue;
        const uint32_t forgeTier = equipcraft::MasteryTierForExp(craft->forge_mastery_exp());
        const uint32_t scrollTier = equipcraft::MasteryTierForExp(craft->scroll_mastery_exp());
        if (forgeTier < equipcraft::kMasteryMaster && scrollTier < equipcraft::kMasteryMaster) continue;
        masterIds.push_back(pid);
    }
    std::sort(masterIds.begin(), masterIds.end());
    for (const auto pid : masterIds) {
        const entt::entity entity = tlsEcs.GetPlayer(pid);
        const auto* craft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(entity);
        if (craft == nullptr) continue;
        const uint32_t forgeTier = equipcraft::MasteryTierForExp(craft->forge_mastery_exp());
        const uint32_t scrollTier = equipcraft::MasteryTierForExp(craft->scroll_mastery_exp());
        auto* stall = response.add_stalls();
        stall->set_master_player_id(pid);
        stall->set_master_name(DisplayNameOf(entity, pid));
        stall->set_forge_mastery_tier(forgeTier);
        stall->set_scroll_mastery_tier(scrollTier);
        stall->set_top_quality_rate_bps(equipcraft::ForgeTopQualityRateBps(forgeTier));
        stall->set_scroll_bonus_rate_bps(equipcraft::ScrollBonusFragmentRateBps(scrollTier));
        stall->set_proxy_forge_fee_gold(forgeTier >= equipcraft::kMasteryMaster ? craft->proxy_forge_fee_gold() : 0u);
        stall->set_proxy_scroll_fee_gold(scrollTier >= equipcraft::kMasteryMaster ? craft->proxy_scroll_fee_gold() : 0u);
    }
}
