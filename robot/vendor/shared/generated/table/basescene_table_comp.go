
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for BaseSceneTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type BaseSceneIdComp struct {
    Value uint32
}

type BaseSceneNav_bin_fileComp struct {
    Value string
}

type BaseSceneSpawn_xComp struct {
    Value float64
}

type BaseSceneSpawn_yComp struct {
    Value float64
}

type BaseSceneSpawn_zComp struct {
    Value float64
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeBaseSceneIdComp(row *pb.BaseSceneTable) BaseSceneIdComp {
    return BaseSceneIdComp{Value: row.Id}
}

func MakeBaseSceneNav_bin_fileComp(row *pb.BaseSceneTable) BaseSceneNav_bin_fileComp {
    return BaseSceneNav_bin_fileComp{Value: row.NavBinFile}
}

func MakeBaseSceneSpawn_xComp(row *pb.BaseSceneTable) BaseSceneSpawn_xComp {
    return BaseSceneSpawn_xComp{Value: row.SpawnX}
}

func MakeBaseSceneSpawn_yComp(row *pb.BaseSceneTable) BaseSceneSpawn_yComp {
    return BaseSceneSpawn_yComp{Value: row.SpawnY}
}

func MakeBaseSceneSpawn_zComp(row *pb.BaseSceneTable) BaseSceneSpawn_zComp {
    return BaseSceneSpawn_zComp{Value: row.SpawnZ}
}

