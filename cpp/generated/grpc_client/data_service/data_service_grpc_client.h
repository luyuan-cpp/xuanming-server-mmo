#pragma once
#include <chrono>
#include <string_view>
#include "entt/src/entt/entity/registry.hpp"
#include "core/network/grpc_client_call.h"

#include "rpc/service_metadata/data_service_service_metadata.h"

namespace data_service {
#pragma region DataServiceReadAccountDirectory
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceReadAccountDirectoryGrpcPath = "/data_service.DataService/ReadAccountDirectory";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceReadAccountDirectory(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReadAccountDirectoryRequest& request, GrpcDone<::data_service::AccountDirectoryResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceReadAccountDirectoryErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceCreateAccountRole
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceCreateAccountRoleGrpcPath = "/data_service.DataService/CreateAccountRole";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceCreateAccountRole(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CreateAccountRoleRequest& request, GrpcDone<::data_service::AccountDirectoryResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceCreateAccountRoleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceLookupAccountRole
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceLookupAccountRoleGrpcPath = "/data_service.DataService/LookupAccountRole";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceLookupAccountRole(entt::registry& registry, entt::entity nodeEntity, const ::data_service::LookupAccountRoleRequest& request, GrpcDone<::data_service::AccountRoleOwnership> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceLookupAccountRoleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceRepairAccountRole
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceRepairAccountRoleGrpcPath = "/data_service.DataService/RepairAccountRole";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceRepairAccountRole(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RepairAccountRoleRequest& request, GrpcDone<::data_service::AccountDirectoryResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceRepairAccountRoleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceRemoveAccountRoles
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceRemoveAccountRolesGrpcPath = "/data_service.DataService/RemoveAccountRoles";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceRemoveAccountRoles(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RemoveAccountRolesRequest& request, GrpcDone<::google::protobuf::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceRemoveAccountRolesErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceBeginAccountRoleTrade
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceBeginAccountRoleTradeGrpcPath = "/data_service.DataService/BeginAccountRoleTrade";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceBeginAccountRoleTrade(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BeginAccountRoleTradeRequest& request, GrpcDone<::data_service::AccountRoleTradeReceipt> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceBeginAccountRoleTradeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceConfirmAccountRoleTradeFreeze
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceConfirmAccountRoleTradeFreezeGrpcPath = "/data_service.DataService/ConfirmAccountRoleTradeFreeze";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceConfirmAccountRoleTradeFreeze(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ConfirmAccountRoleTradeFreezeRequest& request, GrpcDone<::data_service::AccountRoleTradeReceipt> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceConfirmAccountRoleTradeFreezeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceCancelAccountRoleTrade
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceCancelAccountRoleTradeGrpcPath = "/data_service.DataService/CancelAccountRoleTrade";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceCancelAccountRoleTrade(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CancelAccountRoleTradeRequest& request, GrpcDone<::data_service::AccountRoleTradeReceipt> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceCancelAccountRoleTradeErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceTransferAccountRole
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceTransferAccountRoleGrpcPath = "/data_service.DataService/TransferAccountRole";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceTransferAccountRole(entt::registry& registry, entt::entity nodeEntity, const ::data_service::TransferAccountRoleRequest& request, GrpcDone<::data_service::AccountRoleTransferReceipt> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceTransferAccountRoleErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceRegisterRoleAdmission
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceRegisterRoleAdmissionGrpcPath = "/data_service.DataService/RegisterRoleAdmission";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceRegisterRoleAdmission(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RegisterRoleAdmissionRequest& request, GrpcDone<::data_service::RegisterRoleAdmissionResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceRegisterRoleAdmissionErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceCloseRoleAdmissions
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceCloseRoleAdmissionsGrpcPath = "/data_service.DataService/CloseRoleAdmissions";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceCloseRoleAdmissions(entt::registry& registry, entt::entity nodeEntity, const ::data_service::CloseRoleAdmissionsRequest& request, GrpcDone<::data_service::CloseRoleAdmissionsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceCloseRoleAdmissionsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceAckRoleTradeDrain
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceAckRoleTradeDrainGrpcPath = "/data_service.DataService/AckRoleTradeDrain";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceAckRoleTradeDrain(entt::registry& registry, entt::entity nodeEntity, const ::data_service::AckRoleTradeDrainRequest& request, GrpcDone<::google::protobuf::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceAckRoleTradeDrainErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceReleaseDeadPlayerStoreHolder
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceReleaseDeadPlayerStoreHolderGrpcPath = "/data_service.DataService/ReleaseDeadPlayerStoreHolder";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceReleaseDeadPlayerStoreHolder(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReleaseDeadPlayerStoreHolderRequest& request, GrpcDone<::google::protobuf::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceReleaseDeadPlayerStoreHolderErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceRegisterPlayerZone
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceRegisterPlayerZoneGrpcPath = "/data_service.DataService/RegisterPlayerZone";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceRegisterPlayerZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::RegisterPlayerZoneRequest& request, GrpcDone<::google::protobuf::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceRegisterPlayerZoneErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceGetPlayerHomeZone
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceGetPlayerHomeZoneGrpcPath = "/data_service.DataService/GetPlayerHomeZone";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerHomeZoneRequest& request, GrpcDone<::data_service::GetPlayerHomeZoneResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceGetPlayerHomeZoneErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceBatchGetPlayerHomeZone
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceBatchGetPlayerHomeZoneGrpcPath = "/data_service.DataService/BatchGetPlayerHomeZone";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceBatchGetPlayerHomeZone(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerHomeZoneRequest& request, GrpcDone<::data_service::BatchGetPlayerHomeZoneResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceBatchGetPlayerHomeZoneErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceGetMergedZones
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceGetMergedZonesGrpcPath = "/data_service.DataService/GetMergedZones";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceGetMergedZones(entt::registry& registry, entt::entity nodeEntity, const ::google::protobuf::Empty& request, GrpcDone<::data_service::GetMergedZonesResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceGetMergedZonesErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceListPlayerSnapshots
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceListPlayerSnapshotsGrpcPath = "/data_service.DataService/ListPlayerSnapshots";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceListPlayerSnapshots(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ListPlayerSnapshotsRequest& request, GrpcDone<::data_service::ListPlayerSnapshotsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceListPlayerSnapshotsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceBatchRecallItems
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceBatchRecallItemsGrpcPath = "/data_service.DataService/BatchRecallItems";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceBatchRecallItems(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchRecallItemsRequest& request, GrpcDone<::data_service::BatchRecallItemsResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceBatchRecallItemsErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceQueryTransactionLog
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceQueryTransactionLogGrpcPath = "/data_service.DataService/QueryTransactionLog";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceQueryTransactionLog(entt::registry& registry, entt::entity nodeEntity, const ::data_service::QueryTransactionLogRequest& request, GrpcDone<::data_service::QueryTransactionLogResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceQueryTransactionLogErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceAllocateIdSegment
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceAllocateIdSegmentGrpcPath = "/data_service.DataService/AllocateIdSegment";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceAllocateIdSegment(entt::registry& registry, entt::entity nodeEntity, const ::data_service::AllocateIdSegmentRequest& request, GrpcDone<::data_service::AllocateIdSegmentResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceAllocateIdSegmentErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceReservePlayerName
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceReservePlayerNameGrpcPath = "/data_service.DataService/ReservePlayerName";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceReservePlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReservePlayerNameRequest& request, GrpcDone<::data_service::ReservePlayerNameResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceReservePlayerNameErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceReleasePlayerName
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceReleasePlayerNameGrpcPath = "/data_service.DataService/ReleasePlayerName";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceReleasePlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::ReleasePlayerNameRequest& request, GrpcDone<::google::protobuf::Empty> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceReleasePlayerNameErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceBatchGetPlayerName
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceBatchGetPlayerNameGrpcPath = "/data_service.DataService/BatchGetPlayerName";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceBatchGetPlayerName(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerNameRequest& request, GrpcDone<::data_service::BatchGetPlayerNameResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceBatchGetPlayerNameErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceBatchGetPlayerDisplay
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceBatchGetPlayerDisplayGrpcPath = "/data_service.DataService/BatchGetPlayerDisplay";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceBatchGetPlayerDisplay(entt::registry& registry, entt::entity nodeEntity, const ::data_service::BatchGetPlayerDisplayRequest& request, GrpcDone<::data_service::BatchGetPlayerDisplayResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceBatchGetPlayerDisplayErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceGetPlayerAssetOpLedger
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceGetPlayerAssetOpLedgerGrpcPath = "/data_service.DataService/GetPlayerAssetOpLedger";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceGetPlayerAssetOpLedger(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerAssetOpLedgerRequest& request, GrpcDone<::data_service::GetPlayerAssetOpLedgerResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceGetPlayerAssetOpLedgerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
#pragma region DataServiceGetPlayerInboxLedger
// gRPC :path(线上契约,与 Go 侧一致)。
inline constexpr std::string_view kDataServiceGetPlayerInboxLedgerGrpcPath = "/data_service.DataService/GetPlayerInboxLedger";
// done 必填且恰好调用一次,从不在 Send 栈内同步调用;为空 LOG_FATAL。响应只在回调期间有效。
void SendDataServiceGetPlayerInboxLedger(entt::registry& registry, entt::entity nodeEntity, const ::data_service::GetPlayerInboxLedgerRequest& request, GrpcDone<::data_service::GetPlayerInboxLedgerResponse> done, const GrpcCallOptions& options = {});
// 类型擦除版只供元数据 sender 使用;动态类型不匹配时异步回 INTERNAL,不发出请求。
void SendDataServiceGetPlayerInboxLedgerErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);
#pragma endregion
// 本文件所有 unary 调用此后使用的 deadline;启动时按目标节点类型设置。
void SetDataServiceCallDeadline(std::chrono::milliseconds deadline);

}// namespace data_service
