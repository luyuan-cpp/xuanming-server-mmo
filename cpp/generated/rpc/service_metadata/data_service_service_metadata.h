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

constexpr uint32_t DataServiceSetAccountRealNameMessageId = 415;
constexpr uint32_t DataServiceSetAccountRealNameIndex = 5;
#define DataServiceSetAccountRealNameMethod  ::DataService_Stub::descriptor()->method(5)

constexpr uint32_t DataServiceGetAccountRealNameMessageId = 416;
constexpr uint32_t DataServiceGetAccountRealNameIndex = 6;
#define DataServiceGetAccountRealNameMethod  ::DataService_Stub::descriptor()->method(6)

constexpr uint32_t DataServiceResetAccountRealNameMessageId = 417;
constexpr uint32_t DataServiceResetAccountRealNameIndex = 7;
#define DataServiceResetAccountRealNameMethod  ::DataService_Stub::descriptor()->method(7)

constexpr uint32_t DataServiceUpsertAccountChannelIdentityMessageId = 437;
constexpr uint32_t DataServiceUpsertAccountChannelIdentityIndex = 8;
#define DataServiceUpsertAccountChannelIdentityMethod  ::DataService_Stub::descriptor()->method(8)

constexpr uint32_t DataServiceGetPlayerChannelIdentityMessageId = 438;
constexpr uint32_t DataServiceGetPlayerChannelIdentityIndex = 9;
#define DataServiceGetPlayerChannelIdentityMethod  ::DataService_Stub::descriptor()->method(9)

constexpr uint32_t DataServiceBeginAccountRoleTradeMessageId = 323;
constexpr uint32_t DataServiceBeginAccountRoleTradeIndex = 10;
#define DataServiceBeginAccountRoleTradeMethod  ::DataService_Stub::descriptor()->method(10)

constexpr uint32_t DataServiceConfirmAccountRoleTradeFreezeMessageId = 324;
constexpr uint32_t DataServiceConfirmAccountRoleTradeFreezeIndex = 11;
#define DataServiceConfirmAccountRoleTradeFreezeMethod  ::DataService_Stub::descriptor()->method(11)

constexpr uint32_t DataServiceCancelAccountRoleTradeMessageId = 325;
constexpr uint32_t DataServiceCancelAccountRoleTradeIndex = 12;
#define DataServiceCancelAccountRoleTradeMethod  ::DataService_Stub::descriptor()->method(12)

constexpr uint32_t DataServiceTransferAccountRoleMessageId = 326;
constexpr uint32_t DataServiceTransferAccountRoleIndex = 13;
#define DataServiceTransferAccountRoleMethod  ::DataService_Stub::descriptor()->method(13)

constexpr uint32_t DataServiceCheckAccountRoleSlotMessageId = 361;
constexpr uint32_t DataServiceCheckAccountRoleSlotIndex = 14;
#define DataServiceCheckAccountRoleSlotMethod  ::DataService_Stub::descriptor()->method(14)

constexpr uint32_t DataServiceGetRoleFenceStateMessageId = 362;
constexpr uint32_t DataServiceGetRoleFenceStateIndex = 15;
#define DataServiceGetRoleFenceStateMethod  ::DataService_Stub::descriptor()->method(15)

constexpr uint32_t DataServiceVerifyRoleAdmissionMessageId = 363;
constexpr uint32_t DataServiceVerifyRoleAdmissionIndex = 16;
#define DataServiceVerifyRoleAdmissionMethod  ::DataService_Stub::descriptor()->method(16)

constexpr uint32_t DataServiceRegisterRoleAdmissionMessageId = 327;
constexpr uint32_t DataServiceRegisterRoleAdmissionIndex = 17;
#define DataServiceRegisterRoleAdmissionMethod  ::DataService_Stub::descriptor()->method(17)

constexpr uint32_t DataServiceCloseRoleAdmissionsMessageId = 328;
constexpr uint32_t DataServiceCloseRoleAdmissionsIndex = 18;
#define DataServiceCloseRoleAdmissionsMethod  ::DataService_Stub::descriptor()->method(18)

constexpr uint32_t DataServiceAckRoleTradeDrainMessageId = 329;
constexpr uint32_t DataServiceAckRoleTradeDrainIndex = 19;
#define DataServiceAckRoleTradeDrainMethod  ::DataService_Stub::descriptor()->method(19)

constexpr uint32_t DataServiceRequestAccountDeletionMessageId = 418;
constexpr uint32_t DataServiceRequestAccountDeletionIndex = 20;
#define DataServiceRequestAccountDeletionMethod  ::DataService_Stub::descriptor()->method(20)

constexpr uint32_t DataServiceCancelAccountDeletionMessageId = 419;
constexpr uint32_t DataServiceCancelAccountDeletionIndex = 21;
#define DataServiceCancelAccountDeletionMethod  ::DataService_Stub::descriptor()->method(21)

constexpr uint32_t DataServiceClaimAccountDeletionsMessageId = 420;
constexpr uint32_t DataServiceClaimAccountDeletionsIndex = 22;
#define DataServiceClaimAccountDeletionsMethod  ::DataService_Stub::descriptor()->method(22)

constexpr uint32_t DataServiceFinishAccountDeletionMessageId = 421;
constexpr uint32_t DataServiceFinishAccountDeletionIndex = 23;
#define DataServiceFinishAccountDeletionMethod  ::DataService_Stub::descriptor()->method(23)

constexpr uint32_t DataServiceReleaseDeadPlayerStoreHolderMessageId = 354;
constexpr uint32_t DataServiceReleaseDeadPlayerStoreHolderIndex = 24;
#define DataServiceReleaseDeadPlayerStoreHolderMethod  ::DataService_Stub::descriptor()->method(24)

constexpr uint32_t DataServiceReadPlayerStoreHolderMessageId = 373;
constexpr uint32_t DataServiceReadPlayerStoreHolderIndex = 25;
#define DataServiceReadPlayerStoreHolderMethod  ::DataService_Stub::descriptor()->method(25)

constexpr uint32_t DataServiceRegisterPlayerZoneMessageId = 86;
constexpr uint32_t DataServiceRegisterPlayerZoneIndex = 26;
#define DataServiceRegisterPlayerZoneMethod  ::DataService_Stub::descriptor()->method(26)

constexpr uint32_t DataServiceGetPlayerHomeZoneMessageId = 93;
constexpr uint32_t DataServiceGetPlayerHomeZoneIndex = 27;
#define DataServiceGetPlayerHomeZoneMethod  ::DataService_Stub::descriptor()->method(27)

constexpr uint32_t DataServiceBatchGetPlayerHomeZoneMessageId = 87;
constexpr uint32_t DataServiceBatchGetPlayerHomeZoneIndex = 28;
#define DataServiceBatchGetPlayerHomeZoneMethod  ::DataService_Stub::descriptor()->method(28)

constexpr uint32_t DataServiceGetMergedZonesMessageId = 355;
constexpr uint32_t DataServiceGetMergedZonesIndex = 29;
#define DataServiceGetMergedZonesMethod  ::DataService_Stub::descriptor()->method(29)

constexpr uint32_t DataServiceCountZoneRolesMessageId = 393;
constexpr uint32_t DataServiceCountZoneRolesIndex = 30;
#define DataServiceCountZoneRolesMethod  ::DataService_Stub::descriptor()->method(30)

constexpr uint32_t DataServiceGetPlayerRouteMessageId = 364;
constexpr uint32_t DataServiceGetPlayerRouteIndex = 31;
#define DataServiceGetPlayerRouteMethod  ::DataService_Stub::descriptor()->method(31)

constexpr uint32_t DataServiceListPlayerSnapshotsMessageId = 97;
constexpr uint32_t DataServiceListPlayerSnapshotsIndex = 32;
#define DataServiceListPlayerSnapshotsMethod  ::DataService_Stub::descriptor()->method(32)

constexpr uint32_t DataServiceBatchRecallItemsMessageId = 108;
constexpr uint32_t DataServiceBatchRecallItemsIndex = 33;
#define DataServiceBatchRecallItemsMethod  ::DataService_Stub::descriptor()->method(33)

constexpr uint32_t DataServiceGetRecallBatchMessageId = 365;
constexpr uint32_t DataServiceGetRecallBatchIndex = 34;
#define DataServiceGetRecallBatchMethod  ::DataService_Stub::descriptor()->method(34)

constexpr uint32_t DataServiceCancelRecallBatchMessageId = 366;
constexpr uint32_t DataServiceCancelRecallBatchIndex = 35;
#define DataServiceCancelRecallBatchMethod  ::DataService_Stub::descriptor()->method(35)

constexpr uint32_t DataServiceResolveRecallOpMessageId = 367;
constexpr uint32_t DataServiceResolveRecallOpIndex = 36;
#define DataServiceResolveRecallOpMethod  ::DataService_Stub::descriptor()->method(36)

constexpr uint32_t DataServiceRegisterRecallReviewHandledMessageId = 384;
constexpr uint32_t DataServiceRegisterRecallReviewHandledIndex = 37;
#define DataServiceRegisterRecallReviewHandledMethod  ::DataService_Stub::descriptor()->method(37)

constexpr uint32_t DataServiceQueryTransactionLogMessageId = 114;
constexpr uint32_t DataServiceQueryTransactionLogIndex = 38;
#define DataServiceQueryTransactionLogMethod  ::DataService_Stub::descriptor()->method(38)

constexpr uint32_t DataServiceAllocateIdSegmentMessageId = 180;
constexpr uint32_t DataServiceAllocateIdSegmentIndex = 39;
#define DataServiceAllocateIdSegmentMethod  ::DataService_Stub::descriptor()->method(39)

constexpr uint32_t DataServiceReservePlayerNameMessageId = 229;
constexpr uint32_t DataServiceReservePlayerNameIndex = 40;
#define DataServiceReservePlayerNameMethod  ::DataService_Stub::descriptor()->method(40)

constexpr uint32_t DataServiceReleasePlayerNameMessageId = 237;
constexpr uint32_t DataServiceReleasePlayerNameIndex = 41;
#define DataServiceReleasePlayerNameMethod  ::DataService_Stub::descriptor()->method(41)

constexpr uint32_t DataServiceBatchGetPlayerNameMessageId = 231;
constexpr uint32_t DataServiceBatchGetPlayerNameIndex = 42;
#define DataServiceBatchGetPlayerNameMethod  ::DataService_Stub::descriptor()->method(42)

constexpr uint32_t DataServiceRenameRoleMessageId = 422;
constexpr uint32_t DataServiceRenameRoleIndex = 43;
#define DataServiceRenameRoleMethod  ::DataService_Stub::descriptor()->method(43)

constexpr uint32_t DataServiceBatchGetPlayerDisplayMessageId = 356;
constexpr uint32_t DataServiceBatchGetPlayerDisplayIndex = 44;
#define DataServiceBatchGetPlayerDisplayMethod  ::DataService_Stub::descriptor()->method(44)

constexpr uint32_t DataServiceGetPlayerAssetOpLedgerMessageId = 248;
constexpr uint32_t DataServiceGetPlayerAssetOpLedgerIndex = 45;
#define DataServiceGetPlayerAssetOpLedgerMethod  ::DataService_Stub::descriptor()->method(45)

constexpr uint32_t DataServiceGetPlayerInboxLedgerMessageId = 258;
constexpr uint32_t DataServiceGetPlayerInboxLedgerIndex = 46;
#define DataServiceGetPlayerInboxLedgerMethod  ::DataService_Stub::descriptor()->method(46)

constexpr uint32_t DataServiceGetPlayerItemGainBlockMessageId = 385;
constexpr uint32_t DataServiceGetPlayerItemGainBlockIndex = 47;
#define DataServiceGetPlayerItemGainBlockMethod  ::DataService_Stub::descriptor()->method(47)

constexpr uint32_t DataServiceUpdateRoleAppearanceMessageId = 441;
constexpr uint32_t DataServiceUpdateRoleAppearanceIndex = 48;
#define DataServiceUpdateRoleAppearanceMethod  ::DataService_Stub::descriptor()->method(48)
