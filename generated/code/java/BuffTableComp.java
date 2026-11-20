
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for BuffTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class BuffTableComp {

    private BuffTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(BuffTable row) {
            return new Id(row.getId());
        }
    }

    public record No_caster(int value) {
        public static No_caster from(BuffTable row) {
            return new No_caster(row.getNoCaster());
        }
    }

    public record Buff_type(int value) {
        public static Buff_type from(BuffTable row) {
            return new Buff_type(row.getBuffType());
        }
    }

    public record Level(int value) {
        public static Level from(BuffTable row) {
            return new Level(row.getLevel());
        }
    }

    public record Max_layer(int value) {
        public static Max_layer from(BuffTable row) {
            return new Max_layer(row.getMaxLayer());
        }
    }

    public record Infinite_duration(int value) {
        public static Infinite_duration from(BuffTable row) {
            return new Infinite_duration(row.getInfiniteDuration());
        }
    }

    public record Duration(double value) {
        public static Duration from(BuffTable row) {
            return new Duration(row.getDuration());
        }
    }

    public record Force_interrupt(int value) {
        public static Force_interrupt from(BuffTable row) {
            return new Force_interrupt(row.getForceInterrupt());
        }
    }

    public record Interval(double value) {
        public static Interval from(BuffTable row) {
            return new Interval(row.getInterval());
        }
    }

    public record Interval_count(int value) {
        public static Interval_count from(BuffTable row) {
            return new Interval_count(row.getIntervalCount());
        }
    }

    public record Movement_speed_boost(double value) {
        public static Movement_speed_boost from(BuffTable row) {
            return new Movement_speed_boost(row.getMovementSpeedBoost());
        }
    }

    public record Movement_speed_reduction(double value) {
        public static Movement_speed_reduction from(BuffTable row) {
            return new Movement_speed_reduction(row.getMovementSpeedReduction());
        }
    }

    public record Health_regeneration(String value) {
        public static Health_regeneration from(BuffTable row) {
            return new Health_regeneration(row.getHealthRegeneration());
        }
    }

    public record Combat_idle_seconds(double value) {
        public static Combat_idle_seconds from(BuffTable row) {
            return new Combat_idle_seconds(row.getCombatIdleSeconds());
        }
    }

    public record Time(int value) {
        public static Time from(BuffTable row) {
            return new Time(row.getTime());
        }
    }

    public record Bonus_damage(String value) {
        public static Bonus_damage from(BuffTable row) {
            return new Bonus_damage(row.getBonusDamage());
        }
    }

    public record Battle_op(int value) {
        public static Battle_op from(BuffTable row) {
            return new Battle_op(row.getBattleOp());
        }
    }

    public record Trigger_event(int value) {
        public static Trigger_event from(BuffTable row) {
            return new Trigger_event(row.getTriggerEvent());
        }
    }

    public record Effect_target(int value) {
        public static Effect_target from(BuffTable row) {
            return new Effect_target(row.getEffectTarget());
        }
    }

    public record Flat_value(double value) {
        public static Flat_value from(BuffTable row) {
            return new Flat_value(row.getFlatValue());
        }
    }

    public record Per_level(double value) {
        public static Per_level from(BuffTable row) {
            return new Per_level(row.getPerLevel());
        }
    }

    public record Ratio(double value) {
        public static Ratio from(BuffTable row) {
            return new Ratio(row.getRatio());
        }
    }

    public record Value_basis(int value) {
        public static Value_basis from(BuffTable row) {
            return new Value_basis(row.getValueBasis());
        }
    }

    public record Damage_filter(int value) {
        public static Damage_filter from(BuffTable row) {
            return new Damage_filter(row.getDamageFilter());
        }
    }

    public record Duration_rounds(int value) {
        public static Duration_rounds from(BuffTable row) {
            return new Duration_rounds(row.getDurationRounds());
        }
    }

    public record Tick_count(int value) {
        public static Tick_count from(BuffTable row) {
            return new Tick_count(row.getTickCount());
        }
    }

    public record Trigger_chance(double value) {
        public static Trigger_chance from(BuffTable row) {
            return new Trigger_chance(row.getTriggerChance());
        }
    }

    public record Per_round_limit(int value) {
        public static Per_round_limit from(BuffTable row) {
            return new Per_round_limit(row.getPerRoundLimit());
        }
    }

    public record Hit_limit(int value) {
        public static Hit_limit from(BuffTable row) {
            return new Hit_limit(row.getHitLimit());
        }
    }

    public record Modifier_stat(int value) {
        public static Modifier_stat from(BuffTable row) {
            return new Modifier_stat(row.getModifierStat());
        }
    }

    public record State_kind(int value) {
        public static State_kind from(BuffTable row) {
            return new State_kind(row.getStateKind());
        }
    }

    public record Application_chance(double value) {
        public static Application_chance from(BuffTable row) {
            return new Application_chance(row.getApplicationChance());
        }
    }

    public record Required_tag(String value) {
        public static Required_tag from(BuffTable row) {
            return new Required_tag(row.getRequiredTag());
        }
    }

    public record Second_required_tag(String value) {
        public static Second_required_tag from(BuffTable row) {
            return new Second_required_tag(row.getSecondRequiredTag());
        }
    }

    public record Conditional_bonus(double value) {
        public static Conditional_bonus from(BuffTable row) {
            return new Conditional_bonus(row.getConditionalBonus());
        }
    }

    public record Secondary_ratio(double value) {
        public static Secondary_ratio from(BuffTable row) {
            return new Secondary_ratio(row.getSecondaryRatio());
        }
    }

    public record Effect_count(int value) {
        public static Effect_count from(BuffTable row) {
            return new Effect_count(row.getEffectCount());
        }
    }

    public record Target_count(int value) {
        public static Target_count from(BuffTable row) {
            return new Target_count(row.getTargetCount());
        }
    }

    public record Pet_target_count(int value) {
        public static Pet_target_count from(BuffTable row) {
            return new Pet_target_count(row.getPetTargetCount());
        }
    }

    public record Stack_key(int value) {
        public static Stack_key from(BuffTable row) {
            return new Stack_key(row.getStackKey());
        }
    }

    public record Refresh_policy(int value) {
        public static Refresh_policy from(BuffTable row) {
            return new Refresh_policy(row.getRefreshPolicy());
        }
    }

    public record Keep_on_death(int value) {
        public static Keep_on_death from(BuffTable row) {
            return new Keep_on_death(row.getKeepOnDeath());
        }
    }

    public record Effect_priority(int value) {
        public static Effect_priority from(BuffTable row) {
            return new Effect_priority(row.getEffectPriority());
        }
    }

    public record Start_round(int value) {
        public static Start_round from(BuffTable row) {
            return new Start_round(row.getStartRound());
        }
    }

    public record Stack_limit(int value) {
        public static Stack_limit from(BuffTable row) {
            return new Stack_limit(row.getStackLimit());
        }
    }

    public record Require_living_caster(int value) {
        public static Require_living_caster from(BuffTable row) {
            return new Require_living_caster(row.getRequireLivingCaster());
        }
    }

    public record Tick_on_apply(int value) {
        public static Tick_on_apply from(BuffTable row) {
            return new Tick_on_apply(row.getTickOnApply());
        }
    }

    public record Keep_on_logout(int value) {
        public static Keep_on_logout from(BuffTable row) {
            return new Keep_on_logout(row.getKeepOnLogout());
        }
    }

    public record Interval_effect(List<Double> values) {
        public static Interval_effect from(BuffTable row) {
            return new Interval_effect(row.getIntervalEffectList());
        }
    }

    public record Sub_buff(List<Integer> values) {
        public static Sub_buff from(BuffTable row) {
            return new Sub_buff(row.getSubBuffList());
        }
    }

    public record Target_sub_buff(List<Integer> values) {
        public static Target_sub_buff from(BuffTable row) {
            return new Target_sub_buff(row.getTargetSubBuffList());
        }
    }

}