
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for EquipArtifactTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class EquipArtifactTableComp {

    private EquipArtifactTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(EquipArtifactTable row) {
            return new Id(row.getId());
        }
    }

    public record Bonus_bps(int value) {
        public static Bonus_bps from(EquipArtifactTable row) {
            return new Bonus_bps(row.getBonusBps());
        }
    }

    public record Cap_bps(int value) {
        public static Cap_bps from(EquipArtifactTable row) {
            return new Cap_bps(row.getCapBps());
        }
    }

    public record Per_round_limit(int value) {
        public static Per_round_limit from(EquipArtifactTable row) {
            return new Per_round_limit(row.getPerRoundLimit());
        }
    }

    public record Battle_limit(int value) {
        public static Battle_limit from(EquipArtifactTable row) {
            return new Battle_limit(row.getBattleLimit());
        }
    }

    public record Active_rounds(int value) {
        public static Active_rounds from(EquipArtifactTable row) {
            return new Active_rounds(row.getActiveRounds());
        }
    }

    public record Health_threshold_bps(int value) {
        public static Health_threshold_bps from(EquipArtifactTable row) {
            return new Health_threshold_bps(row.getHealthThresholdBps());
        }
    }

    public record Name(String value) {
        public static Name from(EquipArtifactTable row) {
            return new Name(row.getName());
        }
    }

    public record Description(String value) {
        public static Description from(EquipArtifactTable row) {
            return new Description(row.getDescription());
        }
    }

}