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
//
// 计划排空(docs/design/scene-node-planned-drain.md §4.2、§4.6):自报排空中的节点(NodeInfo.admission = CLOSED)
// 一律不返回 —— 不放新频道、新副本,也不当迁移与疏散的目标;它仍然活着、仍是名下场景的属主(IsNodeAlive 不看这里)。
// 返回值里不在外出构建上的节点排在前面,两组内部各自保持负载升序,所以「最空的节点」是新构建里最空的那台。
func getNodesForPurpose(svcCtx *svc.ServiceContext, zoneId uint32, purpose constants.NodePurpose) []string {
	pairs, err := svcCtx.Redis.ZrangeWithScores(nodeLoadKey(zoneId), 0, -1)
	if err != nil || len(pairs) == 0 {
		return nil
	}

	admission := snapshotNodeAdmission(zoneId)
	allNodes := make([]string, 0, len(pairs))
	preferred := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if isKnownNodeIdentityAmbiguous(zoneId, p.Key) {
			logx.Errorf("[NodeSelection] zone %d node %s has duplicate registrations; refusing this identity",
				zoneId, p.Key)
			continue
		}
		if admission.isDraining(p.Key) {
			continue
		}
		allNodes = append(allNodes, p.Key)

		typ, classified, err := readNodeSceneType(svcCtx, zoneId, p.Key)
		if err != nil {
			// 读不到类型就不知道这个节点能不能承载这个用途;当「未分类」会把世界频道派到只跑副本的节点上。
			// 这一轮当没有可用节点,调用方都是跳过或回可重试错误,和上面读负载集失败一致。
			noteReadUnknown(zoneId, readUnknownSiteNodeSelection, fmt.Sprintf("scene type of node %s", p.Key), err)
			return nil
		}
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
		return admission.preferredFirst(preferred)
	}

	if svcCtx.Config.StrictNodeTypeSeparation {
		logx.Errorf("[NodeSelection] zone %d: no nodes match purpose %d (strict mode — refusing fallback)",
			zoneId, purpose)
		return nil
	}

	logx.Infof("[NodeSelection] zone %d: no nodes match purpose %d, falling back to full pool (strict=false)",
		zoneId, purpose)
	return admission.preferredFirst(allNodes)
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
// Returns (type, true, nil) when a value is stored, (0, false, nil) otherwise so
// callers can distinguish "unclassified" from "classified as 0". err 只表示读失败:
// 此时不知道节点是什么类型,不能当成「未分类」。
func readNodeSceneType(svcCtx *svc.ServiceContext, zoneId uint32, nodeId string) (uint32, bool, error) {
	s, err := svcCtx.Redis.Get(nodeSceneNodeTypeKey(zoneId, nodeId))
	if err != nil {
		return 0, false, err
	}
	if s == "" {
		return 0, false, nil
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, false, nil
	}
	return uint32(v), true, nil
}
