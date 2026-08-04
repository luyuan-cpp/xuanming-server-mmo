package logic

// 阶段 C(Agones 高密度容量预占)的单元测试。
//
// 全部用 fake Allocator 跑,不依赖真实 K8s / Agones。真集群行为(GSA 的
// selector 语义、Counters and Lists 的 beta FeatureGate、status 字段形状)
// 必须由 dev 集群 E2E 验收,单测覆盖不到,也不假装覆盖到了。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"

	"scene_manager/internal/agones"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"

	"proto/scene_manager"
	scenenodepb "proto/scene_manager"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// ---------------------------------------------------------------------------
// fake allocator
// ---------------------------------------------------------------------------

type fakeGameServer struct {
	name     string
	podIP    string
	state    string
	count    int64
	capacity int64
}

type fakeAllocator struct {
	mu sync.Mutex

	servers []*fakeGameServer

	// 脚本化失败。
	allocateErr    error
	releaseErr     error
	listErr        error
	acquireOnGsErr error

	// 调用计数,用来断言"回滚发生了 / 没发生"。
	allocateCalls    int
	releaseCalls     int
	acquireOnGsCalls int
	releasedNames    []string
}

func newFakeAllocator(servers ...*fakeGameServer) *fakeAllocator {
	return &fakeAllocator{servers: servers}
}

func (f *fakeAllocator) Allocate(_ context.Context, req agones.AllocationRequest) (*agones.AllocationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allocateCalls++
	if f.allocateErr != nil {
		return nil, f.allocateErr
	}
	// 优先 Allocated 且有余量,其次 Ready 且有余量 —— 与真实 GSA 的
	// selector 顺序保持一致,这样"高密度优先复用已用进程"的语义在测试
	// 里也是被验证的。
	for _, wanted := range []string{"Allocated", "Ready"} {
		for _, gs := range f.servers {
			if gs.state != wanted {
				continue
			}
			if gs.capacity > 0 && gs.count >= gs.capacity {
				continue
			}
			gs.count += req.RoomsAmount
			gs.state = "Allocated"
			return &agones.AllocationResult{
				GameServerName: gs.name,
				PodIP:          gs.podIP,
				RoomsCount:     gs.count,
				RoomsCapacity:  gs.capacity,
			}, nil
		}
	}
	return nil, fmt.Errorf("%w (fake: all full)", agones.ErrNoCapacity)
}

func (f *fakeAllocator) AcquireRoomOnGameServer(_ context.Context, _, gameServerName string, delta int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquireOnGsCalls++
	if f.acquireOnGsErr != nil {
		return f.acquireOnGsErr
	}
	for _, gs := range f.servers {
		if gs.name != gameServerName {
			continue
		}
		if gs.capacity > 0 && gs.count+delta > gs.capacity {
			return fmt.Errorf("%w (fake: %s full)", agones.ErrNoCapacity, gameServerName)
		}
		gs.count += delta
		gs.state = "Allocated"
		return nil
	}
	return fmt.Errorf("%w (fake: %s not found)", agones.ErrNoCapacity, gameServerName)
}

func (f *fakeAllocator) ReleaseRoom(_ context.Context, _, gameServerName string, delta int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls++
	f.releasedNames = append(f.releasedNames, gameServerName)
	if f.releaseErr != nil {
		return f.releaseErr
	}
	for _, gs := range f.servers {
		if gs.name == gameServerName {
			gs.count -= delta
			if gs.count < 0 {
				gs.count = 0
			}
			if gs.count == 0 {
				gs.state = "Ready"
			}
			return nil
		}
	}
	// GameServer 已经消失:真实实现把它当成功(没有可回滚的东西)。
	return nil
}

func (f *fakeAllocator) ListGameServerRooms(_ context.Context, _, _ string) ([]agones.GameServerRooms, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]agones.GameServerRooms, 0, len(f.servers))
	for _, gs := range f.servers {
		out = append(out, agones.GameServerRooms{
			Name: gs.name, PodIP: gs.podIP, State: gs.state,
			Count: gs.count, Capacity: gs.capacity,
		})
	}
	return out, nil
}

func (f *fakeAllocator) roomsOf(name string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, gs := range f.servers {
		if gs.name == name {
			return gs.count
		}
	}
	return -1
}

func (f *fakeAllocator) stats() (allocate, release, acquireOnGs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.allocateCalls, f.releaseCalls, f.acquireOnGsCalls
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// withReachableSceneNode 让某个 nodeId 在测试里"真的能被 CreateScene RPC
// 打通"。CreateScene 失败会回滚,所以任何断言"创建成功"的用例都需要它。
//
// knownNodes 是**包级全局**,registerWorldNodeFromHarness 只写不清。用例结束
// 后必须把条目摘掉,否则下一个用例会把同名 nodeId 解析到一个已经关掉的
// bufconn listener 上,表现为 gRPC 拨号超时十几秒然后莫名其妙地失败。
// 整个测试包共用**一个** fake scene node。
//
// 为什么不是每个用例起一个:CreateScene 现在在 C++ RPC 失败时会回滚,于是
// 几乎每个"建个场景再断言点什么"的用例都需要一个能应答的节点。每个用例都
// 起一遍 bufconn server + grpc.NewClient + 拆掉,单个用例要多花约 1 秒,
// 整包从 1s 涨到 47s。节点本身是无状态的,共用即可。
//
// 用 catch-all 的 resolver + dialer 而不是复用 integration_test 的
// installBufconnDialer:那套按 "bufnet:<numericNodeId>" 索引 knownNodes 里的
// 端口,而本包不少用例用的是 "hot"/"cool"/"A"/"B" 这种非数字 nodeId。
//
// 也刻意**不**写 knownNodes / 不调 updateNodeLoad:很多用例自己 ZAdd 了负载
// 分数来断言"选最闲的那个",动这两处会把它们的前提抹掉;而且 knownNodes 是
// 包级全局,写了不清会污染后续用例。
var sharedFakeSceneNode *fakeSceneNode

func TestMain(m *testing.M) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	sharedFakeSceneNode = &fakeSceneNode{}
	scenenodepb.RegisterSceneNodeGrpcServer(srv, sharedFakeSceneNode)
	go func() { _ = srv.Serve(lis) }()

	restoreResolver := SetNodeEndpointResolverForTest(func(zoneId uint32, nodeId string) (string, bool) {
		// 已经在 knownNodes 里注册过的节点走真实解析 —— integration 用例
		// 靠 "bufnet:<nodeId>" 把不同节点路由到各自的 bufconn listener,
		// 一律 catch-all 会把它们的路由拍平,断言"CreateScene 打到了新节点"
		// 之类的用例就失去意义了。
		if ep, ok, err := resolveFromKnownNodes(zoneId, nodeId); err == nil && ok {
			return ep, true
		}
		return "bufnet:" + nodeId, true
	})
	restoreDialer := SetNodeDialerForTest(func(ctx context.Context, _ string) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough://bufnet",
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
		)
	})

	code := m.Run()

	ResetNodeConnCacheForTest()
	restoreDialer()
	restoreResolver()
	srv.Stop()
	_ = lis.Close()
	os.Exit(code)
}

// installDefaultFakeSceneNode 返回包级共享的 fake 节点,并在用例结束时把
// 它上面被改过的脚本化行为复位 —— 共享对象忘了复位会让后面的用例莫名其妙
// 地失败。本包没有 t.Parallel(),顺序执行,共享是安全的。
func installDefaultFakeSceneNode(t *testing.T) *fakeSceneNode {
	t.Helper()
	// 只复位脚本化行为,**不**清连接缓存:共享 listener 活到整个测试进程结束,
	// 缓存的连接一直有效。每个用例清一次缓存的话,下一个用例要重新建立 gRPC
	// 连接,整包会慢几十秒。
	t.Cleanup(func() { sharedFakeSceneNode.createShouldFail = nil })
	return sharedFakeSceneNode
}

// withReachableSceneNode 是 installDefaultFakeSceneNode 的显式别名,
// 用在那些需要拿到 fake 句柄(比如设 createShouldFail)的用例里。
// nodeIDs 参数保留只是为了可读性 —— resolver 是 catch-all,任何 id 都通。
func withReachableSceneNode(t *testing.T, _ *svc.ServiceContext, _ ...string) *fakeSceneNode {
	t.Helper()
	return installDefaultFakeSceneNode(t)
}

// withAgones 打开 Agones 模式并注入 fake allocator,测试结束自动还原。
func withAgones(t *testing.T, sc *svc.ServiceContext, alloc agones.Allocator, highDensity bool) {
	t.Helper()
	sc.Config.Agones.Enabled = true
	sc.Config.Agones.HighDensityEnabled = highDensity
	sc.Config.Agones.Namespace = "mmorpg-zone-test"
	SetAgonesAllocator(alloc)
	t.Cleanup(func() { SetAgonesAllocator(nil) })
}

// bindPodIP 把一个 PodIP 关联到 knownNodes 里的某个 nodeId,让
// FindNodeByPodIP 能映射回来。
func bindPodIP(t *testing.T, nodeID, podIP string, zoneID, sceneNodeType uint32) {
	t.Helper()
	restore := SetKnownNodeForTest("SceneNodeService.rpc/agones-"+nodeID, nodeID, podIP, zoneID, sceneNodeType)
	t.Cleanup(restore)
}

// ---------------------------------------------------------------------------
// 分配路径
// ---------------------------------------------------------------------------

// 优先选已经 Allocated 且有余量的 GameServer —— 这就是"高密度"的定义:
// 新房间尽量塞进已经在跑的进程,而不是点亮一个新的。
func TestAgones_PrefersAllocatedNodeOverReady(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	mr.ZAdd(testLoadKey(), 0, "10")

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-ready", podIP: "10.0.0.9", state: "Ready", capacity: 4},
		&fakeGameServer{name: "gs-hot", podIP: "10.0.0.8", state: "Allocated", count: 1, capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeInstance)

	placement, err := AcquireAgonesPlacement(context.Background(), sc, testZoneId, constants.NodePurposeInstance)
	require.NoError(t, err)
	assert.Equal(t, "gs-hot", placement.GameServerName)
	assert.Equal(t, "10", placement.NodeID)
	assert.EqualValues(t, 2, alloc.roomsOf("gs-hot"))
	assert.EqualValues(t, 0, alloc.roomsOf("gs-ready"), "ready node must stay cold")
}

// 已 Allocated 的都满了,才回落到 Ready。
func TestAgones_FallsBackToReadyWhenAllocatedIsFull(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	mr.ZAdd(testLoadKey(), 0, "11")

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-full", podIP: "10.0.0.8", state: "Allocated", count: 4, capacity: 4},
		&fakeGameServer{name: "gs-ready", podIP: "10.0.0.9", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "11", "10.0.0.9", testZoneId, constants.SceneNodeTypeInstance)

	placement, err := AcquireAgonesPlacement(context.Background(), sc, testZoneId, constants.NodePurposeInstance)
	require.NoError(t, err)
	assert.Equal(t, "gs-ready", placement.GameServerName)
	assert.EqualValues(t, 4, alloc.roomsOf("gs-full"), "full node must not be oversold")
}

// 全满 -> ErrNoCapacity,且不能预占任何名额。
func TestAgones_NoCapacityIsFailClosed(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	mr.ZAdd(testLoadKey(), 0, "12")

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-full", podIP: "10.0.0.8", state: "Allocated", count: 2, capacity: 2},
	)
	withAgones(t, sc, alloc, true)

	_, err := AcquireAgonesPlacement(context.Background(), sc, testZoneId, constants.NodePurposeInstance)
	require.Error(t, err)
	assert.True(t, errors.Is(err, agones.ErrNoCapacity))
	assert.EqualValues(t, 2, alloc.roomsOf("gs-full"))
}

// GSA 成功但那个 Pod 还没注册进 etcd(启动竞态):必须把名额还回去并拒绝。
// 这一条最容易被写成"先放行,反正一会儿就注册上了"—— 那样会留下一个
// SceneManager 根本不认识的房间。
func TestAgones_AllocationSucceedsButNodeNotRegistered_RollsBack(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-1", podIP: "10.9.9.9", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	// 刻意不 bindPodIP。

	_, err := AcquireAgonesPlacement(context.Background(), sc, testZoneId, constants.NodePurposeInstance)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a registered scene node")
	assert.EqualValues(t, 0, alloc.roomsOf("gs-1"), "room must be returned")

	_, release, _ := alloc.stats()
	assert.Equal(t, 1, release)
}

// zone 对不上:同样还名额 + 拒绝。
func TestAgones_ZoneMismatchRollsBack(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId+777, constants.SceneNodeTypeInstance)

	_, err := AcquireAgonesPlacement(context.Background(), sc, testZoneId, constants.NodePurposeInstance)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belongs to zone")
	assert.EqualValues(t, 0, alloc.roomsOf("gs-1"))
}

// role 对不上(Fleet 标签写的是 instance,但 C++ 报的是 world):
// 放行会让副本房间落到主世界进程上,必须拒绝。
func TestAgones_RoleMismatchRollsBack(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)

	_, err := AcquireAgonesPlacement(context.Background(), sc, testZoneId, constants.NodePurposeInstance)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match purpose")
	assert.EqualValues(t, 0, alloc.roomsOf("gs-1"))
}

// ---------------------------------------------------------------------------
// CreateScene 端到端(含回滚)
// ---------------------------------------------------------------------------

// Agones 打开且没容量 -> CreateScene 必须失败,而且**不允许**悄悄退回
// 按 Redis 负载分数选节点。后者会绕过整个容量约束。
func TestCreateScene_AgonesEnabled_NoCapacity_FailsClosed(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	mr.ZAdd(testLoadKey(), 0, "10")
	withReachableSceneNode(t, sc, "10")

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-full", podIP: "10.0.0.8", state: "Allocated", count: 1, capacity: 1},
	)
	withAgones(t, sc, alloc, true)

	l := NewCreateSceneLogic(context.Background(), sc)
	resp, err := l.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
		SceneType:   scene_manager.SceneType(constants.SceneTypeInstance),
	})
	require.NoError(t, err)
	assert.NotEqual(t, uint32(0), resp.ErrorCode, "must not silently fall back to redis node selection")
	assert.Zero(t, resp.SceneId)

	members, _ := sc.Redis.ZrangeWithScores(activeInstancesKey(testZoneId), 0, -1)
	assert.Empty(t, members, "no phantom scene may be tracked")
}

// C++ CreateScene RPC 失败 -> Redis 状态和 Agones 计数都要回滚,
// 而且不能返回成功。这是修掉 "(Redis state committed)" 那个老行为的回归。
func TestCreateScene_NodeRpcFails_RollsBackRedisAndCounter(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	mr.ZAdd(testLoadKey(), 0, "10")

	fake := withReachableSceneNode(t, sc, "10")
	fake.createShouldFail = errors.New("boom: node refused")

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeInstance)

	l := NewCreateSceneLogic(context.Background(), sc)
	resp, err := l.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
		SceneType:   scene_manager.SceneType(constants.SceneTypeInstance),
	})
	require.NoError(t, err)
	assert.NotEqual(t, uint32(0), resp.ErrorCode, "must not report success for a scene the node rejected")

	// Agones 名额还回去了。
	assert.EqualValues(t, 0, alloc.roomsOf("gs-1"))

	// Redis 里不能留下任何 phantom 状态。
	members, _ := sc.Redis.ZrangeWithScores(activeInstancesKey(testZoneId), 0, -1)
	assert.Empty(t, members)
	assert.False(t, mr.Exists(fmt.Sprintf(SceneNodeKeyFmt, resp.SceneId)))

	// 节点 scene_count 必须减回去,否则负载分数永久偏高。
	cnt, _ := sc.Redis.Get(nodeSceneCountKey(testZoneId, "10"))
	assert.Contains(t, []string{"0", ""}, cnt, "node scene_count must be restored, got %q", cnt)
}

// 非 Agones 模式下同样要回滚 —— phantom scene 与 Agones 无关,
// 是"返回成功但节点上没实体"这件事本身有害。
func TestCreateScene_NodeRpcFails_RollsBackWithoutAgones(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	mr.ZAdd(testLoadKey(), 0, "10")

	fake := withReachableSceneNode(t, sc, "10")
	fake.createShouldFail = errors.New("boom")

	l := NewCreateSceneLogic(context.Background(), sc)
	resp, err := l.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
		SceneType:   scene_manager.SceneType(constants.SceneTypeInstance),
	})
	require.NoError(t, err)
	assert.NotEqual(t, uint32(0), resp.ErrorCode)

	members, _ := sc.Redis.ZrangeWithScores(activeInstancesKey(testZoneId), 0, -1)
	assert.Empty(t, members)
}

// 成功路径:写下 scene:{id}:agones_gs,后续回滚才有依据。
func TestCreateScene_AgonesSuccess_RecordsGameServerMapping(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	mr.ZAdd(testLoadKey(), 0, "10")
	withReachableSceneNode(t, sc, "10")

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeInstance)

	l := NewCreateSceneLogic(context.Background(), sc)
	resp, err := l.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
		SceneType:   scene_manager.SceneType(constants.SceneTypeInstance),
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)

	gs, _ := sc.Redis.Get(sceneAgonesGsKey(resp.SceneId))
	assert.Equal(t, "gs-1", gs)
	assert.EqualValues(t, 1, alloc.roomsOf("gs-1"))
}

// 镜像仍然与源 Scene 共置:Agones 模式下走的是"对指定 GameServer 预占",
// 不是让 GSA 自由挑。共置是硬约束(复用已驻留的地图/AI/spawn)。
func TestCreateScene_Mirror_StillColocatesUnderAgones(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	mr.ZAdd(testLoadKey(), 0, "10")
	withReachableSceneNode(t, sc, "10")

	const sourceSceneId = uint64(777)
	sc.Redis.Set(sceneNodeKey(sourceSceneId), "10")
	sc.Redis.Set(sceneZoneKey(sourceSceneId), fmt.Sprintf("%d", testZoneId))

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-source", podIP: "10.0.0.8", state: "Allocated", count: 1, capacity: 8},
		&fakeGameServer{name: "gs-other", podIP: "10.0.0.9", state: "Ready", capacity: 8},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeInstance)

	l := NewCreateSceneLogic(context.Background(), sc)
	resp, err := l.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		ZoneId:         testZoneId,
		SceneType:      scene_manager.SceneType(constants.SceneTypeInstance),
		SourceSceneId:  sourceSceneId,
		MirrorConfigId: 5,
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, "10", resp.NodeId, "mirror must stay on the source node")

	gs, _ := sc.Redis.Get(sceneAgonesGsKey(resp.SceneId))
	assert.Equal(t, "gs-source", gs)
	assert.EqualValues(t, 2, alloc.roomsOf("gs-source"))
	assert.EqualValues(t, 0, alloc.roomsOf("gs-other"))

	_, _, acquireOnGs := alloc.stats()
	assert.Equal(t, 1, acquireOnGs, "co-location must pin the specific gameserver, not use free GSA")
}

// 源节点满了:镜像失去共置优化,但不能因此把玩家卡死 —— 回落到自由分配。
func TestCreateScene_Mirror_SourceFull_FallsBackToFreeAllocation(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	mr.ZAdd(testLoadKey(), 0, "10")
	mr.ZAdd(testLoadKey(), 1, "20")
	withReachableSceneNode(t, sc, "10", "20")

	const sourceSceneId = uint64(778)
	sc.Redis.Set(sceneNodeKey(sourceSceneId), "10")
	sc.Redis.Set(sceneZoneKey(sourceSceneId), fmt.Sprintf("%d", testZoneId))

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-source", podIP: "10.0.0.8", state: "Allocated", count: 2, capacity: 2},
		&fakeGameServer{name: "gs-other", podIP: "10.0.0.9", state: "Ready", capacity: 8},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeInstance)
	bindPodIP(t, "20", "10.0.0.9", testZoneId, constants.SceneNodeTypeInstance)

	l := NewCreateSceneLogic(context.Background(), sc)
	resp, err := l.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId:    2001,
		ZoneId:         testZoneId,
		SceneType:      scene_manager.SceneType(constants.SceneTypeInstance),
		SourceSceneId:  sourceSceneId,
		MirrorConfigId: 5,
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, "20", resp.NodeId)
	assert.EqualValues(t, 2, alloc.roomsOf("gs-source"), "full source must not be oversold")
	assert.EqualValues(t, 1, alloc.roomsOf("gs-other"))
}

// ---------------------------------------------------------------------------
// 销毁 / 幂等 / 节点死亡
// ---------------------------------------------------------------------------

// 销毁要把名额还回去,而且重复 Destroy 只还一次。
func TestDestroyInstance_ReleasesRoomOnceAndIsIdempotent(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	mr.ZAdd(testLoadKey(), 0, "10")
	withReachableSceneNode(t, sc, "10")

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeInstance)

	ctx := context.Background()
	l := NewCreateSceneLogic(ctx, sc)
	resp, err := l.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
		SceneType:   scene_manager.SceneType(constants.SceneTypeInstance),
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	require.EqualValues(t, 1, alloc.roomsOf("gs-1"))

	destroyInstanceForce(ctx, sc, testZoneId, resp.SceneId, "test")
	assert.EqualValues(t, 0, alloc.roomsOf("gs-1"))

	// 第二次 destroy:映射已经被删,不能再减一次。
	before := alloc.roomsOf("gs-1")
	destroyInstanceForce(ctx, sc, testZoneId, resp.SceneId, "test-again")
	assert.EqualValues(t, before, alloc.roomsOf("gs-1"), "repeated destroy must not double-release")
}

// 原子销毁脚本必须把 scene:{id}:agones_gs 一起删掉,并把值返回给调用方。
// 不返回 = 删完就再也不知道该向谁归还名额。
func TestAtomicDestroyIfIdle_ReturnsAndWipesAgonesMapping(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "node-1")
	sceneId := uint64(4242)

	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sceneId), "10")
	sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, sceneId), "0")
	sc.Redis.Set(sceneAgonesGsKey(sceneId), "gs-77")

	nodeId, gsName, err := AtomicDestroyIfIdle(sc, testZoneId, sceneId)
	require.NoError(t, err)
	assert.Equal(t, "10", nodeId)
	assert.Equal(t, "gs-77", gsName)

	left, _ := sc.Redis.Get(sceneAgonesGsKey(sceneId))
	assert.Equal(t, "", left, "agones mapping must be wiped with the rest of the scene state")
}

// 有玩家时中止:什么都不能动,名额也不能还。
func TestAtomicDestroyIfIdle_AbortKeepsAgonesMapping(t *testing.T) {
	sc, _ := newTestSvcCtx(t, "node-1")
	sceneId := uint64(4243)

	sc.Redis.Set(fmt.Sprintf(SceneNodeKeyFmt, sceneId), "10")
	sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, sceneId), "3")
	sc.Redis.Set(sceneAgonesGsKey(sceneId), "gs-77")

	nodeId, gsName, err := AtomicDestroyIfIdle(sc, testZoneId, sceneId)
	require.NoError(t, err)
	assert.Equal(t, "", nodeId)
	assert.Equal(t, "", gsName)

	left, _ := sc.Redis.Get(sceneAgonesGsKey(sceneId))
	assert.Equal(t, "gs-77", left, "aborted destroy must not touch the mapping")
}

// 归还失败不能把异常吞掉当成功 —— 计数从此漂移,只有 reconcile 能发现。
// 这里只验证"不 panic、不误报成功";告警靠
// scene_manager_agones_counter_rollback_total{outcome="failed"}。
func TestReleaseAgonesRoom_FailureIsSurfacedNotSwallowed(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	alloc := newFakeAllocator(&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Allocated", count: 1, capacity: 4})
	alloc.releaseErr = errors.New("apiserver down")
	withAgones(t, sc, alloc, true)

	ReleaseAgonesRoom(context.Background(), sc, "ns", "gs-1", "test")

	_, release, _ := alloc.stats()
	assert.Equal(t, 1, release)
	assert.EqualValues(t, 1, alloc.roomsOf("gs-1"), "count stays drifted; reconcile must catch it")
}

// ---------------------------------------------------------------------------
// reconcile
// ---------------------------------------------------------------------------

// Agones 说有 3 个房间,Redis 说 1 个 -> 必须报漂移。
func TestReconcile_DetectsCounterDrift(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Allocated", count: 3, capacity: 8},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeInstance)
	sc.Redis.Set(nodeSceneCountKey(testZoneId, "10"), "1")

	drift := ReconcileAgonesRoomsForZone(context.Background(), sc, testZoneId)
	assert.Equal(t, 1, drift)
}

// 两边一致 -> 零漂移。
func TestReconcile_NoDriftWhenConsistent(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-1", podIP: "10.0.0.8", state: "Allocated", count: 2, capacity: 8},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeInstance)
	sc.Redis.Set(nodeSceneCountKey(testZoneId, "10"), "2")

	assert.Equal(t, 0, ReconcileAgonesRoomsForZone(context.Background(), sc, testZoneId))
}

// 节点死亡:Agones 还认为它有房间,但 SceneManager 已经不认识这个 Pod。
// 这正是要暴露的漂移,不能静默。
func TestReconcile_DeadNodeWithRoomsIsDrift(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)

	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-dead", podIP: "10.0.0.250", state: "Allocated", count: 2, capacity: 8},
	)
	withAgones(t, sc, alloc, true)
	// 不 bindPodIP == 节点没注册/已死。

	assert.Equal(t, 1, ReconcileAgonesRoomsForZone(context.Background(), sc, testZoneId))
}

// 关掉 Agones 时,reconcile 与所有分配动作都必须是 no-op。
func TestAgonesDisabled_IsCompletelyInert(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	mr.ZAdd(testLoadKey(), 0, "10")
	withReachableSceneNode(t, sc, "10")

	assert.False(t, AgonesEnabled(sc))
	assert.Equal(t, 0, ReconcileAgonesRoomsForZone(context.Background(), sc, testZoneId))

	l := NewCreateSceneLogic(context.Background(), sc)
	resp, err := l.CreateScene(&scene_manager.CreateSceneRequest{
		SceneConfId: 2001,
		ZoneId:      testZoneId,
		SceneType:   scene_manager.SceneType(constants.SceneTypeInstance),
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.ErrorCode)

	gs, _ := sc.Redis.Get(sceneAgonesGsKey(resp.SceneId))
	assert.Equal(t, "", gs, "no agones mapping may be written when disabled")
}
