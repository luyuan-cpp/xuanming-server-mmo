#pragma once

#include <cstdint>
#include <string_view>
#include <unordered_set>
#include <vector>

class NodeAllocator {
public:
	// 选最小空闲号(node_id_selection.h),排除:发现快照里的同类节点、本次启动输掉的号、最近一次读回的全服分配清单。
	// 快照只回答「本节点看得到的同类」:zone-scoped 类型只看本 zone,battle 只看自己,所以全服清单才是主依据。
	static void SelectNodeId();

	// 分配键 CAS 失败时,同一个 txn 读回了全服这一类型已分配的键(只取键,EtcdHelper::PutIfAbsentListingOnConflict)。
	// 整份替换上一份清单:它是那一刻的快照,之后被别人拿走的号照旧靠 CAS 失败发现。认不出的键忽略。
	static void RecordAllocatedIds(std::string_view allocationPrefix, const std::vector<std::string_view> &allocatedKeys);

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
	// 最近一次 CAS 失败读回的全服分配清单(RecordAllocatedIds 整份替换)。只在注册所在的 loop 线程读写。
	static std::unordered_set<uint32_t>& allocatedIds();
};
