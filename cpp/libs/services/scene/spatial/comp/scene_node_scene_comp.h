#pragma once

#include <algorithm>
#include <cstdint>
#include <unordered_map>
#include <vector>

#include "engine/core/type_define/type_define.h"
#include "spatial/constants/aoi_priority.h"

// ---------------------------------------------------------------------------
// AOI 单向关注(docs/design/aoi-directed-interest.md)。
// 关注是有向的:观察者 A 关注目标 B,A 的客户端显示 B、接收 B 的移动与属性;B 是否关注 A 是另一条关系。
// 一条关系的来源分两类:
//   - 空间来源:网格评估(本格及相邻六格 + CanSee)认定 A 能看见 B,由 AoiSystem 维护;
//   - 显式来源:玩法按需建立和撤销(技能追踪、队伍、任务……),可以超出网格范围。
// 下面两张索引描述同一批关系,只由 InterestSystem 写:正向在观察者身上,反向在目标身上。
// ---------------------------------------------------------------------------

// 显式来源的种类。新增玩法接入时在 kCount 之前加一项,kCount 固定最后。
enum class AoiFollowKind : uint8_t
{
    kTeam  = 0,   // 队伍逻辑建立的关注:id 为 team_id
    kSkill = 1,   // 技能或 Buff 效果建立的关注:id 为该效果的实例号(如 buff_id)
    kCount,
};

// 显式来源的身份。同一观察者与目标之间,同一身份最多登记一份;撤销时只撤这一份。
struct AoiFollowSource
{
    AoiFollowKind kind = AoiFollowKind::kTeam;
    uint64_t id = 0;

    friend bool operator==(const AoiFollowSource&, const AoiFollowSource&) = default;
};

// 一个显式来源的登记内容。
struct AoiFollowGrant
{
    AoiFollowSource source;
    AoiPriority priority = AoiPriority::kNormal;  // 参与容量淘汰的权重标签
    bool revealsStealth = false;                  // 玩法明确授予的侦测能力:目标隐身时仍可见
};

// 观察者对一个目标的关注条目(正向索引的值)。
// 不变量(InterestSystem 维护):
//   - 条目存在 ⇔ spatial 为真或 follows 非空;
//   - spatial 为真 ⇒ visible 为真(空间来源以"能看见"为前提,看不见就连同空间来源一起撤掉);
//   - visible 为真 ⇒ 目标未隐身,或某个显式来源授予了侦测能力;且占用观察者的一个容量名额。
struct AoiEntry
{
    AoiPriority priority = AoiPriority::kNormal;  // 空间来源的标签,由网格评估写入;spatial 为假时恒为 kNormal
    bool spatial = false;                         // 空间来源是否存在
    bool visible = false;                         // 观察者客户端当前是否显示该目标
    std::vector<AoiFollowGrant> follows;          // 显式来源,通常为 0~2 个

    [[nodiscard]] bool RevealsStealth() const
    {
        return std::ranges::any_of(follows, [](const AoiFollowGrant& grant) { return grant.revealsStealth; });
    }
};

// 正向索引:观察者关注的全部目标,含暂时不可见的显式关注(隐身、容量不足)。
// 按需状态,只在 InterestSystem 第一次建立关系时创建;外部只读。
struct AoiListComp
{
    std::unordered_map<entt::entity, AoiEntry> entries;
    std::size_t visibleCount = 0;  // visible 为真的条目数,即已占用的容量

    // 观察者客户端当前是否显示 target。
    [[nodiscard]] bool Contains(entt::entity target) const
    {
        const auto it = entries.find(target);
        return it != entries.end() && it->second.visible;
    }

    // 是否存在关注关系(可能暂时不可见)。
    [[nodiscard]] bool HasRelation(entt::entity target) const { return entries.contains(target); }

    // 当前显示中的目标数(占用的容量)。
    [[nodiscard]] std::size_t Size() const { return visibleCount; }
};

// 反向索引:关注本实体的观察者 → 该关系当前是否可见。与各观察者 AoiListComp 的条目一一对应。
// 按需状态,只由 InterestSystem 维护;移动、属性和技能广播按值为 true 的观察者投递,
// 离场、销毁按它找到全部入边,不扫描全节点。
struct AoiWatchersComp
{
    std::unordered_map<entt::entity, bool> watchers;
};

// ---------------------------------------------------------------------------
// Per-entity: client-reported display count (how many entities the client wants
// to see).  Updated when the client sends a settings RPC.
// ---------------------------------------------------------------------------
struct AoiClientCapacityComp
{
    uint32_t clientDesiredCount = kAoiListCapacityDefault;
};

// ---------------------------------------------------------------------------
// Per-scene: server pressure metric.
// pressureFactor in [0, 1]: 0 = idle, 1 = maximum pressure.
// GetServerCapacity() linearly interpolates between max and min capacity.
// ---------------------------------------------------------------------------
struct ScenePressureComp
{
    float pressureFactor = 0.0f;

    [[nodiscard]] std::size_t GetServerCapacity() const
    {
        const auto range = static_cast<float>(kAoiListCapacityMax - kAoiListCapacityMin);
        const auto cap = static_cast<uint32_t>(kAoiListCapacityMax - pressureFactor * range);
        return std::clamp(cap, kAoiListCapacityMin, kAoiListCapacityMax);
    }
};

// ---------------------------------------------------------------------------
// Per-scene: active priority policy.  Defaults to open-world ordering.
// Set once at scene creation (e.g. dungeon vs arena vs open-world).
// ---------------------------------------------------------------------------
struct ScenePriorityPolicyComp
{
    AoiPolicyKind policy = AoiPolicyKind::kOpenWorld;
};
