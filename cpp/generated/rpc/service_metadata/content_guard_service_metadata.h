#pragma once
#include <cstdint>

#include "proto/content_guard/content_guard.pb.h"

constexpr uint32_t ContentGuardCheckTextMessageId = 413;
constexpr uint32_t ContentGuardCheckTextIndex = 0;
#define ContentGuardCheckTextMethod  ::ContentGuard_Stub::descriptor()->method(0)

constexpr uint32_t ContentGuardCheckPlatformTextMessageId = 436;
constexpr uint32_t ContentGuardCheckPlatformTextIndex = 1;
#define ContentGuardCheckPlatformTextMethod  ::ContentGuard_Stub::descriptor()->method(1)
