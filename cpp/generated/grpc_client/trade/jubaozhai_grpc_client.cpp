#include "muduo/base/Logging.h"

#include "jubaozhai_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <atomic>
#include <chrono>
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
// 本文件所有 unary 调用的 deadline(毫秒)。启动时 SetJubaozhaiCallDeadline 按目标节点类型写入
// (Node::Initialize → grpc_call_deadline::Apply);原子量:与应答处理器一样是进程级全局。
std::atomic<uint32_t> callDeadlineMs{kDefaultGrpcCallDeadlineMs};

std::chrono::system_clock::time_point NextCallDeadline() {
    return std::chrono::system_clock::now() +
        std::chrono::milliseconds(callDeadlineMs.load(std::memory_order_relaxed));
}
}

namespace trade {
struct JubaozhaiCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region ClientPlayerJubaozhaiBrowseListings
boost::object_pool<AsyncClientPlayerJubaozhaiBrowseListingsGrpcClient> ClientPlayerJubaozhaiBrowseListingsPool;
using AsyncClientPlayerJubaozhaiBrowseListingsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::BrowseListingsResponse&)>;
AsyncClientPlayerJubaozhaiBrowseListingsHandlerFunctionType AsyncClientPlayerJubaozhaiBrowseListingsHandler;
AsyncClientPlayerJubaozhaiBrowseListingsFailedHandlerFunctionType AsyncClientPlayerJubaozhaiBrowseListingsFailedHandler;

void AsyncCompleteGrpcClientPlayerJubaozhaiBrowseListings(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerJubaozhaiBrowseListingsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerJubaozhaiBrowseListingsHandler) {
            AsyncClientPlayerJubaozhaiBrowseListingsHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerJubaozhai.BrowseListings reply dropped: AsyncClientPlayerJubaozhaiBrowseListingsHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerJubaozhaiBrowseListingsFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerJubaozhai.BrowseListings", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerJubaozhaiBrowseListingsFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerJubaozhai.BrowseListings failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerJubaozhaiBrowseListingsPool.destroy(call);
}

void SendClientPlayerJubaozhaiBrowseListings(entt::registry& registry, entt::entity nodeEntity, const ::trade::BrowseListingsRequest& request) {

    SendClientPlayerJubaozhaiBrowseListings(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerJubaozhaiBrowseListings(entt::registry& registry, entt::entity nodeEntity, const ::trade::BrowseListingsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerJubaozhaiBrowseListingsPool.construct());
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
        .get<ClientPlayerJubaozhaiStubPtr>(nodeEntity)
        ->PrepareAsyncBrowseListings(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerJubaozhaiBrowseListingsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerJubaozhaiBrowseListings(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::trade::BrowseListingsRequest& derived = static_cast<const ::trade::BrowseListingsRequest&>(message);
    SendClientPlayerJubaozhaiBrowseListings(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetListingDetail
boost::object_pool<AsyncClientPlayerJubaozhaiGetListingDetailGrpcClient> ClientPlayerJubaozhaiGetListingDetailPool;
using AsyncClientPlayerJubaozhaiGetListingDetailHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::GetListingDetailResponse&)>;
AsyncClientPlayerJubaozhaiGetListingDetailHandlerFunctionType AsyncClientPlayerJubaozhaiGetListingDetailHandler;
AsyncClientPlayerJubaozhaiGetListingDetailFailedHandlerFunctionType AsyncClientPlayerJubaozhaiGetListingDetailFailedHandler;

void AsyncCompleteGrpcClientPlayerJubaozhaiGetListingDetail(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerJubaozhaiGetListingDetailGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerJubaozhaiGetListingDetailHandler) {
            AsyncClientPlayerJubaozhaiGetListingDetailHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerJubaozhai.GetListingDetail reply dropped: AsyncClientPlayerJubaozhaiGetListingDetailHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerJubaozhaiGetListingDetailFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerJubaozhai.GetListingDetail", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerJubaozhaiGetListingDetailFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerJubaozhai.GetListingDetail failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerJubaozhaiGetListingDetailPool.destroy(call);
}

void SendClientPlayerJubaozhaiGetListingDetail(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetListingDetailRequest& request) {

    SendClientPlayerJubaozhaiGetListingDetail(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerJubaozhaiGetListingDetail(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetListingDetailRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerJubaozhaiGetListingDetailPool.construct());
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
        .get<ClientPlayerJubaozhaiStubPtr>(nodeEntity)
        ->PrepareAsyncGetListingDetail(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerJubaozhaiGetListingDetailMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerJubaozhaiGetListingDetail(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::trade::GetListingDetailRequest& derived = static_cast<const ::trade::GetListingDetailRequest&>(message);
    SendClientPlayerJubaozhaiGetListingDetail(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerJubaozhaiSetFavorite
boost::object_pool<AsyncClientPlayerJubaozhaiSetFavoriteGrpcClient> ClientPlayerJubaozhaiSetFavoritePool;
using AsyncClientPlayerJubaozhaiSetFavoriteHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::SetFavoriteResponse&)>;
AsyncClientPlayerJubaozhaiSetFavoriteHandlerFunctionType AsyncClientPlayerJubaozhaiSetFavoriteHandler;
AsyncClientPlayerJubaozhaiSetFavoriteFailedHandlerFunctionType AsyncClientPlayerJubaozhaiSetFavoriteFailedHandler;

void AsyncCompleteGrpcClientPlayerJubaozhaiSetFavorite(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerJubaozhaiSetFavoriteGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerJubaozhaiSetFavoriteHandler) {
            AsyncClientPlayerJubaozhaiSetFavoriteHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerJubaozhai.SetFavorite reply dropped: AsyncClientPlayerJubaozhaiSetFavoriteHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerJubaozhaiSetFavoriteFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerJubaozhai.SetFavorite", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerJubaozhaiSetFavoriteFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerJubaozhai.SetFavorite failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerJubaozhaiSetFavoritePool.destroy(call);
}

void SendClientPlayerJubaozhaiSetFavorite(entt::registry& registry, entt::entity nodeEntity, const ::trade::SetFavoriteRequest& request) {

    SendClientPlayerJubaozhaiSetFavorite(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerJubaozhaiSetFavorite(entt::registry& registry, entt::entity nodeEntity, const ::trade::SetFavoriteRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerJubaozhaiSetFavoritePool.construct());
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
        .get<ClientPlayerJubaozhaiStubPtr>(nodeEntity)
        ->PrepareAsyncSetFavorite(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerJubaozhaiSetFavoriteMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerJubaozhaiSetFavorite(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::trade::SetFavoriteRequest& derived = static_cast<const ::trade::SetFavoriteRequest&>(message);
    SendClientPlayerJubaozhaiSetFavorite(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerJubaozhaiGetMyShelf
boost::object_pool<AsyncClientPlayerJubaozhaiGetMyShelfGrpcClient> ClientPlayerJubaozhaiGetMyShelfPool;
using AsyncClientPlayerJubaozhaiGetMyShelfHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::GetMyShelfResponse&)>;
AsyncClientPlayerJubaozhaiGetMyShelfHandlerFunctionType AsyncClientPlayerJubaozhaiGetMyShelfHandler;
AsyncClientPlayerJubaozhaiGetMyShelfFailedHandlerFunctionType AsyncClientPlayerJubaozhaiGetMyShelfFailedHandler;

void AsyncCompleteGrpcClientPlayerJubaozhaiGetMyShelf(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerJubaozhaiGetMyShelfGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerJubaozhaiGetMyShelfHandler) {
            AsyncClientPlayerJubaozhaiGetMyShelfHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerJubaozhai.GetMyShelf reply dropped: AsyncClientPlayerJubaozhaiGetMyShelfHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerJubaozhaiGetMyShelfFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerJubaozhai.GetMyShelf", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerJubaozhaiGetMyShelfFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerJubaozhai.GetMyShelf failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerJubaozhaiGetMyShelfPool.destroy(call);
}

void SendClientPlayerJubaozhaiGetMyShelf(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetMyShelfRequest& request) {

    SendClientPlayerJubaozhaiGetMyShelf(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerJubaozhaiGetMyShelf(entt::registry& registry, entt::entity nodeEntity, const ::trade::GetMyShelfRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerJubaozhaiGetMyShelfPool.construct());
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
        .get<ClientPlayerJubaozhaiStubPtr>(nodeEntity)
        ->PrepareAsyncGetMyShelf(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerJubaozhaiGetMyShelfMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerJubaozhaiGetMyShelf(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::trade::GetMyShelfRequest& derived = static_cast<const ::trade::GetMyShelfRequest&>(message);
    SendClientPlayerJubaozhaiGetMyShelf(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleJubaozhaiCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case ClientPlayerJubaozhaiBrowseListingsMessageId:
            AsyncCompleteGrpcClientPlayerJubaozhaiBrowseListings(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerJubaozhaiGetListingDetailMessageId:
            AsyncCompleteGrpcClientPlayerJubaozhaiGetListingDetail(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerJubaozhaiSetFavoriteMessageId:
            AsyncCompleteGrpcClientPlayerJubaozhaiSetFavorite(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerJubaozhaiGetMyShelfMessageId:
            AsyncCompleteGrpcClientPlayerJubaozhaiGetMyShelf(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetJubaozhaiHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncClientPlayerJubaozhaiBrowseListingsHandler = handler;
    AsyncClientPlayerJubaozhaiGetListingDetailHandler = handler;
    AsyncClientPlayerJubaozhaiSetFavoriteHandler = handler;
    AsyncClientPlayerJubaozhaiGetMyShelfHandler = handler;
}

void SetJubaozhaiIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncClientPlayerJubaozhaiBrowseListingsHandler) {
        AsyncClientPlayerJubaozhaiBrowseListingsHandler = handler;
    }
    if (!AsyncClientPlayerJubaozhaiGetListingDetailHandler) {
        AsyncClientPlayerJubaozhaiGetListingDetailHandler = handler;
    }
    if (!AsyncClientPlayerJubaozhaiSetFavoriteHandler) {
        AsyncClientPlayerJubaozhaiSetFavoriteHandler = handler;
    }
    if (!AsyncClientPlayerJubaozhaiGetMyShelfHandler) {
        AsyncClientPlayerJubaozhaiGetMyShelfHandler = handler;
    }
}

void SetJubaozhaiFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    AsyncClientPlayerJubaozhaiBrowseListingsFailedHandler = handler;
    AsyncClientPlayerJubaozhaiGetListingDetailFailedHandler = handler;
    AsyncClientPlayerJubaozhaiSetFavoriteFailedHandler = handler;
    AsyncClientPlayerJubaozhaiGetMyShelfFailedHandler = handler;
}

void SetJubaozhaiIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    if (!AsyncClientPlayerJubaozhaiBrowseListingsFailedHandler) {
        AsyncClientPlayerJubaozhaiBrowseListingsFailedHandler = handler;
    }
    if (!AsyncClientPlayerJubaozhaiGetListingDetailFailedHandler) {
        AsyncClientPlayerJubaozhaiGetListingDetailFailedHandler = handler;
    }
    if (!AsyncClientPlayerJubaozhaiSetFavoriteFailedHandler) {
        AsyncClientPlayerJubaozhaiSetFavoriteFailedHandler = handler;
    }
    if (!AsyncClientPlayerJubaozhaiGetMyShelfFailedHandler) {
        AsyncClientPlayerJubaozhaiGetMyShelfFailedHandler = handler;
    }
}

void SetJubaozhaiCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

void InitJubaozhaiGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<ClientPlayerJubaozhaiStubPtr>(nodeEntity, ClientPlayerJubaozhai::NewStub(channel));

}

}// namespace trade
