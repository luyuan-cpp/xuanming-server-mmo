#pragma once
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/battle/battle_node.grpc.pb.h"

#include "rpc/service_metadata/battle_node_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

using BattleNodeStubPtr = std::unique_ptr<BattleNode::Stub>;
#pragma region BattleNodeCreateBattle

struct AsyncBattleNodeCreateBattleGrpcClient {
    uint32_t messageId{ BattleNodeCreateBattleMessageId };
    ClientContext context;
    Status status;
    ::CreateBattleResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::CreateBattleResponse>> response_reader;
};

class ::CreateBattleRequest;
using AsyncBattleNodeCreateBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::CreateBattleResponse&)>;
extern AsyncBattleNodeCreateBattleHandlerFunctionType AsyncBattleNodeCreateBattleHandler;

void SendBattleNodeCreateBattle(entt::registry& registry, entt::entity nodeEntity, const ::CreateBattleRequest& request);
void SendBattleNodeCreateBattle(entt::registry& registry, entt::entity nodeEntity, const ::CreateBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleNodeCreateBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleNodeDestroyBattle

struct AsyncBattleNodeDestroyBattleGrpcClient {
    uint32_t messageId{ BattleNodeDestroyBattleMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
};

class ::DestroyBattleRequest;
using AsyncBattleNodeDestroyBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleNodeDestroyBattleHandlerFunctionType AsyncBattleNodeDestroyBattleHandler;

void SendBattleNodeDestroyBattle(entt::registry& registry, entt::entity nodeEntity, const ::DestroyBattleRequest& request);
void SendBattleNodeDestroyBattle(entt::registry& registry, entt::entity nodeEntity, const ::DestroyBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleNodeDestroyBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetBattleNodeHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetBattleNodeIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void HandleBattleNodeCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitBattleNodeGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
