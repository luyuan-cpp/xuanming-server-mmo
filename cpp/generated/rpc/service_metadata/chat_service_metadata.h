#pragma once
#include <cstdint>

#include "proto/chat/chat.pb.h"

constexpr uint32_t ClientPlayerChatSendChatMessageId = 61;
constexpr uint32_t ClientPlayerChatSendChatIndex = 0;
#define ClientPlayerChatSendChatMethod  ::ClientPlayerChat_Stub::descriptor()->method(0)

constexpr uint32_t ClientPlayerChatPullChatHistoryMessageId = 28;
constexpr uint32_t ClientPlayerChatPullChatHistoryIndex = 1;
#define ClientPlayerChatPullChatHistoryMethod  ::ClientPlayerChat_Stub::descriptor()->method(1)

constexpr uint32_t ClientPlayerChatReportChatMessageId = 433;
constexpr uint32_t ClientPlayerChatReportChatIndex = 2;
#define ClientPlayerChatReportChatMethod  ::ClientPlayerChat_Stub::descriptor()->method(2)

constexpr uint32_t ClientPlayerChatRetractWorldChatMessageId = 434;
constexpr uint32_t ClientPlayerChatRetractWorldChatIndex = 3;
#define ClientPlayerChatRetractWorldChatMethod  ::ClientPlayerChat_Stub::descriptor()->method(3)

constexpr uint32_t ClientPlayerChatPublishSystemWorldNoticeMessageId = 545;
constexpr uint32_t ClientPlayerChatPublishSystemWorldNoticeIndex = 4;
#define ClientPlayerChatPublishSystemWorldNoticeMethod  ::ClientPlayerChat_Stub::descriptor()->method(4)

constexpr uint32_t ClientPlayerChatNotifyWorldChatMessageId = 383;
constexpr uint32_t ClientPlayerChatNotifyWorldChatIndex = 5;
#define ClientPlayerChatNotifyWorldChatMethod  ::ClientPlayerChat_Stub::descriptor()->method(5)

constexpr uint32_t ClientPlayerChatNotifyWorldChatRetractedMessageId = 435;
constexpr uint32_t ClientPlayerChatNotifyWorldChatRetractedIndex = 6;
#define ClientPlayerChatNotifyWorldChatRetractedMethod  ::ClientPlayerChat_Stub::descriptor()->method(6)
