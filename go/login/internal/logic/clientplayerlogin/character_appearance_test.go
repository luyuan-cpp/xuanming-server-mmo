package clientplayerloginlogic

import (
	"testing"

	"login/internal/logic/pkg/homezone"
	pbbase "proto/common/base"
	loginpb "proto/login"
)

func TestCharacterAppearanceRejectsRemovedAndMalformedIDs(t *testing.T) {
	for _, id := range []string{"", "04_mountain_guardian_boy", "20_star_formation_master_girl"} {
		if !validCharacterAppearance(id) {
			t.Fatalf("保留身份被拒绝: %q", id)
		}
	}
	for _, id := range []string{"11_removed", "04", "04_mountain_guardian_boy/../other", " 04_mountain_guardian_boy"} {
		if validCharacterAppearance(id) {
			t.Fatalf("非法或删除身份被放行: %q", id)
		}
	}
}

func TestCreateRetryRequiresSameAppearance(t *testing.T) {
	players := []*pbbase.AccountSimplePlayer{{PlayerId: 1, ClassId: 3, Gender: 1, AppearanceId: "04_mountain_guardian_boy"}}
	if !isLostResponseRetry(players, 1, 3, 1, "04_mountain_guardian_boy") {
		t.Fatal("同一次建角重试应保留已创建身份")
	}
	if isLostResponseRetry(players, 1, 3, 1, "06_thunder_caster_boy") || isLostResponseRetry(players, 1, 3, 1, "") {
		t.Fatal("相同职业性别不能代替外观身份")
	}
}

func TestCreatePlayerAppearanceIsSavedInAccountAndResponse(t *testing.T) {
	fake := &fakeRegisterClient{}
	h := newCreatePlayerHarnessWithNames(t, homezone.New(fake, 0, 0, 0), fake)
	response, err := h.l.CreatePlayer(&loginpb.CreatePlayerRequest{AppearanceId: "04_mountain_guardian_boy"})
	if err != nil || response.GetErrorMessage().GetId() != 0 {
		t.Fatalf("建角失败: %v %v", err, response.GetErrorMessage())
	}
	players := storedPlayers(t, h.ctx, h.rdb)
	if len(players) != 1 || players[0].GetAppearanceId() != "04_mountain_guardian_boy" {
		t.Fatalf("账号持久化丢失外观: %v", players)
	}
	if len(response.GetPlayers()) != 1 || response.GetPlayers()[0].GetPlayer().GetAppearanceId() != players[0].GetAppearanceId() {
		t.Fatal("选角响应与已保存人物身份不一致")
	}
}
