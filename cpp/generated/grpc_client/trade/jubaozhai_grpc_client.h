#pragma once
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/trade/jubaozhai.grpc.pb.h"

#include "rpc/service_metadata/jubaozhai_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

namespace trade {
using ClientPlayerJubaozhaiStubPtr = std::unique_ptr<ClientPlayerJubaozhai::Stub>;
#pragma region ClientPlayerJubaozhaiBrowseListings

struct AsyncClientPlayerJubaozhaiBrowseListingsGrpcClient {
    uint32_t messageId{ ClientPlayerJubaozhaiBrowseListingsMessageId };
    ClientContext context;
    Status status;
    ::trade::BrowseListingsResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::trade::BrowseListingsResponse>> response_reader;
};

class ::trade::BrowseListingsRequest;
using AsyncClientPlayerJubaozhaiBrowseListingsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::BrowseListingsResponse&)>;
extern AsyncClientPlayerJubaozhaiBrowseListingsHandlerFunctionType AsyncClientPlayerJubaozhaiBrowseListingsHandler;

void SendClientPlayerJubaozhaiBrowseListings(entt::registry& registry, entt::entity nodeEntity, const ::trade::BrowseListingsRequest& request);
void SendClientPlayerJubaozhaiBrowseListings(entt::registry& registry, entt::entity nodeEntity, const ::trade::BrowseListingsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerJubaozhaiBrowseListings(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetListingDetail

struct AsyncClientPlayerJubaozhaiGetListingDetailGrpcClient {
    uint32_t messageId{ ClientPlayerJubaozhaiGetListingDetailMessageId };
    ClientContext context;
    Status status;
    ::trade::GetListingDetailResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::trade::GetListingDetailResponse>> response_reader;
};

class ::trade::GetListingDetailRequest;
using AsyncClientPlayerJubaozhaiGetListingDetailHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::GetListingDetailResponse&)>;
extern AsyncClientPlayerJubaozhaiGetListingDetailHandlerFunctionType AsyncClientPlayerJubaozhaiGetListingDetailHandler;

void SendClientPlayerJubaozhaiGetListingDetail(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetListingDetailRequest& request);
void SendClientPlayerJubaozhaiGetListingDetail(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetListingDetailRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerJubaozhaiGetListingDetail(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerJubaozhaiSetFavorite

struct AsyncClientPlayerJubaozhaiSetFavoriteGrpcClient {
    uint32_t messageId{ ClientPlayerJubaozhaiSetFavoriteMessageId };
    ClientContext context;
    Status status;
    ::trade::SetFavoriteResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::trade::SetFavoriteResponse>> response_reader;
};

class ::trade::SetFavoriteRequest;
using AsyncClientPlayerJubaozhaiSetFavoriteHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::SetFavoriteResponse&)>;
extern AsyncClientPlayerJubaozhaiSetFavoriteHandlerFunctionType AsyncClientPlayerJubaozhaiSetFavoriteHandler;

void SendClientPlayerJubaozhaiSetFavorite(entt::registry& registry, entt::entity nodeEntity, const ::trade::SetFavoriteRequest& request);
void SendClientPlayerJubaozhaiSetFavorite(entt::registry& registry, entt::entity nodeEntity, const ::trade::SetFavoriteRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerJubaozhaiSetFavorite(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetMyShelf

struct AsyncClientPlayerJubaozhaiGetMyShelfGrpcClient {
    uint32_t messageId{ ClientPlayerJubaozhaiGetMyShelfMessageId };
    ClientContext context;
    Status status;
    ::trade::GetMyShelfResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::trade::GetMyShelfResponse>> response_reader;
};

class ::trade::GetMyShelfRequest;
using AsyncClientPlayerJubaozhaiGetMyShelfHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::GetMyShelfResponse&)>;
extern AsyncClientPlayerJubaozhaiGetMyShelfHandlerFunctionType AsyncClientPlayerJubaozhaiGetMyShelfHandler;

void SendClientPlayerJubaozhaiGetMyShelf(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetMyShelfRequest& request);
void SendClientPlayerJubaozhaiGetMyShelf(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetMyShelfRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerJubaozhaiGetMyShelf(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetJubaozhaiHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetJubaozhaiIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void HandleJubaozhaiCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitJubaozhaiGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace trade
