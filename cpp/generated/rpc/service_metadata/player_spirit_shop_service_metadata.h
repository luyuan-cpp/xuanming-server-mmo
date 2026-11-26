#pragma once
#include <cstdint>

#include "proto/scene/player_spirit_shop.pb.h"

constexpr uint32_t SceneSpiritShopClientPlayerGetSpiritShopInfoMessageId = 509;
constexpr uint32_t SceneSpiritShopClientPlayerGetSpiritShopInfoIndex = 0;
#define SceneSpiritShopClientPlayerGetSpiritShopInfoMethod  ::SceneSpiritShopClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t SceneSpiritShopClientPlayerBuySpiritShopGoodsMessageId = 510;
constexpr uint32_t SceneSpiritShopClientPlayerBuySpiritShopGoodsIndex = 1;
#define SceneSpiritShopClientPlayerBuySpiritShopGoodsMethod  ::SceneSpiritShopClientPlayer_Stub::descriptor()->method(1)
