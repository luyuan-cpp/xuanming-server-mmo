
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for ItemTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class ItemTableComp {

    private ItemTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(ItemTable row) {
            return new Id(row.getId());
        }
    }

    public record Max_stack_size(int value) {
        public static Max_stack_size from(ItemTable row) {
            return new Max_stack_size(row.getMaxStackSize());
        }
    }

    public record Equip_kind(int value) {
        public static Equip_kind from(ItemTable row) {
            return new Equip_kind(row.getEquipKind());
        }
    }

    public record Battle_usable(int value) {
        public static Battle_usable from(ItemTable row) {
            return new Battle_usable(row.getBattleUsable());
        }
    }

    public record Battle_heal_hp(long value) {
        public static Battle_heal_hp from(ItemTable row) {
            return new Battle_heal_hp(row.getBattleHealHp());
        }
    }

    public record Battle_heal_mp(long value) {
        public static Battle_heal_mp from(ItemTable row) {
            return new Battle_heal_mp(row.getBattleHealMp());
        }
    }

    public record Name(String value) {
        public static Name from(ItemTable row) {
            return new Name(row.getName());
        }
    }

    public record Description(String value) {
        public static Description from(ItemTable row) {
            return new Description(row.getDescription());
        }
    }

    public record Icon_key(String value) {
        public static Icon_key from(ItemTable row) {
            return new Icon_key(row.getIconKey());
        }
    }

    public record Equip_level(int value) {
        public static Equip_level from(ItemTable row) {
            return new Equip_level(row.getEquipLevel());
        }
    }

    public record Equip_class(int value) {
        public static Equip_class from(ItemTable row) {
            return new Equip_class(row.getEquipClass());
        }
    }

    public record Affix_pool(int value) {
        public static Affix_pool from(ItemTable row) {
            return new Affix_pool(row.getAffixPool());
        }
    }

    public record Affix_rule(int value) {
        public static Affix_rule from(ItemTable row) {
            return new Affix_rule(row.getAffixRule());
        }
    }

    public record Equip_profile(int value) {
        public static Equip_profile from(ItemTable row) {
            return new Equip_profile(row.getEquipProfile());
        }
    }

    public record Forge_material_id(int value) {
        public static Forge_material_id from(ItemTable row) {
            return new Forge_material_id(row.getForgeMaterialId());
        }
    }

    public record Forge_material_count(int value) {
        public static Forge_material_count from(ItemTable row) {
            return new Forge_material_count(row.getForgeMaterialCount());
        }
    }

    public record Artifact_id(int value) {
        public static Artifact_id from(ItemTable row) {
            return new Artifact_id(row.getArtifactId());
        }
    }

}