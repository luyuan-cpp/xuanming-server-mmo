package logic

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"scene_manager/internal/constants"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	NodeLoadKeyFmt     = "scene_nodes:zone:%d:load"
	NodeSceneCountKey  = "node:zone:%d:%s:scene_count"
	NodePlayerCountKey = "node:zone:%d:%s:player_count"
	// NodeSceneNodeTypeKey mirrors the scene_node_type declared by the C++
	// node (0=MainWorld, 1=Instance, 2=MainWorldCross, 3=InstanceCross). The
	// load reporter writes this when a node registers so selection logic can
	// filter by purpose without hitting etcd.
	NodeSceneNodeTypeKey = "node:zone:%d:%s:scene_node_type"
	LoadReportInterval   = 5 * time.Second
	sceneNodePrefix      = "SceneNodeService.rpc/"
)

// activeZones holds the zone IDs derived from currently known nodes.
var (
	activeZonesMu sync.RWMutex
	activeZones   []uint32
)

// GetActiveZones returns the zone IDs from the current known node set.
func GetActiveZones() []uint32 {
	activeZonesMu.RLock()
	defer activeZonesMu.RUnlock()
	result := make([]uint32, len(activeZones))
	copy(result, activeZones)
	return result
}

// nodeLoadKey returns the zone-scoped Redis sorted-set key.
func nodeLoadKey(zoneID uint32) string {
	return fmt.Sprintf(NodeLoadKeyFmt, zoneID)
}

func nodeSceneCountKey(zoneID uint32, nodeID string) string {
	return fmt.Sprintf(NodeSceneCountKey, zoneID, nodeID)
}

func nodePlayerCountKey(zoneID uint32, nodeID string) string {
	return fmt.Sprintf(NodePlayerCountKey, zoneID, nodeID)
}

func nodeSceneNodeTypeKey(zoneID uint32, nodeID string) string {
	return fmt.Sprintf(NodeSceneNodeTypeKey, zoneID, nodeID)
}

// sceneNodeRegistration mirrors the JSON that C++ scene nodes write to etcd.
// The field names match protobuf's MessageToJsonString camelCase encoding of
// proto/common/base/common.proto NodeInfo. `sceneNodeType` is the node's
// declared role (see constants.SceneNodeType*); `nodeType` is the coarse
// service class (SceneNodeService = 0x14).
type sceneNodeRegistration struct {
	NodeId        uint32 `json:"nodeId"`
	NodeType      uint32 `json:"nodeType"`
	SceneNodeType uint32 `json:"sceneNodeType"`
	Endpoint      struct {
		IP   string `json:"ip"`
		Port uint32 `json:"port"`
	} `json:"endpoint"`
	GrpcEndpoint struct {
		IP   string `json:"ip"`
		Port uint32 `json:"port"`
	} `json:"grpcEndpoint"`
	ZoneId uint32 `json:"zoneId"`
}

// nodeEntry stores a parsed node with its string ID.
type nodeEntry struct {
	reg    sceneNodeRegistration
	nodeID string
}

// knownNodes tracks nodes currently registered in etcd, keyed by etcd key.
var (
	knownNodesMu sync.RWMutex
	knownNodes   = make(map[string]nodeEntry)
)

// knownNodeIdentityMatchCount 返回当前 watch 快照中同一 (zone,node) 身份的
// 注册条数。node_id 只在 zone 内唯一；同一身份出现两条及以上注册意味着租约、
// 部署或注册链已经分叉，任何一个 endpoint / Kafka 目标都不能被安全选中。
func knownNodeIdentityMatchCount(zoneID uint32, nodeID string) int {
	knownNodesMu.RLock()
	defer knownNodesMu.RUnlock()

	matches := 0
	for _, entry := range knownNodes {
		if entry.reg.ZoneId == zoneID && entry.nodeID == nodeID {
			matches++
		}
	}
	return matches
}

func isKnownNodeIdentityAmbiguous(zoneID uint32, nodeID string) bool {
	return knownNodeIdentityMatchCount(zoneID, nodeID) > 1
}

// parseNodeEntry parses a raw etcd value into a nodeEntry. Also validates
// scene_node_type: anything outside constants.SceneNodeType* is a
// misconfiguration (usually a stale image or env typo) — we keep the node
// but log loudly so it shows up on dashboards.
func parseNodeEntry(value []byte) (nodeEntry, bool) {
	var reg sceneNodeRegistration
	if err := json.Unmarshal(value, &reg); err != nil {
		logx.Errorf("[LoadReporter] failed to parse node registration: %v", err)
		return nodeEntry{}, false
	}
	if !isKnownSceneNodeType(reg.SceneNodeType) {
		logx.Errorf("[LoadReporter] unknown scene_node_type=%d on node %d (zone %d); "+
			"expected 0=MainWorld, 1=Instance, 2=MainWorldCross, 3=InstanceCross. "+
			"Check SCENE_NODE_TYPE env / game_config.yaml",
			reg.SceneNodeType, reg.NodeId, reg.ZoneId)
	}
	return nodeEntry{
		reg:    reg,
		nodeID: strconv.FormatUint(uint64(reg.NodeId), 10),
	}, true
}

// isKnownSceneNodeType reports whether t is one of the four declared
// eSceneNodeType values. Unknown values still participate in routing, but
// MatchesPurpose will reject them from both world and instance pools.
func isKnownSceneNodeType(t uint32) bool {
	return t == constants.SceneNodeTypeMainWorld ||
		t == constants.SceneNodeTypeInstance ||
		t == constants.SceneNodeTypeMainWorldCross ||
		t == constants.SceneNodeTypeInstanceCross
}

// updateNodeLoad reads the node's scene_count and player_count from Redis,
// writes the composite load score into the zone sorted set, and mirrors the
// scene_node_type so downstream purpose-based selection doesn't need to hit
// etcd. Score = α·scene_count + β·player_count; see Config weights.
func updateNodeLoad(svcCtx *svc.ServiceContext, entry nodeEntry) {
	sceneCount := readInt64(svcCtx, nodeSceneCountKey(entry.reg.ZoneId, entry.nodeID))
	playerCount := readInt64(svcCtx, nodePlayerCountKey(entry.reg.ZoneId, entry.nodeID))
	score := computeNodeLoadScore(svcCtx, sceneCount, playerCount)

	loadKey := nodeLoadKey(entry.reg.ZoneId)
	if _, err := svcCtx.Redis.Zadd(loadKey, int64(score), entry.nodeID); err != nil {
		logx.Errorf("[LoadReporter] failed to update load for node %s (zone %d): %v", entry.nodeID, entry.reg.ZoneId, err)
	}

	// Mirror scene_node_type into Redis so getNodesForPurpose can filter
	// without touching etcd on every CreateScene request.
	if err := svcCtx.Redis.Set(nodeSceneNodeTypeKey(entry.reg.ZoneId, entry.nodeID),
		strconv.FormatUint(uint64(entry.reg.SceneNodeType), 10)); err != nil {
		logx.Errorf("[LoadReporter] failed to mirror scene_node_type for node %s: %v", entry.nodeID, err)
	}

	metrics.ObserveNode(entry.nodeID, entry.reg.ZoneId, entry.reg.SceneNodeType,
		sceneCount, playerCount, score)
}

// computeNodeLoadScore combines scene_count and player_count using the
// configured weights. Weights default to 1.0 / 0.01 so scene_count dominates
// but many concurrent players still push a node down the preference list.
// Returns a float64 kept as int64 in the Redis sorted set for go-zero's
// typed API; the truncation rounds toward zero which is fine for load
// ordering (ties broken lexically by Redis).
func computeNodeLoadScore(svcCtx *svc.ServiceContext, sceneCount, playerCount int64) float64 {
	wScene := svcCtx.Config.NodeLoadWeightSceneCount
	wPlayer := svcCtx.Config.NodeLoadWeightPlayerCount
	if wScene <= 0 {
		wScene = 1.0
	}
	if wPlayer < 0 {
		wPlayer = 0
	}
	return wScene*float64(sceneCount) + wPlayer*float64(playerCount)
}

// readInt64 reads a string-encoded int from Redis, returning 0 on any error.
func readInt64(svcCtx *svc.ServiceContext, key string) int64 {
	s, err := svcCtx.Redis.Get(key)
	if err != nil || s == "" {
		return 0
	}
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// removeNodeFromRedis cleans up a node's load-set entry, cached connection,
// and mirrored scene_node_type.
//
// It also kicks off orphan scene reconciliation — instance scenes that
// were hosted on this dead node are force-destroyed, since the C++
// process holding their ECS entities is gone and they'd otherwise linger
// in Redis forever. World channels are left alone; the rebalance system
// migrates them in a separate path.
func removeNodeFromRedis(svcCtx *svc.ServiceContext, entry nodeEntry) {
	loadKey := nodeLoadKey(entry.reg.ZoneId)
	svcCtx.Redis.Zrem(loadKey, entry.nodeID)
	svcCtx.Redis.Del(nodeSceneNodeTypeKey(entry.reg.ZoneId, entry.nodeID))
	RemoveNodeConn(entry.reg.ZoneId, entry.nodeID)
	metrics.ForgetNode(entry.nodeID, entry.reg.ZoneId, entry.reg.SceneNodeType)
	logx.Infof("[LoadReporter] removed node %s from Redis load set (zone %d)", entry.nodeID, entry.reg.ZoneId)

	reconcileDeadNodeScenes(svcCtx, entry)

	// scene_count / player_count 必须在 reconcile **之后**清掉。
	// 这两个键全仓只有 Incr/Decr,没有任何"节点重新上线时归零"的路径 ——
	// 旧注释声称"下一个复用 id 的节点会覆盖它们"是不成立的:死节点残留的
	// 正计数会被复用同 id 的新节点原样继承,负载分永久虚高,调度一直躲着它。
	// 顺序放在 reconcile 之后,是因为 reconcile 里的 destroyInstanceForce
	// 自己还会对这两个键做减法;先删会被它们重新建出来。删完之后若还有
	// 极晚到的在途 destroy 把键重建成小负数,player_count 有 <0 归零钳制,
	// scene_count 的 -1 会被新节点的第一次 Incr 冲平 —— 都是有界的,
	// 与无界的正残留不同。
	deleteNodeCounters(svcCtx, entry.reg.ZoneId, entry.nodeID)
}

// deleteNodeCounters 清掉一个已判死节点的负载计数键。
func deleteNodeCounters(svcCtx *svc.ServiceContext, zoneID uint32, nodeID string) {
	if _, err := svcCtx.Redis.Del(nodeSceneCountKey(zoneID, nodeID)); err != nil {
		logx.Errorf("[LoadReporter] failed to delete scene_count for dead node %s (zone %d): %v", nodeID, zoneID, err)
	}
	if _, err := svcCtx.Redis.Del(nodePlayerCountKey(zoneID, nodeID)); err != nil {
		logx.Errorf("[LoadReporter] failed to delete player_count for dead node %s (zone %d): %v", nodeID, zoneID, err)
	}
}

// reconcileDeadNodeScenes is invoked when a node disappears from etcd.
// Walks node:zone:{zoneId}:{nodeId}:scenes and force-destroys orphaned instance scenes
// whose scene:{id}:node mapping still points at this dead node. Main
// world channels are left for the rebalance path; stale set entries for
// scenes that already got moved elsewhere are quietly dropped.
//
// Why not destroy world channels here:
//
//	Rebalance already has the logic to pick a replacement node AND tell
//	it to recreate the channel's ECS entity. Destroying a world channel
//	here would force players to re-enter a different scene and lose
//	their in-world context. Instances are per-run disposable, so
//	destroying an orphan is the right default.
func reconcileDeadNodeScenes(svcCtx *svc.ServiceContext, entry nodeEntry) {
	setKey := nodeScenesKey(entry.reg.ZoneId, entry.nodeID)
	members, err := svcCtx.Redis.Smembers(setKey)
	if err != nil {
		logx.Errorf("[Reconcile] Smembers %s failed: %v", setKey, err)
		return
	}
	if len(members) == 0 {
		svcCtx.Redis.Del(setKey)
		return
	}

	zoneId := entry.reg.ZoneId
	activeKey := activeInstancesKey(zoneId)
	destroyed, skippedWorld, stale := 0, 0, 0

	for _, s := range members {
		sceneId, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			continue
		}

		// Did this scene already get reassigned to a live node?
		currentNode, _ := svcCtx.Redis.Get(sceneNodeKey(sceneId))
		if currentNode != "" && currentNode != entry.nodeID {
			stale++
			continue
		}

		// Is this an instance? Only instances are tracked in the active set.
		// World channels live in world:channels:zone:{z}:conf:{c} and are
		// handled by the rebalance pipeline.
		_, zerr := svcCtx.Redis.Zscore(activeKey, s)
		if zerr != nil {
			skippedWorld++
			continue
		}

		logx.Infof("[Reconcile] Force-destroying orphan instance %d (was on dead node %s, zone %d)",
			sceneId, entry.nodeID, zoneId)
		destroyInstanceForce(context.Background(), svcCtx, zoneId, sceneId, "node_death")
		destroyed++
	}

	svcCtx.Redis.Del(setKey)

	metrics.ObserveSceneOrphansReconciled(zoneId, destroyed)

	if destroyed > 0 || stale > 0 || skippedWorld > 0 {
		logx.Infof("[Reconcile] Node %s (zone %d): total=%d destroyed=%d world_skipped=%d stale=%d",
			entry.nodeID, zoneId, len(members), destroyed, skippedWorld, stale)
	}
}

// FindNodeByPodIP maps a Pod IP back to a registered scene node.
//
// Agones 分配链路需要这个:GameServerAllocation 给回来的是
// gameServerName + PodIP,而 SceneManager 下游(Redis 映射、CreateScene RPC)
// 一律用 C++ 侧自己注册的 nodeID。两者唯一的公共键就是 Pod IP ——
// C++ 节点注册进 etcd 的 endpoint/grpcEndpoint IP 就是容器里的 POD_IP
// (Fleet 模板通过 downward API 注入)。
//
// 返回 (nodeID, zoneID, sceneNodeType, ok)。ok==false 表示这个 Pod 还没
// 把自己注册进 etcd(启动竞态),调用方必须 fail-closed。
//
// gRPC 与普通 endpoint 都比对:两者在 K8s 里是同一个 Pod IP,但不同版本
// 的 C++ 节点可能只填其中一个。
func FindNodeByPodIP(podIP string) (string, uint32, uint32, bool) {
	if podIP == "" {
		return "", 0, 0, false
	}

	knownNodesMu.RLock()
	defer knownNodesMu.RUnlock()
	var found nodeEntry
	matches := 0
	for _, entry := range knownNodes {
		if entry.reg.GrpcEndpoint.IP == podIP || entry.reg.Endpoint.IP == podIP {
			matches++
			found = entry
		}
	}
	// 即使两条记录声称同一 (zone,node) 且 endpoint 相同，也不能把重复注册
	// 当作一个健康节点。Agones rooms 预占一旦落到错误实例就无法精确归还。
	if matches == 1 {
		return found.nodeID, found.reg.ZoneId, found.reg.SceneNodeType, true
	}
	return "", 0, 0, false
}

// FindPodIPByNodeID 是 FindNodeByPodIP 的反向查询,供镜像共置路径把
// "要共置的 nodeID" 翻回 Pod IP,再去反查对应的 Agones GameServer。
func FindPodIPByNodeID(zoneID uint32, nodeID string) (string, bool) {
	if nodeID == "" {
		return "", false
	}

	knownNodesMu.RLock()
	defer knownNodesMu.RUnlock()
	var podIP string
	matches := 0
	for _, entry := range knownNodes {
		if entry.reg.ZoneId != zoneID || entry.nodeID != nodeID {
			continue
		}
		matches++
		if ip := entry.reg.GrpcEndpoint.IP; ip != "" {
			if podIP != "" && podIP != ip {
				return "", false
			}
			podIP = ip
			continue
		}
		if ip := entry.reg.Endpoint.IP; ip != "" {
			if podIP != "" && podIP != ip {
				return "", false
			}
			podIP = ip
			continue
		}
		return "", false
	}
	return podIP, matches == 1 && podIP != ""
}

// SetKnownNodeForTest 让单元测试在不起 etcd 的情况下往 knownNodes 里塞条目。
// 返回的 restore 必须配合 t.Cleanup 调用,否则会污染后续用例。
func SetKnownNodeForTest(etcdKey, nodeID, podIP string, zoneID, sceneNodeType uint32) func() {
	knownNodesMu.Lock()
	defer knownNodesMu.Unlock()

	prev, existed := knownNodes[etcdKey]
	entry := nodeEntry{nodeID: nodeID}
	entry.reg.ZoneId = zoneID
	entry.reg.SceneNodeType = sceneNodeType
	entry.reg.GrpcEndpoint.IP = podIP
	entry.reg.Endpoint.IP = podIP
	knownNodes[etcdKey] = entry

	return func() {
		knownNodesMu.Lock()
		defer knownNodesMu.Unlock()
		if existed {
			knownNodes[etcdKey] = prev
		} else {
			delete(knownNodes, etcdKey)
		}
	}
}

// rebuildActiveZones derives activeZones from knownNodes.
func rebuildActiveZones() {
	knownNodesMu.RLock()
	zoneSet := make(map[uint32]struct{})
	for _, entry := range knownNodes {
		zoneSet[entry.reg.ZoneId] = struct{}{}
	}
	knownNodesMu.RUnlock()

	zones := make([]uint32, 0, len(zoneSet))
	for z := range zoneSet {
		zones = append(zones, z)
	}
	activeZonesMu.Lock()
	activeZones = zones
	activeZonesMu.Unlock()
}

// ---------------------------------------------------------------------------
// StartLoadReporter — list-watch pattern (like K8s informer)
// ---------------------------------------------------------------------------

// StartLoadReporter uses etcd Watch to reactively discover scene node changes,
// with a periodic ticker for load-score refresh and grace-period checks.
// On Watch error or channel close, it re-lists and re-watches automatically.
func StartLoadReporter(ctx context.Context, svcCtx *svc.ServiceContext) {
	for {
		rev, err := fullSync(ctx, svcCtx)
		if err != nil {
			logx.Errorf("[LoadReporter] full sync failed: %v, retrying in 3s", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
				continue
			}
		}

		watchAndRefresh(ctx, svcCtx, rev)

		if ctx.Err() != nil {
			return
		}
		logx.Info("[LoadReporter] watch interrupted, re-syncing")
	}
}

// fullSync does a complete etcd Get to build baseline state, updates Redis,
// and ensures main scenes for all discovered zones. Returns the etcd revision.
func fullSync(ctx context.Context, svcCtx *svc.ServiceContext) (int64, error) {
	resp, err := svcCtx.Etcd.Get(ctx, sceneNodePrefix, clientv3.WithPrefix())
	if err != nil {
		return 0, fmt.Errorf("etcd get %s: %w", sceneNodePrefix, err)
	}

	seenByZone := make(map[uint32]map[string]struct{})

	knownNodesMu.Lock()
	knownNodes = make(map[string]nodeEntry, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		key := string(kv.Key)
		entry, ok := parseNodeEntry(kv.Value)
		if !ok {
			continue
		}
		knownNodes[key] = entry

		zoneId := entry.reg.ZoneId
		if seenByZone[zoneId] == nil {
			seenByZone[zoneId] = make(map[string]struct{})
		}
		seenByZone[zoneId][entry.nodeID] = struct{}{}

		updateNodeLoad(svcCtx, entry)
	}
	knownNodesMu.Unlock()

	// Update activeZones.
	zones := make([]uint32, 0, len(seenByZone))
	for z := range seenByZone {
		zones = append(zones, z)
	}
	activeZonesMu.Lock()
	activeZones = zones
	activeZonesMu.Unlock()

	// Clean stale Redis entries not in current etcd snapshot.
	for _, zoneId := range zones {
		seen := seenByZone[zoneId]
		loadKey := nodeLoadKey(zoneId)
		pairs, err := svcCtx.Redis.ZrangeWithScores(loadKey, 0, -1)
		if err != nil {
			continue
		}
		for _, p := range pairs {
			if _, ok := seen[p.Key]; !ok {
				svcCtx.Redis.Zrem(loadKey, p.Key)
				svcCtx.Redis.Del(nodeSceneNodeTypeKey(zoneId, p.Key))
				RemoveNodeConn(zoneId, p.Key)
				// SceneManager 停机期间死掉的节点走不到 watch DELETE,
				// 计数残留同样要在这里清(理由见 removeNodeFromRedis)。
				deleteNodeCounters(svcCtx, zoneId, p.Key)
			}
		}
	}

	// Ensure world scenes for all zones (handles SceneManager restart). We
	// run init followed by a rebalance pass: init handles under-provisioning
	// (new confIds, fresh zone) while rebalance handles drift that
	// accumulated while SceneManager was offline (nodes joined/left without
	// us observing the PUT/DELETE event).
	if wids := worldConfIds(); len(wids) > 0 {
		for _, z := range zones {
			initWorldScenesForZone(ctx, svcCtx, z, wids)
			RebalanceWorldChannelsForZone(ctx, svcCtx, z, wids)
		}
	}

	// Opt-out orphan cleanup: drop world_channels:* sets for confIds that
	// no longer exist in World.json. Runs once per fullSync iteration so a
	// long-lived SceneManager picks up table edits after a live reload.
	if svcCtx.Config.CleanupOrphanChannelsOnStartup {
		CleanupOrphanWorldChannels(ctx, svcCtx)
	}

	logx.Infof("[LoadReporter] full sync: %d nodes, %d zones, rev=%d",
		len(resp.Kvs), len(zones), resp.Header.Revision)
	return resp.Header.Revision, nil
}

// watchAndRefresh starts an etcd Watch from the given revision, a periodic
// ticker for load-score refresh and grace-period expiry checks, and a
// longer-period ticker for best-effort rebalance convergence. The rebalance
// tick catches drift that events alone can miss (hot channels that drained
// after the last join/leave event) and acts as a self-healing safety net
// when etcd watch misses an update during a network blip.
func watchAndRefresh(ctx context.Context, svcCtx *svc.ServiceContext, rev int64) {
	watchCh := svcCtx.Etcd.Watch(ctx, sceneNodePrefix,
		clientv3.WithPrefix(),
		clientv3.WithRev(rev+1),
		clientv3.WithPrevKV(),
	)

	loadTicker := time.NewTicker(LoadReportInterval)
	defer loadTicker.Stop()

	rebalanceTicker, stopRebalanceTicker := newRebalanceTicker(svcCtx)
	defer stopRebalanceTicker()

	for {
		select {
		case <-ctx.Done():
			return
		case watchResp, ok := <-watchCh:
			if !ok {
				logx.Info("[LoadReporter] watch channel closed")
				return
			}
			if watchResp.Err() != nil {
				logx.Errorf("[LoadReporter] watch error: %v", watchResp.Err())
				return
			}
			for _, ev := range watchResp.Events {
				handleWatchEvent(ctx, svcCtx, ev)
			}
		case <-loadTicker.C:
			refreshLoadScores(svcCtx)
		case <-rebalanceTicker:
			runPeriodicRebalance(ctx, svcCtx)
		}
	}
}

// newRebalanceTicker returns a receive-only channel that fires every
// RebalanceCheckIntervalSeconds, and a stop function to release its
// underlying resources. When the interval is 0 the returned channel is
// nil (a nil channel blocks forever in select, so the ticker branch is
// effectively disabled without a special-case in the caller).
func newRebalanceTicker(svcCtx *svc.ServiceContext) (<-chan time.Time, func()) {
	secs := svcCtx.Config.RebalanceCheckIntervalSeconds
	if secs <= 0 {
		return nil, func() {}
	}
	t := time.NewTicker(time.Duration(secs) * time.Second)
	return t.C, t.Stop
}

// runPeriodicRebalance fires RebalanceWorldChannelsForZone for every active
// zone. This is the fallback path for drift that etcd events don't surface
// (e.g. a hot channel drained and became opportunistic-migratable).
func runPeriodicRebalance(ctx context.Context, svcCtx *svc.ServiceContext) {
	wids := worldConfIds()
	if len(wids) == 0 {
		return
	}
	for _, z := range GetActiveZones() {
		RebalanceWorldChannelsForZone(ctx, svcCtx, z, wids)
	}
}

// handleWatchEvent processes a single etcd PUT or DELETE event.
func handleWatchEvent(ctx context.Context, svcCtx *svc.ServiceContext, ev *clientv3.Event) {
	key := string(ev.Kv.Key)

	switch ev.Type {
	case clientv3.EventTypePut:
		entry, ok := parseNodeEntry(ev.Kv.Value)
		if !ok {
			return
		}

		knownNodesMu.Lock()
		prev, existed := knownNodes[key]
		knownNodes[key] = entry
		knownNodesMu.Unlock()

		// Detect role / zone flip on the same etcd key. A legitimate restart
		// keeps the (zone_id, scene_node_type) tuple stable; a flip almost
		// always means operator error (wrong env on redeploy, wrong pod
		// picking up an old key before etcd TTL expired). Warn loudly.
		if existed {
			if prev.reg.SceneNodeType != entry.reg.SceneNodeType {
				logx.Errorf("[LoadReporter] node %s scene_node_type changed %d -> %d on etcd PUT "+
					"(same key %s). Likely a deploy mix-up; routing will follow the new value.",
					entry.nodeID, prev.reg.SceneNodeType, entry.reg.SceneNodeType, key)
			}
			if prev.reg.ZoneId != entry.reg.ZoneId {
				logx.Errorf("[LoadReporter] node %s zone_id changed %d -> %d on etcd PUT "+
					"(same key %s). Likely a deploy mix-up; old zone load set may leak.",
					entry.nodeID, prev.reg.ZoneId, entry.reg.ZoneId, key)
			}
		}

		// Clear stale gRPC connection: endpoint may have changed after restart.
		RemoveNodeConn(entry.reg.ZoneId, entry.nodeID)
		if existed && prev.reg.ZoneId != entry.reg.ZoneId {
			RemoveNodeConn(prev.reg.ZoneId, prev.nodeID)
			svcCtx.Redis.Zrem(nodeLoadKey(prev.reg.ZoneId), prev.nodeID)
			svcCtx.Redis.Del(nodeSceneNodeTypeKey(prev.reg.ZoneId, prev.nodeID))
		}

		updateNodeLoad(svcCtx, entry)
		rebuildActiveZones()

		if wids := worldConfIds(); len(wids) > 0 {
			if !existed {
				logx.Infof("[LoadReporter] Zone %d: node %s appeared (watch PUT, role=%d)",
					entry.reg.ZoneId, entry.nodeID, entry.reg.SceneNodeType)
				initWorldScenesForZone(ctx, svcCtx, entry.reg.ZoneId, wids)
			}
			// A world-hosting node joined (or re-registered after a restart /
			// role flip): the hash ring changed. Try to rebalance empty
			// channels toward the new shape. Skipping on non-world-hosting
			// nodes avoids a pointless scan of the world channel set.
			roleChanged := existed && prev.reg.SceneNodeType != entry.reg.SceneNodeType
			if constants.IsWorldHostingType(entry.reg.SceneNodeType) && (!existed || roleChanged) {
				RebalanceWorldChannelsForZone(ctx, svcCtx, entry.reg.ZoneId, wids)
			}
		}

	case clientv3.EventTypeDelete:
		knownNodesMu.Lock()
		entry, existed := knownNodes[key]
		if existed {
			delete(knownNodes, key)
		}
		knownNodesMu.Unlock()

		if !existed {
			return
		}

		rebuildActiveZones()
		removeNodeFromRedis(svcCtx, entry)

		// A world-hosting node departed: channels mapped to it are now
		// unreachable for new players. Rebalance immediately so those
		// channels find a new home on the next EnterScene, not on the
		// next LoadReportInterval tick.
		if constants.IsWorldHostingType(entry.reg.SceneNodeType) {
			if wids := worldConfIds(); len(wids) > 0 {
				RebalanceWorldChannelsForZone(ctx, svcCtx, entry.reg.ZoneId, wids)
			}
		}
	}
}

// refreshLoadScores updates Redis load scores for all currently known nodes
// and refreshes the nodes_by_role metric so dashboards reflect the current
// cluster topology even when individual node scores don't move.
func refreshLoadScores(svcCtx *svc.ServiceContext) {
	knownNodesMu.RLock()
	defer knownNodesMu.RUnlock()

	counts := make(map[struct {
		ZoneID uint32
		Role   uint32
	}]int)
	for _, entry := range knownNodes {
		updateNodeLoad(svcCtx, entry)
		counts[struct {
			ZoneID uint32
			Role   uint32
		}{entry.reg.ZoneId, entry.reg.SceneNodeType}]++
	}
	metrics.SetNodesByRole(counts)
}

// GetBestNode selects the instance-hosting node with the lowest load from
// the given zone. Kept as the default selector for historical callers that
// request instance-style nodes (mirror fallbacks, ad-hoc instances).
//
// See GetBestNodeForPurpose for the purpose-aware entry point.
func GetBestNode(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32) (string, error) {
	return GetBestNodeForPurpose(ctx, svcCtx, zoneId, constants.NodePurposeInstance)
}

// IsNodeAlive checks whether a node is present in the zone's Redis load sorted set
// and has no duplicate (zone,node) registration in the current etcd snapshot.
func IsNodeAlive(svcCtx *svc.ServiceContext, zoneId uint32, nodeId string) bool {
	if isKnownNodeIdentityAmbiguous(zoneId, nodeId) {
		return false
	}
	_, err := svcCtx.Redis.Zscore(nodeLoadKey(zoneId), nodeId)
	return err == nil
}
