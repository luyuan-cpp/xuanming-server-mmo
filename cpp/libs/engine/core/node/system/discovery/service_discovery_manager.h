#pragma once
#include "node/system/etcd/etcd_service.h"
#include "time/comp/timer_task_comp.h"

class ServiceDiscoveryManager {
public:
	void Init();

	void Shutdown();

	void FetchServiceNodes();

	void AddServiceNode(const std::string& nodeJson, uint32_t nodeType);

	void HandleServiceNodeStart(const std::string& key, const std::string& value);

	EtcdService etcdService;

private:
	TimerTaskComp serviceNodeSyncTimer_;
};
