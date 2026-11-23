
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for RankedPresetTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class RankedPresetTableComp {

    private RankedPresetTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(RankedPresetTable row) {
            return new Id(row.getId());
        }
    }

    public record Role_kind(int value) {
        public static Role_kind from(RankedPresetTable row) {
            return new Role_kind(row.getRoleKind());
        }
    }

    public record Code(String value) {
        public static Code from(RankedPresetTable row) {
            return new Code(row.getCode());
        }
    }

    public record Name(String value) {
        public static Name from(RankedPresetTable row) {
            return new Name(row.getName());
        }
    }

    public record Role_tag(String value) {
        public static Role_tag from(RankedPresetTable row) {
            return new Role_tag(row.getRoleTag());
        }
    }

    public record Pet_table_id(int value) {
        public static Pet_table_id from(RankedPresetTable row) {
            return new Pet_table_id(row.getPetTableId());
        }
    }

    public record Weight_con(int value) {
        public static Weight_con from(RankedPresetTable row) {
            return new Weight_con(row.getWeightCon());
        }
    }

    public record Weight_spi(int value) {
        public static Weight_spi from(RankedPresetTable row) {
            return new Weight_spi(row.getWeightSpi());
        }
    }

    public record Weight_str(int value) {
        public static Weight_str from(RankedPresetTable row) {
            return new Weight_str(row.getWeightStr());
        }
    }

    public record Weight_agi(int value) {
        public static Weight_agi from(RankedPresetTable row) {
            return new Weight_agi(row.getWeightAgi());
        }
    }

    public record Sort(int value) {
        public static Sort from(RankedPresetTable row) {
            return new Sort(row.getSort());
        }
    }

    public record Desc(String value) {
        public static Desc from(RankedPresetTable row) {
            return new Desc(row.getDesc());
        }
    }

    public record Skill_ids(List<Integer> values) {
        public static Skill_ids from(RankedPresetTable row) {
            return new Skill_ids(row.getSkillIdsList());
        }
    }

    public record Pet_entry_ids(List<Integer> values) {
        public static Pet_entry_ids from(RankedPresetTable row) {
            return new Pet_entry_ids(row.getPetEntryIdsList());
        }
    }

    public record Pet_entry_points(List<Integer> values) {
        public static Pet_entry_points from(RankedPresetTable row) {
            return new Pet_entry_points(row.getPetEntryPointsList());
        }
    }

}