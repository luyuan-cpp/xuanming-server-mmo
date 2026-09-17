#pragma once
#include <cstdint>

#include "proto/trade/jubaozhai.pb.h"

constexpr uint32_t ClientPlayerJubaozhaiBrowseListingsMessageId = 196;
constexpr uint32_t ClientPlayerJubaozhaiBrowseListingsIndex = 0;
#define ClientPlayerJubaozhaiBrowseListingsMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(0)

constexpr uint32_t ClientPlayerJubaozhaiGetListingDetailMessageId = 197;
constexpr uint32_t ClientPlayerJubaozhaiGetListingDetailIndex = 1;
#define ClientPlayerJubaozhaiGetListingDetailMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(1)

constexpr uint32_t ClientPlayerJubaozhaiSetFavoriteMessageId = 198;
constexpr uint32_t ClientPlayerJubaozhaiSetFavoriteIndex = 2;
#define ClientPlayerJubaozhaiSetFavoriteMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(2)

constexpr uint32_t ClientPlayerJubaozhaiGetMyShelfMessageId = 200;
constexpr uint32_t ClientPlayerJubaozhaiGetMyShelfIndex = 3;
#define ClientPlayerJubaozhaiGetMyShelfMethod  ::ClientPlayerJubaozhai_Stub::descriptor()->method(3)
