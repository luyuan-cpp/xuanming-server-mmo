package logic

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// Redis key for the active-instance sorted set (score = create timestamp).
const (
	ActiveInstancesKeyFmt  = "instances:zone:%d:active"
	InstancePlayerCountKey = "instance:%d:player_count"
	SceneNodeKeyFmt        = "scene:%d:node"
	SceneZoneKeyFmt        = "scene:%d:zone"
	// SceneMirrorFlagKeyFmt marks a scene as a mirror so the lifecycle
	// manager can apply MirrorIdleTimeoutSeconds instead of the standard
	// InstanceIdleTimeoutSeconds. Value is "1" when present, key absent
	// for non-mirror scenes.
	SceneMirrorFlagKeyFmt = "scene:%d:mirror"
	// SceneSourceKeyFmt stores the source scene id of a mirror so we can
	// reverse-look up which source's `mirrors` set this scene is a member of
	// when destroying the mirror. Only present when the mirror was created
	// with source_scene_id > 0.
	SceneSourceKeyFmt = "scene:%d:source"
	// SceneMirrorsKeyFmt is the Redis SET of mirror scene ids whose source
	// is the given scene id. Populated at mirror creation, consumed by the
	// cascade-destroy path when a scene with mirrors is removed.
	SceneMirrorsKeyFmt = "scene:%d:mirrors"
	// NodeScenesKeyFmt is the Redis SET of scene ids currently hosted by a
	// scene node. Populated by allocateScene, drained by every destroy path.
	// Used by the node-death reconciliation loop to find orphan instances.
	NodeScenesKeyFmt = "node:zone:%d:%s:scenes"
)

func sceneMirrorFlagKey(sceneID uint64) string {
	return fmt.Sprintf(SceneMirrorFlagKeyFmt, sceneID)
}

func sceneSourceKey(sceneID uint64) string {
	return fmt.Sprintf(SceneSourceKeyFmt, sceneID)
}

func sceneMirrorsKey(sourceSceneID uint64) string {
	return fmt.Sprintf(SceneMirrorsKeyFmt, sourceSceneID)
}

func nodeScenesKey(zoneID uint32, nodeID string) string {
	return fmt.Sprintf(NodeScenesKeyFmt, zoneID, nodeID)
}

func sceneZoneKey(sceneID uint64) string {
	return fmt.Sprintf(SceneZoneKeyFmt, sceneID)
}

func activeInstancesKey(zoneID uint32) string {
	return fmt.Sprintf(ActiveInstancesKeyFmt, zoneID)
}

func sceneNodeKey(sceneID uint64) string {
	return fmt.Sprintf(SceneNodeKeyFmt, sceneID)
}

type CreateSceneLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateSceneLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateSceneLogic {
	return &CreateSceneLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CreateScene routes to world or instance creation based on scene type.
// If scene_type is unset, it auto-detects from the World table.
func (l *CreateSceneLogic) CreateScene(in *scene_manager.CreateSceneRequest) (*scene_manager.CreateSceneResponse, error) {
	sceneType := l.resolveSceneType(in)

	switch sceneType {
	case constants.SceneTypeMainWorld:
		return l.createMainWorldScene(in)
	case constants.SceneTypeInstance:
		return l.createInstance(in)
	default:
		return &scene_manager.CreateSceneResponse{
			ErrorCode:    constants.ErrInvalidSceneType,
			ErrorMessage: "unknown scene type",
		}, nil
	}
}

// findExistingMirror picks one mirror id from scene:{sourceId}:mirrors. Used
// by the MirrorDedupBySource code path. Returns (0, false) when the set is
// empty or the read fails. The selection is arbitrary (Redis SET ordering is
// undefined), which matches the contract of "any existing mirror is fine".
func (l *CreateSceneLogic) findExistingMirror(sourceId uint64) (uint64, bool) {
	members, err := l.svcCtx.Redis.Smembers(sceneMirrorsKey(sourceId))
	if err != nil || len(members) == 0 {
		return 0, false
	}
	for _, m := range members {
		id, err := strconv.ParseUint(m, 10, 64)
		if err == nil && id > 0 {
			return id, true
		}
	}
	return 0, false
}

// resolveSceneType determines the scene type from the request or config.
func (l *CreateSceneLogic) resolveSceneType(in *scene_manager.CreateSceneRequest) uint32 {
	// Proto enum: 1 = main world, 2 = instance. 0 = auto-detect.
	if in.SceneType > 0 {
		return uint32(in.SceneType)
	}
	if IsWorldConf(in.SceneConfId) {
		return constants.SceneTypeMainWorld
	}
	return constants.SceneTypeInstance
}

// createMainWorldScene returns the least-loaded channel for a world scene.
// Channels are pre-created by initWorldScenesForZone at startup; this is a fallback
// that also creates channels on-demand if they somehow don't exist yet.
func (l *CreateSceneLogic) createMainWorldScene(in *scene_manager.CreateSceneRequest) (*scene_manager.CreateSceneResponse, error) {
	// Fast path: channels already exist — return the least-loaded one.
	sceneId, nodeId, _ := GetBestWorldChannel(l.ctx, l.svcCtx, in.SceneConfId, in.ZoneId)
	if sceneId > 0 {
		l.Logger.Infof("[MainWorld] Best channel: scene=%d conf=%d node=%s", sceneId, in.SceneConfId, nodeId)
		return &scene_manager.CreateSceneResponse{SceneId: sceneId, NodeId: nodeId}, nil
	}

	// Slow path: no channels exist -- create them now (startup race or missing init).
	l.Logger.Infof("[MainWorld] No channels for conf %d in zone %d, creating on demand", in.SceneConfId, in.ZoneId)
	initWorldScenesForZone(l.ctx, l.svcCtx, in.ZoneId, []uint64{in.SceneConfId})

	sceneId, nodeId, _ = GetBestWorldChannel(l.ctx, l.svcCtx, in.SceneConfId, in.ZoneId)
	if sceneId > 0 {
		return &scene_manager.CreateSceneResponse{SceneId: sceneId, NodeId: nodeId}, nil
	}

	return &scene_manager.CreateSceneResponse{
		ErrorCode:    constants.ErrNoAvailableNode,
		ErrorMessage: "failed to create main world channels",
	}, nil
}

// createInstance creates an on-demand instance with lifecycle tracking.
//
// Mirror co-location: when source_scene_id > 0 and the source node is alive
// and not over MirrorSourceNodeLoadCap, the mirror is created on the SAME
// node as its source scene. This reuses already-resident map/AI/spawn data
// and lets clients switch between source ↔ mirror without a cross-node
// handoff. Falls back to GetBestNode otherwise.
//
// Defensive checks performed before allocation:
//
//  1. "Source-clone" mirror — MirrorConfigId == 0 && SourceSceneId > 0 —
//     means the mirror inherits ALL state (map, NPC table, spawn data)
//     from the source. If the source is gone there is nothing to clone,
//     so we reject with ErrSourceSceneGone instead of silently producing
//     an unplayable scene. Mirrors with their own MirrorConfigId carry
//     their own definition and are fine even when the source disappears
//     (only co-location placement falls back).
//  2. MirrorDedupBySource enabled -> if any mirror already exists for the
//     given source, return that mirror's id instead of allocating a new
//     one. Off by default because the typical mirror use case is per-player
//     phasing where independent copies are intentional.
func (l *CreateSceneLogic) createInstance(in *scene_manager.CreateSceneRequest) (*scene_manager.CreateSceneResponse, error) {
	if in.SourceSceneId > 0 {
		// Only enforce source-existence when the source IS the template
		// (no MirrorConfigId provided). Otherwise the source is just a
		// co-location hint and pickInstanceNode handles the fallback.
		if in.MirrorConfigId == 0 {
			exists, err := l.svcCtx.Redis.Exists(sceneNodeKey(in.SourceSceneId))
			if err != nil {
				l.Logger.Errorf("[Instance] EXISTS scene:%d:node failed for source-clone mirror: %v", in.SourceSceneId, err)
			} else if !exists {
				l.Logger.Infof("[Instance] Refusing source-clone mirror: source scene %d is gone (caller=%v conf=%d)",
					in.SourceSceneId, in.CreatorIds, in.SceneConfId)
				metrics.ObserveMirrorSourceMissing(in.ZoneId)
				return &scene_manager.CreateSceneResponse{
					ErrorCode:    constants.ErrSourceSceneGone,
					ErrorMessage: fmt.Sprintf("source scene %d no longer exists", in.SourceSceneId),
				}, nil
			}
		}

		if l.svcCtx.Config.MirrorDedupBySource {
			if existingId, ok := l.findExistingMirror(in.SourceSceneId); ok {
				if nodeId, err := l.svcCtx.Redis.Get(sceneNodeKey(existingId)); err == nil && nodeId != "" {
					l.Logger.Infof("[Instance] Dedup: reusing mirror %d (source %d) on node %s",
						existingId, in.SourceSceneId, nodeId)
					metrics.ObserveMirrorDedup(in.ZoneId, "hit")
					return &scene_manager.CreateSceneResponse{
						SceneId:    existingId,
						NodeId:     nodeId,
						CreatorIds: in.CreatorIds,
					}, nil
				}
				// Stale entry in the mirrors set — the mirror was destroyed
				// but its membership wasn't cleaned. Drop it and fall through
				// to a fresh allocation.
				l.svcCtx.Redis.Srem(sceneMirrorsKey(in.SourceSceneId), strconv.FormatUint(existingId, 10))
				metrics.ObserveMirrorDedup(in.ZoneId, "stale")
			} else {
				metrics.ObserveMirrorDedup(in.ZoneId, "miss")
			}
		}
	}

	// 选节点。Agones 模式下这一步同时在 Agones 侧原子预占一个 rooms 名额,
	// 返回的 placement 带 GameServerName —— 后面所有回滚都靠它。
	targetNode, placement, err := l.resolveInstancePlacement(in)
	if err != nil {
		l.Logger.Errorf("[Instance] No instance-hosting node for zone %d (strict=%v agones=%v): %v",
			in.ZoneId, l.svcCtx.Config.StrictNodeTypeSeparation, AgonesEnabled(l.svcCtx), err)
		// Use ErrNoNodeForPurpose when strict mode rejected the request — it
		// tells operators the pool was empty by policy, not by outage.
		code := constants.ErrNoAvailableNode
		if l.svcCtx.Config.StrictNodeTypeSeparation {
			code = constants.ErrNoNodeForPurpose
		}
		return &scene_manager.CreateSceneResponse{ErrorCode: code, ErrorMessage: err.Error()}, nil
	}

	sceneId, err := l.allocateScene(in.SceneConfId, targetNode, in.ZoneId)
	if err != nil {
		// scene id 都没拿到,Redis 还没写任何东西,只需要把 Agones 名额还回去。
		if placement != nil {
			ReleaseAgonesRoom(l.ctx, l.svcCtx, placement.Namespace, placement.GameServerName, "allocate_scene_failed")
		}
		return &scene_manager.CreateSceneResponse{ErrorCode: constants.ErrRedis, ErrorMessage: err.Error()}, nil
	}

	// 记住这个 Scene 开在哪个 GameServer 上。必须在调 C++ CreateScene **之前**
	// 写:如果 RPC 失败后才写,进程正好在中间崩溃就永远找不回该减哪个计数。
	if placement != nil {
		if err := l.svcCtx.Redis.Set(sceneAgonesGsKey(sceneId), placement.GameServerName); err != nil {
			l.Logger.Errorf("[Instance] Failed to record agones gs for scene %d: %v", sceneId, err)
		}
	}

	// Track in active instances sorted set (score = creation timestamp).
	nowUnix := time.Now().Unix()
	instKey := activeInstancesKey(in.ZoneId)
	if _, err := l.svcCtx.Redis.Zadd(instKey, nowUnix, fmt.Sprintf("%d", sceneId)); err != nil {
		l.Logger.Errorf("[Instance] Failed to track instance %d: %v", sceneId, err)
	}

	// Initialize player count to 0.
	l.svcCtx.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, sceneId), "0")

	// Tag mirrors so the lifecycle manager can apply the shorter
	// MirrorIdleTimeoutSeconds. Mirrors re-init NPCs on every entry, so
	// lingering with 0 players is pure waste.
	if in.MirrorConfigId > 0 || in.SourceSceneId > 0 {
		if err := l.svcCtx.Redis.Set(sceneMirrorFlagKey(sceneId), "1"); err != nil {
			l.Logger.Errorf("[Instance] Failed to set mirror flag for scene %d: %v", sceneId, err)
		}
	}

	// Reverse indexes for cascade destroy and node-death reconciliation.
	// These use SET operations which are naturally idempotent — safe to
	// re-run on retry.
	if in.SourceSceneId > 0 {
		// scene:{sourceId}:mirrors gives us O(1) enumeration of "mirrors of
		// this scene" when the source is destroyed or the source node dies.
		if _, err := l.svcCtx.Redis.Sadd(sceneMirrorsKey(in.SourceSceneId), fmt.Sprintf("%d", sceneId)); err != nil {
			l.Logger.Errorf("[Instance] Failed to add mirror %d to source %d's mirror set: %v", sceneId, in.SourceSceneId, err)
		}
		// scene:{mirrorId}:source remembers the source so the mirror can
		// SREM itself from that set on destroy without a full table scan.
		l.svcCtx.Redis.Set(sceneSourceKey(sceneId), fmt.Sprintf("%d", in.SourceSceneId))
	}

	// Notify the C++ scene node to instantiate the ECS scene entity.
	//
	// 这一步失败必须回滚,不能像以前那样只打一条 "(Redis state committed)"
	// 然后照样返回成功 —— 那会留下一个 phantom scene:Redis 里有映射、
	// 节点上没有实体,玩家被路由进去后 EnterScene 永远成功不了。
	//
	// Agones 模式下同时把刚预占的 rooms 名额还回去,否则容量永久漂移。
	if _, err := RequestNodeCreateSceneWithOptions(
		l.ctx, l.svcCtx, in.ZoneId, targetNode,
		uint32(in.SceneConfId), sceneId,
		in.MirrorConfigId, in.CreatorIds,
	); err != nil {
		l.Logger.Errorf("[Instance] CreateScene RPC failed on node %s for instance %d: %v; rolling back",
			targetNode, sceneId, err)
		l.rollbackInstanceAllocation(sceneId, targetNode, in, placement)
		return &scene_manager.CreateSceneResponse{
			ErrorCode:    constants.ErrNoAvailableNode,
			ErrorMessage: fmt.Sprintf("scene node %s rejected CreateScene: %v", targetNode, err),
		}, nil
	}

	if in.MirrorConfigId > 0 || in.SourceSceneId > 0 {
		l.Logger.Infof("[Instance] Created mirror %d (conf=%d mirror_conf=%d source_scene=%d) on node %s",
			sceneId, in.SceneConfId, in.MirrorConfigId, in.SourceSceneId, targetNode)
	} else {
		l.Logger.Infof("[Instance] Created instance %d (conf=%d) on node %s", sceneId, in.SceneConfId, targetNode)
	}
	// Echo creator_ids so async callers (notably the C++ SceneNode driving a
	// mirror create on behalf of a player) can dispatch a follow-up EnterScene
	// without keeping per-call request state. Empty for system creates.
	return &scene_manager.CreateSceneResponse{
		SceneId:    sceneId,
		NodeId:     targetNode,
		CreatorIds: in.CreatorIds,
	}, nil
}

// resolveInstancePlacement 决定实例开在哪个节点上,并在 Agones 模式下
// 顺带预占一个房间名额。
//
// 两种模式的边界很关键:
//
//   - Agones 关闭:完全走老路(TargetNodeId / 镜像共置 / Redis 负载分数),
//     placement 返回 nil,后续所有 Agones 相关动作都是 no-op。
//
//   - Agones 打开:容量的权威是 Agones,不是 Redis 负载分数。
//     GSA 拿不到名额时**必须 fail-closed**,不允许"退回 Redis 选节点"——
//     那等于绕过容量约束,把房间塞进一个 Agones 认为已经满了的进程,
//     阶段 C 的整套计数会从此对不上。
//
// 镜像共置的例外:镜像必须和源 Scene 同进程(复用已驻留的地图/AI/spawn),
// 这条约束比"由 Agones 挑进程"更硬。所以 Agones 模式下镜像仍然按源节点
// 共置,但仍然要向那个**具体的** GameServer 预占名额 —— 见
// acquireRoomOnSpecificNode。共置失败时才回落到 GSA 自由选择。
func (l *CreateSceneLogic) resolveInstancePlacement(in *scene_manager.CreateSceneRequest) (string, *agonesPlacement, error) {
	if !AgonesEnabled(l.svcCtx) {
		node, err := l.pickInstanceNode(in)
		return node, nil, err
	}

	// 显式指定节点 / 镜像共置:目标节点是被业务规则钉死的,不能交给 GSA 挑。
	if pinned, reason := l.pinnedInstanceNode(in); pinned != "" {
		placement, err := l.acquireRoomOnSpecificNode(pinned, in.ZoneId)
		if err == nil {
			return pinned, placement, nil
		}
		// 钉住的节点没容量:镜像失去共置优化,但不能因此把玩家卡死。
		// 退回 GSA 自由选择,并把降级记在日志里(共置命中率指标已有)。
		l.Logger.Infof("[Agones] pinned node %s (%s) has no room capacity (%v), falling back to free allocation",
			pinned, reason, err)
	}

	placement, err := AcquireAgonesPlacement(l.ctx, l.svcCtx, in.ZoneId, constants.NodePurposeInstance)
	if err != nil {
		return "", nil, err
	}
	return placement.NodeID, placement, nil
}

// pinnedInstanceNode 返回被业务规则钉死的目标节点(显式 TargetNodeId 或
// 镜像共置的源节点)。没有钉死时返回 ""。
func (l *CreateSceneLogic) pinnedInstanceNode(in *scene_manager.CreateSceneRequest) (string, string) {
	if in.TargetNodeId != "" {
		return in.TargetNodeId, "explicit target"
	}
	if in.SourceSceneId > 0 {
		if node, reason := l.resolveMirrorSourceNode(in.SourceSceneId, in.ZoneId); node != "" {
			metrics.ObserveMirrorColocate(in.ZoneId, "hit", "ok")
			l.Logger.Infof("[Mirror] Co-locating mirror (conf=%d mirror_conf=%d) with source scene %d on node %s (agones)",
				in.SceneConfId, in.MirrorConfigId, in.SourceSceneId, node)
			return node, "mirror co-location"
		} else {
			metrics.ObserveMirrorColocate(in.ZoneId, "fallback", reason)
		}
	}
	return "", ""
}

// acquireRoomOnSpecificNode 在一个**指定**的 scene node 上预占房间名额。
//
// GameServerAllocation 无法指向某一个具体 GameServer,所以这里的做法是:
// 反查该 node 对应的 GameServer 名字(通过 PodIP),再直接对它的 rooms
// 计数做一次带 CAS 的 +1。反查不到就当作"这个节点不受 Agones 管理",
// fail-closed 交给调用方回落。
func (l *CreateSceneLogic) acquireRoomOnSpecificNode(nodeID string, zoneID uint32) (*agonesPlacement, error) {
	return AcquireAgonesRoomOnNode(l.ctx, l.svcCtx, nodeID, zoneID)
}

// rollbackInstanceAllocation 精确撤销一次失败的实例创建。
//
// 只撤销"本次未交付"的写:Redis 里这个 scene 的全部状态 + 节点计数 +
// 反向索引 + Agones 房间名额。不碰任何已经交付给玩家的东西。
//
// 顺序:先删 scene:{id}:node(其它路径判断"这个 scene 还在不在"看的就是它),
// 再清剩下的,最后还 Agones 名额。
func (l *CreateSceneLogic) rollbackInstanceAllocation(
	sceneId uint64,
	targetNode string,
	in *scene_manager.CreateSceneRequest,
	placement *agonesPlacement,
) {
	sceneIdStr := fmt.Sprintf("%d", sceneId)

	l.svcCtx.Redis.Del(sceneNodeKey(sceneId))
	l.svcCtx.Redis.Del(sceneZoneKey(sceneId))
	l.svcCtx.Redis.Del(fmt.Sprintf(InstancePlayerCountKey, sceneId))
	l.svcCtx.Redis.Del(sceneMirrorFlagKey(sceneId))
	l.svcCtx.Redis.Del(sceneSourceKey(sceneId))
	l.svcCtx.Redis.Del(sceneAgonesGsKey(sceneId))
	l.svcCtx.Redis.Zrem(activeInstancesKey(in.ZoneId), sceneIdStr)
	l.svcCtx.Redis.Srem(nodeScenesKey(in.ZoneId, targetNode), sceneIdStr)

	if in.SourceSceneId > 0 {
		l.svcCtx.Redis.Srem(sceneMirrorsKey(in.SourceSceneId), sceneIdStr)
	}

	// 节点 scene_count 是 allocateScene 加上去的,必须原样减回。
	if _, err := l.svcCtx.Redis.Decr(nodeSceneCountKey(in.ZoneId, targetNode)); err != nil {
		l.Logger.Errorf("[Instance] rollback: failed to decrement scene count for node %s: %v", targetNode, err)
	}

	if placement != nil {
		ReleaseAgonesRoom(l.ctx, l.svcCtx, placement.Namespace, placement.GameServerName, "create_rpc_failed")
	}

	l.Logger.Infof("[Instance] rolled back scene %d on node %s (no phantom scene left behind)", sceneId, targetNode)
}

// pickInstanceNode decides which scene node should host a new instance.
//
// Priority:
//  1. Explicit TargetNodeId from the caller (respects existing behaviour —
//     operators can always pin a target node, e.g. diagnostic sessions).
//  2. Mirror co-location: if SourceSceneId > 0, reuse the source scene's node
//     when it is alive and under the soft load cap. Intentionally bypasses
//     the instance-purpose filter so mirrors of main-world scenes can share
//     the world node's already-resident map/AI/spawn data. Without this
//     escape hatch StrictNodeTypeSeparation=true would force every mirror
//     to a fresh instance node, defeating the whole optimization.
//  3. Default: GetBestNodeForPurpose(Instance) — picks the lowest-load
//     instance-hosting node.
//
// The mirror path logs its decision at INFO on success and WARN on fallback
// so operators can observe how often co-location is taking effect.
func (l *CreateSceneLogic) pickInstanceNode(in *scene_manager.CreateSceneRequest) (string, error) {
	if in.TargetNodeId != "" {
		return in.TargetNodeId, nil
	}

	if in.SourceSceneId > 0 {
		if node, reason := l.resolveMirrorSourceNode(in.SourceSceneId, in.ZoneId); node != "" {
			metrics.ObserveMirrorColocate(in.ZoneId, "hit", "ok")
			l.Logger.Infof("[Mirror] Co-locating mirror (conf=%d mirror_conf=%d) with source scene %d on node %s (cross-type allowed)",
				in.SceneConfId, in.MirrorConfigId, in.SourceSceneId, node)
			return node, nil
		} else {
			metrics.ObserveMirrorColocate(in.ZoneId, "fallback", reason)
			// Reason already logged inside resolveMirrorSourceNode.
		}
	}

	return GetBestNodeForPurpose(l.ctx, l.svcCtx, in.ZoneId, constants.NodePurposeInstance)
}

// resolveMirrorSourceNode returns the node hosting sourceSceneId when that
// node is a viable co-location target. On success returns (node, "ok").
// On fallback returns ("", <reason>) where reason is one of:
//   - "no_mapping":   source scene has no scene:{id}:node entry
//   - "zone_mismatch": source scene lives in a different zone (would punch
//     a hole in zone isolation, e.g. mirror request from
//     zone 2 pointing at a zone 1 source — co-location
//     would route the mirror onto zone 1's node)
//   - "node_dead":    source node is not in the requested zone's load set
//   - "overloaded":   source node is at/over MirrorSourceNodeLoadCap
//
// The reason string is used as a Prometheus label and also logged so
// operators can diagnose why a mirror fell through to GetBestNode.
//
// zone_mismatch is treated as a soft fallback (not an error) so callers
// that pass a stale source_scene_id from a different zone still get a
// usable mirror — just without the co-location optimization.
func (l *CreateSceneLogic) resolveMirrorSourceNode(sourceSceneId uint64, zoneId uint32) (string, string) {
	nodeId, err := l.svcCtx.Redis.Get(sceneNodeKey(sourceSceneId))
	if err != nil || nodeId == "" {
		l.Logger.Infof("[Mirror] source scene %d has no node mapping (err=%v), falling back to GetBestNode", sourceSceneId, err)
		return "", "no_mapping"
	}

	// Zone isolation guard. Allowing a cross-zone source to drive
	// co-location would land the new mirror on the source's zone node,
	// which then can't be reconciled by the requesting zone's node-death
	// loop (different active set), creating an orphan on every restart.
	// We only enforce when the source has a recorded zone — sources
	// created before scene:{id}:zone existed return 0 from GetSceneZone
	// and skip the check (back-compat).
	if srcZone := GetSceneZone(l.svcCtx, sourceSceneId); srcZone != 0 && srcZone != zoneId {
		l.Logger.Infof("[Mirror] source scene %d lives in zone %d but request is for zone %d, falling back to GetBestNode",
			sourceSceneId, srcZone, zoneId)
		return "", "zone_mismatch"
	}

	if !IsNodeAlive(l.svcCtx, zoneId, nodeId) {
		l.Logger.Infof("[Mirror] source node %s (scene %d) is not alive in zone %d, falling back to GetBestNode",
			nodeId, sourceSceneId, zoneId)
		return "", "node_dead"
	}

	loadCap := l.svcCtx.Config.MirrorSourceNodeLoadCap
	if loadCap > 0 {
		if load, ok := l.getNodeSceneCount(zoneId, nodeId); ok && load >= loadCap {
			l.Logger.Infof("[Mirror] source node %s load=%d >= cap=%d, falling back to GetBestNode",
				nodeId, load, loadCap)
			return "", "overloaded"
		}
	}

	return nodeId, "ok"
}

// getNodeSceneCount reads the per-node scene_count counter used for soft load
// estimation. Returns (0, false) if the key is missing or unreadable so the
// caller treats the node as unknown-load rather than zero-load.
func (l *CreateSceneLogic) getNodeSceneCount(zoneId uint32, nodeId string) (int64, bool) {
	countStr, err := l.svcCtx.Redis.Get(nodeSceneCountKey(zoneId, nodeId))
	if err != nil || countStr == "" {
		return 0, false
	}
	count, err := strconv.ParseInt(countStr, 10, 64)
	if err != nil {
		return 0, false
	}
	return count, true
}

// allocateScene generates a scene ID and registers it in Redis.
// Shared by both main-world and instance creation.
func (l *CreateSceneLogic) allocateScene(confId uint64, targetNode string, zoneId uint32) (uint64, error) {
	// 失租后发号器被 fence,建场景整体失败。scene_id 撞号的后果比建帮更重:
	// 下面是裸 Redis SET(无 CAS),两个场景共用一个 id 会互相覆盖路由,玩家被静默送错节点。
	sceneId, err := l.svcCtx.SceneIDGen.Generate()
	if err != nil {
		return 0, fmt.Errorf("scene id generator unavailable: %w", err)
	}

	// scene -> node mapping.
	if err := l.svcCtx.Redis.Set(sceneNodeKey(sceneId), targetNode); err != nil {
		return 0, fmt.Errorf("redis set scene node failed: %w", err)
	}
	// scene -> zone mapping for cross-zone lookups.
	l.svcCtx.Redis.Set(sceneZoneKey(sceneId), fmt.Sprintf("%d", zoneId))

	// Increment node scene count for load tracking.
	sceneCountKey := nodeSceneCountKey(zoneId, targetNode)
	if _, err := l.svcCtx.Redis.Incr(sceneCountKey); err != nil {
		l.Logger.Errorf("Failed to increment scene count for node %s: %v", targetNode, err)
	}

	// Reverse index: node -> scenes. The node-death reconciliation loop
	// uses this to find orphan scenes without scanning the entire keyspace.
	if _, err := l.svcCtx.Redis.Sadd(nodeScenesKey(zoneId, targetNode), fmt.Sprintf("%d", sceneId)); err != nil {
		l.Logger.Errorf("Failed to add scene %d to node %s scenes set: %v", sceneId, targetNode, err)
	}

	return sceneId, nil
}
