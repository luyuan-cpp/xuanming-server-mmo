
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/tournamentrule_table.pb.h"

// ============================================================
// Per-column ECS components for TournamentRuleTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct TournamentRuleIdComp {
    uint32_t value;
};

struct TournamentRuleRule_versionComp {
    uint32_t value;
};

struct TournamentRuleEnabledComp {
    bool value;
};

struct TournamentRuleBattle_config_idComp {
    uint32_t value;
};

struct TournamentRuleBattle_team_sizeComp {
    uint32_t value;
};

struct TournamentRuleRoster_max_sizeComp {
    uint32_t value;
};

struct TournamentRuleMin_levelComp {
    uint32_t value;
};

struct TournamentRuleRegistration_daysComp {
    uint32_t value;
};

struct TournamentRuleQualifier_daysComp {
    uint32_t value;
};

struct TournamentRuleKnockout_daysComp {
    uint32_t value;
};

struct TournamentRuleSettle_display_daysComp {
    uint32_t value;
};

struct TournamentRuleQualifier_max_matchesComp {
    uint32_t value;
};

struct TournamentRuleParticipation_min_matchesComp {
    uint32_t value;
};

struct TournamentRuleKnockout_max_teamsComp {
    uint32_t value;
};

struct TournamentRuleKnockout_round_timeout_secondsComp {
    uint32_t value;
};

struct TournamentRuleQualifier_win_pointsComp {
    uint32_t value;
};

struct TournamentRuleQualifier_draw_pointsComp {
    uint32_t value;
};

struct TournamentRuleQualifier_loss_pointsComp {
    uint32_t value;
};

struct TournamentRuleInvite_ttl_secondsComp {
    uint32_t value;
};

struct TournamentRuleDescComp {
    std::string_view value;
};

struct TournamentRuleBo3_top4_enabledComp {
    bool value;
};

struct TournamentRuleIntermission_secondsComp {
    uint32_t value;
};

struct TournamentRulePublic_knockout_replayComp {
    bool value;
};

struct TournamentRuleSalute_gold_amountComp {
    uint32_t value;
};

struct TournamentRuleSalute_item_idComp {
    uint32_t value;
};

struct TournamentRuleSalute_item_countComp {
    uint32_t value;
};

struct TournamentRuleWarmup_spar_enabledComp {
    bool value;
};

struct TournamentRulePrediction_enabledComp {
    bool value;
};

struct TournamentRulePrediction_reward_min_hitsComp {
    uint32_t value;
};

struct TournamentRuleThird_place_match_enabledComp {
    bool value;
};

struct TournamentRuleLeyline_ban_enabledComp {
    bool value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline TournamentRuleIdComp MakeTournamentRuleIdComp(const TournamentRuleTable& row) {
    return { row.id() };
}
inline TournamentRuleRule_versionComp MakeTournamentRuleRule_versionComp(const TournamentRuleTable& row) {
    return { row.rule_version() };
}
inline TournamentRuleEnabledComp MakeTournamentRuleEnabledComp(const TournamentRuleTable& row) {
    return { row.enabled() };
}
inline TournamentRuleBattle_config_idComp MakeTournamentRuleBattle_config_idComp(const TournamentRuleTable& row) {
    return { row.battle_config_id() };
}
inline TournamentRuleBattle_team_sizeComp MakeTournamentRuleBattle_team_sizeComp(const TournamentRuleTable& row) {
    return { row.battle_team_size() };
}
inline TournamentRuleRoster_max_sizeComp MakeTournamentRuleRoster_max_sizeComp(const TournamentRuleTable& row) {
    return { row.roster_max_size() };
}
inline TournamentRuleMin_levelComp MakeTournamentRuleMin_levelComp(const TournamentRuleTable& row) {
    return { row.min_level() };
}
inline TournamentRuleRegistration_daysComp MakeTournamentRuleRegistration_daysComp(const TournamentRuleTable& row) {
    return { row.registration_days() };
}
inline TournamentRuleQualifier_daysComp MakeTournamentRuleQualifier_daysComp(const TournamentRuleTable& row) {
    return { row.qualifier_days() };
}
inline TournamentRuleKnockout_daysComp MakeTournamentRuleKnockout_daysComp(const TournamentRuleTable& row) {
    return { row.knockout_days() };
}
inline TournamentRuleSettle_display_daysComp MakeTournamentRuleSettle_display_daysComp(const TournamentRuleTable& row) {
    return { row.settle_display_days() };
}
inline TournamentRuleQualifier_max_matchesComp MakeTournamentRuleQualifier_max_matchesComp(const TournamentRuleTable& row) {
    return { row.qualifier_max_matches() };
}
inline TournamentRuleParticipation_min_matchesComp MakeTournamentRuleParticipation_min_matchesComp(const TournamentRuleTable& row) {
    return { row.participation_min_matches() };
}
inline TournamentRuleKnockout_max_teamsComp MakeTournamentRuleKnockout_max_teamsComp(const TournamentRuleTable& row) {
    return { row.knockout_max_teams() };
}
inline TournamentRuleKnockout_round_timeout_secondsComp MakeTournamentRuleKnockout_round_timeout_secondsComp(const TournamentRuleTable& row) {
    return { row.knockout_round_timeout_seconds() };
}
inline TournamentRuleQualifier_win_pointsComp MakeTournamentRuleQualifier_win_pointsComp(const TournamentRuleTable& row) {
    return { row.qualifier_win_points() };
}
inline TournamentRuleQualifier_draw_pointsComp MakeTournamentRuleQualifier_draw_pointsComp(const TournamentRuleTable& row) {
    return { row.qualifier_draw_points() };
}
inline TournamentRuleQualifier_loss_pointsComp MakeTournamentRuleQualifier_loss_pointsComp(const TournamentRuleTable& row) {
    return { row.qualifier_loss_points() };
}
inline TournamentRuleInvite_ttl_secondsComp MakeTournamentRuleInvite_ttl_secondsComp(const TournamentRuleTable& row) {
    return { row.invite_ttl_seconds() };
}
inline TournamentRuleDescComp MakeTournamentRuleDescComp(const TournamentRuleTable& row) {
    return { std::string_view(row.desc()) };
}
inline TournamentRuleBo3_top4_enabledComp MakeTournamentRuleBo3_top4_enabledComp(const TournamentRuleTable& row) {
    return { row.bo3_top4_enabled() };
}
inline TournamentRuleIntermission_secondsComp MakeTournamentRuleIntermission_secondsComp(const TournamentRuleTable& row) {
    return { row.intermission_seconds() };
}
inline TournamentRulePublic_knockout_replayComp MakeTournamentRulePublic_knockout_replayComp(const TournamentRuleTable& row) {
    return { row.public_knockout_replay() };
}
inline TournamentRuleSalute_gold_amountComp MakeTournamentRuleSalute_gold_amountComp(const TournamentRuleTable& row) {
    return { row.salute_gold_amount() };
}
inline TournamentRuleSalute_item_idComp MakeTournamentRuleSalute_item_idComp(const TournamentRuleTable& row) {
    return { row.salute_item_id() };
}
inline TournamentRuleSalute_item_countComp MakeTournamentRuleSalute_item_countComp(const TournamentRuleTable& row) {
    return { row.salute_item_count() };
}
inline TournamentRuleWarmup_spar_enabledComp MakeTournamentRuleWarmup_spar_enabledComp(const TournamentRuleTable& row) {
    return { row.warmup_spar_enabled() };
}
inline TournamentRulePrediction_enabledComp MakeTournamentRulePrediction_enabledComp(const TournamentRuleTable& row) {
    return { row.prediction_enabled() };
}
inline TournamentRulePrediction_reward_min_hitsComp MakeTournamentRulePrediction_reward_min_hitsComp(const TournamentRuleTable& row) {
    return { row.prediction_reward_min_hits() };
}
inline TournamentRuleThird_place_match_enabledComp MakeTournamentRuleThird_place_match_enabledComp(const TournamentR