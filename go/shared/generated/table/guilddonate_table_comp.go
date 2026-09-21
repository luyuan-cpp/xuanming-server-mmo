
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for GuildDonateTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type GuildDonateIdComp struct {
    Value uint32
}

type GuildDonateNameComp struct {
    Value string
}

type GuildDonateCurrency_typeComp struct {
    Value uint32
}

type GuildDonateCost_amountComp struct {
    Value uint64
}

type GuildDonateContribution_gainComp struct {
    Value uint64
}

type GuildDonateFunds_gainComp struct {
    Value uint64
}

type GuildDonateDaily_limitComp struct {
    Value uint32
}

type GuildDonateMin_guild_levelComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeGuildDonateIdComp(row *pb.GuildDonateTable) GuildDonateIdComp {
    return GuildDonateIdComp{Value: row.Id}
}

func MakeGuildDonateNameComp(row *pb.GuildDonateTable) GuildDonateNameComp {
    return GuildDonateNameComp{Value: row.Name}
}

func MakeGuildDonateCurrency_typeComp(row *pb.GuildDonateTable) GuildDonateCurrency_typeComp {
    return GuildDonateCurrency_typeComp{Value: row.CurrencyType}
}

func MakeGuildDonateCost_amountComp(row *pb.GuildDonateTable) GuildDonateCost_amountComp {
    return GuildDonateCost_amountComp{Value: row.CostAmount}
}

func MakeGuildDonateContribution_gainComp(row *pb.GuildDonateTable) GuildDonateContribution_gainComp {
    return GuildDonateContribution_gainComp{Value: row.ContributionGain}
}

func MakeGuildDonateFunds_gainComp(row *pb.GuildDonateTable) GuildDonateFunds_gainComp {
    return GuildDonateFunds_gainComp{Value: row.FundsGain}
}

func MakeGuildDonateDaily_limitComp(row *pb.GuildDonateTable) GuildDonateDaily_limitComp {
    return GuildDonateDaily_limitComp{Value: row.DailyLimit}
}

func MakeGuildDonateMin_guild_levelComp(row *pb.GuildDonateTable) GuildDonateMin_guild_levelComp {
    return GuildDonateMin_guild_levelComp{Value: row.MinGuildLevel}
}

