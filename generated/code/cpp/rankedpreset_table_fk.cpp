#include "rankedpreset_table_fk.h"
#include "rankedpreset_table.h"
#include "pet_table.h"
#include "petentry_table.h"
#include "skill_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for RankedPresetTable
// ---------------------------------------------------------------------------

/// Resolve RankedPreset.pet_table_id -> Pet row.
const PetTable* GetRankedPresetPetTableIdRow(const RankedPresetTable& row) {
    auto [ptr, _] = PetTableManager::Instance().FindByIdSilent(row.pet_table_id());
    return ptr;
}

/// Resolve RankedPreset.pet_table_id -> Pet row (by RankedPreset id).
const PetTable* GetRankedPresetPetTableIdRow(uint32_t tableId) {
    auto [row, _] = RankedPresetTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetRankedPresetPetTableIdRow(*row);
}

/// Resolve RankedPreset.skill_ids[] -> Skill rows.
std::vector<const SkillTable*> GetRankedPresetSkillIdsRows(const RankedPresetTable& row) {
    std::vector<const SkillTable*> result;
    for (auto id : row.skill_ids()) {
        auto [ptr, _] = SkillTableManager::Instance().FindByIdSilent(id);
        if (ptr) result.push_back(ptr);
    }
    return result;
}

/// Resolve RankedPreset.skill_ids[] -> Skill rows (by RankedPreset id).
std::vector<const SkillTable*> GetRankedPresetSkillIdsRows(uint32_t tableId) {
    auto [row, _] = RankedPresetTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return {};
    return GetRankedPresetSkillIdsRows(*row);
}

/// Resolve RankedPreset.pet_entry_ids[] -> PetEntry rows.
std::vector<const PetEntryTable*> GetRankedPresetPetEntryIdsRows(const RankedPresetTable& row) {
    std::vector<const PetEntryTable*> result;
    for (auto id : row.pet_entry_ids()) {
        auto [ptr, _] = PetEntryTableManager::Instance().FindByIdSilent(id);
        if (ptr) result.push_back(ptr);
    }
    return result;
}

/// Resolve RankedPreset.pet_entry_ids[] -> PetEntry rows (by RankedPreset id).
std::vector<const PetEntryTable*> GetRankedPresetPetEntryIdsRows(uint32_t tableId) {
    auto [row, _] = RankedPresetTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return {};
    return GetRankedPresetPetEntryIdsRows(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all RankedPreset rows whose pet_table_id == key.
const std::vector<const RankedPresetTable*>& FindRankedPresetRowsByPetTableId(uint32_t key) {
    return RankedPresetTableManager::Instance().GetByPetTableId(key);
}
