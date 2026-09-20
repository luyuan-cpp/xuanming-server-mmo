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
#pragma region GuildServiceSetGuildMemberRole
boost::object_pool<AsyncGuildServiceSetGuildMemberRoleGrpcClient> GuildServiceSetGuildMemberRolePool;
using AsyncGuildServiceSetGuildMemberRoleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::SetGuildMemberRoleResponse&)>;
AsyncGuildServiceSetGuildMemberRoleHandlerFunctionType AsyncGuildServiceSetGuildMemberRoleHandler;

void AsyncCompleteGrpcGuildServiceSetGuildMemberRole(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceSetGuildMemberRoleGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceSetGuildMemberRoleHandler) {
            AsyncGuildServiceSetGuildMemberRoleHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceSetGuildMemberRolePool.destroy(call);
}

void SendGuildServiceSetGuildMemberRole(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetGuildMemberRoleRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceSetGuildMemberRolePool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncSetGuildMemberRole(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceSetGuildMemberRoleMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceSetGuildMemberRole(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetGuildMemberRoleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceSetGuildMemberRolePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncSetGuildMemberRole(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceSetGuildMemberRoleMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceSetGuildMemberRole(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::SetGuildMemberRoleRequest& derived = static_cast<const ::guildpb::SetGuildMemberRoleRequest&>(message);
    SendGuildServiceSetGuildMemberRole(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceKickGuildMember
boost::object_pool<AsyncGuildServiceKickGuildMemberGrpcClient> GuildServiceKickGuildMemberPool;
using AsyncGuildServiceKickGuildMemberHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::KickGuildMemberResponse&)>;
AsyncGuildServiceKickGuildMemberHandlerFunctionType AsyncGuildServiceKickGuildMemberHandler;

void AsyncCompleteGrpcGuildServiceKickGuildMember(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceKickGuildMemberGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceKickGuildMemberHandler) {
            AsyncGuildServiceKickGuildMemberHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceKickGuildMemberPool.destroy(call);
}

void SendGuildServiceKickGuildMember(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::KickGuildMemberRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceKickGuildMemberPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncKickGuildMember(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceKickGuildMemberMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceKickGuildMember(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::KickGuildMemberRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceKickGuildMemberPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncKickGuildMember(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceKickGuildMemberMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceKickGuildMember(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::KickGuildMemberRequest& derived = static_cast<const ::guildpb::KickGuildMemberRequest&>(message);
    SendGuildServiceKickGuildMember(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceTransferGuildLeader
boost::object_pool<AsyncGuildServiceTransferGuildLeaderGrpcClient> GuildServiceTransferGuildLeaderPool;
using AsyncGuildServiceTransferGuildLeaderHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::TransferGuildLeaderResponse&)>;
AsyncGuildServiceTransferGuildLeaderHandlerFunctionType AsyncGuildServiceTransferGuildLeaderHandler;

void AsyncCompleteGrpcGuildServiceTransferGuildLeader(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceTransferGuildLeaderGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceTransferGuildLeaderHandler) {
            AsyncGuildServiceTransferGuildLeaderHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceTransferGuildLeaderPool.destroy(call);
}

void SendGuildServiceTransferGuildLeader(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::TransferGuildLeaderRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceTransferGuildLeaderPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncTransferGuildLeader(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceTransferGuildLeaderMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceTransferGuildLeader(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::TransferGuildLeaderRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceTransferGuildLeaderPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncTransferGuildLeader(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceTransferGuildLeaderMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceTransferGuildLeader(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::TransferGuildLeaderRequest& derived = static_cast<const ::guildpb::TransferGuildLeaderRequest&>(message);
    SendGuildServiceTransferGuildLeader(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceApplyJoinGuild
boost::object_pool<AsyncGuildServiceApplyJoinGuildGrpcClient> GuildServiceApplyJoinGuildPool;
using AsyncGuildServiceApplyJoinGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::ApplyJoinGuildResponse&)>;
AsyncGuildServiceApplyJoinGuildHandlerFunctionType AsyncGuildServiceApplyJoinGuildHandler;

void AsyncCompleteGrpcGuildServiceApplyJoinGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceApplyJoinGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceApplyJoinGuildHandler) {
            AsyncGuildServiceApplyJoinGuildHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceApplyJoinGuildPool.destroy(call);
}

void SendGuildServiceApplyJoinGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ApplyJoinGuildRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceApplyJoinGuildPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncApplyJoinGuild(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceApplyJoinGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceApplyJoinGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ApplyJoinGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceApplyJoinGuildPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncApplyJoinGuild(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceApplyJoinGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceApplyJoinGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::ApplyJoinGuildRequest& derived = static_cast<const ::guildpb::ApplyJoinGuildRequest&>(message);
    SendGuildServiceApplyJoinGuild(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceCancelGuildApplication
boost::object_pool<AsyncGuildServiceCancelGuildApplicationGrpcClient> GuildServiceCancelGuildApplicationPool;
using AsyncGuildServiceCancelGuildApplicationHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::CancelGuildApplicationResponse&)>;
AsyncGuildServiceCancelGuildApplicationHandlerFunctionType AsyncGuildServiceCancelGuildApplicationHandler;

void AsyncCompleteGrpcGuildServiceCancelGuildApplication(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceCancelGuildApplicationGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceCancelGuildApplicationHandler) {
            AsyncGuildServiceCancelGuildApplicationHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceCancelGuildApplicationPool.destroy(call);
}

void SendGuildServiceCancelGuildApplication(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CancelGuildApplicationRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceCancelGuildApplicationPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncCancelGuildApplication(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceCancelGuildApplicationMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceCancelGuildApplication(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CancelGuildApplicationRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceCancelGuildApplicationPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncCancelGuildApplication(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceCancelGuildApplicationMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceCancelGuildApplication(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::CancelGuildApplicationRequest& derived = static_cast<const ::guildpb::CancelGuildApplicationRequest&>(message);
    SendGuildServiceCancelGuildApplication(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceListMyGuildApplications
boost::object_pool<AsyncGuildServiceListMyGuildApplicationsGrpcClient> GuildServiceListMyGuildApplicationsPool;
using AsyncGuildServiceListMyGuildApplicationsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::ListMyGuildApplicationsResponse&)>;
AsyncGuildServiceListMyGuildApplicationsHandlerFunctionType AsyncGuildServiceListMyGuildApplicationsHandler;

void AsyncCompleteGrpcGuildServiceListMyGuildApplications(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceListMyGuildApplicationsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceListMyGuildApplicationsHandler) {
            AsyncGuildServiceListMyGuildApplicationsHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceListMyGuildApplicationsPool.destroy(call);
}

void SendGuildServiceListMyGuildApplications(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListMyGuildApplicationsRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceListMyGuildApplicationsPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncListMyGuildApplications(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceListMyGuildApplicationsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceListMyGuildApplications(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListMyGuildApplicationsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceListMyGuildApplicationsPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncListMyGuildApplications(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceListMyGuildApplicationsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceListMyGuildApplications(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::ListMyGuildApplicationsRequest& derived = static_cast<const ::guildpb::ListMyGuildApplicationsRequest&>(message);
    SendGuildServiceListMyGuildApplications(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceListGuildApplications
boost::object_pool<AsyncGuildServiceListGuildApplicationsGrpcClient> GuildServiceListGuildApplicationsPool;
using AsyncGuildServiceListGuildApplicationsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::ListGuildApplicationsResponse&)>;
AsyncGuildServiceListGuildApplicationsHandlerFunctionType AsyncGuildServiceListGuildApplicationsHandler;

void AsyncCompleteGrpcGuildServiceListGuildApplications(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceListGuildApplicationsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceListGuildApplicationsHandler) {
            AsyncGuildServiceListGuildApplicationsHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceListGuildApplicationsPool.destroy(call);
}

void SendGuildServiceListGuildApplications(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListGuildApplicationsRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceListGuildApplicationsPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncListGuildApplications(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceListGuildApplicationsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceListGuildApplications(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListGuildApplicationsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceListGuildApplicationsPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncListGuildApplications(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceListGuildApplicationsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceListGuildApplications(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::ListGuildApplicationsRequest& derived = static_cast<const ::guildpb::ListGuildApplicationsRequest&>(message);
    SendGuildServiceListGuildApplications(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceReviewGuildApplication
boost::object_pool<AsyncGuildServiceReviewGuildApplicationGrpcClient> GuildServiceReviewGuildApplicationPool;
using AsyncGuildServiceReviewGuildApplicationHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::ReviewGuildApplicationResponse&)>;
AsyncGuildServiceReviewGuildApplicationHandlerFunctionType AsyncGuildServiceReviewGuildApplicationHandler;

void AsyncCompleteGrpcGuildServiceReviewGuildApplication(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceReviewGuildApplicationGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceReviewGuildApplicationHandler) {
            AsyncGuildServiceReviewGuildApplicationHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceReviewGuildApplicationPool.destroy(call);
}

void SendGuildServiceReviewGuildApplication(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ReviewGuildApplicationRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceReviewGuildApplicationPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncReviewGuildApplication(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceReviewGuildApplicationMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceReviewGuildApplication(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ReviewGuildApplicationRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceReviewGuildApplicationPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncReviewGuildApplication(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceReviewGuildApplicationMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceReviewGuildApplication(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::ReviewGuildApplicationRequest& derived = static_cast<const ::guildpb::ReviewGuildApplicationRequest&>(message);
    SendGuildServiceReviewGuildApplication(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceNotifyGuildChanged
boost::object_pool<AsyncGuildServiceNotifyGuildChangedGrpcClient> GuildServiceNotifyGuildChangedPool;
using AsyncGuildServiceNotifyGuildChangedHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncGuildServiceNotifyGuildChangedHandlerFunctionType AsyncGuildServiceNotifyGuildChangedHandler;

void AsyncCompleteGrpcGuildServiceNotifyGuildChanged(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceNotifyGuildChangedGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceNotifyGuildChangedHandler) {
            AsyncGuildServiceNotifyGuildChangedHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	GuildServiceNotifyGuildChangedPool.destroy(call);
}

void SendGuildServiceNotifyGuildChanged(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GuildChangedS2C& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(GuildServiceNotifyGuildChangedPool.construct());
    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyGuildChanged(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceNotifyGuildChangedMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceNotifyGuildChanged(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GuildChangedS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceNotifyGuildChangedPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyGuildChanged(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceNotifyGuildChangedMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceNotifyGuildChanged(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::GuildChangedS2C& derived = static_cast<const ::guildpb::GuildChangedS2C&>(message);
    SendGuildServiceNotifyGuildChanged(registry, nodeEntity, derived, metaKeys, metaValues);
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
        case GuildServiceSetGuildMemberRoleMessageId:
            AsyncCompleteGrpcGuildServiceSetGuildMemberRole(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceKickGuildMemberMessageId:
            AsyncCompleteGrpcGuildServiceKickGuildMember(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceTransferGuildLeaderMessageId:
            AsyncCompleteGrpcGuildServiceTransferGuildLeader(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceApplyJoinGuildMessageId:
            AsyncCompleteGrpcGuildServiceApplyJoinGuild(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceCancelGuildApplicationMessageId:
            AsyncCompleteGrpcGuildServiceCancelGuildApplication(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceListMyGuildApplicationsMessageId:
            AsyncCompleteGrpcGuildServiceListMyGuildApplications(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceListGuildApplicationsMessageId:
            AsyncCompleteGrpcGuildServiceListGuildApplications(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceReviewGuildApplicationMessageId:
            AsyncCompleteGrpcGuildServiceReviewGuildApplication(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceNotifyGuildChangedMessageId:
            AsyncCompleteGrpcGuildServiceNotifyGuildChanged(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
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
    AsyncGuildServiceLeaveGuildHandler = handler;
    AsyncGuildServiceDisbandGuildHandler = handler;
    AsyncGuildServiceSetAnnouncementHandler = handler;
    AsyncGuildServiceSetGuildMemberRoleHandler = handler;
    AsyncGuildServiceKickGuildMemberHandler = handler;
    AsyncGuildServiceTransferGuildLeaderHandler = handler;
    AsyncGuildServiceApplyJoinGuildHandler = handler;
    AsyncGuildServiceCancelGuildApplicationHandler = handler;
    AsyncGuildServiceListMyGuildApplicationsHandler = handler;
    AsyncGuildServiceListGuildApplicationsHandler = handler;
    AsyncGuildServiceReviewGuildApplicationHandler = handler;
    AsyncGuildServiceNotifyGuildChangedHandler = handler;
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
    if (!AsyncGuildServiceLeaveGuildHandler) {
        AsyncGuildServiceLeaveGuildHandler = handler;
    }
    if (!AsyncGuildServiceDisbandGuildHandler) {
        AsyncGuildServiceDisbandGuildHandler = handler;
    }
    if (!AsyncGuildServiceSetAnnouncementHandler) {
        AsyncGuildServiceSetAnnouncementHandler = handler;
    }
    if (!AsyncGuildServiceSetGuildMemberRoleHandler) {
        AsyncGuildServiceSetGuildMemberRoleHandler = handler;
    }
    if (!AsyncGuildServiceKickGuildMemberHandler) {
        AsyncGuildServiceKickGuildMemberHandler = handler;
    }
    if (!AsyncGuildServiceTransferGuildLeaderHandler) {
        AsyncGuildServiceTransferGuildLeaderHandler = handler;
    }
    if (!AsyncGuildServiceApplyJoinGuildHandler) {
        AsyncGuildServiceApplyJoinGuildHandler = handler;
    }
    if (!AsyncGuildServiceCancelGuildApplicationHandler) {
        AsyncGuildServiceCancelGuildApplicationHandler = handler;
    }
    if (!AsyncGuildServiceListMyGuildApplicationsHandler) {
        AsyncGuildServiceListMyGuildApplicationsHandler = handler;
    }
    if (!AsyncGuildServiceListGuildApplicationsHandler) {
        AsyncGuildServiceListGuildApplicationsHandler = handler;
    }
    if (!AsyncGuildServiceReviewGuildApplicationHandler) {
        AsyncGuildServiceReviewGuildApplicationHandler = handler;
    }
    if (!AsyncGuildServiceNotifyGuildChangedHandler) {
        AsyncGuildServiceNotifyGuildChangedHandler = handler;
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
