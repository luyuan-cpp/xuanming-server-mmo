
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for GuildShopTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class GuildShopTableComp {

    private GuildShopTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(GuildShopTable row) {
            return new Id(row.getId());
        }
    }

    public record Name(String value) {
        public static Name from(GuildShopTable row) {
            return new Name(row.getName());
        }
    }

    public record Category(int value) {
        public static Category from(GuildShopTable row) {
            return new Category(row.getCategory());
        }
    }

    public record Item_id(int value) {
        public static Item_id from(GuildShopTable row) {
            return new Item_id(row.getItemId());
        }
    }

    public record Item_count(int value) {
        public static Item_count from(GuildShopTable row) {
            return new Item_count(row.getItemCount());
        }
    }

    public record Cost_contribution(long value) {
        public static Cost_contribution from(GuildShopTable row) {
            return new Cost_contribution(row.getCostContribution());
        }
    }

    public record Required_guild_level(int value) {
        public static Required_guild_level from(GuildShopTable row) {
            return new Required_guild_level(row.getRequiredGuildLevel());
        }
    }

    public record Limit_period(int value) {
        public static Limit_period from(GuildShopTable row) {
            return new Limit_period(row.getLimitPeriod());
        }
    }

    public record Limit_count(int value) {
        public static Limit_count from(GuildShopTable row) {
            return new Limit_count(row.getLimitCount());
        }
    }

}