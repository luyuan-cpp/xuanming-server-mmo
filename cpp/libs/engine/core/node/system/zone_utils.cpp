#include "zone_utils.h"
#include "node/system/node/node_util.h"
#include "proto/common/base/common.pb.h"

uint32_t GetZoneIdFromNodeId(NodeId nodeId) {
	// node_id 是业务节点号,不是 entt 实体整数(uuid 主键重构之后两者不再相等)。
	// 裸 `entt::entity{nodeId}` 在这里的后果是把**别的节点**的 zone 当成答案返回,
	// 而调用方 IsCrossZone 会据此判定跨不跨 zone —— 静默的错误分类比查不到更糟。
	// 与 scene_handler / gate_service_handler 已修的两处同源
	// (docs/design/routing-identity-audit-20260908.md R03 的同类残留)。
	const auto nodeEntityOpt = NodeUtils::FindNodeEntityByNodeId(eNodeType::SceneNodeService, nodeId);
	if (!nodeEntityOpt) {
		return kInvalidNodeId;
	}

	auto& registry = NodeUtils::GetRegistryForNodeType(eNodeType::SceneNodeService);

	const NodeInfo* nodeInfo = registry.try_get<NodeInfo>(*nodeEntityOpt);
	if (!nodeInfo) {
		return kInvalidNodeId;
	}

	return nodeInfo->zone_id();
}


bool IsCrossZone(NodeId  fromNodeId, NodeId toNodeId) {
	return GetZoneIdFromNodeId(fromNodeId) != GetZoneIdFromNodeId(toNodeId);
}