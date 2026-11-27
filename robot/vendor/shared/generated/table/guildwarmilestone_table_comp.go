
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for GuildWarMilestoneTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type GuildWarMilestoneIdComp struct {
    Value uint32
}

type GuildWarMilestoneRequired_guild_meritComp struct {
    Value uint64
}

type GuildWarMilestoneReward_item1_idComp struct {
    Value uint32
}

type GuildWarMilestoneReward_item1_countComp struct {
    Value uint32
}

type GuildWarMilestoneReward_item2_idComp struct {
    Value uint32
}

type GuildWarMilestoneReward_item2_countComp struct {
    Value uint32
}

type GuildWarMilestoneReward_silverComp struct {
    Value uint64
}

type GuildWarMilestoneReward_contributionComp struct {
    Value uint64
}

type GuildWarMilestoneReward_previewComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeGuildWarMilestoneIdComp(row *pb.GuildWarMilestoneTable) GuildWarMilestoneIdComp {
    return GuildWarMilestoneIdComp{Value: row.Id}
}

func MakeGuildWarMilestoneRequired_guild_meritComp(row *pb.GuildWarMilestoneTable) GuildWarMilestoneRequired_guild_meritComp {
    return GuildWarMilestoneRequired_guild_meritComp{Value: row.RequiredGuildMerit}
}

func MakeGuildWarMilestoneReward_item1_idComp(row *pb.GuildWarMilestoneTable) GuildWarMilestoneReward_item1_idComp {
    return GuildWarMilestoneReward_item1_idComp{Value: row.RewardItem1Id}
}

func MakeGuildWarMilestoneReward_item1_countComp(row *pb.GuildWarMilestoneTable) GuildWarMilestoneReward_item1_countComp {
    return GuildWarMilestoneReward_item1_countComp{Value: row.RewardItem1Count}
}

func MakeGuildWarMilestoneReward_item2_idComp(row *pb.GuildWarMilestoneTable) GuildWarMilestoneReward_item2_idComp {
    return GuildWarMilestoneReward_item2_idComp{Value: row.RewardItem2Id}
}

func MakeGuildWarMilestoneReward_item2_countComp(row *pb.GuildWarMilestoneTable) GuildWarMilestoneReward_item2_countComp {
    return GuildWarMilestoneReward_item2_countComp{Value: row.RewardItem2Count}
}

func MakeGuildWarMilestoneReward_silverComp(row *pb.GuildWarMilestoneTable) GuildWarMilestoneReward_silverComp {
    return GuildWarMilestoneReward_silverComp{Value: row.RewardSilver}
}

func MakeGuildWarMilestoneReward_contributionComp(row *pb.GuildWarMilestoneTable) GuildWarMilestoneReward_contributionComp {
    return GuildWarMilestoneReward_contributionComp{Value: row.RewardContribution}
}

func MakeGuildWarMilestoneReward_previewComp(row *pb.GuildWarMilestoneTable) GuildWarMilestoneReward_previewComp {
    return GuildWarMilestoneReward_previewComp{Value: row.RewardPreview}
}

