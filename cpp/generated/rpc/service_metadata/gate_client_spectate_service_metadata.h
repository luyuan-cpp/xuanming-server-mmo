#pragma once
#include <cstdint>

#include "proto/gate/gate_client_spectate.pb.h"

constexpr uint32_t GateClientSpectateSubscribeSpectateMessageId = 351;
constexpr uint32_t GateClientSpectateSubscribeSpectateIndex = 0;
#define GateClientSpectateSubscribeSpectateMethod  ::GateClientSpectate_Stub::descriptor()->method(0)

constexpr uint32_t GateClientSpectateUnsubscribeSpectateMessageId = 352;
constexpr uint32_t GateClientSpectateUnsubscribeSpectateIndex = 1;
#define GateClientSpectateUnsubscribeSpectateMethod  ::GateClientSpectate_Stub::descriptor()->method(1)
