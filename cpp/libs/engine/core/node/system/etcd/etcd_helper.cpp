#include "etcd_helper.h"
#include <utility>
#include <google/protobuf/util/json_util.h>
#include <muduo/base/Logging.h>
#include "node/system/etcd/etcd_transport.h"
#include "node/system/node/node.h"

namespace
{
// 全部 etcd 请求的唯一出口(docs/design/grpc-muduo-transport.md §13.4)。EtcdService 还没 Init、或已经 Shutdown 时
// 没有传输:请求丢弃并记一笔(关机之后的写本来就不会被处理;旧实现此时也是投进正在关闭的 CQ)。
EtcdTransport *ActiveTransport(const char *operation)
{
	EtcdTransport *transport =
		gNode != nullptr ? gNode->GetServiceDiscoveryManager().etcdService.Transport() : nullptr;
	if (transport == nullptr)
	{
		LOG_WARN << "etcd " << operation << " dropped: the etcd transport is not running";
	}
	return transport;
}

void SendRegistrationTxn(const etcdserverpb::TxnRequest &request, EtcdHelper::TxnCompletion done)
{
	if (auto *transport = ActiveTransport("Txn"))
	{
		transport->Txn(request, std::move(done));
		return;
	}
	done(std::nullopt);
}
} // namespace

void EtcdHelper::PutServiceNodeInfo(const NodeInfo &nodeInfo, const std::string &key, int64_t lease)
{
	etcdserverpb::PutRequest request;

	request.set_key(key);
	request.set_prev_kv(true);
	if (lease > 0)
	{
		request.set_lease(lease);
	}

	std::string jsonValue;
	auto status = google::protobuf::util::MessageToJsonString(nodeInfo, &jsonValue);
	if (!status.ok())
	{
		LOG_ERROR << "[EtcdHelper::PutServiceNodeInfo] Failed to serialize NodeInfo to JSON. "
				  << "Error: " << status.message().data()
				  << ", key: " << key;
		return;
	}
	request.set_value(jsonValue);

	if (auto *transport = ActiveTransport("Put"))
	{
		transport->Put(request);
	}
}

void EtcdHelper::RangeQuery(const std::string &prefix)
{
	etcdserverpb::RangeRequest request;
	request.set_key(prefix);

	std::string range_end = prefix;
	range_end.back() += 1; // last char + 1
	request.set_range_end(range_end);

	if (auto *transport = ActiveTransport("Range"))
	{
		transport->Range(request);
	}
}

void EtcdHelper::StartWatchingPrefix(const std::string &prefix, int64_t revision)
{
	etcdserverpb::WatchRequest request;
	auto &createReq = *request.mutable_create_request();

	createReq.set_prev_kv(true);
	createReq.set_key(prefix);

	std::string range_end = prefix;
	range_end.back() += 1;
	createReq.set_range_end(range_end);

	if (revision > 0)
	{
		createReq.set_start_revision(revision);
	}

	if (auto *transport = ActiveTransport("Watch"))
	{
		transport->WatchWrite(request);
	}
}

void EtcdHelper::RangeKey(const std::string &key)
{
	etcdserverpb::RangeRequest request;
	request.set_key(key); // range_end 留空 = 只这一个键

	if (auto *transport = ActiveTransport("Range"))
	{
		transport->Range(request);
	}
}

void EtcdHelper::StartWatchingKey(const std::string &key, int64_t revision)
{
	etcdserverpb::WatchRequest request;
	auto &createReq = *request.mutable_create_request();

	createReq.set_prev_kv(true);
	createReq.set_key(key); // range_end 留空 = 只这一个键

	if (revision > 0)
	{
		createReq.set_start_revision(revision);
	}

	if (auto *transport = ActiveTransport("Watch"))
	{
		transport->WatchWrite(request);
	}
}

void EtcdHelper::StopAllWatching()
{
	// TODO: Add cancel_request implementation if needed
}

void EtcdHelper::GrantLease(uint32_t ttlSeconds, EtcdRegistrationCompletion<etcdserverpb::LeaseGrantResponse> done)
{
	etcdserverpb::LeaseGrantRequest leaseReq;
	leaseReq.set_ttl(ttlSeconds);

	if (auto *transport = ActiveTransport("LeaseGrant"))
	{
		transport->LeaseGrant(leaseReq, std::move(done));
		return;
	}
	done(std::nullopt);
}

void EtcdHelper::PutIfAbsent(const std::string &key, const std::string &newValue, int64_t currentVersion,
							 int64_t lease, TxnCompletion done, bool checkSameLease)
{
	if (checkSameLease && lease > 0)
	{
		// 此键的上一条 CAS 结局未知。只承认同一租约下已经写好的键，不接管别的节点的键。
		auto recovery = BuildClaimIfAbsentOrOwnedTxn(key, newValue, "", lease);
		auto* compare = recovery.mutable_failure(0)->mutable_request_txn()->mutable_compare(0);
		compare->set_target(etcdserverpb::Compare::LEASE);
		compare->set_lease(lease);
		SendRegistrationTxn(recovery, std::move(done));
		return;
	}
	etcdserverpb::TxnRequest txn;

	// Compare: version == 0 means key does not exist
	auto &compare = *txn.add_compare();
	compare.set_key(key);
	compare.set_target(etcdserverpb::Compare::VERSION);
	compare.set_result(etcdserverpb::Compare::EQUAL);
	compare.set_version(currentVersion);

	auto &successOp = *txn.add_success()->mutable_request_put();
	successOp.set_key(key);
	successOp.set_value(newValue);
	successOp.set_lease(lease);

	SendRegistrationTxn(txn, std::move(done));
}

void EtcdHelper::PutWithLease(const std::string &key, const std::string &newValue, int64_t lease, TxnCompletion done)
{
	etcdserverpb::TxnRequest txn;

	// 不加任何 compare:成功应答仍与 CAS 共用同一注册步骤。
	auto &successOp = *txn.add_success()->mutable_request_put();
	successOp.set_key(key);
	successOp.set_value(newValue);
	successOp.set_lease(lease);

	SendRegistrationTxn(txn, std::move(done));
}

void EtcdHelper::PutWithLease(const std::string &key, const NodeInfo &nodeInfo, int64_t lease, TxnCompletion done)
{
	std::string jsonValue;
	auto status = google::protobuf::util::MessageToJsonString(nodeInfo, &jsonValue);
	if (!status.ok())
	{
		LOG_ERROR << " Failed to serialize NodeInfo to JSON. "
				  << "Error: " << status.message().data();
		done(std::nullopt);
		return;
	}

	PutWithLease(key, jsonValue, lease, std::move(done));
}

etcdserverpb::TxnRequest EtcdHelper::BuildClaimIfAbsentOrOwnedTxn(const std::string &key,
																	const std::string &value,
																	const std::string &ownerValue,
																	int64_t lease,
																	const std::vector<etcdserverpb::RequestOp> &extraSuccessOps)
{
	etcdserverpb::TxnRequest txn;

	auto &absent = *txn.add_compare();
	absent.set_key(key);
	absent.set_target(etcdserverpb::Compare::CREATE);
	absent.set_result(etcdserverpb::Compare::EQUAL);
	absent.set_create_revision(0);

	auto appendClaim = [&](google::protobuf::RepeatedPtrField<etcdserverpb::RequestOp> &ops)
	{
		auto &put = *ops.Add()->mutable_request_put();
		put.set_key(key);
		put.set_value(value);
		put.set_lease(lease);
		for (const auto &extra : extraSuccessOps)
		{
			*ops.Add() = extra;
		}
	};
	appendClaim(*txn.mutable_success());

	// 外层 compare 失败 = key 存在。内层再问一次"是不是我的":Value 比较对不存在的 key
	// 恒 false,所以内层不会误放行一个刚好在两次 compare 之间被删掉的 key。
	auto &nested = *txn.add_failure()->mutable_request_txn();
	auto &owned = *nested.add_compare();
	owned.set_key(key);
	owned.set_target(etcdserverpb::Compare::VALUE);
	owned.set_result(etcdserverpb::Compare::EQUAL);
	owned.set_value(ownerValue);
	appendClaim(*nested.mutable_success());
	auto &whoHolds = *nested.add_failure()->mutable_request_range();
	whoHolds.set_key(key);

	return txn;
}

void EtcdHelper::PutIfAbsentOrOwned(const std::string &key, const std::string &value, const std::string &ownerValue,
								    int64_t lease, TxnCompletion done)
{
	const auto txn = BuildClaimIfAbsentOrOwnedTxn(key, value, ownerValue, lease);
	SendRegistrationTxn(txn, std::move(done));
}

bool EtcdHelper::TxnClaimSucceeded(const etcdserverpb::TxnResponse &reply)
{
	if (reply.succeeded())
	{
		return true;
	}
	// 嵌套形态:失败分支唯一的一个响应是内层 txn,它的 succeeded 才是"已经是我的"。
	if (reply.responses_size() == 1 && reply.responses(0).has_response_txn())
	{
		return reply.responses(0).response_txn().succeeded();
	}
	return false;
}

const mvccpb::KeyValue *EtcdHelper::TxnClaimCurrentHolder(const etcdserverpb::TxnResponse &reply)
{
	if (reply.succeeded() || reply.responses_size() != 1 || !reply.responses(0).has_response_txn())
	{
		return nullptr;
	}
	const auto &nested = reply.responses(0).response_txn();
	if (nested.succeeded() || nested.responses_size() != 1 || !nested.responses(0).has_response_range())
	{
		return nullptr;
	}
	const auto &range = nested.responses(0).response_range();
	return range.kvs_size() > 0 ? &range.kvs(0) : nullptr;
}

void EtcdHelper::PutIfAbsent(const std::string &key, const NodeInfo &nodeInfo, int64_t lease, TxnCompletion done,
							 bool checkSameLease)
{
	std::string jsonValue;
	auto status = google::protobuf::util::MessageToJsonString(nodeInfo, &jsonValue);
	if (!status.ok())
	{
		LOG_ERROR << " Failed to serialize NodeInfo to JSON. "
				  << "Error: " << status.message().data();
		done(std::nullopt);
		return;
	}

	PutIfAbsent(key, jsonValue, 0, lease, std::move(done), checkSameLease);
}

void EtcdHelper::PutIfAbsentListingOnConflict(const std::string &key, const std::string &value, int64_t lease,
											  const std::string &listPrefix, TxnCompletion done)
{
	etcdserverpb::TxnRequest txn;

	// Compare: version == 0 means key does not exist
	auto &compare = *txn.add_compare();
	compare.set_key(key);
	compare.set_target(etcdserverpb::Compare::VERSION);
	compare.set_result(etcdserverpb::Compare::EQUAL);
	compare.set_version(0);

	auto &successOp = *txn.add_success()->mutable_request_put();
	successOp.set_key(key);
	successOp.set_value(value);
	successOp.set_lease(lease);

	// 失败分支:只取键列出整个前缀。键里就是号,不要值(NodeInfo / uuid),应答按键数线性增长。
	auto &listing = *txn.add_failure()->mutable_request_range();
	listing.set_key(listPrefix);
	std::string rangeEnd = listPrefix;
	rangeEnd.back() += 1; // last char + 1
	listing.set_range_end(rangeEnd);
	listing.set_keys_only(true);

	SendRegistrationTxn(txn, std::move(done));
}

const etcdserverpb::RangeResponse *EtcdHelper::TxnConflictListing(const etcdserverpb::TxnResponse &reply)
{
	if (reply.succeeded() || reply.responses_size() != 1 || !reply.responses(0).has_response_range())
	{
		return nullptr;
	}
	return &reply.responses(0).response_range();
}

void EtcdHelper::RevokeLeaseAndCleanup(int64_t leaseId)
{
	etcdserverpb::LeaseRevokeRequest request;
	request.set_id(leaseId);

	if (auto *transport = ActiveTransport("LeaseRevoke"))
	{
		transport->LeaseRevoke(request);
	}
}

void EtcdHelper::DeleteRange(const std::string &key, bool isPrefix)
{
	etcdserverpb::DeleteRangeRequest request;
	request.set_key(key);

	if (isPrefix)
	{
		std::string range_end = key;
		range_end.back() += 1; // prefix range delete
		request.set_range_end(range_end);
	}

	if (auto *transport = ActiveTransport("DeleteRange"))
	{
		transport->DeleteRange(request);
	}
}


void EtcdHelper::KeepAliveLease(int64_t leaseId)
{
	etcdserverpb::LeaseKeepAliveRequest request;
	request.set_id(leaseId);

	if (auto *transport = ActiveTransport("LeaseKeepAlive"))
	{
		transport->KeepAliveWrite(request);
	}
}
