#include "muduo/base/Logging.h"

#include "friend_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <atomic>
#include <chrono>
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
// 本文件所有 unary 调用的 deadline(毫秒)。启动时 SetFriendCallDeadline 按目标节点类型写入
// (Node::Initialize → grpc_call_deadline::Apply);原子量:与应答处理器一样是进程级全局。
std::atomic<uint32_t> callDeadlineMs{kDefaultGrpcCallDeadlineMs};

std::chrono::system_clock::time_point NextCallDeadline() {
    return std::chrono::system_clock::now() +
        std::chrono::milliseconds(callDeadlineMs.load(std::memory_order_relaxed));
}
}

namespace friendpb {
struct FriendCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region ClientPlayerFriendAddFriend
boost::object_pool<AsyncClientPlayerFriendAddFriendGrpcClient> ClientPlayerFriendAddFriendPool;
using AsyncClientPlayerFriendAddFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::AddFriendResponse&)>;
AsyncClientPlayerFriendAddFriendHandlerFunctionType AsyncClientPlayerFriendAddFriendHandler;
AsyncClientPlayerFriendAddFriendFailedHandlerFunctionType AsyncClientPlayerFriendAddFriendFailedHandler;

void AsyncCompleteGrpcClientPlayerFriendAddFriend(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendAddFriendGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendAddFriendHandler) {
            AsyncClientPlayerFriendAddFriendHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerFriend.AddFriend reply dropped: AsyncClientPlayerFriendAddFriendHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerFriendAddFriendFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerFriend.AddFriend", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerFriendAddFriendFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerFriend.AddFriend failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerFriendAddFriendPool.destroy(call);
}

void SendClientPlayerFriendAddFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AddFriendRequest& request) {

    SendClientPlayerFriendAddFriend(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerFriendAddFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AddFriendRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendAddFriendPool.construct());
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
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncAddFriend(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendAddFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendAddFriend(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::AddFriendRequest& derived = static_cast<const ::friendpb::AddFriendRequest&>(message);
    SendClientPlayerFriendAddFriend(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendAcceptFriend
boost::object_pool<AsyncClientPlayerFriendAcceptFriendGrpcClient> ClientPlayerFriendAcceptFriendPool;
using AsyncClientPlayerFriendAcceptFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::AcceptFriendResponse&)>;
AsyncClientPlayerFriendAcceptFriendHandlerFunctionType AsyncClientPlayerFriendAcceptFriendHandler;
AsyncClientPlayerFriendAcceptFriendFailedHandlerFunctionType AsyncClientPlayerFriendAcceptFriendFailedHandler;

void AsyncCompleteGrpcClientPlayerFriendAcceptFriend(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendAcceptFriendGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendAcceptFriendHandler) {
            AsyncClientPlayerFriendAcceptFriendHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerFriend.AcceptFriend reply dropped: AsyncClientPlayerFriendAcceptFriendHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerFriendAcceptFriendFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerFriend.AcceptFriend", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerFriendAcceptFriendFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerFriend.AcceptFriend failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerFriendAcceptFriendPool.destroy(call);
}

void SendClientPlayerFriendAcceptFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AcceptFriendRequest& request) {

    SendClientPlayerFriendAcceptFriend(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerFriendAcceptFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::AcceptFriendRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendAcceptFriendPool.construct());
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
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncAcceptFriend(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendAcceptFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendAcceptFriend(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::AcceptFriendRequest& derived = static_cast<const ::friendpb::AcceptFriendRequest&>(message);
    SendClientPlayerFriendAcceptFriend(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendRejectFriend
boost::object_pool<AsyncClientPlayerFriendRejectFriendGrpcClient> ClientPlayerFriendRejectFriendPool;
using AsyncClientPlayerFriendRejectFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::RejectFriendResponse&)>;
AsyncClientPlayerFriendRejectFriendHandlerFunctionType AsyncClientPlayerFriendRejectFriendHandler;
AsyncClientPlayerFriendRejectFriendFailedHandlerFunctionType AsyncClientPlayerFriendRejectFriendFailedHandler;

void AsyncCompleteGrpcClientPlayerFriendRejectFriend(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendRejectFriendGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendRejectFriendHandler) {
            AsyncClientPlayerFriendRejectFriendHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerFriend.RejectFriend reply dropped: AsyncClientPlayerFriendRejectFriendHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerFriendRejectFriendFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerFriend.RejectFriend", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerFriendRejectFriendFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerFriend.RejectFriend failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerFriendRejectFriendPool.destroy(call);
}

void SendClientPlayerFriendRejectFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RejectFriendRequest& request) {

    SendClientPlayerFriendRejectFriend(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerFriendRejectFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RejectFriendRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendRejectFriendPool.construct());
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
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncRejectFriend(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendRejectFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendRejectFriend(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::RejectFriendRequest& derived = static_cast<const ::friendpb::RejectFriendRequest&>(message);
    SendClientPlayerFriendRejectFriend(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendRemoveFriend
boost::object_pool<AsyncClientPlayerFriendRemoveFriendGrpcClient> ClientPlayerFriendRemoveFriendPool;
using AsyncClientPlayerFriendRemoveFriendHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::RemoveFriendResponse&)>;
AsyncClientPlayerFriendRemoveFriendHandlerFunctionType AsyncClientPlayerFriendRemoveFriendHandler;
AsyncClientPlayerFriendRemoveFriendFailedHandlerFunctionType AsyncClientPlayerFriendRemoveFriendFailedHandler;

void AsyncCompleteGrpcClientPlayerFriendRemoveFriend(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendRemoveFriendGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendRemoveFriendHandler) {
            AsyncClientPlayerFriendRemoveFriendHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerFriend.RemoveFriend reply dropped: AsyncClientPlayerFriendRemoveFriendHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerFriendRemoveFriendFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerFriend.RemoveFriend", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerFriendRemoveFriendFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerFriend.RemoveFriend failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerFriendRemoveFriendPool.destroy(call);
}

void SendClientPlayerFriendRemoveFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RemoveFriendRequest& request) {

    SendClientPlayerFriendRemoveFriend(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerFriendRemoveFriend(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RemoveFriendRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendRemoveFriendPool.construct());
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
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncRemoveFriend(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendRemoveFriendMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendRemoveFriend(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::RemoveFriendRequest& derived = static_cast<const ::friendpb::RemoveFriendRequest&>(message);
    SendClientPlayerFriendRemoveFriend(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendGetFriendList
boost::object_pool<AsyncClientPlayerFriendGetFriendListGrpcClient> ClientPlayerFriendGetFriendListPool;
using AsyncClientPlayerFriendGetFriendListHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::GetFriendListResponse&)>;
AsyncClientPlayerFriendGetFriendListHandlerFunctionType AsyncClientPlayerFriendGetFriendListHandler;
AsyncClientPlayerFriendGetFriendListFailedHandlerFunctionType AsyncClientPlayerFriendGetFriendListFailedHandler;

void AsyncCompleteGrpcClientPlayerFriendGetFriendList(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendGetFriendListGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendGetFriendListHandler) {
            AsyncClientPlayerFriendGetFriendListHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerFriend.GetFriendList reply dropped: AsyncClientPlayerFriendGetFriendListHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerFriendGetFriendListFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerFriend.GetFriendList", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerFriendGetFriendListFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerFriend.GetFriendList failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerFriendGetFriendListPool.destroy(call);
}

void SendClientPlayerFriendGetFriendList(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetFriendListRequest& request) {

    SendClientPlayerFriendGetFriendList(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerFriendGetFriendList(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetFriendListRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendGetFriendListPool.construct());
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
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncGetFriendList(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendGetFriendListMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendGetFriendList(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::GetFriendListRequest& derived = static_cast<const ::friendpb::GetFriendListRequest&>(message);
    SendClientPlayerFriendGetFriendList(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendGetPendingRequests
boost::object_pool<AsyncClientPlayerFriendGetPendingRequestsGrpcClient> ClientPlayerFriendGetPendingRequestsPool;
using AsyncClientPlayerFriendGetPendingRequestsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::GetPendingRequestsResponse&)>;
AsyncClientPlayerFriendGetPendingRequestsHandlerFunctionType AsyncClientPlayerFriendGetPendingRequestsHandler;
AsyncClientPlayerFriendGetPendingRequestsFailedHandlerFunctionType AsyncClientPlayerFriendGetPendingRequestsFailedHandler;

void AsyncCompleteGrpcClientPlayerFriendGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendGetPendingRequestsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendGetPendingRequestsHandler) {
            AsyncClientPlayerFriendGetPendingRequestsHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerFriend.GetPendingRequests reply dropped: AsyncClientPlayerFriendGetPendingRequestsHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerFriendGetPendingRequestsFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerFriend.GetPendingRequests", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerFriendGetPendingRequestsFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerFriend.GetPendingRequests failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerFriendGetPendingRequestsPool.destroy(call);
}

void SendClientPlayerFriendGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetPendingRequestsRequest& request) {

    SendClientPlayerFriendGetPendingRequests(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerFriendGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::GetPendingRequestsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendGetPendingRequestsPool.construct());
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
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncGetPendingRequests(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendGetPendingRequestsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendGetPendingRequests(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::GetPendingRequestsRequest& derived = static_cast<const ::friendpb::GetPendingRequestsRequest&>(message);
    SendClientPlayerFriendGetPendingRequests(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendBlock
boost::object_pool<AsyncClientPlayerFriendBlockGrpcClient> ClientPlayerFriendBlockPool;
using AsyncClientPlayerFriendBlockHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::BlockResponse&)>;
AsyncClientPlayerFriendBlockHandlerFunctionType AsyncClientPlayerFriendBlockHandler;
AsyncClientPlayerFriendBlockFailedHandlerFunctionType AsyncClientPlayerFriendBlockFailedHandler;

void AsyncCompleteGrpcClientPlayerFriendBlock(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendBlockGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendBlockHandler) {
            AsyncClientPlayerFriendBlockHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerFriend.Block reply dropped: AsyncClientPlayerFriendBlockHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerFriendBlockFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerFriend.Block", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerFriendBlockFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerFriend.Block failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerFriendBlockPool.destroy(call);
}

void SendClientPlayerFriendBlock(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::BlockRequest& request) {

    SendClientPlayerFriendBlock(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerFriendBlock(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::BlockRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendBlockPool.construct());
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
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncBlock(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendBlockMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendBlock(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::BlockRequest& derived = static_cast<const ::friendpb::BlockRequest&>(message);
    SendClientPlayerFriendBlock(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendUnblock
boost::object_pool<AsyncClientPlayerFriendUnblockGrpcClient> ClientPlayerFriendUnblockPool;
using AsyncClientPlayerFriendUnblockHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::UnblockResponse&)>;
AsyncClientPlayerFriendUnblockHandlerFunctionType AsyncClientPlayerFriendUnblockHandler;
AsyncClientPlayerFriendUnblockFailedHandlerFunctionType AsyncClientPlayerFriendUnblockFailedHandler;

void AsyncCompleteGrpcClientPlayerFriendUnblock(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendUnblockGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendUnblockHandler) {
            AsyncClientPlayerFriendUnblockHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerFriend.Unblock reply dropped: AsyncClientPlayerFriendUnblockHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerFriendUnblockFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerFriend.Unblock", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerFriendUnblockFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerFriend.Unblock failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerFriendUnblockPool.destroy(call);
}

void SendClientPlayerFriendUnblock(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::UnblockRequest& request) {

    SendClientPlayerFriendUnblock(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerFriendUnblock(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::UnblockRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendUnblockPool.construct());
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
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncUnblock(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendUnblockMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendUnblock(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::UnblockRequest& derived = static_cast<const ::friendpb::UnblockRequest&>(message);
    SendClientPlayerFriendUnblock(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendListBlocks
boost::object_pool<AsyncClientPlayerFriendListBlocksGrpcClient> ClientPlayerFriendListBlocksPool;
using AsyncClientPlayerFriendListBlocksHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::ListBlocksResponse&)>;
AsyncClientPlayerFriendListBlocksHandlerFunctionType AsyncClientPlayerFriendListBlocksHandler;
AsyncClientPlayerFriendListBlocksFailedHandlerFunctionType AsyncClientPlayerFriendListBlocksFailedHandler;

void AsyncCompleteGrpcClientPlayerFriendListBlocks(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendListBlocksGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendListBlocksHandler) {
            AsyncClientPlayerFriendListBlocksHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerFriend.ListBlocks reply dropped: AsyncClientPlayerFriendListBlocksHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerFriendListBlocksFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerFriend.ListBlocks", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerFriendListBlocksFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerFriend.ListBlocks failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerFriendListBlocksPool.destroy(call);
}

void SendClientPlayerFriendListBlocks(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::ListBlocksRequest& request) {

    SendClientPlayerFriendListBlocks(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerFriendListBlocks(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::ListBlocksRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendListBlocksPool.construct());
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
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncListBlocks(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendListBlocksMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendListBlocks(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::ListBlocksRequest& derived = static_cast<const ::friendpb::ListBlocksRequest&>(message);
    SendClientPlayerFriendListBlocks(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendRecommendFriends
boost::object_pool<AsyncClientPlayerFriendRecommendFriendsGrpcClient> ClientPlayerFriendRecommendFriendsPool;
using AsyncClientPlayerFriendRecommendFriendsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::friendpb::RecommendFriendsResponse&)>;
AsyncClientPlayerFriendRecommendFriendsHandlerFunctionType AsyncClientPlayerFriendRecommendFriendsHandler;
AsyncClientPlayerFriendRecommendFriendsFailedHandlerFunctionType AsyncClientPlayerFriendRecommendFriendsFailedHandler;

void AsyncCompleteGrpcClientPlayerFriendRecommendFriends(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendRecommendFriendsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendRecommendFriendsHandler) {
            AsyncClientPlayerFriendRecommendFriendsHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerFriend.RecommendFriends reply dropped: AsyncClientPlayerFriendRecommendFriendsHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerFriendRecommendFriendsFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerFriend.RecommendFriends", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerFriendRecommendFriendsFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerFriend.RecommendFriends failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerFriendRecommendFriendsPool.destroy(call);
}

void SendClientPlayerFriendRecommendFriends(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RecommendFriendsRequest& request) {

    SendClientPlayerFriendRecommendFriends(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerFriendRecommendFriends(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::RecommendFriendsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendRecommendFriendsPool.construct());
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
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncRecommendFriends(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendRecommendFriendsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendRecommendFriends(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::RecommendFriendsRequest& derived = static_cast<const ::friendpb::RecommendFriendsRequest&>(message);
    SendClientPlayerFriendRecommendFriends(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerFriendNotifyFriendEvent
boost::object_pool<AsyncClientPlayerFriendNotifyFriendEventGrpcClient> ClientPlayerFriendNotifyFriendEventPool;
using AsyncClientPlayerFriendNotifyFriendEventHandlerFunctionType =
    std::function<void(const ClientContext&, const ::Empty&)>;
AsyncClientPlayerFriendNotifyFriendEventHandlerFunctionType AsyncClientPlayerFriendNotifyFriendEventHandler;
AsyncClientPlayerFriendNotifyFriendEventFailedHandlerFunctionType AsyncClientPlayerFriendNotifyFriendEventFailedHandler;

void AsyncCompleteGrpcClientPlayerFriendNotifyFriendEvent(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerFriendNotifyFriendEventGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerFriendNotifyFriendEventHandler) {
            AsyncClientPlayerFriendNotifyFriendEventHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerFriend.NotifyFriendEvent reply dropped: AsyncClientPlayerFriendNotifyFriendEventHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerFriendNotifyFriendEventFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerFriend.NotifyFriendEvent", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerFriendNotifyFriendEventFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerFriend.NotifyFriendEvent failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerFriendNotifyFriendEventPool.destroy(call);
}

void SendClientPlayerFriendNotifyFriendEvent(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::FriendEventS2C& request) {

    SendClientPlayerFriendNotifyFriendEvent(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerFriendNotifyFriendEvent(entt::registry& registry, entt::entity nodeEntity, const ::friendpb::FriendEventS2C& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerFriendNotifyFriendEventPool.construct());
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
        .get<ClientPlayerFriendStubPtr>(nodeEntity)
        ->PrepareAsyncNotifyFriendEvent(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerFriendNotifyFriendEventMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerFriendNotifyFriendEvent(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::friendpb::FriendEventS2C& derived = static_cast<const ::friendpb::FriendEventS2C&>(message);
    SendClientPlayerFriendNotifyFriendEvent(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleFriendCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case ClientPlayerFriendAddFriendMessageId:
            AsyncCompleteGrpcClientPlayerFriendAddFriend(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendAcceptFriendMessageId:
            AsyncCompleteGrpcClientPlayerFriendAcceptFriend(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendRejectFriendMessageId:
            AsyncCompleteGrpcClientPlayerFriendRejectFriend(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendRemoveFriendMessageId:
            AsyncCompleteGrpcClientPlayerFriendRemoveFriend(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendGetFriendListMessageId:
            AsyncCompleteGrpcClientPlayerFriendGetFriendList(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendGetPendingRequestsMessageId:
            AsyncCompleteGrpcClientPlayerFriendGetPendingRequests(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendBlockMessageId:
            AsyncCompleteGrpcClientPlayerFriendBlock(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendUnblockMessageId:
            AsyncCompleteGrpcClientPlayerFriendUnblock(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendListBlocksMessageId:
            AsyncCompleteGrpcClientPlayerFriendListBlocks(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendRecommendFriendsMessageId:
            AsyncCompleteGrpcClientPlayerFriendRecommendFriends(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerFriendNotifyFriendEventMessageId:
            AsyncCompleteGrpcClientPlayerFriendNotifyFriendEvent(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetFriendHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncClientPlayerFriendAddFriendHandler = handler;
    AsyncClientPlayerFriendAcceptFriendHandler = handler;
    AsyncClientPlayerFriendRejectFriendHandler = handler;
    AsyncClientPlayerFriendRemoveFriendHandler = handler;
    AsyncClientPlayerFriendGetFriendListHandler = handler;
    AsyncClientPlayerFriendGetPendingRequestsHandler = handler;
    AsyncClientPlayerFriendBlockHandler = handler;
    AsyncClientPlayerFriendUnblockHandler = handler;
    AsyncClientPlayerFriendListBlocksHandler = handler;
    AsyncClientPlayerFriendRecommendFriendsHandler = handler;
    AsyncClientPlayerFriendNotifyFriendEventHandler = handler;
}

void SetFriendIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncClientPlayerFriendAddFriendHandler) {
        AsyncClientPlayerFriendAddFriendHandler = handler;
    }
    if (!AsyncClientPlayerFriendAcceptFriendHandler) {
        AsyncClientPlayerFriendAcceptFriendHandler = handler;
    }
    if (!AsyncClientPlayerFriendRejectFriendHandler) {
        AsyncClientPlayerFriendRejectFriendHandler = handler;
    }
    if (!AsyncClientPlayerFriendRemoveFriendHandler) {
        AsyncClientPlayerFriendRemoveFriendHandler = handler;
    }
    if (!AsyncClientPlayerFriendGetFriendListHandler) {
        AsyncClientPlayerFriendGetFriendListHandler = handler;
    }
    if (!AsyncClientPlayerFriendGetPendingRequestsHandler) {
        AsyncClientPlayerFriendGetPendingRequestsHandler = handler;
    }
    if (!AsyncClientPlayerFriendBlockHandler) {
        AsyncClientPlayerFriendBlockHandler = handler;
    }
    if (!AsyncClientPlayerFriendUnblockHandler) {
        AsyncClientPlayerFriendUnblockHandler = handler;
    }
    if (!AsyncClientPlayerFriendListBlocksHandler) {
        AsyncClientPlayerFriendListBlocksHandler = handler;
    }
    if (!AsyncClientPlayerFriendRecommendFriendsHandler) {
        AsyncClientPlayerFriendRecommendFriendsHandler = handler;
    }
    if (!AsyncClientPlayerFriendNotifyFriendEventHandler) {
        AsyncClientPlayerFriendNotifyFriendEventHandler = handler;
    }
}

void SetFriendFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    AsyncClientPlayerFriendAddFriendFailedHandler = handler;
    AsyncClientPlayerFriendAcceptFriendFailedHandler = handler;
    AsyncClientPlayerFriendRejectFriendFailedHandler = handler;
    AsyncClientPlayerFriendRemoveFriendFailedHandler = handler;
    AsyncClientPlayerFriendGetFriendListFailedHandler = handler;
    AsyncClientPlayerFriendGetPendingRequestsFailedHandler = handler;
    AsyncClientPlayerFriendBlockFailedHandler = handler;
    AsyncClientPlayerFriendUnblockFailedHandler = handler;
    AsyncClientPlayerFriendListBlocksFailedHandler = handler;
    AsyncClientPlayerFriendRecommendFriendsFailedHandler = handler;
    AsyncClientPlayerFriendNotifyFriendEventFailedHandler = handler;
}

void SetFriendIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    if (!AsyncClientPlayerFriendAddFriendFailedHandler) {
        AsyncClientPlayerFriendAddFriendFailedHandler = handler;
    }
    if (!AsyncClientPlayerFriendAcceptFriendFailedHandler) {
        AsyncClientPlayerFriendAcceptFriendFailedHandler = handler;
    }
    if (!AsyncClientPlayerFriendRejectFriendFailedHandler) {
        AsyncClientPlayerFriendRejectFriendFailedHandler = handler;
    }
    if (!AsyncClientPlayerFriendRemoveFriendFailedHandler) {
        AsyncClientPlayerFriendRemoveFriendFailedHandler = handler;
    }
    if (!AsyncClientPlayerFriendGetFriendListFailedHandler) {
        AsyncClientPlayerFriendGetFriendListFailedHandler = handler;
    }
    if (!AsyncClientPlayerFriendGetPendingRequestsFailedHandler) {
        AsyncClientPlayerFriendGetPendingRequestsFailedHandler = handler;
    }
    if (!AsyncClientPlayerFriendBlockFailedHandler) {
        AsyncClientPlayerFriendBlockFailedHandler = handler;
    }
    if (!AsyncClientPlayerFriendUnblockFailedHandler) {
        AsyncClientPlayerFriendUnblockFailedHandler = handler;
    }
    if (!AsyncClientPlayerFriendListBlocksFailedHandler) {
        AsyncClientPlayerFriendListBlocksFailedHandler = handler;
    }
    if (!AsyncClientPlayerFriendRecommendFriendsFailedHandler) {
        AsyncClientPlayerFriendRecommendFriendsFailedHandler = handler;
    }
    if (!AsyncClientPlayerFriendNotifyFriendEventFailedHandler) {
        AsyncClientPlayerFriendNotifyFriendEventFailedHandler = handler;
    }
}

void SetFriendCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

void InitFriendGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<ClientPlayerFriendStubPtr>(nodeEntity, ClientPlayerFriend::NewStub(channel));

}

}// namespace friendpb
