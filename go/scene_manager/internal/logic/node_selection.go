package logic

import (
	"context"
	"fmt"
	"strconv"

	"scene_manager/internal/constants"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// getNodesForPurpose returns the subset of the zone's registered nodes that
// can host the given purpose (world vs instance), preserving the ascending
// load-score order from the Redis sorted set. Classification reads
// `node:zone:{zoneId}:{nodeId}:scene_node_type` mirrored by load_reporter.
//
// Filtering rules:
//   - A node with a KNOWN scene_node_type is included only if it matches
//     the purpose (see constants.MatchesPurpose).
//   - A node with NO scene_node_type mirrored yet (e.g. mid-registration or
//     a legacy node that never set GameConfig.scene_node_type) is treated as
//     "unclassified" and included for either purpose. This is the graceful
//     path — strict enforcement happens in the caller via the returned set
//     being empty when StrictNodeTypeSeparation + strict filtering disagree.
//
// Fallback rules (when the preferred pool is empty):
//   - StrictNodeTypeSeparation=false → return ALL zone nodes (dev/single-node
//     deployments where one process plays both roles).
//   - StrictNodeTypeSeparation=true  → return nil. Callers must surface
//     ErrNoNodeForPurpose rather than dump the wrong purpose onto the wrong
//     type of node.
//
// Mirror co-location deliberately does NOT go through this filter; see
// createscenelogic.pickInstanceNode for the rationale.
func getNodesForPurpose(svcCtx *svc.ServiceContext, zoneId uint32, purpose constants.NodePurpose) []string {
	pairs, err := svcCtx.Redis.ZrangeWithScores(nodeLoadKey(zoneId), 0, -1)
	if err != nil || len(pairs) == 0 {
		return nil
	}

	allNodes := make([]string, 0, len(pairs))
	preferred := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if isKnownNodeIdentityAmbiguous(zoneId, p.Key) {
			logx.Errorf("[NodeSelection] zone %d node %s has duplicate registrations; refusing this identity",
				zoneId, p.Key)
			continue
		}
		allNodes = append(allNodes, p.Key)

		typ, classified := readNodeSceneType(svcCtx, zoneId, p.Key)
		switch {
		case !classified:
			// Unclassified node — include in every pool so freshly-registered
			// nodes aren't starved while the type mirror is still catching up.
			preferred = append(preferred, p.Key)
		case constants.MatchesPurpose(typ, purpose):
			preferred = append(preferred, p.Key)
		}
	}

	if len(preferred) > 0 {
		return preferred
	}

	if svcCtx.Config.StrictNodeTypeSeparation {
		logx.Errorf("[NodeSelection] zone %d: no nodes match purpose %d (strict mode — refusing fallback)",
			zoneId, purpose)
		return nil
	}

	logx.Infof("[NodeSelection] zone %d: no nodes match purpose %d, falling back to full pool (strict=false)",
		zoneId, purpose)
	return allNodes
}

// GetBestNodeForPurpose returns the lowest-load node that can host the given
// purpose for the zone. Lowest-load semantics come from the Redis sorted-set
// order (already ascending) intersected with the purpose filter.
func GetBestNodeForPurpose(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, purpose constants.NodePurpose) (string, error) {
	candidates := getNodesForPurpose(svcCtx, zoneId, purpose)
	if len(candidates) == 0 {
		return "", fmt.Errorf("no scene nodes for purpose %d in zone %d", purpose, zoneId)
	}
	// getNodesForPurpose already returns candidates in ascending load order.
	return candidates[0], nil
}

// readNodeSceneType reads the mirrored scene_node_type for a node.
// Returns (type, true) when a value is stored, (0, false) otherwise so
// callers can distinguish "unclassified" from "classified as 0".
func readNodeSceneType(svcCtx *svc.ServiceContext, zoneId uint32, nodeId string) (uint32, bool) {
	s, err := svcCtx.Redis.Get(nodeSceneNodeTypeKey(zoneId, nodeId))
	if err != nil || s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}
