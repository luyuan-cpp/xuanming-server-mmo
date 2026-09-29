#include "muduo/base/Logging.h"

#include "guild_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <atomic>
#include <chrono>
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
// 本文件所有 unary 调用的 deadline(毫秒)。启动时 SetGuildCallDeadline 按目标节点类型写入
// (Node::Initialize → grpc_call_deadline::Apply);原子量:与应答处理器一样是进程级全局。
std::atomic<uint32_t> callDeadlineMs{kDefaultGrpcCallDeadlineMs};

std::chrono::system_clock::time_point NextCallDeadline() {
    return std::chrono::system_clock::now() +
        std::chrono::milliseconds(callDeadlineMs.load(std::memory_order_relaxed));
}
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
AsyncGuildServiceCreateGuildFailedHandlerFunctionType AsyncGuildServiceCreateGuildFailedHandler;

void AsyncCompleteGrpcGuildServiceCreateGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceCreateGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceCreateGuildHandler) {
            AsyncGuildServiceCreateGuildHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.CreateGuild reply dropped: AsyncGuildServiceCreateGuildHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceCreateGuildFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.CreateGuild", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceCreateGuildFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.CreateGuild failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceCreateGuildPool.destroy(call);
}

void SendGuildServiceCreateGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CreateGuildRequest& request) {

    SendGuildServiceCreateGuild(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceCreateGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CreateGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceCreateGuildPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncCreateGuild(&call->context, call->request,
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
AsyncGuildServiceGetGuildFailedHandlerFunctionType AsyncGuildServiceGetGuildFailedHandler;

void AsyncCompleteGrpcGuildServiceGetGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceGetGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceGetGuildHandler) {
            AsyncGuildServiceGetGuildHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.GetGuild reply dropped: AsyncGuildServiceGetGuildHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceGetGuildFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.GetGuild", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceGetGuildFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.GetGuild failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceGetGuildPool.destroy(call);
}

void SendGuildServiceGetGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRequest& request) {

    SendGuildServiceGetGuild(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceGetGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceGetGuildPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetGuild(&call->context, call->request,
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
AsyncGuildServiceGetPlayerGuildFailedHandlerFunctionType AsyncGuildServiceGetPlayerGuildFailedHandler;

void AsyncCompleteGrpcGuildServiceGetPlayerGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceGetPlayerGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceGetPlayerGuildHandler) {
            AsyncGuildServiceGetPlayerGuildHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.GetPlayerGuild reply dropped: AsyncGuildServiceGetPlayerGuildHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceGetPlayerGuildFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.GetPlayerGuild", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceGetPlayerGuildFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.GetPlayerGuild failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceGetPlayerGuildPool.destroy(call);
}

void SendGuildServiceGetPlayerGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetPlayerGuildRequest& request) {

    SendGuildServiceGetPlayerGuild(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceGetPlayerGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetPlayerGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceGetPlayerGuildPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetPlayerGuild(&call->context, call->request,
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
AsyncGuildServiceLeaveGuildFailedHandlerFunctionType AsyncGuildServiceLeaveGuildFailedHandler;

void AsyncCompleteGrpcGuildServiceLeaveGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceLeaveGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceLeaveGuildHandler) {
            AsyncGuildServiceLeaveGuildHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.LeaveGuild reply dropped: AsyncGuildServiceLeaveGuildHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceLeaveGuildFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.LeaveGuild", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceLeaveGuildFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.LeaveGuild failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceLeaveGuildPool.destroy(call);
}

void SendGuildServiceLeaveGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::LeaveGuildRequest& request) {

    SendGuildServiceLeaveGuild(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceLeaveGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::LeaveGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceLeaveGuildPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncLeaveGuild(&call->context, call->request,
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
AsyncGuildServiceDisbandGuildFailedHandlerFunctionType AsyncGuildServiceDisbandGuildFailedHandler;

void AsyncCompleteGrpcGuildServiceDisbandGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceDisbandGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceDisbandGuildHandler) {
            AsyncGuildServiceDisbandGuildHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.DisbandGuild reply dropped: AsyncGuildServiceDisbandGuildHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceDisbandGuildFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.DisbandGuild", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceDisbandGuildFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.DisbandGuild failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceDisbandGuildPool.destroy(call);
}

void SendGuildServiceDisbandGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::DisbandGuildRequest& request) {

    SendGuildServiceDisbandGuild(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceDisbandGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::DisbandGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceDisbandGuildPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncDisbandGuild(&call->context, call->request,
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
AsyncGuildServiceSetAnnouncementFailedHandlerFunctionType AsyncGuildServiceSetAnnouncementFailedHandler;

void AsyncCompleteGrpcGuildServiceSetAnnouncement(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceSetAnnouncementGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceSetAnnouncementHandler) {
            AsyncGuildServiceSetAnnouncementHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.SetAnnouncement reply dropped: AsyncGuildServiceSetAnnouncementHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceSetAnnouncementFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.SetAnnouncement", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceSetAnnouncementFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.SetAnnouncement failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceSetAnnouncementPool.destroy(call);
}

void SendGuildServiceSetAnnouncement(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetAnnouncementRequest& request) {

    SendGuildServiceSetAnnouncement(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceSetAnnouncement(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetAnnouncementRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceSetAnnouncementPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncSetAnnouncement(&call->context, call->request,
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
AsyncGuildServiceSetGuildMemberRoleFailedHandlerFunctionType AsyncGuildServiceSetGuildMemberRoleFailedHandler;

void AsyncCompleteGrpcGuildServiceSetGuildMemberRole(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceSetGuildMemberRoleGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceSetGuildMemberRoleHandler) {
            AsyncGuildServiceSetGuildMemberRoleHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.SetGuildMemberRole reply dropped: AsyncGuildServiceSetGuildMemberRoleHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceSetGuildMemberRoleFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.SetGuildMemberRole", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceSetGuildMemberRoleFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.SetGuildMemberRole failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceSetGuildMemberRolePool.destroy(call);
}

void SendGuildServiceSetGuildMemberRole(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetGuildMemberRoleRequest& request) {

    SendGuildServiceSetGuildMemberRole(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceSetGuildMemberRole(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::SetGuildMemberRoleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceSetGuildMemberRolePool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncSetGuildMemberRole(&call->context, call->request,
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
AsyncGuildServiceKickGuildMemberFailedHandlerFunctionType AsyncGuildServiceKickGuildMemberFailedHandler;

void AsyncCompleteGrpcGuildServiceKickGuildMember(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceKickGuildMemberGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceKickGuildMemberHandler) {
            AsyncGuildServiceKickGuildMemberHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.KickGuildMember reply dropped: AsyncGuildServiceKickGuildMemberHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceKickGuildMemberFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.KickGuildMember", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceKickGuildMemberFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.KickGuildMember failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceKickGuildMemberPool.destroy(call);
}

void SendGuildServiceKickGuildMember(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::KickGuildMemberRequest& request) {

    SendGuildServiceKickGuildMember(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceKickGuildMember(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::KickGuildMemberRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceKickGuildMemberPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncKickGuildMember(&call->context, call->request,
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
AsyncGuildServiceTransferGuildLeaderFailedHandlerFunctionType AsyncGuildServiceTransferGuildLeaderFailedHandler;

void AsyncCompleteGrpcGuildServiceTransferGuildLeader(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceTransferGuildLeaderGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceTransferGuildLeaderHandler) {
            AsyncGuildServiceTransferGuildLeaderHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.TransferGuildLeader reply dropped: AsyncGuildServiceTransferGuildLeaderHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceTransferGuildLeaderFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.TransferGuildLeader", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceTransferGuildLeaderFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.TransferGuildLeader failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceTransferGuildLeaderPool.destroy(call);
}

void SendGuildServiceTransferGuildLeader(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::TransferGuildLeaderRequest& request) {

    SendGuildServiceTransferGuildLeader(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceTransferGuildLeader(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::TransferGuildLeaderRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceTransferGuildLeaderPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncTransferGuildLeader(&call->context, call->request,
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
AsyncGuildServiceApplyJoinGuildFailedHandlerFunctionType AsyncGuildServiceApplyJoinGuildFailedHandler;

void AsyncCompleteGrpcGuildServiceApplyJoinGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceApplyJoinGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceApplyJoinGuildHandler) {
            AsyncGuildServiceApplyJoinGuildHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.ApplyJoinGuild reply dropped: AsyncGuildServiceApplyJoinGuildHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceApplyJoinGuildFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.ApplyJoinGuild", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceApplyJoinGuildFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.ApplyJoinGuild failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceApplyJoinGuildPool.destroy(call);
}

void SendGuildServiceApplyJoinGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ApplyJoinGuildRequest& request) {

    SendGuildServiceApplyJoinGuild(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceApplyJoinGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ApplyJoinGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceApplyJoinGuildPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncApplyJoinGuild(&call->context, call->request,
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
AsyncGuildServiceCancelGuildApplicationFailedHandlerFunctionType AsyncGuildServiceCancelGuildApplicationFailedHandler;

void AsyncCompleteGrpcGuildServiceCancelGuildApplication(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceCancelGuildApplicationGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceCancelGuildApplicationHandler) {
            AsyncGuildServiceCancelGuildApplicationHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.CancelGuildApplication reply dropped: AsyncGuildServiceCancelGuildApplicationHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceCancelGuildApplicationFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.CancelGuildApplication", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceCancelGuildApplicationFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.CancelGuildApplication failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceCancelGuildApplicationPool.destroy(call);
}

void SendGuildServiceCancelGuildApplication(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CancelGuildApplicationRequest& request) {

    SendGuildServiceCancelGuildApplication(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceCancelGuildApplication(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::CancelGuildApplicationRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceCancelGuildApplicationPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncCancelGuildApplication(&call->context, call->request,
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
AsyncGuildServiceListMyGuildApplicationsFailedHandlerFunctionType AsyncGuildServiceListMyGuildApplicationsFailedHandler;

void AsyncCompleteGrpcGuildServiceListMyGuildApplications(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceListMyGuildApplicationsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceListMyGuildApplicationsHandler) {
            AsyncGuildServiceListMyGuildApplicationsHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.ListMyGuildApplications reply dropped: AsyncGuildServiceListMyGuildApplicationsHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceListMyGuildApplicationsFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.ListMyGuildApplications", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceListMyGuildApplicationsFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.ListMyGuildApplications failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceListMyGuildApplicationsPool.destroy(call);
}

void SendGuildServiceListMyGuildApplications(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListMyGuildApplicationsRequest& request) {

    SendGuildServiceListMyGuildApplications(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceListMyGuildApplications(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListMyGuildApplicationsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceListMyGuildApplicationsPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncListMyGuildApplications(&call->context, call->request,
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
AsyncGuildServiceListGuildApplicationsFailedHandlerFunctionType AsyncGuildServiceListGuildApplicationsFailedHandler;

void AsyncCompleteGrpcGuildServiceListGuildApplications(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceListGuildApplicationsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceListGuildApplicationsHandler) {
            AsyncGuildServiceListGuildApplicationsHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.ListGuildApplications reply dropped: AsyncGuildServiceListGuildApplicationsHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceListGuildApplicationsFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.ListGuildApplications", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceListGuildApplicationsFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.ListGuildApplications failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceListGuildApplicationsPool.destroy(call);
}

void SendGuildServiceListGuildApplications(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListGuildApplicationsRequest& request) {

    SendGuildServiceListGuildApplications(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceListGuildApplications(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ListGuildApplicationsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceListGuildApplicationsPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncListGuildApplications(&call->context, call->request,
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
AsyncGuildServiceReviewGuildApplicationFailedHandlerFunctionType AsyncGuildServiceReviewGuildApplicationFailedHandler;

void AsyncCompleteGrpcGuildServiceReviewGuildApplication(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceReviewGuildApplicationGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceReviewGuildApplicationHandler) {
            AsyncGuildServiceReviewGuildApplicationHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.ReviewGuildApplication reply dropped: AsyncGuildServiceReviewGuildApplicationHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceReviewGuildApplicationFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.ReviewGuildApplication", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceReviewGuildApplicationFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.ReviewGuildApplication failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceReviewGuildApplicationPool.destroy(call);
}

void SendGuildServiceReviewGuildApplication(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ReviewGuildApplicationRequest& request) {

    SendGuildServiceReviewGuildApplication(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceReviewGuildApplication(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ReviewGuildApplicationRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceReviewGuildApplicationPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncReviewGuildApplication(&call->context, call->request,
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
AsyncGuildServiceNotifyGuildChangedFailedHandlerFunctionType AsyncGuildServiceNotifyGuildChangedFailedHandler;

void AsyncCompleteGrpcGuildServiceNotifyGuildChanged(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceNotifyGuildChangedGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceNotifyGuildChangedHandler) {
            AsyncGuildServiceNotifyGuildChangedHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.NotifyGuildChanged reply dropped: AsyncGuildServiceNotifyGuildChangedHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceNotifyGuildChangedFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.NotifyGuildChanged", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceNotifyGuildChangedFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.NotifyGuildChanged failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceNotifyGuildChangedPool.destroy(call);
}

void SendGuildServiceNotifyGuildChanged(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GuildChangedS2C& request) {

    SendGuildServiceNotifyGuildChanged(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceNotifyGuildChanged(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GuildChangedS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceNotifyGuildChangedPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyGuildChanged(&call->context, call->request,
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
AsyncGuildServiceUpdateGuildScoreFailedHandlerFunctionType AsyncGuildServiceUpdateGuildScoreFailedHandler;

void AsyncCompleteGrpcGuildServiceUpdateGuildScore(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceUpdateGuildScoreGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceUpdateGuildScoreHandler) {
            AsyncGuildServiceUpdateGuildScoreHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.UpdateGuildScore reply dropped: AsyncGuildServiceUpdateGuildScoreHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceUpdateGuildScoreFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.UpdateGuildScore", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceUpdateGuildScoreFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.UpdateGuildScore failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceUpdateGuildScorePool.destroy(call);
}

void SendGuildServiceUpdateGuildScore(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::UpdateGuildScoreRequest& request) {

    SendGuildServiceUpdateGuildScore(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceUpdateGuildScore(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::UpdateGuildScoreRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceUpdateGuildScorePool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncUpdateGuildScore(&call->context, call->request,
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
AsyncGuildServiceGetGuildRankFailedHandlerFunctionType AsyncGuildServiceGetGuildRankFailedHandler;

void AsyncCompleteGrpcGuildServiceGetGuildRank(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceGetGuildRankGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceGetGuildRankHandler) {
            AsyncGuildServiceGetGuildRankHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.GetGuildRank reply dropped: AsyncGuildServiceGetGuildRankHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceGetGuildRankFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.GetGuildRank", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceGetGuildRankFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.GetGuildRank failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceGetGuildRankPool.destroy(call);
}

void SendGuildServiceGetGuildRank(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankRequest& request) {

    SendGuildServiceGetGuildRank(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceGetGuildRank(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceGetGuildRankPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetGuildRank(&call->context, call->request,
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
AsyncGuildServiceGetGuildRankByGuildFailedHandlerFunctionType AsyncGuildServiceGetGuildRankByGuildFailedHandler;

void AsyncCompleteGrpcGuildServiceGetGuildRankByGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceGetGuildRankByGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceGetGuildRankByGuildHandler) {
            AsyncGuildServiceGetGuildRankByGuildHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.GetGuildRankByGuild reply dropped: AsyncGuildServiceGetGuildRankByGuildHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceGetGuildRankByGuildFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.GetGuildRankByGuild", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceGetGuildRankByGuildFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.GetGuildRankByGuild failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceGetGuildRankByGuildPool.destroy(call);
}

void SendGuildServiceGetGuildRankByGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankByGuildRequest& request) {

    SendGuildServiceGetGuildRankByGuild(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceGetGuildRankByGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildRankByGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceGetGuildRankByGuildPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetGuildRankByGuild(&call->context, call->request,
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
#pragma region GuildServiceGetGuildDonateOptions
boost::object_pool<AsyncGuildServiceGetGuildDonateOptionsGrpcClient> GuildServiceGetGuildDonateOptionsPool;
using AsyncGuildServiceGetGuildDonateOptionsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::GetGuildDonateOptionsResponse&)>;
AsyncGuildServiceGetGuildDonateOptionsHandlerFunctionType AsyncGuildServiceGetGuildDonateOptionsHandler;
AsyncGuildServiceGetGuildDonateOptionsFailedHandlerFunctionType AsyncGuildServiceGetGuildDonateOptionsFailedHandler;

void AsyncCompleteGrpcGuildServiceGetGuildDonateOptions(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceGetGuildDonateOptionsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceGetGuildDonateOptionsHandler) {
            AsyncGuildServiceGetGuildDonateOptionsHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.GetGuildDonateOptions reply dropped: AsyncGuildServiceGetGuildDonateOptionsHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceGetGuildDonateOptionsFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.GetGuildDonateOptions", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceGetGuildDonateOptionsFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.GetGuildDonateOptions failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceGetGuildDonateOptionsPool.destroy(call);
}

void SendGuildServiceGetGuildDonateOptions(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildDonateOptionsRequest& request) {

    SendGuildServiceGetGuildDonateOptions(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceGetGuildDonateOptions(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildDonateOptionsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceGetGuildDonateOptionsPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetGuildDonateOptions(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceGetGuildDonateOptionsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceGetGuildDonateOptions(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::GetGuildDonateOptionsRequest& derived = static_cast<const ::guildpb::GetGuildDonateOptionsRequest&>(message);
    SendGuildServiceGetGuildDonateOptions(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceDonateToGuild
boost::object_pool<AsyncGuildServiceDonateToGuildGrpcClient> GuildServiceDonateToGuildPool;
using AsyncGuildServiceDonateToGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::DonateToGuildResponse&)>;
AsyncGuildServiceDonateToGuildHandlerFunctionType AsyncGuildServiceDonateToGuildHandler;
AsyncGuildServiceDonateToGuildFailedHandlerFunctionType AsyncGuildServiceDonateToGuildFailedHandler;

void AsyncCompleteGrpcGuildServiceDonateToGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceDonateToGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceDonateToGuildHandler) {
            AsyncGuildServiceDonateToGuildHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.DonateToGuild reply dropped: AsyncGuildServiceDonateToGuildHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceDonateToGuildFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.DonateToGuild", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceDonateToGuildFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.DonateToGuild failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceDonateToGuildPool.destroy(call);
}

void SendGuildServiceDonateToGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::DonateToGuildRequest& request) {

    SendGuildServiceDonateToGuild(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceDonateToGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::DonateToGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceDonateToGuildPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncDonateToGuild(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceDonateToGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceDonateToGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::DonateToGuildRequest& derived = static_cast<const ::guildpb::DonateToGuildRequest&>(message);
    SendGuildServiceDonateToGuild(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceUpgradeGuild
boost::object_pool<AsyncGuildServiceUpgradeGuildGrpcClient> GuildServiceUpgradeGuildPool;
using AsyncGuildServiceUpgradeGuildHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::UpgradeGuildResponse&)>;
AsyncGuildServiceUpgradeGuildHandlerFunctionType AsyncGuildServiceUpgradeGuildHandler;
AsyncGuildServiceUpgradeGuildFailedHandlerFunctionType AsyncGuildServiceUpgradeGuildFailedHandler;

void AsyncCompleteGrpcGuildServiceUpgradeGuild(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceUpgradeGuildGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceUpgradeGuildHandler) {
            AsyncGuildServiceUpgradeGuildHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.UpgradeGuild reply dropped: AsyncGuildServiceUpgradeGuildHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceUpgradeGuildFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.UpgradeGuild", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceUpgradeGuildFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.UpgradeGuild failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceUpgradeGuildPool.destroy(call);
}

void SendGuildServiceUpgradeGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::UpgradeGuildRequest& request) {

    SendGuildServiceUpgradeGuild(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceUpgradeGuild(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::UpgradeGuildRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceUpgradeGuildPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncUpgradeGuild(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceUpgradeGuildMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceUpgradeGuild(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::UpgradeGuildRequest& derived = static_cast<const ::guildpb::UpgradeGuildRequest&>(message);
    SendGuildServiceUpgradeGuild(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceGetGuildShop
boost::object_pool<AsyncGuildServiceGetGuildShopGrpcClient> GuildServiceGetGuildShopPool;
using AsyncGuildServiceGetGuildShopHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::GetGuildShopResponse&)>;
AsyncGuildServiceGetGuildShopHandlerFunctionType AsyncGuildServiceGetGuildShopHandler;
AsyncGuildServiceGetGuildShopFailedHandlerFunctionType AsyncGuildServiceGetGuildShopFailedHandler;

void AsyncCompleteGrpcGuildServiceGetGuildShop(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceGetGuildShopGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceGetGuildShopHandler) {
            AsyncGuildServiceGetGuildShopHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.GetGuildShop reply dropped: AsyncGuildServiceGetGuildShopHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceGetGuildShopFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.GetGuildShop", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceGetGuildShopFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.GetGuildShop failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceGetGuildShopPool.destroy(call);
}

void SendGuildServiceGetGuildShop(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildShopRequest& request) {

    SendGuildServiceGetGuildShop(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceGetGuildShop(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildShopRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceGetGuildShopPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetGuildShop(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceGetGuildShopMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceGetGuildShop(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::GetGuildShopRequest& derived = static_cast<const ::guildpb::GetGuildShopRequest&>(message);
    SendGuildServiceGetGuildShop(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceBuyGuildShopGoods
boost::object_pool<AsyncGuildServiceBuyGuildShopGoodsGrpcClient> GuildServiceBuyGuildShopGoodsPool;
using AsyncGuildServiceBuyGuildShopGoodsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::BuyGuildShopGoodsResponse&)>;
AsyncGuildServiceBuyGuildShopGoodsHandlerFunctionType AsyncGuildServiceBuyGuildShopGoodsHandler;
AsyncGuildServiceBuyGuildShopGoodsFailedHandlerFunctionType AsyncGuildServiceBuyGuildShopGoodsFailedHandler;

void AsyncCompleteGrpcGuildServiceBuyGuildShopGoods(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceBuyGuildShopGoodsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceBuyGuildShopGoodsHandler) {
            AsyncGuildServiceBuyGuildShopGoodsHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.BuyGuildShopGoods reply dropped: AsyncGuildServiceBuyGuildShopGoodsHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceBuyGuildShopGoodsFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.BuyGuildShopGoods", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceBuyGuildShopGoodsFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.BuyGuildShopGoods failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceBuyGuildShopGoodsPool.destroy(call);
}

void SendGuildServiceBuyGuildShopGoods(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::BuyGuildShopGoodsRequest& request) {

    SendGuildServiceBuyGuildShopGoods(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceBuyGuildShopGoods(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::BuyGuildShopGoodsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceBuyGuildShopGoodsPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncBuyGuildShopGoods(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceBuyGuildShopGoodsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceBuyGuildShopGoods(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::BuyGuildShopGoodsRequest& derived = static_cast<const ::guildpb::BuyGuildShopGoodsRequest&>(message);
    SendGuildServiceBuyGuildShopGoods(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceGetGuildActivities
boost::object_pool<AsyncGuildServiceGetGuildActivitiesGrpcClient> GuildServiceGetGuildActivitiesPool;
using AsyncGuildServiceGetGuildActivitiesHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::GetGuildActivitiesResponse&)>;
AsyncGuildServiceGetGuildActivitiesHandlerFunctionType AsyncGuildServiceGetGuildActivitiesHandler;
AsyncGuildServiceGetGuildActivitiesFailedHandlerFunctionType AsyncGuildServiceGetGuildActivitiesFailedHandler;

void AsyncCompleteGrpcGuildServiceGetGuildActivities(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceGetGuildActivitiesGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceGetGuildActivitiesHandler) {
            AsyncGuildServiceGetGuildActivitiesHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.GetGuildActivities reply dropped: AsyncGuildServiceGetGuildActivitiesHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceGetGuildActivitiesFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.GetGuildActivities", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceGetGuildActivitiesFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.GetGuildActivities failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceGetGuildActivitiesPool.destroy(call);
}

void SendGuildServiceGetGuildActivities(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildActivitiesRequest& request) {

    SendGuildServiceGetGuildActivities(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceGetGuildActivities(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::GetGuildActivitiesRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceGetGuildActivitiesPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetGuildActivities(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceGetGuildActivitiesMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceGetGuildActivities(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::GetGuildActivitiesRequest& derived = static_cast<const ::guildpb::GetGuildActivitiesRequest&>(message);
    SendGuildServiceGetGuildActivities(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceLightGuildLantern
boost::object_pool<AsyncGuildServiceLightGuildLanternGrpcClient> GuildServiceLightGuildLanternPool;
using AsyncGuildServiceLightGuildLanternHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::LightGuildLanternResponse&)>;
AsyncGuildServiceLightGuildLanternHandlerFunctionType AsyncGuildServiceLightGuildLanternHandler;
AsyncGuildServiceLightGuildLanternFailedHandlerFunctionType AsyncGuildServiceLightGuildLanternFailedHandler;

void AsyncCompleteGrpcGuildServiceLightGuildLantern(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceLightGuildLanternGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceLightGuildLanternHandler) {
            AsyncGuildServiceLightGuildLanternHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.LightGuildLantern reply dropped: AsyncGuildServiceLightGuildLanternHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceLightGuildLanternFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.LightGuildLantern", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceLightGuildLanternFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.LightGuildLantern failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceLightGuildLanternPool.destroy(call);
}

void SendGuildServiceLightGuildLantern(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::LightGuildLanternRequest& request) {

    SendGuildServiceLightGuildLantern(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceLightGuildLantern(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::LightGuildLanternRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceLightGuildLanternPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncLightGuildLantern(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceLightGuildLanternMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceLightGuildLantern(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::LightGuildLanternRequest& derived = static_cast<const ::guildpb::LightGuildLanternRequest&>(message);
    SendGuildServiceLightGuildLantern(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceClaimGuildReunion
boost::object_pool<AsyncGuildServiceClaimGuildReunionGrpcClient> GuildServiceClaimGuildReunionPool;
using AsyncGuildServiceClaimGuildReunionHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::ClaimGuildReunionResponse&)>;
AsyncGuildServiceClaimGuildReunionHandlerFunctionType AsyncGuildServiceClaimGuildReunionHandler;
AsyncGuildServiceClaimGuildReunionFailedHandlerFunctionType AsyncGuildServiceClaimGuildReunionFailedHandler;

void AsyncCompleteGrpcGuildServiceClaimGuildReunion(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceClaimGuildReunionGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceClaimGuildReunionHandler) {
            AsyncGuildServiceClaimGuildReunionHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.ClaimGuildReunion reply dropped: AsyncGuildServiceClaimGuildReunionHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceClaimGuildReunionFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.ClaimGuildReunion", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceClaimGuildReunionFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.ClaimGuildReunion failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceClaimGuildReunionPool.destroy(call);
}

void SendGuildServiceClaimGuildReunion(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ClaimGuildReunionRequest& request) {

    SendGuildServiceClaimGuildReunion(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceClaimGuildReunion(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::ClaimGuildReunionRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceClaimGuildReunionPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncClaimGuildReunion(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceClaimGuildReunionMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceClaimGuildReunion(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::ClaimGuildReunionRequest& derived = static_cast<const ::guildpb::ClaimGuildReunionRequest&>(message);
    SendGuildServiceClaimGuildReunion(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceStartGuildTrial
boost::object_pool<AsyncGuildServiceStartGuildTrialGrpcClient> GuildServiceStartGuildTrialPool;
using AsyncGuildServiceStartGuildTrialHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::StartGuildTrialResponse&)>;
AsyncGuildServiceStartGuildTrialHandlerFunctionType AsyncGuildServiceStartGuildTrialHandler;
AsyncGuildServiceStartGuildTrialFailedHandlerFunctionType AsyncGuildServiceStartGuildTrialFailedHandler;

void AsyncCompleteGrpcGuildServiceStartGuildTrial(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceStartGuildTrialGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceStartGuildTrialHandler) {
            AsyncGuildServiceStartGuildTrialHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.StartGuildTrial reply dropped: AsyncGuildServiceStartGuildTrialHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceStartGuildTrialFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.StartGuildTrial", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceStartGuildTrialFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.StartGuildTrial failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceStartGuildTrialPool.destroy(call);
}

void SendGuildServiceStartGuildTrial(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::StartGuildTrialRequest& request) {

    SendGuildServiceStartGuildTrial(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceStartGuildTrial(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::StartGuildTrialRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceStartGuildTrialPool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncStartGuildTrial(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceStartGuildTrialMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceStartGuildTrial(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::StartGuildTrialRequest& derived = static_cast<const ::guildpb::StartGuildTrialRequest&>(message);
    SendGuildServiceStartGuildTrial(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region GuildServiceRespondGuildTrialInvite
boost::object_pool<AsyncGuildServiceRespondGuildTrialInviteGrpcClient> GuildServiceRespondGuildTrialInvitePool;
using AsyncGuildServiceRespondGuildTrialInviteHandlerFunctionType =
    std::function<void(const ClientContext&, const ::guildpb::RespondGuildTrialInviteResponse&)>;
AsyncGuildServiceRespondGuildTrialInviteHandlerFunctionType AsyncGuildServiceRespondGuildTrialInviteHandler;
AsyncGuildServiceRespondGuildTrialInviteFailedHandlerFunctionType AsyncGuildServiceRespondGuildTrialInviteFailedHandler;

void AsyncCompleteGrpcGuildServiceRespondGuildTrialInvite(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncGuildServiceRespondGuildTrialInviteGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncGuildServiceRespondGuildTrialInviteHandler) {
            AsyncGuildServiceRespondGuildTrialInviteHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC GuildService.RespondGuildTrialInvite reply dropped: AsyncGuildServiceRespondGuildTrialInviteHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncGuildServiceRespondGuildTrialInviteFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "GuildService.RespondGuildTrialInvite", call->context, call->status, call->sentMetadata};
        AsyncGuildServiceRespondGuildTrialInviteFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC GuildService.RespondGuildTrialInvite failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	GuildServiceRespondGuildTrialInvitePool.destroy(call);
}

void SendGuildServiceRespondGuildTrialInvite(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::RespondGuildTrialInviteRequest& request) {

    SendGuildServiceRespondGuildTrialInvite(registry, nodeEntity, request, {}, {});

}

void SendGuildServiceRespondGuildTrialInvite(entt::registry& registry, entt::entity nodeEntity, const ::guildpb::RespondGuildTrialInviteRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(GuildServiceRespondGuildTrialInvitePool.construct());
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
        .get<GuildServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRespondGuildTrialInvite(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(GuildServiceRespondGuildTrialInviteMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendGuildServiceRespondGuildTrialInvite(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::guildpb::RespondGuildTrialInviteRequest& derived = static_cast<const ::guildpb::RespondGuildTrialInviteRequest&>(message);
    SendGuildServiceRespondGuildTrialInvite(registry, nodeEntity, derived, metaKeys, metaValues);
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
        case GuildServiceGetGuildDonateOptionsMessageId:
            AsyncCompleteGrpcGuildServiceGetGuildDonateOptions(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceDonateToGuildMessageId:
            AsyncCompleteGrpcGuildServiceDonateToGuild(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceUpgradeGuildMessageId:
            AsyncCompleteGrpcGuildServiceUpgradeGuild(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceGetGuildShopMessageId:
            AsyncCompleteGrpcGuildServiceGetGuildShop(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceBuyGuildShopGoodsMessageId:
            AsyncCompleteGrpcGuildServiceBuyGuildShopGoods(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceGetGuildActivitiesMessageId:
            AsyncCompleteGrpcGuildServiceGetGuildActivities(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceLightGuildLanternMessageId:
            AsyncCompleteGrpcGuildServiceLightGuildLantern(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceClaimGuildReunionMessageId:
            AsyncCompleteGrpcGuildServiceClaimGuildReunion(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceStartGuildTrialMessageId:
            AsyncCompleteGrpcGuildServiceStartGuildTrial(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case GuildServiceRespondGuildTrialInviteMessageId:
            AsyncCompleteGrpcGuildServiceRespondGuildTrialInvite(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
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
    AsyncGuildServiceGetGuildDonateOptionsHandler = handler;
    AsyncGuildServiceDonateToGuildHandler = handler;
    AsyncGuildServiceUpgradeGuildHandler = handler;
    AsyncGuildServiceGetGuildShopHandler = handler;
    AsyncGuildServiceBuyGuildShopGoodsHandler = handler;
    AsyncGuildServiceGetGuildActivitiesHandler = handler;
    AsyncGuildServiceLightGuildLanternHandler = handler;
    AsyncGuildServiceClaimGuildReunionHandler = handler;
    AsyncGuildServiceStartGuildTrialHandler = handler;
    AsyncGuildServiceRespondGuildTrialInviteHandler = handler;
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
    if (!AsyncGuildServiceGetGuildDonateOptionsHandler) {
        AsyncGuildServiceGetGuildDonateOptionsHandler = handler;
    }
    if (!AsyncGuildServiceDonateToGuildHandler) {
        AsyncGuildServiceDonateToGuildHandler = handler;
    }
    if (!AsyncGuildServiceUpgradeGuildHandler) {
        AsyncGuildServiceUpgradeGuildHandler = handler;
    }
    if (!AsyncGuildServiceGetGuildShopHandler) {
        AsyncGuildServiceGetGuildShopHandler = handler;
    }
    if (!AsyncGuildServiceBuyGuildShopGoodsHandler) {
        AsyncGuildServiceBuyGuildShopGoodsHandler = handler;
    }
    if (!AsyncGuildServiceGetGuildActivitiesHandler) {
        AsyncGuildServiceGetGuildActivitiesHandler = handler;
    }
    if (!AsyncGuildServiceLightGuildLanternHandler) {
        AsyncGuildServiceLightGuildLanternHandler = handler;
    }
    if (!AsyncGuildServiceClaimGuildReunionHandler) {
        AsyncGuildServiceClaimGuildReunionHandler = handler;
    }
    if (!AsyncGuildServiceStartGuildTrialHandler) {
        AsyncGuildServiceStartGuildTrialHandler = handler;
    }
    if (!AsyncGuildServiceRespondGuildTrialInviteHandler) {
        AsyncGuildServiceRespondGuildTrialInviteHandler = handler;
    }
}

void SetGuildFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    AsyncGuildServiceCreateGuildFailedHandler = handler;
    AsyncGuildServiceGetGuildFailedHandler = handler;
    AsyncGuildServiceGetPlayerGuildFailedHandler = handler;
    AsyncGuildServiceLeaveGuildFailedHandler = handler;
    AsyncGuildServiceDisbandGuildFailedHandler = handler;
    AsyncGuildServiceSetAnnouncementFailedHandler = handler;
    AsyncGuildServiceSetGuildMemberRoleFailedHandler = handler;
    AsyncGuildServiceKickGuildMemberFailedHandler = handler;
    AsyncGuildServiceTransferGuildLeaderFailedHandler = handler;
    AsyncGuildServiceApplyJoinGuildFailedHandler = handler;
    AsyncGuildServiceCancelGuildApplicationFailedHandler = handler;
    AsyncGuildServiceListMyGuildApplicationsFailedHandler = handler;
    AsyncGuildServiceListGuildApplicationsFailedHandler = handler;
    AsyncGuildServiceReviewGuildApplicationFailedHandler = handler;
    AsyncGuildServiceNotifyGuildChangedFailedHandler = handler;
    AsyncGuildServiceUpdateGuildScoreFailedHandler = handler;
    AsyncGuildServiceGetGuildRankFailedHandler = handler;
    AsyncGuildServiceGetGuildRankByGuildFailedHandler = handler;
    AsyncGuildServiceGetGuildDonateOptionsFailedHandler = handler;
    AsyncGuildServiceDonateToGuildFailedHandler = handler;
    AsyncGuildServiceUpgradeGuildFailedHandler = handler;
    AsyncGuildServiceGetGuildShopFailedHandler = handler;
    AsyncGuildServiceBuyGuildShopGoodsFailedHandler = handler;
    AsyncGuildServiceGetGuildActivitiesFailedHandler = handler;
    AsyncGuildServiceLightGuildLanternFailedHandler = handler;
    AsyncGuildServiceClaimGuildReunionFailedHandler = handler;
    AsyncGuildServiceStartGuildTrialFailedHandler = handler;
    AsyncGuildServiceRespondGuildTrialInviteFailedHandler = handler;
}

void SetGuildIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    if (!AsyncGuildServiceCreateGuildFailedHandler) {
        AsyncGuildServiceCreateGuildFailedHandler = handler;
    }
    if (!AsyncGuildServiceGetGuildFailedHandler) {
        AsyncGuildServiceGetGuildFailedHandler = handler;
    }
    if (!AsyncGuildServiceGetPlayerGuildFailedHandler) {
        AsyncGuildServiceGetPlayerGuildFailedHandler = handler;
    }
    if (!AsyncGuildServiceLeaveGuildFailedHandler) {
        AsyncGuildServiceLeaveGuildFailedHandler = handler;
    }
    if (!AsyncGuildServiceDisbandGuildFailedHandler) {
        AsyncGuildServiceDisbandGuildFailedHandler = handler;
    }
    if (!AsyncGuildServiceSetAnnouncementFailedHandler) {
        AsyncGuildServiceSetAnnouncementFailedHandler = handler;
    }
    if (!AsyncGuildServiceSetGuildMemberRoleFailedHandler) {
        AsyncGuildServiceSetGuildMemberRoleFailedHandler = handler;
    }
    if (!AsyncGuildServiceKickGuildMemberFailedHandler) {
        AsyncGuildServiceKickGuildMemberFailedHandler = handler;
    }
    if (!AsyncGuildServiceTransferGuildLeaderFailedHandler) {
        AsyncGuildServiceTransferGuildLeaderFailedHandler = handler;
    }
    if (!AsyncGuildServiceApplyJoinGuildFailedHandler) {
        AsyncGuildServiceApplyJoinGuildFailedHandler = handler;
    }
    if (!AsyncGuildServiceCancelGuildApplicationFailedHandler) {
        AsyncGuildServiceCancelGuildApplicationFailedHandler = handler;
    }
    if (!AsyncGuildServiceListMyGuildApplicationsFailedHandler) {
        AsyncGuildServiceListMyGuildApplicationsFailedHandler = handler;
    }
    if (!AsyncGuildServiceListGuildApplicationsFailedHandler) {
        AsyncGuildServiceListGuildApplicationsFailedHandler = handler;
    }
    if (!AsyncGuildServiceReviewGuildApplicationFailedHandler) {
        AsyncGuildServiceReviewGuildApplicationFailedHandler = handler;
    }
    if (!AsyncGuildServiceNotifyGuildChangedFailedHandler) {
        AsyncGuildServiceNotifyGuildChangedFailedHandler = handler;
    }
    if (!AsyncGuildServiceUpdateGuildScoreFailedHandler) {
        AsyncGuildServiceUpdateGuildScoreFailedHandler = handler;
    }
    if (!AsyncGuildServiceGetGuildRankFailedHandler) {
        AsyncGuildServiceGetGuildRankFailedHandler = handler;
    }
    if (!AsyncGuildServiceGetGuildRankByGuildFailedHandler) {
        AsyncGuildServiceGetGuildRankByGuildFailedHandler = handler;
    }
    if (!AsyncGuildServiceGetGuildDonateOptionsFailedHandler) {
        AsyncGuildServiceGetGuildDonateOptionsFailedHandler = handler;
    }
    if (!AsyncGuildServiceDonateToGuildFailedHandler) {
        AsyncGuildServiceDonateToGuildFailedHandler = handler;
    }
    if (!AsyncGuildServiceUpgradeGuildFailedHandler) {
        AsyncGuildServiceUpgradeGuildFailedHandler = handler;
    }
    if (!AsyncGuildServiceGetGuildShopFailedHandler) {
        AsyncGuildServiceGetGuildShopFailedHandler = handler;
    }
    if (!AsyncGuildServiceBuyGuildShopGoodsFailedHandler) {
        AsyncGuildServiceBuyGuildShopGoodsFailedHandler = handler;
    }
    if (!AsyncGuildServiceGetGuildActivitiesFailedHandler) {
        AsyncGuildServiceGetGuildActivitiesFailedHandler = handler;
    }
    if (!AsyncGuildServiceLightGuildLanternFailedHandler) {
        AsyncGuildServiceLightGuildLanternFailedHandler = handler;
    }
    if (!AsyncGuildServiceClaimGuildReunionFailedHandler) {
        AsyncGuildServiceClaimGuildReunionFailedHandler = handler;
    }
    if (!AsyncGuildServiceStartGuildTrialFailedHandler) {
        AsyncGuildServiceStartGuildTrialFailedHandler = handler;
    }
    if (!AsyncGuildServiceRespondGuildTrialInviteFailedHandler) {
        AsyncGuildServiceRespondGuildTrialInviteFailedHandler = handler;
    }
}

void SetGuildCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

void InitGuildGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<GuildServiceStubPtr>(nodeEntity, GuildService::NewStub(channel));

}

}// namespace guildpb
