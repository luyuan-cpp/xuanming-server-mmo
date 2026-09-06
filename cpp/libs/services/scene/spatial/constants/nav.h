#pragma once

#include <cstdint>

constexpr uint32_t kMaxMeshQueryNodes = 4096;

// 客户端上报速度的信任上限(米/秒)。客户端基础移速 9(TianyongMapConfig
// .moveSpeed),留 ~10% 网络抖动余量。移速 buff 接入后应改为
// max(kMaxTrustedClientSpeed, MoveSpeedComp.moveSpeed * 1.1)。
constexpr double kMaxTrustedClientSpeed = 10.0;

// 上报位置与服务器裁决位置的水平差超过该值才回发 MoveAck 纠偏,
// 避免高度吸附的毫米级差异刷 ack。
// 取 2 个烘焙体素(cs=0.25m → 0.5m):烘焙器的 rcFilterLedgeSpans 会把紧邻
// 不可走格的那一圈体素当悬崖剔掉,网格边界比客户端 mask 格线向内缩 0.25m,
// 客户端贴墙停下时服务器的阻挡点(再回退 0.05m)最多差 ~0.3m —— 这是数据
// 契约的固有差值,不是作弊,不能为它刷 ack(客户端对 <1.5m 的 ack 在移动中
// 不应用,只会每 0.25s 一条日志)。改烘焙 cs 时同步改这里。
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
// 目前所有场景配置共用同一张天墉城地图:BaseScene 表 nav_bin_file 指向
// main(id 1-16)/dungeon(17-19)/mirror(20-21) 三个文件,但三份都是同一次
// 烘焙的拷贝(scene-navmesh-pipeline.md §6),所以出生点也只有一份,
// LoadNavBins 的出生点探针对三份都成立。真正分场景(副本/镜像烘自己的图)
// 后必须把出生点迁到 BaseScene 表列(spawn_x/spawn_y/spawn_z)并按行探针,
// 否则不含 (200,0,180) 的副本网格会被探针误拒;
// SceneSpawnSystem::DefaultSpawnFor 是唯一读取点。
// ---------------------------------------------------------------------------
constexpr double kTianyongSpawnX = 180.0;
constexpr double kTianyongSpawnY = 200.0;
constexpr double kTianyongSpawnZ = 0.0;
