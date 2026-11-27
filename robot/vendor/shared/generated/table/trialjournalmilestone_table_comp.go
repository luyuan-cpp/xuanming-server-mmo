
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for TrialJournalMilestoneTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type TrialJournalMilestoneIdComp struct {
    Value uint32
}

type TrialJournalMilestoneScope_chapter_idComp struct {
    Value uint32
}

type TrialJournalMilestoneSeason_idComp struct {
    Value uint32
}

type TrialJournalMilestoneRequired_sealsComp struct {
    Value uint32
}

type TrialJournalMilestoneBound_sand_countComp struct {
    Value uint32
}

type TrialJournalMilestoneBound_artifact_voucher_countComp struct {
    Value uint32
}

type TrialJournalMilestoneBadge_keyComp struct {
    Value string
}

type TrialJournalMilestoneBadge_nameComp struct {
    Value string
}

type TrialJournalMilestoneCommentary_keyComp struct {
    Value string
}

type TrialJournalMilestoneCommentary_titleComp struct {
    Value string
}

type TrialJournalMilestoneCommentary_bodyComp struct {
    Value string
}

type TrialJournalMilestoneGrant_appearance_idComp struct {
    Value string
}

type TrialJournalMilestoneEnabledComp struct {
    Value bool
}

type TrialJournalMilestoneTitleComp struct {
    Value string
}

type TrialJournalMilestoneDescComp struct {
    Value string
}

type TrialJournalMilestoneGrant_pet_cosmetic_idComp struct {
    Value uint32
}

type TrialJournalMilestoneGrant_skill_vfx_idComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeTrialJournalMilestoneIdComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneIdComp {
    return TrialJournalMilestoneIdComp{Value: row.Id}
}

func MakeTrialJournalMilestoneScope_chapter_idComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneScope_chapter_idComp {
    return TrialJournalMilestoneScope_chapter_idComp{Value: row.ScopeChapterId}
}

func MakeTrialJournalMilestoneSeason_idComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneSeason_idComp {
    return TrialJournalMilestoneSeason_idComp{Value: row.SeasonId}
}

func MakeTrialJournalMilestoneRequired_sealsComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneRequired_sealsComp {
    return TrialJournalMilestoneRequired_sealsComp{Value: row.RequiredSeals}
}

func MakeTrialJournalMilestoneBound_sand_countComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneBound_sand_countComp {
    return TrialJournalMilestoneBound_sand_countComp{Value: row.BoundSandCount}
}

func MakeTrialJournalMilestoneBound_artifact_voucher_countComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneBound_artifact_voucher_countComp {
    return TrialJournalMilestoneBound_artifact_voucher_countComp{Value: row.BoundArtifactVoucherCount}
}

func MakeTrialJournalMilestoneBadge_keyComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneBadge_keyComp {
    return TrialJournalMilestoneBadge_keyComp{Value: row.BadgeKey}
}

func MakeTrialJournalMilestoneBadge_nameComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneBadge_nameComp {
    return TrialJournalMilestoneBadge_nameComp{Value: row.BadgeName}
}

func MakeTrialJournalMilestoneCommentary_keyComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneCommentary_keyComp {
    return TrialJournalMilestoneCommentary_keyComp{Value: row.CommentaryKey}
}

func MakeTrialJournalMilestoneCommentary_titleComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneCommentary_titleComp {
    return TrialJournalMilestoneCommentary_titleComp{Value: row.CommentaryTitle}
}

func MakeTrialJournalMilestoneCommentary_bodyComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneCommentary_bodyComp {
    return TrialJournalMilestoneCommentary_bodyComp{Value: row.CommentaryBody}
}

func MakeTrialJournalMilestoneGrant_appearance_idComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneGrant_appearance_idComp {
    return TrialJournalMilestoneGrant_appearance_idComp{Value: row.GrantAppearanceId}
}

func MakeTrialJournalMilestoneEnabledComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneEnabledComp {
    return TrialJournalMilestoneEnabledComp{Value: row.Enabled}
}

func MakeTrialJournalMilestoneTitleComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneTitleComp {
    return TrialJournalMilestoneTitleComp{Value: row.Title}
}

func MakeTrialJournalMilestoneDescComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneDescComp {
    return TrialJournalMilestoneDescComp{Value: row.Desc}
}

func MakeTrialJournalMilestoneGrant_pet_cosmetic_idComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneGrant_pet_cosmetic_idComp {
    return TrialJournalMilestoneGrant_pet_cosmetic_idComp{Value: row.GrantPetCosmeticId}
}

func MakeTrialJournalMilestoneGrant_skill_vfx_idComp(row *pb.TrialJournalMilestoneTable) TrialJournalMilestoneGrant_skill_vfx_idComp {
    return TrialJournalMilestoneGrant_skill_vfx_idComp{Value: row.GrantSkillVfxId}
}

