
#include "player_movement_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include <cmath>

#include "muduo/base/Logging.h"

#include "proto/common/component/actor_comp.pb.h"
#include "proto/scene/player_state_attribute_sync.pb.h"
#include "generated/attribute/actorbaseattributess2c_attribute_sync.h"
#include "network/player_message_utils.h"
#include "rpc/service_metadata/player_movement_service_metadata.h"
#include "spatial/comp/nav_comp.h"
#include "spatial/constants/nav.h"
#include "spatial/system/nav_query.h"
#include "thread_context/ecs_context.h"
#include "time/system/time.h"

namespace
{

// 服务器坐标系里"水平"是 x/y(z 为高度,见 nav_query.h 的坐标约定)。
double HorizontalDistance(const Location& a, const Location& b)
{
	const double dx = a.x() - b.x();
	const double dy = a.y() - b.y();
	return std::sqrt(dx * dx + dy * dy);
}

// Transform.location 的 proto 类型是 Vector3,移动协议字段与导航接口用的是
// 结构等价的 Location(proto 消息间无隐式转换),在 Transform 边界显式换壳
// (与 movement.cpp 同一对助手,两个编译单元各自持有)。
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

// 客户端上报速度按信任上限截断后写入 Velocity 组件(供 MovementSystem
// 每 tick 积分),并置脏位走 ActorBaseAttributesS2C 通道下发。
// 注意只缩放矢量、不改方向 —— 见 movement.cpp 关于标量/矢量语义的注释。
void ApplyReportedVelocity(const entt::entity player, const Velocity& reported)
{
	Velocity clamped = reported;
	const double speed = std::sqrt(reported.x() * reported.x() +
		reported.y() * reported.y() + reported.z() * reported.z());
	if (speed > kMaxTrustedClientSpeed)
	{
		const double scale = kMaxTrustedClientSpeed / speed;
		clamped.set_x(reported.x() * scale);
		clamped.set_y(reported.y() * scale);
		clamped.set_z(reported.z() * scale);
	}
	tlsEcs.actorRegistry.emplace_or_replace<Velocity>(player, clamped);
	SetActorBaseAttributesS2CAttrDirtyBit(player, ActorBaseAttributesS2C::kVelocityFieldNumber);
}

// 用场景导航网格裁决一次客户端上报位置(寻路阻挡点校验的核心):
//   1. 服务器当前位置 → 上报位置做 raycast,撞墙则截断在阻挡点;
//   2. 服务器当前位置不在网格上(首进场 (0,0,0) 契约缺口,见
//      TianyongMap.md §spawn)时,退化为直接吸附上报点引导落位;
//   3. 场景没有导航数据时 fail-open,原样接受(与旧行为一致,不卡人)。
// 落地 Transform + 置脏位;裁决位置与上报位置水平差超阈值时回发 MoveAck
// 让客户端纠偏。
void ApplyReportedLocation(const entt::entity player, const Location& reported,
	const Rotation& rotation, const uint32_t inputSeq)
{
	auto* transform = tlsEcs.actorRegistry.try_get<Transform>(player);
	if (!transform)
	{
		LOG_WARN << "move dropped: Transform missing, entity="
			<< static_cast<uint64_t>(entt::to_integral(player));
		return;
	}

	const Location current = ToLocation(transform->location());
	Location accepted = reported;
	if (auto* nav = NavQuerySystem::GetNavForPlayer(player))
	{
		Location clamped;
		if (NavQuerySystem::ValidateMove(*nav, current, reported, clamped))
		{
			accepted = clamped;
		}
		else
		{
			Location snappedCurrent;
			if (NavQuerySystem::SnapToMesh(*nav, current, snappedCurrent))
			{
				// 起点合法、途中撞墙:clamped 即阻挡点。
				accepted = clamped;
			}
			else
			{
				// 起点不在网格上:引导落位到上报点的吸附结果;
				// 两头都不在网格 → 拒绝本次移动,保持原位。
				Location snappedReported;
				if (NavQuerySystem::SnapToMesh(*nav, reported, snappedReported))
				{
					accepted = snappedReported;
				}
				else
				{
					accepted = current;
				}
			}
		}
	}

	WriteLocation(*transform->mutable_location(), accepted);
	*transform->mutable_rotation() = rotation;
	SetActorBaseAttributesS2CAttrDirtyBit(player, ActorBaseAttributesS2C::kTransformFieldNumber);

	if (HorizontalDistance(accepted, reported) > kMoveCorrectionEpsilon)
	{
		MoveAckS2C ack;
		ack.set_input_seq(inputSeq);
		*ack.mutable_server_location() = accepted;
		if (const auto* velocity = tlsEcs.actorRegistry.try_get<Velocity>(player))
		{
			*ack.mutable_server_velocity() = *velocity;
		}
		ack.set_server_time_ms(TimeSystem::NowMilliseconds());
		SendMessageToClientViaGate(SceneMovementClientPlayerNotifyMoveAckMessageId, ack, player);
	}
}

}  // namespace
///<<< END WRITING YOUR CODE

void SceneMovementClientPlayerHandler::MoveStart(entt::entity player,const ::MoveStartC2S* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
	ApplyReportedLocation(player, request->start_location(), request->rotation(),
		request->input_seq());
	ApplyReportedVelocity(player, request->velocity());
///<<< END WRITING YOUR CODE

}

void SceneMovementClientPlayerHandler::MoveStop(entt::entity player,const ::MoveStopC2S* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
	ApplyReportedLocation(player, request->end_location(), request->rotation(),
		request->input_seq());
	// 停止:清零运动学矢量,MovementSystem 不再积分。
	ApplyReportedVelocity(player, Velocity());
///<<< END WRITING YOUR CODE

}

void SceneMovementClientPlayerHandler::MoveSync(entt::entity player,const ::MoveSyncC2S* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
	ApplyReportedLocation(player, request->location(), request->rotation(),
		request->input_seq());
	ApplyReportedVelocity(player, request->velocity());
///<<< END WRITING YOUR CODE

}

void SceneMovementClientPlayerHandler::TeleportRequest(entt::entity player,const ::TeleportRequestC2S* request,
	::TeleportRequestC2SResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}

void SceneMovementClientPlayerHandler::NotifyMoveAck(entt::entity player,const ::MoveAckS2C* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}

void SceneMovementClientPlayerHandler::NotifyActorMove(entt::entity player,const ::ActorMoveS2C* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}

void SceneMovementClientPlayerHandler::NotifyActorMoveList(entt::entity player,const ::ActorMoveListS2C* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}

void SceneMovementClientPlayerHandler::NotifyTeleport(entt::entity player,const ::TeleportS2C* request,
	::Empty* response)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

}
