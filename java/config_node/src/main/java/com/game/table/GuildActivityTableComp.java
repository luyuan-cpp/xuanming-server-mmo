
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for GuildActivityTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class GuildActivityTableComp {

    private GuildActivityTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(GuildActivityTable row) {
            return new Id(row.getId());
        }
    }

    public record Name(String value) {
        public static Name from(GuildActivityTable row) {
            return new Name(row.getName());
        }
    }

    public record Type(int value) {
        public static Type from(GuildActivityTable row) {
            return new Type(row.getType());
        }
    }

    public record Enabled(boolean value) {
        public static Enabled from(GuildActivityTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Start_at_ms(long value) {
        public static Start_at_ms from(GuildActivityTable row) {
            return new Start_at_ms(row.getStartAtMs());
        }
    }

    public record End_at_ms(long value) {
        public static End_at_ms from(GuildActivityTable row) {
            return new End_at_ms(row.getEndAtMs());
        }
    }

    public record Min_guild_level(int value) {
        public static Min_guild_level from(GuildActivityTable row) {
            return new Min_guild_level(row.getMinGuildLevel());
        }
    }

    public record Personal_contribution(long value) {
        public static Personal_contribution from(GuildActivityTable row) {
            return new Personal_contribution(row.getPersonalContribution());
        }
    }

    public record Guild_funds(long value) {
        public static Guild_funds from(GuildActivityTable row) {
            return new Guild_funds(row.getGuildFunds());
        }
    }

    public record Guild_threshold(int value) {
        public static Guild_threshold from(GuildActivityTable row) {
            return new Guild_threshold(row.getGuildThreshold());
        }
    }

    public record Reward_id(int value) {
        public static Reward_id from(GuildActivityTable row) {
            return new Reward_id(row.getRewardId());
        }
    }

    public record Dungeon_id(int value) {
        public static Dungeon_id from(GuildActivityTable row) {
            return new Dungeon_id(row.getDungeonId());
        }
    }

    public record Team_size_min(int value) {
        public static Team_size_min from(GuildActivityTable row) {
            return new Team_size_min(row.getTeamSizeMin());
        }
    }

    public record Team_size_max(int value) {
        public static Team_size_max from(GuildActivityTable row) {
            return new Team_size_max(row.getTeamSizeMax());
        }
    }

    public record Daily_limit(int value) {
        public static Daily_limit from(GuildActivityTable row) {
            return new Daily_limit(row.getDailyLimit());
        }
    }

}