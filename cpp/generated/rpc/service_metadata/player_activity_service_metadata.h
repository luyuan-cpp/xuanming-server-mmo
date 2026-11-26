#pragma once
#include <cstdint>

#include "proto/scene/player_activity.pb.h"

constexpr uint32_t SceneActivityClientPlayerGetActivityListMessageId = 190;
constexpr uint32_t SceneActivityClientPlayerGetActivityListIndex = 0;
#define SceneActivityClientPlayerGetActivityListMethod  ::SceneActivityClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t SceneActivityClientPlayerClaimActivityVitalityRewardMessageId = 348;
constexpr uint32_t SceneActivityClientPlayerClaimActivityVitalityRewardIndex = 1;
#define SceneActivityClientPlayerClaimActivityVitalityRewardMethod  ::SceneActivityClientPlayer_Stub::descriptor()->method(1)

constexpr uint32_t SceneActivityClientPlayerGetFestivalBeastDetailMessageId = 476;
constexpr uint32_t SceneActivityClientPlayerGetFestivalBeastDetailIndex = 2;
#define SceneActivityClientPlayerGetFestivalBeastDetailMethod  ::SceneActivityClientPlayer_Stub::descriptor()->method(2)

constexpr uint32_t SceneActivityClientPlayerStartFestivalBeastChallengeMessageId = 477;
constexpr uint32_t SceneActivityClientPlayerStartFestivalBeastChallengeIndex = 3;
#define SceneActivityClientPlayerStartFestivalBeastChallengeMethod  ::SceneActivityClientPlayer_Stub::descriptor()->method(3)

constexpr uint32_t SceneActivityClientPlayerClaimFestivalBeastRewardMessageId = 478;
constexpr uint32_t SceneActivityClientPlayerClaimFestivalBeastRewardIndex = 4;
#define SceneActivityClientPlayerClaimFestivalBeastRewardMethod  ::SceneActivityClientPlayer_Stub::descriptor()->method(4)

constexpr uint32_t SceneActivityClientPlayerGetTravelPassStateMessageId = 526;
constexpr uint32_t SceneActivityClientPlayerGetTravelPassStateIndex = 5;
#define SceneActivityClientPlayerGetTravelPassStateMethod  ::SceneActivityClientPlayer_Stub::descriptor()->method(5)

constexpr uint32_t SceneActivityClientPlayerUnlockTravelPassPremiumMessageId = 527;
constexpr uint32_t SceneActivityClientPlayerUnlockTravelPassPremiumIndex = 6;
#define SceneActivityClientPlayerUnlockTravelPassPremiumMethod  ::SceneActivityClientPlayer_Stub::descriptor()->method(6)

constexpr uint32_t SceneActivityClientPlayerClaimTravelPassRewardMessageId = 528;
constexpr uint32_t SceneActivityClientPlayerClaimTravelPassRewardIndex = 7;
#define SceneActivityClientPlayerClaimTravelPassRewardMethod  ::SceneActivityClientPlayer_Stub::descriptor()->method(7)

constexpr uint32_t SceneActivityClientPlayerEquipTravelPassCosmeticMessageId = 529;
constexpr uint32_t SceneActivityClientPlayerEquipTravelPassCosmeticIndex = 8;
#define SceneActivityClientPlayerEquipTravelPassCosmeticMethod  ::SceneActivityClientPlayer_Stub::descriptor()->method(8)
