package logic

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// CreateScene 的节点侧请求带场景所属的 zone(docs/design/redis-cluster-all-domains-20261110.md §3.4):scene 节点的归属对账
// 只按节点自己的 zone 拼键,节点据此拒建别的 zone 的场景(C++ SceneLifecycle::AdmitsCreateZone)。两个建场景入口
// (副本 / 镜像、世界频道)都经 createSceneOnNode,都要带上本进程的 zone。
func TestCreateSceneOnNodeCarriesTheOwnZone(t *testing.T) {
	clearKnownNodesForTest()
	ResetNodeConnCacheForTest()
	t.Cleanup(clearKnownNodesForTest)
	t.Cleanup(ResetNodeConnCacheForTest)

	sc, _ := newIntegrationSvcCtx(t)
	h := startFakeSceneNode(t, "10")
	installBufconnDialer(t, h)
	registerWorldNodeFromHarness(t, sc, testZoneId, h)

	_, err := RequestNodeCreateSceneWithOptions(context.Background(), sc, testZoneId, "10", 1001, 9001, 0, nil)
	require.NoError(t, err)
	require.EqualValues(t, 9001, h.fake.lastCreateScene.Load())
	require.EqualValues(t, sc.Config.ZoneId, h.fake.lastCreateZone.Load(), "副本 / 镜像的建场景带本进程的 zone")

	_, err = RequestNodeCreateSceneAtGeneration(context.Background(), sc, testZoneId, "10", 1001, 9002, 5)
	require.NoError(t, err)
	require.EqualValues(t, 9002, h.fake.lastCreateScene.Load())
	require.EqualValues(t, sc.Config.ZoneId, h.fake.lastCreateZone.Load(), "世界频道的建场景同样带本进程的 zone")
	require.NotZero(t, h.fake.lastCreateZone.Load(), "节点把 0 当成没填,一律拒建")
}
