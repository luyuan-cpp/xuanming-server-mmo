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
		ProfileComponent:  &comppb.PlayerProfileComp{Name: "青云子", AppearanceId: "04_mountain_guardian_boy", Gender: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	level, classID, name, appearanceID, gender := parsePlayerBrief(context.Background(), 42, string(raw))
	v := memberView(42, 1, 0, 42, displayCache{42: {Level: level, ClassId: classID, Name: name, AppearanceId: appearanceID, Gender: gender}})
	if v.GetAppearanceId() != "04_mountain_guardian_boy" || v.GetClassId() != 3 || v.GetGender() != 1 {
		t.Fatalf("队伍丢失人物身份: %v", v)
	}
	if v.GetName() != "青云子" {
		t.Fatalf("队伍视图没带上存档里的昵称: %q", v.GetName())
	}
	_, _, wrongName, wrongID, _ := parsePlayerBrief(context.Background(), 43, string(raw))
	if wrongID != "" || wrongName != "" {
		t.Fatal("不得从其他玩家的存档取外观或昵称")
	}
}

// 存档里没有昵称(新号尚未取名 / 旧存档)时,视图里的 name 是空串,由客户端兜底显示,服务端不编造名字。
func TestTeamMemberNameEmptyWhenProfileHasNoName(t *testing.T) {
	raw, err := proto.Marshal(&dbpb.PlayerAllData{PlayerDatabaseData: &dbpb.PlayerDatabase{
		PlayerId:         7,
		ProfileComponent: &comppb.PlayerProfileComp{AppearanceId: "01_taoist_girl"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, name, _, _ := parsePlayerBrief(context.Background(), 7, string(raw))
	if name != "" {
		t.Fatalf("没有昵称时应返回空串,实际 %q", name)
	}
}
