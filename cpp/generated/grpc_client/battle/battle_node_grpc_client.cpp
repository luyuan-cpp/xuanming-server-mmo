#include "muduo/base/Logging.h"

#include "battle_node_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <atomic>
#include <chrono>
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
// 本文件所有 unary 调用的 deadline(毫秒)。启动时 SetBattleNodeCallDeadline 按目标节点类型写入
// (Node::Initialize → grpc_call_deadline::Apply);原子量:与应答处理器一样是进程级全局。
std::atomic<uint32_t> callDeadlineMs{kDefaultGrpcCallDeadlineMs};

std::chrono::system_clock::time_point NextCallDeadline() {
    return std::chrono::system_clock::now() +
        std::chrono::milliseconds(callDeadlineMs.load(std::memory_order_relaxed));
}
}

struct BattleNodeCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region BattleNodeCreateBattle
boost::object_pool<AsyncBattleNodeCreateBattleGrpcClient> BattleNodeCreateBattlePool;
using AsyncBattleNodeCreateBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::CreateBattleResponse&)>;
AsyncBattleNodeCreateBattleHandlerFunctionType AsyncBattleNodeCreateBattleHandler;
AsyncBattleNodeCreateBattleFailedHandlerFunctionType AsyncBattleNodeCreateBattleFailedHandler;

void AsyncCompleteGrpcBattleNodeCreateBattle(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleNodeCreateBattleGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleNodeCreateBattleHandler) {
            AsyncBattleNodeCreateBattleHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC BattleNode.CreateBattle reply dropped: AsyncBattleNodeCreateBattleHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncBattleNodeCreateBattleFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "BattleNode.CreateBattle", call->context, call->status, call->sentMetadata};
        AsyncBattleNodeCreateBattleFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC BattleNode.CreateBattle failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	BattleNodeCreateBattlePool.destroy(call);
}

void SendBattleNodeCreateBattle(entt::registry& registry, entt::entity nodeEntity, const ::CreateBattleRequest& request) {

    SendBattleNodeCreateBattle(registry, nodeEntity, request, {}, {});

}

void SendBattleNodeCreateBattle(entt::registry& registry, entt::entity nodeEntity, const ::CreateBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleNodeCreateBattlePool.construct());
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
        .get<BattleNodeStubPtr>(nodeEntity)
        ->PrepareAsyncCreateBattle(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleNodeCreateBattleMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleNodeCreateBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::CreateBattleRequest& derived = static_cast<const ::CreateBattleRequest&>(message);
    SendBattleNodeCreateBattle(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region BattleNodeDestroyBattle
boost::object_pool<AsyncBattleNodeDestroyBattleGrpcClient> BattleNodeDestroyBattlePool;
using AsyncBattleNodeDestroyBattleHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncBattleNodeDestroyBattleHandlerFunctionType AsyncBattleNodeDestroyBattleHandler;
AsyncBattleNodeDestroyBattleFailedHandlerFunctionType AsyncBattleNodeDestroyBattleFailedHandler;

void AsyncCompleteGrpcBattleNodeDestroyBattle(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleNodeDestroyBattleGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleNodeDestroyBattleHandler) {
            AsyncBattleNodeDestroyBattleHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC BattleNode.DestroyBattle reply dropped: AsyncBattleNodeDestroyBattleHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncBattleNodeDestroyBattleFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "BattleNode.DestroyBattle", call->context, call->status, call->sentMetadata};
        AsyncBattleNodeDestroyBattleFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC BattleNode.DestroyBattle failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	BattleNodeDestroyBattlePool.destroy(call);
}

void SendBattleNodeDestroyBattle(entt::registry& registry, entt::entity nodeEntity, const ::DestroyBattleRequest& request) {

    SendBattleNodeDestroyBattle(registry, nodeEntity, request, {}, {});

}

void SendBattleNodeDestroyBattle(entt::registry& registry, entt::entity nodeEntity, const ::DestroyBattleRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleNodeDestroyBattlePool.construct());
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
        .get<BattleNodeStubPtr>(nodeEntity)
        ->PrepareAsyncDestroyBattle(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleNodeDestroyBattleMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleNodeDestroyBattle(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::DestroyBattleRequest& derived = static_cast<const ::DestroyBattleRequest&>(message);
    SendBattleNodeDestroyBattle(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region BattleNodeAddObserver
boost::object_pool<AsyncBattleNodeAddObserverGrpcClient> BattleNodeAddObserverPool;
using AsyncBattleNodeAddObserverHandlerFunctionType =
    std::function<void(const ClientContext&, const ::AddObserverResponse&)>;
AsyncBattleNodeAddObserverHandlerFunctionType AsyncBattleNodeAddObserverHandler;
AsyncBattleNodeAddObserverFailedHandlerFunctionType AsyncBattleNodeAddObserverFailedHandler;

void AsyncCompleteGrpcBattleNodeAddObserver(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleNodeAddObserverGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleNodeAddObserverHandler) {
            AsyncBattleNodeAddObserverHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC BattleNode.AddObserver reply dropped: AsyncBattleNodeAddObserverHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncBattleNodeAddObserverFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "BattleNode.AddObserver", call->context, call->status, call->sentMetadata};
        AsyncBattleNodeAddObserverFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC BattleNode.AddObserver failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	BattleNodeAddObserverPool.destroy(call);
}

void SendBattleNodeAddObserver(entt::registry& registry, entt::entity nodeEntity, const ::AddObserverRequest& request) {

    SendBattleNodeAddObserver(registry, nodeEntity, request, {}, {});

}

void SendBattleNodeAddObserver(entt::registry& registry, entt::entity nodeEntity, const ::AddObserverRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleNodeAddObserverPool.construct());
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
        .get<BattleNodeStubPtr>(nodeEntity)
        ->PrepareAsyncAddObserver(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleNodeAddObserverMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleNodeAddObserver(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::AddObserverRequest& derived = static_cast<const ::AddObserverRequest&>(message);
    SendBattleNodeAddObserver(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region BattleNodeRemoveObserver
boost::object_pool<AsyncBattleNodeRemoveObserverGrpcClient> BattleNodeRemoveObserverPool;
using AsyncBattleNodeRemoveObserverHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncBattleNodeRemoveObserverHandlerFunctionType AsyncBattleNodeRemoveObserverHandler;
AsyncBattleNodeRemoveObserverFailedHandlerFunctionType AsyncBattleNodeRemoveObserverFailedHandler;

void AsyncCompleteGrpcBattleNodeRemoveObserver(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleNodeRemoveObserverGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleNodeRemoveObserverHandler) {
            AsyncBattleNodeRemoveObserverHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC BattleNode.RemoveObserver reply dropped: AsyncBattleNodeRemoveObserverHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncBattleNodeRemoveObserverFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "BattleNode.RemoveObserver", call->context, call->status, call->sentMetadata};
        AsyncBattleNodeRemoveObserverFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC BattleNode.RemoveObserver failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	BattleNodeRemoveObserverPool.destroy(call);
}

void SendBattleNodeRemoveObserver(entt::registry& registry, entt::entity nodeEntity, const ::RemoveObserverRequest& request) {

    SendBattleNodeRemoveObserver(registry, nodeEntity, request, {}, {});

}

void SendBattleNodeRemoveObserver(entt::registry& registry, entt::entity nodeEntity, const ::RemoveObserverRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleNodeRemoveObserverPool.construct());
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
        .get<BattleNodeStubPtr>(nodeEntity)
        ->PrepareAsyncRemoveObserver(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleNodeRemoveObserverMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleNodeRemoveObserver(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::RemoveObserverRequest& derived = static_cast<const ::RemoveObserverRequest&>(message);
    SendBattleNodeRemoveObserver(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region BattleNodeIssueBattleTicket
boost::object_pool<AsyncBattleNodeIssueBattleTicketGrpcClient> BattleNodeIssueBattleTicketPool;
using AsyncBattleNodeIssueBattleTicketHandlerFunctionType =
    std::function<void(const ClientContext&, const ::IssueBattleTicketResponse&)>;
AsyncBattleNodeIssueBattleTicketHandlerFunctionType AsyncBattleNodeIssueBattleTicketHandler;
AsyncBattleNodeIssueBattleTicketFailedHandlerFunctionType AsyncBattleNodeIssueBattleTicketFailedHandler;

void AsyncCompleteGrpcBattleNodeIssueBattleTicket(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncBattleNodeIssueBattleTicketGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncBattleNodeIssueBattleTicketHandler) {
            AsyncBattleNodeIssueBattleTicketHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC BattleNode.IssueBattleTicket reply dropped: AsyncBattleNodeIssueBattleTicketHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncBattleNodeIssueBattleTicketFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "BattleNode.IssueBattleTicket", call->context, call->status, call->sentMetadata};
        AsyncBattleNodeIssueBattleTicketFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC BattleNode.IssueBattleTicket failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	BattleNodeIssueBattleTicketPool.destroy(call);
}

void SendBattleNodeIssueBattleTicket(entt::registry& registry, entt::entity nodeEntity, const ::IssueBattleTicketRequest& request) {

    SendBattleNodeIssueBattleTicket(registry, nodeEntity, request, {}, {});

}

void SendBattleNodeIssueBattleTicket(entt::registry& registry, entt::entity nodeEntity, const ::IssueBattleTicketRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(BattleNodeIssueBattleTicketPool.construct());
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
        .get<BattleNodeStubPtr>(nodeEntity)
        ->PrepareAsyncIssueBattleTicket(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(BattleNodeIssueBattleTicketMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendBattleNodeIssueBattleTicket(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::IssueBattleTicketRequest& derived = static_cast<const ::IssueBattleTicketRequest&>(message);
    SendBattleNodeIssueBattleTicket(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleBattleNodeCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case BattleNodeCreateBattleMessageId:
            AsyncCompleteGrpcBattleNodeCreateBattle(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case BattleNodeDestroyBattleMessageId:
            AsyncCompleteGrpcBattleNodeDestroyBattle(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case BattleNodeAddObserverMessageId:
            AsyncCompleteGrpcBattleNodeAddObserver(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case BattleNodeRemoveObserverMessageId:
            AsyncCompleteGrpcBattleNodeRemoveObserver(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case BattleNodeIssueBattleTicketMessageId:
            AsyncCompleteGrpcBattleNodeIssueBattleTicket(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetBattleNodeHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncBattleNodeCreateBattleHandler = handler;
    AsyncBattleNodeDestroyBattleHandler = handler;
    AsyncBattleNodeAddObserverHandler = handler;
    AsyncBattleNodeRemoveObserverHandler = handler;
    AsyncBattleNodeIssueBattleTicketHandler = handler;
}

void SetBattleNodeIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncBattleNodeCreateBattleHandler) {
        AsyncBattleNodeCreateBattleHandler = handler;
    }
    if (!AsyncBattleNodeDestroyBattleHandler) {
        AsyncBattleNodeDestroyBattleHandler = handler;
    }
    if (!AsyncBattleNodeAddObserverHandler) {
        AsyncBattleNodeAddObserverHandler = handler;
    }
    if (!AsyncBattleNodeRemoveObserverHandler) {
        AsyncBattleNodeRemoveObserverHandler = handler;
    }
    if (!AsyncBattleNodeIssueBattleTicketHandler) {
        AsyncBattleNodeIssueBattleTicketHandler = handler;
    }
}

void SetBattleNodeFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    AsyncBattleNodeCreateBattleFailedHandler = handler;
    AsyncBattleNodeDestroyBattleFailedHandler = handler;
    AsyncBattleNodeAddObserverFailedHandler = handler;
    AsyncBattleNodeRemoveObserverFailedHandler = handler;
    AsyncBattleNodeIssueBattleTicketFailedHandler = handler;
}

void SetBattleNodeIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    if (!AsyncBattleNodeCreateBattleFailedHandler) {
        AsyncBattleNodeCreateBattleFailedHandler = handler;
    }
    if (!AsyncBattleNodeDestroyBattleFailedHandler) {
        AsyncBattleNodeDestroyBattleFailedHandler = handler;
    }
    if (!AsyncBattleNodeAddObserverFailedHandler) {
        AsyncBattleNodeAddObserverFailedHandler = handler;
    }
    if (!AsyncBattleNodeRemoveObserverFailedHandler) {
        AsyncBattleNodeRemoveObserverFailedHandler = handler;
    }
    if (!AsyncBattleNodeIssueBattleTicketFailedHandler) {
        AsyncBattleNodeIssueBattleTicketFailedHandler = handler;
    }
}

void SetBattleNodeCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

void InitBattleNodeGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<BattleNodeStubPtr>(nodeEntity, BattleNode::NewStub(channel));

}
