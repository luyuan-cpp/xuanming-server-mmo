
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for FestivalBeastActivityTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type FestivalBeastActivityIdComp struct {
    Value uint32
}

type FestivalBeastActivityEnabledComp struct {
    Value bool
}

type FestivalBeastActivityConfig_revisionComp struct {
    Value uint32
}

type FestivalBeastActivityActivity_nameComp struct {
    Value string
}

type FestivalBeastActivityBoss_nameComp struct {
    Value string
}

type FestivalBeastActivityBanner_asset_keyComp struct {
    Value string
}

type FestivalBeastActivityIcon_keyComp struct {
    Value string
}

type FestivalBeastActivityEffective_start_at_msComp struct {
    Value uint64
}

type FestivalBeastActivityEffective_end_at_msComp struct {
    Value uint64
}

type FestivalBeastActivityClaim_showcase_daysComp struct {
    Value uint32
}

type FestivalBeastActivityBattle_config_idComp struct {
    Value uint32
}

type FestivalBeastActivityRequired_winsComp struct {
    Value uint32
}

type FestivalBeastActivityReward_identityComp struct {
    Value string
}

type FestivalBeastActivityReward_definition_versionComp struct {
    Value uint32
}

type FestivalBeastActivityReward_pet_table_idComp struct {
    Value uint32
}

type FestivalBeastActivityRound_limitComp struct {
    Value uint32
}

type FestivalBeastActivityCycle_roundsComp struct {
    Value uint32
}

type FestivalBeastActivityFirst_heavy_roundComp struct {
    Value uint32
}

type FestivalBeastActivityTelegraph_lead_roundsComp struct {
    Value uint32
}

type FestivalBeastActivityHeavy_damage_percentComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeFestivalBeastActivityIdComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityIdComp {
    return FestivalBeastActivityIdComp{Value: row.Id}
}

func MakeFestivalBeastActivityEnabledComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityEnabledComp {
    return FestivalBeastActivityEnabledComp{Value: row.Enabled}
}

func MakeFestivalBeastActivityConfig_revisionComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityConfig_revisionComp {
    return FestivalBeastActivityConfig_revisionComp{Value: row.ConfigRevision}
}

func MakeFestivalBeastActivityActivity_nameComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityActivity_nameComp {
    return FestivalBeastActivityActivity_nameComp{Value: row.ActivityName}
}

func MakeFestivalBeastActivityBoss_nameComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityBoss_nameComp {
    return FestivalBeastActivityBoss_nameComp{Value: row.BossName}
}

func MakeFestivalBeastActivityBanner_asset_keyComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityBanner_asset_keyComp {
    return FestivalBeastActivityBanner_asset_keyComp{Value: row.BannerAssetKey}
}

func MakeFestivalBeastActivityIcon_keyComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityIcon_keyComp {
    return FestivalBeastActivityIcon_keyComp{Value: row.IconKey}
}

func MakeFestivalBeastActivityEffective_start_at_msComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityEffective_start_at_msComp {
    return FestivalBeastActivityEffective_start_at_msComp{Value: row.EffectiveStartAtMs}
}

func MakeFestivalBeastActivityEffective_end_at_msComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityEffective_end_at_msComp {
    return FestivalBeastActivityEffective_end_at_msComp{Value: row.EffectiveEndAtMs}
}

func MakeFestivalBeastActivityClaim_showcase_daysComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityClaim_showcase_daysComp {
    return FestivalBeastActivityClaim_showcase_daysComp{Value: row.ClaimShowcaseDays}
}

func MakeFestivalBeastActivityBattle_config_idComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityBattle_config_idComp {
    return FestivalBeastActivityBattle_config_idComp{Value: row.BattleConfigId}
}

func MakeFestivalBeastActivityRequired_winsComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityRequired_winsComp {
    return FestivalBeastActivityRequired_winsComp{Value: row.RequiredWins}
}

func MakeFestivalBeastActivityReward_identityComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityReward_identityComp {
    return FestivalBeastActivityReward_identityComp{Value: row.RewardIdentity}
}

func MakeFestivalBeastActivityReward_definition_versionComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityReward_definition_versionComp {
    return FestivalBeastActivityReward_definition_versionComp{Value: row.RewardDefinitionVersion}
}

func MakeFestivalBeastActivityReward_pet_table_idComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityReward_pet_table_idComp {
    return FestivalBeastActivityReward_pet_table_idComp{Value: row.RewardPetTableId}
}

func MakeFestivalBeastActivityRound_limitComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityRound_limitComp {
    return FestivalBeastActivityRound_limitComp{Value: row.RoundLimit}
}

func MakeFestivalBeastActivityCycle_roundsComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityCycle_roundsComp {
    return FestivalBeastActivityCycle_roundsComp{Value: row.CycleRounds}
}

func MakeFestivalBeastActivityFirst_heavy_roundComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityFirst_heavy_roundComp {
    return FestivalBeastActivityFirst_heavy_roundComp{Value: row.FirstHeavyRound}
}

func MakeFestivalBeastActivityTelegraph_lead_roundsComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityTelegraph_lead_roundsComp {
    return FestivalBeastActivityTelegraph_lead_roundsComp{Value: row.TelegraphLeadRounds}
}

func MakeFestivalBeastActivityHeavy_damage_percentComp(row *pb.FestivalBeastActivityTable) FestivalBeastActivityHeavy_damage_percentComp {
    return FestivalBeastActivityHeavy_damage_percentComp{Value: row.HeavyDamagePercent}
}

