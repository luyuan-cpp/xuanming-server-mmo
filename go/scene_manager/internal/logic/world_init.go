package logic

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"

	"scene_manager/internal/constants"
	"scene_manager/internal/svc"

	"shared/generated/table"

	"github.com/zeromicro/go-zero/core/logx"
)

// Redis keys for world scene management.
const (
	// SET per (zone, confId): holds sceneId strings for each channel.
	WorldChannelsKeyFmt = "world_channels:zone:%d:%d"
)

type WorldChannelCandidate struct {
	SceneID uint64
	NodeID  string
}

func worldChannelsKey(zoneID uint32, confId uint64) string {
	return fmt.Sprintf(WorldChannelsKeyFmt, zoneID, confId)
}

// initWorldScenesForZone ensures world scenes (with N channels each) exist for a single zone.
// Called by fullSync (on startup / re-sync) and handleWatchEvent (on node PUT).
// Two-layer idempotency:
//   - Redis layer: only creates missing channels (if existing < ChannelCount).
//   - C++ layer: CreateScene is idempotent by scene_id (returns existing entity).
//
// Always sends CreateScene RPC for all channels to handle C++ node restarts.
//
// Node selection uses the world-hosting pool (scene_node_type ∈
// {kMainSceneNode, kMainSceneCrossNode}) so instance-only nodes are never
// handed a persistent world channel. When StrictNodeTypeSeparation=false and
// the pool is empty, selection falls back to any available node in the zone
// (dev / single-process convenience).
func initWorldScenesForZone(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, confIds []uint64) {
	nodes := getNodesForPurpose(svcCtx, zoneId, constants.NodePurposeWorld)
	if len(nodes) == 0 {
		logx.Errorf("[World] Zone %d: no world-hosting nodes available (strict=%v), skipping",
			zoneId, svcCtx.Config.StrictNodeTypeSeparation)
		return
	}
	// assignNodeByHash needs a stable ordering independent of live load
	// scores, otherwise the same (confId, channel) could land on different
	// nodes across two scheduling runs whenever a node's score changed.
	sort.Strings(nodes)

	logx.Infof("[World] Zone %d: ensuring %d world scenes across %d world-hosting nodes",
		zoneId, len(confIds), len(nodes))

	// Track nodes proven dead during this batch to avoid repeated failures.
	deadNodes := make(map[string]struct{})
	liveNodes := make([]string, len(nodes))
	copy(liveNodes, nodes)

	created, ensured := 0, 0

	for _, confId := range confIds {
		channelSetKey := worldChannelsKey(zoneId, confId)
		// 期望频道数的权威在 Redis,不是配置 —— 配置只是首次播种的种子。
		// 直接读 ChannelCountFor 的话,自动缩容刚摘掉的频道会在这里被
		// 重新补出来,缩容根本不成立。见 world_autoscale.go 顶部注释。
		channelCount := DesiredWorldChannelCount(svcCtx, zoneId, confId)

		// Get existing channels for this confId.
		existingMembers, _ := svcCtx.Redis.Smembers(channelSetKey)
		existingCount := len(existingMembers)

		// Create missing channels — only assign to live nodes.
		for i := existingCount; i < channelCount; i++ {
			if len(liveNodes) == 0 {
				logx.Errorf("[World] No live nodes remaining for zone %d, stopping allocation", zoneId)
				break
			}

			sceneId := svcCtx.SceneIDGen.Generate()

			// Vary hash input per channel so channels may land on different nodes.
			targetNode := assignNodeByHash(confId*1000+uint64(i), liveNodes)

			sceneNodeKey := fmt.Sprintf("scene:%d:node", sceneId)
			if err := svcCtx.Redis.Set(sceneNodeKey, targetNode); err != nil {
				logx.Errorf("[World] Failed to store scene mapping for scene %d: %v", sceneId, err)
				continue
			}
			// Store scene -> zone mapping for cross-zone lookups.
			svcCtx.Redis.Set(fmt.Sprintf("scene:%d:zone", sceneId), fmt.Sprintf("%d", zoneId))

			if _, err := svcCtx.Redis.Sadd(channelSetKey, fmt.Sprintf("%d", sceneId)); err != nil {
				logx.Errorf("[World] Failed to add channel to set for conf %d: %v", confId, err)
				continue
			}

			// Initialize player count.
			svcCtx.Redis.Set(fmt.Sprintf(InstancePlayerCountKey, sceneId), "0")

			sceneCountKey := fmt.Sprintf(NodeSceneCountKey, targetNode)
			svcCtx.Redis.Incr(sceneCountKey)

			// Reverse index so node-death reconciliation knows about this
			// world channel. World scenes are filtered out by the orphan
			// cleanup loop (they get rebalanced instead of destroyed), but
			// the set still needs to be accurate for diagnostics.
			svcCtx.Redis.Sadd(nodeScenesKey(targetNode), fmt.Sprintf("%d", sceneId))

			created++
			logx.Infof("[World] Allocated channel %d (scene=%d, conf=%d) on node %s in zone %d",
				i, sceneId, confId, targetNode, zoneId)
		}

		// Always send CreateScene RPC for all channels — C++ deduplicates by scene_id.
		// If a channel's target node is dead, reassign to a live node.
		allMembers, _ := svcCtx.Redis.Smembers(channelSetKey)
		for _, sceneIdStr := range allMembers {
			sceneId, _ := strconv.ParseUint(sceneIdStr, 10, 64)
			nodeKey := fmt.Sprintf("scene:%d:node", sceneId)
			targetNode, _ := svcCtx.Redis.Get(nodeKey)
			if targetNode == "" {
				logx.Errorf("[World] Missing node for scene %d, skipping RPC", sceneId)
				continue
			}

			// Reassign if the target node is known dead.
			if _, dead := deadNodes[targetNode]; dead {
				if len(liveNodes) == 0 {
					logx.Errorf("[World] No live nodes remaining, skipping scene %d", sceneId)
					continue
				}
				newNode := assignNodeByHash(confId*1000+uint64(ensured), liveNodes)
				logx.Infof("[World] Reassigning scene %d from dead node %s to live node %s", sceneId, targetNode, newNode)
				reassignSceneNode(svcCtx, sceneId, targetNode, newNode)
				targetNode = newNode
			}

			if _, err := RequestNodeCreateScene(ctx, svcCtx, targetNode, uint32(confId), sceneId); err != nil {
				logx.Errorf("[World] Failed to call CreateScene for conf %d scene %d: %v", confId, sceneId, err)
				// Mark node as dead so we don't keep retrying it for remaining scenes.
				if isNodeUnreachableError(err) {
					markNodeDead(svcCtx, zoneId, targetNode, deadNodes, &liveNodes)
					// Reassign this scene to a live node and retry once.
					if len(liveNodes) > 0 {
						newNode := assignNodeByHash(confId*1000+uint64(ensured), liveNodes)
						logx.Infof("[World] Retrying scene %d on live node %s after dead node %s", sceneId, newNode, targetNode)
						reassignSceneNode(svcCtx, sceneId, targetNode, newNode)
						if _, err := RequestNodeCreateScene(ctx, svcCtx, newNode, uint32(confId), sceneId); err != nil {
							logx.Errorf("[World] Retry also failed for conf %d scene %d on node %s: %v", confId, sceneId, newNode, err)
						}
					}
				}
			}
			ensured++
		}
	}

	logx.Infof("[World] Zone %d done: created=%d channels, ensured=%d RPCs, dead_nodes=%d",
		zoneId, created, ensured, len(deadNodes))
}

// GetBestWorldChannel returns the (sceneId, nodeId) of the least-loaded channel
// for the given world confId in the zone. Returns (0, "", nil) if no channels exist.
//
// Self-healing: if all channels are mapped to dead nodes (e.g. the old scene node
// crashed and a new one just registered), it lazily reassigns stale channels to
// live nodes instead of returning "no channel". This covers the race window between
// a new node's etcd PUT event and initWorldScenesForZone completing reassignment.
func GetBestWorldChannel(ctx context.Context, svcCtx *svc.ServiceContext, confId uint64, zoneId uint32) (uint64, string, error) {
	channelSetKey := worldChannelsKey(zoneId, confId)
	members, err := svcCtx.Redis.Smembers(channelSetKey)
	if err != nil || len(members) == 0 {
		return 0, "", err
	}

	var bestSceneId uint64
	var bestNodeId string
	bestCount := int64(math.MaxInt64)

	var staleSceneIds []uint64

	for _, m := range members {
		sceneId, _ := strconv.ParseUint(m, 10, 64)
		if sceneId == 0 {
			continue
		}

		nodeKey := fmt.Sprintf("scene:%d:node", sceneId)
		nid, _ := svcCtx.Redis.Get(nodeKey)
		if nid == "" || !IsNodeAlive(svcCtx, zoneId, nid) {
			staleSceneIds = append(staleSceneIds, sceneId)
			continue
		}

		countStr, _ := svcCtx.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, sceneId))
		count, _ := strconv.ParseInt(countStr, 10, 64)

		if count < bestCount {
			bestCount = count
			bestSceneId = sceneId
			bestNodeId = nid
		}
	}

	if bestSceneId > 0 {
		return bestSceneId, bestNodeId, nil
	}

	// All channels mapped to dead nodes — lazy-reassign to live world-hosting
	// nodes. Mixing purposes here would put a persistent world onto an
	// instance-only node and future EnterScene requests would be rejected by
	// the C++ handler (see player_scene_handler.cpp).
	liveNodes := getNodesForPurpose(svcCtx, zoneId, constants.NodePurposeWorld)
	sort.Strings(liveNodes)
	if len(liveNodes) == 0 {
		logx.Errorf("[World] All %d channels for conf %d (zone %d) are stale and no live nodes available",
			len(staleSceneIds), confId, zoneId)
		return 0, "", nil
	}

	logx.Infof("[World] All %d channels for conf %d (zone %d) are stale, lazy-reassigning to %d live nodes",
		len(staleSceneIds), confId, zoneId, len(liveNodes))

	for i, sceneId := range staleSceneIds {
		targetNode := assignNodeByHash(confId*1000+uint64(i), liveNodes)
		nodeKey := fmt.Sprintf("scene:%d:node", sceneId)
		oldNode, _ := svcCtx.Redis.Get(nodeKey)
		if err := svcCtx.Redis.Set(nodeKey, targetNode); err != nil {
			logx.Errorf("[World] Lazy-reassign: failed to update scene %d -> node %s: %v", sceneId, targetNode, err)
			continue
		}
		if oldNode != "" && oldNode != targetNode {
			svcCtx.Redis.Srem(nodeScenesKey(oldNode), fmt.Sprintf("%d", sceneId))
		}
		svcCtx.Redis.Sadd(nodeScenesKey(targetNode), fmt.Sprintf("%d", sceneId))

		if _, err := RequestNodeCreateScene(ctx, svcCtx, targetNode, uint32(confId), sceneId); err != nil {
			logx.Errorf("[World] Lazy-reassign: CreateScene failed for scene %d on node %s: %v", sceneId, targetNode, err)
			continue
		}

		logx.Infof("[World] Lazy-reassigned stale channel (scene=%d, conf=%d) to live node %s", sceneId, confId, targetNode)
		return sceneId, targetNode, nil
	}

	return 0, "", nil
}

// ReserveBestWorldChannelForEnter selects and reserves a world channel for an
// EnterScene request. It preserves GetBestWorldChannel's stale-channel healing,
// then performs the final least-loaded choice and player-count reservation in
// one Redis Lua script.
func ReserveBestWorldChannelForEnter(ctx context.Context, svcCtx *svc.ServiceContext, confId uint64, zoneId uint32) (uint64, string, error) {
	channelSetKey := worldChannelsKey(zoneId, confId)
	members, err := svcCtx.Redis.Smembers(channelSetKey)
	if err != nil || len(members) == 0 {
		return 0, "", err
	}

	var candidates []WorldChannelCandidate
	var staleSceneIds []uint64

	for _, m := range members {
		sceneId, _ := strconv.ParseUint(m, 10, 64)
		if sceneId == 0 {
			continue
		}

		nodeKey := fmt.Sprintf("scene:%d:node", sceneId)
		nid, _ := svcCtx.Redis.Get(nodeKey)
		if nid == "" || !IsNodeAlive(svcCtx, zoneId, nid) {
			staleSceneIds = append(staleSceneIds, sceneId)
			continue
		}
		candidates = append(candidates, WorldChannelCandidate{SceneID: sceneId, NodeID: nid})
	}

	if len(candidates) == 0 {
		liveNodes := getNodesForPurpose(svcCtx, zoneId, constants.NodePurposeWorld)
		sort.Strings(liveNodes)
		if len(liveNodes) == 0 {
			logx.Errorf("[World] All %d channels for conf %d (zone %d) are stale and no live nodes available",
				len(staleSceneIds), confId, zoneId)
			return 0, "", nil
		}

		logx.Infof("[World] All %d channels for conf %d (zone %d) are stale, lazy-reassigning to %d live nodes",
			len(staleSceneIds), confId, zoneId, len(liveNodes))

		for i, sceneId := range staleSceneIds {
			targetNode := assignNodeByHash(confId*1000+uint64(i), liveNodes)
			nodeKey := fmt.Sprintf("scene:%d:node", sceneId)
			oldNode, _ := svcCtx.Redis.Get(nodeKey)
			if err := svcCtx.Redis.Set(nodeKey, targetNode); err != nil {
				logx.Errorf("[World] Reserve lazy-reassign: failed to update scene %d -> node %s: %v", sceneId, targetNode, err)
				continue
			}
			if oldNode != "" && oldNode != targetNode {
				svcCtx.Redis.Srem(nodeScenesKey(oldNode), fmt.Sprintf("%d", sceneId))
			}
			svcCtx.Redis.Sadd(nodeScenesKey(targetNode), fmt.Sprintf("%d", sceneId))

			if _, err := RequestNodeCreateScene(ctx, svcCtx, targetNode, uint32(confId), sceneId); err != nil {
				logx.Errorf("[World] Reserve lazy-reassign: CreateScene failed for scene %d on node %s: %v", sceneId, targetNode, err)
				continue
			}
			candidates = append(candidates, WorldChannelCandidate{SceneID: sceneId, NodeID: targetNode})
		}
	}

	sceneId, nodeId, _, err := ReserveBestWorldChannel(svcCtx, candidates)
	if err != nil {
		return 0, "", err
	}
	return sceneId, nodeId, nil
}

// reassignSceneNode moves a scene's node mapping atomically from the
// caller's perspective: updates scene:{id}:node, fixes the reverse
// node:{id}:scenes membership on both sides. Errors are logged and
// swallowed — the reverse index is best-effort and the node-death
// reconciliation loop double-checks scene:{id}:node before destroying.
func reassignSceneNode(svcCtx *svc.ServiceContext, sceneId uint64, oldNode, newNode string) {
	nodeKey := fmt.Sprintf("scene:%d:node", sceneId)
	if err := svcCtx.Redis.Set(nodeKey, newNode); err != nil {
		logx.Errorf("[reassign] Failed to update scene %d -> node %s: %v", sceneId, newNode, err)
		return
	}
	sceneStr := fmt.Sprintf("%d", sceneId)
	if oldNode != "" && oldNode != newNode {
		svcCtx.Redis.Srem(nodeScenesKey(oldNode), sceneStr)
	}
	svcCtx.Redis.Sadd(nodeScenesKey(newNode), sceneStr)
}

// GetAllWorldChannels returns all channel sceneIds for a confId in a zone.
func GetAllWorldChannels(ctx context.Context, svcCtx *svc.ServiceContext, confId uint64, zoneId uint32) ([]uint64, error) {
	channelSetKey := worldChannelsKey(zoneId, confId)
	members, err := svcCtx.Redis.Smembers(channelSetKey)
	if err != nil {
		return nil, err
	}
	ids := make([]uint64, 0, len(members))
	for _, m := range members {
		id, _ := strconv.ParseUint(m, 10, 64)
		if id > 0 {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// worldConfIds extracts base scene config IDs from the loaded World table.
// worldConfIdsOverride, when non-nil, replaces the World-table lookup.
// Same test-seam pattern as nodeDialer / nodeEndpointOverride: the World
// table is empty in unit tests, so anything that iterates world maps
// (autoscaler, orphan cleanup) would be a no-op without this.
// Production never sets it.
var worldConfIdsOverride []uint64

// SetWorldConfIdsForTest installs a fake World-table id list and returns a
// restore func. MUST be paired with t.Cleanup.
func SetWorldConfIdsForTest(ids []uint64) (restore func()) {
	prev := worldConfIdsOverride
	worldConfIdsOverride = ids
	return func() { worldConfIdsOverride = prev }
}

func worldConfIds() []uint64 {
	if worldConfIdsOverride != nil {
		return worldConfIdsOverride
	}
	rows := table.WorldTableManagerInstance.FindAll()
	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, uint64(row.SceneId))
	}
	return ids
}

// IsWorldConf checks if a scene_conf_id is a world scene.
func IsWorldConf(confId uint64) bool {
	for _, row := range table.WorldTableManagerInstance.FindAll() {
		if uint64(row.SceneId) == confId {
			return true
		}
	}
	return false
}

// assignNodeByHash uses FNV-1a to consistently map a key to a node.
// Deterministic: same key + same node list -> same node.
func assignNodeByHash(key uint64, sortedNodes []string) string {
	h := fnv.New32a()
	h.Write([]byte(fmt.Sprintf("%d", key)))
	idx := int(h.Sum32()) % len(sortedNodes)
	return sortedNodes[idx]
}

// isNodeUnreachableError returns true if the error indicates the node process is
// not reachable (connection refused, unavailable). This identifies zombie nodes
// whose etcd lease hasn't expired yet but whose process is dead.
func isNodeUnreachableError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "Unavailable") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "connectex") ||
		strings.Contains(s, "not found in etcd")
}

// markNodeDead removes a zombie node from Redis load set and connection cache,
// and updates the local liveNodes slice.
func markNodeDead(svcCtx *svc.ServiceContext, zoneId uint32, nodeId string, deadNodes map[string]struct{}, liveNodes *[]string) {
	if _, already := deadNodes[nodeId]; already {
		return
	}
	deadNodes[nodeId] = struct{}{}

	// Remove from the local live list.
	filtered := (*liveNodes)[:0]
	for _, n := range *liveNodes {
		if n != nodeId {
			filtered = append(filtered, n)
		}
	}
	*liveNodes = filtered

	// Remove from Redis load set so future calls don't pick this node.
	loadKey := nodeLoadKey(zoneId)
	svcCtx.Redis.Zrem(loadKey, nodeId)

	// Remove cached gRPC connection (stale endpoint).
	RemoveNodeConn(nodeId)

	logx.Infof("[World] Marked node %s as dead (zone %d), removed from load set", nodeId, zoneId)
}
