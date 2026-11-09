#include "service_discovery_manager.h"
#include "discovery_resync_policy.h"
#include <thread_context/redis_manager.h>
#include "node/system/etcd/etcd_helper.h"
#include <muduo/base/Logging.h>
#include "node/system/node/node_util.h"
#include "node/system/node/node.h"
#include "node/system/node/node_connector.h"
#include "node/system/node/release_track.h"
#include "google/protobuf/util/json_util.h"
#include <network/node_utils.h>
#include <node_config_manager.h>

void ServiceDiscoveryManager::Init()
{
	etcdService.Init();
	// 启动时立即拉一次:watch 要从这次快照的下一个 revision 建起。
	FetchServiceNodes();
	ScheduleNextResync();
}

void ServiceDiscoveryManager::Shutdown()
{
	serviceNodeSyncTimer_.Cancel();
	etcdService.Shutdown();
}

void ServiceDiscoveryManager::ScheduleNextResync()
{
	// 兜底全量 Range:约 60 秒一次、±20% 抖动(discovery_resync_policy.h)。RunAfter 链而不是 RunEvery,每一拍重新抽抖动。
	std::uniform_real_distribution<double> unit(0.0, 1.0);
	serviceNodeSyncTimer_.RunAfter(discovery_resync::ResyncDelaySeconds(unit(resyncJitter_)), [this] {
		FetchServiceNodes();
		ScheduleNextResync();
	});
}

std::vector<std::string> ServiceDiscoveryManager::EffectiveDiscoveryPrefixes() const
{
	// zone-scoped 类型只看本 zone(control-plane-zone-scoping-20261105.md §3.1);本节点的 zone 在 InitRpcServer 里已定,
	// 早于 InitEtcdService。Range 与 watch 重建都从这里取,两处不会各算各的。
	const uint32_t zoneId = gNode->GetNodeInfo().zone_id();
	std::vector<std::string> prefixes;
	for (const auto &configured : tlsNodeConfigManager.GetBaseDeployConfig().service_discovery_prefixes())
	{
		prefixes.push_back(NodeUtils::DiscoveryPrefixFor(configured, zoneId));
	}
	return prefixes;
}

void ServiceDiscoveryManager::FetchServiceNodes()
{
	for (const auto &prefix : EffectiveDiscoveryPrefixes())
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
	// D77:忽略未知字段且永久保留。严格解析时,滚动期间新版本节点多带的字段(如 clientEndpoint)
	// 会让旧解析方把整个节点判为缺席;与 Go 侧 shared/nodeinfo.Unmarshal 同一口径。
	google::protobuf::util::JsonParseOptions parseOptions;
	parseOptions.ignore_unknown_fields = true;
	auto parseResult = google::protobuf::util::JsonStringToMessage(nodeJson, &discoveredNode, parseOptions);
	if (!parseResult.ok())
	{
		LOG_ERROR << "Parse node JSON failed, type: " << nodeType
				  << ", JSON: " << nodeJson
				  << ", Error: " << parseResult.message().data();
		return;
	}
	if (discoveredNode.node_type() != nodeType)
	{
		// etcd 前缀决定所属桶；value 不得把另一个服务的身份带进此桶或取消其摘除。
		LOG_ERROR << "Service discovery node type mismatch: prefix_type=" << nodeType
				  << ", value_type=" << discoveredNode.node_type();
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
	//   Cross-zone services (DataService, Match, ...) are kept as a global
	//   pool intentionally: DataService serves global account data. Their
	//   entity ids are auto-assigned in ConnectToGrpcNode and looked up by
	//   uuid in ExecuteNodeRemoval, so cross-zone is safe for them.
	//   SceneManager 2026-11-05 起是 zone-scoped:每个 zone 的 scene_manager 只编排本 zone,跨 zone 重定向由本 zone 的
	//   实例签发(docs/design/control-plane-zone-scoping-20261105.md §3.1)。
	//
	//   The authoritative classification lives in NodeUtils::IsZoneScopedNodeType.
	//   zone-scoped 类型的发现前缀已收窄到本 zone(EffectiveDiscoveryPrefixes),这里的过滤是纵深防御:
	//   前缀配错、或别处直接调进来时仍不收别的 zone 的节点。
	if (NodeUtils::IsZoneScopedNodeType(nodeType) &&
		discoveredNode.zone_id() != gNode->GetNodeInfo().zone_id())
	{
		LOG_INFO << "Skip zone-scoped node from foreign zone. type=" << nodeType
				 << ", node_zone=" << discoveredNode.zone_id()
				 << ", self_zone=" << gNode->GetNodeInfo().zone_id()
				 << ", uuid=" << discoveredNode.node_uuid();
		return;
	}

	// 发布轨道(docs/design/release-track-gray-canary.md §2.5):全局服务的 canary 副本只接金丝雀会话经路由服来的
	// 客户端请求,服务间调用只连 stable。C++ 节点里只有 gate 选的是客户端请求的去向(路由服,以及直连模式下的业务服务,
	// 由 PickRandomNode 按 gate 自身轨道选);其余节点(scene、battle)对不分 zone 的类型只收 stable 节点,轨道认不出的也不收。
	// 必须在进发现快照之前拦下:NodeConnector::ConnectAllNodes 会把快照里的节点全部连上。本节点自己的注册记录照收(自检要用)。
	// zone 内类型不过这一关:一个 zone 只有一条轨道(§2.2)。这里不收的节点之后来的停止事件按 uuid 摘除,找不到即无事可做。
	if (!NodeUtils::IsZoneScopedNodeType(nodeType) &&
		static_cast<eNodeType>(gNode->GetNodeInfo().node_type()) != eNodeType::GateNodeService &&
		!gNode->IsCurrentNode(discoveredNode) &&
		release_track::Parse(discoveredNode.release_track()) != release_track::Track::kStable)
	{
		LOG_DEBUG << "Skip a non-stable global node for service-to-service calls, type: " << nodeType
				  << ", uuid: " << discoveredNode.node_uuid() << ", release_track: " << discoveredNode.release_track();
		return;
	}

	// Cancel any pending grace-period removal for this node.
	gNode->CancelPendingNodeRemoval(discoveredNode.node_uuid());

	const auto result = serviceNodes_.Upsert(nodeType, discoveredNode);
	if (result == ServiceNodeSnapshot::UpsertResult::kRejected)
	{
		return;
	}
	if (result == ServiceNodeSnapshot::UpsertResult::kAdded)
	{
		LOG_INFO << "Node added, type: " << nodeType
				 << ", node_id: " << discoveredNode.node_id()
				 << ", uuid: " << discoveredNode.node_uuid()
				 << ", ip: " << discoveredNode.endpoint().ip()
				 << ", port: " << discoveredNode.endpoint().port()
				 << ", release_track: " << discoveredNode.release_track();
		LOG_TRACE << "Node added detail: " << discoveredNode.DebugString();
		// 发布轨道认不出(docs/design/release-track-gray-canary.md §2.4):gate 给全局类型选实例时会跳过它。
		// 生产者对非法取值拒启,走到这里只可能是更新的版本带来了新取值;只在首次加入时记一次。
		if (!release_track::Parse(discoveredNode.release_track()))
		{
			LOG_ERROR << "Node advertises an unknown release track; it will be skipped when picking global instances, type: "
					  << nodeType << ", uuid: " << discoveredNode.node_uuid()
					  << ", release_track: " << discoveredNode.release_track();
		}
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
	if (NodeUtils::IsNodeTracked(nodeType, discoveredNode))
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
