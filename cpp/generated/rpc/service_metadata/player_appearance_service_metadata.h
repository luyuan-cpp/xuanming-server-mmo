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

constexpr uint32_t SceneAppearanceClientPlayerEquipPetCosmeticMessageId = 530;
constexpr uint32_t SceneAppearanceClientPlayerEquipPetCosmeticIndex = 6;
#define SceneAppearanceClientPlayerEquipPetCosmeticMethod  ::SceneAppearanceClientPlayer_Stub::descriptor()->method(6)

constexpr uint32_t SceneAppearanceClientPlayerEquipSkillVfxMessageId = 531;
constexpr uint32_t SceneAppearanceClientPlayerEquipSkillVfxIndex = 7;
#define SceneAppearanceClientPlayerEquipSkillVfxMethod  ::SceneAppearanceClientPlayer_Stub::descriptor()->method(7)

constexpr uint32_t SceneAppearanceClientPlayerBuyAppearanceSeriesBundleMessageId = 592;
constexpr uint32_t SceneAppearanceClientPlayerBuyAppearanceSeriesBundleIndex = 8;
#define SceneAppearanceClientPlayerBuyAppearanceSeriesBundleMethod  ::SceneAppearanceClientPlayer_Stub::descriptor()->method(8)

constexpr uint32_t SceneAppearanceClientPlayerEquipShowcaseCosmeticMessageId = 593;
constexpr uint32_t SceneAppearanceClientPlayerEquipShowcaseCosmeticIndex = 9;
#define SceneAppearanceClientPlayerEquipShowcaseCosmeticMethod  ::SceneAppearanceClientPlayer_Stub::descriptor()->method(9)

constexpr uint32_t SceneAppearanceClientPlayerCastCityCelebrationMessageId = 594;
constexpr uint32_t SceneAppearanceClientPlayerCastCityCelebrationIndex = 10;
#define SceneAppearanceClientPlayerCastCityCelebrationMethod  ::SceneAppearanceClientPlayer_Stub::descriptor()->method(10)

constexpr uint32_t SceneAppearanceClientPlayerClaimCityCelebrationGiftMessageId = 595;
constexpr uint32_t SceneAppearanceClientPlayerClaimCityCelebrationGiftIndex = 11;
#define SceneAppearanceClientPlayerClaimCityCelebrationGiftMethod  ::SceneAppearanceClientPlayer_Stub::descriptor()->method(11)

constexpr uint32_t SceneAppearanceClientPlayerNotifyCityCelebrationMessageId = 596;
constexpr uint32_t SceneAppearanceClientPlayerNotifyCityCelebrationIndex = 12;
#define SceneAppearanceClientPlayerNotifyCityCelebrationMethod  ::SceneAppearanceClientPlayer_Stub::descriptor()->method(12)

constexpr uint32_t SceneAppearanceClientPlayerSendCosmeticGiftMessageId = 597;
constexpr uint32_t SceneAppearanceClientPlayerSendCosmeticGiftIndex = 13;
#define SceneAppearanceClientPlayerSendCosmeticGiftMethod  ::SceneAppearanceClientPlayer_Stub::descriptor()->method(13)
