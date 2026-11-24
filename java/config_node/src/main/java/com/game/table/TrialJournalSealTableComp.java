
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for TrialJournalSealTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class TrialJournalSealTableComp {

    private TrialJournalSealTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(TrialJournalSealTable row) {
            return new Id(row.getId());
        }
    }

    public record Entry_id(int value) {
        public static Entry_id from(TrialJournalSealTable row) {
            return new Entry_id(row.getEntryId());
        }
    }

    public record Seal_slot_kind(int value) {
        public static Seal_slot_kind from(TrialJournalSealTable row) {
            return new Seal_slot_kind(row.getSealSlotKind());
        }
    }

    public record Condition_type(int value) {
        public static Condition_type from(TrialJournalSealTable row) {
            return new Condition_type(row.getConditionType());
        }
    }

    public record Condition_param(int value) {
        public static Condition_param from(TrialJournalSealTable row) {
            return new Condition_param(row.getConditionParam());
        }
    }

    public record Enabled(boolean value) {
        public static Enabled from(TrialJournalSealTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Title(String value) {
        public static Title from(TrialJournalSealTable row) {
            return new Title(row.getTitle());
        }
    }

    public record Desc(String value) {
        public static Desc from(TrialJournalSealTable row) {
            return new Desc(row.getDesc());
        }
    }

}