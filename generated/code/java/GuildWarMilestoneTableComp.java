
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for GuildWarMilestoneTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class GuildWarMilestoneTableComp {

    private GuildWarMilestoneTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(GuildWarMilestoneTable row) {
            return new Id(row.getId());
        }
    }

    public record Required_guild_merit(long value) {
        public static Required_guild_merit from(GuildWarMilestoneTable row) {
            return new Required_guild_merit(row.getRequiredGuildMerit());
        }
    }

    public record Reward_item1_id(int value) {
        public static Reward_item1_id from(GuildWarMilestoneTable row) {
            return new Reward_item1_id(row.getRewardItem1Id());
        }
    }

    public record Reward_item1_count(int value) {
        public static Reward_item1_count from(GuildWarMilestoneTable row) {
            return new Reward_item1_count(row.getRewardItem1Count());
        }
    }

    public record Reward_item2_id(int value) {
        public static Reward_item2_id from(GuildWarMilestoneTable row) {
            return new Reward_item2_id(row.getRewardItem2Id());
        }
    }

    public record Reward_item2_count(int value) {
        public static Reward_item2_count from(GuildWarMilestoneTable row) {
            return new Reward_item2_count(row.getRewardItem2Count());
        }
    }

    public record Reward_silver(long value) {
        public static Reward_silver from(GuildWarMilestoneTable row) {
            return new Reward_silver(row.getRewardSilver());
        }
    }

    public record Reward_contribution(long value) {
        public static Reward_contribution from(GuildWarMilestoneTable row) {
            return new Reward_contribution(row.getRewardContribution());
        }
    }

    public record Reward_preview(String value) {
        public static Reward_preview from(GuildWarMilestoneTable row) {
            return new Reward_preview(row.getRewardPreview());
        }
    }

}