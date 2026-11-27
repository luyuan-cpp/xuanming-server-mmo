
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for RewardedAdSlotTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type RewardedAdSlotIdComp struct {
    Value uint32
}

type RewardedAdSlotEnabledComp struct {
    Value uint32
}

type RewardedAdSlotMin_levelComp struct {
    Value uint32
}

type RewardedAdSlotDaily_limitComp struct {
    Value uint32
}

type RewardedAdSlotCooldown_secComp struct {
    Value uint32
}

type RewardedAdSlotMin_watch_secComp struct {
    Value uint32
}

type RewardedAdSlotSession_ttl_secComp struct {
    Value uint32
}

type RewardedAdSlotCurrency_typeComp struct {
    Value uint32
}

type RewardedAdSlotReward_amountComp struct {
    Value uint64
}

type RewardedAdSlotRequire_ssvComp struct {
    Value uint32
}

type RewardedAdSlotPlatform_maskComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeRewardedAdSlotIdComp(row *pb.RewardedAdSlotTable) RewardedAdSlotIdComp {
    return RewardedAdSlotIdComp{Value: row.Id}
}

func MakeRewardedAdSlotEnabledComp(row *pb.RewardedAdSlotTable) RewardedAdSlotEnabledComp {
    return RewardedAdSlotEnabledComp{Value: row.Enabled}
}

func MakeRewardedAdSlotMin_levelComp(row *pb.RewardedAdSlotTable) RewardedAdSlotMin_levelComp {
    return RewardedAdSlotMin_levelComp{Value: row.MinLevel}
}

func MakeRewardedAdSlotDaily_limitComp(row *pb.RewardedAdSlotTable) RewardedAdSlotDaily_limitComp {
    return RewardedAdSlotDaily_limitComp{Value: row.DailyLimit}
}

func MakeRewardedAdSlotCooldown_secComp(row *pb.RewardedAdSlotTable) RewardedAdSlotCooldown_secComp {
    return RewardedAdSlotCooldown_secComp{Value: row.CooldownSec}
}

func MakeRewardedAdSlotMin_watch_secComp(row *pb.RewardedAdSlotTable) RewardedAdSlotMin_watch_secComp {
    return RewardedAdSlotMin_watch_secComp{Value: row.MinWatchSec}
}

func MakeRewardedAdSlotSession_ttl_secComp(row *pb.RewardedAdSlotTable) RewardedAdSlotSession_ttl_secComp {
    return RewardedAdSlotSession_ttl_secComp{Value: row.SessionTtlSec}
}

func MakeRewardedAdSlotCurrency_typeComp(row *pb.RewardedAdSlotTable) RewardedAdSlotCurrency_typeComp {
    return RewardedAdSlotCurrency_typeComp{Value: row.CurrencyType}
}

func MakeRewardedAdSlotReward_amountComp(row *pb.RewardedAdSlotTable) RewardedAdSlotReward_amountComp {
    return RewardedAdSlotReward_amountComp{Value: row.RewardAmount}
}

func MakeRewardedAdSlotRequire_ssvComp(row *pb.RewardedAdSlotTable) RewardedAdSlotRequire_ssvComp {
    return RewardedAdSlotRequire_ssvComp{Value: row.RequireSsv}
}

func MakeRewardedAdSlotPlatform_maskComp(row *pb.RewardedAdSlotTable) RewardedAdSlotPlatform_maskComp {
    return RewardedAdSlotPlatform_maskComp{Value: row.PlatformMask}
}

