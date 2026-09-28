#include "muduo/base/Logging.h"

#include "db_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <atomic>
#include <chrono>
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
// 本文件所有 unary 调用的 deadline(毫秒)。启动时 SetDbCallDeadline 按目标节点类型写入
// (Node::Initialize → grpc_call_deadline::Apply);原子量:与应答处理器一样是进程级全局。
std::atomic<uint32_t> callDeadlineMs{kDefaultGrpcCallDeadlineMs};

std::chrono::system_clock::time_point NextCallDeadline() {
    return std::chrono::system_clock::now() +
        std::chrono::milliseconds(callDeadlineMs.load(std::memory_order_relaxed));
}
}

struct DbCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region dbTest
boost::object_pool<AsyncdbTestGrpcClient> dbTestPool;
using AsyncdbTestHandlerFunctionType =
    std::function<void(const ClientContext&, const ::TestResponse&)>;
AsyncdbTestHandlerFunctionType AsyncdbTestHandler;
AsyncdbTestFailedHandlerFunctionType AsyncdbTestFailedHandler;

void AsyncCompleteGrpcdbTest(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncdbTestGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncdbTestHandler) {
            AsyncdbTestHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC db.Test reply dropped: AsyncdbTestHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncdbTestFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "db.Test", call->context, call->status, call->sentMetadata};
        AsyncdbTestFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC db.Test failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	dbTestPool.destroy(call);
}

void SenddbTest(entt::registry& registry, entt::entity nodeEntity, const ::TestRequest& request) {

    SenddbTest(registry, nodeEntity, request, {}, {});

}

void SenddbTest(entt::registry& registry, entt::entity nodeEntity, const ::TestRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(dbTestPool.construct());
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
        .get<dbStubPtr>(nodeEntity)
        ->PrepareAsyncTest(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(dbTestMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SenddbTest(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::TestRequest& derived = static_cast<const ::TestRequest&>(message);
    SenddbTest(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleDbCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case dbTestMessageId:
            AsyncCompleteGrpcdbTest(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetDbHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncdbTestHandler = handler;
}

void SetDbIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncdbTestHandler) {
        AsyncdbTestHandler = handler;
    }
}

void SetDbFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    AsyncdbTestFailedHandler = handler;
}

void SetDbIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    if (!AsyncdbTestFailedHandler) {
        AsyncdbTestFailedHandler = handler;
    }
}

void SetDbCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

void InitDbGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<dbStubPtr>(nodeEntity, db::NewStub(channel));

}
