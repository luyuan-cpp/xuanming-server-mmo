#pragma once
#include <chrono>
#include <functional>
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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::trade::BrowseListingsRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerJubaozhaiBrowseListingsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::BrowseListingsResponse&)>;
extern AsyncClientPlayerJubaozhaiBrowseListingsHandlerFunctionType AsyncClientPlayerJubaozhaiBrowseListingsHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerJubaozhaiBrowseListingsFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::trade::BrowseListingsRequest&)>;
extern AsyncClientPlayerJubaozhaiBrowseListingsFailedHandlerFunctionType AsyncClientPlayerJubaozhaiBrowseListingsFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::trade::GetListingDetailRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerJubaozhaiGetListingDetailHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::GetListingDetailResponse&)>;
extern AsyncClientPlayerJubaozhaiGetListingDetailHandlerFunctionType AsyncClientPlayerJubaozhaiGetListingDetailHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerJubaozhaiGetListingDetailFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::trade::GetListingDetailRequest&)>;
extern AsyncClientPlayerJubaozhaiGetListingDetailFailedHandlerFunctionType AsyncClientPlayerJubaozhaiGetListingDetailFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::trade::SetFavoriteRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerJubaozhaiSetFavoriteHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::SetFavoriteResponse&)>;
extern AsyncClientPlayerJubaozhaiSetFavoriteHandlerFunctionType AsyncClientPlayerJubaozhaiSetFavoriteHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerJubaozhaiSetFavoriteFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::trade::SetFavoriteRequest&)>;
extern AsyncClientPlayerJubaozhaiSetFavoriteFailedHandlerFunctionType AsyncClientPlayerJubaozhaiSetFavoriteFailedHandler;

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
    // 失败时交还失败处理器:应答里的回显字段与服务端回写的 metadata 这时都拿不到(见 GrpcCallFailure)。
    // 必须是副本:gate 通用路径发出的是 gRpcMethodRegistry 里的共享原型,下一条客户端消息就会覆盖它。
    ::trade::GetMyShelfRequest request;
    GrpcSentMetadata sentMetadata;
};

using AsyncClientPlayerJubaozhaiGetMyShelfHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::GetMyShelfResponse&)>;
extern AsyncClientPlayerJubaozhaiGetMyShelfHandlerFunctionType AsyncClientPlayerJubaozhaiGetMyShelfHandler;
// 调用以非 OK 状态结束时的回调(deadline 到期 / 连接不可用 / 服务端报错),拿到发出的请求副本。
// 未装时只打 ERROR 日志。失败 = 结果未知,语义见 GrpcCallFailure。
using AsyncClientPlayerJubaozhaiGetMyShelfFailedHandlerFunctionType =
    std::function<void(const GrpcCallFailure&, const ::trade::GetMyShelfRequest&)>;
extern AsyncClientPlayerJubaozhaiGetMyShelfFailedHandlerFunctionType AsyncClientPlayerJubaozhaiGetMyShelfFailedHandler;

void SendClientPlayerJubaozhaiGetMyShelf(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetMyShelfRequest& request);
void SendClientPlayerJubaozhaiGetMyShelf(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetMyShelfRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendClientPlayerJubaozhaiGetMyShelf(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetJubaozhaiHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetJubaozhaiIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetJubaozhaiFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
void SetJubaozhaiIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);
// 本文件所有 unary 调用此后使用的 deadline;启动时由 SetGrpcCallDeadline 按目标节点类型调用。流式调用不设。
void SetJubaozhaiCallDeadline(std::chrono::milliseconds deadline);
void HandleJubaozhaiCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitJubaozhaiGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace trade
