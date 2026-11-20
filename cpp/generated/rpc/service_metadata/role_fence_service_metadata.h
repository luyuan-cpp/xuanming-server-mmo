#pragma once
#include <cstdint>

#include "proto/role_fence/role_fence.pb.h"

constexpr uint32_t RoleFenceParticipantPrepareRoleFreezeMessageId = 359;
constexpr uint32_t RoleFenceParticipantPrepareRoleFreezeIndex = 0;
#define RoleFenceParticipantPrepareRoleFreezeMethod  ::RoleFenceParticipant_Stub::descriptor()->method(0)

constexpr uint32_t RoleFenceParticipantApplyRoleFenceMessageId = 360;
constexpr uint32_t RoleFenceParticipantApplyRoleFenceIndex = 1;
#define RoleFenceParticipantApplyRoleFenceMethod  ::RoleFenceParticipant_Stub::descriptor()->method(1)

constexpr uint32_t RoleFenceParticipantPurgeRemovedRoleMessageId = 414;
constexpr uint32_t RoleFenceParticipantPurgeRemovedRoleIndex = 2;
#define RoleFenceParticipantPurgeRemovedRoleMethod  ::RoleFenceParticipant_Stub::descriptor()->method(2)
