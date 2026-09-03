#pragma once
#include <cstdint>

#include "proto/scene/player_attribute.pb.h"

constexpr uint32_t SceneAttributeClientPlayerGetAttributePanelMessageId = 167;
constexpr uint32_t SceneAttributeClientPlayerGetAttributePanelIndex = 0;
#define SceneAttributeClientPlayerGetAttributePanelMethod  ::SceneAttributeClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t SceneAttributeClientPlayerAllocateAttributePointsMessageId = 168;
constexpr uint32_t SceneAttributeClientPlayerAllocateAttributePointsIndex = 1;
#define SceneAttributeClientPlayerAllocateAttributePointsMethod  ::SceneAttributeClientPlayer_Stub::descriptor()->method(1)

constexpr uint32_t SceneAttributeClientPlayerResetAttributePointsMessageId = 172;
constexpr uint32_t SceneAttributeClientPlayerResetAttributePointsIndex = 2;
#define SceneAttributeClientPlayerResetAttributePointsMethod  ::SceneAttributeClientPlayer_Stub::descriptor()->method(2)

constexpr uint32_t SceneAttributeClientPlayerAutoAllocateAttributePointsMessageId = 173;
constexpr uint32_t SceneAttributeClientPlayerAutoAllocateAttributePointsIndex = 3;
#define SceneAttributeClientPlayerAutoAllocateAttributePointsMethod  ::SceneAttributeClientPlayer_Stub::descriptor()->method(3)

constexpr uint32_t SceneAttributeClientPlayerCreateAttributeSchemeMessageId = 174;
constexpr uint32_t SceneAttributeClientPlayerCreateAttributeSchemeIndex = 4;
#define SceneAttributeClientPlayerCreateAttributeSchemeMethod  ::SceneAttributeClientPlayer_Stub::descriptor()->method(4)

constexpr uint32_t SceneAttributeClientPlayerSwitchAttributeSchemeMessageId = 171;
constexpr uint32_t SceneAttributeClientPlayerSwitchAttributeSchemeIndex = 5;
#define SceneAttributeClientPlayerSwitchAttributeSchemeMethod  ::SceneAttributeClientPlayer_Stub::descriptor()->method(5)

constexpr uint32_t SceneAttributeClientPlayerRenameAttributeSchemeMessageId = 169;
constexpr uint32_t SceneAttributeClientPlayerRenameAttributeSchemeIndex = 6;
#define SceneAttributeClientPlayerRenameAttributeSchemeMethod  ::SceneAttributeClientPlayer_Stub::descriptor()->method(6)

constexpr uint32_t SceneAttributeClientPlayerNotifyAttributePanelChangedMessageId = 170;
constexpr uint32_t SceneAttributeClientPlayerNotifyAttributePanelChangedIndex = 7;
#define SceneAttributeClientPlayerNotifyAttributePanelChangedMethod  ::SceneAttributeClientPlayer_Stub::descriptor()->method(7)

constexpr uint32_t SceneAttributeClientPlayerGmSetPlayerLevelMessageId = 175;
constexpr uint32_t SceneAttributeClientPlayerGmSetPlayerLevelIndex = 8;
#define SceneAttributeClientPlayerGmSetPlayerLevelMethod  ::SceneAttributeClientPlayer_Stub::descriptor()->method(8)
