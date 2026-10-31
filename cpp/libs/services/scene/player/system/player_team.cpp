#include "player_team.h"
#include "engine/core/type_define/player_guid_comp.h"

#include <hiredis/hiredis.h>

#include <cmath>
#include <cstdlib>
#include <algorithm>
#include <limits>
#include <string>

#include "muduo/base/Logging.h"

#include "thread_context/ecs_context.h"
#include "thread_context/node_context_manager.h"
#include "infra/storage/redis_cluster/redis_cluster_client.h"

#include "network/network_utils.h"
#include "network/node_utils.h"
#include "type_alias/player_session_type_alias.h"

#include "hexagons_grid.h" // Hex:实体是否已被 AoiSystem::Update 放进格子

#include "battle/system/battle_lock_rules.h"
#include "battle/system/player_battle.h"
#include "core/security/service_call_auth.h"
#include "grpc_client/team/team_authority_grpc_client.h"
#include "player/system/role_admission_system.h"
#include "time/system/time.h"
#include "muduo_grpc/channel.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "modules/scene/comp/scene_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/player_lifecycle.h" // IsSceneChangeBusy / RequestSceneChange:普通 EnterScene 的发送侧闸与唯一发送入口
#include "spatial/comp/scene_node_scene_comp.h" // AoiListComp:归队前判断队员与队长是否互相可见
#include "spatial/system/movement.h"            // TeleportWithinScene:归队瞬移

#include "grpc_client/scene_manager/scene_manager_service_grpc_client.h"
#include "proto/common/component/actor_comp.pb.h" // Transform:归队时读队长位置
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_network_comp.pb.h"
#include "proto/common/component/team_comp.pb.h"
#include "proto/scene/scene_info.pb.h"
#include "proto/scene_manager/scene_manager_service.pb.h"
#include "proto/scene_manager/storage.pb.h"

// PlayerTeamRefreshEvent:全局命名空间(team_event.proto 无 package),event_id=48,
// 由 scene_kafka_command_router.cpp 的 DispatchSceneKafkaCommand 白名单解包后直接调用 TeamEventHandler。
#include "proto/common/event/team_event.pb.h"

namespace
{
	// key 契约见 player_team.h 头注释;与 Go 侧 go/match/internal/team/keys.go 同一份口径。
	constexpr std::string_view kTeamReadMethod = "/teampb.TeamAuthority/ReadMembership";
	struct TeamSnapshotVersion { uint64_t epoch{0}, version{0}; };
	entt::entity TeamAuthorityNode(uint64_t playerId)
	{
		auto& registry = tlsNodeContextManager.GetRegistry(common::base::TeamNodeService);
		std::vector<entt::entity> ready;
		for (const auto entity : registry.view<NodeInfo, MuduoGrpcChannelPtr>()) {
			const auto& channel = registry.get<MuduoGrpcChannelPtr>(entity);
			if (channel && channel->GetState(true) == muduo_grpc::ConnectivityState::kReady) ready.push_back(entity);
		}
		std::sort(ready.begin(), ready.end(), [&registry](auto left, auto right) {
			return registry.get<NodeInfo>(left).node_uuid() < registry.get<NodeInfo>(right).node_uuid();
		});
		return ready.empty() ? entt::null : ready[playerId % ready.size()];
	}
	// player:{id}:location 的键名与解析是共享的:player_ownership::LocationRedisKey /
	// player_ownership::ParsePlayerLocationElement(player_ownership_comp.h / player_lifecycle.h)。

	// 战斗锁与位置都是玩家域键,走 tlsPlayerRedis。这里为真只说明至少一个节点连着,具体 slot 可不可用要看 command() 的返回值。
	bool RedisReady()
	{
		return tlsPlayerRedis.connected();
	}

	uint64_t GuidOf(entt::entity player)
	{
		const auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
		return guid != nullptr ? guid->value : 0;
	}

	// 多跳回调的统一核对:实体仍存在且仍是发命令时的那个玩家(槽位复用后 Guid 会不同)。
	bool IsSamePlayer(entt::entity player, uint64_t playerId)
	{
		return playerId != 0 && tlsEcs.actorRegistry.valid(player) && GuidOf(player) == playerId;
	}

	// 归属正在交接中的实体不得跟随(cross-zone-scene-travel.md CZ-4/CZ-5、player_ownership_comp.h)。
	// 交接在途时实体与 gate 会话都还在,所以 HasLiveSession 看不出来。两个组件由
	// PlayerLifecycleSystem::StartTravelHandoff 成对挂上(跨 zone 传送 / 同 zone 跨节点换图),查两个是防御:
	//   PlayerFrozenComp        —— 输入已冻结,盘上那份才是要交给目标节点的真值;
	//   PlayerTravelHandoffComp —— 交接意图(存盘在途,或 handoff 标记已写、EnterScene 已发)。
	// 此刻本节点手里的状态已落盘且不得再写,再替他发一次 EnterScene 会把刚交接出去的玩家按
	// "同节点换图"重新落回本节点(scene_manager 对同物理节点的落点不过换手门、不铸 epoch,
	// 见 enterscenelogic.go samePhysicalNode 分支),与在途的交接互相覆盖。应答虽已按 correlation_id
	// 对号、不会再被当成传送应答,这道闸仍然必需:要挡住的是这次同节点重落本身把交接覆盖掉。
	// 与仓库里其它业务系统按 PlayerFrozenComp 拦写(buff / skill / afk / 属性同步)同一道闸。
	bool IsOwnershipInFlight(entt::entity player)
	{
		return tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player) ||
			   tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player);
	}

	// 该玩家已有一条普通 EnterScene 在途(客户端换图 / 镜像自动进场 / 上一次跟随),应答还没回来。
	// 在途记录(PlayerSceneChangeInFlightComp)只有一个槽,应答按 correlation_id 对号:跟随若此时再登记,
	// 会覆盖客户端那条的目标与号,客户端那条随后到达的 18 对不上号而被丢弃、同 zone 交接不发起,
	// 玩家既没换成图也收不到任何提示。与 EnterSceneC2S / 镜像自动进场共用同一道发送侧闸,
	// 玩家主动换图优先:跟随让路,下一次刷新信号(队长再换图 / 自己进场)会再查一遍。
	bool IsSceneChangeInFlight(entt::entity player)
	{
		return PlayerLifecycleSystem::IsSceneChangeBusy(player);
	}

	// 会话仍然活着才允许请求 SceneManager(照 player_lifecycle.cpp HandlePlayerAsyncSaved 的判法):
	// 断线宽限期内仍绑在旧 session 上的实体,拿旧 gate_session_id 去切场景会改写离线玩家的
	// location,还会产生一次无效的 gate 路由。
	bool HasLiveSession(entt::entity player, uint64_t playerId)
	{
		if (tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player))
		{
			return false;
		}
		const auto* snapshot = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
		if (snapshot == nullptr || snapshot->gate_session_id() == 0 ||
			snapshot->gate_session_id() == kInvalidSessionId)
		{
			return false;
		}
		const auto& sessions = SessionMap();
		const auto it = sessions.find(snapshot->gate_session_id());
		return it != sessions.end() && it->second == playerId;
	}

	uint64_t CurrentSceneId(entt::entity player)
	{
		const auto* sceneEntity = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
		if (sceneEntity == nullptr)
		{
			return 0;
		}
		const auto* sceneInfo = tlsEcs.sceneRegistry.try_get<SceneInfoComp>(sceneEntity->sceneEntity);
		return sceneInfo != nullptr ? sceneInfo->scene_id() : 0;
	}
} // namespace

void PlayerTeamSystem::OnEnteredScene(entt::entity player)
{
	RefreshMembership(player, FollowMode::kFollowLeaderAndFanout);
}

void PlayerTeamSystem::OnRefreshEvent(const PlayerTeamRefreshEvent& event)
{
	const uint64_t playerId = event.player_id();
	const auto player = tlsEcs.GetPlayer(playerId);
	if (player == entt::null || !tlsEcs.actorRegistry.valid(player))
	{
		// 玩家不在本节点(已下线 / 已被改派):下次进场会自己拉取,丢弃信号即可
		LOG_DEBUG << "[PlayerTeam] Refresh signal discarded: player is not on this node, player_id=" << playerId;
		return;
	}
	// 入队即跟随(J-12 修订):信号只发给 TeamId 变了的人,被移出者刷新后 TeamId 为 0、
	// 建队者是队长,两者都不会走到跟随;真正被带走的只有新加入的队员。不扇出。
	RefreshMembership(player, FollowMode::kFollowLeader);
}

void PlayerTeamSystem::OnBattleFreezeCleared(entt::entity player)
{
	RefreshAndFollow(player);
}

void PlayerTeamSystem::RefreshAndFollow(entt::entity player)
{
	RefreshMembership(player, FollowMode::kFollowLeader);
}

void PlayerTeamSystem::RefreshMembership(entt::entity player, FollowMode mode)
{
    if (!tlsEcs.actorRegistry.valid(player)) return;
    const uint64_t playerId = GuidOf(player);
    const auto admission = RoleAdmissionSystem::ConfirmedIdentity(playerId);
    const char* secret = std::getenv("MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE");
    if (!admission || secret == nullptr) return;
    const auto node = TeamAuthorityNode(playerId);
    if (node == entt::null) return;
    teampb::ReadTeamMembershipRequest request;
    *request.mutable_admission() = *admission;
    if (!service_call_auth::Sign(kTeamReadMethod, request, *request.mutable_auth(), "scene", secret,
                                TimeSystem::NowMillisecondsUTC())) return;
    const auto digest = service_call_auth::Digest(request);
    auto& registry = tlsNodeContextManager.GetRegistry(common::base::TeamNodeService);
    teampb::SendTeamAuthorityReadMembership(registry, node, request,
        [player, playerId, mode, original = *admission, digest](const GrpcCallOutcome& outcome,
                                                              const teampb::ReadTeamMembershipResponse* reply) {
            if (!outcome.ok() || reply == nullptr || !IsSamePlayer(player, playerId)) return;
            const auto current = RoleAdmissionSystem::ConfirmedIdentity(playerId);
            const char* responseSecret = std::getenv("MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE");
            if (!current || current->SerializeAsString() != original.SerializeAsString() || responseSecret == nullptr
                || reply->request_digest() != digest
                || !service_call_auth::Verify(std::string(kTeamReadMethod) + "/reply", *reply, reply->auth(),
                                              "team", responseSecret, TimeSystem::NowMillisecondsUTC())) return;
            const auto* applied = tlsEcs.actorRegistry.try_get<TeamSnapshotVersion>(player);
            if (applied != nullptr && (reply->membership_epoch() < applied->epoch
                || (reply->membership_epoch() == applied->epoch && reply->version() < applied->version))) return;
            const auto& record = reply->record();
            if (reply->team_id() != 0) {
                if (reply->membership_epoch() == 0 || reply->version() == 0 || record.team_id() != reply->team_id()
                    || record.leader_id() == 0 || record.members_size() < 1 || record.members_size() > 5) return;
                bool member = false, leader = false;
                for (const auto& item : record.members()) {
                    member = member || item.player_id() == playerId;
                    leader = leader || item.player_id() == record.leader_id();
                }
                if (!member || !leader) return;
            } else if (record.team_id() != 0) return;
            const auto* previous = tlsEcs.actorRegistry.try_get<TeamId>(player);
            if (previous != nullptr && previous->membership_epoch() == reply->membership_epoch()
                && previous->team_id() != reply->team_id()) return;
            ApplyMembership(player, reply->team_id(), reply->membership_epoch());
            tlsEcs.actorRegistry.emplace_or_replace<TeamSnapshotVersion>(player, reply->membership_epoch(), reply->version());
            if (mode == FollowMode::kRefreshOnly || reply->team_id() == 0) return;
            if (record.leader_id() != playerId) {
                CheckFollowLeader(player, record.leader_id());
                return;
            }
            if (mode != FollowMode::kFollowLeaderAndFanout) return;
            for (const auto& item : record.members()) {
                if (item.player_id() == playerId) continue;
                const auto member = tlsEcs.GetPlayer(item.player_id());
                if (member != entt::null && tlsEcs.actorRegistry.valid(member)) RefreshAndFollow(member);
            }
        });
}
void PlayerTeamSystem::ApplyMembership(entt::entity player, uint64_t teamId, uint64_t epoch)
{
	const auto* current = tlsEcs.actorRegistry.try_get<TeamId>(player);
	const bool hasComponent = current != nullptr;
	const uint64_t currentEpoch = hasComponent ? current->membership_epoch() : 0;
	if (!ShouldApplyMembership(currentEpoch, hasComponent, epoch))
	{
		return;
	}

	// 先抄旧值:emplace_or_replace / remove 之后 current 指针失效
	const uint64_t oldTeamId = hasComponent ? current->team_id() : 0;

	// 无队也保留epoch墓碑，离队回包之后到达的旧入队回复不能复活成员关系。
	{
		auto& component = tlsEcs.actorRegistry.emplace_or_replace<TeamId>(player);
		component.set_team_id(teamId);
		component.set_membership_epoch(epoch);
	}

	if (oldTeamId != teamId)
	{
		LOG_INFO << "[PlayerTeam] TeamId changed: player_id=" << GuidOf(player) << " old_team_id=" << oldTeamId
				 << " new_team_id=" << teamId << " epoch=" << epoch;
		RefreshTeammateAoi(player);
	}
}

void PlayerTeamSystem::CheckFollowLeader(entt::entity player, uint64_t leaderId)
{
	const uint64_t playerId = GuidOf(player);
	if (PlayerBattleSystem::IsInBattle(player))
	{
		LOG_INFO << "[PlayerTeam] metric=team_follow_skipped reason=in_battle player_id=" << playerId;
		return;
	}
	if (IsOwnershipInFlight(player))
	{
		LOG_INFO << "[PlayerTeam] metric=team_follow_skipped reason=ownership_in_flight player_id=" << playerId;
		return;
	}
	if (IsSceneChangeInFlight(player))
	{
		LOG_INFO << "[PlayerTeam] metric=team_follow_skipped reason=scene_change_in_flight player_id=" << playerId;
		return;
	}
	if (!HasLiveSession(player, playerId))
	{
		LOG_INFO << "[PlayerTeam] metric=team_follow_skipped reason=session_not_live player_id=" << playerId;
		return;
	}
	if (!RedisReady())
	{
		LOG_WARN << "[PlayerTeam] Leader location read skipped(Redis disconnected), player_id=" << playerId << " leader_id=" << leaderId;
		return;
	}

	// 两跳读:先读自己的战斗锁,再读队长位置。登录时 InBattleComp 由 PlayerBattleSystem 异步重建,
	// 不依赖两条回调链的跳数先后,直接读锁 fail-closed(§F.2)。两把键属于不同玩家、不同 slot,不能并进一条 MGET
	// (docs/design/redis-player-store-cluster.md §5.3)。顺序依赖只有「本人删锁先于本人读锁」
	// (PlayerBattleSystem 先发条件删锁再摘组件、摘组件才触发这里),两条命令都在本人 slot。
	const int ret = tlsPlayerRedis.command(
		[player, playerId, leaderId](hiredis::Hiredis*, redisReply* reply) {
			if (!IsSamePlayer(player, playerId))
			{
				return;
			}
			if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
			{
				LOG_WARN << "[PlayerTeam] metric=team_follow_skipped reason=redis_error player_id=" << playerId;
				return;
			}
			if (reply->type != REDIS_REPLY_NIL)
			{
				// 锁存在,或回复形状不认识:都按战斗在途 fail-closed
				LOG_INFO << "[PlayerTeam] metric=team_follow_skipped reason=battle_lock player_id=" << playerId;
				return;
			}
			ReadLeaderLocationForFollow(player, playerId, leaderId);
		},
		(std::string("GET ") + battle_lock::kBattleLockKeyFmt).c_str(), playerId);
	if (ret != REDIS_OK)
	{
		LOG_WARN << "[PlayerTeam] metric=team_follow_skipped reason=redis_error player_id=" << playerId;
	}
}

void PlayerTeamSystem::ReadLeaderLocationForFollow(entt::entity player, uint64_t playerId, uint64_t leaderId)
{
	if (!RedisReady())
	{
		LOG_WARN << "[PlayerTeam] Leader location read skipped(Redis disconnected), player_id=" << playerId << " leader_id=" << leaderId;
		return;
	}
	const std::string leaderLocationKey = player_ownership::LocationRedisKey(leaderId);
	const int ret = tlsPlayerRedis.command(
		[player, playerId, leaderId](hiredis::Hiredis*, redisReply* reply) {
			if (!IsSamePlayer(player, playerId))
			{
				return;
			}
			if (reply == nullptr)
			{
				LOG_WARN << "[PlayerTeam] metric=team_follow_skipped reason=redis_error player_id=" << playerId;
				return;
			}
			// 回调期间可能刚进入战斗 / 刚断线 / 刚被发起跨 zone 交接:重新核对
			if (PlayerBattleSystem::IsInBattle(player))
			{
				LOG_INFO << "[PlayerTeam] metric=team_follow_skipped reason=in_battle player_id=" << playerId;
				return;
			}
			if (IsOwnershipInFlight(player))
			{
				LOG_INFO << "[PlayerTeam] metric=team_follow_skipped reason=ownership_in_flight player_id=" << playerId;
				return;
			}
			if (IsSceneChangeInFlight(player))
			{
				LOG_INFO << "[PlayerTeam] metric=team_follow_skipped reason=scene_change_in_flight player_id=" << playerId;
				return;
			}
			if (!HasLiveSession(player, playerId))
			{
				LOG_INFO << "[PlayerTeam] metric=team_follow_skipped reason=session_not_live player_id=" << playerId;
				return;
			}

			storage::PlayerLocation leaderLocation;
			if (!player_ownership::ParsePlayerLocationElement(reply, leaderLocation))
			{
				return; // 队长不在任何场景(或位置缺失/损坏):不跟随
			}
			if (leaderLocation.zone_id() != GetZoneId())
			{
				LOG_INFO << "[PlayerTeam] metric=team_follow_skipped reason=cross_zone player_id=" << playerId
						 << " leader_id=" << leaderId << " leader_zone_id=" << leaderLocation.zone_id();
				return;
			}
			// PlayerLocation.node_id 是十进制字符串(scene_manager strconv.FormatUint 写入)
			if (leaderLocation.node_id() != std::to_string(GetNodeInfo().node_id()))
			{
				LOG_INFO << "[PlayerTeam] metric=team_follow_skipped reason=cross_node player_id=" << playerId
						 << " leader_id=" << leaderId << " leader_node_id=" << leaderLocation.node_id();
				return;
			}

			const uint64_t leaderSceneId = leaderLocation.scene_id();
			if (leaderSceneId == 0)
			{
				return;
			}
			if (leaderSceneId == CurrentSceneId(player))
			{
				// 已经和队长在同一个场景实例:不用换图,只需要把人带到队长看得见的地方
				RegroupToLeader(player, leaderId);
				return;
			}

			const auto* session = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
			if (session == nullptr)
			{
				return;
			}

			// node_id 不是 entt 实体句柄(network_utils.h 禁令),必须走 ResolveLocalZoneGateEntity,
			// 否则 gate_instance_id 为空,scene_manager 侧的 gate 防僵尸过滤会失效
			const NodeId gateNodeId = GetGateNodeId(session->gate_session_id());
			std::string gateInstanceId;
			if (const auto gateEntityOpt = ResolveLocalZoneGateEntity(session->gate_session_id()))
			{
				auto& gateRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
				if (const auto* gateNodeInfo = gateRegistry.try_get<NodeInfo>(*gateEntityOpt))
				{
					gateInstanceId = gateNodeInfo->node_uuid();
				}
			}

			const auto smEntity = GetSceneManagerEntity(playerId);
			if (smEntity == entt::null)
			{
				LOG_WARN << "[PlayerTeam] No available SceneManager node;follow abandoned, player_id=" << playerId
						 << " leader_scene_id=" << leaderSceneId;
				return;
			}

			::scene_manager::EnterSceneRequest request;
			request.set_player_id(playerId);
			request.set_scene_id(leaderSceneId);
			request.set_session_id(session->gate_session_id());
			request.set_gate_id(std::to_string(gateNodeId));
			request.set_gate_instance_id(gateInstanceId);
			request.set_gate_zone_id(GetZoneId());
			request.set_zone_id(leaderLocation.zone_id());
			// 不设 request_id:同 player_lifecycle.cpp DispatchEmergencyRelocate 的理由,
			// SceneManager 的 60s 去重会吞掉同一玩家短时间内的第二次合法跟随

			// RequestSceneChange 在发送之前登记单槽在途记录(与上面的 IsSceneChangeInFlight 成对):跟随在途
			// 期间客户端再发换图会被 EnterSceneC2S 以"切换中"拒掉,不会覆盖这条记录;应答按 correlation_id
			// 对号。playerRequested=false:玩家没在等这条请求的结果,被拒只记日志,不起交接、不回 tip
			// (见 PlayerSceneChangeInFlightComp)。应答 / 进场路由到达即摘;应答丢失靠短 TTL 失效。
			const uint64_t correlationId = PlayerLifecycleSystem::RequestSceneChange(player, smEntity, request,
																					 /*playerRequested=*/false);

			// SceneManager 拒绝时只在其回包处理里记日志,队员留在原场景
			LOG_INFO << "[PlayerTeam] Requesting scene change to follow leader: player_id=" << playerId << " leader_id=" << leaderId
					 << " leader_scene_id=" << leaderSceneId << " corr=" << correlationId;
		},
		"GET %s", leaderLocationKey.c_str());
	if (ret != REDIS_OK)
	{
		LOG_WARN << "[PlayerTeam] metric=team_follow_skipped reason=redis_error player_id=" << playerId;
	}
}

void PlayerTeamSystem::RegroupToLeader(entt::entity player, uint64_t leaderId)
{
	const uint64_t playerId = GuidOf(player);
	const auto leader = tlsEcs.GetPlayer(leaderId);
	if (leader == entt::null || !tlsEcs.actorRegistry.valid(leader) || leader == player)
	{
		return; // 队长实体不在本节点(位置记录滞后 / 刚下线):没有可对齐的目标
	}
	// Redis 里的位置记录可能滞后于 ECS:以两个实体此刻实际绑定的场景为准,不一致就等下一次信号
	const uint64_t sceneId = CurrentSceneId(player);
	if (sceneId == 0 || CurrentSceneId(leader) != sceneId)
	{
		return;
	}
	// 队长正在被交接出本节点:他马上就不在这个场景了,不往一个即将消失的位置上凑
	if (IsOwnershipInFlight(leader))
	{
		LOG_INFO << "[PlayerTeam] metric=team_regroup_skipped reason=leader_ownership_in_flight player_id="
				 << playerId << " leader_id=" << leaderId;
		return;
	}

	const auto* transform = tlsEcs.actorRegistry.try_get<Transform>(player);
	const auto* leaderTransform = tlsEcs.actorRegistry.try_get<Transform>(leader);
	if (transform == nullptr || leaderTransform == nullptr)
	{
		return;
	}

	// 兴趣表只有在双方都已经被 AoiSystem::Update 放进格子(有 Hex)之后才说明问题。
	// 任一方刚进场 / 刚被瞬移、还没轮到下一次 Update 时表是空的:那不是"看不见",而是"还没评估"。
	// 此时只看距离 —— 相距不超过 kTeamRegroupDistance(< 可见半径 10)的两个人必在同格或邻格,
	// 后进格的那一方做首次进场评估时会把双向可见性一起建好。
	// 不这样区分的话:进场链(三跳 Redis 回调通常早于首次 Update)每次都会瞬移;
	// 一次瞬移之后、下一次 Update 之前再来一次跟随检查,还会重复瞬移。
	const bool visibilityEvaluated = tlsEcs.actorRegistry.any_of<Hex>(player) &&
									 tlsEcs.actorRegistry.any_of<Hex>(leader);
	const auto* ownAoi = tlsEcs.actorRegistry.try_get<AoiListComp>(player);
	const auto* leaderAoi = tlsEcs.actorRegistry.try_get<AoiListComp>(leader);
	const bool mutuallyVisible = !visibilityEvaluated ||
								 (ownAoi != nullptr && ownAoi->Contains(leader) && leaderAoi != nullptr &&
								  leaderAoi->Contains(player));
	// 服务器坐标的水平面是 x/y(z 为高度)
	const double dx = transform->location().x() - leaderTransform->location().x();
	const double dy = transform->location().y() - leaderTransform->location().y();
	const double distance = std::sqrt(dx * dx + dy * dy);
	if (!ShouldRegroup(distance, mutuallyVisible))
	{
		return;
	}

	// 落在队长脚下:那是队长刚被导航裁决过的合法点。排成一列是客户端的事 ——
	// 队员客户端看到队长之后会自己走到身后的槽位。
	// 先拷一份再传:TeleportWithinScene 会改写 player 的 Transform,不把别的实体组件的引用带进去。
	const Vector3 targetLocation = leaderTransform->location();
	const Rotation targetRotation = leaderTransform->rotation();
	LOG_INFO << "[PlayerTeam] metric=team_regroup player_id=" << playerId << " leader_id=" << leaderId
			 << " scene_id=" << sceneId << " distance=" << distance << " mutually_visible=" << mutuallyVisible
			 << " visibility_evaluated=" << visibilityEvaluated;
	MovementSystem::TeleportWithinScene(player, targetLocation, targetRotation, kTeleportReasonTeamRegroup);
}
