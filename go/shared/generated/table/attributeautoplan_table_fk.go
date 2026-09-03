package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for AttributeAutoPlanTable
// ---------------------------------------------------------------------------

// GetAttributeAutoPlanPoolIdRow resolves AttributeAutoPlan.pool_id -> AttributePool row.
func GetAttributeAutoPlanPoolIdRow(row *pb.AttributeAutoPlanTable) (*pb.AttributePoolTable, bool) {
    return AttributePoolTableManagerInstance.FindById(row.PoolId)
}

// GetAttributeAutoPlanPoolIdRowById resolves AttributeAutoPlan.pool_id -> AttributePool row (by AttributeAutoPlan id).
func GetAttributeAutoPlanPoolIdRowById(tableId uint32) (*pb.AttributePoolTable, bool) {
    row, ok := AttributeAutoPlanTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetAttributeAutoPlanPoolIdRow(row)
}

// GetAttributeAutoPlanDimensionRows resolves AttributeAutoPlan.dimension[] -> AttributeDimension rows.
func GetAttributeAutoPlanDimensionRows(row *pb.AttributeAutoPlanTable) []*pb.AttributeDimensionTable {
    var result []*pb.AttributeDimensionTable
    for _, id := range row.Dimension {
        if r, ok := AttributeDimensionTableManagerInstance.FindById(id); ok {
            result = append(result, r)
        }
    }
    return result
}

// GetAttributeAutoPlanDimensionRowsById resolves AttributeAutoPlan.dimension[] -> AttributeDimension rows (by AttributeAutoPlan id).
func GetAttributeAutoPlanDimensionRowsById(tableId uint32) []*pb.AttributeDimensionTable {
    row, ok := AttributeAutoPlanTableManagerInstance.FindById(tableId)
    if !ok {
        return nil
    }
    return GetAttributeAutoPlanDimensionRows(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindAttributeAutoPlanRowsByPoolId returns all AttributeAutoPlan rows whose pool_id == key.
func FindAttributeAutoPlanRowsByPoolId(key uint32) []*pb.AttributeAutoPlanTable {
    return AttributeAutoPlanTableManagerInstance.GetByPoolId(key)
}
