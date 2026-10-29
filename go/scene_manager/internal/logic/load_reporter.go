package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
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
	// NodeUuid 是 C++ 进程启动时生成的实例号(NodeInfo.node_uuid):同一个 node_id 被重启后的新进程复用时,
	// 只有它能区分两代进程。交接资格里盖的就是它(owner_replaced.go,GO-3)。旧版本注册可能不带。
	NodeUuid string `json:"nodeUuid"`
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
	// knownNodesSynced:本进程是否已经至少完成过一次 etcd 全量同步。
	// 在那之前 knownNodes 是空表,「表里没有这个节点」不是任何事情的证据 ——
	// 要拿「节点已从 etcd 注销」当死亡证据的判定(playerLocationOwnerDead)必须先看它。
	// 只置真、不复位:watch 中断后的重同步期间表里留着的是旧条目,旧条目只会让节点
	// 「看起来还在」,方向是 fail-closed。
	knownNodesSynced atomic.Bool
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

// nodeGoneObservedAt 记录**本副本**亲眼看到某个 (zone,node) 从 etcd 注册表消失的时刻
// (watch DELETE、watch PUT 里同一个键换了身份,或重同步时新旧快照按身份算的差集)。
//
// 为什么 Redis 里已经有 death_at 还要再记一份进程本地的:death_at 只有 leader 写。
// leader 缺位的窗口里节点丢了租约,没有人写 death_at,而「没有 death_at」在
// CanReclaimDeadNode 里的语义是放行 —— 跟随者若据此接管玩家,正好撞上老进程 15s 的
// 紧急疏散存盘(PUT 分支提前 clearNodeDeath 也是同一类缺口;markNodeDeath 的 Setex 失败
// 已不再摘负载集,见 retryDeferredNodeDetaches)。
// 本地这一份不依赖任何人写 Redis:只要本副本看到了它消失,屏障就从那一刻起算。
// 本副本没看到(进程在那之后才启动)时没有记录,退回只看 Redis 的 death_at。
var (
	nodeGoneObservedMu sync.Mutex
	nodeGoneObservedAt = make(map[string]time.Time)
)

func nodeGoneObservedKey(zoneID uint32, nodeID string) string {
	return strconv.FormatUint(uint64(zoneID), 10) + "/" + nodeID
}

// noteNodeGoneFromRegistry 在本副本观察到节点从注册表消失时调用(每个副本都调,不走 leader 闸门)。
// 同一身份重复观察保留**较早**的时刻不合适 —— 节点可能重新注册后再次死亡,屏障必须从最近
// 一次消失起算,所以直接覆盖。顺手清掉早已过期的旧记录,表不会无限增长。
func noteNodeGoneFromRegistry(zoneID uint32, nodeID string) {
	if nodeID == "" {
		return
	}
	now := time.Now()
	nodeGoneObservedMu.Lock()
	defer nodeGoneObservedMu.Unlock()
	for key, at := range nodeGoneObservedAt {
		if now.Sub(at) > constants.NodeDeathMarkTTL {
			delete(nodeGoneObservedAt, key)
		}
	}
	nodeGoneObservedAt[nodeGoneObservedKey(zoneID, nodeID)] = now
}

// localGoneBarrierBlocks:本副本看到该节点消失还不到一个再入屏障时长。没有本地记录返回 false
// (由 Redis 的 death_at 判定)。墙钟回拨导致的负时长按「刚消失」处理,方向是 fail-closed。
func localGoneBarrierBlocks(svcCtx *svc.ServiceContext, zoneID uint32, nodeID string) bool {
	nodeGoneObservedMu.Lock()
	at, ok := nodeGoneObservedAt[nodeGoneObservedKey(zoneID, nodeID)]
	nodeGoneObservedMu.Unlock()
	if !ok {
		return false
	}
	elapsed := time.Since(at)
	return elapsed < 0 || elapsed < sceneReentryBarrier(svcCtx)
}

// isNodeGoneFromRegistry 报告 (zone,node) 是否已经确认**不在 etcd 注册表里**。
//
// 这是比「不在 Redis 负载集里」强得多的死亡证据:负载集成员资格由 leader 周期刷新,
// 缺席可能只是「这一刻没看到它」(刷新间隔、leader 缺位、别的路径短暂改写),而且那些路径
// 未必写 death_at —— 而「没有 death_at」在 CanReclaimDeadNode 里的语义是放行。
// etcd 注册则只在租约到期或进程主动注销时消失,那时 removeNodeFromRedis / 周期巡检会先写
// death_at 再摘负载集,再入屏障才有意义;death_at 写不进时推迟摘除,满一个屏障才不带标记摘
// (GO-6)。所以「已从注册表消失」只是死亡证据,改派还要看负载集(world_init 两处已补)。
// 用例:world_init 的改派前提也复用本函数(ba2337b0d),口径一致。
//
// knownNodes 每个副本都在维护(watch 的 PUT / DELETE 先改内存表,之后才判 leader),
// 所以非 leader 副本上同样可用。尚未完成首次全量同步时返回 false(拿不到证据)。
func isNodeGoneFromRegistry(zoneID uint32, nodeID string) bool {
	if !knownNodesSynced.Load() {
		return false
	}
	return knownNodeIdentityMatchCount(zoneID, nodeID) == 0
}

// isZoneGoneFromRegistry 报告 zone 是否已经确认**在 etcd 注册表里没有任何场景节点**:整 zone 下线(合服后源区
// 永久下线)的正面证据,EnterScene 的陈旧位置判定(playerLocationOwnerGone)靠它。
//
// 与 isNodeGoneFromRegistry 同源、同前提:尚未完成首次全量同步时返回 false(拿不到证据)。「zone 负载集为空」
// 替代不了它:负载键丢失 / 被淘汰时 ZCARD 读作 0,节点却都活着、都还在注册表里(第二轮审计 r2-sm#3 同源缺陷)。
// 依赖 knownNodes 是**全服**注册表的镜像(watch 前缀不分 zone)。将来把 watch 收窄到本 zone(容量模型 A1)时,
// 别的 zone 在这里恒为「没有节点」,必须先改成按 zone 查注册表,否则跨 zone 的陈旧位置会被一律判成已下线。
func isZoneGoneFromRegistry(zoneID uint32) bool {
	if !knownNodesSynced.Load() {
		return false
	}
	knownNodesMu.RLock()
	defer knownNodesMu.RUnlock()
	for _, entry := range knownNodes {
		if entry.reg.ZoneId == zoneID {
			return false
		}
	}
	return true
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

// updateNodeLoad refreshes one node's composite load score in the zone sorted
// set and mirrors its scene_node_type so downstream purpose-based selection
// doesn't need to hit etcd. Score = α·scene_count + β·player_count; see Config
// weights. Called on watch PUT; the periodic path is refreshLoadScores.
//
// 只有领导者做,走与每拍刷新同一条批量路径(load_refresh.go refreshZoneLoadSets,一个 zone、一个节点),
// 读失败语义也相同:保留旧分 / 不在负载集里的以哨兵分加入,绝不按 0 写。跟随者直接返回:不读、不写、
// 不发每节点指标。不记 load_refresh_total —— 那是「每拍刷新还活着」的信号(告警
// SceneManagerLoadRefreshStalled),PUT 只在节点注册时发生,混进去会掩盖停摆。
//
// Redis 写只归领导者(单写者):这不是幂等同值写 —— 跟随者的 watch 镜像可能落后
// 于领导者的清理,比如领导者刚把死节点 Zrem 出负载集,跟随者的 5s 刷新
// 又把它 Zadd 回去;而 DELETE 事件领导者已经消费过,没有第二次删除,
// "复活"的条目会一直留到下一次 fullSync。
// 跟随者以前仍然每拍逐个读全服节点的计数、只为发每节点指标(2026-10-16 起不再读):开服规模下那是
// 随 zone 数 × 节点数增长的白读(容量模型 §2.2)。每节点指标改为只由领导者导出,切主时序列从一个 pod
// 换到另一个 pod;跟随者只发布本 zone 的 nodes_by_role(见 refreshLoadScores)。
func updateNodeLoad(svcCtx *svc.ServiceContext, entry nodeEntry) {
	if !isLeader() {
		return
	}
	refreshZoneLoadSets(svcCtx, map[uint32][]loadRefreshNode{
		entry.reg.ZoneId: {{
			zoneID: entry.reg.ZoneId,
			nodeID: entry.nodeID,
			role:   entry.reg.SceneNodeType,
		}},
	})
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
// 不用于负载分刷新:那里「读失败」与「计数为 0」必须分开(load_refresh.go classifyCounterReply)。
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
	//
	// 写不进就不摘(GO-6):以前 Setex 失败只打一条日志就照样 Zrem,等于亲手造出上面那个
	// 「不在负载集 + 没有 death_at」的组合。现在写失败 → 节点留在负载集(按存活处理)进推迟
	// 队列,由 retryDeferredNodeDetaches 每拍补写,补不上则满一个屏障后才不带标记摘除;
	// death_at 已写、摘负载集失败同样进队列,不再等下一次 fullSync。见 reentry_barrier.go
	// 「推迟的摘除」一段。
	if err := markNodeDeath(svcCtx, entry.reg.ZoneId, entry.nodeID); err != nil {
		deferNodeDetach(svcCtx, entry, err)
		return
	}
	// 已有推迟任务(fullSync 重扫到它时这次写成了):沿用首次推迟时抄的判死时刻快照。
	// 没有任务时 task 是零值(scenes=nil、scenesCaptured=false),detach 退回现读快照,与旧行为一致。
	task, hadTask := lookupDeferredNodeDetach(entry.reg.ZoneId, entry.nodeID)
	if err := detachDeadNode(svcCtx, entry, task.scenes, task.scenesCaptured); err != nil {
		// death_at 已写,摘负载集失败:节点留在负载集按存活处理,进推迟队列每拍重试,不再等 fullSync。
		deferNodeDetach(svcCtx, entry, err)
		return
	}
	if hadTask && forgetDeferredNodeDetach(entry.reg.ZoneId, entry.nodeID) {
		metrics.ObserveNodeDetachDeferred(entry.reg.ZoneId, metrics.NodeDetachOutcomeRecovered)
	}
}

// detachDeadNode 把一个已判死的节点摘出调度面:负载集、类型镜像、gRPC 连接缓存、指标,
// 然后把它名下场景的收尾交给屏障队列。
//
// 调用前提:death_at 已落地,或推迟已满一个屏障(G6-I1,见 reentry_barrier.go「推迟的摘除」)。
// **不得单独调用**:绕过 markNodeDeath 直接摘,就是 GO-6 修掉的那个洞。
//
// 第一步 Zrem 失败时什么都不做就返回错误:不删类型镜像 —— node_selection.go getNodesForPurpose
// 把「没有类型镜像」的节点当作未分类、放进所有用途的池子,节点还在负载集里时删掉镜像反而让
// 死节点进了更多的池;也不入收尾队列 —— 节点还在负载集里、按存活处理,收尾要等它真正被摘除时
// 由推迟任务带着判死时刻快照一次交出(或节点重新注册时交出),这里先入队只会重复。
// 调用方把节点放进推迟队列重试。
func detachDeadNode(svcCtx *svc.ServiceContext, entry nodeEntry, scenes []string, scenesCaptured bool) error {
	zoneID := entry.reg.ZoneId
	if _, err := svcCtx.Redis.Zrem(nodeLoadKey(zoneID), entry.nodeID); err != nil {
		return fmt.Errorf("摘负载集失败: zone=%d node=%s: %w", zoneID, entry.nodeID, err)
	}
	if _, err := svcCtx.Redis.Del(nodeSceneNodeTypeKey(zoneID, entry.nodeID)); err != nil {
		// 节点已不在负载集,残留的类型镜像不参与选点(getNodesForPurpose 只遍历负载集),
		// 重新注册时 updateNodeLoad 会覆盖它。记一笔即可。
		logx.Errorf("[LoadReporter] failed to delete scene_node_type mirror for dead node %s (zone %d): %v",
			entry.nodeID, zoneID, err)
	}
	RemoveNodeConn(zoneID, entry.nodeID)
	metrics.ForgetNode(entry.nodeID, zoneID, entry.reg.SceneNodeType)
	logx.Infof("[LoadReporter] removed node %s from Redis load set (zone %d)", entry.nodeID, zoneID)

	if scenesCaptured {
		enqueueDeadNodeReconcileSnapshot(entry, scenes)
	} else {
		enqueueDeadNodeReconcile(svcCtx, entry)
	}
	return nil
}

// deleteNodeCounters 清掉一个已判死节点的负载计数键。
//
// 这两个键全仓只有 Incr/Decr,没有任何"节点重新上线时归零"的路径 ——
// 旧注释声称"下一个复用 id 的节点会覆盖它们"是不成立的:死节点残留的正计数
// 会被复用同 id 的新节点原样继承,负载分永久虚高,调度一直躲着它。
//
// ⚠️ 调用顺序:必须在 reconcileDeadNodeScenes **之后**。reconcile 里的
// destroyOrphanOfDeadNode 认领成功时还会对死节点的这两个键做减法(脚本比对期望属主,
// 对死节点计数的减法只来自这里;级联到别的节点上的镜像时扣的是镜像属主的计数,与这里的
// 顺序无关),先删会被它们重新建出来。
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
// 「仍指向本节点」以销毁脚本里的比较为准(destroyOrphanOfDeadNode,2026-10-22):循环里那次 GET
// 只是快速过滤。GET 之后、销毁之前,只要有写者用 reassignSceneNode 把实例改派到活节点并 CreateScene,
// 以前不比对属主的 force 脚本就会把新属主的场景抹掉、向新节点发 DestroyScene、扣新节点的计数。现在
// 脚本发现属主已不是死节点就一个键都不动,记进 stale。(修复时这样的写者是 resolveScene 的死节点自愈;
// 10-23 起它只重建世界频道,比对作为纵深防御保留。)
//
// 结果分四类:
//   - destroyed:认领并销毁;
//   - stale:属主已指向别的节点,或映射在 Zscore 之后、脚本之前消失(并发销毁,或
//     createscenelogic.go rollbackInstanceAllocation 先 DEL 映射、后 ZREM 的那几个往返里)。映射消失时
//     不当作可认领:删映射的那条路径负责其余清理,这里再抹会与它双还 Agones 名额;
//   - world_skipped:不在 active 集合里。除世界频道外,还包括在本次 GET 之前就已被销毁的实例,例如同一轮
//     里先被源场景级联抹掉的共置镜像 —— 所有销毁脚本都在同一次 EVAL 里 DEL 映射并 ZREM;
//   - failed:Redis 出错,结论未知。failed 的成员留在反向索引里不摘:它的 scene:{id}:node 多半仍指向
//     死节点,摘掉就让索引与属主映射不一致,同一 node_id 下次判死时也抄不进快照。
//
// 限制(修复前即有,见 docs/PROGRESS.md 2026-10-23「死节点收尾与显式 DestroyScene 的销毁改为按认领时属主」
// 条目残余 R1):收尾队列在进程内,drain 跑完一次不论结果都出队。failed 成员没有重试;本函数中途降级时的
// 剩余成员也不会被新领导者接手(新领导者的 fullSync 只扫负载集,扫不到已摘出负载集的死节点)。它们只能等
// 同一 node_id 下次判死时重新抄进快照,或者等实例人数经 LeaveScene(player_locator 租约到期)或玩家重新进场
// 扣到 0、过了空闲超时后由空闲回收销毁。
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
	var failedMembers []string

	for _, s := range members {
		// 降级即停,且**不删** setKey:剩余成员原样留在集合里。注意新领导者接不到它们(见函数注释「限制」)。
		// destroyOrphanOfDeadNode 内部是 Lua 原子认领,即便与新领导者短暂并发,每个场景的副作用也只会执行一次。
		if !isLeader() {
			logx.Infof("[Reconcile] lost leadership mid-sweep for node %s (zone %d), aborting; remaining members stay in %s",
				entry.nodeID, entry.reg.ZoneId, setKey)
			return
		}
		sceneId, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			continue
		}

		// Did this scene already get reassigned to a live node? 快速过滤;
		// 权威比较在下面的销毁脚本里(见函数注释)。
		currentNode, _ := svcCtx.Redis.Get(sceneNodeKey(sceneId))
		if currentNode != "" && currentNode != entry.nodeID {
			stale++
			continue
		}

		// Is this an instance? Only instances are tracked in the active set.
		// World channels live in world:channels:zone:{z}:conf:{c} and are
		// handled by the rebalance pipeline. 只有 redis.Nil(确实不在集合里)才算世界频道;
		// 读出错时结论未知,计 failed 并留在反向索引里,不能当成世界频道摘掉。
		_, zerr := svcCtx.Redis.Zscore(activeKey, s)
		if errors.Is(zerr, redis.Nil) {
			skippedWorld++
			continue
		}
		if zerr != nil {
			logx.Errorf("[Reconcile] Zscore %s for scene %d failed; left in %s, retried only if node %s is declared dead again: %v",
				activeKey, sceneId, setKey, entry.nodeID, zerr)
			failedMembers = append(failedMembers, s)
			continue
		}

		logx.Infof("[Reconcile] Force-destroying orphan instance %d (was on dead node %s, zone %d)",
			sceneId, entry.nodeID, zoneId)
		ok, err := destroyOrphanOfDeadNode(ctx, svcCtx, zoneId, sceneId, entry.nodeID)
		switch {
		case err != nil:
			// 已在 destroyOrphanOfDeadNode 里记了错误;不计入 destroyed,免得指标多报;留在反向索引里(见函数注释)。
			failedMembers = append(failedMembers, s)
		case !ok:
			// 脚本比对属主落空,一个键都没动:读属主之后被改派走了,或映射已被别的路径删掉。
			logx.Infof("[Reconcile] Scene %d not owned by dead node %s (zone %d) at destroy time "+
				"(reassigned after the owner read, or mapping already gone); skipped", sceneId, entry.nodeID, zoneId)
			stale++
		default:
			destroyed++
		}
	}

	// 只摘掉快照里的成员,不 Del 整个集合:屏障窗口里可能有新场景被建到同一个
	// node_id 上并加进了这个集合,Del 会把它们的反向索引一起抹掉,以后再没有
	// 任何路径能发现它们。集合被摘空后 Redis 会自动回收键。failed 的成员不摘。
	if values := membersExcept(members, failedMembers); len(values) > 0 {
		if _, err := svcCtx.Redis.Srem(setKey, values...); err != nil {
			logx.Errorf("[Reconcile] Srem %s failed: %v", setKey, err)
		}
	}

	metrics.ObserveSceneOrphansReconciled(zoneId, destroyed)

	failed := len(failedMembers)
	if failed > 0 {
		logx.Errorf("[Reconcile] Node %s (zone %d): total=%d destroyed=%d world_skipped=%d stale=%d failed=%d; "+
			"failed members left in %s (no retry scheduled; picked up only if this node_id is declared dead again)",
			entry.nodeID, zoneId, len(members), destroyed, skippedWorld, stale, failed, setKey)
	} else if destroyed > 0 || stale > 0 || skippedWorld > 0 {
		logx.Infof("[Reconcile] Node %s (zone %d): total=%d destroyed=%d world_skipped=%d stale=%d failed=0",
			entry.nodeID, zoneId, len(members), destroyed, skippedWorld, stale)
	}
}

// membersExcept 返回 members 中不在 keep 里的成员,按 Srem 的参数类型给出。
func membersExcept(members, keep []string) []any {
	skip := make(map[string]struct{}, len(keep))
	for _, k := range keep {
		skip[k] = struct{}{}
	}
	out := make([]any, 0, len(members))
	for _, m := range members {
		if _, ok := skip[m]; !ok {
			out = append(out, m)
		}
	}
	return out
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
	// 每个副本启动时登记自己的 ZoneId 进 scene_manager:zones(跟随者此后不再发 Redis 命令,见 zone_registry.go)。
	registerOwnZoneOnStartup(svcCtx)
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
		knownNodesSynced.Store(true)

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

	// 与每拍刷新同一条路径:领导者按 zone 批量刷新(并登记 zone,先于下面的清扫);跟随者只发本 zone 的
	// nodes_by_role,不碰 Redis。
	refreshLoadScores(svcCtx)

	// 每副本的进程本地清理(不走领导者闸门):watch DELETE 在跟随者上只做
	// 本地清理,但整段 DELETE 事件也可能被错过(watch 重建窗口)。用新旧
	// 快照的差集兜底,清掉已消失节点的 gRPC 连接缓存与 Prometheus 序列 ——
	// 这两样是进程私有的,领导者替代不了。
	// 差集按 (zone,node) 身份比,不按 etcd 键比:同一个键换了 zone 或 node_id 时,旧身份同样是此刻才被本副本看到消失
	// (与 watch PUT 的换身份分支同一口径);身份还挂在别的键上的,不算消失。
	for _, prev := range prevKnown {
		if _, still := seenByZone[prev.reg.ZoneId][prev.nodeID]; still {
			continue
		}
		RemoveNodeConn(prev.reg.ZoneId, prev.nodeID)
		metrics.ForgetNode(prev.nodeID, prev.reg.ZoneId, prev.reg.SceneNodeType)
		// watch 重建窗口里漏掉的 DELETE / 换身份:本副本是在**此刻**才知道它没了。
		noteNodeGoneFromRegistry(prev.reg.ZoneId, prev.nodeID)
	}
	// 实例级的同一件事(GO-3):同号新进程顶上来时 (zone,node) 身份还在,消失的是旧进程实例。
	seenInstances := make(map[string]struct{}, len(next))
	for _, entry := range next {
		if entry.reg.NodeUuid != "" {
			seenInstances[entry.reg.NodeUuid] = struct{}{}
		}
	}
	for _, prev := range prevKnown {
		if prev.reg.NodeUuid == "" {
			continue
		}
		if _, still := seenInstances[prev.reg.NodeUuid]; !still {
			noteInstanceGoneFromRegistry(prev.reg.NodeUuid)
		}
	}
	// 在册的实例作废旧的消失记录;仍不在册的已记录实例改记为此刻(断档期间可能「回来又消失」,保守后推一个屏障)。
	reconcileInstanceGoneObservationsAfterResync(seenInstances)

	// Update activeZones.
	zones := make([]uint32, 0, len(seenByZone))
	for z := range seenByZone {
		zones = append(zones, z)
	}
	activeZonesMu.Lock()
	activeZones = zones
	activeZonesMu.Unlock()

	// 以下全部是**变更类**动作,多副本时只有领导者执行(单写者语义);
	// 跟随者的 fullSync 只负责重建本进程的 etcd 内存镜像(负载分也只由领导者刷新)。
	// 领导者缺位窗口(选举间隙 / 领导者刚挂)里漏掉的动作,由新任领导者
	// 当选时触发的 RequestLoadReporterResync -> fullSync 一次补齐。
	if isLeader() {
		// 中途失去领导权:降级即停,新领导者的 resync fullSync 会重扫同一批(与旧写法一致,
		// 后面的 drain / init / rebalance / 孤儿清理本轮都不做)。
		if !sweepStaleLoadSetMembers(svcCtx, seenByZone, zones) {
			return resp.Header.Revision, nil
		}

		// 清扫不判截止(写不进只续推迟);先重试推迟的摘除(GO-6)再 drain,本次写成的节点同一拍
		// 入收尾队列(屏障照样压着)。
		retryDeferredNodeDetaches(svcCtx)

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

// sweepStaleLoadSetMembers 清掉 Redis 负载集里、但不在本次 etcd 快照里的节点(只在领导者上调用)。
// seenByZone 是快照里每个 zone 的节点集合,snapshotZones 是快照里出现过的 zone。
// 中途失去领导权返回 false,调用方应停止本轮后续的变更动作;扫完返回 true。
//
// 扫描范围 = 快照里的 zone ∪ Redis 里还留有负载集的 zone。只按快照扫有个洞:某 zone 的
// **最后一个**节点在无领导窗口(或 SM 整体停机)里死掉时,快照里根本没有这个 zone,它的
// 负载集条目、计数和孤儿实例就永远没人清(runPeriodicRebalance 也只迭代 GetActiveZones)。
// 「还留有负载集的 zone」以前靠 SCAN scene_nodes:zone:*:load 发现,全库遍历在开服规模下是小时级;
// 2026-10-16 起改读 zone 登记集合 scene_manager:zones(zone_registry.go):领导者刷新时先登记再写
// 负载集,有活节点的 zone 每拍自愈。读登记集合失败时只扫快照里的 zone(打 Error),与旧 SCAN 出错即
// break 的退化方向一致。覆盖不到的只有 zone_registry.go 文件头列出的两类缺口。
//
// 每个陈旧成员与 watch DELETE 走同一条摘除路径 removeNodeFromRedis:先写 death_at 再摘负载集,
// 写不进就推迟(GO-6)。已在推迟队列里的节点被重扫到时:这次写成 death_at 就按首次推迟时的
// 快照摘除(记 recovered),写不进(或写成但摘负载集失败)就续推迟(沿用首次时刻与快照);
// 清扫从不判截止,也从不不带标记摘,截止时间只由 retryDeferredNodeDetaches 判。
// 与旧写法的差异:这条路径现在会调
// ForgetNode(nodeID, zone, 0) —— 本进程不认识这个节点,删不存在的序列是 no-op;同身份的活节点
// 也不在清扫范围里。
func sweepStaleLoadSetMembers(svcCtx *svc.ServiceContext, seenByZone map[uint32]map[string]struct{}, snapshotZones []uint32) bool {
	registered, err := listRegisteredZones(svcCtx)
	if err != nil {
		logx.Errorf("[LoadReporter] read zone registry failed; sweeping snapshot zones only: %v", err)
	}

	for _, zoneId := range unionZones(snapshotZones, registered) {
		// seen 对快照外的 zone 是 nil map —— 该 zone 所有条目都视为 stale。
		seen := seenByZone[zoneId]
		pairs, err := svcCtx.Redis.ZrangeWithScores(nodeLoadKey(zoneId), 0, -1)
		if err != nil {
			continue
		}
		for _, p := range pairs {
			// 长循环里领导权可能中途易主:降级即停,
			// 新领导者的 resync fullSync 会重扫同一批。
			if !isLeader() {
				return false
			}
			if _, ok := seen[p.Key]; ok {
				continue
			}
			// SceneManager 停机 / 领导者缺位窗口期间死掉的节点走不到 watch DELETE,这里是唯一
			// 能观察到它们消失的地方。死亡时刻必须补记:不记的话 CanReclaimDeadNode 读不到标记,
			// 会把它们当成「没死过」直接放行改派,而它们很可能是几秒前刚断的、C++ 侧还在 drain。
			// 我们不知道真实死亡时刻,只能用「首次观察到」这个偏晚的时刻 —— 方向是安全的
			// (屏障结束点跟着偏晚)。孤儿实例场景与计数残留也只有这条路径能补收(这也是新任
			// 领导者补齐跟随期间漏掉的 DELETE 事件的唯一路径):与 watch DELETE 同构,入队等
			// 再入屏障走完,由 drainPendingDeadNodeReconciles 收尾;计数由它在 reconcile **之后**清
			// (顺序理由见 deleteNodeCounters)。
			staleEntry := nodeEntry{nodeID: p.Key}
			staleEntry.reg.ZoneId = zoneId
			removeNodeFromRedis(svcCtx, staleEntry)
		}
	}
	return true
}

// watchAndRefresh starts an etcd Watch from the given revision, a periodic
// ticker for load-score refresh and grace-period expiry checks, and a
// longer-period ticker for best-effort rebalance convergence. The rebalance
// tick catches drift that events alone can miss (hot channels that drained
// after the last join/leave event) and acts as a self-healing safety net
// when etcd watch misses an update during a network blip.
func watchAndRefresh(ctx context.Context, svcCtx *svc.ServiceContext, rev int64) {
	// clientv3 要求 watch 不再使用时取消它的 ctx。本函数会因 resync / watch 出错 / 通道关闭返回,若直接拿长寿命的服务 ctx
	// 建 watch,每返回一次就留下一条 watch 子流(客户端一条 goroutine 加无界缓冲,服务端一个 watcher)。用随本函数返回而
	// 取消的子 ctx 建;handleWatchEvent 里的补频道 / rebalance 仍用服务 ctx,不受影响。
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	watchCh := svcCtx.Etcd.Watch(watchCtx, sceneNodePrefix,
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
			// death_at 写不进 / 摘负载集失败而推迟的摘除(GO-6):在 drain 之前重试,
			// 本拍摘成的节点同一拍入收尾队列(屏障照样压着)。
			retryDeferredNodeDetaches(svcCtx)
			// 被再入屏障压着的死节点收尾:屏障(默认 20s)远大于一拍(5s),
			// 所以「等屏障」就是「多等几拍」,不需要每个死节点起一条 goroutine。
			drainPendingDeadNodeReconciles(ctx, svcCtx)
		case <-rebalanceTicker:
			runPeriodicRebalance(ctx, svcCtx)
		case <-loadReporterResync:
			// 领导权刚变化:返回让外层循环重跑 fullSync。新任领导者借此
			// 立即补齐跟随者期间跳过的变更动作(补频道 / rebalance / 清理),
			// 而不是等下一次 watch 中断。另一个来源是推迟摘除放弃时(retryDeferredNodeDetaches 的
			// abandoned 分支),要 fullSync 的陈旧清扫把还留在负载集里的死节点重新推迟。
			logx.Info("[LoadReporter] resync requested (leadership change or abandoned deferred detach)")
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
		// 这个实例(重新)在册:作废它旧的消失记录(GO-3,owner_replaced.go)。
		forgetInstanceGoneObservation(entry.reg.NodeUuid)

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
					"(same key %s). Likely a deploy mix-up; the old zone identity is retired like a DELETE "+
					"(death_at + reentry barrier) unless another key still registers it.",
					entry.nodeID, prev.reg.ZoneId, entry.reg.ZoneId, key)
			}
		}

		// 节点(重新)注册 = 它此刻是活的,上一次的死亡标记必须抹掉,否则
		// IsNodeAlive 说活、再入屏障却还压着它名下的场景,两个判定自相矛盾。
		clearNodeDeath(svcCtx, entry.reg.ZoneId, entry.nodeID)
		// 同理取消上一次死亡的推迟摘除(GO-6;只有领导者的队列里可能有它)。不取消的话
		// 「重新注册后再死」会沿用上一次死亡的 firstObservedAt,截止时间提前。
		cancelDeferredNodeDetachOnReregister(entry.reg.ZoneId, entry.nodeID)

		// Clear stale gRPC connection: endpoint may have changed after restart.
		RemoveNodeConn(entry.reg.ZoneId, entry.nodeID)
		// 同一个 etcd 键换了身份(zone 或 node_id 变了):旧的 (zone,node) 身份从注册表消失,按 watch DELETE 同样收尾。
		// 以前换 zone 时在每个副本上直接 Zrem 旧 zone 负载集、删类型镜像:不写 death_at、不记本地消失时刻、
		// 不走领导者闸门,也不收孤儿实例。它若是旧 zone 唯一的节点,isZoneGoneFromRegistry(旧 zone) 随即为真,
		// playerLocationOwnerGone / playerLocationOwnerDead 就会不经再入屏障放行指向它的陈旧位置,而老进程
		// 可能还在存盘(cross-zone-scene-travel.md §13.9「handleWatchEvent 换 zone」一条,2026-10-22 落码)。
		identityChanged := existed && (prev.reg.ZoneId != entry.reg.ZoneId || prev.nodeID != entry.nodeID)
		if identityChanged {
			retireNodeIdentityIfGone(ctx, svcCtx, prev)
		}
		// 同一个键换了进程实例(重启后的新进程覆盖了旧注册):旧实例从注册表消失,记下时刻(GO-3)。
		if existed && prev.reg.NodeUuid != entry.reg.NodeUuid {
			noteInstanceRetiredIfGone(prev)
		}

		updateNodeLoad(svcCtx, entry)
		rebuildActiveZones()

		// 补频道 / rebalance 是变更动作,只有领导者做;跟随者只维护上面的
		// 内存镜像(updateNodeLoad 在跟随者上直接返回)。跟随者当选时会经 resync -> fullSync 补齐。
		if wids := worldConfIds(); len(wids) > 0 && isLeader() {
			// 换了身份时,对新身份所在的 zone 来说它同样是"新出现的节点"。
			if !existed || identityChanged {
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
			if constants.IsWorldHostingType(entry.reg.SceneNodeType) && (!existed || roleChanged || identityChanged) {
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
		retireNodeIdentityIfGone(ctx, svcCtx, entry)
		noteInstanceRetiredIfGone(entry)
	}
}

// retireNodeIdentityIfGone:某个 etcd 键不再代表 gone 这个 (zone,node) 身份时调用(watch DELETE,或 PUT 换了身份)。
// 身份已经没有任何键在册,才按死亡收尾(retireNodeIdentity)。同一身份还挂在别的键上(双代进程并存、node_id 被复用,
// 选点与判死都会因身份歧义拒绝它)时,它没有从注册表消失:不判死、不写 death_at,只丢掉可能已过期的连接缓存;
// 等最后一个键也消失时再收尾。方向是 fail-closed:宁可晚判死,不对还在册的身份写死亡标记、摘负载集。
// 调用前 knownNodes 必须已经反映这次变更(DELETE 已删键、PUT 已写入新值)。
func retireNodeIdentityIfGone(ctx context.Context, svcCtx *svc.ServiceContext, gone nodeEntry) {
	if knownNodeIdentityMatchCount(gone.reg.ZoneId, gone.nodeID) > 0 {
		RemoveNodeConn(gone.reg.ZoneId, gone.nodeID)
		return
	}
	retireNodeIdentity(ctx, svcCtx, gone)
}

// retireNodeIdentity 把一个已从 etcd 注册表消失的 (zone,node) 身份按「死亡」收尾。只经 retireNodeIdentityIfGone 调用,
// 前提是该身份已没有任何键在册。
//
// 每个副本都做(在领导者闸门之前):
//   - 记下本副本看到它消失的时刻,屏障从这一刻起算,不依赖领导者写的 death_at(见 nodeGoneObservedAt);
//   - 清进程私有的 gRPC 连接缓存与 Prometheus 标签,不清会拿着死节点的 stale endpoint 继续发请求。
//
// 只有领导者做:
//   - Redis 侧清理(负载集摘除 / 孤儿实例销毁 / 计数删除)。走 removeNodeFromRedis:先写 death_at 再摘负载集,
//     写不进就推迟(GO-6)。领导者缺位窗口里漏掉的,由新任领导者当选时的 fullSync 补齐(其 stale 清理路径
//     会跑同一套 reconcile)。
//   - 世界频道宿主离开时立刻对该 zone rebalance,让映射到它的频道在下一次 EnterScene 就有新家,
//     而不是等下一个 LoadReportInterval。
func retireNodeIdentity(ctx context.Context, svcCtx *svc.ServiceContext, gone nodeEntry) {
	noteNodeGoneFromRegistry(gone.reg.ZoneId, gone.nodeID)
	RemoveNodeConn(gone.reg.ZoneId, gone.nodeID)
	metrics.ForgetNode(gone.nodeID, gone.reg.ZoneId, gone.reg.SceneNodeType)

	if !isLeader() {
		return
	}
	removeNodeFromRedis(svcCtx, gone)

	if constants.IsWorldHostingType(gone.reg.SceneNodeType) {
		if wids := worldConfIds(); len(wids) > 0 {
			RebalanceWorldChannelsForZone(ctx, svcCtx, gone.reg.ZoneId, wids)
		}
	}
}

// refreshLoadScores updates Redis load scores for all currently known nodes
// and refreshes the nodes_by_role metric so dashboards reflect the current
// cluster topology even when individual node scores don't move.
// Called on every LoadReportInterval tick and from fullSync.
//
// 领导者:发布 etcd 镜像里全部 zone 的 nodes_by_role,再按 zone 批量刷新全部节点的负载分
// (load_refresh.go refreshZoneLoadSets),记一次 load_refresh_total 与耗时。
// 跟随者:不发任何 Redis 命令;nodes_by_role 只发布本进程 ZoneId,每节点序列每拍清一次 —— 降级那一刻
// OnStateChange 的 ResetLeaderGauges 与正在跑的刷新在不同 goroutine 上,刷新可能在 Reset 之后又写进
// 一批,跟随者的下一拍把它清掉。
//
// nodes_by_role 先于 Redis I/O 发布:它只来自 etcd 快照,不该被 Redis 慢 / 熔断拖住。
// 两种身份都给本进程配置的 ZoneId 补 0:scene_manager 按 c.ZoneId 注册,该 zone 的
// scene 节点正是靠这个注册找到它,所以「服务 zone Z 的 SM 期待 Z 里有 scene 节点」是现成的事实。
// 不补的话整 zone 全灭时该 zone 一条序列都不剩,PoolEmpty 告警无从触发(见 SetNodesByRole)。
func refreshLoadScores(svcCtx *svc.ServiceContext) {
	byZone, counts := snapshotLoadRefreshNodes()
	ownZone := svcCtx.Config.ZoneId

	if !isLeader() {
		own := make(map[nodesByRoleKey]int)
		for k, v := range counts {
			if k.ZoneID == ownZone {
				own[k] = v
			}
		}
		metrics.SetNodesByRole(own, []uint32{ownZone})
		metrics.ResetNodeSeries()
		return
	}

	metrics.SetNodesByRole(counts, []uint32{ownZone})
	start := time.Now()
	report := refreshZoneLoadSets(svcCtx, byZone)
	metrics.ObserveLoadRefresh(report.outcome(), time.Since(start), report.nodeReadErrors)
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
	logx.Errorf("[LoadReporter] IsNodeAlive state unknown; treating as alive to avoid incorrect reassignment: zone=%d node=%s err=%v",
		zoneId, nodeId, err)
	return true
}
