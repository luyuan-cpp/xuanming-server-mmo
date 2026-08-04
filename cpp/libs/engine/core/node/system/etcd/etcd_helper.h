#pragma once

#include <string>
#include "proto/common/base/common.pb.h"

namespace EtcdHelper
{
	void PutServiceNodeInfo(const NodeInfo &nodeInfo, const std::string &key, int64_t lease = 0);
	void RangeQuery(const std::string &prefix);
	void StartWatchingPrefix(const std::string &prefix, int64_t revision);
	void StopAllWatching(); // placeholder — extensible
	void GrantLease(uint32_t ttlSeconds);
	void PutIfAbsent(const std::string &key, const std::string &newValue, int64_t currentVersion, int64_t lease);
	void PutIfAbsent(const std::string &key, const NodeInfo &nodeInfo, int64_t lease);
	// 无条件把 key 挂到 lease 上(txn 不带 compare,恒成功),走同一套 txn 回调,
	// 因此 OnTxnSucceeded / pendingTxnKey 流程不变。
	// 只给"这个 key 按定义就该属于自己"的场景用,例如按 IP+端口作用域的端口 key 重注册。
	void PutWithLease(const std::string &key, const std::string &newValue, int64_t lease);
	void RevokeLeaseAndCleanup(int64_t leaseId);
	void DeleteRange(const std::string &key, bool isPrefix);
}
