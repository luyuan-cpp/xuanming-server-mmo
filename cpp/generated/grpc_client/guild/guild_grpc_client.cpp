#include "muduo/base/Logging.h"

#include "guild_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
}

namespace guildpb {
struct GuildCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region GuildServiceCreateGuild
boost::object_pool<AsyncGuildServiceCreateGuildGrpcClient> GuildServiceCreateGuildPool;
using AsyncGuildServiceCreateGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::CreateGuildResponse&)>;
AsyncGuildServiceCreateGuildHandlerFunctionType AsyncGuildServiceCreateGuildHandler;

void AsyncCompleteGrpcGuildServiceCreateGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceCreateGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceCreateGuildHandler) {
            AsyncGuildServiceCreateGuildHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceCreateGuildPool.destroy(call);
}

void SendGuildServiceCreateGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CreateGuildRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceCreateGuildPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncCreateGuild(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceCreateGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceCreateGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CreateGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceCreateGuildPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncCreateGuild(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceCreateGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceCreateGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::CreateGuildRequest& derived = static_cast<const ::guildpb::CreateGuildRequest&>(message);
    SendGuildServiceCreateGuild(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceGetGuild
boost::object_pool<AsyncGuildServiceGetGuildGrpcClient> GuildServiceGetGuildPool;
using AsyncGuildServiceGetGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::GetGuildResponse&)>;
AsyncGuildServiceGetGuildHandlerFunctionType AsyncGuildServiceGetGuildHandler;

void AsyncCompleteGrpcGuildServiceGetGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceGetGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceGetGuildHandler) {
            AsyncGuildServiceGetGuildHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceGetGuildPool.destroy(call);
}

void SendGuildServiceGetGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceGetGuildPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetGuild(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceGetGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceGetGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceGetGuildPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetGuild(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceGetGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceGetGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::GetGuildRequest& derived = static_cast<const ::guildpb::GetGuildRequest&>(message);
    SendGuildServiceGetGuild(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceGetPlayerGuild
boost::object_pool<AsyncGuildServiceGetPlayerGuildGrpcClient> GuildServiceGetPlayerGuildPool;
using AsyncGuildServiceGetPlayerGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::GetPlayerGuildResponse&)>;
AsyncGuildServiceGetPlayerGuildHandlerFunctionType AsyncGuildServiceGetPlayerGuildHandler;

void AsyncCompleteGrpcGuildServiceGetPlayerGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceGetPlayerGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceGetPlayerGuildHandler) {
            AsyncGuildServiceGetPlayerGuildHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceGetPlayerGuildPool.destroy(call);
}

void SendGuildServiceGetPlayerGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetPlayerGuildRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceGetPlayerGuildPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetPlayerGuild(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceGetPlayerGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceGetPlayerGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetPlayerGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceGetPlayerGuildPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetPlayerGuild(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceGetPlayerGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceGetPlayerGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::GetPlayerGuildRequest& derived = static_cast<const ::guildpb::GetPlayerGuildRequest&>(message);
    SendGuildServiceGetPlayerGuild(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceJoinGuild
boost::object_pool<AsyncGuildServiceJoinGuildGrpcClient> GuildServiceJoinGuildPool;
using AsyncGuildServiceJoinGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::JoinGuildResponse&)>;
AsyncGuildServiceJoinGuildHandlerFunctionType AsyncGuildServiceJoinGuildHandler;

void AsyncCompleteGrpcGuildServiceJoinGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceJoinGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceJoinGuildHandler) {
            AsyncGuildServiceJoinGuildHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceJoinGuildPool.destroy(call);
}

void SendGuildServiceJoinGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::JoinGuildRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceJoinGuildPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncJoinGuild(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceJoinGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceJoinGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::JoinGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceJoinGuildPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncJoinGuild(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceJoinGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceJoinGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::JoinGuildRequest& derived = static_cast<const ::guildpb::JoinGuildRequest&>(message);
    SendGuildServiceJoinGuild(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceLeaveGuild
boost::object_pool<AsyncGuildServiceLeaveGuildGrpcClient> GuildServiceLeaveGuildPool;
using AsyncGuildServiceLeaveGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::LeaveGuildResponse&)>;
AsyncGuildServiceLeaveGuildHandlerFunctionType AsyncGuildServiceLeaveGuildHandler;

void AsyncCompleteGrpcGuildServiceLeaveGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceLeaveGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceLeaveGuildHandler) {
            AsyncGuildServiceLeaveGuildHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceLeaveGuildPool.destroy(call);
}

void SendGuildServiceLeaveGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::LeaveGuildRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceLeaveGuildPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncLeaveGuild(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceLeaveGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceLeaveGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::LeaveGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceLeaveGuildPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncLeaveGuild(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceLeaveGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceLeaveGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::LeaveGuildRequest& derived = static_cast<const ::guildpb::LeaveGuildRequest&>(message);
    SendGuildServiceLeaveGuild(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceDisbandGuild
boost::object_pool<AsyncGuildServiceDisbandGuildGrpcClient> GuildServiceDisbandGuildPool;
using AsyncGuildServiceDisbandGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::DisbandGuildResponse&)>;
AsyncGuildServiceDisbandGuildHandlerFunctionType AsyncGuildServiceDisbandGuildHandler;

void AsyncCompleteGrpcGuildServiceDisbandGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceDisbandGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceDisbandGuildHandler) {
            AsyncGuildServiceDisbandGuildHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceDisbandGuildPool.destroy(call);
}

void SendGuildServiceDisbandGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::DisbandGuildRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceDisbandGuildPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncDisbandGuild(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceDisbandGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceDisbandGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::DisbandGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceDisbandGuildPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncDisbandGuild(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceDisbandGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceDisbandGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::DisbandGuildRequest& derived = static_cast<const ::guildpb::DisbandGuildRequest&>(message);
    SendGuildServiceDisbandGuild(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceSetAnnouncement
boost::object_pool<AsyncGuildServiceSetAnnouncementGrpcClient> GuildServiceSetAnnouncementPool;
using AsyncGuildServiceSetAnnouncementHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::SetAnnouncementResponse&)>;
AsyncGuildServiceSetAnnouncementHandlerFunctionType AsyncGuildServiceSetAnnouncementHandler;

void AsyncCompleteGrpcGuildServiceSetAnnouncement(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceSetAnnouncementGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceSetAnnouncementHandler) {
            AsyncGuildServiceSetAnnouncementHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceSetAnnouncementPool.destroy(call);
}

void SendGuildServiceSetAnnouncement(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetAnnouncementRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceSetAnnouncementPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncSetAnnouncement(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceSetAnnouncementMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceSetAnnouncement(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetAnnouncementRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceSetAnnouncementPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncSetAnnouncement(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceSetAnnouncementMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceSetAnnouncement(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::SetAnnouncementRequest& derived = static_cast<const ::guildpb::SetAnnouncementRequest&>(message);
    SendGuildServiceSetAnnouncement(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceUpdateGuildScore
boost::object_pool<AsyncGuildServiceUpdateGuildScoreGrpcClient> GuildServiceUpdateGuildScorePool;
using AsyncGuildServiceUpdateGuildScoreHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::UpdateGuildScoreResponse&)>;
AsyncGuildServiceUpdateGuildScoreHandlerFunctionType AsyncGuildServiceUpdateGuildScoreHandler;

void AsyncCompleteGrpcGuildServiceUpdateGuildScore(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceUpdateGuildScoreGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceUpdateGuildScoreHandler) {
            AsyncGuildServiceUpdateGuildScoreHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceUpdateGuildScorePool.destroy(call);
}

void SendGuildServiceUpdateGuildScore(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::UpdateGuildScoreRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceUpdateGuildScorePool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncUpdateGuildScore(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceUpdateGuildScoreMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceUpdateGuildScore(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::UpdateGuildScoreRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceUpdateGuildScorePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncUpdateGuildScore(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceUpdateGuildScoreMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceUpdateGuildScore(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::UpdateGuildScoreRequest& derived = static_cast<const ::guildpb::UpdateGuildScoreRequest&>(message);
    SendGuildServiceUpdateGuildScore(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceGetGuildRank
boost::object_pool<AsyncGuildServiceGetGuildRankGrpcClient> GuildServiceGetGuildRankPool;
using AsyncGuildServiceGetGuildRankHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::GetGuildRankResponse&)>;
AsyncGuildServiceGetGuildRankHandlerFunctionType AsyncGuildServiceGetGuildRankHandler;

void AsyncCompleteGrpcGuildServiceGetGuildRank(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceGetGuildRankGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceGetGuildRankHandler) {
            AsyncGuildServiceGetGuildRankHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceGetGuildRankPool.destroy(call);
}

void SendGuildServiceGetGuildRank(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceGetGuildRankPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetGuildRank(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceGetGuildRankMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceGetGuildRank(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceGetGuildRankPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetGuildRank(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceGetGuildRankMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceGetGuildRank(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::GetGuildRankRequest& derived = static_cast<const ::guildpb::GetGuildRankRequest&>(message);
    SendGuildServiceGetGuildRank(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceGetGuildRankByGuild
boost::object_pool<AsyncGuildServiceGetGuildRankByGuildGrpcClient> GuildServiceGetGuildRankByGuildPool;
using AsyncGuildServiceGetGuildRankByGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::GetGuildRankByGuildResponse&)>;
AsyncGuildServiceGetGuildRankByGuildHandlerFunctionType AsyncGuildServiceGetGuildRankByGuildHandler;

void AsyncCompleteGrpcGuildServiceGetGuildRankByGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceGetGuildRankByGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceGetGuildRankByGuildHandler) {
            AsyncGuildServiceGetGuildRankByGuildHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceGetGuildRankByGuildPool.destroy(call);
}

void SendGuildServiceGetGuildRankByGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankByGuildRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceGetGuildRankByGuildPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetGuildRankByGuild(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceGetGuildRankByGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceGetGuildRankByGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankByGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceGetGuildRankByGuildPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetGuildRankByGuild(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceGetGuildRankByGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceGetGuildRankByGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::GetGuildRankByGuildRequest& derived = static_cast<const ::guildpb::GetGuildRankByGuildRequest&>(message);
    SendGuildServiceGetGuildRankByGuild(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleGuildCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case GuildServiceCreateGuildMessageId:
            AsyncCompleteGrpcGuildServiceCreateGuild(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceGetGuildMessageId:
            AsyncCompleteGrpcGuildServiceGetGuild(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceGetPlayerGuildMessageId:
            AsyncCompleteGrpcGuildServiceGetPlayerGuild(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceJoinGuildMessageId:
            AsyncCompleteGrpcGuildServiceJoinGuild(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceLeaveGuildMessageId:
            AsyncCompleteGrpcGuildServiceLeaveGuild(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceDisbandGuildMessageId:
            AsyncCompleteGrpcGuildServiceDisbandGuild(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceSetAnnouncementMessageId:
            AsyncCompleteGrpcGuildServiceSetAnnouncement(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceUpdateGuildScoreMessageId:
            AsyncCompleteGrpcGuildServiceUpdateGuildScore(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceGetGuildRankMessageId:
            AsyncCompleteGrpcGuildServiceGetGuildRank(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceGetGuildRankByGuildMessageId:
            AsyncCompleteGrpcGuildServiceGetGuildRankByGuild(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetGuildHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncGuildServiceCreateGuildHandler = handler;
    AsyncGuildServiceGetGuildHandler = handler;
    AsyncGuildServiceGetPlayerGuildHandler = handler;
    AsyncGuildServiceJoinGuildHandler = handler;
    AsyncGuildServiceLeaveGuildHandler = handler;
    AsyncGuildServiceDisbandGuildHandler = handler;
    AsyncGuildServiceSetAnnouncementHandler = handler;
    AsyncGuildServiceUpdateGuildScoreHandler = handler;
    AsyncGuildServiceGetGuildRankHandler = handler;
    AsyncGuildServiceGetGuildRankByGuildHandler = handler;
}

void SetGuildIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncGuildServiceCreateGuildHandler) {
        AsyncGuildServiceCreateGuildHandler = handler;
    }
    if (!AsyncGuildServiceGetGuildHandler) {
        AsyncGuildServiceGetGuildHandler = handler;
    }
    if (!AsyncGuildServiceGetPlayerGuildHandler) {
        AsyncGuildServiceGetPlayerGuildHandler = handler;
    }
    if (!AsyncGuildServiceJoinGuildHandler) {
        AsyncGuildServiceJoinGuildHandler = handler;
    }
    if (!AsyncGuildServiceLeaveGuildHandler) {
        AsyncGuildServiceLeaveGuildHandler = handler;
    }
    if (!AsyncGuildServiceDisbandGuildHandler) {
        AsyncGuildServiceDisbandGuildHandler = handler;
    }
    if (!AsyncGuildServiceSetAnnouncementHandler) {
        AsyncGuildServiceSetAnnouncementHandler = handler;
    }
    if (!AsyncGuildServiceUpdateGuildScoreHandler) {
        AsyncGuildServiceUpdateGuildScoreHandler = handler;
    }
    if (!AsyncGuildServiceGetGuildRankHandler) {
        AsyncGuildServiceGetGuildRankHandler = handler;
    }
    if (!AsyncGuildServiceGetGuildRankByGuildHandler) {
        AsyncGuildServiceGetGuildRankByGuildHandler = handler;
    }
}

void InitGuildGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<GuildServiceStubPtr>(nodeEntity, GuildService::NewStub(channel));

}

}// namespace guildpb
