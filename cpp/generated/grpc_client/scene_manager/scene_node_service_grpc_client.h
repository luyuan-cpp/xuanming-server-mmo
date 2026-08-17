#pragma once
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/scene_manager/scene_node_service.grpc.pb.h"

#include "rpc/service_metadata/scene_node_service_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

namespace scene_node {
using SceneNodeGrpcStubPtr = std::unique_ptr<SceneNodeGrpc::Stub>;
#pragma region SceneNodeGrpcCreateScene

struct AsyncSceneNodeGrpcCreateSceneGrpcClient {
    uint32_t messageId{ SceneNodeGrpcCreateSceneMessageId };
    ClientContext context;
    Status status;
    ::CreateSceneResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::CreateSceneResponse>> response_reader;
};

class ::CreateSceneRequest;
using AsyncSceneNodeGrpcCreateSceneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::CreateSceneResponse&)>;
extern AsyncSceneNodeGrpcCreateSceneHandlerFunctionType AsyncSceneNodeGrpcCreateSceneHandler;

void SendSceneNodeGrpcCreateScene(entt::registry& registry, entt::entity nodeEntity, const ::CreateSceneRequest& request);
void SendSceneNodeGrpcCreateScene(entt::registry& registry, entt::entity nodeEntity, const ::CreateSceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendSceneNodeGrpcCreateScene(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region SceneNodeGrpcDestroyScene

struct AsyncSceneNodeGrpcDestroySceneGrpcClient {
    uint32_t messageId{ SceneNodeGrpcDestroySceneMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
};

class ::DestroySceneRequest;
using AsyncSceneNodeGrpcDestroySceneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncSceneNodeGrpcDestroySceneHandlerFunctionType AsyncSceneNodeGrpcDestroySceneHandler;

void SendSceneNodeGrpcDestroyScene(entt::registry& registry, entt::entity nodeEntity, const ::DestroySceneRequest& request);
void SendSceneNodeGrpcDestroyScene(entt::registry& registry, entt::entity nodeEntity, const ::DestroySceneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendSceneNodeGrpcDestroyScene(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region SceneNodeGrpcReleasePlayer

struct AsyncSceneNodeGrpcReleasePlayerGrpcClient {
    uint32_t messageId{ SceneNodeGrpcReleasePlayerMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
};

class ::scene_node::ReleasePlayerRequest;
using AsyncSceneNodeGrpcReleasePlayerHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncSceneNodeGrpcReleasePlayerHandlerFunctionType AsyncSceneNodeGrpcReleasePlayerHandler;

void SendSceneNodeGrpcReleasePlayer(entt::registry& registry, entt::entity nodeEntity, const ::scene_node::ReleasePlayerRequest& request);
void SendSceneNodeGrpcReleasePlayer(entt::registry& registry, entt::entity nodeEntity, const ::scene_node::ReleasePlayerRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendSceneNodeGrpcReleasePlayer(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region SceneNodeGrpcPrepareBattle

struct AsyncSceneNodeGrpcPrepareBattleGrpcClient {
    uint32_t messageId{ SceneNodeGrpcPrepareBattleMessageId };
    ClientContext context;
    Status status;
    ::PrepareBattleResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::PrepareBattleResponse>> response_reader;
};

class ::PrepareBattleRequest;
using AsyncSceneNodeGrpcPrepareBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::PrepareBattleResponse&)>;
extern AsyncSceneNodeGrpcPrepareBattleHandlerFunctionType AsyncSceneNodeGrpcPrepareBattleHandler;

void SendSceneNodeGrpcPrepareBattle(entt::registry& registry, entt::entity nodeEntity, const ::PrepareBattleRequest& request);
void SendSceneNodeGrpcPrepareBattle(entt::registry& registry, entt::entity nodeEntity, const ::PrepareBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendSceneNodeGrpcPrepareBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region SceneNodeGrpcCancelBattlePrepare

struct AsyncSceneNodeGrpcCancelBattlePrepareGrpcClient {
    uint32_t messageId{ SceneNodeGrpcCancelBattlePrepareMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
};

class ::CancelBattlePrepareRequest;
using AsyncSceneNodeGrpcCancelBattlePrepareHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncSceneNodeGrpcCancelBattlePrepareHandlerFunctionType AsyncSceneNodeGrpcCancelBattlePrepareHandler;

void SendSceneNodeGrpcCancelBattlePrepare(entt::registry& registry, entt::entity nodeEntity, const ::CancelBattlePrepareRequest& request);
void SendSceneNodeGrpcCancelBattlePrepare(entt::registry& registry, entt::entity nodeEntity, const ::CancelBattlePrepareRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendSceneNodeGrpcCancelBattlePrepare(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetSceneNodeServiceHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetSceneNodeServiceIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void HandleSceneNodeServiceCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitSceneNodeServiceGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace scene_node
