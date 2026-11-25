#include "festivalbeastactivity_table_fk.h"
#include "festivalbeastactivity_table.h"
#include "dungeon_table.h"
#include "pet_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for FestivalBeastActivityTable
// ---------------------------------------------------------------------------

/// Resolve FestivalBeastActivity.battle_config_id -> Dungeon row.
const DungeonTable* GetFestivalBeastActivityBattleConfigIdRow(const FestivalBeastActivityTable& row) {
    auto [ptr, _] = DungeonTableManager::Instance().FindByIdSilent(row.battle_config_id());
    return ptr;
}

/// Resolve FestivalBeastActivity.battle_config_id -> Dungeon row (by FestivalBeastActivity id).
const DungeonTable* GetFestivalBeastActivityBattleConfigIdRow(uint32_t tableId) {
    auto [row, _] = FestivalBeastActivityTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetFestivalBeastActivityBattleConfigIdRow(*row);
}

/// Resolve FestivalBeastActivity.reward_pet_table_id -> Pet row.
const PetTable* GetFestivalBeastActivityRewardPetTableIdRow(const FestivalBeastActivityTable& row) {
    auto [ptr, _] = PetTableManager::Instance().FindByIdSilent(row.reward_pet_table_id());
    return ptr;
}

/// Resolve FestivalBeastActivity.reward_pet_table_id -> Pet row (by FestivalBeastActivity id).
const PetTable* GetFestivalBeastActivityRewardPetTableIdRow(uint32_t tableId) {
    auto [row, _] = FestivalBeastActivityTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetFestivalBeastActivityRewardPetTableIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all FestivalBeastActivity rows whose battle_config_id == key.
const std::vector<const FestivalBeastActivityTable*>& FindFestivalBeastActivityRowsByBattleConfigId(uint32_t key) {
    return FestivalBeastActivityTableManager::Instance().GetByBattleConfigId(key);
}

/// Reverse FK: find all FestivalBeastActivity rows whose reward_pet_table_id == key.
const std::vector<const FestivalBeastActivityTable*>& FindFestivalBeastActivityRowsByRewardPetTableId(uint32_t key) {
    return FestivalBeastActivityTableManager::Instance().GetByRewardPetTableId(key);
}
