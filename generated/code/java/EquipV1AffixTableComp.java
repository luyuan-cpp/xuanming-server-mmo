
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for EquipV1AffixTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class EquipV1AffixTableComp {

    private EquipV1AffixTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(EquipV1AffixTable row) {
            return new Id(row.getId());
        }
    }

    public record Pool(int value) {
        public static Pool from(EquipV1AffixTable row) {
            return new Pool(row.getPool());
        }
    }

    public record Effect(int value) {
        public static Effect from(EquipV1AffixTable row) {
            return new Effect(row.getEffect());
        }
    }

    public record Effect_param(int value) {
        public static Effect_param from(EquipV1AffixTable row) {
            return new Effect_param(row.getEffectParam());
        }
    }

    public record Value_permyriad(int value) {
        public static Value_permyriad from(EquipV1AffixTable row) {
            return new Value_permyriad(row.getValuePermyriad());
        }
    }

    public record Source_cap(int value) {
        public static Source_cap from(EquipV1AffixTable row) {
            return new Source_cap(row.getSourceCap());
        }
    }

    public record Name(String value) {
        public static Name from(EquipV1AffixTable row) {
            return new Name(row.getName());
        }
    }

    public record Description(String value) {
        public static Description from(EquipV1AffixTable row) {
            return new Description(row.getDescription());
        }
    }

    public record Material_id(int value) {
        public static Material_id from(EquipV1AffixTable row) {
            return new Material_id(row.getMaterialId());
        }
    }

    public record Material_count(int value) {
        public static Material_count from(EquipV1AffixTable row) {
            return new Material_count(row.getMaterialCount());
        }
    }

}