#pragma once

#include <bitset>

#include "actor/attribute/constants/actor_state_attribute_calculator_constants.h"

struct AttributeDirtyFlagsComp
{
    std::bitset<kAttributeCalculatorMax> attributeBits;
};

// 移速**属性**(标量,单位:距离/秒),由移速类 buff 聚合而来。
//
// 千万不要把它写进 Velocity:Velocity 是 MovementSystem 每 tick 按
// location += velocity * delta 积分的**运动学矢量**。此前 UpdateVelocity
// 把这个标量灌进 velocity 的 x/y/z 三轴,挂移速 buff 的角色就沿 (1,1,1)
// 方向匀速漂移(减速 buff 则钻地),权威位置直接被破坏。属性归属性,
// 运动归运动,两个语义各自一个组件。
// 未来移动系统实现时,用 moveSpeed 去缩放输入方向得出 Velocity。
struct MoveSpeedComp
{
    double moveSpeed{0.0};
};

