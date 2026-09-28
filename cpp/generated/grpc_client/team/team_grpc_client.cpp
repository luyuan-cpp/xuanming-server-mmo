#include "muduo/base/Logging.h"

#include "team_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <atomic>
#include <chrono>
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
// 本文件所有 unary 调用的 deadline(毫秒)。启动时 SetTeamCallDeadline 按目标节点类型写入
// (Node::Initialize → grpc_call_deadline::Apply);原子量:与应答处理器一样是进程级全局。
std::atomic<uint32_t> callDeadlineMs{kDefaultGrpcCallDeadlineMs};

std::chrono::system_clock::time_point NextCallDeadline() {
    return std::chrono::system_clock::now() +
        std::chrono::milliseconds(callDeadlineMs.load(std::memory_order_relaxed));
}
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
AsyncClientPlayerTeamCreateTeamFailedHandlerFunctionType AsyncClientPlayerTeamCreateTeamFailedHandler;

void AsyncCompleteGrpcClientPlayerTeamCreateTeam(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamCreateTeamGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamCreateTeamHandler) {
            AsyncClientPlayerTeamCreateTeamHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerTeam.CreateTeam reply dropped: AsyncClientPlayerTeamCreateTeamHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerTeamCreateTeamFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerTeam.CreateTeam", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerTeamCreateTeamFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerTeam.CreateTeam failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerTeamCreateTeamPool.destroy(call);
}

void SendClientPlayerTeamCreateTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::CreateTeamRequest& request) {

    SendClientPlayerTeamCreateTeam(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerTeamCreateTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::CreateTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamCreateTeamPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncCreateTeam(&call->context, call->request,
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
AsyncClientPlayerTeamGetMyTeamFailedHandlerFunctionType AsyncClientPlayerTeamGetMyTeamFailedHandler;

void AsyncCompleteGrpcClientPlayerTeamGetMyTeam(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamGetMyTeamGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamGetMyTeamHandler) {
            AsyncClientPlayerTeamGetMyTeamHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerTeam.GetMyTeam reply dropped: AsyncClientPlayerTeamGetMyTeamHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerTeamGetMyTeamFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerTeam.GetMyTeam", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerTeamGetMyTeamFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerTeam.GetMyTeam failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerTeamGetMyTeamPool.destroy(call);
}

void SendClientPlayerTeamGetMyTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::GetMyTeamRequest& request) {

    SendClientPlayerTeamGetMyTeam(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerTeamGetMyTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::GetMyTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamGetMyTeamPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncGetMyTeam(&call->context, call->request,
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
AsyncClientPlayerTeamApplyJoinTeamFailedHandlerFunctionType AsyncClientPlayerTeamApplyJoinTeamFailedHandler;

void AsyncCompleteGrpcClientPlayerTeamApplyJoinTeam(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamApplyJoinTeamGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamApplyJoinTeamHandler) {
            AsyncClientPlayerTeamApplyJoinTeamHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerTeam.ApplyJoinTeam reply dropped: AsyncClientPlayerTeamApplyJoinTeamHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerTeamApplyJoinTeamFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerTeam.ApplyJoinTeam", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerTeamApplyJoinTeamFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerTeam.ApplyJoinTeam failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerTeamApplyJoinTeamPool.destroy(call);
}

void SendClientPlayerTeamApplyJoinTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::ApplyJoinTeamRequest& request) {

    SendClientPlayerTeamApplyJoinTeam(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerTeamApplyJoinTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::ApplyJoinTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamApplyJoinTeamPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncApplyJoinTeam(&call->context, call->request,
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
AsyncClientPlayerTeamHandleApplicationFailedHandlerFunctionType AsyncClientPlayerTeamHandleApplicationFailedHandler;

void AsyncCompleteGrpcClientPlayerTeamHandleApplication(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamHandleApplicationGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamHandleApplicationHandler) {
            AsyncClientPlayerTeamHandleApplicationHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerTeam.HandleApplication reply dropped: AsyncClientPlayerTeamHandleApplicationHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerTeamHandleApplicationFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerTeam.HandleApplication", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerTeamHandleApplicationFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerTeam.HandleApplication failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerTeamHandleApplicationPool.destroy(call);
}

void SendClientPlayerTeamHandleApplication(entt::registry& registry, entt::entity nodeEntity, const ::teampb::HandleApplicationRequest& request) {

    SendClientPlayerTeamHandleApplication(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerTeamHandleApplication(entt::registry& registry, entt::entity nodeEntity, const ::teampb::HandleApplicationRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamHandleApplicationPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncHandleApplication(&call->context, call->request,
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
AsyncClientPlayerTeamInviteToTeamFailedHandlerFunctionType AsyncClientPlayerTeamInviteToTeamFailedHandler;

void AsyncCompleteGrpcClientPlayerTeamInviteToTeam(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamInviteToTeamGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamInviteToTeamHandler) {
            AsyncClientPlayerTeamInviteToTeamHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerTeam.InviteToTeam reply dropped: AsyncClientPlayerTeamInviteToTeamHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerTeamInviteToTeamFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerTeam.InviteToTeam", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerTeamInviteToTeamFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerTeam.InviteToTeam failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerTeamInviteToTeamPool.destroy(call);
}

void SendClientPlayerTeamInviteToTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::InviteToTeamRequest& request) {

    SendClientPlayerTeamInviteToTeam(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerTeamInviteToTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::InviteToTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamInviteToTeamPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncInviteToTeam(&call->context, call->request,
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
AsyncClientPlayerTeamRespondInviteFailedHandlerFunctionType AsyncClientPlayerTeamRespondInviteFailedHandler;

void AsyncCompleteGrpcClientPlayerTeamRespondInvite(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamRespondInviteGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamRespondInviteHandler) {
            AsyncClientPlayerTeamRespondInviteHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerTeam.RespondInvite reply dropped: AsyncClientPlayerTeamRespondInviteHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerTeamRespondInviteFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerTeam.RespondInvite", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerTeamRespondInviteFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerTeam.RespondInvite failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerTeamRespondInvitePool.destroy(call);
}

void SendClientPlayerTeamRespondInvite(entt::registry& registry, entt::entity nodeEntity, const ::teampb::RespondInviteRequest& request) {

    SendClientPlayerTeamRespondInvite(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerTeamRespondInvite(entt::registry& registry, entt::entity nodeEntity, const ::teampb::RespondInviteRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamRespondInvitePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncRespondInvite(&call->context, call->request,
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
AsyncClientPlayerTeamListMyInvitesFailedHandlerFunctionType AsyncClientPlayerTeamListMyInvitesFailedHandler;

void AsyncCompleteGrpcClientPlayerTeamListMyInvites(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamListMyInvitesGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamListMyInvitesHandler) {
            AsyncClientPlayerTeamListMyInvitesHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerTeam.ListMyInvites reply dropped: AsyncClientPlayerTeamListMyInvitesHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerTeamListMyInvitesFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerTeam.ListMyInvites", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerTeamListMyInvitesFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerTeam.ListMyInvites failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerTeamListMyInvitesPool.destroy(call);
}

void SendClientPlayerTeamListMyInvites(entt::registry& registry, entt::entity nodeEntity, const ::teampb::ListMyInvitesRequest& request) {

    SendClientPlayerTeamListMyInvites(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerTeamListMyInvites(entt::registry& registry, entt::entity nodeEntity, const ::teampb::ListMyInvitesRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamListMyInvitesPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncListMyInvites(&call->context, call->request,
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
AsyncClientPlayerTeamLeaveTeamFailedHandlerFunctionType AsyncClientPlayerTeamLeaveTeamFailedHandler;

void AsyncCompleteGrpcClientPlayerTeamLeaveTeam(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamLeaveTeamGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamLeaveTeamHandler) {
            AsyncClientPlayerTeamLeaveTeamHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerTeam.LeaveTeam reply dropped: AsyncClientPlayerTeamLeaveTeamHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerTeamLeaveTeamFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerTeam.LeaveTeam", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerTeamLeaveTeamFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerTeam.LeaveTeam failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerTeamLeaveTeamPool.destroy(call);
}

void SendClientPlayerTeamLeaveTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::LeaveTeamRequest& request) {

    SendClientPlayerTeamLeaveTeam(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerTeamLeaveTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::LeaveTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamLeaveTeamPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncLeaveTeam(&call->context, call->request,
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
AsyncClientPlayerTeamKickMemberFailedHandlerFunctionType AsyncClientPlayerTeamKickMemberFailedHandler;

void AsyncCompleteGrpcClientPlayerTeamKickMember(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamKickMemberGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamKickMemberHandler) {
            AsyncClientPlayerTeamKickMemberHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerTeam.KickMember reply dropped: AsyncClientPlayerTeamKickMemberHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerTeamKickMemberFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerTeam.KickMember", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerTeamKickMemberFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerTeam.KickMember failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerTeamKickMemberPool.destroy(call);
}

void SendClientPlayerTeamKickMember(entt::registry& registry, entt::entity nodeEntity, const ::teampb::KickMemberRequest& request) {

    SendClientPlayerTeamKickMember(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerTeamKickMember(entt::registry& registry, entt::entity nodeEntity, const ::teampb::KickMemberRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamKickMemberPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncKickMember(&call->context, call->request,
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
AsyncClientPlayerTeamTransferLeaderFailedHandlerFunctionType AsyncClientPlayerTeamTransferLeaderFailedHandler;

void AsyncCompleteGrpcClientPlayerTeamTransferLeader(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamTransferLeaderGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamTransferLeaderHandler) {
            AsyncClientPlayerTeamTransferLeaderHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerTeam.TransferLeader reply dropped: AsyncClientPlayerTeamTransferLeaderHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerTeamTransferLeaderFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerTeam.TransferLeader", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerTeamTransferLeaderFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerTeam.TransferLeader failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerTeamTransferLeaderPool.destroy(call);
}

void SendClientPlayerTeamTransferLeader(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TransferLeaderRequest& request) {

    SendClientPlayerTeamTransferLeader(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerTeamTransferLeader(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TransferLeaderRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamTransferLeaderPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncTransferLeader(&call->context, call->request,
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
AsyncClientPlayerTeamDisbandTeamFailedHandlerFunctionType AsyncClientPlayerTeamDisbandTeamFailedHandler;

void AsyncCompleteGrpcClientPlayerTeamDisbandTeam(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamDisbandTeamGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamDisbandTeamHandler) {
            AsyncClientPlayerTeamDisbandTeamHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerTeam.DisbandTeam reply dropped: AsyncClientPlayerTeamDisbandTeamHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerTeamDisbandTeamFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerTeam.DisbandTeam", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerTeamDisbandTeamFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerTeam.DisbandTeam failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerTeamDisbandTeamPool.destroy(call);
}

void SendClientPlayerTeamDisbandTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::DisbandTeamRequest& request) {

    SendClientPlayerTeamDisbandTeam(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerTeamDisbandTeam(entt::registry& registry, entt::entity nodeEntity, const ::teampb::DisbandTeamRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamDisbandTeamPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncDisbandTeam(&call->context, call->request,
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
AsyncClientPlayerTeamStartTeamMatchFailedHandlerFunctionType AsyncClientPlayerTeamStartTeamMatchFailedHandler;

void AsyncCompleteGrpcClientPlayerTeamStartTeamMatch(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamStartTeamMatchGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamStartTeamMatchHandler) {
            AsyncClientPlayerTeamStartTeamMatchHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerTeam.StartTeamMatch reply dropped: AsyncClientPlayerTeamStartTeamMatchHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerTeamStartTeamMatchFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerTeam.StartTeamMatch", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerTeamStartTeamMatchFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerTeam.StartTeamMatch failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerTeamStartTeamMatchPool.destroy(call);
}

void SendClientPlayerTeamStartTeamMatch(entt::registry& registry, entt::entity nodeEntity, const ::teampb::StartTeamMatchRequest& request) {

    SendClientPlayerTeamStartTeamMatch(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerTeamStartTeamMatch(entt::registry& registry, entt::entity nodeEntity, const ::teampb::StartTeamMatchRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamStartTeamMatchPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncStartTeamMatch(&call->context, call->request,
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
AsyncClientPlayerTeamNotifyTeamSnapshotFailedHandlerFunctionType AsyncClientPlayerTeamNotifyTeamSnapshotFailedHandler;

void AsyncCompleteGrpcClientPlayerTeamNotifyTeamSnapshot(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamNotifyTeamSnapshotGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamNotifyTeamSnapshotHandler) {
            AsyncClientPlayerTeamNotifyTeamSnapshotHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerTeam.NotifyTeamSnapshot reply dropped: AsyncClientPlayerTeamNotifyTeamSnapshotHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerTeamNotifyTeamSnapshotFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerTeam.NotifyTeamSnapshot", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerTeamNotifyTeamSnapshotFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerTeam.NotifyTeamSnapshot failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerTeamNotifyTeamSnapshotPool.destroy(call);
}

void SendClientPlayerTeamNotifyTeamSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamSnapshotS2C& request) {

    SendClientPlayerTeamNotifyTeamSnapshot(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerTeamNotifyTeamSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamSnapshotS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamNotifyTeamSnapshotPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyTeamSnapshot(&call->context, call->request,
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
AsyncClientPlayerTeamNotifyTeamInviteFailedHandlerFunctionType AsyncClientPlayerTeamNotifyTeamInviteFailedHandler;

void AsyncCompleteGrpcClientPlayerTeamNotifyTeamInvite(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamNotifyTeamInviteGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamNotifyTeamInviteHandler) {
            AsyncClientPlayerTeamNotifyTeamInviteHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerTeam.NotifyTeamInvite reply dropped: AsyncClientPlayerTeamNotifyTeamInviteHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerTeamNotifyTeamInviteFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerTeam.NotifyTeamInvite", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerTeamNotifyTeamInviteFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerTeam.NotifyTeamInvite failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerTeamNotifyTeamInvitePool.destroy(call);
}

void SendClientPlayerTeamNotifyTeamInvite(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamInviteS2C& request) {

    SendClientPlayerTeamNotifyTeamInvite(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerTeamNotifyTeamInvite(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamInviteS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamNotifyTeamInvitePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyTeamInvite(&call->context, call->request,
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
AsyncClientPlayerTeamNotifyTeamEventFailedHandlerFunctionType AsyncClientPlayerTeamNotifyTeamEventFailedHandler;

void AsyncCompleteGrpcClientPlayerTeamNotifyTeamEvent(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerTeamNotifyTeamEventGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerTeamNotifyTeamEventHandler) {
            AsyncClientPlayerTeamNotifyTeamEventHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerTeam.NotifyTeamEvent reply dropped: AsyncClientPlayerTeamNotifyTeamEventHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerTeamNotifyTeamEventFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerTeam.NotifyTeamEvent", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerTeamNotifyTeamEventFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerTeam.NotifyTeamEvent failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerTeamNotifyTeamEventPool.destroy(call);
}

void SendClientPlayerTeamNotifyTeamEvent(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamEventS2C& request) {

    SendClientPlayerTeamNotifyTeamEvent(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerTeamNotifyTeamEvent(entt::registry& registry, entt::entity nodeEntity, const ::teampb::TeamEventS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerTeamNotifyTeamEventPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerTeamStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyTeamEvent(&call->context, call->request,
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

void SetTeamFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    AsyncClientPlayerTeamCreateTeamFailedHandler = handler;
    AsyncClientPlayerTeamGetMyTeamFailedHandler = handler;
    AsyncClientPlayerTeamApplyJoinTeamFailedHandler = handler;
    AsyncClientPlayerTeamHandleApplicationFailedHandler = handler;
    AsyncClientPlayerTeamInviteToTeamFailedHandler = handler;
    AsyncClientPlayerTeamRespondInviteFailedHandler = handler;
    AsyncClientPlayerTeamListMyInvitesFailedHandler = handler;
    AsyncClientPlayerTeamLeaveTeamFailedHandler = handler;
    AsyncClientPlayerTeamKickMemberFailedHandler = handler;
    AsyncClientPlayerTeamTransferLeaderFailedHandler = handler;
    AsyncClientPlayerTeamDisbandTeamFailedHandler = handler;
    AsyncClientPlayerTeamStartTeamMatchFailedHandler = handler;
    AsyncClientPlayerTeamNotifyTeamSnapshotFailedHandler = handler;
    AsyncClientPlayerTeamNotifyTeamInviteFailedHandler = handler;
    AsyncClientPlayerTeamNotifyTeamEventFailedHandler = handler;
}

void SetTeamIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    if (!AsyncClientPlayerTeamCreateTeamFailedHandler) {
        AsyncClientPlayerTeamCreateTeamFailedHandler = handler;
    }
    if (!AsyncClientPlayerTeamGetMyTeamFailedHandler) {
        AsyncClientPlayerTeamGetMyTeamFailedHandler = handler;
    }
    if (!AsyncClientPlayerTeamApplyJoinTeamFailedHandler) {
        AsyncClientPlayerTeamApplyJoinTeamFailedHandler = handler;
    }
    if (!AsyncClientPlayerTeamHandleApplicationFailedHandler) {
        AsyncClientPlayerTeamHandleApplicationFailedHandler = handler;
    }
    if (!AsyncClientPlayerTeamInviteToTeamFailedHandler) {
        AsyncClientPlayerTeamInviteToTeamFailedHandler = handler;
    }
    if (!AsyncClientPlayerTeamRespondInviteFailedHandler) {
        AsyncClientPlayerTeamRespondInviteFailedHandler = handler;
    }
    if (!AsyncClientPlayerTeamListMyInvitesFailedHandler) {
        AsyncClientPlayerTeamListMyInvitesFailedHandler = handler;
    }
    if (!AsyncClientPlayerTeamLeaveTeamFailedHandler) {
        AsyncClientPlayerTeamLeaveTeamFailedHandler = handler;
    }
    if (!AsyncClientPlayerTeamKickMemberFailedHandler) {
        AsyncClientPlayerTeamKickMemberFailedHandler = handler;
    }
    if (!AsyncClientPlayerTeamTransferLeaderFailedHandler) {
        AsyncClientPlayerTeamTransferLeaderFailedHandler = handler;
    }
    if (!AsyncClientPlayerTeamDisbandTeamFailedHandler) {
        AsyncClientPlayerTeamDisbandTeamFailedHandler = handler;
    }
    if (!AsyncClientPlayerTeamStartTeamMatchFailedHandler) {
        AsyncClientPlayerTeamStartTeamMatchFailedHandler = handler;
    }
    if (!AsyncClientPlayerTeamNotifyTeamSnapshotFailedHandler) {
        AsyncClientPlayerTeamNotifyTeamSnapshotFailedHandler = handler;
    }
    if (!AsyncClientPlayerTeamNotifyTeamInviteFailedHandler) {
        AsyncClientPlayerTeamNotifyTeamInviteFailedHandler = handler;
    }
    if (!AsyncClientPlayerTeamNotifyTeamEventFailedHandler) {
        AsyncClientPlayerTeamNotifyTeamEventFailedHandler = handler;
    }
}

void SetTeamCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

void InitTeamGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<ClientPlayerTeamStubPtr>(nodeEntity, ClientPlayerTeam::NewStub(channel));

}

}// namespace teampb
