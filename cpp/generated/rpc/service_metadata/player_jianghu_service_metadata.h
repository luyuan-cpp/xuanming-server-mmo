#pragma once
#include <cstdint>

#include "proto/scene/player_jianghu.pb.h"

constexpr uint32_t SceneJianghuClientPlayerGetJianghuProfileMessageId = 555;
constexpr uint32_t SceneJianghuClientPlayerGetJianghuProfileIndex = 0;
#define SceneJianghuClientPlayerGetJianghuProfileMethod  ::SceneJianghuClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t SceneJianghuClientPlayerSetJianghuRefuseStrangersMessageId = 556;
constexpr uint32_t SceneJianghuClientPlayerSetJianghuRefuseStrangersIndex = 1;
#define SceneJianghuClientPlayerSetJianghuRefuseStrangersMethod  ::SceneJianghuClientPlayer_Stub::descriptor()->method(1)

constexpr uint32_t SceneJianghuClientPlayerClearJianghuDefeatMarkMessageId = 557;
constexpr uint32_t SceneJianghuClientPlayerClearJianghuDefeatMarkIndex = 2;
#define SceneJianghuClientPlayerClearJianghuDefeatMarkMethod  ::SceneJianghuClientPlayer_Stub::descriptor()->method(2)
