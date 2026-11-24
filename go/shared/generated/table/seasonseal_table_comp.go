
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for SeasonSealTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type SeasonSealIdComp struct {
    Value uint32
}

type SeasonSealSeason_idComp struct {
    Value uint32
}

type SeasonSealNameComp struct {
    Value string
}

type SeasonSealDescriptionComp struct {
    Value string
}

type SeasonSealIcon_keyComp struct {
    Value string
}

type SeasonSealEffect_idComp struct {
    Value uint32
}

type SeasonSealBonus_bpsComp struct {
    Value uint32
}

type SeasonSealCap_bpsComp struct {
    Value uint32
}

type SeasonSealPer_round_limitComp struct {
    Value uint32
}

type SeasonSealBattle_limitComp struct {
    Value uint32
}

type SeasonSealActive_roundsComp struct {
    Value uint32
}

type SeasonSealHealth_threshold_bpsComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeSeasonSealIdComp(row *pb.SeasonSealTable) SeasonSealIdComp {
    return SeasonSealIdComp{Value: row.Id}
}

func MakeSeasonSealSeason_idComp(row *pb.SeasonSealTable) SeasonSealSeason_idComp {
    return SeasonSealSeason_idComp{Value: row.SeasonId}
}

func MakeSeasonSealNameComp(row *pb.SeasonSealTable) SeasonSealNameComp {
    return SeasonSealNameComp{Value: row.Name}
}

func MakeSeasonSealDescriptionComp(row *pb.SeasonSealTable) SeasonSealDescriptionComp {
    return SeasonSealDescriptionComp{Value: row.Description}
}

func MakeSeasonSealIcon_keyComp(row *pb.SeasonSealTable) SeasonSealIcon_keyComp {
    return SeasonSealIcon_keyComp{Value: row.IconKey}
}

func MakeSeasonSealEffect_idComp(row *pb.SeasonSealTable) SeasonSealEffect_idComp {
    return SeasonSealEffect_idComp{Value: row.EffectId}
}

func MakeSeasonSealBonus_bpsComp(row *pb.SeasonSealTable) SeasonSealBonus_bpsComp {
    return SeasonSealBonus_bpsComp{Value: row.BonusBps}
}

func MakeSeasonSealCap_bpsComp(row *pb.SeasonSealTable) SeasonSealCap_bpsComp {
    return SeasonSealCap_bpsComp{Value: row.CapBps}
}

func MakeSeasonSealPer_round_limitComp(row *pb.SeasonSealTable) SeasonSealPer_round_limitComp {
    return SeasonSealPer_round_limitComp{Value: row.PerRoundLimit}
}

func MakeSeasonSealBattle_limitComp(row *pb.SeasonSealTable) SeasonSealBattle_limitComp {
    return SeasonSealBattle_limitComp{Value: row.BattleLimit}
}

func MakeSeasonSealActive_roundsComp(row *pb.SeasonSealTable) SeasonSealActive_roundsComp {
    return SeasonSealActive_roundsComp{Value: row.ActiveRounds}
}

func MakeSeasonSealHealth_threshold_bpsComp(row *pb.SeasonSealTable) SeasonSealHealth_threshold_bpsComp {
    return SeasonSealHealth_threshold_bpsComp{Value: row.HealthThresholdBps}
}

