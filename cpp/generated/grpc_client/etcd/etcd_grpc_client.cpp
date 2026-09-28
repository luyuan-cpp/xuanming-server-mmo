#include "muduo/base/Logging.h"

#include "etcd_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <atomic>
#include <chrono>
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
// 本文件所有 unary 调用的 deadline(毫秒)。启动时 SetEtcdCallDeadline 按目标节点类型写入
// (Node::Initialize → grpc_call_deadline::Apply);原子量:与应答处理器一样是进程级全局。
std::atomic<uint32_t> callDeadlineMs{kDefaultGrpcCallDeadlineMs};

std::chrono::system_clock::time_point NextCallDeadline() {
    return std::chrono::system_clock::now() +
        std::chrono::milliseconds(callDeadlineMs.load(std::memory_order_relaxed));
}
}

namespace etcdserverpb {
struct EtcdCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region KVRange
boost::object_pool<AsyncKVRangeGrpcClient> KVRangePool;
using AsyncKVRangeHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::RangeResponse&)>;
AsyncKVRangeHandlerFunctionType AsyncKVRangeHandler;
AsyncKVRangeFailedHandlerFunctionType AsyncKVRangeFailedHandler;

void AsyncCompleteGrpcKVRange(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncKVRangeGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncKVRangeHandler) {
            AsyncKVRangeHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC KV.Range reply dropped: AsyncKVRangeHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncKVRangeFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "KV.Range", call->context, call->status, call->sentMetadata};
        AsyncKVRangeFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC KV.Range failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	KVRangePool.destroy(call);
}

void SendKVRange(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::RangeRequest& request) {

    SendKVRange(registry, nodeEntity, request, {}, {});

}

void SendKVRange(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::RangeRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(KVRangePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<KVStubPtr>(nodeEntity)
        ->PrepareAsyncRange(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(KVRangeMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendKVRange(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::etcdserverpb::RangeRequest& derived = static_cast<const ::etcdserverpb::RangeRequest&>(message);
    SendKVRange(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region KVPut
boost::object_pool<AsyncKVPutGrpcClient> KVPutPool;
using AsyncKVPutHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::PutResponse&)>;
AsyncKVPutHandlerFunctionType AsyncKVPutHandler;
AsyncKVPutFailedHandlerFunctionType AsyncKVPutFailedHandler;

void AsyncCompleteGrpcKVPut(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncKVPutGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncKVPutHandler) {
            AsyncKVPutHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC KV.Put reply dropped: AsyncKVPutHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncKVPutFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "KV.Put", call->context, call->status, call->sentMetadata};
        AsyncKVPutFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC KV.Put failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	KVPutPool.destroy(call);
}

void SendKVPut(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::PutRequest& request) {

    SendKVPut(registry, nodeEntity, request, {}, {});

}

void SendKVPut(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::PutRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(KVPutPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<KVStubPtr>(nodeEntity)
        ->PrepareAsyncPut(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(KVPutMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendKVPut(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::etcdserverpb::PutRequest& derived = static_cast<const ::etcdserverpb::PutRequest&>(message);
    SendKVPut(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region KVDeleteRange
boost::object_pool<AsyncKVDeleteRangeGrpcClient> KVDeleteRangePool;
using AsyncKVDeleteRangeHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::DeleteRangeResponse&)>;
AsyncKVDeleteRangeHandlerFunctionType AsyncKVDeleteRangeHandler;
AsyncKVDeleteRangeFailedHandlerFunctionType AsyncKVDeleteRangeFailedHandler;

void AsyncCompleteGrpcKVDeleteRange(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncKVDeleteRangeGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncKVDeleteRangeHandler) {
            AsyncKVDeleteRangeHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC KV.DeleteRange reply dropped: AsyncKVDeleteRangeHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncKVDeleteRangeFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "KV.DeleteRange", call->context, call->status, call->sentMetadata};
        AsyncKVDeleteRangeFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC KV.DeleteRange failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	KVDeleteRangePool.destroy(call);
}

void SendKVDeleteRange(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::DeleteRangeRequest& request) {

    SendKVDeleteRange(registry, nodeEntity, request, {}, {});

}

void SendKVDeleteRange(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::DeleteRangeRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(KVDeleteRangePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<KVStubPtr>(nodeEntity)
        ->PrepareAsyncDeleteRange(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(KVDeleteRangeMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendKVDeleteRange(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::etcdserverpb::DeleteRangeRequest& derived = static_cast<const ::etcdserverpb::DeleteRangeRequest&>(message);
    SendKVDeleteRange(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region KVTxn
boost::object_pool<AsyncKVTxnGrpcClient> KVTxnPool;
using AsyncKVTxnHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::TxnResponse&)>;
AsyncKVTxnHandlerFunctionType AsyncKVTxnHandler;
AsyncKVTxnFailedHandlerFunctionType AsyncKVTxnFailedHandler;

void AsyncCompleteGrpcKVTxn(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncKVTxnGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncKVTxnHandler) {
            AsyncKVTxnHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC KV.Txn reply dropped: AsyncKVTxnHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncKVTxnFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "KV.Txn", call->context, call->status, call->sentMetadata};
        AsyncKVTxnFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC KV.Txn failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	KVTxnPool.destroy(call);
}

void SendKVTxn(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::TxnRequest& request) {

    SendKVTxn(registry, nodeEntity, request, {}, {});

}

void SendKVTxn(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::TxnRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(KVTxnPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<KVStubPtr>(nodeEntity)
        ->PrepareAsyncTxn(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(KVTxnMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendKVTxn(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::etcdserverpb::TxnRequest& derived = static_cast<const ::etcdserverpb::TxnRequest&>(message);
    SendKVTxn(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region KVCompact
boost::object_pool<AsyncKVCompactGrpcClient> KVCompactPool;
using AsyncKVCompactHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::CompactionResponse&)>;
AsyncKVCompactHandlerFunctionType AsyncKVCompactHandler;
AsyncKVCompactFailedHandlerFunctionType AsyncKVCompactFailedHandler;

void AsyncCompleteGrpcKVCompact(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncKVCompactGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncKVCompactHandler) {
            AsyncKVCompactHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC KV.Compact reply dropped: AsyncKVCompactHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncKVCompactFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "KV.Compact", call->context, call->status, call->sentMetadata};
        AsyncKVCompactFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC KV.Compact failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	KVCompactPool.destroy(call);
}

void SendKVCompact(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::CompactionRequest& request) {

    SendKVCompact(registry, nodeEntity, request, {}, {});

}

void SendKVCompact(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::CompactionRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(KVCompactPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<KVStubPtr>(nodeEntity)
        ->PrepareAsyncCompact(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(KVCompactMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendKVCompact(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::etcdserverpb::CompactionRequest& derived = static_cast<const ::etcdserverpb::CompactionRequest&>(message);
    SendKVCompact(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region WatchWatch
boost::object_pool<AsyncWatchWatchGrpcClient> WatchWatchPool;
using AsyncWatchWatchHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::WatchResponse&)>;
AsyncWatchWatchHandlerFunctionType AsyncWatchWatchHandler;

void TryWriteNextNextWatchWatch(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq) {
    auto& writeInProgress = registry.get<WatchRequestWriteInProgress>(nodeEntity);
    auto& pendingWritesBuffer = registry.get<WatchRequestBuffer>(nodeEntity).pendingWritesBuffer;

    if (writeInProgress.isInProgress || pendingWritesBuffer.empty()) {
        return;
    }

    auto& client = registry.get<AsyncWatchWatchGrpcClient>(nodeEntity);
    auto& request = pendingWritesBuffer.front();

    writeInProgress.isInProgress = true;
    GrpcTag* got_tag(tagPool.construct(WatchWatchMessageId,  (void*)GrpcOperation::WRITE));
    client.stream->Write(request, (void*)(got_tag));
}
void AsyncCompleteGrpcWatchWatch(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto& client = registry.get<AsyncWatchWatchGrpcClient>(nodeEntity);
    auto& writeInProgress = registry.get<WatchRequestWriteInProgress>(nodeEntity);

    switch (static_cast<GrpcOperation>(reinterpret_cast<intptr_t>(got_tag))) {
        case GrpcOperation::WRITE: {
            auto& pendingWritesBuffer = registry.get<WatchRequestBuffer>(nodeEntity).pendingWritesBuffer;
            if (!pendingWritesBuffer.empty()) {
                pendingWritesBuffer.pop_front();
            }
            writeInProgress.isInProgress = false;
            TryWriteNextNextWatchWatch(registry, nodeEntity, cq);
            break;
        }
        case GrpcOperation::WRITES_DONE: {
            GrpcTag* got_tag(tagPool.construct(WatchWatchMessageId,  (void*)GrpcOperation::READ));
            client.stream->Finish(&client.status, (void*)(got_tag));
            break;
        }
        case GrpcOperation::FINISH:
            cq.Shutdown();
            break;
        case GrpcOperation::READ: {
            auto& response = registry.get<::etcdserverpb::WatchResponse>(nodeEntity);
            if (AsyncWatchWatchHandler) {
                AsyncWatchWatchHandler(client.context, response);
            }
            GrpcTag* got_tag(tagPool.construct(WatchWatchMessageId, (void*)GrpcOperation::READ));
            client.stream->Read(&response, (void*)got_tag);
            TryWriteNextNextWatchWatch(registry, nodeEntity, cq);
            break;
        }
        case GrpcOperation::INIT: {
            GrpcTag* got_tag(tagPool.construct(WatchWatchMessageId, (void*)GrpcOperation::READ));
            auto& response = registry.get<::etcdserverpb::WatchResponse>(nodeEntity);
            client.stream->Read(&response, (void*)got_tag);
            TryWriteNextNextWatchWatch(registry, nodeEntity, cq);
            break;
        }
        default:
            break;
    }
}

void SendWatchWatch(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::WatchRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto& pendingWritesBuffer = registry.get<WatchRequestBuffer>(nodeEntity).pendingWritesBuffer;
    pendingWritesBuffer.push_back(request);
    TryWriteNextNextWatchWatch(registry, nodeEntity, cq);

}

void SendWatchWatch(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::WatchRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto& pendingWritesBuffer = registry.get<WatchRequestBuffer>(nodeEntity).pendingWritesBuffer;
    pendingWritesBuffer.push_back(request);
    TryWriteNextNextWatchWatch(registry, nodeEntity, cq);

}

void SendWatchWatch(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::etcdserverpb::WatchRequest& derived = static_cast<const ::etcdserverpb::WatchRequest&>(message);
    SendWatchWatch(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region LeaseLeaseGrant
boost::object_pool<AsyncLeaseLeaseGrantGrpcClient> LeaseLeaseGrantPool;
using AsyncLeaseLeaseGrantHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::LeaseGrantResponse&)>;
AsyncLeaseLeaseGrantHandlerFunctionType AsyncLeaseLeaseGrantHandler;
AsyncLeaseLeaseGrantFailedHandlerFunctionType AsyncLeaseLeaseGrantFailedHandler;

void AsyncCompleteGrpcLeaseLeaseGrant(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncLeaseLeaseGrantGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncLeaseLeaseGrantHandler) {
            AsyncLeaseLeaseGrantHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC Lease.LeaseGrant reply dropped: AsyncLeaseLeaseGrantHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncLeaseLeaseGrantFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "Lease.LeaseGrant", call->context, call->status, call->sentMetadata};
        AsyncLeaseLeaseGrantFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC Lease.LeaseGrant failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	LeaseLeaseGrantPool.destroy(call);
}

void SendLeaseLeaseGrant(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseGrantRequest& request) {

    SendLeaseLeaseGrant(registry, nodeEntity, request, {}, {});

}

void SendLeaseLeaseGrant(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseGrantRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(LeaseLeaseGrantPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<LeaseStubPtr>(nodeEntity)
        ->PrepareAsyncLeaseGrant(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(LeaseLeaseGrantMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendLeaseLeaseGrant(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::etcdserverpb::LeaseGrantRequest& derived = static_cast<const ::etcdserverpb::LeaseGrantRequest&>(message);
    SendLeaseLeaseGrant(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region LeaseLeaseRevoke
boost::object_pool<AsyncLeaseLeaseRevokeGrpcClient> LeaseLeaseRevokePool;
using AsyncLeaseLeaseRevokeHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::LeaseRevokeResponse&)>;
AsyncLeaseLeaseRevokeHandlerFunctionType AsyncLeaseLeaseRevokeHandler;
AsyncLeaseLeaseRevokeFailedHandlerFunctionType AsyncLeaseLeaseRevokeFailedHandler;

void AsyncCompleteGrpcLeaseLeaseRevoke(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncLeaseLeaseRevokeGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncLeaseLeaseRevokeHandler) {
            AsyncLeaseLeaseRevokeHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC Lease.LeaseRevoke reply dropped: AsyncLeaseLeaseRevokeHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncLeaseLeaseRevokeFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "Lease.LeaseRevoke", call->context, call->status, call->sentMetadata};
        AsyncLeaseLeaseRevokeFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC Lease.LeaseRevoke failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	LeaseLeaseRevokePool.destroy(call);
}

void SendLeaseLeaseRevoke(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseRevokeRequest& request) {

    SendLeaseLeaseRevoke(registry, nodeEntity, request, {}, {});

}

void SendLeaseLeaseRevoke(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseRevokeRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(LeaseLeaseRevokePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<LeaseStubPtr>(nodeEntity)
        ->PrepareAsyncLeaseRevoke(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(LeaseLeaseRevokeMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendLeaseLeaseRevoke(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::etcdserverpb::LeaseRevokeRequest& derived = static_cast<const ::etcdserverpb::LeaseRevokeRequest&>(message);
    SendLeaseLeaseRevoke(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region LeaseLeaseKeepAlive
boost::object_pool<AsyncLeaseLeaseKeepAliveGrpcClient> LeaseLeaseKeepAlivePool;
using AsyncLeaseLeaseKeepAliveHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::LeaseKeepAliveResponse&)>;
AsyncLeaseLeaseKeepAliveHandlerFunctionType AsyncLeaseLeaseKeepAliveHandler;

void TryWriteNextNextLeaseLeaseKeepAlive(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq) {
    auto& writeInProgress = registry.get<LeaseKeepAliveRequestWriteInProgress>(nodeEntity);
    auto& pendingWritesBuffer = registry.get<LeaseKeepAliveRequestBuffer>(nodeEntity).pendingWritesBuffer;

    if (writeInProgress.isInProgress || pendingWritesBuffer.empty()) {
        return;
    }

    auto& client = registry.get<AsyncLeaseLeaseKeepAliveGrpcClient>(nodeEntity);
    auto& request = pendingWritesBuffer.front();

    writeInProgress.isInProgress = true;
    GrpcTag* got_tag(tagPool.construct(LeaseLeaseKeepAliveMessageId,  (void*)GrpcOperation::WRITE));
    client.stream->Write(request, (void*)(got_tag));
}
void AsyncCompleteGrpcLeaseLeaseKeepAlive(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto& client = registry.get<AsyncLeaseLeaseKeepAliveGrpcClient>(nodeEntity);
    auto& writeInProgress = registry.get<LeaseKeepAliveRequestWriteInProgress>(nodeEntity);

    switch (static_cast<GrpcOperation>(reinterpret_cast<intptr_t>(got_tag))) {
        case GrpcOperation::WRITE: {
            auto& pendingWritesBuffer = registry.get<LeaseKeepAliveRequestBuffer>(nodeEntity).pendingWritesBuffer;
            if (!pendingWritesBuffer.empty()) {
                pendingWritesBuffer.pop_front();
            }
            writeInProgress.isInProgress = false;
            TryWriteNextNextLeaseLeaseKeepAlive(registry, nodeEntity, cq);
            break;
        }
        case GrpcOperation::WRITES_DONE: {
            GrpcTag* got_tag(tagPool.construct(LeaseLeaseKeepAliveMessageId,  (void*)GrpcOperation::READ));
            client.stream->Finish(&client.status, (void*)(got_tag));
            break;
        }
        case GrpcOperation::FINISH:
            cq.Shutdown();
            break;
        case GrpcOperation::READ: {
            auto& response = registry.get<::etcdserverpb::LeaseKeepAliveResponse>(nodeEntity);
            if (AsyncLeaseLeaseKeepAliveHandler) {
                AsyncLeaseLeaseKeepAliveHandler(client.context, response);
            }
            GrpcTag* got_tag(tagPool.construct(LeaseLeaseKeepAliveMessageId, (void*)GrpcOperation::READ));
            client.stream->Read(&response, (void*)got_tag);
            TryWriteNextNextLeaseLeaseKeepAlive(registry, nodeEntity, cq);
            break;
        }
        case GrpcOperation::INIT: {
            GrpcTag* got_tag(tagPool.construct(LeaseLeaseKeepAliveMessageId, (void*)GrpcOperation::READ));
            auto& response = registry.get<::etcdserverpb::LeaseKeepAliveResponse>(nodeEntity);
            client.stream->Read(&response, (void*)got_tag);
            TryWriteNextNextLeaseLeaseKeepAlive(registry, nodeEntity, cq);
            break;
        }
        default:
            break;
    }
}

void SendLeaseLeaseKeepAlive(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseKeepAliveRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto& pendingWritesBuffer = registry.get<LeaseKeepAliveRequestBuffer>(nodeEntity).pendingWritesBuffer;
    pendingWritesBuffer.push_back(request);
    TryWriteNextNextLeaseLeaseKeepAlive(registry, nodeEntity, cq);

}

void SendLeaseLeaseKeepAlive(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseKeepAliveRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto& pendingWritesBuffer = registry.get<LeaseKeepAliveRequestBuffer>(nodeEntity).pendingWritesBuffer;
    pendingWritesBuffer.push_back(request);
    TryWriteNextNextLeaseLeaseKeepAlive(registry, nodeEntity, cq);

}

void SendLeaseLeaseKeepAlive(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::etcdserverpb::LeaseKeepAliveRequest& derived = static_cast<const ::etcdserverpb::LeaseKeepAliveRequest&>(message);
    SendLeaseLeaseKeepAlive(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region LeaseLeaseTimeToLive
boost::object_pool<AsyncLeaseLeaseTimeToLiveGrpcClient> LeaseLeaseTimeToLivePool;
using AsyncLeaseLeaseTimeToLiveHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::LeaseTimeToLiveResponse&)>;
AsyncLeaseLeaseTimeToLiveHandlerFunctionType AsyncLeaseLeaseTimeToLiveHandler;
AsyncLeaseLeaseTimeToLiveFailedHandlerFunctionType AsyncLeaseLeaseTimeToLiveFailedHandler;

void AsyncCompleteGrpcLeaseLeaseTimeToLive(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncLeaseLeaseTimeToLiveGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncLeaseLeaseTimeToLiveHandler) {
            AsyncLeaseLeaseTimeToLiveHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC Lease.LeaseTimeToLive reply dropped: AsyncLeaseLeaseTimeToLiveHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncLeaseLeaseTimeToLiveFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "Lease.LeaseTimeToLive", call->context, call->status, call->sentMetadata};
        AsyncLeaseLeaseTimeToLiveFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC Lease.LeaseTimeToLive failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	LeaseLeaseTimeToLivePool.destroy(call);
}

void SendLeaseLeaseTimeToLive(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseTimeToLiveRequest& request) {

    SendLeaseLeaseTimeToLive(registry, nodeEntity, request, {}, {});

}

void SendLeaseLeaseTimeToLive(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseTimeToLiveRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(LeaseLeaseTimeToLivePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<LeaseStubPtr>(nodeEntity)
        ->PrepareAsyncLeaseTimeToLive(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(LeaseLeaseTimeToLiveMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendLeaseLeaseTimeToLive(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::etcdserverpb::LeaseTimeToLiveRequest& derived = static_cast<const ::etcdserverpb::LeaseTimeToLiveRequest&>(message);
    SendLeaseLeaseTimeToLive(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region LeaseLeaseLeases
boost::object_pool<AsyncLeaseLeaseLeasesGrpcClient> LeaseLeaseLeasesPool;
using AsyncLeaseLeaseLeasesHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::LeaseLeasesResponse&)>;
AsyncLeaseLeaseLeasesHandlerFunctionType AsyncLeaseLeaseLeasesHandler;
AsyncLeaseLeaseLeasesFailedHandlerFunctionType AsyncLeaseLeaseLeasesFailedHandler;

void AsyncCompleteGrpcLeaseLeaseLeases(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncLeaseLeaseLeasesGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncLeaseLeaseLeasesHandler) {
            AsyncLeaseLeaseLeasesHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC Lease.LeaseLeases reply dropped: AsyncLeaseLeaseLeasesHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncLeaseLeaseLeasesFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "Lease.LeaseLeases", call->context, call->status, call->sentMetadata};
        AsyncLeaseLeaseLeasesFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC Lease.LeaseLeases failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	LeaseLeaseLeasesPool.destroy(call);
}

void SendLeaseLeaseLeases(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseLeasesRequest& request) {

    SendLeaseLeaseLeases(registry, nodeEntity, request, {}, {});

}

void SendLeaseLeaseLeases(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseLeasesRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(LeaseLeaseLeasesPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<LeaseStubPtr>(nodeEntity)
        ->PrepareAsyncLeaseLeases(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(LeaseLeaseLeasesMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendLeaseLeaseLeases(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::etcdserverpb::LeaseLeasesRequest& derived = static_cast<const ::etcdserverpb::LeaseLeasesRequest&>(message);
    SendLeaseLeaseLeases(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleEtcdCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case KVRangeMessageId:
            AsyncCompleteGrpcKVRange(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case KVPutMessageId:
            AsyncCompleteGrpcKVPut(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case KVDeleteRangeMessageId:
            AsyncCompleteGrpcKVDeleteRange(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case KVTxnMessageId:
            AsyncCompleteGrpcKVTxn(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case KVCompactMessageId:
            AsyncCompleteGrpcKVCompact(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case WatchWatchMessageId:
            AsyncCompleteGrpcWatchWatch(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case LeaseLeaseGrantMessageId:
            AsyncCompleteGrpcLeaseLeaseGrant(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case LeaseLeaseRevokeMessageId:
            AsyncCompleteGrpcLeaseLeaseRevoke(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case LeaseLeaseKeepAliveMessageId:
            AsyncCompleteGrpcLeaseLeaseKeepAlive(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case LeaseLeaseTimeToLiveMessageId:
            AsyncCompleteGrpcLeaseLeaseTimeToLive(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case LeaseLeaseLeasesMessageId:
            AsyncCompleteGrpcLeaseLeaseLeases(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetEtcdHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncKVRangeHandler = handler;
    AsyncKVPutHandler = handler;
    AsyncKVDeleteRangeHandler = handler;
    AsyncKVTxnHandler = handler;
    AsyncKVCompactHandler = handler;
    AsyncWatchWatchHandler = handler;
    AsyncLeaseLeaseGrantHandler = handler;
    AsyncLeaseLeaseRevokeHandler = handler;
    AsyncLeaseLeaseKeepAliveHandler = handler;
    AsyncLeaseLeaseTimeToLiveHandler = handler;
    AsyncLeaseLeaseLeasesHandler = handler;
}

void SetEtcdIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncKVRangeHandler) {
        AsyncKVRangeHandler = handler;
    }
    if (!AsyncKVPutHandler) {
        AsyncKVPutHandler = handler;
    }
    if (!AsyncKVDeleteRangeHandler) {
        AsyncKVDeleteRangeHandler = handler;
    }
    if (!AsyncKVTxnHandler) {
        AsyncKVTxnHandler = handler;
    }
    if (!AsyncKVCompactHandler) {
        AsyncKVCompactHandler = handler;
    }
    if (!AsyncWatchWatchHandler) {
        AsyncWatchWatchHandler = handler;
    }
    if (!AsyncLeaseLeaseGrantHandler) {
        AsyncLeaseLeaseGrantHandler = handler;
    }
    if (!AsyncLeaseLeaseRevokeHandler) {
        AsyncLeaseLeaseRevokeHandler = handler;
    }
    if (!AsyncLeaseLeaseKeepAliveHandler) {
        AsyncLeaseLeaseKeepAliveHandler = handler;
    }
    if (!AsyncLeaseLeaseTimeToLiveHandler) {
        AsyncLeaseLeaseTimeToLiveHandler = handler;
    }
    if (!AsyncLeaseLeaseLeasesHandler) {
        AsyncLeaseLeaseLeasesHandler = handler;
    }
}

void SetEtcdFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    AsyncKVRangeFailedHandler = handler;
    AsyncKVPutFailedHandler = handler;
    AsyncKVDeleteRangeFailedHandler = handler;
    AsyncKVTxnFailedHandler = handler;
    AsyncKVCompactFailedHandler = handler;
    AsyncLeaseLeaseGrantFailedHandler = handler;
    AsyncLeaseLeaseRevokeFailedHandler = handler;
    AsyncLeaseLeaseTimeToLiveFailedHandler = handler;
    AsyncLeaseLeaseLeasesFailedHandler = handler;
}

void SetEtcdIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    if (!AsyncKVRangeFailedHandler) {
        AsyncKVRangeFailedHandler = handler;
    }
    if (!AsyncKVPutFailedHandler) {
        AsyncKVPutFailedHandler = handler;
    }
    if (!AsyncKVDeleteRangeFailedHandler) {
        AsyncKVDeleteRangeFailedHandler = handler;
    }
    if (!AsyncKVTxnFailedHandler) {
        AsyncKVTxnFailedHandler = handler;
    }
    if (!AsyncKVCompactFailedHandler) {
        AsyncKVCompactFailedHandler = handler;
    }
    if (!AsyncLeaseLeaseGrantFailedHandler) {
        AsyncLeaseLeaseGrantFailedHandler = handler;
    }
    if (!AsyncLeaseLeaseRevokeFailedHandler) {
        AsyncLeaseLeaseRevokeFailedHandler = handler;
    }
    if (!AsyncLeaseLeaseTimeToLiveFailedHandler) {
        AsyncLeaseLeaseTimeToLiveFailedHandler = handler;
    }
    if (!AsyncLeaseLeaseLeasesFailedHandler) {
        AsyncLeaseLeaseLeasesFailedHandler = handler;
    }
}

void SetEtcdCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

void InitEtcdGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<KVStubPtr>(nodeEntity, KV::NewStub(channel));
    registry.emplace<WatchStubPtr>(nodeEntity, Watch::NewStub(channel));
    registry.emplace<LeaseStubPtr>(nodeEntity, Lease::NewStub(channel));
    {
        GrpcTag* got_tag(tagPool.construct(WatchWatchMessageId, (void*)GrpcOperation::INIT));

        auto& client = registry.emplace<AsyncWatchWatchGrpcClient>(nodeEntity);
        registry.emplace<WatchRequestBuffer>(nodeEntity);
        registry.emplace<WatchRequestWriteInProgress>(nodeEntity);
        registry.emplace<::etcdserverpb::WatchResponse>(nodeEntity);
        registry.emplace<::etcdserverpb::WatchRequest>(nodeEntity);

        client.stream = registry
            .get<WatchStubPtr>(nodeEntity)
            ->AsyncWatch(&client.context,
                                        &registry.get<grpc::CompletionQueue>(nodeEntity),
                                        (void*)(got_tag));
    }
    {
        GrpcTag* got_tag(tagPool.construct(LeaseLeaseKeepAliveMessageId, (void*)GrpcOperation::INIT));

        auto& client = registry.emplace<AsyncLeaseLeaseKeepAliveGrpcClient>(nodeEntity);
        registry.emplace<LeaseKeepAliveRequestBuffer>(nodeEntity);
        registry.emplace<LeaseKeepAliveRequestWriteInProgress>(nodeEntity);
        registry.emplace<::etcdserverpb::LeaseKeepAliveResponse>(nodeEntity);
        registry.emplace<::etcdserverpb::LeaseKeepAliveRequest>(nodeEntity);

        client.stream = registry
            .get<LeaseStubPtr>(nodeEntity)
            ->AsyncLeaseKeepAlive(&client.context,
                                        &registry.get<grpc::CompletionQueue>(nodeEntity),
                                        (void*)(got_tag));
    }

}

}// namespace etcdserverpb
