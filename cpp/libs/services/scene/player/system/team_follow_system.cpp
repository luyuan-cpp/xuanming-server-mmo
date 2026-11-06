#include "team_follow_system.h"

#include <cmath>
#include <optional>
#include <utility>
#include <vector>

#include "muduo/base/Logging.h"

#include "actor/actor_net_id.h"
#include "core/constants/fps.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/type_define/type_define.h" // kInvalidSessionId
#include "frame/manager/frame_time.h"
#include "generated/attribute/actorbaseattributess2c_attribute_sync.h"
#include "modules/scene/comp/scene_comp.h"
#include "network/client_message_sender.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h" // PlayerTravelHandoffComp
#include "player/comp/team_follow_comp.h"
#include "player/system/team_follow_formation.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"         // InBattleComp
#include "proto/common/component/player_comp.pb.h"         // UnregisterPlayer
#include "proto/common/component/player_network_comp.pb.h" // PlayerSessionSnapshotComp
#include "proto/common/component/team_comp.pb.h"           // TeamId
#include "proto/scene/player_state_attribute_sync.pb.h"
#include "rpc/service_metadata/player_movement_service_metadata.h"
#include "spatial/comp/nav_comp.h"
#include "spatial/system/movement.h"
#include "spatial/system/nav_query.h"
#include "thread_context/ecs_context.h"

// 纯逻辑层的广播节流按帧数写死了"250ms";帧率改了要一起改。
static_assert(kTeamFollowBroadcastIntervalFrames * 4 == kTargetFPS,
			  "kTeamFollowBroadcastIntervalFrames must stay 250ms at kTargetFPS");

namespace
{
	// 加入时离队长走过的路不超过这个水平距离(米),就地接上,不挪人。
	constexpr double kTeamFollowSnapToTrailRadius = 0.75;
	// 加入时走到队尾的路超过这么长(米)就不走了,直接放进槽位。取归队阈值 kTeamRegroupDistance(8)的两倍:
	// 能走的前提本来就是两人相距不超过 8,绕开障碍的路长到两倍以上,说明中间隔着墙,走过去不如直接放。
	constexpr double kTeamFollowMaxApproachPath = 16.0;
	// 寻路终点离队尾超过这个距离(米),说明终点不可达、Detour 给的是半截路:不能拿直线把它连到队尾。
	constexpr double kTeamFollowApproachEndTolerance = 0.5;
	// 队长一 tick 挪了这么远(米)就不是走过去的(被瞬移、被裁决拉回到远处):轨迹从新位置重来。
	// 正常行走一 tick 不到 0.5,一次上报纠正至多两三米。
	constexpr double kTeamFollowMaxLeaderJump = 4.0;

	FollowPoint ToFollowPoint(const Vector3& v)
	{
		return FollowPoint{v.x(), v.y(), v.z()};
	}

	FollowPoint ToFollowPoint(const Location& l)
	{
		return FollowPoint{l.x(), l.y(), l.z()};
	}

	Location ToLocation(const FollowPoint& p)
	{
		Location out;
		out.set_x(p.x);
		out.set_y(p.y);
		out.set_z(p.z);
		return out;
	}

	uint64_t GuidOf(entt::entity entity)
	{
		const auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(entity);
		return guid != nullptr ? guid->value : 0;
	}

	// 按玩家号找本节点上的实体,并核对槽位没有被别的玩家复用。
	entt::entity LivePlayer(uint64_t playerId)
	{
		if (playerId == 0)
		{
			return entt::null;
		}
		const auto entity = tlsEcs.GetPlayer(playerId);
		if (entity == entt::null || !tlsEcs.actorRegistry.valid(entity) || GuidOf(entity) != playerId)
		{
			return entt::null;
		}
		return entity;
	}

	SceneEntity SceneOf(entt::entity entity)
	{
		const auto* scene = tlsEcs.actorRegistry.try_get<SceneEntityComp>(entity);
		return scene != nullptr ? scene->sceneEntity : SceneEntity{entt::null};
	}

	uint64_t TeamOf(entt::entity entity)
	{
		const auto* team = tlsEcs.actorRegistry.try_get<TeamId>(entity);
		return team != nullptr ? team->team_id() : 0;
	}

	uint32_t JoinSeqOf(entt::entity entity)
	{
		const auto* joinSeq = tlsEcs.actorRegistry.try_get<TeamFollowJoinSeqComp>(entity);
		return joinSeq != nullptr ? joinSeq->joinSeq : 0;
	}

	// 正在离开本节点:归属交接在途(冻结 / 交接意图)或退出中。此刻的状态已经或即将落盘交出去,不能再写。
	bool IsLeavingNode(entt::entity entity)
	{
		return tlsEcs.actorRegistry.any_of<PlayerFrozenComp, PlayerTravelHandoffComp, UnregisterPlayer>(entity);
	}

	// 这一 tick 必须原地不动,但仍在队形里:战斗中。
	// 离线不算:断线保留中(PlayerConnectionLostComp)的队员照样被驱动 —— 走位不需要客户端,
	// 以后的离线托管 / 离线挂机也靠这一点让下线的队员继续跟着队长走(team-follow-server-trail.md §3.6)。
	bool IsHeldStill(entt::entity entity)
	{
		return tlsEcs.actorRegistry.any_of<InBattleComp>(entity);
	}

	// 有可投递的网关会话:断线保留中的实体会话已被摘(置无效),此时不发,免得每条都打一行"找不到会话"。
	bool HasLiveGateSession(entt::entity entity)
	{
		const auto* snapshot = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(entity);
		return snapshot != nullptr && snapshot->gate_session_id() != 0 &&
			   snapshot->gate_session_id() != kInvalidSessionId;
	}

	void SendFollowState(entt::entity follower, bool driven, entt::entity leader, uint32_t slot,
		TeamFollowStopReason reason)
	{
		if (!HasLiveGateSession(follower))
		{
			return;
		}
		TeamFollowStateS2C state;
		state.set_driven(driven);
		if (driven)
		{
			state.set_leader_entity(actor_net_id::Of(tlsEcs.actorRegistry, leader));
			state.set_slot(slot);
		}
		else
		{
			state.set_reason(reason);
		}
		SendToPlayerClient(SceneMovementClientPlayerNotifyTeamFollowStateMessageId, state, follower);
	}

	// 回收捷径的可走性:沿导航射线校验。导航数据按需取(只有出现回收候选时才查);
	// 场景没有导航数据时放行,与移动裁决的 fail-open 同口径。
	TeamFollowTrail::WalkableCheck MakeWalkableCheck(entt::entity leader)
	{
		return [leader](const FollowPoint& from, const FollowPoint& to) {
			NavComp* nav = NavQuerySystem::GetNavForPlayer(leader);
			if (nav == nullptr)
			{
				return true;
			}
			Location clamped;
			return NavQuerySystem::ValidateMove(*nav, ToLocation(from), ToLocation(to), clamped);
		};
	}

	// 停在原地:清零速度;上一条广播还带着行走速度时补一条停步(观察者与本人都按它外推)。
	void StopMotion(entt::entity follower, TeamFollowerComp& driven)
	{
		auto& registry = tlsEcs.actorRegistry;
		if (auto* velocity = registry.try_get<Velocity>(follower);
			velocity != nullptr && (velocity->x() != 0.0 || velocity->y() != 0.0 || velocity->z() != 0.0))
		{
			velocity->Clear();
			SetActorBaseAttributesS2CAttrDirtyBit(follower, ActorBaseAttributesS2C::kVelocityFieldNumber);
		}
		if (driven.broadcastMoving)
		{
			MovementSystem::BroadcastMove(follower, /*includeSelf=*/true);
			driven.broadcastMoving = false;
			driven.broadcastVelocityX = 0.0;
			driven.broadcastVelocityY = 0.0;
			driven.broadcastFrame = tlsFrameTimeManager.frameTime.current_frame();
		}
	}

	// 把一步走位写进 Transform / Velocity,按节流规则广播(观察者 + 本人)。
	void ApplyWalkStep(entt::entity follower, TeamFollowerComp& driven, const TeamFollowStep& step,
		double deltaSeconds)
	{
		auto& registry = tlsEcs.actorRegistry;
		auto* transform = registry.try_get<Transform>(follower);
		if (transform == nullptr)
		{
			return;
		}
		if (step.moved <= 0.0 || deltaSeconds <= 0.0)
		{
			StopMotion(follower, driven);
			return;
		}

		const FollowPoint from = ToFollowPoint(transform->location());
		const double dx = step.position.x - from.x;
		const double dy = step.position.y - from.y;
		const double dz = step.position.z - from.z;
		// 别的系统在两 tick 之间把他停下过(断线保留进场时的 StopMotionForExit 会清速度并给观察者发停步):
		// 观察者手里最后一条已经是"停",这一 tick 要当作重新起步立刻补发,而不是等周期校正。
		const auto* before = registry.try_get<Velocity>(follower);
		const bool stoppedElsewhere = driven.broadcastMoving &&
			(before == nullptr || (before->x() == 0.0 && before->y() == 0.0 && before->z() == 0.0));

		auto* location = transform->mutable_location();
		location->set_x(step.position.x);
		location->set_y(step.position.y);
		location->set_z(step.position.z);
		if (dx * dx + dy * dy > 1e-12)
		{
			// 朝向沿行进方向,与客户端上报同一口径(客户端 WorldCoordinateConverter):Rotation.z 是 Unity 的偏航角,
			// 单位是**度**;服务器坐标里朝向 = (cos z, sin z),所以 z = atan2(dy, dx) 换成度。x / y(俯仰、翻滚)取 0。
			// 不能照抄 ViewSystem::LookAtPosition(它写的是弧度,与客户端口径不一致)。
			constexpr double kRadiansToDegrees = 180.0 / 3.14159265358979323846;
			auto* rotation = transform->mutable_rotation();
			rotation->set_x(0.0);
			rotation->set_y(0.0);
			rotation->set_z(std::atan2(dy, dx) * kRadiansToDegrees);
		}
		SetActorBaseAttributesS2CAttrDirtyBit(follower, ActorBaseAttributesS2C::kTransformFieldNumber);

		// 运动学矢量 = 这一 tick 的位移 / 时长:观察者按它外推到下一条广播。
		// 写入口按需创建(移动 handler 同样是写时 emplace)。
		auto& velocity = registry.get_or_emplace<Velocity>(follower);
		velocity.set_x(dx / deltaSeconds);
		velocity.set_y(dy / deltaSeconds);
		velocity.set_z(dz / deltaSeconds);
		SetActorBaseAttributesS2CAttrDirtyBit(follower, ActorBaseAttributesS2C::kVelocityFieldNumber);

		const uint32_t frame = tlsFrameTimeManager.frameTime.current_frame();
		const TeamFollowBroadcastState last{driven.broadcastMoving && !stoppedElsewhere, driven.broadcastVelocityX,
											driven.broadcastVelocityY};
		const TeamFollowBroadcastState now{true, velocity.x(), velocity.y()};
		if (TeamFollowShouldBroadcast(last, now, frame - driven.broadcastFrame))
		{
			MovementSystem::BroadcastMove(follower, /*includeSelf=*/true);
			driven.broadcastMoving = true;
			driven.broadcastVelocityX = now.velocityX;
			driven.broadcastVelocityY = now.velocityY;
			driven.broadcastFrame = frame;
		}
	}

	// 直接放进槽位:同场景瞬移(TeleportS2C 给本人,AOI 按新位置重建)。朝向取队长的朝向。
	void TeleportIntoSlot(entt::entity follower, TeamFollowerComp& driven, entt::entity leader,
		const FollowPoint& slot)
	{
		auto& registry = tlsEcs.actorRegistry;
		Vector3 location;
		location.set_x(slot.x);
		location.set_y(slot.y);
		location.set_z(slot.z);
		Rotation rotation;
		if (const auto* leaderTransform = registry.try_get<Transform>(leader))
		{
			rotation = leaderTransform->rotation();
		}
		MovementSystem::TeleportWithinScene(follower, location, rotation, kTeleportReasonTeamRegroup);
		// TeleportWithinScene 已清零速度并广播过新位置。
		driven.broadcastMoving = false;
		driven.broadcastVelocityX = 0.0;
		driven.broadcastVelocityY = 0.0;
		driven.broadcastFrame = tlsFrameTimeManager.frameTime.current_frame();
	}

	// 摘掉驱动的副作用(不碰队形;调用方已把他移出队形,或队形已不存在)。
	void ReleaseFollower(entt::entity follower, TeamFollowStopReason reason)
	{
		auto& registry = tlsEcs.actorRegistry;
		auto* driven = registry.try_get<TeamFollowerComp>(follower);
		if (driven == nullptr)
		{
			return;
		}
		// 先停:此后 MovementSystem 不再排除他,不清零的话会按队形最后写的速度继续积分。
		StopMotion(follower, *driven);
		registry.remove<TeamFollowerComp>(follower);
		// 瞬移落位等待只对客户端上报有意义;驱动期间上报全被丢弃,恢复上报时以当前位置为准。
		MovementSystem::CancelTeleportSettle(follower);
		SendFollowState(follower, false, entt::null, 0, reason);
		LOG_INFO << "[TeamFollow] metric=team_follow_detached reason=" << TeamFollowStopReason_Name(reason)
				 << " player_id=" << GuidOf(follower);
	}

	void DissolveFormation(entt::entity leader, TeamFollowStopReason reason)
	{
		auto& registry = tlsEcs.actorRegistry;
		auto* comp = registry.try_get<TeamFollowFormationComp>(leader);
		if (comp == nullptr)
		{
			return;
		}
		// 先拷名单再摘组件:摘掉之后 comp 失效。
		const auto members = comp->formation.Members();
		registry.remove<TeamFollowFormationComp>(leader);
		for (const auto& member : members)
		{
			const auto follower = LivePlayer(member.playerId);
			if (follower == entt::null)
			{
				continue;
			}
			const auto* driven = registry.try_get<TeamFollowerComp>(follower);
			if (driven != nullptr && driven->leader == leader)
			{
				ReleaseFollower(follower, reason);
			}
		}
		LOG_INFO << "[TeamFollow] metric=team_follow_dissolved reason=" << TeamFollowStopReason_Name(reason)
				 << " leader_id=" << GuidOf(leader) << " members=" << members.size();
	}

	// 队形还能不能用(队长侧)。不能用时给出队员看到的原因。
	std::optional<TeamFollowStopReason> FormationStopReason(entt::entity leader, const TeamFollowFormationComp& comp)
	{
		auto& registry = tlsEcs.actorRegistry;
		if (GuidOf(leader) == 0 || !registry.all_of<Transform>(leader) || IsLeavingNode(leader))
		{
			return TEAM_FOLLOW_STOP_REASON_LEADER_GONE;
		}
		if (SceneOf(leader) != comp.scene)
		{
			return TEAM_FOLLOW_STOP_REASON_SCENE_CHANGED;
		}
		// 队长离队 / 换队,或他自己成了别人的随从:对队员来说都是"队长换了"。
		if (TeamOf(leader) != comp.teamId || registry.all_of<TeamFollowerComp>(leader))
		{
			return TEAM_FOLLOW_STOP_REASON_LEADER_CHANGED;
		}
		return std::nullopt;
	}

	// 这名队员还能不能留在队形里(队员侧)。
	std::optional<TeamFollowStopReason> MemberStopReason(entt::entity follower, entt::entity leader,
		const TeamFollowFormationComp& comp)
	{
		auto& registry = tlsEcs.actorRegistry;
		if (follower == entt::null || !registry.all_of<Transform>(follower))
		{
			return TEAM_FOLLOW_STOP_REASON_LEAVING;
		}
		const auto* driven = registry.try_get<TeamFollowerComp>(follower);
		if (driven == nullptr || driven->leader != leader)
		{
			return TEAM_FOLLOW_STOP_REASON_LEADER_CHANGED;
		}
		if (!registry.all_of<TeamFollowIntentComp>(follower))
		{
			return TEAM_FOLLOW_STOP_REASON_LEFT_FOLLOW;
		}
		if (IsLeavingNode(follower))
		{
			return TEAM_FOLLOW_STOP_REASON_LEAVING;
		}
		if (TeamOf(follower) != comp.teamId)
		{
			return TEAM_FOLLOW_STOP_REASON_TEAM_CHANGED;
		}
		if (SceneOf(follower) != comp.scene)
		{
			return TEAM_FOLLOW_STOP_REASON_SCENE_CHANGED;
		}
		return std::nullopt;
	}

	// 新队员加入:剪掉谁都不需要的旧历史,寻路到队尾,把这段路接在轨迹尾部。走不过去返回 false。
	bool PrependApproachPath(entt::entity follower, TeamFollowFormation& formation, std::size_t rank,
		const FollowPoint& position)
	{
		formation.TrimTailForNewcomer(rank);
		auto& trail = formation.Trail();
		const FollowPoint tail = trail.Tail();

		std::vector<FollowPoint> path;
		if (NavComp* nav = NavQuerySystem::GetNavForPlayer(follower))
		{
			std::vector<Location> navPath;
			if (!NavQuerySystem::FindPath(*nav, ToLocation(position), ToLocation(tail), navPath) || navPath.empty())
			{
				return false;
			}
			path.reserve(navPath.size());
			for (const auto& point : navPath)
			{
				path.push_back(ToFollowPoint(point));
			}
			// 终点不可达时 Detour 给最近可达的半截路:那一截到队尾之间没有可走的保证。
			if (FollowHorizontalDistance(path.back(), tail) > kTeamFollowApproachEndTolerance)
			{
				return false;
			}
		}
		else
		{
			// 场景没有导航数据:直线(与移动裁决的 fail-open 同口径)。
			path = {position, tail};
		}

		double length = 0.0;
		for (std::size_t i = 1; i < path.size(); ++i)
		{
			length += FollowHorizontalDistance(path[i - 1], path[i]);
		}
		if (length > kTeamFollowMaxApproachPath)
		{
			return false;
		}
		trail.PrependPath(path);
		return true;
	}

	// 把 follower 放进 leader 的队形(调用方已确认他不在任何队形里、队形未满)。
	void PlaceMember(entt::entity leader, TeamFollowFormationComp& comp, entt::entity follower, bool walkAllowed)
	{
		auto& registry = tlsEcs.actorRegistry;
		auto& formation = comp.formation;
		const uint64_t followerId = GuidOf(follower);
		const uint32_t joinSeq = JoinSeqOf(follower);
		const std::size_t rank = formation.RankFor(joinSeq, followerId);
		const FollowPoint position = ToFollowPoint(registry.get<Transform>(follower).location());

		TeamFollowMember member;
		member.playerId = followerId;
		member.joinSeq = joinSeq;
		const char* placement = "snap";
		bool teleport = false;
		if (const auto odo = formation.Trail().NearestOdo(position, kTeamFollowSnapToTrailRadius))
		{
			// 1. 已经站在队长走过的路上(例如整队打完一场战斗):原地接上,槽位靠前就沿路赶过去,靠后就等。
			member.cursorOdo = *odo;
		}
		else if (walkAllowed && PrependApproachPath(follower, formation, rank, position))
		{
			// 2. 走过去:到队尾的那段路已接在轨迹尾部,自己就站在新的尾部。
			member.cursorOdo = formation.Trail().TailOdo();
			placement = "walk";
		}
		else
		{
			// 3. 看不见队长、离得太远或中间隔着墙:直接放进槽位。
			member.cursorOdo = formation.SlotOdo(rank);
			placement = "teleport";
			teleport = true;
		}
		formation.Add(member);

		auto& driven = registry.emplace_or_replace<TeamFollowerComp>(follower);
		driven = TeamFollowerComp{};
		driven.leader = leader;
		driven.leaderPlayerId = GuidOf(leader);
		driven.broadcastFrame = tlsFrameTimeManager.frameTime.current_frame();
		if (teleport)
		{
			TeleportIntoSlot(follower, driven, leader, formation.Trail().PointAt(member.cursorOdo));
		}

		const auto finalRank = formation.RankOf(followerId).value_or(rank);
		SendFollowState(follower, true, leader, static_cast<uint32_t>(finalRank + 1), TEAM_FOLLOW_STOP_REASON_UNSPECIFIED);
		LOG_INFO << "[TeamFollow] metric=team_follow_attached placement=" << placement << " player_id=" << followerId
				 << " leader_id=" << GuidOf(leader) << " slot=" << (finalRank + 1)
				 << " trail_points=" << formation.Trail().PointCount();
	}

	// 队长不连续(被瞬移、被裁决拉回到远处):轨迹从新位置重来,全体按加入规则重新落位。
	void RestartFormation(entt::entity leader, TeamFollowFormationComp& comp, const FollowPoint& leaderPosition)
	{
		auto& registry = tlsEcs.actorRegistry;
		const auto members = comp.formation.Members();
		comp.formation = TeamFollowFormation(leaderPosition, comp.formation.Spacing());
		for (const auto& member : members)
		{
			const auto follower = LivePlayer(member.playerId);
			const auto* transform = follower != entt::null ? registry.try_get<Transform>(follower) : nullptr;
			if (transform == nullptr || registry.try_get<TeamFollowerComp>(follower) == nullptr)
			{
				continue; // 下面的孤儿清理会收尾
			}
			const bool withinWalkDistance =
				FollowHorizontalDistance(ToFollowPoint(transform->location()), leaderPosition) <= kTeamFollowMaxApproachPath * 0.5;
			PlaceMember(leader, comp, follower, withinWalkDistance);
		}
		LOG_INFO << "[TeamFollow] metric=team_follow_restarted leader_id=" << GuidOf(leader)
				 << " members=" << members.size();
	}

	// 队员身上挂着驱动,但他的队形已经不在了(队长实体销毁、队形被解散时他不在名单里):摘掉。
	void ReleaseOrphans()
	{
		auto& registry = tlsEcs.actorRegistry;
		static thread_local std::vector<entt::entity> orphans;
		orphans.clear();
		for (auto&& [follower, driven] : registry.view<TeamFollowerComp>().each())
		{
			const bool leaderAlive = registry.valid(driven.leader) && GuidOf(driven.leader) == driven.leaderPlayerId;
			const auto* comp = leaderAlive ? registry.try_get<TeamFollowFormationComp>(driven.leader) : nullptr;
			if (comp == nullptr || comp->formation.Find(GuidOf(follower)) == nullptr)
			{
				orphans.push_back(follower);
			}
		}
		for (const auto follower : orphans)
		{
			ReleaseFollower(follower, TEAM_FOLLOW_STOP_REASON_LEADER_GONE);
		}
	}
} // namespace

void TeamFollowSystem::Update(const double deltaSeconds)
{
	auto& registry = tlsEcs.actorRegistry;
	static thread_local std::vector<std::pair<entt::entity, TeamFollowStopReason>> dissolving;
	static thread_local std::vector<std::pair<uint64_t, TeamFollowStopReason>> leaving;
	static thread_local std::vector<TeamFollowStep> steps;
	dissolving.clear();

	for (auto&& [leader, comp] : registry.view<TeamFollowFormationComp>().each())
	{
		if (const auto reason = FormationStopReason(leader, comp))
		{
			dissolving.emplace_back(leader, *reason);
			continue;
		}
		// 队长在战斗里:整队原地不动(整队开战时队员也都在战斗里;战后接着走,不用重新排队)。
		// 进战那一刻还在走的队员补一条停步,否则观察者会按最后一条广播的速度把他外推出去。
		if (registry.any_of<InBattleComp>(leader))
		{
			for (const auto& member : comp.formation.Members())
			{
				const auto follower = LivePlayer(member.playerId);
				if (auto* driven = follower != entt::null ? registry.try_get<TeamFollowerComp>(follower) : nullptr)
				{
					StopMotion(follower, *driven);
				}
			}
			continue;
		}

		auto& formation = comp.formation;
		leaving.clear();
		for (const auto& member : formation.Members())
		{
			const auto follower = LivePlayer(member.playerId);
			if (const auto reason = MemberStopReason(follower, leader, comp))
			{
				leaving.emplace_back(member.playerId, *reason);
			}
		}
		for (const auto& [playerId, reason] : leaving)
		{
			formation.Remove(playerId);
			if (const auto follower = LivePlayer(playerId); follower != entt::null)
			{
				const auto* driven = registry.try_get<TeamFollowerComp>(follower);
				if (driven != nullptr && driven->leader == leader)
				{
					ReleaseFollower(follower, reason);
				}
			}
		}

		const FollowPoint leaderPosition = ToFollowPoint(registry.get<Transform>(leader).location());
		if (FollowHorizontalDistance(formation.Trail().Head(), leaderPosition) > kTeamFollowMaxLeaderJump)
		{
			RestartFormation(leader, comp, leaderPosition);
			continue;
		}

		for (const auto& member : formation.Members())
		{
			const auto follower = LivePlayer(member.playerId);
			formation.SetSuspended(member.playerId, follower == entt::null || IsHeldStill(follower));
		}

		formation.Tick(leaderPosition, deltaSeconds, MakeWalkableCheck(leader), steps);

		for (const auto& step : steps)
		{
			const auto follower = LivePlayer(step.playerId);
			auto* driven = follower != entt::null ? registry.try_get<TeamFollowerComp>(follower) : nullptr;
			if (driven == nullptr)
			{
				continue;
			}
			if (step.suspended)
			{
				StopMotion(follower, *driven);
				continue;
			}
			if (step.placed)
			{
				LOG_INFO << "[TeamFollow] metric=team_follow_placed reason=fell_behind player_id=" << step.playerId
						 << " leader_id=" << GuidOf(leader);
				TeleportIntoSlot(follower, *driven, leader, step.position);
				continue;
			}
			ApplyWalkStep(follower, *driven, step, deltaSeconds);
		}
	}

	for (const auto& [leader, reason] : dissolving)
	{
		DissolveFormation(leader, reason);
	}
	ReleaseOrphans();
}

bool TeamFollowSystem::IsDriven(entt::entity player)
{
	return tlsEcs.actorRegistry.valid(player) && tlsEcs.actorRegistry.all_of<TeamFollowerComp>(player);
}

void TeamFollowSystem::SetFollowIntent(entt::entity player, const bool follow)
{
	auto& registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return;
	}
	if (follow)
	{
		if (!registry.all_of<TeamFollowIntentComp>(player))
		{
			registry.emplace<TeamFollowIntentComp>(player);
		}
		return;
	}
	registry.remove<TeamFollowIntentComp>(player);
	Detach(player, TEAM_FOLLOW_STOP_REASON_LEFT_FOLLOW);
}

bool TeamFollowSystem::WantsFollow(entt::entity player)
{
	return tlsEcs.actorRegistry.valid(player) && tlsEcs.actorRegistry.all_of<TeamFollowIntentComp>(player);
}

void TeamFollowSystem::RememberJoinSeq(entt::entity player, const uint32_t joinSeq)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}
	tlsEcs.actorRegistry.emplace_or_replace<TeamFollowJoinSeqComp>(player, TeamFollowJoinSeqComp{joinSeq});
}

bool TeamFollowSystem::TryAttach(entt::entity follower, entt::entity leader, const bool walkAllowed)
{
	auto& registry = tlsEcs.actorRegistry;
	if (follower == leader || !registry.valid(follower) || !registry.valid(leader) || !WantsFollow(follower))
	{
		return false;
	}
	const uint64_t followerId = GuidOf(follower);
	const uint64_t leaderId = GuidOf(leader);
	const auto* leaderTransform = registry.try_get<Transform>(leader);
	if (followerId == 0 || leaderId == 0 || leaderTransform == nullptr || !registry.all_of<Transform>(follower))
	{
		return false;
	}
	// 两边的成员关系(TeamId)由各自的刷新写入,先后不定;没对齐就等下一次信号,不按半新半旧的关系排队。
	const uint64_t teamId = TeamOf(leader);
	if (teamId == 0 || TeamOf(follower) != teamId)
	{
		return false;
	}
	if (IsLeavingNode(follower) || IsLeavingNode(leader))
	{
		return false;
	}

	// 已经接在这个队长身后:只补一次状态通知(客户端重复声明跟随时它要知道自己仍在被驱动)。
	if (const auto* driven = registry.try_get<TeamFollowerComp>(follower); driven != nullptr && driven->leader == leader)
	{
		if (const auto* comp = registry.try_get<TeamFollowFormationComp>(leader))
		{
			if (const auto rank = comp->formation.RankOf(followerId))
			{
				SendFollowState(follower, true, leader, static_cast<uint32_t>(*rank + 1),
					TEAM_FOLLOW_STOP_REASON_UNSPECIFIED);
				return true;
			}
		}
	}

	Detach(follower, TEAM_FOLLOW_STOP_REASON_LEADER_CHANGED);          // 接在别人身后:先摘下来
	DissolveFormation(follower, TEAM_FOLLOW_STOP_REASON_LEADER_CHANGED); // 自己还挂着队形(曾是队长)
	Detach(leader, TEAM_FOLLOW_STOP_REASON_BECAME_LEADER);              // 队长不能同时被驱动

	auto* comp = registry.try_get<TeamFollowFormationComp>(leader);
	if (comp != nullptr && (comp->teamId != teamId || comp->scene != SceneOf(leader)))
	{
		DissolveFormation(leader, TEAM_FOLLOW_STOP_REASON_LEADER_CHANGED);
		comp = nullptr;
	}
	if (comp == nullptr)
	{
		comp = &registry.emplace<TeamFollowFormationComp>(
			leader, TeamFollowFormationComp{TeamFollowFormation(ToFollowPoint(leaderTransform->location())), teamId,
											 SceneOf(leader)});
	}
	if (comp->formation.Members().size() >= kTeamFollowMaxMembers)
	{
		LOG_WARN << "[TeamFollow] metric=team_follow_skipped reason=formation_full player_id=" << followerId
				 << " leader_id=" << leaderId;
		return false;
	}

	PlaceMember(leader, *comp, follower, walkAllowed);
	return true;
}

void TeamFollowSystem::Detach(entt::entity follower, const TeamFollowStopReason reason)
{
	auto& registry = tlsEcs.actorRegistry;
	if (!registry.valid(follower))
	{
		return;
	}
	const auto* driven = registry.try_get<TeamFollowerComp>(follower);
	if (driven == nullptr)
	{
		return;
	}
	if (registry.valid(driven->leader))
	{
		if (auto* comp = registry.try_get<TeamFollowFormationComp>(driven->leader))
		{
			comp->formation.Remove(GuidOf(follower));
		}
	}
	ReleaseFollower(follower, reason);
}

void TeamFollowSystem::ResendStateAfterEntry(entt::entity player)
{
	auto& registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return;
	}
	const auto* driven = registry.try_get<TeamFollowerComp>(player);
	if (driven == nullptr || !registry.valid(driven->leader))
	{
		return;
	}
	const auto* comp = registry.try_get<TeamFollowFormationComp>(driven->leader);
	if (comp == nullptr || comp->scene != SceneOf(player))
	{
		return;
	}
	if (const auto rank = comp->formation.RankOf(GuidOf(player)))
	{
		SendFollowState(player, true, driven->leader, static_cast<uint32_t>(*rank + 1),
			TEAM_FOLLOW_STOP_REASON_UNSPECIFIED);
	}
}

void TeamFollowSystem::OnTeamChanged(entt::entity player)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}
	Detach(player, TEAM_FOLLOW_STOP_REASON_TEAM_CHANGED);
	DissolveFormation(player, TEAM_FOLLOW_STOP_REASON_LEADER_CHANGED);
}

void TeamFollowSystem::OnBecameLeader(entt::entity player)
{
	Detach(player, TEAM_FOLLOW_STOP_REASON_BECAME_LEADER);
}

void TeamFollowSystem::OnNotLeader(entt::entity player)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}
	DissolveFormation(player, TEAM_FOLLOW_STOP_REASON_LEADER_CHANGED);
}
