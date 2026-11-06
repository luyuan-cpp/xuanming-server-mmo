#pragma once
#include <cstdint>

#include "proto/chat/chat.pb.h"

constexpr uint32_t ClientPlayerChatSendChatMessageId = 61;
constexpr uint32_t ClientPlayerChatSendChatIndex = 0;
#define ClientPlayerChatSendChatMethod  ::ClientPlayerChat_Stub::descriptor()->method(0)

constexpr uint32_t ClientPlayerChatPullChatHistoryMessageId = 28;
constexpr uint32_t ClientPlayerChatPullChatHistoryIndex = 1;
#define ClientPlayerChatPullChatHistoryMethod  ::ClientPlayerChat_Stub::descriptor()->method(1)

constexpr uint32_t ClientPlayerChatNotifyWorldChatMessageId = 383;
constexpr uint32_t ClientPlayerChatNotifyWorldChatIndex = 2;
#define ClientPlayerChatNotifyWorldChatMethod  ::ClientPlayerChat_Stub::descriptor()->method(2)
