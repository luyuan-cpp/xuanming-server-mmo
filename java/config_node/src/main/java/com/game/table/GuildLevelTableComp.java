
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for GuildLevelTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class GuildLevelTableComp {

    private GuildLevelTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(GuildLevelTable row) {
            return new Id(row.getId());
        }
    }

    public record Max_members(int value) {
        public static Max_members from(GuildLevelTable row) {
            return new Max_members(row.getMaxMembers());
        }
    }

    public record Max_officers(int value) {
        public static Max_officers from(GuildLevelTable row) {
            return new Max_officers(row.getMaxOfficers());
        }
    }

    public record Upgrade_cost_funds(long value) {
        public static Upgrade_cost_funds from(GuildLevelTable row) {
            return new Upgrade_cost_funds(row.getUpgradeCostFunds());
        }
    }

}