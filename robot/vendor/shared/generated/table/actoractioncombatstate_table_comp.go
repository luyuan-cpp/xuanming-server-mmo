
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for ActorActionCombatStateTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type ActorActionCombatStateIdComp struct {
    Value uint32
}

type ActorActionCombatStateState_idComp struct {
    Value uint32
}

type ActorActionCombatStateState_modeComp struct {
    Value uint32
}

type ActorActionCombatStateState_tipComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeActorActionCombatStateIdComp(row *pb.ActorActionCombatStateTable) ActorActionCombatStateIdComp {
    return ActorActionCombatStateIdComp{Value: row.Id}
}

func MakeActorActionCombatStateState_idComp(row *pb.ActorActionCombatStateTable) ActorActionCombatStateState_idComp {
    return ActorActionCombatStateState_idComp{Value: row.StateId}
}

func MakeActorActionCombatStateState_modeComp(row *pb.ActorActionCombatStateTable) ActorActionCombatStateState_modeComp {
    return ActorActionCombatStateState_modeComp{Value: row.StateMode}
}

func MakeActorActionCombatStateState_tipComp(row *pb.ActorActionCombatStateTable) ActorActionCombatStateState_tipComp {
    return ActorActionCombatStateState_tipComp{Value: row.StateTip}
}

