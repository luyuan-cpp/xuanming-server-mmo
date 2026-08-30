#include "nav_query.h"

#include <cmath>
#include <limits>

#include "Detour/DetourNavMeshQuery.h"

#include "modules/scene/comp/scene_comp.h"
#include "proto/scene/scene_info.pb.h"
#include "spatial/comp/nav_comp.h"
#include "spatial/manager/scene_nav.h"
#include "thread_context/ecs_context.h"

namespace
{

// 吸附搜索范围(导航坐标系):水平 ±2m、垂直 ±4m。
// 水平给到 2m 是容忍客户端 mask 格粒度(2m)带来的边界误差;
// 垂直放宽是因为服务器侧 z(高度)在平面场景里恒为 0 附近。
constexpr dtReal kSnapExtents[3] = {2.0, 4.0, 2.0};

// raycast 途径多边形缓冲。天墉城 400x300 对角线也用不满,溢出时
// Detour 返回 DT_BUFFER_TOO_SMALL,按撞墙保守处理。
constexpr int kMaxRaycastPolys = 256;

// 撞墙后从阻挡点往回缩的距离,避免 clamp 出的位置恰好骑在网格边缘、
// 下一次吸附落到墙对面。
constexpr dtReal kHitPullback = 0.05;

struct NavVec
{
	dtReal v[3];
};

// 服务器 Z-up → 导航(Unity)Y-up,见 nav_query.h 头注释。
NavVec ToNav(const Location& in)
{
	return NavVec{{in.y(), in.z(), in.x()}};
}

void ToServer(const dtReal* nav, Location& out)
{
	out.set_x(nav[2]);
	out.set_y(nav[0]);
	out.set_z(nav[1]);
}

bool SnapNav(NavComp& nav, const NavVec& in, dtPolyRef& outRef, NavVec& outPos)
{
	const dtQueryFilter filter;
	outRef = 0;
	const dtStatus status = nav.navQuery.findNearestPoly(
		in.v, kSnapExtents, &filter, &outRef, outPos.v);
	return dtStatusSucceed(status) && outRef != 0;
}

}  // namespace

NavComp* NavQuerySystem::GetNavForPlayer(const entt::entity player)
{
	const auto* sceneEntityComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
	if (!sceneEntityComp || sceneEntityComp->sceneEntity == entt::null)
	{
		return nullptr;
	}
	const auto* sceneInfo =
		tlsEcs.sceneRegistry.try_get<SceneInfoComp>(sceneEntityComp->sceneEntity);
	if (!sceneInfo)
	{
		return nullptr;
	}
	return SceneNavManager::Instance().Get(sceneInfo->scene_config_id());
}

bool NavQuerySystem::SnapToMesh(NavComp& nav, const Location& in, Location& out)
{
	dtPolyRef ref = 0;
	NavVec snapped{};
	if (!SnapNav(nav, ToNav(in), ref, snapped))
	{
		return false;
	}
	ToServer(snapped.v, out);
	return true;
}

bool NavQuerySystem::ValidateMove(NavComp& nav, const Location& from, const Location& to,
	Location& clamped)
{
	dtPolyRef startRef = 0;
	NavVec startPos{};
	if (!SnapNav(nav, ToNav(from), startRef, startPos))
	{
		return false;
	}

	const NavVec endPos = ToNav(to);
	const dtQueryFilter filter;
	dtReal t = 0;
	dtReal hitNormal[3] = {0, 0, 0};
	dtPolyRef visited[kMaxRaycastPolys];
	int visitedCount = 0;
	const dtStatus status = nav.navQuery.raycast(startRef, startPos.v, endPos.v,
		&filter, &t, hitNormal, visited, &visitedCount, kMaxRaycastPolys);
	if (dtStatusFailed(status))
	{
		// 查询本身失败(含 buffer 溢出):保守当作从起点就被挡。
		ToServer(startPos.v, clamped);
		return false;
	}

	// Detour 约定:未撞墙时 t 为"极大值"(> 1),撞墙时 t∈[0,1)。
	if (t > 1.0)
	{
		// 全程无阻挡。终点吸附一次拿到网格上的合法高度。
		dtPolyRef endRef = 0;
		NavVec snappedEnd{};
		if (SnapNav(nav, endPos, endRef, snappedEnd))
		{
			ToServer(snappedEnd.v, clamped);
			return true;
		}
		// 射线可达但终点吸附不到(贴边浮点误差):按撞在终点前处理。
	}

	// 阻挡点:from + (to-from)*t,再往回缩 kHitPullback 防骑墙。
	const dtReal hitT = t > 1.0 ? 1.0 : t;
	NavVec hit{};
	dtReal dir[3] = {endPos.v[0] - startPos.v[0], endPos.v[1] - startPos.v[1],
		endPos.v[2] - startPos.v[2]};
	const dtReal len = std::sqrt(dir[0] * dir[0] + dir[1] * dir[1] + dir[2] * dir[2]);
	const dtReal pullbackT = len > kHitPullback ? hitT - kHitPullback / len : 0.0;
	const dtReal finalT = pullbackT > 0.0 ? pullbackT : 0.0;
	for (int i = 0; i < 3; ++i)
	{
		hit.v[i] = startPos.v[i] + dir[i] * finalT;
	}

	dtPolyRef hitRef = 0;
	NavVec snappedHit{};
	if (SnapNav(nav, hit, hitRef, snappedHit))
	{
		ToServer(snappedHit.v, clamped);
	}
	else
	{
		ToServer(startPos.v, clamped);
	}
	return false;
}

bool NavQuerySystem::FindPath(NavComp& nav, const Location& from, const Location& to,
	std::vector<Location>& outPath)
{
	outPath.clear();

	dtPolyRef startRef = 0;
	dtPolyRef endRef = 0;
	NavVec startPos{};
	NavVec endPos{};
	if (!SnapNav(nav, ToNav(from), startRef, startPos) ||
		!SnapNav(nav, ToNav(to), endRef, endPos))
	{
		return false;
	}

	const dtQueryFilter filter;
	dtQueryResult pathResult;
	const dtStatus pathStatus = nav.navQuery.findPath(startRef, endRef,
		startPos.v, endPos.v, std::numeric_limits<dtReal>::max(), &filter,
		pathResult, nullptr);
	if (dtStatusFailed(pathStatus) || pathResult.size() == 0)
	{
		return false;
	}
	// DT_PARTIAL_RESULT(终点不可达,给出最近可达路径)也照常返回,
	// 调用方按"走到最近可达点"处理 —— 与客户端 FindNearestWalkable 语义一致。

	std::vector<dtPolyRef> polys(pathResult.size());
	pathResult.copyRefs(polys.data(), static_cast<int>(polys.size()));

	dtQueryResult straight;
	const dtStatus straightStatus = nav.navQuery.findStraightPath(
		startPos.v, endPos.v, polys.data(), static_cast<int>(polys.size()), straight);
	if (dtStatusFailed(straightStatus) || straight.size() == 0)
	{
		return false;
	}

	outPath.reserve(straight.size());
	for (int i = 0; i < straight.size(); ++i)
	{
		Location point;
		ToServer(straight.getPos(i), point);
		outPath.push_back(point);
	}
	return true;
}
