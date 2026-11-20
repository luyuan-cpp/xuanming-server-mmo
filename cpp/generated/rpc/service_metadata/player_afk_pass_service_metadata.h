#pragma once
#include <cstdint>

#include "proto/scene/player_afk_pass.pb.h"

constexpr uint32_t SceneAfkPassClientPlayerGetAfkPassMessageId = 430;
constexpr uint32_t SceneAfkPassClientPlayerGetAfkPassIndex = 0;
#define SceneAfkPassClientPlayerGetAfkPassMethod  ::SceneAfkPassClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t SceneAfkPassClientPlayerBuyAfkPassMessageId = 431;
constexpr uint32_t SceneAfkPassClientPlayerBuyAfkPassIndex = 1;
#define SceneAfkPassClientPlayerBuyAfkPassMethod  ::SceneAfkPassClientPlayer_Stub::descriptor()->method(1)

constexpr uint32_t SceneAfkPassClientPlayerGmSetAfkPassMessageId = 432;
constexpr uint32_t SceneAfkPassClientPlayerGmSetAfkPassIndex = 2;
#define SceneAfkPassClientPlayerGmSetAfkPassMethod  ::SceneAfkPassClientPlayer_Stub::descriptor()->method(2)
