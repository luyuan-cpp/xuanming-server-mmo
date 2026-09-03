package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for DungeonTable
// ---------------------------------------------------------------------------

// GetDungeonSceneIdRow resolves Dungeon.scene_id -> BaseScene row.
func GetDungeonSceneIdRow(row *pb.DungeonTable) (*pb.BaseSceneTable, bool) {
    return BaseSceneTableManagerInstance.FindById(row.SceneId)
}

// GetDungeonSceneIdRowById resolves Dungeon.scene_id -> BaseScene row (by Dungeon id).
func GetDungeonSceneIdRowById(tableId uint32) (*pb.BaseSceneTable, bool) {
    row, ok := DungeonTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetDungeonSceneIdRow(row)
}

// GetDungeonMonsterRows resolves Dungeon.monster[] -> Monster rows.
func GetDungeonMonsterRows(row *pb.DungeonTable) []*pb.MonsterTable {
    var result []*pb.MonsterTable
    for _, id := range row.Monster {
        if r, ok := MonsterTableManagerInstance.FindById(id); ok {
            result = append(result, r)
        }
    }
    return result
}

// GetDungeonMonsterRowsById resolves Dungeon.monster[] -> Monster rows (by Dungeon id).
func GetDungeonMonsterRowsById(tableId uint32) []*pb.MonsterTable {
    row, ok := DungeonTableManagerInstance.FindById(tableId)
    if !ok {
        return nil
    }
    return GetDungeonMonsterRows(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindDungeonRowsBySceneId returns all Dungeon rows whose scene_id == key.
func FindDungeonRowsBySceneId(key uint32) []*pb.DungeonTable {
    return DungeonTableManagerInstance.GetBySceneId(key)
}
