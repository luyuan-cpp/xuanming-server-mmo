
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for CharacterAppearanceTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type CharacterAppearanceIdComp struct {
    Value uint32
}

type CharacterAppearanceAppearance_idComp struct {
    Value string
}

type CharacterAppearanceNameComp struct {
    Value string
}

type CharacterAppearanceEnabledComp struct {
    Value uint32
}

type CharacterAppearanceSort_orderComp struct {
    Value uint32
}

type CharacterAppearanceDescComp struct {
    Value string
}

type CharacterAppearanceStarter_creatableComp struct {
    Value uint32
}

type CharacterAppearanceCard_unlockableComp struct {
    Value uint32
}

type CharacterAppearanceSeries_idComp struct {
    Value uint32
}

type CharacterAppearanceArchive_nameComp struct {
    Value string
}

type CharacterAppearanceNicknameComp struct {
    Value string
}

type CharacterAppearanceBiography_summaryComp struct {
    Value string
}

type CharacterAppearanceStory_titleComp struct {
    Value string
}

type CharacterAppearanceStory_bodyComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeCharacterAppearanceIdComp(row *pb.CharacterAppearanceTable) CharacterAppearanceIdComp {
    return CharacterAppearanceIdComp{Value: row.Id}
}

func MakeCharacterAppearanceAppearance_idComp(row *pb.CharacterAppearanceTable) CharacterAppearanceAppearance_idComp {
    return CharacterAppearanceAppearance_idComp{Value: row.AppearanceId}
}

func MakeCharacterAppearanceNameComp(row *pb.CharacterAppearanceTable) CharacterAppearanceNameComp {
    return CharacterAppearanceNameComp{Value: row.Name}
}

func MakeCharacterAppearanceEnabledComp(row *pb.CharacterAppearanceTable) CharacterAppearanceEnabledComp {
    return CharacterAppearanceEnabledComp{Value: row.Enabled}
}

func MakeCharacterAppearanceSort_orderComp(row *pb.CharacterAppearanceTable) CharacterAppearanceSort_orderComp {
    return CharacterAppearanceSort_orderComp{Value: row.SortOrder}
}

func MakeCharacterAppearanceDescComp(row *pb.CharacterAppearanceTable) CharacterAppearanceDescComp {
    return CharacterAppearanceDescComp{Value: row.Desc}
}

func MakeCharacterAppearanceStarter_creatableComp(row *pb.CharacterAppearanceTable) CharacterAppearanceStarter_creatableComp {
    return CharacterAppearanceStarter_creatableComp{Value: row.StarterCreatable}
}

func MakeCharacterAppearanceCard_unlockableComp(row *pb.CharacterAppearanceTable) CharacterAppearanceCard_unlockableComp {
    return CharacterAppearanceCard_unlockableComp{Value: row.CardUnlockable}
}

func MakeCharacterAppearanceSeries_idComp(row *pb.CharacterAppearanceTable) CharacterAppearanceSeries_idComp {
    return CharacterAppearanceSeries_idComp{Value: row.SeriesId}
}

func MakeCharacterAppearanceArchive_nameComp(row *pb.CharacterAppearanceTable) CharacterAppearanceArchive_nameComp {
    return CharacterAppearanceArchive_nameComp{Value: row.ArchiveName}
}

func MakeCharacterAppearanceNicknameComp(row *pb.CharacterAppearanceTable) CharacterAppearanceNicknameComp {
    return CharacterAppearanceNicknameComp{Value: row.Nickname}
}

func MakeCharacterAppearanceBiography_summaryComp(row *pb.CharacterAppearanceTable) CharacterAppearanceBiography_summaryComp {
    return CharacterAppearanceBiography_summaryComp{Value: row.BiographySummary}
}

func MakeCharacterAppearanceStory_titleComp(row *pb.CharacterAppearanceTable) CharacterAppearanceStory_titleComp {
    return CharacterAppearanceStory_titleComp{Value: row.StoryTitle}
}

func MakeCharacterAppearanceStory_bodyComp(row *pb.CharacterAppearanceTable) CharacterAppearanceStory_bodyComp {
    return CharacterAppearanceStory_bodyComp{Value: row.StoryBody}
}

