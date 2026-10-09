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
// 以下几个头只为 SendSceneInfo 的分线目录转发:zone Redis 的异步 GET(tlsRedis)、hiredis 应答类型
// (redisReply / REDIS_REPLY_* / REDIS_OK)、长度保护与命令拼接。
// hiredis.h 已被 redis_manager.h 经 muduo 的 Hiredis.h 带进来,这里显式再包含一次,
// 是为了不靠传递包含拿这些符号(本库 player_team.h / player_battle.cpp 同样显式包含)。
#include "thread_context/redis_manager.h"
#include <hiredis/hiredis.h>

#include <limits>
#include <string>

namespace {

// 分线目录的 Redis 键(STRING,值是 SceneChannelDirectory 的序列化字节);两个 %u 依次是 zone_id、scene_config_id。
// 与 Go 侧 go/scene_manager/internal/logic/world_channel_directory.go 的 WorldChannelDirectoryKeyFmt 是同一份契约,改一边必须改另一边。
constexpr char kWorldChannelDirectoryKeyFmt[] = "world_channel_directory:zone:%u:%u";

// Best-effort player guid for logging (0 if the Guid component is absent).
uint64_t GuidForLog(entt::entity player)
{
	const auto* g = tlsEcs.actorRegistry.try_get<Guid>(player);
	return g ? *g : 0;
}

bool ZoneRedisReady()
{
	auto& redis = tlsRedis.GetZoneRedis();
	return redis && redis->connected();
}

// 异步回调里的身份核对:实体仍存在,且仍是发命令时的那名玩家(实体槽位被复用后 Guid 会不同)。
// playerId 为 0 一律不算同一人:拿不到 Guid 就无从核对,调用方不得带着 0 走异步路径。
bool IsSamePlayer(entt::entity player, uint64_t playerId)
{
	return playerId != 0 && tlsEcs.actorRegistry.valid(player) && GuidForLog(player) == playerId;
}

// 玩家当前所在场景的 SceneInfoComp;不在任何场景、或场景实体上没有该组件时返回 nullptr。
// 返回值指向 sceneRegistry 的组件存储,只在当前同步调用栈内有效,不得带进异步回调。
const SceneInfoComp* CurrentSceneInfo(entt::entity player)
{
	const auto* sceneEntityComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
	if (sceneEntityComp == nullptr || sceneEntityComp->sceneEntity == entt::null)
	{
		return nullptr;
	}
	// SceneEntityComp::sceneEntity 是 sceneRegistry 的句柄,SceneInfoComp 只挂在 sceneRegistry 上。
	return tlsEcs.sceneRegistry.try_get<SceneInfoComp>(sceneEntityComp->sceneEntity);
}

// 推一条 NotifySceneInfo。directory 为 nullptr = 不带分线目录;非空时其内容被换进消息(Swap),
// 调用返回后 *directory 不可再用。
void PushSceneInfo(entt::entity player, const SceneInfoComp& sceneInfo, ::SceneChannelDirectory* directory)
{
	SceneInfoS2C message;
	message.add_scene_info()->CopyFrom(sceneInfo);
	if (directory != nullptr)
	{
		message.mutable_channel_directory()->Swap(directory);
	}
	SendMessageToClientViaGate(SceneSceneClientPlayerNotifySceneInfoMessageId, message, player);
}

// 把 "GET 分线目录" 的应答解析进 out。只有"应答是字符串、能解析、且确实是这张地图的目录"才返回 true;
// 返回 false 时 out 的内容不可用,调用方按"不带目录"推送。
// 日志分级的依据:
//   - 键不存在是正常瞬态(目录按周期重发且带 TTL,刚开服 / scene_manager 领导者切换时会短暂缺失):DEBUG;
//   - 连接断开(hiredis 用空应答回调挂起命令)或 Redis 报错是读失败:WARN;
//   - 应答不是字符串 / 字节解析不了 / 目录写的不是这张地图,说明 Go 与 C++ 两侧的键名或消息契约
//     被破坏(改了一边没改另一边):ERROR。
bool ParseChannelDirectoryReply(const redisReply* reply, uint32_t sceneConfigId, uint64_t playerId,
								::SceneChannelDirectory& out)
{
	if (reply == nullptr)
	{
		LOG_WARN << "SendSceneInfo: 读分线目录无应答(Redis 连接断开), 本次不带目录, player_id=" << playerId
				 << " scene_config_id=" << sceneConfigId;
		return false;
	}
	if (reply->type == REDIS_REPLY_NIL)
	{
		LOG_DEBUG << "SendSceneInfo: 分线目录尚未发布或已过期, 本次不带目录, player_id=" << playerId
				  << " scene_config_id=" << sceneConfigId;
		return false;
	}
	if (reply->type == REDIS_REPLY_ERROR)
	{
		LOG_WARN << "SendSceneInfo: 读分线目录被 Redis 拒绝, 本次不带目录, player_id=" << playerId
				 << " scene_config_id=" << sceneConfigId << " err=" << (reply->str != nullptr ? reply->str : "");
		return false;
	}
	// 先判 type 再读 str / len;长度保护是因为 ParseFromArray 只收 int。
	if (reply->type != REDIS_REPLY_STRING || reply->str == nullptr ||
		reply->len > static_cast<size_t>(std::numeric_limits<int>::max()))
	{
		LOG_ERROR << "SendSceneInfo: 分线目录应答形状非法, 本次不带目录, player_id=" << playerId
				  << " scene_config_id=" << sceneConfigId << " reply_type=" << reply->type;
		return false;
	}
	if (!out.ParseFromArray(reply->str, static_cast<int>(reply->len)))
	{
		LOG_ERROR << "SendSceneInfo: 分线目录解析失败, 本次不带目录, player_id=" << playerId
				  << " scene_config_id=" << sceneConfigId << " bytes=" << reply->len;
		return false;
	}
	if (out.scene_config_id() != sceneConfigId)
	{
		LOG_ERROR << "SendSceneInfo: 分线目录写的不是当前地图, 本次不带目录, player_id=" << playerId
				  << " scene_config_id=" << sceneConfigId << " directory_scene_config_id=" << out.scene_config_id();
		return false;
	}
	return true;
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

void PlayerSceneSystem::SendSceneInfo(entt::entity player, bool withChannelDirectory)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		LOG_WARN << "SendSceneInfo: 玩家实体无效, 不推送, entity=" << entt::to_integral(player);
		return;
	}
	const uint64_t playerId = GuidForLog(player);

	const auto* sceneInfo = CurrentSceneInfo(player);
	if (sceneInfo == nullptr)
	{
		LOG_WARN << "SendSceneInfo: 玩家不在任何场景或场景实体缺少 SceneInfoComp, 不推送, player_id=" << playerId;
		return;
	}

	// 只有大世界分线才有目录:副本 / 镜像按 id 进入,没有"线"的概念。
	const bool isWorldChannel = sceneInfo->mirror_config_id() == 0 && sceneInfo->dungeon_config_id() == 0 &&
								sceneInfo->scene_config_id() != 0;
	// 调用方没要目录(withChannelDirectory == false)时不读 Redis:与加目录之前的开销完全相同。
	// 实体上没有 Guid(playerId == 0)时回调里无从核对"还是不是这名玩家",同样不走异步。
	if (!withChannelDirectory || !isWorldChannel || playerId == 0 || !ZoneRedisReady())
	{
		PushSceneInfo(player, *sceneInfo, nullptr);
		return;
	}

	// 回调要用的东西全部按值带走(sceneInfo 指向 sceneRegistry 的组件存储,应答回来时场景可能已销毁)。
	// 捕获一律用 init-capture:MSVC 对 entt::entity 的简单捕获有 C3495(.github/copilot-instructions.md)。
	const uint64_t sceneId = sceneInfo->scene_id();
	const uint32_t sceneConfigId = sceneInfo->scene_config_id();
	const int ret = tlsRedis.GetZoneRedis()->command(
		[player = player, playerId = playerId, sceneId = sceneId, sceneConfigId = sceneConfigId,
		 sceneInfoCopy = *sceneInfo](hiredis::Hiredis*, redisReply* reply) {
			if (!IsSamePlayer(player, playerId))
			{
				return;
			}
			// 重新取当前场景再比:应答回来之前玩家可能已经换线 / 换图。此时不推 —— 推了就是把旧线的
			// scene_info 与目录盖到新场景上;客户端进新场景后会重新请求。
			const auto* currentInfo = CurrentSceneInfo(player);
			if (currentInfo == nullptr || currentInfo->scene_id() != sceneId)
			{
				LOG_DEBUG << "SendSceneInfo: 应答回来时玩家已不在发起时的场景, 丢弃, player_id=" << playerId
						  << " scene_id=" << sceneId;
				return;
			}
			// 带不带目录都要推:客户端靠这条推送结束等待。
			::SceneChannelDirectory directory;
			const bool hasDirectory = ParseChannelDirectoryReply(reply, sceneConfigId, playerId, directory);
			PushSceneInfo(player, sceneInfoCopy, hasDirectory ? &directory : nullptr);
		},
		(std::string("GET ") + kWorldChannelDirectoryKeyFmt).c_str(), GetZoneId(), sceneConfigId);
	if (ret != REDIS_OK)
	{
		// 命令没被 hiredis 收下(连接正在断开 / 释放,或格式化失败)时回调永远不会被调用,
		// 必须在这里补推,否则客户端等不到任何应答。sceneInfo 此刻仍有效:command 不会同步执行回调,
		// 也不碰 ECS。
		LOG_WARN << "SendSceneInfo: 读分线目录的命令未发出, 本次不带目录, player_id=" << playerId
				 << " scene_config_id=" << sceneConfigId << " ret=" << ret;
		PushSceneInfo(player, *sceneInfo, nullptr);
	}
}

