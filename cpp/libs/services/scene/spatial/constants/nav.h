#pragma once

#include <cstdint>

constexpr uint32_t kMaxMeshQueryNodes = 4096;

// 客户端上报速度的信任上限(米/秒)。客户端基础移速 9(TianyongMapConfig
// .moveSpeed),留 ~10% 网络抖动余量。移速 buff 接入后应改为
// max(kMaxTrustedClientSpeed, MoveSpeedComp.moveSpeed * 1.1)。
constexpr double kMaxTrustedClientSpeed = 10.0;

// 上报位置与服务器裁决位置的水平差超过该值才回发 MoveAck 纠偏,
// 避免高度吸附的毫米级差异刷 ack。
// 取 2 个烘焙体素(cs=0.25m → 0.5m)作为容差:烘焙器在 painted-city 模式已跳过
// rcFilterLedgeSpans,网格边界与客户端 mask 格线精确重合,剩下的只有阻挡点回退
// (nav_query.cpp kHitPullback 0.05m)和浮点/高度吸附误差;留 0.5m 是不让贴墙停下
// 的毫米级差异刷 ack(客户端对 <1.5m 的 ack 在移动中不应用,只会每 0.25s 一条日志)。
// 改烘焙 cs 时同步改这里。
constexpr double kMoveCorrectionEpsilon = 0.5;

// ---------------------------------------------------------------------------
// 场景出生点(服务器坐标,Z-up:x 前 y 右 z 上)。
//
// 天墉城出生点与客户端 TianyongMapDefinition.DefaultSpawn 同源:
//   Unity (x=200, y=0, z=180)  →  服务器 (x=unity.z, y=unity.x, z=unity.y)
//                              =  (180, 200, 0)
// 换轴规则见 nav_query.h / 客户端 WorldCoordinateConverter,两端不能各转各的。
// 该点落在中央大道(客户端 WalkMask 里 x∈[190,206] 的可走走廊),烘焙器
// --painted-city 模式会在出包时探针校验它在网格上(navmesh_baker --probe)。
//
// 各地图出生点的权威来源是 BaseScene 表 spawn_x/y/z,由
// SceneSpawnSystem::DefaultSpawnFor 唯一读取。下列常量只用于未载表/旧版表
// 兼容与无场景兜底,新地图不得再追加并行常量。
// ---------------------------------------------------------------------------
constexpr double kTianyongSpawnX = 180.0;
constexpr double kTianyongSpawnY = 200.0;
constexpr double kTianyongSpawnZ = 0.0;
