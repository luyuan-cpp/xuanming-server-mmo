package team

import (
	"context"
	"testing"

	dspb "proto/data_service"
)

func TestTeamAppearanceComesFromPlayerDisplay(t *testing.T) {
	reader := presenceReader{displays: &displayDataService{players: map[uint64]*dspb.PlayerDisplay{
		42: {ClassId: 3, Name: "青云子", AppearanceId: "04_mountain_guardian_boy", Gender: 1},
	}}}
	display := reader.loadDisplay(context.Background(), []uint64{42, 43})
	v := memberView(42, 1, 0, 42, display)
	if v.GetAppearanceId() != "04_mountain_guardian_boy" || v.GetClassId() != 3 || v.GetGender() != 1 {
		t.Fatalf("队伍丢失人物身份: %v", v)
	}
	if v.GetName() != "青云子" {
		t.Fatalf("队伍视图没带上存档里的昵称: %q", v.GetName())
	}
	if other := display[43]; other.AppearanceId != "" || other.Name != "" {
		t.Fatal("不得从其他玩家的存档取外观或昵称")
	}
}

// 存档里没有昵称(新号尚未取名)时,视图里的 name 是空串,由客户端兜底显示。
func TestTeamMemberNameEmptyWhenProfileHasNoName(t *testing.T) {
	reader := presenceReader{displays: &displayDataService{players: map[uint64]*dspb.PlayerDisplay{
		7: {AppearanceId: "01_taoist_girl"},
	}}}
	if name := reader.loadDisplay(context.Background(), []uint64{7})[7].Name; name != "" {
		t.Fatalf("没有昵称时应返回空串,实际 %q", name)
	}
}
