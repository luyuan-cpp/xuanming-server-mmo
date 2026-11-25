#include "guildwarrule_table_fk.h"
#include "guildwarrule_table.h"
#include "dungeon_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for GuildWarRuleTable
// ---------------------------------------------------------------------------

/// Resolve GuildWarRule.battle_config_id -> Dungeon row.
const DungeonTable* GetGuildWarRuleBattleConfigIdRow(const GuildWarRuleTable& row) {
    auto [ptr, _] = DungeonTableManager::Instance().FindByIdSilent(row.battle_config_id());
    return ptr;
}

/// Resolve GuildWarRule.battle_config_id -> Dungeon row (by GuildWarRule id).
const DungeonTable* GetGuildWarRuleBattleConfigIdRow(uint32_t tableId) {
    auto [row, _] = GuildWarRuleTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetGuildWarRuleBattleConfigIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all GuildWarRule rows whose battle_config_id == key.
const std::vector<const GuildWarRuleTable*>& FindGuildWarRuleRowsByBattleConfigId(uint32_t key) {
    return GuildWarRuleTableManager::Instance().GetByBattleConfigId(key);
}
