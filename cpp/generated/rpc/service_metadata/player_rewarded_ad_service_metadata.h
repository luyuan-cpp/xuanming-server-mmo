#pragma once
#include <cstdint>

#include "proto/scene/player_rewarded_ad.pb.h"

constexpr uint32_t SceneRewardedAdClientPlayerGetRewardedAdInfoMessageId = 540;
constexpr uint32_t SceneRewardedAdClientPlayerGetRewardedAdInfoIndex = 0;
#define SceneRewardedAdClientPlayerGetRewardedAdInfoMethod  ::SceneRewardedAdClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t SceneRewardedAdClientPlayerBeginRewardedAdSessionMessageId = 541;
constexpr uint32_t SceneRewardedAdClientPlayerBeginRewardedAdSessionIndex = 1;
#define SceneRewardedAdClientPlayerBeginRewardedAdSessionMethod  ::SceneRewardedAdClientPlayer_Stub::descriptor()->method(1)

constexpr uint32_t SceneRewardedAdClientPlayerClaimRewardedAdMessageId = 542;
constexpr uint32_t SceneRewardedAdClientPlayerClaimRewardedAdIndex = 2;
#define SceneRewardedAdClientPlayerClaimRewardedAdMethod  ::SceneRewardedAdClientPlayer_Stub::descriptor()->method(2)
