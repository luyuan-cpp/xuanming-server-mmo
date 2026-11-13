#include "movement.h"
#include "actor/actor_net_id.h"

#include <algorithm>
#include <bit>
#include <chrono>
#include <cmath>
#include <utility>
#include <vector>

#include "muduo/base/Logging.h"

#include "generated/attribute/actorbaseattributess2c_attribute_sync.h"
#include "modules/scene/comp/scene_comp.h"  // SceneEntityComp:瞬移落位等待只对当时的场景有效
#include "network/client_message_sender.h"
#include "player/comp/afk_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/team_follow_comp.h"  // TeamFollowerComp:组队跟随中的队员位置由队形写,不积分
#include "proto/common/component/battle_comp.pb.h"  // InBattleComp:战斗在途不积分位移
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"  // Player:只有玩家实体才有客户端可发 TeleportS2C
#include "proto/common/component/player_network_comp.pb.h"  // PlayerSessionSnapshotComp:广播只发给有网关会话的玩家
#include "proto/scene/player_movement.pb.h"
#include "proto/scene/player_state_attribute_sync.pb.h"
#include "rpc/service_metadata/player_movement_service_metadata.h"
#include "spatial/comp/nav_comp.h"
#include "spatial/comp/teleport_settle_comp.h"
#include "spatial/system/actor_motion.h"  // 移动消息的内容、不积分位移的状态目录
#include "spatial/system/aoi.h"
#include "spatial/system/interest.h"  // GetVisibleWatchers:移动广播的接收者
#include "spatial/system/nav_query.h"
#include "thread_context/ecs_context.h"
#include "time/system/time.h"

namespace
{
	// Transform.location 的 proto 类型是 Vector3,导航接口(NavQuerySystem)与移动
	// 协议字段用的是结构等价的 Location(同为 double x/y/z 的独立消息,proto 间无
	// 隐式转换)。在 Transform 边界显式换壳,两侧契约都不动。
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

	// 进程内的超时用单调时钟(AGENTS §11.3):系统时间被回拨时,"等待客户端落位"不能因此永不过期。
	uint64_t SteadyNowMs()
	{
		return static_cast<uint64_t>(std::chrono::duration_cast<std::chrono::milliseconds>(
			std::chrono::steady_clock::now().time_since_epoch()).count());
	}

	// 移动广播的接收者(口径见 MovementSystem::BroadcastMove 的注释),写进 receivers(先清空)。
	void CollectMoveReceivers(const entt::entity entity, const bool includeSelf, EntityVector& receivers)
	{
		receivers.clear();
		// 接收者 = 客户端正在显示我的观察者(AOI 反向索引中可见的那些):关注是单向的,
		// 我是否看得见对方与此无关;网格范围外的显式关注者同样收到。没显示我的人(隐身、对方表满)
		// 收到坐标既没有意义,也是在泄露位置。
		InterestSystem::GetVisibleWatchers(entity, receivers);
		// 没有会话快照的实体(NPC 等)没有客户端可发;留在名单里只会让广播函数为每一个打一条 ERROR。
		std::erase_if(receivers, [](entt::entity observer)
					  { return !tlsEcs.actorRegistry.any_of<PlayerSessionSnapshotComp>(observer); });
		// 本人:只在会话有效时发(断线保留中的会话已被置无效,发了也只会打一条"找不到会话")。
		if (includeSelf && std::ranges::find(receivers, entity) == receivers.end())
		{
			if (const auto* session = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(entity);
				session != nullptr && session->gate_session_id() != 0 && session->gate_session_id() != kInvalidSessionId)
			{
				receivers.push_back(entity);
			}
		}
	}

	// entity 此刻的移动状态写成 ActorMoveS2C。没有 Transform 或网络号时返回 false(后者记 ERROR)。
	bool BuildActorMove(const entt::entity entity, ActorMoveS2C& move)
	{
		const auto* transform = tlsEcs.actorRegistry.try_get<Transform>(entity);
		if (transform == nullptr)
		{
			return false;
		}
		const auto netId = actor_net_id::Of(tlsEcs.actorRegistry, entity);
		if (netId == 0)
		{
			LOG_ERROR << "BroadcastMove: actor network id missing";
			return false;
		}
		// 消息内容只在 actor_motion.h 定:进视野补发(AoiSystem)发的是同一份。
		actor_motion::FillActorMove(netId, *transform, tlsEcs.actorRegistry.try_get<Velocity>(entity),
									TimeSystem::NowMilliseconds(), move);
		return true;
	}
}

void MovementSystem::Update(const double delta)
{
	// Velocity 是**运动学矢量**(方向 × 速率),只能由真实的移动来源写入
	// (移动输入 handler:player_movement_handler 的 MoveStart/MoveSync;
	// AI 寻路仍是空桩)。
	// 移速 buff 目前未接入移动；接入时用独立标量缩放输入方向，
	// 绝不要把标量灌进 Velocity，避免实体沿 (1,1,1) 匀速漂移(2026-08 的 P1)。
	//
	// PlayerFrozenComp exclude:归属交接在途(PlayerLifecycleSystem::StartTravelHandoff:
	// 跨 zone 传送 / 同 zone 跨节点换图)的玩家不得在源端继续积分位移 —— 目标节点稍后从盘上
	// 加载的 Transform 就是冻结那一次存盘里的值,此后的位移不会再落盘,放行后随
	// DestroyDeposedPlayer 丢弃(已没有 Kafka 迁移包与 ACK)。被动 tick 排除目录见
	// cross-zone-readiness-audit.md §11.2(该文只有 §11 仍有效)。
	// InBattleComp:回合制战斗在途不积分位移(进战时的残留速度会让玩家在战斗中
	// 一路飘走,战后位置与客户端画面对不上)
	// TeamFollowerComp:组队跟随中由服务器驱动的队员,位置每 tick 由 TeamFollowSystem 沿队长轨迹写入,
	// Velocity 只是给观察者外推用的这一 tick 位移;再积分一次就走了两遍(team-follow-server-trail.md)。
	// 排除目录收在 actor_motion.h:"哪些状态不积分"与"新观察者该不该看到他在走"(IsAdvancing)是同一份知识。
	auto view = tlsEcs.actorRegistry.view<Transform, Velocity>(actor_motion::kNotIntegratedByMovement);
	for (auto&& [entity, transform, velocity] : view.each())
	{
		if (velocity.x() == 0.0 && velocity.y() == 0.0 && velocity.z() == 0.0)
		{
			continue;
		}

		const Location current = ToLocation(transform.location());
		Location target = current;
		target.set_x(target.x() + velocity.x() * delta);
		target.set_y(target.y() + velocity.y() * delta);
		target.set_z(target.z() + velocity.z() * delta);

		// 导航阻挡夹持:撞墙就停在阻挡点并清零速度,防止两次 MoveSync
		// 之间积分穿墙。场景无导航数据时 fail-open 原样积分。
		bool stoppedByWall = false;
		if (auto* nav = NavQuerySystem::GetNavForPlayer(entity))
		{
			// 预置为当前位置:ValidateMove 在"起点不在网格上"时不写 clamped,
			// 此时原地冻结而不是跳到默认 (0,0,0)。
			Location clamped = current;
			if (!NavQuerySystem::ValidateMove(*nav, current, target, clamped))
			{
				velocity.set_x(0.0);
				velocity.set_y(0.0);
				velocity.set_z(0.0);
				SetActorBaseAttributesS2CAttrDirtyBit(
					entity, ActorBaseAttributesS2C::kVelocityFieldNumber);
				stoppedByWall = true;
			}
			target = clamped;
		}
		else if (NavQuerySystem::MissingRequiredNavigation(entity))
		{
			target = current;
			velocity.Clear();
			SetActorBaseAttributesS2CAttrDirtyBit(entity, ActorBaseAttributesS2C::kVelocityFieldNumber);
			stoppedByWall = true;
		}

		WriteLocation(*transform.mutable_location(), target);
		SetActorBaseAttributesS2CAttrDirtyBit(
			entity, ActorBaseAttributesS2C::kTransformFieldNumber);

		// 服务器自己把他停下了:立刻告诉观察者,否则他们会按上一次上报的速度继续往墙里外推,
		// 直到客户端的下一条上报。只在撞墙的那一 tick 发一次(速度已清零,之后不再进到这里)。
		if (stoppedByWall)
		{
			BroadcastMove(entity);
		}
	}
}

void MovementSystem::BroadcastMove(const entt::entity entity, const bool includeSelf)
{
	EntityVector receivers;
	CollectMoveReceivers(entity, includeSelf, receivers);
	if (receivers.empty())
	{
		return;
	}
	ActorMoveS2C move;
	if (!BuildActorMove(entity, move))
	{
		return;
	}
	BroadcastToPlayerClients(SceneMovementClientPlayerNotifyActorMoveMessageId, move, receivers);
}

void MovementSystem::HaltMotion(const entt::entity entity)
{
	// 只改已有组件、不 emplace:没有运动学组件的实体(NPC、测试宿主)什么都不做。
	auto* velocity = tlsEcs.actorRegistry.try_get<Velocity>(entity);
	if (velocity == nullptr || !actor_motion::HasVelocity(*velocity))
	{
		return;
	}
	velocity->Clear();
	// 与撞墙清速同一写法:属性同步与重连后拿到的都是 0 速度。
	SetActorBaseAttributesS2CAttrDirtyBit(entity, ActorBaseAttributesS2C::kVelocityFieldNumber);
	// 观察者手里最后一条移动还带着行走速度:补一条"停了"。
	BroadcastMove(entity);
}

void ActorMoveBatch::Add(const entt::entity entity, const bool includeSelf)
{
	if (const auto it = std::ranges::find(entries_, entity, &Entry::entity); it != entries_.end())
	{
		it->includeSelf = it->includeSelf || includeSelf;
		return;
	}
	if (entries_.size() == kMaxEntities)
	{
		Flush();
	}
	entries_.push_back(Entry{entity, includeSelf});
}

void ActorMoveBatch::Flush()
{
	if (entries_.empty())
	{
		return;
	}
	// 只有一个实体:就是一次普通广播。
	if (entries_.size() == 1)
	{
		const Entry only = entries_.front();
		entries_.clear();
		if (tlsEcs.actorRegistry.valid(only.entity))
		{
			MovementSystem::BroadcastMove(only.entity, only.includeSelf);
		}
		return;
	}

	// 每个实体一条移动(内容取此刻),连同各自的接收者;没有接收者或拿不到移动状态的跳过。
	std::vector<ActorMoveS2C> moves;
	std::vector<EntityVector> receiversPerMove;
	moves.reserve(entries_.size());
	receiversPerMove.reserve(entries_.size());
	for (const auto& entry : entries_)
	{
		if (!tlsEcs.actorRegistry.valid(entry.entity))
		{
			continue;
		}
		EntityVector receivers;
		CollectMoveReceivers(entry.entity, entry.includeSelf, receivers);
		ActorMoveS2C move;
		if (receivers.empty() || !BuildActorMove(entry.entity, move))
		{
			continue;
		}
		moves.push_back(std::move(move));
		receiversPerMove.push_back(std::move(receivers));
	}
	entries_.clear();

	// 要收同一组移动的接收者共用一条消息:一条时照旧发 ActorMoveS2C,多条时发 ActorMoveListS2C(按登记先后)。
	for (const auto& group : GroupMoveReceivers(receiversPerMove))
	{
		if (std::has_single_bit(group.moveMask))
		{
			const auto only = static_cast<std::size_t>(std::countr_zero(group.moveMask));
			BroadcastToPlayerClients(SceneMovementClientPlayerNotifyActorMoveMessageId, moves[only], group.receivers);
			continue;
		}
		ActorMoveListS2C list;
		for (std::size_t index = 0; index < moves.size(); ++index)
		{
			if ((group.moveMask >> index) & 1u)
			{
				*list.add_moves() = moves[index];
			}
		}
		BroadcastToPlayerClients(SceneMovementClientPlayerNotifyActorMoveListMessageId, list, group.receivers);
	}
}


bool MovementSystem::TeleportWithinScene(const entt::entity entity, const Vector3& location,
	const Rotation& rotation, const uint32_t reason)
{
	auto* transform = tlsEcs.actorRegistry.try_get<Transform>(entity);
	if (transform == nullptr)
	{
		return false;
	}
	if (NavQuerySystem::MissingRequiredNavigation(entity)) return false;
	Vector3 acceptedLocation = location;
	if (auto* nav = NavQuerySystem::GetNavForPlayer(entity); nav != nullptr && nav->strictMask)
	{
		std::vector<Location> path;
		Location destination;
		if (!NavQuerySystem::SnapToMesh(*nav, ToLocation(location), destination) ||
			!NavQuerySystem::FindPath(*nav, ToLocation(transform->location()), destination, path)) return false;
		WriteLocation(acceptedLocation, destination);
	}

	*transform->mutable_location() = acceptedLocation;
	*transform->mutable_rotation() = rotation;
	SetActorBaseAttributesS2CAttrDirtyBit(entity, ActorBaseAttributesS2C::kTransformFieldNumber);

	// 清零运动学矢量:否则 Update 会从新位置按瞬移之前的速度继续积分,人一落地就自己飘走。
	if (auto* velocity = tlsEcs.actorRegistry.try_get<Velocity>(entity))
	{
		velocity->Clear();
		SetActorBaseAttributesS2CAttrDirtyBit(entity, ActorBaseAttributesS2C::kVelocityFieldNumber);
	}

	// 事件驱动路径(不在 per-tick 上),emplace_or_replace 可用;重复瞬移以最后一次的目标为准。
	auto& settle = tlsEcs.actorRegistry.emplace_or_replace<TeleportSettleComp>(entity);
	const auto* sceneComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(entity);
	settle.scene = sceneComp != nullptr ? sceneComp->sceneEntity : entt::null;
	settle.targetX = location.x();
	settle.targetY = location.y();
	settle.expireAtMs = SteadyNowMs() + kTeleportSettleTimeoutMs;

	const auto netId = actor_net_id::Of(tlsEcs.actorRegistry, entity);
	if (tlsEcs.actorRegistry.any_of<Player>(entity) && netId == 0)
	{
		LOG_ERROR << "TeleportWithinScene: client notification skipped, actor network id missing";
	}
	if (tlsEcs.actorRegistry.any_of<Player>(entity) && netId != 0)
	{
		TeleportS2C teleport;
		teleport.set_entity(netId);
		*teleport.mutable_transform() = *transform;
		teleport.set_reason(reason);
		SendToPlayerClient(SceneMovementClientPlayerNotifyTeleportMessageId, teleport, entity);
	}

	AoiSystem::ResetEntityVisibility(entity);
	// 重置只撤空间来源,网格外的显式关注者仍显示它:立即把新位置发给它们,不必等它下一次移动上报。
	BroadcastMove(entity);
	return true;
}

bool MovementSystem::ShouldDropReportAfterTeleport(const entt::entity entity, const Location& reported)
{
	// 每条移动上报都会走到这里:只用 try_get,绝大多数实体没有这个组件,一次查找即返回。
	const auto* settle = tlsEcs.actorRegistry.try_get<TeleportSettleComp>(entity);
	if (settle == nullptr)
	{
		return false;
	}

	// 瞬移之后换了场景:目标点属于上一个场景,不再有可比性,直接作废并照常裁决。
	const auto* sceneComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(entity);
	const SceneEntity currentScene = sceneComp != nullptr ? sceneComp->sceneEntity : SceneEntity{entt::null};
	if (currentScene != settle->scene)
	{
		tlsEcs.actorRegistry.remove<TeleportSettleComp>(entity);
		return false;
	}

	const double dx = reported.x() - settle->targetX;
	const double dy = reported.y() - settle->targetY;
	const auto verdict = JudgeReportAfterTeleport(std::sqrt(dx * dx + dy * dy),
		SteadyNowMs(), settle->expireAtMs);
	if (verdict == TeleportReportVerdict::kStale)
	{
		return true;
	}

	if (verdict == TeleportReportVerdict::kExpired)
	{
		// 客户端一直没有在目标附近上报:TeleportS2C 丢了,或客户端没有应用它。
		// 恢复照常裁决(相信客户端上报的位置),只留一条日志供排查。
		LOG_WARN << "teleport not acknowledged in time, resume normal move validation: entity="
			<< static_cast<uint64_t>(entt::to_integral(entity))
			<< " target=(" << settle->targetX << "," << settle->targetY << ")"
			<< " reported=(" << reported.x() << "," << reported.y() << ")";
	}
	// settle 指针在 remove 之后失效,上面的日志必须先打完。
	tlsEcs.actorRegistry.remove<TeleportSettleComp>(entity);
	return false;
}

void MovementSystem::CancelTeleportSettle(const entt::entity entity)
{
	if (tlsEcs.actorRegistry.valid(entity))
	{
		tlsEcs.actorRegistry.remove<TeleportSettleComp>(entity);
	}
}
