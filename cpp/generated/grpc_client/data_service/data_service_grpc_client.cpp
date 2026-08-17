#include "muduo/base/Logging.h"

#include "data_service_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
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

void AsyncCompleteGrpcDataServiceLoadPlayerData(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceLoadPlayerDataGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceLoadPlayerDataHandler) {
            AsyncDataServiceLoadPlayerDataHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceLoadPlayerDataPool.destroy(call);
}

void SendDataServiceLoadPlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::LoadPlayerDataRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceLoadPlayerDataPool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncLoadPlayerData(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceLoadPlayerDataMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceLoadPlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::LoadPlayerDataRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceLoadPlayerDataPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncLoadPlayerData(&call->context, request,
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

void AsyncCompleteGrpcDataServiceSavePlayerData(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceSavePlayerDataGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceSavePlayerDataHandler) {
            AsyncDataServiceSavePlayerDataHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceSavePlayerDataPool.destroy(call);
}

void SendDataServiceSavePlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::SavePlayerDataRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceSavePlayerDataPool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncSavePlayerData(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceSavePlayerDataMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceSavePlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::SavePlayerDataRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceSavePlayerDataPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncSavePlayerData(&call->context, request,
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

void AsyncCompleteGrpcDataServiceGetPlayerField(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceGetPlayerFieldGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceGetPlayerFieldHandler) {
            AsyncDataServiceGetPlayerFieldHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceGetPlayerFieldPool.destroy(call);
}

void SendDataServiceGetPlayerField(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerFieldRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceGetPlayerFieldPool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetPlayerField(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceGetPlayerFieldMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceGetPlayerField(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerFieldRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceGetPlayerFieldPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetPlayerField(&call->context, request,
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

void AsyncCompleteGrpcDataServiceSetPlayerField(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceSetPlayerFieldGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceSetPlayerFieldHandler) {
            AsyncDataServiceSetPlayerFieldHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceSetPlayerFieldPool.destroy(call);
}

void SendDataServiceSetPlayerField(entt::registry& registry, entt::entity nodeEntity, const ::data_service::SetPlayerFieldRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceSetPlayerFieldPool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncSetPlayerField(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceSetPlayerFieldMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceSetPlayerField(entt::registry& registry, entt::entity nodeEntity, const ::data_service::SetPlayerFieldRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceSetPlayerFieldPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncSetPlayerField(&call->context, request,
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

void AsyncCompleteGrpcDataServiceRegisterPlayerZone(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceRegisterPlayerZoneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceRegisterPlayerZoneHandler) {
            AsyncDataServiceRegisterPlayerZoneHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceRegisterPlayerZonePool.destroy(call);
}

void SendDataServiceRegisterPlayerZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RegisterPlayerZoneRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceRegisterPlayerZonePool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRegisterPlayerZone(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceRegisterPlayerZoneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceRegisterPlayerZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RegisterPlayerZoneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceRegisterPlayerZonePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRegisterPlayerZone(&call->context, request,
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

void AsyncCompleteGrpcDataServiceGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceGetPlayerHomeZoneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceGetPlayerHomeZoneHandler) {
            AsyncDataServiceGetPlayerHomeZoneHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceGetPlayerHomeZonePool.destroy(call);
}

void SendDataServiceGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerHomeZoneRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceGetPlayerHomeZonePool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetPlayerHomeZone(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceGetPlayerHomeZoneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerHomeZoneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceGetPlayerHomeZonePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetPlayerHomeZone(&call->context, request,
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

void AsyncCompleteGrpcDataServiceBatchGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceBatchGetPlayerHomeZoneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceBatchGetPlayerHomeZoneHandler) {
            AsyncDataServiceBatchGetPlayerHomeZoneHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceBatchGetPlayerHomeZonePool.destroy(call);
}

void SendDataServiceBatchGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerHomeZoneRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceBatchGetPlayerHomeZonePool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncBatchGetPlayerHomeZone(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceBatchGetPlayerHomeZoneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceBatchGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerHomeZoneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceBatchGetPlayerHomeZonePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncBatchGetPlayerHomeZone(&call->context, request,
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

void AsyncCompleteGrpcDataServiceRemapHomeZoneForMerge(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceRemapHomeZoneForMergeGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceRemapHomeZoneForMergeHandler) {
            AsyncDataServiceRemapHomeZoneForMergeHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceRemapHomeZoneForMergePool.destroy(call);
}

void SendDataServiceRemapHomeZoneForMerge(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RemapHomeZoneForMergeRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceRemapHomeZoneForMergePool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRemapHomeZoneForMerge(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceRemapHomeZoneForMergeMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceRemapHomeZoneForMerge(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RemapHomeZoneForMergeRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceRemapHomeZoneForMergePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRemapHomeZoneForMerge(&call->context, request,
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

void AsyncCompleteGrpcDataServiceDeletePlayerData(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceDeletePlayerDataGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceDeletePlayerDataHandler) {
            AsyncDataServiceDeletePlayerDataHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceDeletePlayerDataPool.destroy(call);
}

void SendDataServiceDeletePlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::DeletePlayerDataRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceDeletePlayerDataPool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncDeletePlayerData(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceDeletePlayerDataMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceDeletePlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::DeletePlayerDataRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceDeletePlayerDataPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncDeletePlayerData(&call->context, request,
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

void AsyncCompleteGrpcDataServiceCreatePlayerSnapshot(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceCreatePlayerSnapshotGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceCreatePlayerSnapshotHandler) {
            AsyncDataServiceCreatePlayerSnapshotHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceCreatePlayerSnapshotPool.destroy(call);
}

void SendDataServiceCreatePlayerSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CreatePlayerSnapshotRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceCreatePlayerSnapshotPool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncCreatePlayerSnapshot(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceCreatePlayerSnapshotMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceCreatePlayerSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CreatePlayerSnapshotRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceCreatePlayerSnapshotPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncCreatePlayerSnapshot(&call->context, request,
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

void AsyncCompleteGrpcDataServiceListPlayerSnapshots(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceListPlayerSnapshotsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceListPlayerSnapshotsHandler) {
            AsyncDataServiceListPlayerSnapshotsHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceListPlayerSnapshotsPool.destroy(call);
}

void SendDataServiceListPlayerSnapshots(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ListPlayerSnapshotsRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceListPlayerSnapshotsPool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncListPlayerSnapshots(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceListPlayerSnapshotsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceListPlayerSnapshots(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ListPlayerSnapshotsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceListPlayerSnapshotsPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncListPlayerSnapshots(&call->context, request,
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

void AsyncCompleteGrpcDataServiceGetPlayerSnapshotDiff(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceGetPlayerSnapshotDiffGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceGetPlayerSnapshotDiffHandler) {
            AsyncDataServiceGetPlayerSnapshotDiffHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceGetPlayerSnapshotDiffPool.destroy(call);
}

void SendDataServiceGetPlayerSnapshotDiff(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerSnapshotDiffRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceGetPlayerSnapshotDiffPool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetPlayerSnapshotDiff(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceGetPlayerSnapshotDiffMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceGetPlayerSnapshotDiff(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerSnapshotDiffRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceGetPlayerSnapshotDiffPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncGetPlayerSnapshotDiff(&call->context, request,
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

void AsyncCompleteGrpcDataServiceRollbackPlayer(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceRollbackPlayerGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceRollbackPlayerHandler) {
            AsyncDataServiceRollbackPlayerHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceRollbackPlayerPool.destroy(call);
}

void SendDataServiceRollbackPlayer(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackPlayerRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceRollbackPlayerPool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRollbackPlayer(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceRollbackPlayerMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceRollbackPlayer(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackPlayerRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceRollbackPlayerPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRollbackPlayer(&call->context, request,
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

void AsyncCompleteGrpcDataServiceRollbackZone(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceRollbackZoneGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceRollbackZoneHandler) {
            AsyncDataServiceRollbackZoneHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceRollbackZonePool.destroy(call);
}

void SendDataServiceRollbackZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackZoneRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceRollbackZonePool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRollbackZone(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceRollbackZoneMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceRollbackZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackZoneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceRollbackZonePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRollbackZone(&call->context, request,
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

void AsyncCompleteGrpcDataServiceRollbackAll(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceRollbackAllGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceRollbackAllHandler) {
            AsyncDataServiceRollbackAllHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceRollbackAllPool.destroy(call);
}

void SendDataServiceRollbackAll(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackAllRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceRollbackAllPool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRollbackAll(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceRollbackAllMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceRollbackAll(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackAllRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceRollbackAllPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncRollbackAll(&call->context, request,
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

void AsyncCompleteGrpcDataServiceBatchRecallItems(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceBatchRecallItemsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceBatchRecallItemsHandler) {
            AsyncDataServiceBatchRecallItemsHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceBatchRecallItemsPool.destroy(call);
}

void SendDataServiceBatchRecallItems(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchRecallItemsRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceBatchRecallItemsPool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncBatchRecallItems(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceBatchRecallItemsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceBatchRecallItems(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchRecallItemsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceBatchRecallItemsPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncBatchRecallItems(&call->context, request,
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

void AsyncCompleteGrpcDataServiceQueryTransactionLog(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceQueryTransactionLogGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceQueryTransactionLogHandler) {
            AsyncDataServiceQueryTransactionLogHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceQueryTransactionLogPool.destroy(call);
}

void SendDataServiceQueryTransactionLog(entt::registry& registry, entt::entity nodeEntity, const ::data_service::QueryTransactionLogRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceQueryTransactionLogPool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncQueryTransactionLog(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceQueryTransactionLogMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceQueryTransactionLog(entt::registry& registry, entt::entity nodeEntity, const ::data_service::QueryTransactionLogRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceQueryTransactionLogPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncQueryTransactionLog(&call->context, request,
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

void AsyncCompleteGrpcDataServiceCreateEventSnapshot(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncDataServiceCreateEventSnapshotGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncDataServiceCreateEventSnapshotHandler) {
            AsyncDataServiceCreateEventSnapshotHandler(call->context, call->reply);
        }
    } else {
        LOG_ERROR << call->status.error_message();
    }

	DataServiceCreateEventSnapshotPool.destroy(call);
}

void SendDataServiceCreateEventSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CreateEventSnapshotRequest& request) {

    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);
    auto call(DataServiceCreateEventSnapshotPool.construct());
    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncCreateEventSnapshot(&call->context, request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(DataServiceCreateEventSnapshotMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendDataServiceCreateEventSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CreateEventSnapshotRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(DataServiceCreateEventSnapshotPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
    }

    call->response_reader = registry
        .get<DataServiceStubPtr>(nodeEntity)
        ->PrepareAsyncCreateEventSnapshot(&call->context, request,
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
}

void InitDataServiceGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<DataServiceStubPtr>(nodeEntity, DataService::NewStub(channel));

}

}// namespace data_service
