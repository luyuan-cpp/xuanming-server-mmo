
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for ActorActionCombatStateTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class ActorActionCombatStateTableComp {

    private ActorActionCombatStateTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(ActorActionCombatStateTable row) {
            return new Id(row.getId());
        }
    }

    public record State_id(int value) {
        public static State_id from(ActorActionCombatStateTable row) {
            return new State_id(row.getStateId());
        }
    }

    public record State_mode(int value) {
        public static State_mode from(ActorActionCombatStateTable row) {
            return new State_mode(row.getStateMode());
        }
    }

    public record State_tip(int value) {
        public static State_tip from(ActorActionCombatStateTable row) {
            return new State_tip(row.getStateTip());
        }
    }

}