#pragma once
#include <cstdint>

#include "proto/scene/player_bag.pb.h"

constexpr uint32_t SceneBagClientPlayerGetBagMessageId = 191;
constexpr uint32_t SceneBagClientPlayerGetBagIndex = 0;
#define SceneBagClientPlayerGetBagMethod  ::SceneBagClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t SceneBagClientPlayerSortBagMessageId = 192;
constexpr uint32_t SceneBagClientPlayerSortBagIndex = 1;
#define SceneBagClientPlayerSortBagMethod  ::SceneBagClientPlayer_Stub::descriptor()->method(1)
