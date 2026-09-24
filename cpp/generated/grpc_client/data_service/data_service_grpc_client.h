#pragma once
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/data_service/data_service.grpc.pb.h"

#include "rpc/service_metadata/data_service_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

namespace data_service {
using DataServiceStubPtr = std::unique_ptr<DataService::Stub>;
#pragma region DataServiceLoadPlayerData

struct AsyncDataServiceLoadPlayerDataGrpcClient {
    uint32_t messageId{ DataServiceLoadPlayerDataMessageId };
    ClientContext context;
    Status status;
    ::data_service::LoadPlayerDataResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::LoadPlayerDataResponse>> response_reader;
};

using AsyncDataServiceLoadPlayerDataHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::LoadPlayerDataResponse&)>;
extern AsyncDataServiceLoadPlayerDataHandlerFunctionType AsyncDataServiceLoadPlayerDataHandler;

void SendDataServiceLoadPlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::LoadPlayerDataRequest& request);
void SendDataServiceLoadPlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::LoadPlayerDataRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceLoadPlayerData(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceSavePlayerData

struct AsyncDataServiceSavePlayerDataGrpcClient {
    uint32_t messageId{ DataServiceSavePlayerDataMessageId };
    ClientContext context;
    Status status;
    ::data_service::SavePlayerDataResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::SavePlayerDataResponse>> response_reader;
};

using AsyncDataServiceSavePlayerDataHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::SavePlayerDataResponse&)>;
extern AsyncDataServiceSavePlayerDataHandlerFunctionType AsyncDataServiceSavePlayerDataHandler;

void SendDataServiceSavePlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::SavePlayerDataRequest& request);
void SendDataServiceSavePlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::SavePlayerDataRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceSavePlayerData(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceGetPlayerField

struct AsyncDataServiceGetPlayerFieldGrpcClient {
    uint32_t messageId{ DataServiceGetPlayerFieldMessageId };
    ClientContext context;
    Status status;
    ::data_service::GetPlayerFieldResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::GetPlayerFieldResponse>> response_reader;
};

using AsyncDataServiceGetPlayerFieldHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::GetPlayerFieldResponse&)>;
extern AsyncDataServiceGetPlayerFieldHandlerFunctionType AsyncDataServiceGetPlayerFieldHandler;

void SendDataServiceGetPlayerField(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerFieldRequest& request);
void SendDataServiceGetPlayerField(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerFieldRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceGetPlayerField(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceSetPlayerField

struct AsyncDataServiceSetPlayerFieldGrpcClient {
    uint32_t messageId{ DataServiceSetPlayerFieldMessageId };
    ClientContext context;
    Status status;
    ::data_service::SetPlayerFieldResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::SetPlayerFieldResponse>> response_reader;
};

using AsyncDataServiceSetPlayerFieldHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::SetPlayerFieldResponse&)>;
extern AsyncDataServiceSetPlayerFieldHandlerFunctionType AsyncDataServiceSetPlayerFieldHandler;

void SendDataServiceSetPlayerField(entt::registry& registry, entt::entity nodeEntity, const ::data_service::SetPlayerFieldRequest& request);
void SendDataServiceSetPlayerField(entt::registry& registry, entt::entity nodeEntity, const ::data_service::SetPlayerFieldRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceSetPlayerField(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceRegisterPlayerZone

struct AsyncDataServiceRegisterPlayerZoneGrpcClient {
    uint32_t messageId{ DataServiceRegisterPlayerZoneMessageId };
    ClientContext context;
    Status status;
    ::google::protobuf::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::google::protobuf::Empty>> response_reader;
};

using AsyncDataServiceRegisterPlayerZoneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::google::protobuf::Empty&)>;
extern AsyncDataServiceRegisterPlayerZoneHandlerFunctionType AsyncDataServiceRegisterPlayerZoneHandler;

void SendDataServiceRegisterPlayerZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RegisterPlayerZoneRequest& request);
void SendDataServiceRegisterPlayerZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RegisterPlayerZoneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceRegisterPlayerZone(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceGetPlayerHomeZone

struct AsyncDataServiceGetPlayerHomeZoneGrpcClient {
    uint32_t messageId{ DataServiceGetPlayerHomeZoneMessageId };
    ClientContext context;
    Status status;
    ::data_service::GetPlayerHomeZoneResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::GetPlayerHomeZoneResponse>> response_reader;
};

using AsyncDataServiceGetPlayerHomeZoneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::GetPlayerHomeZoneResponse&)>;
extern AsyncDataServiceGetPlayerHomeZoneHandlerFunctionType AsyncDataServiceGetPlayerHomeZoneHandler;

void SendDataServiceGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerHomeZoneRequest& request);
void SendDataServiceGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerHomeZoneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceBatchGetPlayerHomeZone

struct AsyncDataServiceBatchGetPlayerHomeZoneGrpcClient {
    uint32_t messageId{ DataServiceBatchGetPlayerHomeZoneMessageId };
    ClientContext context;
    Status status;
    ::data_service::BatchGetPlayerHomeZoneResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::BatchGetPlayerHomeZoneResponse>> response_reader;
};

using AsyncDataServiceBatchGetPlayerHomeZoneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::BatchGetPlayerHomeZoneResponse&)>;
extern AsyncDataServiceBatchGetPlayerHomeZoneHandlerFunctionType AsyncDataServiceBatchGetPlayerHomeZoneHandler;

void SendDataServiceBatchGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerHomeZoneRequest& request);
void SendDataServiceBatchGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerHomeZoneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceBatchGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceRemapHomeZoneForMerge

struct AsyncDataServiceRemapHomeZoneForMergeGrpcClient {
    uint32_t messageId{ DataServiceRemapHomeZoneForMergeMessageId };
    ClientContext context;
    Status status;
    ::data_service::RemapHomeZoneForMergeResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::RemapHomeZoneForMergeResponse>> response_reader;
};

using AsyncDataServiceRemapHomeZoneForMergeHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::RemapHomeZoneForMergeResponse&)>;
extern AsyncDataServiceRemapHomeZoneForMergeHandlerFunctionType AsyncDataServiceRemapHomeZoneForMergeHandler;

void SendDataServiceRemapHomeZoneForMerge(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RemapHomeZoneForMergeRequest& request);
void SendDataServiceRemapHomeZoneForMerge(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RemapHomeZoneForMergeRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceRemapHomeZoneForMerge(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceDeletePlayerData

struct AsyncDataServiceDeletePlayerDataGrpcClient {
    uint32_t messageId{ DataServiceDeletePlayerDataMessageId };
    ClientContext context;
    Status status;
    ::data_service::DeletePlayerDataResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::DeletePlayerDataResponse>> response_reader;
};

using AsyncDataServiceDeletePlayerDataHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::DeletePlayerDataResponse&)>;
extern AsyncDataServiceDeletePlayerDataHandlerFunctionType AsyncDataServiceDeletePlayerDataHandler;

void SendDataServiceDeletePlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::DeletePlayerDataRequest& request);
void SendDataServiceDeletePlayerData(entt::registry& registry, entt::entity nodeEntity, const ::data_service::DeletePlayerDataRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceDeletePlayerData(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceCreatePlayerSnapshot

struct AsyncDataServiceCreatePlayerSnapshotGrpcClient {
    uint32_t messageId{ DataServiceCreatePlayerSnapshotMessageId };
    ClientContext context;
    Status status;
    ::data_service::CreatePlayerSnapshotResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::CreatePlayerSnapshotResponse>> response_reader;
};

using AsyncDataServiceCreatePlayerSnapshotHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::CreatePlayerSnapshotResponse&)>;
extern AsyncDataServiceCreatePlayerSnapshotHandlerFunctionType AsyncDataServiceCreatePlayerSnapshotHandler;

void SendDataServiceCreatePlayerSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CreatePlayerSnapshotRequest& request);
void SendDataServiceCreatePlayerSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CreatePlayerSnapshotRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceCreatePlayerSnapshot(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceListPlayerSnapshots

struct AsyncDataServiceListPlayerSnapshotsGrpcClient {
    uint32_t messageId{ DataServiceListPlayerSnapshotsMessageId };
    ClientContext context;
    Status status;
    ::data_service::ListPlayerSnapshotsResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::ListPlayerSnapshotsResponse>> response_reader;
};

using AsyncDataServiceListPlayerSnapshotsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::ListPlayerSnapshotsResponse&)>;
extern AsyncDataServiceListPlayerSnapshotsHandlerFunctionType AsyncDataServiceListPlayerSnapshotsHandler;

void SendDataServiceListPlayerSnapshots(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ListPlayerSnapshotsRequest& request);
void SendDataServiceListPlayerSnapshots(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ListPlayerSnapshotsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceListPlayerSnapshots(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceGetPlayerSnapshotDiff

struct AsyncDataServiceGetPlayerSnapshotDiffGrpcClient {
    uint32_t messageId{ DataServiceGetPlayerSnapshotDiffMessageId };
    ClientContext context;
    Status status;
    ::data_service::GetPlayerSnapshotDiffResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::GetPlayerSnapshotDiffResponse>> response_reader;
};

using AsyncDataServiceGetPlayerSnapshotDiffHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::GetPlayerSnapshotDiffResponse&)>;
extern AsyncDataServiceGetPlayerSnapshotDiffHandlerFunctionType AsyncDataServiceGetPlayerSnapshotDiffHandler;

void SendDataServiceGetPlayerSnapshotDiff(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerSnapshotDiffRequest& request);
void SendDataServiceGetPlayerSnapshotDiff(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerSnapshotDiffRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceGetPlayerSnapshotDiff(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceRollbackPlayer

struct AsyncDataServiceRollbackPlayerGrpcClient {
    uint32_t messageId{ DataServiceRollbackPlayerMessageId };
    ClientContext context;
    Status status;
    ::data_service::RollbackPlayerResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::RollbackPlayerResponse>> response_reader;
};

using AsyncDataServiceRollbackPlayerHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::RollbackPlayerResponse&)>;
extern AsyncDataServiceRollbackPlayerHandlerFunctionType AsyncDataServiceRollbackPlayerHandler;

void SendDataServiceRollbackPlayer(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackPlayerRequest& request);
void SendDataServiceRollbackPlayer(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackPlayerRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceRollbackPlayer(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceRollbackZone

struct AsyncDataServiceRollbackZoneGrpcClient {
    uint32_t messageId{ DataServiceRollbackZoneMessageId };
    ClientContext context;
    Status status;
    ::data_service::RollbackZoneResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::RollbackZoneResponse>> response_reader;
};

using AsyncDataServiceRollbackZoneHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::RollbackZoneResponse&)>;
extern AsyncDataServiceRollbackZoneHandlerFunctionType AsyncDataServiceRollbackZoneHandler;

void SendDataServiceRollbackZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackZoneRequest& request);
void SendDataServiceRollbackZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackZoneRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceRollbackZone(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceRollbackAll

struct AsyncDataServiceRollbackAllGrpcClient {
    uint32_t messageId{ DataServiceRollbackAllMessageId };
    ClientContext context;
    Status status;
    ::data_service::RollbackAllResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::RollbackAllResponse>> response_reader;
};

using AsyncDataServiceRollbackAllHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::RollbackAllResponse&)>;
extern AsyncDataServiceRollbackAllHandlerFunctionType AsyncDataServiceRollbackAllHandler;

void SendDataServiceRollbackAll(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackAllRequest& request);
void SendDataServiceRollbackAll(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RollbackAllRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceRollbackAll(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceBatchRecallItems

struct AsyncDataServiceBatchRecallItemsGrpcClient {
    uint32_t messageId{ DataServiceBatchRecallItemsMessageId };
    ClientContext context;
    Status status;
    ::data_service::BatchRecallItemsResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::BatchRecallItemsResponse>> response_reader;
};

using AsyncDataServiceBatchRecallItemsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::BatchRecallItemsResponse&)>;
extern AsyncDataServiceBatchRecallItemsHandlerFunctionType AsyncDataServiceBatchRecallItemsHandler;

void SendDataServiceBatchRecallItems(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchRecallItemsRequest& request);
void SendDataServiceBatchRecallItems(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchRecallItemsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceBatchRecallItems(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceQueryTransactionLog

struct AsyncDataServiceQueryTransactionLogGrpcClient {
    uint32_t messageId{ DataServiceQueryTransactionLogMessageId };
    ClientContext context;
    Status status;
    ::data_service::QueryTransactionLogResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::QueryTransactionLogResponse>> response_reader;
};

using AsyncDataServiceQueryTransactionLogHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::QueryTransactionLogResponse&)>;
extern AsyncDataServiceQueryTransactionLogHandlerFunctionType AsyncDataServiceQueryTransactionLogHandler;

void SendDataServiceQueryTransactionLog(entt::registry& registry, entt::entity nodeEntity, const ::data_service::QueryTransactionLogRequest& request);
void SendDataServiceQueryTransactionLog(entt::registry& registry, entt::entity nodeEntity, const ::data_service::QueryTransactionLogRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceQueryTransactionLog(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceCreateEventSnapshot

struct AsyncDataServiceCreateEventSnapshotGrpcClient {
    uint32_t messageId{ DataServiceCreateEventSnapshotMessageId };
    ClientContext context;
    Status status;
    ::data_service::CreateEventSnapshotResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::CreateEventSnapshotResponse>> response_reader;
};

using AsyncDataServiceCreateEventSnapshotHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::CreateEventSnapshotResponse&)>;
extern AsyncDataServiceCreateEventSnapshotHandlerFunctionType AsyncDataServiceCreateEventSnapshotHandler;

void SendDataServiceCreateEventSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CreateEventSnapshotRequest& request);
void SendDataServiceCreateEventSnapshot(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CreateEventSnapshotRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceCreateEventSnapshot(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceAllocateIdSegment

struct AsyncDataServiceAllocateIdSegmentGrpcClient {
    uint32_t messageId{ DataServiceAllocateIdSegmentMessageId };
    ClientContext context;
    Status status;
    ::data_service::AllocateIdSegmentResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::AllocateIdSegmentResponse>> response_reader;
};

using AsyncDataServiceAllocateIdSegmentHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::AllocateIdSegmentResponse&)>;
extern AsyncDataServiceAllocateIdSegmentHandlerFunctionType AsyncDataServiceAllocateIdSegmentHandler;

void SendDataServiceAllocateIdSegment(entt::registry& registry, entt::entity nodeEntity, const ::data_service::AllocateIdSegmentRequest& request);
void SendDataServiceAllocateIdSegment(entt::registry& registry, entt::entity nodeEntity, const ::data_service::AllocateIdSegmentRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceAllocateIdSegment(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceReservePlayerName

struct AsyncDataServiceReservePlayerNameGrpcClient {
    uint32_t messageId{ DataServiceReservePlayerNameMessageId };
    ClientContext context;
    Status status;
    ::data_service::ReservePlayerNameResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::ReservePlayerNameResponse>> response_reader;
};

using AsyncDataServiceReservePlayerNameHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::ReservePlayerNameResponse&)>;
extern AsyncDataServiceReservePlayerNameHandlerFunctionType AsyncDataServiceReservePlayerNameHandler;

void SendDataServiceReservePlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReservePlayerNameRequest& request);
void SendDataServiceReservePlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReservePlayerNameRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceReservePlayerName(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceReleasePlayerName

struct AsyncDataServiceReleasePlayerNameGrpcClient {
    uint32_t messageId{ DataServiceReleasePlayerNameMessageId };
    ClientContext context;
    Status status;
    ::google::protobuf::Empty reply;
    std::unique_ptr<ClientAsyncResponseReader<::google::protobuf::Empty>> response_reader;
};

using AsyncDataServiceReleasePlayerNameHandlerFunctionType =
    std::function<void(const ClientContext&, const ::google::protobuf::Empty&)>;
extern AsyncDataServiceReleasePlayerNameHandlerFunctionType AsyncDataServiceReleasePlayerNameHandler;

void SendDataServiceReleasePlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReleasePlayerNameRequest& request);
void SendDataServiceReleasePlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReleasePlayerNameRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceReleasePlayerName(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
#pragma region DataServiceBatchGetPlayerName

struct AsyncDataServiceBatchGetPlayerNameGrpcClient {
    uint32_t messageId{ DataServiceBatchGetPlayerNameMessageId };
    ClientContext context;
    Status status;
    ::data_service::BatchGetPlayerNameResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::data_service::BatchGetPlayerNameResponse>> response_reader;
};

using AsyncDataServiceBatchGetPlayerNameHandlerFunctionType =
    std::function<void(const ClientContext&, const ::data_service::BatchGetPlayerNameResponse&)>;
extern AsyncDataServiceBatchGetPlayerNameHandlerFunctionType AsyncDataServiceBatchGetPlayerNameHandler;

void SendDataServiceBatchGetPlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerNameRequest& request);
void SendDataServiceBatchGetPlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerNameRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SendDataServiceBatchGetPlayerName(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetDataServiceHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetDataServiceIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void HandleDataServiceCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitDataServiceGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);

}// namespace data_service
