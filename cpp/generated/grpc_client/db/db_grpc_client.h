#pragma once
#include <chrono>
#include <functional>
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/db/db.grpc.pb.h"

#include "rpc/service_metadata/db_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

using dbStubPtr = std::unique_ptr<db::Stub>;
#pragma region dbTest

struct AsyncdbTestGrpcClient {
    uint32_t messageId{ dbTestMessageId };
    ClientContext context;
    Status status;
    ::TestResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::TestResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::TestRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncdbTestHandlerFunctionType =
    std::function<void(const ClientContext&, const ::TestResponse&)>;
extern AsyncdbTestHandlerFunctionType AsyncdbTestHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncdbTestFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::TestRequest&)>;
extern AsyncdbTestFailedHandlerFunctionType AsyncdbTestFailedHandler;

void SenddbTest(entt::registry& registry, entt::entity nodeEntity, const ::TestRequest& request);
void SenddbTest(entt::registry& registry, entt::entity nodeEntity, const ::TestRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SenddbTest(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetDbHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetDbIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetDbFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
void SetDbIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
// 本文件所有 unary 调用此后使用的 deadline;启动时由 SetGrpcCallDeadline 按目标节点类型调用。流式调用不设。
void SetDbCallDeadline(std::chrono::milliseconds deadline);
void HandleDbCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitDbGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
