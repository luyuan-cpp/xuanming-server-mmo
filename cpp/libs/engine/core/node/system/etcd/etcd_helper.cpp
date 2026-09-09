#include "etcd_helper.h"
#include <google/protobuf/util/json_util.h>
#include "thread_context/redis_manager.h"
#include "grpc_client/etcd/etcd_grpc_client.h"
#include <muduo/base/Logging.h>
#include "thread_context/node_context_manager.h"

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

	SendKVPut(tlsNodeContextManager.GetRegistry(EtcdNodeService), tlsNodeContextManager.GetGlobalEntity(EtcdNodeService), request);
}

void EtcdHelper::RangeQuery(const std::string &prefix)
{
	etcdserverpb::RangeRequest request;
	request.set_key(prefix);

	std::string range_end = prefix;
	range_end.back() += 1; // last char + 1
	request.set_range_end(range_end);

	SendKVRange(tlsNodeContextManager.GetRegistry(EtcdNodeService), tlsNodeContextManager.GetGlobalEntity(EtcdNodeService), request);
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

	SendWatchWatch(tlsNodeContextManager.GetRegistry(EtcdNodeService), tlsNodeContextManager.GetGlobalEntity(EtcdNodeService), request);
}

void EtcdHelper::StopAllWatching()
{
	// TODO: Add cancel_request implementation if needed
}

void EtcdHelper::GrantLease(uint32_t ttlSeconds)
{
	etcdserverpb::LeaseGrantRequest leaseReq;
	leaseReq.set_ttl(ttlSeconds);

	SendLeaseLeaseGrant(tlsNodeContextManager.GetRegistry(EtcdNodeService), tlsNodeContextManager.GetGlobalEntity(EtcdNodeService), leaseReq);
}

void EtcdHelper::PutIfAbsent(const std::string &key, const std::string &newValue, int64_t currentVersion, int64_t lease)
{
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

	SendKVTxn(tlsNodeContextManager.GetRegistry(EtcdNodeService), tlsNodeContextManager.GetGlobalEntity(EtcdNodeService), txn);
}

void EtcdHelper::PutWithLease(const std::string &key, const std::string &newValue, int64_t lease)
{
	etcdserverpb::TxnRequest txn;

	// 不加任何 compare:txn 恒成功。走 txn 而不是裸 Put,是为了让响应仍然落进
	// OnTxnSucceeded / pendingTxnKey 这套既有回调链,不必再造一条并行路径。
	auto &successOp = *txn.add_success()->mutable_request_put();
	successOp.set_key(key);
	successOp.set_value(newValue);
	successOp.set_lease(lease);

	SendKVTxn(tlsNodeContextManager.GetRegistry(EtcdNodeService), tlsNodeContextManager.GetGlobalEntity(EtcdNodeService), txn);
}

void EtcdHelper::PutWithLease(const std::string &key, const NodeInfo &nodeInfo, int64_t lease)
{
	std::string jsonValue;
	auto status = google::protobuf::util::MessageToJsonString(nodeInfo, &jsonValue);
	if (!status.ok())
	{
		LOG_ERROR << " Failed to serialize NodeInfo to JSON. "
				  << "Error: " << status.message().data();
		return;
	}

	PutWithLease(key, jsonValue, lease);
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

void EtcdHelper::PutIfAbsentOrOwned(const std::string &key, const std::string &value, const std::string &ownerValue, int64_t lease)
{
	const auto txn = BuildClaimIfAbsentOrOwnedTxn(key, value, ownerValue, lease);
	SendKVTxn(tlsNodeContextManager.GetRegistry(EtcdNodeService), tlsNodeContextManager.GetGlobalEntity(EtcdNodeService), txn);
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

void EtcdHelper::PutIfAbsent(const std::string &key, const NodeInfo &nodeInfo, int64_t lease)
{
	std::string jsonValue;
	auto status = google::protobuf::util::MessageToJsonString(nodeInfo, &jsonValue);
	if (!status.ok())
	{
		LOG_ERROR << " Failed to serialize NodeInfo to JSON. "
				  << "Error: " << status.message().data();
		return;
	}

	PutIfAbsent(key, jsonValue, 0, lease);
}

void EtcdHelper::RevokeLeaseAndCleanup(int64_t leaseId)
{
	etcdserverpb::LeaseRevokeRequest request;
	request.set_id(leaseId);

	SendLeaseLeaseRevoke(tlsNodeContextManager.GetRegistry(EtcdNodeService), tlsNodeContextManager.GetGlobalEntity(EtcdNodeService), request);
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

	SendKVDeleteRange(tlsNodeContextManager.GetRegistry(EtcdNodeService), tlsNodeContextManager.GetGlobalEntity(EtcdNodeService), request);
}
