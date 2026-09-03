
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for MonsterTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type MonsterIdComp struct {
    Value uint32
}

type MonsterHealthComp struct {
    Value uint64
}

type MonsterStrengthComp struct {
    Value uint64
}

type MonsterArmorComp struct {
    Value uint64
}

type MonsterResistanceComp struct {
    Value uint64
}

type MonsterCritchanceComp struct {
    Value uint64
}

type MonsterSpeedComp struct {
    Value uint64
}

type MonsterExp_rewardComp struct {
    Value uint64
}

type MonsterGold_rewardComp struct {
    Value uint64
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeMonsterIdComp(row *pb.MonsterTable) MonsterIdComp {
    return MonsterIdComp{Value: row.Id}
}

func MakeMonsterHealthComp(row *pb.MonsterTable) MonsterHealthComp {
    return MonsterHealthComp{Value: row.Health}
}

func MakeMonsterStrengthComp(row *pb.MonsterTable) MonsterStrengthComp {
    return MonsterStrengthComp{Value: row.Strength}
}

func MakeMonsterArmorComp(row *pb.MonsterTable) MonsterArmorComp {
    return MonsterArmorComp{Value: row.Armor}
}

func MakeMonsterResistanceComp(row *pb.MonsterTable) MonsterResistanceComp {
    return MonsterResistanceComp{Value: row.Resistance}
}

func MakeMonsterCritchanceComp(row *pb.MonsterTable) MonsterCritchanceComp {
    return MonsterCritchanceComp{Value: row.Critchance}
}

func MakeMonsterSpeedComp(row *pb.MonsterTable) MonsterSpeedComp {
    return MonsterSpeedComp{Value: row.Speed}
}

func MakeMonsterExp_rewardComp(row *pb.MonsterTable) MonsterExp_rewardComp {
    return MonsterExp_rewardComp{Value: row.ExpReward}
}

func MakeMonsterGold_rewardComp(row *pb.MonsterTable) MonsterGold_rewardComp {
    return MonsterGold_rewardComp{Value: row.GoldReward}
}

