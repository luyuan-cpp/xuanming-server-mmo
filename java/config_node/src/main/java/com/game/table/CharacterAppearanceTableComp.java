
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for CharacterAppearanceTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class CharacterAppearanceTableComp {

    private CharacterAppearanceTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(CharacterAppearanceTable row) {
            return new Id(row.getId());
        }
    }

    public record Appearance_id(String value) {
        public static Appearance_id from(CharacterAppearanceTable row) {
            return new Appearance_id(row.getAppearanceId());
        }
    }

    public record Name(String value) {
        public static Name from(CharacterAppearanceTable row) {
            return new Name(row.getName());
        }
    }

    public record Enabled(int value) {
        public static Enabled from(CharacterAppearanceTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Sort_order(int value) {
        public static Sort_order from(CharacterAppearanceTable row) {
            return new Sort_order(row.getSortOrder());
        }
    }

    public record Desc(String value) {
        public static Desc from(CharacterAppearanceTable row) {
            return new Desc(row.getDesc());
        }
    }

    public record Starter_creatable(int value) {
        public static Starter_creatable from(CharacterAppearanceTable row) {
            return new Starter_creatable(row.getStarterCreatable());
        }
    }

    public record Card_unlockable(int value) {
        public static Card_unlockable from(CharacterAppearanceTable row) {
            return new Card_unlockable(row.getCardUnlockable());
        }
    }

    public record Series_id(int value) {
        public static Series_id from(CharacterAppearanceTable row) {
            return new Series_id(row.getSeriesId());
        }
    }

    public record Archive_name(String value) {
        public static Archive_name from(CharacterAppearanceTable row) {
            return new Archive_name(row.getArchiveName());
        }
    }

    public record Nickname(String value) {
        public static Nickname from(CharacterAppearanceTable row) {
            return new Nickname(row.getNickname());
        }
    }

    public record Biography_summary(String value) {
        public static Biography_summary from(CharacterAppearanceTable row) {
            return new Biography_summary(row.getBiographySummary());
        }
    }

    public record Story_title(String value) {
        public static Story_title from(CharacterAppearanceTable row) {
            return new Story_title(row.getStoryTitle());
        }
    }

    public record Story_body(String value) {
        public static Story_body from(CharacterAppearanceTable row) {
            return new Story_body(row.getStoryBody());
        }
    }

}