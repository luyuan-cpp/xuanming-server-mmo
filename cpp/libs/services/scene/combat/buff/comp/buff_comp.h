#pragma once

#include <memory>
#include <string>
#include <unordered_map>
#include "time/comp/timer_task_comp.h"
#include "proto/common/component/buff_comp.pb.h"

using BuffMessagePtr = std::shared_ptr<google::protobuf::Message>;

struct BuffEntry
{
    BuffComp buffPb;
    TimerTaskComp expireTimerTaskComp;
    BuffMessagePtr dataPbPtr;
    // 到期的 UTC 毫秒,与到期定时器同一处设置;0 = 永久。定时器不跨存盘,存档按它写(下线保留的 Buff)。
    uint64_t expireAtMs{0};
    // 从存档恢复的条目带回的未知字段(更新版本加的 SavedBuffComp 字段),存盘时原样并回;其余条目为空。
    std::string persistUnknown;
};

struct BuffListComp : std::unordered_map<uint64_t, BuffEntry>
{
    using unordered_map::unordered_map;
};

// 存档里读出来、还没生效的下线保留 Buff(docs/design/realtime-cooldown-buff-persistence.md §6)。按需状态:
// 加载时存档里有条目才挂;InitPlayerFromAllData 在实体初始化完之后经 BuffSystem::RestoreSavedBuffs 消费并摘掉。
// 存盘时还挂着(恢复没跑到)就原样写回,条目不丢。
struct SavedBuffsPendingComp
{
    google::protobuf::RepeatedPtrField<SavedBuffComp> buffs;
};

// Tag component: present when the entity has at least one active stealth buff.
// Maintained by BuffSystem::OnBuffStart / OnBuffRemove to avoid per-tick
// linear scan + table lookup in ViewSystem::IsStealthed.
struct StealthedTagComp {};
