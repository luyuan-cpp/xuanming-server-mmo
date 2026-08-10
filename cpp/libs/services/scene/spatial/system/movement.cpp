#include "movement.h"

#include "player/comp/afk_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "proto/common/component/actor_comp.pb.h"
#include "thread_context/ecs_context.h"

void MovementSystem::Update(const double delta)
{
	// Velocity 是**运动学矢量**(方向 × 速率),只能由真实的移动来源写入
	// (移动输入 handler / AI 寻路;目前两者都还是空桩,所以本视图当前为空)。
	// 移速 buff 的标量属性在 MoveSpeedComp 里,由未来的移动实现拿去缩放
	// 输入方向 —— 绝不要把标量直接灌进 Velocity,那会让实体沿 (1,1,1)
	// 匀速漂移(2026-08 修过的 P1)。
	// 接入真实移动时还要补:改完 Transform 置 kTransformFieldNumber 脏位,
	// 否则位置变化不会经 ActorBaseAttributesS2C 通道下发。
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
		auto& location = *transform.mutable_location();
		location.set_x(location.x() + velocity.x() * delta);
		location.set_y(location.y() + velocity.y() * delta);
		location.set_z(location.z() + velocity.z() * delta);
	}
}

