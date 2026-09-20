
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for GuildRuleTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type GuildRuleIdComp struct {
    Value uint32
}

type GuildRuleApplication_expire_hoursComp struct {
    Value uint32
}

type GuildRuleMax_pending_applications_per_playerComp struct {
    Value uint32
}

type GuildRuleMax_pending_applications_per_guildComp struct {
    Value uint32
}

type GuildRuleAsset_op_deadline_secondsComp struct {
    Value uint32
}

type GuildRuleAsset_op_retry_base_msComp struct {
    Value uint32
}

type GuildRuleReunion_min_online_membersComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeGuildRuleIdComp(row *pb.GuildRuleTable) GuildRuleIdComp {
    return GuildRuleIdComp{Value: row.Id}
}

func MakeGuildRuleApplication_expire_hoursComp(row *pb.GuildRuleTable) GuildRuleApplication_expire_hoursComp {
    return GuildRuleApplication_expire_hoursComp{Value: row.ApplicationExpireHours}
}

func MakeGuildRuleMax_pending_applications_per_playerComp(row *pb.GuildRuleTable) GuildRuleMax_pending_applications_per_playerComp {
    return GuildRuleMax_pending_applications_per_playerComp{Value: row.MaxPendingApplicationsPerPlayer}
}

func MakeGuildRuleMax_pending_applications_per_guildComp(row *pb.GuildRuleTable) GuildRuleMax_pending_applications_per_guildComp {
    return GuildRuleMax_pending_applications_per_guildComp{Value: row.MaxPendingApplicationsPerGuild}
}

func MakeGuildRuleAsset_op_deadline_secondsComp(row *pb.GuildRuleTable) GuildRuleAsset_op_deadline_secondsComp {
    return GuildRuleAsset_op_deadline_secondsComp{Value: row.AssetOpDeadlineSeconds}
}

func MakeGuildRuleAsset_op_retry_base_msComp(row *pb.GuildRuleTable) GuildRuleAsset_op_retry_base_msComp {
    return GuildRuleAsset_op_retry_base_msComp{Value: row.AssetOpRetryBaseMs}
}

func MakeGuildRuleReunion_min_online_membersComp(row *pb.GuildRuleTable) GuildRuleReunion_min_online_membersComp {
    return GuildRuleReunion_min_online_membersComp{Value: row.ReunionMinOnlineMembers}
}

