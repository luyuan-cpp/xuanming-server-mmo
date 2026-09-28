#include "muduo/base/Logging.h"

#include "data_service_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <atomic>
#include <chrono>
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
// 本文件所有 unary 调用的 deadline(毫秒)。启动时 SetDataServiceCallDeadline 按目标节点类型写入
// (Node::Initialize → grpc_call_deadline::Apply);原子量:与应答处理器一样是进程级全局。
std::atomic<uint32_t> callDeadlineMs{kDefaultGrpcCallDeadlineMs};

std::chrono::system_clock::time_point NextCallDeadline() {
    return std::chrono::system_clock::now() +
        std::chrono::milliseconds(callDeadlineMs.load(std::memory_order_relaxed));
}
}

namespace data_service {
struct DataServiceCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region DataServiceLoadPlayerData
boost::object_pool<AsyncDataServiceLoadPlayerDataGrpcClient> DataServiceLoadPlayerDataPool;
using AsyncDataServiceLoadPlayerDataHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::LoadPlayerDataResponse&)>;
AsyncDataServiceLoadPlayerDataHandlerFunctionType AsyncDataServiceLoadPlayerDataHandler;
AsyncDataServiceLoadPlayerDataFailedHandlerFunctionType AsyncDataServiceLoadPlayerDataFailedHandler;

void AsyncCompleteGrpcDataServiceLoadPlayerData(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceLoadPlayerDataGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceLoadPlayerDataHandler) {
            AsyncDataServiceLoadPlayerDataHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.LoadPlayerData reply dropped: AsyncDataServiceLoadPlayerDataHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceLoadPlayerDataFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.LoadPlayerData", call->context, call->status, call->sentMetadata};
        AsyncDataServiceLoadPlayerDataFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.LoadPlayerData failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceLoadPlayerDataPool.destroy(call);
}

void SendDataServiceLoadPlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::LoadPlayerDataRequest& request) {

    SendDataServiceLoadPlayerData(registry, nodeEntity, request, {}, {});

}

void SendDataServiceLoadPlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::LoadPlayerDataRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceLoadPlayerDataPool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncLoadPlayerData(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceLoadPlayerDataMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceLoadPlayerData(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::LoadPlayerDataRequest& derived = static_cast<const ::data_service::LoadPlayerDataRequest&>(message);
    SendDataServiceLoadPlayerData(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceSavePlayerData
boost::object_pool<AsyncDataServiceSavePlayerDataGrpcClient> DataServiceSavePlayerDataPool;
using AsyncDataServiceSavePlayerDataHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::SavePlayerDataResponse&)>;
AsyncDataServiceSavePlayerDataHandlerFunctionType AsyncDataServiceSavePlayerDataHandler;
AsyncDataServiceSavePlayerDataFailedHandlerFunctionType AsyncDataServiceSavePlayerDataFailedHandler;

void AsyncCompleteGrpcDataServiceSavePlayerData(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceSavePlayerDataGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceSavePlayerDataHandler) {
            AsyncDataServiceSavePlayerDataHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.SavePlayerData reply dropped: AsyncDataServiceSavePlayerDataHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceSavePlayerDataFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.SavePlayerData", call->context, call->status, call->sentMetadata};
        AsyncDataServiceSavePlayerDataFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.SavePlayerData failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceSavePlayerDataPool.destroy(call);
}

void SendDataServiceSavePlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::SavePlayerDataRequest& request) {

    SendDataServiceSavePlayerData(registry, nodeEntity, request, {}, {});

}

void SendDataServiceSavePlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::SavePlayerDataRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceSavePlayerDataPool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncSavePlayerData(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceSavePlayerDataMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceSavePlayerData(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::SavePlayerDataRequest& derived = static_cast<const ::data_service::SavePlayerDataRequest&>(message);
    SendDataServiceSavePlayerData(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceGetPlayerField
boost::object_pool<AsyncDataServiceGetPlayerFieldGrpcClient> DataServiceGetPlayerFieldPool;
using AsyncDataServiceGetPlayerFieldHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::GetPlayerFieldResponse&)>;
AsyncDataServiceGetPlayerFieldHandlerFunctionType AsyncDataServiceGetPlayerFieldHandler;
AsyncDataServiceGetPlayerFieldFailedHandlerFunctionType AsyncDataServiceGetPlayerFieldFailedHandler;

void AsyncCompleteGrpcDataServiceGetPlayerField(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceGetPlayerFieldGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceGetPlayerFieldHandler) {
            AsyncDataServiceGetPlayerFieldHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.GetPlayerField reply dropped: AsyncDataServiceGetPlayerFieldHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceGetPlayerFieldFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.GetPlayerField", call->context, call->status, call->sentMetadata};
        AsyncDataServiceGetPlayerFieldFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.GetPlayerField failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceGetPlayerFieldPool.destroy(call);
}

void SendDataServiceGetPlayerField(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerFieldRequest& request) {

    SendDataServiceGetPlayerField(registry, nodeEntity, request, {}, {});

}

void SendDataServiceGetPlayerField(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerFieldRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceGetPlayerFieldPool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetPlayerField(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceGetPlayerFieldMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceGetPlayerField(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::GetPlayerFieldRequest& derived = static_cast<const ::data_service::GetPlayerFieldRequest&>(message);
    SendDataServiceGetPlayerField(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceSetPlayerField
boost::object_pool<AsyncDataServiceSetPlayerFieldGrpcClient> DataServiceSetPlayerFieldPool;
using AsyncDataServiceSetPlayerFieldHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::SetPlayerFieldResponse&)>;
AsyncDataServiceSetPlayerFieldHandlerFunctionType AsyncDataServiceSetPlayerFieldHandler;
AsyncDataServiceSetPlayerFieldFailedHandlerFunctionType AsyncDataServiceSetPlayerFieldFailedHandler;

void AsyncCompleteGrpcDataServiceSetPlayerField(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceSetPlayerFieldGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceSetPlayerFieldHandler) {
            AsyncDataServiceSetPlayerFieldHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.SetPlayerField reply dropped: AsyncDataServiceSetPlayerFieldHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceSetPlayerFieldFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.SetPlayerField", call->context, call->status, call->sentMetadata};
        AsyncDataServiceSetPlayerFieldFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.SetPlayerField failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceSetPlayerFieldPool.destroy(call);
}

void SendDataServiceSetPlayerField(entt::registry& registry, entt::entity nodeEntity, const ::data_service::SetPlayerFieldRequest& request) {

    SendDataServiceSetPlayerField(registry, nodeEntity, request, {}, {});

}

void SendDataServiceSetPlayerField(entt::registry& registry, entt::entity nodeEntity, const ::data_service::SetPlayerFieldRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceSetPlayerFieldPool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncSetPlayerField(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceSetPlayerFieldMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceSetPlayerField(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::SetPlayerFieldRequest& derived = static_cast<const ::data_service::SetPlayerFieldRequest&>(message);
    SendDataServiceSetPlayerField(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceRegisterPlayerZone
boost::object_pool<AsyncDataServiceRegisterPlayerZoneGrpcClient> DataServiceRegisterPlayerZonePool;
using AsyncDataServiceRegisterPlayerZoneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::google::protobuf::Empty&)>;
AsyncDataServiceRegisterPlayerZoneHandlerFunctionType AsyncDataServiceRegisterPlayerZoneHandler;
AsyncDataServiceRegisterPlayerZoneFailedHandlerFunctionType AsyncDataServiceRegisterPlayerZoneFailedHandler;

void AsyncCompleteGrpcDataServiceRegisterPlayerZone(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceRegisterPlayerZoneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceRegisterPlayerZoneHandler) {
            AsyncDataServiceRegisterPlayerZoneHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.RegisterPlayerZone reply dropped: AsyncDataServiceRegisterPlayerZoneHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceRegisterPlayerZoneFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.RegisterPlayerZone", call->context, call->status, call->sentMetadata};
        AsyncDataServiceRegisterPlayerZoneFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.RegisterPlayerZone failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceRegisterPlayerZonePool.destroy(call);
}

void SendDataServiceRegisterPlayerZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RegisterPlayerZoneRequest& request) {

    SendDataServiceRegisterPlayerZone(registry, nodeEntity, request, {}, {});

}

void SendDataServiceRegisterPlayerZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RegisterPlayerZoneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceRegisterPlayerZonePool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRegisterPlayerZone(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceRegisterPlayerZoneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceRegisterPlayerZone(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::RegisterPlayerZoneRequest& derived = static_cast<const ::data_service::RegisterPlayerZoneRequest&>(message);
    SendDataServiceRegisterPlayerZone(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceGetPlayerHomeZone
boost::object_pool<AsyncDataServiceGetPlayerHomeZoneGrpcClient> DataServiceGetPlayerHomeZonePool;
using AsyncDataServiceGetPlayerHomeZoneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::GetPlayerHomeZoneResponse&)>;
AsyncDataServiceGetPlayerHomeZoneHandlerFunctionType AsyncDataServiceGetPlayerHomeZoneHandler;
AsyncDataServiceGetPlayerHomeZoneFailedHandlerFunctionType AsyncDataServiceGetPlayerHomeZoneFailedHandler;

void AsyncCompleteGrpcDataServiceGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceGetPlayerHomeZoneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceGetPlayerHomeZoneHandler) {
            AsyncDataServiceGetPlayerHomeZoneHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.GetPlayerHomeZone reply dropped: AsyncDataServiceGetPlayerHomeZoneHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceGetPlayerHomeZoneFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.GetPlayerHomeZone", call->context, call->status, call->sentMetadata};
        AsyncDataServiceGetPlayerHomeZoneFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.GetPlayerHomeZone failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceGetPlayerHomeZonePool.destroy(call);
}

void SendDataServiceGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerHomeZoneRequest& request) {

    SendDataServiceGetPlayerHomeZone(registry, nodeEntity, request, {}, {});

}

void SendDataServiceGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerHomeZoneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceGetPlayerHomeZonePool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetPlayerHomeZone(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceGetPlayerHomeZoneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::GetPlayerHomeZoneRequest& derived = static_cast<const ::data_service::GetPlayerHomeZoneRequest&>(message);
    SendDataServiceGetPlayerHomeZone(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceBatchGetPlayerHomeZone
boost::object_pool<AsyncDataServiceBatchGetPlayerHomeZoneGrpcClient> DataServiceBatchGetPlayerHomeZonePool;
using AsyncDataServiceBatchGetPlayerHomeZoneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::BatchGetPlayerHomeZoneResponse&)>;
AsyncDataServiceBatchGetPlayerHomeZoneHandlerFunctionType AsyncDataServiceBatchGetPlayerHomeZoneHandler;
AsyncDataServiceBatchGetPlayerHomeZoneFailedHandlerFunctionType AsyncDataServiceBatchGetPlayerHomeZoneFailedHandler;

void AsyncCompleteGrpcDataServiceBatchGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceBatchGetPlayerHomeZoneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceBatchGetPlayerHomeZoneHandler) {
            AsyncDataServiceBatchGetPlayerHomeZoneHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.BatchGetPlayerHomeZone reply dropped: AsyncDataServiceBatchGetPlayerHomeZoneHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceBatchGetPlayerHomeZoneFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.BatchGetPlayerHomeZone", call->context, call->status, call->sentMetadata};
        AsyncDataServiceBatchGetPlayerHomeZoneFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.BatchGetPlayerHomeZone failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceBatchGetPlayerHomeZonePool.destroy(call);
}

void SendDataServiceBatchGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerHomeZoneRequest& request) {

    SendDataServiceBatchGetPlayerHomeZone(registry, nodeEntity, request, {}, {});

}

void SendDataServiceBatchGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerHomeZoneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceBatchGetPlayerHomeZonePool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncBatchGetPlayerHomeZone(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceBatchGetPlayerHomeZoneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceBatchGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::BatchGetPlayerHomeZoneRequest& derived = static_cast<const ::data_service::BatchGetPlayerHomeZoneRequest&>(message);
    SendDataServiceBatchGetPlayerHomeZone(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceRemapHomeZoneForMerge
boost::object_pool<AsyncDataServiceRemapHomeZoneForMergeGrpcClient> DataServiceRemapHomeZoneForMergePool;
using AsyncDataServiceRemapHomeZoneForMergeHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::RemapHomeZoneForMergeResponse&)>;
AsyncDataServiceRemapHomeZoneForMergeHandlerFunctionType AsyncDataServiceRemapHomeZoneForMergeHandler;
AsyncDataServiceRemapHomeZoneForMergeFailedHandlerFunctionType AsyncDataServiceRemapHomeZoneForMergeFailedHandler;

void AsyncCompleteGrpcDataServiceRemapHomeZoneForMerge(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceRemapHomeZoneForMergeGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceRemapHomeZoneForMergeHandler) {
            AsyncDataServiceRemapHomeZoneForMergeHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.RemapHomeZoneForMerge reply dropped: AsyncDataServiceRemapHomeZoneForMergeHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceRemapHomeZoneForMergeFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.RemapHomeZoneForMerge", call->context, call->status, call->sentMetadata};
        AsyncDataServiceRemapHomeZoneForMergeFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.RemapHomeZoneForMerge failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceRemapHomeZoneForMergePool.destroy(call);
}

void SendDataServiceRemapHomeZoneForMerge(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RemapHomeZoneForMergeRequest& request) {

    SendDataServiceRemapHomeZoneForMerge(registry, nodeEntity, request, {}, {});

}

void SendDataServiceRemapHomeZoneForMerge(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RemapHomeZoneForMergeRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceRemapHomeZoneForMergePool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRemapHomeZoneForMerge(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceRemapHomeZoneForMergeMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceRemapHomeZoneForMerge(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::RemapHomeZoneForMergeRequest& derived = static_cast<const ::data_service::RemapHomeZoneForMergeRequest&>(message);
    SendDataServiceRemapHomeZoneForMerge(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceDeletePlayerData
boost::object_pool<AsyncDataServiceDeletePlayerDataGrpcClient> DataServiceDeletePlayerDataPool;
using AsyncDataServiceDeletePlayerDataHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::DeletePlayerDataResponse&)>;
AsyncDataServiceDeletePlayerDataHandlerFunctionType AsyncDataServiceDeletePlayerDataHandler;
AsyncDataServiceDeletePlayerDataFailedHandlerFunctionType AsyncDataServiceDeletePlayerDataFailedHandler;

void AsyncCompleteGrpcDataServiceDeletePlayerData(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceDeletePlayerDataGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceDeletePlayerDataHandler) {
            AsyncDataServiceDeletePlayerDataHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.DeletePlayerData reply dropped: AsyncDataServiceDeletePlayerDataHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceDeletePlayerDataFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.DeletePlayerData", call->context, call->status, call->sentMetadata};
        AsyncDataServiceDeletePlayerDataFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.DeletePlayerData failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceDeletePlayerDataPool.destroy(call);
}

void SendDataServiceDeletePlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::DeletePlayerDataRequest& request) {

    SendDataServiceDeletePlayerData(registry, nodeEntity, request, {}, {});

}

void SendDataServiceDeletePlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::DeletePlayerDataRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceDeletePlayerDataPool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncDeletePlayerData(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceDeletePlayerDataMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceDeletePlayerData(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::DeletePlayerDataRequest& derived = static_cast<const ::data_service::DeletePlayerDataRequest&>(message);
    SendDataServiceDeletePlayerData(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceCreatePlayerSnapshot
boost::object_pool<AsyncDataServiceCreatePlayerSnapshotGrpcClient> DataServiceCreatePlayerSnapshotPool;
using AsyncDataServiceCreatePlayerSnapshotHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::CreatePlayerSnapshotResponse&)>;
AsyncDataServiceCreatePlayerSnapshotHandlerFunctionType AsyncDataServiceCreatePlayerSnapshotHandler;
AsyncDataServiceCreatePlayerSnapshotFailedHandlerFunctionType AsyncDataServiceCreatePlayerSnapshotFailedHandler;

void AsyncCompleteGrpcDataServiceCreatePlayerSnapshot(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceCreatePlayerSnapshotGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceCreatePlayerSnapshotHandler) {
            AsyncDataServiceCreatePlayerSnapshotHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.CreatePlayerSnapshot reply dropped: AsyncDataServiceCreatePlayerSnapshotHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceCreatePlayerSnapshotFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.CreatePlayerSnapshot", call->context, call->status, call->sentMetadata};
        AsyncDataServiceCreatePlayerSnapshotFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.CreatePlayerSnapshot failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceCreatePlayerSnapshotPool.destroy(call);
}

void SendDataServiceCreatePlayerSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CreatePlayerSnapshotRequest& request) {

    SendDataServiceCreatePlayerSnapshot(registry, nodeEntity, request, {}, {});

}

void SendDataServiceCreatePlayerSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CreatePlayerSnapshotRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceCreatePlayerSnapshotPool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncCreatePlayerSnapshot(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceCreatePlayerSnapshotMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceCreatePlayerSnapshot(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::CreatePlayerSnapshotRequest& derived = static_cast<const ::data_service::CreatePlayerSnapshotRequest&>(message);
    SendDataServiceCreatePlayerSnapshot(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceListPlayerSnapshots
boost::object_pool<AsyncDataServiceListPlayerSnapshotsGrpcClient> DataServiceListPlayerSnapshotsPool;
using AsyncDataServiceListPlayerSnapshotsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::ListPlayerSnapshotsResponse&)>;
AsyncDataServiceListPlayerSnapshotsHandlerFunctionType AsyncDataServiceListPlayerSnapshotsHandler;
AsyncDataServiceListPlayerSnapshotsFailedHandlerFunctionType AsyncDataServiceListPlayerSnapshotsFailedHandler;

void AsyncCompleteGrpcDataServiceListPlayerSnapshots(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceListPlayerSnapshotsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceListPlayerSnapshotsHandler) {
            AsyncDataServiceListPlayerSnapshotsHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.ListPlayerSnapshots reply dropped: AsyncDataServiceListPlayerSnapshotsHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceListPlayerSnapshotsFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.ListPlayerSnapshots", call->context, call->status, call->sentMetadata};
        AsyncDataServiceListPlayerSnapshotsFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.ListPlayerSnapshots failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceListPlayerSnapshotsPool.destroy(call);
}

void SendDataServiceListPlayerSnapshots(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ListPlayerSnapshotsRequest& request) {

    SendDataServiceListPlayerSnapshots(registry, nodeEntity, request, {}, {});

}

void SendDataServiceListPlayerSnapshots(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ListPlayerSnapshotsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceListPlayerSnapshotsPool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncListPlayerSnapshots(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceListPlayerSnapshotsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceListPlayerSnapshots(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::ListPlayerSnapshotsRequest& derived = static_cast<const ::data_service::ListPlayerSnapshotsRequest&>(message);
    SendDataServiceListPlayerSnapshots(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceGetPlayerSnapshotDiff
boost::object_pool<AsyncDataServiceGetPlayerSnapshotDiffGrpcClient> DataServiceGetPlayerSnapshotDiffPool;
using AsyncDataServiceGetPlayerSnapshotDiffHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::GetPlayerSnapshotDiffResponse&)>;
AsyncDataServiceGetPlayerSnapshotDiffHandlerFunctionType AsyncDataServiceGetPlayerSnapshotDiffHandler;
AsyncDataServiceGetPlayerSnapshotDiffFailedHandlerFunctionType AsyncDataServiceGetPlayerSnapshotDiffFailedHandler;

void AsyncCompleteGrpcDataServiceGetPlayerSnapshotDiff(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceGetPlayerSnapshotDiffGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceGetPlayerSnapshotDiffHandler) {
            AsyncDataServiceGetPlayerSnapshotDiffHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.GetPlayerSnapshotDiff reply dropped: AsyncDataServiceGetPlayerSnapshotDiffHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceGetPlayerSnapshotDiffFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.GetPlayerSnapshotDiff", call->context, call->status, call->sentMetadata};
        AsyncDataServiceGetPlayerSnapshotDiffFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.GetPlayerSnapshotDiff failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceGetPlayerSnapshotDiffPool.destroy(call);
}

void SendDataServiceGetPlayerSnapshotDiff(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerSnapshotDiffRequest& request) {

    SendDataServiceGetPlayerSnapshotDiff(registry, nodeEntity, request, {}, {});

}

void SendDataServiceGetPlayerSnapshotDiff(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerSnapshotDiffRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceGetPlayerSnapshotDiffPool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetPlayerSnapshotDiff(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceGetPlayerSnapshotDiffMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceGetPlayerSnapshotDiff(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::GetPlayerSnapshotDiffRequest& derived = static_cast<const ::data_service::GetPlayerSnapshotDiffRequest&>(message);
    SendDataServiceGetPlayerSnapshotDiff(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceRollbackPlayer
boost::object_pool<AsyncDataServiceRollbackPlayerGrpcClient> DataServiceRollbackPlayerPool;
using AsyncDataServiceRollbackPlayerHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::RollbackPlayerResponse&)>;
AsyncDataServiceRollbackPlayerHandlerFunctionType AsyncDataServiceRollbackPlayerHandler;
AsyncDataServiceRollbackPlayerFailedHandlerFunctionType AsyncDataServiceRollbackPlayerFailedHandler;

void AsyncCompleteGrpcDataServiceRollbackPlayer(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceRollbackPlayerGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceRollbackPlayerHandler) {
            AsyncDataServiceRollbackPlayerHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.RollbackPlayer reply dropped: AsyncDataServiceRollbackPlayerHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceRollbackPlayerFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.RollbackPlayer", call->context, call->status, call->sentMetadata};
        AsyncDataServiceRollbackPlayerFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.RollbackPlayer failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceRollbackPlayerPool.destroy(call);
}

void SendDataServiceRollbackPlayer(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackPlayerRequest& request) {

    SendDataServiceRollbackPlayer(registry, nodeEntity, request, {}, {});

}

void SendDataServiceRollbackPlayer(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackPlayerRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceRollbackPlayerPool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRollbackPlayer(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceRollbackPlayerMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceRollbackPlayer(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::RollbackPlayerRequest& derived = static_cast<const ::data_service::RollbackPlayerRequest&>(message);
    SendDataServiceRollbackPlayer(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceRollbackZone
boost::object_pool<AsyncDataServiceRollbackZoneGrpcClient> DataServiceRollbackZonePool;
using AsyncDataServiceRollbackZoneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::RollbackZoneResponse&)>;
AsyncDataServiceRollbackZoneHandlerFunctionType AsyncDataServiceRollbackZoneHandler;
AsyncDataServiceRollbackZoneFailedHandlerFunctionType AsyncDataServiceRollbackZoneFailedHandler;

void AsyncCompleteGrpcDataServiceRollbackZone(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceRollbackZoneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceRollbackZoneHandler) {
            AsyncDataServiceRollbackZoneHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.RollbackZone reply dropped: AsyncDataServiceRollbackZoneHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceRollbackZoneFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.RollbackZone", call->context, call->status, call->sentMetadata};
        AsyncDataServiceRollbackZoneFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.RollbackZone failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceRollbackZonePool.destroy(call);
}

void SendDataServiceRollbackZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackZoneRequest& request) {

    SendDataServiceRollbackZone(registry, nodeEntity, request, {}, {});

}

void SendDataServiceRollbackZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackZoneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceRollbackZonePool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRollbackZone(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceRollbackZoneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceRollbackZone(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::RollbackZoneRequest& derived = static_cast<const ::data_service::RollbackZoneRequest&>(message);
    SendDataServiceRollbackZone(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceRollbackAll
boost::object_pool<AsyncDataServiceRollbackAllGrpcClient> DataServiceRollbackAllPool;
using AsyncDataServiceRollbackAllHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::RollbackAllResponse&)>;
AsyncDataServiceRollbackAllHandlerFunctionType AsyncDataServiceRollbackAllHandler;
AsyncDataServiceRollbackAllFailedHandlerFunctionType AsyncDataServiceRollbackAllFailedHandler;

void AsyncCompleteGrpcDataServiceRollbackAll(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceRollbackAllGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceRollbackAllHandler) {
            AsyncDataServiceRollbackAllHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.RollbackAll reply dropped: AsyncDataServiceRollbackAllHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceRollbackAllFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.RollbackAll", call->context, call->status, call->sentMetadata};
        AsyncDataServiceRollbackAllFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.RollbackAll failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceRollbackAllPool.destroy(call);
}

void SendDataServiceRollbackAll(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackAllRequest& request) {

    SendDataServiceRollbackAll(registry, nodeEntity, request, {}, {});

}

void SendDataServiceRollbackAll(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackAllRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceRollbackAllPool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRollbackAll(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceRollbackAllMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceRollbackAll(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::RollbackAllRequest& derived = static_cast<const ::data_service::RollbackAllRequest&>(message);
    SendDataServiceRollbackAll(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceBatchRecallItems
boost::object_pool<AsyncDataServiceBatchRecallItemsGrpcClient> DataServiceBatchRecallItemsPool;
using AsyncDataServiceBatchRecallItemsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::BatchRecallItemsResponse&)>;
AsyncDataServiceBatchRecallItemsHandlerFunctionType AsyncDataServiceBatchRecallItemsHandler;
AsyncDataServiceBatchRecallItemsFailedHandlerFunctionType AsyncDataServiceBatchRecallItemsFailedHandler;

void AsyncCompleteGrpcDataServiceBatchRecallItems(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceBatchRecallItemsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceBatchRecallItemsHandler) {
            AsyncDataServiceBatchRecallItemsHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.BatchRecallItems reply dropped: AsyncDataServiceBatchRecallItemsHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceBatchRecallItemsFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.BatchRecallItems", call->context, call->status, call->sentMetadata};
        AsyncDataServiceBatchRecallItemsFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.BatchRecallItems failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceBatchRecallItemsPool.destroy(call);
}

void SendDataServiceBatchRecallItems(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchRecallItemsRequest& request) {

    SendDataServiceBatchRecallItems(registry, nodeEntity, request, {}, {});

}

void SendDataServiceBatchRecallItems(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchRecallItemsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceBatchRecallItemsPool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncBatchRecallItems(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceBatchRecallItemsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceBatchRecallItems(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::BatchRecallItemsRequest& derived = static_cast<const ::data_service::BatchRecallItemsRequest&>(message);
    SendDataServiceBatchRecallItems(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceQueryTransactionLog
boost::object_pool<AsyncDataServiceQueryTransactionLogGrpcClient> DataServiceQueryTransactionLogPool;
using AsyncDataServiceQueryTransactionLogHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::QueryTransactionLogResponse&)>;
AsyncDataServiceQueryTransactionLogHandlerFunctionType AsyncDataServiceQueryTransactionLogHandler;
AsyncDataServiceQueryTransactionLogFailedHandlerFunctionType AsyncDataServiceQueryTransactionLogFailedHandler;

void AsyncCompleteGrpcDataServiceQueryTransactionLog(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceQueryTransactionLogGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceQueryTransactionLogHandler) {
            AsyncDataServiceQueryTransactionLogHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.QueryTransactionLog reply dropped: AsyncDataServiceQueryTransactionLogHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceQueryTransactionLogFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.QueryTransactionLog", call->context, call->status, call->sentMetadata};
        AsyncDataServiceQueryTransactionLogFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.QueryTransactionLog failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceQueryTransactionLogPool.destroy(call);
}

void SendDataServiceQueryTransactionLog(entt::registry& registry, entt::entity nodeEntity, const ::data_service::QueryTransactionLogRequest& request) {

    SendDataServiceQueryTransactionLog(registry, nodeEntity, request, {}, {});

}

void SendDataServiceQueryTransactionLog(entt::registry& registry, entt::entity nodeEntity, const ::data_service::QueryTransactionLogRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceQueryTransactionLogPool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncQueryTransactionLog(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceQueryTransactionLogMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceQueryTransactionLog(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::QueryTransactionLogRequest& derived = static_cast<const ::data_service::QueryTransactionLogRequest&>(message);
    SendDataServiceQueryTransactionLog(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceCreateEventSnapshot
boost::object_pool<AsyncDataServiceCreateEventSnapshotGrpcClient> DataServiceCreateEventSnapshotPool;
using AsyncDataServiceCreateEventSnapshotHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::CreateEventSnapshotResponse&)>;
AsyncDataServiceCreateEventSnapshotHandlerFunctionType AsyncDataServiceCreateEventSnapshotHandler;
AsyncDataServiceCreateEventSnapshotFailedHandlerFunctionType AsyncDataServiceCreateEventSnapshotFailedHandler;

void AsyncCompleteGrpcDataServiceCreateEventSnapshot(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceCreateEventSnapshotGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceCreateEventSnapshotHandler) {
            AsyncDataServiceCreateEventSnapshotHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.CreateEventSnapshot reply dropped: AsyncDataServiceCreateEventSnapshotHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceCreateEventSnapshotFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.CreateEventSnapshot", call->context, call->status, call->sentMetadata};
        AsyncDataServiceCreateEventSnapshotFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.CreateEventSnapshot failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceCreateEventSnapshotPool.destroy(call);
}

void SendDataServiceCreateEventSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CreateEventSnapshotRequest& request) {

    SendDataServiceCreateEventSnapshot(registry, nodeEntity, request, {}, {});

}

void SendDataServiceCreateEventSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CreateEventSnapshotRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceCreateEventSnapshotPool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncCreateEventSnapshot(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceCreateEventSnapshotMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceCreateEventSnapshot(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::CreateEventSnapshotRequest& derived = static_cast<const ::data_service::CreateEventSnapshotRequest&>(message);
    SendDataServiceCreateEventSnapshot(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceAllocateIdSegment
boost::object_pool<AsyncDataServiceAllocateIdSegmentGrpcClient> DataServiceAllocateIdSegmentPool;
using AsyncDataServiceAllocateIdSegmentHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::AllocateIdSegmentResponse&)>;
AsyncDataServiceAllocateIdSegmentHandlerFunctionType AsyncDataServiceAllocateIdSegmentHandler;
AsyncDataServiceAllocateIdSegmentFailedHandlerFunctionType AsyncDataServiceAllocateIdSegmentFailedHandler;

void AsyncCompleteGrpcDataServiceAllocateIdSegment(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceAllocateIdSegmentGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceAllocateIdSegmentHandler) {
            AsyncDataServiceAllocateIdSegmentHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.AllocateIdSegment reply dropped: AsyncDataServiceAllocateIdSegmentHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceAllocateIdSegmentFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.AllocateIdSegment", call->context, call->status, call->sentMetadata};
        AsyncDataServiceAllocateIdSegmentFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.AllocateIdSegment failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceAllocateIdSegmentPool.destroy(call);
}

void SendDataServiceAllocateIdSegment(entt::registry& registry, entt::entity nodeEntity, const ::data_service::AllocateIdSegmentRequest& request) {

    SendDataServiceAllocateIdSegment(registry, nodeEntity, request, {}, {});

}

void SendDataServiceAllocateIdSegment(entt::registry& registry, entt::entity nodeEntity, const ::data_service::AllocateIdSegmentRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceAllocateIdSegmentPool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncAllocateIdSegment(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceAllocateIdSegmentMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceAllocateIdSegment(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::AllocateIdSegmentRequest& derived = static_cast<const ::data_service::AllocateIdSegmentRequest&>(message);
    SendDataServiceAllocateIdSegment(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceReservePlayerName
boost::object_pool<AsyncDataServiceReservePlayerNameGrpcClient> DataServiceReservePlayerNamePool;
using AsyncDataServiceReservePlayerNameHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::ReservePlayerNameResponse&)>;
AsyncDataServiceReservePlayerNameHandlerFunctionType AsyncDataServiceReservePlayerNameHandler;
AsyncDataServiceReservePlayerNameFailedHandlerFunctionType AsyncDataServiceReservePlayerNameFailedHandler;

void AsyncCompleteGrpcDataServiceReservePlayerName(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceReservePlayerNameGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceReservePlayerNameHandler) {
            AsyncDataServiceReservePlayerNameHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.ReservePlayerName reply dropped: AsyncDataServiceReservePlayerNameHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceReservePlayerNameFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.ReservePlayerName", call->context, call->status, call->sentMetadata};
        AsyncDataServiceReservePlayerNameFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.ReservePlayerName failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceReservePlayerNamePool.destroy(call);
}

void SendDataServiceReservePlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReservePlayerNameRequest& request) {

    SendDataServiceReservePlayerName(registry, nodeEntity, request, {}, {});

}

void SendDataServiceReservePlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReservePlayerNameRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceReservePlayerNamePool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncReservePlayerName(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceReservePlayerNameMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceReservePlayerName(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::ReservePlayerNameRequest& derived = static_cast<const ::data_service::ReservePlayerNameRequest&>(message);
    SendDataServiceReservePlayerName(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceReleasePlayerName
boost::object_pool<AsyncDataServiceReleasePlayerNameGrpcClient> DataServiceReleasePlayerNamePool;
using AsyncDataServiceReleasePlayerNameHandlerFunctionType =
    std::function<void(const ClientContext&, const ::google::protobuf::Empty&)>;
AsyncDataServiceReleasePlayerNameHandlerFunctionType AsyncDataServiceReleasePlayerNameHandler;
AsyncDataServiceReleasePlayerNameFailedHandlerFunctionType AsyncDataServiceReleasePlayerNameFailedHandler;

void AsyncCompleteGrpcDataServiceReleasePlayerName(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceReleasePlayerNameGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceReleasePlayerNameHandler) {
            AsyncDataServiceReleasePlayerNameHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.ReleasePlayerName reply dropped: AsyncDataServiceReleasePlayerNameHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceReleasePlayerNameFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.ReleasePlayerName", call->context, call->status, call->sentMetadata};
        AsyncDataServiceReleasePlayerNameFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.ReleasePlayerName failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceReleasePlayerNamePool.destroy(call);
}

void SendDataServiceReleasePlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReleasePlayerNameRequest& request) {

    SendDataServiceReleasePlayerName(registry, nodeEntity, request, {}, {});

}

void SendDataServiceReleasePlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReleasePlayerNameRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceReleasePlayerNamePool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncReleasePlayerName(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceReleasePlayerNameMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceReleasePlayerName(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::ReleasePlayerNameRequest& derived = static_cast<const ::data_service::ReleasePlayerNameRequest&>(message);
    SendDataServiceReleasePlayerName(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region DataServiceBatchGetPlayerName
boost::object_pool<AsyncDataServiceBatchGetPlayerNameGrpcClient> DataServiceBatchGetPlayerNamePool;
using AsyncDataServiceBatchGetPlayerNameHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::BatchGetPlayerNameResponse&)>;
AsyncDataServiceBatchGetPlayerNameHandlerFunctionType AsyncDataServiceBatchGetPlayerNameHandler;
AsyncDataServiceBatchGetPlayerNameFailedHandlerFunctionType AsyncDataServiceBatchGetPlayerNameFailedHandler;

void AsyncCompleteGrpcDataServiceBatchGetPlayerName(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceBatchGetPlayerNameGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceBatchGetPlayerNameHandler) {
            AsyncDataServiceBatchGetPlayerNameHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC DataService.BatchGetPlayerName reply dropped: AsyncDataServiceBatchGetPlayerNameHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncDataServiceBatchGetPlayerNameFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "DataService.BatchGetPlayerName", call->context, call->status, call->sentMetadata};
        AsyncDataServiceBatchGetPlayerNameFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC DataService.BatchGetPlayerName failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	DataServiceBatchGetPlayerNamePool.destroy(call);
}

void SendDataServiceBatchGetPlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerNameRequest& request) {

    SendDataServiceBatchGetPlayerName(registry, nodeEntity, request, {}, {});

}

void SendDataServiceBatchGetPlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerNameRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceBatchGetPlayerNamePool.construct());
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
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncBatchGetPlayerName(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceBatchGetPlayerNameMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceBatchGetPlayerName(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::data_service::BatchGetPlayerNameRequest& derived = static_cast<const ::data_service::BatchGetPlayerNameRequest&>(message);
    SendDataServiceBatchGetPlayerName(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleDataServiceCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case DataServiceLoadPlayerDataMessageId:
            AsyncCompleteGrpcDataServiceLoadPlayerData(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceSavePlayerDataMessageId:
            AsyncCompleteGrpcDataServiceSavePlayerData(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceGetPlayerFieldMessageId:
            AsyncCompleteGrpcDataServiceGetPlayerField(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceSetPlayerFieldMessageId:
            AsyncCompleteGrpcDataServiceSetPlayerField(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceRegisterPlayerZoneMessageId:
            AsyncCompleteGrpcDataServiceRegisterPlayerZone(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceGetPlayerHomeZoneMessageId:
            AsyncCompleteGrpcDataServiceGetPlayerHomeZone(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceBatchGetPlayerHomeZoneMessageId:
            AsyncCompleteGrpcDataServiceBatchGetPlayerHomeZone(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceRemapHomeZoneForMergeMessageId:
            AsyncCompleteGrpcDataServiceRemapHomeZoneForMerge(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceDeletePlayerDataMessageId:
            AsyncCompleteGrpcDataServiceDeletePlayerData(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceCreatePlayerSnapshotMessageId:
            AsyncCompleteGrpcDataServiceCreatePlayerSnapshot(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceListPlayerSnapshotsMessageId:
            AsyncCompleteGrpcDataServiceListPlayerSnapshots(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceGetPlayerSnapshotDiffMessageId:
            AsyncCompleteGrpcDataServiceGetPlayerSnapshotDiff(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceRollbackPlayerMessageId:
            AsyncCompleteGrpcDataServiceRollbackPlayer(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceRollbackZoneMessageId:
            AsyncCompleteGrpcDataServiceRollbackZone(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceRollbackAllMessageId:
            AsyncCompleteGrpcDataServiceRollbackAll(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceBatchRecallItemsMessageId:
            AsyncCompleteGrpcDataServiceBatchRecallItems(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceQueryTransactionLogMessageId:
            AsyncCompleteGrpcDataServiceQueryTransactionLog(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceCreateEventSnapshotMessageId:
            AsyncCompleteGrpcDataServiceCreateEventSnapshot(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceAllocateIdSegmentMessageId:
            AsyncCompleteGrpcDataServiceAllocateIdSegment(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceReservePlayerNameMessageId:
            AsyncCompleteGrpcDataServiceReservePlayerName(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceReleasePlayerNameMessageId:
            AsyncCompleteGrpcDataServiceReleasePlayerName(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case DataServiceBatchGetPlayerNameMessageId:
            AsyncCompleteGrpcDataServiceBatchGetPlayerName(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetDataServiceHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncDataServiceLoadPlayerDataHandler = handler;
    AsyncDataServiceSavePlayerDataHandler = handler;
    AsyncDataServiceGetPlayerFieldHandler = handler;
    AsyncDataServiceSetPlayerFieldHandler = handler;
    AsyncDataServiceRegisterPlayerZoneHandler = handler;
    AsyncDataServiceGetPlayerHomeZoneHandler = handler;
    AsyncDataServiceBatchGetPlayerHomeZoneHandler = handler;
    AsyncDataServiceRemapHomeZoneForMergeHandler = handler;
    AsyncDataServiceDeletePlayerDataHandler = handler;
    AsyncDataServiceCreatePlayerSnapshotHandler = handler;
    AsyncDataServiceListPlayerSnapshotsHandler = handler;
    AsyncDataServiceGetPlayerSnapshotDiffHandler = handler;
    AsyncDataServiceRollbackPlayerHandler = handler;
    AsyncDataServiceRollbackZoneHandler = handler;
    AsyncDataServiceRollbackAllHandler = handler;
    AsyncDataServiceBatchRecallItemsHandler = handler;
    AsyncDataServiceQueryTransactionLogHandler = handler;
    AsyncDataServiceCreateEventSnapshotHandler = handler;
    AsyncDataServiceAllocateIdSegmentHandler = handler;
    AsyncDataServiceReservePlayerNameHandler = handler;
    AsyncDataServiceReleasePlayerNameHandler = handler;
    AsyncDataServiceBatchGetPlayerNameHandler = handler;
}

void SetDataServiceIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncDataServiceLoadPlayerDataHandler) {
        AsyncDataServiceLoadPlayerDataHandler = handler;
    }
    if (!AsyncDataServiceSavePlayerDataHandler) {
        AsyncDataServiceSavePlayerDataHandler = handler;
    }
    if (!AsyncDataServiceGetPlayerFieldHandler) {
        AsyncDataServiceGetPlayerFieldHandler = handler;
    }
    if (!AsyncDataServiceSetPlayerFieldHandler) {
        AsyncDataServiceSetPlayerFieldHandler = handler;
    }
    if (!AsyncDataServiceRegisterPlayerZoneHandler) {
        AsyncDataServiceRegisterPlayerZoneHandler = handler;
    }
    if (!AsyncDataServiceGetPlayerHomeZoneHandler) {
        AsyncDataServiceGetPlayerHomeZoneHandler = handler;
    }
    if (!AsyncDataServiceBatchGetPlayerHomeZoneHandler) {
        AsyncDataServiceBatchGetPlayerHomeZoneHandler = handler;
    }
    if (!AsyncDataServiceRemapHomeZoneForMergeHandler) {
        AsyncDataServiceRemapHomeZoneForMergeHandler = handler;
    }
    if (!AsyncDataServiceDeletePlayerDataHandler) {
        AsyncDataServiceDeletePlayerDataHandler = handler;
    }
    if (!AsyncDataServiceCreatePlayerSnapshotHandler) {
        AsyncDataServiceCreatePlayerSnapshotHandler = handler;
    }
    if (!AsyncDataServiceListPlayerSnapshotsHandler) {
        AsyncDataServiceListPlayerSnapshotsHandler = handler;
    }
    if (!AsyncDataServiceGetPlayerSnapshotDiffHandler) {
        AsyncDataServiceGetPlayerSnapshotDiffHandler = handler;
    }
    if (!AsyncDataServiceRollbackPlayerHandler) {
        AsyncDataServiceRollbackPlayerHandler = handler;
    }
    if (!AsyncDataServiceRollbackZoneHandler) {
        AsyncDataServiceRollbackZoneHandler = handler;
    }
    if (!AsyncDataServiceRollbackAllHandler) {
        AsyncDataServiceRollbackAllHandler = handler;
    }
    if (!AsyncDataServiceBatchRecallItemsHandler) {
        AsyncDataServiceBatchRecallItemsHandler = handler;
    }
    if (!AsyncDataServiceQueryTransactionLogHandler) {
        AsyncDataServiceQueryTransactionLogHandler = handler;
    }
    if (!AsyncDataServiceCreateEventSnapshotHandler) {
        AsyncDataServiceCreateEventSnapshotHandler = handler;
    }
    if (!AsyncDataServiceAllocateIdSegmentHandler) {
        AsyncDataServiceAllocateIdSegmentHandler = handler;
    }
    if (!AsyncDataServiceReservePlayerNameHandler) {
        AsyncDataServiceReservePlayerNameHandler = handler;
    }
    if (!AsyncDataServiceReleasePlayerNameHandler) {
        AsyncDataServiceReleasePlayerNameHandler = handler;
    }
    if (!AsyncDataServiceBatchGetPlayerNameHandler) {
        AsyncDataServiceBatchGetPlayerNameHandler = handler;
    }
}

void SetDataServiceFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    AsyncDataServiceLoadPlayerDataFailedHandler = handler;
    AsyncDataServiceSavePlayerDataFailedHandler = handler;
    AsyncDataServiceGetPlayerFieldFailedHandler = handler;
    AsyncDataServiceSetPlayerFieldFailedHandler = handler;
    AsyncDataServiceRegisterPlayerZoneFailedHandler = handler;
    AsyncDataServiceGetPlayerHomeZoneFailedHandler = handler;
    AsyncDataServiceBatchGetPlayerHomeZoneFailedHandler = handler;
    AsyncDataServiceRemapHomeZoneForMergeFailedHandler = handler;
    AsyncDataServiceDeletePlayerDataFailedHandler = handler;
    AsyncDataServiceCreatePlayerSnapshotFailedHandler = handler;
    AsyncDataServiceListPlayerSnapshotsFailedHandler = handler;
    AsyncDataServiceGetPlayerSnapshotDiffFailedHandler = handler;
    AsyncDataServiceRollbackPlayerFailedHandler = handler;
    AsyncDataServiceRollbackZoneFailedHandler = handler;
    AsyncDataServiceRollbackAllFailedHandler = handler;
    AsyncDataServiceBatchRecallItemsFailedHandler = handler;
    AsyncDataServiceQueryTransactionLogFailedHandler = handler;
    AsyncDataServiceCreateEventSnapshotFailedHandler = handler;
    AsyncDataServiceAllocateIdSegmentFailedHandler = handler;
    AsyncDataServiceReservePlayerNameFailedHandler = handler;
    AsyncDataServiceReleasePlayerNameFailedHandler = handler;
    AsyncDataServiceBatchGetPlayerNameFailedHandler = handler;
}

void SetDataServiceIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    if (!AsyncDataServiceLoadPlayerDataFailedHandler) {
        AsyncDataServiceLoadPlayerDataFailedHandler = handler;
    }
    if (!AsyncDataServiceSavePlayerDataFailedHandler) {
        AsyncDataServiceSavePlayerDataFailedHandler = handler;
    }
    if (!AsyncDataServiceGetPlayerFieldFailedHandler) {
        AsyncDataServiceGetPlayerFieldFailedHandler = handler;
    }
    if (!AsyncDataServiceSetPlayerFieldFailedHandler) {
        AsyncDataServiceSetPlayerFieldFailedHandler = handler;
    }
    if (!AsyncDataServiceRegisterPlayerZoneFailedHandler) {
        AsyncDataServiceRegisterPlayerZoneFailedHandler = handler;
    }
    if (!AsyncDataServiceGetPlayerHomeZoneFailedHandler) {
        AsyncDataServiceGetPlayerHomeZoneFailedHandler = handler;
    }
    if (!AsyncDataServiceBatchGetPlayerHomeZoneFailedHandler) {
        AsyncDataServiceBatchGetPlayerHomeZoneFailedHandler = handler;
    }
    if (!AsyncDataServiceRemapHomeZoneForMergeFailedHandler) {
        AsyncDataServiceRemapHomeZoneForMergeFailedHandler = handler;
    }
    if (!AsyncDataServiceDeletePlayerDataFailedHandler) {
        AsyncDataServiceDeletePlayerDataFailedHandler = handler;
    }
    if (!AsyncDataServiceCreatePlayerSnapshotFailedHandler) {
        AsyncDataServiceCreatePlayerSnapshotFailedHandler = handler;
    }
    if (!AsyncDataServiceListPlayerSnapshotsFailedHandler) {
        AsyncDataServiceListPlayerSnapshotsFailedHandler = handler;
    }
    if (!AsyncDataServiceGetPlayerSnapshotDiffFailedHandler) {
        AsyncDataServiceGetPlayerSnapshotDiffFailedHandler = handler;
    }
    if (!AsyncDataServiceRollbackPlayerFailedHandler) {
        AsyncDataServiceRollbackPlayerFailedHandler = handler;
    }
    if (!AsyncDataServiceRollbackZoneFailedHandler) {
        AsyncDataServiceRollbackZoneFailedHandler = handler;
    }
    if (!AsyncDataServiceRollbackAllFailedHandler) {
        AsyncDataServiceRollbackAllFailedHandler = handler;
    }
    if (!AsyncDataServiceBatchRecallItemsFailedHandler) {
        AsyncDataServiceBatchRecallItemsFailedHandler = handler;
    }
    if (!AsyncDataServiceQueryTransactionLogFailedHandler) {
        AsyncDataServiceQueryTransactionLogFailedHandler = handler;
    }
    if (!AsyncDataServiceCreateEventSnapshotFailedHandler) {
        AsyncDataServiceCreateEventSnapshotFailedHandler = handler;
    }
    if (!AsyncDataServiceAllocateIdSegmentFailedHandler) {
        AsyncDataServiceAllocateIdSegmentFailedHandler = handler;
    }
    if (!AsyncDataServiceReservePlayerNameFailedHandler) {
        AsyncDataServiceReservePlayerNameFailedHandler = handler;
    }
    if (!AsyncDataServiceReleasePlayerNameFailedHandler) {
        AsyncDataServiceReleasePlayerNameFailedHandler = handler;
    }
    if (!AsyncDataServiceBatchGetPlayerNameFailedHandler) {
        AsyncDataServiceBatchGetPlayerNameFailedHandler = handler;
    }
}

void SetDataServiceCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

void InitDataServiceGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<DataServiceStubPtr>(nodeEntity, DataService::NewStub(channel));

}

}// namespace data_service
