
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for PetTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class PetTableComp {

    private PetTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(PetTable row) {
            return new Id(row.getId());
        }
    }

    public record Name(String value) {
        public static Name from(PetTable row) {
            return new Name(row.getName());
        }
    }

    public record Desc(String value) {
        public static Desc from(PetTable row) {
            return new Desc(row.getDesc());
        }
    }

    public record Model_id(int value) {
        public static Model_id from(PetTable row) {
            return new Model_id(row.getModelId());
        }
    }

    public record Quality(int value) {
        public static Quality from(PetTable row) {
            return new Quality(row.getQuality());
        }
    }

    public record Unlock_level(int value) {
        public static Unlock_level from(PetTable row) {
            return new Unlock_level(row.getUnlockLevel());
        }
    }

    public record Level_cap(int value) {
        public static Level_cap from(PetTable row) {
            return new Level_cap(row.getLevelCap());
        }
    }

    public record Init_health(long value) {
        public static Init_health from(PetTable row) {
            return new Init_health(row.getInitHealth());
        }
    }

    public record Init_mana(long value) {
        public static Init_mana from(PetTable row) {
            return new Init_mana(row.getInitMana());
        }
    }

    public record Init_speed(long value) {
        public static Init_speed from(PetTable row) {
            return new Init_speed(row.getInitSpeed());
        }
    }

    public record Aptitude_min(List<Integer> values) {
        public static Aptitude_min from(PetTable row) {
            return new Aptitude_min(row.getAptitudeMinList());
        }
    }

    public record Aptitude_max(List<Integer> values) {
        public static Aptitude_max from(PetTable row) {
            return new Aptitude_max(row.getAptitudeMaxList());
        }
    }

    public record Skill(List<Integer> values) {
        public static Skill from(PetTable row) {
            return new Skill(row.getSkillList());
        }
    }

}