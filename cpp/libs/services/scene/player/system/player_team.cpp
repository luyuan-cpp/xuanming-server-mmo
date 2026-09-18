#include "player_team.h"

#include <limits>
#include <string>

#include "muduo/base/Logging.h"

#include "thread_context/ecs_context.h"
#include "thread_context/node_context_manager.h"
#include "thread_context/redis_manager.h"

#include "network/network_utils.h"
#include "network/node_utils.h"
#include "type_alias/player_session_type_alias.h"

#include "battle/system/player_battle.h"
#include "modules/scene/comp/scene_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/player_lifecycle.h" // IsSceneChangeBusy / NoteSceneChangeRequested:普通 EnterScene 的发送侧闸

#include "grpc_client/scene_manager/scene_manager_service_grpc_client.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_network_comp.pb.h"
#include "proto/common/component/team_comp.pb.h"
#include "proto/scene/scene_info.pb.h"
#include "proto/scene_manager/scene_manager_service.pb.h"
#include "proto/scene_manager/storage.pb.h"

// PlayerTeamRefreshEvent:全局命名空间(team_event.proto 无 package),event_id=48,
// 由 rpc_event_registry.cpp 的 DispatchProtoEvent 解包后进程内分发到 TeamEventHandler。
#include "proto/common/event/team_event.pb.h"

namespace
{
	// key 契约见 player_team.h 头注释;与 Go 侧 go/match/internal/team/keys.go 同一份口径。
	constexpr char kTeamIndexKeyFmt[] = "team:player:%llu";
	constexpr char kTeamProjectionKeyFmt[] = "team:%llu";
	constexpr char kTeamRecordKeyFmt[] = "team:rec:%llu";
	constexpr char kPlayerLocationKeyFmt[] = "player:%llu:location";
	constexpr char kBattleLockKeyFmt[] = "battle:lock:%llu";

	bool RedisReady()
	{
		auto& redis = tlsRedis.GetZoneRedis();
		return redis && redis->connected();
	}

	uint64_t GuidOf(entt::entity player)
	{
		const auto* guid = tlsEcs.actorRegistry.try_get<Guid>(player);
		return guid != nullptr ? *guid : 0;
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
	// 见 enterscenelogic.go samePhysicalNode 分支),与在途的交接互相覆盖;应答还会被
	// scene_manager_response_handler 当成传送应答喂给 HandleTravelEnterSceneReply。
	// 与仓库里其它业务系统按 PlayerFrozenComp 拦写(buff / skill / afk / 属性同步)同一道闸。
	bool IsOwnershipInFlight(entt::entity player)
	{
		return tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player) ||
			   tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player);
	}

	// 该玩家已有一条普通 EnterScene 在途(客户端换图 / 镜像自动进场 / 上一次跟随),应答还没回来。
	// EnterSceneResponse 只回显 player_id:两条同时在途,跟随的应答会把客户端那条记下的目标
	// (PlayerSceneChangeInFlightComp)摘掉,客户端那条随后到达的 18 找不到目标、同 zone 交接不发起,
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

	// 解析 MGET 数组里的 PlayerLocation 元素。NIL / 非字符串 / 解析失败都返回 false。
	bool ParseLocationElement(const redisReply* element, storage::PlayerLocation& out)
	{
		if (element == nullptr || element->type != REDIS_REPLY_STRING)
		{
			return false;
		}
		if (element->len > static_cast<size_t>(std::numeric_limits<int>::max()))
		{
			return false;
		}
		return out.ParseFromArray(element->str, static_cast<int>(element->len));
	}

	uint64_t CurrentSceneId(entt::entity player)
	{
		const auto* sceneEntity = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
		if (sceneEntity == nullptr)
		{
			return 0;
		}
		// SceneEntityComp::sceneEntity 是 sceneRegistry 的句柄,SceneInfoComp 只挂在 sceneRegistry 上
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
		LOG_DEBUG << "[PlayerTeam] 刷新信号丢弃: 玩家不在本节点, player_id=" << playerId;
		return;
	}
	RefreshMembership(player, FollowMode::kRefreshOnly);
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
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}
	const uint64_t playerId = GuidOf(player);
	if (playerId == 0)
	{
		return;
	}
	if (!RedisReady())
	{
		LOG_WARN << "[PlayerTeam] 刷新成员关系跳过(Redis 未连接), player_id=" << playerId;
		return;
	}

	tlsRedis.GetZoneRedis()->command(
		[player, playerId, mode](hiredis::Hiredis*, redisReply* reply) {
			if (!IsSamePlayer(player, playerId))
			{
				return;
			}
			const TeamIndexReply parsed = ParseTeamIndexReply(reply);
			if (parsed.kind == TeamIndexReplyKind::kUnknown)
			{
				LOG_WARN << "[PlayerTeam] metric=team_index_read_unknown player_id=" << playerId
						 << ",HMGET 回复错误或形状非法,保持现有 TeamId 不变";
				return;
			}
			const bool keyMissing = parsed.kind == TeamIndexReplyKind::kKeyMissing;
			ApplyMembership(player, parsed.teamId, parsed.epoch, keyMissing);

			if (mode == FollowMode::kRefreshOnly)
			{
				return;
			}
			// 以组件的最终状态为准继续:epoch 判定为乱序而没有应用时,组件仍是更新的那份
			const auto* teamId = tlsEcs.actorRegistry.try_get<TeamId>(player);
			if (teamId == nullptr || teamId->team_id() == 0)
			{
				return;
			}
			LoadTeamInfo(player, teamId->team_id(), mode);
		},
		(std::string("HMGET ") + kTeamIndexKeyFmt + " tid epoch").c_str(), playerId);
}

void PlayerTeamSystem::ApplyMembership(entt::entity player, uint64_t teamId, uint64_t epoch, bool keyMissing)
{
	const auto* current = tlsEcs.actorRegistry.try_get<TeamId>(player);
	const bool hasComponent = current != nullptr;
	const uint64_t currentEpoch = hasComponent ? current->membership_epoch() : 0;
	if (!ShouldApplyMembership(currentEpoch, hasComponent, epoch, keyMissing))
	{
		return;
	}

	// 先抄旧值:emplace_or_replace / remove 之后 current 指针失效
	const uint64_t oldTeamId = hasComponent ? current->team_id() : 0;
	const uint64_t newTeamId = keyMissing ? 0 : teamId;

	if (newTeamId != 0)
	{
		auto& component = tlsEcs.actorRegistry.emplace_or_replace<TeamId>(player);
		component.set_team_id(newTeamId);
		component.set_membership_epoch(epoch);
	}
	else
	{
		tlsEcs.actorRegistry.remove<TeamId>(player);
	}

	if (oldTeamId != newTeamId)
	{
		LOG_INFO << "[PlayerTeam] TeamId 变更: player_id=" << GuidOf(player) << " old_team_id=" << oldTeamId
				 << " new_team_id=" << newTeamId << " epoch=" << epoch << " key_missing=" << keyMissing;
		RefreshTeammateAoi(player);
	}
}

void PlayerTeamSystem::LoadTeamInfo(entt::entity player, uint64_t teamId, FollowMode mode)
{
	const uint64_t playerId = GuidOf(player);
	if (!RedisReady())
	{
		LOG_WARN << "[PlayerTeam] 读队伍投影跳过(Redis 未连接), player_id=" << playerId << " team_id=" << teamId;
		return;
	}

	tlsRedis.GetZoneRedis()->command(
		[player, playerId, teamId, mode](hiredis::Hiredis*, redisReply* reply) {
			if (!IsSamePlayer(player, playerId))
			{
				return;
			}
			if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
			{
				LOG_WARN << "[PlayerTeam] metric=team_projection_read_unknown player_id=" << playerId
						 << " team_id=" << teamId << ",GET 投影失败";
				return;
			}

			if (reply->type == REDIS_REPLY_NIL)
			{
				// 投影缺失:回查权威记录,区分"投影丢了(未知)"与"队伍已不存在(无队)"
				if (!RedisReady())
				{
					return;
				}
				tlsRedis.GetZoneRedis()->command(
					[player, playerId, teamId](hiredis::Hiredis*, redisReply* existsReply) {
						if (!IsSamePlayer(player, playerId))
						{
							return;
						}
						if (existsReply == nullptr || existsReply->type != REDIS_REPLY_INTEGER)
						{
							return; // 回复错误 = 未知
						}
						if (existsReply->integer != 0)
						{
							// 记录还在,投影只是丢了:未知,不清组件也不跟随,等 team 下一次提交/触碰重写投影
							LOG_WARN << "[PlayerTeam] metric=team_projection_missing player_id=" << playerId
									 << " team_id=" << teamId << ",记录仍在,本次不跟随";
							return;
						}
						// 记录也不在:与 Go 侧口径一致,按无队处理
						const auto* current = tlsEcs.actorRegistry.try_get<TeamId>(player);
						if (current != nullptr && current->team_id() == teamId)
						{
							ApplyMembership(player, 0, 0, /*keyMissing=*/true);
						}
					},
					(std::string("EXISTS ") + kTeamRecordKeyFmt).c_str(), teamId);
				return;
			}

			if (reply->type != REDIS_REPLY_STRING ||
				reply->len > static_cast<size_t>(std::numeric_limits<int>::max()))
			{
				return;
			}
			TeamInfo teamInfo;
			if (!teamInfo.ParseFromArray(reply->str, static_cast<int>(reply->len)))
			{
				LOG_ERROR << "[PlayerTeam] 队伍投影解析失败, player_id=" << playerId << " team_id=" << teamId;
				return;
			}
			if (teamInfo.team_id() != teamId)
			{
				return;
			}
			bool selfIsMember = false;
			for (const uint64_t memberId : teamInfo.members())
			{
				if (memberId == playerId)
				{
					selfIsMember = true;
					break;
				}
			}
			if (!selfIsMember)
			{
				// 投影里没有自己:TeamId 可能刚过时,等下一次刷新信号,不跟随
				return;
			}

			const uint64_t leaderId = teamInfo.leader_id();
			if (leaderId == 0)
			{
				return;
			}
			if (leaderId != playerId)
			{
				CheckFollowLeader(player, leaderId);
				return;
			}

			// 队长自己进场:对本节点上的其他成员逐个刷新并跟随,不再扇出(防循环)
			if (mode != FollowMode::kFollowLeaderAndFanout)
			{
				return;
			}
			for (const uint64_t memberId : teamInfo.members())
			{
				if (memberId == playerId)
				{
					continue;
				}
				const auto member = tlsEcs.GetPlayer(memberId);
				if (member == entt::null || !tlsEcs.actorRegistry.valid(member))
				{
					continue; // 不在本节点(DV-6):不跨节点拉人
				}
				RefreshAndFollow(member);
			}
		},
		(std::string("GET ") + kTeamProjectionKeyFmt).c_str(), teamId);
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
		LOG_WARN << "[PlayerTeam] 读队长位置跳过(Redis 未连接), player_id=" << playerId << " leader_id=" << leaderId;
		return;
	}

	// 一跳读两个 key:队长位置 + 自己的战斗锁。登录时 InBattleComp 由 PlayerBattleSystem 异步重建,
	// 不依赖两条回调链的跳数先后,直接读锁 fail-closed(§F.2)。
	tlsRedis.GetZoneRedis()->command(
		[player, playerId, leaderId](hiredis::Hiredis*, redisReply* reply) {
			if (!IsSamePlayer(player, playerId))
			{
				return;
			}
			if (reply == nullptr || reply->type != REDIS_REPLY_ARRAY || reply->elements != 2 ||
				reply->element == nullptr || reply->element[1] == nullptr)
			{
				LOG_WARN << "[PlayerTeam] metric=team_follow_skipped reason=redis_error player_id=" << playerId;
				return;
			}
			const redisReply* lockElement = reply->element[1];
			if (lockElement->type != REDIS_REPLY_NIL)
			{
				// 锁存在,或回复形状不认识:都按战斗在途 fail-closed
				LOG_INFO << "[PlayerTeam] metric=team_follow_skipped reason=battle_lock player_id=" << playerId;
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
			if (!ParseLocationElement(reply->element[0], leaderLocation))
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
			if (leaderSceneId == 0 || leaderSceneId == CurrentSceneId(player))
			{
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
				LOG_WARN << "[PlayerTeam] 无可用 SceneManager 节点,跟随放弃, player_id=" << playerId
						 << " leader_scene_id=" << leaderSceneId;
				return;
			}
			auto& smRegistry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);

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

			// 发送之前登记在途(与上面的 IsSceneChangeInFlight 成对):跟随在途期间客户端再发换图会被
			// EnterSceneC2S 以"切换中"拒掉,两条应答不会串号。playerRequested=false:玩家没在等这条
			// 请求的结果,被拒只记日志,不起交接、不回 tip(见 PlayerSceneChangeInFlightComp)。
			// 应答 / 进场路由到达即摘;应答丢失靠短 TTL 失效。
			PlayerLifecycleSystem::NoteSceneChangeRequested(player, leaderSceneId, /*sceneConfigId=*/0,
															/*playerRequested=*/false);
			scene_manager::SendSceneManagerEnterScene(smRegistry, smEntity, request);

			// SceneManager 拒绝时只在其回包处理里记日志,队员留在原场景
			LOG_INFO << "[PlayerTeam] 请求跟随队长切场景: player_id=" << playerId << " leader_id=" << leaderId
					 << " leader_scene_id=" << leaderSceneId;
		},
		(std::string("MGET ") + kPlayerLocationKeyFmt + " " + kBattleLockKeyFmt).c_str(), leaderId, playerId);
}
