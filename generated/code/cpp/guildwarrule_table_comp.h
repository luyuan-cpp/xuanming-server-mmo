
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/guildwarrule_table.pb.h"

// ============================================================
// Per-column ECS components for GuildWarRuleTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct GuildWarRuleIdComp {
    uint32_t value;
};

struct GuildWarRuleMin_levelComp {
    uint32_t value;
};

struct GuildWarRulePeace_cooldown_secComp {
    uint32_t value;
};

struct GuildWarRulePost_battle_protect_secComp {
    uint32_t value;
};

struct GuildWarRuleLeyline_max_banner_hpComp {
    uint32_t value;
};

struct GuildWarRuleLeyline_capture_init_hpComp {
    uint32_t value;
};

struct GuildWarRuleLeyline_shield_secComp {
    uint32_t value;
};

struct GuildWarRulePvp_win_banner_damageComp {
    uint32_t value;
};

struct GuildWarRulePvp_defend_banner_healComp {
    uint32_t value;
};

struct GuildWarRuleSiege_win_banner_damageComp {
    uint32_t value;
};

struct GuildWarRuleGarrison_max_slotsComp {
    uint32_t value;
};

struct GuildWarRuleGarrison_refresh_cooldown_secComp {
    uint32_t value;
};

struct GuildWarRuleFrenzy_start_roundComp {
    uint32_t value;
};

struct GuildWarRuleBounty_normal_silverComp {
    uint64_t value;
};

struct GuildWarRuleBounty_heavy_silverComp {
    uint64_t value;
};

struct GuildWarRuleBounty_tax_bpsComp {
    uint32_t value;
};

struct GuildWarRuleBounty_public_fundsComp {
    uint64_t value;
};

struct GuildWarRuleBounty_public_daily_limitComp {
    uint32_t value;
};

struct GuildWarRuleBounty_ttl_secComp {
    uint32_t value;
};

struct GuildWarRuleBounty_protect_secComp {
    uint32_t value;
};

struct GuildWarRuleBounty_counter_kill_tearComp {
    uint32_t value;
};

struct GuildWarRuleBounty_daily_payout_limitComp {
    uint32_t value;
};

struct GuildWarRuleTrack_cooldown_secComp {
    uint32_t value;
};

struct GuildWarRuleDiminish_window_secComp {
    uint32_t value;
};

struct GuildWarRuleDividend_interval_secComp {
    uint32_t value;
};

struct GuildWarRuleDividend_guild_weekly_capComp {
    uint32_t value;
};

struct GuildWarRuleDividend_player_weekly_capComp {
    uint32_t value;
};

struct GuildWarRuleDividend_min_personal_meritComp {
    uint64_t value;
};

struct GuildWarRuleBattle_config_idComp {
    uint32_t value;
};

struct GuildWarRuleGold_leyline_multiplier_bpsComp {
    uint32_t value;
};

struct GuildWarRuleRivalry_bonus_bpsComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline GuildWarRuleIdComp MakeGuildWarRuleIdComp(const GuildWarRuleTable& row) {
    return { row.id() };
}
inline GuildWarRuleMin_levelComp MakeGuildWarRuleMin_levelComp(const GuildWarRuleTable& row) {
    return { row.min_level() };
}
inline GuildWarRulePeace_cooldown_secComp MakeGuildWarRulePeace_cooldown_secComp(const GuildWarRuleTable& row) {
    return { row.peace_cooldown_sec() };
}
inline GuildWarRulePost_battle_protect_secComp MakeGuildWarRulePost_battle_protect_secComp(const GuildWarRuleTable& row) {
    return { row.post_battle_protect_sec() };
}
inline GuildWarRuleLeyline_max_banner_hpComp MakeGuildWarRuleLeyline_max_banner_hpComp(const GuildWarRuleTable& row) {
    return { row.leyline_max_banner_hp() };
}
inline GuildWarRuleLeyline_capture_init_hpComp MakeGuildWarRuleLeyline_capture_init_hpComp(const GuildWarRuleTable& row) {
    return { row.leyline_capture_init_hp() };
}
inline GuildWarRuleLeyline_shield_secComp MakeGuildWarRuleLeyline_shield_secComp(const GuildWarRuleTable& row) {
    return { row.leyline_shield_sec() };
}
inline GuildWarRulePvp_win_banner_damageComp MakeGuildWarRulePvp_win_banner_damageComp(const GuildWarRuleTable& row) {
    return { row.pvp_win_banner_damage() };
}
inline GuildWarRulePvp_defend_banner_healComp MakeGuildWarRulePvp_defend_banner_healComp(const GuildWarRuleTable& row) {
    return { row.pvp_defend_banner_heal() };
}
inline GuildWarRuleSiege_win_banner_damageComp MakeGuildWarRuleSiege_win_banner_damageComp(const GuildWarRuleTable& row) {
    return { row.siege_win_banner_damage() };
}
inline GuildWarRuleGarrison_max_slotsComp MakeGuildWarRuleGarrison_max_slotsComp(const GuildWarRuleTable& row) {
    return { row.garrison_max_slots() };
}
inline GuildWarRuleGarrison_refresh_cooldown_secComp MakeGuildWarRuleGarrison_refresh_cooldown_secComp(const GuildWarRuleTable& row) {
    return { row.garrison_refresh_cooldown_sec() };
}
inline GuildWarRuleFrenzy_start_roundComp MakeGuildWarRuleFrenzy_start_roundComp(const GuildWarRuleTable& row) {
    return { row.frenzy_start_round() };
}
inline GuildWarRuleBounty_normal_silverComp MakeGuildWarRuleBounty_normal_silverComp(const GuildWarRuleTable& row) {
    return { row.bounty_normal_silver() };
}
inline GuildWarRuleBounty_heavy_silverComp MakeGuildWarRuleBounty_heavy_silverComp(const GuildWarRuleTable& row) {
    return { row.bounty_heavy_silver() };
}
inline GuildWarRuleBounty_tax_bpsComp MakeGuildWarRuleBounty_tax_bpsComp(const GuildWarRuleTable& row) {
    return { row.bounty_tax_bps() };
}
inline GuildWarRuleBounty_public_fundsComp MakeGuildWarRuleBounty_public_fundsComp(const GuildWarRuleTable& row) {
    return { row.bounty_public_funds() };
}
inline GuildWarRuleBounty_public_daily_limitComp MakeGuildWarRuleBounty_public_daily_limitComp(const GuildWarRuleTable& row) {
    return { row.bounty_public_daily_limit() };
}
inline GuildWarRuleBounty_ttl_secComp MakeGuildWarRuleBounty_ttl_secComp(const GuildWarRuleTable& row) {
    return { row.bounty_ttl_sec() };
}
inline GuildWarRuleBounty_protect_secComp MakeGuildWarRuleBounty_protect_secComp(const GuildWarRuleTable& row) {
    return { row.bounty_protect_sec() };
}
inline GuildWarRuleBounty_counter_kill_tearComp MakeGuildWarRuleBounty_counter_kill_tearComp(const GuildWarRuleTable& row) {
    return { row.bounty_counter_kill_tear() };
}
inline GuildWarRuleBounty_daily_payout_limitComp MakeGuildWarRuleBounty_daily_payout_limitComp(const GuildWarRuleTable& row) {
    return { row.bounty_daily_payout_limit() };
}
inline GuildWarRuleTrack_cooldown_secComp MakeGuildWarRuleTrack_cooldown_secComp(const GuildWarRuleTable& row) {
    return { row.track_cooldown_sec() };
}
inline GuildWarRuleDiminish_window_secComp MakeGuildWarRuleDiminish_window_secComp(const GuildWarRuleTable& row) {
    return { row.diminish_window_sec() };
}
inline GuildWarRuleDividend_interval_secComp MakeGuildWarRuleDividend_interval_secComp(const GuildWarRuleTable& row) {
    return { row.dividend_interval_sec() };
}
inline GuildWarRuleDividend_guild_weekly_capComp MakeGuildWarRuleDividend_guild_weekly_capComp(const GuildWarRuleTable& row) {
    return { row.dividend_guild_weekly_cap() };
}
inline GuildWarRuleDividend_player_weekly_capComp MakeGuildWarRuleDividend_player_weekly_capComp(const GuildWarRuleTable& row) {
    return { row.dividend_player_weekly_cap() };
}
inline GuildWarRuleDividend_min_personal_meritComp MakeGuildWarRuleDividend_min_personal_meritComp(const GuildWarRuleTable& row) {
    return { row.dividend_min_personal_merit() };
}
inline GuildWarRuleBattle_config_idComp MakeGuildWarRuleBattle_config_idComp(const GuildWarRuleTable& row) {
    return { row.battle_config_id() };
}
inline GuildWarRuleGold_leyline_multiplier_bpsComp MakeGuildWarRuleGold_leyline_multiplier_bpsComp(const GuildWarRuleTable& row) {
    return { row.gold_leyline_multiplier_bps() };
}
inline GuildWarRuleRivalry_bonus_bpsComp MakeGuildWarRuleRivalry_bonus_bpsComp(const GuildWarRuleTable& row) {
    return { row.rivalry_bonus_bps() };
}
