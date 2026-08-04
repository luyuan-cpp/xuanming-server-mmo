package logic

// Integration harness for the world-channel rebalance pipeline.
//
// Most tests in this package exercise pure helpers (planner, config loader,
// Redis plumbing) in isolation. This file plugs the whole pipeline together
// using in-process fakes:
//
//   Redis           -> miniredis
//   Etcd watch      -> simulated by calling handleWatchEvent / direct
//                      knownNodes mutation (driving the in-memory registry
//                      the same way the watch loop would)
//   C++ scene node  -> bufconn-backed fake SceneNodeGrpcServer whose RPC
//                      counters are asserted at the end of each test
//
// Goal: catch regressions that pure-unit tests miss, e.g. a rebalance
// planner that looks correct on paper but never actually fires CreateScene
// on the new node because the dialer / cache / endpoint-resolution path
// was silently broken by a refactor.

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"

	basepb "proto/common/base"
	scenepb "proto/scene"
	scenenodepb "proto/scene_manager"

	"scene_manager/internal/config"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
	"shared/snowflake"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// fakeSceneNode is the bufconn-backed test double for a C++ scene node.
// Counters use atomics so the test goroutine can observe them without
// synchronising with the gRPC server goroutines.
type fakeSceneNode struct {
	scenenodepb.UnimplementedSceneNodeGrpcServer

	createCalls      atomic.Int64
	destroyCalls     atomic.Int64
	releaseCalls     atomic.Int64
	lastCreateConf   atomic.Uint64
	lastCreateScene  atomic.Uint64
	lastDestroyScene atomic.Uint64

	// createShouldFail, if non-nil, makes CreateScene return it. Used by
	// the "new node misbehaving" test to prove the redis mapping stays on
	// the old node when the migration can't complete.
	createShouldFail error
}

func (f *fakeSceneNode) CreateScene(_ context.Context, req *scenepb.CreateSceneRequest) (*scenepb.CreateSceneResponse, error) {
	f.createCalls.Add(1)
	f.lastCreateConf.Store(uint64(req.ConfigId))
	f.lastCreateScene.Store(req.SceneId)
	if f.createShouldFail != nil {
		return nil, f.createShouldFail
	}
	return &scenepb.CreateSceneResponse{}, nil
}

func (f *fakeSceneNode) DestroyScene(_ context.Context, req *scenepb.DestroySceneRequest) (*basepb.Empty, error) {
	f.destroyCalls.Add(1)
	f.lastDestroyScene.Store(req.SceneId)
	return &basepb.Empty{}, nil
}

func (f *fakeSceneNode) ReleasePlayer(_ context.Context, _ *scenenodepb.ReleasePlayerRequest) (*basepb.Empty, error) {
	f.releaseCalls.Add(1)
	return &basepb.Empty{}, nil
}

// sceneNodeHarness bundles a running bufconn-backed fake scene node with
// the endpoint string the SceneManager will use to "dial" it. The endpoint
// is a synthetic "bufnet:<nodeId>" — the installed test dialer routes it
// by nodeId, so the scene manager code path is unchanged.
type sceneNodeHarness struct {
	nodeID string
	fake   *fakeSceneNode
	lis    *bufconn.Listener
	srv    *grpc.Server
}

func (h *sceneNodeHarness) stop() {
	h.srv.Stop()
	_ = h.lis.Close()
}

// startFakeSceneNode spins up a bufconn gRPC server hosting a fakeSceneNode.
// The returned harness is automatically torn down by t.Cleanup so a panic
// mid-test doesn't leak goroutines into the next test.
func startFakeSceneNode(t *testing.T, nodeID string) *sceneNodeHarness {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	fake := &fakeSceneNode{}
	scenenodepb.RegisterSceneNodeGrpcServer(srv, fake)

	go func() {
		// bufconn.Listener.Accept returns io.EOF once Close is called;
		// grpc.Server.Serve treats that as a clean shutdown, not an error.
		_ = srv.Serve(lis)
	}()

	h := &sceneNodeHarness{nodeID: nodeID, fake: fake, lis: lis, srv: srv}
	t.Cleanup(h.stop)
	return h
}

// installBufconnDialer wires a test dialer that routes "bufnet:<nodeId>"
// endpoints to the matching bufconn listener. Any unknown endpoint produces
// an explicit error so a typo in the test surfaces loudly instead of
// hanging for grpc.NewClient's default timeout.
func installBufconnDialer(t *testing.T, harnesses ...*sceneNodeHarness) {
	t.Helper()
	index := make(map[string]*bufconn.Listener, len(harnesses))
	for _, h := range harnesses {
		index["bufnet:"+h.nodeID] = h.lis
	}
	restore := SetNodeDialerForTest(func(ctx context.Context, endpoint string) (*grpc.ClientConn, error) {
		lis, ok := index[endpoint]
		if !ok {
			return nil, fmt.Errorf("test dialer: unknown endpoint %q", endpoint)
		}
		return grpc.NewClient("passthrough://bufnet",
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
		)
	})
	t.Cleanup(func() {
		ResetNodeConnCacheForTest() // drop cached conns pointing at torn-down listeners
		restore()
	})
}

// newIntegrationSvcCtx returns a ServiceContext backed by miniredis, with
// rebalance- and load-related config populated. No Etcd client is wired:
// the integration tests drive the knownNodes map directly to side-step the
// etcd watch layer. (There is a separate lightweight test elsewhere in
// this package that covers the watch-event handler with synthetic events.)
func newIntegrationSvcCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rds := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})

	c := config.Config{}
	c.WorldChannelCount = 1
	c.MaxRebalanceMigrationsPerTick = 16 // generous so tests don't hit budget
	c.NodeLoadWeightSceneCount = 1.0
	c.NodeLoadWeightPlayerCount = 0.01

	return &svc.ServiceContext{
		Config:     c,
		Redis:      rds,
		SceneIDGen: snowflake.NewNode(0),
	}, mr
}

// registerWorldNodeFromHarness mirrors what the real watch loop does when
// a scene node appears in etcd: it inserts an entry into knownNodes AND
// calls updateNodeLoad so the zone load-set is populated and the node
// type mirror is written. This is the minimum state getNodesForPurpose /
// PlanWorldChannelRebalance need to see the node as a live world host.
func registerWorldNodeFromHarness(t *testing.T, sc *svc.ServiceContext, zoneId uint32, h *sceneNodeHarness) {
	t.Helper()
	nodeIdU64, err := strconv.ParseUint(h.nodeID, 10, 32)
	require.NoError(t, err, "harness nodeID must be numeric for registration")

	entry := nodeEntry{
		nodeID: h.nodeID,
		reg: sceneNodeRegistration{
			NodeId:        uint32(nodeIdU64),
			NodeType:      0x14, // SceneNodeService
			SceneNodeType: constants.SceneNodeTypeMainWorld,
			ZoneId:        zoneId,
		},
	}
	// Endpoint string must round-trip as "bufnet:<nodeId>" because that's
	// what installBufconnDialer indexes on. resolveFromKnownNodes formats
	// "%s:%d" from (IP, Port), so encode the numeric nodeId into Port
	// and set IP="bufnet".
	port, _ := strconv.ParseUint(h.nodeID, 10, 32)
	if port == 0 {
		port = 1
	}
	entry.reg.GrpcEndpoint.IP = "bufnet"
	entry.reg.GrpcEndpoint.Port = uint32(port)
	entry.reg.Endpoint.IP = "bufnet"
	entry.reg.Endpoint.Port = uint32(port)

	knownNodesMu.Lock()
	knownNodes["SceneNodeService.rpc/"+h.nodeID] = entry
	knownNodesMu.Unlock()

	updateNodeLoad(sc, entry)
	rebuildActiveZones()
}

// killNode marks a previously-registered node as gone: drop from the load
// set, remove its type mirror, evict from knownNodes, drop any cached
// gRPC conn. After this, getNodesForPurpose no longer returns it and
// IsNodeAlive reports false. Matches the end-state the real watch loop
// reaches on an etcd DELETE.
func killNode(sc *svc.ServiceContext, zoneId uint32, nodeID string) {
	loadKey := nodeLoadKey(zoneId)
	sc.Redis.Zrem(loadKey, nodeID)
	sc.Redis.Del(nodeSceneNodeTypeKey(zoneId, nodeID))

	knownNodesMu.Lock()
	for k, v := range knownNodes {
		if v.reg.ZoneId == zoneId && v.nodeID == nodeID {
			delete(knownNodes, k)
		}
	}
	knownNodesMu.Unlock()
	rebuildActiveZones()
	RemoveNodeConn(zoneId, nodeID)
}

// clearKnownNodesForTest resets the package-level registry between tests
// so state from one scenario doesn't bleed into the next. Safe to call
// even if the map is already empty.
func clearKnownNodesForTest() {
	knownNodesMu.Lock()
	knownNodes = make(map[string]nodeEntry)
	knownNodesMu.Unlock()
	rebuildActiveZones()
}

// findSceneIdHashingTo searches sceneIds 1..limit for the first one that
// assignNodeByHash lands on want when the node ring is sortedNodes. Used
// to construct "opportunistic migration" scenarios where we need the
// hash to dictate moving a channel off node A and onto node B.
func findSceneIdHashingTo(t *testing.T, sortedNodes []string, want string, limit uint64) uint64 {
	t.Helper()
	for sid := uint64(1); sid <= limit; sid++ {
		if assignNodeByHash(sid, sortedNodes) == want {
			return sid
		}
	}
	t.Fatalf("no sceneId in [1,%d] hashes to node %s given ring %v", limit, want, sortedNodes)
	return 0
}

func TestSceneNodeConnectionCacheIsScopedByZoneAndNodeID(t *testing.T) {
	ResetNodeConnCacheForTest()
	t.Cleanup(ResetNodeConnCacheForTest)
	sc, _ := newIntegrationSvcCtx(t)
	zone1Node10 := startFakeSceneNode(t, "10")
	zone2Node10 := startFakeSceneNode(t, "10")

	restoreResolver := SetNodeEndpointResolverForTest(func(zoneID uint32, nodeID string) (string, bool) {
		if nodeID != "10" {
			return "", false
		}
		switch zoneID {
		case 1:
			return "bufnet:zone-1-node-10", true
		case 2:
			return "bufnet:zone-2-node-10", true
		default:
			return "", false
		}
	})
	t.Cleanup(restoreResolver)
	restoreDialer := SetNodeDialerForTest(func(_ context.Context, endpoint string) (*grpc.ClientConn, error) {
		var lis *bufconn.Listener
		switch endpoint {
		case "bufnet:zone-1-node-10":
			lis = zone1Node10.lis
		case "bufnet:zone-2-node-10":
			lis = zone2Node10.lis
		default:
			return nil, fmt.Errorf("unknown scoped endpoint %q", endpoint)
		}
		return grpc.NewClient("passthrough://bufnet",
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
		)
	})
	t.Cleanup(restoreDialer)

	_, err := RequestNodeCreateScene(context.Background(), sc, 1, "10", 1001, 9001)
	require.NoError(t, err)
	_, err = RequestNodeCreateScene(context.Background(), sc, 2, "10", 2001, 9002)
	require.NoError(t, err)
	require.EqualValues(t, 1, zone1Node10.fake.createCalls.Load())
	require.EqualValues(t, 1, zone2Node10.fake.createCalls.Load(),
		"zone 2 的同号节点不能复用 zone 1 的连接")
}

func TestResolveKnownNodeRejectsDuplicateZoneNodeIdentity(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	first := nodeEntry{nodeID: "10"}
	first.reg.ZoneId = 1
	first.reg.GrpcEndpoint.IP = "10.0.0.1"
	first.reg.GrpcEndpoint.Port = 7001
	second := nodeEntry{nodeID: "10"}
	second.reg.ZoneId = 1
	second.reg.GrpcEndpoint.IP = "10.0.0.2"
	second.reg.GrpcEndpoint.Port = 7001
	knownNodesMu.Lock()
	knownNodes["SceneNodeService.rpc/a"] = first
	knownNodes["SceneNodeService.rpc/b"] = second
	knownNodesMu.Unlock()

	endpoint, ok, err := resolveFromKnownNodes(1, "10")
	require.Error(t, err)
	require.False(t, ok)
	require.Empty(t, endpoint)
}

func TestCachedConnectionRejectsLaterDuplicateZoneNodeIdentity(t *testing.T) {
	clearKnownNodesForTest()
	ResetNodeConnCacheForTest()
	t.Cleanup(clearKnownNodesForTest)
	t.Cleanup(ResetNodeConnCacheForTest)

	sc, _ := newIntegrationSvcCtx(t)
	h := startFakeSceneNode(t, "10")
	installBufconnDialer(t, h)
	registerWorldNodeFromHarness(t, sc, 1, h)

	_, err := RequestNodeCreateScene(context.Background(), sc, 1, "10", 1001, 9001)
	require.NoError(t, err)
	require.EqualValues(t, 1, h.fake.createCalls.Load())

	duplicate := nodeEntry{nodeID: "10"}
	duplicate.reg.ZoneId = 1
	duplicate.reg.SceneNodeType = constants.SceneNodeTypeMainWorld
	duplicate.reg.GrpcEndpoint.IP = "duplicate"
	duplicate.reg.GrpcEndpoint.Port = 7010
	knownNodesMu.Lock()
	knownNodes["SceneNodeService.rpc/duplicate"] = duplicate
	knownNodesMu.Unlock()

	_, err = RequestNodeCreateScene(context.Background(), sc, 1, "10", 1001, 9002)
	require.Error(t, err, "既有 cache 命中不得掩盖后来出现的重复身份")
	require.EqualValues(t, 1, h.fake.createCalls.Load(), "歧义身份不得收到第二次 RPC")
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestIntegration_Rebalance_NodeGone_EndToEnd exercises the urgent
// migration path all the way through gRPC:
//
//  1. Two world-hosting scene nodes (A, B) come up.
//  2. A world channel (conf=100, scene=42) is bound to A.
//  3. A disappears (process crash / pod evicted).
//  4. SceneManager runs RebalanceWorldChannelsForZone.
//
// Expected: CreateScene(conf=100, scene=42) was actually received by the
// surviving node B, Redis flips scene:42:node -> B, A's scene_count is
// decremented, B's is incremented. Because A is marked dead, DestroyScene
// is intentionally skipped (avoids a long RPC timeout during the urgent
// path), so fake A observes zero destroy calls.
func TestIntegration_Rebalance_NodeGone_EndToEnd(t *testing.T) {
	clearKnownNodesForTest()
	ResetNodeConnCacheForTest()

	const zoneId uint32 = 1
	const confId uint64 = 100
	const sceneId uint64 = 42

	sc, mr := newIntegrationSvcCtx(t)

	hA := startFakeSceneNode(t, "1001")
	hB := startFakeSceneNode(t, "1002")
	installBufconnDialer(t, hA, hB)

	registerWorldNodeFromHarness(t, sc, zoneId, hA)
	registerWorldNodeFromHarness(t, sc, zoneId, hB)

	// Seed the world channel on node A.
	mr.SAdd(worldChannelsKey(zoneId, confId), strconv.FormatUint(sceneId, 10))
	require.NoError(t, sc.Redis.Set(fmt.Sprintf("scene:%d:node", sceneId), hA.nodeID))
	require.NoError(t, sc.Redis.Set(fmt.Sprintf("scene:%d:zone", sceneId), strconv.FormatUint(uint64(zoneId), 10)))
	_, _ = sc.Redis.Incr(nodeSceneCountKey(zoneId, hA.nodeID)) // A now owns 1 scene

	// Kill A.
	killNode(sc, zoneId, hA.nodeID)

	// Act.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	RebalanceWorldChannelsForZone(ctx, sc, zoneId, []uint64{confId})

	// Assert: the scene was recreated on B.
	require.Equal(t, int64(1), hB.fake.createCalls.Load(), "B should have received exactly one CreateScene")
	require.Equal(t, confId, hB.fake.lastCreateConf.Load())
	require.Equal(t, sceneId, hB.fake.lastCreateScene.Load())

	// A is dead, so the urgent path must NOT issue DestroyScene to it
	// (it would just time out). hA.fake.destroyCalls stays 0.
	require.Equal(t, int64(0), hA.fake.destroyCalls.Load(), "dead node must not receive DestroyScene")

	// Redis state flipped to B and scene counters moved.
	got, err := sc.Redis.Get(fmt.Sprintf("scene:%d:node", sceneId))
	require.NoError(t, err)
	require.Equal(t, hB.nodeID, got)

	// A's scene_count went from 1 -> 0, B's went from 0 -> 1.
	require.EqualValues(t, 0, readInt64(sc, nodeSceneCountKey(zoneId, hA.nodeID)))
	require.EqualValues(t, 1, readInt64(sc, nodeSceneCountKey(zoneId, hB.nodeID)))
}

// TestIntegration_Rebalance_BetterHome_EndToEnd exercises the
// opportunistic path where both nodes are alive but the hash ring says
// the channel belongs somewhere else. Because the old node is alive,
// migrateWorldChannel ALSO fires DestroyScene on it (best-effort).
func TestIntegration_Rebalance_BetterHome_EndToEnd(t *testing.T) {
	clearKnownNodesForTest()
	ResetNodeConnCacheForTest()

	const zoneId uint32 = 1
	const confId uint64 = 200

	sc, mr := newIntegrationSvcCtx(t)

	hA := startFakeSceneNode(t, "2001")
	hB := startFakeSceneNode(t, "2002")
	installBufconnDialer(t, hA, hB)

	registerWorldNodeFromHarness(t, sc, zoneId, hA)
	registerWorldNodeFromHarness(t, sc, zoneId, hB)

	// PlanWorldChannelRebalance sorts nodes before hashing; use the same
	// order here so our hashing matches the planner's.
	ring := []string{hA.nodeID, hB.nodeID} // already sorted because "2001" < "2002"
	sceneId := findSceneIdHashingTo(t, ring, hB.nodeID, 200)

	// Seed: channel currently wrongly parked on A.
	mr.SAdd(worldChannelsKey(zoneId, confId), strconv.FormatUint(sceneId, 10))
	require.NoError(t, sc.Redis.Set(fmt.Sprintf("scene:%d:node", sceneId), hA.nodeID))
	require.NoError(t, sc.Redis.Set(fmt.Sprintf("scene:%d:zone", sceneId), strconv.FormatUint(uint64(zoneId), 10)))
	_, _ = sc.Redis.Incr(nodeSceneCountKey(zoneId, hA.nodeID))
	// Seed the reverse index the way a real allocateScene would. The
	// migration must move the sceneId from hA's set to hB's set; otherwise
	// node-death reconciliation later on will either miss the scene (if
	// hB dies) or force-destroy a still-live scene (if hA dies).
	_, _ = sc.Redis.Sadd(nodeScenesKey(zoneId, hA.nodeID), strconv.FormatUint(sceneId, 10))
	// No players on this channel, so it's eligible for opportunistic move.

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	RebalanceWorldChannelsForZone(ctx, sc, zoneId, []uint64{confId})

	require.Equal(t, int64(1), hB.fake.createCalls.Load(), "B should have received CreateScene")
	require.Equal(t, int64(1), hA.fake.destroyCalls.Load(), "live A should have received best-effort DestroyScene")
	require.Equal(t, sceneId, hA.fake.lastDestroyScene.Load())

	got, err := sc.Redis.Get(fmt.Sprintf("scene:%d:node", sceneId))
	require.NoError(t, err)
	require.Equal(t, hB.nodeID, got)

	// Reverse index must have been rewritten in lockstep with scene:{id}:node.
	oldMembers, _ := sc.Redis.Smembers(nodeScenesKey(zoneId, hA.nodeID))
	newMembers, _ := sc.Redis.Smembers(nodeScenesKey(zoneId, hB.nodeID))
	require.NotContains(t, oldMembers, strconv.FormatUint(sceneId, 10),
		"migrated sceneId must be removed from old node's :scenes set")
	require.Contains(t, newMembers, strconv.FormatUint(sceneId, 10),
		"migrated sceneId must be added to new node's :scenes set")
}

// TestIntegration_Rebalance_CreateSceneFailure_KeepsOldMapping proves the
// safety invariant: if the new node rejects CreateScene, the Redis scene
// -> node mapping stays on the old node, so no player gets routed to a
// scene that doesn't exist. The next rebalance tick will try again.
func TestIntegration_Rebalance_CreateSceneFailure_KeepsOldMapping(t *testing.T) {
	clearKnownNodesForTest()
	ResetNodeConnCacheForTest()

	const zoneId uint32 = 1
	const confId uint64 = 300
	const sceneId uint64 = 777

	sc, mr := newIntegrationSvcCtx(t)

	hA := startFakeSceneNode(t, "3001")
	hB := startFakeSceneNode(t, "3002")
	hB.fake.createShouldFail = fmt.Errorf("simulated: scene_manager rejecting CreateScene")
	installBufconnDialer(t, hA, hB)

	registerWorldNodeFromHarness(t, sc, zoneId, hA)
	registerWorldNodeFromHarness(t, sc, zoneId, hB)

	mr.SAdd(worldChannelsKey(zoneId, confId), strconv.FormatUint(sceneId, 10))
	require.NoError(t, sc.Redis.Set(fmt.Sprintf("scene:%d:node", sceneId), hA.nodeID))
	_, _ = sc.Redis.Incr(nodeSceneCountKey(zoneId, hA.nodeID))

	killNode(sc, zoneId, hA.nodeID) // urgent scenario: A is dead, only B is available

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	RebalanceWorldChannelsForZone(ctx, sc, zoneId, []uint64{confId})

	require.Equal(t, int64(1), hB.fake.createCalls.Load(), "CreateScene attempted on B")
	require.Equal(t, int64(0), hB.fake.destroyCalls.Load())

	// CRITICAL: mapping must NOT have been flipped, because the channel
	// doesn't actually live on B yet.
	got, err := sc.Redis.Get(fmt.Sprintf("scene:%d:node", sceneId))
	require.NoError(t, err)
	require.Equal(t, hA.nodeID, got, "scene mapping must stay on old node when CreateScene fails")

	// Scene counts unchanged from pre-migration: A still shows 1 (we never
	// decremented because the move didn't complete); B still shows 0.
	require.EqualValues(t, 1, readInt64(sc, nodeSceneCountKey(zoneId, hA.nodeID)))
	require.EqualValues(t, 0, readInt64(sc, nodeSceneCountKey(zoneId, hB.nodeID)))
}

// TestIntegration_Rebalance_ActivePlayersBlockMigration guards the hottest
// safety rule: a world channel with online players is never moved, even
// when the hash ring says it belongs elsewhere. Live cross-node migration
// would need state transfer, which this codebase does not implement.
func TestIntegration_Rebalance_ActivePlayersBlockMigration(t *testing.T) {
	clearKnownNodesForTest()
	ResetNodeConnCacheForTest()

	const zoneId uint32 = 1
	const confId uint64 = 400

	sc, mr := newIntegrationSvcCtx(t)

	hA := startFakeSceneNode(t, "4001")
	hB := startFakeSceneNode(t, "4002")
	installBufconnDialer(t, hA, hB)

	registerWorldNodeFromHarness(t, sc, zoneId, hA)
	registerWorldNodeFromHarness(t, sc, zoneId, hB)

	ring := []string{hA.nodeID, hB.nodeID}
	sceneId := findSceneIdHashingTo(t, ring, hB.nodeID, 200)

	mr.SAdd(worldChannelsKey(zoneId, confId), strconv.FormatUint(sceneId, 10))
	require.NoError(t, sc.Redis.Set(fmt.Sprintf("scene:%d:node", sceneId), hA.nodeID))
	// 5 players online -> PlanWorldChannelRebalance must skip this channel.
	require.NoError(t, sc.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, sceneId), "5"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	RebalanceWorldChannelsForZone(ctx, sc, zoneId, []uint64{confId})

	require.Equal(t, int64(0), hA.fake.createCalls.Load())
	require.Equal(t, int64(0), hA.fake.destroyCalls.Load())
	require.Equal(t, int64(0), hB.fake.createCalls.Load())
	require.Equal(t, int64(0), hB.fake.destroyCalls.Load())

	got, err := sc.Redis.Get(fmt.Sprintf("scene:%d:node", sceneId))
	require.NoError(t, err)
	require.Equal(t, hA.nodeID, got)
}

// TestIntegration_WatchEvent_PutTriggersRebalance drives the etcd-watch
// codepath itself: we synthesise a clientv3 PUT event for node B so
// handleWatchEvent runs end-to-end, and assert that the event causes the
// orphan channel on the dead node to migrate. This covers the glue
// between LoadReporter and the rebalance pipeline, which PlanRebalance
// unit tests skip.
//
// NOTE: This test doesn't use a real etcd client. Since
// handleWatchEvent only consumes the *clientv3.Event fields (Type, Kv,
// Kv.Key, Kv.Value) we can synthesise them directly. The rebalance
// trigger inside handleWatchEvent is gated by worldConfIds() returning
// a non-empty list, which reads from the global World config table —
// that's populated (or not) by upstream config loading, not by the
// handler. We therefore drive RebalanceWorldChannelsForZone directly
// rather than asserting through the handler in this first integration
// pass; adding a WorldConfIds injector is a follow-up if/when the gating
// logic itself needs coverage.

// TestIntegration_Rebalance_CascadesDependentMirrors covers the
// mirror-co-location guardrail: when the rebalancer migrates a world
// channel that has co-located mirrors on the OLD node, those mirrors
// must be force-destroyed. Otherwise the optimization silently rots —
// mirrors stay on the old node, can't reach the source's resident
// world data anymore, and pin the old node's memory until idle timeout.
//
// End-to-end assertions:
//
//  1. CreateScene fires on the new node for the migrated channel.
//  2. DestroyScene fires on the old node for BOTH the migrated channel
//     AND every co-located mirror (best-effort cleanup).
//  3. scene:{mirrorId}:node mappings are wiped from Redis.
//  4. scene:{source}:mirrors set is drained.
func TestIntegration_Rebalance_CascadesDependentMirrors(t *testing.T) {
	clearKnownNodesForTest()
	ResetNodeConnCacheForTest()

	const zoneId uint32 = 1
	const confId uint64 = 500

	sc, mr := newIntegrationSvcCtx(t)

	hA := startFakeSceneNode(t, "5001")
	hB := startFakeSceneNode(t, "5002")
	installBufconnDialer(t, hA, hB)

	registerWorldNodeFromHarness(t, sc, zoneId, hA)
	registerWorldNodeFromHarness(t, sc, zoneId, hB)

	// Pick a sceneId that the planner will want to migrate from A to B
	// (the hash ring is sorted lexicographically: "5001" < "5002").
	ring := []string{hA.nodeID, hB.nodeID}
	sourceSceneId := findSceneIdHashingTo(t, ring, hB.nodeID, 200)

	// Seed: source channel currently parked on A.
	mr.SAdd(worldChannelsKey(zoneId, confId), strconv.FormatUint(sourceSceneId, 10))
	require.NoError(t, sc.Redis.Set(fmt.Sprintf("scene:%d:node", sourceSceneId), hA.nodeID))
	require.NoError(t, sc.Redis.Set(fmt.Sprintf("scene:%d:zone", sourceSceneId), strconv.FormatUint(uint64(zoneId), 10)))
	_, _ = sc.Redis.Incr(nodeSceneCountKey(zoneId, hA.nodeID))
	_, _ = sc.Redis.Sadd(nodeScenesKey(zoneId, hA.nodeID), strconv.FormatUint(sourceSceneId, 10))

	// Spawn three mirrors co-located on hA. Routing through the real
	// CreateSceneLogic exercises pickInstanceNode -> resolveMirrorSourceNode
	// so the test catches regressions in the wiring, not just in
	// migrateWorldChannel itself.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mirrorIds []uint64
	for i := 0; i < 3; i++ {
		resp, err := NewCreateSceneLogic(ctx, sc).CreateScene(&scenenodepb.CreateSceneRequest{
			SceneConfId:    2001,
			ZoneId:         zoneId,
			SceneType:      scenenodepb.SceneType_SCENE_TYPE_INSTANCE,
			SourceSceneId:  sourceSceneId,
			MirrorConfigId: 9050,
		})
		require.NoError(t, err)
		require.Zero(t, resp.ErrorCode, "mirror %d setup must succeed", i)
		require.Equal(t, hA.nodeID, resp.NodeId, "mirror %d must co-locate on hA", i)
		mirrorIds = append(mirrorIds, resp.SceneId)
	}
	require.Equal(t, int64(3), hA.fake.createCalls.Load(), "hA receives 3 mirror creates during setup")

	preCascade, _ := sc.Redis.Smembers(sceneMirrorsKey(sourceSceneId))
	require.Len(t, preCascade, 3, "setup: source's mirrors set must be populated")

	// Reset the per-fake counters so post-rebalance assertions only see
	// rebalance traffic, not setup traffic.
	hA.fake.createCalls.Store(0)
	hA.fake.destroyCalls.Store(0)

	// Trigger the migration.
	RebalanceWorldChannelsForZone(ctx, sc, zoneId, []uint64{confId})

	// (1) Source recreated on hB.
	require.Equal(t, int64(1), hB.fake.createCalls.Load(), "source channel recreated on hB")
	require.Equal(t, sourceSceneId, hB.fake.lastCreateScene.Load())

	// (2) hA receives DestroyScene for the source AND each mirror (4 total).
	require.Equal(t, int64(4), hA.fake.destroyCalls.Load(),
		"hA must receive DestroyScene for the migrated source plus all 3 cascaded mirrors")

	// (3) Every mirror is wiped from Redis.
	for _, mid := range mirrorIds {
		nid, _ := sc.Redis.Get(fmt.Sprintf("scene:%d:node", mid))
		require.Equal(t, "", nid, "mirror %d must be force-destroyed after source migrated", mid)
	}

	// (4) Mirrors index is drained so the next mirror request for this
	// source starts clean (and would now correctly land on hB).
	postCascade, _ := sc.Redis.Smembers(sceneMirrorsKey(sourceSceneId))
	require.Empty(t, postCascade, "scene:{source}:mirrors must be drained after cascade")

	// Source's scene mapping flipped to hB.
	got, err := sc.Redis.Get(fmt.Sprintf("scene:%d:node", sourceSceneId))
	require.NoError(t, err)
	require.Equal(t, hB.nodeID, got)
}
