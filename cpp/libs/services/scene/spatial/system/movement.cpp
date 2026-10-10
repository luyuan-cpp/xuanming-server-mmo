#include "movement.h"

#include <chrono>
#include <cmath>

#include "muduo/base/Logging.h"

#include "generated/attribute/actorbaseattributess2c_attribute_sync.h"
#include "modules/scene/comp/scene_comp.h"  // SceneEntityComp:瞬移落位等待只对当时的场景有效
#include "network/player_message_utils.h"
#include "player/comp/afk_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "proto/common/component/battle_comp.pb.h"  // InBattleComp:战斗在途不积分位移
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"  // Player:只有玩家实体才有客户端可发 TeleportS2C
#include "proto/common/component/player_network_comp.pb.h"  // PlayerSessionSnapshotComp:广播只发给有网关会话的玩家
#include "proto/scene/player_movement.pb.h"
#include "proto/scene/player_state_attribute_sync.pb.h"
#include "rpc/service_metadata/player_movement_service_metadata.h"
#include "spatial/comp/nav_comp.h"
#include "spatial/comp/scene_node_scene_comp.h"  // AoiListComp:移动广播的接收者
#include "spatial/comp/teleport_settle_comp.h"
#include "spatial/system/aoi.h"
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
}

void MovementSystem::Update(const double delta)
{
	// Velocity 是**运动学矢量**(方向 × 速率),只能由真实的移动来源写入
	// (移动输入 handler:player_movement_handler 的 MoveStart/MoveSync;
	// AI 寻路仍是空桩)。
	// 移速 buff 的标量属性在 MoveSpeedComp 里,由移动来源拿去缩放
	// 输入方向 —— 绝不要把标量直接灌进 Velocity,那会让实体沿 (1,1,1)
	// 匀速漂移(2026-08 修过的 P1)。
	//
	// PlayerFrozenComp exclude:归属交接在途(PlayerLifecycleSystem::StartTravelHandoff:
	// 跨 zone 传送 / 同 zone 跨节点换图)的玩家不得在源端继续积分位移 —— 目标节点稍后从盘上
	// 加载的 Transform 就是冻结那一次存盘里的值,此后的位移不会再落盘,放行后随
	// DestroyDeposedPlayer 丢弃(已没有 Kafka 迁移包与 ACK)。被动 tick 排除目录见
	// cross-zone-readiness-audit.md §11.2(该文只有 §11 仍有效)。
	// InBattleComp:回合制战斗在途不积分位移(进战时的残留速度会让玩家在战斗中
	// 一路飘走,战后位置与客户端画面对不上)
	auto view = tlsEcs.actorRegistry.view<Transform, Velocity>(
		entt::exclude<Acceleration, AfkComp, PlayerFrozenComp, InBattleComp>);
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

void MovementSystem::BroadcastMove(const entt::entity entity)
{
	const auto* aoiList = tlsEcs.actorRegistry.try_get<AoiListComp>(entity);
	if (aoiList == nullptr || aoiList->entries.empty())
	{
		return;
	}
	const auto* transform = tlsEcs.actorRegistry.try_get<Transform>(entity);
	if (transform == nullptr)
	{
		return;
	}

	EntityVector observers;
	observers.reserve(aoiList->entries.size());
	for (const auto& [observer, _] : aoiList->entries)
	{
		if (!tlsEcs.actorRegistry.valid(observer))
		{
			continue;
		}
		// 没有会话快照的实体(NPC 等)没有客户端可发;
		// 留在名单里只会让广播函数为每一个打一条 ERROR。
		if (!tlsEcs.actorRegistry.any_of<PlayerSessionSnapshotComp>(observer))
		{
			continue;
		}
		// 对方的兴趣表里也得有我:否则对方客户端上根本没有这个角色(隐身、对方表满),
		// 把我的坐标发过去既没有意义,也是在泄露位置。
		const auto* observerAoi = tlsEcs.actorRegistry.try_get<AoiListComp>(observer);
		if (observerAoi == nullptr || !observerAoi->Contains(entity))
		{
			continue;
		}
		observers.emplace_back(observer);
	}
	if (observers.empty())
	{
		return;
	}

	ActorMoveS2C move;
	move.set_entity(entt::to_integral(entity));
	*move.mutable_transform() = *transform;
	bool moving = false;
	if (const auto* velocity = tlsEcs.actorRegistry.try_get<Velocity>(entity))
	{
		*move.mutable_velocity() = *velocity;
		moving = velocity->x() != 0.0 || velocity->y() != 0.0 || velocity->z() != 0.0;
	}
	// 协议时间戳:客户端可见,用墙钟毫秒(与 MoveAckS2C.server_time_ms 同一口径)。
	move.set_server_time_ms(TimeSystem::NowMilliseconds());
	move.set_move_state(moving ? kMoveStateWalking : kMoveStateStopped);

	BroadcastMessageToPlayers(SceneMovementClientPlayerNotifyActorMoveMessageId, move, observers);
}


void MovementSystem::TeleportWithinScene(const entt::entity entity, const Vector3& location,
	const Rotation& rotation, const uint32_t reason)
{
	auto* transform = tlsEcs.actorRegistry.try_get<Transform>(entity);
	if (transform == nullptr)
	{
		return;
	}

	*transform->mutable_location() = location;
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

	if (tlsEcs.actorRegistry.any_of<Player>(entity))
	{
		TeleportS2C teleport;
		teleport.set_entity(entt::to_integral(entity));
		*teleport.mutable_transform() = *transform;
		teleport.set_reason(reason);
		SendMessageToClientViaGate(SceneMovementClientPlayerNotifyTeleportMessageId, teleport, entity);
	}

	AoiSystem::ResetEntityVisibility(entity);
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
	const entt::entity currentScene = sceneComp != nullptr ? sceneComp->sceneEntity : entt::null;
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
