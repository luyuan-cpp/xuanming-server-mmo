
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for BattleProgressionRuleTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class BattleProgressionRuleTableComp {

    private BattleProgressionRuleTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(BattleProgressionRuleTable row) {
            return new Id(row.getId());
        }
    }

    public record Weekly_silver_soft_cap(long value) {
        public static Weekly_silver_soft_cap from(BattleProgressionRuleTable row) {
            return new Weekly_silver_soft_cap(row.getWeeklySilverSoftCap());
        }
    }

    public record Diminished_rate_permyriad(int value) {
        public static Diminished_rate_permyriad from(BattleProgressionRuleTable row) {
            return new Diminished_rate_permyriad(row.getDiminishedRatePermyriad());
        }
    }

    public record Weekly_silver_hard_cap(long value) {
        public static Weekly_silver_hard_cap from(BattleProgressionRuleTable row) {
            return new Weekly_silver_hard_cap(row.getWeeklySilverHardCap());
        }
    }

}