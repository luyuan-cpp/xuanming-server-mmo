#pragma once

#include <cstdint>

class ActivityScheduleTable;
class MissionTable;
class PlayerActivityInfo;

// 活动排期唯一判定入口。生产调用只在 scene EventLoop 上执行，不保留配表指针。
// 所有时间由调用者显式传入 UTC Unix 毫秒；未配置排期的活动保持未开放。
class PlayerActivityScheduleSystem {
public:
    // 只检查运营时间窗，不代替任务接取、冻结、完成或资产条件的校验。
    // 开放返回 kSuccess，合法但未开放返回 kFeatureUnavailable，配置错误返回对应 tip。
    static uint32_t CheckOpen(uint32_t missionId, uint64_t nowMs);

    // 输出整个活动目录记录；成功包括未排期、待开放和已结束。
    // can_participate 总是 false，由上层结合玩家任务能力设置。
    // 失败会清理旧输出并保留不可参与状态，不发布旧时间窗。
    static uint32_t BuildInfo(uint32_t missionId, uint64_t nowMs, PlayerActivityInfo& out);

    // 使用同一配表协议的纯计算入口，供确定性的边界测试与配置预检复用。
    // 不访问全局表、不修改输入、不保留引用；schedule 为 nullptr 表示未配置。
    static uint32_t BuildInfo(const MissionTable& mission, const ActivityScheduleTable* schedule,
                              uint64_t nowMs, PlayerActivityInfo& out);
};
