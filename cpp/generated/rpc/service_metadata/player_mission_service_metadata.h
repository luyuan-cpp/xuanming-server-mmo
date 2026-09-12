#pragma once
#include <cstdint>

#include "proto/scene/player_mission.pb.h"

constexpr uint32_t SceneMissionClientPlayerGetMissionListMessageId = 193;
constexpr uint32_t SceneMissionClientPlayerGetMissionListIndex = 0;
#define SceneMissionClientPlayerGetMissionListMethod  ::SceneMissionClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t SceneMissionClientPlayerAcceptMissionMessageId = 194;
constexpr uint32_t SceneMissionClientPlayerAcceptMissionIndex = 1;
#define SceneMissionClientPlayerAcceptMissionMethod  ::SceneMissionClientPlayer_Stub::descriptor()->method(1)

constexpr uint32_t SceneMissionClientPlayerClaimMissionRewardMessageId = 195;
constexpr uint32_t SceneMissionClientPlayerClaimMissionRewardIndex = 2;
#define SceneMissionClientPlayerClaimMissionRewardMethod  ::SceneMissionClientPlayer_Stub::descriptor()->method(2)
