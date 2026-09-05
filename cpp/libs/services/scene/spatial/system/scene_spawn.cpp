#include "scene_spawn.h"

#include "muduo/base/Logging.h"

#include "generated/attribute/actorbaseattributess2c_attribute_sync.h"
#include "modules/scene/comp/scene_comp.h"
#include "proto/scene/player_state_attribute_sync.pb.h"
#include "proto/scene/scene_info.pb.h"
#include "spatial/comp/nav_comp.h"
#include "spatial/constants/nav.h"
#include "spatial/manager/scene_nav.h"
#include "spatial/system/nav_query.h"
#include "thread_context/ecs_context.h"

namespace
{

Location ToLocation(const Vector3& v)
{
	Location out;
	out.set_x(v.x());
	out.set_y(v.y());
	out.set_z(v.z());
	return out;
}

void WriteLocation(Vector3& dst, const Location& src)
{
	dst.set_x(src.x());
	dst.set_y(src.y());
	dst.set_z(src.z());
}

uint64_t GuidForLog(const entt::entity player)
{
	const auto* g = tlsEcs.actorRegistry.try_get<Guid>(player);
	return g ? *g : 0;
}

}  // namespace

Location SceneSpawnSystem::DefaultSpawnFor(const uint32_t /*sceneConfigId*/)
{
	// 所有场景当前共用天墉城一张图(BaseScene.nav_bin_file 全部 main_scene.bin),
	// 出生点只有一份;分场景后此处改查 BaseScene 表。
	Location spawn;
	spawn.set_x(kTianyongSpawnX);
	spawn.set_y(kTianyongSpawnY);
	spawn.set_z(kTianyongSpawnZ);
	return spawn;
}

Location SceneSpawnSystem::ResolveSpawnOnMesh(NavComp* nav, const uint32_t sceneConfigId)
{
	const Location spawn = DefaultSpawnFor(sceneConfigId);
	if (nav == nullptr)
	{
		return spawn;
	}
	Location snapped;
	if (NavQuerySystem::SnapToMesh(*nav, spawn, snapped))
	{
		return snapped;
	}
	// 出生点不在网格上 = 烘焙数据与出生点契约不一致(旧占位 bin / 改了地图没
	// 重烘)。NavigationSystem::LoadNavBins 启动时已探针过一次并拒绝注册这种
	// 网格,正常不会走到这里;万一走到,宁可放行出生点也不能把玩家卡死。
	LOG_ERROR << "scene spawn point is off the navmesh: scene_config_id=" << sceneConfigId
			  << " spawn=(" << spawn.x() << "," << spawn.y() << "," << spawn.z()
			  << ") — nav bin does not match the map, re-bake with tools/navmesh_baker";
	return spawn;
}

bool SceneSpawnSystem::IsUnsetLocation(const Vector3& location)
{
	return location.x() == 0.0 && location.y() == 0.0 && location.z() == 0.0;
}

bool SceneSpawnSystem::EnsureValidEnterLocation(const entt::entity player, const entt::entity scene)
{
	const auto* sceneInfo = tlsEcs.sceneRegistry.try_get<SceneInfoComp>(scene);
	if (sceneInfo == nullptr)
	{
		return false;
	}
	const uint32_t sceneConfigId = sceneInfo->scene_config_id();
	auto& transform = tlsEcs.actorRegistry.get_or_emplace<Transform>(player);
	const Location current = ToLocation(transform.location());

	NavComp* nav = SceneNavManager::Instance().Get(sceneConfigId);
	Location accepted = current;
	bool relocated = false;
	const char* reason = nullptr;

	if (nav != nullptr)
	{
		Location snapped;
		if (NavQuerySystem::SnapToMesh(*nav, current, snapped))
		{
			// 合法:只把高度/贴边误差吸附到网格,不算改写。
			accepted = snapped;
		}
		else
		{
			accepted = ResolveSpawnOnMesh(nav, sceneConfigId);
			relocated = true;
			reason = IsUnsetLocation(transform.location()) ? "unset (0,0,0)" : "off navmesh";
		}
	}
	else if (IsUnsetLocation(transform.location()))
	{
		// 场景无导航(fail-open)时无法判合法性,只修"从未落位"的默认值。
		accepted = ResolveSpawnOnMesh(nullptr, sceneConfigId);
		relocated = true;
		reason = "unset (0,0,0), scene has no navmesh";
	}

	WriteLocation(*transform.mutable_location(), accepted);
	SetActorBaseAttributesS2CAttrDirtyBit(player, ActorBaseAttributesS2C::kTransformFieldNumber);

	if (relocated)
	{
		LOG_INFO << "EnterScene spawn: player " << GuidForLog(player)
				 << " scene_config_id=" << sceneConfigId
				 << " location (" << current.x() << "," << current.y() << "," << current.z()
				 << ") " << reason << " -> spawn ("
				 << accepted.x() << "," << accepted.y() << "," << accepted.z() << ")";
	}
	return relocated;
}

Location SceneSpawnSystem::FallbackLocationForPlayer(const entt::entity player, NavComp* nav)
{
	uint32_t sceneConfigId = 0;
	if (const auto* sceneEntityComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
		sceneEntityComp != nullptr && sceneEntityComp->sceneEntity != entt::null)
	{
		if (const auto* sceneInfo =
				tlsEcs.sceneRegistry.try_get<SceneInfoComp>(sceneEntityComp->sceneEntity))
		{
			sceneConfigId = sceneInfo->scene_config_id();
		}
	}
	return ResolveSpawnOnMesh(nav, sceneConfigId);
}
