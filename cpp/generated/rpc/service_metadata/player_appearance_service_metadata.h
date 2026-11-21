#pragma once
#include <cstdint>

#include "proto/scene/player_appearance.pb.h"

constexpr uint32_t SceneAppearanceClientPlayerGetAppearanceInfoMessageId = 442;
constexpr uint32_t SceneAppearanceClientPlayerGetAppearanceInfoIndex = 0;
#define SceneAppearanceClientPlayerGetAppearanceInfoMethod  ::SceneAppearanceClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t SceneAppearanceClientPlayerSwitchAppearanceMessageId = 443;
constexpr uint32_t SceneAppearanceClientPlayerSwitchAppearanceIndex = 1;
#define SceneAppearanceClientPlayerSwitchAppearanceMethod  ::SceneAppearanceClientPlayer_Stub::descriptor()->method(1)

constexpr uint32_t SceneAppearanceClientPlayerStartAppearanceTrialMessageId = 444;
constexpr uint32_t SceneAppearanceClientPlayerStartAppearanceTrialIndex = 2;
#define SceneAppearanceClientPlayerStartAppearanceTrialMethod  ::SceneAppearanceClientPlayer_Stub::descriptor()->method(2)

constexpr uint32_t SceneAppearanceClientPlayerCancelAppearanceTrialMessageId = 445;
constexpr uint32_t SceneAppearanceClientPlayerCancelAppearanceTrialIndex = 3;
#define SceneAppearanceClientPlayerCancelAppearanceTrialMethod  ::SceneAppearanceClientPlayer_Stub::descriptor()->method(3)

constexpr uint32_t SceneAppearanceClientPlayerNotifyActorAppearanceChangedMessageId = 446;
constexpr uint32_t SceneAppearanceClientPlayerNotifyActorAppearanceChangedIndex = 4;
#define SceneAppearanceClientPlayerNotifyActorAppearanceChangedMethod  ::SceneAppearanceClientPlayer_Stub::descriptor()->method(4)

constexpr uint32_t SceneAppearanceClientPlayerNotifyAppearanceStateMessageId = 447;
constexpr uint32_t SceneAppearanceClientPlayerNotifyAppearanceStateIndex = 5;
#define SceneAppearanceClientPlayerNotifyAppearanceStateMethod  ::SceneAppearanceClientPlayer_Stub::descriptor()->method(5)
