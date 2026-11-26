
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for BattleProgressionRewardTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class BattleProgressionRewardTableComp {

    private BattleProgressionRewardTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(BattleProgressionRewardTable row) {
            return new Id(row.getId());
        }
    }

    public record Match_mode(int value) {
        public static Match_mode from(BattleProgressionRewardTable row) {
            return new Match_mode(row.getMatchMode());
        }
    }

    public record Difficulty(int value) {
        public static Difficulty from(BattleProgressionRewardTable row) {
            return new Difficulty(row.getDifficulty());
        }
    }

    public record Outcome_tier(int value) {
        public static Outcome_tier from(BattleProgressionRewardTable row) {
            return new Outcome_tier(row.getOutcomeTier());
        }
    }

    public record Exp_gain(long value) {
        public static Exp_gain from(BattleProgressionRewardTable row) {
            return new Exp_gain(row.getExpGain());
        }
    }

    public record Silver_gain(long value) {
        public static Silver_gain from(BattleProgressionRewardTable row) {
            return new Silver_gain(row.getSilverGain());
        }
    }

}