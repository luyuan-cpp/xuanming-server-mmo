
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for AppearanceSeriesTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class AppearanceSeriesTableComp {

    private AppearanceSeriesTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(AppearanceSeriesTable row) {
            return new Id(row.getId());
        }
    }

    public record Series_key(String value) {
        public static Series_key from(AppearanceSeriesTable row) {
            return new Series_key(row.getSeriesKey());
        }
    }

    public record Name(String value) {
        public static Name from(AppearanceSeriesTable row) {
            return new Name(row.getName());
        }
    }

    public record Sort_order(int value) {
        public static Sort_order from(AppearanceSeriesTable row) {
            return new Sort_order(row.getSortOrder());
        }
    }

    public record Enabled(int value) {
        public static Enabled from(AppearanceSeriesTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Desc(String value) {
        public static Desc from(AppearanceSeriesTable row) {
            return new Desc(row.getDesc());
        }
    }

    public record Completion_badge_name(String value) {
        public static Completion_badge_name from(AppearanceSeriesTable row) {
            return new Completion_badge_name(row.getCompletionBadgeName());
        }
    }

    public record Epilogue_title(String value) {
        public static Epilogue_title from(AppearanceSeriesTable row) {
            return new Epilogue_title(row.getEpilogueTitle());
        }
    }

    public record Epilogue_body(String value) {
        public static Epilogue_body from(AppearanceSeriesTable row) {
            return new Epilogue_body(row.getEpilogueBody());
        }
    }

}