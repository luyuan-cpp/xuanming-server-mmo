package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for FestivalBeastActivityTable
// ---------------------------------------------------------------------------

// GetFestivalBeastActivityBattleConfigIdRow resolves FestivalBeastActivity.battle_config_id -> Dungeon row.
func GetFestivalBeastActivityBattleConfigIdRow(row *pb.FestivalBeastActivityTable) (*pb.DungeonTable, bool) {
    return DungeonTableManagerInstance.FindById(row.BattleConfigId)
}

// GetFestivalBeastActivityBattleConfigIdRowById resolves FestivalBeastActivity.battle_config_id -> Dungeon row (by FestivalBeastActivity id).
func GetFestivalBeastActivityBattleConfigIdRowById(tableId uint32) (*pb.DungeonTable, bool) {
    row, ok := FestivalBeastActivityTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetFestivalBeastActivityBattleConfigIdRow(row)
}

// GetFestivalBeastActivityRewardPetTableIdRow resolves FestivalBeastActivity.reward_pet_table_id -> Pet row.
func GetFestivalBeastActivityRewardPetTableIdRow(row *pb.FestivalBeastActivityTable) (*pb.PetTable, bool) {
    return PetTableManagerInstance.FindById(row.RewardPetTableId)
}

// GetFestivalBeastActivityRewardPetTableIdRowById resolves FestivalBeastActivity.reward_pet_table_id -> Pet row (by FestivalBeastActivity id).
func GetFestivalBeastActivityRewardPetTableIdRowById(tableId uint32) (*pb.PetTable, bool) {
    row, ok := FestivalBeastActivityTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetFestivalBeastActivityRewardPetTableIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindFestivalBeastActivityRowsByBattleConfigId returns all FestivalBeastActivity rows whose battle_config_id == key.
func FindFestivalBeastActivityRowsByBattleConfigId(key uint32) []*pb.FestivalBeastActivityTable {
    return FestivalBeastActivityTableManagerInstance.GetByBattleConfigId(key)
}

// FindFestivalBeastActivityRowsByRewardPetTableId returns all FestivalBeastActivity rows whose reward_pet_table_id == key.
func FindFestivalBeastActivityRowsByRewardPetTableId(key uint32) []*pb.FestivalBeastActivityTable {
    return FestivalBeastActivityTableManagerInstance.GetByRewardPetTableId(key)
}
