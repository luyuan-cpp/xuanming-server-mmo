#pragma once
#include <cstdint>

#include "proto/player_inbox/player_inbox_admin.pb.h"

constexpr uint32_t PlayerInboxAdminGmSanctionPlayerMessageId = 259;
constexpr uint32_t PlayerInboxAdminGmSanctionPlayerIndex = 0;
#define PlayerInboxAdminGmSanctionPlayerMethod  ::PlayerInboxAdmin_Stub::descriptor()->method(0)

constexpr uint32_t PlayerInboxAdminGetSummaryMessageId = 256;
constexpr uint32_t PlayerInboxAdminGetSummaryIndex = 1;
#define PlayerInboxAdminGetSummaryMethod  ::PlayerInboxAdmin_Stub::descriptor()->method(1)

constexpr uint32_t PlayerInboxAdminListItemsMessageId = 260;
constexpr uint32_t PlayerInboxAdminListItemsIndex = 2;
#define PlayerInboxAdminListItemsMethod  ::PlayerInboxAdmin_Stub::descriptor()->method(2)

constexpr uint32_t PlayerInboxAdminLookupByRequestMessageId = 261;
constexpr uint32_t PlayerInboxAdminLookupByRequestIndex = 3;
#define PlayerInboxAdminLookupByRequestMethod  ::PlayerInboxAdmin_Stub::descriptor()->method(3)

constexpr uint32_t PlayerInboxAdminVoidItemMessageId = 262;
constexpr uint32_t PlayerInboxAdminVoidItemIndex = 4;
#define PlayerInboxAdminVoidItemMethod  ::PlayerInboxAdmin_Stub::descriptor()->method(4)

constexpr uint32_t PlayerInboxAdminReconcilePlayerMessageId = 263;
constexpr uint32_t PlayerInboxAdminReconcilePlayerIndex = 5;
#define PlayerInboxAdminReconcilePlayerMethod  ::PlayerInboxAdmin_Stub::descriptor()->method(5)

constexpr uint32_t PlayerInboxAdminGetItemGainBlocksMessageId = 386;
constexpr uint32_t PlayerInboxAdminGetItemGainBlocksIndex = 6;
#define PlayerInboxAdminGetItemGainBlocksMethod  ::PlayerInboxAdmin_Stub::descriptor()->method(6)
