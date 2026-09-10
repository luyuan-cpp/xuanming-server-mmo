
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for PetTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type PetIdComp struct {
    Value uint32
}

type PetNameComp struct {
    Value string
}

type PetDescComp struct {
    Value string
}

type PetModel_idComp struct {
    Value uint32
}

type PetQualityComp struct {
    Value uint32
}

type PetUnlock_levelComp struct {
    Value uint32
}

type PetLevel_capComp struct {
    Value uint32
}

type PetInit_healthComp struct {
    Value uint64
}

type PetInit_manaComp struct {
    Value uint64
}

type PetInit_speedComp struct {
    Value uint64
}

type PetAptitude_minComp struct {
    Values []uint32
}

type PetAptitude_maxComp struct {
    Values []uint32
}

type PetSkillComp struct {
    Values []uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakePetIdComp(row *pb.PetTable) PetIdComp {
    return PetIdComp{Value: row.Id}
}

func MakePetNameComp(row *pb.PetTable) PetNameComp {
    return PetNameComp{Value: row.Name}
}

func MakePetDescComp(row *pb.PetTable) PetDescComp {
    return PetDescComp{Value: row.Desc}
}

func MakePetModel_idComp(row *pb.PetTable) PetModel_idComp {
    return PetModel_idComp{Value: row.ModelId}
}

func MakePetQualityComp(row *pb.PetTable) PetQualityComp {
    return PetQualityComp{Value: row.Quality}
}

func MakePetUnlock_levelComp(row *pb.PetTable) PetUnlock_levelComp {
    return PetUnlock_levelComp{Value: row.UnlockLevel}
}

func MakePetLevel_capComp(row *pb.PetTable) PetLevel_capComp {
    return PetLevel_capComp{Value: row.LevelCap}
}

func MakePetInit_healthComp(row *pb.PetTable) PetInit_healthComp {
    return PetInit_healthComp{Value: row.InitHealth}
}

func MakePetInit_manaComp(row *pb.PetTable) PetInit_manaComp {
    return PetInit_manaComp{Value: row.InitMana}
}

func MakePetInit_speedComp(row *pb.PetTable) PetInit_speedComp {
    return PetInit_speedComp{Value: row.InitSpeed}
}

func MakePetAptitude_minComp(row *pb.PetTable) PetAptitude_minComp {
    return PetAptitude_minComp{Values: row.AptitudeMin}
}

func MakePetAptitude_maxComp(row *pb.PetTable) PetAptitude_maxComp {
    return PetAptitude_maxComp{Values: row.AptitudeMax}
}

func MakePetSkillComp(row *pb.PetTable) PetSkillComp {
    return PetSkillComp{Values: row.Skill}
}

