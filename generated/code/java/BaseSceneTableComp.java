
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for BaseSceneTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class BaseSceneTableComp {

    private BaseSceneTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(BaseSceneTable row) {
            return new Id(row.getId());
        }
    }

    public record Nav_bin_file(String value) {
        public static Nav_bin_file from(BaseSceneTable row) {
            return new Nav_bin_file(row.getNavBinFile());
        }
    }

    public record Spawn_x(double value) {
        public static Spawn_x from(BaseSceneTable row) {
            return new Spawn_x(row.getSpawnX());
        }
    }

    public record Spawn_y(double value) {
        public static Spawn_y from(BaseSceneTable row) {
            return new Spawn_y(row.getSpawnY());
        }
    }

    public record Spawn_z(double value) {
        public static Spawn_z from(BaseSceneTable row) {
            return new Spawn_z(row.getSpawnZ());
        }
    }

}