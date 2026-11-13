package logic

// 回归(2026-11-11):世界频道改派前先占名额(docs/design/agones-scene-node-high-density.md §13.10)。
//
// 以前四条改派路径(rebalance 迁移、懒改派、init 的死节点改派、EnterScene 的死节点自愈)按哈希或最低负载挑目标,
// 先改归属、建实体,事后才转名额;目标 GameServer 满了就把名额留在旧 GameServer 上。整个 Fleet 的总数对,却记错了台:
// GSA 按新的那台「还有空位」继续往里放,那台进程超出压测定下的容量。现在改派与新建频道一样先占名额再搬:
//   - rebalance 换更好的家只认计划的目标,满了这次不搬(BetterHomeSkipsAFullTarget);
//   - 属主已死的四条路径,目标满了交给 GSA 另挑一台有空位的,落点改成它,交给调用方的也是它(DeadOwnerFallsBack…);
//   - 哪台都占不到就不搬,一个字节不改、不发 CreateScene(DeadOwnerWithoutCapacity…);
//   - 回落分配挑中属主自己(Pod 还在 etcd 里、负载集已判它不在)也不搬,占的那一条放弃(FallbackPickingTheDeadOwner…);
//   - CAS 落空时按回收脚本比对出的属主收拾名额:属主恰是落点就提交,否则放弃(LostCASSettlesTheRoom…);
//   - 进场请求路径上(EnterScene 自愈、懒改派)同一进程里同时到的改派按频道合并成一次(ConcurrentRequestPathMoves…)。
//
// 修复前这些路径没有「先占名额」这一步,新用例按变异取红(每次一种,在 %TEMP% 的副本里做;与 PROGRESS 2026-11-11
// 「世界频道改派前先占名额」条目的 R1~R7 一致):
//   - R1 reserveAgonesRoomForWorldChannel 不看 allowFallback、总是回落:BetterHomeSkipsAFullTarget 红,
//     agones_world_channel_test.go 的 MoveRefusesAFullTargetWithoutFallback 红。
//   - R2 migrateWorldChannel 的回落条件写成 reason == reasonBetterHome:DeadOwnerFallsBack 的 rebalance_node_gone
//     子例与 BetterHomeSkipsAFullTarget 红。
//   - R3 删掉 moveStaleWorldChannel(懒改派的搬迁一半)里「落点 == 老属主」的判断:FallbackPickingTheDeadOwner 的
//     lazy_reassign 子例红(另外三条路径的同一判断各对应自己的子例)。
//   - R4 moveStaleWorldChannel 成功时回 targetNode 而不是实际落点:DeadOwnerFallsBack 的 lazy_reassign 子例红。
//   - R5 moveChannelOffDeadNode(死节点自愈的搬迁一半)成功时回 bestNodeId:DeadOwnerFallsBack 的 enter_scene_self_heal
//     子例红。
//   - R6 settleLostChannelMove 不看比对结果、一律放弃名额:LostCASSettlesTheRoom 的 same_target 子例红。
//     (反过来「一律提交」取不红:提交的 CAS 自己比对属主,属主不是落点就放弃这一条。)
//   - R7 删掉 commitPreCreatedWorldChannel 里的 commitReservedAgonesRoom:DeadOwnerFallsBack 的 lazy_reassign、
//     init_dead_node 子例红(记录仍在旧条目上、在途占用没消费)。
//   - R8 把 reassignSceneOffDeadNode、lazyReassignStaleWorldChannel 里的 joinChannelMove 换成直接调用搬迁函数:
//     ConcurrentRequestPathMovesShareOneMove 红(等待者的前提等不到,放开之后占名额、建实体都不止一次)。

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
)

const (
	reserveMoveZone   uint32 = 1
	reserveMoveConfID uint64 = 4797
)

// 节点号不与别的用例重名,理由同 reassign_cas_test.go 的 casNode* 常量。
const (
	reserveNodeOwner  = "8971" // 频道的属主
	reserveNodeTarget = "8972" // 计划的目标:rebalance 的计划、懒改派与 init 的哈希目标、EnterScene 挑的最低负载节点
	reserveNodeSpare  = "8973" // 回落分配能挑到的另一台
)

// 三台假 GameServer,容量都是 2。
const (
	reserveGsOwner  = "gs-owner"
	reserveGsTarget = "gs-target"
	reserveGsSpare  = "gs-spare"
)

// reserveMoveSetup 是现场的可变部分。
type reserveMoveSetup struct {
	// ownerAlive:属主在负载集里(rebalance 换更好的家);否则已判死,再入屏障放行。
	ownerAlive bool
	// ownerGsState:属主 GameServer 的状态。死节点的一般已是 Unhealthy,GSA 不会挑;
	// Allocated 且有空位时,GSA 会优先挑它(假 Allocate 与真 GSA 的 selector 顺序一致)。
	ownerGsState string
	targetFull   bool
	spareFull    bool
}

// reserveMoveCase 是一次频道改派的现场:频道 sceneID 是 reserveMoveConfID 在 zone 1 的在役频道,归 owner(属主映射、
// 反向索引、scene_count=1 齐全),名额记在 gs-owner 上(record)。target、spare 是活的世界节点,负载分让 EnterScene
// 挑 target。Agones 的节点 → Pod 映射要 knownNodes 里有独立的 Pod IP,所以与 TestResolveScene_DeadOwner_AgonesRoomFollowsOwnership
// 一样不用 bufconn 桩节点,CreateScene / DestroyScene 落到 TestMain 的共享桩上。
type reserveMoveCase struct {
	sc      *svc.ServiceContext
	mr      *miniredis.Miniredis
	alloc   *fakeAllocator
	sceneID uint64
	record  string
}

func newReserveMoveCase(t *testing.T, sceneID uint64, setup reserveMoveSetup, opts ...redis.Hook) *reserveMoveCase {
	t.Helper()
	clearKnownNodesForTest()
	ResetNodeConnCacheForTest()
	t.Cleanup(clearKnownNodesForTest)
	resetNodeDeathQueuesForTest(t)

	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, opts...)
	installDefaultFakeSceneNode(t)
	gameServer := func(name, podIP, state string, full bool) *fakeGameServer {
		gs := &fakeGameServer{name: name, podIP: podIP, state: state, capacity: 2}
		if full {
			gs.count = 2
		}
		return gs
	}
	alloc := newFakeAllocator(
		gameServer(reserveGsOwner, "10.0.88.71", setup.ownerGsState, false),
		gameServer(reserveGsTarget, "10.0.88.72", "Allocated", setup.targetFull),
		gameServer(reserveGsSpare, "10.0.88.73", "Ready", setup.spareFull),
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, reserveNodeOwner, "10.0.88.71", reserveMoveZone, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, reserveNodeTarget, "10.0.88.72", reserveMoveZone, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, reserveNodeSpare, "10.0.88.73", reserveMoveZone, constants.SceneNodeTypeMainWorld)

	_, err := mr.ZAdd(nodeLoadKey(reserveMoveZone), 0, reserveNodeTarget)
	require.NoError(t, err)
	_, err = mr.ZAdd(nodeLoadKey(reserveMoveZone), 5, reserveNodeSpare)
	require.NoError(t, err)
	if setup.ownerAlive {
		_, err = mr.ZAdd(nodeLoadKey(reserveMoveZone), 9, reserveNodeOwner)
		require.NoError(t, err)
	}

	_, err = mr.SAdd(worldChannelsKey(reserveMoveZone, reserveMoveConfID), strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)
	seedSceneOwnedBy(t, mr, reserveMoveZone, sceneID, reserveNodeOwner)
	record := seedAgonesRoom(t, sc, alloc, sceneID, reserveGsOwner)

	// 前提:属主死活与设定一致、死属主的屏障放行,EnterScene 挑的是 target。
	require.Equal(t, setup.ownerAlive, IsNodeAlive(sc, reserveMoveZone, reserveNodeOwner))
	if !setup.ownerAlive {
		allowed, _ := CanReclaimDeadNode(sc, reserveMoveZone, reserveNodeOwner)
		require.True(t, allowed, "前提:再入屏障放行")
	}
	best, err := GetBestNodeForPurpose(context.Background(), sc, reserveMoveZone, constants.NodePurposeWorld)
	require.NoError(t, err)
	require.Equal(t, reserveNodeTarget, best, "前提:EnterScene 的死节点自愈挑 target")

	return &reserveMoveCase{sc: sc, mr: mr, alloc: alloc, sceneID: sceneID, record: record}
}

// requireOwnedBy 断言频道的记账全部指向 node:属主映射、反向索引、scene_count 都只改了一次(或都没改)。
func (c *reserveMoveCase) requireOwnedBy(t *testing.T, node string) {
	t.Helper()
	requireSceneOwner(t, c.sc, c.sceneID, node)
	var others []string
	for _, n := range []string{reserveNodeOwner, reserveNodeTarget, reserveNodeSpare} {
		if n != node {
			others = append(others, n)
		}
	}
	requireSceneIndexedOnlyOn(t, c.sc, reserveMoveZone, c.sceneID, node, others...)
	require.EqualValues(t, 1, readInt64(c.sc, nodeSceneCountKey(reserveMoveZone, node)), "属主的 scene_count")
	for _, n := range others {
		require.EqualValues(t, 0, readInt64(c.sc, nodeSceneCountKey(reserveMoveZone, n)), "节点 %s 的 scene_count", n)
	}
}

// requireUnmoved 断言归属、记账与名额记录一个字节都没改,属主 GameServer 上的旧条目还在,也没留下在途占用。
func (c *reserveMoveCase) requireUnmoved(t *testing.T) {
	t.Helper()
	c.requireOwnedBy(t, reserveNodeOwner)
	record, err := c.sc.Redis.Get(sceneAgonesGsKey(reserveMoveZone, c.sceneID))
	require.NoError(t, err)
	require.Equal(t, c.record, record, "名额记录不动")
	require.Contains(t, c.alloc.entriesOf(reserveGsOwner), parseAgonesRoom(c.record).Entry, "属主 GameServer 上的旧条目不动")
	require.Empty(t, agonesHoldsOf(t, c.sc, reserveMoveZone), "没搬就不留在途占用")
}

// deadOwnerMovePath 是属主已死时把频道搬走的一条路径。move 回这条路径交给调用方的落点(没搬时为空串):
// 懒改派按它记候选与应答,init 按它记不可达,EnterScene 按它把玩家派过去。
type deadOwnerMovePath struct {
	name string
	move func(t *testing.T, c *reserveMoveCase) string
}

func deadOwnerMovePaths() []deadOwnerMovePath {
	return []deadOwnerMovePath{
		{name: "rebalance_node_gone", move: func(t *testing.T, c *reserveMoveCase) string {
			if !migrateWorldChannel(context.Background(), c.sc, reserveMoveZone, reserveMoveConfID, c.sceneID,
				reserveNodeOwner, reserveNodeTarget, reasonNodeGone) {
				return ""
			}
			// rebalance 不回落点,以迁移之后的属主为准。
			owner, err := c.sc.Redis.Get(sceneNodeKey(reserveMoveZone, c.sceneID))
			require.NoError(t, err)
			return owner
		}},
		{name: "lazy_reassign", move: func(t *testing.T, c *reserveMoveCase) string {
			movedTo, ok := lazyReassignStaleWorldChannel(context.Background(), c.sc, reserveMoveZone, reserveMoveConfID,
				c.sceneID, reserveNodeTarget)
			if !ok {
				require.Empty(t, movedTo, "没搬时不回落点")
			}
			return movedTo
		}},
		{name: "init_dead_node", move: func(t *testing.T, c *reserveMoveCase) string {
			rpcNode, _, err := moveWorldChannelOffDeadNode(context.Background(), c.sc, reserveMoveZone, reserveMoveConfID,
				c.sceneID, reserveNodeOwner, reserveNodeTarget)
			require.NoError(t, err)
			return rpcNode
		}},
		{name: "enter_scene_self_heal", move: func(t *testing.T, c *reserveMoveCase) string {
			sid, nid, err := NewEnterSceneLogic(context.Background(), c.sc).resolveScene(c.sceneID, reserveMoveConfID, reserveMoveZone)
			if err != nil {
				require.False(t, errors.Is(err, ErrReentryBarrierPending), "屏障早已放行,这不是屏障拒绝")
				return ""
			}
			require.Equal(t, c.sceneID, sid)
			return nid
		}},
	}
}

// 属主已死、目标满了:四条路径都交给 GSA 另挑一台有空位的(spare),落点改成它,交给调用方的也是它;
// 名额提交到 spare 的 GameServer,属主 GameServer 上的旧条目删掉,目标那台一个都不多占。
func TestReserveBeforeMove_DeadOwnerFallsBackToAFreeGameServer(t *testing.T) {
	for i, path := range deadOwnerMovePaths() {
		t.Run(path.name, func(t *testing.T) {
			c := newReserveMoveCase(t, 88601+uint64(i), reserveMoveSetup{ownerGsState: "Unhealthy", targetFull: true})
			createsBefore := sharedFakeSceneNode.createCalls.Load()

			got := path.move(t, c)

			require.Equal(t, reserveNodeSpare, got, "交给调用方的必须是实际落点,不是满了的目标")
			c.requireOwnedBy(t, reserveNodeSpare)
			require.Equal(t, createsBefore+1, sharedFakeSceneNode.createCalls.Load(), "只在落点上建一次实体")
			require.Equal(t, c.sceneID, sharedFakeSceneNode.lastCreateScene.Load())
			requireAgonesRecordOn(t, c.sc, c.alloc, c.sceneID, reserveGsSpare)
			require.EqualValues(t, 1, c.alloc.roomsOf(reserveGsSpare))
			require.EqualValues(t, 0, c.alloc.roomsOf(reserveGsOwner), "属主 GameServer 上的旧条目必须删掉")
			require.EqualValues(t, 2, c.alloc.roomsOf(reserveGsTarget), "满了的目标不得被超卖")
			require.Empty(t, agonesHoldsOf(t, c.sc, reserveMoveZone), "提交时在途占用一起消费")
		})
	}
}

// 属主已死、目标与另一台都满了:四条路径都不搬 —— 归属、记账、名额记录一个字节不改,也不发 CreateScene。
// 与新建频道没有容量时 fail-closed 同一取舍:不把房间塞进 Agones 认为已经满了的进程。频道留在死节点上、暂时
// 进不去,下一拍、下一次请求再试,FleetAutoscaler 扩出空位之后就能搬。
func TestReserveBeforeMove_DeadOwnerWithoutCapacityDoesNotMove(t *testing.T) {
	for i, path := range deadOwnerMovePaths() {
		t.Run(path.name, func(t *testing.T) {
			c := newReserveMoveCase(t, 88611+uint64(i),
				reserveMoveSetup{ownerGsState: "Unhealthy", targetFull: true, spareFull: true})
			createsBefore := sharedFakeSceneNode.createCalls.Load()

			require.Empty(t, path.move(t, c), "哪台都占不到就不搬")

			c.requireUnmoved(t)
			require.Equal(t, createsBefore, sharedFakeSceneNode.createCalls.Load(), "没占到名额就不建实体")
			require.EqualValues(t, 2, c.alloc.roomsOf(reserveGsTarget))
			require.EqualValues(t, 2, c.alloc.roomsOf(reserveGsSpare))
		})
	}
}

// 回落分配挑中了属主自己:它的 Pod 还在 etcd 里(GSA 按 Pod 地址认节点),负载集却已判它不在,它的 GameServer
// 也还有空位。搬过去等于没搬:四条路径都不搬,GSA 在属主 GameServer 上加的那一条放弃。
func TestReserveBeforeMove_FallbackPickingTheDeadOwnerDoesNotMove(t *testing.T) {
	for i, path := range deadOwnerMovePaths() {
		t.Run(path.name, func(t *testing.T) {
			c := newReserveMoveCase(t, 88621+uint64(i), reserveMoveSetup{ownerGsState: "Allocated", targetFull: true})
			createsBefore := sharedFakeSceneNode.createCalls.Load()

			require.Empty(t, path.move(t, c))

			c.requireUnmoved(t)
			require.Equal(t, []string{parseAgonesRoom(c.record).Entry}, c.alloc.entriesOf(reserveGsOwner),
				"GSA 在属主 GameServer 上加的那一条必须放弃")
			allocations, _, _ := c.alloc.stats()
			require.Equal(t, 1, allocations, "前提:回落分配真的走了一次")
			require.Equal(t, createsBefore, sharedFakeSceneNode.createCalls.Load(), "不在属主上重建实体")
			require.EqualValues(t, 0, c.alloc.roomsOf(reserveGsSpare))
		})
	}
}

// rebalance 换更好的家(属主活着)只认计划的目标:目标满了这次不搬,也不交给 GSA 另挑 —— 搬到别处就违背了这次
// 迁移的目的。对照:目标空出位子之后照常搬,名额提交到目标,属主 GameServer 上的旧条目删掉。
func TestReserveBeforeMove_BetterHomeSkipsAFullTarget(t *testing.T) {
	c := newReserveMoveCase(t, 88631, reserveMoveSetup{ownerAlive: true, ownerGsState: "Allocated", targetFull: true})
	ctx := context.Background()
	migrate := func() bool {
		return migrateWorldChannel(ctx, c.sc, reserveMoveZone, reserveMoveConfID, c.sceneID,
			reserveNodeOwner, reserveNodeTarget, reasonBetterHome)
	}
	createsBefore := sharedFakeSceneNode.createCalls.Load()

	require.False(t, migrate(), "目标满了这次不搬")
	c.requireUnmoved(t)
	require.Equal(t, createsBefore, sharedFakeSceneNode.createCalls.Load(), "没占到名额就不建实体")
	allocations, _, _ := c.alloc.stats()
	require.Zero(t, allocations, "换更好的家不回落到 GSA 自由分配")
	require.EqualValues(t, 0, c.alloc.roomsOf(reserveGsSpare))
	require.EqualValues(t, 2, c.alloc.roomsOf(reserveGsTarget))

	// 对照:目标空出一个位子。
	require.NoError(t, c.alloc.RemoveRoomEntry(ctx, "", reserveGsTarget, "seed-"+reserveGsTarget+"-0"))
	require.True(t, migrate())
	c.requireOwnedBy(t, reserveNodeTarget)
	requireAgonesRecordOn(t, c.sc, c.alloc, c.sceneID, reserveGsTarget)
	require.EqualValues(t, 2, c.alloc.roomsOf(reserveGsTarget))
	require.EqualValues(t, 0, c.alloc.roomsOf(reserveGsOwner), "提交了就删旧条目")
	require.Empty(t, agonesHoldsOf(t, c.sc, reserveMoveZone))
}

// CAS 落空时按回收脚本比对出的属主收拾这次占的名额(settleLostChannelMove)。另一个写者在这次的 CAS 之前抢先提交、
// 没提交名额:
//   - 它也搬到了 target:实体留着,这次占的那一条照常提交,旧条目删掉;
//   - 它搬到了 spare:回收 target 上预建的实体,这次占的那一条放弃,旧条目与记录不动(归改派成功的写者处理)。
func TestReserveBeforeMove_LostCASSettlesTheRoomByTheCurrentOwner(t *testing.T) {
	cases := []struct {
		name    string
		sceneID uint64
		winner  string
	}{
		{name: "same_target", sceneID: 88641, winner: reserveNodeTarget},
		{name: "another_node", sceneID: 88642, winner: reserveNodeSpare},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := &beforeEvalHook{script: luaReassignSceneNode}
			c := newReserveMoveCase(t, tc.sceneID, reserveMoveSetup{ownerGsState: "Unhealthy"}, hook)
			// go-redis 的 hook 跑在调用方协程上,可以用 require。
			hook.arm(func() {
				require.True(t, reassignSceneNode(c.sc, reserveMoveZone, c.sceneID, reserveNodeOwner, tc.winner,
					freshPlacementGeneration(t, c.sc, reserveMoveZone)))
			})
			destroysBefore := sharedFakeSceneNode.destroyCalls.Load()

			require.False(t, migrateWorldChannel(context.Background(), c.sc, reserveMoveZone, reserveMoveConfID, c.sceneID,
				reserveNodeOwner, reserveNodeTarget, reasonNodeGone))
			require.True(t, hook.fired.Load(), "前提:另一个写者插在这次的 CAS 之前")

			c.requireOwnedBy(t, tc.winner)
			require.Empty(t, agonesHoldsOf(t, c.sc, reserveMoveZone))
			if tc.winner == reserveNodeTarget {
				require.Equal(t, destroysBefore, sharedFakeSceneNode.destroyCalls.Load(), "target 上是属主的活场景,不得回收")
				requireAgonesRecordOn(t, c.sc, c.alloc, c.sceneID, reserveGsTarget)
				require.EqualValues(t, 1, c.alloc.roomsOf(reserveGsTarget))
				require.EqualValues(t, 0, c.alloc.roomsOf(reserveGsOwner), "提交了就删旧条目")
				return
			}
			require.Equal(t, destroysBefore+1, sharedFakeSceneNode.destroyCalls.Load(), "target 上预建的实体没有归属,必须回收")
			record, err := c.sc.Redis.Get(sceneAgonesGsKey(reserveMoveZone, c.sceneID))
			require.NoError(t, err)
			require.Equal(t, c.record, record, "属主不是这次的落点:记录不动")
			require.EqualValues(t, 0, c.alloc.roomsOf(reserveGsTarget), "这次在 target 上占的那一条必须放弃")
			require.EqualValues(t, 1, c.alloc.roomsOf(reserveGsOwner), "旧条目归改派成功的写者处理")
		})
	}
}

// gatedAllocator 把第一次 AddRoomEntry 卡住:进入时关 entered,等 release 关闭再往下走。用例借它把一次改派停在
// 「占名额」上,让别的请求在这时进来。之后的 AddRoomEntry 不卡(用例里不该再有)。
type gatedAllocator struct {
	*fakeAllocator
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedAllocator) AddRoomEntry(ctx context.Context, namespace, gameServerName, entry string) error {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return g.fakeAllocator.AddRoomEntry(ctx, namespace, gameServerName, entry)
}

// 节点一死,它上面的玩家一起重连、一起进同一个频道:同一进程里对同一个频道同时到的 EnterScene 自愈与懒改派合并成
// 一次改派(joinChannelMove)—— 只占一次名额、只建一次实体,其余请求拿同一个落点。
// 不合并时每个请求都先占名额:目标 GameServer 被这些临时占用占满,后到的回落到 GSA、在别的节点上白建白拆,
// K8s 写入随请求数放大。变异取红:把 reassignSceneOffDeadNode / lazyReassignStaleWorldChannel 里的 joinChannelMove
// 换成直接调用搬迁函数,本用例的「只占一次」「只建一次」红(waiters 前提也等不到)。
func TestReserveBeforeMove_ConcurrentRequestPathMovesShareOneMove(t *testing.T) {
	c := newReserveMoveCase(t, 88651, reserveMoveSetup{ownerGsState: "Unhealthy"})
	gate := &gatedAllocator{fakeAllocator: c.alloc, entered: make(chan struct{}), release: make(chan struct{})}
	SetAgonesAllocator(gate) // withAgones 已登记结束时复位
	createsBefore := sharedFakeSceneNode.createCalls.Load()

	type outcome struct {
		node string
		err  error
	}
	selfHeal := func() outcome {
		_, nid, err := NewEnterSceneLogic(context.Background(), c.sc).resolveScene(c.sceneID, reserveMoveConfID, reserveMoveZone)
		return outcome{node: nid, err: err}
	}
	lazy := func() outcome {
		movedTo, ok := lazyReassignStaleWorldChannel(context.Background(), c.sc, reserveMoveZone, reserveMoveConfID,
			c.sceneID, reserveNodeTarget)
		if !ok {
			return outcome{err: errors.New("lazy reassign did not move the channel")}
		}
		return outcome{node: movedTo}
	}

	const followers = 6
	results := make([]outcome, followers+1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0] = selfHeal()
	}()
	waitClosedForTest(t, gate.entered, "the first move to reach the room reservation")

	for i := 1; i <= followers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				results[i] = lazy()
				return
			}
			results[i] = selfHeal()
		}(i)
	}
	require.Eventually(t, func() bool { return channelMoveWaitersForTest(c.sceneID) == followers },
		5*time.Second, time.Millisecond, "前提:后到的 %d 个请求都在等第一次的结果", followers)

	close(gate.release)
	wg.Wait()

	for i, r := range results {
		require.NoError(t, r.err, "请求 %d", i)
		require.Equal(t, reserveNodeTarget, r.node, "请求 %d 拿到的落点", i)
	}
	c.requireOwnedBy(t, reserveNodeTarget)
	allocations, _, adds := c.alloc.stats()
	require.Equal(t, 1, adds, "只占一次名额")
	require.Zero(t, allocations, "没有请求回落到 GSA")
	require.Equal(t, createsBefore+1, sharedFakeSceneNode.createCalls.Load(), "只建一次实体")
	requireAgonesRecordOn(t, c.sc, c.alloc, c.sceneID, reserveGsTarget)
	require.EqualValues(t, 1, c.alloc.roomsOf(reserveGsTarget))
	require.EqualValues(t, 0, c.alloc.roomsOf(reserveGsOwner), "提交了就删旧条目")
	require.Empty(t, agonesHoldsOf(t, c.sc, reserveMoveZone))
	require.False(t, channelMoveRunningForTest(c.sceneID), "结束之后不留记录")
}
