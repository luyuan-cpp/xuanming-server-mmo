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
#pragma region BattleNodeAddObserver

struct AsyncBattleNodeAddObserverGrpcClient {
    uint32_t messageId{ BattleNodeAddObserverMessageId };
    ClientContext context;
    Status status;
    ::AddObserverResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::AddObserverResponse>> response_reader;
};

class ::AddObserverRequest;
using AsyncBattleNodeAddObserverHandlerFunctionType =
    std::function<void(const ClientContext&, const ::AddObserverResponse&)>;
extern AsyncBattleNodeAddObserverHandlerFunctionType AsyncBattleNodeAddObserverHandler;

void SendBattleNodeAddObserver(entt::registry& registry, entt::entity nodeEntity, const ::AddObserverRequest& request);
void SendBattleNodeAddObserver(entt::registry& registry, entt::entity nodeEntity, const ::AddObserverRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleNodeAddObserver(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleNodeRemoveObserver

struct AsyncBattleNodeRemoveObserverGrpcClient {
    uint32_t messageId{ BattleNodeRemoveObserverMessageId };
    ClientContext context;
    Status status;
    ::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::Empty>> response_reader;
};

class ::RemoveObserverRequest;
using AsyncBattleNodeRemoveObserverHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
extern AsyncBattleNodeRemoveObserverHandlerFunctionType AsyncBattleNodeRemoveObserverHandler;

void SendBattleNodeRemoveObserver(entt::registry& registry, entt::entity nodeEntity, const ::RemoveObserverRequest& request);
void SendBattleNodeRemoveObserver(entt::registry& registry, entt::entity nodeEntity, const ::RemoveObserverRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleNodeRemoveObserver(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region BattleNodeIssueBattleTicket

struct AsyncBattleNodeIssueBattleTicketGrpcClient {
    uint32_t messageId{ BattleNodeIssueBattleTicketMessageId };
    ClientContext context;
    Status status;
    ::IssueBattleTicketResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::IssueBattleTicketResponse>> response_reader;
};

class ::IssueBattleTicketRequest;
using AsyncBattleNodeIssueBattleTicketHandlerFunctionType =
    std::function<void(const ClientContext&, const ::IssueBattleTicketResponse&)>;
extern AsyncBattleNodeIssueBattleTicketHandlerFunctionType AsyncBattleNodeIssueBattleTicketHandler;

void SendBattleNodeIssueBattleTicket(entt::registry& registry, entt::entity nodeEntity, const ::IssueBattleTicketRequest& request);
void SendBattleNodeIssueBattleTicket(entt::registry& registry, entt::entity nodeEntity, const ::IssueBattleTicketRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendBattleNodeIssueBattleTicket(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetBattleNodeHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetBattleNodeIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void HandleBattleNodeCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitBattleNodeGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
