
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for CosmeticShowcaseTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class CosmeticShowcaseTableComp {

    private CosmeticShowcaseTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(CosmeticShowcaseTable row) {
            return new Id(row.getId());
        }
    }

    public record Cosmetic_key(String value) {
        public static Cosmetic_key from(CosmeticShowcaseTable row) {
            return new Cosmetic_key(row.getCosmeticKey());
        }
    }

    public record Slot_kind(int value) {
        public static Slot_kind from(CosmeticShowcaseTable row) {
            return new Slot_kind(row.getSlotKind());
        }
    }

    public record Name(String value) {
        public static Name from(CosmeticShowcaseTable row) {
            return new Name(row.getName());
        }
    }

    public record Asset_key(String value) {
        public static Asset_key from(CosmeticShowcaseTable row) {
            return new Asset_key(row.getAssetKey());
        }
    }

    public record Secondary_asset_key(String value) {
        public static Secondary_asset_key from(CosmeticShowcaseTable row) {
            return new Secondary_asset_key(row.getSecondaryAssetKey());
        }
    }

    public record Card_item_id(int value) {
        public static Card_item_id from(CosmeticShowcaseTable row) {
            return new Card_item_id(row.getCardItemId());
        }
    }

    public record Card_item_count(int value) {
        public static Card_item_count from(CosmeticShowcaseTable row) {
            return new Card_item_count(row.getCardItemCount());
        }
    }

    public record Spirit_shop_goods_id(int value) {
        public static Spirit_shop_goods_id from(CosmeticShowcaseTable row) {
            return new Spirit_shop_goods_id(row.getSpiritShopGoodsId());
        }
    }

    public record Enabled(int value) {
        public static Enabled from(CosmeticShowcaseTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Sort_order(int value) {
        public static Sort_order from(CosmeticShowcaseTable row) {
            return new Sort_order(row.getSortOrder());
        }
    }

    public record Acquire_hint(String value) {
        public static Acquire_hint from(CosmeticShowcaseTable row) {
            return new Acquire_hint(row.getAcquireHint());
        }
    }

    public record Desc(String value) {
        public static Desc from(CosmeticShowcaseTable row) {
            return new Desc(row.getDesc());
        }
    }

}