
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for GuildShopTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type GuildShopIdComp struct {
    Value uint32
}

type GuildShopNameComp struct {
    Value string
}

type GuildShopCategoryComp struct {
    Value uint32
}

type GuildShopItem_idComp struct {
    Value uint32
}

type GuildShopItem_countComp struct {
    Value uint32
}

type GuildShopCost_contributionComp struct {
    Value uint64
}

type GuildShopRequired_guild_levelComp struct {
    Value uint32
}

type GuildShopLimit_periodComp struct {
    Value uint32
}

type GuildShopLimit_countComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeGuildShopIdComp(row *pb.GuildShopTable) GuildShopIdComp {
    return GuildShopIdComp{Value: row.Id}
}

func MakeGuildShopNameComp(row *pb.GuildShopTable) GuildShopNameComp {
    return GuildShopNameComp{Value: row.Name}
}

func MakeGuildShopCategoryComp(row *pb.GuildShopTable) GuildShopCategoryComp {
    return GuildShopCategoryComp{Value: row.Category}
}

func MakeGuildShopItem_idComp(row *pb.GuildShopTable) GuildShopItem_idComp {
    return GuildShopItem_idComp{Value: row.ItemId}
}

func MakeGuildShopItem_countComp(row *pb.GuildShopTable) GuildShopItem_countComp {
    return GuildShopItem_countComp{Value: row.ItemCount}
}

func MakeGuildShopCost_contributionComp(row *pb.GuildShopTable) GuildShopCost_contributionComp {
    return GuildShopCost_contributionComp{Value: row.CostContribution}
}

func MakeGuildShopRequired_guild_levelComp(row *pb.GuildShopTable) GuildShopRequired_guild_levelComp {
    return GuildShopRequired_guild_levelComp{Value: row.RequiredGuildLevel}
}

func MakeGuildShopLimit_periodComp(row *pb.GuildShopTable) GuildShopLimit_periodComp {
    return GuildShopLimit_periodComp{Value: row.LimitPeriod}
}

func MakeGuildShopLimit_countComp(row *pb.GuildShopTable) GuildShopLimit_countComp {
    return GuildShopLimit_countComp{Value: row.LimitCount}
}

