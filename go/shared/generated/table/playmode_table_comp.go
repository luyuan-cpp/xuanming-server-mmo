
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for PlayModeTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type PlayModeIdComp struct {
    Value uint32
}

type PlayModeMatch_modeComp struct {
    Value uint32
}

type PlayModeBattle_config_idComp struct {
    Value uint32
}

type PlayModeEnabledComp struct {
    Value bool
}

type PlayModeTeam_size_maxComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakePlayModeIdComp(row *pb.PlayModeTable) PlayModeIdComp {
    return PlayModeIdComp{Value: row.Id}
}

func MakePlayModeMatch_modeComp(row *pb.PlayModeTable) PlayModeMatch_modeComp {
    return PlayModeMatch_modeComp{Value: row.MatchMode}
}

func MakePlayModeBattle_config_idComp(row *pb.PlayModeTable) PlayModeBattle_config_idComp {
    return PlayModeBattle_config_idComp{Value: row.BattleConfigId}
}

func MakePlayModeEnabledComp(row *pb.PlayModeTable) PlayModeEnabledComp {
    return PlayModeEnabledComp{Value: row.Enabled}
}

func MakePlayModeTeam_size_maxComp(row *pb.PlayModeTable) PlayModeTeam_size_maxComp {
    return PlayModeTeam_size_maxComp{Value: row.TeamSizeMax}
}

