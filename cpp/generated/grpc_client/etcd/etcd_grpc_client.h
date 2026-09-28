#pragma once
#include <chrono>
#include <functional>
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/etcd/etcd.grpc.pb.h"

#include "rpc/service_metadata/etcd_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

namespace etcdserverpb {
using KVStubPtr = std::unique_ptr<KV::Stub>;
#pragma region KVRange

struct AsyncKVRangeGrpcClient {
    uint32_t messageId{ KVRangeMessageId };
    ClientContext context;
    Status status;
    ::etcdserverpb::RangeResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::etcdserverpb::RangeResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::etcdserverpb::RangeRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncKVRangeHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::RangeResponse&)>;
extern AsyncKVRangeHandlerFunctionType AsyncKVRangeHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncKVRangeFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::etcdserverpb::RangeRequest&)>;
extern AsyncKVRangeFailedHandlerFunctionType AsyncKVRangeFailedHandler;

void SendKVRange(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::RangeRequest& request);
void SendKVRange(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::RangeRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendKVRange(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region KVPut

struct AsyncKVPutGrpcClient {
    uint32_t messageId{ KVPutMessageId };
    ClientContext context;
    Status status;
    ::etcdserverpb::PutResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::etcdserverpb::PutResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::etcdserverpb::PutRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncKVPutHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::PutResponse&)>;
extern AsyncKVPutHandlerFunctionType AsyncKVPutHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncKVPutFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::etcdserverpb::PutRequest&)>;
extern AsyncKVPutFailedHandlerFunctionType AsyncKVPutFailedHandler;

void SendKVPut(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::PutRequest& request);
void SendKVPut(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::PutRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendKVPut(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region KVDeleteRange

struct AsyncKVDeleteRangeGrpcClient {
    uint32_t messageId{ KVDeleteRangeMessageId };
    ClientContext context;
    Status status;
    ::etcdserverpb::DeleteRangeResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::etcdserverpb::DeleteRangeResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::etcdserverpb::DeleteRangeRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncKVDeleteRangeHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::DeleteRangeResponse&)>;
extern AsyncKVDeleteRangeHandlerFunctionType AsyncKVDeleteRangeHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncKVDeleteRangeFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::etcdserverpb::DeleteRangeRequest&)>;
extern AsyncKVDeleteRangeFailedHandlerFunctionType AsyncKVDeleteRangeFailedHandler;

void SendKVDeleteRange(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::DeleteRangeRequest& request);
void SendKVDeleteRange(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::DeleteRangeRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendKVDeleteRange(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region KVTxn

struct AsyncKVTxnGrpcClient {
    uint32_t messageId{ KVTxnMessageId };
    ClientContext context;
    Status status;
    ::etcdserverpb::TxnResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::etcdserverpb::TxnResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::etcdserverpb::TxnRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncKVTxnHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::TxnResponse&)>;
extern AsyncKVTxnHandlerFunctionType AsyncKVTxnHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncKVTxnFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::etcdserverpb::TxnRequest&)>;
extern AsyncKVTxnFailedHandlerFunctionType AsyncKVTxnFailedHandler;

void SendKVTxn(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::TxnRequest& request);
void SendKVTxn(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::TxnRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendKVTxn(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region KVCompact

struct AsyncKVCompactGrpcClient {
    uint32_t messageId{ KVCompactMessageId };
    ClientContext context;
    Status status;
    ::etcdserverpb::CompactionResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::etcdserverpb::CompactionResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::etcdserverpb::CompactionRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncKVCompactHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::CompactionResponse&)>;
extern AsyncKVCompactHandlerFunctionType AsyncKVCompactHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncKVCompactFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::etcdserverpb::CompactionRequest&)>;
extern AsyncKVCompactFailedHandlerFunctionType AsyncKVCompactFailedHandler;

void SendKVCompact(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::CompactionRequest& request);
void SendKVCompact(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::CompactionRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendKVCompact(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
using WatchStubPtr = std::unique_ptr<Watch::Stub>;
#pragma region WatchWatch

struct AsyncWatchWatchGrpcClient {
    uint32_t messageId{ WatchWatchMessageId };
    ClientContext context;
    Status status;
    ::etcdserverpb::WatchResponse reply;
    std::unique_ptr<grpc::ClientAsyncReaderWriter<::etcdserverpb::WatchRequest, ::etcdserverpb::WatchResponse>> stream;
};

struct WatchRequestBuffer {
    boost::circular_buffer<::etcdserverpb::WatchRequest> pendingWritesBuffer{200};
};

struct WatchRequestWriteInProgress {
    bool isInProgress{false};
};

using AsyncWatchWatchHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::WatchResponse&)>;
extern AsyncWatchWatchHandlerFunctionType AsyncWatchWatchHandler;

void SendWatchWatch(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::WatchRequest& request);
void SendWatchWatch(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::WatchRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendWatchWatch(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
using LeaseStubPtr = std::unique_ptr<Lease::Stub>;
#pragma region LeaseLeaseGrant

struct AsyncLeaseLeaseGrantGrpcClient {
    uint32_t messageId{ LeaseLeaseGrantMessageId };
    ClientContext context;
    Status status;
    ::etcdserverpb::LeaseGrantResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::etcdserverpb::LeaseGrantResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::etcdserverpb::LeaseGrantRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncLeaseLeaseGrantHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::LeaseGrantResponse&)>;
extern AsyncLeaseLeaseGrantHandlerFunctionType AsyncLeaseLeaseGrantHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncLeaseLeaseGrantFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::etcdserverpb::LeaseGrantRequest&)>;
extern AsyncLeaseLeaseGrantFailedHandlerFunctionType AsyncLeaseLeaseGrantFailedHandler;

void SendLeaseLeaseGrant(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseGrantRequest& request);
void SendLeaseLeaseGrant(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseGrantRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendLeaseLeaseGrant(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region LeaseLeaseRevoke

struct AsyncLeaseLeaseRevokeGrpcClient {
    uint32_t messageId{ LeaseLeaseRevokeMessageId };
    ClientContext context;
    Status status;
    ::etcdserverpb::LeaseRevokeResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::etcdserverpb::LeaseRevokeResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::etcdserverpb::LeaseRevokeRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncLeaseLeaseRevokeHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::LeaseRevokeResponse&)>;
extern AsyncLeaseLeaseRevokeHandlerFunctionType AsyncLeaseLeaseRevokeHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncLeaseLeaseRevokeFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::etcdserverpb::LeaseRevokeRequest&)>;
extern AsyncLeaseLeaseRevokeFailedHandlerFunctionType AsyncLeaseLeaseRevokeFailedHandler;

void SendLeaseLeaseRevoke(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseRevokeRequest& request);
void SendLeaseLeaseRevoke(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseRevokeRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendLeaseLeaseRevoke(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region LeaseLeaseKeepAlive

struct AsyncLeaseLeaseKeepAliveGrpcClient {
    uint32_t messageId{ LeaseLeaseKeepAliveMessageId };
    ClientContext context;
    Status status;
    ::etcdserverpb::LeaseKeepAliveResponse reply;
    std::unique_ptr<grpc::ClientAsyncReaderWriter<::etcdserverpb::LeaseKeepAliveRequest, ::etcdserverpb::LeaseKeepAliveResponse>> stream;
};

struct LeaseKeepAliveRequestBuffer {
    boost::circular_buffer<::etcdserverpb::LeaseKeepAliveRequest> pendingWritesBuffer{200};
};

struct LeaseKeepAliveRequestWriteInProgress {
    bool isInProgress{false};
};

using AsyncLeaseLeaseKeepAliveHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::LeaseKeepAliveResponse&)>;
extern AsyncLeaseLeaseKeepAliveHandlerFunctionType AsyncLeaseLeaseKeepAliveHandler;

void SendLeaseLeaseKeepAlive(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseKeepAliveRequest& request);
void SendLeaseLeaseKeepAlive(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseKeepAliveRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendLeaseLeaseKeepAlive(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region LeaseLeaseTimeToLive

struct AsyncLeaseLeaseTimeToLiveGrpcClient {
    uint32_t messageId{ LeaseLeaseTimeToLiveMessageId };
    ClientContext context;
    Status status;
    ::etcdserverpb::LeaseTimeToLiveResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::etcdserverpb::LeaseTimeToLiveResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::etcdserverpb::LeaseTimeToLiveRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncLeaseLeaseTimeToLiveHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::LeaseTimeToLiveResponse&)>;
extern AsyncLeaseLeaseTimeToLiveHandlerFunctionType AsyncLeaseLeaseTimeToLiveHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncLeaseLeaseTimeToLiveFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::etcdserverpb::LeaseTimeToLiveRequest&)>;
extern AsyncLeaseLeaseTimeToLiveFailedHandlerFunctionType AsyncLeaseLeaseTimeToLiveFailedHandler;

void SendLeaseLeaseTimeToLive(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseTimeToLiveRequest& request);
void SendLeaseLeaseTimeToLive(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseTimeToLiveRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendLeaseLeaseTimeToLive(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region LeaseLeaseLeases

struct AsyncLeaseLeaseLeasesGrpcClient {
    uint32_t messageId{ LeaseLeaseLeasesMessageId };
    ClientContext context;
    Status status;
    ::etcdserverpb::LeaseLeasesResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::etcdserverpb::LeaseLeasesResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::etcdserverpb::LeaseLeasesRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncLeaseLeaseLeasesHandlerFunctionType =
    std::function<void(const ClientContext&, const ::etcdserverpb::LeaseLeasesResponse&)>;
extern AsyncLeaseLeaseLeasesHandlerFunctionType AsyncLeaseLeaseLeasesHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncLeaseLeaseLeasesFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::etcdserverpb::LeaseLeasesRequest&)>;
extern AsyncLeaseLeaseLeasesFailedHandlerFunctionType AsyncLeaseLeaseLeasesFailedHandler;

void SendLeaseLeaseLeases(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseLeasesRequest& request);
void SendLeaseLeaseLeases(entt::registry& registry, entt::entity nodeEntity, const ::etcdserverpb::LeaseLeasesRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendLeaseLeaseLeases(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetEtcdHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetEtcdIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetEtcdFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
void SetEtcdIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
// 本文件所有 unary 调用此后使用的 deadline;启动时由 SetGrpcCallDeadline 按目标节点类型调用。流式调用不设。
void SetEtcdCallDeadline(std::chrono::milliseconds deadline);
void HandleEtcdCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitEtcdGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace etcdserverpb
