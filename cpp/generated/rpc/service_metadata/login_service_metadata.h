#pragma once
#include <cstdint>

#include "proto/login/login.pb.h"

constexpr uint32_t ClientPlayerLoginLoginMessageId = 48;
constexpr uint32_t ClientPlayerLoginLoginIndex = 0;
#define ClientPlayerLoginLoginMethod  ::ClientPlayerLogin_Stub::descriptor()->method(0)

constexpr uint32_t ClientPlayerLoginCreatePlayerMessageId = 14;
constexpr uint32_t ClientPlayerLoginCreatePlayerIndex = 1;
#define ClientPlayerLoginCreatePlayerMethod  ::ClientPlayerLogin_Stub::descriptor()->method(1)

constexpr uint32_t ClientPlayerLoginEnterGameMessageId = 26;
constexpr uint32_t ClientPlayerLoginEnterGameIndex = 2;
#define ClientPlayerLoginEnterGameMethod  ::ClientPlayerLogin_Stub::descriptor()->method(2)

constexpr uint32_t ClientPlayerLoginLeaveGameMessageId = 17;
constexpr uint32_t ClientPlayerLoginLeaveGameIndex = 3;
#define ClientPlayerLoginLeaveGameMethod  ::ClientPlayerLogin_Stub::descriptor()->method(3)

constexpr uint32_t ClientPlayerLoginDisconnectMessageId = 58;
constexpr uint32_t ClientPlayerLoginDisconnectIndex = 4;
#define ClientPlayerLoginDisconnectMethod  ::ClientPlayerLogin_Stub::descriptor()->method(4)

constexpr uint32_t ClientPlayerLoginRefreshTokenMessageId = 127;
constexpr uint32_t ClientPlayerLoginRefreshTokenIndex = 5;
#define ClientPlayerLoginRefreshTokenMethod  ::ClientPlayerLogin_Stub::descriptor()->method(5)

constexpr uint32_t ClientPlayerLoginRequestGateMigrationMessageId = 375;
constexpr uint32_t ClientPlayerLoginRequestGateMigrationIndex = 6;
#define ClientPlayerLoginRequestGateMigrationMethod  ::ClientPlayerLogin_Stub::descriptor()->method(6)

constexpr uint32_t ClientPlayerLoginRenewSessionMessageId = 394;
constexpr uint32_t ClientPlayerLoginRenewSessionIndex = 7;
#define ClientPlayerLoginRenewSessionMethod  ::ClientPlayerLogin_Stub::descriptor()->method(7)

constexpr uint32_t LoginPreGateAssignGateMessageId = 118;
constexpr uint32_t LoginPreGateAssignGateIndex = 0;
#define LoginPreGateAssignGateMethod  ::LoginPreGate_Stub::descriptor()->method(0)

constexpr uint32_t LoginPreGateQueryQueueStatusMessageId = 138;
constexpr uint32_t LoginPreGateQueryQueueStatusIndex = 1;
#define LoginPreGateQueryQueueStatusMethod  ::LoginPreGate_Stub::descriptor()->method(1)

constexpr uint32_t LoginPreGateGetZoneRoleCountMessageId = 395;
constexpr uint32_t LoginPreGateGetZoneRoleCountIndex = 2;
#define LoginPreGateGetZoneRoleCountMethod  ::LoginPreGate_Stub::descriptor()->method(2)

constexpr uint32_t LoginAdminRemovePlayersFromAccountsMessageId = 111;
constexpr uint32_t LoginAdminRemovePlayersFromAccountsIndex = 0;
#define LoginAdminRemovePlayersFromAccountsMethod  ::LoginAdmin_Stub::descriptor()->method(0)
