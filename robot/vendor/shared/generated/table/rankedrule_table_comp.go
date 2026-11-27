
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for RankedRuleTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type RankedRuleIdComp struct {
    Value uint32
}

type RankedRuleRule_versionComp struct {
    Value uint32
}

type RankedRuleBattle_config_idComp struct {
    Value uint32
}

type RankedRuleReference_class_idComp struct {
    Value uint32
}

type RankedRuleCompetitive_levelComp struct {
    Value uint32
}

type RankedRuleDefault_character_preset_idComp struct {
    Value uint32
}

type RankedRuleDefault_pet_preset_idComp struct {
    Value uint32
}

type RankedRuleSeason_duration_daysComp struct {
    Value uint32
}

type RankedRuleWait_prompt_secondsComp struct {
    Value uint32
}

type RankedRuleSpread_tier1_secondsComp struct {
    Value uint32
}

type RankedRuleSpread_tier1_pointsComp struct {
    Value uint32
}

type RankedRuleSpread_tier2_secondsComp struct {
    Value uint32
}

type RankedRuleSpread_tier2_pointsComp struct {
    Value uint32
}

type RankedRuleSpread_tier3_pointsComp struct {
    Value uint32
}

type RankedRuleMilestone_avatar_matchesComp struct {
    Value uint32
}

type RankedRuleMilestone_card_matchesComp struct {
    Value uint32
}

type RankedRuleMilestone_talisman_matchesComp struct {
    Value uint32
}

type RankedRuleTier_qingfeng_centiComp struct {
    Value uint32
}

type RankedRuleTier_yunzhang_centiComp struct {
    Value uint32
}

type RankedRuleTier_tianshu_centiComp struct {
    Value uint32
}

type RankedRuleTier_wushuang_centiComp struct {
    Value uint32
}

type RankedRuleDescComp struct {
    Value string
}

type RankedRuleMilestone_win_matchesComp struct {
    Value uint32
}

type RankedRuleSkill_ban_slots_per_teamComp struct {
    Value uint32
}

type RankedRuleSkill_ban_min_rating_centiComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeRankedRuleIdComp(row *pb.RankedRuleTable) RankedRuleIdComp {
    return RankedRuleIdComp{Value: row.Id}
}

func MakeRankedRuleRule_versionComp(row *pb.RankedRuleTable) RankedRuleRule_versionComp {
    return RankedRuleRule_versionComp{Value: row.RuleVersion}
}

func MakeRankedRuleBattle_config_idComp(row *pb.RankedRuleTable) RankedRuleBattle_config_idComp {
    return RankedRuleBattle_config_idComp{Value: row.BattleConfigId}
}

func MakeRankedRuleReference_class_idComp(row *pb.RankedRuleTable) RankedRuleReference_class_idComp {
    return RankedRuleReference_class_idComp{Value: row.ReferenceClassId}
}

func MakeRankedRuleCompetitive_levelComp(row *pb.RankedRuleTable) RankedRuleCompetitive_levelComp {
    return RankedRuleCompetitive_levelComp{Value: row.CompetitiveLevel}
}

func MakeRankedRuleDefault_character_preset_idComp(row *pb.RankedRuleTable) RankedRuleDefault_character_preset_idComp {
    return RankedRuleDefault_character_preset_idComp{Value: row.DefaultCharacterPresetId}
}

func MakeRankedRuleDefault_pet_preset_idComp(row *pb.RankedRuleTable) RankedRuleDefault_pet_preset_idComp {
    return RankedRuleDefault_pet_preset_idComp{Value: row.DefaultPetPresetId}
}

func MakeRankedRuleSeason_duration_daysComp(row *pb.RankedRuleTable) RankedRuleSeason_duration_daysComp {
    return RankedRuleSeason_duration_daysComp{Value: row.SeasonDurationDays}
}

func MakeRankedRuleWait_prompt_secondsComp(row *pb.RankedRuleTable) RankedRuleWait_prompt_secondsComp {
    return RankedRuleWait_prompt_secondsComp{Value: row.WaitPromptSeconds}
}

func MakeRankedRuleSpread_tier1_secondsComp(row *pb.RankedRuleTable) RankedRuleSpread_tier1_secondsComp {
    return RankedRuleSpread_tier1_secondsComp{Value: row.SpreadTier1Seconds}
}

func MakeRankedRuleSpread_tier1_pointsComp(row *pb.RankedRuleTable) RankedRuleSpread_tier1_pointsComp {
    return RankedRuleSpread_tier1_pointsComp{Value: row.SpreadTier1Points}
}

func MakeRankedRuleSpread_tier2_secondsComp(row *pb.RankedRuleTable) RankedRuleSpread_tier2_secondsComp {
    return RankedRuleSpread_tier2_secondsComp{Value: row.SpreadTier2Seconds}
}

func MakeRankedRuleSpread_tier2_pointsComp(row *pb.RankedRuleTable) RankedRuleSpread_tier2_pointsComp {
    return RankedRuleSpread_tier2_pointsComp{Value: row.SpreadTier2Points}
}

func MakeRankedRuleSpread_tier3_pointsComp(row *pb.RankedRuleTable) RankedRuleSpread_tier3_pointsComp {
    return RankedRuleSpread_tier3_pointsComp{Value: row.SpreadTier3Points}
}

func MakeRankedRuleMilestone_avatar_matchesComp(row *pb.RankedRuleTable) RankedRuleMilestone_avatar_matchesComp {
    return RankedRuleMilestone_avatar_matchesComp{Value: row.MilestoneAvatarMatches}
}

func MakeRankedRuleMilestone_card_matchesComp(row *pb.RankedRuleTable) RankedRuleMilestone_card_matchesComp {
    return RankedRuleMilestone_card_matchesComp{Value: row.MilestoneCardMatches}
}

func MakeRankedRuleMilestone_talisman_matchesComp(row *pb.RankedRuleTable) RankedRuleMilestone_talisman_matchesComp {
    return RankedRuleMilestone_talisman_matchesComp{Value: row.MilestoneTalismanMatches}
}

func MakeRankedRuleTier_qingfeng_centiComp(row *pb.RankedRuleTable) RankedRuleTier_qingfeng_centiComp {
    return RankedRuleTier_qingfeng_centiComp{Value: row.TierQingfengCenti}
}

func MakeRankedRuleTier_yunzhang_centiComp(row *pb.RankedRuleTable) RankedRuleTier_yunzhang_centiComp {
    return RankedRuleTier_yunzhang_centiComp{Value: row.TierYunzhangCenti}
}

func MakeRankedRuleTier_tianshu_centiComp(row *pb.RankedRuleTable) RankedRuleTier_tianshu_centiComp {
    return RankedRuleTier_tianshu_centiComp{Value: row.TierTianshuCenti}
}

func MakeRankedRuleTier_wushuang_centiComp(row *pb.RankedRuleTable) RankedRuleTier_wushuang_centiComp {
    return RankedRuleTier_wushuang_centiComp{Value: row.TierWushuangCenti}
}

func MakeRankedRuleDescComp(row *pb.RankedRuleTable) RankedRuleDescComp {
    return RankedRuleDescComp{Value: row.Desc}
}

func MakeRankedRuleMilestone_win_matchesComp(row *pb.RankedRuleTable) RankedRuleMilestone_win_matchesComp {
    return RankedRuleMilestone_win_matchesComp{Value: row.MilestoneWinMatches}
}

func MakeRankedRuleSkill_ban_slots_per_teamComp(row *pb.RankedRuleTable) RankedRuleSkill_ban_slots_per_teamComp {
    return RankedRuleSkill_ban_slots_per_teamComp{Value: row.SkillBanSlotsPerTeam}
}

func MakeRankedRuleSkill_ban_min_rating_centiComp(row *pb.RankedRuleTable) RankedRuleSkill_ban_min_rating_centiComp {
    return RankedRuleSkill_ban_min_rating_centiComp{Value: row.SkillBanMinRatingCenti}
}

