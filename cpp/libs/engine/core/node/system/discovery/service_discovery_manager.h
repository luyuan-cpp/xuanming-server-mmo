#pragma once
#include "node/system/etcd/etcd_service.h"
#include "service_node_snapshot.h"
#include "time/comp/timer_task_comp.h"

#include <random>
#include <string>
#include <vector>

class ServiceDiscoveryManager {
public:
	// 同步定时器捕获 this,生命周期内禁止复制或移动宿主。
	ServiceDiscoveryManager() = default;
	ServiceDiscoveryManager(const ServiceDiscoveryManager&) = delete;
	ServiceDiscoveryManager& operator=(const ServiceDiscoveryManager&) = delete;
	ServiceDiscoveryManager(ServiceDiscoveryManager&&) = delete;
	ServiceDiscoveryManager& operator=(ServiceDiscoveryManager&&) = delete;

	void Init();

	void Shutdown();

	void FetchServiceNodes();

	// 本节点实际 Range / watch 的范围(docs/design/connection-fanout-subsetting-20261106.md §3.4):
	//   * 配置的 service_discovery_prefixes 里属于本节点发现集合(Node 构造时给的)的那些,zone-scoped 类型只看本 zone
	//     (NodeUtils::PlanDiscoveryScopes);配置里有、集合里没有的不看;
	//   * 自身类型不在集合里时,加上本节点自己那条发现键(精确键,WatchOwnRegistrationKey 之后才有)。
	// 只在所属 loop 线程调用。
	std::vector<std::string> EffectiveDiscoveryPrefixes() const;

	// 自身类型不在发现集合里时(battle),开始 Range 并 watch 本节点自己那条发现键:劫持检测、注册自检、battle 的
	// 可分配状态回读都只看它。node_id 定下之后调用一次(首次 StartRpcServer);自身类型在集合里或已开始过时什么也不做。
	// C++ 节点的 node_id 在进程内不变(重注册沿用原号),这条键之后也不变。
	void WatchOwnRegistrationKey();

	void AddServiceNode(const std::string& nodeJson, uint32_t nodeType);

	void HandleServiceNodeStart(const std::string& key, const std::string& value);

	// 返回的快照及其借用列表只在所属 loop 线程访问。
	const ServiceNodeSnapshot &ServiceNodes() const { return serviceNodes_; }
	// 仅供 Node 的 etcd DELETE / 宽限期收尾移除快照,仍在所属 loop 线程执行。
	int RemoveServiceNode(uint32_t nodeType, const std::string &nodeUuid)
	{
		return serviceNodes_.Remove(nodeType, nodeUuid);
	}

	EtcdService etcdService;

private:
	// 排下一次兜底全量 Range(discovery_resync_policy.h)。定时器捕获 this,见类头的禁止复制 / 移动。
	void ScheduleNextResync();

	ServiceNodeSnapshot serviceNodes_;
	// 自身类型不在发现集合里时本节点自己的发现键(精确键);空 = 自身类型按前缀看,或 node_id 还没定。
	std::string ownRegistrationKey_;
	TimerTaskComp serviceNodeSyncTimer_;
	// 抖动的随机源,只在 loop 线程用。
	std::mt19937 resyncJitter_{std::random_device{}()};
};
