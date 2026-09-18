
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for GuildLevelTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type GuildLevelIdComp struct {
    Value uint32
}

type GuildLevelMax_membersComp struct {
    Value uint32
}

type GuildLevelMax_officersComp struct {
    Value uint32
}

type GuildLevelUpgrade_cost_fundsComp struct {
    Value uint64
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeGuildLevelIdComp(row *pb.GuildLevelTable) GuildLevelIdComp {
    return GuildLevelIdComp{Value: row.Id}
}

func MakeGuildLevelMax_membersComp(row *pb.GuildLevelTable) GuildLevelMax_membersComp {
    return GuildLevelMax_membersComp{Value: row.MaxMembers}
}

func MakeGuildLevelMax_officersComp(row *pb.GuildLevelTable) GuildLevelMax_officersComp {
    return GuildLevelMax_officersComp{Value: row.MaxOfficers}
}

func MakeGuildLevelUpgrade_cost_fundsComp(row *pb.GuildLevelTable) GuildLevelUpgrade_cost_fundsComp {
    return GuildLevelUpgrade_cost_fundsComp{Value: row.UpgradeCostFunds}
}

