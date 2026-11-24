package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for TrialJournalSealTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class TrialJournalSealTableForeignKeys {
    private TrialJournalSealTableForeignKeys() {}

    /** Resolve TrialJournalSeal.entry_id -> TrialJournalEntry row. */
    public static TrialJournalEntryTable getEntryIdRow(TrialJournalSealTable row) {
        return TrialJournalEntryTableManager.getInstance().findById(row.getEntryId());
    }

    /** Resolve TrialJournalSeal.entry_id -> TrialJournalEntry row (by TrialJournalSeal id). */
    public static TrialJournalEntryTable getEntryIdRow(int tableId) {
        TrialJournalSealTable row = TrialJournalSealTableManager.getInstance().findById(tableId);
        if (row == null) { return null; }
        return getEntryIdRow(row);
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all TrialJournalSeal rows whose entry_id == key. */
    public static List<TrialJournalSealTable> findRowsByEntryId(int key) {
        return TrialJournalSealTableManager.getInstance().getByEntryId(key);
    }

}
