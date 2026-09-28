#include "muduo/base/Logging.h"

#include "chat_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <atomic>
#include <chrono>
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
// 本文件所有 unary 调用的 deadline(毫秒)。启动时 SetChatCallDeadline 按目标节点类型写入
// (Node::Initialize → grpc_call_deadline::Apply);原子量:与应答处理器一样是进程级全局。
std::atomic<uint32_t> callDeadlineMs{kDefaultGrpcCallDeadlineMs};

std::chrono::system_clock::time_point NextCallDeadline() {
    return std::chrono::system_clock::now() +
        std::chrono::milliseconds(callDeadlineMs.load(std::memory_order_relaxed));
}
}

namespace chatpb {
struct ChatCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region ClientPlayerChatSendChat
boost::object_pool<AsyncClientPlayerChatSendChatGrpcClient> ClientPlayerChatSendChatPool;
using AsyncClientPlayerChatSendChatHandlerFunctionType =
    std::function<void(const ClientContext&, const ::chatpb::SendChatResponse&)>;
AsyncClientPlayerChatSendChatHandlerFunctionType AsyncClientPlayerChatSendChatHandler;
AsyncClientPlayerChatSendChatFailedHandlerFunctionType AsyncClientPlayerChatSendChatFailedHandler;

void AsyncCompleteGrpcClientPlayerChatSendChat(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerChatSendChatGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerChatSendChatHandler) {
            AsyncClientPlayerChatSendChatHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerChat.SendChat reply dropped: AsyncClientPlayerChatSendChatHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerChatSendChatFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerChat.SendChat", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerChatSendChatFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerChat.SendChat failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerChatSendChatPool.destroy(call);
}

void SendClientPlayerChatSendChat(entt::registry& registry, entt::entity nodeEntity, const ::chatpb::SendChatRequest& request) {

    SendClientPlayerChatSendChat(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerChatSendChat(entt::registry& registry, entt::entity nodeEntity, const ::chatpb::SendChatRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerChatSendChatPool.construct());
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
        .get<ClientPlayerChatStubPtr>(nodeEntity)
        ->PrepareAsyncSendChat(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerChatSendChatMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerChatSendChat(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::chatpb::SendChatRequest& derived = static_cast<const ::chatpb::SendChatRequest&>(message);
    SendClientPlayerChatSendChat(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerChatPullChatHistory
boost::object_pool<AsyncClientPlayerChatPullChatHistoryGrpcClient> ClientPlayerChatPullChatHistoryPool;
using AsyncClientPlayerChatPullChatHistoryHandlerFunctionType =
    std::function<void(const ClientContext&, const ::chatpb::PullChatHistoryResponse&)>;
AsyncClientPlayerChatPullChatHistoryHandlerFunctionType AsyncClientPlayerChatPullChatHistoryHandler;
AsyncClientPlayerChatPullChatHistoryFailedHandlerFunctionType AsyncClientPlayerChatPullChatHistoryFailedHandler;

void AsyncCompleteGrpcClientPlayerChatPullChatHistory(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerChatPullChatHistoryGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerChatPullChatHistoryHandler) {
            AsyncClientPlayerChatPullChatHistoryHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerChat.PullChatHistory reply dropped: AsyncClientPlayerChatPullChatHistoryHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerChatPullChatHistoryFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerChat.PullChatHistory", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerChatPullChatHistoryFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerChat.PullChatHistory failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerChatPullChatHistoryPool.destroy(call);
}

void SendClientPlayerChatPullChatHistory(entt::registry& registry, entt::entity nodeEntity, const ::chatpb::PullChatHistoryRequest& request) {

    SendClientPlayerChatPullChatHistory(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerChatPullChatHistory(entt::registry& registry, entt::entity nodeEntity, const ::chatpb::PullChatHistoryRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerChatPullChatHistoryPool.construct());
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
        .get<ClientPlayerChatStubPtr>(nodeEntity)
        ->PrepareAsyncPullChatHistory(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerChatPullChatHistoryMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerChatPullChatHistory(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::chatpb::PullChatHistoryRequest& derived = static_cast<const ::chatpb::PullChatHistoryRequest&>(message);
    SendClientPlayerChatPullChatHistory(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleChatCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case ClientPlayerChatSendChatMessageId:
            AsyncCompleteGrpcClientPlayerChatSendChat(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerChatPullChatHistoryMessageId:
            AsyncCompleteGrpcClientPlayerChatPullChatHistory(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetChatHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncClientPlayerChatSendChatHandler = handler;
    AsyncClientPlayerChatPullChatHistoryHandler = handler;
}

void SetChatIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncClientPlayerChatSendChatHandler) {
        AsyncClientPlayerChatSendChatHandler = handler;
    }
    if (!AsyncClientPlayerChatPullChatHistoryHandler) {
        AsyncClientPlayerChatPullChatHistoryHandler = handler;
    }
}

void SetChatFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    AsyncClientPlayerChatSendChatFailedHandler = handler;
    AsyncClientPlayerChatPullChatHistoryFailedHandler = handler;
}

void SetChatIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    if (!AsyncClientPlayerChatSendChatFailedHandler) {
        AsyncClientPlayerChatSendChatFailedHandler = handler;
    }
    if (!AsyncClientPlayerChatPullChatHistoryFailedHandler) {
        AsyncClientPlayerChatPullChatHistoryFailedHandler = handler;
    }
}

void SetChatCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

void InitChatGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<ClientPlayerChatStubPtr>(nodeEntity, ClientPlayerChat::NewStub(channel));

}

}// namespace chatpb
