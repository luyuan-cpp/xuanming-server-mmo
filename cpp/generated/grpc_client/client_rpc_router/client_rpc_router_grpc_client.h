#pragma once
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/client_rpc_router/client_rpc_router.grpc.pb.h"

#include "rpc/service_metadata/client_rpc_router_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

namespace client_rpc_router {
using ClientRpcRouterStubPtr = std::unique_ptr<ClientRpcRouter::Stub>;
#pragma region ClientRpcRouterForward

struct AsyncClientRpcRouterForwardGrpcClient {
    uint32_t messageId{ ClientRpcRouterForwardMessageId };
    ClientContext context;
    Status status;
    ::MessageContent reply;
    std::unique_ptr<ClientAsyncResponseReader<::MessageContent>> response_reader;
};

class ::client_rpc_router::ForwardRequest;
using AsyncClientRpcRouterForwardHandlerFunctionType =
    std::function<void(const ClientContext&, const ::MessageContent&)>;
extern AsyncClientRpcRouterForwardHandlerFunctionType AsyncClientRpcRouterForwardHandler;

void SendClientRpcRouterForward(entt::registry& registry, entt::entity nodeEntity, const ::client_rpc_router::ForwardRequest& request);
void SendClientRpcRouterForward(entt::registry& registry, entt::entity nodeEntity, const ::client_rpc_router::ForwardRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientRpcRouterForward(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetClientRpcRouterHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetClientRpcRouterIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void HandleClientRpcRouterCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitClientRpcRouterGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace client_rpc_router
