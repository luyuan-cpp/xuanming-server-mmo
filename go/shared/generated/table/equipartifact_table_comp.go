
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for EquipArtifactTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type EquipArtifactIdComp struct {
    Value uint32
}

type EquipArtifactBonus_bpsComp struct {
    Value uint32
}

type EquipArtifactCap_bpsComp struct {
    Value uint32
}

type EquipArtifactPer_round_limitComp struct {
    Value uint32
}

type EquipArtifactBattle_limitComp struct {
    Value uint32
}

type EquipArtifactActive_roundsComp struct {
    Value uint32
}

type EquipArtifactHealth_threshold_bpsComp struct {
    Value uint32
}

type EquipArtifactNameComp struct {
    Value string
}

type EquipArtifactDescriptionComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeEquipArtifactIdComp(row *pb.EquipArtifactTable) EquipArtifactIdComp {
    return EquipArtifactIdComp{Value: row.Id}
}

func MakeEquipArtifactBonus_bpsComp(row *pb.EquipArtifactTable) EquipArtifactBonus_bpsComp {
    return EquipArtifactBonus_bpsComp{Value: row.BonusBps}
}

func MakeEquipArtifactCap_bpsComp(row *pb.EquipArtifactTable) EquipArtifactCap_bpsComp {
    return EquipArtifactCap_bpsComp{Value: row.CapBps}
}

func MakeEquipArtifactPer_round_limitComp(row *pb.EquipArtifactTable) EquipArtifactPer_round_limitComp {
    return EquipArtifactPer_round_limitComp{Value: row.PerRoundLimit}
}

func MakeEquipArtifactBattle_limitComp(row *pb.EquipArtifactTable) EquipArtifactBattle_limitComp {
    return EquipArtifactBattle_limitComp{Value: row.BattleLimit}
}

func MakeEquipArtifactActive_roundsComp(row *pb.EquipArtifactTable) EquipArtifactActive_roundsComp {
    return EquipArtifactActive_roundsComp{Value: row.ActiveRounds}
}

func MakeEquipArtifactHealth_threshold_bpsComp(row *pb.EquipArtifactTable) EquipArtifactHealth_threshold_bpsComp {
    return EquipArtifactHealth_threshold_bpsComp{Value: row.HealthThresholdBps}
}

func MakeEquipArtifactNameComp(row *pb.EquipArtifactTable) EquipArtifactNameComp {
    return EquipArtifactNameComp{Value: row.Name}
}

func MakeEquipArtifactDescriptionComp(row *pb.EquipArtifactTable) EquipArtifactDescriptionComp {
    return EquipArtifactDescriptionComp{Value: row.Description}
}

