
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for AttributeAutoPlanTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class AttributeAutoPlanTableComp {

    private AttributeAutoPlanTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(AttributeAutoPlanTable row) {
            return new Id(row.getId());
        }
    }

    public record Class_id(int value) {
        public static Class_id from(AttributeAutoPlanTable row) {
            return new Class_id(row.getClassId());
        }
    }

    public record Pool_id(int value) {
        public static Pool_id from(AttributeAutoPlanTable row) {
            return new Pool_id(row.getPoolId());
        }
    }

    public record Desc(String value) {
        public static Desc from(AttributeAutoPlanTable row) {
            return new Desc(row.getDesc());
        }
    }

    public record Dimension(List<Integer> values) {
        public static Dimension from(AttributeAutoPlanTable row) {
            return new Dimension(row.getDimensionList());
        }
    }

    public record Weight(List<Integer> values) {
        public static Weight from(AttributeAutoPlanTable row) {
            return new Weight(row.getWeightList());
        }
    }

}