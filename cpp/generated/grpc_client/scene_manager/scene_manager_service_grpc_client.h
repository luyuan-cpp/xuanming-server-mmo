#pragma once
#include <chrono>
#include <functional>
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/scene_manager/scene_manager_service.grpc.pb.h"

#include "rpc/service_metadata/scene_manager_service_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

namespace scene_manager {
using SceneManagerStubPtr = std::unique_ptr<SceneManager::Stub>;
#pragma region SceneManagerCreateScene

struct AsyncSceneManagerCreateSceneGrpcClient {
    uint32_t messageId{ SceneManagerCreateSceneMessageId };
    ClientContext context;
    Status status;
    ::scene_manager::CreateSceneResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::scene_manager::CreateSceneResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::scene_manager::CreateSceneRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncSceneManagerCreateSceneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::scene_manager::CreateSceneResponse&)>;
extern AsyncSceneManagerCreateSceneHandlerFunctionType AsyncSceneManagerCreateSceneHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncSceneManagerCreateSceneFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::scene_manager::CreateSceneRequest&)>;
extern AsyncSceneManagerCreateSceneFailedHandlerFunctionType AsyncSceneManagerCreateSceneFailedHandler;

void SendSceneManagerCreateScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::CreateSceneRequest& request);
void SendSceneManagerCreateScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::CreateSceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendSceneManagerCreateScene(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region SceneManagerDestroyScene

struct AsyncSceneManagerDestroySceneGrpcClient {
    uint32_t messageId{ SceneManagerDestroySceneMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::scene_manager::DestroySceneRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncSceneManagerDestroySceneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncSceneManagerDestroySceneHandlerFunctionType AsyncSceneManagerDestroySceneHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncSceneManagerDestroySceneFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::scene_manager::DestroySceneRequest&)>;
extern AsyncSceneManagerDestroySceneFailedHandlerFunctionType AsyncSceneManagerDestroySceneFailedHandler;

void SendSceneManagerDestroyScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::DestroySceneRequest& request);
void SendSceneManagerDestroyScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::DestroySceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendSceneManagerDestroyScene(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region SceneManagerEnterScene

struct AsyncSceneManagerEnterSceneGrpcClient {
    uint32_t messageId{ SceneManagerEnterSceneMessageId };
    ClientContext context;
    Status status;
    ::scene_manager::EnterSceneResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::scene_manager::EnterSceneResponse>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::scene_manager::EnterSceneRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncSceneManagerEnterSceneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::scene_manager::EnterSceneResponse&)>;
extern AsyncSceneManagerEnterSceneHandlerFunctionType AsyncSceneManagerEnterSceneHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncSceneManagerEnterSceneFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::scene_manager::EnterSceneRequest&)>;
extern AsyncSceneManagerEnterSceneFailedHandlerFunctionType AsyncSceneManagerEnterSceneFailedHandler;

void SendSceneManagerEnterScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::EnterSceneRequest& request);
void SendSceneManagerEnterScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::EnterSceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendSceneManagerEnterScene(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region SceneManagerLeaveScene

struct AsyncSceneManagerLeaveSceneGrpcClient {
    uint32_t messageId{ SceneManagerLeaveSceneMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::scene_manager::LeaveSceneRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncSceneManagerLeaveSceneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncSceneManagerLeaveSceneHandlerFunctionType AsyncSceneManagerLeaveSceneHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncSceneManagerLeaveSceneFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::scene_manager::LeaveSceneRequest&)>;
extern AsyncSceneManagerLeaveSceneFailedHandlerFunctionType AsyncSceneManagerLeaveSceneFailedHandler;

void SendSceneManagerLeaveScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::LeaveSceneRequest& request);
void SendSceneManagerLeaveScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::LeaveSceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendSceneManagerLeaveScene(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetSceneManagerServiceHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetSceneManagerServiceIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetSceneManagerServiceFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
void SetSceneManagerServiceIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
// 本文件所有 unary 调用此后使用的 deadline;启动时由 SetGrpcCallDeadline 按目标节点类型调用。流式调用不设。
void SetSceneManagerServiceCallDeadline(std::chrono::milliseconds deadline);
void HandleSceneManagerServiceCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitSceneManagerServiceGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace scene_manager
