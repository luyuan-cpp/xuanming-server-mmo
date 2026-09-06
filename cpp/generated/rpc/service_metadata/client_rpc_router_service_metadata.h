#pragma once
#include <cstdint>

#include "proto/client_rpc_router/client_rpc_router.pb.h"

constexpr uint32_t ClientRpcRouterForwardMessageId = 176;
constexpr uint32_t ClientRpcRouterForwardIndex = 0;
#define ClientRpcRouterForwardMethod  ::ClientRpcRouter_Stub::descriptor()->method(0)
