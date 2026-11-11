#pragma once

// 空间视野口径(米,三维欧氏距离)。客户端可见,与 Java 版一致:
// Java 仓 docs/reference/mmorpg-client-contract-aoi.md §8「重判」、PARITY.md「视野(AOI)进出通知」行。

// 默认视野半径:距离不超过它(含等号)才进视野。没有 ViewRadius 组件的实体用它,
// 玩家进场时也用它写 ViewRadius(PlayerLifecycleSystem,player_lifecycle.cpp)。
constexpr double kMaxViewRadius = 10;

// 离开半径 = 视野半径 × 这个系数:已建立的空间关注,距离超过它才出视野。
// 进 ≤ 10 m、出 > 20 m 之间是滞后区间,目标在视野边界附近来回走时不会反复创建、移除。
constexpr double kAoiLeaveRadiusFactor = 2.0;

// 移动中(速度非零)的实体,相对上次视野评估的位置累计位移达到它才重新评估;
// 静止的实体(刚停步、被挪位)位置一变就在下一帧评估。移动中的进出视野因此最多晚约 1 m 位移,
// 换来人群里每人从每帧评估一次降到约每走 1 m 一次(Java 版出生点人群基准见 PARITY 同一行)。
constexpr double kAoiReevaluateDistance = 1.0;

// AOI 六边形格子的尺寸:格心到顶点的距离。本格加六个邻格对格内任意一点至少覆盖这么远
// (最坏是站在本格顶点上),所以视野半径不能超过它,否则半径内的目标可能不在候选格里、永远进不了视野。
// 离开按距离判,不依赖格子,不受这条约束。
constexpr double kAoiHexSize = 20;

static_assert(kMaxViewRadius <= kAoiHexSize,
              "AOI view radius must stay within the guaranteed coverage of the 7-hex candidate cells");
