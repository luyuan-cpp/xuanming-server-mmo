#include "monster_table_fk.h"
#include "monster_table.h"
#include "pet_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for MonsterTable
// ---------------------------------------------------------------------------

/// Resolve Monster.capture_pet_table_id -> Pet row.
const PetTable* GetMonsterCapturePetTableIdRow(const MonsterTable& row) {
    auto [ptr, _] = PetTableManager::Instance().FindByIdSilent(row.capture_pet_table_id());
    return ptr;
}

/// Resolve Monster.capture_pet_table_id -> Pet row (by Monster id).
const PetTable* GetMonsterCapturePetTableIdRow(uint32_t tableId) {
    auto [row, _] = MonsterTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetMonsterCapturePetTableIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all Monster rows whose capture_pet_table_id == key.
const std::vector<const MonsterTable*>& FindMonsterRowsByCapturePetTableId(uint32_t key) {
    return MonsterTableManager::Instance().GetByCapturePetTableId(key);
}
