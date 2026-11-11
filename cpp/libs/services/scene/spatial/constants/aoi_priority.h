#pragma once

#include <array>
#include <cstdint>

// ---------------------------------------------------------------------------
// AOI interest priority — semantic tags, not hard ordinals.
// The actual ordering is determined by AoiPriorityPolicy per scene type.
// Reference: https://zhuanlan.zhihu.com/p/148077453
// ---------------------------------------------------------------------------
enum class AoiPriority : uint8_t
{
    kNormal      = 0,   // Default: entities discovered via spatial grid query
    kTeammate    = 1,   // Party/team members
    kCombat      = 2,   // 交手对象:我打的、打我的、我治疗的、治疗我的(CombatEngagementSystem 登记,脱战撤销)
    kQuestNpc    = 3,   // NPCs related to active quests/missions
    kBoss        = 4,   // Boss / elite monsters
    kPinned      = 5,   // 显式关注的最高权重(如技能追踪鹰眼术、暗杀目标),登记在 AoiFollowGrant 上

    kCount       = 6,   // Sentinel — always last
};

// ---------------------------------------------------------------------------
// Priority policy — maps each AoiPriority tag to a numeric weight.
// Higher weight = harder to evict. Scenes pick a policy at creation time.
// kPinned always gets max weight so it can never be evicted by spatial logic.
// 标签只决定淘汰权重;来源(空间或显式)与可见条件见 spatial/comp/scene_node_scene_comp.h 的 AoiEntry。
// ---------------------------------------------------------------------------
struct AoiPriorityPolicy
{
    // weight[static_cast<uint8_t>(tag)] → eviction weight
    std::array<uint8_t, static_cast<std::size_t>(AoiPriority::kCount)> weight{};

    [[nodiscard]] uint8_t GetWeight(AoiPriority p) const
    {
        return weight[static_cast<uint8_t>(p)];
    }
};

// ---- Built-in policies ----
//
// 权重表是静态的:一条关注的权重只由「为什么关注它」(各来源的标签)决定,不随观察者的状态切换表。
// 「战斗时先看交手的人、脱战后任务 NPC 回到最前」靠来源有寿命实现:交手来源(kCombat)只在交手期间存在,
// 所以 kCombat 在每张表里都是除 kPinned 外最高的。按观察者状态切换权重表是被否掉的做法,
// 理由见 docs/design/aoi-directed-interest.md「被否掉的做法：按观察者状态切换权重表」。

// Open-world default: combat > boss > quest NPC > teammate > normal
inline constexpr AoiPriorityPolicy kPolicyOpenWorld {{
    /* kNormal   */ 0,
    /* kTeammate */ 1,
    /* kCombat   */ 5,
    /* kQuestNpc */ 3,
    /* kBoss     */ 4,
    /* kPinned   */ 255,
}};

// Dungeon / instance: combat > boss > teammate > quest NPC > normal
inline constexpr AoiPriorityPolicy kPolicyDungeon {{
    /* kNormal   */ 0,
    /* kTeammate */ 2,
    /* kCombat   */ 5,
    /* kQuestNpc */ 1,
    /* kBoss     */ 4,
    /* kPinned   */ 255,
}};

// PvP arena: combat > boss > teammate > quest NPC > normal
inline constexpr AoiPriorityPolicy kPolicyPvpArena {{
    /* kNormal   */ 0,
    /* kTeammate */ 2,
    /* kCombat   */ 4,
    /* kQuestNpc */ 1,
    /* kBoss     */ 3,
    /* kPinned   */ 255,
}};

// 交手来源在每张表里都必须高于除 kPinned 外的所有标签(上方说明的前提),改表时由编译期守住。
constexpr bool CombatOutranksAllButPinned(const AoiPriorityPolicy& policy)
{
    const auto combatIndex = static_cast<std::size_t>(AoiPriority::kCombat);
    const auto pinnedIndex = static_cast<std::size_t>(AoiPriority::kPinned);
    for (std::size_t i = 0; i < policy.weight.size(); ++i)
    {
        if (i != combatIndex && i != pinnedIndex && policy.weight[i] >= policy.weight[combatIndex])
        {
            return false;
        }
    }
    return policy.weight[pinnedIndex] > policy.weight[combatIndex];
}

static_assert(CombatOutranksAllButPinned(kPolicyOpenWorld) && CombatOutranksAllButPinned(kPolicyDungeon) &&
                  CombatOutranksAllButPinned(kPolicyPvpArena),
              "kCombat must outrank every tag except kPinned in every AOI priority policy");

// ---------------------------------------------------------------------------
// Policy handle — stored on the scene as a plain enum (no pointer/reference),
// resolved to a static policy via PolicyOf(). Avoids dangling references.
// ---------------------------------------------------------------------------
enum class AoiPolicyKind : uint8_t
{
    kOpenWorld = 0,
    kDungeon   = 1,
    kPvpArena  = 2,
};

[[nodiscard]] inline const AoiPriorityPolicy& PolicyOf(AoiPolicyKind kind)
{
    switch (kind)
    {
    case AoiPolicyKind::kDungeon:  return kPolicyDungeon;
    case AoiPolicyKind::kPvpArena: return kPolicyPvpArena;
    case AoiPolicyKind::kOpenWorld:
    default:                       return kPolicyOpenWorld;
    }
}

// ---------------------------------------------------------------------------
// Dynamic AOI capacity — resolved per-entity per-frame.
// effectiveCapacity = min(clientReportedCapacity, serverPressureCapacity)
// ---------------------------------------------------------------------------
constexpr std::size_t kAoiListCapacityDefault = 100;
constexpr uint32_t kAoiListCapacityMin     = 20;
constexpr uint32_t kAoiListCapacityMax     = 200;

// ---------------------------------------------------------------------------
// 服务器压力(AoiLoadMonitor,spatial/system/aoi_load.h):模拟步耗时 / 步长 = 利用率,
// 指数平均后映射成压力因子,写进 ScenePressureComp 调容量。
// 默认值没有压测数据支撑,上线前随 AOI 压测校准(AGENTS.md §6)。
// ---------------------------------------------------------------------------
constexpr double kAoiLoadSmoothing = 0.05;              // 每步的指数平均权重:20 FPS 下时间常数约 1 秒
constexpr double kAoiLoadDecisionIntervalSeconds = 1.0; // 压力档位最多每秒变一次
constexpr double kAoiPressureLowUtilization = 0.5;      // 平滑利用率到这里之前压力为 0
constexpr double kAoiPressureHighUtilization = 0.9;     // 到这里压力为 1,容量降到 kAoiListCapacityMin
constexpr int kAoiPressureLevels = 10;                  // 压力量化成 10 档
constexpr double kAoiPressureHysteresisLevels = 0.75;   // 偏离当前档位超过 3/4 档才换档,边界附近的抖动不改容量

static_assert(kAoiPressureLowUtilization < kAoiPressureHighUtilization, "AOI pressure thresholds must be ordered");
