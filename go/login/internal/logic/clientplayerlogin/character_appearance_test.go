package clientplayerloginlogic

import (
	"testing"

	"login/internal/logic/pkg/homezone"
	pbbase "proto/common/base"
	loginpb "proto/login"
	"shared/generated/pb/table"
)

func TestCharacterAppearanceRejectsRemovedAndMalformedIDs(t *testing.T) {
	for _, id := range []string{
		"", "00_reference_topright_boy", "01_ice_sword_girl", "02_fire_talisman_boy",
		"03_lotus_healer_girl", "04_mountain_guardian_boy", "05_celestial_musician_girl",
		"08_alchemy_prodigy_boy", "09_bamboo_archer_girl",
		"10_crimson_spear_girl", "15_water_dragon_scholar_boy", "17_ghost_script_calligrapher_boy",
	} {
		if !validCharacterAppearance(id) {
			t.Fatalf("保留身份被拒绝: %q", id)
		}
	}
	for _, id := range []string{
		"06_thunder_caster_boy", "07_moon_shadow_assassin_girl", "14_short_hair_snow_summoner_girl", "20_star_formation_master_girl",
		"11_removed", "04", "04_mountain_guardian_boy/../other", " 04_mountain_guardian_boy",
	} {
		if validCharacterAppearance(id) {
			t.Fatalf("非法或删除身份被放行: %q", id)
		}
	}
	if retiredCharacterAppearance("06_thunder_caster_boy") {
		t.Fatal("彻底删除的外观不能通过退役角色重试入口恢复")
	}
}

func TestRetiredAppearanceOnlyReturnsExistingExactRetry(t *testing.T) {
	for _, id := range []string{
		"07_moon_shadow_assassin_girl", "14_short_hair_snow_summoner_girl", "20_star_formation_master_girl",
	} {
		t.Run(id, func(t *testing.T) {
			fake := &fakeRegisterClient{}
			h := newCreatePlayerHarnessWithNames(t, homezone.New(fake, 0, 0, 0), fake)
			// 将发号器设为不可用，证明拒绝与兼容重试都能在发号前完成。
			h.l.svcCtx.PlayerIDMinter = nil
			directory := fixtureDirectory(t, h.rdb)
			role := &pbbase.AccountSimplePlayer{PlayerId: 42, Gender: 2, Name: "OldHero", AppearanceId: id, ZoneId: 3}
			directory.seed("other-account", role)

			assertRejected := func(name string, gender uint32) {
				t.Helper()
				response, err := h.l.CreatePlayer(&loginpb.CreatePlayerRequest{Name: name, Gender: gender, AppearanceId: id})
				if err != nil || response.GetErrorMessage().GetId() != uint32(table.LoginError_kLoginUnknownError) || len(response.GetPlayers()) != 0 {
					t.Fatalf("退役外观的新建或错配未拒绝: %v %v", response, err)
				}
			}
			assertRejected("OldHero", 2)

			directory.seed(hzAccount, role,
				&pbbase.AccountSimplePlayer{PlayerId: 43}, &pbbase.AccountSimplePlayer{PlayerId: 44},
				&pbbase.AccountSimplePlayer{PlayerId: 45}, &pbbase.AccountSimplePlayer{PlayerId: 46})
			assertRejected("NewHero", 2)
			assertRejected("oldhero", 2)
			assertRejected("OldHero", 1)
			assertRejected("", 2)

			response, err := h.l.CreatePlayer(&loginpb.CreatePlayerRequest{Name: " ＯｌｄＨｅｒｏ ", Gender: 2, AppearanceId: id})
			got := response.GetPlayers()
			if err != nil || response.GetErrorMessage().GetId() != 0 || len(got) != 5 ||
				got[0].GetPlayer().GetPlayerId() != role.GetPlayerId() ||
				got[0].GetPlayer().GetZoneId() != role.GetZoneId() ||
				got[0].GetPlayer().GetAppearanceId() != id {
				t.Fatalf("退役外观的已提交重试未原样返回: %v %v", response, err)
			}
			if len(fake.events) != 0 || len(storedPlayers(t, h.ctx, h.rdb)) != 5 {
				t.Fatalf("重试触发了新建写入: %v", fake.events)
			}
		})
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
