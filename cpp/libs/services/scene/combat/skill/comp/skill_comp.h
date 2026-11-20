#pragma once
#include <memory>
#include <unordered_map>
#include "time/comp/timer_task_comp.h"
#include "proto/common/component/skill_comp.pb.h"

// 施法相位定时器(前摇 / 引导 / 后摇)是按需状态组件,生命周期只有两个出入口(F051):
// - 相位开始时 emplace_or_replace:SetupCastingTimer(前摇)、HandleChannelSkillSpell(引导)、
//   HandleSkillRecovery(后摇)。被顶掉的旧组件经移动赋值取消它的旧定时器。
// - 相位结束或被打断时 remove:前摇 / 后摇在各自定时器回调里摘,引导在 HandleChannelFinish 里摘,
//   打断(或发现定时器已不在跑的残留组件)时,CheckTimerPhase 收掉这次施法的上下文并把全部相位一起摘。
// 于是「组件在」⇔「这一相位正在进行」,读它的地方(CheckTimerPhase)只用 try_get,
// 不存在是正常分支(没在这一相位),不是缺必需组件。
//
// CooldownTimeListComp / SkillContextCompMap 同样按需存在:分别在 StartCooldown 与
// AddSkillContext 第一次写入时创建;读取缺失表示没有冷却 / 在途上下文。
// 冷却列表随玩家存档保留,加载时存档里有记录也会由 SkillSystem::RestoreCooldowns 挂上
// (docs/design/realtime-cooldown-buff-persistence.md)。

// Pre-cast timer
struct CastingTimerComp
{
    // TimerTaskComp 移动时转移调度状态，保留原地删除仅为维持已有组件指针稳定。
    static constexpr bool in_place_delete = true;
    TimerTaskComp timer;
    // 这一相位正在跑的技能实例 id(不是技能表 id)。被打断时要靠它把
    // SkillContextCompMap 里的在途上下文摘掉,否则永远没人删。
    uint64_t skillId{0};
};

// Post-cast recovery timer
struct RecoveryTimerComp
{
    static constexpr bool in_place_delete = true;
    TimerTaskComp timer;
    uint64_t skillId{0};
};

// Channel finish timer
struct ChannelFinishTimerComp
{
    static constexpr bool in_place_delete = true;
    TimerTaskComp timer;
    uint64_t skillId{0};
};
// Channel interval timer
struct ChannelIntervalTimerComp
{
    static constexpr bool in_place_delete = true;
    TimerTaskComp timer;
};

using SkillContextPtrComp = std::shared_ptr<SkillContextComp>;

// 仅保存本实体作为施法者的在途上下文;目标不存镜像,销毁施法者时自然回收。
struct SkillContextCompMap : std::unordered_map<uint64_t, SkillContextPtrComp>
{
    using unordered_map::unordered_map;
};
