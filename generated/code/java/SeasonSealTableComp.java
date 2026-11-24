
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for SeasonSealTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class SeasonSealTableComp {

    private SeasonSealTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(SeasonSealTable row) {
            return new Id(row.getId());
        }
    }

    public record Season_id(int value) {
        public static Season_id from(SeasonSealTable row) {
            return new Season_id(row.getSeasonId());
        }
    }

    public record Name(String value) {
        public static Name from(SeasonSealTable row) {
            return new Name(row.getName());
        }
    }

    public record Description(String value) {
        public static Description from(SeasonSealTable row) {
            return new Description(row.getDescription());
        }
    }

    public record Icon_key(String value) {
        public static Icon_key from(SeasonSealTable row) {
            return new Icon_key(row.getIconKey());
        }
    }

    public record Effect_id(int value) {
        public static Effect_id from(SeasonSealTable row) {
            return new Effect_id(row.getEffectId());
        }
    }

    public record Bonus_bps(int value) {
        public static Bonus_bps from(SeasonSealTable row) {
            return new Bonus_bps(row.getBonusBps());
        }
    }

    public record Cap_bps(int value) {
        public static Cap_bps from(SeasonSealTable row) {
            return new Cap_bps(row.getCapBps());
        }
    }

    public record Per_round_limit(int value) {
        public static Per_round_limit from(SeasonSealTable row) {
            return new Per_round_limit(row.getPerRoundLimit());
        }
    }

    public record Battle_limit(int value) {
        public static Battle_limit from(SeasonSealTable row) {
            return new Battle_limit(row.getBattleLimit());
        }
    }

    public record Active_rounds(int value) {
        public static Active_rounds from(SeasonSealTable row) {
            return new Active_rounds(row.getActiveRounds());
        }
    }

    public record Health_threshold_bps(int value) {
        public static Health_threshold_bps from(SeasonSealTable row) {
            return new Health_threshold_bps(row.getHealthThresholdBps());
        }
    }

}