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
