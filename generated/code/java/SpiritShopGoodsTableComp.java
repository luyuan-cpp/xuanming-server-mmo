
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for SpiritShopGoodsTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class SpiritShopGoodsTableComp {

    private SpiritShopGoodsTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(SpiritShopGoodsTable row) {
            return new Id(row.getId());
        }
    }

    public record Tab_id(int value) {
        public static Tab_id from(SpiritShopGoodsTable row) {
            return new Tab_id(row.getTabId());
        }
    }

    public record Sort_order(int value) {
        public static Sort_order from(SpiritShopGoodsTable row) {
            return new Sort_order(row.getSortOrder());
        }
    }

    public record Enabled(int value) {
        public static Enabled from(SpiritShopGoodsTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Name(String value) {
        public static Name from(SpiritShopGoodsTable row) {
            return new Name(row.getName());
        }
    }

    public record Description(String value) {
        public static Description from(SpiritShopGoodsTable row) {
            return new Description(row.getDescription());
        }
    }

    public record Icon_key(String value) {
        public static Icon_key from(SpiritShopGoodsTable row) {
            return new Icon_key(row.getIconKey());
        }
    }

    public record Currency_type(int value) {
        public static Currency_type from(SpiritShopGoodsTable row) {
            return new Currency_type(row.getCurrencyType());
        }
    }

    public record Price(long value) {
        public static Price from(SpiritShopGoodsTable row) {
            return new Price(row.getPrice());
        }
    }

    public record Max_buy_per_request(int value) {
        public static Max_buy_per_request from(SpiritShopGoodsTable row) {
            return new Max_buy_per_request(row.getMaxBuyPerRequest());
        }
    }

    public record Item_id_1(int value) {
        public static Item_id_1 from(SpiritShopGoodsTable row) {
            return new Item_id_1(row.getItemId1());
        }
    }

    public record Item_count_1(int value) {
        public static Item_count_1 from(SpiritShopGoodsTable row) {
            return new Item_count_1(row.getItemCount1());
        }
    }

    public record Item_bound_1(int value) {
        public static Item_bound_1 from(SpiritShopGoodsTable row) {
            return new Item_bound_1(row.getItemBound1());
        }
    }

    public record Item_id_2(int value) {
        public static Item_id_2 from(SpiritShopGoodsTable row) {
            return new Item_id_2(row.getItemId2());
        }
    }

    public record Item_count_2(int value) {
        public static Item_count_2 from(SpiritShopGoodsTable row) {
            return new Item_count_2(row.getItemCount2());
        }
    }

    public record Item_bound_2(int value) {
        public static Item_bound_2 from(SpiritShopGoodsTable row) {
            return new Item_bound_2(row.getItemBound2());
        }
    }

    public record Limit_reset_kind(int value) {
        public static Limit_reset_kind from(SpiritShopGoodsTable row) {
            return new Limit_reset_kind(row.getLimitResetKind());
        }
    }

    public record Limit_count(int value) {
        public static Limit_count from(SpiritShopGoodsTable row) {
            return new Limit_count(row.getLimitCount());
        }
    }

    public record Limit_group_id(int value) {
        public static Limit_group_id from(SpiritShopGoodsTable row) {
            return new Limit_group_id(row.getLimitGroupId());
        }
    }

    public record Limit_group_weight(int value) {
        public static Limit_group_weight from(SpiritShopGoodsTable row) {
            return new Limit_group_weight(row.getLimitGroupWeight());
        }
    }

    public record Badge_text(String value) {
        public static Badge_text from(SpiritShopGoodsTable row) {
            return new Badge_text(row.getBadgeText());
        }
    }

    public record Start_time_ms(long value) {
        public static Start_time_ms from(SpiritShopGoodsTable row) {
            return new Start_time_ms(row.getStartTimeMs());
        }
    }

    public record End_time_ms(long value) {
        public static End_time_ms from(SpiritShopGoodsTable row) {
            return new End_time_ms(row.getEndTimeMs());
        }
    }

    public record Grant_mode(int value) {
        public static Grant_mode from(SpiritShopGoodsTable row) {
            return new Grant_mode(row.getGrantMode());
        }
    }

    public record Item_id_3(int value) {
        public static Item_id_3 from(SpiritShopGoodsTable row) {
            return new Item_id_3(row.getItemId3());
        }
    }

    public record Item_count_3(int value) {
        public static Item_count_3 from(SpiritShopGoodsTable row) {
            return new Item_count_3(row.getItemCount3());
        }
    }

    public record Item_bound_3(int value) {
        public static Item_bound_3 from(SpiritShopGoodsTable row) {
            return new Item_bound_3(row.getItemBound3());
        }
    }

    public record Item_id_4(int value) {
        public static Item_id_4 from(SpiritShopGoodsTable row) {
            return new Item_id_4(row.getItemId4());
        }
    }

    public record Item_count_4(int value) {
        public static Item_count_4 from(SpiritShopGoodsTable row) {
            return new Item_count_4(row.getItemCount4());
        }
    }

    public record Item_bound_4(int value) {
        public static Item_bound_4 from(SpiritShopGoodsTable row) {
            return new Item_bound_4(row.getItemBound4());
        }
    }

    public record Original_price(long value) {
        public static Original_price from(SpiritShopGoodsTable row) {
            return new Original_price(row.getOriginalPrice());
        }
    }

    public record Min_level(int value) {
        public static Min_level from(SpiritShopGoodsTable row) {
            return new Min_level(row.getMinLevel());
        }
    }

    public record Max_level(int value) {
        public static Max_level from(SpiritShopGoodsTable row) {
            return new Max_level(row.getMaxLevel());
        }
    }

}