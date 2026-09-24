#pragma once
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/trade/trade_admin.grpc.pb.h"

#include "rpc/service_metadata/trade_admin_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

namespace trade {
using TradeAdminStubPtr = std::unique_ptr<TradeAdmin::Stub>;
#pragma region TradeAdminSeedListing

struct AsyncTradeAdminSeedListingGrpcClient {
    uint32_t messageId{ TradeAdminSeedListingMessageId };
    ClientContext context;
    Status status;
    ::trade::SeedListingResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::trade::SeedListingResponse>> response_reader;
};

using AsyncTradeAdminSeedListingHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::SeedListingResponse&)>;
extern AsyncTradeAdminSeedListingHandlerFunctionType AsyncTradeAdminSeedListingHandler;

void SendTradeAdminSeedListing(entt::registry& registry, entt::entity nodeEntity, const ::trade::SeedListingRequest& request);
void SendTradeAdminSeedListing(entt::registry& registry, entt::entity nodeEntity, const ::trade::SeedListingRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendTradeAdminSeedListing(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetTradeAdminHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetTradeAdminIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void HandleTradeAdminCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitTradeAdminGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace trade
