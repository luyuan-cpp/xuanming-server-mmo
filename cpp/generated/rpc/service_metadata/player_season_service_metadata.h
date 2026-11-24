#pragma once
#include <cstdint>

#include "proto/scene/player_season.pb.h"

constexpr uint32_t SceneSeasonClientPlayerGetSeasonStateMessageId = 453;
constexpr uint32_t SceneSeasonClientPlayerGetSeasonStateIndex = 0;
#define SceneSeasonClientPlayerGetSeasonStateMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t SceneSeasonClientPlayerEquipSeasonSealMessageId = 454;
constexpr uint32_t SceneSeasonClientPlayerEquipSeasonSealIndex = 1;
#define SceneSeasonClientPlayerEquipSeasonSealMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(1)

constexpr uint32_t SceneSeasonClientPlayerCatchupSeasonSealMessageId = 455;
constexpr uint32_t SceneSeasonClientPlayerCatchupSeasonSealIndex = 2;
#define SceneSeasonClientPlayerCatchupSeasonSealMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(2)
