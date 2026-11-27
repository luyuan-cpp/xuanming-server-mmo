
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for JianghuRuleTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type JianghuRuleIdComp struct {
    Value uint32
}

type JianghuRuleEnabledComp struct {
    Value uint32
}

type JianghuRuleSilver_writ_costComp struct {
    Value uint64
}

type JianghuRuleGold_writ_item_idComp struct {
    Value uint32
}

type JianghuRuleInvite_ttl_secComp struct {
    Value uint32
}

type JianghuRuleTeam_confirm_secComp struct {
    Value uint32
}

type JianghuRuleSame_target_cooldown_secComp struct {
    Value uint32
}

type JianghuRuleDaily_unmatched_invite_limitComp struct {
    Value uint32
}

type JianghuRuleDuel_round_limitComp struct {
    Value uint32
}

type JianghuRuleDuel_timeout_secComp struct {
    Value uint32
}

type JianghuRuleDefeat_mark_duration_secComp struct {
    Value uint32
}

type JianghuRuleSilver_clear_costComp struct {
    Value uint64
}

type JianghuRuleClear_item_idComp struct {
    Value uint32
}

type JianghuRuleClear_badge_duration_secComp struct {
    Value uint32
}

type JianghuRuleGrudge_max_rivalsComp struct {
    Value uint32
}

type JianghuRuleGrudge_max_logs_per_rivalComp struct {
    Value uint32
}

type JianghuRuleGuild_wall_max_recordsComp struct {
    Value uint32
}

type JianghuRuleStreak_highlight_thresholdComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeJianghuRuleIdComp(row *pb.JianghuRuleTable) JianghuRuleIdComp {
    return JianghuRuleIdComp{Value: row.Id}
}

func MakeJianghuRuleEnabledComp(row *pb.JianghuRuleTable) JianghuRuleEnabledComp {
    return JianghuRuleEnabledComp{Value: row.Enabled}
}

func MakeJianghuRuleSilver_writ_costComp(row *pb.JianghuRuleTable) JianghuRuleSilver_writ_costComp {
    return JianghuRuleSilver_writ_costComp{Value: row.SilverWritCost}
}

func MakeJianghuRuleGold_writ_item_idComp(row *pb.JianghuRuleTable) JianghuRuleGold_writ_item_idComp {
    return JianghuRuleGold_writ_item_idComp{Value: row.GoldWritItemId}
}

func MakeJianghuRuleInvite_ttl_secComp(row *pb.JianghuRuleTable) JianghuRuleInvite_ttl_secComp {
    return JianghuRuleInvite_ttl_secComp{Value: row.InviteTtlSec}
}

func MakeJianghuRuleTeam_confirm_secComp(row *pb.JianghuRuleTable) JianghuRuleTeam_confirm_secComp {
    return JianghuRuleTeam_confirm_secComp{Value: row.TeamConfirmSec}
}

func MakeJianghuRuleSame_target_cooldown_secComp(row *pb.JianghuRuleTable) JianghuRuleSame_target_cooldown_secComp {
    return JianghuRuleSame_target_cooldown_secComp{Value: row.SameTargetCooldownSec}
}

func MakeJianghuRuleDaily_unmatched_invite_limitComp(row *pb.JianghuRuleTable) JianghuRuleDaily_unmatched_invite_limitComp {
    return JianghuRuleDaily_unmatched_invite_limitComp{Value: row.DailyUnmatchedInviteLimit}
}

func MakeJianghuRuleDuel_round_limitComp(row *pb.JianghuRuleTable) JianghuRuleDuel_round_limitComp {
    return JianghuRuleDuel_round_limitComp{Value: row.DuelRoundLimit}
}

func MakeJianghuRuleDuel_timeout_secComp(row *pb.JianghuRuleTable) JianghuRuleDuel_timeout_secComp {
    return JianghuRuleDuel_timeout_secComp{Value: row.DuelTimeoutSec}
}

func MakeJianghuRuleDefeat_mark_duration_secComp(row *pb.JianghuRuleTable) JianghuRuleDefeat_mark_duration_secComp {
    return JianghuRuleDefeat_mark_duration_secComp{Value: row.DefeatMarkDurationSec}
}

func MakeJianghuRuleSilver_clear_costComp(row *pb.JianghuRuleTable) JianghuRuleSilver_clear_costComp {
    return JianghuRuleSilver_clear_costComp{Value: row.SilverClearCost}
}

func MakeJianghuRuleClear_item_idComp(row *pb.JianghuRuleTable) JianghuRuleClear_item_idComp {
    return JianghuRuleClear_item_idComp{Value: row.ClearItemId}
}

func MakeJianghuRuleClear_badge_duration_secComp(row *pb.JianghuRuleTable) JianghuRuleClear_badge_duration_secComp {
    return JianghuRuleClear_badge_duration_secComp{Value: row.ClearBadgeDurationSec}
}

func MakeJianghuRuleGrudge_max_rivalsComp(row *pb.JianghuRuleTable) JianghuRuleGrudge_max_rivalsComp {
    return JianghuRuleGrudge_max_rivalsComp{Value: row.GrudgeMaxRivals}
}

func MakeJianghuRuleGrudge_max_logs_per_rivalComp(row *pb.JianghuRuleTable) JianghuRuleGrudge_max_logs_per_rivalComp {
    return JianghuRuleGrudge_max_logs_per_rivalComp{Value: row.GrudgeMaxLogsPerRival}
}

func MakeJianghuRuleGuild_wall_max_recordsComp(row *pb.JianghuRuleTable) JianghuRuleGuild_wall_max_recordsComp {
    return JianghuRuleGuild_wall_max_recordsComp{Value: row.GuildWallMaxRecords}
}

func MakeJianghuRuleStreak_highlight_thresholdComp(row *pb.JianghuRuleTable) JianghuRuleStreak_highlight_thresholdComp {
    return JianghuRuleStreak_highlight_thresholdComp{Value: row.StreakHighlightThreshold}
}

