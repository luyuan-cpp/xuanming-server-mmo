#include "player_trial_journal.h"

#include <algorithm>
#include <limits>
#include <map>
#include <set>
#include <string>
#include <utility>
#include <vector>

#include <muduo/base/Logging.h>

#include "battle/system/player_battle.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/utils/bit_index/bit_index_util.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "player/system/client_request_ledger.h"
#include "player/system/player_appearance.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "player/system/player_season.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/player_trial_journal_comp.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"
#include "proto/scene/player_trial_journal.pb.h"
#include "services/battle/system/trial_journal_rules.h"
#include "table/code/bit_index/trialjournalmilestone_table_id_bit_index.h"
#include "table/code/bit_index/trialjournalseal_table_id_bit_index.h"
#include "table/code/characterappearance_table.h"
#include "table/code/equipartifact_table.h"
#include "table/code/item_table.h"
#include "table/code/trialjournalentry_table.h"
#include "table/code/trialjournalmilestone_table.h"
#include "table/code/trialjournalseal_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/trial_journal_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {

PlayerTrialJournalSystem::PersistFn gTrialJournalPersistFn = &PlayerLifecycleSystem::SavePlayerForAssetCommit;

bool IsSeasonRowVisible(uint32_t rowSeasonId, uint32_t activeSeasonId) {
    return rowSeasonId == 0 || (activeSeasonId != 0 && rowSeasonId == activeSeasonId);
}

const SeasonTrialJournalArchive* FindSeasonArchive(const PlayerTrialJournalComp& comp, uint32_t seasonId) {
    for (const auto& archive : comp.season_records()) {
        if (archive.season_id() == seasonId) {
            return &archive;
        }
    }
    return nullptr;
}

SeasonTrialJournalArchive* FindOrAddSeasonArchive(PlayerTrialJournalComp& comp, uint32_t seasonId) {
    for (auto& archive : *comp.mutable_season_records()) {
        if (archive.season_id() == seasonId) {
            return &archive;
        }
    }
    auto* added = comp.add_season_records();
    added->set_season_id(seasonId);
    return added;
}

bool ContainsUint32(const google::protobuf::RepeatedField<uint32_t>& values, uint32_t target) {
    return std::find(values.begin(), values.end(), target) != values.end();
}

bool IsSealLit(const PlayerTrialJournalComp& comp, uint32_t sealId, uint32_t entrySeasonId) {
    if (entrySeasonId != 0) {
        const auto* archive = FindSeasonArchive(comp, entrySeasonId);
        return archive != nullptr && ContainsUint32(archive->lit_seal_ids(), sealId);
    }
    const auto bitIdx = GetBitIndex(TrialJournalSealBitMap, sealId);
    return bitIdx.has_value() && TestPackedBit(comp.lit_seal_bits(), *bitIdx);
}

bool MarkSealLit(PlayerTrialJournalComp& comp, uint32_t sealId, uint32_t entrySeasonId) {
    if (entrySeasonId != 0) {
        auto* archive = FindOrAddSeasonArchive(comp, entrySeasonId);
        if (ContainsUint32(archive->lit_seal_ids(), sealId)) {
            return false;
        }
        archive->add_lit_seal_ids(sealId);
        return true;
    }
    const auto bitIdx = GetBitIndex(TrialJournalSealBitMap, sealId);
    if (!bitIdx.has_value()) {
        return false;
    }
    if (TestPackedBit(comp.lit_seal_bits(), *bitIdx)) {
        return false;
    }
    SetPackedBit(*comp.mutable_lit_seal_bits(), *bitIdx);
    return true;
}

bool IsMilestoneClaimed(const PlayerTrialJournalComp& comp, uint32_t milestoneId, uint32_t milestoneSeasonId) {
    if (milestoneSeasonId != 0) {
        const auto* archive = FindSeasonArchive(comp, milestoneSeasonId);
        return archive != nullptr && ContainsUint32(archive->claimed_milestone_ids(), milestoneId);
    }
    const auto bitIdx = GetBitIndex(TrialJournalMilestoneBitMap, milestoneId);
    return bitIdx.has_value() && TestPackedBit(comp.claimed_milestone_bits(), *bitIdx);
}

bool MarkMilestoneClaimed(PlayerTrialJournalComp& comp, uint32_t milestoneId, uint32_t milestoneSeasonId) {
    if (milestoneSeasonId != 0) {
        auto* archive = FindOrAddSeasonArchive(comp, milestoneSeasonId);
        if (ContainsUint32(archive->claimed_milestone_ids(), milestoneId)) {
            return false;
        }
        archive->add_claimed_milestone_ids(milestoneId);
        return true;
    }
    const auto bitIdx = GetBitIndex(TrialJournalMilestoneBitMap, milestoneId);
    if (!bitIdx.has_value()) {
        return false;
    }
    if (TestPackedBit(comp.claimed_milestone_bits(), *bitIdx)) {
        return false;
    }
    SetPackedBit(*comp.mutable_claimed_milestone_bits(), *bitIdx);
    return true;
}

const TrialJournalEntryRecord* FindEntryRecord(const PlayerTrialJournalComp& comp,
                                               uint32_t entryId,
                                               uint32_t ruleVersion) {
    for (const auto& record : comp.records()) {
        if (record.entry_id() == entryId && record.rule_version() == ruleVersion) {
            return &record;
        }
    }
    return nullptr;
}

bool HasAnyLegacyEntryRecord(const PlayerTrialJournalComp& comp,
                             uint32_t entryId,
                             uint32_t currentRuleVersion) {
    for (const auto& record : comp.records()) {
        if (record.entry_id() == entryId && record.rule_version() != currentRuleVersion &&
            (record.solo_best().best_rounds() > 0 || record.coop_best().best_rounds() > 0)) {
            return true;
        }
    }
    return false;
}

TrialJournalEntryRecord* FindOrAddEntryRecord(PlayerTrialJournalComp& comp,
                                              uint32_t entryId,
                                              uint32_t ruleVersion) {
    for (auto& record : *comp.mutable_records()) {
        if (record.entry_id() == entryId && record.rule_version() == ruleVersion) {
            return &record;
        }
    }
    auto* added = comp.add_records();
    added->set_entry_id(entryId);
    added->set_rule_version(ruleVersion);
    return added;
}

void BumpRevision(PlayerTrialJournalComp& comp) {
    if (comp.state_revision() < std::numeric_limits<uint64_t>::max()) {
        comp.set_state_revision(comp.state_revision() + 1);
    }
}

uint32_t CheckWritable(entt::entity player) {
    if (!player_mutability::IsPlayerMutable(player)) {
        return kTrialJournalFrozen;
    }
    if (PlayerBattleSystem::IsInBattle(player)) {
        return kTrialJournalInBattle;
    }
    return kSuccess;
}

// 统计当前有效目录中各卷及永久汇总的已点亮战法印数（严格按有效目录行过滤，禁用 bitset.count）。
struct EffectiveSealCounts {
    std::map<uint32_t, uint32_t> litByChapter;
    std::map<uint32_t, uint32_t> totalByChapter;
    uint32_t permanentLit{0};
    uint32_t permanentTotal{0};
    bool anyNormalBreakthroughCleared{false};
};

EffectiveSealCounts CountEffectiveSeals(const PlayerTrialJournalComp& comp, uint32_t activeSeasonId) {
    EffectiveSealCounts counts;
    for (const auto& entry : TrialJournalEntryTableManager::Instance().FindAll().data()) {
        if (!entry.enabled() || !IsSeasonRowVisible(entry.season_id(), activeSeasonId)) {
            continue;
        }
        for (const auto* seal : TrialJournalSealTableManager::Instance().GetByEntryId(entry.id())) {
            if (seal == nullptr || !seal->enabled()) {
                continue;
            }
            const bool lit = IsSealLit(comp, seal->id(), entry.season_id());
            ++counts.totalByChapter[entry.chapter_id()];
            if (lit) {
                ++counts.litByChapter[entry.chapter_id()];
            }
            if (entry.season_id() == 0) {
                ++counts.permanentTotal;
                if (lit) {
                    ++counts.permanentLit;
                }
            }
            if (lit && entry.chapter_id() == trialjournal::kChapterBreakthrough &&
                entry.difficulty() == 1 &&
                seal->condition_type() == static_cast<uint32_t>(trialjournal::SealConditionType::kClear)) {
                counts.anyNormalBreakthroughCleared = true;
            }
        }
    }
    return counts;
}

uint32_t MilestoneCurrentSeals(const EffectiveSealCounts& counts, uint32_t scopeChapterId) {
    if (scopeChapterId == trialjournal::kScopeAllPermanent) {
        return counts.permanentLit;
    }
    const auto it = counts.litByChapter.find(scopeChapterId);
    return it == counts.litByChapter.end() ? 0u : it->second;
}

std::string FindAppearanceDisplayName(const std::string& appearanceId) {
    if (appearanceId.empty()) {
        return {};
    }
    for (const auto& row : CharacterAppearanceTableManager::Instance().FindAll().data()) {
        if (row.appearance_id() == appearanceId) {
            return row.name();
        }
    }
    return {};
}

bool IsValidExclusiveAppearance(const std::string& appearanceId) {
    if (appearanceId.empty()) {
        return true;
    }
    for (const auto& row : CharacterAppearanceTableManager::Instance().FindAll().data()) {
        if (row.appearance_id() == appearanceId) {
            return row.enabled() != 0 && row.card_unlockable() == 0;
        }
    }
    return false;
}

bool IsValidRedeemableArtifactItem(const ItemTable& row) {
    if (row.max_stack_size() != 1 || row.equip_profile() != 3 || row.equip_kind() != 16 ||
        row.artifact_id() < trialjournal::kMinArtifactEffectId ||
        row.artifact_id() > trialjournal::kMaxArtifactEffectId) {
        return false;
    }
    return EquipArtifactTableManager::Instance().FindByIdSilent(row.artifact_id()).first != nullptr;
}

uint32_t CountOwnedItemAcrossBags(const PlayerBagsComp& bags, uint32_t configId) {
    uint32_t total = 0;
    for (const auto bagType : {kInventory, kEquipment}) {
        bags.bags[bagType].ForEachItem([&](Guid /*id*/, const ItemComp& item) {
            if (item.config_id() == configId) {
                total += item.size();
            }
        });
    }
    return total;
}

uint32_t FillSnapshot(entt::entity player, uint64_t nowMs, TrialJournalSnapshot& out) {
    out.Clear();
    out.set_server_now_ms(nowMs);
    if (!tlsEcs.actorRegistry.valid(player)) {
        return kEntityIsNull;
    }
    const auto* journal = tlsEcs.actorRegistry.try_get<PlayerTrialJournalComp>(player);
    const auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
    if (journal == nullptr || bags == nullptr) {
        return kTrialJournalUnavailable;
    }

    const uint32_t activeSeasonId = PlayerSeasonSystem::ActiveSeasonId(nowMs);
    const auto counts = CountEffectiveSeals(*journal, activeSeasonId);

    // 收集当前全部有效目录行，按 chapter_id 与 sort_order 排序。
    std::map<uint32_t, std::vector<const TrialJournalEntryTable*>> entriesByChapter;
    for (const auto& entry : TrialJournalEntryTableManager::Instance().FindAll().data()) {
        if (!entry.enabled() || !IsSeasonRowVisible(entry.season_id(), activeSeasonId)) {
            continue;
        }
        entriesByChapter[entry.chapter_id()].push_back(&entry);
    }

    out.set_state_revision(journal->state_revision());
    out.set_total_permanent_lit_seals(counts.permanentLit);
    out.set_total_permanent_seals(counts.permanentTotal);
    out.set_voucher_balance(
        BagService::RemovableCount(player, bags->bags[kInventory], trialjournal::kBoundArtifactVoucherItemId));
    out.set_bound_sand_balance(
        BagService::RemovableCount(player, bags->bags[kInventory], trialjournal::kBoundReforgeSandItemId));

    const uint32_t writableErr = CheckWritable(player);
    out.set_can_operate(writableErr == kSuccess);
    if (writableErr == kTrialJournalInBattle) {
        out.set_action_unavailable_reason("战斗中暂不能领取里程碑或兑换法宝，请在结算后重试");
    } else if (writableErr != kSuccess) {
        out.set_action_unavailable_reason("当前角色状态暂时不能操作试炼志，请稍后再试");
    }

    if (entriesByChapter.empty()) {
        out.set_available(false);
        out.set_unavailable_reason("试炼志暂不可用");
        return kSuccess;
    }
    out.set_available(true);

    for (auto& [chapterId, entries] : entriesByChapter) {
        std::sort(entries.begin(), entries.end(), [](const TrialJournalEntryTable* lhs, const TrialJournalEntryTable* rhs) {
            return lhs->sort_order() != rhs->sort_order() ? lhs->sort_order() < rhs->sort_order() : lhs->id() < rhs->id();
        });
        auto* chapterView = out.add_chapters();
        chapterView->set_chapter_id(chapterId);
        chapterView->set_season_id(entries.front()->season_id());
        chapterView->set_chapter_name(entries.front()->chapter_name());
        chapterView->set_lit_seal_count(counts.litByChapter.contains(chapterId) ? counts.litByChapter.at(chapterId) : 0u);
        chapterView->set_total_seal_count(counts.totalByChapter.contains(chapterId) ? counts.totalByChapter.at(chapterId) : 0u);

        for (const auto* entry : entries) {
            auto* entryView = chapterView->add_entries();
            entryView->set_entry_id(entry->id());
            entryView->set_chapter_id(entry->chapter_id());
            entryView->set_season_id(entry->season_id());
            entryView->set_source_kind(entry->source_kind());
            entryView->set_difficulty(entry->difficulty());
            entryView->set_variant_key(entry->variant_key());
            entryView->set_rule_version(entry->rule_version());
            entryView->set_rule_version_name(entry->rule_version_name());
            entryView->set_sort_order(entry->sort_order());
            entryView->set_display_name(entry->display_name());
            entryView->set_icon_key(entry->icon_key());
            entryView->set_desc(entry->desc());
            entryView->set_jump_target(entry->jump_target());

            std::vector<const TrialJournalSealTable*> seals;
            for (const auto* seal : TrialJournalSealTableManager::Instance().GetByEntryId(entry->id())) {
                if (seal != nullptr && seal->enabled()) {
                    seals.push_back(seal);
                }
            }
            std::sort(seals.begin(), seals.end(), [](const TrialJournalSealTable* lhs, const TrialJournalSealTable* rhs) {
                return lhs->id() < rhs->id();
            });
            uint32_t entryLit = 0;
            for (const auto* seal : seals) {
                const bool lit = IsSealLit(*journal, seal->id(), entry->season_id());
                if (lit) {
                    ++entryLit;
                }
                auto* sealView = entryView->add_seals();
                sealView->set_seal_id(seal->id());
                sealView->set_seal_slot_kind(seal->seal_slot_kind());
                sealView->set_condition_type(seal->condition_type());
                sealView->set_condition_param(seal->condition_param());
                sealView->set_title(seal->title());
                sealView->set_desc(seal->desc());
                sealView->set_lit(lit);
            }
            entryView->set_lit_seal_count(entryLit);
            entryView->set_total_seal_count(static_cast<uint32_t>(seals.size()));

            const auto* record = FindEntryRecord(*journal, entry->id(), entry->rule_version());
            const bool hasSolo = record != nullptr && record->solo_best().best_rounds() > 0;
            const bool hasCoop = record != nullptr && record->coop_best().best_rounds() > 0;
            entryView->set_solo_recorded(hasSolo);
            if (hasSolo) {
                *entryView->mutable_solo_best() = record->solo_best();
            }
            entryView->set_coop_recorded(hasCoop);
            if (hasCoop) {
                *entryView->mutable_coop_best() = record->coop_best();
            }
            entryView->set_has_legacy_record_only(
                !hasSolo && !hasCoop && HasAnyLegacyEntryRecord(*journal, entry->id(), entry->rule_version()));

            if (writableErr == kTrialJournalInBattle) {
                entryView->set_can_enter(false);
                entryView->set_enter_blocked_reason("当前正在战斗中，暂不能前往试炼");
            } else if (entry->unlock_mode() == trialjournal::kUnlockModeRequireAnyNormalBreakthroughClear &&
                       !counts.anyNormalBreakthroughCleared) {
                entryView->set_can_enter(false);
                entryView->set_enter_blocked_reason("本人普通首领试炼通关一次后开启困难");
            } else {
                entryView->set_can_enter(writableErr == kSuccess);
                if (writableErr != kSuccess) {
                    entryView->set_enter_blocked_reason("当前状态暂不可前往试炼");
                }
            }
        }
    }

    // 里程碑目录（按 scope_chapter_id、required_seals、id 升序输出）。
    std::vector<const TrialJournalMilestoneTable*> milestones;
    for (const auto& milestone : TrialJournalMilestoneTableManager::Instance().FindAll().data()) {
        if (!milestone.enabled() || !IsSeasonRowVisible(milestone.season_id(), activeSeasonId)) {
            continue;
        }
        milestones.push_back(&milestone);
    }
    std::sort(milestones.begin(), milestones.end(),
              [](const TrialJournalMilestoneTable* lhs, const TrialJournalMilestoneTable* rhs) {
                  if (lhs->scope_chapter_id() != rhs->scope_chapter_id()) {
                      return lhs->scope_chapter_id() < rhs->scope_chapter_id();
                  }
                  if (lhs->required_seals() != rhs->required_seals()) {
                      return lhs->required_seals() < rhs->required_seals();
                  }
                  return lhs->id() < rhs->id();
              });
    for (const auto* milestone : milestones) {
        const uint32_t currentSeals = MilestoneCurrentSeals(counts, milestone->scope_chapter_id());
        const bool reached = currentSeals >= milestone->required_seals();
        const bool claimed = IsMilestoneClaimed(*journal, milestone->id(), milestone->season_id());
        auto* view = out.add_milestones();
        view->set_milestone_id(milestone->id());
        view->set_scope_chapter_id(milestone->scope_chapter_id());
        view->set_season_id(milestone->season_id());
        view->set_required_seals(milestone->required_seals());
        view->set_current_seals(currentSeals);
        view->set_title(milestone->title());
        view->set_desc(milestone->desc());
        view->set_bound_sand_count(milestone->bound_sand_count());
        view->set_bound_artifact_voucher_count(milestone->bound_artifact_voucher_count());
        view->set_badge_key(milestone->badge_key());
        view->set_badge_name(milestone->badge_name());
        view->set_commentary_key(milestone->commentary_key());
        view->set_commentary_title(milestone->commentary_title());
        view->set_commentary_body(milestone->commentary_body());
        view->set_grant_appearance_id(milestone->grant_appearance_id());
        view->set_grant_appearance_name(FindAppearanceDisplayName(milestone->grant_appearance_id()));
        view->set_reached(reached);
        view->set_claimed(claimed);
        view->set_claimable(reached && !claimed && writableErr == kSuccess);
    }

    // 8 件可凭「自用法宝拓印」兑换的 V1 法宝列表（按 artifact_id 升序）。
    std::vector<const ItemTable*> artifactItems;
    for (const auto& item : ItemTableManager::Instance().FindAll().data()) {
        if (IsValidRedeemableArtifactItem(item)) {
            artifactItems.push_back(&item);
        }
    }
    std::sort(artifactItems.begin(), artifactItems.end(), [](const ItemTable* lhs, const ItemTable* rhs) {
        return lhs->artifact_id() != rhs->artifact_id() ? lhs->artifact_id() < rhs->artifact_id() : lhs->id() < rhs->id();
    });
    for (const auto* item : artifactItems) {
        const auto* artifact = EquipArtifactTableManager::Instance().FindByIdSilent(item->artifact_id()).first;
        if (artifact == nullptr) {
            continue;
        }
        auto* option = out.add_redeemable_artifacts();
        option->set_item_config_id(item->id());
        option->set_artifact_effect_id(artifact->id());
        option->set_name(artifact->name());
        option->set_description(artifact->description());
        option->set_icon_key(item->icon_key());
        option->set_owned_count(CountOwnedItemAcrossBags(*bags, item->id()));
    }
    return kSuccess;
}

} // namespace

bool PlayerTrialJournalSystem::ValidateTables() {
    const auto& entries = TrialJournalEntryTableManager::Instance().FindAll().data();
    const auto& seals = TrialJournalSealTableManager::Instance().FindAll().data();
    const auto& milestones = TrialJournalMilestoneTableManager::Instance().FindAll().data();

    if (entries.empty()) {
        if (!seals.empty() || !milestones.empty()) {
            LOG_ERROR << "[PlayerTrialJournal] table validation failed: Seal/Milestone has rows while Entry is empty";
            return false;
        }
        return true;
    }

    if (!ItemTableManager::Instance().Exists(trialjournal::kBoundReforgeSandItemId) ||
        !ItemTableManager::Instance().Exists(trialjournal::kBoundArtifactVoucherItemId)) {
        LOG_ERROR << "[PlayerTrialJournal] table validation failed: missing bound sand(3403) or voucher(3404) in Item";
        return false;
    }

    for (const auto& entry : entries) {
        if (entry.id() == 0 || entry.chapter_id() == 0 || entry.rule_version() == 0) {
            LOG_ERROR << "[PlayerTrialJournal] table validation failed: invalid entry_id=" << entry.id();
            return false;
        }
    }

    for (const auto& seal : seals) {
        const auto* entry = TrialJournalEntryTableManager::Instance().FindByIdSilent(seal.entry_id()).first;
        if (entry == nullptr) {
            LOG_ERROR << "[PlayerTrialJournal] table validation failed: seal_id=" << seal.id()
                      << " references missing entry_id=" << seal.entry_id();
            return false;
        }
        if (!trialjournal::IsValidSealSlotKind(seal.seal_slot_kind()) ||
            !trialjournal::IsValidConditionType(seal.condition_type())) {
            LOG_ERROR << "[PlayerTrialJournal] table validation failed: seal_id=" << seal.id()
                      << " has invalid slot_kind or condition_type";
            return false;
        }
        if (entry->season_id() == 0 && !GetBitIndex(TrialJournalSealBitMap, seal.id()).has_value()) {
            LOG_ERROR << "[PlayerTrialJournal] table validation failed: permanent seal_id=" << seal.id()
                      << " missing cfg_bit_index mapping";
            return false;
        }
    }

    for (const auto& milestone : milestones) {
        if (milestone.id() == 0 || milestone.required_seals() == 0) {
            LOG_ERROR << "[PlayerTrialJournal] table validation failed: invalid milestone_id=" << milestone.id();
            return false;
        }
        if (milestone.season_id() == 0 && !GetBitIndex(TrialJournalMilestoneBitMap, milestone.id()).has_value()) {
            LOG_ERROR << "[PlayerTrialJournal] table validation failed: permanent milestone_id=" << milestone.id()
                      << " missing cfg_bit_index mapping";
            return false;
        }
        if (!IsValidExclusiveAppearance(milestone.grant_appearance_id())) {
            LOG_ERROR << "[PlayerTrialJournal] table validation failed: milestone_id=" << milestone.id()
                      << " grant_appearance_id=" << milestone.grant_appearance_id()
                      << " is missing, disabled, or card_unlockable!=0";
            return false;
        }
    }
    return true;
}

bool PlayerTrialJournalSystem::HasJournalUpdate(const BattleSettlementData& settlement) {
    if (!trialjournal::IsValidTrialSettlement(settlement)) {
        return false;
    }
    const auto* entry =
        TrialJournalEntryTableManager::Instance().FindByIdSilent(settlement.trial_facts().journal_entry_id()).first;
    return entry != nullptr;
}

uint32_t PlayerTrialJournalSystem::PrepareBattleCredit(const PlayerTrialJournalComp& current,
                                                       const BattleSettlementData& settlement,
                                                       uint64_t nowMs,
                                                       PlayerTrialJournalComp& candidate,
                                                       SettlementDelta& delta) {
    candidate = current;
    delta = SettlementDelta{};
    if (!trialjournal::IsValidTrialSettlement(settlement)) {
        return kSuccess;
    }

    const auto& facts = settlement.trial_facts();
    const auto* entry = TrialJournalEntryTableManager::Instance().FindByIdSilent(facts.journal_entry_id()).first;
    if (entry == nullptr) {
        return kSuccess;
    }
    delta.entryId = entry->id();

    // 1. 评估当前关卡格全部有效战法印（跨 rule_version 永久保留，点亮即不可逆）。
    std::vector<const TrialJournalSealTable*> seals;
    for (const auto* seal : TrialJournalSealTableManager::Instance().GetByEntryId(entry->id())) {
        if (seal != nullptr && seal->enabled()) {
            seals.push_back(seal);
        }
    }
    std::sort(seals.begin(), seals.end(), [](const TrialJournalSealTable* lhs, const TrialJournalSealTable* rhs) {
        return lhs->id() < rhs->id();
    });
    for (const auto* seal : seals) {
        if (!trialjournal::EvaluateSealCondition(seal->condition_type(), seal->condition_param(), settlement)) {
            continue;
        }
        if (MarkSealLit(candidate, seal->id(), entry->season_id())) {
            delta.newlyLitSealIds.push_back(seal->id());
            delta.changed = true;
        }
    }

    // 2. 只有胜出才刷新对应 (entry_id, frozen_rule_version) 的单人或好友合战最佳回合（迟到旧版只更新旧版，不覆写新版）。
    if (trialjournal::IsPlayerVictory(settlement.outcome(), settlement.player_team_index())) {
        const uint64_t achievedAtMs = settlement.finished_at_ms() != 0 ? settlement.finished_at_ms() : nowMs;
        if (trialjournal::IsSoloTrial(facts)) {
            const auto* existing = FindEntryRecord(candidate, entry->id(), facts.trial_rule_version());
            const uint32_t oldRounds = existing != nullptr ? existing->solo_best().best_rounds() : 0u;
            if (oldRounds == 0 || settlement.total_rounds() < oldRounds) {
                auto* record = FindOrAddEntryRecord(candidate, entry->id(), facts.trial_rule_version());
                auto* solo = record->mutable_solo_best();
                solo->Clear();
                solo->set_best_rounds(settlement.total_rounds());
                solo->set_achieved_at_ms(achievedAtMs);
                auto* loadout = solo->mutable_loadout();
                loadout->set_pet_table_id(facts.used_pet_table_id());
                for (const uint32_t effectId : trialjournal::NormalizeArtifactEffectIds(facts.used_artifact_effect_ids())) {
                    loadout->add_artifact_effect_ids(effectId);
                }
                loadout->set_season_seal_id(facts.used_season_seal_id());
                delta.newSoloBest = true;
                delta.changed = true;
            }
        } else if (trialjournal::IsCoopTrial(facts)) {
            const auto* existing = FindEntryRecord(candidate, entry->id(), facts.trial_rule_version());
            const uint32_t oldRounds = existing != nullptr ? existing->coop_best().best_rounds() : 0u;
            if (oldRounds == 0 || settlement.total_rounds() < oldRounds) {
                auto* record = FindOrAddEntryRecord(candidate, entry->id(), facts.trial_rule_version());
                auto* coop = record->mutable_coop_best();
                coop->Clear();
                coop->set_best_rounds(settlement.total_rounds());
                coop->set_party_size(facts.initial_human_count());
                coop->set_achieved_at_ms(achievedAtMs);
                coop->set_pet_table_id(facts.used_pet_table_id());
                const int maxNames = std::min<int>(facts.teammate_names_size(),
                                                   static_cast<int>(trialjournal::kMaxCoopTeammates));
                for (int i = 0; i < maxNames; ++i) {
                    if (!facts.teammate_names(i).empty()) {
                        coop->add_teammate_names(facts.teammate_names(i));
                    }
                }
                delta.newCoopBest = true;
                delta.changed = true;
            }
        }
    }

    if (delta.changed) {
        BumpRevision(candidate);
    }
    return kSuccess;
}

uint32_t PlayerTrialJournalSystem::GetJournal(entt::entity player, uint64_t nowMs, GetTrialJournalResponse& response) {
    response.Clear();
    return FillSnapshot(player, nowMs, *response.mutable_snapshot());
}

void PlayerTrialJournalSystem::ClaimMilestone(entt::entity player,
                                              uint64_t nowMs,
                                              const ClaimTrialJournalMilestoneRequest& request,
                                              ClaimTrialJournalMilestoneResponse& response) {
    response.Clear();
    const auto finish = [&](uint32_t err) {
        if (err != kSuccess) {
            response.mutable_error_message()->set_id(err);
        }
        if (const uint32_t snapErr = FillSnapshot(player, nowMs, *response.mutable_snapshot()); snapErr != kSuccess) {
            response.clear_snapshot();
            if (err == kSuccess) {
                response.mutable_error_message()->set_id(snapErr);
            }
        }
    };

    if (!tlsEcs.actorRegistry.valid(player)) {
        response.mutable_error_message()->set_id(kEntityIsNull);
        return;
    }
    const uint64_t clientRequestId = request.client_request_id();
    if (clientRequestId == 0 || request.milestone_id() == 0) {
        finish(kInvalidParameter);
        return;
    }

    // 1. 幂等查账排在一切状态校验之前，防止超时重发误报已领取或双发道具。
    std::string fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    switch (client_request_dedup::Check(player, clientRequestId, ClientRequestKind::kClaimTrialJournalMilestone,
                                        fingerprint, first)) {
    case ClientRequestVerdict::kReplay: {
        response.set_replayed(true);
        response.set_granted_sand_count(first.trialMilestoneSandCount);
        response.set_granted_voucher_count(first.trialMilestoneVoucherCount);
        if (const auto* row = TrialJournalMilestoneTableManager::Instance().FindByIdSilent(first.trialMilestoneId).first;
            row != nullptr && !row->grant_appearance_id().empty()) {
            response.set_granted_appearance_id(row->grant_appearance_id());
        }
        finish(kSuccess);
        return;
    }
    case ClientRequestVerdict::kMismatch:
        finish(kInvalidParameter);
        return;
    case ClientRequestVerdict::kUnavailable:
        finish(kServiceUnavailable);
        return;
    default:
        break;
    }

    auto* journal = tlsEcs.actorRegistry.try_get<PlayerTrialJournalComp>(player);
    auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
    auto* ledger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
    if (journal == nullptr || bags == nullptr || ledger == nullptr ||
        tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player)) {
        finish(kTrialJournalUnavailable);
        return;
    }
    if (const uint32_t writableErr = CheckWritable(player); writableErr != kSuccess) {
        finish(writableErr);
        return;
    }

    const auto* milestone = TrialJournalMilestoneTableManager::Instance().FindByIdSilent(request.milestone_id()).first;
    const uint32_t activeSeasonId = PlayerSeasonSystem::ActiveSeasonId(nowMs);
    if (milestone == nullptr || !milestone->enabled() || !IsSeasonRowVisible(milestone->season_id(), activeSeasonId)) {
        finish(kTrialJournalMilestoneNotFound);
        return;
    }
    if (IsMilestoneClaimed(*journal, milestone->id(), milestone->season_id())) {
        finish(kTrialJournalMilestoneAlreadyClaimed);
        return;
    }
    const auto counts = CountEffectiveSeals(*journal, activeSeasonId);
    if (MilestoneCurrentSeals(counts, milestone->scope_chapter_id()) < milestone->required_seals()) {
        finish(kTrialJournalMilestoneNotReached);
        return;
    }
    if (!IsValidExclusiveAppearance(milestone->grant_appearance_id())) {
        finish(kTrialJournalUnavailable);
        return;
    }

    // 2. 构造仅自用道具批次并在任何副作用之前严格预检主背包空间（满包绝不先置位）。
    ItemCountMap gainedCounts;
    std::vector<InitItemParam> gainedParams;
    if (milestone->bound_sand_count() > 0) {
        gainedCounts[trialjournal::kBoundReforgeSandItemId] += milestone->bound_sand_count();
        InitItemParam sandParam;
        sandParam.itemPBComp.set_item_id(kInvalidGuid);
        sandParam.itemPBComp.set_config_id(trialjournal::kBoundReforgeSandItemId);
        sandParam.itemPBComp.set_size(milestone->bound_sand_count());
        gainedParams.push_back(std::move(sandParam));
    }
    if (milestone->bound_artifact_voucher_count() > 0) {
        gainedCounts[trialjournal::kBoundArtifactVoucherItemId] += milestone->bound_artifact_voucher_count();
        InitItemParam voucherParam;
        voucherParam.itemPBComp.set_item_id(kInvalidGuid);
        voucherParam.itemPBComp.set_config_id(trialjournal::kBoundArtifactVoucherItemId);
        voucherParam.itemPBComp.set_size(milestone->bound_artifact_voucher_count());
        gainedParams.push_back(std::move(voucherParam));
    }

    if (!gainedCounts.empty()) {
        if (const uint32_t spaceErr = BagService::CheckInventoryExchangeSpace(player, {}, gainedCounts);
            spaceErr != kSuccess) {
            finish(kTrialJournalBagFull);
            return;
        }
    }

    PlayerTrialJournalComp nextJournal = *journal;
    if (!MarkMilestoneClaimed(nextJournal, milestone->id(), milestone->season_id())) {
        finish(kTrialJournalMilestoneAlreadyClaimed);
        return;
    }
    BumpRevision(nextJournal);

    ClientRequestResult applied;
    applied.trialMilestoneId = milestone->id();
    applied.trialMilestoneSandCount = milestone->bound_sand_count();
    applied.trialMilestoneVoucherCount = milestone->bound_artifact_voucher_count();

    const std::string journalBefore = journal->SerializeAsString();
    const std::string ledgerBefore = ledger->SerializeAsString();

    const auto commitState = [&]() -> uint32_t {
        if (journal->SerializeAsString() != journalBefore || ledger->SerializeAsString() != ledgerBefore ||
            CheckWritable(player) != kSuccess) {
            return kTrialJournalUnavailable;
        }
        ClientRequestLedgerComp nextLedger = *ledger;
        if (!client_request_ledger::Remember(nextLedger, clientRequestId, ClientRequestKind::kClaimTrialJournalMilestone,
                                             fingerprint, applied) ||
            !client_request_ledger::Validate(nextLedger).empty()) {
            return kTrialJournalUnavailable;
        }
        if (!milestone->grant_appearance_id().empty()) {
            const uint32_t grantErr =
                PlayerAppearanceSystem::GrantAppearance(player, milestone->grant_appearance_id(), nowMs / 1000u);
            if (grantErr != kSuccess) {
                return grantErr;
            }
        }
        journal->Swap(&nextJournal);
        ledger->Swap(&nextLedger);
        return kSuccess;
    };

    if (!gainedParams.empty()) {
        std::vector<GuidVector> written;
        const uint32_t exchangeErr = BagService::ExchangeInventoryExact(
            player, {}, gainedParams, {}, clientRequestId,
            "{\"operation\":\"trial_journal_milestone\"}", written,
            [&](const std::vector<GuidVector>&) -> uint32_t { return commitState(); });
        if (exchangeErr != kSuccess) {
            finish(exchangeErr == kInvalidParameter ? exchangeErr : kTrialJournalBagFull);
            return;
        }
    } else {
        if (const uint32_t err = commitState(); err != kSuccess) {
            finish(err);
            return;
        }
    }

    const auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    LOG_INFO << "[PlayerTrialJournal] Milestone claimed: player_id=" << (guid != nullptr ? guid->value : 0)
             << " milestone_id=" << milestone->id()
             << " bound_sand=" << milestone->bound_sand_count()
             << " bound_voucher=" << milestone->bound_artifact_voucher_count()
             << " appearance=" << milestone->grant_appearance_id();

    (void)gTrialJournalPersistFn(player);
    response.set_granted_sand_count(milestone->bound_sand_count());
    response.set_granted_voucher_count(milestone->bound_artifact_voucher_count());
    if (!milestone->grant_appearance_id().empty()) {
        response.set_granted_appearance_id(milestone->grant_appearance_id());
    }
    finish(kSuccess);
}

void PlayerTrialJournalSystem::RedeemArtifactVoucher(entt::entity player,
                                                     uint64_t nowMs,
                                                     const RedeemBoundArtifactVoucherRequest& request,
                                                     RedeemBoundArtifactVoucherResponse& response) {
    response.Clear();
    const auto finish = [&](uint32_t err) {
        if (err != kSuccess) {
            response.mutable_error_message()->set_id(err);
        }
        if (const uint32_t snapErr = FillSnapshot(player, nowMs, *response.mutable_snapshot()); snapErr != kSuccess) {
            response.clear_snapshot();
            if (err == kSuccess) {
                response.mutable_error_message()->set_id(snapErr);
            }
        }
    };

    if (!tlsEcs.actorRegistry.valid(player)) {
        response.mutable_error_message()->set_id(kEntityIsNull);
        return;
    }
    const uint64_t clientRequestId = request.client_request_id();
    if (clientRequestId == 0 || request.artifact_item_config_id() == 0) {
        finish(kInvalidParameter);
        return;
    }

    // 1. 幂等查账排在一切状态校验之前。
    std::string fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    switch (client_request_dedup::Check(player, clientRequestId, ClientRequestKind::kRedeemBoundArtifactVoucher,
                                        fingerprint, first)) {
    case ClientRequestVerdict::kReplay:
        response.set_replayed(true);
        response.set_created_item_id(first.redeemedArtifactItemId);
        response.set_artifact_item_config_id(first.redeemedArtifactConfigId);
        finish(kSuccess);
        return;
    case ClientRequestVerdict::kMismatch:
        finish(kInvalidParameter);
        return;
    case ClientRequestVerdict::kUnavailable:
        finish(kServiceUnavailable);
        return;
    default:
        break;
    }

    auto* journal = tlsEcs.actorRegistry.try_get<PlayerTrialJournalComp>(player);
    auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
    auto* ledger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
    if (journal == nullptr || bags == nullptr || ledger == nullptr ||
        tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player)) {
        finish(kTrialJournalUnavailable);
        return;
    }
    if (const uint32_t writableErr = CheckWritable(player); writableErr != kSuccess) {
        finish(writableErr);
        return;
    }

    const auto* itemRow = ItemTableManager::Instance().FindByIdSilent(request.artifact_item_config_id()).first;
    if (itemRow == nullptr || !IsValidRedeemableArtifactItem(*itemRow)) {
        finish(kTrialJournalArtifactInvalid);
        return;
    }

    const ItemCountMap consumed{{trialjournal::kBoundArtifactVoucherItemId, 1}};
    const ItemCountMap gainedCounts{{itemRow->id(), 1}};
    const auto verdict = BagService::CheckRemovable(player, bags->bags[kInventory], consumed);
    if (verdict == BagService::RemovalVerdict::kInsufficient) {
        finish(kTrialJournalVoucherNotEnough);
        return;
    }
    if (verdict != BagService::RemovalVerdict::kEnough) {
        finish(kTrialJournalUnavailable);
        return;
    }
    if (BagService::CheckInventoryExchangeSpace(player, consumed, gainedCounts) != kSuccess) {
        finish(kTrialJournalBagFull);
        return;
    }

    // 产出仅自用法宝实例（bound = true，允许与已有同名法宝并存，不可交易/拆解）。
    InitItemParam artifactParam;
    artifactParam.itemPBComp.set_item_id(kInvalidGuid);
    artifactParam.itemPBComp.set_config_id(itemRow->id());
    artifactParam.itemPBComp.set_size(1);
    artifactParam.itemPBComp.mutable_equip();
    artifactParam.itemPBComp.set_bound(true);

    std::vector<InitItemParam> gainedParams;
    gainedParams.push_back(std::move(artifactParam));

    const std::string ledgerBefore = ledger->SerializeAsString();
    ClientRequestResult applied;
    applied.redeemedArtifactConfigId = itemRow->id();

    std::vector<GuidVector> written;
    const uint32_t exchangeErr = BagService::ExchangeInventoryExact(
        player, consumed, gainedParams, {}, clientRequestId,
        "{\"operation\":\"trial_artifact_redeem\"}", written,
        [&](const std::vector<GuidVector>& created) -> uint32_t {
            if (ledger->SerializeAsString() != ledgerBefore || CheckWritable(player) != kSuccess) {
                return kTrialJournalUnavailable;
            }
            if (created.size() != 1 || created.front().size() != 1 || created.front().front() == 0 ||
                created.front().front() == kInvalidGuid) {
                return kTrialJournalUnavailable;
            }
            applied.redeemedArtifactItemId = created.front().front();
            ClientRequestLedgerComp nextLedger = *ledger;
            if (!client_request_ledger::Remember(nextLedger, clientRequestId,
                                                 ClientRequestKind::kRedeemBoundArtifactVoucher,
                                                 fingerprint, applied) ||
                !client_request_ledger::Validate(nextLedger).empty()) {
                return kTrialJournalUnavailable;
            }
            BumpRevision(*journal);
            ledger->Swap(&nextLedger);
            return kSuccess;
        });

    if (exchangeErr != kSuccess) {
        finish(exchangeErr == kTrialJournalUnavailable ? exchangeErr : kTrialJournalBagFull);
        return;
    }

    const auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    LOG_INFO << "[PlayerTrialJournal] Bound artifact redeemed: player_id=" << (guid != nullptr ? guid->value : 0)
             << " config_id=" << itemRow->id() << " artifact_id=" << itemRow->artifact_id()
             << " item_id=" << applied.redeemedArtifactItemId;

    (void)gTrialJournalPersistFn(player);
    response.set_created_item_id(applied.redeemedArtifactItemId);
    response.set_artifact_item_config_id(applied.redeemedArtifactConfigId);
    finish(kSuccess);
}

void PlayerTrialJournalSystem::SetPersistForTest(PersistFn fn) {
    gTrialJournalPersistFn = fn == nullptr ? &PlayerLifecycleSystem::SavePlayerForAssetCommit : fn;
}
