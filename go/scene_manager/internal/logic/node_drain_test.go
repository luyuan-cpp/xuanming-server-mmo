package logic

import (
	"context"
	"testing"

	"proto/scene_manager"
	"scene_manager/internal/constants"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scene 节点计划排空在 scene_manager 一侧的单测(docs/design/scene-node-planned-drain.md §4.2~§4.6)。
// 排空状态与构建标识只来自 etcd 镜像 knownNodes,这里直接摆镜像;负载集、角色镜像、频道与属主摆在 miniredis 里。

// putSceneNodeForTest 往 etcd 镜像里放一台本 zone 的主世界节点,自报排空与构建。调用方负责 clearKnownNodesForTest。
func putSceneNodeForTest(nodeID string, draining bool, build string) {
	entry := nodeEntry{nodeID: nodeID}
	entry.reg.ZoneId = testZoneId
	entry.reg.SceneNodeType = constants.SceneNodeTypeMainWorld
	entry.reg.Draining = draining
	entry.reg.Build = build
	knownNodesMu.Lock()
	knownNodes[sceneNodePrefix+"drain-test/"+nodeID] = entry
	knownNodesMu.Unlock()
}

func TestParseNodeEntryReadsAdmissionAndBuild(t *testing.T) {
	cases := []struct {
		name     string
		value    string
		draining bool
		build    string
	}{
		{"closed by name", `{"nodeId":10,"zoneId":1,"admission":"NODE_ADMISSION_CLOSED","build":"v2"}`, true, "v2"},
		{"closed by number", `{"nodeId":10,"zoneId":1,"admission":2}`, true, ""},
		{"open", `{"nodeId":10,"zoneId":1,"admission":"NODE_ADMISSION_OPEN","build":"v1"}`, false, "v1"},
		{"not reported", `{"nodeId":10,"zoneId":1}`, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entry, ok := parseNodeEntry([]byte(c.value))
			require.True(t, ok)
			assert.Equal(t, c.draining, entry.reg.Draining)
			assert.Equal(t, c.build, entry.reg.Build)
			assert.Equal(t, "10", entry.nodeID)
		})
	}
}

func TestNodeAdmissionViewPrefersBuildsNotBeingDrained(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	putSceneNodeForTest("10", true, "old")
	putSceneNodeForTest("20", false, "old")
	putSceneNodeForTest("30", false, "new")
	putSceneNodeForTest("40", false, "")

	v := snapshotNodeAdmission(testZoneId)
	assert.True(t, v.isDraining("10"))
	assert.False(t, v.isDraining("20"))
	assert.False(t, v.isPreferred("10"), "排空中的节点不是落点")
	assert.False(t, v.isPreferred("20"), "外出构建上的开放节点排在后面")
	assert.True(t, v.isPreferred("30"))
	assert.True(t, v.isPreferred("40"), "没上报构建的节点没有偏好依据,不算外出构建")

	assert.Equal(t, []string{"30", "40", "20"}, v.preferredFirst([]string{"20", "30", "40"}), "两组内部保持原顺序")
	assert.Equal(t, []string{"30", "40"}, v.placementRing([]string{"20", "30", "40"}))
	assert.Equal(t, []string{"20"}, v.placementRing([]string{"20"}), "没有首选落点时退回全部开放节点")

	assert.Empty(t, snapshotNodeAdmission(testZoneId+1).draining, "别的 zone 不进快照")
}

func TestNodeAdmissionViewWithoutDrainingHasNoPreference(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	putSceneNodeForTest("10", false, "old")
	putSceneNodeForTest("20", false, "new")

	v := snapshotNodeAdmission(testZoneId)
	nodes := []string{"20", "10"}
	assert.Equal(t, nodes, v.preferredFirst(nodes), "没有排空节点就没有外出构建,顺序不变")
	assert.Equal(t, nodes, v.placementRing(nodes))
}

func TestGetNodesForPurposeSkipsDrainingAndPrefersIncomingBuild(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeMainWorld, 1)
	registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeMainWorld, 2)
	registerTypedNode(mr, testZoneId, "30", constants.SceneNodeTypeMainWorld, 3)
	putSceneNodeForTest("10", true, "old")
	putSceneNodeForTest("20", false, "old")
	putSceneNodeForTest("30", false, "new")

	assert.Equal(t, []string{"30", "20"}, getNodesForPurpose(sc, testZoneId, constants.NodePurposeWorld))
	best, err := GetBestNodeForPurpose(context.Background(), sc, testZoneId, constants.NodePurposeWorld)
	require.NoError(t, err)
	assert.Equal(t, "30", best, "最空的节点在外出构建上,新构建优先")
	assert.True(t, IsNodeAlive(sc, testZoneId, "10"), "排空中的节点仍然活着、仍是名下场景的属主")
}

func TestPlanRebalance_DrainingOwnerMovesWithPlayersToIncomingBuild(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.MaxRebalanceMigrationsPerTick = 10
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeMainWorld, 0)
	registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeMainWorld, 0)
	registerTypedNode(mr, testZoneId, "30", constants.SceneNodeTypeMainWorld, 0)
	putSceneNodeForTest("10", true, "old")
	putSceneNodeForTest("20", false, "old")
	putSceneNodeForTest("30", false, "new")
	confIDs := []uint64{1001}

	seedWorldChannel(sc, testZoneId, 1001, 555, "10")
	seedSceneMembersVia(sc, 555, 5)
	// 外出构建上、还没排空的节点托着有人的频道:不动(没有热迁移),等它自己排空时再迁。
	seedWorldChannel(sc, testZoneId, 1001, 556, "20")
	seedSceneMembersVia(sc, 556, 3)

	urgent, opportunistic, _ := PlanWorldChannelRebalance(sc, testZoneId, confIDs)
	require.Len(t, urgent, 1)
	assert.Equal(t, uint64(555), urgent[0].SceneId)
	assert.Equal(t, reasonNodeDraining, urgent[0].Reason, "排空节点上的频道有人也迁")
	assert.Equal(t, "10", urgent[0].OldNode)
	assert.Equal(t, "30", urgent[0].NewNode, "目标只在不在外出构建上的节点里选")
	assert.Empty(t, opportunistic, "有人的频道不做机会性迁移")
	assert.Empty(t, withoutDrainingMoves(urgent), "rebalance 不执行排空项,交给排空迁移协程")

	sc.Config.MaxRebalanceMigrationsPerTick = 0
	plan := planDrainMigrations(sc, testZoneId, "10", confIDs)
	require.Len(t, plan, 1, "关掉 rebalance(名额为 0)不影响计划排空")
	assert.Equal(t, uint64(555), plan[0].SceneId)
	assert.Empty(t, planDrainMigrations(sc, testZoneId, "20", confIDs), "没在排空的节点不出排空迁移项")
}

func TestPlanRebalance_EmptyChannelOnOutgoingBuildMovesToIncomingBuild(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.MaxRebalanceMigrationsPerTick = 10
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeMainWorld, 0)
	registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeMainWorld, 0)
	registerTypedNode(mr, testZoneId, "30", constants.SceneNodeTypeMainWorld, 0)
	putSceneNodeForTest("10", true, "old")
	putSceneNodeForTest("20", false, "old")
	putSceneNodeForTest("30", false, "new")

	// 滚动期间哈希环只含新构建:外出构建上没人的频道被顺带迁走,以后排空它时就没有人要搬。
	seedWorldChannel(sc, testZoneId, 1001, 557, "20")
	_, opportunistic, _ := PlanWorldChannelRebalance(sc, testZoneId, []uint64{1001})
	require.Len(t, opportunistic, 1)
	assert.Equal(t, reasonBetterHome, opportunistic[0].Reason)
	assert.Equal(t, "30", opportunistic[0].NewNode)
}

func TestTierWorldChannelCandidates(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	putSceneNodeForTest("10", true, "old")
	putSceneNodeForTest("20", false, "old")
	putSceneNodeForTest("30", false, "new")
	v := snapshotNodeAdmission(testZoneId)

	c10 := WorldChannelCandidate{SceneID: 1, NodeID: "10", ZoneID: testZoneId}
	c20 := WorldChannelCandidate{SceneID: 2, NodeID: "20", ZoneID: testZoneId}
	c30 := WorldChannelCandidate{SceneID: 3, NodeID: "30", ZoneID: testZoneId}

	normal := worldChannelPick{}
	evacuation := worldChannelPick{evacuation: true}
	assert.Equal(t, []WorldChannelCandidate{c30}, tierWorldChannelCandidates([]WorldChannelCandidate{c10, c20, c30}, v, normal))
	assert.Equal(t, []WorldChannelCandidate{c20}, tierWorldChannelCandidates([]WorldChannelCandidate{c10, c20}, v, normal))
	assert.Equal(t, []WorldChannelCandidate{c10}, tierWorldChannelCandidates([]WorldChannelCandidate{c10}, v, normal),
		"普通进场别无选择时仍可进排空节点上的频道")
	assert.Empty(t, tierWorldChannelCandidates([]WorldChannelCandidate{c10}, v, evacuation), "疏散不进排空节点")
	assert.Equal(t, []WorldChannelCandidate{c20}, tierWorldChannelCandidates([]WorldChannelCandidate{c10, c20}, v, evacuation))
}

func TestEvacuationSourceNode(t *testing.T) {
	loc := &scene_manager.PlayerLocation{NodeId: "10", ZoneId: testZoneId, SceneId: 555}
	assert.Equal(t, "10", evacuationSourceNode(loc, testZoneId, testZoneId))
	assert.Empty(t, evacuationSourceNode(loc, testZoneId+1, testZoneId), "别的 zone 的节点号不能拿来排除本 zone 的节点")
	assert.Empty(t, evacuationSourceNode(loc, 0, 0), "zone 不明不排除")
	assert.Empty(t, evacuationSourceNode(nil, testZoneId, testZoneId))
}

func TestResolveEvacuationTarget(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	defer SetWorldConfIdsForTest([]uint64{1001, 1002})()
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	registerTypedNode(mr, testZoneId, "10", constants.SceneNodeTypeMainWorld, 0)
	registerTypedNode(mr, testZoneId, "20", constants.SceneNodeTypeMainWorld, 0)
	registerTypedNode(mr, testZoneId, "30", constants.SceneNodeTypeMainWorld, 0)
	putSceneNodeForTest("10", true, "old")
	putSceneNodeForTest("20", false, "old")
	putSceneNodeForTest("30", false, "new")
	seedWorldChannel(sc, testZoneId, 1001, 555, "30") // 原频道已由排空迁移挪到 30
	seedWorldChannel(sc, testZoneId, 1001, 556, "20")
	l := NewEnterSceneLogic(context.Background(), sc)

	channelReq := &scene_manager.EnterSceneRequest{PlayerId: 9001, SceneId: 555, SceneConfId: 1001, ZoneId: testZoneId, Evacuation: true}

	t.Run("original channel moved away", func(t *testing.T) {
		sid, nid, reserved, err := l.resolveEvacuationTarget(channelReq, "10", testZoneId)
		require.NoError(t, err)
		assert.Equal(t, uint64(555), sid)
		assert.Equal(t, "30", nid)
		assert.False(t, reserved, "按 scene_id 落点不预占,与普通按 id 进场一致")
	})

	t.Run("original channel still on the source node", func(t *testing.T) {
		require.NoError(t, sc.Redis.Set(sceneNodeKey(testZoneId, 555), "10"))
		t.Cleanup(func() { _ = sc.Redis.Set(sceneNodeKey(testZoneId, 555), "30") })
		sid, nid, reserved, err := l.resolveEvacuationTarget(channelReq, "10", testZoneId)
		require.NoError(t, err)
		assert.Equal(t, uint64(556), sid, "不派回将要拆掉的那份实例,改落同图别的频道")
		assert.Equal(t, "20", nid)
		assert.True(t, reserved)
	})

	t.Run("dungeon falls back to the default world", func(t *testing.T) {
		dungeonReq := &scene_manager.EnterSceneRequest{PlayerId: 9002, ZoneId: testZoneId, Evacuation: true}
		sid, _, _, err := l.resolveEvacuationTarget(dungeonReq, "10", testZoneId)
		require.NoError(t, err)
		assert.Contains(t, []uint64{555, 556}, sid, "默认大世界(World 表第一行 1001)")
	})

	t.Run("map without channels falls back to the default world", func(t *testing.T) {
		otherMapReq := &scene_manager.EnterSceneRequest{PlayerId: 9003, SceneConfId: 1002, ZoneId: testZoneId, Evacuation: true}
		sid, _, _, err := l.resolveEvacuationTarget(otherMapReq, "10", testZoneId)
		require.NoError(t, err)
		assert.Contains(t, []uint64{555, 556}, sid)
	})

	t.Run("nowhere to go", func(t *testing.T) {
		putSceneNodeForTest("20", true, "old")
		putSceneNodeForTest("30", true, "new")
		t.Cleanup(func() {
			putSceneNodeForTest("20", false, "old")
			putSceneNodeForTest("30", false, "new")
		})
		_, _, _, err := l.resolveEvacuationTarget(channelReq, "10", testZoneId)
		assert.Error(t, err, "全部节点都在排空:没有去处,交给 scene 侧核实后踢线")
	})
}
