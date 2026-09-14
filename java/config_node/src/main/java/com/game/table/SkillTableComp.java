
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

}