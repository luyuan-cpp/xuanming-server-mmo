
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for SeasonRuleTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class SeasonRuleTableComp {

    private SeasonRuleTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(SeasonRuleTable row) {
            return new Id(row.getId());
        }
    }

    public record Name(String value) {
        public static Name from(SeasonRuleTable row) {
            return new Name(row.getName());
        }
    }

    public record Starts_at_ms(long value) {
        public static Starts_at_ms from(SeasonRuleTable row) {
            return new Starts_at_ms(row.getStartsAtMs());
        }
    }

    public record Ends_at_ms(long value) {
        public static Ends_at_ms from(SeasonRuleTable row) {
            return new Ends_at_ms(row.getEndsAtMs());
        }
    }

    public record Enabled(boolean value) {
        public static Enabled from(SeasonRuleTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Seal_max_points(int value) {
        public static Seal_max_points from(SeasonRuleTable row) {
            return new Seal_max_points(row.getSealMaxPoints());
        }
    }

    public record Pve_win_points(int value) {
        public static Pve_win_points from(SeasonRuleTable row) {
            return new Pve_win_points(row.getPveWinPoints());
        }
    }

    public record Pvp_win_points(int value) {
        public static Pvp_win_points from(SeasonRuleTable row) {
            return new Pvp_win_points(row.getPvpWinPoints());
        }
    }

    public record Pvp_other_points(int value) {
        public static Pvp_other_points from(SeasonRuleTable row) {
            return new Pvp_other_points(row.getPvpOtherPoints());
        }
    }

    public record Max_health_bps_cap(int value) {
        public static Max_health_bps_cap from(SeasonRuleTable row) {
            return new Max_health_bps_cap(row.getMaxHealthBpsCap());
        }
    }

    public record Defense_bps_cap(int value) {
        public static Defense_bps_cap from(SeasonRuleTable row) {
            return new Defense_bps_cap(row.getDefenseBpsCap());
        }
    }

    public record Catchup_currency_type(int value) {
        public static Catchup_currency_type from(SeasonRuleTable row) {
            return new Catchup_currency_type(row.getCatchupCurrencyType());
        }
    }

    public record Catchup_price_per_point(long value) {
        public static Catchup_price_per_point from(SeasonRuleTable row) {
            return new Catchup_price_per_point(row.getCatchupPricePerPoint());
        }
    }

    public record Default_seal_id(int value) {
        public static Default_seal_id from(SeasonRuleTable row) {
            return new Default_seal_id(row.getDefaultSealId());
        }
    }

    public record Desc(String value) {
        public static Desc from(SeasonRuleTable row) {
            return new Desc(row.getDesc());
        }
    }

    public record Honor_title_name(String value) {
        public static Honor_title_name from(SeasonRuleTable row) {
            return new Honor_title_name(row.getHonorTitleName());
        }
    }

    public record Honor_badge_icon_key(String value) {
        public static Honor_badge_icon_key from(SeasonRuleTable row) {
            return new Honor_badge_icon_key(row.getHonorBadgeIconKey());
        }
    }

    public record Cycle_order(int value) {
        public static Cycle_order from(SeasonRuleTable row) {
            return new Cycle_order(row.getCycleOrder());
        }
    }

    public record Cycle_duration_days(int value) {
        public static Cycle_duration_days from(SeasonRuleTable row) {
            return new Cycle_duration_days(row.getCycleDurationDays());
        }
    }

}