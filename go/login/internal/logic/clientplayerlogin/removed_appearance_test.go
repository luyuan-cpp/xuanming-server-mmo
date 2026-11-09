package clientplayerloginlogic

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	base "proto/common/base"
	db "proto/common/database"
	loginpb "proto/login"
	"shared/generated/pb/table"
)

func TestRemovedAppearanceCannotEnterOrAppearInResponse(t *testing.T) {
	roles := []*base.AccountSimplePlayer{
		{PlayerId: identityPlayerID, Name: "removed", AppearanceId: "06_thunder_caster_boy"},
		{PlayerId: 7002, Name: "legacy", AppearanceId: ""},
		{PlayerId: 7003, Name: "other", AppearanceId: "08_alchemy_prodigy_boy"},
	}
	h := newEnterWiringHarness(t, roles, nil)
	resp, err := h.l.EnterGame(&loginpb.EnterGameRequest{PlayerId: identityPlayerID})
	require.NoError(t, err)
	require.Equal(t, uint32(table.LoginError_kLoginEnterGameGuid), resp.GetErrorMessage().GetId())
	require.EqualValues(t, 0, h.locator.setSessionCalls.Load())
	require.EqualValues(t, 0, h.locator.reconnectCalls.Load())
	projected := buildRoleList(context.Background(), nil, true, roles)
	require.Len(t, projected, 2)
	require.Equal(t, uint64(7002), projected[0].GetPlayer().GetPlayerId())
	require.Equal(t, uint64(7003), projected[1].GetPlayer().GetPlayerId())
	createResp := &loginpb.CreatePlayerResponse{}
	appendRoleList(createResp, &db.UserAccounts{SimplePlayers: &base.AccountSimplePlayerList{Players: roles}})
	require.Len(t, createResp.GetPlayers(), 2)
	require.Len(t, roles, 3)
}
