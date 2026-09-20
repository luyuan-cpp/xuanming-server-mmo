// PlayerTeamSystem::RefreshTeammateAoi(team-system.md §F.4)。
//
// 刻意与 player_team.cpp 分成两个编译单元:这里只依赖 ECS + InterestSystem + GridSystem。
// cpp/tests/aoi_test 链接 scene.lib 测这个函数时,静态库只会拉进本 .obj;
// 若写在 player_team.cpp 里,会连带拉进 PlayerBattleSystem(battle.lib)与
// scene_manager gRPC 客户端,而 aoi_test.vcxproj 不链接 battle.lib。

#include "player_team.h"

#include "hexagons_grid.h"
#include "modules/scene/comp/scene_comp.h"
#include "proto/common/component/team_comp.pb.h"
#include "spatial/comp/grid_comp.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include "spatial/constants/aoi_priority.h"
#include "spatial/system/grid.h"
#include "spatial/system/interest.h"
#include "thread_context/ecs_context.h"

namespace
{
	// 与 aoi.cpp DetermineAoiPriority 同一条判定:双方都有 TeamId,team_id 非 0 且相等。
	bool IsSameTeam(entt::entity lhs, entt::entity rhs)
	{
		const auto* lhsTeam = tlsEcs.actorRegistry.try_get<TeamId>(lhs);
		const auto* rhsTeam = tlsEcs.actorRegistry.try_get<TeamId>(rhs);
		return lhsTeam != nullptr && rhsTeam != nullptr && lhsTeam->team_id() != 0 &&
			   lhsTeam->team_id() == rhsTeam->team_id();
	}

	// 修正 watcher 对 target 的条目(条目不存在时两个调用都是静默 no-op)。
	void FixTeammateEntry(entt::entity watcher, entt::entity target, bool sameTeam)
	{
		const auto* list = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
		if (list == nullptr || !list->Contains(target))
		{
			return;
		}
		if (sameTeam)
		{
			InterestSystem::UpgradePriority(watcher, target, AoiPriority::kTeammate);
		}
		else
		{
			InterestSystem::DowngradePriority(watcher, target, AoiPriority::kTeammate, AoiPriority::kNormal);
		}
	}
} // namespace

void PlayerTeamSystem::RefreshTeammateAoi(entt::entity player)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}
	// 不在任何场景 / 还没进格子(首次 AOI Update 前):没有可修正的条目,进视野时自然按 TeamId 判定
	const auto* hex = tlsEcs.actorRegistry.try_get<Hex>(player);
	const auto* sceneComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
	if (hex == nullptr || sceneComp == nullptr || !tlsEcs.sceneRegistry.valid(sceneComp->sceneEntity))
	{
		return;
	}
	const auto* gridList = tlsEcs.sceneRegistry.try_get<SceneGridListComp>(sceneComp->sceneEntity);
	if (gridList == nullptr)
	{
		return;
	}

	// 遍历源是当前格及邻格的实体,而不是自己的兴趣表:兴趣条目可能不对称
	// (容量满被淘汰 / CanSee 单向),只遍历自己的表会漏掉"对方表里有我"的那一侧。
	GridSet grids;
	GridSystem::GetCurrentAndNeighborGridIds(*hex, grids);
	for (const auto& gridId : grids)
	{
		const auto gridIt = gridList->find(gridId);
		if (gridIt == gridList->end())
		{
			continue;
		}
		for (const auto other : gridIt->second.entities)
		{
			if (other == player || !tlsEcs.actorRegistry.valid(other))
			{
				continue;
			}
			const bool sameTeam = IsSameTeam(player, other);
			FixTeammateEntry(player, other, sameTeam); // 我对它
			FixTeammateEntry(other, player, sameTeam); // 它对我
		}
	}
}
