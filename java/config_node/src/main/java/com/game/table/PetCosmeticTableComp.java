
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for PetCosmeticTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class PetCosmeticTableComp {

    private PetCosmeticTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(PetCosmeticTable row) {
            return new Id(row.getId());
        }
    }

    public record Cosmetic_key(String value) {
        public static Cosmetic_key from(PetCosmeticTable row) {
            return new Cosmetic_key(row.getCosmeticKey());
        }
    }

    public record Name(String value) {
        public static Name from(PetCosmeticTable row) {
            return new Name(row.getName());
        }
    }

    public record Target_pet_table_id(int value) {
        public static Target_pet_table_id from(PetCosmeticTable row) {
            return new Target_pet_table_id(row.getTargetPetTableId());
        }
    }

    public record Visual_pet_table_id(int value) {
        public static Visual_pet_table_id from(PetCosmeticTable row) {
            return new Visual_pet_table_id(row.getVisualPetTableId());
        }
    }

    public record Visual_art_key(String value) {
        public static Visual_art_key from(PetCosmeticTable row) {
            return new Visual_art_key(row.getVisualArtKey());
        }
    }

    public record Enabled(int value) {
        public static Enabled from(PetCosmeticTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Sort_order(int value) {
        public static Sort_order from(PetCosmeticTable row) {
            return new Sort_order(row.getSortOrder());
        }
    }

    public record Mall_sku_id(long value) {
        public static Mall_sku_id from(PetCosmeticTable row) {
            return new Mall_sku_id(row.getMallSkuId());
        }
    }

    public record Acquire_hint(String value) {
        public static Acquire_hint from(PetCosmeticTable row) {
            return new Acquire_hint(row.getAcquireHint());
        }
    }

    public record Desc(String value) {
        public static Desc from(PetCosmeticTable row) {
            return new Desc(row.getDesc());
        }
    }

    public record Card_item_id(int value) {
        public static Card_item_id from(PetCosmeticTable row) {
            return new Card_item_id(row.getCardItemId());
        }
    }

    public record Card_item_count(int value) {
        public static Card_item_count from(PetCosmeticTable row) {
            return new Card_item_count(row.getCardItemCount());
        }
    }

    public record Spirit_shop_goods_id(int value) {
        public static Spirit_shop_goods_id from(PetCosmeticTable row) {
            return new Spirit_shop_goods_id(row.getSpiritShopGoodsId());
        }
    }

}