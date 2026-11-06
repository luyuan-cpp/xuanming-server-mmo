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

	// 本节点实际 Range / watch 的前缀:配置的 service_discovery_prefixes 逐项经 NodeUtils::DiscoveryPrefixFor 收窄,
	// zone-scoped 类型只看本 zone。只在所属 loop 线程调用。
	std::vector<std::string> EffectiveDiscoveryPrefixes() const;

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
	TimerTaskComp serviceNodeSyncTimer_;
	// 抖动的随机源,只在 loop 线程用。
	std::mt19937 resyncJitter_{std::random_device{}()};
};
