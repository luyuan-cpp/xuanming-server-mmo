
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for TrialJournalEntryTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type TrialJournalEntryIdComp struct {
    Value uint32
}

type TrialJournalEntryChapter_idComp struct {
    Value uint32
}

type TrialJournalEntrySeason_idComp struct {
    Value uint32
}

type TrialJournalEntrySource_kindComp struct {
    Value uint32
}

type TrialJournalEntryDifficultyComp struct {
    Value uint32
}

type TrialJournalEntryVariant_keyComp struct {
    Value string
}

type TrialJournalEntryRule_versionComp struct {
    Value uint32
}

type TrialJournalEntryRule_version_nameComp struct {
    Value string
}

type TrialJournalEntryEnabledComp struct {
    Value bool
}

type TrialJournalEntrySort_orderComp struct {
    Value uint32
}

type TrialJournalEntryDisplay_nameComp struct {
    Value string
}

type TrialJournalEntryChapter_nameComp struct {
    Value string
}

type TrialJournalEntryIcon_keyComp struct {
    Value string
}

type TrialJournalEntryUnlock_modeComp struct {
    Value uint32
}

type TrialJournalEntryJump_targetComp struct {
    Value string
}

type TrialJournalEntryDescComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeTrialJournalEntryIdComp(row *pb.TrialJournalEntryTable) TrialJournalEntryIdComp {
    return TrialJournalEntryIdComp{Value: row.Id}
}

func MakeTrialJournalEntryChapter_idComp(row *pb.TrialJournalEntryTable) TrialJournalEntryChapter_idComp {
    return TrialJournalEntryChapter_idComp{Value: row.ChapterId}
}

func MakeTrialJournalEntrySeason_idComp(row *pb.TrialJournalEntryTable) TrialJournalEntrySeason_idComp {
    return TrialJournalEntrySeason_idComp{Value: row.SeasonId}
}

func MakeTrialJournalEntrySource_kindComp(row *pb.TrialJournalEntryTable) TrialJournalEntrySource_kindComp {
    return TrialJournalEntrySource_kindComp{Value: row.SourceKind}
}

func MakeTrialJournalEntryDifficultyComp(row *pb.TrialJournalEntryTable) TrialJournalEntryDifficultyComp {
    return TrialJournalEntryDifficultyComp{Value: row.Difficulty}
}

func MakeTrialJournalEntryVariant_keyComp(row *pb.TrialJournalEntryTable) TrialJournalEntryVariant_keyComp {
    return TrialJournalEntryVariant_keyComp{Value: row.VariantKey}
}

func MakeTrialJournalEntryRule_versionComp(row *pb.TrialJournalEntryTable) TrialJournalEntryRule_versionComp {
    return TrialJournalEntryRule_versionComp{Value: row.RuleVersion}
}

func MakeTrialJournalEntryRule_version_nameComp(row *pb.TrialJournalEntryTable) TrialJournalEntryRule_version_nameComp {
    return TrialJournalEntryRule_version_nameComp{Value: row.RuleVersionName}
}

func MakeTrialJournalEntryEnabledComp(row *pb.TrialJournalEntryTable) TrialJournalEntryEnabledComp {
    return TrialJournalEntryEnabledComp{Value: row.Enabled}
}

func MakeTrialJournalEntrySort_orderComp(row *pb.TrialJournalEntryTable) TrialJournalEntrySort_orderComp {
    return TrialJournalEntrySort_orderComp{Value: row.SortOrder}
}

func MakeTrialJournalEntryDisplay_nameComp(row *pb.TrialJournalEntryTable) TrialJournalEntryDisplay_nameComp {
    return TrialJournalEntryDisplay_nameComp{Value: row.DisplayName}
}

func MakeTrialJournalEntryChapter_nameComp(row *pb.TrialJournalEntryTable) TrialJournalEntryChapter_nameComp {
    return TrialJournalEntryChapter_nameComp{Value: row.ChapterName}
}

func MakeTrialJournalEntryIcon_keyComp(row *pb.TrialJournalEntryTable) TrialJournalEntryIcon_keyComp {
    return TrialJournalEntryIcon_keyComp{Value: row.IconKey}
}

func MakeTrialJournalEntryUnlock_modeComp(row *pb.TrialJournalEntryTable) TrialJournalEntryUnlock_modeComp {
    return TrialJournalEntryUnlock_modeComp{Value: row.UnlockMode}
}

func MakeTrialJournalEntryJump_targetComp(row *pb.TrialJournalEntryTable) TrialJournalEntryJump_targetComp {
    return TrialJournalEntryJump_targetComp{Value: row.JumpTarget}
}

func MakeTrialJournalEntryDescComp(row *pb.TrialJournalEntryTable) TrialJournalEntryDescComp {
    return TrialJournalEntryDescComp{Value: row.Desc}
}

