
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for DungeonTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class DungeonTableComp {

    private DungeonTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(DungeonTable row) {
            return new Id(row.getId());
        }
    }

    public record Scene_id(int value) {
        public static Scene_id from(DungeonTable row) {
            return new Scene_id(row.getSceneId());
        }
    }

    public record Max_team_size(int value) {
        public static Max_team_size from(DungeonTable row) {
            return new Max_team_size(row.getMaxTeamSize());
        }
    }

    public record Time_limit(int value) {
        public static Time_limit from(DungeonTable row) {
            return new Time_limit(row.getTimeLimit());
        }
    }

    public record Monster(List<Integer> values) {
        public static Monster from(DungeonTable row) {
            return new Monster(row.getMonsterList());
        }
    }

}