package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for AttributeDimensionTable
// ---------------------------------------------------------------------------

// GetAttributeDimensionPoolIdRow resolves AttributeDimension.pool_id -> AttributePool row.
func GetAttributeDimensionPoolIdRow(row *pb.AttributeDimensionTable) (*pb.AttributePoolTable, bool) {
    return AttributePoolTableManagerInstance.FindById(row.PoolId)
}

// GetAttributeDimensionPoolIdRowById resolves AttributeDimension.pool_id -> AttributePool row (by AttributeDimension id).
func GetAttributeDimensionPoolIdRowById(tableId uint32) (*pb.AttributePoolTable, bool) {
    row, ok := AttributeDimensionTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetAttributeDimensionPoolIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindAttributeDimensionRowsByPoolId returns all AttributeDimension rows whose pool_id == key.
func FindAttributeDimensionRowsByPoolId(key uint32) []*pb.AttributeDimensionTable {
    return AttributeDimensionTableManagerInstance.GetByPoolId(key)
}
