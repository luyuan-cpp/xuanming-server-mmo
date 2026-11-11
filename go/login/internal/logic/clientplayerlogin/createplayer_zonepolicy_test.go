package clientplayerloginlogic

import (
	"context"
	"testing"

	"login/internal/logic/pkg/homezone"
	"login/internal/logic/pkg/zonepolicy"
	loginpb "proto/login"
	"shared/generated/pb/table"
	"shared/redisslot"
)

// loadedZonePolicy 返回一个已经读到 value 的本区区服策略读者(只读一次,不起循环)。
func loadedZonePolicy(t *testing.T, value string) *zonepolicy.Reader {
	t.Helper()
	r := zonepolicy.NewReader(hzZoneID, func(context.Context, string) ([]byte, bool, error) {
		return []byte(value), true, nil
	})
	if outcome, err := r.Refresh(context.Background()); outcome != zonepolicy.OutcomeOK || err != nil {
		t.Fatalf("load zone policy %s: outcome=%s err=%v", value, outcome, err)
	}
	return r
}

// 本区暂停创建新角色(区服目录 create_role_closed):回 kLoginZoneCreateRoleClosed,而且在任何副作用之前——
// 不拿建角锁、不铸号、不登记名字与归属、不落盘。
func TestCreatePlayer_ZoneCreateRoleClosedRejectsBeforeAnySideEffect(t *testing.T) {
	fake := &fakeRegisterClient{}
	h := newCreatePlayerHarnessWithNames(t, homezone.New(fake, 0, 0, 0), fake)
	h.l.svcCtx.ZonePolicy = loadedZonePolicy(t, `{"capacity":45000,"createRoleClosed":true}`)

	resp, err := h.l.CreatePlayer(&loginpb.CreatePlayerRequest{})
	if err != nil {
		t.Fatalf("CreatePlayer: %v", err)
	}
	if got, want := resp.GetErrorMessage().GetId(), uint32(table.LoginError_kLoginZoneCreateRoleClosed); got != want {
		t.Fatalf("tip = %d, want %d (kLoginZoneCreateRoleClosed)", got, want)
	}
	if fake.calls != 0 || len(fake.events) != 0 {
		t.Fatalf("data_service was called (register calls=%d, events=%v); the rejection must come before any side effect",
			fake.calls, fake.events)
	}
	if players := storedPlayers(t, h.ctx, h.rdb); len(players) != 0 {
		t.Fatalf("persisted players = %v, want none", players)
	}
	if h.mr.Exists(redisslot.AccountCreateLockKey(hzAccount)) {
		t.Fatal("the create lock was taken; the rejection must come before step 3")
	}
}

// 开着时照常建角;读不到区服策略(读者为 nil)的照常建角由其它建角用例覆盖。
func TestCreatePlayer_ZoneCreateRoleOpenCreatesNormally(t *testing.T) {
	fake := &fakeRegisterClient{}
	h := newCreatePlayerHarnessWithNames(t, homezone.New(fake, 0, 0, 0), fake)
	h.l.svcCtx.ZonePolicy = loadedZonePolicy(t, `{"capacity":45000,"createRoleClosed":false}`)

	resp, err := h.l.CreatePlayer(&loginpb.CreatePlayerRequest{})
	if err != nil {
		t.Fatalf("CreatePlayer: %v", err)
	}
	if resp.GetErrorMessage() != nil {
		t.Fatalf("unexpected tip %v", resp.GetErrorMessage())
	}
	if players := storedPlayers(t, h.ctx, h.rdb); len(players) != 1 || players[0].GetPlayerId() != hzPlayerID {
		t.Fatalf("persisted players = %v, want exactly player %d", players, hzPlayerID)
	}
}
