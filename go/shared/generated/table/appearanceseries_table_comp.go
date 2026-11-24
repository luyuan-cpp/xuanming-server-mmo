
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for AppearanceSeriesTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type AppearanceSeriesIdComp struct {
    Value uint32
}

type AppearanceSeriesSeries_keyComp struct {
    Value string
}

type AppearanceSeriesNameComp struct {
    Value string
}

type AppearanceSeriesSort_orderComp struct {
    Value uint32
}

type AppearanceSeriesEnabledComp struct {
    Value uint32
}

type AppearanceSeriesDescComp struct {
    Value string
}

type AppearanceSeriesCompletion_badge_nameComp struct {
    Value string
}

type AppearanceSeriesEpilogue_titleComp struct {
    Value string
}

type AppearanceSeriesEpilogue_bodyComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeAppearanceSeriesIdComp(row *pb.AppearanceSeriesTable) AppearanceSeriesIdComp {
    return AppearanceSeriesIdComp{Value: row.Id}
}

func MakeAppearanceSeriesSeries_keyComp(row *pb.AppearanceSeriesTable) AppearanceSeriesSeries_keyComp {
    return AppearanceSeriesSeries_keyComp{Value: row.SeriesKey}
}

func MakeAppearanceSeriesNameComp(row *pb.AppearanceSeriesTable) AppearanceSeriesNameComp {
    return AppearanceSeriesNameComp{Value: row.Name}
}

func MakeAppearanceSeriesSort_orderComp(row *pb.AppearanceSeriesTable) AppearanceSeriesSort_orderComp {
    return AppearanceSeriesSort_orderComp{Value: row.SortOrder}
}

func MakeAppearanceSeriesEnabledComp(row *pb.AppearanceSeriesTable) AppearanceSeriesEnabledComp {
    return AppearanceSeriesEnabledComp{Value: row.Enabled}
}

func MakeAppearanceSeriesDescComp(row *pb.AppearanceSeriesTable) AppearanceSeriesDescComp {
    return AppearanceSeriesDescComp{Value: row.Desc}
}

func MakeAppearanceSeriesCompletion_badge_nameComp(row *pb.AppearanceSeriesTable) AppearanceSeriesCompletion_badge_nameComp {
    return AppearanceSeriesCompletion_badge_nameComp{Value: row.CompletionBadgeName}
}

func MakeAppearanceSeriesEpilogue_titleComp(row *pb.AppearanceSeriesTable) AppearanceSeriesEpilogue_titleComp {
    return AppearanceSeriesEpilogue_titleComp{Value: row.EpilogueTitle}
}

func MakeAppearanceSeriesEpilogue_bodyComp(row *pb.AppearanceSeriesTable) AppearanceSeriesEpilogue_bodyComp {
    return AppearanceSeriesEpilogue_bodyComp{Value: row.EpilogueBody}
}

