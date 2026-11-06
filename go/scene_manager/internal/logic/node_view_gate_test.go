package logic

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"

	"shared/liveview"
)

// nodeViewTestClock 是闸门用的手动时钟,不读墙钟。
type nodeViewTestClock struct{ now time.Time }

func (c *nodeViewTestClock) Now() time.Time { return c.now }

func (c *nodeViewTestClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newNodeViewTestClock() *nodeViewTestClock {
	return &nodeViewTestClock{now: time.Unix(1_800_000_000, 0)}
}

// useNodeViewGateForTest 给包级闸门换上干净状态与手动时钟,用例结束复原。
func useNodeViewGateForTest(t *testing.T) *nodeViewTestClock {
	t.Helper()
	clock := newNodeViewTestClock()
	sceneNodeViewGate.mu.Lock()
	prevNow, prevZones := sceneNodeViewGate.now, sceneNodeViewGate.zones
	sceneNodeViewGate.now, sceneNodeViewGate.zones = clock.Now, make(map[uint32]*liveview.Gate)
	sceneNodeViewGate.mu.Unlock()
	t.Cleanup(func() {
		sceneNodeViewGate.mu.Lock()
		sceneNodeViewGate.now, sceneNodeViewGate.zones = prevNow, prevZones
		sceneNodeViewGate.mu.Unlock()
	})
	return clock
}

func newTestNodeViewGate(clock *nodeViewTestClock) *nodeViewGate {
	return &nodeViewGate{zones: make(map[uint32]*liveview.Gate), now: clock.Now}
}

// nodeSet 把节点号拼成 (zone → node 集合)。
func nodeSet(zoneID uint32, nodeIDs ...uint32) map[uint32]map[string]struct{} {
	nodes := make(map[string]struct{}, len(nodeIDs))
	for _, id := range nodeIDs {
		nodes[fmt.Sprint(id)] = struct{}{}
	}
	return map[uint32]map[string]struct{}{zoneID: nodes}
}

func mergeNodeSets(sets ...map[uint32]map[string]struct{}) map[uint32]map[string]struct{} {
	out := make(map[uint32]map[string]struct{})
	for _, set := range sets {
		for zoneID, nodes := range set {
			if out[zoneID] == nil {
				out[zoneID] = make(map[string]struct{})
			}
			for id := range nodes {
				out[zoneID][id] = struct{}{}
			}
		}
	}
	return out
}

func idRange(from, to uint32) []uint32 {
	ids := make([]uint32, 0, to-from+1)
	for id := from; id <= to; id++ {
		ids = append(ids, id)
	}
	return ids
}

// sceneNodeKVsForTest 拼一份 etcd 快照:每个节点一条 NodeInfo 注册。
func sceneNodeKVsForTest(zoneID uint32, nodeIDs ...uint32) []*mvccpb.KeyValue {
	kvs := make([]*mvccpb.KeyValue, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		kvs = append(kvs, &mvccpb.KeyValue{
			Key:   []byte(fmt.Sprintf("SceneNodeService.rpc/view-test/zone/%d/node/%d", zoneID, id)),
			Value: []byte(fmt.Sprintf(`{"nodeId":%d,"zoneId":%d,"sceneNodeType":1}`, id, zoneID)),
		})
	}
	return kvs
}

// ── 闸门本身 ──────────────────────────────────────────────────────────

// 整个快照为空、基线不空:一直压住,压多久都不放行;基线也为空时放行。
func TestNodeViewGate_EmptySnapshotIsHeldForever(t *testing.T) {
	clock := newNodeViewTestClock()
	g := newTestNodeViewGate(clock)
	for i := 0; i < 5; i++ {
		require.Error(t, g.check(nodeSet(1, 1, 2), nil, true), "第 %d 次", i)
		clock.Advance(nodeViewHold)
	}
	require.NoError(t, g.check(nil, nil, true), "没有基线时空快照是正常的(首次部署、全部节点都没起)")
}

// 某个 zone 缺席过半:整轮压住;缺席数稳定满 nodeViewHold 才放行;别的 zone 好好的也一样压住整轮。
func TestNodeViewGate_ZoneDropHoldsTheWholeSyncUntilStable(t *testing.T) {
	clock := newNodeViewTestClock()
	g := newTestNodeViewGate(clock)
	baseline := mergeNodeSets(nodeSet(1, idRange(1, 10)...), nodeSet(2, 21, 22, 23, 24))
	seen := mergeNodeSets(nodeSet(1, 1, 2, 3, 4), nodeSet(2, 21, 22, 23, 24)) // zone 1 缺 6 个

	err := g.check(baseline, seen, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zone=1 held_drop known=10 missing=6")
	assert.NotContains(t, err.Error(), "zone=2")

	clock.Advance(nodeViewHold - time.Second)
	require.Error(t, g.check(baseline, seen, false), "差一秒满")
	clock.Advance(time.Second)
	require.NoError(t, g.check(baseline, seen, false), "缺席数稳定满 nodeViewHold 放行")
}

// 压住期间有节点回来(正在重新注册):计时从头算,恢复到一半以下也继续压住,恢复完放行。
func TestNodeViewGate_RecoveryRestartsTheClock(t *testing.T) {
	clock := newNodeViewTestClock()
	g := newTestNodeViewGate(clock)
	baseline := nodeSet(1, idRange(1, 10)...)
	require.Error(t, g.check(baseline, nodeSet(1, 1), false))
	clock.Advance(50 * time.Second)
	require.Error(t, g.check(baseline, nodeSet(1, idRange(1, 8)...), false), "还缺两个,仍在压住")
	clock.Advance(50 * time.Second)
	require.Error(t, g.check(baseline, nodeSet(1, idRange(1, 8)...), false), "从回来那一刻起不满 nodeViewHold")
	require.NoError(t, g.check(baseline, nodeSet(1, idRange(1, 10)...), false), "全部回来")
}

// 小 zone 挂两台、大 zone 挂不到一半:日常损失,不压。
func TestNodeViewGate_OrdinaryLossPasses(t *testing.T) {
	g := newTestNodeViewGate(newNodeViewTestClock())
	baseline := mergeNodeSets(nodeSet(1, 1, 2, 3), nodeSet(2, idRange(21, 24)...))
	seen := mergeNodeSets(nodeSet(1, 1), nodeSet(2, 21, 22)) // zone 1:3 个挂 2 个(少于 4);zone 2:4 个挂 2 个(不超过一半)
	require.NoError(t, g.check(baseline, seen, false))
}

// 基线是旧镜像 ∪ 负载集:进程刚启动、旧镜像为空时,负载集一样能比出骤降。
func TestNodeViewBaseline_LoadSetCoversAnEmptyMirror(t *testing.T) {
	baseline := nodeViewBaseline(nil, map[uint32][]string{1: {"1", "2", "3", "4", "5"}})
	assert.Len(t, baseline[1], 5)
	g := newTestNodeViewGate(newNodeViewTestClock())
	require.Error(t, g.check(baseline, nodeSet(1, 1), false), "旧镜像为空、负载集有 5 个、快照只剩 1 个:压住")

	merged := nodeViewBaseline(nodeSet(1, 1, 2, 6), map[uint32][]string{1: {"1", "2", "3"}})
	assert.Len(t, merged[1], 4, "两份按节点号去重合并")
}

// 节点全部收尾完、不再出现在基线里的 zone,闸门状态随之清掉。
func TestNodeViewGate_ForgetsZonesThatLeftTheBaseline(t *testing.T) {
	clock := newNodeViewTestClock()
	g := newTestNodeViewGate(clock)
	require.Error(t, g.check(nodeSet(1, idRange(1, 4)...), nodeSet(1, 1), false))
	require.NoError(t, g.check(nil, nil, false))
	assert.Empty(t, g.zones)
}

// ── 接进 fullSync 之后(applyNodeSnapshot,miniredis,不起 etcd) ─────────────────

// 进程刚启动的领导者:旧镜像为空,负载集里 8 个,快照只剩 2 个。压住期间一个字节不写、镜像不换;
// 缺席数稳定满 nodeViewHold 之后放行,6 个缺席的照常写 death_at、摘负载集。
func TestApplyNodeSnapshot_FreshLeaderHoldsMassVanishingAgainstLoadSet(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	clock := useNodeViewGateForTest(t)
	const zone = uint32(71)
	sc.Config.ZoneId = zone // 本进程编排的 zone(orchestratedZones)
	for _, id := range idRange(7101, 7108) {
		_, err := mr.ZAdd(nodeLoadKey(zone), 0, fmt.Sprint(id))
		require.NoError(t, err)
	}
	snapshot := sceneNodeKVsForTest(zone, 7101, 7102)

	_, err := applyNodeSnapshot(context.Background(), sc, snapshot, 10)
	require.Error(t, err, "8 个里缺 6 个:压住")
	assert.Len(t, loadSetMembersForTest(t, mr, zone), 8, "压住期间不清扫")
	assert.False(t, mr.Exists(nodeDeathAtKey(zone, "7103")), "压住期间不写 death_at")
	assert.Zero(t, knownNodeIdentityMatchCount(zone, "7101"), "压住期间镜像不换")

	clock.Advance(nodeViewHold - time.Second)
	_, err = applyNodeSnapshot(context.Background(), sc, snapshot, 11)
	require.Error(t, err, "差一秒满")

	clock.Advance(time.Second)
	rev, err := applyNodeSnapshot(context.Background(), sc, snapshot, 12)
	require.NoError(t, err, "缺席数稳定满 nodeViewHold:放行")
	assert.Equal(t, int64(12), rev)
	assert.Equal(t, 1, knownNodeIdentityMatchCount(zone, "7101"))
	assert.ElementsMatch(t, []string{"7101", "7102"}, loadSetMembersForTest(t, mr, zone))
	for _, id := range idRange(7103, 7108) {
		assert.True(t, mr.Exists(nodeDeathAtKey(zone, fmt.Sprint(id))), "放行之后 %d 照常补记死亡时刻", id)
	}
}

// 跟随者只拿旧镜像比:镜像里 8 个、快照只剩 2 个就压住,镜像不换;放行之后照常换镜像,但跟随者从不清扫。
func TestApplyNodeSnapshot_FollowerHoldsAgainstItsMirror(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	t.Cleanup(SetLeaderCheckForTest(func() bool { return false }))
	clock := useNodeViewGateForTest(t)
	const zone = uint32(72)
	sc.Config.ZoneId = zone
	for _, id := range idRange(7201, 7208) {
		t.Cleanup(SetKnownNodeForTest(fmt.Sprintf("SceneNodeService.rpc/view-test/zone/%d/node/%d", zone, id), fmt.Sprint(id), "", zone, 1))
		_, err := mr.ZAdd(nodeLoadKey(zone), 0, fmt.Sprint(id))
		require.NoError(t, err)
	}
	snapshot := sceneNodeKVsForTest(zone, 7201, 7202)

	_, err := applyNodeSnapshot(context.Background(), sc, snapshot, 20)
	require.Error(t, err)
	assert.Equal(t, 1, knownNodeIdentityMatchCount(zone, "7203"), "压住期间镜像不换")

	clock.Advance(nodeViewHold)
	_, err = applyNodeSnapshot(context.Background(), sc, snapshot, 21)
	require.NoError(t, err)
	assert.Zero(t, knownNodeIdentityMatchCount(zone, "7203"), "放行之后镜像照常换")
	assert.Len(t, loadSetMembersForTest(t, mr, zone), 8, "跟随者不清扫")
}

// 快照一个节点都没有、负载集里还有:领导者一直压住(以前只看旧镜像,进程刚启动时旧镜像为空,会直接放行并清空负载集)。
func TestApplyNodeSnapshot_EmptySnapshotAgainstLoadSetIsHeldForever(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	clock := useNodeViewGateForTest(t)
	const zone = uint32(73)
	sc.Config.ZoneId = zone
	_, err := mr.ZAdd(nodeLoadKey(zone), 0, "7301")
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		_, err = applyNodeSnapshot(context.Background(), sc, nil, int64(30+i))
		require.Error(t, err, "第 %d 次", i)
		clock.Advance(10 * nodeViewHold)
	}
	assert.Equal(t, []string{"7301"}, loadSetMembersForTest(t, mr, zone))
	assert.False(t, mr.Exists(nodeDeathAtKey(zone, "7301")))
}

// 读基线时还是跟随者、同步到一半才当选:这一轮不清扫负载集。它没拿负载集比过这份快照(进程刚启动、旧镜像为空时
// 闸门在跟随者身份下没有可比的,一份残缺的快照会直接放行),清扫范围也只来自领导者身份下读到的那份负载集;
// 当选时请求的 resync 会以领导者身份再跑一遍。
func TestApplyNodeSnapshot_LeaderElectedMidSyncSkipsTheSweep(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	useNodeViewGateForTest(t)
	var calls atomic.Int32
	t.Cleanup(SetLeaderCheckForTest(func() bool { return calls.Add(1) > 1 })) // 第一次问是跟随者,之后都是领导者
	const zone = uint32(74)
	sc.Config.ZoneId = zone
	for _, id := range idRange(7401, 7408) {
		_, err := mr.ZAdd(nodeLoadKey(zone), 0, fmt.Sprint(id))
		require.NoError(t, err)
	}

	_, err := applyNodeSnapshot(context.Background(), sc, sceneNodeKVsForTest(zone, 7401, 7402), 40)
	require.NoError(t, err, "跟随者身份读基线,旧镜像为空,没有可比的,放行")
	assert.Len(t, loadSetMembersForTest(t, mr, zone), 8, "中途当选的这一轮不清扫")
	assert.False(t, mr.Exists(nodeDeathAtKey(zone, "7403")))
}
