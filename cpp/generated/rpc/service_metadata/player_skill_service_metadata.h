#pragma once
#include <cstdint>

#include "proto/scene/player_skill.pb.h"

constexpr uint32_t SceneSkillClientPlayerReleaseSkillMessageId = 84;
constexpr uint32_t SceneSkillClientPlayerReleaseSkillIndex = 0;
#define SceneSkillClientPlayerReleaseSkillMethod  ::SceneSkillClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t SceneSkillClientPlayerNotifySkillUsedMessageId = 70;
constexpr uint32_t SceneSkillClientPlayerNotifySkillUsedIndex = 1;
#define SceneSkillClientPlayerNotifySkillUsedMethod  ::SceneSkillClientPlayer_Stub::descriptor()->method(1)

constexpr uint32_t SceneSkillClientPlayerNotifySkillInterruptedMessageId = 33;
constexpr uint32_t SceneSkillClientPlayerNotifySkillInterruptedIndex = 2;
#define SceneSkillClientPlayerNotifySkillInterruptedMethod  ::SceneSkillClientPlayer_Stub::descriptor()->method(2)

constexpr uint32_t SceneSkillClientPlayerListSkillsMessageId = 77;
constexpr uint32_t SceneSkillClientPlayerListSkillsIndex = 3;
#define SceneSkillClientPlayerListSkillsMethod  ::SceneSkillClientPlayer_Stub::descriptor()->method(3)

constexpr uint32_t SceneSkillClientPlayerUpgradeSkillMessageId = 317;
constexpr uint32_t SceneSkillClientPlayerUpgradeSkillIndex = 4;
#define SceneSkillClientPlayerUpgradeSkillMethod  ::SceneSkillClientPlayer_Stub::descriptor()->method(4)

constexpr uint32_t SceneSkillClientPlayerGetSkillCraftingStateMessageId = 459;
constexpr uint32_t SceneSkillClientPlayerGetSkillCraftingStateIndex = 5;
#define SceneSkillClientPlayerGetSkillCraftingStateMethod  ::SceneSkillClientPlayer_Stub::descriptor()->method(5)

constexpr uint32_t SceneSkillClientPlayerCraftSkillBookMessageId = 460;
constexpr uint32_t SceneSkillClientPlayerCraftSkillBookIndex = 6;
#define SceneSkillClientPlayerCraftSkillBookMethod  ::SceneSkillClientPlayer_Stub::descriptor()->method(6)

constexpr uint32_t SceneSkillClientPlayerLearnSkillBookMessageId = 461;
constexpr uint32_t SceneSkillClientPlayerLearnSkillBookIndex = 7;
#define SceneSkillClientPlayerLearnSkillBookMethod  ::SceneSkillClientPlayer_Stub::descriptor()->method(7)

constexpr uint32_t SceneSkillClientPlayerDismantleSkillBookMessageId = 462;
constexpr uint32_t SceneSkillClientPlayerDismantleSkillBookIndex = 8;
#define SceneSkillClientPlayerDismantleSkillBookMethod  ::SceneSkillClientPlayer_Stub::descriptor()->method(8)

constexpr uint32_t SceneSkillClientPlayerSetCraftProxyStallMessageId = 463;
constexpr uint32_t SceneSkillClientPlayerSetCraftProxyStallIndex = 9;
#define SceneSkillClientPlayerSetCraftProxyStallMethod  ::SceneSkillClientPlayer_Stub::descriptor()->method(9)

constexpr uint32_t SceneSkillClientPlayerListCraftProxyStallsMessageId = 464;
constexpr uint32_t SceneSkillClientPlayerListCraftProxyStallsIndex = 10;
#define SceneSkillClientPlayerListCraftProxyStallsMethod  ::SceneSkillClientPlayer_Stub::descriptor()->method(10)

constexpr uint32_t SceneSkillClientPlayerSaveSkillLoadoutMessageId = 502;
constexpr uint32_t SceneSkillClientPlayerSaveSkillLoadoutIndex = 11;
#define SceneSkillClientPlayerSaveSkillLoadoutMethod  ::SceneSkillClientPlayer_Stub::descriptor()->method(11)

constexpr uint32_t SceneSkillClientPlayerSwitchSkillLoadoutMessageId = 503;
constexpr uint32_t SceneSkillClientPlayerSwitchSkillLoadoutIndex = 12;
#define SceneSkillClientPlayerSwitchSkillLoadoutMethod  ::SceneSkillClientPlayer_Stub::descriptor()->method(12)
