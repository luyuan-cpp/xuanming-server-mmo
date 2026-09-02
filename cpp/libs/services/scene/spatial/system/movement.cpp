#include "movement.h"

#include "generated/attribute/actorbaseattributess2c_attribute_sync.h"
#include "player/comp/afk_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/scene/player_state_attribute_sync.pb.h"
#include "spatial/comp/nav_comp.h"
#include "spatial/system/nav_query.h"
#include "thread_context/ecs_context.h"

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
	// PlayerFrozenComp exclude: players mid-cross-zone-migration must not
	// keep advancing position on the source side — the destination already
	// has the marshaled Transform and any further movement here would be
	// discarded on ACK + DestroyPlayer. See cross-zone-readiness-audit.md
	// §11.2 (passive-tick exclusion catalogue).
	auto view = tlsEcs.actorRegistry.view<Transform, Velocity>(
		entt::exclude<Acceleration, AfkComp, PlayerFrozenComp>);
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
			}
			target = clamped;
		}

		WriteLocation(*transform.mutable_location(), target);
		SetActorBaseAttributesS2CAttrDirtyBit(
			entity, ActorBaseAttributesS2C::kTransformFieldNumber);
	}
}

