
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for TrialJournalEntryTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class TrialJournalEntryTableComp {

    private TrialJournalEntryTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(TrialJournalEntryTable row) {
            return new Id(row.getId());
        }
    }

    public record Chapter_id(int value) {
        public static Chapter_id from(TrialJournalEntryTable row) {
            return new Chapter_id(row.getChapterId());
        }
    }

    public record Season_id(int value) {
        public static Season_id from(TrialJournalEntryTable row) {
            return new Season_id(row.getSeasonId());
        }
    }

    public record Source_kind(int value) {
        public static Source_kind from(TrialJournalEntryTable row) {
            return new Source_kind(row.getSourceKind());
        }
    }

    public record Difficulty(int value) {
        public static Difficulty from(TrialJournalEntryTable row) {
            return new Difficulty(row.getDifficulty());
        }
    }

    public record Variant_key(String value) {
        public static Variant_key from(TrialJournalEntryTable row) {
            return new Variant_key(row.getVariantKey());
        }
    }

    public record Rule_version(int value) {
        public static Rule_version from(TrialJournalEntryTable row) {
            return new Rule_version(row.getRuleVersion());
        }
    }

    public record Rule_version_name(String value) {
        public static Rule_version_name from(TrialJournalEntryTable row) {
            return new Rule_version_name(row.getRuleVersionName());
        }
    }

    public record Enabled(boolean value) {
        public static Enabled from(TrialJournalEntryTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Sort_order(int value) {
        public static Sort_order from(TrialJournalEntryTable row) {
            return new Sort_order(row.getSortOrder());
        }
    }

    public record Display_name(String value) {
        public static Display_name from(TrialJournalEntryTable row) {
            return new Display_name(row.getDisplayName());
        }
    }

    public record Chapter_name(String value) {
        public static Chapter_name from(TrialJournalEntryTable row) {
            return new Chapter_name(row.getChapterName());
        }
    }

    public record Icon_key(String value) {
        public static Icon_key from(TrialJournalEntryTable row) {
            return new Icon_key(row.getIconKey());
        }
    }

    public record Unlock_mode(int value) {
        public static Unlock_mode from(TrialJournalEntryTable row) {
            return new Unlock_mode(row.getUnlockMode());
        }
    }

    public record Jump_target(String value) {
        public static Jump_target from(TrialJournalEntryTable row) {
            return new Jump_target(row.getJumpTarget());
        }
    }

    public record Desc(String value) {
        public static Desc from(TrialJournalEntryTable row) {
            return new Desc(row.getDesc());
        }
    }

}