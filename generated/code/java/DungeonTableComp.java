
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

    public record Allow_pet_capture(int value) {
        public static Allow_pet_capture from(DungeonTable row) {
            return new Allow_pet_capture(row.getAllowPetCapture());
        }
    }

    public record Boss_trial_kind(int value) {
        public static Boss_trial_kind from(DungeonTable row) {
            return new Boss_trial_kind(row.getBossTrialKind());
        }
    }

    public record Difficulty(int value) {
        public static Difficulty from(DungeonTable row) {
            return new Difficulty(row.getDifficulty());
        }
    }

    public record Round_limit(int value) {
        public static Round_limit from(DungeonTable row) {
            return new Round_limit(row.getRoundLimit());
        }
    }

    public record Monster(List<Integer> values) {
        public static Monster from(DungeonTable row) {
            return new Monster(row.getMonsterList());
        }
    }

}