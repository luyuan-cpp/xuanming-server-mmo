
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for TournamentRuleTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type TournamentRuleIdComp struct {
    Value uint32
}

type TournamentRuleRule_versionComp struct {
    Value uint32
}

type TournamentRuleEnabledComp struct {
    Value bool
}

type TournamentRuleBattle_config_idComp struct {
    Value uint32
}

type TournamentRuleBattle_team_sizeComp struct {
    Value uint32
}

type TournamentRuleRoster_max_sizeComp struct {
    Value uint32
}

type TournamentRuleMin_levelComp struct {
    Value uint32
}

type TournamentRuleRegistration_daysComp struct {
    Value uint32
}

type TournamentRuleQualifier_daysComp struct {
    Value uint32
}

type TournamentRuleKnockout_daysComp struct {
    Value uint32
}

type TournamentRuleSettle_display_daysComp struct {
    Value uint32
}

type TournamentRuleQualifier_max_matchesComp struct {
    Value uint32
}

type TournamentRuleParticipation_min_matchesComp struct {
    Value uint32
}

type TournamentRuleKnockout_max_teamsComp struct {
    Value uint32
}

type TournamentRuleKnockout_round_timeout_secondsComp struct {
    Value uint32
}

type TournamentRuleQualifier_win_pointsComp struct {
    Value uint32
}

type TournamentRuleQualifier_draw_pointsComp struct {
    Value uint32
}

type TournamentRuleQualifier_loss_pointsComp struct {
    Value uint32
}

type TournamentRuleInvite_ttl_secondsComp struct {
    Value uint32
}

type TournamentRuleDescComp struct {
    Value string
}

type TournamentRuleBo3_top4_enabledComp struct {
    Value bool
}

type TournamentRuleIntermission_secondsComp struct {
    Value uint32
}

type TournamentRulePublic_knockout_replayComp struct {
    Value bool
}

type TournamentRuleSalute_gold_amountComp struct {
    Value uint32
}

type TournamentRuleSalute_item_idComp struct {
    Value uint32
}

type TournamentRuleSalute_item_countComp struct {
    Value uint32
}

type TournamentRuleWarmup_spar_enabledComp struct {
    Value bool
}

type TournamentRulePrediction_enabledComp struct {
    Value bool
}

type TournamentRulePrediction_reward_min_hitsComp struct {
    Value uint32
}

type TournamentRuleThird_place_match_enabledComp struct {
    Value bool
}

type TournamentRuleLeyline_ban_enabledComp struct {
    Value bool
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeTournamentRuleIdComp(row *pb.TournamentRuleTable) TournamentRuleIdComp {
    return TournamentRuleIdComp{Value: row.Id}
}

func MakeTournamentRuleRule_versionComp(row *pb.TournamentRuleTable) TournamentRuleRule_versionComp {
    return TournamentRuleRule_versionComp{Value: row.RuleVersion}
}

func MakeTournamentRuleEnabledComp(row *pb.TournamentRuleTable) TournamentRuleEnabledComp {
    return TournamentRuleEnabledComp{Value: row.Enabled}
}

func MakeTournamentRuleBattle_config_idComp(row *pb.TournamentRuleTable) TournamentRuleBattle_config_idComp {
    return TournamentRuleBattle_config_idComp{Value: row.BattleConfigId}
}

func MakeTournamentRuleBattle_team_sizeComp(row *pb.TournamentRuleTable) TournamentRuleBattle_team_sizeComp {
    return TournamentRuleBattle_team_sizeComp{Value: row.BattleTeamSize}
}

func MakeTournamentRuleRoster_max_sizeComp(row *pb.TournamentRuleTable) TournamentRuleRoster_max_sizeComp {
    return TournamentRuleRoster_max_sizeComp{Value: row.RosterMaxSize}
}

func MakeTournamentRuleMin_levelComp(row *pb.TournamentRuleTable) TournamentRuleMin_levelComp {
    return TournamentRuleMin_levelComp{Value: row.MinLevel}
}

func MakeTournamentRuleRegistration_daysComp(row *pb.TournamentRuleTable) TournamentRuleRegistration_daysComp {
    return TournamentRuleRegistration_daysComp{Value: row.RegistrationDays}
}

func MakeTournamentRuleQualifier_daysComp(row *pb.TournamentRuleTable) TournamentRuleQualifier_daysComp {
    return TournamentRuleQualifier_daysComp{Value: row.QualifierDays}
}

func MakeTournamentRuleKnockout_daysComp(row *pb.TournamentRuleTable) TournamentRuleKnockout_daysComp {
    return TournamentRuleKnockout_daysComp{Value: row.KnockoutDays}
}

func MakeTournamentRuleSettle_display_daysComp(row *pb.TournamentRuleTable) TournamentRuleSettle_display_daysComp {
    return TournamentRuleSettle_display_daysComp{Value: row.SettleDisplayDays}
}

func MakeTournamentRuleQualifier_max_matchesComp(row *pb.TournamentRuleTable) TournamentRuleQualifier_max_matchesComp {
    return TournamentRuleQualifier_max_matchesComp{Value: row.QualifierMaxMatches}
}

func MakeTournamentRuleParticipation_min_matchesComp(row *pb.TournamentRuleTable) TournamentRuleParticipation_min_matchesComp {
    return TournamentRuleParticipation_min_matchesComp{Value: row.ParticipationMinMatches}
}

func MakeTournamentRuleKnockout_max_teamsComp(row *pb.TournamentRuleTable) TournamentRuleKnockout_max_teamsComp {
    return TournamentRuleKnockout_max_teamsComp{Value: row.KnockoutMaxTeams}
}

func MakeTournamentRuleKnockout_round_timeout_secondsComp(row *pb.TournamentRuleTable) TournamentRuleKnockout_round_timeout_secondsComp {
    return TournamentRuleKnockout_round_timeout_secondsComp{Value: row.KnockoutRoundTimeoutSeconds}
}

func MakeTournamentRuleQualifier_win_pointsComp(row *pb.TournamentRuleTable) TournamentRuleQualifier_win_pointsComp {
    return TournamentRuleQualifier_win_pointsComp{Value: row.QualifierWinPoints}
}

func MakeTournamentRuleQualifier_draw_pointsComp(row *pb.TournamentRuleTable) TournamentRuleQualifier_draw_pointsComp {
    return TournamentRuleQualifier_draw_pointsComp{Value: row.QualifierDrawPoints}
}

func MakeTournamentRuleQualifier_loss_pointsComp(row *pb.TournamentRuleTable) TournamentRuleQualifier_loss_pointsComp {
    return TournamentRuleQualifier_loss_pointsComp{Value: row.QualifierLossPoints}
}

func MakeTournamentRuleInvite_ttl_secondsComp(row *pb.TournamentRuleTable) TournamentRuleInvite_ttl_secondsComp {
    return TournamentRuleInvite_ttl_secondsComp{Value: row.InviteTtlSeconds}
}

func MakeTournamentRuleDescComp(row *pb.TournamentRuleTable) TournamentRuleDescComp {
    return TournamentRuleDescComp{Value: row.Desc}
}

func MakeTournamentRuleBo3_top4_enabledComp(row *pb.TournamentRuleTable) TournamentRuleBo3_top4_enabledComp {
    return TournamentRuleBo3_top4_enabledComp{Value: row.Bo3Top4Enabled}
}

func MakeTournamentRuleIntermission_secondsComp(row *pb.TournamentRuleTable) TournamentRuleIntermission_secondsComp {
    return TournamentRuleIntermission_secondsComp{Value: row.IntermissionSeconds}
}

func MakeTournamentRulePublic_knockout_replayComp(row *pb.TournamentRuleTable) TournamentRulePublic_knockout_replayComp {
    return TournamentRulePublic_knockout_replayComp{Value: row.PublicKnockoutReplay}
}

func MakeTournamentRuleSalute_gold_amountComp(row *pb.TournamentRuleTable) TournamentRuleSalute_gold_amountComp {
    return TournamentRuleSalute_gold_amountComp{Value: row.SaluteGoldAmount}
}

func MakeTournamentRuleSalute_item_idComp(row *pb.TournamentRuleTable) TournamentRuleSalute_item_idComp {
    return TournamentRuleSalute_item_idComp{Value: row.SaluteItemId}
}

func MakeTournamentRuleSalute_item_countComp(row *pb.TournamentRuleTable) TournamentRuleSalute_item_countComp {
    return TournamentRuleSalute_item_countComp{Value: row.SaluteItemCount}
}

func MakeTournamentRuleWarmup_spar_enabledComp(row *pb.TournamentRuleTable) TournamentRuleWarmup_spar_enabledComp {
    return TournamentRuleWarmup_spar_enabledComp{Value: row.WarmupSparEnabled}
}

func MakeTournamentRulePrediction_enabledComp(row *pb.TournamentRuleTable) TournamentRulePrediction_enabledComp {
    return TournamentRulePrediction_enabledComp{Value: row.PredictionEnabled}
}

func MakeTournamentRulePrediction_reward_min_hitsComp(row *pb.TournamentRuleTable) TournamentRulePrediction_reward_min_hitsComp {
    return TournamentRulePrediction_reward_min_hitsComp{Value: row.PredictionRewardMinHits}
}

func MakeTournamentRuleThird_place_match_enabledComp(row *pb.TournamentRuleTable) TournamentRuleThird_place_match_enabledComp {
    return TournamentRu