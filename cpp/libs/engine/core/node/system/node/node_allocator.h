#pragma once

#include <cstdint>
#include <unordered_set>

class NodeAllocator {
public:
	static void SelectNodeId();

	// Returns false when no RPC port could be allocated. The caller must retry;
	// nothing is published to etcd on failure. Publishing port=0 would advertise
	// an endpoint peers cannot connect to and is never a valid outcome.
	static bool AcquireNodePort();

	// 记录本进程已抢号失败的 id，跨定时重试保留，不等待服务发现快照追上获胜节点。
	static void RecordLostId(uint32_t id);

private:
	// Process-local set of node_ids whose global allocation CAS we have
	// already lost during this boot. Cleared only on process exit.
	static std::unordered_set<uint32_t>& lostIds();
};
