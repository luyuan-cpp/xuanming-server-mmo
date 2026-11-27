#pragma once
#include <cstdint>

#include "proto/login/login_admin.pb.h"

constexpr uint32_t LoginAdminRemovePlayersFromAccountsMessageId = 111;
constexpr uint32_t LoginAdminRemovePlayersFromAccountsIndex = 0;
#define LoginAdminRemovePlayersFromAccountsMethod  ::LoginAdmin_Stub::descriptor()->method(0)

constexpr uint32_t LoginAdminScheduleAccountDeletionMessageId = 426;
constexpr uint32_t LoginAdminScheduleAccountDeletionIndex = 1;
#define LoginAdminScheduleAccountDeletionMethod  ::LoginAdmin_Stub::descriptor()->method(1)

constexpr uint32_t LoginAdminResetAccountRealNameMessageId = 427;
constexpr uint32_t LoginAdminResetAccountRealNameIndex = 2;
#define LoginAdminResetAccountRealNameMethod  ::LoginAdmin_Stub::descriptor()->method(2)
