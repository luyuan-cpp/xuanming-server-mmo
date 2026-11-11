#pragma once
#include "proto/common/base/node.pb.h"
#include "proto/common/base/common.pb.h"
#include "entt/src/entt/entt.hpp"
#include <muduo/net/TcpConnection.h>
#include <algorithm>
#include <optional>
#include <set>
#include <string>
#include <vector>

// Re-export common::base types at global scope for backward compatibility.
using common::base::eNodeProtocolType;
using common::base::eNodeType;
using common::base::eSceneNodeType;

using common::base::eNodeType_ARRAYSIZE;
using common::base::eNodeType_IsValid;
using common::base::eNodeType_Name;
using common::base::eSceneNodeType_ARRAYSIZE;
using common::base::eSceneNodeType_Name;

// Re-export eNodeType enumerator values.
using common::base::ActivityNodeService;
using common::base::AiNodeService;
using common::base::AnalyticsNodeService;
using common::base::BattleNodeService;
using common::base::ChatNodeService;
using common::base::ClientRpcRouterNodeService;
using common::base::CrossServerNodeService;
using common::base::DataServiceNodeService;
using common::base::EtcdNodeService;
using common::base::FriendNodeService;
using common::base::GateNodeService;
using common::base::GmNodeService;
using common::base::GuildNodeService;
using common::base::LoginNodeService;
using common::base::LogNodeService;
using common::base::MailNodeService;
using common::base::MatchNodeService;
using common::base::PaymentNodeService;
using common::base::PlayerInboxNodeService;
using common::base::PlayerLocatorNodeService;
using common::base::PlayerStoreNodeService;
using common::base::RankNodeService;
using common::base::RedisNodeService;
using common::base::SceneManagerNodeService;
using common::base::SceneNodeService;
using common::base::SecurityNodeService;
using common::base::TaskNodeService;
using common::base::TeamNodeService;
using common::base::TradeNodeService;
using common::base::UnknownNodeService;

// Re-export eSceneNodeType enumerator values.
using common::base::kMainSceneCrossNode;
using common::base::kMainSceneNode;
using common::base::kSceneNode;
using common::base::kSceneSceneCrossNode;

// Re-export eNodeProtocolType enumerator values.
using common::base::PROTOCOL_GRPC;
using common::base::PROTOCOL_HTTP;
using common::base::PROTOCOL_TCP;

namespace NodeUtils
{
	// GateNodeService -> "gate", SceneNodeService -> "scene", etc.
	inline std::string NodeTypeToShortName(uint32_t nodeType)
	{
		std::string name = eNodeType_Name(static_cast<eNodeType>(nodeType));
		const std::string suffix = "NodeService";
		if (name.size() > suffix.size() &&
			name.compare(name.size() - suffix.size(), suffix.size(), suffix) == 0)
		{
			name.resize(name.size() - suffix.size());
		}
		std::transform(name.begin(), name.end(), name.begin(),
					   [](unsigned char c)
					   { return static_cast<char>(std::tolower(c)); });
		return name;
	}

	eNodeType GetServiceTypeFromPrefix(const std::string &prefix);
	entt::registry &GetRegistryForNodeType(uint32_t nodeType);
	// Linear scan through the registry's NodeInfo view; matches the FIRST node
	// with the given node_id, regardless of zone. Use only when zone is known
	// to be unique (e.g. self-zone GateNode). Prefer FindNodeEntityByZoneAndNodeId.
	std::optional<entt::entity> FindNodeEntityByNodeId(uint32_t nodeType, uint32_t nodeId);
	// Multi-zone safe lookup: matches both zone_id and node_id. Use this for
	// any cross-zone capable code path (login routing, gate↔scene reverse
	// lookup, etc.) so that node_id collisions across zones do not alias.
	std::optional<entt::entity> FindNodeEntityByZoneAndNodeId(uint32_t nodeType, uint32_t zoneId, uint32_t nodeId);
	std::optional<entt::entity> FindNodeEntityByUuid(uint32_t nodeType, const std::string &nodeUuid);
	std::string GetRegistryName(const entt::registry &registry);
	eNodeType GetRegistryType(const entt::registry &registry);
	bool IsSameNode(const std::string &uuid1, const std::string &uuid2);
	// 仅查本线程登记：TCP 要有客户端组件，gRPC 要有节点记录；不表示连接已建立或业务就绪。
	bool IsNodeTracked(uint32_t nodeType, const NodeInfo &info);

	// 摘掉所有(任意节点类型注册表里)仍引用这条 TcpConnection 的 RpcSession 组件,返回摘掉的个数。
	// 给 RpcServer 的 DOWN 回调用。RpcSession::connection 是 weak_ptr,不摘也不会钉住连接对象;
	// 摘是为了让路由方立刻看到"节点不可达",而不是留一个 lock() 永远失败的空壳等到 etcd 键过期。
	// 匹配按 lock() 后的指针精确比对,顺带清掉 weak 已过期的会话。
	// 只能在 loop 线程调用(tlsNodeContextManager 是 thread_local)。
	// 见 docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md §7.4。
	std::size_t RemoveRpcSessionsBoundTo(const muduo::net::TcpConnectionPtr &conn);

	// 节点关机阶段 0:关闭全部节点间 RPC0 通道(各 registry 里出站 RpcClient 与入站 RpcSession 的 channel)。
	// 在途调用以 kChannelClosed 失败,回调经 queueInLoop 投递 —— 调用方随后必须把拆除排在这些回调之后
	// (Node::FinalizeShutdownInLoop 的两段式)。关闭后的 channel 拒收新调用,但已接管的延后回复仍可在连接存活时发出。
	// 只能在 loop 线程调用。docs/design/muduo-rpc-channel.md §6.5。
	void CloseAllRpcChannels();

	// True for services whose node_id is only unique within a zone and whose
	// runtime contract is "caller only talks to the same zone". The local
	// service registry on each node must refuse to insert such nodes from
	// foreign zones; otherwise node_id collisions (e.g. LoginNode node_id=1
	// present in zones 2, 3 and 4) alias different zones onto the same
	// entt::entity slot and break PickRandomNode's zone filter.
	//
	//   zone-scoped (local-only)  : GateNode, SceneNode, LoginNode, PlayerLocator, PlayerStore, SceneManager
	//   cross-zone (global pool)  : DataService, Match, Battle, ClientRpcRouter, PlayerInbox, Team, Chat/Guild/...
	//
	// SceneManager 2026-11-05 起是 zone-scoped(docs/design/control-plane-zone-scoping-20261105.md §3.1):
	// 每个 zone 有自己的 scene_manager(按 zone 选主、只编排本 zone),gate / scene 只连本 zone 的实例。
	// 跨 zone 重定向仍由本 zone 的 scene_manager 签发,不需要连别的 zone 的实例。
	// DataService runs one logical pool for global data (e.g. account lookup) that every zone shares.
	//
	// 这份分类同时决定发现前缀(DiscoveryPrefixFor):zone-scoped 类型只 Range / watch 本 zone 的前缀。
	bool IsZoneScopedNodeType(uint32_t nodeType);

	// 配置里的一个发现前缀(service_discovery_prefixes 的一项,形如 "SceneNodeService.rpc")在本节点实际 Range / watch 的前缀。
	// zone-scoped 类型返回 "<prefix>/zone/<zoneId>/",与 EtcdManager::MakeNodeEtcdPrefix 的键形一致;别的 zone 的键与
	// 分配键 "<prefix>/allocated/..." 都不在里面。其余类型、认不出的前缀原样返回(全局池照旧看全服)。
	// 纯函数,只按字面精确匹配 "<eNodeType 名>.rpc",不做子串匹配。
	std::string DiscoveryPrefixFor(const std::string &configuredPrefix, uint32_t zoneId);

	// 配置的一个发现前缀对应的节点类型:只认字面精确的 "<eNodeType 名>.rpc",认不出返回 nullopt。
	std::optional<uint32_t> NodeTypeOfDiscoveryPrefix(const std::string &configuredPrefix);

	// 本节点实际要 Range / watch 的前缀(docs/design/connection-fanout-subsetting-20261106.md §3.4)。
	// 配置的前缀表只回答「部署里有哪些服务」;本节点只看其中属于自己发现集合的类型,zone-scoped 类型再收窄到本 zone
	// (DiscoveryPrefixFor)。纯函数。自身类型不在发现集合里时本节点自己那条发现键另由 ServiceDiscoveryManager 加上。
	struct DiscoveryScopePlan
	{
		// 要 Range / watch 的前缀,按配置里的顺序,去重。
		std::vector<std::string> scopes;
		// 配置里有、但不属于发现集合(或认不出)的前缀:本节点不看。
		std::vector<std::string> skippedPrefixes;
		// 发现集合里有、配置里没有的类型:部署里没有这个服务,读它的调用方照旧报找不到。
		std::vector<uint32_t> missingTypes;
	};
	DiscoveryScopePlan PlanDiscoveryScopes(const std::vector<std::string> &configuredPrefixes,
										   const std::set<uint32_t> &discoverTypes, uint32_t zoneId);

	// 全局池节点类型:实例不分 zone,任一 zone 的调用方都可以路由过去。
	// gate 的 PickRandomNode 对这两类**不比对 zone**(设计文档
	// cross-zone-matchmaking.md D11):match 是无状态编排者、battle 是纯内存房间,
	// 排队池 / 房间池全服共享。只列明确的全局池类型,不扩大到其他非 zone-scoped
	// 服务(friend/guild/chat 的路由语义保持原样)。
	//
	// 注意:(node_type, node_id) 由 etcd allocation key **全局** CAS 分配
	// (Go noderegistry / C++ etcd_service 的 allocated/ 路径都不带 zone),所以
	// 跨 zone 路由后 gate-{id} / scene-{id} 这类 Kafka topic 不会撞名;
	// 上面 IsZoneScopedNodeType 注释里"node_id 只在 zone 内唯一"是历史防御性
	// 措辞,分配器的实际行为是全局唯一。
	bool IsGlobalPoolNodeType(uint32_t nodeType);

	// 纯 gRPC 协议的 C++ 节点:不对外提供节点间 muduo TCP RPC 服务,注册进 etcd 的
	// NodeInfo.protocol_type 必须是 PROTOCOL_GRPC —— C++ 发现方(node_connector)
	// 按 protocol_type 分派连接方式;哪个 C++ 节点的建连白名单里有 battle,标错成 TCP 就会让它
	// 用 RpcCodec 反复去拨 battle 的 TCP 端口(那里挂的是客户端战斗直连面,走客户端 ProtobufCodec
	// 帧而非节点间 RpcCodec),而且永远建不出 gRPC stub。现在没有 C++ 节点连 battle:各节点的发现
	// 集合都不含 Battle,battle 自己只看自己那一条发现键(docs/design/connection-fanout-subsetting-20261106.md
	// §3.4);判定照旧保留,它决定 battle 以 PROTOCOL_GRPC 注册。
	// Go match 只认 grpc_endpoint,不受 protocol_type 影响。
	// TCP 端口照常分配(gRPC 端口 = TCP + 30000 的派生规则依赖它,见
	// node_allocator.cpp),且不是空闲占位:battle 自身在上面挂载客户端战斗直连面
	// (BattleClientEdge),不可回收。节点间控制面只剩 Go match 经 grpc_endpoint
	// 调用(turn-based §22 D66)。
	//
	//   目前:BattleNodeService(回合制战斗,全局池,gRPC only)
	bool IsGrpcOnlyNodeType(uint32_t nodeType);
};
