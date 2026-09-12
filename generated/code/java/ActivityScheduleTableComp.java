
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for ActivityScheduleTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class ActivityScheduleTableComp {

    private ActivityScheduleTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(ActivityScheduleTable row) {
            return new Id(row.getId());
        }
    }

    public record Enabled(boolean value) {
        public static Enabled from(ActivityScheduleTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Baseline_start_at_ms(long value) {
        public static Baseline_start_at_ms from(ActivityScheduleTable row) {
            return new Baseline_start_at_ms(row.getBaselineStartAtMs());
        }
    }

    public record Baseline_end_at_ms(long value) {
        public static Baseline_end_at_ms from(ActivityScheduleTable row) {
            return new Baseline_end_at_ms(row.getBaselineEndAtMs());
        }
    }

}