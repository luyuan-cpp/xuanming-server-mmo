
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for SkillVfxCosmeticTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class SkillVfxCosmeticTableComp {

    private SkillVfxCosmeticTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(SkillVfxCosmeticTable row) {
            return new Id(row.getId());
        }
    }

    public record Vfx_key(String value) {
        public static Vfx_key from(SkillVfxCosmeticTable row) {
            return new Vfx_key(row.getVfxKey());
        }
    }

    public record Name(String value) {
        public static Name from(SkillVfxCosmeticTable row) {
            return new Name(row.getName());
        }
    }

    public record Skill_table_id(int value) {
        public static Skill_table_id from(SkillVfxCosmeticTable row) {
            return new Skill_table_id(row.getSkillTableId());
        }
    }

    public record Fx_id(String value) {
        public static Fx_id from(SkillVfxCosmeticTable row) {
            return new Fx_id(row.getFxId());
        }
    }

    public record Projectile_fx_id(String value) {
        public static Projectile_fx_id from(SkillVfxCosmeticTable row) {
            return new Projectile_fx_id(row.getProjectileFxId());
        }
    }

    public record Telegraph_fx_id(String value) {
        public static Telegraph_fx_id from(SkillVfxCosmeticTable row) {
            return new Telegraph_fx_id(row.getTelegraphFxId());
        }
    }

    public record Tint_hex(String value) {
        public static Tint_hex from(SkillVfxCosmeticTable row) {
            return new Tint_hex(row.getTintHex());
        }
    }

    public record Enabled(int value) {
        public static Enabled from(SkillVfxCosmeticTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Sort_order(int value) {
        public static Sort_order from(SkillVfxCosmeticTable row) {
            return new Sort_order(row.getSortOrder());
        }
    }

    public record Mall_sku_id(long value) {
        public static Mall_sku_id from(SkillVfxCosmeticTable row) {
            return new Mall_sku_id(row.getMallSkuId());
        }
    }

    public record Acquire_hint(String value) {
        public static Acquire_hint from(SkillVfxCosmeticTable row) {
            return new Acquire_hint(row.getAcquireHint());
        }
    }

    public record Desc(String value) {
        public static Desc from(SkillVfxCosmeticTable row) {
            return new Desc(row.getDesc());
        }
    }

    public record Card_item_id(int value) {
        public static Card_item_id from(SkillVfxCosmeticTable row) {
            return new Card_item_id(row.getCardItemId());
        }
    }

    public record Card_item_count(int value) {
        public static Card_item_count from(SkillVfxCosmeticTable row) {
            return new Card_item_count(row.getCardItemCount());
        }
    }

    public record Spirit_shop_goods_id(int value) {
        public static Spirit_shop_goods_id from(SkillVfxCosmeticTable row) {
            return new Spirit_shop_goods_id(row.getSpiritShopGoodsId());
        }
    }

}