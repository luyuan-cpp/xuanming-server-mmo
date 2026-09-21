#pragma once
#include <cstdint>

#include "proto/friend/friend.pb.h"

constexpr uint32_t ClientPlayerFriendAddFriendMessageId = 234;
constexpr uint32_t ClientPlayerFriendAddFriendIndex = 0;
#define ClientPlayerFriendAddFriendMethod  ::ClientPlayerFriend_Stub::descriptor()->method(0)

constexpr uint32_t ClientPlayerFriendAcceptFriendMessageId = 238;
constexpr uint32_t ClientPlayerFriendAcceptFriendIndex = 1;
#define ClientPlayerFriendAcceptFriendMethod  ::ClientPlayerFriend_Stub::descriptor()->method(1)

constexpr uint32_t ClientPlayerFriendRejectFriendMessageId = 232;
constexpr uint32_t ClientPlayerFriendRejectFriendIndex = 2;
#define ClientPlayerFriendRejectFriendMethod  ::ClientPlayerFriend_Stub::descriptor()->method(2)

constexpr uint32_t ClientPlayerFriendRemoveFriendMessageId = 11;
constexpr uint32_t ClientPlayerFriendRemoveFriendIndex = 3;
#define ClientPlayerFriendRemoveFriendMethod  ::ClientPlayerFriend_Stub::descriptor()->method(3)

constexpr uint32_t ClientPlayerFriendGetFriendListMessageId = 12;
constexpr uint32_t ClientPlayerFriendGetFriendListIndex = 4;
#define ClientPlayerFriendGetFriendListMethod  ::ClientPlayerFriend_Stub::descriptor()->method(4)

constexpr uint32_t ClientPlayerFriendGetPendingRequestsMessageId = 230;
constexpr uint32_t ClientPlayerFriendGetPendingRequestsIndex = 5;
#define ClientPlayerFriendGetPendingRequestsMethod  ::ClientPlayerFriend_Stub::descriptor()->method(5)

constexpr uint32_t ClientPlayerFriendBlockMessageId = 7;
constexpr uint32_t ClientPlayerFriendBlockIndex = 6;
#define ClientPlayerFriendBlockMethod  ::ClientPlayerFriend_Stub::descriptor()->method(6)

constexpr uint32_t ClientPlayerFriendUnblockMessageId = 236;
constexpr uint32_t ClientPlayerFriendUnblockIndex = 7;
#define ClientPlayerFriendUnblockMethod  ::ClientPlayerFriend_Stub::descriptor()->method(7)

constexpr uint32_t ClientPlayerFriendListBlocksMessageId = 2;
constexpr uint32_t ClientPlayerFriendListBlocksIndex = 8;
#define ClientPlayerFriendListBlocksMethod  ::ClientPlayerFriend_Stub::descriptor()->method(8)

constexpr uint32_t ClientPlayerFriendRecommendFriendsMessageId = 119;
constexpr uint32_t ClientPlayerFriendRecommendFriendsIndex = 9;
#define ClientPlayerFriendRecommendFriendsMethod  ::ClientPlayerFriend_Stub::descriptor()->method(9)

constexpr uint32_t ClientPlayerFriendNotifyFriendEventMessageId = 235;
constexpr uint32_t ClientPlayerFriendNotifyFriendEventIndex = 10;
#define ClientPlayerFriendNotifyFriendEventMethod  ::ClientPlayerFriend_Stub::descriptor()->method(10)
