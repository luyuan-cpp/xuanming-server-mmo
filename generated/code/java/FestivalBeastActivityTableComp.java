
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for FestivalBeastActivityTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class FestivalBeastActivityTableComp {

    private FestivalBeastActivityTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(FestivalBeastActivityTable row) {
            return new Id(row.getId());
        }
    }

    public record Enabled(boolean value) {
        public static Enabled from(FestivalBeastActivityTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Config_revision(int value) {
        public static Config_revision from(FestivalBeastActivityTable row) {
            return new Config_revision(row.getConfigRevision());
        }
    }

    public record Activity_name(String value) {
        public static Activity_name from(FestivalBeastActivityTable row) {
            return new Activity_name(row.getActivityName());
        }
    }

    public record Boss_name(String value) {
        public static Boss_name from(FestivalBeastActivityTable row) {
            return new Boss_name(row.getBossName());
        }
    }

    public record Banner_asset_key(String value) {
        public static Banner_asset_key from(FestivalBeastActivityTable row) {
            return new Banner_asset_key(row.getBannerAssetKey());
        }
    }

    public record Icon_key(String value) {
        public static Icon_key from(FestivalBeastActivityTable row) {
            return new Icon_key(row.getIconKey());
        }
    }

    public record Effective_start_at_ms(long value) {
        public static Effective_start_at_ms from(FestivalBeastActivityTable row) {
            return new Effective_start_at_ms(row.getEffectiveStartAtMs());
        }
    }

    public record Effective_end_at_ms(long value) {
        public static Effective_end_at_ms from(FestivalBeastActivityTable row) {
            return new Effective_end_at_ms(row.getEffectiveEndAtMs());
        }
    }

    public record Claim_showcase_days(int value) {
        public static Claim_showcase_days from(FestivalBeastActivityTable row) {
            return new Claim_showcase_days(row.getClaimShowcaseDays());
        }
    }

    public record Battle_config_id(int value) {
        public static Battle_config_id from(FestivalBeastActivityTable row) {
            return new Battle_config_id(row.getBattleConfigId());
        }
    }

    public record Required_wins(int value) {
        public static Required_wins from(FestivalBeastActivityTable row) {
            return new Required_wins(row.getRequiredWins());
        }
    }

    public record Reward_identity(String value) {
        public static Reward_identity from(FestivalBeastActivityTable row) {
            return new Reward_identity(row.getRewardIdentity());
        }
    }

    public record Reward_definition_version(int value) {
        public static Reward_definition_version from(FestivalBeastActivityTable row) {
            return new Reward_definition_version(row.getRewardDefinitionVersion());
        }
    }

    public record Reward_pet_table_id(int value) {
        public static Reward_pet_table_id from(FestivalBeastActivityTable row) {
            return new Reward_pet_table_id(row.getRewardPetTableId());
        }
    }

    public record Round_limit(int value) {
        public static Round_limit from(FestivalBeastActivityTable row) {
            return new Round_limit(row.getRoundLimit());
        }
    }

    public record Cycle_rounds(int value) {
        public static Cycle_rounds from(FestivalBeastActivityTable row) {
            return new Cycle_rounds(row.getCycleRounds());
        }
    }

    public record First_heavy_round(int value) {
        public static First_heavy_round from(FestivalBeastActivityTable row) {
            return new First_heavy_round(row.getFirstHeavyRound());
        }
    }

    public record Telegraph_lead_rounds(int value) {
        public static Telegraph_lead_rounds from(FestivalBeastActivityTable row) {
            return new Telegraph_lead_rounds(row.getTelegraphLeadRounds());
        }
    }

    public record Heavy_damage_percent(int value) {
        public static Heavy_damage_percent from(FestivalBeastActivityTable row) {
            return new Heavy_damage_percent(row.getHeavyDamagePercent());
        }
    }

}