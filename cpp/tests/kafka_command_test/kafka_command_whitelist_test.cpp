#include <gtest/gtest.h>

#include <algorithm>
#include <array>
#include <cstdint>
#include <limits>

#include "rpc/service_metadata/kafka_command_event_whitelist.h"
#include "rpc/service_metadata/rpc_event_registry.h"

namespace
{
// 固定已发布事件号，既检查生产入口，也防止墓碑或内部事件重新进入白名单。
// 49 = PlayerInboxNotifyEvent（player_inbox 在线提醒）。
// 42 = BattleSettlementEvent 已退役：结算只经收件箱，event_id.txt 里只留墓碑号，不再是 scene 入口。
constexpr std::array<uint32_t, 3> kSceneEvents{45, 48, 49};
constexpr std::array<uint32_t, 9> kGateEvents{10, 11, 18, 32, 34, 37, 38, 39, 40};

template <size_t Size>
bool Contains(const std::array<uint32_t, Size>& events, uint32_t value)
{
    return std::find(events.begin(), events.end(), value) != events.end();
}
}

TEST(KafkaCommandWhitelist, SceneAcceptsOnlyItsPublishedIngressEvents)
{
    for (uint32_t eventId = 0; eventId <= kMaxEventCount; ++eventId)
        EXPECT_EQ(Contains(kSceneEvents, eventId), IsSceneCommandEvent(eventId)) << eventId;
    EXPECT_FALSE(IsSceneCommandEvent(std::numeric_limits<uint32_t>::max()));
}

TEST(KafkaCommandWhitelist, GateRejectsInternalAndOtherTopicEvents)
{
    for (uint32_t eventId = 0; eventId <= kMaxEventCount; ++eventId)
        EXPECT_EQ(Contains(kGateEvents, eventId), IsGateCommandEvent(eventId)) << eventId;
    EXPECT_FALSE(IsGateCommandEvent(std::numeric_limits<uint32_t>::max()));
}
