#include "player_scene.h"

#include "muduo/base/Logging.h"

#include "proto/common/event/scene_event.pb.h"
#include "hexagons_grid.h"

#include "rpc/service_metadata/player_scene_service_metadata.h"

#include "network/player_message_utils.h"
#include "network/node_message_utils.h"
#include "network/network_utils.h"
#include "engine/thread_context/node_context_manager.h"
#include "proto/scene_manager/scene_manager_service.pb.h"
#include "grpc_client/scene_manager/scene_manager_service_grpc_client.h"
#include "network/node_utils.h"
#include <modules/scene/comp/scene_comp.h>
#include <modules/scene/comp/scene_node_comp.h>
#include <proto/common/component/player_network_comp.pb.h>
#include "node/system/node/node_util.h"
#include "battle/system/player_battle.h"
#include "spatial/system/scene_spawn.h"
#include "spatial/system/view.h"
#include <thread_context/ecs_context.h>

namespace {

// Best-effort player guid for logging (0 if the Guid component is absent).
uint64_t GuidForLog(entt::entity player)
{
	const auto* g = tlsEcs.actorRegistry.try_get<Guid>(player);
	return g ? *g : 0;
}

}  // namespace

// 组队跟随(原第 5 步 OnGetTeamInfo / OnGetLeaderLocation)已迁到 PlayerTeamSystem
// (player_team.{h,cpp},team-system.md §F.2):调用点改为 HandleEnterScene 之后,
// 因为本函数开头的"已在目标场景"幂等早退会让同场景重连漏掉刷新。

void PlayerSceneSystem::HandleEnterScene(entt::entity player, entt::entity scene)
{
	const auto sceneInfo = tlsEcs.sceneRegistry.try_get<SceneInfoComp>(scene);
	if (sceneInfo == nullptr)
	{
		LOG_ERROR << "HandleEnterScene: scene info not found for player: "
		          << entt::to_integral(player);
		return;
	}

	// 0. Idempotency: if player is already in the target scene, skip.
	auto *oldSceneComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
	if (oldSceneComp != nullptr && oldSceneComp->sceneEntity == scene)
	{
		LOG_INFO << "HandleEnterScene: player " << GuidForLog(player)
		         << " already in scene_id=" << sceneInfo->scene_id() << ", skipping";
		return;
	}

	// 同图换线保留坐标,换地图使用目标出生点;判断须在旧场景绑定被替换前完成。
	bool mapChanged = false;
	if (oldSceneComp != nullptr && oldSceneComp->sceneEntity != entt::null)
	{
		const auto* oldInfo = tlsEcs.sceneRegistry.try_get<SceneInfoComp>(oldSceneComp->sceneEntity);
		mapChanged = oldInfo != nullptr && oldInfo->scene_config_id() != sceneInfo->scene_config_id();
	}

	// 1. Leave old scene if player is already in one.
	if (oldSceneComp != nullptr && oldSceneComp->sceneEntity != entt::null
	    && oldSceneComp->sceneEntity != scene)
	{
		// Clean up AOI grid before leaving the old scene.
		BeforeLeaveScene leaveEvent;
		leaveEvent.set_entity(entt::to_integral(player));
		tlsEcs.dispatcher.trigger(leaveEvent);

		// Remove old hex so AOI treats the new scene as a fresh entry.
		tlsEcs.actorRegistry.remove<Hex>(player);

		auto *oldPlayers = tlsEcs.sceneRegistry.try_get<ScenePlayers>(oldSceneComp->sceneEntity);
		if (oldPlayers)
		{
			oldPlayers->erase(player);
		}
	}

	// 2. Bind player to the new scene entity.
	tlsEcs.actorRegistry.get_or_emplace<SceneEntityComp>(player).sceneEntity = scene;

	// 3. Track player in the scene's player set.
	auto *scenePlayers = tlsEcs.sceneRegistry.try_get<ScenePlayers>(scene);
	if (scenePlayers)
	{
		scenePlayers->emplace(player);
	}

	// 3.5 服务器权威落位:Transform 来自 DB(新号是 proto 默认 (0,0,0),旧号可能
	// 存着旧地图/别的场景的坐标),必须在自身 ActorCreate(4.5)与 AOI 广播之前
	// 校验它在目标场景导航网格上,不在就落到场景出生点。否则客户端会本地把
	// 人挪到城内、服务器仍在 (0,0,0),首次移动的 MoveAck 把客户端拉回原点卡死
	// (2026-09-05 移动诊断)。同图换线/重登保留合法坐标,换地图落到目标出生点。
	SceneSpawnSystem::EnsureValidEnterLocation(player, scene, mapChanged);

	// 4. Notify client of scene entry.
	EnterSceneS2C message;
	message.mutable_scene_info()->CopyFrom(*sceneInfo);
	SendMessageToClientViaGate(SceneSceneClientPlayerNotifyEnterSceneMessageId, message, player);

	// 4.5 补发"自己"的 ActorCreate:AOI 可见性遍历刻意跳过 observer 本身
	// (aoi.cpp HandleEntityVisibility 的 otherEntity == entity 分支),因此
	// 除这里之外没有任何路径把玩家自己的 actor 下发给客户端——单人在线时
	// 客户端将收不到任何 ActorCreate,本地角色/相机绑定/移动控制器全部
	// 无法建立。客户端靠 guid == player_id 识别并绑定本地角色。
	ActorCreateS2C selfCreate;
	ViewSystem::FillActorCreateMessageInfo(player, player, selfCreate);
	SendMessageToClientViaGate(SceneSceneClientPlayerNotifyActorCreateMessageId, selfCreate, player);

	LOG_INFO << "HandleEnterScene: player " << GuidForLog(player)
			 << " entered scene_id=" << sceneInfo->scene_id();
}

bool PlayerSceneSystem::RequestEnterMirrorScene(entt::entity player, uint32_t mirrorConfigId)
{
	if (mirrorConfigId == 0)
	{
		LOG_WARN << "RequestEnterMirrorScene: refusing source-clone mirror (mirrorConfigId == 0); "
		            "this helper only supports template-driven mirrors";
		return false;
	}

	if (!tlsEcs.actorRegistry.valid(player))
	{
		LOG_WARN << "RequestEnterMirrorScene: invalid player entity " << entt::to_integral(player);
		return false;
	}

	// 回合制战斗冻结拦截:战斗在途拒绝进镜像副本(设计文档 §5.3 冻结清单)
	if (PlayerBattleSystem::IsInBattle(player))
	{
		LOG_WARN << "[PlayerBattle] RequestEnterMirrorScene 被拒: 玩家战斗在途, player_id="
				 << GuidForLog(player);
		return false;
	}

	const auto* sceneEntityComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
	if (sceneEntityComp == nullptr || sceneEntityComp->sceneEntity == entt::null)
	{
		LOG_WARN << "RequestEnterMirrorScene: player " << entt::to_integral(player)
		         << " has no current scene; mirror requires a source";
		return false;
	}

	const auto* sourceInfo = tlsEcs.sceneRegistry.try_get<SceneInfoComp>(sceneEntityComp->sceneEntity);
	if (sourceInfo == nullptr || sourceInfo->scene_id() == 0)
	{
		LOG_WARN << "RequestEnterMirrorScene: source scene missing SceneInfoComp or scene_id=0";
		return false;
	}

	// Refuse to mirror a mirror — the source must be a real (non-mirror)
	// scene. This keeps the source's resident map/AI/spawn data as the
	// authoritative template.
	if (sourceInfo->mirror_config_id() != 0)
	{
		LOG_WARN << "RequestEnterMirrorScene: refusing to mirror an existing mirror (source scene "
		         << sourceInfo->scene_id() << ", mirror_config_id="
		         << sourceInfo->mirror_config_id() << ")";
		return false;
	}

	const auto* playerSessionPB = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
	if (playerSessionPB == nullptr)
	{
		LOG_WARN << "RequestEnterMirrorScene: missing PlayerSessionSnapshotComp on player "
		         << entt::to_integral(player);
		return false;
	}

	auto smEntity = GetSceneManagerEntity(playerSessionPB->player_id());
	if (smEntity == entt::null)
	{
		LOG_WARN << "RequestEnterMirrorScene: no SceneManager node available";
		return false;
	}

	auto& smRegistry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);

	::scene_manager::CreateSceneRequest req;
	req.set_scene_conf_id(sourceInfo->scene_config_id());
	req.set_scene_type(::scene_manager::SCENE_TYPE_INSTANCE);
	req.set_zone_id(GetZoneId());
	req.set_source_scene_id(sourceInfo->scene_id());
	req.set_mirror_config_id(mirrorConfigId);
	req.add_creator_ids(playerSessionPB->player_id());

	scene_manager::SendSceneManagerCreateScene(smRegistry, smEntity, req);

	LOG_INFO << "RequestEnterMirrorScene: dispatched create_mirror player=" << playerSessionPB->player_id()
	         << " source_scene=" << sourceInfo->scene_id()
	         << " mirror_config=" << mirrorConfigId
	         << " scene_conf=" << sourceInfo->scene_config_id();
	return true;
}

