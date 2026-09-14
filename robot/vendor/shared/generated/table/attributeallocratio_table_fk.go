package table

import (
    pb "shared/generated/pb/table"
)

// ---------------------------------------------------------------------------
// Foreign key helpers for AttributeAllocRatioTable
// ---------------------------------------------------------------------------

// GetAttributeAllocRatioDimensionIdRow resolves AttributeAllocRatio.dimension_id -> AttributeDimension row.
func GetAttributeAllocRatioDimensionIdRow(row *pb.AttributeAllocRatioTable) (*pb.AttributeDimensionTable, bool) {
    return AttributeDimensionTableManagerInstance.FindById(row.DimensionId)
}

// GetAttributeAllocRatioDimensionIdRowById resolves AttributeAllocRatio.dimension_id -> AttributeDimension row (by AttributeAllocRatio id).
func GetAttributeAllocRatioDimensionIdRowById(tableId uint32) (*pb.AttributeDimensionTable, bool) {
    row, ok := AttributeAllocRatioTableManagerInstance.FindById(tableId)
    if !ok {
        return nil, false
    }
    return GetAttributeAllocRatioDimensionIdRow(row)
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

// FindAttributeAllocRatioRowsByDimensionId returns all AttributeAllocRatio rows whose dimension_id == key.
func FindAttributeAllocRatioRowsByDimensionId(key uint32) []*pb.AttributeAllocRatioTable {
    return AttributeAllocRatioTableManagerInstance.GetByDimensionId(key)
}
