
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for TournamentRuleTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class TournamentRuleTableComp {

    private TournamentRuleTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(TournamentRuleTable row) {
            return new Id(row.getId());
        }
    }

    public record Rule_version(int value) {
        public static Rule_version from(TournamentRuleTable row) {
            return new Rule_version(row.getRuleVersion());
        }
    }

    public record Enabled(boolean value) {
        public static Enabled from(TournamentRuleTable row) {
            return new Enabled(row.getEnabled());
        }
    }

    public record Battle_config_id(int value) {
        public static Battle_config_id from(TournamentRuleTable row) {
            return new Battle_config_id(row.getBattleConfigId());
        }
    }

    public record Battle_team_size(int value) {
        public static Battle_team_size from(TournamentRuleTable row) {
            return new Battle_team_size(row.getBattleTeamSize());
        }
    }

    public record Roster_max_size(int value) {
        public static Roster_max_size from(TournamentRuleTable row) {
            return new Roster_max_size(row.getRosterMaxSize());
        }
    }

    public record Min_level(int value) {
        public static Min_level from(TournamentRuleTable row) {
            return new Min_level(row.getMinLevel());
        }
    }

    public record Registration_days(int value) {
        public static Registration_days from(TournamentRuleTable row) {
            return new Registration_days(row.getRegistrationDays());
        }
    }

    public record Qualifier_days(int value) {
        public static Qualifier_days from(TournamentRuleTable row) {
            return new Qualifier_days(row.getQualifierDays());
        }
    }

    public record Knockout_days(int value) {
        public static Knockout_days from(TournamentRuleTable row) {
            return new Knockout_days(row.getKnockoutDays());
        }
    }

    public record Settle_display_days(int value) {
        public static Settle_display_days from(TournamentRuleTable row) {
            return new Settle_display_days(row.getSettleDisplayDays());
        }
    }

    public record Qualifier_max_matches(int value) {
        public static Qualifier_max_matches from(TournamentRuleTable row) {
            return new Qualifier_max_matches(row.getQualifierMaxMatches());
        }
    }

    public record Participation_min_matches(int value) {
        public static Participation_min_matches from(TournamentRuleTable row) {
            return new Participation_min_matches(row.getParticipationMinMatches());
        }
    }

    public record Knockout_max_teams(int value) {
        public static Knockout_max_teams from(TournamentRuleTable row) {
            return new Knockout_max_teams(row.getKnockoutMaxTeams());
        }
    }

    public record Knockout_round_timeout_seconds(int value) {
        public static Knockout_round_timeout_seconds from(TournamentRuleTable row) {
            return new Knockout_round_timeout_seconds(row.getKnockoutRoundTimeoutSeconds());
        }
    }

    public record Qualifier_win_points(int value) {
        public static Qualifier_win_points from(TournamentRuleTable row) {
            return new Qualifier_win_points(row.getQualifierWinPoints());
        }
    }

    public record Qualifier_draw_points(int value) {
        public static Qualifier_draw_points from(TournamentRuleTable row) {
            return new Qualifier_draw_points(row.getQualifierDrawPoints());
        }
    }

    public record Qualifier_loss_points(int value) {
        public static Qualifier_loss_points from(TournamentRuleTable row) {
            return new Qualifier_loss_points(row.getQualifierLossPoints());
        }
    }

    public record Invite_ttl_seconds(int value) {
        public static Invite_ttl_seconds from(TournamentRuleTable row) {
            return new Invite_ttl_seconds(row.getInviteTtlSeconds());
        }
    }

    public record Desc(String value) {
        public static Desc from(TournamentRuleTable row) {
            return new Desc(row.getDesc());
        }
    }

}