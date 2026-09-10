#pragma once
#include <cstdint>

#include "proto/scene/player_mission.pb.h"

constexpr uint32_t SceneMissionClientPlayerGetMissionListMessageId = 193;
constexpr uint32_t SceneMissionClientPlayerGetMissionListIndex = 0;
#define SceneMissionClientPlayerGetMissionListMethod  ::SceneMissionClientPlayer_Stub::descriptor()->method(0)
