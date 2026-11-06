#pragma once
#include <cstdint>

#include "proto/scene/player_bag.pb.h"

constexpr uint32_t SceneBagClientPlayerGetBagMessageId = 191;
constexpr uint32_t SceneBagClientPlayerGetBagIndex = 0;
#define SceneBagClientPlayerGetBagMethod  ::SceneBagClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t SceneBagClientPlayerSortBagMessageId = 192;
constexpr uint32_t SceneBagClientPlayerSortBagIndex = 1;
#define SceneBagClientPlayerSortBagMethod  ::SceneBagClientPlayer_Stub::descriptor()->method(1)

constexpr uint32_t SceneBagClientPlayerEquipItemMessageId = 249;
constexpr uint32_t SceneBagClientPlayerEquipItemIndex = 2;
#define SceneBagClientPlayerEquipItemMethod  ::SceneBagClientPlayer_Stub::descriptor()->method(2)

constexpr uint32_t SceneBagClientPlayerUnequipItemMessageId = 246;
constexpr uint32_t SceneBagClientPlayerUnequipItemIndex = 3;
#define SceneBagClientPlayerUnequipItemMethod  ::SceneBagClientPlayer_Stub::descriptor()->method(3)

constexpr uint32_t SceneBagClientPlayerGmGrantItemMessageId = 247;
constexpr uint32_t SceneBagClientPlayerGmGrantItemIndex = 4;
#define SceneBagClientPlayerGmGrantItemMethod  ::SceneBagClientPlayer_Stub::descriptor()->method(4)

constexpr uint32_t SceneBagClientPlayerEnhanceEquipMessageId = 251;
constexpr uint32_t SceneBagClientPlayerEnhanceEquipIndex = 5;
#define SceneBagClientPlayerEnhanceEquipMethod  ::SceneBagClientPlayer_Stub::descriptor()->method(5)

constexpr uint32_t SceneBagClientPlayerInlayGemMessageId = 250;
constexpr uint32_t SceneBagClientPlayerInlayGemIndex = 6;
#define SceneBagClientPlayerInlayGemMethod  ::SceneBagClientPlayer_Stub::descriptor()->method(6)

constexpr uint32_t SceneBagClientPlayerRemoveGemMessageId = 252;
constexpr uint32_t SceneBagClientPlayerRemoveGemIndex = 7;
#define SceneBagClientPlayerRemoveGemMethod  ::SceneBagClientPlayer_Stub::descriptor()->method(7)

constexpr uint32_t SceneBagClientPlayerRetrieveTemporaryItemsMessageId = 374;
constexpr uint32_t SceneBagClientPlayerRetrieveTemporaryItemsIndex = 8;
#define SceneBagClientPlayerRetrieveTemporaryItemsMethod  ::SceneBagClientPlayer_Stub::descriptor()->method(8)
