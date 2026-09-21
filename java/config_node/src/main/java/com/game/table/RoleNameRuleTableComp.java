
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for RoleNameRuleTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class RoleNameRuleTableComp {

    private RoleNameRuleTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(RoleNameRuleTable row) {
            return new Id(row.getId());
        }
    }

    public record Min_chars(int value) {
        public static Min_chars from(RoleNameRuleTable row) {
            return new Min_chars(row.getMinChars());
        }
    }

    public record Max_chars(int value) {
        public static Max_chars from(RoleNameRuleTable row) {
            return new Max_chars(row.getMaxChars());
        }
    }

    public record Generated_prefix(String value) {
        public static Generated_prefix from(RoleNameRuleTable row) {
            return new Generated_prefix(row.getGeneratedPrefix());
        }
    }

    public record Generated_suffix_len(int value) {
        public static Generated_suffix_len from(RoleNameRuleTable row) {
            return new Generated_suffix_len(row.getGeneratedSuffixLen());
        }
    }

    public record Max_generate_attempts(int value) {
        public static Max_generate_attempts from(RoleNameRuleTable row) {
            return new Max_generate_attempts(row.getMaxGenerateAttempts());
        }
    }

    public record Desc(String value) {
        public static Desc from(RoleNameRuleTable row) {
            return new Desc(row.getDesc());
        }
    }

}