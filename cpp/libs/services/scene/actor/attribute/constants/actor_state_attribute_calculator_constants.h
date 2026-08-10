#pragma once

#include <cstdint>

enum eAttributeCalculator : uint32_t {
    // 移速**属性**(标量)重算位。改名自 kVelocity:这个位驱动的是
    // UpdateMoveSpeed → MoveSpeedComp,与运动学矢量 Velocity 组件无关
    // (把两者混为一谈正是当年移速 buff 让角色漂移的根因,别再叫回去)。
    kMoveSpeed,
    kHealth,
    kEnergy,
    kDamage,
    kStatusEffect,
    kCombatState,
    kAttributeCalculatorMax
};