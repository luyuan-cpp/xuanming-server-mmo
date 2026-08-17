#include "muduo/base/Logging.h"

#include "db_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
}

struct DbCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region dbTest
boost::object_pool<AsyncdbTestGrpcClient> dbTestPool;
using AsyncdbTestHandlerFunctionType =
    std::function<void(const ClientContext&, const ::TestResponse&)>;
AsyncdbTestHandlerFunctionType AsyncdbTestHandler;

void AsyncCompleteGrpcdbTest(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncdbTestGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncdbTestHandler) {
            AsyncdbTestHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	dbTestPool.destroy(call);
}

void SenddbTest(entt::registry& registry, entt::entity nodeEntity, const ::TestRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(dbTestPool.construct());
    call->response_reader = registry
        .get<dbStubPtr>(nodeEntity)
        ->PrepareAsyncTest(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(dbTestMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SenddbTest(entt::registry& registry, entt::entity nodeEntity, const ::TestRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(dbTestPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<dbStubPtr>(nodeEntity)
        ->PrepareAsyncTest(&call->context, request,
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

void InitDbGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<dbStubPtr>(nodeEntity, db::NewStub(channel));

}
