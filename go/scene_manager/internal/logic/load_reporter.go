package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"scene_manager/internal/constants"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
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

	// Redis 写只归领导者:这不是幂等同值写 —— 跟随者的 watch 镜像可能落后
	// 于领导者的清理,比如领导者刚把死节点 Zrem 出负载集,跟随者的 5s 刷新
	// 又把它 Zadd 回去;而 DELETE 事件领导者已经消费过,没有第二次删除,
	// "复活"的条目会一直留到下一次 fullSync。指标每副本照常发布,
	// 跟随者的 dashboard 不断流。
	if isLeader() {
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
// 动作被再入屏障切成两半(docs/design/scene-owner-reentry-barrier.md §3.2):
//
//	**立刻做** —— 先记下死亡时刻,再把节点摘出调度面(负载集、类型镜像、
//	  gRPC 连接缓存、指标)。这些都不改写任何 scene ownership,也不销毁任何
//	  东西,只是让新玩家不再被派到它上面,越早越好。两者的先后**不可交换**,
//	  理由见函数体内第一段注释。
//
//	**推迟做** —— 孤儿实例场景的强制销毁与节点计数清理。C++ 老节点丢租约后
//	  还有 kDrainBudget(15s)的 emergency relocate,期间仍在 SavePlayerToRedis;
//	  此刻销毁它名下的场景映射,等于在老节点停笔前就把归属改了。任务入队,
//	  由 drainPendingDeadNodeReconciles 在屏障走完后执行。
//
// 大世界频道不在这条路径里销毁 —— rebalance 有专门的迁移逻辑(它同样受屏障
// 约束),在这里销毁会让玩家丢掉世界内上下文。
func removeNodeFromRedis(svcCtx *svc.ServiceContext, entry nodeEntry) {
	// 死亡时刻必须**第一个**落地,早于把节点摘出负载集。
	//
	// 判死的可观测顺序决定了屏障还有没有用。改派点的判据是两个独立的键:
	// IsNodeAlive 读的正是下面 Zrem 的那个负载集,CanReclaimDeadNode 读的是
	// death_at,而后者「键不存在」的语义是**放行**。所以只要 Zrem 先执行,
	// 在 death_at 写进去之前的那几个 Redis 往返里,并发的 EnterScene 看到的
	// 组合就是「节点已死(不在负载集)」+「没有 death_at ⇒ 屏障已过」——
	// 玩家会在老节点 15s emergency drain 还没跑完时就被改派到新节点,
	// 正是这道屏障要防的双写/回档(docs/design/scene-owner-reentry-barrier.md §2)。
	// 窗口虽窄,但开服浪涌下 EnterScene 的频率足以撞上,且后果是玩家数据回档。
	//
	// 反过来的失败模式是安全的:标记写成功而进程随即崩在 Zrem 之前,节点仍留在
	// 负载集里 ⇒ IsNodeAlive 为真,本来就不会触发改派;屏障多压一个 TTL 也只是
	// 少自愈一轮。这正是本文件其余判定一贯的 fail-closed 方向。
	markNodeDeath(svcCtx, entry.reg.ZoneId, entry.nodeID)

	loadKey := nodeLoadKey(entry.reg.ZoneId)
	svcCtx.Redis.Zrem(loadKey, entry.nodeID)
	svcCtx.Redis.Del(nodeSceneNodeTypeKey(entry.reg.ZoneId, entry.nodeID))
	RemoveNodeConn(entry.reg.ZoneId, entry.nodeID)
	metrics.ForgetNode(entry.nodeID, entry.reg.ZoneId, entry.reg.SceneNodeType)
	logx.Infof("[LoadReporter] removed node %s from Redis load set (zone %d)", entry.nodeID, entry.reg.ZoneId)

	enqueueDeadNodeReconcile(svcCtx, entry)
}

// deleteNodeCounters 清掉一个已判死节点的负载计数键。
//
// 这两个键全仓只有 Incr/Decr,没有任何"节点重新上线时归零"的路径 ——
// 旧注释声称"下一个复用 id 的节点会覆盖它们"是不成立的:死节点残留的正计数
// 会被复用同 id 的新节点原样继承,负载分永久虚高,调度一直躲着它。
//
// ⚠️ 调用顺序:必须在 reconcileDeadNodeScenes **之后**。reconcile 里的
// destroyInstanceForce 自己还会对这两个键做减法,先删会被它们重新建出来。
// 删完之后若还有极晚到的在途 destroy 把键重建成小负数,player_count 有 <0
// 归零钳制,scene_count 的 -1 会被新节点的第一次 Incr 冲平 —— 都是有界的,
// 与无界的正残留不同。
func deleteNodeCounters(svcCtx *svc.ServiceContext, zoneID uint32, nodeID string) {
	if _, err := svcCtx.Redis.Del(nodeSceneCountKey(zoneID, nodeID)); err != nil {
		logx.Errorf("[LoadReporter] failed to delete scene_count for dead node %s (zone %d): %v", nodeID, zoneID, err)
	}
	if _, err := svcCtx.Redis.Del(nodePlayerCountKey(zoneID, nodeID)); err != nil {
		logx.Errorf("[LoadReporter] failed to delete player_count for dead node %s (zone %d): %v", nodeID, zoneID, err)
	}
}

// reconcileDeadNodeScenes force-destroys the orphaned instance scenes of a node
// that disappeared from etcd, once its re-entry barrier has elapsed.
//
// members 是**判死那一刻**抄下的 node:zone:{zoneId}:{nodeId}:scenes 快照,由
// enqueueDeadNodeReconcile 传进来 —— 不在这里现读。屏障窗口(默认 20s)里完全
// 可能有新场景被建到同一个 node_id 上(进程换代但 id 被复用),现读会把这些
// 新场景一起强制销毁。快照把收尾范围钉死在「它死时确实属于它的那些场景」。
//
// 只销毁实例场景:scene:{id}:node 仍指向本节点、且在 active instances 集合里的
// 那些。大世界频道留给 rebalance 路径;已经被改派走的陈旧条目静默丢弃。
//
// Why not destroy world channels here:
//
//	Rebalance already has the logic to pick a replacement node AND tell
//	it to recreate the channel's ECS entity. Destroying a world channel
//	here would force players to re-enter a different scene and lose
//	their in-world context. Instances are per-run disposable, so
//	destroying an orphan is the right default.
func reconcileDeadNodeScenes(ctx context.Context, svcCtx *svc.ServiceContext, entry nodeEntry, members []string) {
	setKey := nodeScenesKey(entry.reg.ZoneId, entry.nodeID)
	if len(members) == 0 {
		// 快照为空:它死时名下没有场景。此刻集合里若有内容,那是屏障窗口里
		// 新建到同一 node_id 上的,不属于本次收尾范围,绝不能 Del 掉。
		return
	}

	zoneId := entry.reg.ZoneId
	activeKey := activeInstancesKey(zoneId)
	destroyed, skippedWorld, stale := 0, 0, 0

	for _, s := range members {
		// 降级即停,且**不删** setKey:留着集合,新领导者的 fullSync
		// stale 清理会对同一个死节点重跑本函数,接手剩余成员。
		// destroyInstanceForce 内部是 Lua 原子认领,即便与新领导者
		// 短暂并发,每个场景的副作用也只会执行一次。
		if !isLeader() {
			logx.Infof("[Reconcile] lost leadership mid-sweep for node %s (zone %d), aborting; successor will re-sweep",
				entry.nodeID, entry.reg.ZoneId)
			return
		}
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
		destroyInstanceForce(ctx, svcCtx, zoneId, sceneId, "node_death")
		destroyed++
	}

	// 只摘掉快照里的成员,不 Del 整个集合:屏障窗口里可能有新场景被建到同一个
	// node_id 上并加进了这个集合,Del 会把它们的反向索引一起抹掉,以后再没有
	// 任何路径能发现它们。集合被摘空后 Redis 会自动回收键。
	if len(members) > 0 {
		values := make([]any, 0, len(members))
		for _, m := range members {
			values = append(values, m)
		}
		if _, err := svcCtx.Redis.Srem(setKey, values...); err != nil {
			logx.Errorf("[Reconcile] Srem %s failed: %v", setKey, err)
		}
	}

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

	next := make(map[string]nodeEntry, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		key := string(kv.Key)
		entry, ok := parseNodeEntry(kv.Value)
		if !ok {
			continue
		}
		next[key] = entry

		zoneId := entry.reg.ZoneId
		if seenByZone[zoneId] == nil {
			seenByZone[zoneId] = make(map[string]struct{})
		}
		seenByZone[zoneId][entry.nodeID] = struct{}{}
	}

	knownNodesMu.Lock()
	prevKnown := knownNodes
	knownNodes = next
	knownNodesMu.Unlock()

	for _, entry := range next {
		updateNodeLoad(svcCtx, entry)
	}

	// 每副本的进程本地清理(不走领导者闸门):watch DELETE 在跟随者上只做
	// 本地清理,但整段 DELETE 事件也可能被错过(watch 重建窗口)。用新旧
	// 快照的差集兜底,清掉已消失节点的 gRPC 连接缓存与 Prometheus 序列 ——
	// 这两样是进程私有的,领导者替代不了。
	for key, prev := range prevKnown {
		if _, still := next[key]; !still {
			RemoveNodeConn(prev.reg.ZoneId, prev.nodeID)
			metrics.ForgetNode(prev.nodeID, prev.reg.ZoneId, prev.reg.SceneNodeType)
		}
	}

	// Update activeZones.
	zones := make([]uint32, 0, len(seenByZone))
	for z := range seenByZone {
		zones = append(zones, z)
	}
	activeZonesMu.Lock()
	activeZones = zones
	activeZonesMu.Unlock()

	// 以下全部是**变更类**动作,多副本时只有领导者执行(单写者语义);
	// 跟随者的 fullSync 只负责重建本进程的 etcd 内存镜像与负载分。
	// 领导者缺位窗口(选举间隙 / 领导者刚挂)里漏掉的动作,由新任领导者
	// 当选时触发的 RequestLoadReporterResync -> fullSync 一次补齐。
	if isLeader() {
		// Clean stale Redis entries not in current etcd snapshot.
		//
		// 扫描范围 = 快照里的 zone ∪ Redis 里还留有负载集的 zone。只按快照
		// 扫有个洞:某 zone 的**最后一个**节点在无领导窗口(或 SM 整体停机)
		// 里死掉时,快照里根本没有这个 zone,它的负载集条目、计数和孤儿
		// 实例就永远没人清(runPeriodicRebalance 也只迭代 GetActiveZones)。
		sweepZones := make(map[uint32]struct{}, len(zones))
		for _, z := range zones {
			sweepZones[z] = struct{}{}
		}
		cursor := uint64(0)
		for {
			loadKeys, nextCursor, err := svcCtx.Redis.Scan(cursor, "scene_nodes:zone:*:load", 64)
			if err != nil {
				logx.Errorf("[LoadReporter] scan zone load keys failed at cursor=%d: %v", cursor, err)
				break
			}
			for _, k := range loadKeys {
				var z uint32
				if _, err := fmt.Sscanf(k, NodeLoadKeyFmt, &z); err == nil {
					sweepZones[z] = struct{}{}
				}
			}
			if nextCursor == 0 {
				break
			}
			cursor = nextCursor
		}

		for zoneId := range sweepZones {
			// seen 对快照外的 zone 是 nil map —— 该 zone 所有条目都视为 stale。
			seen := seenByZone[zoneId]
			loadKey := nodeLoadKey(zoneId)
			pairs, err := svcCtx.Redis.ZrangeWithScores(loadKey, 0, -1)
			if err != nil {
				continue
			}
			for _, p := range pairs {
				// 长循环里领导权可能中途易主:降级即停,
				// 新领导者的 resync fullSync 会重扫同一批。
				if !isLeader() {
					return resp.Header.Revision, nil
				}
				if _, ok := seen[p.Key]; !ok {
					svcCtx.Redis.Zrem(loadKey, p.Key)
					svcCtx.Redis.Del(nodeSceneNodeTypeKey(zoneId, p.Key))
					RemoveNodeConn(zoneId, p.Key)
					// SceneManager 停机 / 领导者缺位窗口期间死掉的节点走不到
					// watch DELETE,这里是唯一能观察到它们消失的地方。死亡时刻
					// 必须补记:不记的话 CanReclaimDeadNode 读不到标记,会把它们
					// 当成「没死过」直接放行改派,而它们很可能是几秒前刚断的、
					// C++ 侧还在 drain。我们不知道真实死亡时刻,只能用「首次
					// 观察到」这个偏晚的时刻 —— 方向是安全的(屏障结束点跟着偏晚)。
					markNodeDeath(svcCtx, zoneId, p.Key)
					// 孤儿实例场景与计数残留也只有这条路径能补收(这也是新任
					// 领导者补齐跟随期间漏掉的 DELETE 事件的唯一路径)。与 watch
					// DELETE 同构:入队等再入屏障走完,由 drainPendingDeadNodeReconciles
					// 收尾;计数由它在 reconcile **之后**清(顺序理由见 deleteNodeCounters)。
					staleEntry := nodeEntry{nodeID: p.Key}
					staleEntry.reg.ZoneId = zoneId
					enqueueDeadNodeReconcile(svcCtx, staleEntry)
				}
			}
		}

		// watch 中断期间入队的死节点收尾在这里也推一把:屏障已过的立刻做掉,
		// 没过的原样留在队列里等下一拍。
		drainPendingDeadNodeReconciles(ctx, svcCtx)

		// Ensure world scenes for all zones (handles SceneManager restart). We
		// run init followed by a rebalance pass: init handles under-provisioning
		// (new confIds, fresh zone) while rebalance handles drift that
		// accumulated while SceneManager was offline (nodes joined/left without
		// us observing the PUT/DELETE event).
		if wids := worldConfIds(); len(wids) > 0 {
			for _, z := range zones {
				initWorldScenesForZone(ctx, svcCtx, z, wids, false)
				RebalanceWorldChannelsForZone(ctx, svcCtx, z, wids)
			}
		}

		// Opt-out orphan cleanup: drop world_channels:* sets for confIds that
		// no longer exist in World.json. Runs once per fullSync iteration so a
		// long-lived SceneManager picks up table edits after a live reload.
		if svcCtx.Config.CleanupOrphanChannelsOnStartup {
			CleanupOrphanWorldChannels(ctx, svcCtx)
		}
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
			// 被再入屏障压着的死节点收尾:屏障(默认 20s)远大于一拍(5s),
			// 所以「等屏障」就是「多等几拍」,不需要每个死节点起一条 goroutine。
			drainPendingDeadNodeReconciles(ctx, svcCtx)
		case <-rebalanceTicker:
			runPeriodicRebalance(ctx, svcCtx)
		case <-loadReporterResync:
			// 领导权刚变化:返回让外层循环重跑 fullSync。新任领导者借此
			// 立即补齐跟随者期间跳过的变更动作(补频道 / rebalance / 清理),
			// 而不是等下一次 watch 中断。
			logx.Info("[LoadReporter] resync requested (leadership change)")
			return
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
	// rebalance 会迁移频道并改写路由,是变更动作 —— 只有领导者做。
	if !isLeader() {
		return
	}
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

		// 节点(重新)注册 = 它此刻是活的,上一次的死亡标记必须抹掉,否则
		// IsNodeAlive 说活、再入屏障却还压着它名下的场景,两个判定自相矛盾。
		clearNodeDeath(svcCtx, entry.reg.ZoneId, entry.nodeID)

		// Clear stale gRPC connection: endpoint may have changed after restart.
		RemoveNodeConn(entry.reg.ZoneId, entry.nodeID)
		if existed && prev.reg.ZoneId != entry.reg.ZoneId {
			RemoveNodeConn(prev.reg.ZoneId, prev.nodeID)
			svcCtx.Redis.Zrem(nodeLoadKey(prev.reg.ZoneId), prev.nodeID)
			svcCtx.Redis.Del(nodeSceneNodeTypeKey(prev.reg.ZoneId, prev.nodeID))
		}

		updateNodeLoad(svcCtx, entry)
		rebuildActiveZones()

		// 补频道 / rebalance 是变更动作,只有领导者做;跟随者只维护上面的
		// 内存镜像与负载分。跟随者当选时会经 resync -> fullSync 补齐。
		if wids := worldConfIds(); len(wids) > 0 && isLeader() {
			if !existed {
				logx.Infof("[LoadReporter] Zone %d: node %s appeared (watch PUT, role=%d)",
					entry.reg.ZoneId, entry.nodeID, entry.reg.SceneNodeType)
				// waitForLock=false:本函数跑在 etcd watch goroutine 上,
				// 忙等锁会堵住 DELETE 事件与定时器;锁忙说明别人在补建,
				// 跳过即可。
				initWorldScenesForZone(ctx, svcCtx, entry.reg.ZoneId, wids, false)
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

		// 进程本地清理每个副本都要做:gRPC 连接缓存与 Prometheus 标签
		// 是各进程私有的,不清会拿着死节点的 stale endpoint 继续发请求。
		RemoveNodeConn(entry.reg.ZoneId, entry.nodeID)
		metrics.ForgetNode(entry.nodeID, entry.reg.ZoneId, entry.reg.SceneNodeType)

		// Redis 侧清理(负载集摘除 / 孤儿实例销毁 / 计数删除)与 rebalance
		// 是全局变更,只有领导者做。领导者缺位窗口里漏掉的 DELETE,由新任
		// 领导者当选时的 fullSync 补齐(其 stale 清理路径会跑同一套 reconcile)。
		if !isLeader() {
			return
		}
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
//
// 三态语义:Redis 查询有三种结果,但本函数只能返回 bool,所以「状态未知」必须
// 被映射到**不会触发破坏性动作**的那一侧。
//
//	err == nil        节点在负载集里            → 存活
//	errors.Is(redis.Nil) 成员确实不在负载集     → 已死
//	其它 err(超时/连不上/EOF) 状态未知         → 按存活处理(见下)
//
// 为什么未知按「存活」:本函数的返回值驱动的是场景改派(enterscenelogic
// resolveScene / world_rebalance)、场景销毁(destroyscenelogic)与孤儿清理
// (orphan_cleanup)。判「死」是**破坏性**的一侧——它会改写 scene:{id}:node
// 并让新节点 CreateScene。而 C++ 老节点在丢租约后还有 15s 的 emergency
// relocate drain(node.cpp kDrainBudget),期间仍在 SavePlayerToRedis。
// 一次 Redis 抖动若被当成「节点已死」,就会在老节点还在存盘时把玩家改派到
// 新节点,造成同一玩家双写 / 回档。
//
// 反过来,未知按「存活」的代价只是本轮不改派 / 不清理:若节点真的死了,下一轮
// (Redis 恢复后)会正确判死并自愈;若只是 Redis 抖动,则什么都没发生 —— 这是
// 可自愈的瞬时降级,不是数据损坏。两害相权,fail-closed against 改派。
//
// 完整修复见 docs/design/scene-owner-reentry-barrier.md:本函数只挡住「误判死」,
// 「判死后立刻改派、不等老节点停笔」那半边要靠再入屏障(§3.2)。
func IsNodeAlive(svcCtx *svc.ServiceContext, zoneId uint32, nodeId string) bool {
	if isKnownNodeIdentityAmbiguous(zoneId, nodeId) {
		return false
	}
	_, err := svcCtx.Redis.Zscore(nodeLoadKey(zoneId), nodeId)
	if err == nil {
		return true
	}
	if errors.Is(err, redis.Nil) {
		// 成员确实不在负载集 —— 这是唯一可以断言「已死」的证据。
		return false
	}
	// 状态未知:绝不能据此触发改派 / 销毁 / 清理。
	logx.Errorf("[LoadReporter] IsNodeAlive 状态未知,按存活处理以免误改派: zone=%d node=%s err=%v",
		zoneId, nodeId, err)
	return true
}
