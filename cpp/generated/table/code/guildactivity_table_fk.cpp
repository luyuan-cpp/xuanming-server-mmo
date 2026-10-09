#include "guildactivity_table_fk.h"
#include "guildactivity_table.h"
#include "dungeon_table.h"
#include "reward_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for GuildActivityTable
// ---------------------------------------------------------------------------

/// Resolve GuildActivity.reward_id -> Reward row.
const RewardTable* GetGuildActivityRewardIdRow(const GuildActivityTable& row) {
    auto [ptr, _] = RewardTableManager::Instance().FindByIdSilent(row.reward_id());
    return ptr;
}

/// Resolve GuildActivity.reward_id -> Reward row (by GuildActivity id).
const RewardTable* GetGuildActivityRewardIdRow(uint32_t tableId) {
    auto [row, _] = GuildActivityTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetGuildActivityRewardIdRow(*row);
}

/// Resolve GuildActivity.dungeon_id -> Dungeon row.
const DungeonTable* GetGuildActivityDungeonIdRow(const GuildActivityTable& row) {
    auto [ptr, _] = DungeonTableManager::Instance().FindByIdSilent(row.dungeon_id());
    return ptr;
}

/// Resolve GuildActivity.dungeon_id -> Dungeon row (by GuildActivity id).
const DungeonTable* GetGuildActivityDungeonIdRow(uint32_t tableId) {
    auto [row, _] = GuildActivityTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetGuildActivityDungeonIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all GuildActivity rows whose reward_id == key.
const std::vector<const GuildActivityTable*>& FindGuildActivityRowsByRewardId(uint32_t key) {
    return GuildActivityTableManager::Instance().GetByRewardId(key);
}

/// Reverse FK: find all GuildActivity rows whose dungeon_id == key.
const std::vector<const GuildActivityTable*>& FindGuildActivityRowsByDungeonId(uint32_t key) {
    return GuildActivityTableManager::Instance().GetByDungeonId(key);
}
