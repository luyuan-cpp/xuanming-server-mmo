
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for GuildWarRuleTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type GuildWarRuleIdComp struct {
    Value uint32
}

type GuildWarRuleMin_levelComp struct {
    Value uint32
}

type GuildWarRulePeace_cooldown_secComp struct {
    Value uint32
}

type GuildWarRulePost_battle_protect_secComp struct {
    Value uint32
}

type GuildWarRuleLeyline_max_banner_hpComp struct {
    Value uint32
}

type GuildWarRuleLeyline_capture_init_hpComp struct {
    Value uint32
}

type GuildWarRuleLeyline_shield_secComp struct {
    Value uint32
}

type GuildWarRulePvp_win_banner_damageComp struct {
    Value uint32
}

type GuildWarRulePvp_defend_banner_healComp struct {
    Value uint32
}

type GuildWarRuleSiege_win_banner_damageComp struct {
    Value uint32
}

type GuildWarRuleGarrison_max_slotsComp struct {
    Value uint32
}

type GuildWarRuleGarrison_refresh_cooldown_secComp struct {
    Value uint32
}

type GuildWarRuleFrenzy_start_roundComp struct {
    Value uint32
}

type GuildWarRuleBounty_normal_silverComp struct {
    Value uint64
}

type GuildWarRuleBounty_heavy_silverComp struct {
    Value uint64
}

type GuildWarRuleBounty_tax_bpsComp struct {
    Value uint32
}

type GuildWarRuleBounty_public_fundsComp struct {
    Value uint64
}

type GuildWarRuleBounty_public_daily_limitComp struct {
    Value uint32
}

type GuildWarRuleBounty_ttl_secComp struct {
    Value uint32
}

type GuildWarRuleBounty_protect_secComp struct {
    Value uint32
}

type GuildWarRuleBounty_counter_kill_tearComp struct {
    Value uint32
}

type GuildWarRuleBounty_daily_payout_limitComp struct {
    Value uint32
}

type GuildWarRuleTrack_cooldown_secComp struct {
    Value uint32
}

type GuildWarRuleDiminish_window_secComp struct {
    Value uint32
}

type GuildWarRuleDividend_interval_secComp struct {
    Value uint32
}

type GuildWarRuleDividend_guild_weekly_capComp struct {
    Value uint32
}

type GuildWarRuleDividend_player_weekly_capComp struct {
    Value uint32
}

type GuildWarRuleDividend_min_personal_meritComp struct {
    Value uint64
}

type GuildWarRuleBattle_config_idComp struct {
    Value uint32
}

type GuildWarRuleGold_leyline_multiplier_bpsComp struct {
    Value uint32
}

type GuildWarRuleRivalry_bonus_bpsComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeGuildWarRuleIdComp(row *pb.GuildWarRuleTable) GuildWarRuleIdComp {
    return GuildWarRuleIdComp{Value: row.Id}
}

func MakeGuildWarRuleMin_levelComp(row *pb.GuildWarRuleTable) GuildWarRuleMin_levelComp {
    return GuildWarRuleMin_levelComp{Value: row.MinLevel}
}

func MakeGuildWarRulePeace_cooldown_secComp(row *pb.GuildWarRuleTable) GuildWarRulePeace_cooldown_secComp {
    return GuildWarRulePeace_cooldown_secComp{Value: row.PeaceCooldownSec}
}

func MakeGuildWarRulePost_battle_protect_secComp(row *pb.GuildWarRuleTable) GuildWarRulePost_battle_protect_secComp {
    return GuildWarRulePost_battle_protect_secComp{Value: row.PostBattleProtectSec}
}

func MakeGuildWarRuleLeyline_max_banner_hpComp(row *pb.GuildWarRuleTable) GuildWarRuleLeyline_max_banner_hpComp {
    return GuildWarRuleLeyline_max_banner_hpComp{Value: row.LeylineMaxBannerHp}
}

func MakeGuildWarRuleLeyline_capture_init_hpComp(row *pb.GuildWarRuleTable) GuildWarRuleLeyline_capture_init_hpComp {
    return GuildWarRuleLeyline_capture_init_hpComp{Value: row.LeylineCaptureInitHp}
}

func MakeGuildWarRuleLeyline_shield_secComp(row *pb.GuildWarRuleTable) GuildWarRuleLeyline_shield_secComp {
    return GuildWarRuleLeyline_shield_secComp{Value: row.LeylineShieldSec}
}

func MakeGuildWarRulePvp_win_banner_damageComp(row *pb.GuildWarRuleTable) GuildWarRulePvp_win_banner_damageComp {
    return GuildWarRulePvp_win_banner_damageComp{Value: row.PvpWinBannerDamage}
}

func MakeGuildWarRulePvp_defend_banner_healComp(row *pb.GuildWarRuleTable) GuildWarRulePvp_defend_banner_healComp {
    return GuildWarRulePvp_defend_banner_healComp{Value: row.PvpDefendBannerHeal}
}

func MakeGuildWarRuleSiege_win_banner_damageComp(row *pb.GuildWarRuleTable) GuildWarRuleSiege_win_banner_damageComp {
    return GuildWarRuleSiege_win_banner_damageComp{Value: row.SiegeWinBannerDamage}
}

func MakeGuildWarRuleGarrison_max_slotsComp(row *pb.GuildWarRuleTable) GuildWarRuleGarrison_max_slotsComp {
    return GuildWarRuleGarrison_max_slotsComp{Value: row.GarrisonMaxSlots}
}

func MakeGuildWarRuleGarrison_refresh_cooldown_secComp(row *pb.GuildWarRuleTable) GuildWarRuleGarrison_refresh_cooldown_secComp {
    return GuildWarRuleGarrison_refresh_cooldown_secComp{Value: row.GarrisonRefreshCooldownSec}
}

func MakeGuildWarRuleFrenzy_start_roundComp(row *pb.GuildWarRuleTable) GuildWarRuleFrenzy_start_roundComp {
    return GuildWarRuleFrenzy_start_roundComp{Value: row.FrenzyStartRound}
}

func MakeGuildWarRuleBounty_normal_silverComp(row *pb.GuildWarRuleTable) GuildWarRuleBounty_normal_silverComp {
    return GuildWarRuleBounty_normal_silverComp{Value: row.BountyNormalSilver}
}

func MakeGuildWarRuleBounty_heavy_silverComp(row *pb.GuildWarRuleTable) GuildWarRuleBounty_heavy_silverComp {
    return GuildWarRuleBounty_heavy_silverComp{Value: row.BountyHeavySilver}
}

func MakeGuildWarRuleBounty_tax_bpsComp(row *pb.GuildWarRuleTable) GuildWarRuleBounty_tax_bpsComp {
    return GuildWarRuleBounty_tax_bpsComp{Value: row.BountyTaxBps}
}

func MakeGuildWarRuleBounty_public_fundsComp(row *pb.GuildWarRuleTable) GuildWarRuleBounty_public_fundsComp {
    return GuildWarRuleBounty_public_fundsComp{Value: row.BountyPublicFunds}
}

func MakeGuildWarRuleBounty_public_daily_limitComp(row *pb.GuildWarRuleTable) GuildWarRuleBounty_public_daily_limitComp {
    return GuildWarRuleBounty_public_daily_limitComp{Value: row.BountyPublicDailyLimit}
}

func MakeGuildWarRuleBounty_ttl_secComp(row *pb.GuildWarRuleTable) GuildWarRuleBounty_ttl_secComp {
    return GuildWarRuleBounty_ttl_secComp{Value: row.BountyTtlSec}
}

func MakeGuildWarRuleBounty_protect_secComp(row *pb.GuildWarRuleTable) GuildWarRuleBounty_protect_secComp {
    return GuildWarRuleBounty_protect_secComp{Value: row.BountyProtectSec}
}

func MakeGuildWarRuleBounty_counter_kill_tearComp(row *pb.GuildWarRuleTable) GuildWarRuleBounty_counter_kill_tearComp {
    return GuildWarRuleBounty_counter_kill_tearComp{Value: row.BountyCounterKillTear}
}

func MakeGuildWarRuleBounty_daily_payout_limitComp(row *pb.GuildWarRuleTable) GuildWarRuleBounty_daily_payout_limitComp {
    return GuildWarRuleBounty_daily_payout_limitComp{Value: row.BountyDailyPayoutLimit}
}

func MakeGuildWarRuleTrack_cooldown_secComp(row *pb.GuildWarRuleTable) GuildWarRuleTrack_cooldown_secComp {
    return GuildWarRuleTrack_cooldown_secComp{Value: row.TrackCooldownSec}
}

func MakeGuildWarRuleDiminish_window_secComp(row *pb.GuildWarRuleTable) GuildWarRuleDiminish_window_secComp {
    return GuildWarRuleDiminish_window_secComp{Value: row.DiminishWindowSec}
}

func MakeGuildWarRuleDividend_interval_secComp(row *pb.GuildWarRuleTable) GuildWarRuleDividend_interval_secComp {
    return GuildWarRuleDividend_interval_secComp{Value: row.DividendIntervalSec}
}

func MakeGuildWarRuleDividend_guild_weekly_capComp(row *pb.GuildWarRuleTable) GuildWarRuleDividend_guild_weekly_capComp {
    return GuildWarRuleDividend_guild_weekly_capComp{Value: row.DividendGuildWeeklyCap}
}

func MakeGuildWarRuleDividend_player_weekly_capComp(row *pb.GuildWarRuleTable) GuildWarRuleDividend_player_weekly_capComp {
    return GuildWarRuleDividend_player_weekly_capComp{Value: row.DividendPlayerWeeklyCap}
}

func MakeGuildWarRuleDividend_min_personal_meritComp(row *pb.GuildWarRuleTable) GuildWarRuleDividend_min_personal_meritComp {
    return GuildWarRuleDividend_min_personal_meritComp{Value: row.DividendMinPersonalMerit}
}

func MakeGuildWarRuleBattle_config_idComp(row *pb.GuildWarRuleTable) GuildWarRuleBattle_config_idComp {
    return GuildWarRuleBattle_config_idComp{Value: row.BattleConfigId}
}

func MakeGuildWarRuleGold_leyline_multiplier_bpsComp(row *pb.GuildWarRuleTable) GuildWarRuleGold_leyline_multiplier_bpsComp {
    return GuildWarRuleGold_leyline_multiplier_bpsComp{Value: row.GoldLeylineMultiplierBps}
}

func MakeGuildWarRuleRivalry_bonus_bpsComp(row *pb.GuildWarRuleTable) GuildWarRuleRivalry_bonus_bpsComp {
    return GuildWarRuleRivalry_bonus_bpsComp{Value: row.RivalryBonusBps}
}

