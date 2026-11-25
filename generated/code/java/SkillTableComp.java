
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for SkillTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class SkillTableComp {

    private SkillTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(SkillTable row) {
            return new Id(row.getId());
        }
    }

    public record Require_target(int value) {
        public static Require_target from(SkillTable row) {
            return new Require_target(row.getRequireTarget());
        }
    }

    public record Target_status(int value) {
        public static Target_status from(SkillTable row) {
            return new Target_status(row.getTargetStatus());
        }
    }

    public record Cast_point(double value) {
        public static Cast_point from(SkillTable row) {
            return new Cast_point(row.getCastPoint());
        }
    }

    public record Recovery_time(double value) {
        public static Recovery_time from(SkillTable row) {
            return new Recovery_time(row.getRecoveryTime());
        }
    }

    public record Immediate(int value) {
        public static Immediate from(SkillTable row) {
            return new Immediate(row.getImmediate());
        }
    }

    public record Channel_think(int value) {
        public static Channel_think from(SkillTable row) {
            return new Channel_think(row.getChannelThink());
        }
    }

    public record Channel_finish(int value) {
        public static Channel_finish from(SkillTable row) {
            return new Channel_finish(row.getChannelFinish());
        }
    }

    public record Think_interval(int value) {
        public static Think_interval from(SkillTable row) {
            return new Think_interval(row.getThinkInterval());
        }
    }

    public record Channel_time(int value) {
        public static Channel_time from(SkillTable row) {
            return new Channel_time(row.getChannelTime());
        }
    }

    public record Range(double value) {
        public static Range from(SkillTable row) {
            return new Range(row.getRange());
        }
    }

    public record Max_range(double value) {
        public static Max_range from(SkillTable row) {
            return new Max_range(row.getMaxRange());
        }
    }

    public record Min_range(double value) {
        public static Min_range from(SkillTable row) {
            return new Min_range(row.getMinRange());
        }
    }

    public record Self_status(int value) {
        public static Self_status from(SkillTable row) {
            return new Self_status(row.getSelfStatus());
        }
    }

    public record Required_status(int value) {
        public static Required_status from(SkillTable row) {
            return new Required_status(row.getRequiredStatus());
        }
    }

    public record Cooldown_id(int value) {
        public static Cooldown_id from(SkillTable row) {
            return new Cooldown_id(row.getCooldownId());
        }
    }

    public record Damage(String value) {
        public static Damage from(SkillTable row) {
            return new Damage(row.getDamage());
        }
    }

    public record Damage_type(int value) {
        public static Damage_type from(SkillTable row) {
            return new Damage_type(row.getDamageType());
        }
    }

    public record Attack_multiplier(double value) {
        public static Attack_multiplier from(SkillTable row) {
            return new Attack_multiplier(row.getAttackMultiplier());
        }
    }

    public record Class_id(int value) {
        public static Class_id from(SkillTable row) {
            return new Class_id(row.getClassId());
        }
    }

    public record Name(String value) {
        public static Name from(SkillTable row) {
            return new Name(row.getName());
        }
    }

    public record Description(String value) {
        public static Description from(SkillTable row) {
            return new Description(row.getDescription());
        }
    }

    public record Icon_key(String value) {
        public static Icon_key from(SkillTable row) {
            return new Icon_key(row.getIconKey());
        }
    }

    public record Category(String value) {
        public static Category from(SkillTable row) {
            return new Category(row.getCategory());
        }
    }

    public record Unlock_level(int value) {
        public static Unlock_level from(SkillTable row) {
            return new Unlock_level(row.getUnlockLevel());
        }
    }

    public record Max_rank(int value) {
        public static Max_rank from(SkillTable row) {
            return new Max_rank(row.getMaxRank());
        }
    }

    public record Target_team(int value) {
        public static Target_team from(SkillTable row) {
            return new Target_team(row.getTargetTeam());
        }
    }

    public record Max_targets(int value) {
        public static Max_targets from(SkillTable row) {
            return new Max_targets(row.getMaxTargets());
        }
    }

    public record Mana_percent(double value) {
        public static Mana_percent from(SkillTable row) {
            return new Mana_percent(row.getManaPercent());
        }
    }

    public record Cooldown_rounds(int value) {
        public static Cooldown_rounds from(SkillTable row) {
            return new Cooldown_rounds(row.getCooldownRounds());
        }
    }

    public record Heal_multiplier(double value) {
        public static Heal_multiplier from(SkillTable row) {
            return new Heal_multiplier(row.getHealMultiplier());
        }
    }

    public record Heal_per_level(double value) {
        public static Heal_per_level from(SkillTable row) {
            return new Heal_per_level(row.getHealPerLevel());
        }
    }

    public record Execute_heal_threshold(double value) {
        public static Execute_heal_threshold from(SkillTable row) {
            return new Execute_heal_threshold(row.getExecuteHealThreshold());
        }
    }

    public record Execute_heal_bonus(double value) {
        public static Execute_heal_bonus from(SkillTable row) {
            return new Execute_heal_bonus(row.getExecuteHealBonus());
        }
    }

    public record Restore_mana_percent(double value) {
        public static Restore_mana_percent from(SkillTable row) {
            return new Restore_mana_percent(row.getRestoreManaPercent());
        }
    }

    public record Dispel_count(int value) {
        public static Dispel_count from(SkillTable row) {
            return new Dispel_count(row.getDispelCount());
        }
    }

    public record Initiative(int value) {
        public static Initiative from(SkillTable row) {
            return new Initiative(row.getInitiative());
        }
    }

    public record Passive_stat(String value) {
        public static Passive_stat from(SkillTable row) {
            return new Passive_stat(row.getPassiveStat());
        }
    }

    public record Passive_percent(double value) {
        public static Passive_percent from(SkillTable row) {
            return new Passive_percent(row.getPassivePercent());
        }
    }

    public record Buff_percent(double value) {
        public static Buff_percent from(SkillTable row) {
            return new Buff_percent(row.getBuffPercent());
        }
    }

    public record Battle_model(int value) {
        public static Battle_model from(SkillTable row) {
            return new Battle_model(row.getBattleModel());
        }
    }

    public record Caster_mask(int value) {
        public static Caster_mask from(SkillTable row) {
            return new Caster_mask(row.getCasterMask());
        }
    }

    public record Ability_kind(int value) {
        public static Ability_kind from(SkillTable row) {
            return new Ability_kind(row.getAbilityKind());
        }
    }

    public record Pet_max_targets(int value) {
        public static Pet_max_targets from(SkillTable row) {
            return new Pet_max_targets(row.getPetMaxTargets());
        }
    }

    public record Prototype_code(String value) {
        public static Prototype_code from(SkillTable row) {
            return new Prototype_code(row.getPrototypeCode());
        }
    }

    public record Max_battle_uses(int value) {
        public static Max_battle_uses from(SkillTable row) {
            return new Max_battle_uses(row.getMaxBattleUses());
        }
    }

    public record Health_cost_mode(int value) {
        public static Health_cost_mode from(SkillTable row) {
            return new Health_cost_mode(row.getHealthCostMode());
        }
    }

    public record Action_tags(int value) {
        public static Action_tags from(SkillTable row) {
            return new Action_tags(row.getActionTags());
        }
    }

    public record Allow_dead_target(int value) {
        public static Allow_dead_target from(SkillTable row) {
            return new Allow_dead_target(row.getAllowDeadTarget());
        }
    }

    public record Forbid_copy(int value) {
        public static Forbid_copy from(SkillTable row) {
            return new Forbid_copy(row.getForbidCopy());
        }
    }

    public record Season_id(int value) {
        public static Season_id from(SkillTable row) {
            return new Season_id(row.getSeasonId());
        }
    }

    public record Scroll_id(int value) {
        public static Scroll_id from(SkillTable row) {
            return new Scroll_id(row.getScrollId());
        }
    }

    public record Rarity_tier(int value) {
        public static Rarity_tier from(SkillTable row) {
            return new Rarity_tier(row.getRarityTier());
        }
    }

    public record Skill_type(List<Integer> values) {
        public static Skill_type from(SkillTable row) {
            return new Skill_type(row.getSkillTypeList());
        }
    }

    public record Targeting_mode(List<Integer> values) {
        public static Targeting_mode from(SkillTable row) {
            return new Targeting_mode(row.getTargetingModeList());
        }
    }

    public record Effect(List<Integer> values) {
        public static Effect from(SkillTable row) {
            return new Effect(row.getEffectList());
        }
    }

    public record Init_effect(List<Integer> values) {
        public static Init_effect from(SkillTable row) {
            return new Init_effect(row.getInitEffectList());
        }
    }

}