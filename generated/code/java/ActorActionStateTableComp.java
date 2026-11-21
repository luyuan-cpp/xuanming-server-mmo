
package com.game.table;

import java.util.List;

/**
 * Auto-generated per-column component records for ActorActionStateTable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public final class ActorActionStateTableComp {

    private ActorActionStateTableComp() {}

    // ============================================================
    // Scalar columns → value records
    // Repeated columns → list records
    // ============================================================


    public record Id(int value) {
        public static Id from(ActorActionStateTable row) {
            return new Id(row.getId());
        }
    }

    public record State_id(int value) {
        public static State_id from(ActorActionStateTable row) {
            return new State_id(row.getStateId());
        }
    }

    public record State_mode(int value) {
        public static State_mode from(ActorActionStateTable row) {
            return new State_mode(row.getStateMode());
        }
    }

    public record State_tip(int value) {
        public static State_tip from(ActorActionStateTable row) {
            return new State_tip(row.getStateTip());
        }
    }

}