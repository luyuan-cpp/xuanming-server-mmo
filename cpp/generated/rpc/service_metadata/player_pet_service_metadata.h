#pragma once
#include <cstdint>

#include "proto/scene/player_pet.pb.h"

constexpr uint32_t ScenePetClientPlayerGetPetListMessageId = 181;
constexpr uint32_t ScenePetClientPlayerGetPetListIndex = 0;
#define ScenePetClientPlayerGetPetListMethod  ::ScenePetClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t ScenePetClientPlayerSummonPetMessageId = 183;
constexpr uint32_t ScenePetClientPlayerSummonPetIndex = 1;
#define ScenePetClientPlayerSummonPetMethod  ::ScenePetClientPlayer_Stub::descriptor()->method(1)

constexpr uint32_t ScenePetClientPlayerRecallPetMessageId = 185;
constexpr uint32_t ScenePetClientPlayerRecallPetIndex = 2;
#define ScenePetClientPlayerRecallPetMethod  ::ScenePetClientPlayer_Stub::descriptor()->method(2)

constexpr uint32_t ScenePetClientPlayerAllocatePetPointsMessageId = 186;
constexpr uint32_t ScenePetClientPlayerAllocatePetPointsIndex = 3;
#define ScenePetClientPlayerAllocatePetPointsMethod  ::ScenePetClientPlayer_Stub::descriptor()->method(3)

constexpr uint32_t ScenePetClientPlayerResetPetPointsMessageId = 182;
constexpr uint32_t ScenePetClientPlayerResetPetPointsIndex = 4;
#define ScenePetClientPlayerResetPetPointsMethod  ::ScenePetClientPlayer_Stub::descriptor()->method(4)

constexpr uint32_t ScenePetClientPlayerAutoAllocatePetPointsMessageId = 188;
constexpr uint32_t ScenePetClientPlayerAutoAllocatePetPointsIndex = 5;
#define ScenePetClientPlayerAutoAllocatePetPointsMethod  ::ScenePetClientPlayer_Stub::descriptor()->method(5)

constexpr uint32_t ScenePetClientPlayerRenamePetMessageId = 189;
constexpr uint32_t ScenePetClientPlayerRenamePetIndex = 6;
#define ScenePetClientPlayerRenamePetMethod  ::ScenePetClientPlayer_Stub::descriptor()->method(6)

constexpr uint32_t ScenePetClientPlayerNotifyPetListChangedMessageId = 184;
constexpr uint32_t ScenePetClientPlayerNotifyPetListChangedIndex = 7;
#define ScenePetClientPlayerNotifyPetListChangedMethod  ::ScenePetClientPlayer_Stub::descriptor()->method(7)

constexpr uint32_t ScenePetClientPlayerGmGrantPetMessageId = 187;
constexpr uint32_t ScenePetClientPlayerGmGrantPetIndex = 8;
#define ScenePetClientPlayerGmGrantPetMethod  ::ScenePetClientPlayer_Stub::descriptor()->method(8)
