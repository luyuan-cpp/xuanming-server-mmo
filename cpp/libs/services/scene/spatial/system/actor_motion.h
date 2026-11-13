#pragma once

#include <cstdint>

#include "entt/src/entt/entity/registry.hpp"

#include "player/comp/afk_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/team_follow_comp.h"
#include "proto/common/component/actor_comp.pb.h"   // Transform、Velocity
#include "proto/common/component/battle_comp.pb.h"  // InBattleComp
#include "proto/scene/player_movement.pb.h"         // ActorMoveS2C

// 一个 actor 的移动状态怎么告诉观察者:消息内容和"他是不是在走"的判据都只在这里定。
// 用它的地方:MovementSystem(移动广播、积分位移的排除目录)与 AoiSystem(新观察者进视野时的补发)。
// 只读组件、没有 I/O,头文件内联:AOI 用它不必依赖 movement.cpp(两者不互相依赖)。
namespace actor_motion
{

// ActorMoveS2C.move_state 的取值(proto/scene/player_movement.proto):0=停止,1=行走。
inline constexpr uint32_t kMoveStateStopped = 0;
inline constexpr uint32_t kMoveStateWalking = 1;

// 速度矢量非零。
inline bool HasVelocity(const Velocity& velocity)
{
	return velocity.x() != 0.0 || velocity.y() != 0.0 || velocity.z() != 0.0;
}

// actor 此刻的移动状态写成 ActorMoveS2C(133,或 135 里的一项)。纯函数,组件由调用方取好传进来。
//   netId:actor 的网络号,调用方保证非 0。
//   velocity:可为空(没有 Velocity 组件),此时不带速度、按停止发。
//   serverTimeMs:协议时间戳,客户端可见,用墙钟毫秒(与 MoveAckS2C.server_time_ms 同一口径)。
// move 先清空再写,复用的消息对象不会带上一次的速度。
inline void FillActorMove(const uint64_t netId, const Transform& transform, const Velocity* velocity,
						  const uint64_t serverTimeMs, ActorMoveS2C& move)
{
	move.Clear();
	move.set_entity(netId);
	*move.mutable_transform() = transform;
	bool moving = false;
	if (velocity != nullptr)
	{
		*move.mutable_velocity() = *velocity;
		moving = HasVelocity(*velocity);
	}
	move.set_server_time_ms(serverTimeMs);
	move.set_move_state(moving ? kMoveStateWalking : kMoveStateStopped);
}

// MovementSystem::Update 不积分位移的状态。
//   - AfkComp(挂机)、PlayerFrozenComp(归属交接 / 交易封存冻结)、InBattleComp(回合制战斗在途):人停着。
//     服务器停止积分时不清速度(等客户端的 MoveStop),所以速度矢量可能还留着,只是位置不再推进。
//   - TeamFollowerComp(组队随从):位置由 TeamFollowSystem 每 tick 沿队长轨迹写,速度就是这一 tick 的位移,
//     人在走,只是不能再积分一遍。
// 往这里加状态时,同时决定它在下面的 IsAdvancing 里算"停着"还是"别的系统在推"。
inline constexpr entt::exclude_t<AfkComp, PlayerFrozenComp, InBattleComp, TeamFollowerComp> kNotIntegratedByMovement{};

// 服务器此刻是不是在推进 actor 的位置,也就是观察者应该看到他在走。
// 只看速度非零不够:停着的三种状态下速度只是残留,发给新观察者,对方会从当前位置凭空外推出一段。
// 随从例外:他可能同时带着 AfkComp(离线托管、长时间没有客户端消息),TeamFollowSystem 不看这个标记,照样每 tick 推他。
inline bool IsAdvancing(const entt::registry& registry, const entt::entity actor)
{
	const auto* velocity = registry.try_get<Velocity>(actor);
	if (velocity == nullptr || !HasVelocity(*velocity))
	{
		return false;
	}
	return registry.any_of<TeamFollowerComp>(actor) ||
		   !registry.any_of<AfkComp, PlayerFrozenComp, InBattleComp>(actor);
}

} // namespace actor_motion
