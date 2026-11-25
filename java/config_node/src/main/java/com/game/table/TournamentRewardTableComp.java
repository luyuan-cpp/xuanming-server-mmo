
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for TournamentRewardTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class TournamentRewardTableComp {

    private TournamentRewardTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(TournamentRewardTable row) {
            return new Id(row.getId());
        }
    }

    public record Tier_kind(int value) {
        public static Tier_kind from(TournamentRewardTable row) {
            return new Tier_kind(row.getTierKind());
        }
    }

    public record Min_rank(int value) {
        public static Min_rank from(TournamentRewardTable row) {
            return new Min_rank(row.getMinRank());
        }
    }

    public record Max_rank(int value) {
        public static Max_rank from(TournamentRewardTable row) {
            return new Max_rank(row.getMaxRank());
        }
    }

    public record Gold_amount(long value) {
        public static Gold_amount from(TournamentRewardTable row) {
            return new Gold_amount(row.getGoldAmount());
        }
    }

    public record Title_code(String value) {
        public static Title_code from(TournamentRewardTable row) {
            return new Title_code(row.getTitleCode());
        }
    }

    public record Badge_code(String value) {
        public static Badge_code from(TournamentRewardTable row) {
            return new Badge_code(row.getBadgeCode());
        }
    }

    public record Mail_title(String value) {
        public static Mail_title from(TournamentRewardTable row) {
            return new Mail_title(row.getMailTitle());
        }
    }

    public record Mail_body(String value) {
        public static Mail_body from(TournamentRewardTable row) {
            return new Mail_body(row.getMailBody());
        }
    }

    public record Desc(String value) {
        public static Desc from(TournamentRewardTable row) {
            return new Desc(row.getDesc());
        }
    }

    public record Item_ids(List<Integer> values) {
        public static Item_ids from(TournamentRewardTable row) {
            return new Item_ids(row.getItemIdsList());
        }
    }

    public record Item_counts(List<Integer> values) {
        public static Item_counts from(TournamentRewardTable row) {
            return new Item_counts(row.getItemCountsList());
        }
    }

}