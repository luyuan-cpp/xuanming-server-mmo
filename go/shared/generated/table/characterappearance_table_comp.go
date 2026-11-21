
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

