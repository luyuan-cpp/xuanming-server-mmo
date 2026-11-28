
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for RewardedAdSlotTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class RewardedAdSlotTableComp {

    private RewardedAdSlotTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(RewardedAdSlotTable row) {
            return new Id(row.getId());
        }
    }

    public record Enabled(int value) {
        public static Enabled from(RewardedAdSlotTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Min_level(int value) {
        public static Min_level from(RewardedAdSlotTable row) {
            return new Min_level(row.getMinLevel());
        }
    }

    public record Daily_limit(int value) {
        public static Daily_limit from(RewardedAdSlotTable row) {
            return new Daily_limit(row.getDailyLimit());
        }
    }

    public record Cooldown_sec(int value) {
        public static Cooldown_sec from(RewardedAdSlotTable row) {
            return new Cooldown_sec(row.getCooldownSec());
        }
    }

    public record Min_watch_sec(int value) {
        public static Min_watch_sec from(RewardedAdSlotTable row) {
            return new Min_watch_sec(row.getMinWatchSec());
        }
    }

    public record Session_ttl_sec(int value) {
        public static Session_ttl_sec from(RewardedAdSlotTable row) {
            return new Session_ttl_sec(row.getSessionTtlSec());
        }
    }

    public record Currency_type(int value) {
        public static Currency_type from(RewardedAdSlotTable row) {
            return new Currency_type(row.getCurrencyType());
        }
    }

    public record Reward_amount(long value) {
        public static Reward_amount from(RewardedAdSlotTable row) {
            return new Reward_amount(row.getRewardAmount());
        }
    }

    public record Require_ssv(int value) {
        public static Require_ssv from(RewardedAdSlotTable row) {
            return new Require_ssv(row.getRequireSsv());
        }
    }

    public record Platform_mask(int value) {
        public static Platform_mask from(RewardedAdSlotTable row) {
            return new Platform_mask(row.getPlatformMask());
        }
    }

    public record Reward_kind(int value) {
        public static Reward_kind from(RewardedAdSlotTable row) {
            return new Reward_kind(row.getRewardKind());
        }
    }

}