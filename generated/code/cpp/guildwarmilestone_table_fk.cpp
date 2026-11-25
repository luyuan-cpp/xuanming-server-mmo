#include "guildwarmilestone_table_fk.h"
#include "guildwarmilestone_table.h"
#include "item_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for GuildWarMilestoneTable
// ---------------------------------------------------------------------------

/// Resolve GuildWarMilestone.reward_item1_id -> Item row.
const ItemTable* GetGuildWarMilestoneRewardItem1IdRow(const GuildWarMilestoneTable& row) {
    auto [ptr, _] = ItemTableManager::Instance().FindByIdSilent(row.reward_item1_id());
    return ptr;
}

/// Resolve GuildWarMilestone.reward_item1_id -> Item row (by GuildWarMilestone id).
const ItemTable* GetGuildWarMilestoneRewardItem1IdRow(uint32_t tableId) {
    auto [row, _] = GuildWarMilestoneTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetGuildWarMilestoneRewardItem1IdRow(*row);
}

/// Resolve GuildWarMilestone.reward_item2_id -> Item row.
const ItemTable* GetGuildWarMilestoneRewardItem2IdRow(const GuildWarMilestoneTable& row) {
    auto [ptr, _] = ItemTableManager::Instance().FindByIdSilent(row.reward_item2_id());
    return ptr;
}

/// Resolve GuildWarMilestone.reward_item2_id -> Item row (by GuildWarMilestone id).
const ItemTable* GetGuildWarMilestoneRewardItem2IdRow(uint32_t tableId) {
    auto [row, _] = GuildWarMilestoneTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetGuildWarMilestoneRewardItem2IdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all GuildWarMilestone rows whose reward_item1_id == key.
const std::vector<const GuildWarMilestoneTable*>& FindGuildWarMilestoneRowsByRewardItem1Id(uint32_t key) {
    return GuildWarMilestoneTableManager::Instance().GetByRewardItem1Id(key);
}

/// Reverse FK: find all GuildWarMilestone rows whose reward_item2_id == key.
const std::vector<const GuildWarMilestoneTable*>& FindGuildWarMilestoneRowsByRewardItem2Id(uint32_t key) {
    return GuildWarMilestoneTableManager::Instance().GetByRewardItem2Id(key);
}
