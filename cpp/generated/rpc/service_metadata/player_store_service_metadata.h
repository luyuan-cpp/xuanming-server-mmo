#pragma once
#include <cstdint>

#include "proto/player_store/player_store.pb.h"

constexpr uint32_t PlayerStoreLoadAndClaimMessageId = 357;
constexpr uint32_t PlayerStoreLoadAndClaimIndex = 0;
#define PlayerStoreLoadAndClaimMethod  ::PlayerStore_Stub::descriptor()->method(0)

constexpr uint32_t PlayerStoreSavePlayerMessageId = 358;
constexpr uint32_t PlayerStoreSavePlayerIndex = 1;
#define PlayerStoreSavePlayerMethod  ::PlayerStore_Stub::descriptor()->method(1)
