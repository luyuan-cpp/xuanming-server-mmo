
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for PlayModeTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class PlayModeTableComp {

    private PlayModeTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(PlayModeTable row) {
            return new Id(row.getId());
        }
    }

    public record Match_mode(int value) {
        public static Match_mode from(PlayModeTable row) {
            return new Match_mode(row.getMatchMode());
        }
    }

    public record Battle_config_id(int value) {
        public static Battle_config_id from(PlayModeTable row) {
            return new Battle_config_id(row.getBattleConfigId());
        }
    }

    public record Enabled(boolean value) {
        public static Enabled from(PlayModeTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Team_size_max(int value) {
        public static Team_size_max from(PlayModeTable row) {
            return new Team_size_max(row.getTeamSizeMax());
        }
    }

}