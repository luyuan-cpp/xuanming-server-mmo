package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for TrialJournalSealTable
// ---------------------------------------------------------------------------

// GetTrialJournalSealEntryIdRow resolves TrialJournalSeal.entry_id -> TrialJournalEntry row.
func GetTrialJournalSealEntryIdRow(row *pb.TrialJournalSealTable) (*pb.TrialJournalEntryTable, bool) {
    return TrialJournalEntryTableManagerInstance.FindById(row.EntryId)
}

// GetTrialJournalSealEntryIdRowById resolves TrialJournalSeal.entry_id -> TrialJournalEntry row (by TrialJournalSeal id).
func GetTrialJournalSealEntryIdRowById(tableId uint32) (*pb.TrialJournalEntryTable, bool) {
    row, ok := TrialJournalSealTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetTrialJournalSealEntryIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindTrialJournalSealRowsByEntryId returns all TrialJournalSeal rows whose entry_id == key.
func FindTrialJournalSealRowsByEntryId(key uint32) []*pb.TrialJournalSealTable {
    return TrialJournalSealTableManagerInstance.GetByEntryId(key)
}
