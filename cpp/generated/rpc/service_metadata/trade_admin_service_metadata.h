#pragma once
#include <cstdint>

#include "proto/trade/trade_admin.pb.h"

constexpr uint32_t TradeAdminSeedListingMessageId = 199;
constexpr uint32_t TradeAdminSeedListingIndex = 0;
#define TradeAdminSeedListingMethod  ::TradeAdmin_Stub::descriptor()->method(0)
