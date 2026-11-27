
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for FestivalBeastTierTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type FestivalBeastTierIdComp struct {
    Value uint32
}

type FestivalBeastTierMin_levelComp struct {
    Value uint32
}

type FestivalBeastTierMax_levelComp struct {
    Value uint32
}

type FestivalBeastTierBoss_healthComp struct {
    Value uint64
}

type FestivalBeastTierBoss_physical_attackComp struct {
    Value uint64
}

type FestivalBeastTierBoss_defenseComp struct {
    Value uint64
}

type FestivalBeastTierBoss_resistanceComp struct {
    Value uint64
}

type FestivalBeastTierBoss_speedComp struct {
    Value uint64
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeFestivalBeastTierIdComp(row *pb.FestivalBeastTierTable) FestivalBeastTierIdComp {
    return FestivalBeastTierIdComp{Value: row.Id}
}

func MakeFestivalBeastTierMin_levelComp(row *pb.FestivalBeastTierTable) FestivalBeastTierMin_levelComp {
    return FestivalBeastTierMin_levelComp{Value: row.MinLevel}
}

func MakeFestivalBeastTierMax_levelComp(row *pb.FestivalBeastTierTable) FestivalBeastTierMax_levelComp {
    return FestivalBeastTierMax_levelComp{Value: row.MaxLevel}
}

func MakeFestivalBeastTierBoss_healthComp(row *pb.FestivalBeastTierTable) FestivalBeastTierBoss_healthComp {
    return FestivalBeastTierBoss_healthComp{Value: row.BossHealth}
}

func MakeFestivalBeastTierBoss_physical_attackComp(row *pb.FestivalBeastTierTable) FestivalBeastTierBoss_physical_attackComp {
    return FestivalBeastTierBoss_physical_attackComp{Value: row.BossPhysicalAttack}
}

func MakeFestivalBeastTierBoss_defenseComp(row *pb.FestivalBeastTierTable) FestivalBeastTierBoss_defenseComp {
    return FestivalBeastTierBoss_defenseComp{Value: row.BossDefense}
}

func MakeFestivalBeastTierBoss_resistanceComp(row *pb.FestivalBeastTierTable) FestivalBeastTierBoss_resistanceComp {
    return FestivalBeastTierBoss_resistanceComp{Value: row.BossResistance}
}

func MakeFestivalBeastTierBoss_speedComp(row *pb.FestivalBeastTierTable) FestivalBeastTierBoss_speedComp {
    return FestivalBeastTierBoss_speedComp{Value: row.BossSpeed}
}

