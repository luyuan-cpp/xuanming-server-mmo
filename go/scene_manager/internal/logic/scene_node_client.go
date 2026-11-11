package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	scenepb "proto/scene"
	scenenodepb "proto/scene_manager"

	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/safego"
	"shared/tracing"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// nodeConnCache caches gRPC connections to C++ scene nodes, keyed by
// (zoneId,nodeId). Node IDs are allocated independently in every zone.
// Connections are reused across calls and cleaned up on node removal.
var (
	nodeConnMu    sync.RWMutex
	nodeConnCache = make(map[string]*grpc.ClientConn)
)

// nodeDialer builds a gRPC ClientConn for the given endpoint. Swappable so
// integration tests can point the whole RPC pipeline at a bufconn-backed
// fake SceneNode server without standing up TCP listeners. Production
// callers never touch this; the default is grpc.NewClient over insecure
// transport (scene manager ↔ scene node is on an internal overlay).
var nodeDialer = defaultNodeDialer

func defaultNodeDialer(_ context.Context, endpoint string) (*grpc.ClientConn, error) {
	// 裸客户端不经 zrpc,traceparent 由 shared/tracing 的拦截器写(docs/design/distributed-tracing.md §6.2)。
	return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(tracing.UnaryClientInterceptor()))
}

// SetNodeDialerForTest installs a custom dialer and returns a restore func.
// MUST be called from *_test.go via t.Cleanup so a failing test doesn't
// leak a bufconn dialer into sibling packages.
func SetNodeDialerForTest(d func(context.Context, string) (*grpc.ClientConn, error)) (restore func()) {
	prev := nodeDialer
	nodeDialer = d
	return func() { nodeDialer = prev }
}

// nodeEndpointOverride, when non-nil, replaces the etcd/knownNodes endpoint
// lookup. Same rationale as nodeDialer: tests need the whole RPC pipeline to
// reach an in-process fake without registering a knownNodes entry for every
// synthetic node id (several tests use non-numeric ids like "hot"/"A").
// Production never sets this.
var nodeEndpointOverride func(zoneId uint32, nodeId string) (string, bool)

// SetNodeEndpointResolverForTest installs an endpoint resolver override and
// returns a restore func. MUST be paired with t.Cleanup.
func SetNodeEndpointResolverForTest(r func(zoneId uint32, nodeId string) (string, bool)) (restore func()) {
	prev := nodeEndpointOverride
	nodeEndpointOverride = r
	return func() { nodeEndpointOverride = prev }
}

// ResetNodeConnCacheForTest evicts every cached gRPC ClientConn. Integration
// tests reuse nodeIDs across sub-tests (node-A in test 1 is an unrelated
// bufconn in test 2); without eviction the second test would dial the
// first test's already-closed listener and hang on RPC.
func ResetNodeConnCacheForTest() {
	nodeConnMu.Lock()
	defer nodeConnMu.Unlock()
	for id, c := range nodeConnCache {
		_ = c.Close()
		delete(nodeConnCache, id)
	}
}

// RequestNodeCreateSceneWithOptions dials the C++ scene node identified by nodeId and calls
// CreateScene to instantiate the ECS scene entity, forwarding mirror_config_id and creator_ids.
// sceneId is the Go-allocated unique ID; C++ uses it for per-scene idempotency.
// 副本与镜像用:不带放置代次,它们不换节点。世界频道一律走 RequestNodeCreateSceneAtGeneration。
func RequestNodeCreateSceneWithOptions(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	zoneId uint32,
	nodeId string,
	configId uint32,
	sceneId uint64,
	mirrorConfigId uint32,
	creatorIds []uint64,
) (*scenepb.CreateSceneResponse, error) {
	return createSceneOnNode(ctx, svcCtx, zoneId, nodeId, &scenepb.CreateSceneRequest{
		ConfigId:       configId,
		SceneId:        sceneId,
		MirrorConfigId: mirrorConfigId,
		CreatorIds:     creatorIds,
	})
}

// RequestNodeCreateSceneAtGeneration 是世界频道用的建场景,带放置代次(docs/design/world-channel-placement-generation.md):
// 换节点时是这次尝试分到的代次,init 向属主补建时是频道当前的代次。节点已按 >= 这个代次拆过这个场景时回空应答,
// 这里按建场景失败返回。
func RequestNodeCreateSceneAtGeneration(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, nodeId string,
	configId uint32, sceneId, placementGeneration uint64) (*scenepb.CreateSceneResponse, error) {
	return createSceneOnNode(ctx, svcCtx, zoneId, nodeId, &scenepb.CreateSceneRequest{
		ConfigId:            configId,
		SceneId:             sceneId,
		PlacementGeneration: placementGeneration,
	})
}

func createSceneOnNode(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, nodeId string,
	req *scenepb.CreateSceneRequest) (*scenepb.CreateSceneResponse, error) {
	if req.SceneId == 0 {
		return nil, fmt.Errorf("CreateScene refused: scene id must be nonzero")
	}
	conn, err := getOrDialNode(ctx, svcCtx, zoneId, nodeId)
	if err != nil {
		return nil, fmt.Errorf("dial scene node %s: %w", nodeId, err)
	}

	client := scenenodepb.NewSceneNodeGrpcClient(conn)
	resp, err := client.CreateScene(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("CreateScene RPC to node %s: %w", nodeId, err)
	}
	// RPC 的 OK 只代表传输完成;C++ 创建入口仍可能因参数、分配许可或放置代次过期拒绝而留下空应答。
	// 在发布/切换场景归属之前确认实际创建的是本次请求的实例,失败沿调用方原有补偿收尾。
	if resp == nil || resp.SceneInfo == nil || resp.SceneInfo.SceneId != req.SceneId {
		return nil, fmt.Errorf("CreateScene node %s did not confirm requested scene %d at placement generation %d (received %d)",
			nodeId, req.SceneId, req.PlacementGeneration, resp.GetSceneInfo().GetSceneId())
	}

	return resp, nil
}

// RequestNodeDestroyScene dials the C++ scene node and calls DestroyScene
// to remove the ECS scene entity.
// 不带放置代次:副本、镜像的销毁与世界频道的退役拆除(排空、孤儿清理),节点无条件执行。
func RequestNodeDestroyScene(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, nodeId string, sceneId uint64) error {
	return destroySceneOnNode(ctx, svcCtx, zoneId, nodeId, &scenepb.DestroySceneRequest{SceneId: sceneId})
}

// RequestNodeDestroySceneAtGeneration 拆掉世界频道在 nodeId 上按不新于 placementGeneration 的代次建出的实体:回收一次落空
// 改派预建的实体,或 rebalance 迁走之后拆老节点。节点已按更新的代次建过这个场景(之后有过一次合法的放置)时忽略这次拆除、
// 照常回 OK,所以 nil 不代表实体一定被拆了。
func RequestNodeDestroySceneAtGeneration(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, nodeId string,
	sceneId, placementGeneration uint64) error {
	return destroySceneOnNode(ctx, svcCtx, zoneId, nodeId, &scenepb.DestroySceneRequest{
		SceneId:             sceneId,
		PlacementGeneration: placementGeneration,
	})
}

func destroySceneOnNode(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, nodeId string,
	req *scenepb.DestroySceneRequest) error {
	conn, err := getOrDialNode(ctx, svcCtx, zoneId, nodeId)
	if err != nil {
		return fmt.Errorf("dial scene node %s: %w", nodeId, err)
	}

	client := scenenodepb.NewSceneNodeGrpcClient(conn)
	if _, err := client.DestroyScene(ctx, req); err != nil {
		return fmt.Errorf("DestroyScene RPC to node %s: %w", nodeId, err)
	}

	return nil
}

// RequestNodeReleasePlayer dials the C++ scene node identified by nodeId
// (the player's previous scene node) and asks it to release the player so
// the new node can load fresh state from Redis. Used during cross-node
// scene switches; same-node switches do not need this.
//
// expectedOwnerEpoch 是 SM 本次 EnterScene 观察到、并用于落点 CAS 的 owner_epoch(scene-enter-chain-residuals.md
// §4.2 ②):scene 只在本地缓存恰好等于它时才执行退出,0 = 按旧行为释放。
func RequestNodeReleasePlayer(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, nodeId string, playerId uint64,
	targetSceneId uint64, targetNodeId string, expectedOwnerEpoch uint64) error {
	return callReleasePlayer(ctx, svcCtx, zoneId, nodeId, &scenenodepb.ReleasePlayerRequest{
		PlayerId:           playerId,
		TargetSceneId:      targetSceneId,
		TargetNodeId:       targetNodeId,
		ExpectedOwnerEpoch: expectedOwnerEpoch,
	})
}

// RequestNodeReleaseEndedSession 请 location 的持有节点释放一名会话已结束的玩家(ReleasePlayerRequest.session_ended,
// docs/design/location-release-binding.md §3.3):持有且缓存 owner_epoch == expectedOwnerEpoch → 按客户端断线退出、存盘后
// 写 A1′ 标记;完全不持有 → 按 owner_epoch 条件补写凭证标记;其余忽略。没有接手节点,target 两项留空。
// covered 是这次释放覆盖到的最新登录身份(§13):持有节点上的实体已绑上比它新的登录时不退出,迟到的释放因此退不掉新会话。
// 零值 = 不带身份,scene 按只比 owner_epoch 的旧行为处理。
func RequestNodeReleaseEndedSession(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, nodeId string,
	playerId uint64, expectedOwnerEpoch uint64, covered placementIdentity) error {
	return callReleasePlayer(ctx, svcCtx, zoneId, nodeId, &scenenodepb.ReleasePlayerRequest{
		PlayerId:               playerId,
		ExpectedOwnerEpoch:     expectedOwnerEpoch,
		SessionEnded:           true,
		CoveredSessionId:       covered.sessionID,
		CoveredLoginGeneration: covered.loginGeneration,
	})
}

func callReleasePlayer(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, nodeId string,
	req *scenenodepb.ReleasePlayerRequest) error {
	conn, err := getOrDialNode(ctx, svcCtx, zoneId, nodeId)
	if err != nil {
		return fmt.Errorf("dial scene node %s: %w", nodeId, err)
	}

	client := scenenodepb.NewSceneNodeGrpcClient(conn)
	if _, err := client.ReleasePlayer(ctx, req); err != nil {
		return fmt.Errorf("ReleasePlayer RPC to node %s (expected_owner_epoch=%d session_ended=%v covered=%d/%d): %w",
			nodeId, req.GetExpectedOwnerEpoch(), req.GetSessionEnded(), req.GetCoveredSessionId(),
			req.GetCoveredLoginGeneration(), err)
	}

	return nil
}

// getOrDialNode returns a cached connection or discovers the node endpoint
// from etcd and dials it.
func nodeIdentity(zoneId uint32, nodeId string) string {
	return fmt.Sprintf("%d/%s", zoneId, nodeId)
}

func getOrDialNode(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, nodeId string) (*grpc.ClientConn, error) {
	identity := nodeIdentity(zoneId, nodeId)
	// 不能让既有 cache 命中掩盖后来出现的重复注册。Create/Destroy/Release
	// 发到任意一个同号进程都会破坏 scene ownership，必须先拒绝身份歧义。
	if matches := knownNodeIdentityMatchCount(zoneId, nodeId); matches > 1 {
		return nil, fmt.Errorf("ambiguous scene node identity zone=%d node=%s has %d registrations", zoneId, nodeId, matches)
	}
	nodeConnMu.RLock()
	conn, ok := nodeConnCache[identity]
	nodeConnMu.RUnlock()
	if ok {
		return conn, nil
	}

	// Discover endpoint from etcd.
	endpoint, err := resolveNodeEndpoint(ctx, svcCtx, zoneId, nodeId)
	if err != nil {
		return nil, err
	}

	conn, err = nodeDialer(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", endpoint, err)
	}

	nodeConnMu.Lock()
	// Double-check: another goroutine may have added it.
	if existing, ok := nodeConnCache[identity]; ok {
		nodeConnMu.Unlock()
		conn.Close()
		return existing, nil
	}
	nodeConnCache[identity] = conn
	nodeConnMu.Unlock()

	logx.Infof("[SceneNodeClient] Connected to scene node %s at %s", nodeId, endpoint)
	return conn, nil
}

// resolveNodeEndpoint returns the "ip:port" gRPC address for a scene node.
// It first checks the in-memory knownNodes map (maintained by the watch loop),
// then falls back to a fresh etcd query.
func resolveNodeEndpoint(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, nodeId string) (string, error) {
	if nodeEndpointOverride != nil {
		if ep, ok := nodeEndpointOverride(zoneId, nodeId); ok {
			return ep, nil
		}
	}

	// Fast path: use the watch-maintained in-memory registry.
	if ep, ok, knownErr := resolveFromKnownNodes(zoneId, nodeId); knownErr != nil {
		return "", knownErr
	} else if ok {
		return ep, nil
	}

	// Slow path: query etcd directly (covers race at startup before first fullSync).
	// 只读那个 zone 的前缀(docs/design/control-plane-zone-scoping-20261105.md §3.4):原来拉全服 SceneNodeService.rpc/,
	// 读量随全服场景节点数增长。
	if svcCtx.Etcd == nil {
		return "", fmt.Errorf("scene node %s not in registry and etcd client unavailable", nodeId)
	}
	prefix := sceneNodeZonePrefix(zoneId)
	resp, err := svcCtx.Etcd.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return "", fmt.Errorf("etcd get %s: %w", prefix, err)
	}

	var matchedEndpoint string
	matched := 0
	for _, kv := range resp.Kvs {
		var reg sceneNodeRegistration
		if err := json.Unmarshal(kv.Value, &reg); err != nil {
			continue
		}
		if reg.ZoneId == zoneId && fmt.Sprintf("%d", reg.NodeId) == nodeId {
			matched++
			if reg.GrpcEndpoint.Port > 0 {
				endpoint := fmt.Sprintf("%s:%d", reg.GrpcEndpoint.IP, reg.GrpcEndpoint.Port)
				if matchedEndpoint != "" && matchedEndpoint != endpoint {
					return "", fmt.Errorf("ambiguous scene node identity zone=%d node=%s has multiple grpc endpoints", zoneId, nodeId)
				}
				matchedEndpoint = endpoint
				continue
			}
			return "", fmt.Errorf("scene node %s has no grpc_endpoint (would fall back to raw TCP port, causing protocol mismatch)", nodeId)
		}
	}
	if matched > 1 {
		return "", fmt.Errorf("ambiguous scene node identity zone=%d node=%s has %d registrations", zoneId, nodeId, matched)
	}
	if matchedEndpoint != "" {
		return matchedEndpoint, nil
	}

	return "", fmt.Errorf("scene node %s not found in etcd", nodeId)
}

// resolveFromKnownNodes checks the in-memory node registry for the endpoint.
func resolveFromKnownNodes(zoneId uint32, nodeId string) (string, bool, error) {
	knownNodesMu.RLock()
	defer knownNodesMu.RUnlock()
	var endpoint string
	matches := 0
	for _, entry := range knownNodes {
		if entry.reg.ZoneId == zoneId && entry.nodeID == nodeId {
			matches++
			if entry.reg.GrpcEndpoint.Port > 0 {
				candidate := fmt.Sprintf("%s:%d", entry.reg.GrpcEndpoint.IP, entry.reg.GrpcEndpoint.Port)
				if endpoint != "" && endpoint != candidate {
					return "", false, fmt.Errorf("ambiguous scene node identity zone=%d node=%s has conflicting endpoints", zoneId, nodeId)
				}
				endpoint = candidate
				continue
			}
			// Do NOT fall back to entry.reg.Endpoint -- that is the raw TCP protobuf
			// port. Dialing it with gRPC sends an HTTP/2 preface which the protobuf
			// codec rejects as InvalidLength.
			return "", false, fmt.Errorf("scene node zone=%d node=%s has no grpc endpoint", zoneId, nodeId)
		}
	}
	if matches > 1 {
		return "", false, fmt.Errorf("ambiguous scene node identity zone=%d node=%s has %d registrations", zoneId, nodeId, matches)
	}
	return endpoint, matches == 1, nil
}

// RemoveNodeConn closes and removes the cached connection for a node.
// Called when a node is removed from etcd.
func RemoveNodeConn(zoneId uint32, nodeId string) {
	identity := nodeIdentity(zoneId, nodeId)
	nodeConnMu.Lock()
	defer nodeConnMu.Unlock()
	if conn, ok := nodeConnCache[identity]; ok {
		conn.Close()
		delete(nodeConnCache, identity)
		logx.Infof("[SceneNodeClient] Removed connection to scene node %s", nodeId)
	}
}

// dispatchReleasePlayer fires a ReleasePlayer notification to the player's
// previous scene node fully asynchronously.
//
// History: prior to Round 14 this did a 500ms synchronous attempt on the hot
// path so EnterScene could "feel" the old node release before returning. Under
// 45k-bot load that 500ms deadline turned into a hard cliff — every reconnect
// hit it (scene node gRPC server was contended, NUM_POLLERS=2), pinning
// scene_manager.EnterScene latency at ~535ms and starving NotifyEnterScene
// (71k robot-side scene-ready timeouts).
//
// New behaviour (Round 14):
//  1. Caller returns immediately; the entire attempt + retry chain runs in a
//     detached goroutine with context.Background() so request cancellation
//     never aborts release.
//  2. First attempt has a 1s deadline (was 500ms, but now off the hot path);
//     followed by up to 2 retries with 1s/3s backoff and 1s deadline each.
//  3. Every terminal outcome (ok / retry_ok / timeout / error) is reported to
//     Prometheus via metrics.ObserveReleasePlayer.
//
// Failures are non-fatal — the C++ scene node's AFK sweeper is the final
// safety net for any player who is never released.
//
// 重试按代际幂等(scene-enter-chain-residuals.md §4.2):首发与每次重试都带同一个 expectedOwnerEpoch,
// scene 侧按「本地缓存 == 期望值才退出」裁决,所以迟到的重试不会退掉已采纳回滚(E+2)或回流后重新载入的实体;
// 被忽略的释放也回 Empty,重试不会改变结论。调用方(EnterScene 第 7b 步)只在本请求提交之后才派发。
func dispatchReleasePlayer(svcCtx *svc.ServiceContext, log logx.Logger, oldZoneID uint32,
	oldNodeId string, playerId uint64, targetSceneId uint64, targetNodeId string, expectedOwnerEpoch uint64) {
	// safego.Go 而不是裸 go func:这条链路挂在 EnterScene 热路径上,45k 并发时
	// 每次跨节点交接都会派生一条。裸写法里任何一次 panic(比如 nil svcCtx.Kafka、
	// 节点连接缓存被并发改坏)都会当场打死整个进程,而日志里只剩一段 runtime 栈,
	// 看不出是哪条后台链路炸的。
	// 退避表在派发时取一次快照,单测替换 releasePlayerRetryBackoffs 不会与在途协程竞争。
	backoffs := releasePlayerRetryBackoffs
	safego.Go(SafePointReleasePlayer, func() {
		attemptCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		err := RequestNodeReleasePlayer(attemptCtx, svcCtx, oldZoneID, oldNodeId, playerId, targetSceneId, targetNodeId, expectedOwnerEpoch)
		cancel()

		if err == nil {
			metrics.ObserveReleasePlayer(oldZoneID, "ok")
			return
		}

		logx.Errorf("ReleasePlayer first attempt failed for player %d on old node %s expected_owner_epoch=%d (will retry in background): %v",
			playerId, oldNodeId, expectedOwnerEpoch, err)

		var lastErr = err
		for i, backoff := range backoffs {
			time.Sleep(backoff)
			retryCtx, retryCancel := context.WithTimeout(context.Background(), 1*time.Second)
			lastErr = RequestNodeReleasePlayer(retryCtx, svcCtx, oldZoneID, oldNodeId, playerId, targetSceneId, targetNodeId, expectedOwnerEpoch)
			retryCancel()
			if lastErr == nil {
				logx.Infof("[ReleasePlayer] Retry %d succeeded for player %d on old node %s expected_owner_epoch=%d",
					i+1, playerId, oldNodeId, expectedOwnerEpoch)
				metrics.ObserveReleasePlayer(oldZoneID, "retry_ok")
				return
			}
			logx.Errorf("[ReleasePlayer] Retry %d failed for player %d on old node %s expected_owner_epoch=%d: %v",
				i+1, playerId, oldNodeId, expectedOwnerEpoch, lastErr)
		}
		outcome := "error"
		if errors.Is(lastErr, context.DeadlineExceeded) {
			outcome = "timeout"
		}
		metrics.ObserveReleasePlayer(oldZoneID, outcome)
		logx.Errorf("[ReleasePlayer] All retries exhausted for player %d on old node %s expected_owner_epoch=%d; relying on AFK cleanup. last_err=%v",
			playerId, oldNodeId, expectedOwnerEpoch, lastErr)
	})
	_ = log // logger no longer used on hot path — kept in signature for ABI stability
}

// releasePlayerRetryBackoffs 是 dispatchReleasePlayer 首发失败后的重试间隔(最坏约 7s 送达)。
// 包级变量只为单测缩短等待;生产从不改它。
var releasePlayerRetryBackoffs = []time.Duration{1 * time.Second, 3 * time.Second}

// releaseDispatcher 是 EnterScene 第 7b 步派发 ReleasePlayer 的接缝,默认就是 dispatchReleasePlayer。
// 包级钩子只为单测能**同步**断言「派发了几次、带什么期望值、是否在路由之后」(不靠 sleep),做法同 nodeDialer;
// 生产从不改它。替换的用例必须用 t.Cleanup 还原。
var releaseDispatcher = dispatchReleasePlayer

// dispatchSessionEndedRelease 在后台协程里发一次 session_ended 释放(docs/design/location-release-binding.md §3.2)。
// 单发、1s 截止、不在内部重试:调用方 LeaveScene 此时已回可重试错误,player_locator 的下一次重试会再走到这里再派发一次,
// 节拍由那条 at-least-once 通道决定,这里不另起退避。结果只进日志:释放是否生效由下一次 LeaveScene 看标记判定,
// 不看这次 RPC 的成败。covered = 被清理的会话身份(§13):这条后台请求可能晚于同一玩家的新登录到达持有节点,
// 实体已绑上更新的登录时由持有节点忽略。
func dispatchSessionEndedRelease(svcCtx *svc.ServiceContext, zoneID uint32, nodeID string, playerID uint64,
	expectedOwnerEpoch uint64, covered placementIdentity) {
	safego.Go(SafePointReleasePlayer, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		if err := RequestNodeReleaseEndedSession(ctx, svcCtx, zoneID, nodeID, playerID, expectedOwnerEpoch, covered); err != nil {
			logx.Errorf("[LeaveRelease] session-ended ReleasePlayer failed (next LeaveScene retry will resend): player=%d zone=%d node=%s expected_owner_epoch=%d covered_session=%d covered_generation=%d err=%v",
				playerID, zoneID, nodeID, expectedOwnerEpoch, covered.sessionID, covered.loginGeneration, err)
			return
		}
		logx.Infof("[LeaveRelease] session-ended ReleasePlayer delivered: player=%d zone=%d node=%s expected_owner_epoch=%d covered_session=%d covered_generation=%d",
			playerID, zoneID, nodeID, expectedOwnerEpoch, covered.sessionID, covered.loginGeneration)
	})
}

// sessionEndedReleaseDispatcher 是 LeaveScene 派发 session_ended 释放的接缝,默认 dispatchSessionEndedRelease。
// 包级钩子只为单测同步断言「派发了没有、发给谁、带什么期望值」,做法同 releaseDispatcher;生产从不改它。
var sessionEndedReleaseDispatcher = dispatchSessionEndedRelease
