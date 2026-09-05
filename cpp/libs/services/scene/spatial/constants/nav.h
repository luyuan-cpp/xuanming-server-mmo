#pragma once

#include <cstdint>

constexpr uint32_t kMaxMeshQueryNodes = 4096;

// 客户端上报速度的信任上限(米/秒)。客户端基础移速 9(TianyongMapConfig
// .moveSpeed),留 ~10% 网络抖动余量。移速 buff 接入后应改为
// max(kMaxTrustedClientSpeed, MoveSpeedComp.moveSpeed * 1.1)。
constexpr double kMaxTrustedClientSpeed = 10.0;

// 上报位置与服务器裁决位置的水平差超过该值才回发 MoveAck 纠偏,
// 避免高度吸附的毫米级差异刷 ack。取导航烘焙 cs(0.25m)。
constexpr double kMoveCorrectionEpsilon = 0.25;

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
// 目前所有场景配置共用同一张天墉城地图(BaseScene 表 nav_bin_file 全部指向
// main_scene.bin),所以出生点也只有一份;真正分场景后应迁到 BaseScene 表列
// (spawn_x/spawn_y/spawn_z),SceneSpawnSystem::DefaultSpawnFor 是唯一读取点。
// ---------------------------------------------------------------------------
constexpr double kTianyongSpawnX = 180.0;
constexpr double kTianyongSpawnY = 200.0;
constexpr double kTianyongSpawnZ = 0.0;
