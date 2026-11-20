// 移动广播:MovementSystem::BroadcastMove 与同一 tick 的合并批 ActorMoveBatch(声明都在 movement.h)。
// 单独成一个编译单元:只依赖兴趣索引、网络号、消息内容(actor_motion.h)与发送函数,不碰导航;
// aoi_delivery_test 连同它的假发送函数直接编进来,断言发出去的是哪条消息、发给了谁。
#include "movement.h"

#include <algorithm>
#include <bit>
#include <utility>
#include <vector>

#include "muduo/base/Logging.h"

#include "actor/actor_net_id.h"
#include "network/client_message_sender.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/player_network_comp.pb.h"  // PlayerSessionSnapshotComp:广播只发给有网关会话的玩家
#include "proto/scene/player_movement.pb.h"
#include "rpc/service_metadata/player_movement_service_metadata.h"
#include "spatial/system/actor_motion.h"  // 移动消息的内容
#include "spatial/system/interest.h"      // GetVisibleWatchers:移动广播的接收者
#include "thread_context/ecs_context.h"
#include "time/system/time.h"

namespace
{
	// 移动广播的接收者(口径见 MovementSystem::BroadcastMove 的注释),写进 receivers(先清空)。
	void CollectMoveReceivers(const entt::entity entity, const bool includeSelf, EntityVector& receivers)
	{
		receivers.clear();
		// 接收者 = 客户端正在显示我的观察者(AOI 反向索引中可见的那些):关注是单向的,
		// 我是否看得见对方与此无关;网格范围外的显式关注者同样收到。没显示我的人(隐身、对方表满)
		// 收到坐标既没有意义,也是在泄露位置。
		InterestSystem::GetVisibleWatchers(entity, receivers);
		// 没有会话快照的实体(NPC 等)或断线保留 / 离线托管中(会话已置无效)没有客户端可发;
		// 留在名单里只会让广播函数为每一个打一条 ERROR,也会在合批里白白多拆出一组。
		std::erase_if(receivers, [](entt::entity observer)
					  {
						  const auto* session = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(observer);
						  return session == nullptr || session->gate_session_id() == kInvalidSessionId;
					  });
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
