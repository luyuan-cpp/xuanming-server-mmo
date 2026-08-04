#include "node_allocator.h"
#include <muduo/base/Logging.h>
#include "node.h"
#include "node/system/etcd/etcd_manager.h"
#include "node/system/etcd/etcd_helper.h"
#include <network/node_utils.h>
#include <thread_context/redis_manager.h>
#include <core/utils/id/snow_flake.h>

#ifdef _WIN32
#include <WinSock2.h>
#pragma comment(lib, "ws2_32.lib")
#else
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <unistd.h>
#endif

uint32_t tryPortId{0};

std::unordered_set<uint32_t>& NodeAllocator::lostIds()
{
	static std::unordered_set<uint32_t> ids;
	return ids;
}

void NodeAllocator::RecordLostId(uint32_t id)
{
	lostIds().insert(id);
}

void NodeAllocator::AcquireNode()
{
	const uint32_t nodeType = gNode->GetNodeType();
	LOG_INFO << "Acquiring node ID for node type: " << nodeType;

	// All services pick next free ID in local snapshot,
	// then rely on etcd PutIfAbsent CAS to enforce same-type uniqueness.
	auto &nodeList = tlsEcs.nodeGlobalRegistry.get_or_emplace<ServiceNodeList>(tlsEcs.GrpcNodeEntity())[nodeType];
	auto &existingNodes = *nodeList.mutable_node_list();

	std::unordered_set<uint32_t> usedIds;
	uint32_t maxUsedId = 0;

	for (const auto &node : existingNodes)
	{
		usedIds.insert(node.node_id());
		if (node.node_id() > maxUsedId)
		{
			maxUsedId = node.node_id();
		}
	}

	// Merge in ids we already lost CAS on this boot. The local ECS snapshot
	// can lag the etcd watch stream by hundreds of ms during cold start, so
	// without this we'd retry the same id forever (the snapshot stays empty
	// because watch events haven't fanned out yet).
	for (uint32_t id : lostIds())
	{
		usedIds.insert(id);
		if (id > maxUsedId)
		{
			maxUsedId = id;
		}
	}

	static constexpr uint32_t kMaxNodeId = static_cast<uint32_t>(kNodeMask);
	uint32_t nextNodeId;

	// Always scan upward through gaps so a previously-lost id slot becomes
	// reusable as soon as its lease drops (existingNodes-derived usedIds
	// gets cleaned up by the discovery layer; lostIds is process-local and
	// only matters for the same boot).
	bool found = false;
	for (uint32_t id = 1; id <= kMaxNodeId; ++id)
	{
		if (usedIds.find(id) == usedIds.end())
		{
			nextNodeId = id;
			found = true;
			break;
		}
	}
	if (!found)
	{
		LOG_FATAL << "No available node ID (max " << kMaxNodeId << ")";
		throw std::runtime_error("Node ID space exhausted");
	}

	(void)maxUsedId; // retained for log/debug parity; iteration starts at 0

	GetNodeInfo().set_node_id(nextNodeId);

	gNode->GetEtcdManager().RegisterNodeService();
}

void NodeAllocator::ReRegisterExistingNode()
{
	// Re-registration must keep existing node_id to preserve SnowFlake worker bits
	// and avoid mid-flight identity change after temporary lease loss.
	LOG_INFO << "Re-registering existing node with node_id=" << GetNodeInfo().node_id()
			 << " port=" << GetNodeInfo().endpoint().port();
	// reRegistering=true:端口 key 仍挂在旧租约上,必须无条件改挂新租约。
	// 见 EtcdManager::RegisterNodePort 的注释 —— 用 PutIfAbsent 会必然 CAS 失败,
	// 而重注册模式下的 OnTxnFailed 会把它判成"身份被抢"并自杀。
	gNode->GetEtcdManager().RegisterNodePort(/*reRegistering=*/true);
}

bool IsGateNodeType(uint32_t type)
{
	return type == eNodeType::GateNodeService;
}

bool IsLocalPortAvailable(uint16_t port)
{
#ifdef _WIN32
	SOCKET sock = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
	if (sock == INVALID_SOCKET)
	{
		return false;
	}
#else
	int sock = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
	if (sock < 0)
	{
		return false;
	}
#endif

	sockaddr_in addr{};
	addr.sin_family = AF_INET;
	addr.sin_addr.s_addr = INADDR_ANY;
	addr.sin_port = htons(port);

	int optval = 1;
	::setsockopt(sock, SOL_SOCKET, SO_REUSEADDR,
				 reinterpret_cast<const char *>(&optval), sizeof(optval));

	bool available = (::bind(sock, reinterpret_cast<const sockaddr *>(&addr), sizeof(addr)) == 0);
#ifdef _WIN32
	::closesocket(sock);
#else
	::close(sock);
#endif
	return available;
}

uint32_t AllocatePortInRange(const std::unordered_set<uint32_t> &usedPorts,
							 uint32_t minPort, uint32_t maxPort, uint32_t tryPortId)
{
	// Scan from tryPortId to maxPort first
	for (uint32_t port = tryPortId; port <= maxPort; ++port)
	{
		if (usedPorts.find(port) == usedPorts.end() && IsLocalPortAvailable(static_cast<uint16_t>(port)))
		{
			return port;
		}
	}

	// Wrap around from minPort to tryPortId - 1
	for (uint32_t port = minPort; port < tryPortId; ++port)
	{
		if (usedPorts.find(port) == usedPorts.end() && IsLocalPortAvailable(static_cast<uint16_t>(port)))
		{
			return port;
		}
	}

	return 0; // No available port
}

bool NodeAllocator::AcquireNodePort()
{
	auto &nodeList = tlsEcs.nodeGlobalRegistry.get_or_emplace<ServiceNodeList>(tlsEcs.GrpcNodeEntity())[gNode->GetNodeType()];
	auto &existingNodes = *nodeList.mutable_node_list();

	// Port layout (TCP + 30000 = gRPC, non-overlapping):
	//
	//   Gate   TCP: 10000-19999   gRPC: 40000-49999
	//   Other  TCP: 20000-35535   gRPC: 50000-65535
	//
	// Protocol isolation: TCP and gRPC ranges never intersect.
	// IP isolation:       only same-IP ports are excluded.
	// Range convention:   see port → know node role + firewall rules.
	constexpr uint32_t kGrpcPortOffset = 30000;

	// Only exclude ports from nodes on the SAME IP — nodes on different
	// machines can safely reuse the same port numbers.
	const auto &localIp = GetNodeInfo().endpoint().ip();
	std::unordered_set<uint32_t> usedPorts;
	for (const auto &node : existingNodes)
	{
		if (node.endpoint().ip() == localIp)
		{
			usedPorts.insert(node.endpoint().port());
		}
	}

	uint32_t assignedPort = 0;

	// Ops convention: Gate and non-Gate nodes use separate TCP ranges so
	// that port numbers alone reveal the node role.  Firewall / LB rules
	// can target each range independently.
	//
	//   Gate   TCP: 10000-19999   gRPC: 40000-49999
	//   Other  TCP: 20000-35535   gRPC: 50000-65535
	//
	// Protocol isolation comes from the +30000 offset, not the ranges;
	// the ranges are purely an operational convenience.
	if (IsGateNodeType(GetNodeInfo().node_type()))
	{
		constexpr uint32_t GATE_MIN = 10000;
		constexpr uint32_t GATE_MAX = 19999;
		if (tryPortId < GATE_MIN || tryPortId > GATE_MAX)
		{
			tryPortId = GATE_MIN;
		}
		assignedPort = AllocatePortInRange(usedPorts, GATE_MIN, GATE_MAX, tryPortId);
		LOG_INFO << "Assigned Gate TCP port: " << assignedPort;
	}
	else
	{
		constexpr uint32_t OTHER_MIN = 20000;
		constexpr uint32_t OTHER_MAX = 65535 - kGrpcPortOffset; // 35535
		if (tryPortId < OTHER_MIN || tryPortId > OTHER_MAX)
		{
			tryPortId = OTHER_MIN;
		}
		assignedPort = AllocatePortInRange(usedPorts, OTHER_MIN, OTHER_MAX, tryPortId);
		LOG_INFO << "Assigned TCP port: " << assignedPort;
	}

	if (assignedPort == 0)
	{
		// fail-closed:绝不把 port=0 写进 NodeInfo 再注册到 etcd。
		// 旧实现照样 set_port(0) + RegisterNodePort(),于是这个节点会以
		// "endpoint=ip:0" 出现在服务发现里,别的节点拿着 0 端口去连,只会得到
		// 一串无法解释的连接失败,而且该节点仍然占着一个 node_id。
		LOG_ERROR << "No available RPC port found (TryPortId was " << tryPortId
				  << "); nothing published to etcd, caller must retry";
		tryPortId = 0; // 下轮从区间头重扫
		return false;
	}

	tryPortId = assignedPort + 1;

	GetNodeInfo().mutable_endpoint()->set_port(assignedPort);

	// gRPC port = TCP port + 30000 (deterministic, separate range).
	if (!gNode->GetGrpcServices().empty())
	{
		const uint32_t grpcPort = assignedPort + kGrpcPortOffset;
		if (!IsLocalPortAvailable(static_cast<uint16_t>(grpcPort)))
		{
			LOG_ERROR << "gRPC port " << grpcPort << " (TCP " << assignedPort
					  << " + " << kGrpcPortOffset << ") not available.";
		}
		else
		{
			GetNodeInfo().mutable_grpc_endpoint()->set_ip(GetNodeInfo().endpoint().ip());
			GetNodeInfo().mutable_grpc_endpoint()->set_port(grpcPort);
			LOG_INFO << "Assigned gRPC port: " << grpcPort
					 << " (TCP " << assignedPort << " + " << kGrpcPortOffset << ")";
		}
	}

	LOG_INFO << "NodeType: " << gNode->GetNodeType()
			 << " IP: " << GetNodeInfo().endpoint().ip()
			 << " Port: " << assignedPort;

	gNode->GetEtcdManager().RegisterNodePort();
	return true;
}
