
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for GuildWarRuleTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class GuildWarRuleTableComp {

    private GuildWarRuleTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(GuildWarRuleTable row) {
            return new Id(row.getId());
        }
    }

    public record Min_level(int value) {
        public static Min_level from(GuildWarRuleTable row) {
            return new Min_level(row.getMinLevel());
        }
    }

    public record Peace_cooldown_sec(int value) {
        public static Peace_cooldown_sec from(GuildWarRuleTable row) {
            return new Peace_cooldown_sec(row.getPeaceCooldownSec());
        }
    }

    public record Post_battle_protect_sec(int value) {
        public static Post_battle_protect_sec from(GuildWarRuleTable row) {
            return new Post_battle_protect_sec(row.getPostBattleProtectSec());
        }
    }

    public record Leyline_max_banner_hp(int value) {
        public static Leyline_max_banner_hp from(GuildWarRuleTable row) {
            return new Leyline_max_banner_hp(row.getLeylineMaxBannerHp());
        }
    }

    public record Leyline_capture_init_hp(int value) {
        public static Leyline_capture_init_hp from(GuildWarRuleTable row) {
            return new Leyline_capture_init_hp(row.getLeylineCaptureInitHp());
        }
    }

    public record Leyline_shield_sec(int value) {
        public static Leyline_shield_sec from(GuildWarRuleTable row) {
            return new Leyline_shield_sec(row.getLeylineShieldSec());
        }
    }

    public record Pvp_win_banner_damage(int value) {
        public static Pvp_win_banner_damage from(GuildWarRuleTable row) {
            return new Pvp_win_banner_damage(row.getPvpWinBannerDamage());
        }
    }

    public record Pvp_defend_banner_heal(int value) {
        public static Pvp_defend_banner_heal from(GuildWarRuleTable row) {
            return new Pvp_defend_banner_heal(row.getPvpDefendBannerHeal());
        }
    }

    public record Siege_win_banner_damage(int value) {
        public static Siege_win_banner_damage from(GuildWarRuleTable row) {
            return new Siege_win_banner_damage(row.getSiegeWinBannerDamage());
        }
    }

    public record Garrison_max_slots(int value) {
        public static Garrison_max_slots from(GuildWarRuleTable row) {
            return new Garrison_max_slots(row.getGarrisonMaxSlots());
        }
    }

    public record Garrison_refresh_cooldown_sec(int value) {
        public static Garrison_refresh_cooldown_sec from(GuildWarRuleTable row) {
            return new Garrison_refresh_cooldown_sec(row.getGarrisonRefreshCooldownSec());
        }
    }

    public record Frenzy_start_round(int value) {
        public static Frenzy_start_round from(GuildWarRuleTable row) {
            return new Frenzy_start_round(row.getFrenzyStartRound());
        }
    }

    public record Bounty_normal_silver(long value) {
        public static Bounty_normal_silver from(GuildWarRuleTable row) {
            return new Bounty_normal_silver(row.getBountyNormalSilver());
        }
    }

    public record Bounty_heavy_silver(long value) {
        public static Bounty_heavy_silver from(GuildWarRuleTable row) {
            return new Bounty_heavy_silver(row.getBountyHeavySilver());
        }
    }

    public record Bounty_tax_bps(int value) {
        public static Bounty_tax_bps from(GuildWarRuleTable row) {
            return new Bounty_tax_bps(row.getBountyTaxBps());
        }
    }

    public record Bounty_public_funds(long value) {
        public static Bounty_public_funds from(GuildWarRuleTable row) {
            return new Bounty_public_funds(row.getBountyPublicFunds());
        }
    }

    public record Bounty_public_daily_limit(int value) {
        public static Bounty_public_daily_limit from(GuildWarRuleTable row) {
            return new Bounty_public_daily_limit(row.getBountyPublicDailyLimit());
        }
    }

    public record Bounty_ttl_sec(int value) {
        public static Bounty_ttl_sec from(GuildWarRuleTable row) {
            return new Bounty_ttl_sec(row.getBountyTtlSec());
        }
    }

    public record Bounty_protect_sec(int value) {
        public static Bounty_protect_sec from(GuildWarRuleTable row) {
            return new Bounty_protect_sec(row.getBountyProtectSec());
        }
    }

    public record Bounty_counter_kill_tear(int value) {
        public static Bounty_counter_kill_tear from(GuildWarRuleTable row) {
            return new Bounty_counter_kill_tear(row.getBountyCounterKillTear());
        }
    }

    public record Bounty_daily_payout_limit(int value) {
        public static Bounty_daily_payout_limit from(GuildWarRuleTable row) {
            return new Bounty_daily_payout_limit(row.getBountyDailyPayoutLimit());
        }
    }

    public record Track_cooldown_sec(int value) {
        public static Track_cooldown_sec from(GuildWarRuleTable row) {
            return new Track_cooldown_sec(row.getTrackCooldownSec());
        }
    }

    public record Diminish_window_sec(int value) {
        public static Diminish_window_sec from(GuildWarRuleTable row) {
            return new Diminish_window_sec(row.getDiminishWindowSec());
        }
    }

    public record Dividend_interval_sec(int value) {
        public static Dividend_interval_sec from(GuildWarRuleTable row) {
            return new Dividend_interval_sec(row.getDividendIntervalSec());
        }
    }

    public record Dividend_guild_weekly_cap(int value) {
        public static Dividend_guild_weekly_cap from(GuildWarRuleTable row) {
            return new Dividend_guild_weekly_cap(row.getDividendGuildWeeklyCap());
        }
    }

    public record Dividend_player_weekly_cap(int value) {
        public static Dividend_player_weekly_cap from(GuildWarRuleTable row) {
            return new Dividend_player_weekly_cap(row.getDividendPlayerWeeklyCap());
        }
    }

    public record Dividend_min_personal_merit(long value) {
        public static Dividend_min_personal_merit from(GuildWarRuleTable row) {
            return new Dividend_min_personal_merit(row.getDividendMinPersonalMerit());
        }
    }

    public record Battle_config_id(int value) {
        public static Battle_config_id from(GuildWarRuleTable row) {
            return new Battle_config_id(row.getBattleConfigId());
        }
    }

    public record Gold_leyline_multiplier_bps(int value) {
        public static Gold_leyline_multiplier_bps from(GuildWarRuleTable row) {
            return new Gold_leyline_multiplier_bps(row.getGoldLeylineMultiplierBps());
        }
    }

    public record Rivalry_bonus_bps(int value) {
        public static Rivalry_bonus_bps from(GuildWarRuleTable row) {
            return new Rivalry_bonus_bps(row.getRivalryBonusBps());
        }
    }

}