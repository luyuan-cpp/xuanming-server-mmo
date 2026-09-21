
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for GuildDonateTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class GuildDonateTableComp {

    private GuildDonateTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(GuildDonateTable row) {
            return new Id(row.getId());
        }
    }

    public record Name(String value) {
        public static Name from(GuildDonateTable row) {
            return new Name(row.getName());
        }
    }

    public record Currency_type(int value) {
        public static Currency_type from(GuildDonateTable row) {
            return new Currency_type(row.getCurrencyType());
        }
    }

    public record Cost_amount(long value) {
        public static Cost_amount from(GuildDonateTable row) {
            return new Cost_amount(row.getCostAmount());
        }
    }

    public record Contribution_gain(long value) {
        public static Contribution_gain from(GuildDonateTable row) {
            return new Contribution_gain(row.getContributionGain());
        }
    }

    public record Funds_gain(long value) {
        public static Funds_gain from(GuildDonateTable row) {
            return new Funds_gain(row.getFundsGain());
        }
    }

    public record Daily_limit(int value) {
        public static Daily_limit from(GuildDonateTable row) {
            return new Daily_limit(row.getDailyLimit());
        }
    }

    public record Min_guild_level(int value) {
        public static Min_guild_level from(GuildDonateTable row) {
            return new Min_guild_level(row.getMinGuildLevel());
        }
    }

}