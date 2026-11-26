#include "skillvfxcosmetic_table_fk.h"
#include "skillvfxcosmetic_table.h"
#include "skill_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for SkillVfxCosmeticTable
// ---------------------------------------------------------------------------

/// Resolve SkillVfxCosmetic.skill_table_id -> Skill row.
const SkillTable* GetSkillVfxCosmeticSkillTableIdRow(const SkillVfxCosmeticTable& row) {
    auto [ptr, _] = SkillTableManager::Instance().FindByIdSilent(row.skill_table_id());
    return ptr;
}

/// Resolve SkillVfxCosmetic.skill_table_id -> Skill row (by SkillVfxCosmetic id).
const SkillTable* GetSkillVfxCosmeticSkillTableIdRow(uint32_t tableId) {
    auto [row, _] = SkillVfxCosmeticTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetSkillVfxCosmeticSkillTableIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all SkillVfxCosmetic rows whose skill_table_id == key.
const std::vector<const SkillVfxCosmeticTable*>& FindSkillVfxCosmeticRowsBySkillTableId(uint32_t key) {
    return SkillVfxCosmeticTableManager::Instance().GetBySkillTableId(key);
}
