#pragma once
#include <cstdint>

#include "proto/data_service/data_service.pb.h"

constexpr uint32_t DataServiceReadAccountDirectoryMessageId = 318;
constexpr uint32_t DataServiceReadAccountDirectoryIndex = 0;
#define DataServiceReadAccountDirectoryMethod  ::DataService_Stub::descriptor()->method(0)

constexpr uint32_t DataServiceCreateAccountRoleMessageId = 319;
constexpr uint32_t DataServiceCreateAccountRoleIndex = 1;
#define DataServiceCreateAccountRoleMethod  ::DataService_Stub::descriptor()->method(1)

constexpr uint32_t DataServiceLookupAccountRoleMessageId = 320;
constexpr uint32_t DataServiceLookupAccountRoleIndex = 2;
#define DataServiceLookupAccountRoleMethod  ::DataService_Stub::descriptor()->method(2)

constexpr uint32_t DataServiceRepairAccountRoleMessageId = 321;
constexpr uint32_t DataServiceRepairAccountRoleIndex = 3;
#define DataServiceRepairAccountRoleMethod  ::DataService_Stub::descriptor()->method(3)

constexpr uint32_t DataServiceRemoveAccountRolesMessageId = 322;
constexpr uint32_t DataServiceRemoveAccountRolesIndex = 4;
#define DataServiceRemoveAccountRolesMethod  ::DataService_Stub::descriptor()->method(4)

constexpr uint32_t DataServiceBeginAccountRoleTradeMessageId = 323;
constexpr uint32_t DataServiceBeginAccountRoleTradeIndex = 5;
#define DataServiceBeginAccountRoleTradeMethod  ::DataService_Stub::descriptor()->method(5)

constexpr uint32_t DataServiceConfirmAccountRoleTradeFreezeMessageId = 324;
constexpr uint32_t DataServiceConfirmAccountRoleTradeFreezeIndex = 6;
#define DataServiceConfirmAccountRoleTradeFreezeMethod  ::DataService_Stub::descriptor()->method(6)

constexpr uint32_t DataServiceCancelAccountRoleTradeMessageId = 325;
constexpr uint32_t DataServiceCancelAccountRoleTradeIndex = 7;
#define DataServiceCancelAccountRoleTradeMethod  ::DataService_Stub::descriptor()->method(7)

constexpr uint32_t DataServiceTransferAccountRoleMessageId = 326;
constexpr uint32_t DataServiceTransferAccountRoleIndex = 8;
#define DataServiceTransferAccountRoleMethod  ::DataService_Stub::descriptor()->method(8)

constexpr uint32_t DataServiceRegisterRoleAdmissionMessageId = 327;
constexpr uint32_t DataServiceRegisterRoleAdmissionIndex = 9;
#define DataServiceRegisterRoleAdmissionMethod  ::DataService_Stub::descriptor()->method(9)

constexpr uint32_t DataServiceCloseRoleAdmissionsMessageId = 328;
constexpr uint32_t DataServiceCloseRoleAdmissionsIndex = 10;
#define DataServiceCloseRoleAdmissionsMethod  ::DataService_Stub::descriptor()->method(10)

constexpr uint32_t DataServiceAckRoleTradeDrainMessageId = 329;
constexpr uint32_t DataServiceAckRoleTradeDrainIndex = 11;
#define DataServiceAckRoleTradeDrainMethod  ::DataService_Stub::descriptor()->method(11)

constexpr uint32_t DataServiceReleaseDeadPlayerStoreHolderMessageId = 354;
constexpr uint32_t DataServiceReleaseDeadPlayerStoreHolderIndex = 12;
#define DataServiceReleaseDeadPlayerStoreHolderMethod  ::DataService_Stub::descriptor()->method(12)

constexpr uint32_t DataServiceRegisterPlayerZoneMessageId = 86;
constexpr uint32_t DataServiceRegisterPlayerZoneIndex = 13;
#define DataServiceRegisterPlayerZoneMethod  ::DataService_Stub::descriptor()->method(13)

constexpr uint32_t DataServiceGetPlayerHomeZoneMessageId = 93;
constexpr uint32_t DataServiceGetPlayerHomeZoneIndex = 14;
#define DataServiceGetPlayerHomeZoneMethod  ::DataService_Stub::descriptor()->method(14)

constexpr uint32_t DataServiceBatchGetPlayerHomeZoneMessageId = 87;
constexpr uint32_t DataServiceBatchGetPlayerHomeZoneIndex = 15;
#define DataServiceBatchGetPlayerHomeZoneMethod  ::DataService_Stub::descriptor()->method(15)

constexpr uint32_t DataServiceGetMergedZonesMessageId = 355;
constexpr uint32_t DataServiceGetMergedZonesIndex = 16;
#define DataServiceGetMergedZonesMethod  ::DataService_Stub::descriptor()->method(16)

constexpr uint32_t DataServiceListPlayerSnapshotsMessageId = 97;
constexpr uint32_t DataServiceListPlayerSnapshotsIndex = 17;
#define DataServiceListPlayerSnapshotsMethod  ::DataService_Stub::descriptor()->method(17)

constexpr uint32_t DataServiceBatchRecallItemsMessageId = 108;
constexpr uint32_t DataServiceBatchRecallItemsIndex = 18;
#define DataServiceBatchRecallItemsMethod  ::DataService_Stub::descriptor()->method(18)

constexpr uint32_t DataServiceQueryTransactionLogMessageId = 114;
constexpr uint32_t DataServiceQueryTransactionLogIndex = 19;
#define DataServiceQueryTransactionLogMethod  ::DataService_Stub::descriptor()->method(19)

constexpr uint32_t DataServiceAllocateIdSegmentMessageId = 180;
constexpr uint32_t DataServiceAllocateIdSegmentIndex = 20;
#define DataServiceAllocateIdSegmentMethod  ::DataService_Stub::descriptor()->method(20)

constexpr uint32_t DataServiceReservePlayerNameMessageId = 229;
constexpr uint32_t DataServiceReservePlayerNameIndex = 21;
#define DataServiceReservePlayerNameMethod  ::DataService_Stub::descriptor()->method(21)

constexpr uint32_t DataServiceReleasePlayerNameMessageId = 237;
constexpr uint32_t DataServiceReleasePlayerNameIndex = 22;
#define DataServiceReleasePlayerNameMethod  ::DataService_Stub::descriptor()->method(22)

constexpr uint32_t DataServiceBatchGetPlayerNameMessageId = 231;
constexpr uint32_t DataServiceBatchGetPlayerNameIndex = 23;
#define DataServiceBatchGetPlayerNameMethod  ::DataService_Stub::descriptor()->method(23)

constexpr uint32_t DataServiceBatchGetPlayerDisplayMessageId = 356;
constexpr uint32_t DataServiceBatchGetPlayerDisplayIndex = 24;
#define DataServiceBatchGetPlayerDisplayMethod  ::DataService_Stub::descriptor()->method(24)

constexpr uint32_t DataServiceGetPlayerAssetOpLedgerMessageId = 248;
constexpr uint32_t DataServiceGetPlayerAssetOpLedgerIndex = 25;
#define DataServiceGetPlayerAssetOpLedgerMethod  ::DataService_Stub::descriptor()->method(25)

constexpr uint32_t DataServiceGetPlayerInboxLedgerMessageId = 258;
constexpr uint32_t DataServiceGetPlayerInboxLedgerIndex = 26;
#define DataServiceGetPlayerInboxLedgerMethod  ::DataService_Stub::descriptor()->method(26)
