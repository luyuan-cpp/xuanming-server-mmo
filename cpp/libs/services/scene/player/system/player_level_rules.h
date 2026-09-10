#pragma once

// 角色等级的合法范围(纯规则,零 ECS / 零表依赖,可单测),设计文档
// docs/design/player-attribute-allocation.md §2.2。
//
// 上限是策划定的硬约束(2026-09-10 由 200 改为 85)。"写等级"与"读存档等级"的入口都经这里:
//   - GmSetPlayerLevel 用 IsValidLevel 拒绝越界请求;
//   - 登录 / 跨 zone 落地 / 回档都走 PlayerDatabaseMessageFieldsUnmarshal,用 ClampToMaxLevel 把上限下调前
//     留下的超限存档压回上限(随后属性系统的"已分配 > 总量"收敛会整池返还多出的点)。
// 只在 GM 入口卡上限拦不住回档和老存档,所以读存档这一侧必须同样收口。
// 经验系统落地后升级循环同样以 kMaxLevel 封顶;若改为表驱动,只改这一处。
//
// 单测:cpp/tests/turn_battle_engine_test/attribute_allocation_rules_test.cpp(PlayerLevelRulesTest)。

#include <cstdint>

namespace playerlevel {

inline constexpr uint32_t kMaxLevel = 85;

// 可写入的等级:1..kMaxLevel(0 不是合法等级)
inline bool IsValidLevel(uint32_t level) {
    return level >= 1 && level <= kMaxLevel;
}

// 存档等级压回上限。0 原样返回:读取侧 PlayerLevel 已把 0 当 1,这里不改写
inline uint32_t ClampToMaxLevel(uint32_t level) {
    return level > kMaxLevel ? kMaxLevel : level;
}

}  // namespace playerlevel
