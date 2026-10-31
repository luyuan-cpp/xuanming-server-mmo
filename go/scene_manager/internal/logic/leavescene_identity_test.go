package logic

// b:迟到的 LeaveScene 不得删掉新登录的 location(docs/design/scene-enter-chain-residuals.md §3.2、§3.7)。
// 夹具沿用 leavescene_cas_test.go(seedLeaveCasFixture / leaveCasValue)与 enterscene_identity_test.go 的 ident* 辅助。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"proto/scene_manager"
)

// leaveIdentRequest 组一条带被删会话身份的 LeaveScene。
func leaveIdentRequest(playerID, sceneID uint64, sessionID uint32, generation uint64) *scene_manager.LeaveSceneRequest {
	return &scene_manager.LeaveSceneRequest{
		PlayerId: playerID, SceneId: sceneID, SessionId: sessionID, LoginGeneration: generation,
	}
}

// b1:A(11, gA)在 S1;B(22, gB)走同落点重连(samePlacement 写下自己的身份);A 迟到的 LeaveScene 到达 →
// location 仍在、身份为 (22, gB)、人数不变。
func TestLeaveScene_StaleLeaveAfterSamePlacementRelogKeepsLocation(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	capturingKafkaWriter(sc)
	const (
		playerID = uint64(8201)
		s1       = uint64(9301)
	)
	seedSceneOnNode(mr, testZoneId, s1, "10", "1")
	seedNodeMembers(mr, testZoneId, "10", 1)
	identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100,
		SessionId: 11, LoginGeneration: identGenA,
	})
	adoptSceneMember(mr, s1, playerID)
	adoptNodeMember(mr, testZoneId, "10", playerID)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: s1, ZoneId: testZoneId, SessionId: 22, LoginGeneration: identGenB,
		GateZoneId: testZoneId, GateId: "2", GateInstanceId: "gate-b-uuid",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	keepBefore := placementIdentityCount(t, "leave", "keep_newer")

	_, err = NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveIdentRequest(playerID, s1, 11, identGenA))
	require.NoError(t, err)

	loc := identLocation(t, sc, playerID)
	require.NotNil(t, loc, "迟到的旧会话 LeaveScene 不得删掉新登录的 location")
	assert.Equal(t, uint32(22), loc.SessionId)
	assert.Equal(t, identGenB, loc.LoginGeneration)
	assert.Equal(t, "1", sceneMemberCount(mr, s1), "不删就不扣人数")
	assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "10"))
	assert.Equal(t, keepBefore+1, placementIdentityCount(t, "leave", "keep_newer"))
}

// 同一连接登出后再登录:会话号不变(11)、location 代次 g2,离场带 g1 < g2 → 不删。按会话号精确匹配会误删这里。
func TestLeaveScene_SameConnectionRelogKeepsLocation(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const (
		playerID = uint64(8202)
		s1       = uint64(9302)
	)
	raw := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100,
		SessionId: 11, LoginGeneration: identGenB,
	})
	seedLeaveCasFixture(t, mr, playerID, s1, raw, "1")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveIdentRequest(playerID, s1, 11, identGenA))
	require.NoError(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Equal(t, "1", sceneMemberCount(mr, s1))
	assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "10"))
}

// b2:B 已把 location 写到 S2;player_locator 现读拿到 S2,发来 LeaveScene{S2, 11, gA} → 不删,S2 人数不变。
func TestLeaveScene_NewerPlacementResolvedByLocatorKept(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const (
		playerID = uint64(8203)
		s2       = uint64(9303)
	)
	raw := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{
		SceneId: s2, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 2, UpdateTime: 200,
		SessionId: 22, LoginGeneration: identGenB,
	})
	seedLeaveCasFixture(t, mr, playerID, s2, raw, "1")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveIdentRequest(playerID, s2, 11, identGenA))
	require.NoError(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Equal(t, "1", sceneMemberCount(mr, s2))
}

// b3:B 落到同一个 scene_id、换了节点(身份随新落点更新)→ 旧会话的 LeaveScene 不删。
func TestLeaveScene_SameSceneRewrittenByNewerLoginKept(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const (
		playerID = uint64(8204)
		s1       = uint64(9304)
	)
	raw := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "20", ZoneId: testZoneId, OwnerEpoch: 2, UpdateTime: 200,
		SessionId: 22, LoginGeneration: identGenB,
	})
	seedLeaveCasFixture(t, mr, playerID, s1, raw, "1")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveIdentRequest(playerID, s1, 11, identGenA))
	require.NoError(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Equal(t, "1", sceneMemberCount(mr, s1))
}

// 防泄漏:location 的主人(11, g1)比离场会话(22, g2)还旧 → 删除,且只扣一次人数(与现状相同)。
func TestLeaveScene_OlderLocationIdentityIsDeleted(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const (
		playerID = uint64(8205)
		s1       = uint64(9305)
	)
	raw := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100,
		SessionId: 11, LoginGeneration: identGenA,
	})
	seedLeaveCasFixture(t, mr, playerID, s1, raw, "2")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveIdentRequest(playerID, s1, 22, identGenB))
	require.NoError(t, err)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.Equal(t, "1", sceneMemberCount(mr, s1))
	assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "10"))

	// 重发同一条:键已不在,不再扣。
	_, err = NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveIdentRequest(playerID, s1, 22, identGenB))
	require.NoError(t, err)
	assert.Equal(t, "1", sceneMemberCount(mr, s1))
}

// 兼容:请求不带身份(旧版 player_locator)→ legacy 删除。
func TestLeaveScene_LegacyRequestWithoutIdentityDeletes(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const (
		playerID = uint64(8206)
		s1       = uint64(9306)
	)
	raw := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100,
		SessionId: 22, LoginGeneration: identGenB,
	})
	seedLeaveCasFixture(t, mr, playerID, s1, raw, "1")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(&scene_manager.LeaveSceneRequest{
		PlayerId: playerID, SceneId: s1,
	})
	require.NoError(t, err)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.Equal(t, "0", sceneMemberCount(mr, s1))
}

// 兼容:location 没有身份(旧版 scene_manager 写入)→ legacy 删除。
func TestLeaveScene_LegacyLocationWithoutIdentityDeletes(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const (
		playerID = uint64(8207)
		s1       = uint64(9307)
	)
	raw := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100,
	})
	seedLeaveCasFixture(t, mr, playerID, s1, raw, "1")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveIdentRequest(playerID, s1, 11, identGenA))
	require.NoError(t, err)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.Equal(t, "0", sceneMemberCount(mr, s1))
}

// 代次相等、会话都有效且不同(违反 G2)→ 不删。删错会让在线玩家丢 location,后果比泄漏重。
func TestLeaveScene_EqualGenerationDifferentSessionNotDeleted(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const (
		playerID = uint64(8208)
		s1       = uint64(9308)
	)
	raw := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100,
		SessionId: 22, LoginGeneration: identGenA,
	})
	seedLeaveCasFixture(t, mr, playerID, s1, raw, "1")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveIdentRequest(playerID, s1, 11, identGenA))
	require.NoError(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Equal(t, "1", sceneMemberCount(mr, s1))
}

// 代次相等、location 会话号为 0xFFFFFFFF(无效)→ 删除:与 d 的判定表(按同会话放行)同一口径。
func TestLeaveScene_EqualGenerationInvalidLocationSessionDeleted(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const (
		playerID = uint64(8209)
		s1       = uint64(9309)
	)
	raw := leaveCasLocationRaw(t, &scene_manager.PlayerLocation{
		SceneId: s1, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100,
		SessionId: 0xFFFFFFFF, LoginGeneration: identGenA,
	})
	seedLeaveCasFixture(t, mr, playerID, s1, raw, "1")

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveIdentRequest(playerID, s1, 11, identGenA))
	require.NoError(t, err)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.Equal(t, "0", sceneMemberCount(mr, s1))
}
