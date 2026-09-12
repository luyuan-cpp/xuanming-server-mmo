#include "player_activity_schedule.h"

#include "proto/scene/player_activity.pb.h"
#include "table/code/activityschedule_table.h"
#include "table/code/mission_table.h"
#include "table/proto/tip/common_error_tip.pb.h"

namespace {

uint32_t RejectConfiguration(PlayerActivityInfo& out, uint32_t error)
{
    out.Clear();
    out.set_unavailable_reason("活动配置异常，暂不可参与");
    return error;
}

} // namespace

uint32_t PlayerActivityScheduleSystem::CheckOpen(uint32_t missionId, uint64_t nowMs)
{
    PlayerActivityInfo info;
    const auto result = BuildInfo(missionId, nowMs, info);
    if (result != kSuccess) return result;
    return info.status() == PLAYER_ACTIVITY_OPEN ? kSuccess : kFeatureUnavailable;
}

uint32_t PlayerActivityScheduleSystem::BuildInfo(uint32_t missionId, uint64_t nowMs,
                                               PlayerActivityInfo& out)
{
    const auto [mission, missionResult] = MissionTableManager::Instance().FindByIdSilent(missionId);
    if (mission == nullptr) return RejectConfiguration(out, kInvalidTableId);
    const auto [schedule, scheduleResult] = ActivityScheduleTableManager::Instance().FindByIdSilent(missionId);
    return BuildInfo(*mission, schedule, nowMs, out);
}

uint32_t PlayerActivityScheduleSystem::BuildInfo(const MissionTable& mission,
                                               const ActivityScheduleTable* schedule,
                                               uint64_t nowMs, PlayerActivityInfo& out)
{
    out.Clear();
    if (mission.id() == 0 || mission.mission_type() != 2 ||
        (schedule != nullptr && schedule->id() != mission.id()))
        return RejectConfiguration(out, kInvalidTableData);

    if (schedule != nullptr)
    {
        const auto startMs = schedule->baseline_start_at_ms();
        const auto endMs = schedule->baseline_end_at_ms();
        const bool emptyWindow = startMs == 0 && endMs == 0;
        if ((!emptyWindow && (startMs == 0 || endMs <= startMs)) ||
            (schedule->enabled() && emptyWindow))
            return RejectConfiguration(out, kInvalidTableData);
    }

    out.set_activity_id(mission.id());
    out.set_mission_id(mission.id());
    out.set_reward_id(mission.reward_id());
    // 合法未排期不暴露停用的旧时间，避免客户端把它显示为有效安排。
    if (schedule == nullptr || !schedule->enabled())
    {
        out.set_status(PLAYER_ACTIVITY_UNSCHEDULED);
        out.set_unavailable_reason("活动尚未排期，敬请期待");
        return kSuccess;
    }

    out.set_starts_at_ms(schedule->baseline_start_at_ms());
    out.set_ends_at_ms(schedule->baseline_end_at_ms());
    if (nowMs < out.starts_at_ms())
    {
        out.set_status(PLAYER_ACTIVITY_UPCOMING);
        out.set_unavailable_reason("活动尚未开始");
    }
    else if (nowMs >= out.ends_at_ms())
    {
        out.set_status(PLAYER_ACTIVITY_ENDED);
        out.set_unavailable_reason("活动已结束");
    }
    else
    {
        out.set_status(PLAYER_ACTIVITY_OPEN);
    }
    return kSuccess;
}
