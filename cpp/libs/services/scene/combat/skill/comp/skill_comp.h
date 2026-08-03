#pragma once
#include "time/comp/timer_task_comp.h"
#include "proto/common/component/skill_comp.pb.h"


// Pre-cast timer
struct CastingTimerComp
{
    // 内嵌 TimerTaskComp 的回调捕获自身地址。EnTT 不会把成员的存储特征
    // 传播给外层类型，因此每个包装组件都必须显式固定为原地删除。
    static constexpr bool in_place_delete = true;
    TimerTaskComp timer;
};

// Post-cast recovery timer
struct RecoveryTimerComp
{
    static constexpr bool in_place_delete = true;
    TimerTaskComp timer;
};

// Channel finish timer
struct ChannelFinishTimerComp
{
    static constexpr bool in_place_delete = true;
    TimerTaskComp timer;
};
// Channel interval timer
struct ChannelIntervalTimerComp
{
    static constexpr bool in_place_delete = true;
    TimerTaskComp timer;
};

using SkillContextPtrComp = std::shared_ptr<SkillContextComp>;

// Skill context container
using SkillContextCompMap = std::unordered_map<uint64_t, SkillContextPtrComp>;
