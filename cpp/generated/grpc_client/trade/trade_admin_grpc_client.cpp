#include "muduo/base/Logging.h"

#include "trade_admin_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <atomic>
#include <chrono>
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
// 本文件所有 unary 调用的 deadline(毫秒)。启动时 SetTradeAdminCallDeadline 按目标节点类型写入
// (Node::Initialize → grpc_call_deadline::Apply);原子量:与应答处理器一样是进程级全局。
std::atomic<uint32_t> callDeadlineMs{kDefaultGrpcCallDeadlineMs};

std::chrono::system_clock::time_point NextCallDeadline() {
    return std::chrono::system_clock::now() +
        std::chrono::milliseconds(callDeadlineMs.load(std::memory_order_relaxed));
}
}

namespace trade {
struct TradeAdminCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region TradeAdminSeedListing
boost::object_pool<AsyncTradeAdminSeedListingGrpcClient> TradeAdminSeedListingPool;
using AsyncTradeAdminSeedListingHandlerFunctionType =
    std::function<void(const ClientContext&, const ::trade::SeedListingResponse&)>;
AsyncTradeAdminSeedListingHandlerFunctionType AsyncTradeAdminSeedListingHandler;
AsyncTradeAdminSeedListingFailedHandlerFunctionType AsyncTradeAdminSeedListingFailedHandler;

void AsyncCompleteGrpcTradeAdminSeedListing(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncTradeAdminSeedListingGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncTradeAdminSeedListingHandler) {
            AsyncTradeAdminSeedListingHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC TradeAdmin.SeedListing reply dropped: AsyncTradeAdminSeedListingHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncTradeAdminSeedListingFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "TradeAdmin.SeedListing", call->context, call->status, call->sentMetadata};
        AsyncTradeAdminSeedListingFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC TradeAdmin.SeedListing failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	TradeAdminSeedListingPool.destroy(call);
}

void SendTradeAdminSeedListing(entt::registry& registry, entt::entity nodeEntity, const ::trade::SeedListingRequest& request) {

    SendTradeAdminSeedListing(registry, nodeEntity, request, {}, {});

}

void SendTradeAdminSeedListing(entt::registry& registry, entt::entity nodeEntity, const ::trade::SeedListingRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(TradeAdminSeedListingPool.construct());
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
        .get<TradeAdminStubPtr>(nodeEntity)
        ->PrepareAsyncSeedListing(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(TradeAdminSeedListingMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendTradeAdminSeedListing(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::trade::SeedListingRequest& derived = static_cast<const ::trade::SeedListingRequest&>(message);
    SendTradeAdminSeedListing(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleTradeAdminCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case TradeAdminSeedListingMessageId:
            AsyncCompleteGrpcTradeAdminSeedListing(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetTradeAdminHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncTradeAdminSeedListingHandler = handler;
}

void SetTradeAdminIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncTradeAdminSeedListingHandler) {
        AsyncTradeAdminSeedListingHandler = handler;
    }
}

void SetTradeAdminFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    AsyncTradeAdminSeedListingFailedHandler = handler;
}

void SetTradeAdminIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    if (!AsyncTradeAdminSeedListingFailedHandler) {
        AsyncTradeAdminSeedListingFailedHandler = handler;
    }
}

void SetTradeAdminCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

void InitTradeAdminGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<TradeAdminStubPtr>(nodeEntity, TradeAdmin::NewStub(channel));

}

}// namespace trade
