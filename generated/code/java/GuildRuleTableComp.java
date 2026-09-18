
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for GuildRuleTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class GuildRuleTableComp {

    private GuildRuleTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(GuildRuleTable row) {
            return new Id(row.getId());
        }
    }

    public record Application_expire_hours(int value) {
        public static Application_expire_hours from(GuildRuleTable row) {
            return new Application_expire_hours(row.getApplicationExpireHours());
        }
    }

    public record Max_pending_applications_per_player(int value) {
        public static Max_pending_applications_per_player from(GuildRuleTable row) {
            return new Max_pending_applications_per_player(row.getMaxPendingApplicationsPerPlayer());
        }
    }

    public record Max_pending_applications_per_guild(int value) {
        public static Max_pending_applications_per_guild from(GuildRuleTable row) {
            return new Max_pending_applications_per_guild(row.getMaxPendingApplicationsPerGuild());
        }
    }

    public record Asset_op_deadline_seconds(int value) {
        public static Asset_op_deadline_seconds from(GuildRuleTable row) {
            return new Asset_op_deadline_seconds(row.getAssetOpDeadlineSeconds());
        }
    }

    public record Asset_op_retry_base_ms(int value) {
        public static Asset_op_retry_base_ms from(GuildRuleTable row) {
            return new Asset_op_retry_base_ms(row.getAssetOpRetryBaseMs());
        }
    }

    public record Reunion_min_online_members(int value) {
        public static Reunion_min_online_members from(GuildRuleTable row) {
            return new Reunion_min_online_members(row.getReunionMinOnlineMembers());
        }
    }

}