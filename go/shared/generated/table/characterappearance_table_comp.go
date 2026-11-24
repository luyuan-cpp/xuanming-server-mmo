
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for CharacterAppearanceTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type CharacterAppearanceIdComp struct {
    Value uint32
}

type CharacterAppearanceAppearance_idComp struct {
    Value string
}

type CharacterAppearanceNameComp struct {
    Value string
}

type CharacterAppearanceEnabledComp struct {
    Value uint32
}

type CharacterAppearanceSort_orderComp struct {
    Value uint32
}

type CharacterAppearanceDescComp struct {
    Value string
}

type CharacterAppearanceStarter_creatableComp struct {
    Value uint32
}

type CharacterAppearanceCard_unlockableComp struct {
    Value uint32
}

type CharacterAppearanceSeries_idComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeCharacterAppearanceIdComp(row *pb.CharacterAppearanceTable) CharacterAppearanceIdComp {
    return CharacterAppearanceIdComp{Value: row.Id}
}

func MakeCharacterAppearanceAppearance_idComp(row *pb.CharacterAppearanceTable) CharacterAppearanceAppearance_idComp {
    return CharacterAppearanceAppearance_idComp{Value: row.AppearanceId}
}

func MakeCharacterAppearanceNameComp(row *pb.CharacterAppearanceTable) CharacterAppearanceNameComp {
    return CharacterAppearanceNameComp{Value: row.Name}
}

func MakeCharacterAppearanceEnabledComp(row *pb.CharacterAppearanceTable) CharacterAppearanceEnabledComp {
    return CharacterAppearanceEnabledComp{Value: row.Enabled}
}

func MakeCharacterAppearanceSort_orderComp(row *pb.CharacterAppearanceTable) CharacterAppearanceSort_orderComp {
    return CharacterAppearanceSort_orderComp{Value: row.SortOrder}
}

func MakeCharacterAppearanceDescComp(row *pb.CharacterAppearanceTable) CharacterAppearanceDescComp {
    return CharacterAppearanceDescComp{Value: row.Desc}
}

func MakeCharacterAppearanceStarter_creatableComp(row *pb.CharacterAppearanceTable) CharacterAppearanceStarter_creatableComp {
    return CharacterAppearanceStarter_creatableComp{Value: row.StarterCreatable}
}

func MakeCharacterAppearanceCard_unlockableComp(row *pb.CharacterAppearanceTable) CharacterAppearanceCard_unlockableComp {
    return CharacterAppearanceCard_unlockableComp{Value: row.CardUnlockable}
}

func MakeCharacterAppearanceSeries_idComp(row *pb.CharacterAppearanceTable) CharacterAppearanceSeries_idComp {
    return CharacterAppearanceSeries_idComp{Value: row.SeriesId}
}

