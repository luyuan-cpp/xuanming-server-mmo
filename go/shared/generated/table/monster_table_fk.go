package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for MonsterTable
// ---------------------------------------------------------------------------

// GetMonsterCapturePetTableIdRow resolves Monster.capture_pet_table_id -> Pet row.
func GetMonsterCapturePetTableIdRow(row *pb.MonsterTable) (*pb.PetTable, bool) {
    return PetTableManagerInstance.FindById(row.CapturePetTableId)
}

// GetMonsterCapturePetTableIdRowById resolves Monster.capture_pet_table_id -> Pet row (by Monster id).
func GetMonsterCapturePetTableIdRowById(tableId uint32) (*pb.PetTable, bool) {
    row, ok := MonsterTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetMonsterCapturePetTableIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindMonsterRowsByCapturePetTableId returns all Monster rows whose capture_pet_table_id == key.
func FindMonsterRowsByCapturePetTableId(key uint32) []*pb.MonsterTable {
    return MonsterTableManagerInstance.GetByCapturePetTableId(key)
}
