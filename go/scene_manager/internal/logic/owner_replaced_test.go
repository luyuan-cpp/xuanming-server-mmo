package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
	"shared/savebarrier"
)

// GO-3(cross-zone-scene-travel.md §13.10):scene 进程崩溃后,重启的新进程拿回原来的 node_id。location 指向的
// (zone,node) 仍在册,但持有这名玩家的是旧进程实例 —— 它在交接资格里盖过自己的实例号。新进程按原 epoch 登记
// 必被拒(同 E 不同实例),落点又是同物理节点、不铸新 epoch,玩家被永久挡在门外。

const (
	replacedOldInstance = "11111111-1111-4111-8111-111111111111"
	replacedNewInstance = "22222222-2222-4222-8222-222222222222"
	replacedNodeKey     = "SceneNodeService.rpc/replaced-10"
)

// registerKnownInstanceForTest 往注册镜像里放一条带实例号的注册。
func registerKnownInstanceForTest(t *testing.T, key string, zoneID uint32, nodeID, instance string) {
	t.Helper()
	entry := nodeEntry{nodeID: nodeID}
	entry.reg.ZoneId = zoneID
	entry.reg.NodeUuid = instance
	knownNodesMu.Lock()
	knownNodes[key] = entry
	knownNodesMu.Unlock()
}

// holderFenceForTest 构造一份 C++ 会写出的交接资格原文 "E|S|N|A|K|T"。
func holderFenceForTest(epoch uint64, instance string, state savebarrier.HandoffFenceState) string {
	return fmt.Sprintf("%d|%s|3|%s|%c|%s", epoch, instance, strings.Repeat("a", 32), state, strings.Repeat("b", 32))
}

func seedHandoffFenceForTest(t *testing.T, mr *miniredis.Miniredis, playerID uint64, raw string) {
	t.Helper()
	mr.HSet(savebarrier.Key(playerID), savebarrier.HandoffFenceField, raw)
}

// forgetInstanceGoneObservationForTest 开始与结束各删一次本副本对该实例的消失记录。
func forgetInstanceGoneObservationForTest(t *testing.T, instance string) {
	t.Helper()
	forgetInstanceGoneObservation(instance)
	t.Cleanup(func() { forgetInstanceGoneObservation(instance) })
}

// observeInstanceGoneAtForTest 把本副本对该实例的消失记录摆到 ago 之前。
func observeInstanceGoneAtForTest(instance string, ago time.Duration) {
	instanceGoneObservedMu.Lock()
	instanceGoneObservedAt[instance] = time.Now().Add(-ago)
	instanceGoneObservedMu.Unlock()
}

// stubFreshRegistryForTest 替换 etcd 复核:holderPresent 为真时在册集合里有旧实例,否则只有新实例。返回调用计数。
func stubFreshRegistryForTest(t *testing.T, holderPresent bool, err error) *atomic.Int32 {
	t.Helper()
	calls := &atomic.Int32{}
	prev := freshRegistryInstancesOverride
	freshRegistryInstancesOverride = func(context.Context) (map[string]struct{}, error) {
		calls.Add(1)
		if err != nil {
			return nil, err
		}
		instances := map[string]struct{}{replacedNewInstance: {}}
		if holderPresent {
			instances[replacedOldInstance] = struct{}{}
		}
		return instances, nil
	}
	t.Cleanup(func() { freshRegistryInstancesOverride = prev })
	return calls
}

// seedReplacedHolder 摆出:玩家在 zone 1 的 node 10 / oldScene、owner_epoch = 1,交接资格由旧实例盖章;
// node 10 现在由新实例注册,旧实例不在册。目标场景 targetID 也在 node 10 上(同物理节点:原来卡死的形态)。
func seedReplacedHolder(t *testing.T, sc *svc.ServiceContext, mr *miniredis.Miniredis, playerID, oldScene, targetID uint64) string {
	t.Helper()
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	markKnownNodesSyncedForTest(t)
	forgetInstanceGoneObservationForTest(t, replacedOldInstance)
	registerKnownInstanceForTest(t, replacedNodeKey, testZoneId, "10", replacedNewInstance)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "10", "4")
	withReachableSceneNode(t, sc, "10")
	raw := identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{
		SceneId: oldScene, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 100,
	})
	seedHandoffFenceForTest(t, mr, playerID, holderFenceForTest(1, replacedOldInstance, savebarrier.HandoffFenceHeld))
	return raw
}

func enterReplacedForTest(t *testing.T, sc *svc.ServiceContext, playerID, sceneID uint64) *scene_manager.EnterSceneResponse {
	t.Helper()
	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: sceneID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	return resp
}

// 屏障已过、etcd 复核确认旧实例不在册:按无持有者落点,一定铸新 epoch。新进程拿 2 登记资格,
// 旧实例手里的 1 之后每一次存盘都被 owner CAS 拒绝。
func TestEnterScene_ReplacedOwnerAfterBarrierIsTakenOver(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6301)
		oldScene = uint64(7301)
		targetID = uint64(7302)
	)
	seedReplacedHolder(t, sc, mr, playerID, oldScene, targetID)
	observeInstanceGoneAtForTest(replacedOldInstance, 2*sceneReentryBarrier(sc))
	calls := stubFreshRegistryForTest(t, false, nil)

	resp := enterReplacedForTest(t, sc, playerID, targetID)
	require.Equal(t, uint32(0), resp.ErrorCode, "持有进程已被顶替且屏障已过,应按无持有者落点")
	assert.Equal(t, int32(1), calls.Load(), "放行前必须向 etcd 复核一次")
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "接管必须铸新 epoch")
	loc := identLocation(t, sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, targetID, loc.SceneId)
	assert.Equal(t, "10", loc.NodeId)
	assert.Equal(t, uint64(2), loc.OwnerEpoch)
	require.Len(t, *captured, 1)
	assert.Equal(t, uint64(2), decodeRoutePlayerEvent(t, (*captured)[0]).OwnerEpoch)
	assert.Equal(t, "2", identValue(t, sc, fmt.Sprintf(InstancePlayerCountKey, oldScene)), "旧场景占的人数要还回去")
}

// 本副本第一次确认旧实例不在册:屏障从此刻起算,一个字节都不改地回 17;屏障内重试不再打 etcd。
func TestEnterScene_ReplacedOwnerFirstObservationStartsBarrier(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6311)
		oldScene = uint64(7311)
		targetID = uint64(7312)
	)
	locRaw := seedReplacedHolder(t, sc, mr, playerID, oldScene, targetID)
	calls := stubFreshRegistryForTest(t, false, nil)

	for attempt := 1; attempt <= 2; attempt++ {
		resp := enterReplacedForTest(t, sc, playerID, targetID)
		assert.Equal(t, constants.ErrSceneReentryBarrier, resp.ErrorCode, "第 %d 次:屏障未过只能可重试地拒绝", attempt)
		assert.Equal(t, locRaw, identLocationRaw(t, sc, playerID), "location 一个字节都不改")
		assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "不得铸造")
		assert.Empty(t, *captured, "不得发路由:同号新进程按原 epoch 登记必被拒")
		assert.Equal(t, "4", identValue(t, sc, fmt.Sprintf(InstancePlayerCountKey, targetID)), "不得预占目标场景人数")
	}
	_, observed := instanceGoneObservedSince(replacedOldInstance)
	assert.True(t, observed, "首次复核要登记观察时刻")
	assert.Equal(t, int32(1), calls.Load(), "屏障内的重试不再复核 etcd")
}

// etcd 复核没做成:镜像已表明持有实例不是现任,照旧路由注定被拒,所以同样一个字节不改地回 17。
func TestEnterScene_ReplacedOwnerUnverifiedRejectsRetryably(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6316)
		oldScene = uint64(7316)
		targetID = uint64(7317)
	)
	locRaw := seedReplacedHolder(t, sc, mr, playerID, oldScene, targetID)
	observeInstanceGoneAtForTest(replacedOldInstance, 2*sceneReentryBarrier(sc))
	stubFreshRegistryForTest(t, false, errors.New("etcd unavailable"))

	resp := enterReplacedForTest(t, sc, playerID, targetID)
	assert.Equal(t, constants.ErrSceneReentryBarrier, resp.ErrorCode)
	assert.Equal(t, locRaw, identLocationRaw(t, sc, playerID))
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	assert.Empty(t, *captured)
}

// 缺任何一条证据都沿用原判定(fail-closed)。每个子用例都把消失观察摆到屏障之前,只变一条前提。
func TestPlayerLocationOwnerReplacedRequiresEveryPieceOfEvidence(t *testing.T) {
	const playerID = uint64(6321)
	loc := &scene_manager.PlayerLocation{SceneId: 7321, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1}
	held := func(t *testing.T, mr *miniredis.Miniredis) {
		seedHandoffFenceForTest(t, mr, playerID, holderFenceForTest(1, replacedOldInstance, savebarrier.HandoffFenceHeld))
	}
	cases := []struct {
		name    string
		arrange func(t *testing.T, mr *miniredis.Miniredis)
		fresh   func(t *testing.T)
		epoch   uint64
		want    ownerReplacedVerdict
	}{
		{name: "资格章就是在册实例(新进程已真的持有)", arrange: func(t *testing.T, mr *miniredis.Miniredis) {
			seedHandoffFenceForTest(t, mr, playerID, holderFenceForTest(1, replacedNewInstance, savebarrier.HandoffFenceHeld))
		}},
		{name: "资格 epoch 不是当前代(铸造后新持有者还没登记)", arrange: held, epoch: 2},
		{name: "恢复墓碑不点名任何实例", arrange: func(t *testing.T, mr *miniredis.Miniredis) {
			seedHandoffFenceForTest(t, mr, playerID, "1||0|"+strings.Repeat("0", 32)+"|R|"+strings.Repeat("b", 32))
		}},
		{name: "没有资格字段", arrange: func(t *testing.T, mr *miniredis.Miniredis) {}},
		{name: "资格原文不规范", arrange: func(t *testing.T, mr *miniredis.Miniredis) {
			seedHandoffFenceForTest(t, mr, playerID, "1|"+replacedOldInstance+"|3|bad|H|bad")
		}},
		{name: "旧实例仍挂在别的键下(失租后以新号重注册,进程还活着)", arrange: func(t *testing.T, mr *miniredis.Miniredis) {
			held(t, mr)
			registerKnownInstanceForTest(t, "SceneNodeService.rpc/replaced-11", testZoneId, "11", replacedOldInstance)
		}},
		{name: "在册注册不带实例号(旧版本)", arrange: func(t *testing.T, mr *miniredis.Miniredis) {
			held(t, mr)
			registerKnownInstanceForTest(t, replacedNodeKey, testZoneId, "10", "")
		}},
		{name: "节点号身份歧义", arrange: func(t *testing.T, mr *miniredis.Miniredis) {
			held(t, mr)
			registerKnownInstanceForTest(t, "SceneNodeService.rpc/replaced-10-dup", testZoneId, "10", "33333333-3333-4333-8333-333333333333")
		}},
		{name: "etcd 复核发现旧实例其实在册(镜像滞后)", arrange: held, fresh: func(t *testing.T) { stubFreshRegistryForTest(t, true, nil) }},
		{name: "etcd 复核失败", arrange: held, want: ownerReplacedUnverified, fresh: func(t *testing.T) {
			stubFreshRegistryForTest(t, false, errors.New("etcd unavailable"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			clearKnownNodesForTest()
			t.Cleanup(clearKnownNodesForTest)
			markKnownNodesSyncedForTest(t)
			forgetInstanceGoneObservationForTest(t, replacedOldInstance)
			registerKnownInstanceForTest(t, replacedNodeKey, testZoneId, "10", replacedNewInstance)
			observeInstanceGoneAtForTest(replacedOldInstance, 2*sceneReentryBarrier(sc))
			if tc.fresh != nil {
				tc.fresh(t)
			} else {
				stubFreshRegistryForTest(t, false, nil)
			}
			tc.arrange(t, mr)
			epoch := tc.epoch
			if epoch == 0 {
				epoch = 1
			}
			verdict := NewEnterSceneLogic(context.Background(), sc).playerLocationOwnerReplaced(playerID, loc, testZoneId, epoch,
				barrierSiteReplacedOwnerTakeover)
			assert.Equal(t, tc.want, verdict)
		})
	}

	// 冻结(F)与撤销(R)同样点名了旧实例:kRegisterFenceLua 对同 E 不同实例一律拒,屏障过后同样接管。
	for _, state := range []savebarrier.HandoffFenceState{savebarrier.HandoffFenceFrozen, savebarrier.HandoffFenceRevoked} {
		t.Run("资格状态 "+string(rune(state))+" 屏障过后同样接管", func(t *testing.T) {
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			clearKnownNodesForTest()
			t.Cleanup(clearKnownNodesForTest)
			markKnownNodesSyncedForTest(t)
			forgetInstanceGoneObservationForTest(t, replacedOldInstance)
			registerKnownInstanceForTest(t, replacedNodeKey, testZoneId, "10", replacedNewInstance)
			observeInstanceGoneAtForTest(replacedOldInstance, 2*sceneReentryBarrier(sc))
			stubFreshRegistryForTest(t, false, nil)
			seedHandoffFenceForTest(t, mr, playerID, holderFenceForTest(1, replacedOldInstance, state))
			verdict := NewEnterSceneLogic(context.Background(), sc).playerLocationOwnerReplaced(playerID, loc, testZoneId, 1,
				barrierSiteReplacedOwnerTakeover)
			assert.Equal(t, ownerReplacedTakeover, verdict)
		})
	}

	// 屏障内资格已是 F:持有者已在同一段 Lua 里原子写出标记,不再写这一代 —— 不拦,交给换手门凭标记判断,
	// 否则失租进程的疏散改派会在屏障内全部被 17 挡住。H 照样拦。
	t.Run("屏障内资格是 F 时交给换手门", func(t *testing.T) {
		sc, mr := newTestSvcCtxWithWorldScenes(t)
		clearKnownNodesForTest()
		t.Cleanup(clearKnownNodesForTest)
		markKnownNodesSyncedForTest(t)
		forgetInstanceGoneObservationForTest(t, replacedOldInstance)
		registerKnownInstanceForTest(t, replacedNodeKey, testZoneId, "10", replacedNewInstance)
		observeInstanceGoneAtForTest(replacedOldInstance, 0)
		stubFreshRegistryForTest(t, false, nil)
		logic := NewEnterSceneLogic(context.Background(), sc)

		seedHandoffFenceForTest(t, mr, playerID, holderFenceForTest(1, replacedOldInstance, savebarrier.HandoffFenceFrozen))
		assert.Equal(t, ownerNotReplaced, logic.playerLocationOwnerReplaced(playerID, loc, testZoneId, 1, barrierSiteReplacedOwnerTakeover))
		seedHandoffFenceForTest(t, mr, playerID, holderFenceForTest(1, replacedOldInstance, savebarrier.HandoffFenceHeld))
		assert.Equal(t, ownerReplacedBarrierPending, logic.playerLocationOwnerReplaced(playerID, loc, testZoneId, 1, barrierSiteReplacedOwnerTakeover))
	})

	t.Run("注册表尚未完成首次同步", func(t *testing.T) {
		sc, mr := newTestSvcCtxWithWorldScenes(t)
		clearKnownNodesForTest()
		t.Cleanup(clearKnownNodesForTest)
		prev := knownNodesSynced.Load()
		knownNodesSynced.Store(false)
		t.Cleanup(func() { knownNodesSynced.Store(prev) })
		forgetInstanceGoneObservationForTest(t, replacedOldInstance)
		registerKnownInstanceForTest(t, replacedNodeKey, testZoneId, "10", replacedNewInstance)
		observeInstanceGoneAtForTest(replacedOldInstance, 2*sceneReentryBarrier(sc))
		stubFreshRegistryForTest(t, false, nil)
		held(t, mr)
		verdict := NewEnterSceneLogic(context.Background(), sc).playerLocationOwnerReplaced(playerID, loc, testZoneId, 1,
			barrierSiteReplacedOwnerTakeover)
		assert.Equal(t, ownerNotReplaced, verdict)
	})
}

// 复核只读占位键:值是 UUID 原文(不是 JSON),空值不算任何进程。
func TestAllocationInstancesReadsRawUUIDs(t *testing.T) {
	instances := allocationInstances([][]byte{[]byte(replacedNewInstance), nil, []byte(""), []byte(replacedOldInstance)})
	assert.Len(t, instances, 2)
	assert.Contains(t, instances, replacedNewInstance)
	assert.Contains(t, instances, replacedOldInstance)
	assert.Empty(t, allocationInstances(nil))
}

// 合并读只复用在调用者到达之后才发出的那一轮:读进行中到达的调用者必须等下一轮,不能拿走正在进行的结果。
func TestRegistryRecheckOnlyReusesRoundsIssuedAfterJoin(t *testing.T) {
	var recheck registryRecheck
	release := make(chan struct{})
	started := make(chan int, 4)
	var reads atomic.Int32
	read := func(context.Context) (map[string]struct{}, error) {
		n := int(reads.Add(1))
		started <- n
		if n == 1 {
			<-release
		}
		return map[string]struct{}{fmt.Sprintf("round-%d", n): {}}, nil
	}

	first := recheck.join(read)
	require.Equal(t, 1, <-started, "第一轮读应立即发出")
	// 第一轮正在读:之后到达的两个调用者加入第二轮,共享一次读。
	second := recheck.join(read)
	third := recheck.join(read)
	assert.Same(t, second, third, "第一轮进行中到达的调用者合并进同一个下一轮")
	assert.NotSame(t, first, second, "不能复用到达之前就已发出的那一轮")
	close(release)

	<-first.done
	<-second.done
	assert.Contains(t, first.instances, "round-1")
	assert.Contains(t, second.instances, "round-2", "第二轮的读在两个调用者到达之后才发出")
	assert.Equal(t, int32(2), reads.Load())
}

// watch 上同一个键换了实例(重启后的新进程覆盖旧注册)、DELETE 都要记下旧实例消失的时刻;实例还挂在别的键下时不记;
// 实例重新在册时作废旧记录;重同步后仍不在册的记录改记为此刻。
func TestRegistryMirrorTracksInstanceObservations(t *testing.T) {
	t.Cleanup(SetWorldConfIdsForTest([]uint64{}))
	putFor := func(sc *svc.ServiceContext, key, value string) {
		handleWatchEvent(context.Background(), sc, &clientv3.Event{Type: clientv3.EventTypePut,
			Kv: &mvccpb.KeyValue{Key: []byte(key), Value: []byte(value)}})
	}
	oldJSON := `{"nodeId":10,"zoneId":1,"sceneNodeType":1,"nodeUuid":"` + replacedOldInstance + `"}`
	newJSON := `{"nodeId":10,"zoneId":1,"sceneNodeType":1,"nodeUuid":"` + replacedNewInstance + `"}`
	setup := func(t *testing.T) *svc.ServiceContext {
		t.Helper()
		sc, _, _ := newNodeDetachTestCtx(t)
		markKnownNodesSyncedForTest(t)
		forgetLocalGoneObservationForTest(t, testZoneId, "10")
		forgetInstanceGoneObservationForTest(t, replacedOldInstance)
		forgetInstanceGoneObservationForTest(t, replacedNewInstance)
		return sc
	}

	t.Run("同键换实例", func(t *testing.T) {
		sc := setup(t)
		putFor(sc, replacedNodeKey, oldJSON)
		_, observed := instanceGoneObservedSince(replacedOldInstance)
		require.False(t, observed, "前提:旧实例在册时没有消失记录")
		putFor(sc, replacedNodeKey, newJSON)
		_, observed = instanceGoneObservedSince(replacedOldInstance)
		assert.True(t, observed)
		instance, ok := knownNodeRegisteredInstance(testZoneId, "10")
		assert.True(t, ok)
		assert.Equal(t, replacedNewInstance, instance)
	})

	t.Run("DELETE", func(t *testing.T) {
		sc := setup(t)
		putFor(sc, replacedNodeKey, oldJSON)
		handleWatchEvent(context.Background(), sc, &clientv3.Event{Type: clientv3.EventTypeDelete,
			Kv: &mvccpb.KeyValue{Key: []byte(replacedNodeKey)}})
		_, observed := instanceGoneObservedSince(replacedOldInstance)
		assert.True(t, observed)
	})

	t.Run("实例还挂在别的键下", func(t *testing.T) {
		sc := setup(t)
		putFor(sc, replacedNodeKey, oldJSON)
		registerKnownInstanceForTest(t, "SceneNodeService.rpc/replaced-11", testZoneId, "11", replacedOldInstance)
		putFor(sc, replacedNodeKey, newJSON)
		_, observed := instanceGoneObservedSince(replacedOldInstance)
		assert.False(t, observed, "实例仍在册,不是消失")
	})

	t.Run("重新在册作废旧记录", func(t *testing.T) {
		sc := setup(t)
		observeInstanceGoneAtForTest(replacedOldInstance, time.Hour/2)
		putFor(sc, replacedNodeKey, oldJSON) // 失租后以同一 UUID 重注册
		_, observed := instanceGoneObservedSince(replacedOldInstance)
		assert.False(t, observed, "陈旧的消失时刻不能留到下一次消失,否则屏障会被提前击穿")
	})

	t.Run("重同步后仍不在册的记录改记为此刻", func(t *testing.T) {
		setup(t)
		observeInstanceGoneAtForTest(replacedOldInstance, time.Hour/2)
		observeInstanceGoneAtForTest(replacedNewInstance, time.Hour/2)
		reconcileInstanceGoneObservationsAfterResync(map[string]struct{}{replacedNewInstance: {}})
		at, observed := instanceGoneObservedSince(replacedOldInstance)
		require.True(t, observed)
		assert.Less(t, time.Since(at), time.Minute, "断档期间可能回来又消失:保守地从此刻重新起算")
		_, observed = instanceGoneObservedSince(replacedNewInstance)
		assert.False(t, observed, "在册的实例作废记录")
	})
}
