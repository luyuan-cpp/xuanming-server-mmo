#include <gtest/gtest.h>

#include <cstdint>
#include <limits>
#include <utility>

#include "proto/scene/player_activity.pb.h"
#include "services/scene/player/system/player_activity_schedule.h"
#include "table/code/activityschedule_table.h"
#include "table/code/mission_table.h"
#include "table/proto/tip/common_error_tip.pb.h"

namespace {

class PlayerActivityScheduleTest : public testing::Test {
protected:
    void SetUp() override
    {
        mission.set_id(15);
        mission.set_mission_type(2);
        mission.set_reward_id(7);
        schedule.set_id(15);
        schedule.set_enabled(true);
        schedule.set_baseline_start_at_ms(1900000000000);
        schedule.set_baseline_end_at_ms(1900000060000);
    }

    uint32_t BuildAt(uint64_t nowMs)
    {
        return PlayerActivityScheduleSystem::BuildInfo(mission, &schedule, nowMs, out);
    }

    MissionTable mission;
    ActivityScheduleTable schedule;
    PlayerActivityInfo out;
};

TEST_F(PlayerActivityScheduleTest, MissingScheduleRemainsUnscheduled)
{
    ASSERT_EQ(kSuccess, PlayerActivityScheduleSystem::BuildInfo(mission, nullptr, 1900000000001, out));
    EXPECT_EQ(PLAYER_ACTIVITY_UNSCHEDULED, out.status());
    EXPECT_EQ(15u, out.activity_id());
    EXPECT_EQ(15u, out.mission_id());
    EXPECT_EQ(7u, out.reward_id());
    EXPECT_EQ(0u, out.starts_at_ms());
    EXPECT_EQ(0u, out.ends_at_ms());
    EXPECT_FALSE(out.can_participate());
    EXPECT_FALSE(out.unavailable_reason().empty());
}

TEST_F(PlayerActivityScheduleTest, DisabledScheduleDoesNotExposeDatesOrParticipation)
{
    schedule.set_enabled(false);
    ASSERT_EQ(kSuccess, BuildAt(1900000000001));
    EXPECT_EQ(PLAYER_ACTIVITY_UNSCHEDULED, out.status());
    EXPECT_EQ(0u, out.starts_at_ms());
    EXPECT_EQ(0u, out.ends_at_ms());
    EXPECT_FALSE(out.can_participate());
    schedule.set_baseline_start_at_ms(0);
    schedule.set_baseline_end_at_ms(0);
    ASSERT_EQ(kSuccess, BuildAt(std::numeric_limits<uint64_t>::max()));
    EXPECT_EQ(PLAYER_ACTIVITY_UNSCHEDULED, out.status());
}

TEST_F(PlayerActivityScheduleTest, StartsInclusiveAndEndsExclusiveUsingServerTime)
{
    const auto start = schedule.baseline_start_at_ms();
    const auto end = schedule.baseline_end_at_ms();
    for (const auto now : {uint64_t{0}, start - 1})
    {
        ASSERT_EQ(kSuccess, BuildAt(now));
        EXPECT_EQ(PLAYER_ACTIVITY_UPCOMING, out.status());
        EXPECT_FALSE(out.unavailable_reason().empty());
    }
    for (const auto now : {start, end - 1})
    {
        ASSERT_EQ(kSuccess, BuildAt(now));
        EXPECT_EQ(PLAYER_ACTIVITY_OPEN, out.status());
        EXPECT_TRUE(out.unavailable_reason().empty());
        EXPECT_FALSE(out.can_participate());
        EXPECT_EQ(start, out.starts_at_ms());
        EXPECT_EQ(end, out.ends_at_ms());
    }
    for (const auto now : {end, std::numeric_limits<uint64_t>::max()})
    {
        ASSERT_EQ(kSuccess, BuildAt(now));
        EXPECT_EQ(PLAYER_ACTIVITY_ENDED, out.status());
        EXPECT_FALSE(out.unavailable_reason().empty());
    }
}

TEST_F(PlayerActivityScheduleTest, KeepsWideTimesWithoutNarrowingOrArithmeticOverflow)
{
    const auto end = std::numeric_limits<uint64_t>::max();
    schedule.set_baseline_start_at_ms(end - 10);
    schedule.set_baseline_end_at_ms(end);
    ASSERT_EQ(kSuccess, BuildAt(end - 1));
    EXPECT_EQ(PLAYER_ACTIVITY_OPEN, out.status());
    EXPECT_EQ(end - 10, out.starts_at_ms());
    EXPECT_EQ(end, out.ends_at_ms());
    ASSERT_EQ(kSuccess, BuildAt(end));
    EXPECT_EQ(PLAYER_ACTIVITY_ENDED, out.status());
}

TEST_F(PlayerActivityScheduleTest, RejectsEnabledEmptyWindow)
{
    schedule.set_baseline_start_at_ms(0);
    schedule.set_baseline_end_at_ms(0);
    EXPECT_EQ(kInvalidTableData, BuildAt(0));
    EXPECT_FALSE(out.can_participate());
    EXPECT_EQ(PLAYER_ACTIVITY_UNSCHEDULED, out.status());
}

TEST_F(PlayerActivityScheduleTest, RejectsPartialAndNonIncreasingWindowsEvenWhenDisabled)
{
    for (const bool enabled : {false, true})
    {
        schedule.set_enabled(enabled);
        for (const auto times : {std::pair<uint64_t, uint64_t>{0, 100},
                                 std::pair<uint64_t, uint64_t>{100, 0},
                                 std::pair<uint64_t, uint64_t>{100, 100},
                                 std::pair<uint64_t, uint64_t>{101, 100}})
        {
            schedule.set_baseline_start_at_ms(times.first);
            schedule.set_baseline_end_at_ms(times.second);
            EXPECT_EQ(kInvalidTableData, BuildAt(100));
            EXPECT_EQ(0u, out.starts_at_ms());
            EXPECT_EQ(0u, out.ends_at_ms());
            EXPECT_FALSE(out.can_participate());
        }
    }
}

TEST_F(PlayerActivityScheduleTest, RejectsNonActivityZeroIdentityAndMismatchedForeignKey)
{
    mission.set_mission_type(1);
    EXPECT_EQ(kInvalidTableData, BuildAt(1900000000001));
    mission.set_mission_type(2);
    mission.set_id(0);
    EXPECT_EQ(kInvalidTableData, BuildAt(1900000000001));
    mission.set_id(15);
    schedule.set_id(16);
    EXPECT_EQ(kInvalidTableData, BuildAt(1900000000001));
    EXPECT_EQ(0u, out.activity_id());
    EXPECT_FALSE(out.can_participate());
}

TEST_F(PlayerActivityScheduleTest, RejectClearsPriorOpenSnapshot)
{
    ASSERT_EQ(kSuccess, BuildAt(1900000000001));
    out.set_can_participate(true);
    schedule.set_baseline_end_at_ms(1);
    EXPECT_EQ(kInvalidTableData, BuildAt(1900000000001));
    EXPECT_EQ(PLAYER_ACTIVITY_UNSCHEDULED, out.status());
    EXPECT_EQ(0u, out.starts_at_ms());
    EXPECT_EQ(0u, out.ends_at_ms());
    EXPECT_EQ(0u, out.reward_id());
    EXPECT_FALSE(out.can_participate());
    EXPECT_FALSE(out.unavailable_reason().empty());
}

TEST(PlayerActivityScheduleProductionTableTest, ExistingActivitiesStayDisabledAndNoOtherMissionCanEnter)
{
    MissionTableManager::Instance().Load();
    ActivityScheduleTableManager::Instance().Load();
    for (uint32_t missionId : {15u, 16u, 17u})
    {
        PlayerActivityInfo out;
        ASSERT_EQ(kSuccess, PlayerActivityScheduleSystem::BuildInfo(missionId, 1900000000000, out));
        EXPECT_EQ(PLAYER_ACTIVITY_UNSCHEDULED, out.status());
        EXPECT_FALSE(out.can_participate());
        EXPECT_EQ(kFeatureUnavailable, PlayerActivityScheduleSystem::CheckOpen(missionId, 1900000000000));
    }
    EXPECT_EQ(kInvalidTableId, PlayerActivityScheduleSystem::CheckOpen(999999, 1900000000000));
    EXPECT_EQ(kInvalidTableData, PlayerActivityScheduleSystem::CheckOpen(1, 1900000000000));
}

} // namespace
