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
	//
	// 还要排除自己:端口注册成功后 etcd watch 会把本节点自己 fan 回这份快照,而
	// EtcdService::AcquirePortWithRetry() 在"等待到期后重查权威"的分支里会再跑一次。
	// 不排除自己的话,重跑时会把自己刚注册的端口当成别人占用的 —— 扫描路径会莫名
	// 其妙换一个端口(etcd 里的端口从此和第一次注册的劈叉),预设端口路径则会永远
	// 失败重试。
	const auto &localIp = GetNodeInfo().endpoint().ip();
	const auto &selfUuid = GetNodeInfo().node_uuid();
	std::unordered_set<uint32_t> usedPorts;
	for (const auto &node : existingNodes)
	{
		if (node.endpoint().ip() != localIp)
		{
			continue;
		}

		if (!selfUuid.empty() && node.node_uuid() == selfUuid)
		{
			continue;
		}

		usedPorts.insert(node.endpoint().port());
	}

	uint32_t assignedPort = 0;

	// 显式配置优先于自动扫描:InitRpcServer() 若从 RPC_PORT / NODE_PORT 读到了端口,
	// 这里就以它为准,只做可用性校验,不再扫区间。
	//
	// 这条分支是必需的而不是锦上添花:K8s 的 Deployment / Fleet 用同一个值同时声明
	// containerPort 和 Service 的 targetPort,进程要是自作主张换一个端口,Service 就会
	// 把流量转发到一个没人监听的端口上;而日志里偏偏还留着一行
	// "Node port from environment: <预设值>",排查时极具误导性。
	//
	// 刻意 fail-closed:预设端口拿不到就返回 false 让调用方退避重试,绝不静默回落到
	// 扫描区间 —— 回落等于把"我按你指定的端口起"悄悄变成"我随便挑了一个",
	// 正是这次要修的病。
	const uint32_t presetPort = GetNodeInfo().endpoint().port();
	if (presetPort != 0)
	{
		if (usedPorts.find(presetPort) != usedPorts.end())
		{
			LOG_ERROR << "Preset RPC port " << presetPort
					  << " is already registered in etcd for ip=" << localIp
					  << "; nothing published to etcd, caller must retry";
			return false;
		}

		if (!IsLocalPortAvailable(static_cast<uint16_t>(presetPort)))
		{
			LOG_ERROR << "Preset RPC port " << presetPort
					  << " is not bindable on this host"
					  << "; nothing published to etcd, caller must retry";
			return false;
		}

		assignedPort = presetPort;
		LOG_INFO << "Using preset RPC port from environment: " << assignedPort;
	}
	// Ops convention: Gate and non-Gate nodes use separate TCP ranges so
	// that port numbers alone reveal the node role.  Firewall / LB rules
	// can target each range independently.
	//
	//   Gate   TCP: 10000-19999   gRPC: 40000-49999
	//   Other  TCP: 20000-35535   gRPC: 50000-65535
	//
	// Protocol isolation comes from the +30000 offset, not the ranges;
	// the ranges are purely an operational convenience.
	else if (IsGateNodeType(GetNodeInfo().node_type()))
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

	// 扫描游标只在真的扫过区间时才推进。预设端口路径没参与扫描,推进它只会让同机
	// 下一个节点无缘无故跳过一段区间。
	if (presetPort == 0)
	{
		tryPortId = assignedPort + 1;
	}

	GetNodeInfo().mutable_endpoint()->set_port(assignedPort);

	// gRPC port = TCP port + 30000 (deterministic, separate range).
	//
	// 注册了 gRPC 服务的节点(battle / match 这类只靠 gRPC 被调用的节点),gRPC 端口拿不到
	// 必须与预设 TCP 端口同样 fail-closed:以前这里只打一条 ERROR 就带着空 grpc_endpoint
	// 发布到 etcd,节点"活着"却没人连得上 —— gate 循环报 "grpc_endpoint is empty",
	// match 因 battle 池为空永远不凑单(2026-09-04 实测:杀掉 battle 后立刻重拉,旧进程的
	// 50100 还没释放,新进程就这样带病注册了)。改为不发布、让调用方退避重试:预设端口
	// 会等到端口释放,扫描路径则因游标已推进而换下一个 TCP/gRPC 端口对。
	if (!gNode->GetGrpcServices().empty())
	{
		const uint32_t grpcPort = assignedPort + kGrpcPortOffset;
		if (!IsLocalPortAvailable(static_cast<uint16_t>(grpcPort)))
		{
			LOG_ERROR << "gRPC port " << grpcPort << " (TCP " << assignedPort
					  << " + " << kGrpcPortOffset << ") not available"
					  << "; nothing published to etcd, caller must retry";
			// 把 TCP 端口退回本轮之前的值:扫描路径要保持 0(下轮继续扫),
			// 否则重试时会把刚挑到的端口误当成"预设端口"死等。
			GetNodeInfo().mutable_endpoint()->set_port(presetPort);
			return false;
		}
		GetNodeInfo().mutable_grpc_endpoint()->set_ip(GetNodeInfo().endpoint().ip());
		GetNodeInfo().mutable_grpc_endpoint()->set_port(grpcPort);
		LOG_INFO << "Assigned gRPC port: " << grpcPort
				 << " (TCP " << assignedPort << " + " << kGrpcPortOffset << ")";
	}

	LOG_INFO << "NodeType: " << gNode->GetNodeType()
			 << " IP: " << GetNodeInfo().endpoint().ip()
			 << " Port: " << assignedPort;

	gNode->GetEtcdManager().RegisterNodePort();
	return true;
}
