
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for GuildActivityTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type GuildActivityIdComp struct {
    Value uint32
}

type GuildActivityNameComp struct {
    Value string
}

type GuildActivityTypeComp struct {
    Value uint32
}

type GuildActivityEnabledComp struct {
    Value bool
}

type GuildActivityStart_at_msComp struct {
    Value uint64
}

type GuildActivityEnd_at_msComp struct {
    Value uint64
}

type GuildActivityMin_guild_levelComp struct {
    Value uint32
}

type GuildActivityPersonal_contributionComp struct {
    Value uint64
}

type GuildActivityGuild_fundsComp struct {
    Value uint64
}

type GuildActivityGuild_thresholdComp struct {
    Value uint32
}

type GuildActivityReward_idComp struct {
    Value uint32
}

type GuildActivityDungeon_idComp struct {
    Value uint32
}

type GuildActivityTeam_size_minComp struct {
    Value uint32
}

type GuildActivityTeam_size_maxComp struct {
    Value uint32
}

type GuildActivityDaily_limitComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeGuildActivityIdComp(row *pb.GuildActivityTable) GuildActivityIdComp {
    return GuildActivityIdComp{Value: row.Id}
}

func MakeGuildActivityNameComp(row *pb.GuildActivityTable) GuildActivityNameComp {
    return GuildActivityNameComp{Value: row.Name}
}

func MakeGuildActivityTypeComp(row *pb.GuildActivityTable) GuildActivityTypeComp {
    return GuildActivityTypeComp{Value: row.Type}
}

func MakeGuildActivityEnabledComp(row *pb.GuildActivityTable) GuildActivityEnabledComp {
    return GuildActivityEnabledComp{Value: row.Enabled}
}

func MakeGuildActivityStart_at_msComp(row *pb.GuildActivityTable) GuildActivityStart_at_msComp {
    return GuildActivityStart_at_msComp{Value: row.StartAtMs}
}

func MakeGuildActivityEnd_at_msComp(row *pb.GuildActivityTable) GuildActivityEnd_at_msComp {
    return GuildActivityEnd_at_msComp{Value: row.EndAtMs}
}

func MakeGuildActivityMin_guild_levelComp(row *pb.GuildActivityTable) GuildActivityMin_guild_levelComp {
    return GuildActivityMin_guild_levelComp{Value: row.MinGuildLevel}
}

func MakeGuildActivityPersonal_contributionComp(row *pb.GuildActivityTable) GuildActivityPersonal_contributionComp {
    return GuildActivityPersonal_contributionComp{Value: row.PersonalContribution}
}

func MakeGuildActivityGuild_fundsComp(row *pb.GuildActivityTable) GuildActivityGuild_fundsComp {
    return GuildActivityGuild_fundsComp{Value: row.GuildFunds}
}

func MakeGuildActivityGuild_thresholdComp(row *pb.GuildActivityTable) GuildActivityGuild_thresholdComp {
    return GuildActivityGuild_thresholdComp{Value: row.GuildThreshold}
}

func MakeGuildActivityReward_idComp(row *pb.GuildActivityTable) GuildActivityReward_idComp {
    return GuildActivityReward_idComp{Value: row.RewardId}
}

func MakeGuildActivityDungeon_idComp(row *pb.GuildActivityTable) GuildActivityDungeon_idComp {
    return GuildActivityDungeon_idComp{Value: row.DungeonId}
}

func MakeGuildActivityTeam_size_minComp(row *pb.GuildActivityTable) GuildActivityTeam_size_minComp {
    return GuildActivityTeam_size_minComp{Value: row.TeamSizeMin}
}

func MakeGuildActivityTeam_size_maxComp(row *pb.GuildActivityTable) GuildActivityTeam_size_maxComp {
    return GuildActivityTeam_size_maxComp{Value: row.TeamSizeMax}
}

func MakeGuildActivityDaily_limitComp(row *pb.GuildActivityTable) GuildActivityDaily_limitComp {
    return GuildActivityDaily_limitComp{Value: row.DailyLimit}
}

