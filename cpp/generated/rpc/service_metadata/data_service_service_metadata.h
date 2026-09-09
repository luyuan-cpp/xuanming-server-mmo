#pragma once
#include <cstdint>

#include "proto/data_service/data_service.pb.h"

constexpr uint32_t DataServiceLoadPlayerDataMessageId = 90;
constexpr uint32_t DataServiceLoadPlayerDataIndex = 0;
#define DataServiceLoadPlayerDataMethod  ::DataService_Stub::descriptor()->method(0)

constexpr uint32_t DataServiceSavePlayerDataMessageId = 92;
constexpr uint32_t DataServiceSavePlayerDataIndex = 1;
#define DataServiceSavePlayerDataMethod  ::DataService_Stub::descriptor()->method(1)

constexpr uint32_t DataServiceGetPlayerFieldMessageId = 91;
constexpr uint32_t DataServiceGetPlayerFieldIndex = 2;
#define DataServiceGetPlayerFieldMethod  ::DataService_Stub::descriptor()->method(2)

constexpr uint32_t DataServiceSetPlayerFieldMessageId = 89;
constexpr uint32_t DataServiceSetPlayerFieldIndex = 3;
#define DataServiceSetPlayerFieldMethod  ::DataService_Stub::descriptor()->method(3)

constexpr uint32_t DataServiceRegisterPlayerZoneMessageId = 86;
constexpr uint32_t DataServiceRegisterPlayerZoneIndex = 4;
#define DataServiceRegisterPlayerZoneMethod  ::DataService_Stub::descriptor()->method(4)

constexpr uint32_t DataServiceGetPlayerHomeZoneMessageId = 93;
constexpr uint32_t DataServiceGetPlayerHomeZoneIndex = 5;
#define DataServiceGetPlayerHomeZoneMethod  ::DataService_Stub::descriptor()->method(5)

constexpr uint32_t DataServiceBatchGetPlayerHomeZoneMessageId = 87;
constexpr uint32_t DataServiceBatchGetPlayerHomeZoneIndex = 6;
#define DataServiceBatchGetPlayerHomeZoneMethod  ::DataService_Stub::descriptor()->method(6)

constexpr uint32_t DataServiceRemapHomeZoneForMergeMessageId = 129;
constexpr uint32_t DataServiceRemapHomeZoneForMergeIndex = 7;
#define DataServiceRemapHomeZoneForMergeMethod  ::DataService_Stub::descriptor()->method(7)

constexpr uint32_t DataServiceDeletePlayerDataMessageId = 88;
constexpr uint32_t DataServiceDeletePlayerDataIndex = 8;
#define DataServiceDeletePlayerDataMethod  ::DataService_Stub::descriptor()->method(8)

constexpr uint32_t DataServiceCreatePlayerSnapshotMessageId = 96;
constexpr uint32_t DataServiceCreatePlayerSnapshotIndex = 9;
#define DataServiceCreatePlayerSnapshotMethod  ::DataService_Stub::descriptor()->method(9)

constexpr uint32_t DataServiceListPlayerSnapshotsMessageId = 97;
constexpr uint32_t DataServiceListPlayerSnapshotsIndex = 10;
#define DataServiceListPlayerSnapshotsMethod  ::DataService_Stub::descriptor()->method(10)

constexpr uint32_t DataServiceGetPlayerSnapshotDiffMessageId = 98;
constexpr uint32_t DataServiceGetPlayerSnapshotDiffIndex = 11;
#define DataServiceGetPlayerSnapshotDiffMethod  ::DataService_Stub::descriptor()->method(11)

constexpr uint32_t DataServiceRollbackPlayerMessageId = 99;
constexpr uint32_t DataServiceRollbackPlayerIndex = 12;
#define DataServiceRollbackPlayerMethod  ::DataService_Stub::descriptor()->method(12)

constexpr uint32_t DataServiceRollbackZoneMessageId = 100;
constexpr uint32_t DataServiceRollbackZoneIndex = 13;
#define DataServiceRollbackZoneMethod  ::DataService_Stub::descriptor()->method(13)

constexpr uint32_t DataServiceRollbackAllMessageId = 101;
constexpr uint32_t DataServiceRollbackAllIndex = 14;
#define DataServiceRollbackAllMethod  ::DataService_Stub::descriptor()->method(14)

constexpr uint32_t DataServiceBatchRecallItemsMessageId = 108;
constexpr uint32_t DataServiceBatchRecallItemsIndex = 15;
#define DataServiceBatchRecallItemsMethod  ::DataService_Stub::descriptor()->method(15)

constexpr uint32_t DataServiceQueryTransactionLogMessageId = 114;
constexpr uint32_t DataServiceQueryTransactionLogIndex = 16;
#define DataServiceQueryTransactionLogMethod  ::DataService_Stub::descriptor()->method(16)

constexpr uint32_t DataServiceCreateEventSnapshotMessageId = 105;
constexpr uint32_t DataServiceCreateEventSnapshotIndex = 17;
#define DataServiceCreateEventSnapshotMethod  ::DataService_Stub::descriptor()->method(17)

constexpr uint32_t DataServiceAllocateIdSegmentMessageId = 180;
constexpr uint32_t DataServiceAllocateIdSegmentIndex = 18;
#define DataServiceAllocateIdSegmentMethod  ::DataService_Stub::descriptor()->method(18)
