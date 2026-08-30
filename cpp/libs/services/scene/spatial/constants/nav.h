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