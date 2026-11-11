#pragma once

#include <string>
#include <vector>
#include "proto/common/base/common.pb.h"
#include "proto/etcd/etcd.pb.h"
#include "node/system/etcd/etcd_transport.h"

namespace EtcdHelper
{
	void PutServiceNodeInfo(const NodeInfo &nodeInfo, const std::string &key, int64_t lease = 0);
	void RangeQuery(const std::string &prefix);
	void StartWatchingPrefix(const std::string &prefix, int64_t revision);
	// 精确键(range_end 为空):只 Range / watch 这一个键。发现键 `.../node_id/1` 当前缀用会把 10、11、100 号也套进来,
	// 本节点只看自己那一条登记时用这两个(docs/design/connection-fanout-subsetting-20261106.md §3.4)。
	void RangeKey(const std::string &key);
	void StartWatchingKey(const std::string &key, int64_t revision);
	void StopAllWatching(); // placeholder — extensible
	using TxnCompletion = EtcdRegistrationCompletion<etcdserverpb::TxnResponse>;
	void GrantLease(uint32_t ttlSeconds, EtcdRegistrationCompletion<etcdserverpb::LeaseGrantResponse> done);
	void PutIfAbsent(const std::string &key, const std::string &newValue, int64_t currentVersion, int64_t lease,
	                 TxnCompletion done, bool checkSameLease = false);
	void PutIfAbsent(const std::string &key, const NodeInfo &nodeInfo, int64_t lease, TxnCompletion done,
	                 bool checkSameLease = false);
	// 与 PutIfAbsent(VERSION == 0 的 CAS)相同,另在失败分支里只取键(keys_only)列出 listPrefix 下的全部键,
	// 一次往返就知道冲突时这一前缀已被占了哪些键。给 node_id 分配键用(NodeAllocator::RecordAllocatedIds)。
	void PutIfAbsentListingOnConflict(const std::string &key, const std::string &value, int64_t lease,
	                                  const std::string &listPrefix, TxnCompletion done);
	// PutIfAbsentListingOnConflict 失败时失败分支的清单;成功、或不是这种 txn 的应答返回 nullptr。
	const etcdserverpb::RangeResponse *TxnConflictListing(const etcdserverpb::TxnResponse &reply);
	// 无条件把 key 挂到 lease 上(txn 不带 compare,恒成功)。
	// 只给"这个 key 按定义就该属于自己"的场景用,例如按 IP+端口作用域的端口 key 重注册。
	void PutWithLease(const std::string &key, const std::string &newValue, int64_t lease, TxnCompletion done);
	void PutWithLease(const std::string &key, const NodeInfo &nodeInfo, int64_t lease, TxnCompletion done);

	// "不存在,或者已经是我的"才写 —— 重注册(临时失租后拿新租约)专用的 CAS。
	//
	// etcd 的 compare 只能 AND,没有 OR,所以用嵌套 txn 表达:
	//   If  CreateRevision(key)==0
	//   Then Put(key, value, lease) + extraSuccessOps
	//   Else Txn{ If Value(key)==ownerValue Then Put(key, value, lease) + extraSuccessOps
	//                                       Else Range(key) }           # 拿到真正的持有者供日志
	// 外层 succeeded 只反映外层 compare,判成功用 TxnClaimSucceeded()。
	//
	// 为什么不能继续用 VERSION==0:RequestReRegistration 只在旧租约仍健康时才触发,此刻
	// key 还挂在旧租约上,VERSION==0 必然失败;而重注册模式下的 OnTxnFailed 把任何失败
	// 判成"身份被抢"并自杀 —— 每次重注册都确定性地把自己杀掉(端口 key 那次先修的同一类病)。
	etcdserverpb::TxnRequest BuildClaimIfAbsentOrOwnedTxn(const std::string &key,
														   const std::string &value,
														   const std::string &ownerValue,
														   int64_t lease,
														   const std::vector<etcdserverpb::RequestOp> &extraSuccessOps = {});
	void PutIfAbsentOrOwned(const std::string &key, const std::string &value, const std::string &ownerValue, int64_t lease,
	                        TxnCompletion done);

	// 普通 txn:reply.succeeded();BuildClaimIfAbsentOrOwnedTxn 的嵌套形态:外层或内层任一成功。
	bool TxnClaimSucceeded(const etcdserverpb::TxnResponse &reply);
	// 申领失败时最内层 Range 拿到的当前持有者;没有(key 已不存在 / 非申领形态)返回 nullptr。
	const mvccpb::KeyValue *TxnClaimCurrentHolder(const etcdserverpb::TxnResponse &reply);

	void RevokeLeaseAndCleanup(int64_t leaseId);
	void DeleteRange(const std::string &key, bool isPrefix);
	// 往 LeaseKeepAlive 流写一拍。新传输在流断开、尚未重建时丢弃(重建后 EtcdService 立即补一拍)。
	void KeepAliveLease(int64_t leaseId);
}
