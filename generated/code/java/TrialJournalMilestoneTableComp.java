
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for TrialJournalMilestoneTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class TrialJournalMilestoneTableComp {

    private TrialJournalMilestoneTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(TrialJournalMilestoneTable row) {
            return new Id(row.getId());
        }
    }

    public record Scope_chapter_id(int value) {
        public static Scope_chapter_id from(TrialJournalMilestoneTable row) {
            return new Scope_chapter_id(row.getScopeChapterId());
        }
    }

    public record Season_id(int value) {
        public static Season_id from(TrialJournalMilestoneTable row) {
            return new Season_id(row.getSeasonId());
        }
    }

    public record Required_seals(int value) {
        public static Required_seals from(TrialJournalMilestoneTable row) {
            return new Required_seals(row.getRequiredSeals());
        }
    }

    public record Bound_sand_count(int value) {
        public static Bound_sand_count from(TrialJournalMilestoneTable row) {
            return new Bound_sand_count(row.getBoundSandCount());
        }
    }

    public record Bound_artifact_voucher_count(int value) {
        public static Bound_artifact_voucher_count from(TrialJournalMilestoneTable row) {
            return new Bound_artifact_voucher_count(row.getBoundArtifactVoucherCount());
        }
    }

    public record Badge_key(String value) {
        public static Badge_key from(TrialJournalMilestoneTable row) {
            return new Badge_key(row.getBadgeKey());
        }
    }

    public record Badge_name(String value) {
        public static Badge_name from(TrialJournalMilestoneTable row) {
            return new Badge_name(row.getBadgeName());
        }
    }

    public record Commentary_key(String value) {
        public static Commentary_key from(TrialJournalMilestoneTable row) {
            return new Commentary_key(row.getCommentaryKey());
        }
    }

    public record Commentary_title(String value) {
        public static Commentary_title from(TrialJournalMilestoneTable row) {
            return new Commentary_title(row.getCommentaryTitle());
        }
    }

    public record Commentary_body(String value) {
        public static Commentary_body from(TrialJournalMilestoneTable row) {
            return new Commentary_body(row.getCommentaryBody());
        }
    }

    public record Grant_appearance_id(String value) {
        public static Grant_appearance_id from(TrialJournalMilestoneTable row) {
            return new Grant_appearance_id(row.getGrantAppearanceId());
        }
    }

    public record Enabled(boolean value) {
        public static Enabled from(TrialJournalMilestoneTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Title(String value) {
        public static Title from(TrialJournalMilestoneTable row) {
            return new Title(row.getTitle());
        }
    }

    public record Desc(String value) {
        public static Desc from(TrialJournalMilestoneTable row) {
            return new Desc(row.getDesc());
        }
    }

    public record Grant_pet_cosmetic_id(int value) {
        public static Grant_pet_cosmetic_id from(TrialJournalMilestoneTable row) {
            return new Grant_pet_cosmetic_id(row.getGrantPetCosmeticId());
        }
    }

    public record Grant_skill_vfx_id(int value) {
        public static Grant_skill_vfx_id from(TrialJournalMilestoneTable row) {
            return new Grant_skill_vfx_id(row.getGrantSkillVfxId());
        }
    }

}