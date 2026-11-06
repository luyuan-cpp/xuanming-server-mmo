
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for ZoneGroupTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class ZoneGroupTableComp {

    private ZoneGroupTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(ZoneGroupTable row) {
            return new Id(row.getId());
        }
    }

    public record Group_id(int value) {
        public static Group_id from(ZoneGroupTable row) {
            return new Group_id(row.getGroupId());
        }
    }

    public record Zone_begin(int value) {
        public static Zone_begin from(ZoneGroupTable row) {
            return new Zone_begin(row.getZoneBegin());
        }
    }

    public record Zone_end(int value) {
        public static Zone_end from(ZoneGroupTable row) {
            return new Zone_end(row.getZoneEnd());
        }
    }

}