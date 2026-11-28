#pragma once
#include <cstdint>

#include "proto/scene/player_season.pb.h"

constexpr uint32_t SceneSeasonClientPlayerGetSeasonStateMessageId = 453;
constexpr uint32_t SceneSeasonClientPlayerGetSeasonStateIndex = 0;
#define SceneSeasonClientPlayerGetSeasonStateMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t SceneSeasonClientPlayerEquipSeasonSealMessageId = 454;
constexpr uint32_t SceneSeasonClientPlayerEquipSeasonSealIndex = 1;
#define SceneSeasonClientPlayerEquipSeasonSealMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(1)

constexpr uint32_t SceneSeasonClientPlayerCatchupSeasonSealMessageId = 455;
constexpr uint32_t SceneSeasonClientPlayerCatchupSeasonSealIndex = 2;
#define SceneSeasonClientPlayerCatchupSeasonSealMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(2)

constexpr uint32_t SceneSeasonClientPlayerEquipWuxingFormationMessageId = 515;
constexpr uint32_t SceneSeasonClientPlayerEquipWuxingFormationIndex = 3;
#define SceneSeasonClientPlayerEquipWuxingFormationMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(3)

constexpr uint32_t SceneSeasonClientPlayerGetWuxingTowerStateMessageId = 516;
constexpr uint32_t SceneSeasonClientPlayerGetWuxingTowerStateIndex = 4;
#define SceneSeasonClientPlayerGetWuxingTowerStateMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(4)

constexpr uint32_t SceneSeasonClientPlayerStartWuxingTowerRunMessageId = 517;
constexpr uint32_t SceneSeasonClientPlayerStartWuxingTowerRunIndex = 5;
#define SceneSeasonClientPlayerStartWuxingTowerRunMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(5)

constexpr uint32_t SceneSeasonClientPlayerPickWuxingTowerSigilMessageId = 518;
constexpr uint32_t SceneSeasonClientPlayerPickWuxingTowerSigilIndex = 6;
#define SceneSeasonClientPlayerPickWuxingTowerSigilMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(6)

constexpr uint32_t SceneSeasonClientPlayerAbandonWuxingTowerRunMessageId = 519;
constexpr uint32_t SceneSeasonClientPlayerAbandonWuxingTowerRunIndex = 7;
#define SceneSeasonClientPlayerAbandonWuxingTowerRunMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(7)

constexpr uint32_t SceneSeasonClientPlayerSaveBattleBuildBookmarkMessageId = 520;
constexpr uint32_t SceneSeasonClientPlayerSaveBattleBuildBookmarkIndex = 8;
#define SceneSeasonClientPlayerSaveBattleBuildBookmarkMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(8)

constexpr uint32_t SceneSeasonClientPlayerDeleteBattleBuildBookmarkMessageId = 521;
constexpr uint32_t SceneSeasonClientPlayerDeleteBattleBuildBookmarkIndex = 9;
#define SceneSeasonClientPlayerDeleteBattleBuildBookmarkMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(9)

constexpr uint32_t SceneSeasonClientPlayerSetRetroHonorTargetMessageId = 569;
constexpr uint32_t SceneSeasonClientPlayerSetRetroHonorTargetIndex = 10;
#define SceneSeasonClientPlayerSetRetroHonorTargetMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(10)

constexpr uint32_t SceneSeasonClientPlayerChooseWuxingRealmBranchMessageId = 585;
constexpr uint32_t SceneSeasonClientPlayerChooseWuxingRealmBranchIndex = 11;
#define SceneSeasonClientPlayerChooseWuxingRealmBranchMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(11)

constexpr uint32_t SceneSeasonClientPlayerResolveWuxingRealmEventMessageId = 586;
constexpr uint32_t SceneSeasonClientPlayerResolveWuxingRealmEventIndex = 12;
#define SceneSeasonClientPlayerResolveWuxingRealmEventMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(12)

constexpr uint32_t SceneSeasonClientPlayerExpandSavedBuildSlotsMessageId = 603;
constexpr uint32_t SceneSeasonClientPlayerExpandSavedBuildSlotsIndex = 13;
#define SceneSeasonClientPlayerExpandSavedBuildSlotsMethod  ::SceneSeasonClientPlayer_Stub::descriptor()->method(13)
