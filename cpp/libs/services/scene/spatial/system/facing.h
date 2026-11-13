#pragma once

#include <cmath>
#include <numbers>
#include <optional>

#include "proto/common/component/actor_comp.pb.h"  // Rotation

// 角色朝向的唯一口径。服务器自己写朝向的地方(技能面向目标 ViewSystem::LookAtPosition、
// 组队跟随沿行进方向 TeamFollowSystem)都经这里,不各自换算。
//
// 契约(与客户端 WorldCoordinateConverter 是同一份,改动要两端同批):
//   - 服务器坐标 z-up:x 前、y 右、z 上;Unity 是 y-up,两边换轴见 spatial/system/nav_query.h。
//   - Rotation 的第 i 个分量 = 绕服务器第 i 根轴的转角,单位是**度**:
//       z = 偏航(Unity eulerAngles.y),朝向向量 = (cos z, sin z);
//       x = 翻滚(Unity eulerAngles.z),y = 俯仰(Unity eulerAngles.x)。
//   - 角色只转偏航:x、y 恒写 0。目标在高处或低处也不让人前倾后仰(那是瞄准,不是朝向)。
// 玩家自己的朝向以客户端上报为准(移动 handler 原样存),这里只管服务器替角色定朝向的场合。
namespace facing
{

// 水平位移不超过它(米)就没有方向:原地、目标在正上方或正下方。此时调用方保留原朝向。
inline constexpr double kMinHorizontalDistance = 1e-6;

// 从当前位置朝 (dx, dy) 方向看过去的偏航角,单位度,范围 [-180, 180]。没有水平方向时返回空。
inline std::optional<double> YawDegreesToward(const double dx, const double dy)
{
	if (dx * dx + dy * dy <= kMinHorizontalDistance * kMinHorizontalDistance)
	{
		return std::nullopt;
	}
	constexpr double kRadiansToDegrees = 180.0 / std::numbers::pi;
	return std::atan2(dy, dx) * kRadiansToDegrees;
}

// 把朝向设成偏航 yawDegrees:只有偏航,翻滚、俯仰清零。
inline void SetYaw(Rotation& rotation, const double yawDegrees)
{
	rotation.set_x(0.0);
	rotation.set_y(0.0);
	rotation.set_z(yawDegrees);
}

}  // namespace facing
