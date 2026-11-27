
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for ActorActionStateTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type ActorActionStateIdComp struct {
    Value uint32
}

type ActorActionStateState_idComp struct {
    Value uint32
}

type ActorActionStateState_modeComp struct {
    Value uint32
}

type ActorActionStateState_tipComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeActorActionStateIdComp(row *pb.ActorActionStateTable) ActorActionStateIdComp {
    return ActorActionStateIdComp{Value: row.Id}
}

func MakeActorActionStateState_idComp(row *pb.ActorActionStateTable) ActorActionStateState_idComp {
    return ActorActionStateState_idComp{Value: row.StateId}
}

func MakeActorActionStateState_modeComp(row *pb.ActorActionStateTable) ActorActionStateState_modeComp {
    return ActorActionStateState_modeComp{Value: row.StateMode}
}

func MakeActorActionStateState_tipComp(row *pb.ActorActionStateTable) ActorActionStateState_tipComp {
    return ActorActionStateState_tipComp{Value: row.StateTip}
}

