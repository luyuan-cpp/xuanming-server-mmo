package clientplayerloginlogic

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"login/internal/constants"
	"login/internal/logic/pkg/homezone"
	pbbase "proto/common/base"
	comppb "proto/common/component"
	dbpb "proto/common/database"
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

func TestBackfillAppearancePersistsAndKeepsStoredIdentity(t *testing.T) {
	l, rdb, _, _ := newIdentityBackfillHarness(t, &dbpb.PlayerDatabase{PlayerId: identityPlayerID})
	state := identityState(3, "云中君")
	state.appearanceID = "04_mountain_guardian_boy"
	state.gender = 1
	if err := l.backfillPlayerIdentity(context.Background(), nil, state); err != nil {
		t.Fatal(err)
	}
	_, player := readIdentityBlob(t, rdb)
	if got := player.GetProfileComponent(); got.GetAppearanceId() != state.appearanceID || got.GetGender() != 1 {
		t.Fatalf("外观未随存档往返: %v", got)
	}
	state.appearanceID = "06_thunder_caster_boy"
	state.gender = 2
	if err := l.backfillPlayerIdentity(context.Background(), nil, state); err == nil {
		t.Fatal("非空身份冲突必须拒绝入场")
	}
	_, player = readIdentityBlob(t, rdb)
	if got := player.GetProfileComponent(); got.GetAppearanceId() != "04_mountain_guardian_boy" || got.GetGender() != 1 {
		t.Fatalf("重登补齐覆盖了已有身份: %v", got)
	}
}

func TestSelfHealedRoleRecoversAppearanceWithoutMutatingOtherRoles(t *testing.T) {
	l, rdb, mr, _ := newIdentityBackfillHarness(t, &dbpb.PlayerDatabase{
		PlayerId: identityPlayerID, Uint32PbComponent: &comppb.PlayerUint32Comp{Class: 3},
		ProfileComponent: &comppb.PlayerProfileComp{AppearanceId: "04_mountain_guardian_boy", Gender: 1},
	})
	account := &dbpb.UserAccounts{SimplePlayers: &pbbase.AccountSimplePlayerList{Players: []*pbbase.AccountSimplePlayer{
		{PlayerId: identityPlayerID, Name: "恢复角色"}, {PlayerId: 7002, AppearanceId: "06_thunder_caster_boy"},
	}}}
	blob, err := proto.Marshal(account)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := rdb.Set(ctx, constants.GetAccountDataKey("appearance-recovery"), blob, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	roles := []*loginpb.AccountSimplePlayerWrapper{{Player: account.SimplePlayers.Players[0]}}
	fillMissingRoleAppearances(ctx, rdb, roles)
	if roles[0].Player.AppearanceId != "04_mountain_guardian_boy" || account.SimplePlayers.Players[0].AppearanceId != "" {
		t.Fatal("选角恢复必须补展示副本，不能原地改账号")
	}
	state := identityState(0, "")
	state.account = "appearance-recovery"
	if err := l.backfillPlayerIdentity(ctx, nil, state); err != nil {
		t.Fatal(err)
	}
	raw, err := rdb.Get(ctx, constants.GetAccountDataKey(state.account)).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(raw, account); err != nil {
		t.Fatal(err)
	}
	if got := account.SimplePlayers.Players[0]; got.AppearanceId != "04_mountain_guardian_boy" || got.ClassId != 3 || got.Gender != 1 {
		t.Fatalf("self-heal身份未修复: %v", got)
	}
	if account.SimplePlayers.Players[1].AppearanceId != "06_thunder_caster_boy" {
		t.Fatal("修复覆盖了其他人物")
	}
	if mr.TTL(identityBlobKey()) <= 0 {
		t.Fatal("外观修复不能取消 PlayerAllData 缓存 TTL")
	}
	mr.FastForward(48 * time.Hour)
	if saved, err := rdb.Get(ctx, constants.GetAccountDataKey(state.account)).Bytes(); err != nil || !proto.Equal(account, mustUnmarshalAccount(t, saved)) {
		t.Fatalf("修复后的账号目录未长期保留: %v", err)
	}
}

func TestBackfillLegacyAppearanceRemainsEmpty(t *testing.T) {
	l, rdb, _, _ := newIdentityBackfillHarness(t, &dbpb.PlayerDatabase{PlayerId: identityPlayerID, ProfileComponent: &comppb.PlayerProfileComp{}})
	state := identityState(3, "旧角色")
	state.gender = 2
	if err := l.backfillPlayerIdentity(context.Background(), nil, state); err != nil {
		t.Fatal(err)
	}
	_, player := readIdentityBlob(t, rdb)
	if got := player.GetProfileComponent(); got.GetAppearanceId() != "" || got.GetGender() != 2 {
		t.Fatalf("旧角色不能被隐式分配新人物: %v", got)
	}
}
