#include "service_discovery_manager.h"
#include <thread_context/redis_manager.h>
#include "node/system/etcd/etcd_helper.h"
#include <muduo/base/Logging.h>
#include "node/system/node/node_util.h"
#include "node/system/node/node.h"
#include "node/system/node/node_connector.h"
#include "google/protobuf/util/json_util.h"
#include <network/node_utils.h>
#include <node_config_manager.h>

void ServiceDiscoveryManager::Init()
{
	etcdService.Init();
	FetchServiceNodes();
	serviceNodeSyncTimer_.RunEvery(2.0, [this] {
		FetchServiceNodes();
	});
}

void ServiceDiscoveryManager::Shutdown()
{
	serviceNodeSyncTimer_.Cancel();
	etcdService.Shutdown();
}

void ServiceDiscoveryManager::FetchServiceNodes()
{
	for (const auto &prefix : tlsNodeConfigManager.GetBaseDeployConfig().service_discovery_prefixes())
	{
		EtcdHelper::RangeQuery(prefix);
	}
}

void ServiceDiscoveryManager::AddServiceNode(const std::string &nodeJson, uint32_t nodeType)
{
	LOG_TRACE << "Add service node type " << nodeType << " JSON: " << nodeJson;

	if (!eNodeType_IsValid(static_cast<int32_t>(nodeType)))
	{
		LOG_ERROR << "Invalid node type: " << nodeType;
		return;
	}

	NodeInfo discoveredNode;
	auto parseResult = google::protobuf::util::JsonStringToMessage(nodeJson, &discoveredNode);
	if (!parseResult.ok())
	{
		LOG_ERROR << "Parse node JSON failed, type: " << nodeType
				  << ", JSON: " << nodeJson
				  << ", Error: " << parseResult.message().data();
		return;
	}

	// Multi-zone safety guard:
	//   Zone-scoped services (Gate / Scene / Login / PlayerLocator) register
	//   under etcd keys that include a zone segment and callers only talk to
	//   the same zone. Their local entt::entity slot is keyed by node_id,
	//   which is unique ONLY within a zone — so accepting a foreign-zone
	//   node into this registry would alias two different zones onto the
	//   same entity slot and break PickRandomNode's zone filter
	//   ("Node not found for session id ..., message id: 48").
	//
	//   Cross-zone services (SceneManager, DataService, ...) are kept as a
	//   global pool intentionally: SceneManager is how we issue cross-zone
	//   Redirect responses, and DataService serves global account data.
	//   Their entity ids are auto-assigned in ConnectToGrpcNode and looked
	//   up by uuid in ExecuteNodeRemoval, so cross-zone is safe for them.
	//
	//   The authoritative classification lives in NodeUtils::IsZoneScopedNodeType.
	if (NodeUtils::IsZoneScopedNodeType(nodeType) &&
		discoveredNode.zone_id() != gNode->GetNodeInfo().zone_id())
	{
		LOG_INFO << "Skip zone-scoped node from foreign zone. type=" << nodeType
				 << ", node_zone=" << discoveredNode.zone_id()
				 << ", self_zone=" << gNode->GetNodeInfo().zone_id()
				 << ", uuid=" << discoveredNode.node_uuid();
		return;
	}

	// Cancel any pending grace-period removal for this node.
	gNode->CancelPendingNodeRemoval(discoveredNode.node_uuid());

	auto &serviceNodesByType = tlsEcs.nodeGlobalRegistry.get_or_emplace<ServiceNodeList>(tlsEcs.GrpcNodeEntity());
	auto &cachedNodesOfType = *serviceNodesByType[nodeType].mutable_node_list();

	bool hasExistingSnapshot = false;
	for (auto &cachedNode : cachedNodesOfType)
	{
		if (!NodeUtils::IsSameNode(cachedNode.node_uuid(), discoveredNode.node_uuid()))
		{
			continue;
		}

		cachedNode.CopyFrom(discoveredNode);
		hasExistingSnapshot = true;
		break;
	}

	if (!hasExistingSnapshot)
	{
		*cachedNodesOfType.Add() = discoveredNode;
		LOG_INFO << "Node added, type: " << nodeType
				 << ", node_id: " << discoveredNode.node_id()
				 << ", uuid: " << discoveredNode.node_uuid()
				 << ", ip: " << discoveredNode.endpoint().ip()
				 << ", port: " << discoveredNode.endpoint().port();
		LOG_TRACE << "Node added detail: " << discoveredNode.DebugString();
	}
	else
	{
		LOG_TRACE << "Node updated from service discovery event, uuid=" << discoveredNode.node_uuid();
	}

	if (gNode->IsCurrentNode(discoveredNode))
	{
		LOG_TRACE << "Node has same lease_id as self, skip adding node. Self uuid: " << discoveredNode.node_uuid();
		return;
	}

	const auto &whitelist = gNode->GetTargetNodeTypeWhitelist();
	if (!whitelist.contains(nodeType))
		return;
	if (NodeUtils::IsNodeConnected(nodeType, discoveredNode))
	{
		LOG_TRACE << "Node already connected, skip reconnect. uuid=" << discoveredNode.node_uuid();
		return;
	}

	if (gNode->IsServiceStarted())
	{
		NodeConnector::ConnectToNode(discoveredNode);
		LOG_INFO << "Connected to node, type: " << nodeType
				 << ", node_id: " << discoveredNode.node_id()
				 << ", uuid: " << discoveredNode.node_uuid();
	}
	else
	{
		LOG_TRACE << "Service not started or node already connected. Skipping connection for now, uuid=" << discoveredNode.node_uuid();
	}
}

void ServiceDiscoveryManager::HandleServiceNodeStart(const std::string &key, const std::string &value)
{
	LOG_TRACE << "Service node start, key: " << key << ", value: " << value;

	// Filter out allocation-slot keys before we attempt to parse the value as
	// a NodeInfo JSON. EtcdManager::RegisterNodeService writes the
	// `<Service>.rpc/allocated/...` slot with the raw node_uuid as the value
	// (it's a CAS-only existence claim, not service discovery data), so a
	// blanket parse would log "Parse node JSON failed ... expected '{'" once
	// per RangeQuery hit per node — under the 45k stress run this produced
	// thousands of error lines per second and crowded out signal logs.
	if (key.find("/allocated/") != std::string::npos)
	{
		LOG_TRACE << "Skip allocation-slot key for service discovery: " << key;
		return;
	}

	if (const auto nodeType = NodeUtils::GetServiceTypeFromPrefix(key); eNodeType_IsValid(nodeType))
	{
		AddServiceNode(value, nodeType);
	}
}
