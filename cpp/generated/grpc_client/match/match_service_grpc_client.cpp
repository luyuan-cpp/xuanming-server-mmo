#include "muduo/base/Logging.h"

#include "match_service_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
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

void AsyncCompleteGrpcMatchServiceJoinQueue(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceJoinQueueGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceJoinQueueHandler) {
            AsyncMatchServiceJoinQueueHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	MatchServiceJoinQueuePool.destroy(call);
}

void SendMatchServiceJoinQueue(entt::registry& registry, entt::entity nodeEntity, const ::match::JoinQueueRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(MatchServiceJoinQueuePool.construct());
    call->response_reader = registry
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncJoinQueue(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceJoinQueueMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceJoinQueue(entt::registry& registry, entt::entity nodeEntity, const ::match::JoinQueueRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceJoinQueuePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncJoinQueue(&call->context, request,
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

void AsyncCompleteGrpcMatchServiceCancelQueue(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceCancelQueueGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceCancelQueueHandler) {
            AsyncMatchServiceCancelQueueHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	MatchServiceCancelQueuePool.destroy(call);
}

void SendMatchServiceCancelQueue(entt::registry& registry, entt::entity nodeEntity, const ::match::CancelQueueRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(MatchServiceCancelQueuePool.construct());
    call->response_reader = registry
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncCancelQueue(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceCancelQueueMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceCancelQueue(entt::registry& registry, entt::entity nodeEntity, const ::match::CancelQueueRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceCancelQueuePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncCancelQueue(&call->context, request,
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

void AsyncCompleteGrpcMatchServiceGetQueueStatus(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceGetQueueStatusGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceGetQueueStatusHandler) {
            AsyncMatchServiceGetQueueStatusHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	MatchServiceGetQueueStatusPool.destroy(call);
}

void SendMatchServiceGetQueueStatus(entt::registry& registry, entt::entity nodeEntity, const ::match::GetQueueStatusRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(MatchServiceGetQueueStatusPool.construct());
    call->response_reader = registry
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetQueueStatus(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceGetQueueStatusMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceGetQueueStatus(entt::registry& registry, entt::entity nodeEntity, const ::match::GetQueueStatusRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceGetQueueStatusPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetQueueStatus(&call->context, request,
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

void AsyncCompleteGrpcMatchServiceChallengePlayer(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceChallengePlayerGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceChallengePlayerHandler) {
            AsyncMatchServiceChallengePlayerHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	MatchServiceChallengePlayerPool.destroy(call);
}

void SendMatchServiceChallengePlayer(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengePlayerRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(MatchServiceChallengePlayerPool.construct());
    call->response_reader = registry
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncChallengePlayer(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceChallengePlayerMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceChallengePlayer(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengePlayerRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceChallengePlayerPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncChallengePlayer(&call->context, request,
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

void AsyncCompleteGrpcMatchServiceRespondChallenge(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceRespondChallengeGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceRespondChallengeHandler) {
            AsyncMatchServiceRespondChallengeHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	MatchServiceRespondChallengePool.destroy(call);
}

void SendMatchServiceRespondChallenge(entt::registry& registry, entt::entity nodeEntity, const ::match::RespondChallengeRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(MatchServiceRespondChallengePool.construct());
    call->response_reader = registry
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRespondChallenge(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceRespondChallengeMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceRespondChallenge(entt::registry& registry, entt::entity nodeEntity, const ::match::RespondChallengeRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceRespondChallengePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRespondChallenge(&call->context, request,
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

void AsyncCompleteGrpcMatchServiceNotifyChallengeInvite(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceNotifyChallengeInviteGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceNotifyChallengeInviteHandler) {
            AsyncMatchServiceNotifyChallengeInviteHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	MatchServiceNotifyChallengeInvitePool.destroy(call);
}

void SendMatchServiceNotifyChallengeInvite(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeInviteS2C& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(MatchServiceNotifyChallengeInvitePool.construct());
    call->response_reader = registry
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyChallengeInvite(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceNotifyChallengeInviteMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceNotifyChallengeInvite(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeInviteS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceNotifyChallengeInvitePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyChallengeInvite(&call->context, request,
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

void AsyncCompleteGrpcMatchServiceNotifyChallengeResult(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncMatchServiceNotifyChallengeResultGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncMatchServiceNotifyChallengeResultHandler) {
            AsyncMatchServiceNotifyChallengeResultHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	MatchServiceNotifyChallengeResultPool.destroy(call);
}

void SendMatchServiceNotifyChallengeResult(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeResultS2C& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(MatchServiceNotifyChallengeResultPool.construct());
    call->response_reader = registry
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyChallengeResult(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(MatchServiceNotifyChallengeResultMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendMatchServiceNotifyChallengeResult(entt::registry& registry, entt::entity nodeEntity, const ::match::ChallengeResultS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(MatchServiceNotifyChallengeResultPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<MatchServiceStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyChallengeResult(&call->context, request,
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
}

void InitMatchServiceGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<MatchServiceStubPtr>(nodeEntity, MatchService::NewStub(channel));

}

}// namespace match
