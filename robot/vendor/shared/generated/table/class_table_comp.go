
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for ClassTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type ClassIdComp struct {
    Value uint32
}

type ClassInit_healthComp struct {
    Value uint64
}

type ClassInit_manaComp struct {
    Value uint64
}

type ClassInit_strengthComp struct {
    Value uint64
}

type ClassInit_armorComp struct {
    Value uint64
}

type ClassInit_resistanceComp struct {
    Value uint64
}

type ClassInit_critchanceComp struct {
    Value uint64
}

type ClassInit_speedComp struct {
    Value uint64
}

type ClassSkillComp struct {
    Values []uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeClassIdComp(row *pb.ClassTable) ClassIdComp {
    return ClassIdComp{Value: row.Id}
}

func MakeClassInit_healthComp(row *pb.ClassTable) ClassInit_healthComp {
    return ClassInit_healthComp{Value: row.InitHealth}
}

func MakeClassInit_manaComp(row *pb.ClassTable) ClassInit_manaComp {
    return ClassInit_manaComp{Value: row.InitMana}
}

func MakeClassInit_strengthComp(row *pb.ClassTable) ClassInit_strengthComp {
    return ClassInit_strengthComp{Value: row.InitStrength}
}

func MakeClassInit_armorComp(row *pb.ClassTable) ClassInit_armorComp {
    return ClassInit_armorComp{Value: row.InitArmor}
}

func MakeClassInit_resistanceComp(row *pb.ClassTable) ClassInit_resistanceComp {
    return ClassInit_resistanceComp{Value: row.InitResistance}
}

func MakeClassInit_critchanceComp(row *pb.ClassTable) ClassInit_critchanceComp {
    return ClassInit_critchanceComp{Value: row.InitCritchance}
}

func MakeClassInit_speedComp(row *pb.ClassTable) ClassInit_speedComp {
    return ClassInit_speedComp{Value: row.InitSpeed}
}

func MakeClassSkillComp(row *pb.ClassTable) ClassSkillComp {
    return ClassSkillComp{Values: row.Skill}
}

