#pragma once
#include <cstdint>

#include "proto/trade/trade_admin.pb.h"

constexpr uint32_t TradeAdminSeedListingMessageId = 199;
constexpr uint32_t TradeAdminSeedListingIndex = 0;
#define TradeAdminSeedListingMethod  ::TradeAdmin_Stub::descriptor()->method(0)

constexpr uint32_t TradeAdminListManualContractsMessageId = 400;
constexpr uint32_t TradeAdminListManualContractsIndex = 1;
#define TradeAdminListManualContractsMethod  ::TradeAdmin_Stub::descriptor()->method(1)

constexpr uint32_t TradeAdminResolveManualContractMessageId = 401;
constexpr uint32_t TradeAdminResolveManualContractIndex = 2;
#define TradeAdminResolveManualContractMethod  ::TradeAdmin_Stub::descriptor()->method(2)
