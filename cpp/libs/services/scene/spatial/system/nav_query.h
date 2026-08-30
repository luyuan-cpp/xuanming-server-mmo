#pragma once

#include <vector>

#include "entt/src/entt/entity/entity.hpp"

#include "proto/common/component/actor_comp.pb.h"

struct NavComp;

// 导航网格查询封装:寻路 / 阻挡点校验 / 网格吸附。
//
// 坐标系约定(与客户端 WorldCoordinateConverter 完全一致,不能各转各的):
//   服务器是 Z-up(x 前 y 右 z 上),导航网格烘焙在 Unity 坐标系(Y-up,米,
//   tools/navmesh_baker 产出)。换轴:
//     nav    = (server.y, server.z, server.x)
//     server = (nav.z,    nav.x,    nav.y)
// 本类所有对外接口收发**服务器坐标**,内部完成换轴,调用方不要自己转。
class NavQuerySystem
{
public:
	// 玩家实体 → 所在场景的导航(SceneEntityComp → SceneInfoComp.scene_config_id
	// → SceneNavManager)。返回 nullptr 表示该场景没有烘焙导航(fail-open:
	// 调用方应放行移动并降级为无校验,不能把玩家卡死)。
	static NavComp* GetNavForPlayer(entt::entity player);

	// 把服务器坐标点吸附到导航网格最近可走面。false = 点不在任何可走面附近
	// (搜索范围:水平 ±2m,垂直 ±4m)。
	static bool SnapToMesh(NavComp& nav, const Location& in, Location& out);

	// 校验 from → to 的直线移动(from 必须已在网格上或可吸附)。
	// 返回 true  = 全程无阻挡,clamped 为吸附后的终点;
	// 返回 false = 中途撞墙,clamped 为**阻挡点**(沿途最后一个合法位置),
	//              或 from 本身不在网格上(此时 clamped 未修改)。
	static bool ValidateMove(NavComp& nav, const Location& from, const Location& to,
		Location& clamped);

	// 寻路:输出拉直(string-pulled)后的路径点序列,含起终点,服务器坐标。
	// false = 起点/终点不在网格上或不可达。
	static bool FindPath(NavComp& nav, const Location& from, const Location& to,
		std::vector<Location>& outPath);
};
