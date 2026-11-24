#include "trialjournalseal_table_fk.h"
#include "trialjournalseal_table.h"
#include "trialjournalentry_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for TrialJournalSealTable
// ---------------------------------------------------------------------------

/// Resolve TrialJournalSeal.entry_id -> TrialJournalEntry row.
const TrialJournalEntryTable* GetTrialJournalSealEntryIdRow(const TrialJournalSealTable& row) {
    auto [ptr, _] = TrialJournalEntryTableManager::Instance().FindByIdSilent(row.entry_id());
    return ptr;
}

/// Resolve TrialJournalSeal.entry_id -> TrialJournalEntry row (by TrialJournalSeal id).
const TrialJournalEntryTable* GetTrialJournalSealEntryIdRow(uint32_t tableId) {
    auto [row, _] = TrialJournalSealTableManager::Instance().FindByIdSilent(tableId);
    if (!row) return nullptr;
    return GetTrialJournalSealEntryIdRow(*row);
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all TrialJournalSeal rows whose entry_id == key.
const std::vector<const TrialJournalSealTable*>& FindTrialJournalSealRowsByEntryId(uint32_t key) {
    return TrialJournalSealTableManager::Instance().GetByEntryId(key);
}
