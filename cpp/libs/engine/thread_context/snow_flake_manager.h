#pragma once

#include "core/utils/id/snow_flake.h"
#include <engine/core/type_define/type_define.h>

// 每线程一份的业务发号器。
//
// 唯一性作用域 = 单个 SnowFlakeManager 对象:同一个 node_id 下,两个线程各自的
// 第 K 个号在同一秒里是**逐位相同**的。因此:
//   * 只有调用过 OnNodeStart() 的线程才允许发号;
//   * 没初始化就发号会静默用 node_id=0(NodeAllocator 保留给"未分配"的哨兵值),
//     跨进程必然撞号 —— 这里 fail-closed 返回 kInvalidGuid 并打 ERROR,
//     而不是让一个坏 ID 流进背包 / 快照 / 交易流水。
//
// Fence():本进程失去 node_id 的 etcd 租约后调用。此时另一个进程可能已经用同一个
// node_id 在发号,继续发就是确定性撞号。fence 之后一律拒发,只保留存盘 / 迁移这类
// 不需要新 ID 的收尾动作。
class SnowFlakeManager
{
public:

	void OnNodeStart(uint32_t nodeId) {
		itemIdGenerator_.set_node_id(nodeId);
		nodeId_ = nodeId;
		initialized_ = true;
	}

	void SetGuardTime(uint64_t guardUtcSeconds) {
		itemIdGenerator_.SetGuardTime(guardUtcSeconds);
	}

	// 失租 / 身份被抢占后停止发号。幂等。
	void Fence() {
		if (!fenced_) {
			fenced_ = true;
			LOG_WARN << "[SnowFlake] fenced: node_id=" << nodeId_
				<< " is no longer owned by this process; refusing to mint further IDs";
		}
	}

	[[nodiscard]] bool IsFenced() const { return fenced_; }
	[[nodiscard]] bool IsInitialized() const { return initialized_; }

	Guid GenerateItemGuid() {
		if (!initialized_) {
			LOG_ERROR << "[SnowFlake] GenerateItemGuid on a thread that never ran OnNodeStart; "
				<< "node segment would be 0 (reserved) and collide across processes";
			return kInvalidGuid;
		}
		if (fenced_) {
			LOG_ERROR << "[SnowFlake] GenerateItemGuid after fence (node_id=" << nodeId_
				<< "); refusing to mint";
			return kInvalidGuid;
		}
		lastGeneratedItemGuid_ = itemIdGenerator_.Generate();
		return lastGeneratedItemGuid_;
	}

	[[nodiscard]] Guid GetLastGeneratedItemGuid() const { return lastGeneratedItemGuid_; }

private:
	Guid lastGeneratedItemGuid_{ kInvalidGuid };
	SnowFlake itemIdGenerator_;
	uint32_t nodeId_{ 0 };
	bool initialized_{ false };
	bool fenced_{ false };
};

extern thread_local SnowFlakeManager tlsSnowflakeManager;
