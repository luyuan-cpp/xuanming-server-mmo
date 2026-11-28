
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for TravelPassSeasonTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class TravelPassSeasonTableComp {

    private TravelPassSeasonTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(TravelPassSeasonTable row) {
            return new Id(row.getId());
        }
    }

    public record Enabled(int value) {
        public static Enabled from(TravelPassSeasonTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Name(String value) {
        public static Name from(TravelPassSeasonTable row) {
            return new Name(row.getName());
        }
    }

    public record Description(String value) {
        public static Description from(TravelPassSeasonTable row) {
            return new Description(row.getDescription());
        }
    }

    public record Icon_key(String value) {
        public static Icon_key from(TravelPassSeasonTable row) {
            return new Icon_key(row.getIconKey());
        }
    }

    public record Banner_asset_key(String value) {
        public static Banner_asset_key from(TravelPassSeasonTable row) {
            return new Banner_asset_key(row.getBannerAssetKey());
        }
    }

    public record Starts_at_ms(long value) {
        public static Starts_at_ms from(TravelPassSeasonTable row) {
            return new Starts_at_ms(row.getStartsAtMs());
        }
    }

    public record Ends_at_ms(long value) {
        public static Ends_at_ms from(TravelPassSeasonTable row) {
            return new Ends_at_ms(row.getEndsAtMs());
        }
    }

    public record Max_points(int value) {
        public static Max_points from(TravelPassSeasonTable row) {
            return new Max_points(row.getMaxPoints());
        }
    }

    public record Pve_win_points(int value) {
        public static Pve_win_points from(TravelPassSeasonTable row) {
            return new Pve_win_points(row.getPveWinPoints());
        }
    }

    public record Hard_or_pvp_win_points(int value) {
        public static Hard_or_pvp_win_points from(TravelPassSeasonTable row) {
            return new Hard_or_pvp_win_points(row.getHardOrPvpWinPoints());
        }
    }

    public record Pvp_qualified_other_points(int value) {
        public static Pvp_qualified_other_points from(TravelPassSeasonTable row) {
            return new Pvp_qualified_other_points(row.getPvpQualifiedOtherPoints());
        }
    }

    public record Premium_currency_type(int value) {
        public static Premium_currency_type from(TravelPassSeasonTable row) {
            return new Premium_currency_type(row.getPremiumCurrencyType());
        }
    }

    public record Premium_price(long value) {
        public static Premium_price from(TravelPassSeasonTable row) {
            return new Premium_price(row.getPremiumPrice());
        }
    }

    public record Config_revision(int value) {
        public static Config_revision from(TravelPassSeasonTable row) {
            return new Config_revision(row.getConfigRevision());
        }
    }

    public record Sort_order(int value) {
        public static Sort_order from(TravelPassSeasonTable row) {
            return new Sort_order(row.getSortOrder());
        }
    }

    public record Is_archived(int value) {
        public static Is_archived from(TravelPassSeasonTable row) {
            return new Is_archived(row.getIsArchived());
        }
    }

}