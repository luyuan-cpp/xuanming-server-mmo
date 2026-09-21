package team

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"
	comppb "proto/common/component"
	dbpb "proto/common/database"
)

func TestTeamAppearanceComesFromSamePlayerBlob(t *testing.T) {
	raw, err := proto.Marshal(&dbpb.PlayerAllData{PlayerDatabaseData: &dbpb.PlayerDatabase{
		PlayerId:          42,
		Uint32PbComponent: &comppb.PlayerUint32Comp{Class: 3},
		ProfileComponent:  &comppb.PlayerProfileComp{AppearanceId: "04_mountain_guardian_boy", Gender: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	level, classID, appearanceID, gender := parsePlayerBrief(context.Background(), 42, string(raw))
	v := memberView(42, 1, 0, 42, displayCache{42: {Level: level, ClassId: classID, AppearanceId: appearanceID, Gender: gender}})
	if v.GetAppearanceId() != "04_mountain_guardian_boy" || v.GetClassId() != 3 || v.GetGender() != 1 {
		t.Fatalf("队伍丢失人物身份: %v", v)
	}
	_, _, wrongID, _ := parsePlayerBrief(context.Background(), 43, string(raw))
	if wrongID != "" {
		t.Fatal("不得从其他玩家的存档取外观")
	}
}
