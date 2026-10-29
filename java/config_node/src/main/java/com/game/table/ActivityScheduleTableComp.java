
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

    public record Definition_key(String value) {
        public static Definition_key from(ActivityScheduleTable row) {
            return new Definition_key(row.getDefinitionKey());
        }
    }

    public record Configuration_revision(long value) {
        public static Configuration_revision from(ActivityScheduleTable row) {
            return new Configuration_revision(row.getConfigurationRevision());
        }
    }

    public record Category(int value) {
        public static Category from(ActivityScheduleTable row) {
            return new Category(row.getCategory());
        }
    }

    public record Name(String value) {
        public static Name from(ActivityScheduleTable row) {
            return new Name(row.getName());
        }
    }

    public record Description(String value) {
        public static Description from(ActivityScheduleTable row) {
            return new Description(row.getDescription());
        }
    }

    public record Icon_key(String value) {
        public static Icon_key from(ActivityScheduleTable row) {
            return new Icon_key(row.getIconKey());
        }
    }

    public record Vitality_points(int value) {
        public static Vitality_points from(ActivityScheduleTable row) {
            return new Vitality_points(row.getVitalityPoints());
        }
    }

}