#include "player_scene.h"

#include "muduo/base/Logging.h"

#include "proto/common/event/scene_event.pb.h"
#include "hexagons_grid.h"

#include "thread_context/redis_manager.h"

#include "rpc/service_metadata/player_scene_service_metadata.h"

#include "network/player_message_utils.h"
#include "network/node_message_utils.h"
#include "network/network_utils.h"
#include "engine/thread_context/node_context_manager.h"
#include "proto/common/component/team_comp.pb.h"
#include "proto/scene_manager/storage.pb.h"
#include "proto/scene_manager/scene_manager_service.pb.h"
#include "grpc_client/scene_manager/scene_manager_service_grpc_client.h"
#include "network/node_utils.h"
#include <modules/scene/comp/scene_comp.h>
#include <modules/scene/comp/scene_node_comp.h>
#include <proto/common/component/player_network_comp.pb.h>
#include "node/system/node/node_util.h"
#include "battle/system/player_battle.h"
#include "spatial/system/view.h"
#include <limits>
#include <thread_context/ecs_context.h>

namespace {

// Best-effort player guid for logging (0 if the Guid component is absent).
uint64_t GuidForLog(entt::entity player)
{
	const auto* g = tlsEcs.actorRegistry.try_get<Guid>(player);
	return g ? *g : 0;
}

// Validate a hiredis reply and parse its payload into a protobuf message.
// Returns false (silently) when the reply is nil/missing or the player is
// gone; logs and returns false on an oversized or corrupt payload.
template <typename T>
bool ParsePlayerRedisReply(entt::entity player, void* replyVoid, const char* payloadName, T& out)
{
	auto* reply = static_cast<redisReply*>(replyVoid);
	if (reply == nullptr || reply->type == REDIS_REPLY_NIL)
	{
		return false;
	}
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return false;
	}
	if (reply->len > static_cast<size_t>(std::numeric_limits<int>::max()))
	{
		LOG_ERROR << payloadName << " payload too large for protobuf parser, len=" << reply->len;
		return false;
	}
	if (!out.ParseFromArray(reply->str, static_cast<int>(reply->len)))
	{
		LOG_ERROR << "Failed to parse " << payloadName << " from Redis for player " << GuidForLog(player);
		return false;
	}
	return true;
}

}  // namespace

// Callback wrapper for Hiredis
void PlayerSceneSystem::OnGetTeamInfo(entt::entity player, void* replyVoid)
{
	TeamInfo teamInfo;
	if (!ParsePlayerRedisReply(player, replyVoid, "TeamInfo", teamInfo))
	{
		return;
	}

	uint64_t leaderId = teamInfo.leader_id();
	uint64_t myId = tlsEcs.actorRegistry.get<Guid>(player);

	if (leaderId == 0 || leaderId == myId)
	{
		return; // Not a follower or invalid team
	}

	// Fetch leader location
	auto cb = [player](hiredis::Hiredis* c, redisReply* r) {
		OnGetLeaderLocation(player, r);
	};
	auto &zoneRedis = tlsRedis.GetZoneRedis();
	if (!zoneRedis || !zoneRedis->connected())
	{
		LOG_WARN << "Skip leader-location lookup: Redis not connected (leaderId=" << leaderId << ")";
		return;
	}
	zoneRedis->command(cb, "GET player:%llu:location", leaderId);
}

void PlayerSceneSystem::OnGetLeaderLocation(entt::entity player, void* replyVoid)
{
	storage::PlayerLocation loc;
	if (!ParsePlayerRedisReply(player, replyVoid, "PlayerLocation", loc))
	{
		return;
	}

	// Compare scenes
	const auto sceneEntity = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
	if (!sceneEntity) {
		return;
	}

	// SceneEntityComp::sceneEntity 是 sceneRegistry 的句柄,不是 actorRegistry 的。
	// 全仓 SceneInfoComp 只在 sceneRegistry 上 emplace(scene_node_service.cpp /
	// scene_handler.cpp,都紧跟 sceneRegistry.create()),本文件 167/261 行以及
	// s2s_player_scene_handler.cpp 也都是从 sceneRegistry 读。这里查 actorRegistry
	// 恒为 nullptr,于是队伍跟随链在这一行就永远早退 —— 队员从来不会被拉去队长的场景。
	const auto sceneInfo = tlsEcs.sceneRegistry.try_get<SceneInfoComp>(sceneEntity->sceneEntity);
	if (!sceneInfo) {
		return;
	}

	uint64_t currentSceneId = sceneInfo->scene_id();
	uint64_t leaderSceneId = loc.scene_id();

	if (leaderSceneId != 0 && leaderSceneId != currentSceneId)
	{
		// 回合制战斗冻结拦截:战斗在途不跟随队长切场景(设计文档 §5.3 冻结清单)
		if (PlayerBattleSystem::IsInBattle(player))
		{
			LOG_INFO << "[PlayerBattle] 队伍跟随切场景跳过: 玩家战斗在途, player_id=" << GuidForLog(player);
			return;
		}

		const auto* playerSessionPB = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
		if (!playerSessionPB)
		{
			LOG_ERROR << "PlayerSessionSnapshotComp missing for follower " << GuidForLog(player);
			return;
		}

		// Resolve gate info
		NodeId gateNodeId = GetGateNodeId(playerSessionPB->gate_session_id());

		// 同 player_lifecycle.cpp:同样不能拿 node_id 当 entt 实体句柄
		// (network_utils.h:27-30 的禁令)。走 ResolveLocalZoneGateEntity,
		// 否则 gateInstanceId 会是空串,而 scene_manager 侧要靠它做 gate 防僵尸过滤。
		std::string gateInstanceId;
		auto& gateRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
		if (const auto gateEntityOpt = ResolveLocalZoneGateEntity(playerSessionPB->gate_session_id()))
		{
			if (const auto* gateNodeInfo = gateRegistry.try_get<NodeInfo>(*gateEntityOpt))
			{
				gateInstanceId = gateNodeInfo->node_uuid();
			}
		}

		// Find a SceneManager gRPC node
		auto smEntity = GetSceneManagerEntity(playerSessionPB->player_id());
		if (smEntity == entt::null)
		{
			LOG_WARN << "No SceneManager node available, cannot follow leader to scene " << leaderSceneId;
			return;
		}

		auto &smRegistry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);

		::scene_manager::EnterSceneRequest req;
		req.set_player_id(playerSessionPB->player_id());
		req.set_scene_id(leaderSceneId);
		req.set_session_id(playerSessionPB->gate_session_id());
		req.set_gate_id(std::to_string(gateNodeId));
		req.set_gate_instance_id(gateInstanceId);
		req.set_gate_zone_id(GetZoneId());
		req.set_zone_id(GetZoneId());

		scene_manager::SendSceneManagerEnterScene(smRegistry, smEntity, req);

		LOG_INFO << "Player " << playerSessionPB->player_id()
				 << " (Follower) requesting SceneManager to switch to Leader's scene " << leaderSceneId;
	}
}

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

	// 5. Team Follow Logic: if player is in a team, check leader location.
	const auto teamIdComp = tlsEcs.actorRegistry.try_get<TeamId>(player);
	if (teamIdComp && teamIdComp->team_id() != 0)
	{
		auto cb = [player](hiredis::Hiredis* c, redisReply* r) {
			OnGetTeamInfo(player, r);
		};
		auto &zoneRedis = tlsRedis.GetZoneRedis();
		if (!zoneRedis || !zoneRedis->connected())
		{
			LOG_WARN << "Skip team-info lookup: Redis not connected (team_id=" << teamIdComp->team_id() << ")";
			return;
		}
		zoneRedis->command(cb, "GET team:%llu", teamIdComp->team_id());
	}
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

