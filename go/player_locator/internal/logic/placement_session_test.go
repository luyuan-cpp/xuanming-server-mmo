package logic

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	pb "proto/player_locator"
	smpb "proto/scene_manager"
)

func TestGetSession_IncludePlacementReadsAuthoritativeLocationSameSlot(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	playerID := uint64(9007199254740995)

	session := makeTestSession(playerID)
	session.SceneId = 0
	session.SceneNodeId = ""
	session.LoginGeneration = 777
	session.GateZoneId = 2
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)

	locRaw, err := proto.Marshal(&smpb.PlayerLocation{
		SceneId:         8888,
		NodeId:          "7",
		ZoneId:          2,
		OwnerEpoch:      15,
		SessionId:       session.SessionId,
		LoginGeneration: 777,
	})
	require.NoError(t, err)
	require.NoError(t, mr.Set(sceneManagerLocationKey(playerID), string(locRaw)))

	// 默认不开启 include_placement 时保持原语义，不附带 placement 且不改写 session.SceneId。
	plain, err := NewGetSessionLogic(ctx, sc).GetSession(&pb.GetSessionRequest{PlayerId: playerID})
	require.NoError(t, err)
	assert.True(t, plain.GetFound())
	assert.Nil(t, plain.GetPlacement())
	assert.Equal(t, uint64(0), plain.GetSession().GetSceneId())
	assert.Equal(t, "", plain.GetSession().GetSceneNodeId())

	// 开启 include_placement 时一次同槽 MGET 返回会话与权威落点。
	withPlacement, err := NewGetSessionLogic(ctx, sc).GetSession(&pb.GetSessionRequest{
		PlayerId:         playerID,
		IncludePlacement: true,
	})
	require.NoError(t, err)
	assert.True(t, withPlacement.GetFound())
	require.NotNil(t, withPlacement.GetPlacement())
	assert.True(t, withPlacement.GetPlacement().GetFound())
	assert.Equal(t, uint32(2), withPlacement.GetPlacement().GetZoneId())
	assert.Equal(t, "7", withPlacement.GetPlacement().GetNodeId())
	assert.Equal(t, uint64(8888), withPlacement.GetPlacement().GetSceneId())
	assert.Equal(t, uint64(15), withPlacement.GetPlacement().GetOwnerEpoch())
	assert.False(t, withPlacement.GetPlacement().GetAwaitingPlacement())
	assert.Equal(t, session.SessionId, withPlacement.GetPlacement().GetSessionId())
	assert.Equal(t, uint64(777), withPlacement.GetPlacement().GetLoginGeneration())
}

func TestGetSession_IncludePlacementReportsAwaitingPlacementAndAbsentLocation(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	playerID := uint64(9007199254740996)

	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: makeTestSession(playerID)})
	require.NoError(t, err)

	// 无 location 键时明确回 placement.found=false。
	noLoc, err := NewGetSessionLogic(ctx, sc).GetSession(&pb.GetSessionRequest{
		PlayerId:         playerID,
		IncludePlacement: true,
	})
	require.NoError(t, err)
	assert.True(t, noLoc.GetFound())
	require.NotNil(t, noLoc.GetPlacement())
	assert.False(t, noLoc.GetPlacement().GetFound())

	// 跨区交接等待落点（node_id 为空且 owner_epoch 非 0）标记 awaiting_placement=true。
	handoffRaw, err := proto.Marshal(&smpb.PlayerLocation{
		ZoneId:     3,
		OwnerEpoch: 22,
	})
	require.NoError(t, err)
	require.NoError(t, mr.Set(sceneManagerLocationKey(playerID), string(handoffRaw)))

	handoff, err := NewGetSessionLogic(ctx, sc).GetSession(&pb.GetSessionRequest{
		PlayerId:         playerID,
		IncludePlacement: true,
	})
	require.NoError(t, err)
	require.NotNil(t, handoff.GetPlacement())
	assert.True(t, handoff.GetPlacement().GetFound())
	assert.True(t, handoff.GetPlacement().GetAwaitingPlacement())
	assert.Equal(t, uint32(3), handoff.GetPlacement().GetZoneId())
	assert.Equal(t, "", handoff.GetPlacement().GetNodeId())
}
