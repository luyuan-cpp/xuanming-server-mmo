#pragma once
#include <cstdint>

#include "proto/scene/player_activity.pb.h"

constexpr uint32_t SceneActivityClientPlayerGetActivityListMessageId = 190;
constexpr uint32_t SceneActivityClientPlayerGetActivityListIndex = 0;
#define SceneActivityClientPlayerGetActivityListMethod  ::SceneActivityClientPlayer_Stub::descriptor()->method(0)
