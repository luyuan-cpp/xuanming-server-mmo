
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for TournamentRewardTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type TournamentRewardIdComp struct {
    Value uint32
}

type TournamentRewardTier_kindComp struct {
    Value uint32
}

type TournamentRewardMin_rankComp struct {
    Value uint32
}

type TournamentRewardMax_rankComp struct {
    Value uint32
}

type TournamentRewardGold_amountComp struct {
    Value uint64
}

type TournamentRewardTitle_codeComp struct {
    Value string
}

type TournamentRewardBadge_codeComp struct {
    Value string
}

type TournamentRewardMail_titleComp struct {
    Value string
}

type TournamentRewardMail_bodyComp struct {
    Value string
}

type TournamentRewardDescComp struct {
    Value string
}

type TournamentRewardItem_idsComp struct {
    Values []uint32
}

type TournamentRewardItem_countsComp struct {
    Values []uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeTournamentRewardIdComp(row *pb.TournamentRewardTable) TournamentRewardIdComp {
    return TournamentRewardIdComp{Value: row.Id}
}

func MakeTournamentRewardTier_kindComp(row *pb.TournamentRewardTable) TournamentRewardTier_kindComp {
    return TournamentRewardTier_kindComp{Value: row.TierKind}
}

func MakeTournamentRewardMin_rankComp(row *pb.TournamentRewardTable) TournamentRewardMin_rankComp {
    return TournamentRewardMin_rankComp{Value: row.MinRank}
}

func MakeTournamentRewardMax_rankComp(row *pb.TournamentRewardTable) TournamentRewardMax_rankComp {
    return TournamentRewardMax_rankComp{Value: row.MaxRank}
}

func MakeTournamentRewardGold_amountComp(row *pb.TournamentRewardTable) TournamentRewardGold_amountComp {
    return TournamentRewardGold_amountComp{Value: row.GoldAmount}
}

func MakeTournamentRewardTitle_codeComp(row *pb.TournamentRewardTable) TournamentRewardTitle_codeComp {
    return TournamentRewardTitle_codeComp{Value: row.TitleCode}
}

func MakeTournamentRewardBadge_codeComp(row *pb.TournamentRewardTable) TournamentRewardBadge_codeComp {
    return TournamentRewardBadge_codeComp{Value: row.BadgeCode}
}

func MakeTournamentRewardMail_titleComp(row *pb.TournamentRewardTable) TournamentRewardMail_titleComp {
    return TournamentRewardMail_titleComp{Value: row.MailTitle}
}

func MakeTournamentRewardMail_bodyComp(row *pb.TournamentRewardTable) TournamentRewardMail_bodyComp {
    return TournamentRewardMail_bodyComp{Value: row.MailBody}
}

func MakeTournamentRewardDescComp(row *pb.TournamentRewardTable) TournamentRewardDescComp {
    return TournamentRewardDescComp{Value: row.Desc}
}

func MakeTournamentRewardItem_idsComp(row *pb.TournamentRewardTable) TournamentRewardItem_idsComp {
    return TournamentRewardItem_idsComp{Values: row.ItemIds}
}

func MakeTournamentRewardItem_countsComp(row *pb.TournamentRewardTable) TournamentRewardItem_countsComp {
    return TournamentRewardItem_countsComp{Values: row.ItemCounts}
}

