
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for ActivityVitalityRuleTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class ActivityVitalityRuleTableComp {

    private ActivityVitalityRuleTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(ActivityVitalityRuleTable row) {
            return new Id(row.getId());
        }
    }

    public record Enabled(boolean value) {
        public static Enabled from(ActivityVitalityRuleTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Configuration_revision(long value) {
        public static Configuration_revision from(ActivityVitalityRuleTable row) {
            return new Configuration_revision(row.getConfigurationRevision());
        }
    }

    public record First_period_starts_at_ms(long value) {
        public static First_period_starts_at_ms from(ActivityVitalityRuleTable row) {
            return new First_period_starts_at_ms(row.getFirstPeriodStartsAtMs());
        }
    }

    public record Period_duration_ms(long value) {
        public static Period_duration_ms from(ActivityVitalityRuleTable row) {
            return new Period_duration_ms(row.getPeriodDurationMs());
        }
    }

    public record Points_limit(int value) {
        public static Points_limit from(ActivityVitalityRuleTable row) {
            return new Points_limit(row.getPointsLimit());
        }
    }

}