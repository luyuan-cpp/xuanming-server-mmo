
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for RankedRuleTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class RankedRuleTableComp {

    private RankedRuleTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(RankedRuleTable row) {
            return new Id(row.getId());
        }
    }

    public record Rule_version(int value) {
        public static Rule_version from(RankedRuleTable row) {
            return new Rule_version(row.getRuleVersion());
        }
    }

    public record Battle_config_id(int value) {
        public static Battle_config_id from(RankedRuleTable row) {
            return new Battle_config_id(row.getBattleConfigId());
        }
    }

    public record Reference_class_id(int value) {
        public static Reference_class_id from(RankedRuleTable row) {
            return new Reference_class_id(row.getReferenceClassId());
        }
    }

    public record Competitive_level(int value) {
        public static Competitive_level from(RankedRuleTable row) {
            return new Competitive_level(row.getCompetitiveLevel());
        }
    }

    public record Default_character_preset_id(int value) {
        public static Default_character_preset_id from(RankedRuleTable row) {
            return new Default_character_preset_id(row.getDefaultCharacterPresetId());
        }
    }

    public record Default_pet_preset_id(int value) {
        public static Default_pet_preset_id from(RankedRuleTable row) {
            return new Default_pet_preset_id(row.getDefaultPetPresetId());
        }
    }

    public record Season_duration_days(int value) {
        public static Season_duration_days from(RankedRuleTable row) {
            return new Season_duration_days(row.getSeasonDurationDays());
        }
    }

    public record Wait_prompt_seconds(int value) {
        public static Wait_prompt_seconds from(RankedRuleTable row) {
            return new Wait_prompt_seconds(row.getWaitPromptSeconds());
        }
    }

    public record Spread_tier1_seconds(int value) {
        public static Spread_tier1_seconds from(RankedRuleTable row) {
            return new Spread_tier1_seconds(row.getSpreadTier1Seconds());
        }
    }

    public record Spread_tier1_points(int value) {
        public static Spread_tier1_points from(RankedRuleTable row) {
            return new Spread_tier1_points(row.getSpreadTier1Points());
        }
    }

    public record Spread_tier2_seconds(int value) {
        public static Spread_tier2_seconds from(RankedRuleTable row) {
            return new Spread_tier2_seconds(row.getSpreadTier2Seconds());
        }
    }

    public record Spread_tier2_points(int value) {
        public static Spread_tier2_points from(RankedRuleTable row) {
            return new Spread_tier2_points(row.getSpreadTier2Points());
        }
    }

    public record Spread_tier3_points(int value) {
        public static Spread_tier3_points from(RankedRuleTable row) {
            return new Spread_tier3_points(row.getSpreadTier3Points());
        }
    }

    public record Milestone_avatar_matches(int value) {
        public static Milestone_avatar_matches from(RankedRuleTable row) {
            return new Milestone_avatar_matches(row.getMilestoneAvatarMatches());
        }
    }

    public record Milestone_card_matches(int value) {
        public static Milestone_card_matches from(RankedRuleTable row) {
            return new Milestone_card_matches(row.getMilestoneCardMatches());
        }
    }

    public record Milestone_talisman_matches(int value) {
        public static Milestone_talisman_matches from(RankedRuleTable row) {
            return new Milestone_talisman_matches(row.getMilestoneTalismanMatches());
        }
    }

    public record Tier_qingfeng_centi(int value) {
        public static Tier_qingfeng_centi from(RankedRuleTable row) {
            return new Tier_qingfeng_centi(row.getTierQingfengCenti());
        }
    }

    public record Tier_yunzhang_centi(int value) {
        public static Tier_yunzhang_centi from(RankedRuleTable row) {
            return new Tier_yunzhang_centi(row.getTierYunzhangCenti());
        }
    }

    public record Tier_tianshu_centi(int value) {
        public static Tier_tianshu_centi from(RankedRuleTable row) {
            return new Tier_tianshu_centi(row.getTierTianshuCenti());
        }
    }

    public record Tier_wushuang_centi(int value) {
        public static Tier_wushuang_centi from(RankedRuleTable row) {
            return new Tier_wushuang_centi(row.getTierWushuangCenti());
        }
    }

    public record Desc(String value) {
        public static Desc from(RankedRuleTable row) {
            return new Desc(row.getDesc());
        }
    }

    public record Milestone_win_matches(int value) {
        public static Milestone_win_matches from(RankedRuleTable row) {
            return new Milestone_win_matches(row.getMilestoneWinMatches());
        }
    }

}