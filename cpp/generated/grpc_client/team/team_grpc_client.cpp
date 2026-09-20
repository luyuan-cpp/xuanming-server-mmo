#include "muduo/base/Logging.h"

#include "team_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
}

namespace teampb {
struct TeamCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region ClientPlayerTeamCreateTeam
boost::object_pool<AsyncClientPlayerTeamCreateTeamGrpcClient> ClientPlayerTeamCreateTeamPool;
using AsyncClientPlayerTeamCreateTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
AsyncClientPlayerTeamCreateTeamHandlerFunctionType AsyncClientPlayerTeamCreateTeamHandler;

void AsyncCompleteGrpcClientPlayerTeamCreateTeam(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamCreateTeamGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamCreateTeamHandler) {
            AsyncClientPlayerTeamCreateTeamHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerTeamCreateTeamPool.destroy(call);
}

void SendClientPlayerTeamCreateTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::CreateTeamRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerTeamCreateTeamPool.construct());
    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncCreateTeam(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamCreateTeamMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamCreateTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::CreateTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamCreateTeamPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncCreateTeam(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamCreateTeamMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamCreateTeam(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::teampb::CreateTeamRequest& derived = static_cast<const ::teampb::CreateTeamRequest&>(message);
    SendClientPlayerTeamCreateTeam(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerTeamGetMyTeam
boost::object_pool<AsyncClientPlayerTeamGetMyTeamGrpcClient> ClientPlayerTeamGetMyTeamPool;
using AsyncClientPlayerTeamGetMyTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
AsyncClientPlayerTeamGetMyTeamHandlerFunctionType AsyncClientPlayerTeamGetMyTeamHandler;

void AsyncCompleteGrpcClientPlayerTeamGetMyTeam(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamGetMyTeamGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamGetMyTeamHandler) {
            AsyncClientPlayerTeamGetMyTeamHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerTeamGetMyTeamPool.destroy(call);
}

void SendClientPlayerTeamGetMyTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::GetMyTeamRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerTeamGetMyTeamPool.construct());
    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncGetMyTeam(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamGetMyTeamMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamGetMyTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::GetMyTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamGetMyTeamPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncGetMyTeam(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamGetMyTeamMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamGetMyTeam(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::teampb::GetMyTeamRequest& derived = static_cast<const ::teampb::GetMyTeamRequest&>(message);
    SendClientPlayerTeamGetMyTeam(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerTeamApplyJoinTeam
boost::object_pool<AsyncClientPlayerTeamApplyJoinTeamGrpcClient> ClientPlayerTeamApplyJoinTeamPool;
using AsyncClientPlayerTeamApplyJoinTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
AsyncClientPlayerTeamApplyJoinTeamHandlerFunctionType AsyncClientPlayerTeamApplyJoinTeamHandler;

void AsyncCompleteGrpcClientPlayerTeamApplyJoinTeam(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamApplyJoinTeamGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamApplyJoinTeamHandler) {
            AsyncClientPlayerTeamApplyJoinTeamHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerTeamApplyJoinTeamPool.destroy(call);
}

void SendClientPlayerTeamApplyJoinTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::ApplyJoinTeamRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerTeamApplyJoinTeamPool.construct());
    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncApplyJoinTeam(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamApplyJoinTeamMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamApplyJoinTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::ApplyJoinTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamApplyJoinTeamPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncApplyJoinTeam(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamApplyJoinTeamMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamApplyJoinTeam(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::teampb::ApplyJoinTeamRequest& derived = static_cast<const ::teampb::ApplyJoinTeamRequest&>(message);
    SendClientPlayerTeamApplyJoinTeam(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerTeamHandleApplication
boost::object_pool<AsyncClientPlayerTeamHandleApplicationGrpcClient> ClientPlayerTeamHandleApplicationPool;
using AsyncClientPlayerTeamHandleApplicationHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
AsyncClientPlayerTeamHandleApplicationHandlerFunctionType AsyncClientPlayerTeamHandleApplicationHandler;

void AsyncCompleteGrpcClientPlayerTeamHandleApplication(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamHandleApplicationGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamHandleApplicationHandler) {
            AsyncClientPlayerTeamHandleApplicationHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerTeamHandleApplicationPool.destroy(call);
}

void SendClientPlayerTeamHandleApplication(entt::registry& registry, entt::entity nodeEntity, const ::teampb::HandleApplicationRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerTeamHandleApplicationPool.construct());
    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncHandleApplication(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamHandleApplicationMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamHandleApplication(entt::registry& registry, entt::entity nodeEntity, const ::teampb::HandleApplicationRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamHandleApplicationPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncHandleApplication(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamHandleApplicationMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamHandleApplication(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::teampb::HandleApplicationRequest& derived = static_cast<const ::teampb::HandleApplicationRequest&>(message);
    SendClientPlayerTeamHandleApplication(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerTeamInviteToTeam
boost::object_pool<AsyncClientPlayerTeamInviteToTeamGrpcClient> ClientPlayerTeamInviteToTeamPool;
using AsyncClientPlayerTeamInviteToTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
AsyncClientPlayerTeamInviteToTeamHandlerFunctionType AsyncClientPlayerTeamInviteToTeamHandler;

void AsyncCompleteGrpcClientPlayerTeamInviteToTeam(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamInviteToTeamGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamInviteToTeamHandler) {
            AsyncClientPlayerTeamInviteToTeamHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerTeamInviteToTeamPool.destroy(call);
}

void SendClientPlayerTeamInviteToTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::InviteToTeamRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerTeamInviteToTeamPool.construct());
    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncInviteToTeam(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamInviteToTeamMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamInviteToTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::InviteToTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamInviteToTeamPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncInviteToTeam(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamInviteToTeamMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamInviteToTeam(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::teampb::InviteToTeamRequest& derived = static_cast<const ::teampb::InviteToTeamRequest&>(message);
    SendClientPlayerTeamInviteToTeam(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerTeamRespondInvite
boost::object_pool<AsyncClientPlayerTeamRespondInviteGrpcClient> ClientPlayerTeamRespondInvitePool;
using AsyncClientPlayerTeamRespondInviteHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
AsyncClientPlayerTeamRespondInviteHandlerFunctionType AsyncClientPlayerTeamRespondInviteHandler;

void AsyncCompleteGrpcClientPlayerTeamRespondInvite(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamRespondInviteGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamRespondInviteHandler) {
            AsyncClientPlayerTeamRespondInviteHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerTeamRespondInvitePool.destroy(call);
}

void SendClientPlayerTeamRespondInvite(entt::registry& registry, entt::entity nodeEntity, const ::teampb::RespondInviteRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerTeamRespondInvitePool.construct());
    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncRespondInvite(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamRespondInviteMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamRespondInvite(entt::registry& registry, entt::entity nodeEntity, const ::teampb::RespondInviteRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamRespondInvitePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncRespondInvite(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamRespondInviteMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamRespondInvite(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::teampb::RespondInviteRequest& derived = static_cast<const ::teampb::RespondInviteRequest&>(message);
    SendClientPlayerTeamRespondInvite(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerTeamListMyInvites
boost::object_pool<AsyncClientPlayerTeamListMyInvitesGrpcClient> ClientPlayerTeamListMyInvitesPool;
using AsyncClientPlayerTeamListMyInvitesHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::ListMyInvitesResponse&)>;
AsyncClientPlayerTeamListMyInvitesHandlerFunctionType AsyncClientPlayerTeamListMyInvitesHandler;

void AsyncCompleteGrpcClientPlayerTeamListMyInvites(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamListMyInvitesGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamListMyInvitesHandler) {
            AsyncClientPlayerTeamListMyInvitesHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerTeamListMyInvitesPool.destroy(call);
}

void SendClientPlayerTeamListMyInvites(entt::registry& registry, entt::entity nodeEntity, const ::teampb::ListMyInvitesRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerTeamListMyInvitesPool.construct());
    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncListMyInvites(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamListMyInvitesMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamListMyInvites(entt::registry& registry, entt::entity nodeEntity, const ::teampb::ListMyInvitesRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamListMyInvitesPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncListMyInvites(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamListMyInvitesMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamListMyInvites(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::teampb::ListMyInvitesRequest& derived = static_cast<const ::teampb::ListMyInvitesRequest&>(message);
    SendClientPlayerTeamListMyInvites(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerTeamLeaveTeam
boost::object_pool<AsyncClientPlayerTeamLeaveTeamGrpcClient> ClientPlayerTeamLeaveTeamPool;
using AsyncClientPlayerTeamLeaveTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
AsyncClientPlayerTeamLeaveTeamHandlerFunctionType AsyncClientPlayerTeamLeaveTeamHandler;

void AsyncCompleteGrpcClientPlayerTeamLeaveTeam(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamLeaveTeamGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamLeaveTeamHandler) {
            AsyncClientPlayerTeamLeaveTeamHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerTeamLeaveTeamPool.destroy(call);
}

void SendClientPlayerTeamLeaveTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::LeaveTeamRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerTeamLeaveTeamPool.construct());
    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncLeaveTeam(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamLeaveTeamMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamLeaveTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::LeaveTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamLeaveTeamPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncLeaveTeam(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamLeaveTeamMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamLeaveTeam(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::teampb::LeaveTeamRequest& derived = static_cast<const ::teampb::LeaveTeamRequest&>(message);
    SendClientPlayerTeamLeaveTeam(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerTeamKickMember
boost::object_pool<AsyncClientPlayerTeamKickMemberGrpcClient> ClientPlayerTeamKickMemberPool;
using AsyncClientPlayerTeamKickMemberHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
AsyncClientPlayerTeamKickMemberHandlerFunctionType AsyncClientPlayerTeamKickMemberHandler;

void AsyncCompleteGrpcClientPlayerTeamKickMember(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamKickMemberGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamKickMemberHandler) {
            AsyncClientPlayerTeamKickMemberHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerTeamKickMemberPool.destroy(call);
}

void SendClientPlayerTeamKickMember(entt::registry& registry, entt::entity nodeEntity, const ::teampb::KickMemberRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerTeamKickMemberPool.construct());
    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncKickMember(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamKickMemberMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamKickMember(entt::registry& registry, entt::entity nodeEntity, const ::teampb::KickMemberRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamKickMemberPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncKickMember(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamKickMemberMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamKickMember(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::teampb::KickMemberRequest& derived = static_cast<const ::teampb::KickMemberRequest&>(message);
    SendClientPlayerTeamKickMember(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerTeamTransferLeader
boost::object_pool<AsyncClientPlayerTeamTransferLeaderGrpcClient> ClientPlayerTeamTransferLeaderPool;
using AsyncClientPlayerTeamTransferLeaderHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
AsyncClientPlayerTeamTransferLeaderHandlerFunctionType AsyncClientPlayerTeamTransferLeaderHandler;

void AsyncCompleteGrpcClientPlayerTeamTransferLeader(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamTransferLeaderGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamTransferLeaderHandler) {
            AsyncClientPlayerTeamTransferLeaderHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerTeamTransferLeaderPool.destroy(call);
}

void SendClientPlayerTeamTransferLeader(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TransferLeaderRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerTeamTransferLeaderPool.construct());
    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncTransferLeader(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamTransferLeaderMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamTransferLeader(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TransferLeaderRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamTransferLeaderPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncTransferLeader(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamTransferLeaderMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamTransferLeader(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::teampb::TransferLeaderRequest& derived = static_cast<const ::teampb::TransferLeaderRequest&>(message);
    SendClientPlayerTeamTransferLeader(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerTeamDisbandTeam
boost::object_pool<AsyncClientPlayerTeamDisbandTeamGrpcClient> ClientPlayerTeamDisbandTeamPool;
using AsyncClientPlayerTeamDisbandTeamHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
AsyncClientPlayerTeamDisbandTeamHandlerFunctionType AsyncClientPlayerTeamDisbandTeamHandler;

void AsyncCompleteGrpcClientPlayerTeamDisbandTeam(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamDisbandTeamGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamDisbandTeamHandler) {
            AsyncClientPlayerTeamDisbandTeamHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerTeamDisbandTeamPool.destroy(call);
}

void SendClientPlayerTeamDisbandTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::DisbandTeamRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerTeamDisbandTeamPool.construct());
    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncDisbandTeam(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamDisbandTeamMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamDisbandTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::DisbandTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamDisbandTeamPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncDisbandTeam(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamDisbandTeamMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamDisbandTeam(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::teampb::DisbandTeamRequest& derived = static_cast<const ::teampb::DisbandTeamRequest&>(message);
    SendClientPlayerTeamDisbandTeam(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerTeamStartTeamMatch
boost::object_pool<AsyncClientPlayerTeamStartTeamMatchGrpcClient> ClientPlayerTeamStartTeamMatchPool;
using AsyncClientPlayerTeamStartTeamMatchHandlerFunctionType =
    std::function<void(const ClientContext&, const ::teampb::TeamResponse&)>;
AsyncClientPlayerTeamStartTeamMatchHandlerFunctionType AsyncClientPlayerTeamStartTeamMatchHandler;

void AsyncCompleteGrpcClientPlayerTeamStartTeamMatch(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamStartTeamMatchGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamStartTeamMatchHandler) {
            AsyncClientPlayerTeamStartTeamMatchHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerTeamStartTeamMatchPool.destroy(call);
}

void SendClientPlayerTeamStartTeamMatch(entt::registry& registry, entt::entity nodeEntity, const ::teampb::StartTeamMatchRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerTeamStartTeamMatchPool.construct());
    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncStartTeamMatch(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamStartTeamMatchMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamStartTeamMatch(entt::registry& registry, entt::entity nodeEntity, const ::teampb::StartTeamMatchRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamStartTeamMatchPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncStartTeamMatch(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamStartTeamMatchMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamStartTeamMatch(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::teampb::StartTeamMatchRequest& derived = static_cast<const ::teampb::StartTeamMatchRequest&>(message);
    SendClientPlayerTeamStartTeamMatch(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerTeamNotifyTeamSnapshot
boost::object_pool<AsyncClientPlayerTeamNotifyTeamSnapshotGrpcClient> ClientPlayerTeamNotifyTeamSnapshotPool;
using AsyncClientPlayerTeamNotifyTeamSnapshotHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncClientPlayerTeamNotifyTeamSnapshotHandlerFunctionType AsyncClientPlayerTeamNotifyTeamSnapshotHandler;

void AsyncCompleteGrpcClientPlayerTeamNotifyTeamSnapshot(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamNotifyTeamSnapshotGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamNotifyTeamSnapshotHandler) {
            AsyncClientPlayerTeamNotifyTeamSnapshotHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerTeamNotifyTeamSnapshotPool.destroy(call);
}

void SendClientPlayerTeamNotifyTeamSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamSnapshotS2C& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerTeamNotifyTeamSnapshotPool.construct());
    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyTeamSnapshot(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamNotifyTeamSnapshotMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamNotifyTeamSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamSnapshotS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamNotifyTeamSnapshotPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyTeamSnapshot(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamNotifyTeamSnapshotMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamNotifyTeamSnapshot(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::teampb::TeamSnapshotS2C& derived = static_cast<const ::teampb::TeamSnapshotS2C&>(message);
    SendClientPlayerTeamNotifyTeamSnapshot(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerTeamNotifyTeamInvite
boost::object_pool<AsyncClientPlayerTeamNotifyTeamInviteGrpcClient> ClientPlayerTeamNotifyTeamInvitePool;
using AsyncClientPlayerTeamNotifyTeamInviteHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncClientPlayerTeamNotifyTeamInviteHandlerFunctionType AsyncClientPlayerTeamNotifyTeamInviteHandler;

void AsyncCompleteGrpcClientPlayerTeamNotifyTeamInvite(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamNotifyTeamInviteGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamNotifyTeamInviteHandler) {
            AsyncClientPlayerTeamNotifyTeamInviteHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerTeamNotifyTeamInvitePool.destroy(call);
}

void SendClientPlayerTeamNotifyTeamInvite(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamInviteS2C& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerTeamNotifyTeamInvitePool.construct());
    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyTeamInvite(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamNotifyTeamInviteMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamNotifyTeamInvite(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamInviteS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamNotifyTeamInvitePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyTeamInvite(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamNotifyTeamInviteMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamNotifyTeamInvite(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::teampb::TeamInviteS2C& derived = static_cast<const ::teampb::TeamInviteS2C&>(message);
    SendClientPlayerTeamNotifyTeamInvite(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerTeamNotifyTeamEvent
boost::object_pool<AsyncClientPlayerTeamNotifyTeamEventGrpcClient> ClientPlayerTeamNotifyTeamEventPool;
using AsyncClientPlayerTeamNotifyTeamEventHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncClientPlayerTeamNotifyTeamEventHandlerFunctionType AsyncClientPlayerTeamNotifyTeamEventHandler;

void AsyncCompleteGrpcClientPlayerTeamNotifyTeamEvent(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamNotifyTeamEventGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamNotifyTeamEventHandler) {
            AsyncClientPlayerTeamNotifyTeamEventHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	ClientPlayerTeamNotifyTeamEventPool.destroy(call);
}

void SendClientPlayerTeamNotifyTeamEvent(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamEventS2C& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(ClientPlayerTeamNotifyTeamEventPool.construct());
    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyTeamEvent(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamNotifyTeamEventMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamNotifyTeamEvent(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamEventS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamNotifyTeamEventPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyTeamEvent(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerTeamNotifyTeamEventMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerTeamNotifyTeamEvent(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::teampb::TeamEventS2C& derived = static_cast<const ::teampb::TeamEventS2C&>(message);
    SendClientPlayerTeamNotifyTeamEvent(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleTeamCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case ClientPlayerTeamCreateTeamMessageId:
            AsyncCompleteGrpcClientPlayerTeamCreateTeam(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerTeamGetMyTeamMessageId:
            AsyncCompleteGrpcClientPlayerTeamGetMyTeam(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerTeamApplyJoinTeamMessageId:
            AsyncCompleteGrpcClientPlayerTeamApplyJoinTeam(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerTeamHandleApplicationMessageId:
            AsyncCompleteGrpcClientPlayerTeamHandleApplication(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerTeamInviteToTeamMessageId:
            AsyncCompleteGrpcClientPlayerTeamInviteToTeam(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerTeamRespondInviteMessageId:
            AsyncCompleteGrpcClientPlayerTeamRespondInvite(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerTeamListMyInvitesMessageId:
            AsyncCompleteGrpcClientPlayerTeamListMyInvites(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerTeamLeaveTeamMessageId:
            AsyncCompleteGrpcClientPlayerTeamLeaveTeam(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerTeamKickMemberMessageId:
            AsyncCompleteGrpcClientPlayerTeamKickMember(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerTeamTransferLeaderMessageId:
            AsyncCompleteGrpcClientPlayerTeamTransferLeader(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerTeamDisbandTeamMessageId:
            AsyncCompleteGrpcClientPlayerTeamDisbandTeam(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerTeamStartTeamMatchMessageId:
            AsyncCompleteGrpcClientPlayerTeamStartTeamMatch(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerTeamNotifyTeamSnapshotMessageId:
            AsyncCompleteGrpcClientPlayerTeamNotifyTeamSnapshot(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerTeamNotifyTeamInviteMessageId:
            AsyncCompleteGrpcClientPlayerTeamNotifyTeamInvite(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerTeamNotifyTeamEventMessageId:
            AsyncCompleteGrpcClientPlayerTeamNotifyTeamEvent(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetTeamHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncClientPlayerTeamCreateTeamHandler = handler;
    AsyncClientPlayerTeamGetMyTeamHandler = handler;
    AsyncClientPlayerTeamApplyJoinTeamHandler = handler;
    AsyncClientPlayerTeamHandleApplicationHandler = handler;
    AsyncClientPlayerTeamInviteToTeamHandler = handler;
    AsyncClientPlayerTeamRespondInviteHandler = handler;
    AsyncClientPlayerTeamListMyInvitesHandler = handler;
    AsyncClientPlayerTeamLeaveTeamHandler = handler;
    AsyncClientPlayerTeamKickMemberHandler = handler;
    AsyncClientPlayerTeamTransferLeaderHandler = handler;
    AsyncClientPlayerTeamDisbandTeamHandler = handler;
    AsyncClientPlayerTeamStartTeamMatchHandler = handler;
    AsyncClientPlayerTeamNotifyTeamSnapshotHandler = handler;
    AsyncClientPlayerTeamNotifyTeamInviteHandler = handler;
    AsyncClientPlayerTeamNotifyTeamEventHandler = handler;
}

void SetTeamIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncClientPlayerTeamCreateTeamHandler) {
        AsyncClientPlayerTeamCreateTeamHandler = handler;
    }
    if (!AsyncClientPlayerTeamGetMyTeamHandler) {
        AsyncClientPlayerTeamGetMyTeamHandler = handler;
    }
    if (!AsyncClientPlayerTeamApplyJoinTeamHandler) {
        AsyncClientPlayerTeamApplyJoinTeamHandler = handler;
    }
    if (!AsyncClientPlayerTeamHandleApplicationHandler) {
        AsyncClientPlayerTeamHandleApplicationHandler = handler;
    }
    if (!AsyncClientPlayerTeamInviteToTeamHandler) {
        AsyncClientPlayerTeamInviteToTeamHandler = handler;
    }
    if (!AsyncClientPlayerTeamRespondInviteHandler) {
        AsyncClientPlayerTeamRespondInviteHandler = handler;
    }
    if (!AsyncClientPlayerTeamListMyInvitesHandler) {
        AsyncClientPlayerTeamListMyInvitesHandler = handler;
    }
    if (!AsyncClientPlayerTeamLeaveTeamHandler) {
        AsyncClientPlayerTeamLeaveTeamHandler = handler;
    }
    if (!AsyncClientPlayerTeamKickMemberHandler) {
        AsyncClientPlayerTeamKickMemberHandler = handler;
    }
    if (!AsyncClientPlayerTeamTransferLeaderHandler) {
        AsyncClientPlayerTeamTransferLeaderHandler = handler;
    }
    if (!AsyncClientPlayerTeamDisbandTeamHandler) {
        AsyncClientPlayerTeamDisbandTeamHandler = handler;
    }
    if (!AsyncClientPlayerTeamStartTeamMatchHandler) {
        AsyncClientPlayerTeamStartTeamMatchHandler = handler;
    }
    if (!AsyncClientPlayerTeamNotifyTeamSnapshotHandler) {
        AsyncClientPlayerTeamNotifyTeamSnapshotHandler = handler;
    }
    if (!AsyncClientPlayerTeamNotifyTeamInviteHandler) {
        AsyncClientPlayerTeamNotifyTeamInviteHandler = handler;
    }
    if (!AsyncClientPlayerTeamNotifyTeamEventHandler) {
        AsyncClientPlayerTeamNotifyTeamEventHandler = handler;
    }
}

void InitTeamGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<ClientPlayerTeamStubPtr>(nodeEntity, ClientPlayerTeam::NewStub(channel));

}

}// namespace teampb
