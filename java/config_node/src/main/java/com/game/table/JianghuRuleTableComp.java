
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for JianghuRuleTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class JianghuRuleTableComp {

    private JianghuRuleTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(JianghuRuleTable row) {
            return new Id(row.getId());
        }
    }

    public record Enabled(int value) {
        public static Enabled from(JianghuRuleTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Silver_writ_cost(long value) {
        public static Silver_writ_cost from(JianghuRuleTable row) {
            return new Silver_writ_cost(row.getSilverWritCost());
        }
    }

    public record Gold_writ_item_id(int value) {
        public static Gold_writ_item_id from(JianghuRuleTable row) {
            return new Gold_writ_item_id(row.getGoldWritItemId());
        }
    }

    public record Invite_ttl_sec(int value) {
        public static Invite_ttl_sec from(JianghuRuleTable row) {
            return new Invite_ttl_sec(row.getInviteTtlSec());
        }
    }

    public record Team_confirm_sec(int value) {
        public static Team_confirm_sec from(JianghuRuleTable row) {
            return new Team_confirm_sec(row.getTeamConfirmSec());
        }
    }

    public record Same_target_cooldown_sec(int value) {
        public static Same_target_cooldown_sec from(JianghuRuleTable row) {
            return new Same_target_cooldown_sec(row.getSameTargetCooldownSec());
        }
    }

    public record Daily_unmatched_invite_limit(int value) {
        public static Daily_unmatched_invite_limit from(JianghuRuleTable row) {
            return new Daily_unmatched_invite_limit(row.getDailyUnmatchedInviteLimit());
        }
    }

    public record Duel_round_limit(int value) {
        public static Duel_round_limit from(JianghuRuleTable row) {
            return new Duel_round_limit(row.getDuelRoundLimit());
        }
    }

    public record Duel_timeout_sec(int value) {
        public static Duel_timeout_sec from(JianghuRuleTable row) {
            return new Duel_timeout_sec(row.getDuelTimeoutSec());
        }
    }

    public record Defeat_mark_duration_sec(int value) {
        public static Defeat_mark_duration_sec from(JianghuRuleTable row) {
            return new Defeat_mark_duration_sec(row.getDefeatMarkDurationSec());
        }
    }

    public record Silver_clear_cost(long value) {
        public static Silver_clear_cost from(JianghuRuleTable row) {
            return new Silver_clear_cost(row.getSilverClearCost());
        }
    }

    public record Clear_item_id(int value) {
        public static Clear_item_id from(JianghuRuleTable row) {
            return new Clear_item_id(row.getClearItemId());
        }
    }

    public record Clear_badge_duration_sec(int value) {
        public static Clear_badge_duration_sec from(JianghuRuleTable row) {
            return new Clear_badge_duration_sec(row.getClearBadgeDurationSec());
        }
    }

    public record Grudge_max_rivals(int value) {
        public static Grudge_max_rivals from(JianghuRuleTable row) {
            return new Grudge_max_rivals(row.getGrudgeMaxRivals());
        }
    }

    public record Grudge_max_logs_per_rival(int value) {
        public static Grudge_max_logs_per_rival from(JianghuRuleTable row) {
            return new Grudge_max_logs_per_rival(row.getGrudgeMaxLogsPerRival());
        }
    }

    public record Guild_wall_max_records(int value) {
        public static Guild_wall_max_records from(JianghuRuleTable row) {
            return new Guild_wall_max_records(row.getGuildWallMaxRecords());
        }
    }

    public record Streak_highlight_threshold(int value) {
        public static Streak_highlight_threshold from(JianghuRuleTable row) {
            return new Streak_highlight_threshold(row.getStreakHighlightThreshold());
        }
    }

}