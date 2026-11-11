#include "player_scene.h"
#include "core/async/coroutine_callback.h"
#include "core/system/scene_task_scope.h"
#include "engine/core/utils/registry/actor_log.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "type_alias/player_session_type_alias.h"
#include "actor/actor_net_id.h"

#define BOOST_COBALT_NO_LIB 1
#include <boost/cobalt/task.hpp>

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"

#include "hexagons_grid.h"

#include "rpc/service_metadata/player_scene_service_metadata.h"

#include "network/client_message_sender.h"
#include "network/network_utils.h"
#include "engine/thread_context/node_context_manager.h"
#include "proto/scene_manager/scene_manager_service.pb.h"
#include "grpc_client/scene_manager/scene_manager_service_grpc_client.h"
#include "network/node_utils.h"
#include <modules/scene/comp/scene_comp.h>
#include <proto/common/component/player_network_comp.pb.h>
#include "node/system/node/node_util.h"
#include "battle/system/player_battle.h"
#include "spatial/system/scene_spawn.h"
#include "spatial/system/nav_query.h"
#include "spatial/manager/scene_nav.h"
#include "spatial/system/view.h"
#include "spatial/system/aoi.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/enter_fence.h"
#include "player/system/player_lifecycle.h" // IsSceneChangeBusy / RequestSceneChange:换图发送侧闸与 EnterScene 唯一发送入口
#include "player/system/player_tip.h"
#include "core/network/grpc_client_call.h"      // GrpcCallOutcome:镜像 CreateScene 的每次调用回调
#include "proto/common/base/node.pb.h"      // NodeInfo:自动进场 EnterScene 的 gate_instance_id
#include "time/system/time.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/scene_error_tip.pb.h"
#include <thread_context/ecs_context.h>
// 以下几个头只为 SendSceneInfo 的分线目录转发:场景控制域 Redis 的异步 GET(tlsSceneControlRedis)、hiredis 应答类型
// (redisReply / REDIS_REPLY_* / REDIS_OK)、长度保护与命令拼接。
// hiredis.h 已被 redis_manager.h 经 muduo 的 Hiredis.h 带进来,这里显式再包含一次,
// 是为了不靠传递包含拿这些符号(本库 player_team.h / player_battle.cpp 同样显式包含)。
#include "thread_context/redis_manager.h"
#include <hiredis/hiredis.h>

#include <limits>
#include <memory>
#include <string>
#include <utility>

namespace {

// 分线目录的 Redis 键(STRING,值是 SceneChannelDirectory 的序列化字节);两个 %u 依次是 zone_id、scene_config_id。
// 与 Go 侧 go/scene_manager/internal/logic/world_channel_directory.go 的 WorldChannelDirectoryKeyFmt 是同一份契约,改一边必须改另一边。
constexpr char kWorldChannelDirectoryKeyFmt[] = "world_channel_directory:zone:%u:%u";

// 镜像 CreateScene 的令牌序号(PlayerMirrorCreateInFlightComp.token)。与 tlsEcs 同属本线程,先自增再用,恒非 0。
thread_local uint64_t tlsMirrorCreateTokenSeq = 0;

namespace asio = boost::asio;
namespace cobalt = boost::cobalt;
using muduo::net::EventLoop;

struct MirrorCreateResult
{
	GrpcCallOutcome outcome;
	std::optional<::scene_manager::CreateSceneResponse> reply;
};

// 建镜像的一次 CreateScene 接到公共等待体。GrpcDone 的回包只在调用期间借用,在回调里复制成自有结果;
// 生成的 gRPC 客户端恰好回调一次(本地拒绝也延后交付),所以提交恒为接纳。本地停止由等待体交 nullopt,
// 停止在提交内重入时也等提交返回之后才交付。
auto AwaitMirrorCreate(EventLoop& loop, entt::entity node, const ::scene_manager::CreateSceneRequest& request,
	const PlayerSceneSystem::MirrorCreateSubmit& submit)
{
	return loop_async::AwaitCallback<MirrorCreateResult>(loop,
		[node, &request, &submit](auto done) {
			submit(node, request, [done](const GrpcCallOutcome& outcome, const ::scene_manager::CreateSceneResponse* reply) {
				MirrorCreateResult result{outcome, std::nullopt};
				if (reply != nullptr) result.reply.emplace(*reply);
				done(std::move(result));
			});
			return true;
		}, MirrorCreateResult{GrpcCallOutcome{kGrpcStatusUnavailable, "mirror create not submitted"}, std::nullopt});
}

cobalt::task<void> CreateMirrorAndEnter(entt::entity player, entt::entity manager,
	::scene_manager::CreateSceneRequest request, PlayerSceneSystem::MirrorCreateSubmit submit);

// 分线目录在本 zone 的场景控制域实例里(scene_manager 写,docs/design/control-plane-zone-scoping-20261105.md §4.5),
// 不在核心 zoneredis 里。
bool SceneControlRedisReady()
{
	auto& redis = tlsSceneControlRedis.GetConnection();
	return redis && redis->connected();
}

// 异步回调里的身份核对:实体仍存在,且仍是发命令时的那名玩家(实体槽位被复用后 Guid 会不同)。
// playerId 为 0 一律不算同一人:拿不到 Guid 就无从核对,调用方不得带着 0 走异步路径。
bool IsSamePlayer(entt::entity player, uint64_t playerId)
{
	return playerId != 0 && tlsEcs.GetPlayerId(player) == playerId;
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
	SendToPlayerClient(SceneSceneClientPlayerNotifySceneInfoMessageId, message, player);
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
		LOG_WARN << "SendSceneInfo: channel directory read got no reply(Redis disconnected);directory omitted this time, player_id=" << playerId
				 << " scene_config_id=" << sceneConfigId;
		return false;
	}
	if (reply->type == REDIS_REPLY_NIL)
	{
		LOG_DEBUG << "SendSceneInfo: channel directory not published or expired;directory omitted this time, player_id=" << playerId
				  << " scene_config_id=" << sceneConfigId;
		return false;
	}
	if (reply->type == REDIS_REPLY_ERROR)
	{
		LOG_WARN << "SendSceneInfo: channel directory read rejected by Redis;directory omitted this time, player_id=" << playerId
				 << " scene_config_id=" << sceneConfigId << " err=" << (reply->str != nullptr ? reply->str : "");
		return false;
	}
	// 先判 type 再读 str / len;长度保护是因为 ParseFromArray 只收 int。
	if (reply->type != REDIS_REPLY_STRING || reply->str == nullptr ||
		reply->len > static_cast<size_t>(std::numeric_limits<int>::max()))
	{
		LOG_ERROR << "SendSceneInfo: invalid channel directory reply shape;directory omitted this time, player_id=" << playerId
				  << " scene_config_id=" << sceneConfigId << " reply_type=" << reply->type;
		return false;
	}
	if (!out.ParseFromArray(reply->str, static_cast<int>(reply->len)))
	{
		LOG_ERROR << "SendSceneInfo: channel directory parse failed;directory omitted this time, player_id=" << playerId
				  << " scene_config_id=" << sceneConfigId << " bytes=" << reply->len;
		return false;
	}
	if (out.scene_config_id() != sceneConfigId)
	{
		LOG_ERROR << "SendSceneInfo: channel directory does not match current map;directory omitted this time, player_id=" << playerId
				  << " scene_config_id=" << sceneConfigId << " directory_scene_config_id=" << out.scene_config_id();
		return false;
	}
	return true;
}

}  // namespace

std::optional<PlayerSceneSystem::EntryOwnershipGuard::Snapshot>
PlayerSceneSystem::EntryOwnershipGuard::Read(entt::entity player, SessionId requestSession)
{
	if (!tlsEcs.actorRegistry.valid(player)) return std::nullopt;
	const auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
	if (guid == nullptr || guid->value == 0) return std::nullopt;
	Snapshot state;
	state.playerId = guid->value;
	state.indexedPlayer = tlsEcs.GetPlayer(state.playerId);
	if (state.indexedPlayer != entt::null && state.indexedPlayer != player) return std::nullopt;
	if (const auto* session = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player))
	{
		state.sessionId = session->gate_session_id();
		state.snapshotPlayerId = session->player_id();
	}
	const auto session = SessionMap().find(requestSession != 0 ? requestSession : state.sessionId);
	if (session != SessionMap().end()) state.sessionOwner = session->second;
	if (const auto* identity = tlsEcs.actorRegistry.try_get<PlayerLoginIdentityComp>(player))
	{
		state.loginSession = identity->sessionId;
		state.loginGeneration = identity->loginGeneration;
	}
	if (const auto* home = tlsEcs.actorRegistry.try_get<PlayerHomeZoneComp>(player)) state.homeZoneId = home->homeZoneId;
	if (const auto* epoch = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player)) state.ownerEpoch = epoch->epoch;
	return state;
}

PlayerSceneSystem::EntryOwnershipGuard::EntryOwnershipGuard(entt::entity player, SessionId requestSession)
	: player_(player), requestSession_(requestSession), snapshot_(Read(player, requestSession)) {}

bool PlayerSceneSystem::EntryOwnershipGuard::IsCurrent() const
{
	return snapshot_.has_value() && snapshot_ == Read(player_, requestSession_);
}

SceneEntity PlayerSceneSystem::FindSceneById(uint64_t sceneId)
{
	if (sceneId == 0) return entt::null;
	auto view = tlsEcs.sceneRegistry.view<SceneInfoComp>();
	for (const auto scene : view)
		if (view.get<SceneInfoComp>(scene).scene_id() == sceneId) return scene;
	return entt::null;
}

uint32_t PlayerSceneSystem::CheckEnterScene(Guid playerId, SceneEntity scene)
{
	if (!tlsEcs.sceneRegistry.valid(scene)) return kEnterSceneNotFound;
	const auto* info = tlsEcs.sceneRegistry.try_get<SceneInfoComp>(scene);
	if (info == nullptr) return kEnterSceneNotFound;
	if (NavQuerySystem::RequiresNavigation(info->scene_config_id()) &&
		SceneNavManager::Instance().Get(info->scene_config_id()) == nullptr) return kServiceUnavailable;
	return playerId != 0 ? kSuccess : kInvalidParameter;
}

uint32_t PlayerSceneSystem::CheckEnterScene(entt::entity player, SceneEntity scene)
{
	if (!tlsEcs.actorRegistry.valid(player)) return kInvalidParameter;
	const auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
	return CheckEnterScene(guid != nullptr ? guid->value : Guid{0}, scene);
}

// 组队跟随(原第 5 步 OnGetTeamInfo / OnGetLeaderLocation)已迁到 PlayerTeamSystem
// (player_team.{h,cpp},team-system.md §F.2):调用点改为 HandleEnterScene 之后,
// 因为本函数开头的"已在目标场景"幂等早退会让同场景重连漏掉刷新。

bool PlayerSceneSystem::HandleEnterScene(entt::entity player, SceneEntity scene)
{
	const EntryOwnershipGuard ownership(player);
	if (!ownership.IsCurrent()) return false;
	if (const auto result = CheckEnterScene(player, scene); result != kSuccess)
	{
		LOG_ERROR << "Scene entry precheck rejected entity=" << entt::to_integral(player) << " tip=" << result;
		return false;
	}
	if (!tlsEcs.actorRegistry.all_of<Transform>(player))
	{
		LOG_ERROR << "HandleEnterScene: persistent component Transform missing, " << ActorLog(player);
		return false;
	}
	const auto sceneInfo = tlsEcs.sceneRegistry.try_get<SceneInfoComp>(scene);
	if (sceneInfo == nullptr)
	{
		LOG_ERROR << "HandleEnterScene: scene info missing, " << ActorLog(player);
		return false;
	}
	SceneInfoComp targetSceneInfo(*sceneInfo);

	// 0. Idempotency: if player is already in the target scene, skip.
	auto *oldSceneComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
	const auto oldScene = oldSceneComp != nullptr ? oldSceneComp->sceneEntity : SceneEntity{entt::null};
	if (oldScene == scene)
	{
		LOG_INFO << "HandleEnterScene: player " << tlsEcs.GetPlayerId(player)
		         << " already in scene_id=" << targetSceneInfo.scene_id() << ", skipping";
		return true;
	}

	// 同图换线保留坐标,换地图使用目标出生点;判断须在旧场景绑定被替换前完成。
	bool mapChanged = false;
	if (oldScene != entt::null)
	{
		const auto* oldInfo = tlsEcs.sceneRegistry.try_get<SceneInfoComp>(oldScene);
		mapChanged = oldInfo != nullptr && oldInfo->scene_config_id() != targetSceneInfo.scene_config_id();
	}

	// 1. Leave old scene if player is already in one.
	if (oldScene != entt::null)
	{
		LeaveCurrentScene(player);
	}

	// 2. Bind player to the new scene entity.
	tlsEcs.actorRegistry.get_or_emplace<SceneEntityComp>(player).sceneEntity = scene;

	// 3.5 服务器权威落位:Transform 来自 DB(新号是 proto 默认 (0,0,0),旧号可能
	// 存着旧地图/别的场景的坐标),必须在自身 ActorCreate(4.5)与 AOI 广播之前
	// 校验它在目标场景导航网格上,不在就落到场景出生点。否则客户端会本地把
	// 人挪到城内、服务器仍在 (0,0,0),首次移动的 MoveAck 把客户端拉回原点卡死
	// (2026-09-05 移动诊断)。同图换线/重登保留合法坐标,换地图落到目标出生点。
	SceneSpawnSystem::EnsureValidEnterLocation(player, scene, mapChanged);

	// 4. Notify client of scene entry.
	EnterSceneS2C message;
	message.mutable_scene_info()->CopyFrom(targetSceneInfo);
	SendToPlayerClient(SceneSceneClientPlayerNotifyEnterSceneMessageId, message, player);

	// 4.5 补发"自己"的 ActorCreate:AOI 可见性遍历刻意跳过 observer 本身
	// (aoi.cpp ForEachNearbyEntity 的 other != entity 判断),因此
	// 除这里之外没有任何路径把玩家自己的 actor 下发给客户端——单人在线时
	// 客户端将收不到任何 ActorCreate,本地角色/相机绑定/移动控制器全部
	// 无法建立。客户端靠 guid == player_id 识别并绑定本地角色。
	ActorCreateS2C selfCreate;
	const auto netId = actor_net_id::Of(tlsEcs.actorRegistry, player);
	if (netId != 0)
	{
		ViewSystem::FillActorCreateMessageInfo(player, netId, selfCreate);
		SendToPlayerClient(SceneSceneClientPlayerNotifyActorCreateMessageId, selfCreate, player);
	}
	else
	{
		LOG_ERROR << "HandleEnterScene: self actor notification skipped, network id missing";
	}

	LOG_INFO << "HandleEnterScene: player " << tlsEcs.GetPlayerId(player)
			 << " entered scene_id=" << targetSceneInfo.scene_id();
	return true;
}

void PlayerSceneSystem::LeaveCurrentScene(entt::entity player)
{
	if (!tlsEcs.actorRegistry.valid(player) || !tlsEcs.actorRegistry.all_of<SceneEntityComp>(player))
	{
		return;
	}
	AoiSystem::OnLeaveScene(player);
	tlsEcs.actorRegistry.remove<SceneEntityComp, Hex>(player);
}

bool PlayerSceneSystem::RequestEnterMirrorScene(entt::entity player, uint32_t mirrorConfigId, MirrorCreateSubmit submit)
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
		LOG_WARN << "[PlayerBattle] RequestEnterMirrorScene rejected: player battle in progress, player_id="
				 << tlsEcs.GetPlayerId(player);
		return false;
	}

	// 发送侧闸(与 EnterSceneC2S 同一道):归属交接在途,或另一件换图 / 建镜像还在途。下面登记的在途组件只有
	// 一个槽,再登记一次会顶掉上一次的令牌,上一次的完成回调就认领不到:玩家既进不了镜像也收不到提示。
	// 调用方 EnterSceneC2S 已经判过,这里在写入口再守一次,不让别的调用方绕过。
	if (PlayerLifecycleSystem::IsSceneChangeBusy(player))
	{
		LOG_INFO << "RequestEnterMirrorScene rejected: scene change already in flight, player_id=" << tlsEcs.GetPlayerId(player);
		return false;
	}

	const auto* sceneEntityComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
	if (sceneEntityComp == nullptr || sceneEntityComp->sceneEntity == entt::null)
	{
		LOG_WARN << "RequestEnterMirrorScene: " << ActorLog(player)
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
		LOG_WARN << "RequestEnterMirrorScene: PlayerSessionSnapshotComp missing, " << ActorLog(player);
		return false;
	}

	if (!SceneTaskScope::IsAccepting())
	{
		return false;
	}
	const auto smEntity = SelectSceneManagerEntity(playerSessionPB->player_id());
	if (smEntity == entt::null)
	{
		LOG_WARN << "RequestEnterMirrorScene: no SceneManager node available";
		return false;
	}

	::scene_manager::CreateSceneRequest req;
	req.set_scene_conf_id(sourceInfo->scene_config_id());
	req.set_scene_type(::scene_manager::SCENE_TYPE_INSTANCE);
	req.set_zone_id(GetZoneId());
	req.set_source_scene_id(sourceInfo->scene_id());
	req.set_mirror_config_id(mirrorConfigId);
	req.add_creator_ids(playerSessionPB->player_id());
	if (!submit)
	{
		submit = [](entt::entity manager, const ::scene_manager::CreateSceneRequest& request,
			GrpcDone<::scene_manager::CreateSceneResponse> done) {
			auto& registry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);
			scene_manager::SendSceneManagerCreateScene(registry, manager, request, std::move(done));
		};
	}
	return SceneTaskScope::TryStart(CreateMirrorAndEnter(player, smEntity, std::move(req), std::move(submit)), "CreateMirrorAndEnter");
}

namespace
{
cobalt::task<void> CreateMirrorAndEnter(entt::entity originalPlayer, entt::entity manager,
	::scene_manager::CreateSceneRequest request, PlayerSceneSystem::MirrorCreateSubmit submit)
{
	auto& loop = *EventLoop::getEventLoopOfCurrentThread();
	loop.assertInLoopThread();
	const Guid playerId = request.creator_ids(0);
	const uint32_t mirrorConfigId = request.mirror_config_id();

	// 登记必须早于发送。已有有效令牌会被预检拦下，只有过期令牌可以替换。
	const uint64_t token = ++tlsMirrorCreateTokenSeq;
	tlsEcs.actorRegistry.emplace_or_replace<PlayerMirrorCreateInFlightComp>(originalPlayer,
		PlayerMirrorCreateInFlightComp{token, TimeSystem::NowMillisecondsUTC(), mirrorConfigId, request.source_scene_id()});
	LOG_INFO << "RequestEnterMirrorScene: dispatching create_mirror player=" << playerId
			 << " source_scene=" << request.source_scene_id() << " mirror_config=" << mirrorConfigId
			 << " scene_conf=" << request.scene_conf_id() << " token=" << token;

	const auto result = co_await AwaitMirrorCreate(loop, manager, request, submit);
	loop.assertInLoopThread();
	if (!result || (co_await cobalt::this_coro::cancelled) != asio::cancellation_type::none)
	{
		const auto* inFlight = tlsEcs.actorRegistry.try_get<PlayerMirrorCreateInFlightComp>(originalPlayer);
		if (inFlight != nullptr && inFlight->token == token)
		{
			tlsEcs.actorRegistry.remove<PlayerMirrorCreateInFlightComp>(originalPlayer);
		}
		co_return;
	}
	// ── 认领:实体还在、令牌对得上才摘槽;TTL 仍有效才允许提示或自动进场 ──
	const auto player = tlsEcs.GetPlayer(playerId);
	if (player != originalPlayer || !tlsEcs.actorRegistry.valid(player))
	{
		// 玩家在建镜像途中离开了本节点(断线 / 换节点):没有客户端可提示,镜像由 scene_manager 按空场景回收。
		LOG_INFO << "CreateScene done: original creator " << playerId << " not present on this node, dropping token=" << token
				 << " ok=" << result->outcome.ok();
		co_return;
	}
	const auto* inFlight = tlsEcs.actorRegistry.try_get<PlayerMirrorCreateInFlightComp>(player);
	if (inFlight == nullptr || inFlight->token != token)
	{
		// 已被顶替(过了 TTL 又发了一次)或实体已重建(重连回本节点):不是当前等待者,不动它、不提示。
		LOG_INFO << "CreateScene done: stale mirror create for player " << playerId << " token=" << token
				 << " current_token=" << (inFlight != nullptr ? inFlight->token : uint64_t{0}) << ", dropping";
		co_return;
	}
	const bool requestWithinTtl = PlayerLifecycleSystem::IsSceneChangeRequestWithinTtl(inFlight->sentAtMs);
	tlsEcs.actorRegistry.remove<PlayerMirrorCreateInFlightComp>(player);
	if (!requestWithinTtl)
	{
		// 闸已允许后来的换图,即使它已经完成也不能再按这次旧结果提示或自动进镜像。
		// 已建好的空镜像由 scene_manager 回收;UTC 回拨同样按失效处理。
		LOG_INFO << "CreateScene done: expired mirror create for player " << playerId << " token=" << token
				 << " mirror_config=" << mirrorConfigId << ", dropping";
		co_return;
	}

	// ── 建镜像没成 ──
	const auto& outcome = result->outcome;
	const auto& reply = result->reply;
	if (!outcome.ok() || !reply)
	{
		// 结果未知(GrpcDone 契约:非 OK 不代表服务端没执行):回「服务器繁忙,请稍后再试」而不是"建镜像失败"。
		// 镜像若其实已经建好,由 scene_manager 按空场景回收。
		LOG_WARN << "SceneManager.CreateScene transport failure for player " << playerId << " code=" << outcome.code
				 << " msg=" << outcome.message << " mirror_config=" << mirrorConfigId << " token=" << token;
		PlayerTipSystem::SendToPlayer(player, kEnterSceneServerBusy, {});
		co_return;
	}
	if (reply->error_code() != 0 || reply->scene_id() == 0)
	{
		// scene_manager 拒绝(业务失败应答不回显 creator_ids,改用每次调用回调之前这条路径告诉不了创建者),
		// 或成功却没给 scene_id。
		LOG_WARN << "SceneManager.CreateScene rejected mirror for player " << playerId << " code=" << reply->error_code()
				 << " msg=" << reply->error_message() << " scene_id=" << reply->scene_id()
				 << " mirror_config=" << mirrorConfigId << " token=" << token;
		PlayerTipSystem::SendToPlayer(player, kEnterSceneFailed, {});
		co_return;
	}

	// ── 建好了:提交点复核,再自动进场 ──
	const uint64_t mirrorSceneId = reply->scene_id();
	// 战斗在途不换场景(设计文档 §5.3 冻结清单):建镜像这一个往返里玩家可能刚进备战。
	if (PlayerBattleSystem::IsInBattle(player))
	{
		LOG_WARN << "[PlayerBattle] mirror auto-enter skipped: player battle in progress, player_id=" << playerId
				 << " mirror_scene=" << mirrorSceneId;
		PlayerTipSystem::SendToPlayer(player, kEnterSceneFailed, {});
		co_return;
	}
	// 自己的槽位已摘,此刻还忙只能是别的换图 / 交接(正常被上面的闸挡住;只在本槽过了 TTL 之后才可能出现)。
	// 交接在途时不提示:交接自己会给结局,补一条"进入场景失败"会让客户端提前结束等待。
	if (PlayerLifecycleSystem::IsSceneChangeBusy(player))
	{
		const bool handoffInFlight = PlayerLifecycleSystem::IsPlayerFrozen(player);
		LOG_WARN << "CreateScene done: player " << playerId << " has another scene change / handoff in flight,"
				 << " skipping auto-enter for mirror_scene=" << mirrorSceneId << " handoff_in_flight=" << handoffInFlight;
		if (!handoffInFlight)
		{
			PlayerTipSystem::SendToPlayer(player, kEnterSceneFailed, {});
		}
		co_return;
	}
	const auto* playerSessionPB = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
	if (playerSessionPB == nullptr)
	{
		// 在线实体缺会话快照是不变量被破坏;没有会话也就送不出提示。
		LOG_ERROR << "CreateScene done: PlayerSessionSnapshotComp missing for creator " << playerId
				  << ", skipping auto-enter for mirror_scene=" << mirrorSceneId;
		co_return;
	}
	const auto smEntity = SelectSceneManagerEntity(playerId);
	if (smEntity == entt::null)
	{
		// 与 EnterSceneC2S 找不到 SceneManager 时同一个提示:服务端暂时不可用,不是客户端的错。
		LOG_WARN << "CreateScene done: no SceneManager node available to follow-up EnterScene for player " << playerId
				 << " mirror_scene=" << mirrorSceneId;
		PlayerTipSystem::SendToPlayer(player, kEnterSceneServerBusy, {});
		co_return;
	}

	const SessionId gateSessionId = playerSessionPB->gate_session_id();
	const NodeId gateNodeId = GetGateNodeId(gateSessionId);
	std::string gateInstanceId;
	if (const auto gateEntityOpt = ResolveLocalZoneGateEntity(gateSessionId))
	{
		auto& gateRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
		if (const auto* gateNodeInfo = gateRegistry.try_get<NodeInfo>(*gateEntityOpt))
		{
			gateInstanceId = gateNodeInfo->node_uuid();
		}
	}

	::scene_manager::EnterSceneRequest req;
	req.set_player_id(playerId);
	req.set_scene_id(mirrorSceneId);
	req.set_session_id(gateSessionId);
	req.set_gate_id(std::to_string(gateNodeId));
	req.set_gate_instance_id(gateInstanceId);
	req.set_gate_zone_id(GetZoneId());
	req.set_zone_id(GetZoneId());

	// 镜像"尽量"与源场景同节点,但不保证:落到别的节点时 scene_manager 会先以 18 暂拒。
	// RequestSceneChange 在发送之前把目标与关联号记进单槽在途记录(PlayerSceneChangeInFlightComp),
	// 应答按号对回来,才能用同一个 scene_id 起交接并重发(见 EnterSceneC2S)。
	const uint64_t correlationId = PlayerLifecycleSystem::RequestSceneChange(player, smEntity, req);

	LOG_INFO << "CreateScene done: dispatched EnterScene player=" << playerId << " mirror_scene=" << mirrorSceneId
			 << " mirror_node=" << reply->node_id() << " token=" << token << " corr=" << correlationId;
}
} // namespace

void PlayerSceneSystem::SendSceneInfo(entt::entity player, bool withChannelDirectory)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		LOG_WARN << "SendSceneInfo: invalid player entity;push skipped, entity=" << entt::to_integral(player);
		return;
	}
	const uint64_t playerId = tlsEcs.GetPlayerId(player);

	const auto* sceneInfo = CurrentSceneInfo(player);
	if (sceneInfo == nullptr)
	{
		LOG_WARN << "SendSceneInfo: player has no scene or scene entity lacks SceneInfoComp;push skipped, player_id=" << playerId;
		return;
	}

	// 只有大世界分线才有目录:副本 / 镜像按 id 进入,没有"线"的概念。
	const bool isWorldChannel = sceneInfo->mirror_config_id() == 0 && sceneInfo->dungeon_config_id() == 0 &&
								sceneInfo->scene_config_id() != 0;
	// 调用方没要目录(withChannelDirectory == false)时不读 Redis:与加目录之前的开销完全相同。
	// 实体上没有 Guid(playerId == 0)时回调里无从核对"还是不是这名玩家",同样不走异步。
	if (!withChannelDirectory || !isWorldChannel || playerId == 0 || !SceneControlRedisReady())
	{
		PushSceneInfo(player, *sceneInfo, nullptr);
		return;
	}

	// 回调要用的东西全部按值带走(sceneInfo 指向 sceneRegistry 的组件存储,应答回来时场景可能已销毁)。
	// 捕获一律用 init-capture:MSVC 对 entt::entity 的简单捕获有 C3495(.github/copilot-instructions.md)。
	const uint64_t sceneId = sceneInfo->scene_id();
	const uint32_t sceneConfigId = sceneInfo->scene_config_id();
	const int ret = tlsSceneControlRedis.GetConnection()->command(
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
				LOG_DEBUG << "SendSceneInfo: player left the original scene before reply;discarded, player_id=" << playerId
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
		LOG_WARN << "SendSceneInfo: channel directory read command not sent;directory omitted this time, player_id=" << playerId
				 << " scene_config_id=" << sceneConfigId << " ret=" << ret;
		PushSceneInfo(player, *sceneInfo, nullptr);
	}
}

