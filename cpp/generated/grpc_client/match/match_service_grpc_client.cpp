#include "muduo/base/Logging.h"

#include "match_service_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <atomic>
#include <chrono>
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
// 本文件所有 unary 调用的 deadline(毫秒)。启动时 SetMatchServiceCallDeadline 按目标节点类型写入
// (Node::Initialize → grpc_call_deadline::Apply);原子量:与应答处理器一样是进程级全局。
std::atomic<uint32_t> callDeadlineMs{kDefaultGrpcCallDeadlineMs};

std::chrono::system_clock::time_point NextCallDeadline() {
    return std::chrono::system_clock::now() +
        std::chrono::milliseconds(callDeadlineMs.load(std::memory_order_relaxed));
}
}

namespace match {
struct MatchServiceCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region MatchServiceJoinQueue
boost::object_pool<AsyncMatchServiceJoinQueueGrpcClient> MatchServiceJoinQueuePool;
using AsyncMatchServiceJoinQueueHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::JoinQueueResponse&)>;
AsyncMatchServiceJoinQueueHandlerFunctionType AsyncMatchServiceJoinQueueHandler;
AsyncMatchServiceJoinQueueFailedHandlerFunctionType AsyncMatchServiceJoinQueueFailedHandler;

void AsyncCompleteGrpcMatchServiceJoinQueue(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceJoinQueueGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceJoinQueueHandler) {
            AsyncMatchServiceJoinQueueHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC MatchService.JoinQueue reply dropped: AsyncMatchServiceJoinQueueHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncMatchServiceJoinQueueFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "MatchService.JoinQueue", call->context, call->status, call->sentMetadata};
        AsyncMatchServiceJoinQueueFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC MatchService.JoinQueue failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	MatchServiceJoinQueuePool.destroy(call);
}

void SendMatchServiceJoinQueue(entt::registry& registry, entt::entity nodeEntity, const ::match::JoinQueueRequest& request) {

    SendMatchServiceJoinQueue(registry, nodeEntity, request, {}, {});

}

void SendMatchServiceJoinQueue(entt::registry& registry, entt::entity nodeEntity, const ::match::JoinQueueRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceJoinQueuePool.construct());
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
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncJoinQueue(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceJoinQueueMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceJoinQueue(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::match::JoinQueueRequest& derived = static_cast<const ::match::JoinQueueRequest&>(message);
    SendMatchServiceJoinQueue(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region MatchServiceCancelQueue
boost::object_pool<AsyncMatchServiceCancelQueueGrpcClient> MatchServiceCancelQueuePool;
using AsyncMatchServiceCancelQueueHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncMatchServiceCancelQueueHandlerFunctionType AsyncMatchServiceCancelQueueHandler;
AsyncMatchServiceCancelQueueFailedHandlerFunctionType AsyncMatchServiceCancelQueueFailedHandler;

void AsyncCompleteGrpcMatchServiceCancelQueue(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceCancelQueueGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceCancelQueueHandler) {
            AsyncMatchServiceCancelQueueHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC MatchService.CancelQueue reply dropped: AsyncMatchServiceCancelQueueHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncMatchServiceCancelQueueFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "MatchService.CancelQueue", call->context, call->status, call->sentMetadata};
        AsyncMatchServiceCancelQueueFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC MatchService.CancelQueue failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	MatchServiceCancelQueuePool.destroy(call);
}

void SendMatchServiceCancelQueue(entt::registry& registry, entt::entity nodeEntity, const ::match::CancelQueueRequest& request) {

    SendMatchServiceCancelQueue(registry, nodeEntity, request, {}, {});

}

void SendMatchServiceCancelQueue(entt::registry& registry, entt::entity nodeEntity, const ::match::CancelQueueRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceCancelQueuePool.construct());
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
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncCancelQueue(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceCancelQueueMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceCancelQueue(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::match::CancelQueueRequest& derived = static_cast<const ::match::CancelQueueRequest&>(message);
    SendMatchServiceCancelQueue(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region MatchServiceGetQueueStatus
boost::object_pool<AsyncMatchServiceGetQueueStatusGrpcClient> MatchServiceGetQueueStatusPool;
using AsyncMatchServiceGetQueueStatusHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::GetQueueStatusResponse&)>;
AsyncMatchServiceGetQueueStatusHandlerFunctionType AsyncMatchServiceGetQueueStatusHandler;
AsyncMatchServiceGetQueueStatusFailedHandlerFunctionType AsyncMatchServiceGetQueueStatusFailedHandler;

void AsyncCompleteGrpcMatchServiceGetQueueStatus(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceGetQueueStatusGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceGetQueueStatusHandler) {
            AsyncMatchServiceGetQueueStatusHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC MatchService.GetQueueStatus reply dropped: AsyncMatchServiceGetQueueStatusHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncMatchServiceGetQueueStatusFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "MatchService.GetQueueStatus", call->context, call->status, call->sentMetadata};
        AsyncMatchServiceGetQueueStatusFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC MatchService.GetQueueStatus failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	MatchServiceGetQueueStatusPool.destroy(call);
}

void SendMatchServiceGetQueueStatus(entt::registry& registry, entt::entity nodeEntity, const ::match::GetQueueStatusRequest& request) {

    SendMatchServiceGetQueueStatus(registry, nodeEntity, request, {}, {});

}

void SendMatchServiceGetQueueStatus(entt::registry& registry, entt::entity nodeEntity, const ::match::GetQueueStatusRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceGetQueueStatusPool.construct());
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
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetQueueStatus(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceGetQueueStatusMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceGetQueueStatus(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::match::GetQueueStatusRequest& derived = static_cast<const ::match::GetQueueStatusRequest&>(message);
    SendMatchServiceGetQueueStatus(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region MatchServiceChallengePlayer
boost::object_pool<AsyncMatchServiceChallengePlayerGrpcClient> MatchServiceChallengePlayerPool;
using AsyncMatchServiceChallengePlayerHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::ChallengePlayerResponse&)>;
AsyncMatchServiceChallengePlayerHandlerFunctionType AsyncMatchServiceChallengePlayerHandler;
AsyncMatchServiceChallengePlayerFailedHandlerFunctionType AsyncMatchServiceChallengePlayerFailedHandler;

void AsyncCompleteGrpcMatchServiceChallengePlayer(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceChallengePlayerGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceChallengePlayerHandler) {
            AsyncMatchServiceChallengePlayerHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC MatchService.ChallengePlayer reply dropped: AsyncMatchServiceChallengePlayerHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncMatchServiceChallengePlayerFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "MatchService.ChallengePlayer", call->context, call->status, call->sentMetadata};
        AsyncMatchServiceChallengePlayerFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC MatchService.ChallengePlayer failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	MatchServiceChallengePlayerPool.destroy(call);
}

void SendMatchServiceChallengePlayer(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengePlayerRequest& request) {

    SendMatchServiceChallengePlayer(registry, nodeEntity, request, {}, {});

}

void SendMatchServiceChallengePlayer(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengePlayerRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceChallengePlayerPool.construct());
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
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncChallengePlayer(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceChallengePlayerMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceChallengePlayer(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::match::ChallengePlayerRequest& derived = static_cast<const ::match::ChallengePlayerRequest&>(message);
    SendMatchServiceChallengePlayer(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region MatchServiceRespondChallenge
boost::object_pool<AsyncMatchServiceRespondChallengeGrpcClient> MatchServiceRespondChallengePool;
using AsyncMatchServiceRespondChallengeHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::RespondChallengeResponse&)>;
AsyncMatchServiceRespondChallengeHandlerFunctionType AsyncMatchServiceRespondChallengeHandler;
AsyncMatchServiceRespondChallengeFailedHandlerFunctionType AsyncMatchServiceRespondChallengeFailedHandler;

void AsyncCompleteGrpcMatchServiceRespondChallenge(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceRespondChallengeGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceRespondChallengeHandler) {
            AsyncMatchServiceRespondChallengeHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC MatchService.RespondChallenge reply dropped: AsyncMatchServiceRespondChallengeHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncMatchServiceRespondChallengeFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "MatchService.RespondChallenge", call->context, call->status, call->sentMetadata};
        AsyncMatchServiceRespondChallengeFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC MatchService.RespondChallenge failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	MatchServiceRespondChallengePool.destroy(call);
}

void SendMatchServiceRespondChallenge(entt::registry& registry, entt::entity nodeEntity, const ::match::RespondChallengeRequest& request) {

    SendMatchServiceRespondChallenge(registry, nodeEntity, request, {}, {});

}

void SendMatchServiceRespondChallenge(entt::registry& registry, entt::entity nodeEntity, const ::match::RespondChallengeRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceRespondChallengePool.construct());
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
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRespondChallenge(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceRespondChallengeMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceRespondChallenge(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::match::RespondChallengeRequest& derived = static_cast<const ::match::RespondChallengeRequest&>(message);
    SendMatchServiceRespondChallenge(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region MatchServiceNotifyChallengeInvite
boost::object_pool<AsyncMatchServiceNotifyChallengeInviteGrpcClient> MatchServiceNotifyChallengeInvitePool;
using AsyncMatchServiceNotifyChallengeInviteHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncMatchServiceNotifyChallengeInviteHandlerFunctionType AsyncMatchServiceNotifyChallengeInviteHandler;
AsyncMatchServiceNotifyChallengeInviteFailedHandlerFunctionType AsyncMatchServiceNotifyChallengeInviteFailedHandler;

void AsyncCompleteGrpcMatchServiceNotifyChallengeInvite(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceNotifyChallengeInviteGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceNotifyChallengeInviteHandler) {
            AsyncMatchServiceNotifyChallengeInviteHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC MatchService.NotifyChallengeInvite reply dropped: AsyncMatchServiceNotifyChallengeInviteHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncMatchServiceNotifyChallengeInviteFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "MatchService.NotifyChallengeInvite", call->context, call->status, call->sentMetadata};
        AsyncMatchServiceNotifyChallengeInviteFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC MatchService.NotifyChallengeInvite failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	MatchServiceNotifyChallengeInvitePool.destroy(call);
}

void SendMatchServiceNotifyChallengeInvite(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeInviteS2C& request) {

    SendMatchServiceNotifyChallengeInvite(registry, nodeEntity, request, {}, {});

}

void SendMatchServiceNotifyChallengeInvite(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeInviteS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceNotifyChallengeInvitePool.construct());
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
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyChallengeInvite(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceNotifyChallengeInviteMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceNotifyChallengeInvite(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::match::ChallengeInviteS2C& derived = static_cast<const ::match::ChallengeInviteS2C&>(message);
    SendMatchServiceNotifyChallengeInvite(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region MatchServiceNotifyChallengeResult
boost::object_pool<AsyncMatchServiceNotifyChallengeResultGrpcClient> MatchServiceNotifyChallengeResultPool;
using AsyncMatchServiceNotifyChallengeResultHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncMatchServiceNotifyChallengeResultHandlerFunctionType AsyncMatchServiceNotifyChallengeResultHandler;
AsyncMatchServiceNotifyChallengeResultFailedHandlerFunctionType AsyncMatchServiceNotifyChallengeResultFailedHandler;

void AsyncCompleteGrpcMatchServiceNotifyChallengeResult(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceNotifyChallengeResultGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceNotifyChallengeResultHandler) {
            AsyncMatchServiceNotifyChallengeResultHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC MatchService.NotifyChallengeResult reply dropped: AsyncMatchServiceNotifyChallengeResultHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncMatchServiceNotifyChallengeResultFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "MatchService.NotifyChallengeResult", call->context, call->status, call->sentMetadata};
        AsyncMatchServiceNotifyChallengeResultFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC MatchService.NotifyChallengeResult failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	MatchServiceNotifyChallengeResultPool.destroy(call);
}

void SendMatchServiceNotifyChallengeResult(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeResultS2C& request) {

    SendMatchServiceNotifyChallengeResult(registry, nodeEntity, request, {}, {});

}

void SendMatchServiceNotifyChallengeResult(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeResultS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceNotifyChallengeResultPool.construct());
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
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyChallengeResult(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceNotifyChallengeResultMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceNotifyChallengeResult(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::match::ChallengeResultS2C& derived = static_cast<const ::match::ChallengeResultS2C&>(message);
    SendMatchServiceNotifyChallengeResult(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region MatchServiceWatchBattle
boost::object_pool<AsyncMatchServiceWatchBattleGrpcClient> MatchServiceWatchBattlePool;
using AsyncMatchServiceWatchBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::WatchBattleResponse&)>;
AsyncMatchServiceWatchBattleHandlerFunctionType AsyncMatchServiceWatchBattleHandler;
AsyncMatchServiceWatchBattleFailedHandlerFunctionType AsyncMatchServiceWatchBattleFailedHandler;

void AsyncCompleteGrpcMatchServiceWatchBattle(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceWatchBattleGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceWatchBattleHandler) {
            AsyncMatchServiceWatchBattleHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC MatchService.WatchBattle reply dropped: AsyncMatchServiceWatchBattleHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncMatchServiceWatchBattleFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "MatchService.WatchBattle", call->context, call->status, call->sentMetadata};
        AsyncMatchServiceWatchBattleFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC MatchService.WatchBattle failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	MatchServiceWatchBattlePool.destroy(call);
}

void SendMatchServiceWatchBattle(entt::registry& registry, entt::entity nodeEntity, const ::match::WatchBattleRequest& request) {

    SendMatchServiceWatchBattle(registry, nodeEntity, request, {}, {});

}

void SendMatchServiceWatchBattle(entt::registry& registry, entt::entity nodeEntity, const ::match::WatchBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceWatchBattlePool.construct());
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
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncWatchBattle(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceWatchBattleMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceWatchBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::match::WatchBattleRequest& derived = static_cast<const ::match::WatchBattleRequest&>(message);
    SendMatchServiceWatchBattle(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region MatchServiceRequestBattleTicket
boost::object_pool<AsyncMatchServiceRequestBattleTicketGrpcClient> MatchServiceRequestBattleTicketPool;
using AsyncMatchServiceRequestBattleTicketHandlerFunctionType =
    std::function<void(const ClientContext&, const ::RequestBattleTicketResponse&)>;
AsyncMatchServiceRequestBattleTicketHandlerFunctionType AsyncMatchServiceRequestBattleTicketHandler;
AsyncMatchServiceRequestBattleTicketFailedHandlerFunctionType AsyncMatchServiceRequestBattleTicketFailedHandler;

void AsyncCompleteGrpcMatchServiceRequestBattleTicket(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceRequestBattleTicketGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceRequestBattleTicketHandler) {
            AsyncMatchServiceRequestBattleTicketHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC MatchService.RequestBattleTicket reply dropped: AsyncMatchServiceRequestBattleTicketHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncMatchServiceRequestBattleTicketFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "MatchService.RequestBattleTicket", call->context, call->status, call->sentMetadata};
        AsyncMatchServiceRequestBattleTicketFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC MatchService.RequestBattleTicket failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	MatchServiceRequestBattleTicketPool.destroy(call);
}

void SendMatchServiceRequestBattleTicket(entt::registry& registry, entt::entity nodeEntity, const ::RequestBattleTicketRequest& request) {

    SendMatchServiceRequestBattleTicket(registry, nodeEntity, request, {}, {});

}

void SendMatchServiceRequestBattleTicket(entt::registry& registry, entt::entity nodeEntity, const ::RequestBattleTicketRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceRequestBattleTicketPool.construct());
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
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRequestBattleTicket(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceRequestBattleTicketMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceRequestBattleTicket(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::RequestBattleTicketRequest& derived = static_cast<const ::RequestBattleTicketRequest&>(message);
    SendMatchServiceRequestBattleTicket(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region MatchServiceListWatchableBattles
boost::object_pool<AsyncMatchServiceListWatchableBattlesGrpcClient> MatchServiceListWatchableBattlesPool;
using AsyncMatchServiceListWatchableBattlesHandlerFunctionType =
    std::function<void(const ClientContext&, const ::match::ListWatchableBattlesResponse&)>;
AsyncMatchServiceListWatchableBattlesHandlerFunctionType AsyncMatchServiceListWatchableBattlesHandler;
AsyncMatchServiceListWatchableBattlesFailedHandlerFunctionType AsyncMatchServiceListWatchableBattlesFailedHandler;

void AsyncCompleteGrpcMatchServiceListWatchableBattles(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceListWatchableBattlesGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceListWatchableBattlesHandler) {
            AsyncMatchServiceListWatchableBattlesHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC MatchService.ListWatchableBattles reply dropped: AsyncMatchServiceListWatchableBattlesHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncMatchServiceListWatchableBattlesFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "MatchService.ListWatchableBattles", call->context, call->status, call->sentMetadata};
        AsyncMatchServiceListWatchableBattlesFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC MatchService.ListWatchableBattles failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	MatchServiceListWatchableBattlesPool.destroy(call);
}

void SendMatchServiceListWatchableBattles(entt::registry& registry, entt::entity nodeEntity, const ::match::ListWatchableBattlesRequest& request) {

    SendMatchServiceListWatchableBattles(registry, nodeEntity, request, {}, {});

}

void SendMatchServiceListWatchableBattles(entt::registry& registry, entt::entity nodeEntity, const ::match::ListWatchableBattlesRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceListWatchableBattlesPool.construct());
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
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncListWatchableBattles(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceListWatchableBattlesMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceListWatchableBattles(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::match::ListWatchableBattlesRequest& derived = static_cast<const ::match::ListWatchableBattlesRequest&>(message);
    SendMatchServiceListWatchableBattles(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleMatchServiceCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case MatchServiceJoinQueueMessageId:
            AsyncCompleteGrpcMatchServiceJoinQueue(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case MatchServiceCancelQueueMessageId:
            AsyncCompleteGrpcMatchServiceCancelQueue(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case MatchServiceGetQueueStatusMessageId:
            AsyncCompleteGrpcMatchServiceGetQueueStatus(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case MatchServiceChallengePlayerMessageId:
            AsyncCompleteGrpcMatchServiceChallengePlayer(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case MatchServiceRespondChallengeMessageId:
            AsyncCompleteGrpcMatchServiceRespondChallenge(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case MatchServiceNotifyChallengeInviteMessageId:
            AsyncCompleteGrpcMatchServiceNotifyChallengeInvite(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case MatchServiceNotifyChallengeResultMessageId:
            AsyncCompleteGrpcMatchServiceNotifyChallengeResult(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case MatchServiceWatchBattleMessageId:
            AsyncCompleteGrpcMatchServiceWatchBattle(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case MatchServiceRequestBattleTicketMessageId:
            AsyncCompleteGrpcMatchServiceRequestBattleTicket(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case MatchServiceListWatchableBattlesMessageId:
            AsyncCompleteGrpcMatchServiceListWatchableBattles(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetMatchServiceHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncMatchServiceJoinQueueHandler = handler;
    AsyncMatchServiceCancelQueueHandler = handler;
    AsyncMatchServiceGetQueueStatusHandler = handler;
    AsyncMatchServiceChallengePlayerHandler = handler;
    AsyncMatchServiceRespondChallengeHandler = handler;
    AsyncMatchServiceNotifyChallengeInviteHandler = handler;
    AsyncMatchServiceNotifyChallengeResultHandler = handler;
    AsyncMatchServiceWatchBattleHandler = handler;
    AsyncMatchServiceRequestBattleTicketHandler = handler;
    AsyncMatchServiceListWatchableBattlesHandler = handler;
}

void SetMatchServiceIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncMatchServiceJoinQueueHandler) {
        AsyncMatchServiceJoinQueueHandler = handler;
    }
    if (!AsyncMatchServiceCancelQueueHandler) {
        AsyncMatchServiceCancelQueueHandler = handler;
    }
    if (!AsyncMatchServiceGetQueueStatusHandler) {
        AsyncMatchServiceGetQueueStatusHandler = handler;
    }
    if (!AsyncMatchServiceChallengePlayerHandler) {
        AsyncMatchServiceChallengePlayerHandler = handler;
    }
    if (!AsyncMatchServiceRespondChallengeHandler) {
        AsyncMatchServiceRespondChallengeHandler = handler;
    }
    if (!AsyncMatchServiceNotifyChallengeInviteHandler) {
        AsyncMatchServiceNotifyChallengeInviteHandler = handler;
    }
    if (!AsyncMatchServiceNotifyChallengeResultHandler) {
        AsyncMatchServiceNotifyChallengeResultHandler = handler;
    }
    if (!AsyncMatchServiceWatchBattleHandler) {
        AsyncMatchServiceWatchBattleHandler = handler;
    }
    if (!AsyncMatchServiceRequestBattleTicketHandler) {
        AsyncMatchServiceRequestBattleTicketHandler = handler;
    }
    if (!AsyncMatchServiceListWatchableBattlesHandler) {
        AsyncMatchServiceListWatchableBattlesHandler = handler;
    }
}

void SetMatchServiceFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    AsyncMatchServiceJoinQueueFailedHandler = handler;
    AsyncMatchServiceCancelQueueFailedHandler = handler;
    AsyncMatchServiceGetQueueStatusFailedHandler = handler;
    AsyncMatchServiceChallengePlayerFailedHandler = handler;
    AsyncMatchServiceRespondChallengeFailedHandler = handler;
    AsyncMatchServiceNotifyChallengeInviteFailedHandler = handler;
    AsyncMatchServiceNotifyChallengeResultFailedHandler = handler;
    AsyncMatchServiceWatchBattleFailedHandler = handler;
    AsyncMatchServiceRequestBattleTicketFailedHandler = handler;
    AsyncMatchServiceListWatchableBattlesFailedHandler = handler;
}

void SetMatchServiceIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    if (!AsyncMatchServiceJoinQueueFailedHandler) {
        AsyncMatchServiceJoinQueueFailedHandler = handler;
    }
    if (!AsyncMatchServiceCancelQueueFailedHandler) {
        AsyncMatchServiceCancelQueueFailedHandler = handler;
    }
    if (!AsyncMatchServiceGetQueueStatusFailedHandler) {
        AsyncMatchServiceGetQueueStatusFailedHandler = handler;
    }
    if (!AsyncMatchServiceChallengePlayerFailedHandler) {
        AsyncMatchServiceChallengePlayerFailedHandler = handler;
    }
    if (!AsyncMatchServiceRespondChallengeFailedHandler) {
        AsyncMatchServiceRespondChallengeFailedHandler = handler;
    }
    if (!AsyncMatchServiceNotifyChallengeInviteFailedHandler) {
        AsyncMatchServiceNotifyChallengeInviteFailedHandler = handler;
    }
    if (!AsyncMatchServiceNotifyChallengeResultFailedHandler) {
        AsyncMatchServiceNotifyChallengeResultFailedHandler = handler;
    }
    if (!AsyncMatchServiceWatchBattleFailedHandler) {
        AsyncMatchServiceWatchBattleFailedHandler = handler;
    }
    if (!AsyncMatchServiceRequestBattleTicketFailedHandler) {
        AsyncMatchServiceRequestBattleTicketFailedHandler = handler;
    }
    if (!AsyncMatchServiceListWatchableBattlesFailedHandler) {
        AsyncMatchServiceListWatchableBattlesFailedHandler = handler;
    }
}

void SetMatchServiceCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

void InitMatchServiceGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<MatchServiceStubPtr>(nodeEntity, MatchService::NewStub(channel));

}

}// namespace match
