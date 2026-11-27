
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for TrialJournalSealTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type TrialJournalSealIdComp struct {
    Value uint32
}

type TrialJournalSealEntry_idComp struct {
    Value uint32
}

type TrialJournalSealSeal_slot_kindComp struct {
    Value uint32
}

type TrialJournalSealCondition_typeComp struct {
    Value uint32
}

type TrialJournalSealCondition_paramComp struct {
    Value uint32
}

type TrialJournalSealEnabledComp struct {
    Value bool
}

type TrialJournalSealTitleComp struct {
    Value string
}

type TrialJournalSealDescComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeTrialJournalSealIdComp(row *pb.TrialJournalSealTable) TrialJournalSealIdComp {
    return TrialJournalSealIdComp{Value: row.Id}
}

func MakeTrialJournalSealEntry_idComp(row *pb.TrialJournalSealTable) TrialJournalSealEntry_idComp {
    return TrialJournalSealEntry_idComp{Value: row.EntryId}
}

func MakeTrialJournalSealSeal_slot_kindComp(row *pb.TrialJournalSealTable) TrialJournalSealSeal_slot_kindComp {
    return TrialJournalSealSeal_slot_kindComp{Value: row.SealSlotKind}
}

func MakeTrialJournalSealCondition_typeComp(row *pb.TrialJournalSealTable) TrialJournalSealCondition_typeComp {
    return TrialJournalSealCondition_typeComp{Value: row.ConditionType}
}

func MakeTrialJournalSealCondition_paramComp(row *pb.TrialJournalSealTable) TrialJournalSealCondition_paramComp {
    return TrialJournalSealCondition_paramComp{Value: row.ConditionParam}
}

func MakeTrialJournalSealEnabledComp(row *pb.TrialJournalSealTable) TrialJournalSealEnabledComp {
    return TrialJournalSealEnabledComp{Value: row.Enabled}
}

func MakeTrialJournalSealTitleComp(row *pb.TrialJournalSealTable) TrialJournalSealTitleComp {
    return TrialJournalSealTitleComp{Value: row.Title}
}

func MakeTrialJournalSealDescComp(row *pb.TrialJournalSealTable) TrialJournalSealDescComp {
    return TrialJournalSealDescComp{Value: row.Desc}
}

